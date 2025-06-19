package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"time"
)

func startClient(serverAddress string, skipIdx uint64, fileSize uint64, blockSize uint32, lastBlockNum uint64, noCompress bool, checksumCache *ChecksumCache) {
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
	var msg []byte
	var err1, err2, err3 error
	var n int

	Log("source size: %d bytes, block %d bytes, blockNum: %d\n", fileSize, blockSize, lastBlockNum)

	for blockIdx := uint64(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {
		if debug {
			Log("\t- send to server blockIdx\n")
		}
		msg, err1 = pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   0,
			Compressed: false,
			LastBlock:  false,
		})
		if err1 != nil {
			Log("\t- cant pack msg-> %s\n", err1)
		}
		if debug {
			Log("\t- sending packed msg-> %s [%d bytes]\n", msg, len(msg))
		}

		n, err2 = conn.Write(msg)
		if err2 != nil && err2 != io.EOF {
			Log("\t- error writing net: [%d] %s\n", n, err2.Error())
			os.Exit(1)
			// break
		}

		hash := checksumCache.WaitFor(blockIdx)
		if debug {
			Log("\t- calc local hash -> [%d] %x\n", blockIdx, hash)
		}

		serverHash := make([]byte, len(hash))
		_, err = conn.Read(serverHash)
		if err != nil {
			println("[client]\t- read data from net failed:", err.Error())
			return
		}
		if debug {
			Log("\t- rcvd server hash: [%d] %x\n", blockIdx, serverHash)
		}

		totOrigSize = totOrigSize + uint64(blockSize)
		secs := time.Since(t0).Seconds()
		blockMb := float64(blockSize) / mb1
		blocksDelta := float64(blockIdx - skipIdx)
		mbsDelta := blocksDelta * blockMb
		blocksLeft := float64(lastBlockNum - 1 - blockIdx)
		mbsLeft := blocksLeft * blockMb
		mbs = mbsDelta / secs

		if mbs > 0 {
			eta = int(mbsLeft / mbs / 60)
		}
		if eta < 0 {
			eta = 0
		}
		if blockIdx > lastBlockNum {
			eta = 0
		}

		if bytes.Equal(hash, serverHash) || bytes.Equal(hash, zeroBlockHash) {
			totCompSize = totCompSize + uint64(blockSize)
			ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))
			Log("block %d/%d (%0.2f%%) [-] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)
			continue
		}

		// Get block data from cache
		blockData := checksumCache.WaitForBlockData(blockIdx)
		if blockData == nil {
			Log("Error: no block data available for block %d\n", blockIdx)
			return
		}

		// Use the stored block data
		totCompSize = totCompSize + uint64(len(blockData.Data))
		ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))

		if noCompress || !blockData.IsCompressed {
			Log("block %d/%d (%0.2f%%) [w] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)

			msg, err1 = pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   blockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   uint32(len(blockData.Data)),
				Compressed: false,
			})
			if err1 != nil {
				Log("\t- cant pack msg-> %s\n", err)
			}

			n, err2 = conn.Write(msg)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err2.Error())
				break
			}

			n, err3 = conn.Write(blockData.Data)
			if err3 != nil && err3 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err3.Error())
				break
			}
		} else {
			Log("block %d/%d (%0.2f%%) [c] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\r", blockIdx, lastBlockNum, percent, fileSize, ratio, mbs, eta)

			msg, err1 = pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   blockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   uint32(len(blockData.Data)),
				Compressed: true,
			})
			if err1 != nil {
				Log("\t- cant pack msg-> %s\n", err)
			}

			n, err2 = conn.Write(msg)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err2.Error())
				break
			}

			n, err3 = conn.Write(blockData.Data)
			if err3 != nil && err3 != io.EOF {
				Log("\t- error writing net: [%d] %s\n", n, err3.Error())
				break
			}
		}
	}

	Log("\nTransfer complete, waiting for server to finish...\n")

	msg, err1 = pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   0,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   0,
		Compressed: false,
		LastBlock:  true,
	})
	if err1 != nil {
		Log("\t- cant pack msg-> %s\n", err1)
	}
	if debug {
		Log("\t- sending packed msg-> %s [%d bytes]\n", msg, len(msg))
	}

	n, err2 = conn.Write(msg)
	if err2 != nil && err2 != io.EOF {
		Log("\t- error writing net: [%d] %s\n", n, err2.Error())
	}

	Log("\nDONE, exiting..\n\n")
	// checksumCache.Close() // No longer needed
	time.Sleep(2 * time.Second)
}
