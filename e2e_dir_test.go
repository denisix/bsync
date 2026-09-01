package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// runDirTransfer starts a directory-mode server on a free loopback port,
// runs startDirClient against it, and returns the destination root and the
// client's SyncSession once the transfer completes (or fails). A thin
// wrapper over runDirTransferOpts with quickCheck=false, matching every one
// of its many existing callers.
func runDirTransfer(t *testing.T, srcRoot string, workers int, noCompress bool) (string, *SyncSession) {
	t.Helper()
	return runDirTransferOpts(t, srcRoot, workers, noCompress, false)
}

// runDirTransferOpts is runDirTransfer with explicit control over
// quickCheck, for tests that need it enabled.
func runDirTransferOpts(t *testing.T, srcRoot string, workers int, noCompress, quickCheck bool) (string, *SyncSession) {
	t.Helper()
	resetEngineGlobals()
	dstRoot := t.TempDir()
	// t.TempDir() already creates the directory; directory mode should be
	// fine either creating or reusing an existing empty root.

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port

	srvErrCh := make(chan error, 1)
	go func() {
		srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, workers, true)
	}()
	waitForListener(t, addr)

	sess, err := startDirClient(srcRoot, addr, 0, noCompress, workers, nil, quickCheck, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}

	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit after tree done")
	}

	if sess.FailedCount() != 0 {
		t.Fatalf("%d file(s) failed: first error for some file is queried via FailedError", sess.FailedCount())
	}
	assertProgressReachedTotal(t, sess)
	return dstRoot, sess
}

// assertProgressReachedTotal pins the progress display's completion
// invariants on every successful helper-driven sync: the files-done counter
// must exactly reach the files-only denominator (totalFilesKnown -- KindFile
// entries; dirs and skipped placeholders are excluded from both sides), so
// a real transfer's final progress line reads N/N rather than stalling
// short of it; and the pre-scan estimator (startDirClient's delayed
// estimateTree goroutine), IF it ran at all, must have fed the session's
// estimate atomics with the same totals -- the source trees in these tests
// are stable, so estimate and exact must converge. The estimator starts
// only after estimateDelay and is skipped entirely when the walk finishes
// first, which is the overwhelmingly common case for these small test
// trees, so a zero estimate is legitimate; a NONZERO estimate that never
// converges is the regression this still nets. The estimate side is polled
// briefly: the pre-scan is deliberately fire-and-forget, so it can still be
// mid-sweep for a moment after startDirClient returns.
// (estimateTree's own counting contract is pinned directly by
// TestEstimateTreeMatchesWalkSource in walk_test.go.)
func assertProgressReachedTotal(t *testing.T, sess *SyncSession) {
	t.Helper()
	done, total := sess.filesDone.Load(), sess.totalFilesKnown.Load()
	if done != total {
		t.Errorf("filesDone = %d, totalFilesKnown = %d -- a successful sync must end at N/N", done, total)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		est := sess.estFiles.Load()
		if est == 0 {
			return // estimator skipped (walk beat estimateDelay) -- exact totals rule
		}
		if est == total && sess.estBytes.Load() == sess.totalBytesKnown.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("pre-scan estimate = %d files / %d bytes, want %d / %d -- estimateTree's session feed is broken",
		sess.estFiles.Load(), sess.estBytes.Load(), total, sess.totalBytesKnown.Load())
}

// TestE2EDirEstimatorActuallyRunsAndConvergesWhenStarted is the direct
// regression assertProgressReachedTotal can no longer provide on its own:
// that helper now accepts estFiles==0 as success, because on every real
// test tree the walk finishes before estimateDelay and the estimator never
// launches at all -- so it can't tell "estimator skipped, correctly" apart
// from "estimator's launch-and-feed wiring is broken". This test forces
// the estimator to actually run (estimateDelay set to 0, so it races
// walkDone from the very first select) and then requires its atomics to
// reach the exact real totals -- exercising the launch goroutine at
// startDirClient's callsite and its sess.estFiles/estBytes.Add wiring,
// which TestEstimateTreeMatchesWalkSource (counting only, no session) and
// the resolveProgressTotals unit tests (consumption only, atomics
// pre-seeded by hand) do not reach.
func TestE2EDirEstimatorActuallyRunsAndConvergesWhenStarted(t *testing.T) {
	restore := estimateDelay
	t.Cleanup(func() { estimateDelay = restore })
	estimateDelay = 0

	resetEngineGlobals()
	src := t.TempDir()
	const nFiles = 200
	var wantBytes uint64
	for i := 0; i < nFiles; i++ {
		content := string(randomBytes(50+i, int64(i)))
		mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%03d.bin", i)), content)
		wantBytes += uint64(len(content))
	}
	dst := t.TempDir()

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, false, 2, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d file(s) failed", sess.FailedCount())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess.estFiles.Load() == uint32(nFiles) && sess.estBytes.Load() == wantBytes {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("pre-scan estimate = %d files / %d bytes, want %d / %d -- with estimateDelay=0 the estimator "+
		"must run and its session feed must converge exactly (launch-and-feed wiring regression)",
		sess.estFiles.Load(), sess.estBytes.Load(), nFiles, wantBytes)
}

// startDirClientAgainst re-runs startDirClient against an EXISTING
// destination root (unlike runDirTransfer/runDirTransferOpts, which always
// create a fresh one), by starting a fresh server bound to dstRoot on a new
// port. Used by the quick-check and -delete tests below to drive a
// second/third sync pass against the same destination, matching the pattern
// TestE2EDirIdempotentSecondRun already uses by hand. A failed file aborts
// the test via t.Fatalf, so it can't be used for a run that's expected to
// leave FailedCount() > 0.
// syncSpecials is variadic purely so the many existing call sites -- none of
// which care about -specials -- stay untouched; pass a single true to enable
// it.
func startDirClientAgainst(t *testing.T, srcRoot, dstRoot string, workers int, noCompress bool, excl []string, quickCheck, deleteExtra, syncLinks bool, syncSpecials ...bool) *SyncSession {
	t.Helper()
	resetEngineGlobals()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, workers, true) }()
	waitForListener(t, addr)

	specials := len(syncSpecials) > 0 && syncSpecials[0]
	sess, err := startDirClient(srcRoot, addr, 0, noCompress, workers, excl, quickCheck, deleteExtra, syncLinks, specials)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d file(s) failed", sess.FailedCount())
	}
	assertProgressReachedTotal(t, sess)
	return sess
}

// snapshotMTimes records the current mtime of every regular file under
// root, keyed by its path relative to root -- for asserting a later run
// left the destination's timestamps untouched.
func snapshotMTimes(t *testing.T, root string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	for _, rel := range listTreeFiles(t, root) {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = info.ModTime()
	}
	return out
}

// listTreeFiles returns every regular file's path (relative to root) found
// under root, sorted.
func listTreeFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// treeEntry is one path's full identity for comparison: enough to catch a
// wrong TYPE, wrong content, wrong link target, or wrong permissions -- not
// just "some bytes at some path".
type treeEntry struct {
	kind  string // file | dir | symlink | fifo | socket | device | other
	size  int64  // regular files only
	sum   string // sha256 of content, regular files only
	link  string // symlink target, verbatim (never followed)
	perm  os.FileMode
	mtime int64 // unix SECONDS; regular files only (see snapshotTree)
}

func (e treeEntry) String() string {
	switch e.kind {
	case "file":
		return fmt.Sprintf("file(%d bytes, sha256:%s, mode %o, mtime %d)", e.size, e.sum[:12], e.perm, e.mtime)
	case "symlink":
		return fmt.Sprintf("symlink(-> %q)", e.link)
	case "dir":
		return "dir"
	default:
		return e.kind
	}
}

// hashFileContent streams a file through SHA-256 rather than reading it whole:
// the comparison must work on files far larger than memory, and streaming is
// what makes this usable as the general equality check for every e2e test.
//
// Deliberately crypto/sha256 directly, NOT this codebase's own checksum():
// that is the hash the transfer itself compares blocks with, so verifying the
// result with it would let a bug in the transfer hash agree with itself.
func hashFileContent(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// snapshotTree records every entry under root, keyed by relative path.
//
// Lstat semantics throughout: a symlink is recorded as a symlink with its
// verbatim target and is NEVER followed, so a link recreated pointing
// somewhere else cannot pass by virtue of the target's bytes matching. A FIFO
// or socket is recorded by type alone and never opened -- opening a FIFO
// blocks forever, which is why the old content-reading comparison could not
// be used by the -specials tests at all.
func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		info, infoErr := d.Info() // Lstat semantics: never follows a symlink
		if infoErr != nil {
			return fmt.Errorf("lstat %s: %w", rel, infoErr)
		}
		e := treeEntry{perm: info.Mode().Perm()}
		switch mode := info.Mode(); {
		case mode&os.ModeSymlink != 0:
			e.kind = "symlink"
			target, rlErr := os.Readlink(path)
			if rlErr != nil {
				return fmt.Errorf("readlink %s: %w", rel, rlErr)
			}
			e.link = target
		case mode.IsDir():
			e.kind = "dir"
			// Permissions and mtime are deliberately NOT recorded for
			// directories: the destination creates them with
			// MkdirAll(0777)&umask and never chmods or chtimes them, so
			// they legitimately differ from the source today. That gap is
			// pinned explicitly by
			// TestE2EDirDirectoryModeIsNotPreserved rather than left to
			// fail every test that uses this helper.
			e.perm = 0
		case mode&os.ModeNamedPipe != 0:
			e.kind = "fifo"
		case mode&os.ModeSocket != 0:
			e.kind = "socket"
		case mode&os.ModeDevice != 0:
			e.kind = "device"
		case mode.IsRegular():
			e.kind = "file"
			e.size = info.Size()
			e.sum = hashFileContent(t, path)
			// Seconds, matching the precision quick-check itself compares
			// at -- finer granularity would make this flake on filesystems
			// with coarse timestamps.
			e.mtime = info.ModTime().Unix()
		default:
			e.kind = "other"
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// assertTreesEqual verifies dst is a faithful reproduction of src: the same
// set of paths, each with the same TYPE, and for regular files the same size,
// content (by SHA-256), permission bits and mtime. Symlinks are compared by
// target, special files by type.
//
// This is the general "did the transfer actually work" assertion for the e2e
// suite. It replaced a version that compared only a path set plus
// os.ReadFile content, which followed symlinks (so a wrong link target
// passed), ignored directories entirely, and would hang on a FIFO.
func assertTreesEqual(t *testing.T, src, dst string) {
	t.Helper()
	for _, problem := range diffTrees(t, src, dst) {
		t.Error(problem)
	}
}

// diffTrees is assertTreesEqual's comparison, returning what differs instead
// of reporting it. Split out so the comparison itself can be unit-tested --
// a test helper this many tests depend on is worth proving actually catches
// the things it claims to (see TestAssertTreesEqualCatchesRealDifferences).
func diffTrees(t *testing.T, src, dst string) []string {
	t.Helper()
	var problems []string
	report := func(format string, args ...interface{}) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	want := snapshotTree(t, src)
	got := snapshotTree(t, dst)

	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			report("%s: missing from destination (source has %s)", rel, w)
			continue
		}
		if g.kind != w.kind {
			report("%s: type is %s at destination, want %s", rel, g.kind, w.kind)
			continue
		}
		switch w.kind {
		case "file":
			if g.sum != w.sum || g.size != w.size {
				report("%s: content differs -- source %d bytes sha256:%s, destination %d bytes sha256:%s",
					rel, w.size, w.sum[:12], g.size, g.sum[:12])
			}
			if g.perm != w.perm {
				report("%s: mode %o at destination, want %o", rel, g.perm, w.perm)
			}
			if g.mtime != w.mtime {
				report("%s: mtime %d at destination, want %d", rel, g.mtime, w.mtime)
			}
		case "symlink":
			if g.link != w.link {
				report("%s: link target %q at destination, want %q (verbatim)", rel, g.link, w.link)
			}
		}
	}
	for rel, g := range got {
		if _, ok := want[rel]; !ok {
			report("%s: present at destination (%s) but not in the source", rel, g)
		}
	}
	sort.Strings(problems)
	return problems
}

func TestE2EDirBasicNestedTree(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "a", "b"))
	mustMkdirAll(t, filepath.Join(src, "c"))
	mustWriteFile(t, filepath.Join(src, "top.txt"), "top level")
	mustWriteFile(t, filepath.Join(src, "a", "one.txt"), "in a")
	mustWriteFile(t, filepath.Join(src, "a", "b", "two.txt"), "in a/b")
	mustWriteFile(t, filepath.Join(src, "c", "three.txt"), "in c")

	dst, _ := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)
}

func TestE2EDirEmptyFiles(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "empty.txt"), "")
	mustWriteFile(t, filepath.Join(src, "nonempty.txt"), "x")

	dst, sess := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)

	info, err := os.Stat(filepath.Join(dst, "empty.txt"))
	if err != nil {
		t.Fatalf("stat empty.txt: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("empty.txt size = %d, want 0", info.Size())
	}
	if sess.filesDone.Load() < 2 {
		t.Errorf("filesDone = %d, want at least 2", sess.filesDone.Load())
	}
}

func TestE2EDirEmptyDirsPreserved(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "empty1"))
	mustMkdirAll(t, filepath.Join(src, "nested", "empty2"))
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")

	dst, _ := runDirTransfer(t, src, 1, false)
	for _, rel := range []string{"empty1", "nested", "nested/empty2"} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("stat %s: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", rel)
		}
	}
}

func TestE2EDirMultiWorker(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 20; i++ {
		mustWriteFile(t, filepath.Join(src, "f"+string(rune('a'+i))+".txt"), string(randomBytes(4096, int64(i))))
	}
	dst, _ := runDirTransfer(t, src, 4, false)
	assertTreesEqual(t, src, dst)
}

func TestE2EDirLargeFileMultiBlock(t *testing.T) {
	src := t.TempDir()
	// treeBlockSize caps at the package default (10MB) unless smaller;
	// force a small cap so this file spans several blocks without the test
	// needing megabytes of data. blockSize is the package global read by
	// treeBlockSize when the cap passed to startDirClient is 0.
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	data := randomBytes(4096*5+123, 60) // 5 full blocks + a partial one
	mustWriteFile(t, filepath.Join(src, "big.bin"), string(data))

	dst, sess := runDirTransfer(t, src, 2, false)
	assertTreesEqual(t, src, dst)
	if sess.blocksDone.Load() < 6 {
		t.Errorf("blocksDone = %d, want at least 6", sess.blocksDone.Load())
	}
}

func TestE2EDirZeroFilledFile(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	mustWriteFile(t, filepath.Join(src, "zeros.bin"), string(make([]byte, 4096*3)))
	dst, _ := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)
}

func TestE2EDirSymlinkNotCreatedAtDestination(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "real.txt"), "x")
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dst, _ := runDirTransfer(t, src, 1, false)
	if _, err := os.Lstat(filepath.Join(dst, "link.txt")); err == nil {
		t.Error("symlink was created at the destination")
	}
	if _, err := os.Stat(filepath.Join(dst, "real.txt")); err != nil {
		t.Errorf("real.txt missing at destination: %v", err)
	}
}

func TestE2EDirModeAndMTimePreserved(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "f.txt")
	mustWriteFile(t, path, "x")
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	dst, _ := runDirTransfer(t, src, 1, false)
	info, err := os.Stat(filepath.Join(dst, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("mode = %o, want 0640", info.Mode().Perm())
	}
	if diff := info.ModTime().Sub(mtime); diff > 2*time.Second || diff < -2*time.Second {
		t.Errorf("mtime = %v, want ~%v (diff %v)", info.ModTime(), mtime, diff)
	}
}

func TestE2EDirNoCompress(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), string(compressibleBytes(8192)))
	dst, _ := runDirTransfer(t, src, 1, true)
	assertTreesEqual(t, src, dst)
}

func TestE2EDirIdempotentSecondRun(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(9000, 61)))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), string(randomBytes(500, 62)))

	dst, sess1 := runDirTransfer(t, src, 2, false)
	assertTreesEqual(t, src, dst)
	if sess1.bytesNet.Load() == 0 {
		t.Fatal("first run transferred zero bytes -- test fixture problem, not idempotence")
	}

	// Re-run against the SAME destination: a fresh temp dir won't do here,
	// so drive the server/client pair directly against dst instead of
	// through runDirTransfer's fresh t.TempDir().
	resetEngineGlobals()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	// Snapshot mtimes before the second run.
	before := map[string]time.Time{}
	for _, rel := range listTreeFiles(t, dst) {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = info.ModTime()
	}

	sess2, err := startDirClient(src, addr, 0, false, 2, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("second startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("second startDirServer: %v", err)
	}
	if sess2.FailedCount() != 0 {
		t.Fatalf("second run had %d failed file(s)", sess2.FailedCount())
	}

	if got := sess2.bytesNet.Load(); got != 0 {
		t.Errorf("second run's payload bytes = %d, want 0 (nothing should have changed)", got)
	}
	if got := sess2.diffs.Load(); got != 0 {
		t.Errorf("second run's diffs = %d, want 0", got)
	}

	assertTreesEqual(t, src, dst)
	for _, rel := range listTreeFiles(t, dst) {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(before[rel]) {
			t.Errorf("%s: mtime changed on a no-op second run: was %v, now %v", rel, before[rel], info.ModTime())
		}
	}
}

