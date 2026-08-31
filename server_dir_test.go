package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// newTestFileTableOpts is the one shared setup behind every directory-mode
// test that needs a fileTable (directly, or wrapped in a dirServerState):
// resets the package globals directory-mode tests are order-dependent on
// (crypto key, block size, ... — see setupTransfer's identical crypto-leak
// fix), builds an OrderedWriter with EnablePerItemErrors (matching
// production wiring in startDirServer), and returns a fileTable rooted at a
// fresh temp dir. budget <= 0 uses newFileTable's own default flooring
// (workers*4, floored at 64); a positive budget bypasses that floor
// entirely via a hand-built fileTable literal, for tests that need to
// exercise budget-exhaustion paths a floor of 64 would make impractical to
// reach.
func newTestFileTableOpts(t *testing.T, workers, budget int) (*fileTable, string) {
	t.Helper()
	resetEngineGlobals()
	root := t.TempDir()
	ow := NewOrderedWriter(nil, workers)
	ow.EnablePerItemErrors()
	t.Cleanup(func() { ow.Close() })
	if budget <= 0 {
		return newFileTable(root, workers, ow), root
	}
	ft := &fileTable{
		root:       root,
		ow:         ow,
		byID:       make(map[uint32]*destFile),
		keep:       newKeepSet(),
		openBudget: budget,
	}
	ft.cond = sync.NewCond(&ft.mu)
	return ft, root
}

func newTestFileTable(t *testing.T, workers int) (*fileTable, string) {
	t.Helper()
	return newTestFileTableOpts(t, workers, 0)
}

// newTestFileTableWithBudget is newTestFileTable but with an explicit,
// possibly tiny, openBudget -- newFileTable itself always floors it at 64,
// which makes budget-exhaustion paths impractical to exercise directly.
func newTestFileTableWithBudget(t *testing.T, workers, budget int) (*fileTable, string) {
	t.Helper()
	return newTestFileTableOpts(t, workers, budget)
}

// --- Announce: path safety ---

func TestFileTableAnnounceRejectsPathTraversal(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	cases := []string{
		"../etc/passwd",
		"a/../../b",
		"/etc/passwd",
		"a/b/../../../c",
	}
	for _, p := range cases {
		err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: p})
		if err == nil {
			t.Errorf("Announce(%q) unexpectedly succeeded", p)
		}
	}
	if ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0 after only rejected announces", ft.AnnouncedCount())
	}
}

func TestFileTableAnnounceCreatesParentDirs(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a/b/c/f.txt", FileSize: 0, Mode: 0644}); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "a", "b", "c"))
	if err != nil {
		t.Fatalf("parent dirs not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("parent path is not a directory")
	}
}

func TestFileTableAnnounceDirIsImmediatelyFinalized(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "adir", Mode: 0755}); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	// A directory retires immediately on announce (see announce's
	// born-finalized path) -- assertEntryRetiredCleanly's lookup/Acquire
	// checks are exactly "finalized immediately, never reachable via
	// Acquire".
	assertEntryRetiredCleanly(t, ft, 1)
}

func TestFileTableAnnounceSkippedIsImmediatelyFinalized(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindSkipped, Path: "a-symlink"}); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	assertEntryRetiredCleanly(t, ft, 1)
	if _, err := os.Lstat(filepath.Join(root, "a-symlink")); err == nil {
		t.Error("a KindSkipped entry must not create anything on disk")
	}
}

// --- Announce: duplicates ---

func TestFileTableAnnounceRejectsDuplicateFileID(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 5, Kind: KindFile, Path: "a.txt"}); err != nil {
		t.Fatalf("first announce: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 5, Kind: KindFile, Path: "b.txt"}); err == nil {
		t.Error("re-announcing the same FileID under a different path unexpectedly succeeded")
	}
	if ft.AnnouncedCount() != 1 {
		t.Errorf("AnnouncedCount = %d, want 1", ft.AnnouncedCount())
	}
}

func TestFileTableAnnounceRejectsDuplicatePath(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "same.txt"}); err != nil {
		t.Fatalf("first announce: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "same.txt"}); err == nil {
		t.Error("two different FileIDs announcing the same path unexpectedly both succeeded")
	}
	if ft.AnnouncedCount() != 1 {
		t.Errorf("AnnouncedCount = %d, want 1", ft.AnnouncedCount())
	}
}

func TestFileTableAnnounceAllowsCaseVariantPaths(t *testing.T) {
	// Same-OS (this test always runs on the server's own filesystem) case
	// variance must NOT be rejected: "A.txt" and "a.txt" are legitimately
	// different files on a case-sensitive filesystem, and the duplicate
	// check is deliberately exact-match, not case-folded, precisely to
	// avoid this false positive.
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "A.txt"}); err != nil {
		t.Fatalf("announce A.txt: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "a.txt"}); err != nil {
		t.Fatalf("announce a.txt: %v", err)
	}
}

// --- Acquire / Release / double latch ---

func TestFileTableAcquireUnknownFileFails(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if _, _, err := ft.Acquire(999); err == nil {
		t.Error("Acquire on a never-announced FileID unexpectedly succeeded")
	}
}

func TestFileTableAcquireCreatesAndTruncates(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 100, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	df, created, err := ft.Acquire(1)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !created {
		t.Error("created = false for a brand-new file")
	}
	if df.refs != 1 {
		t.Errorf("refs = %d, want 1", df.refs)
	}
	info, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 100 {
		t.Errorf("size = %d, want 100 (truncated to the announced size)", info.Size())
	}
}

func TestFileTableAcquireIsIdempotentAcrossConnections(t *testing.T) {
	// Two "connections" (simulated by two direct Acquire calls) binding to
	// the same file is exactly what soft binding relies on: a redialed
	// connection re-acquiring a file it already held must not corrupt
	// anything, just bump refs again.
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df1, created1, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	df2, created2, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if df1 != df2 {
		t.Error("Acquire returned different destFile pointers for the same FileID")
	}
	if created2 {
		t.Error("second Acquire reported created=true for an already-open file")
	}
	if df1.refs != 2 {
		t.Errorf("refs = %d, want 2 after two Acquire calls", df1.refs)
	}
	_ = created1
}

func TestFileTableDoubleLatchReleaseThenMarkDone(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if df.finalized {
		t.Fatal("finalized before either half of the latch closed")
	}
	if err := ft.Release(df); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if df.finalized {
		t.Error("finalized after Release alone (refs==0 but sawDone still false)")
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if !df.finalized {
		t.Error("not finalized after both refs==0 and sawDone==true")
	}
}

func TestFileTableDoubleLatchMarkDoneThenRelease(t *testing.T) {
	// The opposite order: OpFileDone can legitimately arrive on the control
	// connection before a worker's own Release, since blocks complete out
	// of order across workers.
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if df.finalized {
		t.Error("finalized after MarkDone alone (a ref is still held)")
	}
	if err := ft.Release(df); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !df.finalized {
		t.Error("not finalized after the remaining ref was released")
	}
}

func TestFileTableDoubleLatchMultipleRefsDelaysFinalize(t *testing.T) {
	// Two concurrent "workers" (multi-worker traffic on one large file)
	// both holding a ref: releasing one must not finalize while the other
	// is still active.
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df1, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	df2, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df1); err != nil {
		t.Fatal(err)
	}
	if df1.finalized {
		t.Fatal("finalized while a second ref (df2) is still held")
	}
	if err := ft.Release(df2); err != nil {
		t.Fatal(err)
	}
	if !df1.finalized {
		t.Error("not finalized after both refs released and sawDone was already true")
	}
}

func TestFileTableAcquireAfterFinalizeFails(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df); err != nil {
		t.Fatal(err)
	}
	if !df.finalized {
		t.Fatal("setup failed: file not finalized")
	}
	if _, _, err := ft.Acquire(1); err == nil {
		t.Error("Acquire on an already-finalized file unexpectedly succeeded")
	}
}

func TestFileTableFinalizeAppliesModeAndMTime(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	mtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := ft.Announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 5,
		Mode: 0640, MTimeSec: mtime.Unix(), MTimeNs: int32(mtime.Nanosecond()),
	}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("mode = %o, want 0640", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), mtime)
	}
}

