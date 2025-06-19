package main

import (
	"hash/fnv"
	"io"
	"os"
	"sync"
)

var zeroBlockHash = make([]byte, 16)

func checksum(data []byte) []byte {
	h := fnv.New128a()
	h.Write(data)
	return h.Sum(nil)
}

type BlockData struct {
	Data         []byte
	IsCompressed bool
}

type ChecksumCache struct {
	checksums   map[uint64]chan []byte
	blocks      map[uint64]chan *BlockData
	maxId       uint64
	window      uint64
	storeData   bool
	compress    bool
	file        *os.File
	blockSize   uint64
	mu          sync.RWMutex // protect map access
	cleanupLock sync.Mutex   // protect cleanup operations
}

func NewChecksumCache(maxId uint64, storeData, compress bool, file *os.File, blockSize uint64, blocksAhead uint64) *ChecksumCache {
	cc := &ChecksumCache{
		checksums: make(map[uint64]chan []byte),
		maxId:     maxId,
		window:    blocksAhead,
		storeData: storeData,
		compress:  compress,
		file:      file,
		blockSize: blockSize,
	}
	if storeData {
		cc.blocks = make(map[uint64]chan *BlockData)
	}
	return cc
}

func (cc *ChecksumCache) cleanup(idx uint64) {
	cc.cleanupLock.Lock()
	defer cc.cleanupLock.Unlock()

	// Remove channels for blocks outside the window
	if idx > cc.window {
		cc.mu.Lock()
		for i := uint64(0); i < idx-cc.window; i++ {
			delete(cc.checksums, i)
			if cc.storeData {
				delete(cc.blocks, i)
			}
		}
		cc.mu.Unlock()
	}
}

func (cc *ChecksumCache) EnsureWindow(idx uint64) {
	if idx > cc.maxId {
		return
	}

	cc.mu.Lock()
	defer cc.mu.Unlock()

	// Create channels for blocks within the window
	for i := idx; i < idx+cc.window && i <= cc.maxId; i++ {
		if _, ok := cc.checksums[i]; !ok {
			cc.checksums[i] = make(chan []byte, 1)
			if cc.storeData && cc.blocks[i] == nil {
				cc.blocks[i] = make(chan *BlockData, 1)
			}
			go processBlock(cc.file, cc.blockSize, i, cc)
		}
	}
}

func (cc *ChecksumCache) WaitFor(idx uint64) []byte {
	if idx > cc.maxId {
		return zeroBlockHash
	}

	cc.EnsureWindow(idx)

	cc.mu.RLock()
	ch, ok := cc.checksums[idx]
	cc.mu.RUnlock()

	if !ok {
		return zeroBlockHash
	}

	hash := <-ch
	cc.cleanup(idx + 1)
	return hash
}

func (cc *ChecksumCache) WaitForBlockData(idx uint64) *BlockData {
	if !cc.storeData || idx > cc.maxId {
		return nil
	}

	cc.EnsureWindow(idx)

	cc.mu.RLock()
	ch, ok := cc.blocks[idx]
	cc.mu.RUnlock()

	if !ok {
		return nil
	}

	data := <-ch
	cc.cleanup(idx + 1)
	return data
}

func processBlock(f *os.File, bs uint64, idx uint64, cc *ChecksumCache) {
	if f == nil || bs == 0 {
		cc.mu.Lock()
		if ch, ok := cc.checksums[idx]; ok {
			ch <- []byte("ERR")
		}
		cc.mu.Unlock()
		return
	}

	buf := make([]byte, bs)
	off := int64(idx) * int64(bs)
	n, err := f.ReadAt(buf, off)

	var sum []byte
	var bd *BlockData

	switch {
	case err != nil && err != io.EOF:
		sum = []byte("ERR")
	case n == 0 && err == io.EOF:
		sum = []byte("EOF")
	case isZeroBlock(buf[:n]):
		sum = zeroBlockHash
		if cc.storeData {
			bd = &BlockData{Data: buf[:n]}
		}
	default:
		sum = checksum(buf[:n])
		if cc.storeData {
			if cc.compress {
				if c, e := compressData(buf[:n]); e == nil && len(c) < n {
					bd = &BlockData{Data: c, IsCompressed: true}
				} else {
					bd = &BlockData{Data: buf[:n]}
				}
			} else {
				bd = &BlockData{Data: buf[:n]}
			}
		}
	}

	cc.mu.Lock()
	if ch, ok := cc.checksums[idx]; ok {
		ch <- sum
	}
	if cc.storeData {
		if ch, ok := cc.blocks[idx]; ok {
			ch <- bd
		}
	}
	cc.mu.Unlock()
}
