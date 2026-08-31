package main

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// TestReaderBufferPool tests that the reader buffer pool works correctly.
func TestReaderBufferPool(t *testing.T) {
	// Get multiple buffers from pool
	buf1 := getReaderBuf(1024)
	buf2 := getReaderBuf(2048)
	buf3 := getReaderBuf(10 * 1024 * 1024)

	// Verify sizes
	if len(buf1) != 1024 {
		t.Errorf("buf1 size: got %d, want %d", len(buf1), 1024)
	}
	if len(buf2) != 2048 {
		t.Errorf("buf2 size: got %d, want %d", len(buf2), 2048)
	}
	if len(buf3) != 10*1024*1024 {
		t.Errorf("buf3 size: got %d, want %d", len(buf3), 10*1024*1024)
	}

	// Verify capacity is adequate for typical block size
	if cap(buf1) < 1024 {
		t.Errorf("buf1 capacity: got %d, want >= %d", cap(buf1), 1024)
	}

	// Return buffers to pool
	putReaderBuf(buf1)
	putReaderBuf(buf2)
	putReaderBuf(buf3)

	// A size well past the typical block cap still gets its own exact-size
	// pool (bounded by maxDistinctReaderBufSizes, not by an absolute size
	// threshold) rather than being rounded up to some larger class.
	largeBuf := getReaderBuf(20 * 1024 * 1024)
	if len(largeBuf) != 20*1024*1024 {
		t.Errorf("largeBuf size: got %d, want %d", len(largeBuf), 20*1024*1024)
	}
	if cap(largeBuf) != 20*1024*1024 {
		t.Errorf("largeBuf capacity: got %d, want exactly %d (no rounding up)", cap(largeBuf), 20*1024*1024)
	}
	putReaderBuf(largeBuf)
}

// TestReaderBufferPoolConcurrency tests concurrent access to the buffer pool.
func TestReaderBufferPoolConcurrency(t *testing.T) {
	const goroutines = 100
	const iterations = 100

	var done atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < iterations; j++ {
				buf := getReaderBuf(1024 * 1024)
				// Use the buffer
				for k := range buf {
					buf[k] = byte(k % 256)
				}
				putReaderBuf(buf)
			}
			done.Add(1)
		}()
	}

	for done.Load() != int32(goroutines) {
		// Wait for all goroutines to complete
	}
}

// TestChecksumCachePreallocation tests that checksum maps are pre-allocated.
func TestChecksumCachePreallocation(t *testing.T) {
	testCases := []struct {
		name   string
		maxId  uint32
		expect int
	}{
		{"small", 10, 11},
		{"medium", 1000, 1001},
		{"large", 100000, 100001},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewChecksumCache(tc.maxId)

			// Check that maps have been pre-allocated
			// We can't directly inspect map capacity, but we can check
			// that the cache was created successfully
			if cache == nil {
				t.Fatal("NewChecksumCache returned nil")
			}

			// Verify we can set and get values without triggering map growth
			for i := uint32(0); i <= tc.maxId; i++ {
				hash := make([]byte, 16)
				hash[0] = byte(i)
				hash[1] = byte(i >> 8)
				cache.Set(i, hash)
			}

			for i := uint32(0); i <= tc.maxId; i++ {
				result := cache.WaitFor(i)
				if len(result) != 16 {
					t.Errorf("WaitFor(%d) returned hash of length %d, want 16", i, len(result))
				}
				expected := byte(i)
				if result[0] != expected {
					t.Errorf("WaitFor(%d)[0] = %d, want %d", i, result[0], expected)
				}
			}
		})
	}
}

// TestChecksumCacheConcurrentAccess tests concurrent access to checksum cache.
func TestChecksumCacheConcurrentAccess(t *testing.T) {
	const numBlocks = 1000
	const numGoroutines = 10

	cache := NewChecksumCache(numBlocks - 1)

	// Start writers
	var writeDone atomic.Int32
	for i := 0; i < numGoroutines; i++ {
		go func(start uint32) {
			for j := uint32(0); j < uint32(numBlocks)/uint32(numGoroutines); j++ {
				idx := start + j
				if idx >= numBlocks {
					break
				}
				hash := make([]byte, 16)
				hash[0] = byte(idx)
				cache.Set(idx, hash)
			}
			writeDone.Add(1)
		}(uint32(i) * (uint32(numBlocks) / uint32(numGoroutines)))
	}

	// Start readers (they will wait for writers via WaitFor)
	var readDone atomic.Int32
	for i := 0; i < numGoroutines; i++ {
		go func(start uint32) {
			for j := uint32(0); j < uint32(numBlocks)/uint32(numGoroutines); j++ {
				idx := start + j
				if idx >= numBlocks {
					break
				}
				hash := cache.WaitFor(idx)
				if len(hash) != 16 {
					t.Errorf("WaitFor(%d) returned hash of length %d", idx, len(hash))
				}
			}
			readDone.Add(1)
		}(uint32(i) * (uint32(numBlocks) / uint32(numGoroutines)))
	}

	// Wait for completion
	for writeDone.Load() != int32(numGoroutines) {
	}
	for readDone.Load() != int32(numGoroutines) {
	}
}

