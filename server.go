package main

import (
	"bufio"
	"encoding/binary"
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
	msgBuf := make([]byte, binary.Size(Msg{}))

	var lastBlockNum uint32 = 0
	var lastPartSize uint64 = 0
	var firstBlock uint32 = 0

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
			Log("\t- unpacked Msg-> %s\n", msg)
		}

		if msg.BlockSize != blockSize {
			blockSize = msg.BlockSize
			filebuf = make([]byte, blockSize)
		}

		offset := int64(msg.BlockIdx) * int64(blockSize)

		if msg.FileSize > 0 {
			if lastBlockNum == 0 {
				lastBlockNum = uint32(msg.FileSize/uint64(blockSize)) + 1
				lastPartSize = msg.FileSize - uint64(lastBlockNum)*uint64(blockSize)
				firstBlock = msg.BlockIdx
				Log("\n\tlast part size = %d\n\tlast block = %d\n\tfirst block = %d\n\tblock size = %d\n\n", lastPartSize, lastBlockNum, firstBlock, msg.BlockSize)

				truncateIfRegularFile(file, msg.FileSize)
			}
		}

		if msg.DataSize == 0 {
			if debug {
				Log("\t- read block from file\n")
			}

			n, err := file.ReadAt(filebuf, offset)
			if err != nil && err != io.EOF {
				Log("\t- error reading from file: [%d] %s\n", n, err.Error())
				break
			}

			// if debug { Log("\t- wait for precomputed hash\n") }
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

			if msg.Zero {
				zero := make([]byte, msg.DataSize)
				n, err := file.WriteAt(zero, offset)
				if err != nil && err != io.EOF {
					Log("\t- error writing to file: [%d] %s\n", n, err.Error())
					break
				}
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
					Log("\t- error writing to file: [%d] %s\n", n, err2.Error())
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

		// if Done?
		if msg.Done {
			Log("\ntransfer DONE\n\n")
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}

	}
}

func startServer(file *os.File, port string, checksumCache *ChecksumCache) {
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		Err("listening: %s\n", err.Error())
		return
	}
	defer listener.Close()

	// time.Sleep(2 * time.Second)
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
