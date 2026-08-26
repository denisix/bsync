package main

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Reader buffer pool for reusing read buffers
// Pool buffers are locked in memory on creation to prevent swap
// Buffers use huge pages (MADV_HUGEPAGE) to reduce TLB misses
var readerBufPool = sync.Pool{
	New: func() interface{} {
		// Pre-allocate with 10MB capacity for typical block size
		buf := make([]byte, 0, 10*1024*1024)
		// Lock the full capacity to prevent swap
		TryLockBuffer(buf[:cap(buf)])
		// Advise kernel to use huge pages (2MB) for better TLB utilization
		AdviseHugePage(buf[:cap(buf)])
		return buf
	},
}

// getReaderBuf gets a buffer from the pool or allocates a new one
func getReaderBuf(size uint32) []byte {
	buf := readerBufPool.Get().([]byte)
	if cap(buf) < int(size) {
		// Pool buffer too small, allocate new one (not returned to pool)
		// New buffers are automatically locked
		newBuf := make([]byte, size)
		TryLockBuffer(newBuf)
		return newBuf
	}
	return buf[:size]
}

// putReaderBuf returns a buffer to the pool if it's appropriately sized
func putReaderBuf(buf []byte) {
	if cap(buf) <= 10*1024*1024 {
		readerBufPool.Put(buf[:0])
	}
	// Large buffers are GC'd instead of polluting the pool
	// Note: munlock happens automatically when GC frees the memory
}

// FileBlock represents one block read from one file: its FileID (0 for the
// implicit single file), index, data, whether it's the file's last block,
// and how to release its buffer once the consumer is done with it. release
// is nil when the buffer isn't pool-owned (should not normally happen, but
// callers must check before calling it).
type FileBlock struct {
	FileID   uint32
	BlockIdx uint32
	Data     []byte
	Last     bool
	release  func()
}

// BlockData is FileBlock under its original single-file-mode name. Kept as
// an alias (not a second type) so SequentialReader and its existing callers
// need no changes: BlockData{BlockIdx: ..., Data: ...} still works exactly
// as before, with FileID/Last/release simply left at their zero values.
type BlockData = FileBlock

// readFileBlocks reads blocks [skipIdx..lastBlockNum] of file into out, in
// ascending order, tagging each with fileID. It does not close out — the
// caller (SequentialReader.Start() for a single file, TreeReader across a
// whole tree) owns that decision, since a tree reader must keep the channel
// open across a file boundary.
//
// acquire, if non-nil, replaces the plain getReaderBuf/putReaderBuf pairing
// used to obtain and release each block's buffer. TreeReader uses this to
// fold a byte-budget acquire/release into the read loop (see byteBudget),
// so a caller with no such need (SequentialReader) can pass nil and get
// today's plain pool checkout.
//
// If done is non-nil and closes while a send is blocked, readFileBlocks
// stops and returns nil — cancellation is a normal way to stop, not an
// error. Returns the number of blocks actually sent to out (needed by
// TreeReader callers to know exactly how many of a file's blocks will ever
// arrive when a failure — or cancellation — stops it short of the full
// count) and the first read error encountered, if any.
func readFileBlocks(file *os.File, fileID uint32, blockSize uint32, fileSize uint64,
	skipIdx uint32, out chan<- FileBlock, done <-chan struct{},
	acquire func(size uint32) (data []byte, release func())) (uint32, error) {
	if fileSize == 0 {
		return 0, nil
	}
	if acquire == nil {
		acquire = func(size uint32) ([]byte, func()) {
			buf := getReaderBuf(size)
			return buf, func() { putReaderBuf(buf) }
		}
	}

	var emitted uint32
	lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))
	for blockIdx := skipIdx; blockIdx <= lastBlockNum; blockIdx++ {
		// Read each block directly into its own buffer. The buffer is handed
		// off on the channel, so reading straight into it (instead of into a
		// shared scratch buffer and then copying) saves a full block-sized
		// memcpy per block.
		offset := int64(blockIdx) * int64(blockSize)
		buf, release := acquire(blockSize)
		n, err := file.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			release()
			return emitted, fmt.Errorf("read block %d: %w", blockIdx, err)
		}

		// We won't read this source range again; drop its clean page-cache
		// pages so a large transfer doesn't evict useful read-ahead pages.
		AdviseDontNeed(file, offset, int64(n))

		fb := FileBlock{
			FileID:   fileID,
			BlockIdx: blockIdx,
			Data:     buf[:n],
			Last:     blockIdx == lastBlockNum,
			release:  release,
		}
		select {
		case out <- fb:
			emitted++
		case <-done:
			release()
			return emitted, nil
		}
	}
	return emitted, nil
}