func TestE2EDirDestinationRootCreatedIfMissing(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")

	resetEngineGlobals()
	parent := t.TempDir()
	dstRoot := filepath.Join(parent, "does", "not", "exist", "yet")

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, false, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dstRoot)
}

// faultyProxy sits between a directory-mode client and the real server. It
// lets skipFirst connections through untouched (so the control connection,
// always dialed first, survives) and then kills each of the next
// killConns connections partway through, once killAfterBytes have crossed
// in either direction -- forcing a transfer worker's AutoReconnectTCP to
// see a real failure and redial through the SAME proxy, which the real
// server sees as a genuinely new TCP connection. This is what exercises
// dirWorkerBind's soft-binding rebind path and the server's
// stale/duplicate-Acquire tolerance, not just the happy path every other
// test in this file takes.
type faultyProxy struct {
	listener       net.Listener
	target         string
	skipFirst      int
	killConns      int
	killAfterBytes int64
	// delayPerChunk, if nonzero, sleeps this long after every ≤512-byte
	// chunk pumped in either direction on every connection -- a
	// deterministic way to make a transfer take real, controllable
	// wall-clock time regardless of how fast local loopback and
	// CPU-bound hashing/compression actually are, for tests that need a
	// reliable window to act in while a transfer is still in progress
	// (see TestE2EDirSourceFileDisappearsAfterWalkStillCompletes).
	delayPerChunk time.Duration

	mu    sync.Mutex
	seen  int
	total atomic64 // cumulative bytes proxied in either direction, across all connections
}

func startFaultyProxy(t *testing.T, target string, skipFirst, killConns int, killAfterBytes int64) (string, *faultyProxy) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultyProxy{listener: l, target: target, skipFirst: skipFirst, killConns: killConns, killAfterBytes: killAfterBytes}
	go p.run()
	t.Cleanup(func() { l.Close() })
	return l.Addr().String(), p
}

// Total returns the cumulative bytes proxied in either direction across
// every connection so far -- a way to measure how much a client/server pair
// actually put on the wire without instrumenting either of them directly.
func (p *faultyProxy) Total() int64 {
	return p.total.add(0)
}

func (p *faultyProxy) run() {
	for {
		c, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.seen++
		n := p.seen
		p.mu.Unlock()
		kill := n > p.skipFirst && n <= p.skipFirst+p.killConns
		go p.handle(c, kill)
	}
}

func (p *faultyProxy) handle(client net.Conn, kill bool) {
	defer client.Close()
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer upstream.Close()

	killed := make(chan struct{})
	var closeOnce sync.Once
	var total atomic64

	pump := func(dst, src net.Conn) {
		buf := make([]byte, 512)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
				p.total.add(int64(n))
				if p.delayPerChunk > 0 {
					time.Sleep(p.delayPerChunk)
				}
				if kill && total.add(int64(n)) > p.killAfterBytes {
					closeOnce.Do(func() { close(killed) })
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	done := make(chan struct{}, 2)
	go func() { pump(upstream, client); done <- struct{}{} }()
	go func() { pump(client, upstream); done <- struct{}{} }()

	select {
	case <-killed:
	case <-done:
	}
}

// atomic64 is a tiny mutex-guarded counter; avoids importing sync/atomic's
// typed wrappers just for one int64 in a test file.
type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) add(d int64) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n += d
	return a.n
}

func TestE2EDirSurvivesMidTransferConnectionDrops(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	for i := 0; i < 12; i++ {
		mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%02d.bin", i)), string(randomBytes(2000, int64(200+i))))
	}
	dst := t.TempDir()

	port := freeTCPPort(t)
	realAddr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, realAddr)

	// skipFirst=1 spares the control connection (always dialed first); the
	// next 2 connections (worker redials) each get killed after a small
	// number of bytes, forcing repeated OpFileOpen rebinds. maxRetries is
	// 3, so this leaves the single worker's 3rd attempt (its 3rd dial
	// overall) to succeed -- killing 3 would exhaust the whole retry
	// budget on the very first block before any redial could pay off.
	proxyAddr, _ := startFaultyProxy(t, realAddr, 1, 2, 200)

	sess, err := startDirClient(src, proxyAddr, 0, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient did not recover from repeated connection drops: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d file(s) failed despite retry budget", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

// --- negative / boundary cases ---

func TestStartDirClientRejectsZeroWorkers(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	if _, err := startDirClient(src, "127.0.0.1:1", 0, false, 0, nil, false, false, false, false); err == nil {
		t.Fatal("startDirClient with workers=0 unexpectedly succeeded")
	}
}

func TestStartDirClientFailsFastOnConnectionRefused(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	// A free port with nothing listening: dial should fail quickly, not
	// hang for the full dialTimeout.
	port := freeTCPPort(t)
	start := time.Now()
	_, err := startDirClient(src, "127.0.0.1:"+port, 0, false, 1, nil, false, false, false, false)
	if err == nil {
		t.Fatal("startDirClient against a closed port unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("connection-refused failure took %v, expected a fast failure", elapsed)
	}
}

func TestStartDirClientRejectsUnresolvableAddress(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")
	if _, err := startDirClient(src, "this-host-does-not-resolve.invalid:9999", 0, false, 1, nil, false, false, false, false); err == nil {
		t.Fatal("startDirClient with an unresolvable address unexpectedly succeeded")
	}
}

func TestStartDirServerRejectsRootThatIsAFile(t *testing.T) {
	resetEngineGlobals()
	notADir := writeTempFile(t, []byte("x"))
	port := freeTCPPort(t)
	err := startDirServer(notADir, "127.0.0.1", port, 1, true)
	if err == nil {
		t.Fatal("startDirServer with a destination root that is a regular file unexpectedly succeeded")
	}
}

// TestE2EDirManyWorkersOneLargeFile forces heavy concurrent contention on a
// SINGLE destFile: many workers pulling blocks of the same file mean many
// concurrent Acquire calls holding overlapping refs, which is exactly the
// scenario the double-latch (sawDone && refs==0) exists to get right --
// finalizing too early here would truncate/chmod/chtimes a file while
// another worker is still mid-write.
func TestE2EDirManyWorkersOneLargeFile(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	data := randomBytes(4096*40+17, 70) // 41 blocks, one worker per few blocks
	mustWriteFile(t, filepath.Join(src, "huge.bin"), string(data))

	dst, sess := runDirTransfer(t, src, 8, false)
	assertTreesEqual(t, src, dst)
	if sess.blocksDone.Load() < 41 {
		t.Errorf("blocksDone = %d, want at least 41", sess.blocksDone.Load())
	}
}

// TestE2EDirLargeFileStillSplitsAcrossWorkers is TestE2EDirManyWorkersOneLargeFile's
// exact shape, plus the routing-specific assertion that pins down WHY it
// still exercises multi-connection cooperation on one destFile after
// affinityWorker (checksum.go) was introduced: a 41-block file with 8
// workers has Blocks(41) > workers(8), so it provably takes the shared/FCFS
// path, never a dedicated one. TestE2EDirManyWorkersOneLargeFile itself is
// intentionally left untouched by the affinity change -- this test exists
// so that fact is asserted directly instead of merely hoped for.
func TestE2EDirLargeFileStillSplitsAcrossWorkers(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	data := randomBytes(4096*40+17, 71) // 41 blocks
	mustWriteFile(t, filepath.Join(src, "huge.bin"), string(data))

	dst, sess := runDirTransfer(t, src, 8, false)
	assertTreesEqual(t, src, dst)
	if sess.blocksDone.Load() < 41 {
		t.Errorf("blocksDone = %d, want at least 41", sess.blocksDone.Load())
	}

	// The transfer has already completed by the time assertTreesEqual
	// returns, so FileID 0's session entry has been released (see
	// SyncSession.Release/claimDone) -- its Blocks count is computed here
	// directly from the same fixture parameters walk.go's treeBlockSize
	// would have used, rather than read back from the session.
	wantBlocks := uint32((uint64(len(data)) + uint64(blockSize) - 1) / uint64(blockSize))
	if wantBlocks != 41 {
		t.Fatalf("huge.bin has %d blocks, want 41 (test fixture assumption broken)", wantBlocks)
	}
	if w := affinityWorker(0, wantBlocks, 8); w != -1 {
		t.Errorf("affinityWorker(0, %d, 8) = %d, want -1 (shared path) -- a 41-block file must not be routed to a single dedicated worker", wantBlocks, w)
	}
}

// TestE2EDirLargeTreeManyFiles is a broader stress/scale sanity check: a
// few hundred small files across nested directories, multiple workers.
func TestE2EDirLargeTreeManyFiles(t *testing.T) {
	src := t.TempDir()
	const nDirs = 5
	const filesPerDir = 40
	for d := 0; d < nDirs; d++ {
		dir := filepath.Join(src, fmt.Sprintf("dir%02d", d))
		mustMkdirAll(t, dir)
		for f := 0; f < filesPerDir; f++ {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), string(randomBytes(200, int64(d*1000+f))))
		}
	}
	dst, sess := runDirTransfer(t, src, 4, false)
	assertTreesEqual(t, src, dst)
	if want := nDirs * filesPerDir; int(sess.filesDone.Load()) != want {
		t.Errorf("filesDone = %d, want %d", sess.filesDone.Load(), want)
	}
}

func TestDirHandleConnAnnounceRejectsTruncatedTrailer(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	trailer, err := encodeAnnounceBatch([]AnnounceRec{{FileID: 1, Kind: KindFile, Path: "a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	// Claim a trailer of the real length but only send half of it.
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounce), DataSize: uint32(len(trailer))})
	if _, err := client.Write(trailer[:len(trailer)/2]); err != nil {
		t.Fatal(err)
	}
	client.Close() // the short write plus close simulates a truncated/dropped stream
	expectHandlerError(t, errCh, "")
}

func TestDirHandleConnFileCloseForUnboundFileIsHarmless(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	// OpFileClose for a file this connection never opened must be a no-op,
	// not a crash or an error -- a legitimate client can race one against
	// a redial.
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileClose), FileID: 1})
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounceEnd), BlockIdx: 1})
	client.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dirHandleConn did not return")
	}
}

// TestE2EDirPartiallyModifiedDestination is the core value proposition of
// block-level diffing, exercised end to end: after a first full sync,
// change only ONE block of a multi-block source file and re-sync. The
// destination must end up byte-identical to the new source, and the second
// run's payload bytes must be far below the whole file's size -- proving
// only the differing block actually moved, not the whole file.
func TestE2EDirPartiallyModifiedDestination(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	data := randomBytes(4096*5, 91) // 5 full blocks
	mustWriteFile(t, filepath.Join(src, "big.bin"), string(data))

	dst, sess1 := runDirTransfer(t, src, 2, true)
	assertTreesEqual(t, src, dst)
	if sess1.bytesNet.Load() == 0 {
		t.Fatal("first run transferred zero bytes -- fixture problem, not the behavior under test")
	}

	// Mutate only the MIDDLE block (block 2 of 5) in the source.
	modified := append([]byte(nil), data...)
	copy(modified[2*4096:3*4096], randomBytes(4096, 92))
	if err := os.WriteFile(filepath.Join(src, "big.bin"), modified, 0644); err != nil {
		t.Fatal(err)
	}

	resetEngineGlobals()
	blockSize = 4096
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	sess2, err := startDirClient(src, addr, 0, true, 2, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("second startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("second startDirServer: %v", err)
	}
	if sess2.FailedCount() != 0 {
		t.Fatalf("second run had %d failed file(s)", sess2.FailedCount())
	}

	got, err := os.ReadFile(filepath.Join(dst, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, modified) {
		t.Error("destination content does not match the modified source after re-sync")
	}
	if got := sess2.bytesNet.Load(); got == 0 || got > uint64(len(data))/2 {
		t.Errorf("second run's payload bytes = %d, want roughly one block's worth (< %d)", got, len(data)/2)
	}
}

func TestE2EDirDestinationLongerThanSourceIsTruncated(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "short")

	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "f.txt"), []byte("this destination content is much longer than the source"), 0644); err != nil {
		t.Fatal(err)
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

func TestE2EDirDestinationShorterIsExtended(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "a considerably longer source file than the destination")

	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "f.txt"), []byte("tiny"), 0644); err != nil {
		t.Fatal(err)
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

// TestE2EDirEncrypted is the directory path's only end-to-end -e coverage.
// Its absence is exactly how the crypto-state leak (setupTransfer's
// t.Cleanup only restoring blockSize, not encryptionKey) stayed hidden
// during earlier development of this phase.
func TestE2EDirEncrypted(t *testing.T) {
	resetEngineGlobals()
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	t.Cleanup(func() {
		encryptionKey = nil
		aeadOnce = sync.Once{}
	})

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "secret.txt"), string(randomBytes(9000, 90)))
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), "small")

	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, false, 2, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

func TestE2EDirUnicodeSpaceAndNewlineNames(t *testing.T) {
	src := t.TempDir()
	names := []string{
		"héllo wörld.txt",
		"space name.txt",
		"semi;colon.txt",
		"emoji-😀-file.txt",
		// A literal newline in a filename is legal on Linux (only NUL and
		// '/' are forbidden); this is specifically what would exercise
		// shellQuote/remoteBinaryCommand over a real SSH invocation, and
		// the most likely of this group to trip something up.
		"new\nline.txt",
	}
	for i, name := range names {
		if err := os.WriteFile(filepath.Join(src, name), []byte(fmt.Sprintf("content %d", i)), 0644); err != nil {
			t.Skipf("filesystem does not support name %q: %v", name, err)
		}
	}
	dst, _ := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)
}

func TestE2EDirDeepNesting(t *testing.T) {
	src := t.TempDir()
	dir := src
	for i := 0; i < 20; i++ {
		dir = filepath.Join(dir, fmt.Sprintf("level%02d", i))
	}
	mustMkdirAll(t, dir)
	mustWriteFile(t, filepath.Join(dir, "deep.txt"), "buried")
	dst, _ := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)
}

// TestE2EDirIdempotentSecondRunSendsFarFewerBytes closes the two idempotence
// assertions TestE2EDirIdempotentSecondRun does not make: an actual wire-byte
// comparison (via a pure-passthrough faultyProxy configured to count instead
// of kill) and unchanged destination mtimes after the second run.
func TestE2EDirIdempotentSecondRunSendsFarFewerBytes(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(20000, 93)))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), string(randomBytes(5000, 94)))
	dst := t.TempDir()

	runOnce := func() int64 {
		port := freeTCPPort(t)
		realAddr := "127.0.0.1:" + port
		srvErrCh := make(chan error, 1)
		go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
		waitForListener(t, realAddr)
		proxyAddr, proxy := startFaultyProxy(t, realAddr, 0, 0, 0) // pure counting passthrough

		sess, err := startDirClient(src, proxyAddr, 0, true, 2, nil, false, false, false, false)
		if err != nil {
			t.Fatalf("startDirClient: %v", err)
		}
		if err := <-srvErrCh; err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
		if sess.FailedCount() != 0 {
			t.Fatalf("%d failed file(s)", sess.FailedCount())
		}
		return proxy.Total()
	}

	firstBytes := runOnce()
	assertTreesEqual(t, src, dst)
	if firstBytes == 0 {
		t.Fatal("first run proxied zero bytes -- fixture problem, not idempotence")
	}

	before := map[string]time.Time{}
	for _, rel := range listTreeFiles(t, dst) {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = info.ModTime()
	}

	resetEngineGlobals()
	secondBytes := runOnce()

	if secondBytes >= firstBytes/2 {
		t.Errorf("second run proxied %d bytes, want far fewer than the first run's %d (nothing changed)", secondBytes, firstBytes)
	}
	assertTreesEqual(t, src, dst)
	for _, rel := range listTreeFiles(t, dst) {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(before[rel]) {
			t.Errorf("%s: mtime changed on a no-op second run: was %v, now %v", rel, before[rel], info.ModTime())
		}
	}
}

