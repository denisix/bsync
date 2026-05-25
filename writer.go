package main

import (
	"container/heap"
	"os"
	"sync"
)

// OrderedWriter serializes all writes to the destination file through a single
// goroutine and emits them in (near) ascending block-offset order. On a
// spinning disk this turns the scattered WriteAt calls of N parallel
// connection handlers into one mostly-sequential stream, which is what lets an
// HDD run at full write speed.
//
// Only changed blocks are written (in-sync blocks never reach here), so the
// indices are sparse and non-contiguous. We therefore cannot wait for an exact
// "next" index. Instead we keep a bounded reorder window: blocks accumulate in
// a min-heap keyed by block index, and once the window is full we pop the
// smallest and write it. Because the senders push blocks roughly in order
// (bounded by the worker count), this yields a monotonically increasing write
// offset in practice while guaranteeing progress and bounded memory.
type writeItem struct {
	blockIdx uint32
	offset   int64
	data     []byte     // bytes to write at offset (already decompressed/decrypted)
	done     chan error // non-nil for SubmitAndWait: receives the write result
}

type writeHeap []writeItem

func (h writeHeap) Len() int            { return len(h) }
func (h writeHeap) Less(i, j int) bool  { return h[i].blockIdx < h[j].blockIdx }
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
	file   *os.File
	ch     chan writeItem
	window int

	errMu sync.Mutex
	err   error

	// syncEvery triggers an asynchronous SyncFileRange writeback once this many
	// bytes have been written, smoothing kernel writeback instead of letting
	// dirty pages flush in large bursts. 0 disables periodic syncing.
	syncEvery  int64
	syncMinOff int64
	syncMaxOff int64
	sinceSync  int64

	wg sync.WaitGroup
}

// NewOrderedWriter starts the single writer goroutine. window controls how many
// out-of-order blocks may be buffered before the smallest is flushed; sized
// from the worker count so late-arriving blocks can still be reordered.
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

func (ow *OrderedWriter) run() {
	defer ow.wg.Done()
	h := &writeHeap{}
	heap.Init(h)

	for {
		// With nothing buffered we must block for the next block (or exit).
		if h.Len() == 0 {
			it, ok := <-ow.ch
			if !ok {
				return
			}
			heap.Push(h, it)
			continue
		}

		select {
		case it, ok := <-ow.ch:
			if !ok {
				// Channel closed: drain remaining blocks in ascending order.
				for h.Len() > 0 {
					ow.writeOne(heap.Pop(h).(writeItem))
				}
				return
			}
			heap.Push(h, it)
			// While input keeps flowing, buffer up to the reorder window so the
			// flushed stream stays near-sequential; flush the smallest once full.
			for h.Len() > ow.window {
				ow.writeOne(heap.Pop(h).(writeItem))
			}
		default:
			// No input ready. Senders that use SubmitAndWait are blocked until
			// their block is written, so we must make progress instead of
			// waiting for the window to fill (which would deadlock). Flush the
			// lowest-offset buffered block.
			ow.writeOne(heap.Pop(h).(writeItem))
		}
	}
}

func (ow *OrderedWriter) writeOne(it writeItem) {
	err := ow.Err()
	if err == nil {
		if _, werr := ow.file.WriteAt(it.data, it.offset); werr != nil {
			ow.errMu.Lock()
			if ow.err == nil {
				ow.err = werr
			}
			err = ow.err
			ow.errMu.Unlock()
		} else {
			ow.maybeSync(it.offset, int64(len(it.data)))
		}
	}
	if it.done != nil {
		it.done <- err
	}
}

// maybeSync issues a best-effort asynchronous writeback once enough bytes have
// accumulated, covering the offset range written since the last sync.
func (ow *OrderedWriter) maybeSync(offset, n int64) {
	if ow.syncEvery <= 0 {
		return
	}
	if ow.syncMinOff < 0 || offset < ow.syncMinOff {
		ow.syncMinOff = offset
	}
	if end := offset + n; end > ow.syncMaxOff {
		ow.syncMaxOff = end
	}
	ow.sinceSync += n
	if ow.sinceSync >= ow.syncEvery {
		SyncFileRange(ow.file, ow.syncMinOff, ow.syncMaxOff-ow.syncMinOff, SyncFileRangeWrite)
		ow.sinceSync = 0
		ow.syncMinOff = -1
		ow.syncMaxOff = 0
	}
}

// Submit queues a block for writing. data must be owned by the writer after
// this call (callers pass a fresh/immutable slice). offset is the byte offset
// in the destination file.
func (ow *OrderedWriter) Submit(blockIdx uint32, offset int64, data []byte) {
	ow.ch <- writeItem{blockIdx: blockIdx, offset: offset, data: data}
}

// SubmitAndWait queues a block and blocks until it has been durably written
// (or a prior write error is observed), returning the write result. This lets
// concurrent callers preserve a per-block acknowledgement contract while still
// having their scattered writes serialized into a near-sequential stream by the
// single writer goroutine. data is only read before this call returns, so the
// caller may safely reuse the buffer afterwards.
func (ow *OrderedWriter) SubmitAndWait(blockIdx uint32, offset int64, data []byte) error {
	done := make(chan error, 1)
	ow.ch <- writeItem{blockIdx: blockIdx, offset: offset, data: data, done: done}
	return <-done
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