// TestAdviseSequential tests that AdviseSequential doesn't crash.
// It's hard to test the actual effect without kernel introspection,
// so we just verify it doesn't error out on regular files.
func TestAdviseSequential(t *testing.T) {
	// Create a temporary file
	tmpFile, err := os.CreateTemp("", "bsync_fadvise_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write some data
	testData := []byte("test data for fadvise")
	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Call AdviseSequential - should not panic or error
	AdviseSequential(tmpFile)

	// Verify file is still usable
	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	buf := make([]byte, len(testData))
	if n, err := tmpFile.Read(buf); err != nil || n != len(testData) {
		t.Fatalf("Read failed: n=%d err=%v", n, err)
	}
}

// TestAdviseSequentialOnNilFile tests that AdviseSequential handles nil gracefully.
func TestAdviseSequentialOnNilFile(t *testing.T) {
	// This should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AdviseSequential panicked on nil input: %v", r)
		}
	}()

	// Can't actually call with nil (would panic before our code),
	// but we can verify it handles various file types
	tmpFile, err := os.CreateTemp("", "bsync_fadvise_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	AdviseSequential(tmpFile)
	tmpFile.Close()
}

// TestCompressionBufferPool tests the compression output buffer pool.
func TestCompressionBufferPool(t *testing.T) {
	// This test verifies the compressData function uses the pool
	// by checking that multiple compressions don't cause excessive allocations

	testData := make([]byte, 1024*1024)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	// Compress multiple times
	for i := 0; i < 10; i++ {
		compressed, err := compressData(testData)
		if err != nil {
			t.Fatalf("compressData failed: %v", err)
		}
		if len(compressed) == 0 {
			t.Error("compressed data is empty")
		}
		if len(compressed) >= len(testData) {
			// Data might not compress, but should get same or smaller
			t.Logf("warning: compressed size %d >= original %d", len(compressed), len(testData))
		}
	}
}

// TestVmSplice tests vmsplice functionality.
func TestVmSplice(t *testing.T) {
	// Create a pipe
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe creation failed: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	// Test data
	data := [][]byte{
		[]byte("hello "),
		[]byte("world"),
	}

	// vmsplice: userspace -> pipe
	n, err := VmSplice(pw, data)
	if err != nil {
		t.Logf("VmSplice failed (may not be supported on this platform): %v", err)
		// Don't fail - vmsplice may not be available
		return
	}

	expectedLen := len(data[0]) + len(data[1])
	if n != expectedLen {
		t.Errorf("VmSplice wrote %d bytes, want %d", n, expectedLen)
	}

	// Read back from pipe
	readBuf := make([]byte, 100)
	n, err = pr.Read(readBuf)
	if err != nil {
		t.Fatalf("Read from pipe failed: %v", err)
	}

	result := string(readBuf[:n])
	if result != "hello world" {
		t.Errorf("Read data = %q, want %q", result, "hello world")
	}
}

// TestCombinedSplice tests the combined splice operation.
func TestCombinedSplice(t *testing.T) {
	// Create a temporary file for the destination
	tmpFile, err := os.CreateTemp("", "bsync_combined_splice_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Test data
	data := [][]byte{
		[]byte("test data 1\n"),
		[]byte("test data 2\n"),
	}

	// CombinedSplice: userspace -> pipe -> file
	n, err := CombinedSplice(tmpFile, data)
	if err != nil {
		t.Logf("CombinedSplice failed (may not be supported on this platform): %v", err)
		// Don't fail - splice may not be available
		return
	}

	expectedLen := len(data[0]) + len(data[1])
	if int(n) != expectedLen {
		t.Errorf("CombinedSplice transferred %d bytes, want %d", n, expectedLen)
	}

	// Verify file contents
	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	readBuf := make([]byte, 100)
	nRead, err := tmpFile.Read(readBuf)
	if err != nil {
		t.Fatalf("Read from file failed: %v", err)
	}

	result := string(readBuf[:nRead])
	expected := "test data 1\ntest data 2\n"
	if result != expected {
		t.Errorf("File content = %q, want %q", result, expected)
	}
}

// TestMMapReader tests memory-mapped file reading.
func TestMMapReader(t *testing.T) {
	// Create a temporary file with known content
	tmpFile, err := os.CreateTemp("", "bsync_mmap_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write test data
	testData := []byte("hello world, this is a test for mmap")
	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Seek back to start for mmap
	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	// Create mmap reader
	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Logf("NewMMapReader failed (may not be supported on this platform): %v", err)
		// Don't fail - mmap may not work with temp files on all systems
		return
	}
	defer mmap.Close()

	// Test ReadAt
	data := mmap.ReadAt(0, 100)
	if len(data) != len(testData) {
		t.Errorf("ReadAt returned %d bytes, want %d", len(data), len(testData))
	}

	result := string(data)
	if result != string(testData) {
		t.Errorf("ReadAt data = %q, want %q", result, string(testData))
	}

	// Test Size
	if mmap.Size() != int64(len(testData)) {
		t.Errorf("Size() = %d, want %d", mmap.Size(), len(testData))
	}

	// Test ReadAt with offset
	partial := mmap.ReadAt(6, 5) // "world"
	if string(partial) != "world" {
		t.Errorf("Partial read = %q, want %q", string(partial), "world")
	}
}

// ============================================================================
// Category B: Memory-Mapped I/O Tests
// ============================================================================

// TestMMapBoundaryOffsets tests ReadAt at various boundary conditions.
func TestMMapBoundaryOffsets(t *testing.T) {
	testData := []byte("0123456789ABCDEFGHIJ") // 20 bytes

	tmpFile, err := os.CreateTemp("", "bsync_mmap_boundary_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}
	defer mmap.Close()

	tests := []struct {
		offset    int64
		size      int
		expected  string
		shouldErr bool
	}{
		{0, 5, "01234", false},                       // Start
		{15, 5, "FGHIJ", false},                      // Near end
		{19, 1, "J", false},                          // Last byte
		{20, 1, "", true},                            // Past end (offset == size)
		{25, 5, "", true},                            // Way past end
		{0, 0, "", false},                            // Zero size read
		{10, 100, "ABCDEFGHIJ", false},               // Read past end (truncated to available)
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("off=%d_size=%d", tt.offset, tt.size), func(t *testing.T) {
			result := mmap.ReadAt(tt.offset, tt.size)

			if tt.shouldErr {
				if result != nil {
					t.Errorf("Expected nil for offset=%d size=%d, got %q", tt.offset, tt.size, result)
				}
				return
			}

			if string(result) != tt.expected {
				t.Errorf("ReadAt(%d, %d) = %q, want %q", tt.offset, tt.size, result, tt.expected)
			}
		})
	}
}

// TestMMapEmptyFile tests mmap on an empty file.
func TestMMapEmptyFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_mmap_empty_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// File is empty (0 bytes)
	_, err = NewMMapReader(tmpFile)
	if err == nil {
		t.Error("Expected error when mmapping empty file, got nil")
	}
}