// TestE2EDirUnreadableSourceFileIsSkippedNotFailed pins a real, previously
// undocumented behavior: walkSource opens each regular file itself (to
// capture size/mode/mtime consistently off the same fd it will later read
// blocks from -- see walk.go), so a permission-denied source file fails AT
// WALK TIME via walkSource's own skip() path, not later via
// TreeReader/failFile. It is therefore never announced to the destination
// at all -- silently absent from the sync -- rather than counted as a
// session failure. Worth asserting explicitly since it's easy to assume the
// opposite (that every unreadable file shows up in FailedCount).
// startDirClientExpectingFailures is startDirClientAgainst for runs where
// individual files are SUPPOSED to fail: it returns the session and the
// server's result instead of calling t.Fatal on them. A per-file failure
// legitimately surfaces on both ends -- the client fails the file, and the
// server reports that the destination copy is not correct -- so a test about
// that behavior cannot use a helper that treats either as fatal.
func startDirClientExpectingFailures(t *testing.T, srcRoot, dstRoot string, workers int, quickCheck, deleteExtra bool) (*SyncSession, error) {
	t.Helper()
	resetEngineGlobals()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, workers, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(srcRoot, addr, 0, false, workers, nil, quickCheck, deleteExtra, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	return sess, <-srvErrCh
}

// TestE2EDirUnreadableSourceFileIsReportedNotFabricated pins what happens to
// a source file that cannot be read. It is ANNOUNCED (so it stays in the
// -delete keep-set and any existing destination copy is protected), it is
// REPORTED as failed rather than silently omitted, and above all no file is
// fabricated for it at the destination: nothing was transferred, so nothing
// about its content is known.
func TestE2EDirUnreadableSourceFileIsReportedNotFabricated(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "good1.txt"), "one")
	badPath := filepath.Join(src, "unreadable.txt")
	mustWriteFile(t, badPath, "will not be readable")
	mustWriteFile(t, filepath.Join(src, "good2.txt"), "two")

	if err := os.Chmod(badPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(badPath, 0644) }) // let t.TempDir's cleanup remove it

	dst := t.TempDir()
	sess, srvErr := startDirClientExpectingFailures(t, src, dst, 2, false, false)
	// The server reports the file as not correct at the destination; that is
	// the point, not an error in the test.
	if srvErr == nil {
		t.Error("the server reported success for a file it never received any data for")
	}
	if sess.FailedCount() != 1 {
		t.Errorf("FailedCount = %d, want 1 -- an unreadable file must be reported, not silently omitted from a backup", sess.FailedCount())
	}
	for _, rel := range []string{"good1.txt", "good2.txt"} {
		got, rerr := os.ReadFile(filepath.Join(dst, rel))
		if rerr != nil {
			t.Errorf("%s missing at destination: %v", rel, rerr)
			continue
		}
		want, _ := os.ReadFile(filepath.Join(src, rel))
		if string(got) != string(want) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	// The critical part: no zero-filled stand-in. Creating the file here
	// would give it the right name, size and mode with content that never
	// existed anywhere.
	if _, err := os.Stat(filepath.Join(dst, "unreadable.txt")); err == nil {
		t.Error("a file was fabricated at the destination for a source that was never read")
	}
}

// TestE2EDirFailedReadLeavesGoodDestinationCopyIntact: the destination
// already holds a good copy from an earlier run and the source then becomes
// unreadable. The earlier copy must survive byte-for-byte -- finalize used to
// create-and-truncate the path to the announced size, destroying a good
// backup whenever the size had changed.
func TestE2EDirFailedReadLeavesGoodDestinationCopyIntact(t *testing.T) {
	src := t.TempDir()
	good := "ORIGINAL GOOD CONTENT v1"
	badPath := filepath.Join(src, "data.txt")
	mustWriteFile(t, badPath, good)

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)
	if got, _ := os.ReadFile(filepath.Join(dst, "data.txt")); string(got) != good {
		t.Fatalf("first run did not transfer: %q", got)
	}

	// The source grows (so a truncate to the new size would be destructive)
	// and becomes unreadable.
	mustWriteFile(t, badPath, good+" now considerably longer")
	if err := os.Chmod(badPath, 0000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(badPath, 0644) })

	// quickCheck off: force the transfer to actually attempt the read.
	sess, _ := startDirClientExpectingFailures(t, src, dst, 2, false, false)
	if sess.FailedCount() != 1 {
		t.Errorf("FailedCount = %d, want 1", sess.FailedCount())
	}
	got, rerr := os.ReadFile(filepath.Join(dst, "data.txt"))
	if rerr != nil {
		t.Fatalf("the good destination copy is gone: %v", rerr)
	}
	if string(got) != good {
		t.Errorf("destination copy = %q, want the untouched original %q", got, good)
	}
}

// TestE2EDirDeleteProtectsUnreadableSourceFile: with -delete, a source file
// that cannot be read must not cost the destination its only copy. The path
// is announced (as a normal KindFile whose transfer then fails), so it stays
// in the keep-set; before, an unreadable file was dropped at walk time,
// never announced, and the delete pass removed the destination's good copy
// while the run still reported success.
func TestE2EDirDeleteProtectsUnreadableSourceFile(t *testing.T) {
	src := t.TempDir()
	irreplaceable := "IRREPLACEABLE BACKUP DATA"
	badPath := filepath.Join(src, "data.txt")
	mustWriteFile(t, badPath, irreplaceable)
	mustWriteFile(t, filepath.Join(src, "other.txt"), "other")

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)

	if err := os.Chmod(badPath, 0000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(badPath, 0644) })

	// quickCheck off so the run genuinely tries (and fails) to read it,
	// which is the case that used to lose the file.
	startDirClientExpectingFailures(t, src, dst, 2, false, true)

	if _, err := os.Lstat(filepath.Join(dst, "data.txt")); err != nil {
		t.Errorf("-delete removed the destination's only copy of an unreadable source file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "other.txt")); err != nil {
		t.Errorf("an unrelated file was lost: %v", err)
	}
}

// TestE2EDirDestinationPathIsADirectory is the regression test for F11: a
// single file whose destination path is blocked by an existing directory
// used to hang startDirServer FOREVER (confirmed via a goroutine dump
// during development of this test) because MarkDone's finalize failure for
// that one file killed the CONTROL connection, orphaning OpTreeDone and
// disabling the idle-timeout escape hatch. Acquire's open fails with
// EISDIR; that failure must be isolated to this one file, not take down the
// control connection every other file's completion depends on.
func TestE2EDirDestinationPathIsADirectory(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "good.txt"), "fine")
	mustWriteFile(t, filepath.Join(src, "blocked.txt"), "cannot land")

	dst := t.TempDir()
	mustMkdirAll(t, filepath.Join(dst, "blocked.txt"))

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	done := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(done)
		sess, clientErr = startDirClient(src, addr, 0, true, 2, nil, false, false, false, false)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("startDirClient did not return -- the transfer hung (F11 regression)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	if sess.FailedCount() != 1 {
		t.Fatalf("FailedCount = %d, want 1", sess.FailedCount())
	}

	select {
	case err := <-srvErrCh:
		if err == nil {
			t.Fatal("startDirServer unexpectedly reported success despite one file failing to finalize")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("startDirServer did not return -- the transfer hung (F11 regression)")
	}

	got, err := os.ReadFile(filepath.Join(dst, "good.txt"))
	if err != nil {
		t.Fatalf("good.txt missing at destination: %v", err)
	}
	if string(got) != "fine" {
		t.Errorf("good.txt content = %q, want %q", got, "fine")
	}
}

func TestE2EDirReadOnlyDestinationRoot(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "x")

	parent := t.TempDir()
	dstRoot := filepath.Join(parent, "dst")
	if err := os.Mkdir(dstRoot, 0555); err != nil { // read+execute only, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dstRoot, 0755) }) // let TempDir's cleanup remove it

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err == nil {
		t.Error("startDirServer unexpectedly succeeded writing into a read-only root")
	}
	if sess.FailedCount() == 0 {
		t.Error("expected at least one failed file against a read-only destination root")
	}
}

func TestE2EDirWorkersExceedFileCount(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), "one")
	mustWriteFile(t, filepath.Join(src, "b.txt"), "two")
	dst, _ := runDirTransfer(t, src, 8, false) // 8 workers, only 2 files
	assertTreesEqual(t, src, dst)
}

// TestE2EDirLargeTreeDoesNotDeadlock is a regression test for the most
// severe bug found in the hardening pass: startDirClient's fan-out
// goroutine wrote every walked entry to BOTH treeFileChan and
// announceChan, but treeFileChan (buffered 256) had no reader until AFTER
// the whole walk finished -- so any tree past roughly 256+256 entries
// deadlocked the client (and consequently the server) forever, with no
// timeout and no error. 700+ entries is comfortably past that threshold;
// every other e2e dir test in this file stays well under it, which is
// exactly why this went unnoticed. Run in its own goroutine with an
// explicit timeout so a regression fails loudly instead of hanging the
// whole test binary.
func TestE2EDirLargeTreeDoesNotDeadlock(t *testing.T) {
	src := t.TempDir()
	const nDirs = 20
	const filesPerDir = 35 // 700 files + 20 dirs = 720 entries
	for d := 0; d < nDirs; d++ {
		dir := filepath.Join(src, fmt.Sprintf("dir%03d", d))
		mustMkdirAll(t, dir)
		for f := 0; f < filesPerDir; f++ {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), "x")
		}
	}

	resetEngineGlobals()
	dstRoot := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 4, true) }()
	waitForListener(t, addr)

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, addr, 0, true, 4, nil, false, false, false, false)
	}()
	select {
	case <-clientDone:
	case <-time.After(15 * time.Second):
		t.Fatal("startDirClient did not complete within 15s -- deadlock regression (large tree)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not complete within 15s -- deadlock regression (large tree)")
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dstRoot)
}

// buildMixedAffinitySizeTree populates root with ~400 one-block small files
// (routed to a dedicated worker under affinityWorker, since Blocks(1) is
// always <= any positive worker count) plus a few large multi-block files
// (Blocks comfortably > workers, so they stay on the shared/FCFS path).
// Forces hashing goroutines to alternate between blocking on a dedicated
// channel and on shared throughout the run -- the scenario
// precomputeTreeBlocksRouted's deadlock-safety argument calls out as the
// one hazard worth a dedicated regression test.
func buildMixedAffinitySizeTree(t *testing.T, root string) {
	t.Helper()
	const nSmall = 400
	const nLarge = 3
	const largeBlocks = 200
	for i := 0; i < nSmall; i++ {
		mustWriteFile(t, filepath.Join(root, fmt.Sprintf("small%04d.txt", i)), fmt.Sprintf("small file %d", i))
	}
	for i := 0; i < nLarge; i++ {
		data := randomBytes(4096*largeBlocks+17, int64(900+i))
		mustWriteFile(t, filepath.Join(root, fmt.Sprintf("large%02d.bin", i)), string(data))
	}
}

// TestE2EDirMixedTreeSmallAndHugeDoesNotDeadlock is a deadlock regression
// test for the affinity-routing pipeline (checksum.go's
// precomputeTreeBlocksRouted / dirsync.go's dual-source
// runDirTransferWorker), modeled on TestE2EDirLargeTreeDoesNotDeadlock's
// explicit-goroutine-plus-timeout style: a bare blocking call would just
// hang the whole test binary on a real regression instead of failing it
// loudly. Mixes ~400 single-block (dedicated-routed) files with a few
// 200-block (shared-routed) files under multiple workers, so hashing
// goroutines are forced to alternate between blocking on a dedicated
// channel and on shared throughout the run.
func TestE2EDirMixedTreeSmallAndHugeDoesNotDeadlock(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()
	buildMixedAffinitySizeTree(t, src)

	resetEngineGlobals()
	dstRoot := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 4, true) }()
	waitForListener(t, addr)

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, addr, 0, true, 4, nil, false, false, false, false)
	}()
	select {
	case <-clientDone:
	case <-time.After(20 * time.Second):
		t.Fatal("startDirClient did not complete within 20s -- deadlock regression (mixed small/large tree)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not complete within 15s -- deadlock regression (mixed small/large tree)")
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dstRoot)
}

// TestE2EDirMixedTreeSingleWorkerDoesNotDeadlock is
// TestE2EDirMixedTreeSmallAndHugeDoesNotDeadlock's workers=1 variant: with
// exactly one worker, EVERY single-block file's affinityWorker threshold is
// blocks<=1, so it's dedicated-routed, while every large (>1-block) file
// stays shared-routed -- and the one worker must correctly drain both its
// own dedicated channel and the shared channel to completion. This is the
// minimal case for the dual-source loop's worker-exit hazard: if either
// channel were ever closed without being fully drained, or the loop could
// exit before both close, this is the cheapest configuration to expose it.
func TestE2EDirMixedTreeSingleWorkerDoesNotDeadlock(t *testing.T) {
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()
	buildMixedAffinitySizeTree(t, src)

	resetEngineGlobals()
	dstRoot := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, addr, 0, true, 1, nil, false, false, false, false)
	}()
	select {
	case <-clientDone:
	case <-time.After(30 * time.Second):
		t.Fatal("startDirClient did not complete within 30s -- deadlock regression (mixed tree, single worker)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not complete within 15s -- deadlock regression (mixed tree, single worker)")
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dstRoot)
}

// TestE2EDirSourceFileDisappearsAfterWalkStillCompletes is a regression
// test for a bug (A5) distinct from TestE2EDirUnreadableSourceFileIsSkippedNotFailed:
// that test chmods the file BEFORE the walk starts, so walkSource's own
// open (for metadata) fails and the file is never announced at all. THIS
// test lets the file be announced normally, then removes it only once a
// preceding sentinel file has already landed at the destination -- proving
// the walk (and this file's own announce) completed successfully well
// before the removal. Before the fix, TreeReader's failure to open an
// ANNOUNCED file never told the client's per-file block counter that no
// blocks were coming, so OpFileDone was never sent for it, and the server
// hung in WaitAllFinalized for its full timeout, failing the WHOLE tree --
// not just the one vanished file.
func TestE2EDirSourceFileDisappearsAfterWalkStillCompletes(t *testing.T) {
	// Linux does not invalidate an already-open fd when its directory
	// entry is unlinked (reads keep succeeding against the orphaned
	// inode), so removal only reproduces a REAL failure if it lands
	// before TreeReader's own os.Open for this file -- and after
	// walkSource has already stat'd it, or this hits the walk-time skip
	// path instead (covered by
	// TestE2EDirUnreadableSourceFileIsReportedNotFabricated). walkSource itself
	// is fast regardless of content (stat-only per entry), so it reliably
	// finishes well before this; the real difficulty is making TreeReader's
	// CONTENT read of earlier files take long enough, in real wall-clock
	// time, to leave a reliable window -- on loopback with tiny files this
	// is a race measured in microseconds. Routing the transfer through a
	// deliberately-slowed proxy (delayPerChunk) makes the window
	// deterministic instead of hoping local I/O happens to be slow enough.
	src := t.TempDir()
	saved := blockSize
	blockSize = 4096
	defer func() { blockSize = saved }()

	// 16 blocks (64KB) deliberately exceeds the default byte budget
	// (workers*4*blockSize = 1*4*4096 = 16KB): TreeReader can read the
	// first ~4 blocks into the budget for free, but must then genuinely
	// BLOCK, waiting for the (proxy-slowed) worker to actually finish
	// sending each earlier block before there's room to read further --
	// including opening the file that comes after this one.
	mustWriteFile(t, filepath.Join(src, "0-pad.bin"), string(randomBytes(16*4096, 901)))
	vanishingPath := filepath.Join(src, "1-vanishing.txt")
	mustWriteFile(t, vanishingPath, "will be removed shortly after the transfer starts")
	mustWriteFile(t, filepath.Join(src, "2-good.txt"), "still fine")

	resetEngineGlobals()
	dst := t.TempDir()
	port := freeTCPPort(t)
	realAddr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, realAddr)
	proxyAddr, proxy := startFaultyProxy(t, realAddr, 0, 0, 0) // pure passthrough
	proxy.delayPerChunk = 3 * time.Millisecond

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, proxyAddr, 0, true, 1, nil, false, false, false, false)
	}()

	// The walk (stat-only) is done essentially instantly; TreeReader is
	// still blocked on the byte budget working through 0-pad.bin's real
	// content when the target is removed.
	time.Sleep(150 * time.Millisecond)
	if err := os.Remove(vanishingPath); err != nil {
		t.Fatalf("remove: %v", err)
	}

	select {
	case <-clientDone:
	case <-time.After(20 * time.Second):
		t.Fatal("startDirClient did not complete -- the transfer hung (A5 regression)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		// The server reports the vanished file as not correct at the
		// destination -- it received no data for it and therefore left the
		// path alone rather than fabricating a zero-filled stand-in. That
		// report is the desired outcome, not a test failure; what matters
		// is that the server still COMPLETED instead of hanging.
		if err == nil {
			t.Error("the server reported success for a file it never received any data for")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startDirServer did not complete after the client finished")
	}

	// Nothing was fabricated for the file that vanished mid-transfer.
	if _, err := os.Stat(filepath.Join(dst, "1-vanishing.txt")); err == nil {
		t.Error("a zero-filled stand-in was created for the vanished file")
	}

	if sess.FailedCount() != 1 {
		t.Fatalf("FailedCount = %d, want 1 (only the vanished file) -- if this flakes, the removal raced TreeReader's read and lost; widen the margin", sess.FailedCount())
	}
	if !sess.isFailed(1) { // 0-pad.bin is FileID 0, vanishing file is FileID 1 in lexical walk order
		t.Error("the vanished file's specific FileID was not the one marked failed")
	}
	// 0-pad.bin and 2-good.txt are entirely unaffected by 1-vanishing.txt's
	// failure -- the whole point of A5 is that one file's failure must not
	// take down the rest of the tree.
	for _, rel := range []string{"0-pad.bin", "2-good.txt"} {
		got, rerr := os.ReadFile(filepath.Join(dst, rel))
		if rerr != nil {
			t.Errorf("%s missing at destination: %v", rel, rerr)
			continue
		}
		want, _ := os.ReadFile(filepath.Join(src, rel))
		if string(got) != string(want) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	// 1-vanishing.txt's destination entry is NOT asserted absent here:
	// removing it mid-transfer (rather than before the walk, which
	// TestE2EDirUnreadableSourceFileIsSkippedNotFailed already covers)
	// exercises TreeReader's MID-FILE failure path -- some of its blocks
	// may have already been read and even written before the removal won
	// the race, so the destination file can legitimately exist, correctly
	// sized but with incomplete content for whichever blocks never
	// arrived (finalizeIO truncates/chmods/chtimes it regardless, exactly
	// as an interrupted single-file transfer would leave things -- a later
	// re-run is what actually fixes it up). What matters, and is asserted
	// above, is that the failure was correctly isolated to this one file
	// and did not affect 0-pad.bin or 2-good.txt, or hang the transfer.
}

// TestE2EDirBlockSizeCapEndToEnd is the end-to-end counterpart to
// TestWalkTreeRespectsBlockSizeCap: confirms startDirClient's own capBS
// resolution actually reaches the walk (not just the byte budget) through
// the real client entry point, not just walkSource directly.
func TestE2EDirBlockSizeCapEndToEnd(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.bin"), string(randomBytes(200000, 902)))

	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	const blockCap = 8192
	sess, err := startDirClient(src, addr, blockCap, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	// The transfer has already completed (startDirClient returned), so
	// FileID 0's session entry has been released (see
	// SyncSession.Release/claimDone). treeBlockSize is the same pure
	// function walk.go used to pick it, given the same fixture size and
	// cap, so it stands in for reading the value back from the session.
	if got := treeBlockSize(200000, blockCap); got != blockCap {
		t.Errorf("treeBlockSize(200000, %d) = %d, want exactly %d (the cap passed to startDirClient)", blockCap, got, blockCap)
	}
	assertTreesEqual(t, src, dst)
}

// TestE2EDirExcludeEndToEnd confirms startDirClient's excl parameter
// actually reaches walkSource through the real client entry point (the
// matcher itself, excluded(), is already covered directly by
// TestWalkTreeExcluded).
func TestE2EDirExcludeEndToEnd(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), "keep me")
	mustWriteFile(t, filepath.Join(src, "skip.log"), "exclude me")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "also.log"), "exclude me too")

	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, true, 1, []string{"*.log"}, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}

	got, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil {
		t.Fatalf("keep.txt missing at destination: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("keep.txt content mismatch")
	}
	for _, excluded := range []string{"skip.log", filepath.Join("sub", "also.log")} {
		if _, err := os.Stat(filepath.Join(dst, excluded)); err == nil {
			t.Errorf("%s should have been excluded but exists at the destination", excluded)
		}
	}
}

