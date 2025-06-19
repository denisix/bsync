package main

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"time"
)

func handleClient(conn net.Conn, file *os.File, checksumCache *ChecksumCache) {
	defer conn.Close()

	// Read initial message
	msg, err := readMsg(conn)
	if err != nil {
		Log("Error reading initial message: %v\n", err)
		return
	}

	// Update block size if needed
	if msg.BlockSize != blockSize {
		blockSize = msg.BlockSize
	}

	truncateIfRegularFile(file, msg.FileSize)

	// Calculate number of blocks (source and destination)
	lastBlockNum := uint64(msg.FileSize / uint64(blockSize))
	if msg.FileSize%uint64(blockSize) != 0 {
		lastBlockNum++
	}

	Log("handling client, src blocks: 0..%d\n", lastBlockNum)

	// --- Block stats ---
	t0 := time.Now()
	totalBlocks := 0
	totalBytes := uint64(0)
	totalCompressed := uint64(0)

	// Send all hashes sequentially in background, retry forever on error
	go func() {
		for i := uint64(0); i <= lastBlockNum; i++ {
			var hash []byte
			if i < lastBlockNum {
				hash = checksumCache.WaitFor(i)
			} else {
				hash = zeroBlockHash
			}
			for {
				_, err := conn.Write(hash)
				if err == nil {
					break
				}
				// Exit if connection is closed or permanent error
				if err == io.EOF || (err != nil && !isTemporaryNetErr(err)) {
					Log("Hash send error (permanent): %v, exiting hash sender goroutine.\n", err)
					return
				}
				Log("Hash send error: %v, retrying in 1s...\n", err)
				time.Sleep(time.Second)
			}
		}
	}()

	// Handle incoming block data
	filebuf := make([]byte, blockSize)
	for {
		msg, err := readMsg(conn)
		if err != nil {
			if err != io.EOF {
				Log("Error reading message: %v\n", err)
			}
			return
		}

		if msg.LastBlock {
			elapsed := time.Since(t0).Seconds()
			mbs := float64(totalBytes) / mb1 / elapsed
			compRatio := float64(totalCompressed) / float64(totalBytes)
			Log("\nTransfer complete: %d blocks, %d bytes, compressed: %d bytes, ratio: %.2f%%, avg %.2f MB/s\n",
				totalBlocks, totalBytes, totalCompressed, 100*compRatio, mbs)
			Log("Received last block, exiting\n")
			os.Exit(0)
			return
		}

		if msg.DataSize > 0 {
			if err := writeBlock(conn, file, msg, filebuf); err != nil {
				Log("Error handling block: %v\n", err)
				return
			}
			totalBlocks++
			totalBytes += msg.DataSize
			if msg.Compressed {
				totalCompressed += msg.DataSize
			}
			Log("block %d/%d\r", msg.BlockIdx, lastBlockNum)
		}
	}
}

func readMsg(conn net.Conn) (*Msg, error) {
	msgBuf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, msgBuf); err != nil {
		return nil, err
	}

	msg, err := unpack(msgBuf)
	if err != nil || msg.MagicHead != stringToFixedSizeArray(magicHead) {
		return nil, err
	}
	return msg, nil
}

func writeBlock(conn net.Conn, file *os.File, msg *Msg, filebuf []byte) error {
	if _, err := io.ReadFull(conn, filebuf[:msg.DataSize]); err != nil {
		return err
	}

	data := filebuf[:msg.DataSize]
	if msg.Compressed {
		decompressed, err := decompressData(data)
		if err != nil {
			return err
		}
		data = decompressed
	}

	offset := int64(msg.BlockIdx) * int64(blockSize)
	_, err := file.WriteAt(data, offset)
	return err
}

func startServer(file *os.File, port string, checksumCache *ChecksumCache) {
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		Log("Error starting server: %v\n", err)
		return
	}
	defer listener.Close()

	Log("READY, listening on 0.0.0.0:%s\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			Log("Error accepting connection: %v\n", err)
			continue
		}
		go handleClient(conn, file, checksumCache)
	}
}

// Helper to check for temporary network errors
func isTemporaryNetErr(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Temporary()
}
