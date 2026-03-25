package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

var (
	activeConns   int64
	doneReceived  int32
	shutdownTimer *time.Timer
)

func serverHandleReq(conn net.Conn, file *os.File, checksumCache *ChecksumCache) {
	defer conn.Close()
	magicBytes := stringToFixedSizeArray(magicHead)

	filebuf := make([]byte, blockSize)
	msgBuf := make([]byte, binary.Size(Msg{}))

	var lastBlockNum uint32 = 0

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
			Log("\t- unpacked Msg-> %v\n", msg)
		}

		if msg.BlockSize != blockSize {
			blockSize = msg.BlockSize
			filebuf = make([]byte, blockSize)
		}

		offset := int64(msg.BlockIdx) * int64(blockSize)

		if msg.FileSize > 0 {
			if lastBlockNum == 0 {
				lastBlockNum = uint32(msg.FileSize / uint64(blockSize))
				truncateIfRegularFile(file, msg.FileSize)
			}
		}

		// Handle zero blocks (Zero=true, DataSize=0) - no data sent
		if msg.Zero && msg.DataSize == 0 {
			// Check if destination block is already zero or doesn't exist (EOF)
			destHash := checksumCache.WaitFor(msg.BlockIdx)
			if bytes.Equal(destHash, zeroBlockHash) || string(destHash) == "EOF" {
				// Destination already zero or beyond file size - nothing to do
				if debug {
					Log("\t- zero block at offset %d, already zero/EOF, skipping\n", offset)
				}
				continue
			}

			// Destination is non-zero - write zeros to overwrite
			if debug {
				Log("\t- zero block at offset %d, writing zeros to overwrite\n", offset)
			}
			zero := getZeroBuf(int(blockSize))
			n, err := file.WriteAt(zero, offset)
			if err != nil && err != io.EOF {
				Log("\t- error writing zero block: [%d] %s\n", n, err.Error())
				break
			}
			continue
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

		// Check for DONE message
		if msg.Done {
			Log("\ntransfer DONE message received, starting graceful shutdown\n")
			atomic.StoreInt32(&doneReceived, 1)
			// Start shutdown timer in main loop
			if shutdownTimer != nil {
				shutdownTimer.Reset(10 * time.Second)
			}
			return
		}

	}
}

func startServer(file *os.File, bindIp, port string, checksumCache *ChecksumCache) {
	bindTo := ":" + port
	if bindIp != "0.0.0.0" {
		bindTo = bindIp + ":" + port
	}

	listener, err := net.Listen("tcp", bindTo)
	if err != nil {
		Err("listening: %s\n", err.Error())
		return
	}
	defer listener.Close()

	Log("READY, listening on %s\n", bindTo)

	// Context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Idle timeout: exit after 2 seconds of no connections
	idleTimer := time.NewTimer(2 * time.Second)
	idleTimer.Stop() // Don't start until we've had at least one connection

	// Shutdown timer: started when DONE received
	shutdownTimer = time.NewTimer(10 * time.Second)
	shutdownTimer.Stop() // Don't start until DONE received

	connChan := make(chan net.Conn)
	errChan := make(chan error)

	// Accept connections in goroutine with context cancellation
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				conn, err := listener.Accept()
				if err != nil {
					select {
					case errChan <- err:
					case <-ctx.Done():
					}
					return
				}
				select {
				case connChan <- conn:
				case <-ctx.Done():
					conn.Close()
					return
				}
			}
		}
	}()

	hadConnection := false

	for {
		select {
		case conn := <-connChan:
			hadConnection = true
			idleTimer.Stop()
			shutdownTimer.Stop() // Stop shutdown timer when new activity
			atomic.AddInt64(&activeConns, 1)
			go func(c net.Conn) {
				defer atomic.AddInt64(&activeConns, -1)
				serverHandleReq(c, file, checksumCache)

				// Check if we should exit after DONE
				if atomic.LoadInt32(&doneReceived) == 1 {
					active := atomic.LoadInt64(&activeConns)
					if active == 0 {
						Log("All connections finished after DONE, exiting\n")
						return
					}
					Log("Graceful shutdown: %d connections still active, waiting...\n", active)
				}

				// Normal idle timer (only when NOT done)
				if atomic.LoadInt64(&activeConns) == 0 && hadConnection && atomic.LoadInt32(&doneReceived) == 0 {
					Log("Last connection closed, starting idle timer\n")
					idleTimer.Reset(2 * time.Second)
				}
			}(conn)

		case err := <-errChan:
			Log("Error accepting: %s\n", err.Error())
			return

		case <-idleTimer.C:
			if atomic.LoadInt64(&activeConns) == 0 && atomic.LoadInt32(&doneReceived) == 0 {
				Log("Server idle timeout, exiting\n")
				return
			}

		case <-shutdownTimer.C:
			active := atomic.LoadInt64(&activeConns)
			if active == 0 {
				Log("Graceful shutdown: no active connections, exiting\n")
				return
			}
			Log("Graceful shutdown: %d connections still active, waiting another 10s\n", active)
			shutdownTimer.Reset(10 * time.Second)
		}
	}
}