// TestE2EDirExcludeSubstringGlobEndToEnd proves the exact pattern syntax a
// user would reach for to exclude "anything containing this substring" --
// *some_file_name* -- works through the real client entry point, both at
// the top level and nested (matched via excluded()'s basename arm).
func TestE2EDirExcludeSubstringGlobEndToEnd(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), "keep me")
	mustWriteFile(t, filepath.Join(src, "my_some_file_name_v2.txt"), "exclude me")
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "also_some_file_name.log"), "exclude me too")

	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, true, 1, []string{"*some_file_name*"}, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}

	got, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil {
		t.Fatalf("keep.txt missing at destination: %v", err)
	}
	if string(got) != "keep me" {
		t.Errorf("keep.txt content mismatch")
	}
	for _, excludedPath := range []string{"my_some_file_name_v2.txt", filepath.Join("sub", "also_some_file_name.log")} {
		if _, err := os.Stat(filepath.Join(dst, excludedPath)); err == nil {
			t.Errorf("%s should have been excluded but exists at the destination", excludedPath)
		}
	}
}

// TestE2EDirServerIdleTimerResetsAfterConnectionCloses is a regression test
// for A2: idleTimer.Stop() ran on the FIRST accepted connection, but
// nothing ever called Reset -- once any client had ever connected, the
// idle-timeout escape hatch was permanently dead, so a client that
// connected and then vanished (crash, network partition, or any other bug)
// left startDirServer blocked in its select loop forever. Connects and
// immediately disconnects without ever completing the protocol, then
// asserts the server still gives up within a bounded time instead of
// hanging.
func TestE2EDirServerIdleTimerResetsAfterConnectionCloses(t *testing.T) {
	savedIdle := dirIdlePeriod
	dirIdlePeriod = 200 * time.Millisecond
	defer func() { dirIdlePeriod = savedIdle }()

	resetEngineGlobals()
	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, addr)

	// Connect (this is what stops the FIRST idle timer) and immediately
	// disconnect without sending anything -- simulating a vanished client.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	select {
	case err := <-srvErrCh:
		if err == nil {
			t.Fatal("startDirServer unexpectedly succeeded after its only client vanished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startDirServer did not time out -- the idle timer never reset (A2 regression)")
	}
}

// TestE2EDirControlConnectionSurvivesLongIdleBetweenFiles is a regression
// test for A4: the control connection used the same short ioTimeout as
// every worker connection, but it legitimately sends nothing between
// OpAnnounceEnd and the first OpFileDone -- for a slow file or a slow link,
// easily longer than ioTimeout. Shrinks ioTimeout drastically (so a control
// connection using it would time out almost immediately) while leaving
// ctrlIoTimeout comfortably longer than the test's own delay, then confirms
// the transfer still completes normally despite an idle gap on the control
// connection that would fail the OLD uniform-deadline behavior.
//
// The delay must be spread across many small per-chunk hops on the WORKER
// connection (one 20KB file cut into 10 blocks via a small blockSizeCap),
// each comfortably under ioTimeout on its own, so their cumulative sum is
// what exceeds ioTimeout on the control connection's idle gap -- not any
// single worker round-trip, which would just make the worker connection
// itself time out instead of exercising the control-connection path this
// test targets. This was empirically double-checked: an earlier version of
// this test (single 1-byte file, one big delayPerChunk) was found to pass
// even with the A4 fix entirely disabled, because the real gap between
// OpAnnounceEnd and OpFileDone for a trivial one-block file turned out to
// be far smaller than assumed (~80ms against a 150ms ioTimeout). This
// version was verified, by temporarily disabling the ctrlIoTimeout branch
// in server_dir.go and rebuilding, to actually FAIL (a broken-pipe /
// io-timeout error) without the fix, and PASS with it restored.
func TestE2EDirControlConnectionSurvivesLongIdleBetweenFiles(t *testing.T) {
	savedIoTimeout := ioTimeout
	savedCtrlIoTimeout := ctrlIoTimeout
	ioTimeout = 100 * time.Millisecond
	ctrlIoTimeout = 5 * time.Second
	defer func() {
		ioTimeout = savedIoTimeout
		ctrlIoTimeout = savedCtrlIoTimeout
	}()

	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), strings.Repeat("x", 20*1024))

	dst := t.TempDir()
	port := freeTCPPort(t)
	realAddr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, realAddr)
	// Delay every chunk so the SUM of per-block hops on the worker
	// connection comfortably exceeds the (deliberately shrunk) worker
	// ioTimeout by the time OpFileDone finally reaches the control
	// connection, while each individual hop stays well under ioTimeout
	// (so the worker connection itself never times out) and the whole
	// transfer still finishes well within ctrlIoTimeout.
	proxyAddr, proxy := startFaultyProxy(t, realAddr, 0, 0, 0)
	proxy.delayPerChunk = 15 * time.Millisecond

	sess, err := startDirClient(src, proxyAddr, 2048, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v (control connection should have survived the idle gap via ctrlIoTimeout)", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

// TestE2EDirSingleWorkerManyFilesConnectionChurn covers a worker-pool gap:
// workers=1 against a few-hundred-file tree means every single file's
// OpFileOpen/OpBlockCheck/OpBlockData/OpFileClose cycle serializes onto the
// SAME connection (dirWorkerBind's "same conn, different file" rebind path,
// not a fresh dial per file) -- unlike every other many-file test in this
// suite, which spreads files across multiple workers/connections and so
// never exercises that one connection carrying hundreds of file
// open/close cycles back to back.
func TestE2EDirSingleWorkerManyFilesConnectionChurn(t *testing.T) {
	src := t.TempDir()
	const nDirs = 5
	const filesPerDir = 60 // 300 files, one worker, one connection
	for d := 0; d < nDirs; d++ {
		dir := filepath.Join(src, fmt.Sprintf("dir%02d", d))
		mustMkdirAll(t, dir)
		for f := 0; f < filesPerDir; f++ {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), string(randomBytes(150, int64(d*1000+f))))
		}
	}
	dst, sess := runDirTransfer(t, src, 1, false)
	assertTreesEqual(t, src, dst)
	if want := nDirs * filesPerDir; int(sess.filesDone.Load()) != want {
		t.Errorf("filesDone = %d, want %d", sess.filesDone.Load(), want)
	}
}

// TestE2EDirWorkerPermanentFailureStillSendsFileDoneForOtherFiles is a
// regression/invariant test for the thing A5's counter-correction and
// runDirTransferWorker's "count every block as resolved, not just
// successful ones" comment both depend on: when a block exhausts every
// retry and the worker gives up on it PERMANENTLY (not a redial that
// eventually succeeds, which TestE2EDirSurvivesMidTransferConnectionDrops
// already covers), recordBlockDone must still fire for that block so
// OpFileDone is still sent for its file -- otherwise one permanently
// unlucky file would hang the ENTIRE tree in WaitAllFinalized, not just
// fail its own file. Kills exactly maxRetries worker connections (after
// sparing the control connection's own first dial), exhausting the first
// file's block-0 retry budget entirely; the worker's later dial for the
// second file is outside the proxy's kill budget and succeeds normally.
func TestE2EDirWorkerPermanentFailureStillSendsFileDoneForOtherFiles(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a-bad.txt"), string(randomBytes(2000, 1)))
	mustWriteFile(t, filepath.Join(src, "b-good.txt"), string(randomBytes(2000, 2)))
	dst := t.TempDir()

	port := freeTCPPort(t)
	realAddr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 1, true) }()
	waitForListener(t, realAddr)

	// skipFirst=1 spares the control connection; killConns=maxRetries kills
	// every one of the single worker's dial attempts for "a-bad.txt"'s only
	// block, exhausting its retry budget entirely (unlike the
	// TestE2EDirSurvivesMidTransferConnectionDrops case, which spares the
	// LAST attempt so it recovers).
	proxyAddr, _ := startFaultyProxy(t, realAddr, 1, maxRetries, 200)

	sess, err := startDirClient(src, proxyAddr, 0, true, 1, nil, false, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v (a permanently failed file must not fail the whole client-side call)", err)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not exit -- the permanently-failed file's OpFileDone was never sent, hanging WaitAllFinalized")
	}

	if got := sess.FailedCount(); got != 1 {
		t.Fatalf("FailedCount = %d, want exactly 1 (only a-bad.txt)", got)
	}
	// Both files' session entries have already been released by now (the
	// transfer completed above -- see SyncSession.Release/claimDone), so
	// they can no longer be found by FileByID/RelPath. There are only two
	// FileIDs (0 and 1, in walk order) and exactly one of them is failed
	// per the FailedCount check just above; isFailed itself is backed by a
	// separate, unreleased structure (SyncSession.failed), so it still
	// works after completion and is what determines which is which here.
	var badID, goodID uint32
	if sess.isFailed(0) {
		badID, goodID = 0, 1
	} else {
		badID, goodID = 1, 0
	}
	if !sess.isFailed(badID) {
		t.Error("a-bad.txt: expected isFailed == true")
	}
	if sess.isFailed(goodID) {
		t.Error("b-good.txt: expected isFailed == false (should have transferred normally)")
	}
	got, err := os.ReadFile(filepath.Join(dst, "b-good.txt"))
	if err != nil {
		t.Fatalf("reading b-good.txt at destination: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(src, "b-good.txt"))
	if err != nil {
		t.Fatalf("reading b-good.txt at source: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("b-good.txt: destination content does not match source, despite a-bad.txt being the only file that should have failed")
	}
}

// TestE2EDirServerConnectionChurnDuringShutdownRace is a regression/stress
// test for A3: startDirServer used to return (and its `defer ow.Close()`
// run) as soon as doneChan closed, with nothing waiting for in-flight
// dirHandleConn goroutines -- one still inside handleBlockData/
// handleBlockZero at that instant would send to the now-closed ow.ch and
// panic the whole process. The fix (connWG, with `defer connWG.Wait()`
// declared AFTER `defer ow.Close()` so LIFO ordering runs the Wait first)
// makes this correct regardless of timing, not just "usually fine" -- so
// this test's job is to maximize goroutine-scheduling entropy around the
// exact moment doneChan closes and let the race detector (`go test -race`)
// and a plain "did the server panic" check confirm nothing slips through.
// A steady stream of raw connect-then-immediately-disconnect connections
// runs concurrently with a real multi-worker, multi-file transfer, kept
// alive until just after the transfer's own OpTreeDone has landed, so
// several of those churn connections are in various stages of being
// accepted/read/torn down at the exact moment the server decides to shut
// down. This is intentionally NOT a byte-for-byte reproduction of the
// original write-to-closed-channel panic (which required a live in-flight
// write racing ow.Close, not just connection churn) -- doneChan can only
// close once every real block has already been acknowledged (the client's
// own wg.Wait() gates sending OpTreeDone), so that exact window is not
// independently reproducible from outside the package without reaching
// into server_dir.go's unexported locals. This test instead pins down the
// weaker, still-valuable invariant that connWG's wait tolerates a busy,
// actively-churning connection count at shutdown without panicking or
// hanging -- run under `-race` (as this whole suite's own verification
// pipeline always does) it also stands a real chance of catching a
// reintroduced version of the underlying bug.
func TestE2EDirServerConnectionChurnDuringShutdownRace(t *testing.T) {
	resetEngineGlobals()
	src := t.TempDir()
	for i := 0; i < 40; i++ {
		mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%02d.bin", i)), string(randomBytes(3000, int64(i))))
	}
	dst := t.TempDir()

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 6, true) }()
	waitForListener(t, addr)

	churnDone := make(chan struct{})
	var churnWG sync.WaitGroup
	for i := 0; i < 8; i++ {
		churnWG.Add(1)
		go func() {
			defer churnWG.Done()
			for {
				select {
				case <-churnDone:
					return
				default:
				}
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					continue // server may be mid-shutdown; that's fine, just stop trying
				}
				conn.Close()
			}
		}()
	}

	sess, err := startDirClient(src, addr, 0, true, 6, nil, false, false, false, false)
	// Give the churners a little more time to keep hammering right around
	// OpTreeDone/shutdown before signaling them to stop.
	time.Sleep(20 * time.Millisecond)
	close(churnDone)
	churnWG.Wait()

	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not exit within 15s")
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dst)
}

// --- quick-check (-quick-check: skip a file whose destination already has
// the same size and modification time) ---

func TestE2EDirQuickCheckColdRunTransfersEverything(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(9000, 81)))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), string(randomBytes(500, 82)))

	dst, sess := runDirTransferOpts(t, src, 2, false, true)
	assertTreesEqual(t, src, dst)
	if sess.bytesNet.Load() == 0 {
		t.Fatal("cold run with -quick-check transferred zero bytes -- test fixture problem")
	}
	if sess.blocksDone.Load() == 0 {
		t.Fatal("cold run with -quick-check hashed zero blocks -- nothing exists yet, so nothing should have been skippable")
	}
}