func TestFileTableFinalizeEmptyFileNeverAcquired(t *testing.T) {
	// A zero-byte file never goes through Acquire in the real client flow
	// (TreeReader skips Size==0 entries), so finalize must handle df.file
	// still being nil.
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "empty.txt", FileSize: 0, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	// MarkDone with refs already at 0 finalizes (and, in this new design,
	// retires) the entry synchronously, before MarkDone returns.
	assertEntryRetiredCleanly(t, ft, 1)
	info, err := os.Stat(filepath.Join(root, "empty.txt"))
	if err != nil {
		t.Fatalf("empty file was never created: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}

// --- negotiateBlockSize ---

func TestDestFileNegotiateBlockSize(t *testing.T) {
	df := &destFile{}
	if err := df.negotiateBlockSize(4096); err != nil {
		t.Fatalf("first negotiate: %v", err)
	}
	if err := df.negotiateBlockSize(4096); err != nil {
		t.Errorf("repeat negotiate with the same size: %v", err)
	}
	if err := df.negotiateBlockSize(8192); err == nil {
		t.Error("negotiate with a different size unexpectedly succeeded")
	}
}

// --- reapStaleRefs ---

func TestFileTableReapStaleRefsForceReleasesAbandonedRef(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	// Simulate the owning connection having vanished a long time ago
	// without ever sending OpFileClose.
	df.mu.Lock()
	df.lastActive = time.Now().Add(-2 * staleRefAge)
	df.mu.Unlock()

	reaped := ft.reapStaleRefs()
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if !df.finalized {
		t.Error("file was not finalized after its stale ref was reaped")
	}
}

func TestFileTableReapStaleRefsLeavesFreshRefsAlone(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	// No MarkDone yet, and lastActive is fresh: must not be touched.
	if reaped := ft.reapStaleRefs(); reaped != 0 {
		t.Errorf("reaped = %d, want 0 for a fresh, in-progress file", reaped)
	}
}

func TestFileTableReapStaleRefsIgnoresRecentActivityEvenAfterDone(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	// sawDone is true and a ref is held, but activity was recent (e.g. a
	// long sequence of checksum-only, no-write exchanges on a slow but
	// alive connection) -- must NOT be reaped.
	if reaped := ft.reapStaleRefs(); reaped != 0 {
		t.Errorf("reaped = %d, want 0 for a recently-active ref", reaped)
	}
	if df.finalized {
		t.Error("finalized despite a still-fresh ref")
	}
}

func TestFileTableWaitAllFinalizedTimesOut(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	// Never MarkDone/Release: the file can never finalize on its own, and
	// staleRefAge is far longer than this test's budget, so the ctx
	// deadline must be what actually ends the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := ft.WaitAllFinalized(ctx)
	if err == nil {
		t.Fatal("WaitAllFinalized returned nil for a file that never finalized")
	}
}

func TestFileTableWaitAllFinalizedReturnsOnceComplete(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ft.WaitAllFinalized(ctx); err != nil {
		t.Fatalf("WaitAllFinalized: %v", err)
	}
}

// TestFileTableWaitAllFinalizedReturnsPromptlyAfterLateFinalize pins the
// finalize-wait tail latency: OpTreeDone (control connection) routinely
// arrives while a worker connection's own teardown -- whose Release is what
// actually finalizes the last file -- is still in flight, so
// WaitAllFinalized's very first allFinalized() check fails and the return
// time is then set by its polling period. That period used to be
// staleRefCheckPeriod (2s), adding up to 2 full seconds of pure tail
// latency to every transfer that lost this race; finalizePollPeriod (10ms)
// is what bounds it now. The 1s assertion budget sits far above any
// plausible scheduling noise on a loaded machine but far below the old 2s
// tick, so it fails on a regression to reap-cadence polling without being
// flaky.
func TestFileTableWaitAllFinalizedReturnsPromptlyAfterLateFinalize(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	// Release -- the call that finalizes, since MarkDone above saw refs>0 --
	// lands only after WaitAllFinalized is already waiting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		ft.Release(df)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := ft.WaitAllFinalized(ctx); err != nil {
		t.Fatalf("WaitAllFinalized: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("WaitAllFinalized took %v after a finalize that landed at +50ms -- polling period regression", elapsed)
	}
}

// --- isWithinRoot ---

func TestIsWithinRoot(t *testing.T) {
	root := "/tmp/dst"
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/dst", true},
		{"/tmp/dst/a", true},
		{"/tmp/dst/a/b", true},
		{"/tmp/dstx", false},
		{"/tmp", false},
		{"/tmp/ds", false},
		{"/etc/passwd", false},
	}
	for _, tt := range cases {
		if got := isWithinRoot(root, tt.path); got != tt.want {
			t.Errorf("isWithinRoot(%q, %q) = %v, want %v", root, tt.path, got, tt.want)
		}
	}
}

// --- finalize / fd budget edge cases ---

// TestFileTableFinalizeNeverAcquiredFileWithExhaustedBudget is a regression
// test for F2: finalizeIO's never-opened branch used to take the fd budget
// for its transient open/truncate/close, but that budget is only ever freed
// by OTHER finalize calls -- so a zero-byte/never-acquired file finalizing
// while every slot is held by still-open files could never get one,
// wedging the connection (and therefore the whole tree) forever.
func TestFileTableFinalizeNeverAcquiredFileWithExhaustedBudget(t *testing.T) {
	ft, _ := newTestFileTableWithBudget(t, 1, 2)

	// Consume the entire budget with two genuinely open (Acquire'd) files.
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "open1.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "open2.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(2); err != nil {
		t.Fatal(err)
	}

	// A never-acquired (zero-byte) file finalizing must not need the fd
	// budget at all.
	if err := ft.Announce(AnnounceRec{FileID: 3, Kind: KindFile, Path: "empty.txt", FileSize: 0}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ft.MarkDone(3) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MarkDone on a never-acquired file deadlocked with the fd budget exhausted")
	}
}

// TestFileTableAcquireDoesNotHoldFileLockWhileWaitingForBudget is a
// regression test for F3: Acquire used to call the table-wide budget wait
// while still holding the target file's own df.mu, serializing every other
// operation on that file (an idempotent re-Acquire from another
// connection, a Release, a stale-ref reap, negotiateBlockSize) behind a
// wait that has nothing to do with that file's own state -- and, combined
// with F2, was the shape that could turn a temporary budget shortage into a
// real deadlock.
func TestFileTableAcquireDoesNotHoldFileLockWhileWaitingForBudget(t *testing.T) {
	ft, _ := newTestFileTableWithBudget(t, 1, 1)

	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "b.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil { // consumes the single budget slot
		t.Fatal(err)
	}

	acquireDone := make(chan error, 1)
	go func() {
		_, _, err := ft.Acquire(2) // must block on the budget alone
		acquireDone <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it actually reach the budget wait

	// While Acquire(2) is blocked, file 2's own lock must still be free: a
	// plain lock/unlock (what isFinalized, negotiateBlockSize, Release,
	// etc. all do) must not itself block on it.
	df2, err := ft.lookup(2)
	if err != nil {
		t.Fatal(err)
	}
	lockAcquired := make(chan struct{})
	go func() {
		df2.mu.Lock()
		close(lockAcquired)
		df2.mu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("file 2's own lock is held merely because Acquire(2) is waiting on the fd budget")
	}
}

func TestFileTableAcquireBudgetReturnedWhenAlreadyOpen(t *testing.T) {
	ft, _ := newTestFileTableWithBudget(t, 1, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	// Repeated Acquire of an already-open file must consume no additional
	// budget -- with the single slot already spent, a second slot being
	// taken here would exhaust the budget and this call would hang.
	done := make(chan error, 1)
	go func() {
		_, _, err := ft.Acquire(1)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Acquire: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire of an already-open file blocked on the fd budget")
	}
}

// --- symlinked-ancestor mitigation (F9) ---

// assertEntryDeadOnArrival is the contract a rejected-but-non-fatal announce
// must satisfy. The rejection is deliberately NOT an Announce error: killing
// the control connection over one bad path aborts the whole tree (every
// later file failing "was never announced"). Instead the entry must be
// registered as announced-but-failed -- keeping AnnouncedCount consistent
// with the client's claimed total and the path in the -delete keep-set --
// finalized with an error, and permanently unacquirable, so no fd is ever
// opened for it and no byte can be written through it.
// assertEntryDeadOnArrival verifies that id was announced but immediately
// retired with an error (a "born-dead" entry: an announce-time failure like
// a symlinked ancestor, an occupied path, or an unrepresentable name). Such
// an entry is never published to byID at all (see announce's born-finalized
// path) -- there is no live *destFile left to inspect by id, only the
// permanent-rejection signal lookup/Acquire give back and the error rolled
// into the table's first-error slot. Every caller of this helper has
// exactly one dead entry per fileTable, so FirstFinalizeErr unambiguously
// belongs to id.
func assertEntryDeadOnArrival(t *testing.T, ft *fileTable, id uint32) {
	t.Helper()
	if _, err := ft.lookup(id); !errors.Is(err, errAlreadyFinalized) {
		t.Fatalf("entry %d: lookup = %v, want errAlreadyFinalized (a failed announce retires immediately)", id, err)
	}
	if ft.FirstFinalizeErr() == nil {
		t.Errorf("entry %d: FirstFinalizeErr() is nil, want the recorded announce failure", id)
	}
	if _, _, acqErr := ft.Acquire(id); acqErr == nil {
		t.Errorf("entry %d: Acquire succeeded; a failed announce must never open an fd", id)
	}
}

// assertEntryRetiredCleanly is assertEntryDeadOnArrival's counterpart for a
// born-finalized entry that succeeded rather than failed: a directory,
// symlink, special file, skipped placeholder, or quick-check skip. Like a
// failed announce, none of these are ever published to byID (see announce's
// born-finalized path), so lookup/Acquire give back the identical
// permanent-rejection signal -- the only difference is that no error is
// expected to have been recorded for it.
func assertEntryRetiredCleanly(t *testing.T, ft *fileTable, id uint32) {
	t.Helper()
	if _, err := ft.lookup(id); !errors.Is(err, errAlreadyFinalized) {
		t.Fatalf("entry %d: lookup = %v, want errAlreadyFinalized (retires immediately on announce)", id, err)
	}
	if _, _, acqErr := ft.Acquire(id); acqErr == nil {
		t.Errorf("entry %d: Acquire succeeded; a born-finalized entry must never open an fd", id)
	}
}

func TestFileTableAnnounceRejectsSymlinkedAncestor(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	// "a" was never announced as KindDir at all -- the file is announced
	// directly under it, which is exactly the gap the ancestor check (not
	// the direct-entry check) has to catch.
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a/x.txt", FileSize: 1}); err != nil {
		t.Fatalf("a symlinked ancestor must fail the ENTRY, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Lstat(filepath.Join(outside, "x.txt")); err == nil {
		t.Error("a file was created outside the destination root through the symlink")
	}
}

func TestFileTableAnnounceRejectsSymlinkedGrandparent(t *testing.T) {
	// The symlink is two levels up from the announced file, not the
	// immediate parent -- rejectSymlinkedAncestors must walk every
	// component, not just the last one.
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a/b/x.txt", FileSize: 1}); err != nil {
		t.Fatalf("a symlinked grandparent must fail the ENTRY, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Lstat(filepath.Join(outside, "b")); err == nil {
		t.Error("a directory was created outside the destination root through the symlink")
	}
}

// TestFileTableAnnounceParentExistsAsRegularFile pins the skip-on-error
// contract for ENVIRONMENTAL announce failures (a mkdir hitting ENOTDIR
// here; ENOSPC/EACCES behave identically): the entry is registered as
// announced-but-failed -- keeping AnnouncedCount consistent with the
// client's total and the path in the -delete keep-set -- rather than
// returning an error, which used to kill the CONTROL connection this
// arrived on and silently orphan the entire multi-hour transfer (observed
// in production with a full disk).
func TestFileTableAnnounceParentExistsAsRegularFile(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f/x.txt", FileSize: 1}); err != nil {
		t.Fatalf("an environmental mkdir failure must not error the announce (it must degrade to a per-file failure): %v", err)
	}
	if ft.AnnouncedCount() != 1 {
		t.Errorf("AnnouncedCount = %d, want 1 -- the failed entry must still register", ft.AnnouncedCount())
	}
	// A mkdir-failed entry is immediately finalized (so WaitAllFinalized
	// never waits on it) and its error is recorded for FirstFinalizeErr.
	assertEntryDeadOnArrival(t, ft, 1)
	// The parent regular file must be untouched.
	got, err := os.ReadFile(filepath.Join(root, "f"))
	if err != nil || string(got) != "x" {
		t.Errorf("pre-existing file at the blocked parent path was altered: %q, %v", got, err)
	}
}

// TestFileTableAnnounceMkdirFailedDirCascades: a KindDir whose own mkdir
// fails registers as failed, and its children each fail their own
// parent-mkdir the same way -- every entry registered, none erroring the
// announce stream.
func TestFileTableAnnounceMkdirFailedDirCascades(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.WriteFile(filepath.Join(root, "d"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "d", Mode: 0755}); err != nil {
		t.Fatalf("dir announce: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "d/child.txt", FileSize: 3}); err != nil {
		t.Fatalf("child announce: %v", err)
	}
	if ft.AnnouncedCount() != 2 {
		t.Errorf("AnnouncedCount = %d, want 2", ft.AnnouncedCount())
	}
	// Both entries failed their own mkdir and retired immediately; with two
	// failures in the same table, FirstFinalizeErr can only speak for
	// whichever retired first, so it's checked once below rather than
	// per-id.
	for _, id := range []uint32{1, 2} {
		if _, err := ft.lookup(id); !errors.Is(err, errAlreadyFinalized) {
			t.Errorf("entry %d: lookup = %v, want errAlreadyFinalized", id, err)
		}
		if _, _, acqErr := ft.Acquire(id); acqErr == nil {
			t.Errorf("entry %d: Acquire succeeded; a mkdir-failed entry must never open an fd", id)
		}
	}
	if ft.FirstFinalizeErr() == nil {
		t.Error("FirstFinalizeErr must surface the mkdir failures")
	}
}

func TestFileTableAnnounceRejectsUnknownKind(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: 99, Path: "x.txt"}); err == nil {
		t.Error("announcing an unknown entry kind unexpectedly succeeded")
	}
	if ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0 after a rejected announce", ft.AnnouncedCount())
	}
}

