//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// madviseSequential advises the kernel that data (an mmap'd region) will be
// accessed sequentially. Darwin's syscall package doesn't wrap madvise like
// it does Mlock/Mlockall, but the underlying SYS_MADVISE number and
// MADV_SEQUENTIAL constant are both available, so it's called directly the
// same way the Linux-only raw syscalls in fileutil.go are. Best-effort;
// failure is silently ignored, matching every other advisory call in this
// codebase.
func madviseSequential(data []byte) {
	if len(data) == 0 {
		return
	}
	syscall.Syscall(syscall.SYS_MADVISE, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(syscall.MADV_SEQUENTIAL))
}