func TestE2EDirQuickCheckSkipsUnchangedFilesOnRerun(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(9000, 91)))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), string(randomBytes(500, 92)))

	dst, sess1 := runDirTransferOpts(t, src, 2, false, true)
	assertTreesEqual(t, src, dst)
	if sess1.bytesNet.Load() == 0 {
		t.Fatal("first run transferred zero bytes -- test fixture problem, not quick-check")
	}
	before := snapshotMTimes(t, dst)

	sess2 := startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)

	// The whole point of quick-check: every file already matches, so
	// TreeReader never even opens them -- zero blocks hashed, zero bytes
	// transferred, on this run.
	if got := sess2.blocksDone.Load(); got != 0 {
		t.Errorf("second run's blocksDone = %d, want 0 -- quick-check should have skipped every file before it was ever read", got)
	}
	if got := sess2.bytesNet.Load(); got != 0 {
		t.Errorf("second run's payload bytes = %d, want 0", got)
	}
	if got := sess2.diffs.Load(); got != 0 {
		t.Errorf("second run's diffs = %d, want 0", got)
	}

	assertTreesEqual(t, src, dst)
	after := snapshotMTimes(t, dst)
	for rel, want := range before {
		if got := after[rel]; !got.Equal(want) {
			t.Errorf("%s: mtime changed on a quick-check no-op rerun: was %v, now %v", rel, want, got)
		}
	}
}

func TestE2EDirQuickCheckOnlyModifiedFileTransfers(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(4000, 101)))
	origB := randomBytes(4000, 102) // same length as its replacement below
	mustWriteFile(t, filepath.Join(src, "b.txt"), string(origB))
	mustWriteFile(t, filepath.Join(src, "c.txt"), string(randomBytes(4000, 103)))

	dst, _ := runDirTransferOpts(t, src, 2, false, true)
	assertTreesEqual(t, src, dst)
	before := snapshotMTimes(t, dst)

	// Rewrite b.txt with DIFFERENT content but the SAME size, and force its
	// mtime forward by a whole second (not relying on real wall-clock
	// progression, and past the second-precision granularity quick-check
	// compares at): a same-size file must never be wrongly skipped just
	// because its size still matches -- content did change.
	newB := randomBytes(4000, 999)
	if string(newB) == string(origB) {
		t.Fatal("test fixture problem: newB must differ from origB")
	}
	mustWriteFile(t, filepath.Join(src, "b.txt"), string(newB))
	bMTime := before["b.txt"].Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(src, "b.txt"), bMTime, bMTime); err != nil {
		t.Fatal(err)
	}

	sess2 := startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
	assertTreesEqual(t, src, dst)

	after := snapshotMTimes(t, dst)
	if after["b.txt"].Equal(before["b.txt"]) {
		t.Error("b.txt's destination mtime did not change -- it should have been re-transferred, not skipped")
	}
	for _, rel := range []string{"a.txt", "c.txt"} {
		if !after[rel].Equal(before[rel]) {
			t.Errorf("%s: mtime changed, want unchanged -- only b.txt was modified", rel)
		}
	}
	if sess2.diffs.Load() == 0 {
		t.Error("second run's diffs = 0, want > 0 for the one modified file")
	}
	got, err := os.ReadFile(filepath.Join(dst, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newB) {
		t.Error("b.txt's destination content was not updated to the new content")
	}
}

func TestE2EDirQuickCheckLargeTreeDoesNotDeadlock(t *testing.T) {
	src := t.TempDir()
	const nDirs = 20
	const filesPerDir = 35 // 700 files + 20 dirs = 720 entries, > 2x announceBatchSize
	for d := 0; d < nDirs; d++ {
		dir := filepath.Join(src, fmt.Sprintf("dir%03d", d))
		mustMkdirAll(t, dir)
		for f := 0; f < filesPerDir; f++ {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), "x")
		}
	}

	resetEngineGlobals()
	dstRoot := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, 4, true) }()
	waitForListener(t, addr)

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, addr, 0, true, 4, nil, true, false, false, false)
	}()
	select {
	case <-clientDone:
	case <-time.After(15 * time.Second):
		t.Fatal("startDirClient did not complete within 15s -- deadlock regression (quick-check, large tree)")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not complete within 15s -- deadlock regression (quick-check, large tree)")
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	assertTreesEqual(t, src, dstRoot)
}

// --- -delete (remove destination-only files/directories) ---

func TestE2EDirDeleteRemovesStaleFileAndDirectory(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(2000, 111)))
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), string(randomBytes(500, 112)))

	dst, _ := runDirTransfer(t, src, 2, false)
	assertTreesEqual(t, src, dst)

	// Seed the destination with a stale file and a stale directory (with
	// content) that the source no longer has -- exactly what -delete
	// exists to clean up.
	mustWriteFile(t, filepath.Join(dst, "stale.txt"), "leftover")
	mustMkdirAll(t, filepath.Join(dst, "staledir"))
	mustWriteFile(t, filepath.Join(dst, "staledir", "inside.txt"), "leftover")

	startDirClientAgainst(t, src, dst, 2, false, nil, false, true, false)

	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt still exists (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "staledir")); !os.IsNotExist(err) {
		t.Errorf("staledir still exists (stat err = %v)", err)
	}
	assertTreesEqual(t, src, dst)
}

// TestE2EDirDeleteOffLeavesStaleEntriesAlone is the negative control for the
// test above: the exact same stale file/directory, synced WITHOUT -delete,
// must survive untouched.
func TestE2EDirDeleteOffLeavesStaleEntriesAlone(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(2000, 121)))

	dst, _ := runDirTransfer(t, src, 2, false)
	mustWriteFile(t, filepath.Join(dst, "stale.txt"), "leftover")
	mustMkdirAll(t, filepath.Join(dst, "staledir"))
	mustWriteFile(t, filepath.Join(dst, "staledir", "inside.txt"), "leftover")

	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)

	if _, err := os.ReadFile(filepath.Join(dst, "stale.txt")); err != nil {
		t.Errorf("stale.txt was removed without -delete: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dst, "staledir", "inside.txt")); err != nil {
		t.Errorf("staledir/inside.txt was removed without -delete: %v", err)
	}
}

// TestE2EDirDeleteProtectsExcludedDirectory proves -delete composes safely
// with -exclude by default, matching rsync's own default: an excluded
// source directory is announced as a KindSkipped placeholder (see
// walk.go's skipKept), which protects it AND everything already sitting
// under it on the destination from a -delete pass -- even on this file's
// very first sync (not just a rerun), since the placeholder exists from the
// very first announce batch.
func TestE2EDirDeleteProtectsExcludedDirectory(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), string(randomBytes(1000, 131)))
	mustMkdirAll(t, filepath.Join(src, "skipme"))
	mustWriteFile(t, filepath.Join(src, "skipme", "real.txt"), "never synced")

	dst := t.TempDir()
	mustMkdirAll(t, filepath.Join(dst, "skipme"))
	mustWriteFile(t, filepath.Join(dst, "skipme", "junk.txt"), "pre-existing, unrelated to source")

	startDirClientAgainst(t, src, dst, 2, false, []string{"skipme"}, false, true, false)

	got, err := os.ReadFile(filepath.Join(dst, "skipme", "junk.txt"))
	if err != nil {
		t.Fatalf("skipme/junk.txt was removed: %v", err)
	}
	if string(got) != "pre-existing, unrelated to source" {
		t.Errorf("skipme/junk.txt content changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "skipme", "real.txt")); !os.IsNotExist(err) {
		t.Error("skipme/real.txt should not have been synced -- skipme is excluded")
	}
	want, err := os.ReadFile(filepath.Join(src, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil {
		t.Fatalf("keep.txt was not synced: %v", err)
	}
	if string(got) != string(want) {
		t.Error("keep.txt content mismatch")
	}
}

// TestE2EDirTransferSummaryCountsChangedFiles is the end-to-end pin for
// SyncSession.FilesChanged()/TransferSummary(): on a re-sync where some
// files are untouched, some have new content, and one is quick-check
// skipped, the changed-file count must match reality exactly -- not
// "how many files were scanned" (that's totalFilesKnown) and not "how many
// blocks differed" (that's diffs), but "how many files actually needed to
// transfer new content". Runs against both the strict and the pipelined
// worker engine, since both must feed the same SyncSession counter.
func TestE2EDirTransferSummaryCountsChangedFiles(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		disabled := disabled
		name := "pipelined"
		if disabled {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			withPipeline(t, disabled, func() {
				src := t.TempDir()
				mustWriteFile(t, filepath.Join(src, "untouched.txt"), string(randomBytes(4000, 501)))
				mustWriteFile(t, filepath.Join(src, "will-change.txt"), string(randomBytes(4000, 502)))
				mustWriteFile(t, filepath.Join(src, "quickcheck-skip.txt"), string(randomBytes(4000, 503)))

				// First pass: quick-check ON, so the second pass's untouched
				// files take the announce-time skip path, not the block
				// checksum path -- both "unchanged" routes are exercised.
				dst, sess := runDirTransferOpts(t, src, 2, false, true)
				assertTreesEqual(t, src, dst)
				if got := sess.FilesChanged(); got != 3 {
					t.Fatalf("first pass FilesChanged = %d, want 3 (every file is new)", got)
				}

				mustWriteFile(t, filepath.Join(src, "will-change.txt"), string(randomBytes(4000, 999)))
				// Same size as before, so quick-check's mtime half of the
				// (size, mtime) test is what must catch the change -- bump
				// it explicitly rather than relying on wall-clock drift
				// between two mustWriteFile calls a few microseconds apart.
				future := time.Now().Add(3 * time.Second)
				if err := os.Chtimes(filepath.Join(src, "will-change.txt"), future, future); err != nil {
					t.Fatal(err)
				}

				sess2 := startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
				assertTreesEqual(t, src, dst)
				if got := sess2.FilesChanged(); got != 1 {
					t.Errorf("second pass FilesChanged = %d, want 1 (only will-change.txt)", got)
				}
				if got := sess2.totalFilesKnown.Load(); got != 3 {
					t.Errorf("totalFilesKnown = %d, want 3", got)
				}
				summary := sess2.TransferSummary()
				if !strings.Contains(summary, "1/3 file(s) changed") {
					t.Errorf("TransferSummary() = %q, want it to contain %q", summary, "1/3 file(s) changed")
				}
			})
		})
	}
}

// TestE2EDirDeleteProtectsExcludedFile is the single-file counterpart of
// TestE2EDirDeleteProtectsExcludedDirectory: an excluded FILE (not a
// directory) is still announced as a KindSkipped placeholder, so its
// destination-side namesake survives a -delete pass with its pre-existing
// content untouched, while an unrelated stale file is still removed.
func TestE2EDirDeleteProtectsExcludedFile(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), string(randomBytes(1000, 161)))
	mustWriteFile(t, filepath.Join(src, "excluded.log"), "should never be read from source")

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "excluded.log"), "pre-existing, unrelated to source")
	mustWriteFile(t, filepath.Join(dst, "stale.txt"), "must be removed")

	startDirClientAgainst(t, src, dst, 2, false, []string{"*.log"}, false, true, false)

	got, err := os.ReadFile(filepath.Join(dst, "excluded.log"))
	if err != nil {
		t.Fatalf("excluded.log was removed: %v", err)
	}
	if string(got) != "pre-existing, unrelated to source" {
		t.Errorf("excluded.log content changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale.txt should have been removed by -delete")
	}
	want, err := os.ReadFile(filepath.Join(src, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil {
		t.Fatalf("keep.txt was not synced: %v", err)
	}
	if string(got) != string(want) {
		t.Error("keep.txt content mismatch")
	}
}

// TestE2EDirDeleteExcludedSubdirCoexistsWithRealDeletion checks the two
// -delete behaviors together in one run: an excluded directory nested
// INSIDE a directory that IS synced must keep its pre-existing contents,
// while a genuinely stale file elsewhere in that same synced directory
// must still be removed. Protection and deletion are not mutually
// exclusive -- one excluded subtree must not blind the sweep to real
// staleness next to it.
func TestE2EDirDeleteExcludedSubdirCoexistsWithRealDeletion(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "app"))
	mustWriteFile(t, filepath.Join(src, "app", "keep.txt"), string(randomBytes(1000, 162)))
	mustMkdirAll(t, filepath.Join(src, "app", "cache"))
	mustWriteFile(t, filepath.Join(src, "app", "cache", "warm.txt"), "never synced")

	dst := t.TempDir()
	mustMkdirAll(t, filepath.Join(dst, "app", "cache"))
	mustWriteFile(t, filepath.Join(dst, "app", "cache", "warm.txt"), "pre-existing cache entry")
	mustWriteFile(t, filepath.Join(dst, "app", "stale.txt"), "must be removed")

	startDirClientAgainst(t, src, dst, 2, false, []string{"cache"}, false, true, false)

	got, err := os.ReadFile(filepath.Join(dst, "app", "cache", "warm.txt"))
	if err != nil {
		t.Fatalf("app/cache/warm.txt was removed: %v", err)
	}
	if string(got) != "pre-existing cache entry" {
		t.Errorf("app/cache/warm.txt content changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "app", "stale.txt")); !os.IsNotExist(err) {
		t.Error("app/stale.txt should have been removed by -delete")
	}
	want, err := os.ReadFile(filepath.Join(src, "app", "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "app", "keep.txt"))
	if err != nil {
		t.Fatalf("app/keep.txt was not synced: %v", err)
	}
	if string(got) != string(want) {
		t.Error("app/keep.txt content mismatch")
	}
}

// TestE2EDirDeleteRemovesDestOnlyEntryMatchingExcludePattern documents an
// asymmetry that is easy to assume away: -exclude only protects paths the
// source WALK actually reaches (announced as a KindSkipped placeholder).
// A path that exists ONLY on the destination -- never walked on the source
// side, so never announced in any form -- is not in the keep set at all,
// even if its name would match an exclude pattern. -delete removes it like
// any other stale entry.
func TestE2EDirDeleteRemovesDestOnlyEntryMatchingExcludePattern(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), string(randomBytes(1000, 163)))

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "orphan.log"), "exists only on the destination")

	startDirClientAgainst(t, src, dst, 2, false, []string{"*.log"}, false, true, false)

	if _, err := os.Stat(filepath.Join(dst, "orphan.log")); !os.IsNotExist(err) {
		t.Error("orphan.log (destination-only, never announced) should have been removed by -delete " +
			"even though its name matches an exclude pattern -- exclude only protects paths reachable from source")
	}
}

// TestE2EDirDeleteProtectsSymlinkNamesake proves a source symlink -- never
// created on the destination, but still announced as a KindSkipped
// placeholder -- protects whatever already occupies that path on the
// destination from a -delete pass, exactly like an excluded path does.
func TestE2EDirDeleteProtectsSymlinkNamesake(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "target.txt"), string(randomBytes(1000, 141)))
	if err := os.Symlink(filepath.Join(src, "target.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "link.txt"), "pre-existing regular file at the symlink's path")

	startDirClientAgainst(t, src, dst, 2, false, nil, false, true, false)

	got, err := os.ReadFile(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("link.txt was removed: %v", err)
	}
	if string(got) != "pre-existing regular file at the symlink's path" {
		t.Errorf("link.txt content changed: %q", got)
	}
	want, err := os.ReadFile(filepath.Join(src, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "target.txt"))
	if err != nil {
		t.Fatalf("target.txt was not synced: %v", err)
	}
	if string(got) != string(want) {
		t.Error("target.txt content mismatch")
	}
}

// TestE2EDirDeleteWithQuickCheckCombo exercises the qcSurvivors branch
// (quick-check AND delete-before together): unchanged files are skipped by
// quick-check, a stale destination file is removed by the delete pass, a
// changed file still transfers -- all in one run.
func TestE2EDirDeleteWithQuickCheckCombo(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), string(randomBytes(3000, 151)))
	mustWriteFile(t, filepath.Join(src, "b.txt"), string(randomBytes(3000, 152)))

	dst, _ := runDirTransferOpts(t, src, 2, false, true)
	assertTreesEqual(t, src, dst)

	// Mutate: b.txt changes (new content + bumped mtime), stale.txt appears
	// on the destination only.
	newB := randomBytes(3000, 953)
	mustWriteFile(t, filepath.Join(src, "b.txt"), string(newB))
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(filepath.Join(src, "b.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dst, "stale.txt"), "leftover")

	sess := startDirClientAgainst(t, src, dst, 2, false, nil, true, true, false)

	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt still exists (stat err = %v)", err)
	}
	assertTreesEqual(t, src, dst)
	if sess.diffs.Load() == 0 {
		t.Error("diffs = 0, want > 0 for the changed b.txt")
	}
	// a.txt was unchanged -- quick-check must have skipped it entirely: the
	// only blocks processed belong to b.txt (one block at its size).
	if got := sess.blocksDone.Load(); got != 1 {
		t.Errorf("blocksDone = %d, want 1 (only the changed file's single block)", got)
	}
}

