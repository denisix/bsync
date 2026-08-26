//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
)

func getDeviceSize(file *os.File) uint64 {
	pos, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		Err("seeking to end of file: %v\n", err)
	}
	file.Seek(0, io.SeekStart)
	Log("file size -> %d bytes.\n", pos)
	return uint64(pos)
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
	}
}

// truncateRegular is truncateIfRegularFile's non-fatal counterpart. The
// single-file path's Err() exits the whole process on failure, which is
// correct there (there is only ever one file), but directory mode must not
// let one bad file kill a transfer of thousands of others.
func truncateRegular(file *os.File, size uint64) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	mode := info.Mode()
	isBlock := mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
	if isBlock || !mode.IsRegular() {
		return nil
	}
	if uint64(info.Size()) == size {
		return nil
	}
	if err := file.Truncate(int64(size)); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}