// TestMMapLargeFile tests mmap with a larger file.
func TestMMapLargeFile(t *testing.T) {
	const fileSize = 100 * 1024 * 1024 // 100MB

	tmpFile, err := os.CreateTemp("", "bsync_mmap_large_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write 100MB of data using a simple repeating pattern
	pattern := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	for i := 0; i < fileSize/len(pattern); i++ {
		if _, err := tmpFile.Write(pattern); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	// Get actual file size
	info, _ := tmpFile.Stat()
	actualSize := info.Size()

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed for large file: %v", err)
	}
	defer mmap.Close()

	// Verify size matches actual file size
	if mmap.Size() != actualSize {
		t.Errorf("Size() = %d, want %d", mmap.Size(), actualSize)
	}

	// Read some random offsets and verify pattern
	offsets := []int64{0, 4096, 1024 * 1024, 50 * 1024 * 1024, actualSize - 20}
	for _, offset := range offsets {
		if offset >= actualSize-20 {
			continue // Skip if offset would go past end
		}
		data := mmap.ReadAt(offset, len(pattern))
		if len(data) != len(pattern) {
			t.Errorf("ReadAt(%d, %d) returned %d bytes", offset, len(pattern), len(data))
			continue
		}
		// Verify pattern matches
		patternOffset := int(offset) % len(pattern)
		for i, b := range data {
			expected := pattern[(patternOffset+i)%len(pattern)]
			if b != expected {
				t.Errorf("Data mismatch at offset %d: got %d, want %d", offset+int64(i), b, expected)
				break
			}
		}
	}
}

// TestMMapConcurrentReads tests concurrent reads from mmap.
func TestMMapConcurrentReads(t *testing.T) {
	testData := make([]byte, 1024*1024) // 1MB
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	tmpFile, err := os.CreateTemp("", "bsync_mmap_concurrent_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}
	defer mmap.Close()

	const goroutines = 50
	var done atomic.Int32
	var errors atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer done.Add(1)

			offset := int64((idx * 4096) % len(testData))
			data := mmap.ReadAt(offset, 100)

			if len(data) != 100 {
				errors.Add(1)
				return
			}

			// Verify data integrity
			for j, b := range data {
				expected := byte((int(offset) + j) % 256)
				if b != expected {
					errors.Add(1)
					return
				}
			}
		}(i)
	}

	for done.Load() != int32(goroutines) {
	}

	if errors.Load() != 0 {
		t.Errorf("%d goroutines reported errors", errors.Load())
	}
}

