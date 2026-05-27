//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// AdviseSequential advises the kernel that the file will be accessed sequentially.
// This improves read-ahead behavior for HDD transfers. Linux-only; no-op on other systems.
// Silently ignores errors (not all file types support fadvise).
func AdviseSequential(file *os.File) {
	// Use raw fadvise64 syscall for Linux
	// On other platforms, this is a no-op
	syscall.Syscall6(
		syscall.SYS_FADVISE64,
		uintptr(file.Fd()),
		uintptr(0),
		uintptr(0),
		uintptr(2), // POSIX_FADV_SEQUENTIAL = 2
		0,
		0,
	)
}

// AdviseDontNeed advises the kernel that we won't read the written data back.
// Useful for write-once patterns like drive cloning. Linux-only.
func AdviseDontNeed(file *os.File, offset int64, length int64) {
	syscall.Syscall6(
		syscall.SYS_FADVISE64,
		uintptr(file.Fd()),
		uintptr(offset),
		uintptr(length),
		uintptr(4), // POSIX_FADV_DONTNEED = 4
		0,
		0,
	)
}

// PreallocateFile attempts to pre-allocate space for a file.
// Uses fallocate on Linux for actual allocation, falls back to
// truncate (creates sparse file) on other systems or on error.
func PreallocateFile(file *os.File, size int64) error {
	if size <= 0 {
		return nil
	}

	// Try fallocate (Linux) for actual allocation
	// FALLOC_FL_KEEP_SIZE is not set, so file size will change
	err := syscall.Fallocate(int(file.Fd()), 0, 0, size)
	if err == nil {
		return nil
	}

	// Fall back to truncate (creates sparse file)
	return file.Truncate(size)
}

// OpenFileNoAtime opens a file with O_NOATIME flag to avoid metadata updates.
// Falls back to regular open if permission denied (non-owner).
func OpenFileNoAtime(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOATIME, uint32(mode))
	if err != nil {
		// Fall back - may not be permitted for non-owner
		return os.OpenFile(path, flags, os.FileMode(mode))
	}
	return os.NewFile(uintptr(fd), path), nil
}

// OpenFileDirect opens a file with O_DIRECT flag for page cache bypass.
// Falls back to regular open on error or non-Linux systems.
// Note: Requires memory-aligned buffers for I/O operations.
func OpenFileDirect(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_DIRECT, uint32(mode))
	if err != nil {
		return os.OpenFile(path, flags, os.FileMode(mode))
	}
	return os.NewFile(uintptr(fd), path), nil
}

const (
	SyncFileRangeWrite        = 2 // Start writeout of dirty pages
	SyncFileRangeWaitBefore   = 4 // Wait for write completion
	SyncFileRangeWaitAfter    = 8 // Wait for write completion
	SyncFileRangeWriteAndWait = SyncFileRangeWrite | SyncFileRangeWaitBefore
)

// SyncFileRange provides explicit writeback control for specified file range.
// Linux-only; silently ignored on other systems.
func SyncFileRange(file *os.File, offset int64, length int64, flags uint32) {
	syscall.Syscall6(
		syscall.SYS_SYNC_FILE_RANGE,
		uintptr(file.Fd()),
		uintptr(offset),
		uintptr(length),
		uintptr(flags),
		0,
		0,
	)
}

// Readv performs scatter/gather read - combines multiple I/Os into single syscall.
// Linux-only; returns error on other platforms.
func Readv(file *os.File, iovs [][]byte) (int, error) {
	vecs := makeIovecs(iovs)
	if len(vecs) == 0 {
		return 0, nil
	}
	n, _, err := syscall.Syscall(
		syscall.SYS_READV,
		uintptr(file.Fd()),
		uintptr(unsafe.Pointer(&vecs[0])),
		uintptr(len(vecs)),
	)
	if err == 0 {
		return int(n), nil
	}
	return int(n), err
}

// Writev performs scatter/gather write - combines multiple I/Os into single syscall.
// Linux-only; returns error on other platforms.
func Writev(file *os.File, iovs [][]byte) (int, error) {
	vecs := makeIovecs(iovs)
	if len(vecs) == 0 {
		return 0, nil
	}
	n, _, err := syscall.Syscall(
		syscall.SYS_WRITEV,
		uintptr(file.Fd()),
		uintptr(unsafe.Pointer(&vecs[0])),
		uintptr(len(vecs)),
	)
	if err == 0 {
		return int(n), nil
	}
	return int(n), err
}

