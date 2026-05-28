package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
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

	// Visualization
	cliVisualizer *Visualizer
)

// ETA, stats
func printStats(job BlockJob, indicator string, diff, bytes uint32) {
	// Skip progress output when visualizer is active
	if visualEnabled() {
		return
	}

	mu.Lock()
	diffs = diffs + diff
	if bytes > 0 {
		currentBlockSize := atomic.LoadUint32(&blockSize)
		totOrigSize = totOrigSize + uint64(currentBlockSize)
		totCompSize = totCompSize + uint64(bytes)
	}
	// Snapshot shared counters while holding the lock; reading them below
	// without the lock would race with other workers' printStats calls.
	localDiffs := diffs
	localComp := totCompSize
	localOrig := totOrigSize
	mu.Unlock()

	currentBlockSize := atomic.LoadUint32(&blockSize)
	blockMb := float64(currentBlockSize) / float64(mb1)

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

	percent := 100.0
	if lastBlockNum > 0 {
		percent = 100 * float64(job.blockIdx) / float64(lastBlockNum)
	}
	ratio := 100.0
	if localOrig > 0 {
		ratio = 100 * float64(localComp) / float64(localOrig)
	}

	Log("block %d/%d (%0.2f%%) [%s] size=%d ratio=%0.2f %0.2f MB/s ETA=%d %s diffs=%d\r", job.blockIdx, lastBlockNum, percent, indicator, v_fileSize, ratio, mbs, eta, etaUnit, localDiffs)
}

// startClient launches workers with persistent connections and uploads blocks.
func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache, workers int) error {
	Log("startClient()\n")
	if blockSize == 0 {
		return fmt.Errorf("block size must be at least 1")
	}
	if workers < 1 {
		return fmt.Errorf("worker count must be at least 1")
	}
	if fileSize > 0 {
		lastBlockNum = uint32((fileSize - 1) / uint64(blockSize))
	} else {
		lastBlockNum = 0
	}
	Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	// Initialize visualizer with total block count
	if visualEnabled() {
		cliVisualizer = NewVisualizer(lastBlockNum + 1)
		cliVisualizer.renderInitial()
	}

	var wg sync.WaitGroup
	t0 = time.Now()
	v_skipIdx = skipIdx
	v_fileSize = fileSize

	// Resolve server address once
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		return fmt.Errorf("resolving: %w", err)
	}

	// Channel for precomputed blocks (hash + compressed data)
	precompressedChan := make(chan PrecomputedBlock, workers*2)
	if fileSize > 0 {
		reader := NewSequentialReader(file, blockSize, fileSize, skipIdx, prefetchDepth)
		reader.Start()
		go precomputeChecksumsParallel(reader, checksumCache, precompressedChan, workers, noCompress)
	} else {
		close(precompressedChan)
	}

	// Start worker goroutines for network transfer
	Log("starting %d transfer workers\n", workers)
	errChan := make(chan error, 1)
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
					if cliVisualizer != nil {
						cliVisualizer.UpdateBlockStatus(block.BlockIdx, StatusProcessing)
					}
					if lastErr = processPrecomputedBlock(conn, block, blockSize, fileSize, noCompress, checksumCache); lastErr == nil {
						break
					}
				}
				if lastErr != nil {
					Log("block %d: failed after %d retries: %v\n", block.BlockIdx, maxRetries, lastErr)
					select {
					case errChan <- fmt.Errorf("block %d: %w", block.BlockIdx, lastErr):
					default:
					}
				}
			}
		}(saddr)
	}

	Log("DONE, waiting for the workers\n")
	wg.Wait()
	select {
	case err := <-errChan:
		return err
	default:
	}

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
		return fmt.Errorf("pack done: %w", err1)
	}

	if err := connWrite(conn, msg); err != nil {
		return fmt.Errorf("send done: %w", err)
	}

	Log("\nDONE, exiting..\n\n")
	if cliVisualizer != nil {
		cliVisualizer.RenderFinal()
	}
	// Brief margin to flush the DONE message; the caller's sshCmd.Wait()
	// already blocks until the server has fully finished.
	time.Sleep(doneFlushDelay)
	return nil
}