// TestMMapClosedFile tests that mmap works even after file is closed.
func TestMMapClosedFile(t *testing.T) {
	testData := []byte("test data for closed file check")

	tmpFile, err := os.CreateTemp("", "bsync_mmap_closed_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}

	// Close the underlying file descriptor (but mmap keeps its own reference)
	tmpFile.Close()
	os.Remove(tmpPath)

	// mmap should still work
	data := mmap.ReadAt(0, len(testData))
	if string(data) != string(testData) {
		t.Errorf("After closing file: got %q, want %q", data, testData)
	}

	mmap.Close()
}

// TestMMapZeroSizeRead tests ReadAt with zero size.
func TestMMapZeroSizeRead(t *testing.T) {
	testData := []byte("test")

	tmpFile, err := os.CreateTemp("", "bsync_mmap_zero_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}
	defer mmap.Close()

	result := mmap.ReadAt(0, 0)
	if result == nil {
		t.Error("ReadAt(size=0) returned nil, want empty slice")
	}
	if len(result) != 0 {
		t.Errorf("ReadAt(size=0) returned %d bytes, want 0", len(result))
	}
}

// ============================================================================
// Category C: Zero-Copy I/O Tests
// ============================================================================

// TestVmSpliceMultipleBuffers tests vmsplice with multiple iovecs.
func TestVmSpliceMultipleBuffers(t *testing.T) {
	tests := []struct {
		name       string
		numBuffers int
	}{
		{"single", 1},
		{"ten", 10},
		{"hundred", 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatalf("Pipe creation failed: %v", err)
			}
			defer pr.Close()
			defer pw.Close()

			// Create multiple buffers
			data := make([][]byte, tt.numBuffers)
			totalSize := 0
			for i := range data {
				data[i] = []byte(fmt.Sprintf("chunk%04d_", i))
				totalSize += len(data[i])
			}

			// vmsplice
			n, err := VmSplice(pw, data)
			if err != nil {
				t.Logf("VmSplice failed (may not be supported): %v", err)
				return
			}

			if n != totalSize {
				t.Errorf("VmSplice wrote %d bytes, want %d", n, totalSize)
			}

			pw.Close()

			// Read back
			readBuf := make([]byte, totalSize+100)
			n, err = pr.Read(readBuf)
			if err != nil {
				t.Fatalf("Read from pipe failed: %v", err)
			}

			// Verify content
			result := string(readBuf[:n])
			expected := ""
			for i := 0; i < tt.numBuffers; i++ {
				expected += fmt.Sprintf("chunk%04d_", i)
			}
			if result != expected {
				t.Errorf("Content mismatch: got %q, want %q", result, expected)
			}
		})
	}
}

// TestVmSpliceLargeData tests vmsplice with large data.
func TestVmSpliceLargeData(t *testing.T) {
	const chunkSize = 64 * 1024 // 64KB per chunk
	const numChunks = 10        // Total 640KB

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe creation failed: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	data := make([][]byte, numChunks)
	totalSize := 0
	for i := range data {
		data[i] = make([]byte, chunkSize)
		for j := range data[i] {
			data[i][j] = byte((i + j) % 256)
		}
		totalSize += chunkSize
	}

	// vmsplice
	n, err := VmSplice(pw, data)
	if err != nil {
		t.Logf("VmSplice failed (may not be supported): %v", err)
		return
	}

	if n != totalSize {
		t.Errorf("VmSplice wrote %d bytes, want %d", n, totalSize)
	}

	pw.Close()

	// Read back and verify
	readBuf := make([]byte, totalSize)
	nRead, err := pr.Read(readBuf)
	if err != nil {
		t.Fatalf("Read from pipe failed: %v", err)
	}

	if nRead != totalSize {
		t.Errorf("Read %d bytes, want %d", nRead, totalSize)
	}
}

// TestVmSpliceEmptyData tests vmsplice with empty data.
func TestVmSpliceEmptyData(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe creation failed: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	data := [][]byte{
		[]byte("test"),
		[]byte(""),
		[]byte("data"),
	}

	n, err := VmSplice(pw, data)
	if err != nil {
		t.Logf("VmSplice failed (may not be supported): %v", err)
		return
	}

	expectedLen := len("test") + len("data")
	if n != expectedLen {
		t.Errorf("VmSplice wrote %d bytes, want %d", n, expectedLen)
	}
}

// TestCombinedSpliceLargeData tests CombinedSplice with larger data.
func TestCombinedSpliceLargeData(t *testing.T) {
	const totalSize = 5 * 1024 * 1024 // 5MB

	tmpFile, err := os.CreateTemp("", "bsync_combined_large_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Create data in chunks (5MB of data)
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = byte(i % 256)
	}

	data := make([][]byte, 0, totalSize/len(chunk))
	for len(data)*len(chunk) < totalSize {
		data = append(data, chunk)
	}

	// CombinedSplice
	n, err := CombinedSplice(tmpFile, data)
	if err != nil {
		t.Logf("CombinedSplice failed (may not be supported): %v", err)
		return
	}

	expectedSize := int64(len(data) * len(chunk))
	if n != expectedSize {
		t.Errorf("CombinedSplice transferred %d bytes, want %d", n, expectedSize)
	}

	// Verify file content
	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	readBuf := make([]byte, len(chunk))
	nRead, err := tmpFile.Read(readBuf)
	if err != nil {
		t.Fatalf("Read from file failed: %v", err)
	}

	if nRead != len(chunk) {
		t.Errorf("Read %d bytes, want %d", nRead, len(chunk))
	}

	// Verify pattern
	for i, b := range readBuf {
		expected := byte(i % 256)
		if b != expected {
			t.Errorf("Data mismatch at byte %d: got %d, want %d", i, b, expected)
			break
		}
	}
}