// Splice performs zero-copy data transfer between file descriptors.
// Useful for network-to-disk or disk-to-network transfers.
// Linux-only; returns error on other platforms.
func Splice(dst *os.File, src *os.File, length int64) (int64, error) {
	n, _, err := syscall.Syscall6(
		syscall.SYS_SPLICE,
		uintptr(src.Fd()),
		0,
		uintptr(dst.Fd()),
		0,
		uintptr(length),
		0,
	)
	if err == 0 {
		return int64(n), nil
	}
	return int64(n), err
}

// VmSplice transfers one logical batch from userspace buffers to a pipe.
// It coalesces fragmented input so a standalone call is not truncated by pipe
// slot limits; CombinedSplice retains the chunked splice pipeline for bulk I/O.
func VmSplice(pipe *os.File, iovs [][]byte) (int, error) {
	total := totalBufferSize(iovs)
	if total == 0 {
		return 0, nil
	}
	// A standalone VmSplice call cannot make progress after filling a pipe
	// because it has no reader to drain it. Grow the pipe where allowed so
	// ordinary batched writes can complete in a single API call.
	_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(pipe.Fd()), uintptr(syscall.F_SETPIPE_SZ), uintptr(total))
	// Coalescing prevents many tiny iovecs from consuming all pipe slots even
	// when their total byte count is below the pipe capacity.
	joined := bytes.Join(iovs, nil)
	return vmSpliceOnce(pipe, [][]byte{joined})
}

func vmSpliceOnce(pipe *os.File, iovs [][]byte) (int, error) {
	vecs := makeIovecs(iovs)
	if len(vecs) == 0 {
		return 0, nil
	}
	n, _, err := syscall.Syscall(
		syscall.SYS_VMSPLICE,
		uintptr(pipe.Fd()),
		uintptr(unsafe.Pointer(&vecs[0])),
		uintptr(len(vecs)),
	)
	if err == 0 {
		return int(n), nil
	}
	return int(n), err
}

func totalBufferSize(buffers [][]byte) int {
	total := 0
	for _, buf := range buffers {
		total += len(buf)
	}
	return total
}

func consumeBuffers(buffers [][]byte, consumed int) [][]byte {
	for len(buffers) > 0 {
		if consumed < len(buffers[0]) {
			return append([][]byte{buffers[0][consumed:]}, buffers[1:]...)
		}
		consumed -= len(buffers[0])
		buffers = buffers[1:]
	}
	return nil
}

func makeIovecs(buffers [][]byte) []syscall.Iovec {
	vecs := make([]syscall.Iovec, 0, len(buffers))
	for _, buf := range buffers {
		if len(buf) == 0 {
			continue
		}
		vecs = append(vecs, syscall.Iovec{Base: &buf[0], Len: uint64(len(buf))})
	}
	return vecs
}

// CombinedSplice performs zero-copy transfer: userspace → pipe → destination.
// This eliminates memory copies when sending to network or disk.
// Returns total bytes transferred.
func CombinedSplice(dst *os.File, data [][]byte) (int64, error) {
	// Create pipe for intermediate transfer
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("pipe creation failed: %w", err)
	}
	defer pr.Close()
	defer pw.Close()

	var transferred int64
	remaining := data
	for len(remaining) > 0 {
		n, err := vmSpliceOnce(pw, remaining)
		if err != nil {
			return transferred, fmt.Errorf("vmsplice failed: %w", err)
		}
		if n == 0 {
			return transferred, fmt.Errorf("vmsplice made no progress")
		}
		remaining = consumeBuffers(remaining, n)
		toDrain := int64(n)
		for toDrain > 0 {
			drained, err := Splice(dst, pr, toDrain)
			if err != nil {
				return transferred, fmt.Errorf("splice failed: %w", err)
			}
			if drained == 0 {
				return transferred, fmt.Errorf("splice made no progress")
			}
			transferred += drained
			toDrain -= drained
		}
	}
	return transferred, nil
}
