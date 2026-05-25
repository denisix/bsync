package main

import (
	"bytes"
	"os"
	"testing"
)

// collectReader runs a SequentialReader to completion and returns the blocks
// in the order they were emitted.
func collectReader(t *testing.T, data []byte, blockSize uint32, skipIdx uint32) []BlockData {
	t.Helper()
	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	sr := NewSequentialReader(f, blockSize, uint64(len(data)), skipIdx, 4)
	sr.Start()

	var blocks []BlockData
	for b := range sr.Blocks() {
		blocks = append(blocks, b)
	}
	sr.Wait()
	return blocks
}

func TestSequentialReaderOrderAndContent(t *testing.T) {
	const bs = 1024
	data := randomBytes(bs*5, 11) // exactly 5 blocks
	blocks := collectReader(t, data, bs, 0)

	if len(blocks) != 5 {
		t.Fatalf("got %d blocks, want 5", len(blocks))
	}
	for i, b := range blocks {
		if b.BlockIdx != uint32(i) {
			t.Errorf("block %d has idx %d (out of order)", i, b.BlockIdx)
		}
		want := data[i*bs : (i+1)*bs]
		if !bytes.Equal(b.Data, want) {
			t.Errorf("block %d data mismatch", i)
		}
	}
}

func TestSequentialReaderPartialLastBlock(t *testing.T) {
	const bs = 1024
	data := randomBytes(bs*2+100, 12) // 2 full + 1 partial (100 bytes)
	blocks := collectReader(t, data, bs, 0)

	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	if len(blocks[2].Data) != 100 {
		t.Errorf("last block len = %d, want 100", len(blocks[2].Data))
	}
	if !bytes.Equal(blocks[2].Data, data[bs*2:]) {
		t.Error("partial last block content mismatch")
	}
}

func TestSequentialReaderSubBlockFile(t *testing.T) {
	const bs = 4096
	data := randomBytes(10, 13) // smaller than one block
	blocks := collectReader(t, data, bs, 0)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if !bytes.Equal(blocks[0].Data, data) {
		t.Error("sub-block content mismatch")
	}
}

func TestSequentialReaderExactMultiple(t *testing.T) {
	const bs = 512
	data := randomBytes(bs*4, 15) // no partial block
	blocks := collectReader(t, data, bs, 0)
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks, want 4", len(blocks))
	}
	for i, b := range blocks {
		if len(b.Data) != bs {
			t.Errorf("block %d len = %d, want %d", i, len(b.Data), bs)
		}
	}
}

func TestSequentialReaderSkipIdx(t *testing.T) {
	const bs = 1024
	data := randomBytes(bs*5, 14)
	blocks := collectReader(t, data, bs, 2) // skip first 2 blocks

	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	if blocks[0].BlockIdx != 2 {
		t.Errorf("first emitted idx = %d, want 2", blocks[0].BlockIdx)
	}
	if !bytes.Equal(blocks[0].Data, data[2*bs:3*bs]) {
		t.Error("skip start content mismatch")
	}
}
