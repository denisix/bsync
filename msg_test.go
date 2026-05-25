package main

import (
	"encoding/binary"
	"testing"
)

func TestUnpackTooShort(t *testing.T) {
	// A buffer shorter than a packed Msg must error.
	if _, err := unpack([]byte{1, 2, 3}); err == nil {
		t.Error("expected error unpacking a too-short buffer")
	}
}

func TestPackedSizeConstant(t *testing.T) {
	msg := &Msg{MagicHead: stringToFixedSizeArray(magicHead)}
	data, err := pack(msg)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	want := binary.Size(Msg{})
	if len(data) != want {
		t.Errorf("packed size = %d, want %d", len(data), want)
	}
	// magic(17)+blockIdx(4)+blockSize(4)+fileSize(8)+dataSize(4)+3 flags = 40
	if want != 40 {
		t.Errorf("binary.Size(Msg) = %d, want 40", want)
	}
}

func TestPackUnpackMaxValues(t *testing.T) {
	orig := &Msg{
		MagicHead:  stringToFixedSizeArray(magicHead),
		BlockIdx:   ^uint32(0),
		BlockSize:  ^uint32(0),
		FileSize:   ^uint64(0),
		DataSize:   ^uint32(0),
		Compressed: true,
		Zero:       true,
		Done:       true,
	}
	data, err := pack(orig)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	got, err := unpack(data)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if *got != *orig {
		t.Errorf("max-value round-trip mismatch:\n got %+v\nwant %+v", got, orig)
	}
}

func TestZeroBlockHashLength(t *testing.T) {
	if len(zeroBlockHash) != 16 {
		t.Errorf("zeroBlockHash length = %d, want 16 (FNV-128a)", len(zeroBlockHash))
	}
}

func TestStringToFixedSizeArrayTruncates(t *testing.T) {
	long := "this string is definitely longer than seventeen bytes"
	arr := stringToFixedSizeArray(long)
	if len(arr) != magicLen {
		t.Fatalf("len = %d, want %d", len(arr), magicLen)
	}
	if string(arr[:]) != long[:magicLen] {
		t.Errorf("truncation mismatch: %q", string(arr[:]))
	}
}

func TestBlockPayloadSizeBoundaries(t *testing.T) {
	tests := []struct {
		fileSize  uint64
		blockSize uint32
		blockIdx  uint32
		want      uint32
	}{
		{0, 4096, 0, 0},
		{4096, 4096, 0, 4096},
		{4097, 4096, 1, 1},
		{4096, 4096, 1, 0},
		{4096, 0, 0, 0},
	}
	for _, tt := range tests {
		if got := blockPayloadSize(tt.fileSize, tt.blockSize, tt.blockIdx); got != tt.want {
			t.Errorf("blockPayloadSize(%d, %d, %d) = %d, want %d", tt.fileSize, tt.blockSize, tt.blockIdx, got, tt.want)
		}
	}
}

func TestValidateProtocolMessageStates(t *testing.T) {
	validUpload := []*Msg{{}, {Done: true}, {Zero: true}, {DataSize: 1}, {DataSize: 1, Compressed: true}}
	for _, msg := range validUpload {
		if err := validateUploadMessage(msg); err != nil {
			t.Errorf("valid upload message %+v rejected: %v", msg, err)
		}
	}
	invalidUpload := []*Msg{{Done: true, DataSize: 1}, {Zero: true, Compressed: true}, {Zero: true, DataSize: 1}, {Compressed: true}}
	for _, msg := range invalidUpload {
		if err := validateUploadMessage(msg); err == nil {
			t.Errorf("invalid upload message %+v accepted", msg)
		}
	}
	if err := validateDownloadRequest(&Msg{}); err != nil {
		t.Fatalf("valid download request rejected: %v", err)
	}
	for _, msg := range []*Msg{{Done: true}, {Zero: true}, {Compressed: true}, {DataSize: 1}} {
		if err := validateDownloadRequest(msg); err == nil {
			t.Errorf("invalid download request %+v accepted", msg)
		}
	}
}