// SequentialReader reads blocks sequentially and pushes to channel
type SequentialReader struct {
	file      *os.File
	blockSize uint32
	fileSize  uint64
	skipIdx   uint32
	empty     bool // fileSize == 0: Start() emits nothing
	blockChan chan BlockData
	wg        sync.WaitGroup
}

// NewSequentialReader creates a new sequential reader.
//
// fileSize == 0 is handled explicitly: (fileSize-1)/blockSize would otherwise
// underflow the uint64 subtraction before truncating to uint32, producing a
// huge lastBlockNum and a multi-billion-iteration loop in Start(). Existing
// callers already guard against this at the call site, but the constructor
// is made safe on its own so future callers (e.g. a per-file reader in
// directory-tree mode) don't have to remember to.
func NewSequentialReader(file *os.File, blockSize uint32, fileSize uint64, skipIdx uint32, bufferAhead int) *SequentialReader {
	sr := &SequentialReader{
		file:      file,
		blockSize: blockSize,
		fileSize:  fileSize,
		skipIdx:   skipIdx,
		blockChan: make(chan BlockData, bufferAhead),
	}
	if fileSize == 0 {
		sr.empty = true
	}
	return sr
}

// Start begins reading blocks in the background
func (sr *SequentialReader) Start() {
	sr.wg.Add(1)
	go func() {
		defer sr.wg.Done()
		defer close(sr.blockChan)

		if sr.empty {
			return
		}

		if _, err := readFileBlocks(sr.file, 0, sr.blockSize, sr.fileSize, sr.skipIdx, sr.blockChan, nil, nil); err != nil {
			Log("sequential reader error: %s\n", err)
		}
	}()
}

// Blocks returns the channel for consumers
func (sr *SequentialReader) Blocks() <-chan BlockData {
	return sr.blockChan
}

// Wait waits for the reader to finish
func (sr *SequentialReader) Wait() {
	sr.wg.Wait()
}

// byteBudget is a weighted semaphore bounding how many bytes of unconsumed
// block data may be in flight at once. Needed because directory-tree mode
// has per-file block sizes: a fixed-COUNT prefetch queue (as single-file
// mode uses, sized workers*4) would be ~320MB at the 10MB default block
// size but only ~128KB for 4KB files — starving the small-file case exactly
// when it most needs deep buffering, since reading many tiny files is
// open()-bound rather than bandwidth-bound.
type byteBudget struct {
	mu   sync.Mutex
	cond *sync.Cond
	max  int64
	used int64
}

