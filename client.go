package main

import (
	"bytes"
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
	} else {
		msg, err1 := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   job.blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   uint32(job.readedBytes),
			Compressed: false,
			Zero:       false,
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
	}
}

// startClient launches threadsCount workers, each with a persistent connection, and pushes file blocks to a jobs channel
func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache, workers int) {
	Log("startClient()\n")
	lastBlockNum := uint32(fileSize / uint64(blockSize))
	Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	jobs := make(chan BlockJob, workers*2)
	var wg sync.WaitGroup

	// Resolve server address once
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		Log("Error: resolving: %s\n", err.Error())
		return
	}

	Log("starting %d workers\n", workers)
	// Start worker goroutines
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
			Log("\t- error reading file: %s\n", err.Error())
			break
		}
		dataCopy := make([]byte, readedBytes)
		copy(dataCopy, buf[:readedBytes])
		jobs <- BlockJob{blockIdx, dataCopy, readedBytes}
	}
	close(jobs)

	Log("DONE, waiting for the workers\n")
	wg.Wait()

	Log("\nDONE, exiting..\n\n")
	time.Sleep(2 * time.Second)
	return
}
