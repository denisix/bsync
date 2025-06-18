package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"time"
)

func serverHandleReq(conn net.Conn, file *os.File, checksumCache *ChecksumCache) {
	Log("serverHandleReq()\n")
	defer conn.Close()
	magicBytes := stringToFixedSizeArray(magicHead)

	filebuf := make([]byte, blockSize)
	msgBuf := make([]byte, 43) // Exact size of Msg structure: 17 + 8 + 4 + 8 + 4 + 1 + 1 = 43 bytes

	var lastBlockNum uint64 = 0
	var lastPartSize uint64 = 0
	var firstBlock uint64 = 0

	t0 := time.Now()
	mbs := 0.0
	eta := 0

	c := bufio.NewReader(conn)

	for {
		_, err1 := io.ReadFull(c, msgBuf)
		if err1 != nil {
			Log("\t- (1) connection ended: %s\n", err1)
			return
		}

		msg, err2 := unpack(msgBuf)
		if err2 != nil {
			Log("\t- unpack msg failed: %s\n", err2)
		}

		if msg.MagicHead != magicBytes {
			conn.Close()
			return
		}

		if debug {
			Log("\t- unpacked Msg-> %v\n", &msg)
		}

		if msg.BlockSize != blockSize {
			blockSize = msg.BlockSize
			filebuf = make([]byte, blockSize)
		}

		offset := int64(msg.BlockIdx) * int64(blockSize)

		if msg.FileSize > 0 {
			if lastBlockNum == 0 {
				lastBlockNum = uint64(msg.FileSize / uint64(blockSize))
				lastPartSize = msg.FileSize - uint64(lastBlockNum-1)*uint64(blockSize)
				firstBlock = msg.BlockIdx
				Log("\n\tlast part size = %d\n\tlast block = %d\n\tfirst block = %d\n\tblock size = %d\n\n", lastPartSize, lastBlockNum-1, firstBlock, msg.BlockSize)

				truncateIfRegularFile(file, msg.FileSize)
			}

			secs := time.Since(t0).Seconds()
			blockMb := float64(msg.BlockSize) / mb1
			blocksDelta := float64(msg.BlockIdx - firstBlock)
			mbsDelta := blocksDelta * blockMb
			blocksLeft := float64(lastBlockNum - 1 - msg.BlockIdx)
			mbsLeft := blocksLeft * blockMb

			mbs = mbsDelta / secs

			if mbs > 0 {
				eta = int(mbsLeft / mbs / 60)
			}
			if eta < 0 {
				eta = 0
			}
			if msg.BlockIdx >= lastBlockNum-1 {
				eta = 0
			}
		}

		if msg.DataSize == 0 {
			if msg.LastBlock {
				Log("\nTransfer complete, received last block\n")
				os.Exit(0)
				return
			}

			if debug {
				Log("\t- read block from file\n")
			}

			n, err := file.ReadAt(filebuf, offset)
			if err != nil && err != io.EOF {
				Log("\t- error reading from file: [%d] %s\n", n, err.Error())
				break
			}

			hash := checksumCache.WaitFor(msg.BlockIdx)

			if debug {
				Log("\t- send hash [%d] %x\n", msg.BlockIdx, hash)
			}
			connWrite(conn, hash[:])
		}

		if msg.DataSize > 0 {
			if debug {
				Log("\t- read block from network %d\n", msg.DataSize)
			}

			_, err1 := io.ReadFull(c, filebuf[:msg.DataSize])
			if err1 != nil {
				Log("\t- (2) connection closed: %s\n", err1)
				return
			}

			if msg.Compressed {
				decompressed, err := decompressData(filebuf[:msg.DataSize])
				if err != nil {
					Log("\t- error uncompressing: %s\n", err.Error())
					break
				}

				if debug {
					Log("\t- write uncompressed bytes: %d [%d bytes]\n", msg.DataSize, len(decompressed))
				}
				n, err2 := file.WriteAt(decompressed, offset)
				if err2 != nil && err2 != io.EOF {
					Log("\t- error reading from file: [%d] %s\n", n, err2.Error())
					break
				}
			} else {
				if debug {
					Log("\t- write non-compressed bytes: %d\n", msg.DataSize)
				}
				n, err2 := file.WriteAt(filebuf[:msg.DataSize], offset)
				if err2 != nil && err2 != io.EOF {
					Log("\t- error reading from file: [%d] %s\n", n, err2.Error())
					break
				}
			}
		}
	}
}

func startServer(file *os.File, port string, checksumCache *ChecksumCache) {
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		Log("Error listening: %s\n", err.Error())
		return
	}
	defer listener.Close()

	Log("READY, listening on 0.0.0.0:%s\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			Log("Error accepting: %s\n", err.Error())
			return
		}
		go serverHandleReq(conn, file, checksumCache)
	}
}
