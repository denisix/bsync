package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- readFileBlocks ---

func TestReadFileBlocksTagsFileID(t *testing.T) {
	const bs = 1024
	data := randomBytes(bs*3+7, 21)
	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := make(chan FileBlock, 8)
	errCh := make(chan error, 1)
	emittedCh := make(chan uint32, 1)
	go func() {
		emitted, err := readFileBlocks(f, 42, bs, uint64(len(data)), 0, out, nil, nil)
		close(out) // readFileBlocks never closes out itself; the caller owns that
		emittedCh <- emitted
		errCh <- err
	}()

	var blocks []FileBlock
	for fb := range out {
		blocks = append(blocks, fb)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("readFileBlocks: %v", err)
	}
	if emitted := <-emittedCh; emitted != 4 {
		t.Errorf("reported emitted = %d, want 4", emitted)
	}
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks, want 4", len(blocks))
	}
	for i, fb := range blocks {
		if fb.FileID != 42 {
			t.Errorf("block %d FileID = %d, want 42", i, fb.FileID)
		}
		if fb.BlockIdx != uint32(i) {
			t.Errorf("block %d BlockIdx = %d, want %d", i, fb.BlockIdx, i)
		}
		wantLast := i == 3
		if fb.Last != wantLast {
			t.Errorf("block %d Last = %v, want %v", i, fb.Last, wantLast)
		}
	}
	if len(blocks[3].Data) != 7 {
		t.Errorf("last block len = %d, want 7", len(blocks[3].Data))
	}
	// Content round-trips correctly.
	var got []byte
	for _, fb := range blocks {
		got = append(got, fb.Data...)
	}
	if !bytes.Equal(got, data) {
		t.Error("reassembled content mismatch")
	}
}

func TestReadFileBlocksEmptyFileNoOp(t *testing.T) {
	path := writeTempFile(t, nil)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := make(chan FileBlock, 1)
	emitted, err := readFileBlocks(f, 0, 4096, 0, 0, out, nil, nil)
	close(out)
	if err != nil {
		t.Fatalf("readFileBlocks on empty file: %v", err)
	}
	if emitted != 0 {
		t.Errorf("emitted = %d, want 0 for an empty file", emitted)
	}
	if _, ok := <-out; ok {
		t.Error("expected no blocks for an empty file")
	}
}

