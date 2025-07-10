package main

import (
	"io"
	"os"
)

func getDeviceSize(file *os.File) uint64 {
	pos, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		Log("error seeking to end of %s: %s\n", err)
		os.Exit(1)
	}
	file.Seek(0, io.SeekStart)
	Log("file size -> %d bytes.\n", pos)
	return uint64(pos)
}

// isZeroBlock -> true if every byte is 0
func isZeroBlock(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func truncateIfRegularFile(file *os.File, size uint64) {
	info, err := file.Stat()
	if err != nil {
		Log("Error: file stat failed: %w\n", err)
	}

	mode := info.Mode()
	isBlock := mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0

	if !isBlock && mode.IsRegular() {
		currentSize := uint64(info.Size())
		if currentSize != size {
			if err := file.Truncate(int64(size)); err != nil {
				Log("Error: truncate failed: %w\n", err)
			}
			Log("file truncated to %d bytes\n", size)
		} else {
			Log("skip truncate: file size already %d bytes\n", size)
		}
	} else {
		Log("skip truncate: destination is a block device or special file\n")
	}
}
