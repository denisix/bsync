package main

import (
	"bytes"
	"math/rand"
	"os"
	"sync"
	"testing"
)

// buildExpected splits data into blocks and returns the per-block byte slices.
func splitBlocks(data []byte, block int) [][]byte {
	var out [][]byte
	for off := 0; off < len(data); off += block {
		end := off + block
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[off:end])
	}
	return out
}

func TestOrderedWriterOutOfOrder(t *testing.T) {
	const block = 1024
	data := randomBytes(block*8, 21)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 4)

	// Submit blocks in a shuffled order.
	order := rand.New(rand.NewSource(99)).Perm(len(blocks))
	for _, idx := range order {
		ow.Submit(uint32(idx), int64(idx)*block, blocks[idx])
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("file content mismatch after out-of-order writes")
	}
}

func TestOrderedWriterWindowOverflow(t *testing.T) {
	// Many more blocks than the reorder window forces mid-stream flushing.
	const block = 256
	const n = 200
	data := randomBytes(block*n, 22)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 2) // small window
	for i, b := range blocks {
		ow.Submit(uint32(i), int64(i)*block, b)
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("file content mismatch after window overflow")
	}
}

func TestOrderedWriterConcurrentStress(t *testing.T) {
	const block = 256
	const n = 500
	data := randomBytes(block*n, 23)
	blocks := splitBlocks(data, block)

	path := writeTempFile(t, zeroBytes(len(data)))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 1) // tiny window -> heavy flushing

	// Submit from several goroutines concurrently (exercise the lock-free
	// channel path under -race).
	const submitters = 8
	var wg sync.WaitGroup
	for s := 0; s < submitters; s++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := start; i < n; i += submitters {
				ow.Submit(uint32(i), int64(i)*block, blocks[i])
			}
		}(s)
	}
	wg.Wait()
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch after concurrent stress")
	}
}

func TestOrderedWriterErrorOnClosedFile(t *testing.T) {
	path := writeTempFile(t, zeroBytes(4096))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ow := NewOrderedWriter(f, 2)
	f.Close() // close underlying file so WriteAt fails

	for i := 0; i < 10; i++ {
		ow.Submit(uint32(i), int64(i)*256, randomBytes(256, int64(i)))
	}
	if err := ow.Close(); err == nil {
		t.Error("expected a write error after closing underlying file")
	}
	if ow.Err() == nil {
		t.Error("Err() should report the write failure")
	}
}

func TestWriteKeyMatchesLegacyBlockIdxOrdering(t *testing.T) {
	// writeKey(0, blockIdx) must equal blockIdx itself so the single-file
	// API's ordering is bit-for-bit unchanged by the keyed rework.
	for _, idx := range []uint32{0, 1, 42, ^uint32(0)} {
		if got := writeKey(0, idx); got != uint64(idx) {
			t.Errorf("writeKey(0, %d) = %d, want %d", idx, got, idx)
		}
	}
	// Different files never interleave in key order: every key for file 0
	// (any blockIdx) is less than every key for file 1.
	if writeKey(0, ^uint32(0)) >= writeKey(1, 0) {
		t.Error("file 0's highest key is not less than file 1's lowest key")
	}
}