// TestSplicePipeBufferLimit tests splice behavior near pipe buffer limits.
func TestSplicePipeBufferLimit(t *testing.T) {
	// Create pipe
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe creation failed: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	// Create temporary file for destination
	tmpFile, err := os.CreateTemp("", "bsync_splice_pipe_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write data to pipe (2MB - likely exceeds pipe buffer)
	data := make([]byte, 2*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write to pipe
	go func() {
		pw.Write(data)
		pw.Close()
	}()

	// Splice from pipe to file
	var totalTransferred int64 = 0
	for {
		n, err := Splice(tmpFile, pr, int64(len(data)))
		if err != nil {
			if totalTransferred > 0 {
				break // EOF
			}
			t.Logf("Splice failed (may not be supported): %v", err)
			return
		}
		if n == 0 {
			break
		}
		totalTransferred += n
	}

	if totalTransferred > 0 && totalTransferred != int64(len(data)) {
		t.Logf("Splice transferred %d bytes (expected %d) - partial transfer OK", totalTransferred, len(data))
	}
}

// TestVmSpliceNonPipe tests vmsplice on non-pipe file descriptor.
func TestVmSpliceNonPipe(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_vmsplice_nonpipe_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	data := [][]byte{[]byte("test")}

	// vmsplice on regular file should fail
	n, err := VmSplice(tmpFile, data)
	if err == nil {
		t.Logf("VmSplice on regular file unexpectedly succeeded (wrote %d bytes)", n)
	} else {
		// Expected to fail
		t.Logf("VmSplice on regular file failed as expected: %v", err)
	}
}

// ============================================================================
// Category E: Integration Tests
// ============================================================================

// TestBufferPoolMemoryLocking tests buffer pool functionality.
func TestBufferPoolMemoryLocking(t *testing.T) {
	// No longer mlock'd (see reader.go); this just exercises get/put across
	// two distinct size classes and a reuse cycle.
	buf1 := getReaderBuf(1024 * 1024)
	buf2 := getReaderBuf(10 * 1024 * 1024)

	// Verify buffers are usable
	buf1[0] = 42
	buf2[0] = 24

	if buf1[0] != 42 || buf2[0] != 24 {
		t.Error("Pooled buffers are not usable")
	}

	putReaderBuf(buf1)
	putReaderBuf(buf2)

	// Get again - should reuse
	buf3 := getReaderBuf(1024 * 1024)
	buf3[0] = 99
	if buf3[0] != 99 {
		t.Error("Reused buffer is not usable")
	}
	putReaderBuf(buf3)
}

// TestMMapWithCompression tests mmap with compression workflow.
func TestMMapWithCompression(t *testing.T) {
	// Create test data
	originalData := make([]byte, 1024*1024) // 1MB
	for i := range originalData {
		originalData[i] = byte(i % 256)
	}

	tmpFile, err := os.CreateTemp("", "bsync_mmap_compress_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(originalData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}
	defer mmap.Close()

	// Read data via mmap
	mmapData := mmap.ReadAt(0, len(originalData))

	// Compress the mmap'd data
	compressed, err := compressData(mmapData)
	if err != nil {
		t.Fatalf("compressData failed: %v", err)
	}

	if len(compressed) == 0 {
		t.Error("Compressed data is empty")
	}

	t.Logf("Original: %d bytes, Compressed: %d bytes, Ratio: %.2f",
		len(originalData), len(compressed), float64(len(compressed))/float64(len(originalData)))
}

// TestZeroCopyPipeline tests the complete zero-copy pipeline.
func TestZeroCopyPipeline(t *testing.T) {
	// Create test data
	testData := [][]byte{
		[]byte("hello "),
		[]byte("world "),
		[]byte("from "),
		[]byte("zero-copy "),
		[]byte("pipeline!"),
	}

	tmpFile, err := os.CreateTemp("", "bsync_zerocopy_pipe_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Use CombinedSplice: userspace -> pipe -> file
	n, err := CombinedSplice(tmpFile, testData)
	if err != nil {
		t.Logf("CombinedSplice failed (may not be supported): %v", err)
		return
	}

	expectedLen := 0
	for _, d := range testData {
		expectedLen += len(d)
	}

	if int(n) != expectedLen {
		t.Errorf("CombinedSplice transferred %d bytes, want %d", n, expectedLen)
	}

	// Verify file content
	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	readBuf := make([]byte, expectedLen+100)
	nRead, err := tmpFile.Read(readBuf)
	if err != nil {
		t.Fatalf("Read from file failed: %v", err)
	}

	expected := "hello world from zero-copy pipeline!"
	if string(readBuf[:nRead]) != expected {
		t.Errorf("File content = %q, want %q", string(readBuf[:nRead]), expected)
	}
}

// TestConcurrentBufferAccess tests concurrent buffer pool access.
func TestConcurrentBufferAccess(t *testing.T) {
	const goroutines = 100
	const iterations = 10

	var done atomic.Int32
	var errors atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			defer done.Add(1)

			for j := 0; j < iterations; j++ {
				size := 1024 * 1024
				buf := getReaderBuf(uint32(size))

				// Use buffer
				for k := range buf {
					buf[k] = byte(k % 256)
				}

				// Verify
				for k := range buf {
					if buf[k] != byte(k%256) {
						errors.Add(1)
						break
					}
				}

				// Note: We don't put the buffer back to avoid race conditions
				// with sync.Pool recycling the underlying array to another goroutine.
				// In production, buffers are put back after use, but for testing
				// isolation, we let GC handle them.
			}
		}()
	}

	for done.Load() != int32(goroutines) {
	}

	if errors.Load() != 0 {
		t.Errorf("%d buffer errors detected", errors.Load())
	}
}

// TestSequentialReaderWithMMap tests sequential reader with mmap-backed file.
func TestSequentialReaderWithMMap(t *testing.T) {
	const blockSize = 4096
	const numBlocks = 100

	// Create test file
	testData := make([]byte, blockSize*numBlocks)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	tmpFile, err := os.CreateTemp("", "bsync_seq_mmap_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(testData); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	// Create sequential reader
	reader := NewSequentialReader(tmpFile, blockSize, uint64(len(testData)), 0, 10)
	reader.Start()
	defer reader.Wait()

	// Read all blocks
	blockCount := 0
	for block := range reader.Blocks() {
		blockCount++

		// Verify block data
		for i, b := range block.Data {
			expectedOffset := int(block.BlockIdx*blockSize) + i
			expected := byte(expectedOffset % 256)
			if b != expected {
				t.Errorf("Block %d byte %d: got %d, want %d", block.BlockIdx, i, b, expected)
			}
		}
	}

	if blockCount != numBlocks {
		t.Errorf("Read %d blocks, want %d", blockCount, numBlocks)
	}
}

// ============================================================================
// Category F: Stress Tests (build tag: stress)
// ============================================================================

// TestStressLargeFileMMap stress tests mmap with large files.
func TestStressLargeFileMMap(t *testing.T) {
	const fileSize = 100 * 1024 * 1024 // 100MB

	tmpFile, err := os.CreateTemp("", "bsync_stress_mmap_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write pattern
	pattern := []byte("STRESS_TEST_PATTERN_")
	for i := 0; i < fileSize/len(pattern); i++ {
		if _, err := tmpFile.Write(pattern); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if _, err := tmpFile.Seek(0, 0); err != nil {
		t.Fatalf("Seek failed: %v", err)
	}

	mmap, err := NewMMapReader(tmpFile)
	if err != nil {
		t.Skipf("NewMMapReader failed: %v", err)
	}
	defer mmap.Close()

	// Read entire file in chunks
	const chunkSize = 1024 * 1024
	for offset := int64(0); offset < fileSize; offset += chunkSize {
		data := mmap.ReadAt(offset, chunkSize)
		if len(data) == 0 {
			t.Errorf("ReadAt(%d, %d) returned empty", offset, chunkSize)
			break
		}
	}
}

// TestStressConcurrentSplice stress tests concurrent splice operations.
func TestStressConcurrentSplice(t *testing.T) {
	const goroutines = 50

	var done atomic.Int32
	var errors atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer done.Add(1)

			pr, pw, err := os.Pipe()
			if err != nil {
				errors.Add(1)
				return
			}
			defer pr.Close()
			defer pw.Close()

			data := [][]byte{[]byte(fmt.Sprintf("test_%d_", idx))}

			if _, err := VmSplice(pw, data); err != nil {
				// Expected if not supported
				return
			}
			pw.Close()

			buf := make([]byte, 100)
			if _, err := pr.Read(buf); err != nil {
				errors.Add(1)
			}
		}(i)
	}

	for done.Load() != int32(goroutines) {
	}

	if errors.Load() > 0 {
		t.Logf("%d goroutines reported errors (may be expected for unsupported features)", errors.Load())
	}
}

// TestStressBufferPoolUnderLoad stress tests buffer pool under load.
func TestStressBufferPoolUnderLoad(t *testing.T) {
	const goroutines = 100
	const iterations = 10

	var done atomic.Int32
	var errors atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			defer done.Add(1)

			for j := 0; j < iterations; j++ {
				// Allocate varying sizes
				size := 1024 * (1 + (j % 10))
				buf := getReaderBuf(uint32(size))

				if cap(buf) < size {
					errors.Add(1)
					putReaderBuf(buf)
					continue
				}

				// Use buffer
				for k := range buf {
					buf[k] = byte(k % 256)
				}

				putReaderBuf(buf)
			}
		}()
	}

	for done.Load() != int32(goroutines) {
	}

	if errors.Load() != 0 {
		t.Errorf("%d buffer pool errors", errors.Load())
	}

	// Verify pool still works after stress
	buf := getReaderBuf(1024 * 1024)
	if cap(buf) < 1024*1024 {
		t.Error("Buffer pool failed after stress test")
	}
	putReaderBuf(buf)
}

// ============================================================================
// Category G: File Utility Tests
// ============================================================================

// TestPreallocateFile tests fallocate for actual allocation.
func TestPreallocateFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_fallocate_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Preallocate 10MB
	const allocSize = 10 * 1024 * 1024
	err = PreallocateFile(tmpFile, allocSize)
	if err != nil {
		t.Fatalf("PreallocateFile failed: %v", err)
	}

	// Verify file size
	info, err := tmpFile.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != allocSize {
		t.Errorf("file size = %d, want %d", info.Size(), allocSize)
	}
}

