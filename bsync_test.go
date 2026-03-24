package main

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// Test isZeroBlock
func TestIsZeroBlock(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected bool
	}{
		{"all zeros", []byte{0, 0, 0, 0}, true},
		{"all zeros single", []byte{0}, true},
		{"not zero", []byte{0, 0, 1, 0}, false},
		{"all 0xFF", []byte{0xFF, 0xFF, 0xFF}, false},
		{"mixed", []byte{0, 0xFF, 0}, false},
		{"empty", []byte{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isZeroBlock(tt.data)
			if result != tt.expected {
				t.Errorf("isZeroBlock() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// Test checksum
func TestChecksum(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x42}},
		{"all zeros", []byte{0, 0, 0, 0}},
		{"all 0xFF", bytes.Repeat([]byte{0xFF}, 100)},
		{"random", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checksum(tt.data)
			if len(result) != 16 {
				t.Errorf("checksum() length = %d, want 16", len(result))
			}
		})
	}
}

// Test ChecksumCache
func TestChecksumCache(t *testing.T) {
	cache := NewChecksumCache(10)

	// Test Set and WaitFor
	t.Run("Set and WaitFor", func(t *testing.T) {
		hash := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
		cache.Set(5, hash)

		// Start goroutine to wait
		done := make(chan []byte, 1)
		go func() {
			result := cache.WaitFor(5)
			done <- result
		}()

		select {
		case result := <-done:
			if !bytes.Equal(result, hash) {
				t.Errorf("WaitFor() = %v, want %v", result, hash)
			}
		case <-time.After(1 * time.Second):
			t.Error("WaitFor() timed out")
		}
	})

	// Test out of bounds
	t.Run("out of bounds", func(t *testing.T) {
		result := cache.WaitFor(999) // beyond maxId=10
		if len(result) != 16 {
			t.Errorf("out-of-bounds WaitFor() length = %d, want 16", len(result))
		}
	})

	// Test concurrent access
	t.Run("concurrent access", func(t *testing.T) {
		cache2 := NewChecksumCache(100)
		hash := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

		// Multiple goroutines setting and waiting
		done := make(chan bool, 10)
		for i := 0; i < 10; i++ {
		go func(idx uint32) {
			cache2.Set(idx, hash)
			result := cache2.WaitFor(idx)
			if !bytes.Equal(result, hash) {
				t.Errorf("concurrent idx=%d: hash mismatch", idx)
			}
			done <- true
		}(uint32(i))
		}

		// Wait for all goroutines
		for i := 0; i < 10; i++ {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("concurrent test timed out")
			}
		}
	})
}

// Test parseSSHTarget
func TestParseSSHTarget(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantUser     string
		wantHost     string
		wantPort     string
		wantFile     string
	}{
		{"basic SSH with user", "root@host:/path/to/file", "root", "host", "", "/path/to/file"},
		{"SSH without user", "host:/path", "", "host", "", "/path"},
		{"local file only", "/local/path/file", "", "", "", "/local/path/file"},
		{"SSH with custom port", "root@1.2.3.4:2222:/path", "root", "1.2.3.4", "2222", "/path"},
		{"SSH with port localhost", "user@localhost:8022:/tmp/file", "user", "localhost", "8022", "/tmp/file"},
		{"local file no colon", "localfile", "", "", "", "localfile"},
		{"IPv4 with port", "user@192.168.1.1:1022:/data", "user", "192.168.1.1", "1022", "/data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, host, port, file := parseSSHTarget(tt.input)
			if user != tt.wantUser {
				t.Errorf("parseSSHTarget() user = %q, want %q", user, tt.wantUser)
			}
			if host != tt.wantHost {
				t.Errorf("parseSSHTarget() host = %q, want %q", host, tt.wantHost)
			}
			if port != tt.wantPort {
				t.Errorf("parseSSHTarget() port = %q, want %q", port, tt.wantPort)
			}
			if file != tt.wantFile {
				t.Errorf("parseSSHTarget() file = %q, want %q", file, tt.wantFile)
			}
		})
	}
}

