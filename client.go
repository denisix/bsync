package main

import (
	"bytes"
	"encoding/binary"
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

// processBlockJob handles hashing, compressing, and sending a block using a persistent connection
func processBlockJob(conn *AutoReconnectTCP, job BlockJob, blockSize uint32, fileSize uint64, noCompress bool, checksumCache *ChecksumCache) {
	magicBytes := stringToFixedSizeArray(magicHead)

	msg, err1 := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   job.blockIdx,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   0,
		Compressed: false,
		Zero:       false,
		Done:       false,
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

	hash := checksumCache.WaitFor(job.blockIdx)

	// buffer to get data
	serverHash := make([]byte, len(hash))
	_, err := conn.Read(serverHash)
	if err != nil {
		Log("[client]\t- read data from net failed: %s\n", err.Error())
		return
	}

	// Block is already in sync, or it's a known zero block — skip sending
	if bytes.Equal(hash, serverHash) {
		printStats(job, "-", 0, 0)
		return
	}

	// Block zero, just send that it's zero
	if bytes.Equal(hash, zeroBlockHash) {
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   job.blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   uint32(job.readedBytes),
			Compressed: false,
			Zero:       true,
			Done:       false,
		})
		if err1 != nil {
			Log("\t- cant pack msg-> %s\n", err1)
			return
		}
		n, err2 := conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Err("\t- error writing net: [%d] %s\n", n, err2.Error())
			return
		}
		n, err3 := conn.Write(job.data)
		if err3 != nil && err3 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err3.Error())
			return
		}
		printStats(job, ".", 1, 0)
		return
	}

	if noCompress {
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   job.blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   uint32(job.readedBytes),
			Compressed: false,
			Zero:       false,
			Done:       false,
		})
		if err1 != nil {
			Log("\t- cant pack msg-> %s\n", err1)
			return
		}
		n, err2 := conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err2.Error())
			return
		}
		if debug {
			Log("\t- send nocompress data [%d] %d: %x\n", job.blockIdx, job.readedBytes, hash)
		}

		n, err3 := conn.Write(job.data)
		if err3 != nil && err3 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err3.Error())
			return
		}
		printStats(job, "w", 1, uint32(job.readedBytes))
		return
	}

	// else compress block:
	compBuf, err := compressData(job.data)
	if err != nil {
		Log("Error: compressing data: %s\n", err)
		return
	}

	compressedBytes := uint32(len(compBuf))

	if compressedBytes < blockSize {
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   job.blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   compressedBytes,
			Compressed: true,
			Zero:       false,
			Done:       false,
		})
		if err1 != nil {
			Log("\t- cant pack msg-> %s\n", err1)
			return
		}
		n, err2 := conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err2.Error())
			return
		}
		n, err3 := conn.Write(compBuf)
		if err3 != nil && err3 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err3.Error())
			return
		}
		printStats(job, "c", 1, compressedBytes)
	} else {
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   job.blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   uint32(job.readedBytes),
			Compressed: false,
			Zero:       false,
			Done:       false,
		})
		if err1 != nil {
			Log("\t- cant pack msg-> %s\n", err1)
			return
		}
		n, err2 := conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err2.Error())
			return
		}
		n, err3 := conn.Write(job.data)
		if err3 != nil && err3 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err3.Error())
			return
		}
		printStats(job, "w", 1, uint32(job.readedBytes))
	}
}

// startClient launches threadsCount workers, each with a persistent connection, and pushes file blocks to a jobs channel
func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache, workers int) {
	Log("startClient()\n")
	lastBlockNum = uint32(fileSize / uint64(blockSize))
	Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	jobs := make(chan BlockJob, workers*2)
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

	// Start worker goroutines
	Log("starting %d workers\n", workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(saddr *net.TCPAddr) {
			defer wg.Done()
			conn := NewAutoReconnectTCP(saddr)
			defer conn.Close()
			for job := range jobs {
				processBlockJob(conn, job, blockSize, fileSize, noCompress, checksumCache)
			}
		}(saddr)
	}

	// Producer: read file sequentially and push jobs
	Log("start reading source\n")
	buf := make([]byte, blockSize)
	for blockIdx := uint32(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {
		offset := int64(blockIdx) * int64(blockSize)
		readedBytes, err := file.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			Err("\t- error reading file: %s\n", err.Error())
			break
		}
		dataCopy := make([]byte, readedBytes)
		copy(dataCopy, buf[:readedBytes])
		jobs <- BlockJob{blockIdx, dataCopy, readedBytes}
	}
	close(jobs)

	// send DONE
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

	Log("DONE, waiting for the workers\n")
	wg.Wait()

	Log("\nDONE, exiting..\n\n")
	time.Sleep(2 * time.Second)
	return
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
	lastBlockNum := uint32(fileSize / uint64(blockSize))

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

		if blockMsg.DataSize > 0 {
			// Read block data
			_, err1 := io.ReadFull(conn, filebuf[:blockMsg.DataSize])
			if err1 != nil {
				Log("\t- error reading block data: %s\n", err1)
				return
			}

			if blockMsg.Zero {
				zero := make([]byte, blockMsg.DataSize)
				n, err := file.WriteAt(zero, offset)
				if err != nil && err != io.EOF {
					Log("\t- error writing zero block: [%d] %s\n", n, err.Error())
					break
				}
			} else if blockMsg.Compressed {
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
