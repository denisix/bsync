package main

import (
  "io"
  "fmt"
  "os"
)

func getDeviceSize(file *os.File) uint64 {
  pos, err := file.Seek(0, io.SeekEnd)
  if err != nil {
    fmt.Printf("error seeking to end of %s: %s\n", err)
    os.Exit(1)
  }
  file.Seek(0, io.SeekStart)
  fmt.Printf("- getDeviceSize(): %d bytes.\n", pos)
  return uint64(pos)
}

