package main

import (
	"sync"
	"unsafe"
)

// Shared zero buffer for writing zero blocks (100MB max to handle typical block sizes)
var (
	zeroBuf    []byte
	zeroBufMu  sync.Mutex
	zeroBufMax = 100 * 1024 * 1024 // 100MB - covers most block sizes
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

