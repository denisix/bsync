package main

import (
  "io"
  "fmt"
  "os"
  "net"
  "bytes"
  "crypto/md5"
)


func startClient(file *os.File, serverAddress string, skipIdx uint32) {
  fmt.Println("- startClient()")
  magicBytes := stringToFixedSizeArray(magicHead)

  saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
    fmt.Println("Error resolving:", err.Error())
    return
	}

  conn := NewAutoReconnectTCP(saddr)
  defer conn.Close()

  var blockIdx uint32 = skipIdx
  var fileSize uint64 = 0
  
  buf := make([]byte, blockSize)

  fileSize = getDeviceSize(file)
  lastBlockNum := uint32(fileSize / uint64(blockSize))

  fmt.Printf("- source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

  for {
    fmt.Println("- block", blockIdx, " / ", lastBlockNum)

    if debug { fmt.Println("\t- send to server blockIdx") }
    msg, err1 := pack(&Msg{ MagicHead: magicBytes, BlockIdx: blockIdx, FileSize: fileSize, DataSize: 0, Compressed: false, })
    if err1 != nil {
      fmt.Println("\t- cant pack msg->", err1)
    }
    if debug { fmt.Println("\t- sending packed msg->", msg, len(msg)) }

    // read file block 1M
    if debug { fmt.Println("\t- read block from file") }
    offset := int64(blockIdx) * int64(blockSize)
    readedBytes, err := file.ReadAt(buf, offset)
    if err != nil && err != io.EOF {
      fmt.Println("\t- error reading file:", err.Error())
      break
    }

    n, err2 := conn.Write(msg) // writeConn(conn, b)
    if err2 != nil && err2 != io.EOF {
      fmt.Println("\t- error writing net:", n, err2.Error())
      break
    }

    hash := md5.Sum(buf[:readedBytes])
    if debug { fmt.Println("\t- calc local hash ->", hash) }

    // buffer to get data
    serverHash := make([]byte, 16)
    _, err = conn.Read(serverHash)
    if err != nil {
      println("\t- read data from net failed:", err.Error())
      os.Exit(1)
    }
    if debug { fmt.Println("\t- rcvd server hash:", serverHash) }

    if bytes.Equal(hash[:], serverHash) {
      // fmt.Println("\t- hash equal -> skip to next")
    } else {
      // fmt.Println("\t- hash differs -> compress block, orig", readedBytes)

      lzmaBuf, err := compressData(buf[:readedBytes])
      if err != nil {
        fmt.Println("\t- error compressing data:", err)
        return
      }

      lzmaBytes := uint32(len(lzmaBuf))

      if lzmaBytes < blockSize {
        // fmt.Println("\t\t- send to server compressed bytes", lzmaBytes, "(<", blockSize, ")")
        m := &Msg{
          MagicHead: magicBytes,
          BlockIdx: blockIdx,
          FileSize: fileSize,
          DataSize: lzmaBytes,
          Compressed: true,
        }
        msg, err1 := pack(m)
        if err1 != nil {
          fmt.Println("\t- cant pack msg->", err)
        }

        n, err2 := conn.Write(msg) // writeConn(conn, b)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error writing net:", n, err2.Error())
          break
        }

        // fmt.Println("\t\t- send to server full block")
        n, err3 := conn.Write(lzmaBuf) // writeConn(conn, b)
        if err3 != nil && err3 != io.EOF {
          fmt.Println("\t- error writing net:", n, err3.Error())
          break
        }
      } else {
        // fmt.Println("\t\t- send to server non-compressed bytes", readedBytes)

        m := &Msg{
          MagicHead: magicBytes,
          BlockIdx: blockIdx,
          FileSize: fileSize,
          DataSize: uint32(readedBytes),
          Compressed: false,
        }
        msg, err1 := pack(m)
        if err1 != nil {
          fmt.Println("\t- cant pack msg->", err)
        }
        /*
        b := make([]byte, 16)
        binary.LittleEndian.PutUint32(b[0:], uint32(readedBytes))
        binary.LittleEndian.PutUint32(b[4:], blockIdx)
        binary.LittleEndian.PutUint64(b[8:], fileSize)
        */
        n, err2 := conn.Write(msg) // writeConn(conn, b)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error writing net:", n, err2.Error())
          break
        }

        n, err3 := conn.Write(buf[:readedBytes]) // writeConn(conn, b)
        if err3 != nil && err3 != io.EOF {
          fmt.Println("\t- error writing net:", n, err3.Error())
          break
        }

      }

    }

    // time.Sleep(1 * time.Second)
    blockIdx++
  }
}
