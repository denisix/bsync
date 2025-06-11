package main

import (
  "flag"
  "fmt"
  "os"
)

var debug bool = false
var blockSize uint32 = 104857600
var mb1 float64 = 1048576.0

func main() {
  var device string
  var remoteAddr string
  var bSize uint
  var skipIdx uint
  var port string
  var noCompress bool

  flag.StringVar(&device, "f", "/dev/zero", "specify file or device, i.e. '/dev/vda'")
  flag.StringVar(&remoteAddr, "r", "", "specify remote address of server")
  flag.UintVar(&bSize, "b", uint(blockSize), "block size, default 100M")
  flag.UintVar(&skipIdx, "s", 0, "skip blocks, default 0")
  flag.StringVar(&port, "p", "8080", "bind to port, default 8080")
  flag.BoolVar(&noCompress, "n", false, "do not compress blocks (by default compress)")

  flag.Parse()  // after declaring flags we need to call it

  blockSize = uint32(bSize)

  if remoteAddr != "" {
    // CLIENT: source file
    fmt.Println("- starting client, transfer: ", device, "->", remoteAddr)

    file, err := os.OpenFile(device, os.O_RDONLY, 0666)
    if err != nil {
      fmt.Println("Error opening file:", err.Error())
      return
    }
    defer file.Close()
	
		fileSize := getDeviceSize(file)
		lastBlockNum := uint32(fileSize / uint64(blockSize))

		checksumCache := NewChecksumCache(lastBlockNum)
		go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache)

    startClient(file, remoteAddr, uint32(skipIdx), fileSize, blockSize, noCompress, checksumCache)

  } else {
    // SERVER: destination file
    fmt.Println("- starting server, remote ->", device, "(init blockSize =", blockSize, ")")
    file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
    if err != nil {
      fmt.Println("Error opening file:", err.Error())
      return
    }
    defer file.Close()

		fileSize := getDeviceSize(file)
		lastBlockNum := uint32(fileSize / uint64(blockSize))

		checksumCache := NewChecksumCache(lastBlockNum)
		go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache)

    startServer(file, port, checksumCache)
  }
}
