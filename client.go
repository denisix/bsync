package main

import (
  "io"
  "fmt"
  "os"
  "net"
  "bytes"
	"time"
)

func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache) {
  fmt.Println("- startClient()")
	magicBytes := stringToFixedSizeArray(magicHead)

  saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
  if err != nil {
    fmt.Println("Error resolving:", err.Error())
    return
  }

  conn := NewAutoReconnectTCP(saddr)
  defer conn.Close()

  // var blockIdx uint32 = skipIdx
  var totCompSize uint64 = 0
  var totOrigSize uint64 = 0

  buf := make([]byte, blockSize)

  lastBlockNum := uint32(fileSize / uint64(blockSize))

  fmt.Printf("- source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

  for blockIdx := uint32(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {

    if debug { fmt.Println("\t- send to server blockIdx") }
    msg, err1 := pack(&Msg{ 
      MagicHead: magicBytes,
      BlockIdx: blockIdx,
      BlockSize: blockSize,
      FileSize: fileSize,
      DataSize: 0,
      Compressed: false,
    })
    if err1 != nil { fmt.Println("\t- cant pack msg->", err1) }
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

    // hash := checksum(buf[:readedBytes])
		hash := checksumCache.WaitFor(blockIdx)

    if debug { fmt.Println("\t- calc local hash ->", hash) }

    // buffer to get data
    serverHash := make([]byte, len(hash))
    _, err = conn.Read(serverHash)
    if err != nil {
      println("\t- read data from net failed:", err.Error())
      os.Exit(1)
    }
    if debug { fmt.Println("\t- rcvd server hash:", serverHash) }

		if bytes.Equal(hash, serverHash) || bytes.Equal(hash, zeroBlockHash) {
 		   // Block is already in sync, or it's a known zero block — skip sending
			fmt.Printf("- block %d/%d: ok\r", blockIdx, lastBlockNum)
			totCompSize = totCompSize + uint64(blockSize)
			totOrigSize = totOrigSize + uint64(blockSize)
			continue
    }

		if noCompress {
			totCompSize = totCompSize + uint64(blockSize)
			totOrigSize = totOrigSize + uint64(blockSize)
			fmt.Printf("- block %d/%d: sync\r", blockIdx, lastBlockNum)

			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: uint32(readedBytes),
				Compressed: false,
			})
			if err1 != nil { fmt.Println("\t- cant pack msg->", err) }

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
			continue
		}

		// else compress block:
		compBuf, err := compressData(buf[:readedBytes])
		if err != nil {
			fmt.Println("\t- error compressing data:", err)
			return
		}

		compressedBytes := uint32(len(compBuf))
		totCompSize = totCompSize + uint64(compressedBytes)
		totOrigSize = totOrigSize + uint64(blockSize)

		if compressedBytes < blockSize {
			ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))
			fmt.Printf("- block %d/%d: sync compress %0.2f%%\r", blockIdx, lastBlockNum, ratio)
			// fmt.Println("\t\t- send to server compressed bytes", compressedBytes, "(<", blockSize, ")")
			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: compressedBytes,
				Compressed: true,
			})
			if err1 != nil { fmt.Println("\t- cant pack msg->", err) }

			n, err2 := conn.Write(msg) // writeConn(conn, b)
			if err2 != nil && err2 != io.EOF {
				fmt.Println("\t- error writing net:", n, err2.Error())
				break
			}

			// fmt.Println("\t\t- send to server full block")
			n, err3 := conn.Write(compBuf) // writeConn(conn, b)
			if err3 != nil && err3 != io.EOF {
				fmt.Println("\t- error writing net:", n, err3.Error())
				break
			}
		} else {
			fmt.Printf("- block %d/%d: sync\r", blockIdx, lastBlockNum)
			// fmt.Println("\t\t- send to server non-compressed bytes", readedBytes)

			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: uint32(readedBytes),
				Compressed: false,
			})
			if err1 != nil { fmt.Println("\t- cant pack msg->", err) }

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
	fmt.Println("\n- transfer done, exiting..")

	// wait connection closed
	time.Sleep(2 * time.Second)
	return
}
