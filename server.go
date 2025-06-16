package main

import (
  "io"
  "fmt"
  "os"
  "net"
  "bufio"
  "time"
)

func serverHandleReq(conn net.Conn, file *os.File, checksumCache *ChecksumCache) {
  fmt.Println("- serverHandleReq()")
  defer conn.Close()
  magicBytes := stringToFixedSizeArray(magicHead)

  filebuf := make([]byte, blockSize)
  msgBuf := make([]byte, 21 + magicLen)

  var percent float32 = 0
  var compressRatio float32 = 100
  var lastBlockNum uint32 = 0
  var lastPartSize uint64 = 0
  var firstBlock uint32 = 0

  t0 := time.Now()
  mbs := 0.0
  eta := 0

  c := bufio.NewReader(conn)

  for {
    _, err1 := io.ReadFull(c, msgBuf)
    if err1 != nil {
      fmt.Println("\t- (1) connection ended", err1)
			return
    }

    msg, err2 := unpack(msgBuf)
    if err2 != nil {
      fmt.Println("\t- unpack msg failed:", err2)
    }

    if msg.MagicHead != magicBytes {
      conn.Close()
      return
    }

    if debug { fmt.Println("\t- unpacked Msg->", msg) }
   
    if msg.BlockSize != blockSize {
      blockSize = msg.BlockSize
      filebuf = make([]byte, blockSize)
    }

    offset := int64(msg.BlockIdx) * int64(blockSize)

    if msg.FileSize > 0 {
      percent = float32(uint64(offset) / (msg.FileSize / 100))

      compressRatio = float32(100 * float32(msg.DataSize) / float32(msg.BlockSize))

      if lastBlockNum == 0 {
        lastBlockNum = uint32(msg.FileSize / uint64(blockSize))
        lastPartSize = msg.FileSize - uint64(lastBlockNum) * uint64(blockSize)
        firstBlock = msg.BlockIdx
        fmt.Println("- last part size ->", lastPartSize, "lastBlock", lastBlockNum, "firstBlock", firstBlock, "blockSize", msg.BlockSize)
      }

      secs := time.Since(t0).Seconds()
      blockMb := float64(msg.BlockSize) / mb1
      blocksDelta := float64(msg.BlockIdx - firstBlock)
      mbsDelta := blocksDelta * blockMb
      blocksLeft := float64(lastBlockNum - msg.BlockIdx)
      mbsLeft := blocksLeft * blockMb
      
      mbs = mbsDelta / secs

			if mbs > 0 { eta = int(mbsLeft / mbs / 60) }
      // fmt.Println("- eta", msg.BlockSize, blocksLeft, mbsLeft, mbs)
      if eta < 0 { eta = 0 }
      if msg.BlockIdx >= lastBlockNum { eta = 0 }
    }

    if msg.DataSize > 0 {
      fmt.Printf("- block %d/%d (%0.2f%%) [w] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", msg.BlockIdx, lastBlockNum, percent, msg.FileSize, compressRatio, mbs, eta)
    } else {
      fmt.Printf("- block %d/%d (%0.2f%%) [K] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", msg.BlockIdx, lastBlockNum, percent, msg.FileSize, compressRatio, mbs, eta)
    }

    if msg.DataSize == 0 {
      if debug { fmt.Println("\t- read block from file") }

      n, err := file.ReadAt(filebuf, offset)
      if err != nil && err != io.EOF {
        fmt.Println("\t- error reading from file:", err.Error(), n)
        break
      }

      // if debug { fmt.Println("\t- calc hash", n) }
      // hash := checksum(filebuf[:n])
			if debug { fmt.Println("\t- wait for precomputed hash") }
      hash := checksumCache.WaitFor(msg.BlockIdx)

      if debug { fmt.Println("\t- send hash", hash) }
      connWrite(conn, hash[:])
    }

    if msg.DataSize > 0 {
      if debug { fmt.Println("\t- read block from network", msg.DataSize) }

      _, err1 := io.ReadFull(c, filebuf[:msg.DataSize])
      if err1 != nil {
        fmt.Println("\t- (2) connection closed:", err1)
				return
      }

      if msg.Compressed {
        decompressed, err := decompressData(filebuf[:msg.DataSize])
        if err != nil {
          fmt.Println("\t- error uncompressing:", err.Error())
          break
        }

        if debug { fmt.Println("\t- write uncompressed bytes:", len(decompressed), msg.DataSize) }
        n, err2 := file.WriteAt(decompressed, offset)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error reading from file:", err2.Error(), n)
          break
        }
      } else {

        if debug { fmt.Println("\t- write non-compressed bytes:", msg.DataSize) }
        n, err2 := file.WriteAt(filebuf[:msg.DataSize], offset)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error reading from file:", err2.Error(), n)
          break
        }

      }

    }

    // last piece?
    if msg.BlockIdx >= lastBlockNum {
      fmt.Println("\n- transfer DONE")
			time.Sleep(2 * time.Second)
      os.Exit(0)
    }

  }
}

func startServer(file *os.File, port string, checksumCache *ChecksumCache) {
  portStr := ":" + port
  listener, err := net.Listen("tcp", portStr)
  if err != nil {
    fmt.Println("Error listening:", err.Error())
    return
  }
  defer listener.Close()
  fmt.Println("- listening on 0.0.0.0" + portStr)

  for {
    conn, err := listener.Accept()
    if err != nil {
      fmt.Println("Error accepting: ", err.Error())
      return
    }
    go serverHandleReq(conn, file, checksumCache)
  }
}