// TestE2EDirEnvironmentalFailureSkipsFilesNotTransfer is the end-to-end
// regression for the production ENOSPC crash: a destination where one
// subtree's path is blocked by a regular file (mkdir fails with ENOTDIR --
// the same environmental class as ENOSPC) must NOT kill the control
// connection and orphan the run. The sync completes, every healthy file is
// byte-identical, exactly the blocked subtree's files fail client-side,
// and the whole thing finishes promptly instead of hanging for hours.
func TestE2EDirEnvironmentalFailureSkipsFilesNotTransfer(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "good1.txt"), string(randomBytes(2000, 161)))
	mustWriteFile(t, filepath.Join(src, "good2.txt"), string(randomBytes(500, 162)))
	mustMkdirAll(t, filepath.Join(src, "blocked"))
	mustWriteFile(t, filepath.Join(src, "blocked", "doomed1.txt"), "never lands")
	mustWriteFile(t, filepath.Join(src, "blocked", "doomed2.txt"), "never lands either")

	resetEngineGlobals()
	dst := t.TempDir()
	// Occupy the "blocked" directory's path with a regular file, so its
	// announce-mkdir (and every child's parent-mkdir) fails.
	mustWriteFile(t, filepath.Join(dst, "blocked"), "a file, not a dir")

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	clientDone := make(chan struct{})
	var sess *SyncSession
	var clientErr error
	go func() {
		defer close(clientDone)
		sess, clientErr = startDirClient(src, addr, 0, true, 2, nil, false, false, false, false)
	}()
	select {
	case <-clientDone:
	case <-time.After(15 * time.Second):
		t.Fatal("startDirClient did not complete within 15s -- the old control-connection-death hang is back")
	}
	if clientErr != nil {
		t.Fatalf("startDirClient must not fail as a whole: %v", clientErr)
	}
	select {
	case err := <-srvErrCh:
		// The server reports the per-file finalize failures as its overall
		// result -- that's the honest outcome, not a crash.
		if err == nil {
			t.Error("startDirServer returned nil, want the doomed files' finalize error surfaced")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startDirServer did not exit within 15s")
	}

	// Exactly the two doomed files failed; both healthy files synced.
	if got := sess.FailedCount(); got != 2 {
		t.Errorf("FailedCount = %d, want 2", got)
	}
	for _, rel := range []string{"good1.txt", "good2.txt"} {
		want, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("%s was not synced: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	// The blocking file itself must be untouched.
	got, err := os.ReadFile(filepath.Join(dst, "blocked"))
	if err != nil || string(got) != "a file, not a dir" {
		t.Errorf("the blocking file was altered: %q, %v", got, err)
	}
}

// --- -links (sync symlinks with identical targets) ---

func TestE2EDirLinksSyncsSymlinksVerbatim(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "target.txt"), string(randomBytes(1000, 171)))
	mustMkdirAll(t, filepath.Join(src, "sub"))
	if err := os.Symlink("target.txt", filepath.Join(src, "rel-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target.txt", filepath.Join(src, "sub", "up-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("does-not-exist", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, true)

	for rel, want := range map[string]string{
		"rel-link":    "target.txt",
		"abs-link":    "/etc/hostname",
		"sub/up-link": "../target.txt",
		"dangling":    "does-not-exist",
	} {
		got, err := os.Readlink(filepath.Join(dst, rel))
		if err != nil {
			t.Errorf("%s: readlink: %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("%s: target = %q, want %q (verbatim)", rel, got, want)
		}
	}
	want, _ := os.ReadFile(filepath.Join(src, "target.txt"))
	got, err := os.ReadFile(filepath.Join(dst, "target.txt"))
	if err != nil || string(got) != string(want) {
		t.Errorf("target.txt content mismatch: %v", err)
	}

	// Idempotent rerun: same links, no diffs, no failures.
	sess2 := startDirClientAgainst(t, src, dst, 2, false, nil, false, false, true)
	if sess2.diffs.Load() != 0 {
		t.Errorf("rerun diffs = %d, want 0", sess2.diffs.Load())
	}
	if gotLink, err := os.Readlink(filepath.Join(dst, "rel-link")); err != nil || gotLink != "target.txt" {
		t.Errorf("rel-link disturbed by rerun: %q, %v", gotLink, err)
	}
}

// TestE2EDirLinksWithDeleteRemovesStaleLink: -links + -delete composition
// -- a synced symlink is kept, a destination-only symlink is removed (its
// target file, being announced, survives).
func TestE2EDirLinksWithDeleteRemovesStaleLink(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "target.txt"), "data")
	if err := os.Symlink("target.txt", filepath.Join(src, "kept-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, true)
	if err := os.Symlink("target.txt", filepath.Join(dst, "stale-link")); err != nil {
		t.Fatal(err)
	}

	startDirClientAgainst(t, src, dst, 2, false, nil, false, true, true)

	if _, err := os.Lstat(filepath.Join(dst, "stale-link")); !os.IsNotExist(err) {
		t.Errorf("stale-link still exists (lstat err = %v)", err)
	}
	if got, err := os.Readlink(filepath.Join(dst, "kept-link")); err != nil || got != "target.txt" {
		t.Errorf("kept-link disturbed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "target.txt")); err != nil || string(got) != "data" {
		t.Errorf("target.txt disturbed: %q, %v", got, err)
	}
}

// TestE2EDirLinksSymlinkToDirectoryFlip is the whole-tree regression: the
// source replaces a symlink with a real directory between two runs. The
// destination's now-stale link used to fail the ANNOUNCE, which killed the
// control connection and aborted the entire transfer -- every remaining file
// failing "was never announced", not just the one conflicting path.
func TestE2EDirLinksSymlinkToDirectoryFlip(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "real.txt"), "payload")
	mustWriteFile(t, filepath.Join(src, "other.txt"), "sibling")
	if err := os.Symlink("sub", filepath.Join(src, "dir-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, true)
	if got, err := os.Readlink(filepath.Join(dst, "dir-link")); err != nil || got != "sub" {
		t.Fatalf("first run did not create the link: %q, %v", got, err)
	}

	// The flip: dir-link becomes a real directory with content of its own.
	if err := os.Remove(filepath.Join(src, "dir-link")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(src, "dir-link"))
	mustWriteFile(t, filepath.Join(src, "dir-link", "x.txt"), "flipped")

	// startDirClientAgainst fails the test on any failed file, so a
	// surviving connection with every file transferred is the assertion.
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, true)

	fi, err := os.Lstat(filepath.Join(dst, "dir-link"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("dir-link is not a real directory: %v (err %v)", fi.Mode(), err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "dir-link", "x.txt")); err != nil || string(got) != "flipped" {
		t.Errorf("dir-link/x.txt = %q, %v; want %q", got, err, "flipped")
	}
	// The sibling files must be unaffected -- this is what the aborted
	// connection used to take down with it.
	if got, err := os.ReadFile(filepath.Join(dst, "other.txt")); err != nil || string(got) != "sibling" {
		t.Errorf("other.txt = %q, %v; want %q", got, err, "sibling")
	}
	if got, err := os.ReadFile(filepath.Join(dst, "sub", "real.txt")); err != nil || string(got) != "payload" {
		t.Errorf("sub/real.txt = %q, %v; want %q", got, err, "payload")
	}
}

// TestE2EDirStaleSymlinkNeverWrittenThrough is the escape regression, end to
// end and WITHOUT -links: once an earlier run has left a symlink at the
// destination, a later plain sync whose source has a regular file at that
// path must replace the link, never follow it into a target that lies
// outside the destination root entirely.
func TestE2EDirStaleSymlinkNeverWrittenThrough(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	mustWriteFile(t, victim, "DO NOT TOUCH")

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "f.txt"), "new source content")

	dst := t.TempDir()
	if err := os.Symlink(victim, filepath.Join(dst, "f.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)

	if got, err := os.ReadFile(victim); err != nil || string(got) != "DO NOT TOUCH" {
		t.Errorf("wrote through the symlink, outside the destination root: victim = %q, %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "f.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("f.txt is still a symlink: %v (err %v)", fi.Mode(), err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "f.txt")); err != nil || string(got) != "new source content" {
		t.Errorf("f.txt = %q, %v; want the source content", got, err)
	}
}

// TestE2EDirSpecialsSyncsFifosAndSockets is the -specials end-to-end case,
// modelled on a backup tree (which is where FIFOs and stray unix sockets
// actually turn up): the nodes are recreated as themselves, regular files
// alongside them still transfer, and a rerun is a clean no-op.
func TestE2EDirSpecialsSyncsFifosAndSockets(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "var", "run"))
	mustWriteFile(t, filepath.Join(src, "var", "data.txt"), string(randomBytes(2000, 42)))
	mkFifo(t, filepath.Join(src, "var", "shim.stdout.log"))
	mkSocket(t, filepath.Join(src, "var", "run", "unix.socket"))

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false, true)

	fi, err := os.Lstat(filepath.Join(dst, "var", "shim.stdout.log"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("shim.stdout.log is not a FIFO: %v (err %v)", fi.Mode(), err)
	}
	fi, err = os.Lstat(filepath.Join(dst, "var", "run", "unix.socket"))
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("unix.socket is not a socket: %v (err %v)", fi.Mode(), err)
	}
	want, _ := os.ReadFile(filepath.Join(src, "var", "data.txt"))
	got, err := os.ReadFile(filepath.Join(dst, "var", "data.txt"))
	if err != nil || string(got) != string(want) {
		t.Errorf("data.txt did not transfer alongside the special files: %v", err)
	}

	// Idempotent rerun: the nodes are left exactly as they are.
	beforeFifo, _ := os.Lstat(filepath.Join(dst, "var", "shim.stdout.log"))
	sess2 := startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false, true)
	if sess2.diffs.Load() != 0 {
		t.Errorf("rerun diffs = %d, want 0", sess2.diffs.Load())
	}
	afterFifo, _ := os.Lstat(filepath.Join(dst, "var", "shim.stdout.log"))
	if !os.SameFile(beforeFifo, afterFifo) {
		t.Error("the FIFO was recreated on an idempotent rerun")
	}
}

// TestE2EDirSpecialsOffSkipsThem pins the default: without -specials the
// nodes are KindSkipped placeholders -- nothing is created for them, and
// -delete still treats their destination namesakes as protected.
func TestE2EDirSpecialsOffSkipsThem(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), "data")
	mkFifo(t, filepath.Join(src, "pipe"))

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)

	if _, err := os.Lstat(filepath.Join(dst, "pipe")); !os.IsNotExist(err) {
		t.Errorf("pipe was created without -specials (lstat err = %v)", err)
	}

	// A destination-side namesake must survive a -delete pass: the source
	// announced the path as skipped, not as absent.
	mkFifo(t, filepath.Join(dst, "pipe"))
	startDirClientAgainst(t, src, dst, 2, false, nil, false, true, false)
	if _, err := os.Lstat(filepath.Join(dst, "pipe")); err != nil {
		t.Errorf("-delete removed a protected skipped-file namesake: %v", err)
	}
}

// TestE2EDirSpecialsWithDeleteRemovesStaleNode: with -specials the nodes are
// genuinely synced, so an unannounced destination FIFO is stale and removed
// like any other extraneous entry.
func TestE2EDirSpecialsWithDeleteRemovesStaleNode(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "keep.txt"), "data")
	mkFifo(t, filepath.Join(src, "kept-pipe"))

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false, true)
	mkFifo(t, filepath.Join(dst, "stale-pipe"))

	startDirClientAgainst(t, src, dst, 2, false, nil, false, true, false, true)

	if _, err := os.Lstat(filepath.Join(dst, "stale-pipe")); !os.IsNotExist(err) {
		t.Errorf("stale-pipe still exists (lstat err = %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "kept-pipe")); err != nil {
		t.Errorf("kept-pipe was removed: %v", err)
	}
}

// TestE2EDirSyncsTrailingDotNames is the reported regression, end to end: a
// bind/zkt backup tree full of DNS zone files whose names carry the root
// label's trailing dot. On a Linux-to-Linux sync these are ordinary files and
// must transfer; they used to be dropped by a Windows-only portability rule,
// silently losing real data with only a "skip ... unsafe path" line.
func TestE2EDirSyncsTrailingDotNames(t *testing.T) {
	// Gated on the OS, deliberately NOT on enforceWindowsPathRules: keying
	// the skip off the very flag under test would let a regression that
	// re-enables Windows rules on Linux silently skip this instead of
	// failing it.
	if runtime.GOOS == "windows" {
		t.Skip("these names genuinely cannot be created on Windows")
	}
	src := t.TempDir()
	zones := filepath.Join(src, "contrib", "zkt-1.1.2", "examples", "views", "intern")
	mustMkdirAll(t, zones)

	names := []string{"keyset-example.net.", "dnskey.db.", "trailing space "}
	for i, n := range names {
		mustWriteFile(t, filepath.Join(zones, n), string(randomBytes(500+i, int64(i+1))))
	}
	// A reserved-on-Windows name, which is likewise ordinary here.
	mustWriteFile(t, filepath.Join(zones, "aux"), "device-name-clash")

	dst := t.TempDir()
	sess := startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)
	if got := sess.filesSkipped.Load(); got != 0 {
		t.Errorf("skipped %d entr(y/ies); these names are valid on this platform", got)
	}

	dstZones := filepath.Join(dst, "contrib", "zkt-1.1.2", "examples", "views", "intern")
	for _, n := range append(names, "aux") {
		want, err := os.ReadFile(filepath.Join(zones, n))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dstZones, n))
		if err != nil {
			t.Errorf("%q did not transfer: %v", n, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%q content mismatch", n)
		}
	}
}

// --- the tree comparison must actually catch things ---