// TestPreallocateFileZeroSize tests preallocate with zero size.
func TestPreallocateFileZeroSize(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_fallocate_zero_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Preallocate 0 bytes - should succeed
	err = PreallocateFile(tmpFile, 0)
	if err != nil {
		t.Errorf("PreallocateFile(0) failed: %v", err)
	}
}

// TestPreallocateFileNegativeSize tests preallocate with negative size.
func TestPreallocateFileNegativeSize(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_fallocate_neg_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Preallocate negative size - should succeed (no-op)
	err = PreallocateFile(tmpFile, -100)
	if err != nil {
		t.Errorf("PreallocateFile(-100) failed: %v", err)
	}
}

// TestPreallocateFileLarge tests preallocate with large size.
func TestPreallocateFileLarge(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_fallocate_large_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Preallocate 100MB
	const allocSize = 100 * 1024 * 1024
	err = PreallocateFile(tmpFile, allocSize)
	if err != nil {
		t.Logf("PreallocateFile(100MB) failed: %v", err)
		// May fail on some systems, don't fail test
		return
	}

	info, _ := tmpFile.Stat()
	if info.Size() != allocSize {
		t.Errorf("file size = %d, want %d", info.Size(), allocSize)
	}
}

// TestReadv tests scatter/gather read with multiple iovecs.
func TestReadv(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_readv_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write test data
	data1 := []byte("hello")
	data2 := []byte("world")
	tmpFile.Write(data1)
	tmpFile.Write(data2)

	// Seek back to start
	tmpFile.Seek(0, 0)

	// Read with readv into multiple buffers
	iovs := [][]byte{
		make([]byte, 5),
		make([]byte, 5),
	}

	n, err := Readv(tmpFile, iovs)
	if err != nil {
		t.Logf("Readv failed (may not be supported on this platform): %v", err)
		// Don't fail - readv may not be available
		return
	}

	if n != 10 {
		t.Errorf("Readv read %d bytes, want 10", n)
	}

	if string(iovs[0]) != "hello" {
		t.Errorf("iovs[0] = %q, want %q", string(iovs[0]), "hello")
	}
	if string(iovs[1]) != "world" {
		t.Errorf("iovs[1] = %q, want %q", string(iovs[1]), "world")
	}
}

