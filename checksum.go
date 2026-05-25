package main

import (
	"hash"
	"hash/fnv"
	"sync"
	"os"
	"io"
)

var zeroBlockHash = make([]byte, 16) // FNV-128a length

// Hasher pool for better performance
var hasherPool = sync.Pool{
	New: func() interface{} {
		return fnv.New128a()
	},
}

// PrecomputedBlock contains hash and compressed data
type PrecomputedBlock struct {
	BlockIdx      uint32
	Hash          []byte
	Data          []byte // Original data
	Compressed    []byte // Compressed data (if beneficial)
	UseCompressed bool   // Whether to use compressed version
	IsZero        bool
}

// compute FNV1a checksum
func checksum(data []byte) []byte {
	hasher := hasherPool.Get().(hash.Hash)
	defer hasherPool.Put(hasher)
	hasher.Reset()
	hasher.Write(data)
	return hasher.Sum(nil)
}

type ChecksumCache struct {
	data         map[uint32][]byte
	ready        map[uint32]bool
	mu           sync.Mutex
	cond         *sync.Cond
	maxId        uint32
	noPrecompute bool // if true, WaitFor returns zero hash immediately (empty destination)
}

func NewChecksumCache(maxId uint32) *ChecksumCache {
	// Pre-allocate map capacity to avoid resizing during transfer
	cc := &ChecksumCache{
		data:   make(map[uint32][]byte, maxId+1),
		ready:  make(map[uint32]bool, maxId+1),
		maxId: maxId,
	}
	cc.cond = sync.NewCond(&cc.mu)
	return cc
}

func (cc *ChecksumCache) Set(idx uint32, checksum []byte) {
	cc.mu.Lock()
  defer cc.mu.Unlock()
	cc.data[idx] = checksum
	cc.ready[idx] = true
	cc.cond.Broadcast()
}

func (cc *ChecksumCache) WaitFor(idx uint32) []byte {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if idx > cc.maxId || cc.noPrecompute {
		return make([]byte, 16) // all-zero hash (out of bounds or no precomputation)
	}

	for !cc.ready[idx] {
		cc.cond.Wait()
	}

	return cc.data[idx]
}

// Delete removes a cached checksum to free memory (optional cleanup)
func (cc *ChecksumCache) Delete(idx uint32) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.data, idx)
	delete(cc.ready, idx)
}

// precomputeChecksumsParallel uses channel-based reader for parallel checksum + compression
func precomputeChecksumsParallel(reader *SequentialReader, cache *ChecksumCache, precompressedChan chan<- PrecomputedBlock, workers int, noCompress bool) {
	Log("checksums: start computing with %d parallel workers..\n", workers)

	var wg sync.WaitGroup

	// Start worker goroutines
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for block := range reader.Blocks() {
				var hash []byte
				var isZero bool
				var compressed []byte
				var useCompressed bool
				var originalData []byte

				if isZeroBlock(block.Data) {
					hash = zeroBlockHash
					isZero = true
				} else {
					hash = checksum(block.Data)
					isZero = false

					if noCompress {
						// -n: never compress; keep the raw block to send.
						compressed = nil
						useCompressed = false
						originalData = block.Data
					} else if comp, err := compressData(block.Data); err == nil && len(comp) < len(block.Data) {
						compressed = comp
						useCompressed = true
						// Don't need original data if using compressed - save memory
						originalData = nil
					} else {
						compressed = nil
						useCompressed = false
						originalData = block.Data
					}
				}

				// Store hash in cache for processBlockJob to use
				cache.Set(block.BlockIdx, hash)

				// Send precomputed block to transfer workers
				precompressedChan <- PrecomputedBlock{
					BlockIdx:      block.BlockIdx,
					Hash:          hash,
					Data:          originalData,
					Compressed:    compressed,
					UseCompressed: useCompressed,
					IsZero:        isZero,
				}
			}
		}()
	}

	// Close channel when all workers done
	go func() {
		wg.Wait()
		close(precompressedChan)
	}()
}

// Note: precomputeChecksumsSequential removed - use precomputeChecksumsParallel instead


// precomputeChecksums computes block hashes for the destination/source file.
// To stay HDD-friendly the disk is read by a SINGLE sequential goroutine
// (ascending offsets, one continuous stream); the CPU-bound hashing is then
// fanned out to `workers` goroutines. Reads no longer scatter across the
// platter the way a per-worker ReadAt pool did.
func precomputeChecksums(file *os.File, blockSize uint32, lastBlockNum uint32, cache *ChecksumCache, skipIdx uint32, workers int) {
	Log("checksums: start computing with %d workers (sequential read)..\n", workers)

	jobs := make(chan BlockData, workers*2)
	var wg sync.WaitGroup

	// CPU-bound hashing workers (parallel)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for blk := range jobs {
				if isZeroBlock(blk.Data) {
					cache.Set(blk.BlockIdx, zeroBlockHash)
					continue
				}
				cache.Set(blk.BlockIdx, checksum(blk.Data))
			}
		}()
	}

	// Single sequential reader: one continuous pass over the device.
	buf := make([]byte, blockSize)
	for idx := skipIdx; idx <= lastBlockNum; idx++ {
		offset := int64(idx) * int64(blockSize)
		n, err := file.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			cache.Set(idx, []byte("ERR"))
			continue
		}
		if n == 0 && err == io.EOF {
			cache.Set(idx, []byte("EOF"))
			continue
		}
		// Copy out the slice before handing to a worker (buf is reused).
		dataCopy := make([]byte, n)
		copy(dataCopy, buf[:n])
		jobs <- BlockData{BlockIdx: idx, Data: dataCopy}
	}
	close(jobs)
	wg.Wait()
}

