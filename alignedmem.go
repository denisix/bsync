package main

import (
	"sync"
	"unsafe"
)

// AlignedBuffer allocates a buffer with 4K alignment for O_DIRECT I/O.
// The extra memory allocated for alignment will be GC'd when the slice is no longer referenced.
// The returned buffer is locked in memory (mlock) to prevent swap.
func AlignedBuffer(size int) []byte {
	// Allocate extra space for alignment (4096 = 4K)
	raw := make([]byte, size+4096)

	// Calculate aligned offset
	// We want (ptr + offset) % 4096 == 0
	// So offset = 4096 - (ptr % 4096)
	ptr := uintptr(unsafe.Pointer(&raw[0]))
	offset := 4096 - (ptr % 4096)

	buf := raw[offset : offset+uintptr(size)]

	// Lock the aligned portion to prevent swap
	TryLockBuffer(buf)

	return buf
}

// IsAligned checks if a pointer is aligned to the specified boundary.
func IsAligned(ptr uintptr, alignment int) bool {
	return ptr%uintptr(alignment) == 0
}

// AlignedReaderBufPool provides 4K-aligned buffers for Direct I/O reads.
// All pool buffers are locked in memory on creation and use huge pages.
var AlignedReaderBufPool = struct {
	sync.Pool
}{
	Pool: sync.Pool{
		New: func() interface{} {
			// Pre-allocate 10MB aligned buffer (automatically locked)
			buf := AlignedBuffer(10 * 1024 * 1024)
			// Advise kernel to use huge pages for better TLB utilization
			AdviseHugePage(buf)
			return buf
		},
	},
}

// getAlignedReaderBuf gets an aligned buffer from the pool or allocates a new one.
func getAlignedReaderBuf(size uint32) []byte {
	buf := AlignedReaderBufPool.Get().([]byte)
	if cap(buf) < int(size) {
		// Pool buffer too small, allocate new aligned one (not returned to pool)
		// New buffers are automatically locked
		return AlignedBuffer(int(size))
	}
	return buf[:size]
}

// putAlignedReaderBuf returns a buffer to the pool if it's appropriately sized.
func putAlignedReaderBuf(buf []byte) {
	if cap(buf) <= 10*1024*1024 {
		// Check if actually aligned before returning to pool
		if IsAligned(uintptr(unsafe.Pointer(&buf[0])), 4096) {
			AlignedReaderBufPool.Put(buf[:0])
		}
	}
	// Non-aligned or large buffers are GC'd instead of polluting the pool
	// Note: munlock happens automatically when GC frees the memory
}