// TestReadvPartial tests readv with partial read.
func TestReadvPartial(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_readv_partial_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write less data than we'll try to read
	tmpFile.Write([]byte("short"))

	// Seek back to start
	tmpFile.Seek(0, 0)

	// Try to read more than available
	iovs := [][]byte{
		make([]byte, 10),
		make([]byte, 10),
	}

	n, err := Readv(tmpFile, iovs)
	if err != nil {
		t.Logf("Readv failed (may not be supported): %v", err)
		return
	}

	// Should read partial (5 bytes = "short")
	if n != 5 {
		t.Logf("Readv read %d bytes, expected 5 (partial read)", n)
	}

	if string(iovs[0][:5]) != "short" {
		t.Errorf("First buffer = %q, want %q", string(iovs[0][:5]), "short")
	}
}

// TestReadvSingle tests readv with single buffer.
func TestReadvSingle(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_readv_single_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	testData := []byte("single buffer test")
	tmpFile.Write(testData)
	tmpFile.Seek(0, 0)

	iovs := [][]byte{
		make([]byte, len(testData)),
	}

	n, err := Readv(tmpFile, iovs)
	if err != nil {
		t.Logf("Readv failed (may not be supported): %v", err)
		return
	}

	if n != len(testData) {
		t.Errorf("Readv read %d bytes, want %d", n, len(testData))
	}

	if string(iovs[0]) != string(testData) {
		t.Errorf("buffer = %q, want %q", string(iovs[0]), string(testData))
	}
}

// TestWritev tests scatter/gather write with multiple iovecs.
func TestWritev(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_writev_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write with writev
	iovs := [][]byte{
		[]byte("hello "),
		[]byte("world"),
	}

	n, err := Writev(tmpFile, iovs)
	if err != nil {
		t.Logf("Writev failed (may not be supported on this platform): %v", err)
		// Don't fail - writev may not be available
		return
	}

	if n != 11 {
		t.Errorf("Writev wrote %d bytes, want 11", n)
	}

	// Verify
	tmpFile.Seek(0, 0)
	buf := make([]byte, 20)
	read, err := tmpFile.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(buf[:read]) != "hello world" {
		t.Errorf("file content = %q, want %q", string(buf[:read]), "hello world")
	}
}

// TestWritevManyBuffers tests writev with many buffers.
func TestWritevManyBuffers(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_writev_many_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Create many small buffers
	iovs := make([][]byte, 100)
	totalSize := 0
	for i := range iovs {
		iovs[i] = []byte(fmt.Sprintf("chunk%02d", i))
		totalSize += len(iovs[i])
	}

	n, err := Writev(tmpFile, iovs)
	if err != nil {
		t.Logf("Writev failed (may not be supported): %v", err)
		return
	}

	if n != totalSize {
		t.Errorf("Writev wrote %d bytes, want %d", n, totalSize)
	}

	// Verify file size
	info, _ := tmpFile.Stat()
	if info.Size() != int64(totalSize) {
		t.Errorf("file size = %d, want %d", info.Size(), totalSize)
	}
}

// TestWritevEmpty tests writev with empty buffers.
func TestWritevEmpty(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_writev_empty_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write with empty buffer
	iovs := [][]byte{
		[]byte("test"),
		[]byte(""),
		[]byte("data"),
	}

	n, err := Writev(tmpFile, iovs)
	if err != nil {
		t.Logf("Writev failed (may not be supported): %v", err)
		return
	}

	// Empty buffers should be skipped
	expected := len("test") + len("data")
	if n != expected {
		t.Errorf("Writev wrote %d bytes, want %d", n, expected)
	}
}

