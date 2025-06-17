package main

import (
	"hash/fnv"
	"sync"
	"os"
	"io"
)

var zeroBlockHash = make([]byte, 16) // FNV-128a length

// compute FNV1a checksum
func checksum(data []byte) []byte {
	hasher := fnv.New128a() // Using 128-bit FNV-1a. You can also use New32a(), New64a() for smaller sizes.
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

func precomputeChecksums(file *os.File, blockSize uint32, lastBlockNum uint32, cache *ChecksumCache) {
	Log("checksums: start computing..\n")
	for idx := uint32(0); idx <= lastBlockNum; idx++ {
		buf := make([]byte, blockSize)
		offset := int64(idx) * int64(blockSize)

		n, err := file.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			Log("Checksum error at block %d: %v\n", idx, err)
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
}

