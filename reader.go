package main

import (
	"io"
	"os"
	"sync"
)

// BlockData represents a block with its index and data
type BlockData struct {
	BlockIdx uint32
	Data     []byte
}

// SequentialReader reads blocks sequentially and pushes to channel
type SequentialReader struct {
	file         *os.File
	blockSize    uint32
	lastBlockNum uint32
	skipIdx      uint32
	blockChan    chan BlockData
	wg           sync.WaitGroup
}

// NewSequentialReader creates a new sequential reader
func NewSequentialReader(file *os.File, blockSize uint32, fileSize uint64, skipIdx uint32, bufferAhead int) *SequentialReader {
	lastBlock := uint32(fileSize / uint64(blockSize))

	return &SequentialReader{
		file:         file,
		blockSize:    blockSize,
		lastBlockNum: lastBlock,
		skipIdx:      skipIdx,
		blockChan:    make(chan BlockData, bufferAhead),
	}
}

// Start begins reading blocks in the background
func (sr *SequentialReader) Start() {
	sr.wg.Add(1)
	go func() {
		defer sr.wg.Done()
		defer close(sr.blockChan)

		buf := make([]byte, sr.blockSize)
		for blockIdx := sr.skipIdx; blockIdx <= sr.lastBlockNum; blockIdx++ {
			// Read block sequentially
			offset := int64(blockIdx) * int64(sr.blockSize)
			n, err := sr.file.ReadAt(buf, offset)
			if err != nil && err != io.EOF {
				Log("sequential reader error reading block %d: %s\n", blockIdx, err)
				return
			}

			// Copy data and push to channel
			dataCopy := make([]byte, n)
			copy(dataCopy, buf[:n])

			sr.blockChan <- BlockData{BlockIdx: blockIdx, Data: dataCopy}
		}
	}()
}

// Blocks returns the channel for consumers
func (sr *SequentialReader) Blocks() <-chan BlockData {
	return sr.blockChan
}

// Wait waits for the reader to finish
func (sr *SequentialReader) Wait() {
	sr.wg.Wait()
}