// --- finalize: mode/permission interaction with re-runs ---

func TestFileTableFinalizePreservesReadOnlyModeAndStillWritable(t *testing.T) {
	// The source file is read-only (0444). Acquire must still be able to
	// open and write it -- it always creates with 0666 and applies the
	// real mode only at finalize -- and finalize must still end up with
	// the destination at exactly 0444.
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "ro.txt", FileSize: 4, Mode: 0444}); err != nil {
		t.Fatal(err)
	}
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatalf("Acquire on a to-be-read-only destination: %v", err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "ro.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0444 {
		t.Errorf("mode = %o, want 0444", info.Mode().Perm())
	}
}

func TestFileTableAcquireOnExistingReadOnlyDestFile(t *testing.T) {
	// A destination left read-only (0444) by a PREVIOUS run must still be
	// openable and writable on a later run -- this is exactly why Acquire
	// always opens with 0666 rather than df.mode: getting this wrong
	// breaks every second run against a file the first run made read-only.
	ft, root := newTestFileTable(t, 1)
	path := filepath.Join(root, "ro.txt")
	if err := os.WriteFile(path, []byte("old!"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "ro.txt", FileSize: 4, Mode: 0444}); err != nil {
		t.Fatal(err)
	}
	df, created, err := ft.Acquire(1)
	if err != nil {
		t.Fatalf("Acquire on a pre-existing read-only destination: %v", err)
	}
	if created {
		t.Error("created = true for a destination that already existed")
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(df); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0444 {
		t.Errorf("mode = %o, want 0444 restored at finalize", info.Mode().Perm())
	}
}

func TestFileTableAcquireTruncatesLongerExistingDestination(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, make([]byte, 1000), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 100, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 100 {
		t.Errorf("size = %d, want 100 (a longer pre-existing destination must be truncated down)", info.Size())
	}
}

func TestFileTableAcquireExtendsShorterExistingDestination(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, make([]byte, 10), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 1000, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ft.Acquire(1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1000 {
		t.Errorf("size = %d, want 1000 (a shorter pre-existing destination must be extended)", info.Size())
	}
}

// --- finalizeComplete vs finalized (F8) ---

// TestFileTableWaitAllFinalizedDoesNotTrustFinalizedAloneWithoutFinalizeComplete
// is a regression test for F8: allFinalized must not consider a file done
// until finalizeIO has actually RETURNED, not merely been decided.
// finalized flips BEFORE finalizeIO's actual drain/truncate/close/chmod/
// chtimes work runs -- by design, to close the Acquire-exclusion TOCTOU
// window (see Release's comment) -- so an allFinalized satisfied by
// finalized alone could return while some other file's finalizeIO call is
// genuinely still in flight elsewhere, which is exactly what raced against
// ow.Close() in the real e2e failures this fix addresses.
//
// allFinalized is now an O(1) counter comparison (retiredCount ==
// announced, see its own comment) rather than a scan of finalizeComplete
// flags, and retiredCount is incremented ONLY by retire -- which
// runFinalizeIO calls strictly AFTER finalizeIO has returned. So the
// invariant this test protects is now enforced by retire never running
// early, rather than by allFinalized checking the right field; this drives
// the table through exactly runFinalizeIO's real sequence (flip
// finalizeComplete, THEN retire) with the two steps deliberately staged
// apart, since finalizeIO's real duration is too short to race reliably in
// a unit test.
func TestFileTableWaitAllFinalizedDoesNotTrustFinalizedAloneWithoutFinalizeComplete(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	df, err := ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	df.mu.Lock()
	df.finalized = true // the decision has been made...
	df.mu.Unlock()
	// ...but finalizeComplete is deliberately left false, and retire is
	// deliberately NOT called yet: finalizeIO has not actually run/returned,
	// as if it were still in flight on another goroutine.

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := ft.WaitAllFinalized(ctx); err == nil {
		t.Fatal("WaitAllFinalized returned nil while the entry had not yet retired")
	}

	// Now run exactly what runFinalizeIO would run once finalizeIO actually
	// returns: flip finalizeComplete, then retire.
	df.mu.Lock()
	df.finalizeComplete = true
	df.mu.Unlock()
	ft.retire(df)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := ft.WaitAllFinalized(ctx2); err != nil {
		t.Fatalf("WaitAllFinalized after retire: %v", err)
	}
}

// TestFileTableDrainFileErrorIsolatedToItsOwnFile is a regression test for
// A6: one file's write failure (a closed fd, ENOSPC, EIO, a race with
// reapStaleRefs) must not poison a DIFFERENT file's finalize. Before the
// fix, DrainFile returned the writer's STICKY GLOBAL error, so every file
// finalized after the first failure anywhere in the tree silently skipped
// its own truncate/chmod/chtimes -- wrong size/mode/mtime on a file whose
// own blocks wrote perfectly, and the whole tree reported failed via
// FirstFinalizeErr() regardless.
func TestFileTableDrainFileErrorIsolatedToItsOwnFile(t *testing.T) {
	ft, root := newTestFileTable(t, 2)
	mtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := ft.Announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "bad.txt", FileSize: 10, Mode: 0640,
		MTimeSec: mtime.Unix(), MTimeNs: int32(mtime.Nanosecond()),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{
		FileID: 2, Kind: KindFile, Path: "good.txt", FileSize: 10, Mode: 0640,
		MTimeSec: mtime.Unix(), MTimeNs: int32(mtime.Nanosecond()),
	}); err != nil {
		t.Fatal(err)
	}

	dfBad, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatal(err)
	}
	dfGood, _, err := ft.Acquire(2)
	if err != nil {
		t.Fatal(err)
	}

	// Force a real write failure on file 1 by closing its underlying fd
	// out from under it, then submitting a write the normal block-handling
	// path (handleBlockData/handleBlockZero) would have made.
	dfBad.file.Close()
	if err := ft.ow.SubmitAndWaitFile(dfBad.file, writeKey(1, 0), 0, make([]byte, 10)); err == nil {
		t.Fatal("expected a write error on the closed fd")
	}

	// File 1 finalizes with a recorded error -- expected, its own write
	// really did fail. MarkDone (refs still 1 here) just records sawDone;
	// Release is what brings refs to 0 and actually triggers finalizeIO,
	// so its return value legitimately IS this file's finalize error, not
	// something to fail the test setup on.
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(dfBad); err == nil {
		t.Fatal("setup problem: expected file 1's finalize (triggered by Release) to fail")
	}
	if dfBad.finalizeErr == nil {
		t.Fatal("setup problem: file 1's finalizeErr should be recorded")
	}

	// File 2's own block wrote fine; its finalize must be entirely
	// unaffected by file 1's earlier, unrelated failure.
	if err := ft.ow.SubmitAndWaitFile(dfGood.file, writeKey(2, 0), 0, make([]byte, 10)); err != nil {
		t.Fatalf("file 2's own write: %v", err)
	}
	if err := ft.MarkDone(2); err != nil {
		t.Fatal(err)
	}
	if err := ft.Release(dfGood); err != nil {
		t.Fatal(err)
	}
	if dfGood.finalizeErr != nil {
		t.Errorf("file 2's finalizeErr = %v, want nil -- must not be poisoned by file 1's failure", dfGood.finalizeErr)
	}
	info, err := os.Stat(filepath.Join(root, "good.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("file 2 mode = %o, want 0640 -- chmod must not have been skipped", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mtime) {
		t.Errorf("file 2 mtime = %v, want %v -- chtimes must not have been skipped", info.ModTime(), mtime)
	}
}

// --- Announce: quick-check (size + mtime, second precision) ---

// writeQuickCheckDest creates a real pre-existing destination file at
// root/relPath with the given content, mode, and mtime -- the shape every
// quick-check test below needs the destination side of announce to see
// before the client's own AnnounceRec arrives.
func writeQuickCheckDest(t *testing.T, root, relPath string, content []byte, mode os.FileMode, mtime time.Time) {
	t.Helper()
	abs := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0777); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(abs, content, mode); err != nil {
		t.Fatalf("write dest: %v", err)
	}
	if err := os.Chmod(abs, mode); err != nil {
		t.Fatalf("chmod dest: %v", err)
	}
	if err := os.Chtimes(abs, mtime, mtime); err != nil {
		t.Fatalf("chtimes dest: %v", err)
	}
}

