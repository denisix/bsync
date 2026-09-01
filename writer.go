package main

import (
	"container/heap"
	"os"
	"sort"
	"sync"
)

// OrderedWriter serializes all writes to one or more destination files
// through a single goroutine and emits them in (near) ascending order. On a
// spinning disk this turns the scattered WriteAt calls of N parallel
// connection handlers into one mostly-sequential stream, which is what lets
// an HDD run at full write speed — including, in directory-tree mode,
// across file boundaries: file IDs are assigned in walk order, which
// roughly tracks on-disk layout, so ascending key approximates ascending
// platter position across files too, not just within one.
//
// Only changed blocks are written (in-sync blocks never reach here), so the
// indices are sparse and non-contiguous. We therefore cannot wait for an
// exact "next" index. Instead we keep a bounded reorder window: blocks
// accumulate in a min-heap keyed by (fileID, blockIdx), and once the window
// is full we pop the smallest and write it. Because the senders push blocks
// roughly in order (bounded by the worker count), this yields a
// monotonically increasing write offset in practice while guaranteeing
// progress and bounded memory.
//
// The single-file API (Submit/SubmitAndWait) is unchanged in behavior: it
// always targets the file NewOrderedWriter was given and keys purely on
// blockIdx (fileID 0), so today's single-file server path needs no changes.
// writeKey(0, blockIdx) == uint64(blockIdx), the same ordering as before.
type writeItem struct {
	key    uint64 // writeKey(fileID, blockIdx); orders across files too
	file   *os.File
	offset int64
	data   []byte     // bytes to write at offset (already decompressed/decrypted)
	done   chan error // non-nil for SubmitAndWait(File): receives the write result

	// drain, when set, is a barrier request rather than a real write: the
	// writer goroutine flushes everything currently buffered FOR THIS
	// ITEM'S FILE — file-scoped, not a full barrier — before signaling
	// drainDone. It used to flush the whole heap, which was correct but
	// meant every OpFileDone (serialized on the control goroutine) force-
	// flushed every other in-flight file's buffered writes too, defeating
	// the reorder window's write locality exactly when many small files
	// finalize back to back. Correctness only ever needed the drained
	// file's own bytes settled before finalize() truncates/closes/chmods
	// it; other files' items stay buffered and keep their ordering.
	drain     bool
	drainDone chan struct{}
}

// writeKey orders writes first by file (in walk/announce order), then by
// block index within it.
func writeKey(fileID, blockIdx uint32) uint64 {
	return uint64(fileID)<<32 | uint64(blockIdx)
}

type writeHeap []writeItem