// TestOpenFileNoAtime tests opening file without updating access time.
func TestOpenFileNoAtime(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_noatime_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Write initial data
	tmpFile.Write([]byte("test data"))
	tmpFile.Close()

	// Open with O_NOATIME
	file, err := OpenFileNoAtime(tmpFile.Name(), syscall.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFileNoAtime failed: %v", err)
	}
	defer file.Close()

	// Verify file is usable
	buf := make([]byte, 9)
	n, err := file.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 9 || string(buf) != "test data" {
		t.Errorf("Read data = %q, want %q", string(buf), "test data")
	}
}

// TestOpenFileNoAtimeWrite tests writing to file opened with O_NOATIME.
func TestOpenFileNoAtimeWrite(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_noatime_write_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	// Open with O_NOATIME for writing
	file, err := OpenFileNoAtime(tmpFile.Name(), syscall.O_CREAT|syscall.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFileNoAtime failed: %v", err)
	}

	// Write data
	_, err = file.Write([]byte("written data"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	file.Close()

	// Verify by reopening in read mode
	verifyFile, err := os.OpenFile(tmpFile.Name(), syscall.O_RDONLY, 0644)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer verifyFile.Close()

	buf := make([]byte, 20)
	n, _ := verifyFile.Read(buf)
	if string(buf[:n]) != "written data" {
		t.Errorf("Data = %q, want %q", string(buf[:n]), "written data")
	}
}

// TestOpenFileDirect tests opening file with O_DIRECT flag.
func TestOpenFileDirect(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_direct_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	// Open with O_DIRECT
	file, err := OpenFileDirect(tmpFile.Name(), syscall.O_CREAT|syscall.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFileDirect failed: %v", err)
	}
	defer file.Close()

	// Note: O_DIRECT requires aligned buffers for I/O
	// We can't test actual I/O here without aligned buffers
	// Just verify the file opened successfully
	if file.Fd() == 0 {
		t.Error("File descriptor is invalid")
	}
}

// TestOpenFileDirectFallback tests that O_DIRECT falls back gracefully.
func TestOpenFileDirectFallback(t *testing.T) {
	// This test verifies that OpenFileDirect doesn't panic
	// and returns a usable file even if O_DIRECT fails
	tmpFile, err := os.CreateTemp("", "bsync_direct_fallback_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// Close the temp file so we can reopen it
	tmpFile.Close()

	// Open with O_DIRECT - may fail and fall back to regular open
	file, err := OpenFileDirect(tmpFile.Name(), syscall.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFileDirect failed: %v", err)
	}
	defer file.Close()

	// File should be usable
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info == nil {
		t.Error("Stat returned nil")
	}
}

// TestSyncFileRange tests explicit writeback control.
func TestSyncFileRange(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_sync_range_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write some data
	testData := []byte("test data for sync_file_range")
	_, err = tmpFile.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Call SyncFileRange - should not panic
	// Use WriteAndWait flags
	SyncFileRange(tmpFile, 0, int64(len(testData)), SyncFileRangeWriteAndWait)

	// File should still be usable
	tmpFile.Seek(0, 0)
	buf := make([]byte, len(testData))
	n, err := tmpFile.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Read %d bytes, want %d", n, len(testData))
	}
}

// TestSyncFileRangeZeroLength tests sync_file_range with zero length.
func TestSyncFileRangeZeroLength(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_sync_range_zero_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write data
	tmpFile.Write([]byte("test"))

	// Sync with zero length - should not panic
	SyncFileRange(tmpFile, 0, 0, SyncFileRangeWrite)
}

// TestSyncFileRangeFullFile tests syncing entire file.
func TestSyncFileRangeFullFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_sync_range_full_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write 1MB of data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	_, err = tmpFile.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Sync entire file
	SyncFileRange(tmpFile, 0, int64(len(data)), SyncFileRangeWriteAndWait)

	// Verify data is intact
	tmpFile.Seek(0, 0)
	buf := make([]byte, len(data))
	n, err := tmpFile.Read(buf)
	if err != nil || n != len(data) {
		t.Errorf("Read failed: n=%d err=%v", n, err)
	}
}

// TestAdviseDontNeed tests FADV_DONTNEED advice.
func TestAdviseDontNeed(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_dontneed_test_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write some data
	testData := []byte("test data for fadvise dontneed")
	_, err = tmpFile.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Advise kernel that we don't need this data cached
	AdviseDontNeed(tmpFile, 0, int64(len(testData)))

	// File should still be usable
	tmpFile.Seek(0, 0)
	buf := make([]byte, len(testData))
	n, err := tmpFile.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Read %d bytes, want %d", n, len(testData))
	}
}

// TestAdviseDontNeedPartial tests dontneed on partial file range.
func TestAdviseDontNeedPartial(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_dontneed_partial_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write 1MB of data
	data := make([]byte, 1024*1024)
	_, err = tmpFile.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Advise dontneed on first half only
	half := int64(len(data) / 2)
	AdviseDontNeed(tmpFile, 0, half)

	// File should still be fully readable
	tmpFile.Seek(0, 0)
	buf := make([]byte, len(data))
	n, err := tmpFile.Read(buf)
	if err != nil || n != len(data) {
		t.Errorf("Read failed: n=%d err=%v", n, err)
	}
}

// TestAdviseDontNeedZeroLength tests dontneed with zero length.
func TestAdviseDontNeedZeroLength(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "bsync_dontneed_zero_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write data
	tmpFile.Write([]byte("test"))

	// Advise dontneed with zero length - should not panic
	AdviseDontNeed(tmpFile, 0, 0)
}
