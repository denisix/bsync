package main

import (
	"io"
	"os"
	"sync"
	"unsafe"
)

// Shared zero buffer for writing zero blocks (1MB max)
var (
	zeroBuf    []byte
	zeroBufMu  sync.Mutex
	zeroBufMax = 1024 * 1024 // 1MB
)

func getZeroBuf(size int) []byte {
	if size <= zeroBufMax {
		zeroBufMu.Lock()
		defer zeroBufMu.Unlock()
		if zeroBuf == nil {
			zeroBuf = make([]byte, zeroBufMax)
		}
		return zeroBuf[:size]
	}
	return make([]byte, size)
}

func getDeviceSize(file *os.File) uint64 {
	pos, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		Err("seeking to end of file: %v\n", err)
	}
	file.Seek(0, io.SeekStart)
	Log("file size -> %d bytes.\n", pos)
	return uint64(pos)
}

// isZeroBlock checks if all bytes are zero using optimized 8-byte comparison
func isZeroBlock(b []byte) bool {
	// Check 8 bytes at a time for better performance
	if len(b) < 8 {
		for _, v := range b {
			if v != 0 {
				return false
			}
		}
		return true
	}

	// Use unsafe pointer arithmetic for fast comparison
	n := len(b)
	for i := 0; i < n-7; i += 8 {
		if *(*uint64)(unsafe.Pointer(&b[i])) != 0 {
			return false
		}
	}

	// Check remaining bytes
	for i := n - (n % 8); i < n; i++ {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

func truncateIfRegularFile(file *os.File, size uint64) {
	info, err := file.Stat()
	if err != nil {
		Log("Error: file stat failed: %v\n", err)
	}

	mode := info.Mode()
	isBlock := mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0

		if !isBlock && mode.IsRegular() {
		currentSize := uint64(info.Size())
		if currentSize != size {
			if err := file.Truncate(int64(size)); err != nil {
				Err("Error: truncate failed: %v\n", err)
			}
			Log("file truncated to %d bytes\n", size)
		}
		// else: skip truncate (no message)
	}
}
