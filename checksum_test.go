package main

import (
	"bytes"
	"os"
	"testing"
)

func TestPrecomputeChecksums(t *testing.T) {
	const bs = 1024
	b0 := randomBytes(bs, 31)
	b1 := randomBytes(bs, 32)
	b2 := zeroBytes(bs)
	data := append(append(append([]byte{}, b0...), b1...), b2...)

	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	lastBlock := uint32(2)
	cache := NewChecksumCache(lastBlock)
	precomputeChecksums(f, bs, lastBlock, cache, 0, 4)

	if !bytes.Equal(cache.WaitFor(0), checksum(b0)) {
		t.Error("block 0 hash mismatch")
	}
	if !bytes.Equal(cache.WaitFor(1), checksum(b1)) {
		t.Error("block 1 hash mismatch")
	}
	if !bytes.Equal(cache.WaitFor(2), zeroBlockHash) {
		t.Error("zero block 2 should map to zeroBlockHash")
	}
}

func TestPrecomputeChecksumsEOF(t *testing.T) {
	const bs = 1024
	data := randomBytes(bs*2, 33) // 2 blocks
	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	// Ask for one block beyond the file -> should be marked "EOF".
	lastBlock := uint32(2)
	cache := NewChecksumCache(lastBlock)
	precomputeChecksums(f, bs, lastBlock, cache, 0, 2)

	if string(cache.WaitFor(2)) != "EOF" {
		t.Errorf("block beyond EOF = %q, want EOF", string(cache.WaitFor(2)))
	}
}

func TestPrecomputeChecksumsParallel(t *testing.T) {
	const bs = 1024
	compressible := bytes.Repeat([]byte("abcd"), bs/4) // shrinks well
	zeros := zeroBytes(bs)
	incompressible := randomBytes(bs, 41)
	data := append(append(append([]byte{}, compressible...), zeros...), incompressible...)

	collect := func(noCompress bool) map[uint32]PrecomputedBlock {
		path := writeTempFile(t, data)
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()

		sr := NewSequentialReader(f, bs, uint64(len(data)), 0, 4)
		sr.Start()
		cache := NewChecksumCache(2)
		ch := make(chan PrecomputedBlock, 8)
		go precomputeChecksumsParallel(sr, cache, ch, 4, noCompress)

		out := make(map[uint32]PrecomputedBlock)
		for b := range ch {
			out[b.BlockIdx] = b
		}
		return out
	}

	// Compression enabled
	res := collect(false)
	if len(res) != 3 {
		t.Fatalf("got %d precomputed blocks, want 3", len(res))
	}
	if !res[0].UseCompressed || res[0].Compressed == nil {
		t.Error("compressible block 0 should use compression")
	}
	if !res[1].IsZero {
		t.Error("block 1 should be detected as zero")
	}
	if res[2].UseCompressed {
		t.Error("incompressible block 2 should not use compression")
	}
	if res[2].Data == nil {
		t.Error("uncompressed block 2 should retain raw Data")
	}

	// noCompress: even the compressible block keeps raw data, no compression.
	resN := collect(true)
	if resN[0].UseCompressed || resN[0].Compressed != nil {
		t.Error("noCompress: block 0 must not be compressed")
	}
	if resN[0].Data == nil {
		t.Error("noCompress: block 0 must retain raw Data")
	}
	if !resN[1].IsZero {
		t.Error("noCompress: block 1 should still be zero")
	}
}

func TestChecksumCacheManyWaiters(t *testing.T) {
	cache := NewChecksumCache(100)
	h := checksum([]byte("payload"))

	const waiters = 50
	done := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		go func(idx uint32) {
			got := cache.WaitFor(idx)
			if !bytes.Equal(got, h) {
				t.Errorf("idx %d: hash mismatch", idx)
			}
			done <- true
		}(uint32(i))
	}
	// Set hashes after waiters are blocked.
	for i := uint32(0); i < waiters; i++ {
		cache.Set(i, h)
	}
	for i := 0; i < waiters; i++ {
		<-done
	}
}

func TestChecksumCacheNoPrecompute(t *testing.T) {
	cache := NewChecksumCache(10)
	cache.noPrecompute = true
	// With noPrecompute every lookup returns the 16-byte zero hash immediately.
	got := cache.WaitFor(3)
	if len(got) != 16 {
		t.Fatalf("len = %d, want 16", len(got))
	}
	if !bytes.Equal(got, make([]byte, 16)) {
		t.Error("noPrecompute should return all-zero hash")
	}
}

func TestChecksumCacheOutOfBounds(t *testing.T) {
	cache := NewChecksumCache(5)
	got := cache.WaitFor(999) // beyond maxId
	if len(got) != 16 {
		t.Errorf("out-of-bounds len = %d, want 16", len(got))
	}
}

func TestChecksumCacheDelete(t *testing.T) {
	cache := NewChecksumCache(5)
	h := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	cache.Set(3, h)
	if !bytes.Equal(cache.WaitFor(3), h) {
		t.Fatal("set/waitfor failed")
	}
	cache.Delete(3)
	cache.mu.Lock()
	_, present := cache.data[3]
	ready := cache.ready[3]
	cache.mu.Unlock()
	if present || ready {
		t.Error("Delete did not remove the cached entry")
	}
}

func TestGetZeroBuf(t *testing.T) {
	b := getZeroBuf(4096)
	if len(b) != 4096 {
		t.Fatalf("len = %d, want 4096", len(b))
	}
	if !isZeroBlock(b) {
		t.Error("getZeroBuf returned non-zero bytes")
	}
	// size beyond the shared buffer max takes the fresh-alloc path
	big := getZeroBuf(zeroBufMax + 10)
	if len(big) != zeroBufMax+10 {
		t.Errorf("big len = %d, want %d", len(big), zeroBufMax+10)
	}
}

func TestIsZeroBlockEmptyAndOne(t *testing.T) {
	if !isZeroBlock([]byte{}) {
		t.Error("empty slice should be considered zero")
	}
	if !isZeroBlock([]byte{0}) {
		t.Error("single zero byte should be zero")
	}
	if isZeroBlock([]byte{1}) {
		t.Error("single non-zero byte should not be zero")
	}
}

func TestIsZeroBlockBoundaries(t *testing.T) {
	for _, n := range []int{7, 8, 9, 15, 16, 31, 64} {
		zero := make([]byte, n)
		if !isZeroBlock(zero) {
			t.Errorf("len %d all-zero reported non-zero", n)
		}
		// flip the last byte
		nonzero := make([]byte, n)
		nonzero[n-1] = 1
		if isZeroBlock(nonzero) {
			t.Errorf("len %d with last byte set reported zero", n)
		}
		// flip a middle byte
		mid := make([]byte, n)
		mid[n/2] = 0xFF
		if isZeroBlock(mid) {
			t.Errorf("len %d with middle byte set reported zero", n)
		}
	}
}