func TestFileTableAnnounceQuickCheckSkipsMatchingFile(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("unchanged content")
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", content, 0644, mtime)

	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)), Mode: 0644, MTimeSec: mtime.Unix(),
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !skip {
		t.Fatal("expected skip=true for a matching size+mtime file")
	}
	assertEntryRetiredCleanly(t, ft, 1)
	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("dest content changed: got %q, want %q -- a skip must never touch file data", got, content)
	}
}

func TestFileTableAnnounceQuickCheckSkipsWithSubSecondMtimeDifference(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("payload")
	// Same whole second, different nanosecond offset -- must still skip:
	// the comparison is deliberately second-precision, not nanosecond.
	mtime := time.Unix(1700000000, 900000000)
	writeQuickCheckDest(t, root, "f.txt", content, 0644, mtime)

	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)), Mode: 0644,
		MTimeSec: 1700000000, MTimeNs: 100000000, // same second, different ns
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !skip {
		t.Error("a sub-second mtime difference within the same whole second must still skip")
	}
}

func TestFileTableAnnounceQuickCheckDoesNotSkipOnSizeMismatch(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("old content, wrong size")
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", content, 0644, mtime)

	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)) + 1, Mode: 0644, MTimeSec: mtime.Unix(),
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("a size mismatch must never skip")
	}
}

func TestFileTableAnnounceQuickCheckDoesNotSkipOnMtimeMismatch(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("content")
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", content, 0644, mtime)

	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)), Mode: 0644, MTimeSec: mtime.Unix() + 1,
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("an mtime mismatch of a full second must never skip")
	}
}

func TestFileTableAnnounceQuickCheckDoesNotSkipMissingDest(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: 10, Mode: 0644, MTimeSec: 1700000000,
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("a nonexistent destination file must never skip")
	}
}

func TestFileTableAnnounceQuickCheckDoesNotSkipWhenDestIsDir(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.MkdirAll(filepath.Join(root, "f.txt"), 0777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: 0, Mode: 0644, MTimeSec: 1700000000,
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("a directory occupying the destination path must never skip")
	}
}

func TestFileTableAnnounceQuickCheckIgnoresZeroByteFiles(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", nil, 0644, mtime)

	// Size and mtime match exactly, but FileSize==0 is deliberately excluded
	// from quick-check: zero-byte files already have their own always-skip
	// mechanism elsewhere (startDirClient's empty-file loop), so this path
	// must leave them alone rather than double-handle them.
	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: 0, Mode: 0644, MTimeSec: mtime.Unix(),
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("a zero-byte file must never be quick-check-skipped")
	}
}

func TestFileTableAnnounceQuickCheckOffNeverSkips(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("content")
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", content, 0644, mtime)

	// Same call as the matching-skip test above, but quickCheck=false: must
	// never skip, and the legacy Announce wrapper must agree.
	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)), Mode: 0644, MTimeSec: mtime.Unix(),
	}, false)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("quickCheck=false must never skip, regardless of a matching dest")
	}
	df, err := ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	if df.isFinalized() {
		t.Error("a non-skipped KindFile entry must not be finalized at announce time")
	}

	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "g.txt", FileSize: uint64(len(content)), Mode: 0644, MTimeSec: mtime.Unix()}); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	df2, err := ft.lookup(2)
	if err != nil {
		t.Fatal(err)
	}
	if df2.isFinalized() {
		t.Error("the legacy Announce wrapper must never skip/finalize a KindFile entry")
	}
}

func TestFileTableAnnounceQuickCheckCorrectsPermissions(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	content := []byte("content")
	mtime := time.Unix(1700000000, 0)
	writeQuickCheckDest(t, root, "f.txt", content, 0600, mtime)

	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: uint64(len(content)), Mode: 0644, MTimeSec: mtime.Unix(),
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if !skip {
		t.Fatal("expected skip=true")
	}
	info, err := os.Stat(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("mode = %o, want 0644 -- a skip must still correct a permission mismatch", info.Mode().Perm())
	}
	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("dest content changed: got %q, want %q", got, content)
	}
}

// --- deleteExtraneous (-delete) ---

func TestFileTableDeleteExtraneousRemovesStaleFile(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "keep.txt", FileSize: 3}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stale.txt"), []byte("xyz"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt still exists (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Errorf("keep.txt was removed: %v", err)
	}
}

