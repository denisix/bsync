package main

import (
	"bytes"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestSetCompressionLevel(t *testing.T) {
	cases := map[string]zstd.EncoderLevel{
		"fast":    zstd.SpeedFastest,
		"default": zstd.SpeedDefault,
		"better":  zstd.SpeedBetterCompression,
		"best":    zstd.SpeedBestCompression,
		"unknown": zstd.SpeedDefault, // falls through to default
	}
	orig := encoderLevel
	defer func() { encoderLevel = orig }()

	for name, want := range cases {
		SetCompressionLevel(name)
		if encoderLevel != want {
			t.Errorf("SetCompressionLevel(%q) = %v, want %v", name, encoderLevel, want)
		}
	}
}

func TestCompressRoundTripAllLevels(t *testing.T) {
	orig := encoderLevel
	defer func() { encoderLevel = orig }()

	// Compressible data (repeating) so compression actually shrinks it.
	data := bytes.Repeat([]byte("the quick brown fox 0123456789 "), 4096)

	for _, level := range []string{"fast", "default", "better", "best"} {
		t.Run(level, func(t *testing.T) {
			SetCompressionLevel(level)
			comp, err := compressData(data)
			if err != nil {
				t.Fatalf("compress: %v", err)
			}
			if len(comp) >= len(data) {
				t.Errorf("compressible data did not shrink: %d -> %d", len(data), len(comp))
			}
			got, err := decompressData(comp)
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Error("round-trip mismatch")
			}
		})
	}
}

func TestCompressIncompressible(t *testing.T) {
	data := randomBytes(65536, 7)
	comp, err := compressData(data)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("round-trip mismatch on random data")
	}
}

func TestCompressEmpty(t *testing.T) {
	comp, err := compressData([]byte{})
	if err != nil {
		t.Fatalf("compress empty: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty round-trip produced %d bytes", len(got))
	}
}

func TestDecompressCorrupt(t *testing.T) {
	// Garbage is not a valid zstd stream.
	if _, err := decompressData([]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02}); err == nil {
		t.Error("expected error decompressing garbage")
	}
	// A truncated valid stream should also fail.
	good, err := compressData(bytes.Repeat([]byte("data"), 1000))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(good) > 4 {
		if _, err := decompressData(good[:len(good)-3]); err == nil {
			t.Error("expected error decompressing a truncated stream")
		}
	}
}

// compressData encrypts after compressing when a key is set; decompressData
// must decrypt then decompress. Regression coverage for the -e path.
func TestCompressWithEncryption(t *testing.T) {
	resetCrypto(t)
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}

	data := bytes.Repeat([]byte("encrypted-and-compressed "), 2048)
	comp, err := compressData(data)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("encrypted+compressed round-trip mismatch")
	}

	// With the wrong key, decompressData should fail at the decrypt stage.
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	if _, err := decompressData(comp); err == nil {
		t.Error("decompress with wrong key should fail")
	}
}
