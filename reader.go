package main

import (
	"io"
	"os"
	"sync"
)

// Reader buffer pool for reusing read buffers
// Pool buffers are locked in memory on creation to prevent swap
// Buffers use huge pages (MADV_HUGEPAGE) to reduce TLB misses
var readerBufPool = sync.Pool{
	New: func() interface{} {
		// Pre-allocate with 10MB capacity for typical block size
		buf := make([]byte, 0, 10*1024*1024)
		// Lock the full capacity to prevent swap
		TryLockBuffer(buf[:cap(buf)])
		// Advise kernel to use huge pages (2MB) for better TLB utilization
		AdviseHugePage(buf[:cap(buf)])
		return buf
	},
}

// getReaderBuf gets a buffer from the pool or allocates a new one
func getReaderBuf(size uint32) []byte {
	buf := readerBufPool.Get().([]byte)
	if cap(buf) < int(size) {
		// Pool buffer too small, allocate new one (not returned to pool)
		// New buffers are automatically locked
		newBuf := make([]byte, size)
		TryLockBuffer(newBuf)
		return newBuf
	}
	return buf[:size]
}

// putReaderBuf returns a buffer to the pool if it's appropriately sized
func putReaderBuf(buf []byte) {
	if cap(buf) <= 10*1024*1024 {
		readerBufPool.Put(buf[:0])
	}
	// Large buffers are GC'd instead of polluting the pool
	// Note: munlock happens automatically when GC frees the memory
}

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
	lastBlock := uint32((fileSize - 1) / uint64(blockSize))

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

		buf := getReaderBuf(sr.blockSize)
		defer putReaderBuf(buf)

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
