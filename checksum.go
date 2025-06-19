package main

import (
	"hash/fnv"
	"io"
	"os"
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
	checksums map[uint64]chan []byte
	blocks    map[uint64]chan *BlockData // only if storeData
	maxId     uint64
	window    uint64
	storeData bool
	compress  bool
	file      *os.File
	blockSize uint32
}

func NewChecksumCache(maxId uint64, storeData, compress bool, file *os.File, blockSize uint32) *ChecksumCache {
	cc := &ChecksumCache{
		checksums: make(map[uint64]chan []byte),
		maxId:     maxId,
		window:    5,
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

func (cc *ChecksumCache) EnsureWindow(idx uint64) {
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
	ch, ok := cc.checksums[idx]
	if !ok {
		return zeroBlockHash
	}
	return <-ch
}

func (cc *ChecksumCache) WaitForBlockData(idx uint64) *BlockData {
	if !cc.storeData {
		return nil
	}
	cc.EnsureWindow(idx)
	ch, ok := cc.blocks[idx]
	if !ok {
		return nil
	}
	return <-ch
}

func processBlock(f *os.File, bs uint32, idx uint64, cc *ChecksumCache) {
	if f == nil || bs == 0 {
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

	cc.checksums[idx] <- sum
	if cc.storeData {
		cc.blocks[idx] <- bd
	}
}