// TestAssertTreesEqualCatchesRealDifferences proves the helper that ~33 e2e
// tests rely on is not vacuous. Each case corrupts a faithful copy in exactly
// one way and asserts the comparison notices -- including the three the
// PREVIOUS content-only comparison silently missed: a symlink recreated with
// the wrong target (it followed links, so matching target bytes passed), a
// missing empty directory (it ignored directories entirely), and a wrong
// mode.
func TestAssertTreesEqualCatchesRealDifferences(t *testing.T) {
	build := func(t *testing.T) (string, string) {
		t.Helper()
		src, dst := t.TempDir(), t.TempDir()
		for _, root := range []string{src, dst} {
			mustMkdirAll(t, filepath.Join(root, "sub"))
			mustMkdirAll(t, filepath.Join(root, "empty"))
			mustWriteFile(t, filepath.Join(root, "sub", "data.bin"), string(randomBytes(4096, 7)))
			if err := os.Chmod(filepath.Join(root, "sub", "data.bin"), 0640); err != nil {
				t.Fatal(err)
			}
			mtime := time.Unix(1700000000, 0)
			if err := os.Chtimes(filepath.Join(root, "sub", "data.bin"), mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("sub/data.bin", filepath.Join(root, "link")); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
		}
		// Sanity: identical trees must produce no complaints at all.
		if problems := diffTrees(t, src, dst); len(problems) != 0 {
			t.Fatalf("identical trees reported differences: %v", problems)
		}
		return src, dst
	}

	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, dst string)
		wantSub string
	}{
		{
			name: "single flipped byte",
			corrupt: func(t *testing.T, dst string) {
				p := filepath.Join(dst, "sub", "data.bin")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)/2] ^= 0xFF
				mustWriteFile(t, p, string(b))
				os.Chmod(p, 0640)
				mt := time.Unix(1700000000, 0)
				os.Chtimes(p, mt, mt)
			},
			wantSub: "content differs",
		},
		{
			name: "truncated file",
			corrupt: func(t *testing.T, dst string) {
				if err := os.Truncate(filepath.Join(dst, "sub", "data.bin"), 100); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "content differs",
		},
		{
			name: "symlink pointing elsewhere",
			corrupt: func(t *testing.T, dst string) {
				p := filepath.Join(dst, "link")
				os.Remove(p)
				// Same CONTENT reachable through it -- only the target
				// string differs, which the old comparison could not see.
				if err := os.Symlink("./sub/data.bin", p); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "link target",
		},
		{
			name: "missing empty directory",
			corrupt: func(t *testing.T, dst string) {
				if err := os.Remove(filepath.Join(dst, "empty")); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "missing from destination",
		},
		{
			name: "wrong file mode",
			corrupt: func(t *testing.T, dst string) {
				if err := os.Chmod(filepath.Join(dst, "sub", "data.bin"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "mode",
		},
		{
			name: "wrong mtime",
			corrupt: func(t *testing.T, dst string) {
				mt := time.Unix(1600000000, 0)
				if err := os.Chtimes(filepath.Join(dst, "sub", "data.bin"), mt, mt); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "mtime",
		},
		{
			name: "extra file at the destination",
			corrupt: func(t *testing.T, dst string) {
				mustWriteFile(t, filepath.Join(dst, "unexpected.txt"), "surprise")
			},
			wantSub: "not in the source",
		},
		{
			name: "regular file where a symlink belongs",
			corrupt: func(t *testing.T, dst string) {
				p := filepath.Join(dst, "link")
				os.Remove(p)
				mustWriteFile(t, p, "not a link")
			},
			wantSub: "type is file at destination, want symlink",
		},
		{
			name: "directory replaced by a file",
			corrupt: func(t *testing.T, dst string) {
				p := filepath.Join(dst, "empty")
				os.Remove(p)
				mustWriteFile(t, p, "not a dir")
			},
			wantSub: "type is file at destination, want dir",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := build(t)
			tc.corrupt(t, dst)
			problems := diffTrees(t, src, dst)
			if len(problems) == 0 {
				t.Fatalf("comparison saw no difference after %q", tc.name)
			}
			joined := strings.Join(problems, "; ")
			if !strings.Contains(joined, tc.wantSub) {
				t.Errorf("problems %q do not mention %q", joined, tc.wantSub)
			}
		})
	}
}

// TestAssertTreesEqualHandlesSpecialFilesWithoutHanging: the previous
// comparison used os.ReadFile, which blocks forever on a FIFO -- so the
// -specials tests could not use it. This one records special files by type.
func TestAssertTreesEqualHandlesSpecialFilesWithoutHanging(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, root := range []string{src, dst} {
		mkFifo(t, filepath.Join(root, "pipe"))
		mkSocket(t, filepath.Join(root, "sock"))
	}
	done := make(chan []string, 1)
	go func() { done <- diffTrees(t, src, dst) }()
	select {
	case problems := <-done:
		if len(problems) != 0 {
			t.Errorf("identical special files reported as different: %v", problems)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("comparison hung on a special file -- it must never open one")
	}

	// A FIFO where a socket belongs must still be caught.
	os.Remove(filepath.Join(dst, "pipe"))
	mkSocket(t, filepath.Join(dst, "pipe"))
	if problems := diffTrees(t, src, dst); len(problems) == 0 {
		t.Error("a socket standing in for a FIFO was not reported")
	}
}

// TestE2EDirDirectoryModeIsNotPreserved pins current behavior rather than
// asserting a wish: the destination creates directories with
// MkdirAll(0777)&umask and never chmods or chtimes them, so a source
// directory's permissions do NOT survive the transfer. Documented here so the
// gap is visible and a future change to preserve them fails loudly instead of
// silently altering behavior no test covered.
func TestE2EDirDirectoryModeIsNotPreserved(t *testing.T) {
	src := t.TempDir()
	odd := filepath.Join(src, "restricted")
	mustMkdirAll(t, odd)
	mustWriteFile(t, filepath.Join(odd, "f.txt"), "content")
	if err := os.Chmod(odd, 0700); err != nil {
		t.Fatal(err)
	}

	dst, _ := runDirTransfer(t, src, 2, false)

	// The file inside it transfers correctly, contents and all.
	assertTreesEqual(t, src, dst)

	si, err := os.Lstat(odd)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Lstat(filepath.Join(dst, "restricted"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() == si.Mode().Perm() {
		t.Log("directory permissions now match; if that is intentional, " +
			"snapshotTree should start comparing them and this test should assert equality")
	}
}

// --- quick-check semantics (now the default) ---

// TestE2EDirQuickCheckSkipsSameSizeAndMTime pins the documented hazard of the
// default: a file rewritten with DIFFERENT content but the same size and the
// same mtime is skipped, because size+mtime is all the quick check looks at.
// This is rsync's long-standing tradeoff, not a bug -- but it is exactly the
// kind of behaviour that must be pinned, since it decides whether real data
// moves.
func TestE2EDirQuickCheckSkipsSameSizeAndMTime(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "data.txt")
	mustWriteFile(t, path, "AAAAAAAAAAAAAAAA") // 16 bytes
	fixed := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
	if got, _ := os.ReadFile(filepath.Join(dst, "data.txt")); string(got) != "AAAAAAAAAAAAAAAA" {
		t.Fatalf("first sync did not transfer: %q", got)
	}

	// Same length, same mtime, different bytes.
	mustWriteFile(t, path, "BBBBBBBBBBBBBBBB")
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatal(err)
	}

	startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
	got, _ := os.ReadFile(filepath.Join(dst, "data.txt"))
	if string(got) != "AAAAAAAAAAAAAAAA" {
		t.Errorf("quick-check transferred a file whose size and mtime were unchanged; "+
			"destination = %q. If this is now desired, the README's stated tradeoff needs updating", got)
	}

	// -ignore-times (quickCheck=false) is the escape hatch and MUST catch it.
	startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)
	assertTreesEqual(t, src, dst)
}

// TestE2EDirQuickCheckTransfersOnSizeOrMTimeChange: the ordinary cases must
// still move, or the default would be silently losing updates.
func TestE2EDirQuickCheckTransfersOnSizeOrMTimeChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(t *testing.T, path string)
	}{
		{"size changed", func(t *testing.T, path string) {
			mustWriteFile(t, path, "a much longer replacement body")
		}},
		{"mtime changed", func(t *testing.T, path string) {
			mustWriteFile(t, path, "BBBBBBBBBBBBBBBB") // same 16 bytes
			later := time.Unix(1800000000, 0)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			path := filepath.Join(src, "data.txt")
			mustWriteFile(t, path, "AAAAAAAAAAAAAAAA")
			fixed := time.Unix(1700000000, 0)
			if err := os.Chtimes(path, fixed, fixed); err != nil {
				t.Fatal(err)
			}

			dst := t.TempDir()
			startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
			tc.modify(t, path)
			startDirClientAgainst(t, src, dst, 2, false, nil, true, false, false)
			assertTreesEqual(t, src, dst)
		})
	}
}

// TestE2EDirMixedTreeVerifiedByChecksum is the broad end-to-end assertion the
// suite lacked: one tree combining every entry kind bsync handles -- nested
// and empty directories, a large multi-block file, a zero-byte file, a
// read-only file, relative/absolute/dangling symlinks, a FIFO and a socket --
// verified in a single pass by content hash, type, link target, mode and
// mtime. Previously each kind was checked by its own bespoke assertions, and
// the comparison helper could not even look at symlinks or special files.
func TestE2EDirMixedTreeVerifiedByChecksum(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "a", "b", "c"))
	mustMkdirAll(t, filepath.Join(src, "empty-top"))
	mustMkdirAll(t, filepath.Join(src, "a", "empty-nested"))
	mustWriteFile(t, filepath.Join(src, "a", "b", "c", "deep.bin"), string(randomBytes(3<<20, 11)))
	mustWriteFile(t, filepath.Join(src, "zero.txt"), "")
	mustWriteFile(t, filepath.Join(src, "a", "readonly.txt"), "cannot write me")
	if err := os.Chmod(filepath.Join(src, "a", "readonly.txt"), 0444); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(src, "spaces and 'quotes'.txt"), "odd name")

	if err := os.Symlink("a/b/c/deep.bin", filepath.Join(src, "rel-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere-at-all", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	mkFifo(t, filepath.Join(src, "a", "pipe"))
	mkSocket(t, filepath.Join(src, "sock"))

	for _, workers := range []int{1, 8} {
		t.Run(fmt.Sprintf("w=%d", workers), func(t *testing.T) {
			dst := t.TempDir()
			// -links and -specials on, so every entry above is reproduced
			// rather than left as a skipped placeholder.
			startDirClientAgainst(t, src, dst, workers, false, nil, true, false, true, true)
			assertTreesEqual(t, src, dst)

			// Idempotent re-run must not disturb anything.
			startDirClientAgainst(t, src, dst, workers, false, nil, true, false, true, true)
			assertTreesEqual(t, src, dst)

			// And the conservative path must agree too.
			startDirClientAgainst(t, src, dst, workers, false, nil, false, false, true, true)
			assertTreesEqual(t, src, dst)
		})
	}
}

// TestE2EDirLiveSessionTableDrainsToZero is the structural proof that the
// client's per-entry bookkeeping is O(in-flight), not O(tree): after a real
// transfer over a tree exercising every entry kind -- directories, a real
// file, a zero-byte file, symlinks (relative/absolute/dangling), a FIFO and
// a socket -- completes, SyncSession.LiveCount() must be exactly 0. Every
// entry has some release point (the fan-out loop, the quick-check batcher's
// flush, sendFileDone/markFileSkippedDone via claimDone, or the dedicated
// empty-file loop -- see startDirClient); a single missed one leaks that
// entry (and its FileEntry, RelPath string, and remaining-block counter)
// for the rest of the process's life, which at tree scale is exactly the
// O(tree) memory growth this design exists to eliminate. Checked across the
// quick-check and non-quick-check paths, since they release entries at two
// different points (see the fan-out loop and flush's comments).
func TestE2EDirLiveSessionTableDrainsToZero(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "a", "b"))
	mustMkdirAll(t, filepath.Join(src, "empty-dir"))
	mustWriteFile(t, filepath.Join(src, "a", "b", "real.bin"), string(randomBytes(50000, 12)))
	mustWriteFile(t, filepath.Join(src, "zero.txt"), "")
	if err := os.Symlink("a/b/real.bin", filepath.Join(src, "rel-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	mkFifo(t, filepath.Join(src, "pipe"))
	mkSocket(t, filepath.Join(src, "sock"))

	for _, tc := range []struct {
		name       string
		quickCheck bool
	}{
		{"quickCheckOff", false},
		{"quickCheckOn", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			sess := startDirClientAgainst(t, src, dst, 4, false, nil, tc.quickCheck, false, true, true)
			assertTreesEqual(t, src, dst)
			if live := sess.LiveCount(); live != 0 {
				t.Errorf("LiveCount() = %d after a completed transfer, want 0 -- some entry was never released", live)
			}

			// A second, idempotent run exercises quick-check's actual skip
			// path (everything already matches), which releases entries at
			// yet another point (markFileSkippedDone, via claimDone) --
			// distinct from the first run's fresh-transfer release points.
			sess2 := startDirClientAgainst(t, src, dst, 4, false, nil, tc.quickCheck, false, true, true)
			assertTreesEqual(t, src, dst)
			if live := sess2.LiveCount(); live != 0 {
				t.Errorf("LiveCount() = %d after a completed idempotent re-run, want 0", live)
			}
		})
	}
}

// countSpillFiles counts idSpill's temp files currently sitting in the
// system temp directory (os.TempDir(), the same location newIDSpill uses
// with a "" dir argument). Used to prove a spill file doesn't leak past the
// transfer that created it.
func countSpillFiles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "bsync-tree-*"))
	if err != nil {
		t.Fatalf("glob spill files: %v", err)
	}
	return len(matches)
}

// TestE2EDirDeleteSpillCleanedUpWhenFeederNeverSpawns is the regression for
// idSpill's ownership split (see startDirClient's spill comment): if the
// destination never sends a delete-pass ack (a wedged or misbehaving
// server), startDirClient returns an error BEFORE the feeder goroutine that
// would normally own the spill's cleanup is ever spawned. The
// treeFeederSpawned-gated defer must close (and so remove) the spill itself
// in that case, or a failed -delete transfer would leak a temp file on
// every occurrence.
func TestE2EDirDeleteSpillCleanedUpWhenFeederNeverSpawns(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), "hello")

	before := countSpillFiles(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain whatever the client sends (announce batches, then
		// OpAnnounceEnd) without ever replying, then hang up -- the client
		// is left waiting for a delete-pass ack that will never come, and
		// eventually sees the connection close instead.
		buf := make([]byte, 4096)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := conn.Read(buf); err != nil && !os.IsTimeout(err) {
				return
			}
		}
	}()

	_, err = startDirClient(src, l.Addr().String(), 0, false, 1, nil, false, true, false, false)
	if err == nil {
		t.Fatal("startDirClient succeeded against a server that never acked the delete pass")
	}

	after := countSpillFiles(t)
	if after > before {
		t.Errorf("spill temp file leaked: %d before, %d after a failed delete-pass wait (want no increase)", before, after)
	}
}

// TestE2EDirExcludedSubtreeLeavesTheRestIntact: an excluded directory with
// content under it must be absent from the destination while everything else
// matches exactly -- checked by hash, not just by path presence.
func TestE2EDirExcludedSubtreeLeavesTheRestIntact(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "keep", "deeper"))
	mustMkdirAll(t, filepath.Join(src, "skipme", "inner"))
	mustWriteFile(t, filepath.Join(src, "keep", "deeper", "a.bin"), string(randomBytes(5000, 3)))
	mustWriteFile(t, filepath.Join(src, "keep", "b.txt"), "kept")
	mustWriteFile(t, filepath.Join(src, "skipme", "inner", "gone.txt"), "should not travel")
	mustWriteFile(t, filepath.Join(src, "top.log"), "excluded by pattern")

	dst := t.TempDir()
	startDirClientAgainst(t, src, dst, 2, false, []string{"skipme", "*.log"}, true, false, false)

	if _, err := os.Lstat(filepath.Join(dst, "skipme")); !os.IsNotExist(err) {
		t.Errorf("excluded directory reached the destination (lstat err = %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "top.log")); !os.IsNotExist(err) {
		t.Errorf("excluded file reached the destination (lstat err = %v)", err)
	}
	// Everything that WAS supposed to travel must match exactly.
	assertTreesEqual(t, filepath.Join(src, "keep"), filepath.Join(dst, "keep"))
}

// captureLog redirects Log() output for the duration of fn and returns
// everything written. Used to assert on what a transfer told the operator,
// which for the destination-rejection path is the whole point of the
// feature (the failure itself is invisible in the destination tree).
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	setLogOutput(&lockedWriter{mu: &mu, w: &buf})
	t.Cleanup(func() { setLogOutput(os.Stdout) })
	fn()
	// (fn is responsible for clearing quiet AFTER its own
	// resetEngineGlobals, which sets it; Log() drops everything but READY
	// while quiet is on.)
	setLogOutput(os.Stdout)
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

