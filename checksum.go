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

	// Tree-mode fields; zero values preserve today's single-file semantics
	// exactly, so the single-file path (precomputeChecksumsParallel) never
	// sets them. A stream mixing blocks from different files has no single
	// global answer for FileSize/BlockSize the way single-file mode's
	// package globals provide, so each block carries its own.
	FileID    uint32
	FileSize  uint64
	BlockSize uint32
	Last      bool   // true for a file's final block
	release   func() // returns Data's underlying buffer to its pool/budget; nil if Data is nil or unpooled
}

// Release returns Data's underlying buffer, if any. Safe to call even when
// release is nil (nothing to do) or Data was never set (compressed-away or
// zero blocks). The transfer worker that actually sends this block must
// call it exactly once, after it's done needing Data — never earlier, and
// never twice.
func (pb *PrecomputedBlock) Release() {
	if pb.release != nil {
		pb.release()
	}
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

				// block.Data is a buffer checked out of readerBufPool
				// (reader.go). It must be returned via putReaderBuf exactly
				// once: either here, immediately, when this goroutine has no
				// further use for it, or later by the transfer worker (see
				// client.go) once it has finished sending it as
				// PrecomputedBlock.Data. Previously nothing ever returned it
				// in either case, so every block cost a fresh 10MB
				// allocation + mlock + MADV_HUGEPAGE instead of a pool
				// checkout.
				if isZeroBlock(block.Data) {
					hash = zeroBlockHash
					isZero = true
					putReaderBuf(block.Data)
				} else {
					hash = checksum(block.Data)
					isZero = false

					if noCompress {
						// -n: never compress; keep the raw block to send.
						// Released by the transfer worker after it sends it.
						compressed = nil
						useCompressed = false
						originalData = block.Data
					} else if comp, err := compressData(block.Data); err == nil && len(comp) < len(block.Data) {
						compressed = comp
						useCompressed = true
						// The compressed copy is sent instead; the raw
						// buffer has no further use.
						originalData = nil
						putReaderBuf(block.Data)
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

// precomputeTreeBlocks is precomputeChecksumsParallel's directory-tree
// sibling: it drains one TreeReader (spanning every file in a tree, not
// just one) and produces PrecomputedBlock values with FileID, FileSize,
// BlockSize, and Last populated. sess supplies each block's per-file
// FileSize/BlockSize via FileByID (a mixed stream has no single global
// answer for either); for a block whose raw buffer is kept rather than
// compressed away, sess's absence isn't fatal (FileSize/BlockSize just stay
// zero), but the release closure is always threaded through regardless, so
// the eventual transfer worker — not this function — decides when the
// buffer (and, in tree mode, its byte-budget reservation) is actually freed.
//
// Unlike precomputeChecksumsParallel, out is NOT closed at a file boundary
// — only once, when every hashing worker has drained the TreeReader dry
// (its channel closes after the last file in the tree finishes).
func precomputeTreeBlocks(tr *TreeReader, sess *SyncSession, out chan<- PrecomputedBlock, workers int, noCompress bool) {
	Log("checksums: start computing (tree mode) with %d parallel workers..\n", workers)

	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for block := range tr.Blocks() {
				var fileSize uint64
				var fileBlockSize uint32
				if sess != nil {
					if fe := sess.FileByID(block.FileID); fe != nil {
						fileSize = fe.Size
						fileBlockSize = fe.BlockSize
					}
				}

				var hash []byte
				var isZero bool
				var compressed []byte
				var useCompressed bool
				var originalData []byte
				var release func()

				if isZeroBlock(block.Data) {
					hash = zeroBlockHash
					isZero = true
					if block.release != nil {
						block.release()
					}
				} else {
					hash = checksum(block.Data)
					isZero = false

					if noCompress {
						// -n: never compress; keep the raw block to send.
						originalData = block.Data
						release = block.release
					} else if comp, err := compressData(block.Data); err == nil && len(comp) < len(block.Data) {
						compressed = comp
						useCompressed = true
						// The compressed copy is sent instead; the raw
						// buffer has no further use.
						originalData = nil
						if block.release != nil {
							block.release()
						}
					} else {
						originalData = block.Data
						release = block.release
					}
				}

				out <- PrecomputedBlock{
					BlockIdx:      block.BlockIdx,
					Hash:          hash,
					Data:          originalData,
					Compressed:    compressed,
					UseCompressed: useCompressed,
					IsZero:        isZero,
					FileID:        block.FileID,
					FileSize:      fileSize,
					BlockSize:     fileBlockSize,
					Last:          block.Last,
					release:       release,
				}
			}
		}()
	}

	// Close channel when all workers done
	go func() {
		wg.Wait()
		close(out)
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

