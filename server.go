package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	activeConns   int64
	doneReceived  int32
	shutdownTimer *time.Timer

	// shutdownResetChan lets connection-handler goroutines request a
	// shutdownTimer reset without touching the Timer directly.
	// time.Timer.Reset called concurrently from multiple goroutines on a
	// possibly-already-fired timer is documented-unsafe; only startServer's
	// own select loop goroutine ever calls shutdownTimer.Reset, and every
	// other goroutine sends the desired duration here instead.
	shutdownResetChan chan time.Duration

	// Server-side progress stats
	suppressProgress   bool // set via -P flag when launched as remote server via -t
	suppressProgressMu sync.RWMutex
	srvOnce            sync.Once
	srvT0              time.Time
	// 64-bit: a uint32 block count overflows at 4G blocks (~16TB at -b 4K).
	srvLastBlockNum uint64 // written once via atomic.StoreUint64, then read-only
	srvFileSize     uint64
	srvBytesNet     uint64 // atomic - compressed/raw bytes received over network
	srvBytesOrig    uint64 // atomic - original bytes written (for ratio)
	srvDiffs        int64  // atomic - blocks actually written

	// Visualization
	srvVisualizer *Visualizer
)

func setSuppressProgress(enabled bool) {
	suppressProgressMu.Lock()
	suppressProgress = enabled
	suppressProgressMu.Unlock()
}

// requestShutdownReset asks startServer's select loop to reset shutdownTimer
// to d. Safe to call from any goroutine. The send is non-blocking: a full
// channel just means a reset is already pending, so dropping this one is
// harmless. A nil channel (serverHandleReq invoked directly, e.g. in tests,
// without startServer ever running) is a silent no-op, matching the previous
// `if shutdownTimer != nil` guard's behavior.
func requestShutdownReset(d time.Duration) {
	if shutdownResetChan == nil {
		return
	}
	select {
	case shutdownResetChan <- d:
	default:
	}
}

func suppressProgressEnabled() bool {
	suppressProgressMu.RLock()
	defer suppressProgressMu.RUnlock()
	return suppressProgress
}

func serverPrintStats(blockIdx uint32, indicator string, netBytes uint32, origBytes uint32) {
	if suppressProgressEnabled() {
		return
	}
	// Skip progress output when visualizer is active
	if visualEnabled() {
		return
	}
	if netBytes > 0 {
		atomic.AddUint64(&srvBytesNet, uint64(netBytes))
		// origBytes is the actual decoded/expected size of this block (the
		// last block of a file is typically shorter than blockSize), not the
		// global block size — using the global here made the reported
		// compression ratio wrong for any non-block-size-multiple file.
		atomic.AddUint64(&srvBytesOrig, uint64(origBytes))
		atomic.AddInt64(&srvDiffs, 1)
	}

	last := atomic.LoadUint64(&srvLastBlockNum)
	if last == 0 {
		return
	}

	currentBlockSize := atomic.LoadUint32(&blockSize)
	blockMb := float64(currentBlockSize) / float64(mb1)
	elapsed := time.Since(srvT0).Seconds()

	var mbs float64
	if elapsed > 0 {
		mbs = float64(blockIdx) * blockMb / elapsed
	}

	blocksLeft := float64(last) - float64(blockIdx)
	eta := 0
	etaUnit := "min"
	if mbs > 0 {
		eta = int(blocksLeft * blockMb / mbs / 60)
		if eta > 180 {
			eta /= 60
			etaUnit = "hr"
		}
	}
	if eta < 0 {
		eta = 0
	}
	if uint64(blockIdx) >= last {
		eta = 0
	}

	percent := 100.0 * float64(blockIdx) / float64(last)
	totalNet := atomic.LoadUint64(&srvBytesNet)
	totalOrig := atomic.LoadUint64(&srvBytesOrig)
	ratio := 100.0
	if totalOrig > 0 {
		ratio = 100.0 * float64(totalNet) / float64(totalOrig)
	}
	diffs := atomic.LoadInt64(&srvDiffs)

	Log("recv block %d/%d (%0.2f%%) [%s] size=%d ratio=%0.2f %0.2f MB/s ETA=%d %s diffs=%d\r",
		blockIdx, last, percent, indicator, srvFileSize, ratio, mbs, eta, etaUnit, diffs)
}