// startServerUpload serves file blocks to requesting clients (upload mode)
func startServerUpload(file *os.File, bindIp, port string, fileSize uint64, checksumCache *ChecksumCache, workers int) {
	bindTo := ":" + port
	if bindIp != "0.0.0.0" {
		bindTo = bindIp + ":" + port
	}

	listener, err := net.Listen("tcp", bindTo)
	if err != nil {
		Err("listening: %s\n", err.Error())
		return
	}
	defer listener.Close()

	Log("READY for upload, listening on %s\n", bindTo)

	for {
		conn, err := listener.Accept()
		if err != nil {
			Log("Error accepting: %s\n", err.Error())
			return
		}
		go serverHandleUpload(conn, file, fileSize, checksumCache)
	}
}

// serverHandleUpload handles upload requests from clients
func serverHandleUpload(conn net.Conn, file *os.File, fileSize uint64, checksumCache *ChecksumCache) {
	Log("serverHandleUpload()\n")
	defer conn.Close()
	magicBytes := stringToFixedSizeArray(magicHead)

	filebuf := make([]byte, blockSize)
	msgBuf := make([]byte, binary.Size(Msg{}))

	lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

	c := bufio.NewReader(conn)

	for {
		_, err1 := io.ReadFull(c, msgBuf)
		if err1 != nil {
			Log("\t- (upload) connection ended: %s\n", err1)
			return
		}

		msg, err2 := unpack(msgBuf)
		if err2 != nil {
			Log("\t- unpack msg failed: %s\n", err2)
			return
		}

		if msg.MagicHead != magicBytes {
			conn.Close()
			return
		}

		if debug {
			Log("\t- unpacked upload Msg-> %v\n", msg)
		}

		// If this is the first request, send file metadata
		if msg.BlockIdx == 0 && msg.FileSize == 0 {
			metaMsg, err1 := pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   0,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   0,
				Compressed: false,
				Zero:       false,
				Done:       false,
			})
			if err1 != nil {
				Log("cant pack metadata msg-> %s\n", err1)
				return
			}

			n, err2 := conn.Write(metaMsg)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing metadata: [%d] %s\n", n, err2.Error())
				return
			}
			continue
		}

		// Handle block request
		offset := int64(msg.BlockIdx) * int64(blockSize)

		if msg.BlockIdx > lastBlockNum {
			// Send done message
			doneMsg, err1 := pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   msg.BlockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   0,
				Compressed: false,
				Zero:       false,
				Done:       true,
			})
			if err1 != nil {
				Log("cant pack done msg-> %s\n", err1)
				return
			}

			n, err2 := conn.Write(doneMsg)
			if err2 != nil && err2 != io.EOF {
				Log("\t- error writing done msg: [%d] %s\n", n, err2.Error())
				return
			}
			return
		}

		// Read block from file
		n, err := file.ReadAt(filebuf, offset)
		if err != nil && err != io.EOF {
			Log("\t- error reading from file: [%d] %s\n", n, err.Error())
			break
		}

		// Get precomputed hash
		hash := checksumCache.WaitFor(msg.BlockIdx)

		// Check if block is zero - send only Msg, NO data (sparse file optimization)
		if bytes.Equal(hash, zeroBlockHash) {
			respMsg, err1 := pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   msg.BlockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   0, // NO data payload
				Compressed: false,
				Zero:       true,
				Done:       false,
			})
			if err1 != nil {
				Log("\t- cant pack zero msg-> %s\n", err1)
				return
			}
			connWrite(conn, respMsg)
			// DON'T send filebuf[:n] - client knows it's zero, will create sparse hole
			continue
		}

		// Compress if beneficial
		compBuf, err := compressData(filebuf[:n])
		if err != nil {
			Log("Error: compressing upload data: %s\n", err)
			break
		}

		compressedBytes := uint32(len(compBuf))

		if compressedBytes < uint32(n) {
			// Send compressed
			msg, err1 := pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   msg.BlockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   compressedBytes,
				Compressed: true,
				Zero:       false,
				Done:       false,
			})
			if err1 != nil {
				Log("\t- cant pack compressed msg-> %s\n", err1)
				return
			}
			connWrite(conn, msg)
			connWrite(conn, compBuf)
		} else {
			// Send uncompressed
			msg, err1 := pack(&Msg{
				MagicHead:  magicBytes,
				BlockIdx:   msg.BlockIdx,
				BlockSize:  blockSize,
				FileSize:   fileSize,
				DataSize:   uint32(n),
				Compressed: false,
				Zero:       false,
				Done:       false,
			})
			if err1 != nil {
				Log("\t- cant pack uncompressed msg-> %s\n", err1)
				return
			}
			connWrite(conn, msg)
			connWrite(conn, filebuf[:n])
		}

		if debug {
			Log("\t- sent block [%d] %d bytes\n", msg.BlockIdx, n)
		}
	}
}
