package main

import (
	"io"
	"net"
	"os"
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
	destSize := getDeviceSize(file)
	destBlockNum := uint64(destSize / uint64(blockSize))
	if destSize%uint64(blockSize) != 0 {
		destBlockNum++
	}
	Log("handling client, src blocks: 0..%d, dst blocks: 0..%d\n", lastBlockNum, destBlockNum)

	// Send all hashes sequentially in background
	hashSendErr := make(chan error, 1)
	go func() {
		for i := uint64(0); i <= lastBlockNum; i++ {
			var hash []byte
			if i < destBlockNum {
				hash = checksumCache.WaitFor(i)
			} else {
				hash = zeroBlockHash
			}
			// Log("sending hash [%d]: %x\n", i, hash)
			if _, err := conn.Write(hash); err != nil {
				Log("Error sending hash %d: %v\n", i, err)
				hashSendErr <- err
				return
			}
		}
		close(hashSendErr)
	}()

	// Handle incoming block data
	filebuf := make([]byte, blockSize)
	for {
		select {
		case err, ok := <-hashSendErr:
			if !ok {
				// Channel closed normally, continue processing
				hashSendErr = nil
				continue
			}
			if err != nil {
				Log("Background hash sending failed: %v\n", err)
				return
			}
		default:
			msg, err := readMsg(conn)
			if err != nil {
				if err != io.EOF {
					Log("Error reading message: %v\n", err)
				}
				return
			}

			if msg.LastBlock {
				Log("Received last block, exiting\n")
				os.Exit(0)
				return
			}

			if msg.DataSize > 0 {
				if err := writeBlock(conn, file, msg, filebuf); err != nil {
					Log("Error handling block: %v\n", err)
					return
				}
			}
		}
	}
}

func readMsg(conn net.Conn) (*Msg, error) {
	msgBuf := make([]byte, 51)
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