func sendBlockAck(conn net.Conn, blockIdx, currentBlockSize uint32, fileSize uint64) error {
	ack, err := pack(&Msg{
		MagicHead: stringToFixedSizeArray(magicHead),
		Op:        uint8(OpAck),
		FileID:    0,
		BlockIdx:  blockIdx,
		BlockSize: currentBlockSize,
		FileSize:  fileSize,
	})
	if err != nil {
		return err
	}
	return connWrite(conn, ack)
}

func writeBlock(file *os.File, data []byte, offset int64) error {
	n, err := file.WriteAt(data, offset)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// writeDestBlock routes a destination write through the shared OrderedWriter
// (which serializes the scattered writes of concurrent connection handlers into
// a near-sequential, HDD-friendly stream) when one is available, otherwise it
// writes directly. SubmitAndWait blocks until the block is durably written, so
// the per-block acknowledgement contract is preserved.
func writeDestBlock(ow *OrderedWriter, file *os.File, blockIdx uint32, data []byte, offset int64) error {
	if ow != nil {
		return ow.SubmitAndWait(blockIdx, offset, data)
	}
	return writeBlock(file, data, offset)
}

func serverHandleReq(conn net.Conn, file *os.File, _ *ChecksumCache, ow *OrderedWriter) error {
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(keepAlivePeriod)
	}

	magicBytes := stringToFixedSizeArray(magicHead)

	// Reserve room for encryption overhead: an encrypted, uncompressed block
	// is blockSize+encryptOverhead bytes on the wire.
	initialBlockSize := atomic.LoadUint32(&blockSize)
	filebuf := make([]byte, initialBlockSize+encryptOverhead)
	msgBuf := make([]byte, msgSize)

	var lastBlockNum uint32 = 0
	var currentBlockSize uint32 = initialBlockSize
	var pendingWrite *Msg

	c := bufio.NewReader(conn)

	for {
		conn.SetDeadline(time.Now().Add(ioTimeout))
		_, err1 := io.ReadFull(c, msgBuf)
		if err1 != nil {
			if err1 == io.EOF || err1 == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read request: %w", err1)
		}

		msg, err2 := unpack(msgBuf)
		if err2 != nil {
			return fmt.Errorf("unpack request: %w", err2)
		}

		if msg.MagicHead != magicBytes {
			return fmt.Errorf("invalid request magic")
		}
		if msg.BlockSize == 0 {
			return fmt.Errorf("invalid zero block size")
		}
		if err := validateClientFrame(msg); err != nil {
			return err
		}

		if debug {
			Log("\t- unpacked Msg-> %v\n", msg)
		}

		// Use atomic load/store for blockSize to avoid race conditions
		currentBlockSize = atomic.LoadUint32(&blockSize)
		if msg.BlockSize != currentBlockSize {
			atomic.StoreUint32(&blockSize, msg.BlockSize)
			currentBlockSize = msg.BlockSize
			filebuf = make([]byte, currentBlockSize+encryptOverhead)
		}

		offset := int64(msg.BlockIdx) * int64(currentBlockSize)

		// Handle DONE before any block processing. The DONE message carries
		// the final source size, including zero for an empty source.
		if Op(msg.Op) == OpFileDone {
			truncateIfRegularFile(file, msg.FileSize)
			Log("\ntransfer DONE message received, starting graceful shutdown\n")
			if srvVisualizer != nil {
				srvVisualizer.RenderFinal()
			}
			atomic.StoreInt32(&doneReceived, 1)
			requestShutdownReset(shutdownLingerPeriod)
			return nil
		}

		expectedSize := blockPayloadSize(msg.FileSize, currentBlockSize, msg.BlockIdx)
		if expectedSize == 0 {
			return fmt.Errorf("invalid block %d for source size %d", msg.BlockIdx, msg.FileSize)
		}
		lastBlockNum = uint32((msg.FileSize - 1) / uint64(currentBlockSize))
		truncateIfRegularFile(file, msg.FileSize)
		srvOnce.Do(func() {
			srvFileSize = msg.FileSize
			atomic.StoreUint64(&srvLastBlockNum, uint64(lastBlockNum))
			srvT0 = time.Now()
			// Initialize visualizer with total block count
			if visualEnabled() {
				srvVisualizer = NewVisualizer(lastBlockNum + 1)
				srvVisualizer.renderInitial()
			}
		})

		if uint64(msg.DataSize) > uint64(currentBlockSize)+uint64(encryptOverhead) {
			return fmt.Errorf("payload for block %d is too large", msg.BlockIdx)
		}
		msgOp := Op(msg.Op)
		isWrite := msgOp == OpBlockZero || msgOp == OpBlockData
		if isWrite {
			if pendingWrite == nil || msg.BlockIdx != pendingWrite.BlockIdx ||
				msg.BlockSize != pendingWrite.BlockSize || msg.FileSize != pendingWrite.FileSize {
				return fmt.Errorf("unsolicited or mismatched write for block %d", msg.BlockIdx)
			}
		}

		// Handle zero blocks - no data sent
		if msgOp == OpBlockZero {
			if err := writeDestBlock(ow, file, msg.BlockIdx, getZeroBuf(int(expectedSize)), offset); err != nil {
				return fmt.Errorf("write zero block %d: %w", msg.BlockIdx, err)
			}
			serverPrintStats(msg.BlockIdx, ".", 0, expectedSize)
			if srvVisualizer != nil {
				srvVisualizer.UpdateBlockStatus(msg.BlockIdx, StatusZero)
			}
			if err := sendBlockAck(conn, msg.BlockIdx, currentBlockSize, msg.FileSize); err != nil {
				return nil
			}
			pendingWrite = nil
			continue
		}

		if msgOp == OpBlockCheck {
			n, err := file.ReadAt(filebuf[:expectedSize], offset)
			if err != nil && err != io.EOF {
				return fmt.Errorf("read destination block %d: %w", msg.BlockIdx, err)
			}
			if n != int(expectedSize) {
				return fmt.Errorf("read destination block %d: %w", msg.BlockIdx, io.ErrUnexpectedEOF)
			}

			var hash []byte
			if isZeroBlock(filebuf[:expectedSize]) {
				hash = zeroBlockHash
			} else {
				hash = checksum(filebuf[:expectedSize])
			}
			// Framed as a proper OpHash message (header + payload) rather
			// than bare bytes: it lets the client tell a hash apart from a
			// truncated/desynced stream, and reserves DataSize/Flags for a
			// future "destination doesn't exist yet" response.
			hashMsg, err := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpHash),
				FileID:    0,
				BlockIdx:  msg.BlockIdx,
				BlockSize: currentBlockSize,
				FileSize:  msg.FileSize,
				DataSize:  uint32(len(hash)),
			})
			if err != nil {
				return fmt.Errorf("pack hash: %w", err)
			}
			if err := connWrite(conn, hashMsg); err != nil {
				return nil
			}
			if err := connWrite(conn, hash[:]); err != nil {
				return nil
			}
			request := *msg
			pendingWrite = &request
			serverPrintStats(msg.BlockIdx, "-", 0, expectedSize)
			continue
		}

		if msgOp == OpBlockData {
			if srvVisualizer != nil {
				srvVisualizer.UpdateBlockStatus(msg.BlockIdx, StatusProcessing)
			}
			conn.SetDeadline(time.Now().Add(ioTimeout))
			_, err1 := io.ReadFull(c, filebuf[:msg.DataSize])
			if err1 != nil {
				return nil
			}

			compressed := msg.Flags&FlagCompressed != 0
			var payload []byte
			if compressed {
				decompressed, err := decompressData(filebuf[:msg.DataSize])
				if err != nil {
					return fmt.Errorf("decompress block %d: %w", msg.BlockIdx, err)
				}
				payload = decompressed
			} else {
				if IsEncryptionEnabled() {
					plain, err := decryptBlock(filebuf[:msg.DataSize])
					if err != nil {
						return fmt.Errorf("decrypt block %d: %w", msg.BlockIdx, err)
					}
					payload = plain
				} else {
					payload = make([]byte, msg.DataSize)
					copy(payload, filebuf[:msg.DataSize])
				}
			}
			if uint32(len(payload)) != expectedSize {
				return fmt.Errorf("block %d has invalid decoded size %d", msg.BlockIdx, len(payload))
			}
			if err := writeDestBlock(ow, file, msg.BlockIdx, payload, offset); err != nil {
				return fmt.Errorf("write block %d: %w", msg.BlockIdx, err)
			}
			indicator := "w"
			if compressed {
				indicator = "c"
			}
			serverPrintStats(msg.BlockIdx, indicator, msg.DataSize, expectedSize)
			if srvVisualizer != nil {
				if compressed {
					srvVisualizer.UpdateBlockStatus(msg.BlockIdx, StatusCompressed)
				} else {
					srvVisualizer.UpdateBlockStatus(msg.BlockIdx, StatusRaw)
				}
			}
			if err := sendBlockAck(conn, msg.BlockIdx, currentBlockSize, msg.FileSize); err != nil {
				return nil
			}
			pendingWrite = nil
		}
	}
}