// lockedWriter serializes concurrent Log() writes from many workers into one
// buffer; bytes.Buffer is not safe for concurrent use.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestE2EDirAnnounceFailureCascadeIsReportedNotFlooded is the regression for
// the production report: with a destination path that cannot be created (a
// regular file standing where a directory must go), EVERY file under it
// fails at announce time. That is correct behavior -- those files genuinely
// never reach the destination -- but the operator's experience of it was
// one unexplained "server rejected file-open permanently" line per file
// (thousands, at -w 512), each costing a killed connection and a redial,
// with the real reason logged only server-side and capped at 10 lines.
//
// This pins the fix end to end: the client learns the reason from the
// destination, the per-file logging is bounded with an aggregated summary,
// and in quick-check mode (the default) the files are resolved from the
// announce ack WITHOUT any file-open attempt at all.
func TestE2EDirAnnounceFailureCascadeIsReportedNotFlooded(t *testing.T) {
	const blocked = 25
	build := func(t *testing.T) (src, dst string) {
		src = t.TempDir()
		mustMkdirAll(t, filepath.Join(src, "blocked"))
		for i := 0; i < blocked; i++ {
			mustWriteFile(t, filepath.Join(src, "blocked", fmt.Sprintf("f%02d.bin", i)), string(randomBytes(1200, int64(700+i))))
		}
		mustWriteFile(t, filepath.Join(src, "fine.txt"), string(randomBytes(1200, 799)))

		dst = t.TempDir()
		// A regular FILE where the "blocked" directory must be created:
		// every announce under it fails its parent mkdir.
		mustWriteFile(t, filepath.Join(dst, "blocked"), "in the way")
		return src, dst
	}

	for _, quickCheck := range []bool{true, false} {
		quickCheck := quickCheck
		name := "quickcheck"
		if !quickCheck {
			name = "ignore-times"
		}
		t.Run(name, func(t *testing.T) {
			src, dst := build(t)
			var sess *SyncSession
			out := captureLog(t, func() {
				sess = startDirClientExpectFailures(t, src, dst, 2, quickCheck)
			})

			if got := sess.FailedCount(); got != blocked {
				t.Errorf("FailedCount = %d, want %d (every file under the blocked parent)", got, blocked)
			}
			// The unaffected file must still sync normally.
			want, err := os.ReadFile(filepath.Join(src, "fine.txt"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dst, "fine.txt"))
			if err != nil {
				t.Fatalf("fine.txt was not synced: %v", err)
			}
			if string(got) != string(want) {
				t.Error("fine.txt content mismatch")
			}
			// The blocking file must be untouched.
			if b, err := os.ReadFile(filepath.Join(dst, "blocked")); err != nil || string(b) != "in the way" {
				t.Errorf("the pre-existing blocking file was altered: %q, %v", b, err)
			}

			// The operator must be told WHY, at least once.
			if !strings.Contains(out, announceFailedReason) {
				t.Errorf("transfer log never explains the announce-time failure; want it to contain %q.\nLog:\n%s", announceFailedReason, out)
			}
			// ...and must NOT be told once per file. The bounded tail
			// prints at most maxSkipLinesPerReason lines, plus the
			// "further ... will be counted" notice and the aggregated
			// summary, so the reason can legitimately appear a few times
			// past the cap -- but nothing close to once per failed file,
			// which is the flood this exists to prevent.
			perFile := strings.Count(out, announceFailedReason)
			if perFile > maxSkipLinesPerReason+3 {
				t.Errorf("reason appears %d times in the log for %d failed files, want it bounded to ~%d.\nLog:\n%s",
					perFile, blocked, maxSkipLinesPerReason, out)
			}
			if !strings.Contains(out, "rejected by the destination") {
				t.Errorf("transfer log has no aggregated rejection summary.\nLog:\n%s", out)
			}

			// At least one failed file's recorded error must carry the
			// reason, so FailedError() is useful programmatically.
			var explained int
			for id := uint32(0); id < uint32(blocked+8); id++ {
				if err := sess.FailedError(id); err != nil && strings.Contains(err.Error(), announceFailedReason) {
					explained++
				}
			}
			if explained == 0 {
				t.Error("no failed file's FailedError() explains the announce-time failure")
			}

			// The SERVER's own per-connection error line is bounded too:
			// it is the other half of what the operator sees, and it used
			// to print once per refused open with no cap.
			if n := strings.Count(out, "connection error: acquire"); n > maxSkipLinesPerReason {
				t.Errorf("server logged %d unbounded per-connection acquire errors, want <= %d.\nLog:\n%s",
					n, maxSkipLinesPerReason, out)
			}

			if quickCheck {
				// The whole point of the ack's failed-list: these files are
				// resolved from the announce reply, so no worker ever tries
				// to open them (which would kill its connection and force a
				// redial, per file).
				if n := strings.Count(out, "server rejected file-open"); n != 0 {
					t.Errorf("quick-check mode attempted %d file-open(s) for announce-failed files; "+
						"the announce ack must resolve them without any open.\nLog:\n%s", n, out)
				}
				// ...and nothing was transferred for them.
				if got := sess.bytesNet.Load(); got == 0 && sess.FilesChanged() == 0 {
					t.Error("nothing at all was transferred; the unaffected file should still have been sent")
				}
			} else {
				// Without quick-check there is no ack to carry the failed
				// list, so these ARE discovered via a refused open -- but
				// the server must summarize rather than print one line per
				// file.
				if !strings.Contains(out, "refused by this server") {
					t.Errorf("server has no aggregated refusal summary for the opens it refused.\nLog:\n%s", out)
				}
			}

			// An announce-failed file never transferred content, so it must
			// never be counted as a CHANGED file.
			if got := sess.FilesChanged(); got != 1 {
				t.Errorf("FilesChanged = %d, want 1 (only fine.txt); announce-failed files must not count as changed", got)
			}
		})
	}
}

// startDirClientExpectFailures is startDirClientAgainst for a transfer that
// is EXPECTED to end with failed files: it drives the same in-process
// server/client pair but does not assert FailedCount()==0, and returns the
// session so the caller can inspect the failures.
func startDirClientExpectFailures(t *testing.T, srcRoot, dstRoot string, workers int, quickCheck bool) *SyncSession {
	t.Helper()
	resetEngineGlobals()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, workers, false) }()
	waitForListener(t, addr)
	// resetEngineGlobals turns quiet ON, which drops every Log line but
	// READY -- and what this transfer TELLS the operator is exactly what
	// the caller asserts on.
	setQuiet(false)
	t.Cleanup(func() { setQuiet(true) })

	sess, err := startDirClient(srcRoot, addr, 0, false, workers, nil, quickCheck, false, false, false)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	// The server reports the first announce-time failure as its own run
	// error; that is expected here, so it is not asserted clean.
	<-srvErrCh
	return sess
}

// TestE2EDirDeeplyNestedRecursiveTree exercises real recursion depth rather
// than the two-or-three-level trees most tests use: a 12-level chain, each
// level holding files AND a subdirectory, plus sibling branches at several
// depths. This is where per-directory state (mkdirAllOnce's createdDirs
// memo, rejectSymlinkedAncestors' verifiedDirs memo, the keep-set, and the
// walker's relative-path construction) either composes correctly or quietly
// desynchronizes at depth.
func TestE2EDirDeeplyNestedRecursiveTree(t *testing.T) {
	const depth = 12
	src := t.TempDir()

	dir := src
	var wantPaths []string
	for lvl := 0; lvl < depth; lvl++ {
		dir = filepath.Join(dir, fmt.Sprintf("level%02d", lvl))
		mustMkdirAll(t, dir)
		for f := 0; f < 3; f++ {
			rel := filepath.Join(dir, fmt.Sprintf("f%d.bin", f))
			mustWriteFile(t, rel, string(randomBytes(500+lvl*37+f, int64(lvl*10+f))))
			wantPaths = append(wantPaths, rel)
		}
		// A sibling branch every few levels, so the tree is not a single
		// chain and sibling ordering at depth is covered too.
		if lvl%4 == 0 {
			sib := filepath.Join(dir, "branch")
			mustMkdirAll(t, filepath.Join(sib, "inner"))
			mustWriteFile(t, filepath.Join(sib, "inner", "deep.txt"), string(randomBytes(800, int64(900+lvl))))
		}
	}
	// An empty directory at maximum depth must be recreated too.
	mustMkdirAll(t, filepath.Join(dir, "empty-leaf"))

	dst, sess := runDirTransferOpts(t, src, 4, false, false)
	assertTreesEqual(t, src, dst)
	if sess.FailedCount() != 0 {
		t.Fatalf("%d file(s) failed", sess.FailedCount())
	}
	if _, err := os.Stat(filepath.Join(strings.Replace(dir, src, dst, 1), "empty-leaf")); err != nil {
		t.Errorf("empty directory at depth %d was not recreated: %v", depth, err)
	}

	// Idempotent re-run over the same deep tree must transfer nothing.
	sess2 := startDirClientAgainst(t, src, dst, 4, false, nil, true, false, false)
	assertTreesEqual(t, src, dst)
	if got := sess2.FilesChanged(); got != 0 {
		t.Errorf("re-run FilesChanged = %d, want 0 over an unchanged deep tree", got)
	}
}

// TestE2EDirRecursiveTreeWithDeleteAndExcludeAtDepth combines recursion with
// the two features that mutate the destination: an excluded subtree nested
// deep inside a synced one must keep its pre-existing contents, while stale
// entries at several different depths are all removed in the same pass.
func TestE2EDirRecursiveTreeWithDeleteAndExcludeAtDepth(t *testing.T) {
	src := t.TempDir()
	deep := filepath.Join(src, "a", "b", "c", "d")
	mustMkdirAll(t, deep)
	mustWriteFile(t, filepath.Join(src, "a", "top.txt"), string(randomBytes(600, 11)))
	mustWriteFile(t, filepath.Join(src, "a", "b", "mid.txt"), string(randomBytes(600, 12)))
	mustWriteFile(t, filepath.Join(deep, "leaf.txt"), string(randomBytes(600, 13)))
	mustMkdirAll(t, filepath.Join(src, "a", "b", "cache"))
	mustWriteFile(t, filepath.Join(src, "a", "b", "cache", "junk.txt"), "never synced")

	dst := t.TempDir()
	// Pre-existing destination content: one protected (under the excluded
	// dir) and three stale entries at different depths.
	mustMkdirAll(t, filepath.Join(dst, "a", "b", "cache"))
	mustWriteFile(t, filepath.Join(dst, "a", "b", "cache", "keepme.txt"), "pre-existing cache")
	mustWriteFile(t, filepath.Join(dst, "stale-root.txt"), "x")
	mustMkdirAll(t, filepath.Join(dst, "a", "b", "c", "d"))
	mustWriteFile(t, filepath.Join(dst, "a", "b", "c", "d", "stale-deep.txt"), "x")
	mustMkdirAll(t, filepath.Join(dst, "a", "stale-dir", "nested"))

	startDirClientAgainst(t, src, dst, 4, false, []string{"cache"}, false, true, false)

	// Protected by the exclude, at depth.
	got, err := os.ReadFile(filepath.Join(dst, "a", "b", "cache", "keepme.txt"))
	if err != nil || string(got) != "pre-existing cache" {
		t.Errorf("excluded nested dir's contents were disturbed: %q, %v", got, err)
	}
	// Removed by -delete, at three different depths.
	for _, gone := range []string{
		filepath.Join(dst, "stale-root.txt"),
		filepath.Join(dst, "a", "b", "c", "d", "stale-deep.txt"),
		filepath.Join(dst, "a", "stale-dir"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed by -delete (stat err = %v)", gone, err)
		}
	}
	// Everything real still matches.
	for _, rel := range []string{"a/top.txt", "a/b/mid.txt", "a/b/c/d/leaf.txt"} {
		want, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s missing at destination: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s content mismatch", rel)
		}
	}
}

// TestE2EDirManyWorkersRacingSharedTree is the concurrency stress for the
// paths this session changed: far more workers than files (so workers
// contend for the same files and rebind constantly), a mix of file sizes
// spanning single-block and many-block, run through both transfer engines.
// Under -race this is what exercises the pipelined engine's window/gate,
// the per-file remaining counter, the fd budget, and the OrderedWriter's
// file-scoped drain all at once.
func TestE2EDirManyWorkersRacingSharedTree(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		disabled := disabled
		name := "pipelined"
		if disabled {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			withPipeline(t, disabled, func() {
				src := t.TempDir()
				mustMkdirAll(t, filepath.Join(src, "sub", "deeper"))
				// Single-block files (the affinity-routed case).
				for i := 0; i < 30; i++ {
					mustWriteFile(t, filepath.Join(src, fmt.Sprintf("small%02d.bin", i)), string(randomBytes(400+i, int64(i))))
				}
				// Multi-block files (the shared-channel case, where several
				// workers legitimately work on ONE file concurrently).
				for i := 0; i < 3; i++ {
					mustWriteFile(t, filepath.Join(src, "sub", fmt.Sprintf("big%d.bin", i)),
						string(randomBytes(400*1024+i*7919, int64(500+i))))
				}
				mustWriteFile(t, filepath.Join(src, "sub", "deeper", "leaf.bin"), string(randomBytes(70*1024, 42)))
				// A sparse file: zero blocks take their own code path.
				sparse := make([]byte, 300*1024)
				copy(sparse[200*1024:], randomBytes(20*1024, 43))
				mustWriteFile(t, filepath.Join(src, "sub", "sparse.bin"), string(sparse))

				// 24 workers over ~35 files: heavy rebinding and contention.
				dst, sess := runDirTransferOpts(t, src, 24, false, false)
				assertTreesEqual(t, src, dst)
				if sess.FailedCount() != 0 {
					t.Fatalf("%d file(s) failed", sess.FailedCount())
				}

				// Re-run with quick-check: nothing may change, and the
				// counters must agree exactly across all those workers.
				sess2 := startDirClientAgainst(t, src, dst, 24, false, nil, true, false, false)
				assertTreesEqual(t, src, dst)
				if got := sess2.FilesChanged(); got != 0 {
					t.Errorf("FilesChanged = %d, want 0 on an unchanged re-run", got)
				}
				if got := sess2.FailedCount(); got != 0 {
					t.Errorf("FailedCount = %d, want 0", got)
				}

				// Mutate a subset and re-run again: exactly those files,
				// no more, must be reported changed despite the worker
				// count exceeding the changed-file count.
				changed := []string{"small01.bin", "small02.bin", "small03.bin"}
				future := time.Now().Add(3 * time.Second)
				for i, rel := range changed {
					p := filepath.Join(src, rel)
					mustWriteFile(t, p, string(randomBytes(400+i+1, int64(9000+i))))
					if err := os.Chtimes(p, future, future); err != nil {
						t.Fatal(err)
					}
				}
				sess3 := startDirClientAgainst(t, src, dst, 24, false, nil, true, false, false)
				assertTreesEqual(t, src, dst)
				if got := sess3.FilesChanged(); got != uint32(len(changed)) {
					t.Errorf("FilesChanged = %d, want %d", got, len(changed))
				}
			})
		})
	}
}

// TestE2EDirAnnounceFailureUnderManyWorkers is the reported production shape
// in miniature: a large cascade of announce-failed files with far more
// workers than files. Every failed file must be accounted exactly once (no
// double-counting, no lost file, no hang) no matter how many workers race
// the ack path, and the unaffected subtree must still sync completely.
func TestE2EDirAnnounceFailureUnderManyWorkers(t *testing.T) {
	const blocked, fine = 40, 10
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "blocked", "nested"))
	for i := 0; i < blocked/2; i++ {
		mustWriteFile(t, filepath.Join(src, "blocked", fmt.Sprintf("f%02d.bin", i)), string(randomBytes(700, int64(i))))
		mustWriteFile(t, filepath.Join(src, "blocked", "nested", fmt.Sprintf("n%02d.bin", i)), string(randomBytes(700, int64(100+i))))
	}
	mustMkdirAll(t, filepath.Join(src, "ok"))
	for i := 0; i < fine; i++ {
		mustWriteFile(t, filepath.Join(src, "ok", fmt.Sprintf("g%02d.bin", i)), string(randomBytes(700, int64(200+i))))
	}

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "blocked"), "in the way")

	sess := startDirClientExpectFailures(t, src, dst, 32, true)

	if got := sess.FailedCount(); got != blocked {
		t.Errorf("FailedCount = %d, want %d -- every blocked file exactly once under 32 workers", got, blocked)
	}
	if got := sess.FilesChanged(); got != fine {
		t.Errorf("FilesChanged = %d, want %d (only the unaffected subtree)", got, fine)
	}
	// The whole unaffected subtree must be byte-identical.
	assertTreesEqual(t, filepath.Join(src, "ok"), filepath.Join(dst, "ok"))
	// The blocking file is untouched and nothing was created under it.
	if b, err := os.ReadFile(filepath.Join(dst, "blocked")); err != nil || string(b) != "in the way" {
		t.Errorf("blocking file altered: %q, %v", b, err)
	}
}

// TestE2EDirAnnounceFailureRecoversAfterFix is the positive counterpart: the
// destination-side obstruction is a transient environmental condition, so
// once it is cleared a plain re-run must sync everything with no residue of
// the previous failed attempt (no poisoned keep-set, no stale finalized
// state, no files silently skipped as "already handled").
func TestE2EDirAnnounceFailureRecoversAfterFix(t *testing.T) {
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "blocked"))
	for i := 0; i < 6; i++ {
		mustWriteFile(t, filepath.Join(src, "blocked", fmt.Sprintf("f%d.bin", i)), string(randomBytes(900, int64(i))))
	}
	mustWriteFile(t, filepath.Join(src, "fine.txt"), string(randomBytes(900, 77)))

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "blocked"), "in the way")

	first := startDirClientExpectFailures(t, src, dst, 4, true)
	if first.FailedCount() != 6 {
		t.Fatalf("first run FailedCount = %d, want 6", first.FailedCount())
	}

	// Clear the obstruction and re-run normally.
	if err := os.Remove(filepath.Join(dst, "blocked")); err != nil {
		t.Fatal(err)
	}
	second := startDirClientAgainst(t, src, dst, 4, false, nil, true, false, false)
	if got := second.FailedCount(); got != 0 {
		t.Errorf("second run FailedCount = %d, want 0 after the obstruction was removed", got)
	}
	assertTreesEqual(t, src, dst)
}

// TestE2EDirAllFilesFailAtAnnounceStillCompletes is the degenerate negative
// case: NOTHING can be created at the destination. The transfer must still
// terminate cleanly (rather than hanging in WaitAllFinalized waiting for
// files that will never be transferred) and report every file failed.
func TestE2EDirAllFilesFailAtAnnounceStillCompletes(t *testing.T) {
	const n = 8
	src := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "top"))
	for i := 0; i < n; i++ {
		mustWriteFile(t, filepath.Join(src, "top", fmt.Sprintf("f%d.bin", i)), string(randomBytes(500, int64(i))))
	}

	dst := t.TempDir()
	mustWriteFile(t, filepath.Join(dst, "top"), "everything is blocked")

	done := make(chan *SyncSession, 1)
	go func() { done <- startDirClientExpectFailures(t, src, dst, 8, true) }()
	select {
	case sess := <-done:
		if got := sess.FailedCount(); got != n {
			t.Errorf("FailedCount = %d, want %d (every file)", got, n)
		}
		if got := sess.FilesChanged(); got != 0 {
			t.Errorf("FilesChanged = %d, want 0 -- nothing could be written", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("transfer hung when every file failed at announce time")
	}
}