func waitForBlockAck(conn io.Reader, blockIdx uint32) error {
	buf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	ack, err := unpack(buf)
	if err != nil {
		return err
	}
	if ack.MagicHead != stringToFixedSizeArray(magicHead) || ack.BlockIdx != blockIdx ||
		ack.Done || ack.Zero || ack.Compressed || ack.DataSize != 0 {
		return fmt.Errorf("invalid acknowledgement for block %d", blockIdx)
	}
	return nil
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
		if cliVisualizer != nil {
			cliVisualizer.UpdateBlockStatus(block.BlockIdx, StatusSkipped)
		}
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
		if err := waitForBlockAck(conn, block.BlockIdx); err != nil {
			return fmt.Errorf("ack zero: %w", err)
		}
		job := BlockJob{blockIdx: block.BlockIdx, data: nil, readedBytes: int(blockSize)}
		printStats(job, ".", 1, 0)
		if cliVisualizer != nil {
			cliVisualizer.UpdateBlockStatus(block.BlockIdx, StatusZero)
		}
		return nil
	}

	// Determine what to send for non-zero blocks
	var dataToSend []byte
	var compressedFlag bool

	if noCompress || !block.UseCompressed {
		compressedFlag = false
		// Raw block. compressData (the compressed path) is where encryption
		// normally happens, so encrypt here to keep -n + -e working.
		if IsEncryptionEnabled() {
			dataToSend = encryptBlock(block.Data)
		} else {
			dataToSend = block.Data
		}
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
	if err := waitForBlockAck(conn, block.BlockIdx); err != nil {
		return fmt.Errorf("ack data: %w", err)
	}

	job := BlockJob{blockIdx: block.BlockIdx, data: block.Data, readedBytes: len(block.Data)}
	if compressedFlag {
		printStats(job, "c", 1, uint32(len(dataToSend)))
		if cliVisualizer != nil {
			cliVisualizer.UpdateBlockStatus(block.BlockIdx, StatusCompressed)
		}
	} else {
		printStats(job, "w", 1, uint32(len(dataToSend)))
		if cliVisualizer != nil {
			cliVisualizer.UpdateBlockStatus(block.BlockIdx, StatusRaw)
		}
	}
	return nil
}

