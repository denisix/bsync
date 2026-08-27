package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// runDirTransfer starts a directory-mode server on a free loopback port,
// runs startDirClient against it, and returns the destination root and the
// client's SyncSession once the transfer completes (or fails).
func runDirTransfer(t *testing.T, srcRoot string, workers int, noCompress bool) (string, *SyncSession) {
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

	sess, err := startDirClient(srcRoot, addr, 0, noCompress, workers, nil)
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
	return dstRoot, sess
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

// assertTreesEqual compares src and dst as sets of relative file paths and
// byte content. It does not compare directory entries directly (an empty
// directory produces no file-path entry either side), so empty-dir
// preservation needs its own assertion.
func assertTreesEqual(t *testing.T, src, dst string) {
	t.Helper()
	srcFiles := listTreeFiles(t, src)
	dstFiles := listTreeFiles(t, dst)
	if len(srcFiles) != len(dstFiles) {
		t.Fatalf("file count mismatch: src has %d (%v), dst has %d (%v)", len(srcFiles), srcFiles, len(dstFiles), dstFiles)
	}
	for i := range srcFiles {
		if srcFiles[i] != dstFiles[i] {
			t.Fatalf("path set mismatch: src %v, dst %v", srcFiles, dstFiles)
		}
	}
	for _, rel := range srcFiles {
		want, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatalf("read src %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read dst %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("content mismatch for %s: src %d bytes, dst %d bytes", rel, len(want), len(got))
		}
	}
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

	sess2, err := startDirClient(src, addr, 0, false, 2, nil)
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

	sess, err := startDirClient(src, addr, 0, false, 1, nil)
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

	sess, err := startDirClient(src, proxyAddr, 0, true, 1, nil)
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
	if _, err := startDirClient(src, "127.0.0.1:1", 0, false, 0, nil); err == nil {
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
	_, err := startDirClient(src, "127.0.0.1:"+port, 0, false, 1, nil)
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
	if _, err := startDirClient(src, "this-host-does-not-resolve.invalid:9999", 0, false, 1, nil); err == nil {
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

	sess2, err := startDirClient(src, addr, 0, true, 2, nil)
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

	sess, err := startDirClient(src, addr, 0, true, 1, nil)
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

	sess, err := startDirClient(src, addr, 0, true, 1, nil)
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

	sess, err := startDirClient(src, addr, 0, false, 2, nil)
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

		sess, err := startDirClient(src, proxyAddr, 0, true, 2, nil)
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
func TestE2EDirUnreadableSourceFileIsSkippedNotFailed(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "good1.txt"), "one")
	badPath := filepath.Join(src, "unreadable.txt")
	mustWriteFile(t, badPath, "will not be readable")
	mustWriteFile(t, filepath.Join(src, "good2.txt"), "two")

	if err := os.Chmod(badPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(badPath, 0644) }) // let t.TempDir's cleanup remove it

	resetEngineGlobals()
	dst := t.TempDir()
	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
	waitForListener(t, addr)

	sess, err := startDirClient(src, addr, 0, false, 2, nil)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Errorf("FailedCount = %d, want 0 -- an unreadable file is skipped at walk time, not recorded as a failure", sess.FailedCount())
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
	if _, err := os.Stat(filepath.Join(dst, "unreadable.txt")); err == nil {
		t.Error("unreadable.txt should not exist at the destination")
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
		sess, clientErr = startDirClient(src, addr, 0, true, 2, nil)
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

	sess, err := startDirClient(src, addr, 0, true, 1, nil)
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
		sess, clientErr = startDirClient(src, addr, 0, true, 4, nil)
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
	// walkSource's own (separate, metadata-only) open+close, or this hits
	// the walk-time skip path instead (already covered by
	// TestE2EDirUnreadableSourceFileIsSkippedNotFailed). walkSource itself
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
		sess, clientErr = startDirClient(src, proxyAddr, 0, true, 1, nil)
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
		if err != nil {
			t.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startDirServer did not complete after the client finished")
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
	sess, err := startDirClient(src, addr, blockCap, true, 1, nil)
	if err != nil {
		t.Fatalf("startDirClient: %v", err)
	}
	if err := <-srvErrCh; err != nil {
		t.Fatalf("startDirServer: %v", err)
	}
	if sess.FailedCount() != 0 {
		t.Fatalf("%d failed file(s)", sess.FailedCount())
	}
	fe := sess.FileByID(0)
	if fe == nil {
		t.Fatal("FileID 0 not found")
	}
	if fe.BlockSize != blockCap {
		t.Errorf("BlockSize = %d, want exactly %d (the cap passed to startDirClient)", fe.BlockSize, blockCap)
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

	sess, err := startDirClient(src, addr, 0, true, 1, []string{"*.log"})
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

	sess, err := startDirClient(src, addr, 0, true, 1, []string{"*some_file_name*"})
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

	sess, err := startDirClient(src, proxyAddr, 2048, true, 1, nil)
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

	sess, err := startDirClient(src, proxyAddr, 0, true, 1, nil)
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
	var badID, goodID uint32 = 1<<32 - 1, 1<<32 - 1
	for i := 0; i < sess.FileCount(); i++ {
		fe := sess.FileByID(uint32(i))
		switch fe.RelPath {
		case "a-bad.txt":
			badID = fe.ID
		case "b-good.txt":
			goodID = fe.ID
		}
	}
	if badID == 1<<32-1 || goodID == 1<<32-1 {
		t.Fatalf("could not find both files by RelPath")
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

	sess, err := startDirClient(src, addr, 0, true, 6, nil)
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
