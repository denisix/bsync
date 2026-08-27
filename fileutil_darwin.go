//go:build darwin

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	SyncFileRangeWrite        = 2 // Start writeout of dirty pages
	SyncFileRangeWaitBefore   = 4 // Wait for write completion
	SyncFileRangeWaitAfter    = 8 // Wait for write completion
	SyncFileRangeWriteAndWait = SyncFileRangeWrite | SyncFileRangeWaitBefore
)

// AdviseSequential is a no-op on Darwin: posix_fadvise exists in libc but is
// a functional stub on this platform (always returns success, does
// nothing), and Go's syscall package doesn't wrap it at all, so there is no
// real advisory hint to give here.
func AdviseSequential(file *os.File) {
	// No-op on Darwin
}

// AdviseDontNeed is a no-op on Darwin for the same reason as AdviseSequential.
func AdviseDontNeed(file *os.File, offset int64, length int64) {
	// No-op on Darwin
}

// PreallocateFile pre-allocates space for a file using fcntl(F_PREALLOCATE),
// Darwin's equivalent of Linux's fallocate. Tries a contiguous allocation
// first (best for sequential I/O), falls back to a non-contiguous one if the
// volume is too fragmented to satisfy that, and finally falls back to
// truncate (sparse file) if F_PREALLOCATE isn't available at all. Unlike
// Linux's fallocate, F_PREALLOCATE reserves the blocks but does not itself
// change the file's reported size, so Truncate always runs afterward to
// make the new size visible -- matching fallocate's behavior on Linux
// (FALLOC_FL_KEEP_SIZE is not set there either).
func PreallocateFile(file *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	fstore := &syscall.Fstore_t{
		Flags:   syscall.F_ALLOCATECONTIG,
		Posmode: syscall.F_PEOFPOSMODE,
		Offset:  0,
		Length:  size,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), uintptr(syscall.F_PREALLOCATE), uintptr(unsafe.Pointer(fstore)))
	if errno != 0 {
		fstore.Flags = syscall.F_ALLOCATEALL
		_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), uintptr(syscall.F_PREALLOCATE), uintptr(unsafe.Pointer(fstore)))
	}
	if errno != 0 {
		return file.Truncate(size)
	}
	return file.Truncate(size)
}

// OpenFileNoAtime opens a file normally on Darwin: there is no O_NOATIME
// equivalent on this platform.
func OpenFileNoAtime(path string, flags int, mode uint32) (*os.File, error) {
	return os.OpenFile(path, flags, os.FileMode(mode))
}

// OpenFileDirect opens a file normally, then best-effort applies
// fcntl(F_NOCACHE), Darwin's closest equivalent to Linux's O_DIRECT (there
// is no open(2) flag for it here; it must be set on an already-open fd).
// Failure is ignored, matching every other advisory call in this file.
func OpenFileDirect(path string, flags int, mode uint32) (*os.File, error) {
	f, err := os.OpenFile(path, flags, os.FileMode(mode))
	if err != nil {
		return nil, err
	}
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_NOCACHE), 1)
	return f, nil
}

// SyncFileRange is a no-op on Darwin: there is no range-based writeback
// primitive equivalent to Linux's sync_file_range. F_FULLFSYNC exists but
// syncs the whole file (and is comparatively expensive), which is not an
// equivalent hint, so it is deliberately not substituted here.
func SyncFileRange(file *os.File, offset int64, length int64, flags uint32) {
	// No-op on Darwin
}

// Readv performs scatter/gather read via the same raw syscall approach as
// the Linux implementation. Darwin's syscall package doesn't wrap Readv
// like it does Mlock/Mlockall, but the underlying SYS_READV number and
// Iovec layout are both available and POSIX-standard on this platform.
func Readv(file *os.File, iovs [][]byte) (int, error) {
	vecs := makeDarwinIovecs(iovs)
	if len(vecs) == 0 {
		return 0, nil
	}
	n, _, err := syscall.Syscall(
		syscall.SYS_READV,
		file.Fd(),
		uintptr(unsafe.Pointer(&vecs[0])),
		uintptr(len(vecs)),
	)
	if err == 0 {
		return int(n), nil
	}
	return int(n), err
}

// Writev performs scatter/gather write; see Readv's comment.
func Writev(file *os.File, iovs [][]byte) (int, error) {
	vecs := makeDarwinIovecs(iovs)
	if len(vecs) == 0 {
		return 0, nil
	}
	n, _, err := syscall.Syscall(
		syscall.SYS_WRITEV,
		file.Fd(),
		uintptr(unsafe.Pointer(&vecs[0])),
		uintptr(len(vecs)),
	)
	if err == 0 {
		return int(n), nil
	}
	return int(n), err
}

func makeDarwinIovecs(buffers [][]byte) []syscall.Iovec {
	vecs := make([]syscall.Iovec, 0, len(buffers))
	for _, buf := range buffers {
		if len(buf) == 0 {
			continue
		}
		vecs = append(vecs, syscall.Iovec{Base: &buf[0], Len: uint64(len(buf))})
	}
	return vecs
}

// Splice is not supported on Darwin - there is no XNU equivalent of Linux's
// splice(2) (kernel-level zero-copy pipe transfer isn't a feature this
// kernel has at all, not just an API Go doesn't wrap).
func Splice(dst *os.File, src *os.File, length int64) (int64, error) {
	return 0, fmt.Errorf("splice not supported on Darwin")
}

// VmSplice is not supported on Darwin; see Splice's comment.
func VmSplice(pipe *os.File, iovs [][]byte) (int, error) {
	return 0, fmt.Errorf("vmsplice not supported on Darwin")
}

// CombinedSplice is not supported on Darwin; see Splice's comment.
func CombinedSplice(dst *os.File, data [][]byte) (int64, error) {
	return 0, fmt.Errorf("combined splice not supported on Darwin")
}
