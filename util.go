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
  Log("- getDeviceSize() -> %d bytes.\n", pos)
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