// startClientDownload connects to server and downloads file blocks.
func startClientDownload(file *os.File, serverAddress string, skipIdx uint32, blockSize uint32, noCompress bool, workers int) error {
	Log("startClientDownload()\n")
	if blockSize == 0 {
		return fmt.Errorf("block size must be at least 1")
	}
	if workers < 1 {
		return fmt.Errorf("worker count must be at least 1")
	}

	// Resolve server address once
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		return fmt.Errorf("resolving: %w", err)
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
		return fmt.Errorf("pack download request: %w", err1)
	}

	if err := connWrite(conn, msg); err != nil {
		return fmt.Errorf("send download request: %w", err)
	}

	// Read file metadata from server
	msgBuf := make([]byte, binary.Size(Msg{}))
	_, err = io.ReadFull(conn, msgBuf)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}

	metaMsg, err3 := unpack(msgBuf)
	if err3 != nil {
		return fmt.Errorf("unpack metadata: %w", err3)
	}
	if metaMsg.MagicHead != magicBytes || metaMsg.BlockSize == 0 ||
		metaMsg.DataSize != 0 || metaMsg.Compressed || metaMsg.Zero || metaMsg.Done {
		return fmt.Errorf("invalid metadata from server")
	}

	fileSize := metaMsg.FileSize
	blockSize = metaMsg.BlockSize
	truncateIfRegularFile(file, fileSize)
	if fileSize == 0 {
		Log("remote file is empty, nothing to download\n")
		return nil
	}
	lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

	Log("remote file size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	var downloadVisualizer *Visualizer
	if visualEnabled() {
		downloadVisualizer = NewVisualizer(lastBlockNum + 1)
		downloadVisualizer.renderInitial()
		defer downloadVisualizer.RenderFinal()
	}

	// Note: Preallocation skipped - using direct block devices (/dev/sda)
	// which have fixed size and don't support fallocate

	// Start receiving blocks. Reserve room for encryption overhead (an
	// encrypted, uncompressed block is blockSize+encryptOverhead on the wire).
	Log("start downloading from server\n")
	filebuf := make([]byte, blockSize+encryptOverhead)

	for blockIdx := uint32(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {
		if downloadVisualizer != nil {
			downloadVisualizer.UpdateBlockStatus(blockIdx, StatusProcessing)
		}
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
			return fmt.Errorf("pack block request: %w", err1)
		}

		if err := connWrite(conn, msg); err != nil {
			return fmt.Errorf("send block request: %w", err)
		}

		// Read response
		_, err = io.ReadFull(conn, msgBuf)
		if err != nil {
			return fmt.Errorf("read block response: %w", err)
		}

		blockMsg, err4 := unpack(msgBuf)
		if err4 != nil {
			return fmt.Errorf("unpack block response: %w", err4)
		}

		if blockMsg.Done {
			return fmt.Errorf("premature completion while awaiting block %d", blockIdx)
		}
		if blockMsg.MagicHead != magicBytes || blockMsg.BlockIdx != blockIdx ||
			blockMsg.BlockSize != blockSize || blockMsg.FileSize != fileSize ||
			blockMsg.DataSize > uint32(len(filebuf)) ||
			(blockMsg.Zero && (blockMsg.DataSize != 0 || blockMsg.Compressed)) ||
			(!blockMsg.Zero && blockMsg.DataSize == 0) {
			return fmt.Errorf("invalid block response for block %d", blockIdx)
		}

		offset := int64(blockIdx) * int64(blockSize)
		expectedSize := blockPayloadSize(fileSize, blockSize, blockIdx)

		// A zero notification must clear old destination data; an existing range
		// cannot be assumed to be a sparse hole.
		if blockMsg.Zero && blockMsg.DataSize == 0 {
			n, err := file.WriteAt(getZeroBuf(int(expectedSize)), offset)
			if err != nil {
				return fmt.Errorf("write zero block %d: %w", blockIdx, err)
			}
			if uint32(n) != expectedSize {
				return fmt.Errorf("write zero block %d: %w", blockIdx, io.ErrShortWrite)
			}
			if downloadVisualizer != nil {
				downloadVisualizer.UpdateBlockStatus(blockIdx, StatusZero)
			}
		}

		if blockMsg.DataSize > 0 {
			// Read block data
			_, err1 := io.ReadFull(conn, filebuf[:blockMsg.DataSize])
			if err1 != nil {
				return fmt.Errorf("read block %d data: %w", blockIdx, err1)
			}

			if blockMsg.Compressed {
				decompressed, err := decompressData(filebuf[:blockMsg.DataSize])
				if err != nil {
					return fmt.Errorf("decompress block %d: %w", blockIdx, err)
				}
				if uint32(len(decompressed)) != expectedSize {
					return fmt.Errorf("block %d has invalid decompressed size %d", blockIdx, len(decompressed))
				}
				n, err2 := file.WriteAt(decompressed, offset)
				if err2 != nil {
					return fmt.Errorf("write block %d: %w", blockIdx, err2)
				}
				if n != len(decompressed) {
					return fmt.Errorf("write block %d: %w", blockIdx, io.ErrShortWrite)
				}
				if downloadVisualizer != nil {
					downloadVisualizer.UpdateBlockStatus(blockIdx, StatusCompressed)
				}
			} else {
				// Uncompressed payload. With -e the raw block is encrypted on
				// the wire, so decrypt before writing.
				out := filebuf[:blockMsg.DataSize]
				if IsEncryptionEnabled() {
					dec, err := decryptBlock(out)
					if err != nil {
						return fmt.Errorf("decrypt block %d: %w", blockIdx, err)
					}
					out = dec
				}
				if uint32(len(out)) != expectedSize {
					return fmt.Errorf("block %d has invalid payload size %d", blockIdx, len(out))
				}
				n, err2 := file.WriteAt(out, offset)
				if err2 != nil {
					return fmt.Errorf("write block %d: %w", blockIdx, err2)
				}
				if n != len(out) {
					return fmt.Errorf("write block %d: %w", blockIdx, io.ErrShortWrite)
				}
				if downloadVisualizer != nil {
					downloadVisualizer.UpdateBlockStatus(blockIdx, StatusRaw)
				}
			}
		}

		percent := 100.0
		if lastBlockNum > 0 {
			percent = 100 * float64(blockIdx) / float64(lastBlockNum)
		}
		if !visualEnabled() {
			Log("downloaded block %d/%d (%0.2f%%)\r", blockIdx, lastBlockNum, percent)
		}
	}

	Log("\ndownload complete\n")
	return nil
}