func TestReadFileBlocksCustomAcquireUsed(t *testing.T) {
	const bs = 512
	data := randomBytes(bs*2, 22)
	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var acquireCalls, releaseCalls int
	var mu sync.Mutex
	acquire := func(size uint32) ([]byte, func()) {
		mu.Lock()
		acquireCalls++
		mu.Unlock()
		buf := make([]byte, size)
		return buf, func() {
			mu.Lock()
			releaseCalls++
			mu.Unlock()
		}
	}

	out := make(chan FileBlock, 8)
	go func() {
		_, _ = readFileBlocks(f, 0, bs, uint64(len(data)), 0, out, nil, acquire)
		close(out)
	}()
	var n int
	for fb := range out {
		n++
		fb.release()
	}
	if n != 2 {
		t.Fatalf("got %d blocks, want 2", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if acquireCalls != 2 {
		t.Errorf("acquire called %d times, want 2", acquireCalls)
	}
	if releaseCalls != 2 {
		t.Errorf("release called %d times, want 2", releaseCalls)
	}
}

func TestReadFileBlocksDoneCancelsEarly(t *testing.T) {
	const bs = 64
	data := randomBytes(bs*100, 23) // plenty of blocks
	path := writeTempFile(t, data)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := make(chan FileBlock) // unbuffered: readFileBlocks blocks on the first send
	done := make(chan struct{})
	close(done) // already cancelled

	emitted, err := readFileBlocks(f, 0, bs, uint64(len(data)), 0, out, done, nil)
	if err != nil {
		t.Fatalf("cancellation should return nil, got %v", err)
	}
	if emitted != 0 {
		t.Errorf("emitted = %d, want 0 -- cancelled before the first send", emitted)
	}
}

// --- byteBudget ---

func TestByteBudgetBasicAcquireRelease(t *testing.T) {
	b := newByteBudget(100)
	b.acquire(60)
	b.acquire(40) // exactly fills the budget

	acquired := make(chan struct{})
	go func() {
		b.acquire(10) // must wait until something is released
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("acquire proceeded while the budget was full")
	case <-time.After(50 * time.Millisecond):
	}

	b.release(40)
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire never unblocked after release")
	}
}

func TestByteBudgetOversizedRequestNotDeadlocked(t *testing.T) {
	b := newByteBudget(10)
	done := make(chan struct{})
	go func() {
		b.acquire(1000) // far bigger than the whole budget
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an oversized request on an empty budget deadlocked")
	}
}

// --- TreeReader ---

// buildTreeFixture writes n files of the given size under dir and returns
// FileEntry values for them (Kind, Size, BlockSize, Blocks set; RelPath is
// just the base name, so a TreeReader rooted at dir opens the real file),
// plus the raw content of each for comparison.
func buildTreeFixture(t *testing.T, dir string, sizes []int, seed int64) ([]*FileEntry, [][]byte) {
	t.Helper()
	entries := make([]*FileEntry, len(sizes))
	contents := make([][]byte, len(sizes))
	for i, sz := range sizes {
		data := randomBytes(sz, seed+int64(i))
		name := fmt.Sprintf("f%02d.bin", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		bs := treeBlockSize(uint64(sz), 4096)
		var blocks uint32
		if sz > 0 {
			blocks = uint32((uint64(sz) + uint64(bs) - 1) / uint64(bs))
		}
		entries[i] = &FileEntry{
			ID:        uint32(i),
			RelPath:   name,
			Size:      uint64(sz),
			Kind:      KindFile,
			BlockSize: bs,
			Blocks:    blocks,
		}
		contents[i] = data
	}
	return entries, contents
}

func feedEntries(entries []*FileEntry) <-chan *FileEntry {
	ch := make(chan *FileEntry, len(entries))
	for _, fe := range entries {
		ch <- fe
	}
	close(ch)
	return ch
}

func TestTreeReaderReadsAllFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	entries, contents := buildTreeFixture(t, dir, []int{300, 4096, 4097, 1}, 30)
	sess := NewSyncSession()
	for _, fe := range entries {
		sess.AddFile(fe)
	}

	tr := NewTreeReader(dir, feedEntries(entries), sess, 8192)
	tr.Start()

	got := make(map[uint32][]byte)
	lastSeenIdx := make(map[uint32]int64) // -1 means none seen
	for _, fe := range entries {
		lastSeenIdx[fe.ID] = -1
	}
	var lastFileID = -1
	for fb := range tr.Blocks() {
		// Blocks for a given file must be sequential and monotonic.
		want := lastSeenIdx[fb.FileID] + 1
		if int64(fb.BlockIdx) != want {
			t.Errorf("file %d: block idx %d out of order (want %d)", fb.FileID, fb.BlockIdx, want)
		}
		lastSeenIdx[fb.FileID] = int64(fb.BlockIdx)
		got[fb.FileID] = append(got[fb.FileID], fb.Data...)
		if fb.release != nil {
			fb.release()
		}
		lastFileID = int(fb.FileID)
	}
	_ = lastFileID
	tr.Wait()

	if sess.FailedCount() != 0 {
		t.Errorf("unexpected failures: %d", sess.FailedCount())
	}
	for _, fe := range entries {
		if fe.Size == 0 {
			continue
		}
		if !bytes.Equal(got[fe.ID], contents[fe.ID]) {
			t.Errorf("file %d (%s): reassembled content mismatch", fe.ID, fe.RelPath)
		}
	}
}

func TestTreeReaderSkipsDirsSkippedAndEmptyEntries(t *testing.T) {
	dir := t.TempDir()
	fileEntries, _ := buildTreeFixture(t, dir, []int{10}, 31)
	entries := []*FileEntry{
		{ID: 0, RelPath: "adir", Kind: KindDir},
		{ID: 1, RelPath: "skipped", Kind: KindSkipped},
		{ID: 2, RelPath: "empty.txt", Kind: KindFile, Size: 0},
		{ID: 3, RelPath: fileEntries[0].RelPath, Kind: KindFile, Size: fileEntries[0].Size, BlockSize: fileEntries[0].BlockSize},
	}
	tr := NewTreeReader(dir, feedEntries(entries), nil, 4096)
	tr.Start()
	var n int
	var sawFileID uint32 = 9999
	for fb := range tr.Blocks() {
		n++
		sawFileID = fb.FileID
		if fb.release != nil {
			fb.release()
		}
	}
	tr.Wait()
	if n != 1 {
		t.Fatalf("got %d blocks, want 1 (only the real non-empty file)", n)
	}
	if sawFileID != 3 {
		t.Errorf("block came from FileID %d, want 3", sawFileID)
	}
}

func TestTreeReaderOpenFailureRecordsSessionFailureAndContinues(t *testing.T) {
	dir := t.TempDir()
	entries, contents := buildTreeFixture(t, dir, []int{50, 50}, 32)
	// Point the first entry at a nonexistent path.
	entries[0].RelPath = "does-not-exist.bin"

	sess := NewSyncSession()
	for _, fe := range entries {
		sess.AddFile(fe)
	}
	tr := NewTreeReader(dir, feedEntries(entries), sess, 4096)
	tr.Start()

	var got []byte
	var sawFileID uint32 = 9999
	for fb := range tr.Blocks() {
		got = append(got, fb.Data...)
		sawFileID = fb.FileID
		if fb.release != nil {
			fb.release()
		}
	}
	tr.Wait()

	if !sess.isFailed(0) {
		t.Error("expected FileID 0 to be marked failed")
	}
	if sess.isFailed(1) {
		t.Error("FileID 1 should not be affected by FileID 0's failure")
	}
	if sawFileID != 1 {
		t.Errorf("blocks came from FileID %d, want 1 (the surviving file)", sawFileID)
	}
	if !bytes.Equal(got, contents[1]) {
		t.Error("surviving file's content mismatch")
	}
}

func TestTreeReaderSkipsAlreadyFailedFile(t *testing.T) {
	dir := t.TempDir()
	entries, _ := buildTreeFixture(t, dir, []int{50, 50}, 33)
	sess := NewSyncSession()
	for _, fe := range entries {
		sess.AddFile(fe)
	}
	sess.failFile(0, errors.New("pre-marked failed"))

	tr := NewTreeReader(dir, feedEntries(entries), sess, 4096)
	tr.Start()
	var sawFileID uint32 = 9999
	var count int
	for fb := range tr.Blocks() {
		count++
		sawFileID = fb.FileID
		if fb.release != nil {
			fb.release()
		}
	}
	tr.Wait()
	if sawFileID != 1 || count == 0 {
		t.Errorf("expected blocks only from FileID 1, got FileID %d, count %d", sawFileID, count)
	}
}

// --- precomputeTreeBlocks ---

func TestPrecomputeTreeBlocksPopulatesFields(t *testing.T) {
	dir := t.TempDir()
	entries, contents := buildTreeFixture(t, dir, []int{4096 * 2, 10}, 40)
	sess := NewSyncSession()
	for _, fe := range entries {
		sess.AddFile(fe)
	}

	tr := NewTreeReader(dir, feedEntries(entries), sess, 1<<20)
	tr.Start()

	out := make(chan PrecomputedBlock, 16)
	precomputeTreeBlocks(tr, sess, out, 3, true) // noCompress=true keeps Data set for direct comparison

	// Keyed by BlockIdx, not appended in receive order: with workers > 1,
	// multiple goroutines pull blocks from tr.Blocks() concurrently and race
	// to push onto out, so two blocks of the SAME file can legitimately
	// arrive out of order. Reassembly must sort by index, not by arrival.
	seen := map[uint32]map[uint32][]byte{}
	for pb := range out {
		fe := sess.FileByID(pb.FileID)
		if fe == nil {
			t.Fatalf("PrecomputedBlock references unknown FileID %d", pb.FileID)
		}
		if pb.FileSize != fe.Size {
			t.Errorf("FileID %d: FileSize = %d, want %d", pb.FileID, pb.FileSize, fe.Size)
		}
		if pb.BlockSize != fe.BlockSize {
			t.Errorf("FileID %d: BlockSize = %d, want %d", pb.FileID, pb.BlockSize, fe.BlockSize)
		}
		wantLast := pb.BlockIdx == fe.Blocks-1
		if pb.Last != wantLast {
			t.Errorf("FileID %d block %d: Last = %v, want %v", pb.FileID, pb.BlockIdx, pb.Last, wantLast)
		}
		if len(pb.Hash) != 16 {
			t.Errorf("FileID %d block %d: hash length = %d, want 16", pb.FileID, pb.BlockIdx, len(pb.Hash))
		}
		if pb.Data == nil {
			t.Errorf("FileID %d block %d: Data is nil with noCompress=true", pb.FileID, pb.BlockIdx)
		}
		if seen[pb.FileID] == nil {
			seen[pb.FileID] = map[uint32][]byte{}
		}
		seen[pb.FileID][pb.BlockIdx] = append([]byte(nil), pb.Data...)
		pb.Release() // must not panic even though release may be nil in some paths
	}

	for _, fe := range entries {
		var reassembled []byte
		for idx := uint32(0); idx < fe.Blocks; idx++ {
			chunk, ok := seen[fe.ID][idx]
			if !ok {
				t.Errorf("FileID %d: block %d never arrived on out", fe.ID, idx)
				continue
			}
			reassembled = append(reassembled, chunk...)
		}
		if !bytes.Equal(reassembled, contents[fe.ID]) {
			t.Errorf("FileID %d: reassembled content mismatch", fe.ID)
		}
	}
}

// TestPrecomputeTreeBlocksRoutesSmallFilesToOneWorker proves the routing
// contract precomputeTreeBlocksRouted adds on top of precomputeTreeBlocks:
// a file with Blocks <= workers has every one of its blocks delivered on
// exactly one dedicated channel (never on shared), while a file with more
// blocks than workers is delivered entirely on shared (never on any
// dedicated channel) -- exactly affinityWorker's contract, exercised
// end-to-end through the real hashing goroutines rather than called
// directly.
func TestPrecomputeTreeBlocksRoutesSmallFilesToOneWorker(t *testing.T) {
	const workers = 4
	dir := t.TempDir()
	// f00: 8202 bytes -> 3 blocks (<=workers, routed). f01: 4097 bytes ->
	// 2 blocks (<=workers, routed). f02: 40960 bytes -> 10 blocks
	// (>workers, must stay on shared).
	entries, contents := buildTreeFixture(t, dir, []int{4096*2 + 10, 4096 + 1, 4096 * 10}, 50)
	sess := NewSyncSession()
	for _, fe := range entries {
		sess.AddFile(fe)
	}

	tr := NewTreeReader(dir, feedEntries(entries), sess, 1<<20)
	tr.Start()

	shared := make(chan PrecomputedBlock, 16)
	dedChans := make([]chan PrecomputedBlock, workers)
	sendDed := make([]chan<- PrecomputedBlock, workers)
	for i := range dedChans {
		dedChans[i] = make(chan PrecomputedBlock, 16)
		sendDed[i] = dedChans[i]
	}
	precomputeTreeBlocksRouted(tr, sess, shared, sendDed, workers, true)

	// Drain all workers+1 channels concurrently, tagging each block with
	// which channel it arrived on ("shared" or a dedicated index).
	type tagged struct {
		pb  PrecomputedBlock
		src string
	}
	results := make(chan tagged, 256)
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		for pb := range shared {
			results <- tagged{pb, "shared"}
		}
	}()
	for i, ch := range dedChans {
		drainWG.Add(1)
		go func(i int, ch <-chan PrecomputedBlock) {
			defer drainWG.Done()
			for pb := range ch {
				results <- tagged{pb, fmt.Sprintf("dedicated[%d]", i)}
			}
		}(i, ch)
	}
	go func() {
		drainWG.Wait()
		close(results)
	}()

	seenSrc := map[uint32]map[string]bool{}      // FileID -> set of sources its blocks arrived on
	seenBlocks := map[uint32]map[uint32][]byte{} // FileID -> BlockIdx -> data
	for r := range results {
		fe := sess.FileByID(r.pb.FileID)
		if fe == nil {
			t.Fatalf("PrecomputedBlock references unknown FileID %d", r.pb.FileID)
		}
		if seenSrc[r.pb.FileID] == nil {
			seenSrc[r.pb.FileID] = map[string]bool{}
			seenBlocks[r.pb.FileID] = map[uint32][]byte{}
		}
		seenSrc[r.pb.FileID][r.src] = true
		seenBlocks[r.pb.FileID][r.pb.BlockIdx] = append([]byte(nil), r.pb.Data...)
		r.pb.Release()
	}

	for _, fe := range entries {
		wantWorker := affinityWorker(fe.ID, fe.Blocks, workers)
		srcs := seenSrc[fe.ID]
		if len(srcs) != 1 {
			t.Errorf("FileID %d (blocks=%d): blocks arrived on %d distinct sources %v, want exactly 1", fe.ID, fe.Blocks, len(srcs), srcs)
			continue
		}
		var gotSrc string
		for s := range srcs {
			gotSrc = s
		}
		wantSrc := "shared"
		if wantWorker >= 0 {
			wantSrc = fmt.Sprintf("dedicated[%d]", wantWorker)
		}
		if gotSrc != wantSrc {
			t.Errorf("FileID %d (blocks=%d): arrived on %s, want %s", fe.ID, fe.Blocks, gotSrc, wantSrc)
		}

		var reassembled []byte
		for idx := uint32(0); idx < fe.Blocks; idx++ {
			chunk, ok := seenBlocks[fe.ID][idx]
			if !ok {
				t.Errorf("FileID %d: block %d never arrived", fe.ID, idx)
				continue
			}
			reassembled = append(reassembled, chunk...)
		}
		if !bytes.Equal(reassembled, contents[fe.ID]) {
			t.Errorf("FileID %d: reassembled content mismatch", fe.ID)
		}
	}
}

