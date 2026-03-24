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
	data  map[uint32][]byte
	ready map[uint32]bool
	mu    sync.Mutex
	cond  *sync.Cond
	maxId uint32
}

func NewChecksumCache(maxId uint32) *ChecksumCache {
	cc := &ChecksumCache{
		data:   make(map[uint32][]byte),
		ready:  make(map[uint32]bool),
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

	if idx > cc.maxId {
		// Out of bounds — return placeholder or nil
		return make([]byte, 16) // all-zero hash
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
func precomputeChecksumsParallel(reader *SequentialReader, cache *ChecksumCache, precompressedChan chan<- PrecomputedBlock, workers int) {
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

					// Compress the block
					comp, err := compressData(block.Data)
					if err == nil && len(comp) < len(block.Data) {
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


func precomputeChecksums(file *os.File, blockSize uint32, lastBlockNum uint32, cache *ChecksumCache, skipIdx uint32, workers int) {
	Log("checksums: start computing with %d workers..\n", workers)

	jobs := make(chan uint32, workers*2)
	var wg sync.WaitGroup

	// Start worker goroutines
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, blockSize)
			for idx := range jobs {
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

				if isZeroBlock(buf[:n]) {
					cache.Set(idx, zeroBlockHash)
					continue
				}

				hash := checksum(buf[:n])
				cache.Set(idx, hash)
			}
		}()
	}

	// Distribute jobs
	for idx := skipIdx; idx <= lastBlockNum; idx++ {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()
}