func TestFileTableDeleteExtraneousRemovesStaleDirectoryRecursively(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "keep.txt", FileSize: 0}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Join(root, "staledir", "nested")
	if err := os.MkdirAll(staleDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "staledir", "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	// The whole "staledir" subtree is removed in one RemoveAll -- counted
	// as a single removal, not one per file inside it.
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "staledir")); !os.IsNotExist(err) {
		t.Errorf("staledir still exists (stat err = %v)", err)
	}
}

func TestFileTableDeleteExtraneousRecursesIntoAnnouncedDirAndRemovesStaleChild(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "sub", Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "sub/keep.txt", FileSize: 3}); err != nil {
		t.Fatal(err)
	}
	// A KindSkipped placeholder NESTED under the announced directory (e.g.
	// "sub/skip.log" excluded while "sub" itself is synced): the protection
	// must hold at any depth the sweep recurses to, not just at the root.
	if err := ft.Announce(AnnounceRec{FileID: 3, Kind: KindSkipped, Path: "sub/skip.log"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "keep.txt"), []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "stale.txt"), []byte("xyz"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "skip.log"), []byte("pre-existing"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("sub/stale.txt still exists (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "keep.txt")); err != nil {
		t.Errorf("sub/keep.txt was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "skip.log")); err != nil {
		t.Errorf("sub/skip.log (nested skipped placeholder) was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub")); err != nil {
		t.Errorf("sub itself was removed: %v", err)
	}
}

// TestFileTableDeleteExtraneousProtectsSkippedPathAndDoesNotRecurseIntoIt is
// the core -delete/-exclude safety guarantee: an excluded source directory
// is announced as a single KindSkipped placeholder (its contents are never
// walked, so no per-child announce records exist for them at all). The
// delete pass must leave that placeholder's destination-side namesake AND
// everything already sitting under it completely untouched -- not just
// "not delete the directory itself" but "never even look inside it",
// exactly like an excluded directory's real contents (which were never
// transferred and could be arbitrary pre-existing data).
func TestFileTableDeleteExtraneousProtectsSkippedPathAndDoesNotRecurseIntoIt(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindSkipped, Path: "excluded-dir"}); err != nil {
		t.Fatal(err)
	}
	// Nothing is created on disk for a KindSkipped announce (see
	// fileTable.announce), so this simulates it having pre-existed on the
	// destination already -- exactly the scenario -delete must not disturb.
	if err := os.MkdirAll(filepath.Join(root, "excluded-dir", "nested"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded-dir", "junk.txt"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "excluded-dir", "nested", "deep.txt"), []byte("deep"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 -- an excluded path's contents must never be touched", removed)
	}
	for _, rel := range []string{"excluded-dir", "excluded-dir/junk.txt", "excluded-dir/nested", "excluded-dir/nested/deep.txt"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s was removed or altered: %v", rel, err)
		}
	}
}

// TestFileTableDeleteExtraneousContinuesPastPerEntryFailure proves one
// unremovable entry doesn't abort the rest of the sweep: an unrelated stale
// file elsewhere in the tree must still be removed.
func TestFileTableDeleteExtraneousContinuesPastPerEntryFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits behave differently on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't restrict access")
	}
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "blocked", Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	blockedDir := filepath.Join(root, "blocked")
	if err := os.WriteFile(filepath.Join(blockedDir, "stale.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stale-elsewhere.txt"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	// Strip write permission from "blocked" so unlinking its child fails
	// with EACCES; restore it afterward so t.TempDir()'s own cleanup can
	// still remove it.
	if err := os.Chmod(blockedDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blockedDir, 0777) })

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only stale-elsewhere.txt should have succeeded)", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "stale-elsewhere.txt")); !os.IsNotExist(err) {
		t.Errorf("stale-elsewhere.txt still exists (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(blockedDir, "stale.txt")); err != nil {
		t.Errorf("blocked/stale.txt should still exist (removal should have failed, not silently succeeded): %v", err)
	}
}

// --- announce: KindSymlink (-links) ---

func announceSymlink(t *testing.T, ft *fileTable, id uint32, path, target string) error {
	t.Helper()
	return ft.Announce(AnnounceRec{FileID: id, Kind: KindSymlink, Path: path, LinkTarget: target})
}