func newByteBudget(max int64) *byteBudget {
	b := &byteBudget{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes are available. A single request larger than
// the whole budget is still let through once the budget is empty (used==0),
// rather than deadlocking forever — matching single-file mode, where one
// oversized block simply occupies its one slot in the fixed-count queue.
func (b *byteBudget) acquire(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used > 0 && b.used+n > b.max {
		b.cond.Wait()
	}
	b.used += n
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.cond.Broadcast()
}

// TreeReader reads blocks across a stream of files, strictly one file at a
// time in the order files arrive on its input channel, preserving the
// single-sequential-reader property SequentialReader exists to provide even
// as boundaries cross files: file k+1 is not opened until file k's blocks
// have all been read (blocks from file k may still be draining downstream —
// that's memory pressure, not disk activity). Two files must never be read
// concurrently, or the source disk loses the sequential-read pattern this
// whole design exists to preserve.
//
// Unlike SequentialReader, which is rebuilt per file by its caller,
// TreeReader is built once for an entire tree and its output channel spans
// every file in it, so it owns closing that channel itself — once, when its
// input channel closes.
type TreeReader struct {
	files  <-chan *FileEntry
	out    chan FileBlock
	sess   *SyncSession
	budget *byteBudget
	wg     sync.WaitGroup

	// onFileFailed, if non-nil, is called synchronously (from the single
	// reader goroutine, so it must not block) whenever a file could not be
	// opened or fully read, reporting exactly how many of its blocks WERE
	// successfully emitted to out before the failure (0 for an open
	// failure). This precision matters: a downstream consumer counting
	// blocks per file (dirTransferState's per-file remaining counter)
	// would otherwise never see that file reach zero and never send its
	// OpFileDone -- but the fix must not just declare the file "done"
	// outright, since blocks that WERE emitted may still be in flight
	// through the transfer pipeline; declaring completion before they are
	// actually written risks truncating/closing the file out from under
	// them. Reporting the exact count lets the caller correct its
	// expected-block-count downward instead, so completion still waits for
	// every block that was really emitted. Set by the caller (e.g.
	// startDirClient wiring it to dirTransferState.forceFileDone) before
	// Start; nil is fine for callers that don't need it (tests, or a
	// future non-network consumer of TreeReader).
	onFileFailed func(fileID uint32, blocksEmitted uint32)
}

// NewTreeReader creates a TreeReader draining files. sess, if non-nil,
// receives a failFile call for a file that can't be opened or read, and the
// file is then skipped rather than aborting the whole tree. prefetchBytes
// bounds how many bytes of unread block data may be in flight at once (see
// byteBudget); <= 0 falls back to a small default.
func NewTreeReader(files <-chan *FileEntry, sess *SyncSession, prefetchBytes int64) *TreeReader {
	if prefetchBytes <= 0 {
		prefetchBytes = int64(minTreeBlock) * 4
	}
	return &TreeReader{
		files:  files,
		out:    make(chan FileBlock, 4),
		sess:   sess,
		budget: newByteBudget(prefetchBytes),
	}
}

// Start begins reading in the background.
func (tr *TreeReader) Start() {
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		defer close(tr.out)
		for fe := range tr.files {
			if fe.Kind != KindFile || fe.Size == 0 {
				continue // directories/skipped entries and empty files carry no blocks
			}
			if tr.sess != nil && tr.sess.isFailed(fe.ID) {
				continue
			}
			tr.readOneFile(fe)
		}
	}()
}

func (tr *TreeReader) readOneFile(fe *FileEntry) {
	f, err := OpenFileNoAtime(fe.AbsPath, os.O_RDONLY, 0)
	if err != nil {
		if tr.sess != nil {
			tr.sess.failFile(fe.ID, fmt.Errorf("open %s: %w", fe.RelPath, err))
		}
		if tr.onFileFailed != nil {
			tr.onFileFailed(fe.ID, 0)
		}
		return
	}
	AdviseSequential(f)

	acquire := func(size uint32) ([]byte, func()) {
		n := int64(size)
		tr.budget.acquire(n)
		buf := getReaderBuf(size)
		return buf, func() {
			putReaderBuf(buf)
			tr.budget.release(n)
		}
	}
	emitted, readErr := readFileBlocks(f, fe.ID, fe.BlockSize, fe.Size, 0, tr.out, nil, acquire)
	closeErr := f.Close()
	if readErr == nil {
		// Every block was emitted; only the close itself failed. Report
		// the full emitted count below (not 0) -- onFileFailed only needs
		// to correct the expected-block-count when FEWER than the whole
		// file's blocks were ever emitted, which isn't the case here.
		readErr = closeErr
	}
	if readErr != nil {
		if tr.sess != nil {
			tr.sess.failFile(fe.ID, fmt.Errorf("read %s: %w", fe.RelPath, readErr))
		}
		if tr.onFileFailed != nil {
			tr.onFileFailed(fe.ID, emitted)
		}
	}
}

// Blocks returns the channel of blocks read across the whole tree, in
// per-file sequential order but each tagged with FileID so consumers
// downstream of a fan-out (multiple hashing/transfer workers) can tell
// which file a block belongs to.
func (tr *TreeReader) Blocks() <-chan FileBlock {
	return tr.out
}

// Wait waits for the reader to finish (its input channel closed and every
// file drained).
func (tr *TreeReader) Wait() {
	tr.wg.Wait()
}
