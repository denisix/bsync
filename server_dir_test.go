package main

import (
	"context"
	"os"
	"path/filepath"
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
		byPath:     make(map[string]uint32),
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
		"a\\b",
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
	df, err := ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	if !df.finalized {
		t.Error("directory entry should be finalized immediately on announce")
	}
	// A directory must never be reachable via Acquire.
	if _, _, err := ft.Acquire(1); err == nil {
		t.Error("Acquire on a directory entry unexpectedly succeeded")
	}
}

func TestFileTableAnnounceSkippedIsImmediatelyFinalized(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindSkipped, Path: "a-symlink"}); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	df, err := ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	if !df.finalized {
		t.Error("skipped entry should be finalized immediately on announce")
	}
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
	df, err := ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	if !df.finalized {
		t.Fatal("empty file not finalized after MarkDone with refs already at 0")
	}
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

func TestFileTableAnnounceRejectsSymlinkedAncestor(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	// "a" was never announced as KindDir at all -- the file is announced
	// directly under it, which is exactly the gap the ancestor check (not
	// the direct-entry check) has to catch.
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a/x.txt", FileSize: 1}); err == nil {
		t.Error("announcing a file under an existing symlinked ancestor unexpectedly succeeded")
	}
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
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a/b/x.txt", FileSize: 1}); err == nil {
		t.Error("announcing under a symlinked grandparent unexpectedly succeeded")
	}
}

func TestFileTableAnnounceParentExistsAsRegularFile(t *testing.T) {
	ft, root := newTestFileTable(t, 1)
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f/x.txt", FileSize: 1}); err == nil {
		t.Error("announcing a file under a path that already exists as a regular file unexpectedly succeeded")
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
// is a regression test for F8: allFinalized must check
// finalizeComplete, not finalized. finalized flips BEFORE finalizeIO's
// actual drain/truncate/close/chmod/chtimes work runs -- by design, to
// close the Acquire-exclusion TOCTOU window (see Release's comment) -- so a
// WaitAllFinalized satisfied by finalized alone could return while some
// other file's finalizeIO call is genuinely still in flight elsewhere, which
// is exactly what raced against ow.Close() in the real e2e failures this
// fix addresses. finalizeIO's real duration is too short to race reliably
// in a unit test, so the "still in flight" state is set up directly here.
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
	// ...but finalizeComplete is deliberately left false: finalizeIO has
	// not actually run/returned yet, as if it were still in flight on
	// another goroutine.

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := ft.WaitAllFinalized(ctx); err == nil {
		t.Fatal("WaitAllFinalized returned nil while finalizeComplete was still false")
	}

	df.mu.Lock()
	df.finalizeComplete = true
	df.mu.Unlock()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := ft.WaitAllFinalized(ctx2); err != nil {
		t.Fatalf("WaitAllFinalized after finalizeComplete flipped: %v", err)
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
