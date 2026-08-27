//go:build linux

package main

import "syscall"

// madviseSequential advises the kernel that data (an mmap'd region) will be
// accessed sequentially. Best-effort; failure is silently ignored, matching
// every other advisory call in this codebase.
func madviseSequential(data []byte) {
	_ = syscall.Madvise(data, syscall.MADV_SEQUENTIAL)
}
