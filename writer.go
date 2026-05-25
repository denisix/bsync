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
	data     []byte // bytes to write at offset (already decompressed/decrypted)
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
	}
	ow.wg.Add(1)
	go ow.run()
	return ow
}

func (ow *OrderedWriter) run() {
	defer ow.wg.Done()
	h := &writeHeap{}
	heap.Init(h)

	for it := range ow.ch {
		heap.Push(h, it)
		// Keep the buffer bounded: once the reorder window is full, flush the
		// lowest-offset block so writes stay near-sequential and memory stays
		// bounded.
		for h.Len() > ow.window {
			ow.writeOne(heap.Pop(h).(writeItem))
		}
	}

	// Channel closed: drain remaining blocks in ascending order.
	for h.Len() > 0 {
		ow.writeOne(heap.Pop(h).(writeItem))
	}
}

func (ow *OrderedWriter) writeOne(it writeItem) {
	if ow.Err() != nil {
		return
	}
	if _, err := ow.file.WriteAt(it.data, it.offset); err != nil {
		ow.errMu.Lock()
		if ow.err == nil {
			ow.err = err
		}
		ow.errMu.Unlock()
	}
}

// Submit queues a block for writing. data must be owned by the writer after
// this call (callers pass a fresh/immutable slice). offset is the byte offset
// in the destination file.
func (ow *OrderedWriter) Submit(blockIdx uint32, offset int64, data []byte) {
	ow.ch <- writeItem{blockIdx: blockIdx, offset: offset, data: data}
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