func TestPrecomputeTreeBlocksHashMatchesDirectComputation(t *testing.T) {
	dir := t.TempDir()
	entries, contents := buildTreeFixture(t, dir, []int{4096}, 41)
	sess := NewSyncSession()
	sess.AddFile(entries[0])

	tr := NewTreeReader(dir, feedEntries(entries), sess, 1<<20)
	tr.Start()
	out := make(chan PrecomputedBlock, 4)
	precomputeTreeBlocks(tr, sess, out, 1, true)

	var pb PrecomputedBlock
	for b := range out {
		pb = b
	}
	want := checksum(contents[0])
	if !bytes.Equal(pb.Hash, want) {
		t.Error("tree-mode hash does not match a direct checksum() call on the same bytes")
	}
}

func TestPrecomputeTreeBlocksReleaseNeverPanicsOnCompressedOrZero(t *testing.T) {
	dir := t.TempDir()
	// A highly compressible (all-zero, i.e. also IsZero) block and a
	// non-zero one, both under noCompress=false so compression is tried.
	zeroPath := filepath.Join(dir, "zero.bin")
	if err := os.WriteFile(zeroPath, make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	entries := []*FileEntry{
		{ID: 0, RelPath: "zero.bin", Kind: KindFile, Size: 4096, BlockSize: 4096},
	}
	sess := NewSyncSession()
	sess.AddFile(entries[0])
	tr := NewTreeReader(dir, feedEntries(entries), sess, 1<<20)
	tr.Start()
	out := make(chan PrecomputedBlock, 4)
	precomputeTreeBlocks(tr, sess, out, 1, false)
	for pb := range out {
		if !pb.IsZero {
			t.Error("an all-zero block was not detected as IsZero")
		}
		pb.Release() // release is nil for a zero block; must be a safe no-op
	}
}
