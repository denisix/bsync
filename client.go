package main

import (
  "io"
  "os"
  "net"
  "bytes"
	"time"
)

func startClient(file *os.File, serverAddress string, skipIdx uint32, fileSize uint64, blockSize uint32, noCompress bool, checksumCache *ChecksumCache) {
  Log("startClient()\n")
	magicBytes := stringToFixedSizeArray(magicHead)

  saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
  if err != nil {
		Log("Error: resolving: %s\n", err.Error())
    return
  }

  conn := NewAutoReconnectTCP(saddr)
  defer conn.Close()

  var totCompSize uint64 = 0
  var totOrigSize uint64 = 0
  var percent float32 = 0
  t0 := time.Now()
  mbs := 0.0
  eta := 0

  buf := make([]byte, blockSize)

  lastBlockNum := uint32(fileSize / uint64(blockSize))

  Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

  for blockIdx := uint32(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {

    if debug { Log("\t- send to server blockIdx\n") }
    msg, err1 := pack(&Msg{ 
      MagicHead: magicBytes,
      BlockIdx: blockIdx,
      BlockSize: blockSize,
      FileSize: fileSize,
      DataSize: 0,
      Compressed: false,
    })
    if err1 != nil { Log("\t- cant pack msg-> %s\n", err1) }
    if debug { Log("\t- sending packed msg-> %s [%d bytes]\n", msg, len(msg)) }

    // read file block 1M
    if debug { Log("\t- read block from file\n") }
    offset := int64(blockIdx) * int64(blockSize)
    readedBytes, err := file.ReadAt(buf, offset)
    if err != nil && err != io.EOF {
      Log("\t- error reading file: %s\n", err.Error())
      break
    }

    n, err2 := conn.Write(msg) // writeConn(conn, b)
    if err2 != nil && err2 != io.EOF {
      Log("\t- error writing net: [%d] %s\n", n, err2.Error())
      break
    }

		hash := checksumCache.WaitFor(blockIdx)
    if debug { Log("\t- calc local hash -> [%d] %x\n", blockIdx, hash) }

    // buffer to get data
    serverHash := make([]byte, len(hash))
    _, err = conn.Read(serverHash)
    if err != nil {
      println("[client]\t- read data from net failed:", err.Error())
			return
    }
    if debug { Log("\t- rcvd server hash: [%d] %x\n", blockIdx, serverHash) }

		// ETA, stats
		totOrigSize = totOrigSize + uint64(blockSize)
		secs := time.Since(t0).Seconds()
		blockMb := float64(blockSize) / mb1
		blocksDelta := float64(blockIdx - skipIdx)
		mbsDelta := blocksDelta * blockMb
		blocksLeft := float64(lastBlockNum - blockIdx)
		mbsLeft := blocksLeft * blockMb
		mbs = mbsDelta / secs

		if mbs > 0 { eta = int(mbsLeft / mbs / 60) }
		if eta < 0 { eta = 0 }
		if blockIdx >= lastBlockNum { eta = 0 }

		if bytes.Equal(hash, serverHash) || bytes.Equal(hash, zeroBlockHash) {
 		   // Block is already in sync, or it's a known zero block — skip sending
			totCompSize = totCompSize + uint64(blockSize)
			ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))
    
			Log("block %d/%d (%0.2f%%) [-] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)
			continue
    }

		if noCompress {
			totCompSize = totCompSize + uint64(blockSize)
			ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))
			Log("block %d/%d (%0.2f%%) [w] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)

			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: uint32(readedBytes),
				Compressed: false,
			})
			if err1 != nil { Log("\t- cant pack msg-> %s\n", err) }

			n, err2 := conn.Write(msg) // writeConn(conn, b)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err2.Error())
				break
			}

			n, err3 := conn.Write(buf[:readedBytes]) // writeConn(conn, b)
			if err3 != nil && err3 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err3.Error())
				break
			}
			continue
		}

		// else compress block:
		compBuf, err := compressData(buf[:readedBytes])
		if err != nil {
			Log("Error: compressing data: %s\n", err)
			return
		}

		compressedBytes := uint32(len(compBuf))
		totCompSize = totCompSize + uint64(compressedBytes)
		ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))

		if compressedBytes < blockSize {
			Log("block %d/%d (%0.2f%%) [c] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)

			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: compressedBytes,
				Compressed: true,
			})
			if err1 != nil { Log("\t- cant pack msg-> %s\n", err) }

			n, err2 := conn.Write(msg) // writeConn(conn, b)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err2.Error())
				break
			}

			n, err3 := conn.Write(compBuf) // writeConn(conn, b)
			if err3 != nil && err3 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err3.Error())
				break
			}
		} else {
			Log("block %d/%d (%0.2f%%) [w] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)

			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				BlockIdx: blockIdx,
				BlockSize: blockSize,
				FileSize: fileSize,
				DataSize: uint32(readedBytes),
				Compressed: false,
			})
			if err1 != nil { Log("\t- cant pack msg-> %s\n", err) }

			n, err2 := conn.Write(msg) // writeConn(conn, b)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err2.Error())
				break
			}

			n, err3 := conn.Write(buf[:readedBytes]) // writeConn(conn, b)
			if err3 != nil && err3 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err3.Error())
				break
			}
		}
  }

	// if file was sparse / empty -> finalize it by writing just end
	Log("\nDONE, exiting..\n\n")

	// wait connection closed
	time.Sleep(2 * time.Second)
	return
}