func startServer(file *os.File, bindIp, port string, checksumCache *ChecksumCache, workers int) error {
	bindTo := ":" + port
	if bindIp != "0.0.0.0" {
		bindTo = bindIp + ":" + port
	}

	listener, err := net.Listen("tcp", bindTo)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	defer listener.Close()

	Log("READY, listening on %s\n", bindTo)

	// One shared ordered writer serializes the scattered WriteAt calls of all
	// concurrent connection handlers into a near-sequential stream, which keeps
	// a spinning destination disk running at full write speed under -w > 1.
	ow := NewOrderedWriter(file, workers)
	defer ow.Close()

	// Context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Idle timeout: exit after 2 seconds of no connections
	idleTimer := time.NewTimer(2 * time.Second)
	idleTimer.Stop() // Don't start until we've had at least one connection

	// Shutdown timer: started when DONE received
	shutdownTimer = time.NewTimer(shutdownLingerPeriod)
	shutdownTimer.Stop() // Don't start until DONE received
	shutdownResetChan = make(chan time.Duration, 8)

	connChan := make(chan net.Conn)
	errChan := make(chan error)
	handlerErrChan := make(chan error)

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

	for {
		select {
		case conn := <-connChan:
			idleTimer.Stop()
			shutdownTimer.Stop() // Stop shutdown timer when new activity
			atomic.AddInt64(&activeConns, 1)
			go func(c net.Conn) {
				defer atomic.AddInt64(&activeConns, -1)
				handlerErr := serverHandleReq(c, file, checksumCache, ow)
				if handlerErr != nil {
					select {
					case handlerErrChan <- handlerErr:
					case <-ctx.Done():
					}
					return
				}

				// Exclude self (still counted until the deferred decrement)
				// so `remaining` reflects other live connections.
				remaining := atomic.LoadInt64(&activeConns) - 1

				// Exit path after DONE: leave promptly when no others remain.
				if atomic.LoadInt32(&doneReceived) == 1 {
					if remaining <= 0 {
						Log("All connections finished after DONE, exiting\n")
						requestShutdownReset(shutdownGracePeriod)
					} else {
						Log("Graceful shutdown: %d connections still active, waiting...\n", remaining)
						requestShutdownReset(shutdownLingerPeriod)
					}
					return
				}

				// Normal idle timer (only when NOT done). A connection
				// goroutine only runs after at least one connection, so no
				// "had a connection" guard is needed here.
				if remaining <= 0 {
					Log("Last connection closed, starting idle timer\n")
					idleTimer.Reset(2 * time.Second)
				}
			}(conn)

		case err := <-errChan:
			return fmt.Errorf("accept: %w", err)

		case err := <-handlerErrChan:
			return err

		case <-idleTimer.C:
			if atomic.LoadInt64(&activeConns) == 0 && atomic.LoadInt32(&doneReceived) == 0 {
				Log("Server idle timeout, exiting\n")
				return nil
			}

		case d := <-shutdownResetChan:
			// Only this goroutine ever calls shutdownTimer.Reset; other
			// goroutines request a reset via requestShutdownReset instead.
			shutdownTimer.Reset(d)

		case <-shutdownTimer.C:
			active := atomic.LoadInt64(&activeConns)
			if active == 0 {
				Log("Graceful shutdown: no active connections, exiting\n")
				return nil
			}
			Log("Graceful shutdown: %d connections still active, waiting another %s\n", active, shutdownLingerPeriod)
			shutdownTimer.Reset(shutdownLingerPeriod)
		}
	}
}

