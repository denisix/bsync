package main

import (
	"bytes"
	"math/rand"
	"os"
	"sync"
	"testing"
)

// buildExpected splits data into blocks and returns the per-block byte slices.
func splitBlocks(data []byte, block int) [][]byte {
	var out [][]byte
	for off := 0; off < len(data); off += block {
		end := off + block
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[off:end])
	}
	return out
}

func TestOrderedWriterOutOfOrder(t *testing.T) {
	const block = 1024
	data := randomBytes(block*8, 21)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 4)

	// Submit blocks in a shuffled order.
	order := rand.New(rand.NewSource(99)).Perm(len(blocks))
	for _, idx := range order {
		ow.Submit(uint32(idx), int64(idx)*block, blocks[idx])
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("file content mismatch after out-of-order writes")
	}
}

func TestOrderedWriterWindowOverflow(t *testing.T) {
	// Many more blocks than the reorder window forces mid-stream flushing.
	const block = 256
	const n = 200
	data := randomBytes(block*n, 22)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 2) // small window
	for i, b := range blocks {
		ow.Submit(uint32(i), int64(i)*block, b)
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("file content mismatch after window overflow")
	}
}

func TestOrderedWriterConcurrentStress(t *testing.T) {
	const block = 256
	const n = 500
	data := randomBytes(block*n, 23)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 1) // tiny window -> heavy flushing

	// Submit from several goroutines concurrently (exercise the lock-free
	// channel path under -race).
	const submitters = 8
	var wg sync.WaitGroup
	for s := 0; s < submitters; s++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := start; i < n; i += submitters {
				ow.Submit(uint32(i), int64(i)*block, blocks[i])
			}
		}(s)
	}
	wg.Wait()
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch after concurrent stress")
	}
}

func TestOrderedWriterErrorOnClosedFile(t *testing.T) {
	path := writeTempFile(t, zeroBytes(4096))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ow := NewOrderedWriter(f, 2)
	f.Close() // close underlying file so WriteAt fails

	for i := 0; i < 10; i++ {
		ow.Submit(uint32(i), int64(i)*256, randomBytes(256, int64(i)))
	}
	if err := ow.Close(); err == nil {
		t.Error("expected a write error after closing underlying file")
	}
	if ow.Err() == nil {
		t.Error("Err() should report the write failure")
	}
}