// TestOrderedWriterMultiFileOrdering exercises SubmitAndWaitFile across two
// separate destination files sharing one OrderedWriter, interleaving
// submissions from both files and asserting each file's own content ends up
// correct regardless of interleaving.
func TestOrderedWriterMultiFileOrdering(t *testing.T) {
	pathA := writeTempFile(t, zeroBytes(4096))
	pathB := writeTempFile(t, zeroBytes(4096))
	fA, err := os.OpenFile(pathA, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer fA.Close()
	fB, err := os.OpenFile(pathB, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer fB.Close()

	ow := NewOrderedWriter(nil, 4) // legacy `file` unused by SubmitAndWaitFile
	defer ow.Close()

	const blocks = 8
	const bs = 512
	dataA := randomBytes(blocks*bs, 50)
	dataB := randomBytes(blocks*bs, 51)

	var wg sync.WaitGroup
	submit := func(f *os.File, fileID uint32, data []byte) {
		defer wg.Done()
		for i := 0; i < blocks; i++ {
			chunk := data[i*bs : (i+1)*bs]
			if err := ow.SubmitAndWaitFile(f, writeKey(fileID, uint32(i)), int64(i*bs), chunk); err != nil {
				t.Errorf("SubmitAndWaitFile file %d block %d: %v", fileID, i, err)
			}
		}
	}
	wg.Add(2)
	go submit(fA, 0, dataA)
	go submit(fB, 1, dataB)
	wg.Wait()

	if err := ow.Err(); err != nil {
		t.Fatalf("OrderedWriter error: %v", err)
	}

	gotA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotA, dataA) {
		t.Error("file A content mismatch after interleaved multi-file writes")
	}
	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotB, dataB) {
		t.Error("file B content mismatch after interleaved multi-file writes")
	}
}

