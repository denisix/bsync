package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Transfer-level benchmarks. Everything here runs the real client and server
// in-process over loopback, exactly like the e2e tests, but timed. They are
// the yardstick for every performance change: run `make bench` before and
// after, compare with benchstat, and keep only what moves a number.
//
// They are skipped under -short because each iteration moves 128 MiB or
// creates 10k files, and `go test ./...` without -bench never runs them.

const (
	benchFileSize  = 128 << 20 // single-file transfer size
	benchBlockSize = 1 << 20   // -b for the single-file benchmarks
	benchTreeFiles = 10000
	benchTreeFile  = 1024 // bytes per small file
)

func benchSkipShort(b *testing.B) {
	b.Helper()
	if testing.Short() {
		b.Skip("transfer benchmarks skipped with -short")
	}
}

// benchSetupGlobals mirrors what main.go establishes before a transfer:
// engine globals reset, block size, prefetch depth derived from workers, no
// progress output.
func benchSetupGlobals(b *testing.B, bs uint32, workers int) {
	b.Helper()
	resetEngineGlobals()
	SetCompressionLevel("default")
	blockSize = bs
	prefetchDepth = workers * 4
	setLogOutput(io.Discard) // keep the servers' READY lines out of the result stream
	b.Cleanup(func() {
		blockSize = 10485760
		prefetchDepth = 0
		setLogOutput(os.Stdout)
	})
}

// benchSourceBytes builds a source image whose blocks cycle through three
// kinds -- incompressible random, highly compressible text, all-zero -- so
// one transfer exercises the raw, compressed and zero wire paths in equal
// measure.
func benchSourceBytes(n, block int, seed int64) []byte {
	out := make([]byte, n)
	rnd := randomBytes(n, seed)
	txt := compressibleBytes(block)
	for off, i := 0, 0; off < n; off, i = off+block, i+1 {
		end := off + block
		if end > n {
			end = n
		}
		switch i % 3 {
		case 0:
			copy(out[off:end], rnd[off:end])
		case 1:
			copy(out[off:end], txt)
		case 2:
			// leave zero
		}
	}
	return out
}

// benchHalfChanged returns a copy of src in which every other block has been
// replaced by fresh random data, so an upload against it sends ~50% of blocks
// and skips the rest after the checksum exchange.
func benchHalfChanged(src []byte, block int, seed int64) []byte {
	dst := append([]byte(nil), src...)
	rnd := randomBytes(len(src), seed)
	for off := 0; off < len(src); off += block * 2 {
		end := off + block
		if end > len(src) {
			end = len(src)
		}
		copy(dst[off:end], rnd[off:end])
	}
	return dst
}

func benchOpenRW(b *testing.B, path string) *os.File {
	b.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		b.Fatalf("open %s: %v", path, err)
	}
	return f
}

// benchUpload runs one timed src->dst upload for each iteration. File setup
// happens with the timer stopped.
func benchUpload(b *testing.B, workers int, dstInit []byte, src []byte) {
	benchSkipShort(b)
	benchSetupGlobals(b, benchBlockSize, workers)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetEngineGlobals()
		srcPath := writeTempFile(b, src)
		dstPath := writeTempFile(b, dstInit)
		srcFile := benchOpenRW(b, srcPath)
		dstFile := benchOpenRW(b, dstPath)
		port := freeTCPPort(b)
		addr := "127.0.0.1:" + port
		srvDone := make(chan error, 1)
		go func() { srvDone <- startServer(dstFile, "127.0.0.1", port, NewChecksumCache(0), workers) }()
		waitForListener(b, addr)
		fileSize := uint64(len(src))
		srcCache := NewChecksumCache(uint32((fileSize - 1) / benchBlockSize))
		b.StartTimer()

		if err := startClient(srcFile, addr, 0, fileSize, benchBlockSize, false, srcCache, workers); err != nil {
			b.Fatalf("client upload: %v", err)
		}
		b.StopTimer() // the transfer is done; server teardown (grace period) is not measured
		select {
		case err := <-srvDone:
			if err != nil {
				b.Fatalf("server: %v", err)
			}
		case <-time.After(60 * time.Second):
			b.Fatal("server did not exit after DONE")
		}

		b.StopTimer()
		srcFile.Close()
		dstFile.Close()
		os.Remove(srcPath)
		os.Remove(dstPath)
		b.StartTimer()
	}
}