// Test isPortNumber
func TestIsPortNumber(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid port 80", "80", true},
		{"valid port 8080", "8080", true},
		{"valid port 22", "22", true},
		{"valid port 65535", "65535", true},
		{"empty string", "", false},
		{"non-numeric", "abc", false},
		{"mixed", "80abc", false},
		{"with colon", "8080:", false},
		{"zero", "0", false},
		{"negative", "-1", false},
		{"too large", "65536", false},
		{"very large", "99999", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPortNumber(tt.input)
			if result != tt.expected {
				t.Errorf("isPortNumber(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// Test pack and unpack round-trip
func TestPackUnpack(t *testing.T) {
	magicBytes := stringToFixedSizeArray(magicHead)

	tests := []struct {
		name string
		msg  Msg
	}{
		{
			name: "basic message",
			msg: Msg{
				MagicHead:  magicBytes,
				BlockIdx:   1,
				BlockSize:  10485760,
				FileSize:   104857600,
				DataSize:   1048576,
				Compressed: false,
				Zero:       false,
				Done:       false,
			},
		},
		{
			name: "compressed message",
			msg: Msg{
				MagicHead:  magicBytes,
				BlockIdx:   5,
				BlockSize:  5242880,
				FileSize:   52428800,
				DataSize:   1000000,
				Compressed: true,
				Zero:       false,
				Done:       false,
			},
		},
		{
			name: "zero block",
			msg: Msg{
				MagicHead:  magicBytes,
				BlockIdx:   10,
				BlockSize:  10485760,
				FileSize:   104857600,
				DataSize:   10485760,
				Compressed: false,
				Zero:       true,
				Done:       false,
			},
		},
		{
			name: "DONE message",
			msg: Msg{
				MagicHead:  magicBytes,
				BlockIdx:   0,
				BlockSize:  10485760,
				FileSize:   0,
				DataSize:   0,
				Compressed: false,
				Zero:       false,
				Done:       true,
			},
		},
		{
			name: "all flags true",
			msg: Msg{
				MagicHead:  magicBytes,
				BlockIdx:   999,
				BlockSize:  512,
				FileSize:   1024000,
				DataSize:   256,
				Compressed: true,
				Zero:       true,
				Done:       true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Pack
			data, err := pack(&tt.msg)
			if err != nil {
				t.Fatalf("pack() error: %v", err)
			}

			// Verify packed size
			// MagicHead: 17 bytes + BlockIdx: 4 + BlockSize: 4 + FileSize: 8 + DataSize: 4 + flags: 3 = 40 bytes
			expectedSize := 17 + 4 + 4 + 8 + 4 + 1 + 1 + 1 // 40 bytes
			if len(data) != expectedSize {
				t.Errorf("pack() size = %d, want %d", len(data), expectedSize)
			}

			// Unpack
			unpacked, err := unpack(data)
			if err != nil {
				t.Fatalf("unpack() error: %v", err)
			}

			// Compare
			if unpacked.MagicHead != tt.msg.MagicHead ||
				unpacked.BlockIdx != tt.msg.BlockIdx ||
				unpacked.BlockSize != tt.msg.BlockSize ||
				unpacked.FileSize != tt.msg.FileSize ||
				unpacked.DataSize != tt.msg.DataSize ||
				unpacked.Compressed != tt.msg.Compressed ||
				unpacked.Zero != tt.msg.Zero ||
				unpacked.Done != tt.msg.Done {
				t.Errorf("round-trip mismatch: got %+v, want %+v", unpacked, tt.msg)
			}
		})
	}
}

// Test stringToFixedSizeArray
func TestStringToFixedSizeArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"exact length", "blockSync-ver0.01"},
		{"short string", "short"},
		{"empty string", ""},
		{"long string", "this is a very long string that exceeds the array size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringToFixedSizeArray(tt.input)
			if len(result) != 17 { // magicLen
				t.Errorf("len(result) = %d, want 17", len(result))
			}
		})
	}
}

// Test truncateIfRegularFile
func TestTruncateIfRegularFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a regular file
	regularFile, err := os.Create(tmpDir + "/regular.img")
	if err != nil {
		t.Fatal(err)
	}
	defer regularFile.Close()

	// Write some data
	regularFile.Write([]byte("test data"))

	// Truncate to larger size
	truncateIfRegularFile(regularFile, 100)
	info, _ := regularFile.Stat()
	if info.Size() != 100 {
		t.Errorf("file size = %d, want 100", info.Size())
	}

	// Truncate to same size (should skip truncate)
	truncateIfRegularFile(regularFile, 100)
}

// Test getDeviceSize
func TestGetDeviceSize(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a test file
	file, err := os.Create(tmpDir + "/test.img")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Write some data
	file.Write([]byte("test"))

	size := getDeviceSize(file)
	if size != 4 {
		t.Errorf("getDeviceSize() = %d, want 4", size)
	}
}

// Test compress and decompress round-trip
func TestCompressDecompress(t *testing.T) {
	testData := []byte("This is test data that should compress reasonably well because it has repeating patterns")

	// Compress
	compressed, err := compressData(testData)
	if err != nil {
		t.Fatalf("compressData() error: %v", err)
	}

	// Decompress
	decompressed, err := decompressData(compressed)
	if err != nil {
		t.Fatalf("decompressData() error: %v", err)
	}

	// Compare
	if !bytes.Equal(decompressed, testData) {
		t.Error("compress/decompress round-trip failed: data mismatch")
	}
}

// Test compressData with zeros
func TestCompressZeros(t *testing.T) {
	zeros := make([]byte, 1024)
	compressed, err := compressData(zeros)
	if err != nil {
		t.Fatalf("compressData() error: %v", err)
	}

	// Zeros should compress well
	if len(compressed) >= len(zeros) {
		t.Logf("Warning: zeros didn't compress: %d -> %d", len(zeros), len(compressed))
	}
}
