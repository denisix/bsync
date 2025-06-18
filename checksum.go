package main

import (
	"hash/fnv"
	"io"
	"os"
	"sync"
)

var zeroBlockHash = make([]byte, 16) // FNV-128a length

// compute FNV1a checksum
func checksum(data []byte) []byte {
	hasher := fnv.New128a() // Using 128-bit FNV-1a. You can also use New32a(), New64a() for smaller sizes.
	hasher.Write(data)
	return hasher.Sum(nil)
}

type BlockData struct {
	Data         []byte
	IsCompressed bool
}

type ChecksumCache struct {
	data           map[uint64][]byte
	ready          map[uint64]bool
	blockData      map[uint64]*BlockData
	blockDataReady map[uint64]bool
	mu             sync.Mutex
	cond           *sync.Cond
	maxId          uint64
	windowSize     uint64
	nextBlock      chan uint64
	done           chan struct{}
}

func NewChecksumCache(maxId uint64) *ChecksumCache {
	cc := &ChecksumCache{
		data:           make(map[uint64][]byte),
		ready:          make(map[uint64]bool),
		blockData:      make(map[uint64]*BlockData),
		blockDataReady: make(map[uint64]bool),
		maxId:          maxId,
		windowSize:     10,
		nextBlock:      make(chan uint64, 1),
		done:           make(chan struct{}),
	}
	cc.cond = sync.NewCond(&cc.mu)
	return cc
}

func (cc *ChecksumCache) Set(idx uint64, checksum []byte) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.data[idx] = checksum
	cc.ready[idx] = true
	cc.cond.Broadcast()
}

func (cc *ChecksumCache) SetBlockData(idx uint64, data []byte, isCompressed bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.blockData[idx] = &BlockData{
		Data:         data,
		IsCompressed: isCompressed,
	}
	cc.blockDataReady[idx] = true
	cc.cond.Broadcast()
}

func (cc *ChecksumCache) WaitFor(idx uint64) []byte {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if idx > cc.maxId {
		return make([]byte, 16) // all-zero hash
	}

	for !cc.ready[idx] {
		cc.cond.Wait()
	}

	return cc.data[idx]
}

func (cc *ChecksumCache) WaitForBlockData(idx uint64) *BlockData {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if idx > cc.maxId {
		return nil
	}

	// Signal that we need to process this block
	select {
	case cc.nextBlock <- idx:
	default:
	}

	for !cc.blockDataReady[idx] {
		cc.cond.Wait()
	}

	return cc.blockData[idx]
}

func (cc *ChecksumCache) GetWindowSize() uint64 {
	return cc.windowSize
}

func (cc *ChecksumCache) Close() {
	close(cc.done)
	cc.cond.Broadcast()
}

func precomputeChecksums(file *os.File, blockSize uint32, lastBlockNum uint64, cache *ChecksumCache, storeData bool, tryCompress bool) {
	Log("checksums: start computing..\n")
	windowSize := cache.GetWindowSize()

	// Process initial window
	for i := uint64(0); i < windowSize && i <= lastBlockNum; i++ {
		processBlock(file, blockSize, i, cache, storeData, tryCompress)
	}

	// Process remaining blocks as requested
	for {
		select {
		case <-cache.done:
			return
		case idx := <-cache.nextBlock:
			// Process any block up to and including the last block
			if idx <= lastBlockNum {
				processBlock(file, blockSize, idx, cache, storeData, tryCompress)
			}
		}
	}
}

func processBlock(file *os.File, blockSize uint32, idx uint64, cache *ChecksumCache, storeData bool, tryCompress bool) {
	// Read block data
	buf := make([]byte, blockSize)
	offset := int64(idx) * int64(blockSize)

	n, err := file.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		Log("Read error at block %d: %v\n", idx, err)
		cache.Set(idx, []byte("ERR"))
		cache.SetBlockData(idx, nil, false)
		return
	}
	if n == 0 && err == io.EOF {
		cache.Set(idx, []byte("EOF"))
		cache.SetBlockData(idx, nil, false)
		return
	}

	// Check if it's a zero block
	if isZeroBlock(buf[:n]) {
		cache.Set(idx, zeroBlockHash)
		if storeData {
			cache.SetBlockData(idx, buf[:n], false)
		} else {
			cache.SetBlockData(idx, nil, false)
		}
		return
	}

	// Compute and store checksum
	hash := checksum(buf[:n])
	cache.Set(idx, hash)

	// Store data if requested
	if storeData {
		if tryCompress {
			compressed, err := compressData(buf[:n])
			if err != nil {
				Log("Compression error at block %d: %v\n", idx, err)
				cache.SetBlockData(idx, buf[:n], false)
			} else if len(compressed) < n {
				cache.SetBlockData(idx, compressed, true)
			} else {
				cache.SetBlockData(idx, buf[:n], false)
			}
		} else {
			cache.SetBlockData(idx, buf[:n], false)
		}
	} else {
		cache.SetBlockData(idx, nil, false)
	}
}