func TestFileTableAnnounceSymlinkCreatesLink(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := announceSymlink(t, ft, 1, "sub/rel-link", "../target.txt"); err != nil {
		t.Fatalf("announce: %v", err)
	}
	got, err := os.Readlink(filepath.Join(root, "sub", "rel-link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "../target.txt" {
		t.Errorf("target = %q, want %q (verbatim, dangling is fine)", got, "../target.txt")
	}
	assertEntryRetiredCleanly(t, ft, 1)
}

func TestFileTableAnnounceSymlinkIdempotentSameTarget(t *testing.T) {
	ft1, root := newTestFileTable(t, 1)
	if err := announceSymlink(t, ft1, 1, "link", "target.txt"); err != nil {
		t.Fatalf("first announce: %v", err)
	}
	// A rerun is a fresh fileTable over the same destination root.
	ow := NewOrderedWriter(nil, 1)
	ow.EnablePerItemErrors()
	t.Cleanup(func() { ow.Close() })
	ft2 := newFileTable(root, 1, ow)
	if err := announceSymlink(t, ft2, 1, "link", "target.txt"); err != nil {
		t.Fatalf("rerun announce: %v", err)
	}
	got, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil || got != "target.txt" {
		t.Errorf("after rerun: readlink = %q, %v", got, err)
	}
	if err := ft2.FirstFinalizeErr(); err != nil {
		t.Errorf("rerun recorded an error: %v", err)
	}
}

func TestFileTableAnnounceSymlinkRetargetsExistingLink(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.Symlink("old-target", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := announceSymlink(t, ft, 1, "link", "new-target"); err != nil {
		t.Fatalf("announce: %v", err)
	}
	got, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil || got != "new-target" {
		t.Errorf("readlink = %q, %v, want new-target", got, err)
	}
}

func TestFileTableAnnounceSymlinkReplacesRegularFile(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.WriteFile(filepath.Join(root, "link"), []byte("was a file"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := announceSymlink(t, ft, 1, "link", "target.txt"); err != nil {
		t.Fatalf("announce: %v", err)
	}
	got, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil || got != "target.txt" {
		t.Errorf("readlink = %q, %v, want target.txt", got, err)
	}
}

func TestFileTableAnnounceSymlinkRefusesDirectoryAtPath(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.MkdirAll(filepath.Join(root, "link", "content"), 0777); err != nil {
		t.Fatal(err)
	}
	// Environmental failure: registered-as-failed, never an announce error,
	// and the directory tree is untouched.
	if err := announceSymlink(t, ft, 1, "link", "target.txt"); err != nil {
		t.Fatalf("a directory at the path must be an environmental skip, not an announce error: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, statErr := os.Stat(filepath.Join(root, "link", "content")); statErr != nil {
		t.Errorf("the pre-existing directory was disturbed: %v", statErr)
	}
}

func TestFileTableAnnounceSymlinkRejectsEmptyTarget(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := announceSymlink(t, ft, 1, "link", ""); err == nil {
		t.Error("an empty target is a corrupt record and must hard-fail the announce")
	}
	if ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0", ft.AnnouncedCount())
	}
}

// TestFileTableAnnounceFileUnderJustCreatedSymlinkRejected is the -links
// security regression: rejectSymlinkedAncestors must fire against a
// symlink THIS transfer just created, not only pre-existing ones --
// otherwise announcing symlink "a" -> anywhere followed by file "a/b"
// would write through the link and escape the destination root.
func TestFileTableAnnounceFileUnderJustCreatedSymlinkRejected(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	escape := t.TempDir()
	if err := announceSymlink(t, ft, 1, "a", escape); err != nil {
		t.Fatalf("symlink announce: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "a/b.txt", FileSize: 4}); err != nil {
		t.Fatalf("the rejection must fail the ENTRY, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 2)
	if _, statErr := os.Stat(filepath.Join(escape, "b.txt")); !os.IsNotExist(statErr) {
		t.Errorf("escape target polluted through the symlink (stat err = %v)", statErr)
	}
	_ = root
}

// TestFileTableDeleteExtraneousWithSymlinks: an ANNOUNCED KindSymlink is a
// protected keep-set leaf; an unannounced destination symlink is stale and
// removed -- via lstat semantics, so its TARGET file must survive.
func TestFileTableDeleteExtraneousWithSymlinks(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "target.txt", FileSize: 4}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := announceSymlink(t, ft, 2, "kept-link", "target.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "stale-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	removed, err := ft.deleteExtraneous()
	if err != nil {
		t.Fatalf("deleteExtraneous: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only stale-link)", removed)
	}
	if _, err := os.Lstat(filepath.Join(root, "stale-link")); !os.IsNotExist(err) {
		t.Errorf("stale-link still exists (lstat err = %v)", err)
	}
	if got, err := os.Readlink(filepath.Join(root, "kept-link")); err != nil || got != "target.txt" {
		t.Errorf("kept-link disturbed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "target.txt")); err != nil || string(got) != "data" {
		t.Errorf("the stale link's TARGET was disturbed: %q, %v -- removal must use lstat semantics", got, err)
	}
}

// --- destination type transitions (a stale symlink where the source now
// has a real file or directory) ---

// TestFileTableAnnounceDirReplacesStaleSymlink: the source turned a symlink
// into a real directory since the last run. That is ordinary out-of-date
// destination state, so the link is replaced and the sync converges -- and
// crucially it must NOT fail the announce, which would kill the control
// connection and abort the entire tree.
func TestFileTableAnnounceDirReplacesStaleSymlink(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "d"}); err != nil {
		t.Fatalf("a stale symlink is destination state, not a corrupt stream: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(root, "d"))
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("d is not a real directory: mode=%v err=%v", fi.Mode(), err)
	}
	// A file announced beneath it now resolves inside the root, not through
	// the old link.
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "d/x.txt", FileSize: 4}); err != nil {
		t.Fatalf("announce beneath the repaired directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "x.txt")); err == nil {
		t.Error("content escaped through the replaced symlink")
	}
	if ft.AnnouncedCount() != 2 {
		t.Errorf("AnnouncedCount = %d, want 2", ft.AnnouncedCount())
	}
}

// TestFileTableAnnounceFileReplacesStaleSymlink is the write-through
// regression: a link an earlier -links run left at a path where the source
// now has a REGULAR FILE must be replaced, never followed. Following it
// would write this file's content into the link's target -- an arbitrary
// path outside the destination root.
func TestFileTableAnnounceFileReplacesStaleSymlink(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}

	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 8}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	// Acquire opens (and truncates to the announced size) the destination
	// file -- the exact step that would clobber the victim.
	df, _, err := ft.Acquire(1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ft.Release(df)

	fi, lerr := os.Lstat(filepath.Join(root, "f.txt"))
	if lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("f.txt is still a symlink: mode=%v err=%v", fi.Mode(), lerr)
	}
	got, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("victim unreadable: %v", rerr)
	}
	if string(got) != "ORIGINAL" {
		t.Errorf("wrote through the symlink: victim = %q, want %q", got, "ORIGINAL")
	}
}

// TestFileTableAnnounceQuickCheckIgnoresStaleSymlink: quick-check compares
// size and mtime, and os.Stat FOLLOWS a link -- so a stale link whose target
// happens to match would make the file "already correct" and skip it,
// leaving the link in place on every future run. The link must be cleared
// before quick-check ever looks.
func TestFileTableAnnounceQuickCheckIgnoresStaleSymlink(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("12345678"), 0644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}

	// Same size and mtime as the link's target: the quick-check match.
	skip, err := ft.announce(AnnounceRec{
		FileID: 1, Kind: KindFile, Path: "f.txt",
		FileSize: 8, MTimeSec: fi.ModTime().Unix(),
	}, true)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if skip {
		t.Error("quick-check matched through a symlink and skipped the file")
	}
	if lfi, lerr := os.Lstat(filepath.Join(root, "f.txt")); lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
		t.Error("the stale symlink survived the announce")
	}
}

// TestFileTableAnnounceFileRefusesDirectoryAtPath: the reverse conflict. A
// real directory where a file is announced is left strictly alone (never
// silently destroyed) and reported as this one entry's failure.
func TestFileTableAnnounceFileRefusesDirectoryAtPath(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.MkdirAll(filepath.Join(root, "f.txt", "content"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4}); err != nil {
		t.Fatalf("must be an environmental skip, not an announce error: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Stat(filepath.Join(root, "f.txt", "content")); err != nil {
		t.Errorf("the pre-existing directory was disturbed: %v", err)
	}
}

// TestFileTableAnnounceDirUnderSymlinkedAncestorRemovesNothing pins the
// ordering inside the KindDir branch: the ancestor check must run BEFORE
// clearConflictingDest, which lstats (and removes) the entry's own path. If
// an ancestor is a symlink out of the tree, that path resolves through it,
// so a removal there would delete a file OUTSIDE the destination root.
func TestFileTableAnnounceDirUnderSymlinkedAncestorRemovesNothing(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	// The thing that must survive: a symlink outside the root, sitting at
	// exactly the name the announced directory would resolve to.
	bystander := filepath.Join(outside, "d")
	if err := os.Symlink("somewhere", bystander); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}

	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "a/d"}); err != nil {
		t.Fatalf("a symlinked ancestor must fail the ENTRY, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Lstat(bystander); err != nil {
		t.Errorf("removed a path outside the destination root: %v", err)
	}
}

// --- -specials: FIFO and socket recreation ---

func announceSpecial(t *testing.T, ft *fileTable, id uint32, path string, mode os.FileMode) error {
	t.Helper()
	return ft.Announce(AnnounceRec{FileID: id, Kind: KindSpecial, Path: path, Mode: uint32(mode)})
}

func TestFileTableAnnounceSpecialCreatesFifoAndSocket(t *testing.T) {
	ft, root := newTestFileTable(t, 2)
	if err := announceSpecial(t, ft, 1, "sub/pipe", os.ModeNamedPipe|0644); err != nil {
		t.Fatalf("fifo announce: %v", err)
	}
	if err := announceSpecial(t, ft, 2, "sock", os.ModeSocket|0644); err != nil {
		t.Fatalf("socket announce: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(root, "sub", "pipe"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("sub/pipe is not a FIFO: %v (err %v)", fi.Mode(), err)
	}
	fi, err = os.Lstat(filepath.Join(root, "sock"))
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("sock is not a socket: %v (err %v)", fi.Mode(), err)
	}
	// Announce-only, like a dir or symlink: no block traffic, so the entry
	// must already be finalized and never acquirable.
	assertEntryRetiredCleanly(t, ft, 1)
}

// TestFileTableAnnounceSpecialIdempotentSameType: a rerun must leave an
// existing node of the right type strictly alone -- recreating a FIFO would
// break any reader or writer currently holding it open.
func TestFileTableAnnounceSpecialIdempotentSameType(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	mkFifo(t, filepath.Join(root, "pipe"))
	before, err := os.Lstat(filepath.Join(root, "pipe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := announceSpecial(t, ft, 1, "pipe", os.ModeNamedPipe|0644); err != nil {
		t.Fatalf("announce: %v", err)
	}
	after, err := os.Lstat(filepath.Join(root, "pipe"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an existing FIFO of the right type was recreated rather than left alone")
	}
}

// TestFileTableAnnounceSpecialReplacesWrongType: a regular file, a symlink,
// or the other node type standing at the path is source-managed now and gets
// replaced -- the same contract createSymlinkAt applies.
func TestFileTableAnnounceSpecialReplacesWrongType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"regular file", func(t *testing.T, p string) { mustWriteFile(t, p, "stale") }},
		{"symlink", func(t *testing.T, p string) {
			if err := os.Symlink("elsewhere", p); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		}},
		{"socket", func(t *testing.T, p string) { mkSocket(t, p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft, root := newTestFileTable(t, 1)
			path := filepath.Join(root, "node")
			tc.setup(t, path)
			if err := announceSpecial(t, ft, 1, "node", os.ModeNamedPipe|0644); err != nil {
				t.Fatalf("announce: %v", err)
			}
			fi, err := os.Lstat(path)
			if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("node is not a FIFO: %v (err %v)", fi.Mode(), err)
			}
		})
	}
}