func (h writeHeap) Len() int            { return len(h) }
func (h writeHeap) Less(i, j int) bool  { return h[i].key < h[j].key }
func (h writeHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *writeHeap) Push(x interface{}) { *h = append(*h, x.(writeItem)) }
func (h *writeHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

type OrderedWriter struct {
	file   *os.File // default target for the legacy blockIdx-only API
	ch     chan writeItem
	window int

	// perItemErr, when true, isolates a write failure to the item that
	// caused it instead of poisoning every subsequent write: a directory
	// tree's files are independent, so one file's I/O error must fail that
	// file (via the caller's failFile) and let the rest of the tree keep
	// going. The legacy single-file writer leaves this false, preserving
	// the original sticky-error contract (Submit/SubmitAndWait callers
	// never checked per-call errors, so the first failure has always had
	// to abort every write that follows).
	perItemErr bool

	errMu sync.Mutex
	err   error

	// syncEvery triggers an asynchronous SyncFileRange writeback once this many
	// bytes have been written, smoothing kernel writeback instead of letting
	// dirty pages flush in large bursts. 0 disables periodic syncing.
	//
	// syncFile is the file the accumulated [syncMinOff,syncMaxOff) range
	// belongs to; when the write target changes, the pending range for the
	// PREVIOUS file is flushed first, or SyncFileRange would be handed an
	// offset range spanning two unrelated files.
	syncEvery  int64
	syncFile   *os.File
	syncMinOff int64
	syncMaxOff int64
	sinceSync  int64

	wg sync.WaitGroup
}

// NewOrderedWriter starts the single writer goroutine. file is the target
// for the legacy single-file API (Submit/SubmitAndWait); directory mode
// instead passes an explicit *os.File per call via SubmitAndWaitFile, so
// file may be nil there. window controls how many out-of-order blocks may
// be buffered before the smallest is flushed; sized from the worker count
// so late-arriving blocks can still be reordered.
func NewOrderedWriter(file *os.File, workers int) *OrderedWriter {
	window := workers * 2
	if window < 4 {
		window = 4
	}
	ow := &OrderedWriter{
		file:   file,
		ch:     make(chan writeItem, window),
		window: window,
		// Flush dirty pages roughly every 64MB to keep writeback steady on HDD.
		syncEvery:  64 * 1024 * 1024,
		syncMinOff: -1,
	}
	ow.wg.Add(1)
	go ow.run()
	return ow
}

// EnablePerItemErrors switches the writer from the legacy sticky-error
// contract to per-item isolation (see the perItemErr field comment). Call
// immediately after NewOrderedWriter, before any Submit/SubmitAndWaitFile —
// it is not safe to flip concurrently with writeOne reading it.
func (ow *OrderedWriter) EnablePerItemErrors() {
	ow.perItemErr = true
}

func (ow *OrderedWriter) run() {
	defer ow.wg.Done()
	h := &writeHeap{}
	heap.Init(h)

	handle := func(it writeItem) {
		if it.drain {
			// File-scoped drain: write out only the drained file's buffered
			// items (in ascending key order), keep everything else exactly
			// as buffered. A nil drain file falls back to the historical
			// full barrier.
			if it.file == nil {
				for h.Len() > 0 {
					ow.writeOne(heap.Pop(h).(writeItem))
				}
				ow.flushSync()
				close(it.drainDone)
				return
			}
			var mine []writeItem
			rest := (*h)[:0]
			for _, b := range *h {
				if b.file == it.file {
					mine = append(mine, b)
				} else {
					rest = append(rest, b)
				}
			}
			if len(mine) > 0 {
				sort.Slice(mine, func(i, j int) bool { return mine[i].key < mine[j].key })
				for _, b := range mine {
					ow.writeOne(b)
				}
				*h = rest
				heap.Init(h)
			}
			// The accumulated sync range must be flushed and cleared IF it
			// belongs to the file the caller is about to close (or
			// truncate/chmod/chtimes) once drainDone fires — leaving a
			// stale ow.syncFile pointing at an fd finalize() then closes is
			// a use-after-close the next unrelated write would trip over.
			// A range accumulated for a DIFFERENT, still-live file is left
			// to keep growing toward its normal syncEvery flush.
			if ow.syncFile == it.file {
				ow.flushSync()
			}
			close(it.drainDone)
			return
		}
		heap.Push(h, it)
	}

	for {
		// With nothing buffered we must block for the next item (or exit).
		if h.Len() == 0 {
			it, ok := <-ow.ch
			if !ok {
				ow.flushSync()
				return
			}
			handle(it)
			continue
		}

		select {
		case it, ok := <-ow.ch:
			if !ok {
				// Channel closed: drain remaining blocks in ascending order.
				for h.Len() > 0 {
					ow.writeOne(heap.Pop(h).(writeItem))
				}
				ow.flushSync()
				return
			}
			handle(it)
			// While input keeps flowing, buffer up to the reorder window so the
			// flushed stream stays near-sequential; flush the smallest once full.
			for h.Len() > ow.window {
				ow.writeOne(heap.Pop(h).(writeItem))
			}
		default:
			// No input ready. Senders that use SubmitAndWait(File) are
			// blocked until their block is written, so we must make
			// progress instead of waiting for the window to fill (which
			// would deadlock). Flush the lowest-key buffered block.
			ow.writeOne(heap.Pop(h).(writeItem))
		}
	}
}

func (ow *OrderedWriter) writeOne(it writeItem) {
	// perItemErr mode never lets a prior file's failure skip this write —
	// each item's outcome is independent (F5). The legacy mode keeps the
	// original sticky short-circuit: once ow.err is set, every subsequent
	// write is skipped and reported with that same error.
	var err error
	if !ow.perItemErr {
		err = ow.Err()
	}
	if err == nil {
		if _, werr := it.file.WriteAt(it.data, it.offset); werr != nil {
			ow.errMu.Lock()
			if ow.err == nil {
				ow.err = werr
			}
			ow.errMu.Unlock()
			err = werr
		} else {
			ow.maybeSync(it.file, it.offset, int64(len(it.data)))
		}
	}
	if it.done != nil {
		it.done <- err
	}
}

// maybeSync issues a best-effort asynchronous writeback once enough bytes have
// accumulated, covering the offset range written since the last sync.
func (ow *OrderedWriter) maybeSync(f *os.File, offset, n int64) {
	if ow.syncEvery <= 0 {
		return
	}
	if ow.syncFile != nil && ow.syncFile != f && ow.sinceSync > 0 {
		// Target changed with a pending accumulated range: flush it against
		// the file it actually belongs to before starting a new one.
		ow.flushSync()
	}
	ow.syncFile = f
	if ow.syncMinOff < 0 || offset < ow.syncMinOff {
		ow.syncMinOff = offset
	}
	if end := offset + n; end > ow.syncMaxOff {
		ow.syncMaxOff = end
	}
	ow.sinceSync += n
	if ow.sinceSync >= ow.syncEvery {
		ow.flushSync()
	}
}

// flushSync issues a SyncFileRange for whatever range is currently
// accumulated (a no-op if nothing is pending) and clears the accumulator,
// including ow.syncFile itself. Called from maybeSync's periodic flush and,
// critically, from every drain/close path in run(): a stale ow.syncFile left
// pointing at a file the caller closes right after draining would make the
// next unrelated write's SyncFileRange call operate on a closed fd.
func (ow *OrderedWriter) flushSync() {
	if ow.syncFile == nil || ow.sinceSync == 0 {
		ow.syncFile = nil
		ow.sinceSync = 0
		ow.syncMinOff = -1
		ow.syncMaxOff = 0
		return
	}
	SyncFileRange(ow.syncFile, ow.syncMinOff, ow.syncMaxOff-ow.syncMinOff, SyncFileRangeWrite)
	ow.syncFile = nil
	ow.sinceSync = 0
	ow.syncMinOff = -1
	ow.syncMaxOff = 0
}

// Submit queues a block for writing to NewOrderedWriter's file, keyed on
// blockIdx alone (fileID 0). data must be owned by the writer after this
// call (callers pass a fresh/immutable slice). offset is the byte offset in
// the destination file.
func (ow *OrderedWriter) Submit(blockIdx uint32, offset int64, data []byte) {
	ow.ch <- writeItem{key: writeKey(0, blockIdx), file: ow.file, offset: offset, data: data}
}

// SubmitAndWait queues a block for NewOrderedWriter's file and blocks until
// it has been durably written (or a prior write error is observed),
// returning the write result. This lets concurrent callers preserve a
// per-block acknowledgement contract while still having their scattered
// writes serialized into a near-sequential stream by the single writer
// goroutine. data is only read before this call returns, so the caller may
// safely reuse the buffer afterwards.
func (ow *OrderedWriter) SubmitAndWait(blockIdx uint32, offset int64, data []byte) error {
	return ow.SubmitAndWaitFile(ow.file, writeKey(0, blockIdx), offset, data)
}

// SubmitAndWaitFile is SubmitAndWait's directory-mode counterpart: the
// caller supplies both the target file and the full ordering key (typically
// writeKey(fileID, blockIdx)), so one OrderedWriter can serialize writes
// across every file in a tree instead of just one.
func (ow *OrderedWriter) SubmitAndWaitFile(f *os.File, key uint64, offset int64, data []byte) error {
	done := make(chan error, 1)
	ow.ch <- writeItem{key: key, file: f, offset: offset, data: data, done: done}
	return <-done
}

// DrainFile blocks until every write submitted so far FOR f has been
// applied — file-scoped (see writeItem's drain field); other files' buffered
// items stay put. Used before finalize() truncates, closes, chmods, and
// chtimes a file, as an explicit safety net beyond the fact that every
// individual SubmitAndWaitFile call already blocks until its own block is
// written. Passing a nil f requests the historical full barrier.
//
// Deliberately does NOT return ow.Err(): that is the STICKY, GLOBAL first
// error across every file this writer has ever touched, not anything
// specific to f. By the time finalizeIO calls this, every one of f's own
// blocks was already individually confirmed via its own SubmitAndWaitFile
// return (which delivers success/failure synchronously to its own caller,
// before that call even returns) — DrainFile's only real job is ensuring
// nothing still buffered in the reorder heap is left unflushed before f is
// truncated/closed, not reporting some OTHER file's historical write
// failure. Returning ow.Err() here used to mean one file's ENOSPC/EIO
// silently made every file finalized afterward skip its own
// truncate/chmod/chtimes (wrong size/mode/mtime, breaking the
// idempotent-second-run property) and fail the WHOLE tree via
// FirstFinalizeErr() — even though each of those later files' own blocks
// had written perfectly.
func (ow *OrderedWriter) DrainFile(f *os.File) error {
	done := make(chan struct{})
	ow.ch <- writeItem{drain: true, drainDone: done, file: f}
	<-done
	return nil
}

// Err returns the first write error encountered, if any.
func (ow *OrderedWriter) Err() error {
	ow.errMu.Lock()
	defer ow.errMu.Unlock()
	return ow.err
}

// Close flushes all buffered blocks and waits for the writer goroutine to
// finish. Returns the first write error, if any. Safe to call once.
func (ow *OrderedWriter) Close() error {
	close(ow.ch)
	ow.wg.Wait()
	return ow.Err()
}
