package main

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"
)

var zeroBlockHash = make([]byte, 16) // FNV-128a length

// Hasher pool for better performance

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
// checksumSize is the digest length on the wire. Kept at 16 bytes across the
// switch from FNV-128a to SHA-256 so nothing that assumed that width has to
// change -- notably ChecksumCache's all-zero "no precomputation" hash below.
const checksumSize = 16

// checksum hashes one block. This is the per-byte cost paid on BOTH ends of
// every transfer -- the sender hashes each block it reads, and the receiver
// hashes the destination block it compares against -- which made the old
// FNV-128a the single largest CPU cost in the pipeline, not compression.
// Go's hash/fnv is byte-at-a-time with a 128-bit multiply per byte; measured
// on 4 MiB blocks:
//
//	FNV-128a       282 MB/s
//	BLAKE2b-128    826 MB/s
//	SHA-256      2,427 MB/s   <- hardware SHA-2 (ARMv8 / x86 SHA-NI)
//
// zstd compresses at ~4,350 MB/s on the same data, so hashing was roughly
// 15x more expensive per byte than the compression it feeds, and it is what
// flattened throughput as -w grew. SHA-256 is both ~8.6x faster here and
// cryptographically strong, unlike FNV; truncating to checksumSize keeps the
// wire format identical in width.
//
// sha256.Sum256 rather than a pooled hash.Hash: it is a single call with no
// allocation and no pool round trip, and there is no incremental hashing to
// support here -- every caller has the whole block already.
func checksum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:checksumSize]
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
		data:  make(map[uint32][]byte, maxId+1),
		ready: make(map[uint32]bool, maxId+1),
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
		return make([]byte, checksumSize) // all-zero hash (out of bounds or no precomputation)
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
					} else if len(block.Data) >= compressMinSize {
						if comp, err := compressData(block.Data); err == nil && len(comp) < len(block.Data) {
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
					} else {
						// Below compressMinSize: not worth even trying.
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
// affinityWorker returns the index of the ONE transfer worker that must
// receive every block of fileID, or -1 for "no affinity: use the shared
// FCFS channel", i.e. exactly the pre-affinity behavior.
//
// blocks is FileEntry.Blocks, the canonical per-file block count walkSource
// already computes once (walk.go: ceil(Size/BlockSize)) -- never
// recomputed here from FileSize/BlockSize, so there is only ever one place
// that can get the empty-file/cap-size edge cases wrong. blocks == 0 means
// "unknown" (no SyncSession, or an entry that never went through
// walkSource) and deliberately falls back to shared, so routing is never
// decided on incomplete information.
//
// The mapping is a pure function of fileID: no map, no counter, no lock.
// walkSource assigns dense sequential FileIDs in walk order (AddFile
// enforces this), so fileID % workers is exact round-robin over walk
// order, and every one of precomputeTreeBlocksRouted's concurrent hashing
// goroutines computes the identical answer for a given block independently.
//
// The threshold (blocks <= workers) is deliberately worker-count-relative:
// it's the regime where the old shared-only scheme could scatter a file
// across the most distinct workers relative to its own size (worst
// redundant-OpFileOpen-to-benefit ratio); a file with more blocks than
// workers already amortizes a rebind's fixed cost over many blocks and
// benefits more from real multi-connection parallelism, so it keeps today's
// cooperative behavior untouched.
func affinityWorker(fileID, blocks uint32, workers int) int {
	if workers <= 0 || blocks == 0 || blocks > uint32(workers) {
		return -1
	}
	return int(fileID % uint32(workers))
}

// precomputeTreeBlocks is precomputeTreeBlocksRouted with routing disabled:
// every block goes to out, exactly the pre-affinity behavior. Kept for
// callers that want one undifferentiated stream (including tests that
// assert arrival order there is not meaningful).
//
// Unlike precomputeChecksumsParallel, out is NOT closed at a file boundary
// — only once, when every hashing worker has drained the TreeReader dry
// (its channel closes after the last file in the tree finishes).
func precomputeTreeBlocks(tr *TreeReader, sess *SyncSession, out chan<- PrecomputedBlock, workers int, noCompress bool) {
	precomputeTreeBlocksRouted(tr, sess, out, nil, workers, noCompress)
}

// precomputeTreeBlocksRouted is precomputeTreeBlocks plus per-worker
// routing. dedicated[i] is transfer worker i's private channel; a file with
// few enough blocks (see affinityWorker) has ALL of its blocks sent to
// exactly one dedicated channel, so exactly one worker — and therefore
// exactly one connection — ever binds it, eliminating the redundant
// OpFileOpen/OpFileClose round trips other workers would otherwise pay for
// blocks of the same small file. Every other file goes to shared, which
// every worker drains FCFS, preserving today's multi-connection cooperation
// on large files. dedicated may be nil (or empty) to disable routing
// entirely — see precomputeTreeBlocks above.
//
// PRECONDITION: shared and every element of dedicated are DISTINCT channels
// this function exclusively owns closing. Aliasing any two of them would
// double-close and panic.
//
// The routing decision is made inline by the existing parallel hashing
// goroutines below, NOT by a separate router stage between this function
// and the transfer workers: a single serial router would be a new
// head-of-line blocking point — one full per-worker channel would stall
// the router's one send to it, which would stall every hashing goroutine
// feeding the router, including ones producing blocks destined for
// completely idle workers. Deciding routing inline lets each hashing
// goroutine block independently on whichever single channel it's sending
// to, without blocking any other hashing goroutine.
func precomputeTreeBlocksRouted(tr *TreeReader, sess *SyncSession, shared chan<- PrecomputedBlock, dedicated []chan<- PrecomputedBlock, workers int, noCompress bool) {
	Log("checksums: start computing (tree mode) with %d parallel workers..\n", workers)

	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for block := range tr.Blocks() {
				var fileSize uint64
				var fileBlockSize uint32
				var fileBlocks uint32
				if sess != nil {
					if fe := sess.FileByID(block.FileID); fe != nil {
						fileSize = fe.Size
						fileBlockSize = fe.BlockSize
						fileBlocks = fe.Blocks
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
					} else if len(block.Data) >= compressMinSize {
						if comp, err := compressData(block.Data); err == nil && len(comp) < len(block.Data) {
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
					} else {
						// Below compressMinSize: not worth even trying.
						originalData = block.Data
						release = block.release
					}
				}

				pb := PrecomputedBlock{
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

				dest := shared
				if w := affinityWorker(block.FileID, fileBlocks, len(dedicated)); w >= 0 {
					dest = dedicated[w]
				}
				dest <- pb
			}
		}()
	}

	// Close every channel when all workers are done -- one closer, one close
	// per channel, strictly after every possible sender has returned, so no
	// send can ever race a close.
	go func() {
		wg.Wait()
		close(shared)
		for _, ch := range dedicated {
			close(ch)
		}
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