// startServerUpload serves file blocks to requesting clients (upload mode)
func startServerUpload(file *os.File, bindIp, port string, fileSize uint64, checksumCache *ChecksumCache, workers int, noCompress bool) error {
	bindTo := ":" + port
	if bindIp != "0.0.0.0" {
		bindTo = bindIp + ":" + port
	}

	listener, err := net.Listen("tcp", bindTo)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	defer listener.Close()

	Log("READY for upload, listening on %s\n", bindTo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Idle exit: once a client has connected and all connections close, exit
	// after a short idle period instead of Accept-looping forever. This lets
	// `-d -t` (download via SSH) terminate cleanly rather than hanging on
	// sshCmd.Wait().
	var upActive int64
	idleTimer := time.NewTimer(uploadIdlePeriod)
	idleTimer.Stop()

	connChan := make(chan net.Conn)
	errChan := make(chan error)
	handlerErrChan := make(chan error)

	go func() {
		for {
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
	}()

	for {
		select {
		case conn := <-connChan:
			idleTimer.Stop()
			atomic.AddInt64(&upActive, 1)
			go func(c net.Conn) {
				handlerErr := serverHandleUpload(c, file, fileSize, checksumCache, workers, noCompress)
				if handlerErr != nil {
					select {
					case handlerErrChan <- handlerErr:
					case <-ctx.Done():
					}
					return
				}
				if atomic.AddInt64(&upActive, -1) == 0 {
					idleTimer.Reset(uploadIdlePeriod)
				}
			}(conn)

		case err := <-errChan:
			return fmt.Errorf("accept: %w", err)

		case err := <-handlerErrChan:
			return err

		case <-idleTimer.C:
			if atomic.LoadInt64(&upActive) == 0 {
				Log("Upload server idle, exiting\n")
				return nil
			}
		}
	}
}

// serverHandleUpload handles upload requests from clients.
//
// The download client requests blocks strictly in ascending, contiguous order
// over a single connection, so we drive a single sequential read-ahead reader
// (SequentialReader) that keeps the source HDD reading as one continuous
// stream and overlaps disk I/O with network transfer, instead of doing one
// idle round-trip per block. If a request ever arrives out of the expected
// order we transparently fall back to a direct ReadAt for that block.
func serverHandleUpload(conn net.Conn, file *os.File, fileSize uint64, checksumCache *ChecksumCache, workers int, noCompress bool) error {
	Log("serverHandleUpload()\n")
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(keepAlivePeriod)
	}

	magicBytes := stringToFixedSizeArray(magicHead)

	filebuf := make([]byte, blockSize)
	msgBuf := make([]byte, msgSize)

	var lastBlockNum uint32
	if fileSize > 0 {
		lastBlockNum = uint32((fileSize - 1) / uint64(blockSize))
	}

	// Sequential read-ahead, lazily started on the first block request so we
	// can seed it at the client's starting block (honors -s skip).
	// Use global prefetchDepth from main.go (default: workers*4)
	depth := prefetchDepth
	if depth < 4 {
		depth = 4
	}
	var pf *SequentialReader
	var pfChan <-chan BlockData

	c := bufio.NewReader(conn)

	for {
		conn.SetDeadline(time.Now().Add(ioTimeout))
		_, err1 := io.ReadFull(c, msgBuf)
		if err1 != nil {
			if err1 == io.EOF || err1 == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read download request: %w", err1)
		}

		msg, err2 := unpack(msgBuf)
		if err2 != nil {
			return fmt.Errorf("unpack download request: %w", err2)
		}

		if msg.MagicHead != magicBytes {
			return fmt.Errorf("invalid download request magic")
		}
		if err := validateDownloadRequest(msg); err != nil {
			return err
		}

		if debug {
			Log("\t- unpacked upload Msg-> %v\n", msg)
		}

		// If this is the first request, send file metadata
		if msg.BlockIdx == 0 && msg.FileSize == 0 {
			metaMsg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpDownData),
				FileID:    0,
				BlockIdx:  0,
				BlockSize: blockSize,
				FileSize:  fileSize,
				DataSize:  0,
			})
			if err1 != nil {
				return fmt.Errorf("pack metadata: %w", err1)
			}

			if err := connWrite(conn, metaMsg); err != nil {
				return fmt.Errorf("write metadata: %w", err)
			}
			continue
		}
		if msg.BlockSize != blockSize || msg.FileSize != fileSize {
			return fmt.Errorf("invalid download request metadata")
		}

		// Handle block request
		offset := int64(msg.BlockIdx) * int64(blockSize)

		if fileSize == 0 || msg.BlockIdx > lastBlockNum {
			// Send done message
			doneMsg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpDownDone),
				FileID:    0,
				BlockIdx:  msg.BlockIdx,
				BlockSize: blockSize,
				FileSize:  fileSize,
				DataSize:  0,
			})
			if err1 != nil {
				return fmt.Errorf("pack download done: %w", err1)
			}

			if err := connWrite(conn, doneMsg); err != nil {
				return fmt.Errorf("write download done: %w", err)
			}
			return nil
		}

		// Lazily start the sequential read-ahead at the first requested block.
		if pf == nil {
			pf = NewSequentialReader(file, blockSize, fileSize, msg.BlockIdx, depth)
			pf.Start()
			pfChan = pf.Blocks()
		}

		// Read block: prefer the sequential prefetch stream; fall back to a
		// direct read if the request is out of the expected order.
		var data []byte
		if bd, ok := <-pfChan; ok && bd.BlockIdx == msg.BlockIdx {
			data = bd.Data
		} else {
			n, err := file.ReadAt(filebuf, offset)
			if err != nil && err != io.EOF {
				return fmt.Errorf("read source block %d: %w", msg.BlockIdx, err)
			}
			data = filebuf[:n]
		}
		n := len(data)

		// Get precomputed hash (currently unused beyond synchronizing with the
		// background precompute pass; see the zero-block check below for why
		// its value must not be trusted for zero-ness).
		_ = checksumCache.WaitFor(msg.BlockIdx)

		// Check if block is zero - send only Msg, NO data (sparse file optimization).
		//
		// Deciding this from the hash (bytes.Equal(hash, zeroBlockHash)) is
		// unsafe: zeroBlockHash is 16 zero bytes, and ChecksumCache.WaitFor
		// also returns 16 zero bytes for an out-of-range or not-yet-computed
		// index (checksum.go's noPrecompute/out-of-bounds path). An unknown
		// hash would then be misreported as a zero block, and the client
		// would zero-fill the destination with data that was never verified.
		// Deciding straight from the just-read bytes has no such ambiguity.
		if isZeroBlock(data) {
			respMsg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpDownData),
				Flags:     FlagZero,
				FileID:    0,
				BlockIdx:  msg.BlockIdx,
				BlockSize: blockSize,
				FileSize:  fileSize,
				DataSize:  0, // NO data payload
			})
			if err1 != nil {
				return fmt.Errorf("pack zero response: %w", err1)
			}
			if err := connWrite(conn, respMsg); err != nil {
				return fmt.Errorf("send zero response: %w", err)
			}
			// Do not send zero payload bytes; the client clears the destination range.
			continue
		}

		// Compress if beneficial, unless the user disabled compression (-n).
		var compBuf []byte
		if !noCompress {
			var cerr error
			compBuf, cerr = compressData(data)
			if cerr != nil {
				return fmt.Errorf("compress source block %d: %w", msg.BlockIdx, cerr)
			}
		}

		if !noCompress && uint32(len(compBuf)) < uint32(n) {
			// Send compressed (compressData also encrypts when -e is set)
			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpDownData),
				Flags:     FlagCompressed,
				FileID:    0,
				BlockIdx:  msg.BlockIdx,
				BlockSize: blockSize,
				FileSize:  fileSize,
				DataSize:  uint32(len(compBuf)),
			})
			if err1 != nil {
				return fmt.Errorf("pack compressed response: %w", err1)
			}
			if err := connWrite(conn, msg); err != nil {
				return fmt.Errorf("send compressed header: %w", err)
			}
			if err := connWrite(conn, compBuf); err != nil {
				return fmt.Errorf("send compressed payload: %w", err)
			}
		} else {
			// Send uncompressed. When encryption is on, encrypt the raw block
			// so -n still produces an encrypted stream.
			var payload []byte
			var pooledPayload []byte // Track to return to pool after send
			if IsEncryptionEnabled() {
				payload = encryptBlock(data)
			} else {
				// CRITICAL FIX: data points to a reused buffer (either bd.Data from
				// SequentialReader or filebuf[:n] from direct ReadAt). connWrite
				// reads asynchronously and the buffer will be overwritten in the next
				// loop iteration, so we must copy the data first.
				// Use pooled buffer to reduce GC pressure.
				payload = GetPayload(data)
				pooledPayload = payload
			}
			msg, err1 := pack(&Msg{
				MagicHead: magicBytes,
				Op:        uint8(OpDownData),
				FileID:    0,
				BlockIdx:  msg.BlockIdx,
				BlockSize: blockSize,
				FileSize:  fileSize,
				DataSize:  uint32(len(payload)),
			})
			if err1 != nil {
				if pooledPayload != nil {
					PutPayload(pooledPayload)
				}
				return fmt.Errorf("pack uncompressed response: %w", err1)
			}
			if err := connWrite(conn, msg); err != nil {
				if pooledPayload != nil {
					PutPayload(pooledPayload)
				}
				return fmt.Errorf("send uncompressed header: %w", err)
			}
			if err := connWrite(conn, payload); err != nil {
				if pooledPayload != nil {
					PutPayload(pooledPayload)
				}
				return fmt.Errorf("send uncompressed payload: %w", err)
			}
			// Return pooled buffer to pool for reuse
			if pooledPayload != nil {
				PutPayload(pooledPayload)
			}
		}

		if debug {
			Log("\t- sent block [%d] %d bytes\n", msg.BlockIdx, n)
		}
	}
}