// BenchmarkUploadLoopback: ~50% of blocks differ, so the check -> data -> ack
// chain runs for half the blocks and check-only for the other half.
func BenchmarkUploadLoopback(b *testing.B) {
	src := benchSourceBytes(benchFileSize, benchBlockSize, 1)
	dst := benchHalfChanged(src, benchBlockSize, 2)
	for _, w := range []int{1, 4} {
		b.Run(fmt.Sprintf("w%d", w), func(b *testing.B) { benchUpload(b, w, dst, src) })
	}
}

// BenchmarkUploadUnchanged: destination already identical, so the transfer
// is nothing but the per-block checksum round trip. Pure protocol latency.
func BenchmarkUploadUnchanged(b *testing.B) {
	src := benchSourceBytes(benchFileSize, benchBlockSize, 1)
	for _, w := range []int{1, 4} {
		b.Run(fmt.Sprintf("w%d", w), func(b *testing.B) { benchUpload(b, w, src, src) })
	}
}

// BenchmarkDownloadLoopback: server is the source, client writes every block
// (download has no checksum comparison). Mirrors main.go's download-source
// setup.
func BenchmarkDownloadLoopback(b *testing.B) {
	benchSkipShort(b)
	const workers = 1
	benchSetupGlobals(b, benchBlockSize, workers)
	src := benchSourceBytes(benchFileSize, benchBlockSize, 3)
	fileSize := uint64(len(src))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetEngineGlobals()
		srcPath := writeTempFile(b, src)
		dstPath := writeTempFile(b, nil)
		srcFile := benchOpenRW(b, srcPath)
		dstFile := benchOpenRW(b, dstPath)
		port := freeTCPPort(b)
		addr := "127.0.0.1:" + port
		srvDone := make(chan error, 1)
		b.StartTimer()

		go func() {
			srvDone <- startServerUpload(srcFile, "127.0.0.1", port, fileSize, NewChecksumCache(0), workers, false)
		}()
		waitForListener(b, addr)
		if err := startClientDownload(dstFile, addr, 0, benchBlockSize, false, workers); err != nil {
			b.Fatalf("client download: %v", err)
		}
		b.StopTimer() // the transfer is done; the source's idle-exit wait is not measured
		select {
		case err := <-srvDone:
			if err != nil {
				b.Fatalf("server: %v", err)
			}
		case <-time.After(60 * time.Second):
			b.Fatal("download source did not exit")
		}

		b.StopTimer()
		srcFile.Close()
		dstFile.Close()
		os.Remove(srcPath)
		os.Remove(dstPath)
		b.StartTimer()
	}
}

// --- directory-tree benchmarks ---

// benchWriteTree writes n files of size bytes under root, spread over 100
// subdirectories, with content derived from (index, seed). mtime is set to a
// fixed instant so a second tree built with a different seed differs in
// content only -- same names, sizes and timestamps.
func benchWriteTree(b *testing.B, root string, n, size int, seed byte) {
	b.Helper()
	mtime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := make([]byte, size)
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%100))
		if i < 100 {
			if err := os.MkdirAll(dir, 0755); err != nil {
				b.Fatal(err)
			}
		}
		for j := range buf {
			buf[j] = byte(i*31+j) ^ seed
		}
		p := filepath.Join(dir, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(p, buf, 0644); err != nil {
			b.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			b.Fatal(err)
		}
	}
}

// benchTreeSync runs one timed directory sync of srcRoot into dstRoot.
func benchTreeSync(b *testing.B, srcRoot, dstRoot string, workers int, quickCheck bool) *SyncSession {
	b.Helper()
	port := freeTCPPort(b)
	addr := "127.0.0.1:" + port
	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- startDirServer(dstRoot, "127.0.0.1", port, workers, true) }()
	waitForListener(b, addr)

	sess, err := startDirClient(srcRoot, addr, 0, false, workers, nil, quickCheck, false, false, false)
	if err != nil {
		b.Fatalf("startDirClient: %v", err)
	}
	b.StopTimer() // client returned: the tree is fully acknowledged; server exit is not measured
	select {
	case err := <-srvErrCh:
		if err != nil {
			b.Fatalf("startDirServer: %v", err)
		}
	case <-time.After(120 * time.Second):
		b.Fatal("server did not exit after tree done")
	}
	if sess.FailedCount() != 0 {
		b.Fatalf("%d file(s) failed", sess.FailedCount())
	}
	return sess
}