// TestFileTableAnnounceSpecialRefusesDirectoryAtPath mirrors the symlink
// case: a directory is never silently destroyed to plant a node.
func TestFileTableAnnounceSpecialRefusesDirectoryAtPath(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.MkdirAll(filepath.Join(root, "node", "content"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := announceSpecial(t, ft, 1, "node", os.ModeNamedPipe|0644); err != nil {
		t.Fatalf("must be an environmental skip, not an announce error: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Stat(filepath.Join(root, "node", "content")); err != nil {
		t.Errorf("the pre-existing directory was disturbed: %v", err)
	}
}

// TestFileTableAnnounceSpecialRejectsUnsyncableMode: the wire's Mode field
// carries the node type, so a record naming a type -specials does not sync
// (a device node, say) must fail that entry rather than attempt a mknod the
// walker would never have asked for.
func TestFileTableAnnounceSpecialRejectsUnsyncableMode(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := announceSpecial(t, ft, 1, "dev", os.ModeDevice|0644); err != nil {
		t.Fatalf("must fail the entry, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)
	if _, err := os.Lstat(filepath.Join(root, "dev")); !os.IsNotExist(err) {
		t.Errorf("a node was created for an unsyncable mode (lstat err = %v)", err)
	}
}

// --- platform-unrepresentable paths (a Windows destination, Linux source) ---

// TestFileTableAnnounceUnrepresentablePathFailsOneEntry: a Linux source
// legitimately holds "example.net."; a Windows destination cannot create it.
// That must cost exactly that entry -- the connection survives, the count
// still matches the client's total (so the -delete safety gate doesn't trip),
// the path stays in the keep-set, and nothing is created for it.
func TestFileTableAnnounceUnrepresentablePathFailsOneEntry(t *testing.T) {
	restore := enforceWindowsPathRules
	t.Cleanup(func() { enforceWindowsPathRules = restore })
	enforceWindowsPathRules = true

	ft, root := newTestFileTable(t, 2)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "zones/example.net.", FileSize: 10}); err != nil {
		t.Fatalf("an unrepresentable name must fail the ENTRY, not the connection: %v", err)
	}
	assertEntryDeadOnArrival(t, ft, 1)

	// A normal entry announced afterwards still works -- the table is not
	// poisoned by the failure.
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "zones/ok.txt", FileSize: 4}); err != nil {
		t.Fatalf("a later normal announce failed: %v", err)
	}
	if ft.AnnouncedCount() != 2 {
		t.Errorf("AnnouncedCount = %d, want 2 (must match the client's claimed total)", ft.AnnouncedCount())
	}
	if _, ok := ft.keep.contains("zones/example.net."); !ok {
		t.Error("the failed path is missing from the keep-set; -delete could remove a namesake")
	}
	// Nothing was created for it, and in particular Windows' silent
	// trailing-dot strip did not produce a colliding "example.net".
	if _, err := os.Lstat(filepath.Join(root, "zones", "example.net")); err == nil {
		t.Error("a trailing-dot-stripped namesake was created")
	}
}

// TestFileTableAnnounceUnsafePathStillKillsConnection: the classification
// must stay sharp -- a traversal is a corrupt stream, not a portability quirk.
func TestFileTableAnnounceUnsafePathStillKillsConnection(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "../escape.txt", FileSize: 4}); err == nil {
		t.Fatal("a traversal path must still hard-fail the announce")
	}
	if ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0", ft.AnnouncedCount())
	}
}

// --- OpFileDone arriving before its announce ---

// TestFileTableMarkDoneBeforeAnnounceStillFinalizes is the deadlock
// regression. Announces are batched, but a file whose source cannot be read
// fails instantly and its OpFileDone is sent straight away, so the done can
// overtake the announce batch still buffered on the client. MarkDone used to
// return "never announced" for it and the caller only logged that, leaving
// the file permanently unfinalized -- WaitAllFinalized then blocked until its
// timeout and failed the entire tree.
func TestFileTableMarkDoneBeforeAnnounceStillFinalizes(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)

	// The done arrives first, for a file the table has never heard of.
	if err := ft.MarkDone(7); err != nil {
		t.Fatalf("an early file-done must be remembered, not rejected: %v", err)
	}

	// Now the announce lands.
	if err := ft.Announce(AnnounceRec{FileID: 7, Kind: KindFile, Path: "late.txt", FileSize: 100}); err != nil {
		t.Fatalf("announce: %v", err)
	}

	// The replayed done reached refs==0 and finalized (then retired) the
	// entry synchronously, inside the Announce call above.
	if _, err := ft.lookup(7); !errors.Is(err, errAlreadyFinalized) {
		t.Errorf("lookup(7) = %v, want errAlreadyFinalized (the file never finalized; WaitAllFinalized would hang until its timeout)", err)
	}

	// WaitAllFinalized must return promptly rather than block.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ft.WaitAllFinalized(ctx); err != nil {
		t.Errorf("WaitAllFinalized: %v", err)
	}
}

// TestFileTableMarkDoneBeforeAnnounceIsBounded: the pending-done set must not
// grow without limit for a peer that sends dones for IDs it never announces.
func TestFileTableMarkDoneBeforeAnnounceIsBounded(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	for i := 0; i < maxPendingDone+100; i++ {
		_ = ft.MarkDone(uint32(i))
	}
	ft.mu.Lock()
	n := len(ft.pendingDone)
	ft.mu.Unlock()
	if n > maxPendingDone {
		t.Errorf("pendingDone grew to %d, past the %d cap", n, maxPendingDone)
	}
}

// TestFileTableMarkDoneBeforeAnnounceOfDirIsHarmless: an early done for an
// entry that announce finalizes on its own must not disturb it.
func TestFileTableMarkDoneBeforeAnnounceOfDirIsHarmless(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.MarkDone(3); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 3, Kind: KindDir, Path: "d"}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	assertEntryRetiredCleanly(t, ft, 3)
	if fi, statErr := os.Lstat(filepath.Join(root, "d")); statErr != nil || !fi.IsDir() {
		t.Errorf("the directory was disturbed: %v", statErr)
	}
}

// --- memory pruning: byID/announcedBits/keep-set (the point of this pass) ---

// TestLookupDistinguishesRetiredFromNeverAnnounced is the test the whole
// retirement design hinges on: a FileID that has retired (announced, then
// fully finalized -- removed from byID, but its announcedBits bit stays set
// forever) must be reported as a PERMANENT rejection (errAlreadyFinalized),
// never confused with a FileID that has genuinely never been announced,
// which is TRANSIENT and expected to resolve once a still-buffering
// announce batch lands. Confusing the two would either wedge a real
// completed transfer waiting on a done that will never come (permanent
// treated as transient) or silently drop a real file's completion signal
// (transient treated as permanent).
func TestLookupDistinguishesRetiredFromNeverAnnounced(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)

	// id 1: announce, then fully finalize it through the real Acquire/
	// MarkDone/Release path so it actually retires and leaves byID.
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a.txt", FileSize: 0}); err != nil {
		t.Fatal(err)
	}
	if err := ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}

	// id 2: never announced at all.
	const neverAnnounced = 2

	_, err1 := ft.lookup(1)
	if !errors.Is(err1, errAlreadyFinalized) {
		t.Errorf("lookup(1) [retired] = %v, want errAlreadyFinalized", err1)
	}
	_, err2 := ft.lookup(neverAnnounced)
	if errors.Is(err2, errAlreadyFinalized) {
		t.Errorf("lookup(2) [never announced] = %v, want a plain \"never announced\" error, NOT errAlreadyFinalized", err2)
	}
	if err2 == nil {
		t.Error("lookup(2) [never announced] returned nil, want an error")
	}

	// The two must also behave differently through Acquire and MarkDone:
	// Acquire on either must fail (both non-openable), but MarkDone must
	// treat them oppositely.
	if _, _, err := ft.Acquire(1); !errors.Is(err, errAlreadyFinalized) {
		t.Errorf("Acquire(1) [retired] = %v, want errAlreadyFinalized", err)
	}
	if _, _, err := ft.Acquire(neverAnnounced); errors.Is(err, errAlreadyFinalized) {
		t.Errorf("Acquire(2) [never announced] = %v, want a plain rejection, NOT errAlreadyFinalized", err)
	}

	// MarkDone(1): already retired, must be a silent no-op -- NOT stashed in
	// pendingDone (that would leak forever, since the announce that would
	// ever clear it has already happened).
	if err := ft.MarkDone(1); err != nil {
		t.Errorf("MarkDone on an already-retired entry returned an error: %v", err)
	}
	// MarkDone(2): never announced, must be remembered in pendingDone so a
	// later announce can replay it.
	if err := ft.MarkDone(neverAnnounced); err != nil {
		t.Errorf("MarkDone on a never-announced entry returned an error: %v", err)
	}
	ft.mu.Lock()
	_, pending := ft.pendingDone[neverAnnounced]
	_, retiredPending := ft.pendingDone[1]
	ft.mu.Unlock()
	if !pending {
		t.Error("MarkDone(2) [never announced] did not get remembered in pendingDone")
	}
	if retiredPending {
		t.Error("MarkDone(1) [already retired] leaked an entry into pendingDone")
	}
}

// TestByIDDropsRetiredEntries is the direct structural proof that per-entry
// state is now O(in-flight), not O(tree): after announcing a large batch of
// entries that all retire immediately (dirs, symlinks, specials, skipped
// placeholders, and quick-check skips -- exactly the shape of a
// mostly-unchanged re-sync), byID must be empty, not sized to the batch.
func TestByIDDropsRetiredEntries(t *testing.T) {
	ft, root := newTestFileTable(t, 8)
	const n = 2000
	for i := 0; i < n; i++ {
		if err := ft.Announce(AnnounceRec{
			FileID: uint32(i), Kind: KindDir, Path: fmt.Sprintf("dir%d", i), Mode: 0755,
		}); err != nil {
			t.Fatalf("announce %d: %v", i, err)
		}
	}
	if ft.AnnouncedCount() != n {
		t.Fatalf("AnnouncedCount = %d, want %d", ft.AnnouncedCount(), n)
	}
	ft.mu.Lock()
	live := len(ft.byID)
	ft.mu.Unlock()
	if live != 0 {
		t.Errorf("byID holds %d entries after %d born-finalized announces, want 0", live, n)
	}
	if !ft.allFinalized() {
		t.Error("allFinalized() is false after every announced entry retired at announce time")
	}
	_ = root
}

