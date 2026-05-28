//go:build windows

package main

import (
	"fmt"
	"os"
)

const (
	SyncFileRangeWrite        = 2 // Start writeout of dirty pages
	SyncFileRangeWaitBefore   = 4 // Wait for write completion
	SyncFileRangeWaitAfter    = 8 // Wait for write completion
	SyncFileRangeWriteAndWait = SyncFileRangeWrite | SyncFileRangeWaitBefore
)

// AdviseSequential is a no-op on Windows.
func AdviseSequential(file *os.File) {
	// No-op on Windows
}

// AdviseDontNeed is a no-op on Windows.
func AdviseDontNeed(file *os.File, offset int64, length int64) {
	// No-op on Windows
}

// PreallocateFile uses truncate on Windows (creates sparse file).
func PreallocateFile(file *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	return file.Truncate(size)
}

// OpenFileNoAtime opens a file normally on Windows (O_NOATIME not available).
func OpenFileNoAtime(path string, flags int, mode uint32) (*os.File, error) {
	return os.OpenFile(path, flags, os.FileMode(mode))
}

// OpenFileDirect opens a file normally on Windows (O_DIRECT not available).
func OpenFileDirect(path string, flags int, mode uint32) (*os.File, error) {
	return os.OpenFile(path, flags, os.FileMode(mode))
}

// SyncFileRange is a no-op on Windows.
func SyncFileRange(file *os.File, offset int64, length int64, flags uint32) {
	// No-op on Windows
}

// Readv is not supported on Windows - returns error.
func Readv(file *os.File, iovs [][]byte) (int, error) {
	return 0, fmt.Errorf("readv not supported on Windows")
}

// Writev is not supported on Windows - returns error.
func Writev(file *os.File, iovs [][]byte) (int, error) {
	return 0, fmt.Errorf("writev not supported on Windows")
}

// Splice is not supported on Windows - returns error.
func Splice(dst *os.File, src *os.File, length int64) (int64, error) {
	return 0, fmt.Errorf("splice not supported on Windows")
}

// VmSplice is not supported on Windows - returns error.
func VmSplice(pipe *os.File, iovs [][]byte) (int, error) {
	return 0, fmt.Errorf("vmsplice not supported on Windows")
}

// CombinedSplice is not supported on Windows - returns error.
func CombinedSplice(dst *os.File, data [][]byte) (int64, error) {
	return 0, fmt.Errorf("combined splice not supported on Windows")
}