func benchReportFiles(b *testing.B, files int) {
	b.Helper()
	b.ReportMetric(float64(files)*float64(b.N)/b.Elapsed().Seconds(), "files/s")
}

// BenchmarkTreeSmallFilesFresh: 10k x 1 KiB files into an empty destination
// (announce + open + data + ack per file, created-fresh fast path).
func BenchmarkTreeSmallFilesFresh(b *testing.B) {
	benchSkipShort(b)
	srcRoot := b.TempDir()
	benchWriteTree(b, srcRoot, benchTreeFiles, benchTreeFile, 0)
	for _, w := range []int{1, 4} {
		b.Run(fmt.Sprintf("w%d", w), func(b *testing.B) {
			benchSetupGlobals(b, 10485760, w)
			b.SetBytes(int64(benchTreeFiles * benchTreeFile))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetEngineGlobals()
				dstRoot := b.TempDir()
				b.StartTimer()
				benchTreeSync(b, srcRoot, dstRoot, w, true)
				b.StopTimer()
				os.RemoveAll(dstRoot)
				b.StartTimer()
			}
			benchReportFiles(b, benchTreeFiles)
		})
	}
}

// BenchmarkTreeSmallFilesChanged: destination has every file with the same
// name, size and mtime but different content; quick-check off, so each file
// pays the full open -> check -> data -> ack exchange.
func BenchmarkTreeSmallFilesChanged(b *testing.B) {
	benchSkipShort(b)
	srcRoot := b.TempDir()
	benchWriteTree(b, srcRoot, benchTreeFiles, benchTreeFile, 0)
	for _, w := range []int{1, 4} {
		b.Run(fmt.Sprintf("w%d", w), func(b *testing.B) {
			benchSetupGlobals(b, 10485760, w)
			b.SetBytes(int64(benchTreeFiles * benchTreeFile))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetEngineGlobals()
				dstRoot := b.TempDir()
				benchWriteTree(b, dstRoot, benchTreeFiles, benchTreeFile, 0xA5)
				b.StartTimer()
				benchTreeSync(b, srcRoot, dstRoot, w, false)
				b.StopTimer()
				os.RemoveAll(dstRoot)
				b.StartTimer()
			}
			benchReportFiles(b, benchTreeFiles)
		})
	}
}

// BenchmarkTreeQuickCheckNoop: destination already identical (size+mtime
// match), quick-check on. Nothing but walk + announce batches + server stats.
func BenchmarkTreeQuickCheckNoop(b *testing.B) {
	benchSkipShort(b)
	srcRoot := b.TempDir()
	benchWriteTree(b, srcRoot, benchTreeFiles, benchTreeFile, 0)
	dstRoot := b.TempDir()
	benchSetupGlobals(b, 10485760, 1)
	benchTreeSync(b, srcRoot, dstRoot, 1, true) // populate once
	b.SetBytes(int64(benchTreeFiles * benchTreeFile))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetEngineGlobals()
		b.StartTimer()
		benchTreeSync(b, srcRoot, dstRoot, 1, true)
	}
	benchReportFiles(b, benchTreeFiles)
}

// BenchmarkTreeLargeFile: one 128 MiB file inside a tree -- the directory
// mode block path (dir-server OrderedWriter, per-block check/data/ack) on a
// large file, for comparison with BenchmarkUploadLoopback.
func BenchmarkTreeLargeFile(b *testing.B) {
	benchSkipShort(b)
	srcRoot := b.TempDir()
	src := benchSourceBytes(benchFileSize, benchBlockSize, 7)
	if err := os.WriteFile(filepath.Join(srcRoot, "big.img"), src, 0644); err != nil {
		b.Fatal(err)
	}
	for _, w := range []int{1, 4} {
		b.Run(fmt.Sprintf("w%d", w), func(b *testing.B) {
			benchSetupGlobals(b, benchBlockSize, w)
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetEngineGlobals()
				dstRoot := b.TempDir()
				b.StartTimer()
				benchTreeSync(b, srcRoot, dstRoot, w, true)
				b.StopTimer()
				os.RemoveAll(dstRoot)
				b.StartTimer()
			}
		})
	}
}
