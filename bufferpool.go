package main

import (
	"sync"
)

var (
	payloadPool = sync.Pool{
		New: func() interface{} {
			// Pre-allocate with 10MB capacity for typical block size
			return make([]byte, 0, 10*1024*1024)
		},
	}
)

// GetPayload copies data into a pooled buffer and returns it.
// The returned buffer is owned by the caller until PutPayload is called.
func GetPayload(data []byte) []byte {
	buf := payloadPool.Get().([]byte)

	// If capacity is insufficient, allocate new buffer
	if cap(buf) < len(data) {
		buf = make([]byte, len(data))
	} else {
		buf = buf[:len(data)]
	}

	copy(buf, data)
	return buf
}

// PutPayload returns a buffer to the pool for reuse.
// The buffer length is reset to 0 but capacity is preserved.
func PutPayload(buf []byte) {
	payloadPool.Put(buf[:0]) // Reset length but keep capacity
}
