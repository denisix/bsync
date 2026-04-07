package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// BlockJob represents a file block to be processed by a worker
type BlockJob struct {
	blockIdx    uint32
	data        []byte
	readedBytes int
}

var (
	lastBlockNum uint32 = 0
	totCompSize  uint64 = 0
	totOrigSize  uint64 = 0
	diffs        uint32 = 0
	v_skipIdx    uint32 = 0
	v_fileSize   uint64 = 0
	t0           time.Time
	mu           sync.Mutex
)

// ETA, stats
func printStats(job BlockJob, indicator string, diff, bytes uint32) {

	mu.Lock()
	diffs = diffs + diff
	if bytes > 0 {
		totOrigSize = totOrigSize + uint64(blockSize)
		totCompSize = totCompSize + uint64(bytes)
	}
	mu.Unlock()

	blockMb := float64(blockSize) / float64(mb1)

	blocksLeft := float64(lastBlockNum - job.blockIdx)
	mbsLeft := blocksLeft * blockMb
	mbsDone := float64(job.blockIdx-v_skipIdx) * blockMb

	mbs := mbsDone / time.Since(t0).Seconds()

	eta := 0
	etaUnit := "min"

	if mbs > 0 {
		eta = int(mbsLeft / mbs / 60)
		etaUnit = "min"
		if eta > 180 {
			eta = eta / 60
			etaUnit = "hr"
		}
	}
	if eta < 0 {
		eta = 0
	}
	if job.blockIdx >= lastBlockNum {
		eta = 0
	}

	percent := 100 * float64(job.blockIdx) / float64(lastBlockNum)
	ratio := 100 * float64(totCompSize) / float64(totOrigSize)

	Log("block %d/%d (%0.2f%%) [%s] size=%d ratio=%0.2f %0.2f MB/s ETA=%d %s diffs=%d\r", job.blockIdx, lastBlockNum, percent, indicator, v_fileSize, ratio, mbs, eta, etaUnit, diffs)
}

// startClient launches threadsCount workers, each with a persistent connection, and pushes file blocks to a jobs channel
func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache, workers int) {
	Log("startClient()\n")
	lastBlockNum = uint32(fileSize / uint64(blockSize))
	Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	var wg sync.WaitGroup
	t0 = time.Now()
	v_skipIdx = skipIdx
	v_fileSize = fileSize

	// Resolve server address once
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		Err("resolving: %s\n", err.Error())
		return
	}

	// Create sequential reader with channel-based output
	reader := NewSequentialReader(file, blockSize, fileSize, skipIdx, workers*2)
	reader.Start()

	// Channel for precomputed blocks (hash + compressed data)
	precompressedChan := make(chan PrecomputedBlock, workers*2)

	// Start parallel checksum + compression workers
	go precomputeChecksumsParallel(reader, checksumCache, precompressedChan, workers)

	// Start worker goroutines for network transfer
	Log("starting %d transfer workers\n", workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(saddr *net.TCPAddr) {
			defer wg.Done()
			conn := NewAutoReconnectTCP(saddr)
			defer conn.Close()
			for block := range precompressedChan {
				var lastErr error
				for retry := 0; retry < maxRetries; retry++ {
					if retry > 0 {
						Log("block %d: retry %d/%d after: %v\n", block.BlockIdx, retry, maxRetries-1, lastErr)
						conn.Close() // force reconnect on next call
						time.Sleep(time.Duration(retry) * time.Second)
					}
					if lastErr = processPrecomputedBlock(conn, block, blockSize, fileSize, noCompress, checksumCache); lastErr == nil {
						break
					}
				}
				if lastErr != nil {
					Log("block %d: failed after %d retries: %v\n", block.BlockIdx, maxRetries, lastErr)
				}
			}
		}(saddr)
	}

	Log("DONE, waiting for the workers\n")
	wg.Wait()

	// Send DONE message to server
	magicBytes := stringToFixedSizeArray(magicHead)
	conn := NewAutoReconnectTCP(saddr)
	defer conn.Close()
	msg, err1 := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   0,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   0,
		Compressed: false,
		Zero:       false,
		Done:       true,
	})
	if err1 != nil {
		Log("cant pack msg-> %s\n", err1)
		return
	}

	n, err2 := conn.Write(msg)
	if err2 != nil && err2 != io.EOF {
		Log("\t- error writing net: [%d] %s\n", n, err2.Error())
		return
	}

	Log("\nDONE, exiting..\n\n")
	time.Sleep(2 * time.Second)
	return
}

