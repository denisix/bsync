package main

import (
	"hash/fnv"
	"sync"
	"os"
	"fmt"
	"io"
)

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
}

func NewChecksumCache() *ChecksumCache {
	cc := &ChecksumCache{
		data:  make(map[uint32][]byte),
		ready: make(map[uint32]bool),
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
	for !cc.ready[idx] {
		cc.cond.Wait()
	}
	return cc.data[idx]
}

func precomputeChecksums(file *os.File, blockSize uint32, lastBlockNum uint32, cache *ChecksumCache) {
	fmt.Println("- checksums: start computing..")
    buf := make([]byte, blockSize)
    for idx := uint32(0); idx <= lastBlockNum; idx++ {
        offset := int64(idx) * int64(blockSize)
        n, err := file.ReadAt(buf, offset)
        if err != nil && err != io.EOF {
            fmt.Println("Checksum reader error:", err)
            break
        }
        hash := checksum(buf[:n])
        cache.Set(idx, hash)
    }
		fmt.Println("- checksums: done")
}