// TestByIDRetainsOnlyLiveFileEntries announces a mix of born-finalized
// entries and real, still-open files, and checks that byID holds exactly
// the still-open ones -- not zero (it must not over-prune something
// genuinely in flight) and not everything (it must not under-prune the
// finished ones).
func TestByIDRetainsOnlyLiveFileEntries(t *testing.T) {
	ft, _ := newTestFileTable(t, 4)
	const dirs = 50
	for i := 0; i < dirs; i++ {
		if err := ft.Announce(AnnounceRec{FileID: uint32(i), Kind: KindDir, Path: fmt.Sprintf("d%d", i), Mode: 0755}); err != nil {
			t.Fatalf("dir announce %d: %v", i, err)
		}
	}
	const files = 10
	openIDs := make([]uint32, 0, files)
	openDfs := make([]*destFile, 0, files)
	for i := 0; i < files; i++ {
		id := uint32(dirs + i)
		if err := ft.Announce(AnnounceRec{FileID: id, Kind: KindFile, Path: fmt.Sprintf("f%d.txt", i), FileSize: 10}); err != nil {
			t.Fatalf("file announce %d: %v", id, err)
		}
		df, _, err := ft.Acquire(id)
		if err != nil {
			t.Fatalf("acquire %d: %v", id, err)
		}
		openIDs = append(openIDs, id)
		openDfs = append(openDfs, df)
	}

	ft.mu.Lock()
	live := len(ft.byID)
	ft.mu.Unlock()
	if live != files {
		t.Errorf("byID holds %d entries, want exactly %d (the still-open files, none of the %d dirs)", live, files, dirs)
	}
	for _, id := range openIDs {
		if _, err := ft.lookup(id); err != nil {
			t.Errorf("lookup(%d) [still open] = %v, want success", id, err)
		}
	}

	// Finishing a file through the real Acquire/MarkDone/Release completion
	// path -- not just a born-finalized announce -- must also drop it from
	// byID. This is what catches a regression to runFinalizeIO forgetting to
	// call retire (the born-finalized path above never touches that code at
	// all, so it can't catch this on its own).
	for i, id := range openIDs {
		if err := ft.MarkDone(id); err != nil {
			t.Fatalf("MarkDone(%d): %v", id, err)
		}
		if err := ft.Release(openDfs[i]); err != nil {
			t.Fatalf("Release(%d): %v", id, err)
		}
	}
	ft.mu.Lock()
	live = len(ft.byID)
	ft.mu.Unlock()
	if live != 0 {
		t.Errorf("byID holds %d entries after every open file completed, want 0", live)
	}
	for _, id := range openIDs {
		if _, err := ft.lookup(id); !errors.Is(err, errAlreadyFinalized) {
			t.Errorf("lookup(%d) [completed] = %v, want errAlreadyFinalized", id, err)
		}
	}
}

// TestAllFinalizedAndFirstFinalizeErrAreAllocationFree pins allFinalized and
// FirstFinalizeErr as O(1): the old implementation allocated a
// []*destFile snapshot of the WHOLE table on every single call, which is
// exactly what made WaitAllFinalized's 10ms poll a livelock at tree scale.
// AllocsPerRun catches any regression back to a table scan far more
// reliably than a timing-based benchmark would.
func TestAllFinalizedAndFirstFinalizeErrAreAllocationFree(t *testing.T) {
	// Real open files, not born-finalized dirs: a born-finalized entry never
	// enters byID at all (see announce), so a table of nothing but dirs
	// would leave byID empty regardless of whether allFinalized scans it or
	// not -- exercising nothing. Live, still-open files are what a scan-based
	// allFinalized would have to walk (and allocate a snapshot slice for)
	// on every single poll.
	const n = 5000
	ft, _ := newTestFileTableWithBudget(t, 4, n)
	for i := 0; i < n; i++ {
		id := uint32(i)
		if err := ft.Announce(AnnounceRec{FileID: id, Kind: KindFile, Path: fmt.Sprintf("f%d.txt", i), FileSize: 10}); err != nil {
			t.Fatalf("announce %d: %v", i, err)
		}
		if _, _, err := ft.Acquire(id); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	ft.mu.Lock()
	live := len(ft.byID)
	ft.mu.Unlock()
	if live != n {
		t.Fatalf("byID holds %d entries, want %d -- test setup is broken", live, n)
	}

	allocs := testing.AllocsPerRun(20, func() {
		ft.allFinalized()
	})
	if allocs > 0 {
		t.Errorf("allFinalized() allocated %.1f times per call over a %d-LIVE-entry table, want 0", allocs, n)
	}
	allocs = testing.AllocsPerRun(20, func() {
		ft.FirstFinalizeErr()
	})
	if allocs > 0 {
		t.Errorf("FirstFinalizeErr() allocated %.1f times per call over a %d-LIVE-entry table, want 0", allocs, n)
	}
}

// TestFreeAnnounceScratchReleasesKeepSetAndVerifiedDirs proves the
// announce-phase-only structures actually get discarded, confining their
// memory to a transient rather than letting it ride out the rest of the
// transfer.
func TestFreeAnnounceScratchReleasesKeepSetAndVerifiedDirs(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindDir, Path: "a", Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "a/b.txt", FileSize: 1}); err != nil {
		t.Fatal(err)
	}
	ft.mu.Lock()
	hadKeep := ft.keep != nil
	hadVerifiedDirs := len(ft.verifiedDirs) > 0
	ft.mu.Unlock()
	if !hadKeep {
		t.Fatal("keep-set was nil before freeAnnounceScratch -- test setup is broken")
	}
	if !hadVerifiedDirs {
		t.Fatal("verifiedDirs is empty before freeAnnounceScratch -- test setup is broken (announcing a/b.txt should have verified 'a')")
	}

	ft.closeAnnounce()
	ft.freeAnnounceScratch()

	ft.mu.Lock()
	keepAfter := ft.keep
	verifiedDirsAfter := ft.verifiedDirs
	ft.mu.Unlock()
	if keepAfter != nil {
		t.Error("keep-set still allocated after freeAnnounceScratch")
	}
	if verifiedDirsAfter != nil {
		t.Error("verifiedDirs still allocated after freeAnnounceScratch")
	}
}

// TestAnnounceAfterCloseIsRejected proves the announceClosed gate: once the
// announce stream is closed (as OpAnnounceEnd does for every tree,
// regardless of -delete), a straggler announce must fail cleanly rather
// than silently succeed against a keep-set that freeAnnounceScratch may
// already have discarded.
func TestAnnounceAfterCloseIsRejected(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a.txt", FileSize: 1}); err != nil {
		t.Fatal(err)
	}
	ft.closeAnnounce()
	if err := ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "b.txt", FileSize: 1}); err == nil {
		t.Error("an announce after closeAnnounce unexpectedly succeeded")
	}
	if ft.AnnouncedCount() != 1 {
		t.Errorf("AnnouncedCount = %d, want 1 (the post-close announce must not register)", ft.AnnouncedCount())
	}
}

// TestAnnounceRejectsFileIDBeyondMax pins the sanity bound on a corrupt or
// hostile FileID: without it, an attacker-controlled FileID could force an
// enormous announcedBits allocation.
func TestAnnounceRejectsFileIDBeyondMax(t *testing.T) {
	ft, _ := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: maxAnnounceFileID, Kind: KindFile, Path: "x.txt", FileSize: 1}); err == nil {
		t.Error("a FileID at the maximum boundary unexpectedly succeeded")
	}
	if ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0", ft.AnnouncedCount())
	}
}

// TestDeleteExtraneousAfterFreeAnnounceScratchFailsCleanly documents that
// deleteExtraneous depends on the keep-set still being alive -- it must
// never run after freeAnnounceScratch in practice (the real handler always
// calls it before freeing), but if it ever did, it must fail loudly rather
// than silently treat an empty keep-set as "everything is extraneous" and
// wipe the destination.
func TestDeleteExtraneousAfterFreeAnnounceScratchFailsCleanly(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "keep.txt", FileSize: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	ft.closeAnnounce()
	ft.freeAnnounceScratch()

	if _, err := ft.deleteExtraneous(); err == nil {
		t.Fatal("deleteExtraneous succeeded with no keep-set -- this could wipe an entire destination")
	}
	if _, statErr := os.Stat(filepath.Join(root, "keep.txt")); statErr != nil {
		t.Errorf("keep.txt was removed despite deleteExtraneous returning an error: %v", statErr)
	}
}