// processPrecomputedBlock sends a pre-hashed and pre-compressed block.
// Returns non-nil error if the block could not be delivered; caller should retry.
func processPrecomputedBlock(conn *AutoReconnectTCP, block PrecomputedBlock, blockSize uint32, fileSize uint64, noCompress bool, checksumCache *ChecksumCache) error {
	magicBytes := stringToFixedSizeArray(magicHead)

	// Send request to server
	msg, err1 := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   block.BlockIdx,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   0,
		Compressed: false,
		Zero:       false,
		Done:       false,
	})
	if err1 != nil {
		return fmt.Errorf("pack: %w", err1)
	}

	if err := connWrite(conn, msg); err != nil {
		return fmt.Errorf("send header: %w", err)
	}

	// Get server hash
	serverHash := make([]byte, len(block.Hash))
	if _, err := io.ReadFull(conn, serverHash); err != nil {
		return fmt.Errorf("read hash: %w", err)
	}

	// Block is already in sync, skip sending
	if bytes.Equal(block.Hash, serverHash) {
		job := BlockJob{blockIdx: block.BlockIdx, data: block.Data, readedBytes: len(block.Data)}
		printStats(job, "-", 0, 0)
		return nil
	}

	// Handle zero blocks - send only Msg, NO data (sparse file optimization)
	if block.IsZero {
		msg2, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   block.BlockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   0,
			Compressed: false,
			Zero:       true,
			Done:       false,
		})
		if err1 != nil {
			return fmt.Errorf("pack zero: %w", err1)
		}
		if err := connWrite(conn, msg2); err != nil {
			return fmt.Errorf("send zero: %w", err)
		}
		job := BlockJob{blockIdx: block.BlockIdx, data: nil, readedBytes: int(blockSize)}
		printStats(job, ".", 1, 0)
		return nil
	}

	// Determine what to send for non-zero blocks
	var dataToSend []byte
	var compressedFlag bool

	if noCompress || !block.UseCompressed {
		compressedFlag = false
		dataToSend = block.Data
	} else {
		compressedFlag = true
		dataToSend = block.Compressed
	}

	if len(dataToSend) == 0 {
		return fmt.Errorf("no data for block %d", block.BlockIdx)
	}

	msg2, err1 := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   block.BlockIdx,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   uint32(len(dataToSend)),
		Compressed: compressedFlag,
		Zero:       false,
		Done:       false,
	})
	if err1 != nil {
		return fmt.Errorf("pack data: %w", err1)
	}

	if err := connWrite(conn, msg2); err != nil {
		return fmt.Errorf("send data header: %w", err)
	}
	if err := connWrite(conn, dataToSend); err != nil {
		return fmt.Errorf("send data payload: %w", err)
	}

	job := BlockJob{blockIdx: block.BlockIdx, data: block.Data, readedBytes: len(block.Data)}
	if compressedFlag {
		printStats(job, "c", 1, uint32(len(dataToSend)))
	} else {
		printStats(job, "w", 1, uint32(len(dataToSend)))
	}
	return nil
}

// startClientDownload connects to server and downloads file blocks
func startClientDownload(file *os.File, serverAddress string, skipIdx uint32, blockSize uint32, noCompress bool, workers int) {
	Log("startClientDownload()\n")

	// Resolve server address once
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		Err("resolving: %s\n", err.Error())
		return
	}

	// Connect to server
	conn := NewAutoReconnectTCP(saddr)
	defer conn.Close()

	// Send download request to server
	magicBytes := stringToFixedSizeArray(magicHead)
	msg, err1 := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   0,
		BlockSize:  blockSize,
		FileSize:   0,
		DataSize:   0,
		Compressed: false,
		Zero:       false,
		Done:       false,
	})
	if err1 != nil {
		Log("cant pack download request msg-> %s\n", err1)
		return
	}

	n, err2 := conn.Write(msg)
	if err2 != nil && err2 != io.EOF {
		Log("\t- error writing download request: [%d] %s\n", n, err2.Error())
		return
	}

	// Read file metadata from server
	msgBuf := make([]byte, binary.Size(Msg{}))
	_, err = io.ReadFull(conn, msgBuf)
	if err != nil {
		Log("Error reading metadata from server: %s\n", err.Error())
		return
	}

	metaMsg, err3 := unpack(msgBuf)
	if err3 != nil {
		Log("\t- unpack metadata failed: %s\n", err3)
		return
	}

	fileSize := metaMsg.FileSize
	lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

	Log("remote file size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	// Truncate local file to match remote size
	truncateIfRegularFile(file, fileSize)

	// Start receiving blocks
	Log("start downloading from server\n")
	filebuf := make([]byte, blockSize)

	for blockIdx := uint32(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {
		// Request block from server
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   0,
			Compressed: false,
			Zero:       false,
			Done:       false,
		})
		if err1 != nil {
			Log("cant pack block request msg-> %s\n", err1)
			return
		}

		n, err2 := conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Log("\t- error writing block request: [%d] %s\n", n, err2.Error())
			return
		}

		// Read response
		_, err = io.ReadFull(conn, msgBuf)
		if err != nil {
			Log("Error reading block response: %s\n", err.Error())
			return
		}

		blockMsg, err4 := unpack(msgBuf)
		if err4 != nil {
			Log("\t- unpack block response failed: %s\n", err4)
			return
		}

		if blockMsg.Done {
			Log("\ndownload DONE\n\n")
			break
		}

		offset := int64(blockIdx) * int64(blockSize)

		// Handle zero blocks (DataSize=0, Zero=true) - no data sent, preserve sparse hole
		if blockMsg.Zero && blockMsg.DataSize == 0 {
			// Don't write anything - file is pre-truncated, creates sparse hole
			if debug {
				Log("\t- zero block at %d, skipping write\n", blockIdx)
			}
		}

		if blockMsg.DataSize > 0 {
			// Read block data
			_, err1 := io.ReadFull(conn, filebuf[:blockMsg.DataSize])
			if err1 != nil {
				Log("\t- error reading block data: %s\n", err1)
				return
			}

			if blockMsg.Compressed {
				decompressed, err := decompressData(filebuf[:blockMsg.DataSize])
				if err != nil {
					Log("\t- error decompressing: %s\n", err.Error())
					break
				}
				n, err2 := file.WriteAt(decompressed, offset)
				if err2 != nil && err2 != io.EOF {
					Log("\t- error writing decompressed block: [%d] %s\n", n, err2.Error())
					break
				}
			} else {
				n, err2 := file.WriteAt(filebuf[:blockMsg.DataSize], offset)
				if err2 != nil && err2 != io.EOF {
					Log("\t- error writing block: [%d] %s\n", n, err2.Error())
					break
				}
			}
		}

		percent := 100 * float64(blockIdx) / float64(lastBlockNum)
		Log("downloaded block %d/%d (%0.2f%%)\r", blockIdx, lastBlockNum, percent)
	}

	Log("\ndownload complete\n")
}