func TestOrderedWriterDrainFileWaitsForPendingWrites(t *testing.T) {
	path := writeTempFile(t, zeroBytes(4096))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ow := NewOrderedWriter(nil, 2)
	defer ow.Close()

	data := randomBytes(4096, 52)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chunk := data[i*512 : (i+1)*512]
			if err := ow.SubmitAndWaitFile(f, writeKey(7, uint32(i)), int64(i*512), chunk); err != nil {
				t.Errorf("SubmitAndWaitFile block %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait() // every SubmitAndWaitFile has already returned

	// DrainFile after every block's own SubmitAndWaitFile has returned is
	// necessarily a no-op in terms of outstanding work, but it must still
	// complete promptly and report no error -- this is the safety-net path
	// finalize() relies on.
	if err := ow.DrainFile(f); err != nil {
		t.Fatalf("DrainFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch")
	}
}

// TestOrderedWriterDrainFileClearsPendingSyncTarget is a regression test for
// a real use-after-close data race: maybeSync's periodic-writeback
// accumulator (syncFile/syncMinOff/syncMaxOff/sinceSync) used to survive
// DrainFile unchanged, so a caller that drains a file, closes it (exactly
// what finalizeIO does), while a DIFFERENT file's write is concurrently
// being processed by the writer goroutine, could have that write's
// maybeSync flush the stale accumulator via SyncFileRange against the
// now-closed fd -- caught by `go test -race` in the real directory-server
// e2e tests (TestE2EDirMultiWorker, TestE2EDirLargeTreeManyFiles,
// TestE2EDirSurvivesMidTransferConnectionDrops). This reproduces the same
// shape directly against OrderedWriter: one goroutine drains-then-closes
// file A while another concurrently submits a write for unrelated file B.
func TestOrderedWriterDrainFileClearsPendingSyncTarget(t *testing.T) {
	pathA := writeTempFile(t, zeroBytes(512))
	pathB := writeTempFile(t, zeroBytes(512))
	fA, err := os.OpenFile(pathA, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fB, err := os.OpenFile(pathB, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer fB.Close()

	ow := NewOrderedWriter(nil, 2)

	dataA := randomBytes(512, 60)
	dataB := randomBytes(512, 61)

	if err := ow.SubmitAndWaitFile(fA, writeKey(1, 0), 0, dataA); err != nil {
		t.Fatalf("write A: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Mirrors finalizeIO's real sequence exactly: drain, then close.
		if err := ow.DrainFile(fA); err != nil {
			t.Errorf("drain A: %v", err)
		}
		if err := fA.Close(); err != nil {
			t.Errorf("close A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Mirrors a different connection's worker writing an unrelated
		// file concurrently with A's finalize.
		if err := ow.SubmitAndWaitFile(fB, writeKey(2, 0), 0, dataB); err != nil {
			t.Errorf("write B: %v", err)
		}
	}()
	wg.Wait()

	if err := ow.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotB, dataB) {
		t.Error("file B content mismatch")
	}
}

// TestOrderedWriterCloseFlushesTailSyncRange exercises the channel-closed
// drain path in run() (as opposed to an explicit DrainFile call), verifying
// Close() completes cleanly and without error once flushSync is called from
// both exit branches, not just the explicit-drain one.
func TestOrderedWriterCloseFlushesTailSyncRange(t *testing.T) {
	path := writeTempFile(t, zeroBytes(2048))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ow := NewOrderedWriter(f, 4)
	data := randomBytes(2048, 62)
	for i := 0; i < 4; i++ {
		ow.Submit(uint32(i), int64(i*512), data[i*512:(i+1)*512])
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch after close-driven flush")
	}
}

// TestOrderedWriterPerItemErrorsDoNotPoisonOtherFiles pins both contracts:
// with EnablePerItemErrors, a write failure on one file must not fail
// unrelated writes to a different, healthy file (directory mode's
// requirement -- see writeOne's perItemErr comment); the legacy writer
// (used by the single-file path) keeps its original sticky-error behavior,
// where the first failure poisons every write that follows it.
func TestOrderedWriterPerItemErrorsDoNotPoisonOtherFiles(t *testing.T) {
	pathBad := writeTempFile(t, zeroBytes(512))
	pathGood := writeTempFile(t, zeroBytes(512))
	fBad, err := os.OpenFile(pathBad, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fGood, err := os.OpenFile(pathGood, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer fGood.Close()
	fBad.Close() // subsequent WriteAt on this fd fails

	ow := NewOrderedWriter(nil, 2)
	ow.EnablePerItemErrors()
	defer ow.Close()

	if err := ow.SubmitAndWaitFile(fBad, writeKey(1, 0), 0, randomBytes(512, 63)); err == nil {
		t.Fatal("expected a write error on the closed file")
	}
	dataGood := randomBytes(512, 64)
	if err := ow.SubmitAndWaitFile(fGood, writeKey(2, 0), 0, dataGood); err != nil {
		t.Fatalf("healthy file's write must not be poisoned by the earlier failure: %v", err)
	}
	got, err := os.ReadFile(pathGood)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dataGood) {
		t.Error("healthy file content mismatch")
	}
	// ow.Err() still records the first failure for Close()'s overall result.
	if ow.Err() == nil {
		t.Error("Err() should still report the recorded failure")
	}
	// A6 regression: DrainFile must NOT report the earlier, unrelated
	// file's sticky error either -- it used to return ow.Err()
	// unconditionally, which meant finalizeIO would skip fGood's own
	// truncate/chmod/chtimes purely because SOME OTHER file failed
	// earlier in the transfer.
	if err := ow.DrainFile(fGood); err != nil {
		t.Errorf("DrainFile(fGood) = %v, want nil -- must not be poisoned by fBad's earlier failure", err)
	}
}

// TestOrderedWriterLegacyStickyErrorPoisonsSubsequentWrites is the
// complementary pin: WITHOUT EnablePerItemErrors (the legacy single-file
// server's mode), the original contract holds -- once one write fails,
// every later write is skipped and reported with that same error, even
// against an otherwise-healthy file.
func TestOrderedWriterLegacyStickyErrorPoisonsSubsequentWrites(t *testing.T) {
	pathBad := writeTempFile(t, zeroBytes(512))
	pathGood := writeTempFile(t, zeroBytes(512))
	fBad, err := os.OpenFile(pathBad, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	fGood, err := os.OpenFile(pathGood, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer fGood.Close()
	fBad.Close()

	ow := NewOrderedWriter(nil, 2) // perItemErr left false: legacy mode
	defer ow.Close()

	firstErr := ow.SubmitAndWaitFile(fBad, writeKey(1, 0), 0, randomBytes(512, 65))
	if firstErr == nil {
		t.Fatal("expected a write error on the closed file")
	}
	secondErr := ow.SubmitAndWaitFile(fGood, writeKey(2, 0), 0, randomBytes(512, 66))
	if secondErr == nil {
		t.Fatal("legacy mode must still poison the next write after a failure")
	}
	got, err := os.ReadFile(pathGood)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, 512)) {
		t.Error("the healthy file must be untouched under the legacy sticky-error contract")
	}
}
