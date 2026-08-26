package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

// drainWalk runs walkSource to completion and returns everything it
// emitted, plus every skip reason reported along the way (formatted as
// "relPath: reason" for easy substring assertions).
func drainWalk(t *testing.T, root string, excl []string) ([]*FileEntry, []string) {
	t.Helper()
	out := make(chan *FileEntry, 64)
	var mu sync.Mutex
	var skips []string
	onSkip := func(relPath, reason string) {
		mu.Lock()
		skips = append(skips, relPath+": "+reason)
		mu.Unlock()
	}
	errCh := make(chan error, 1)
	go func() { errCh <- walkSource(root, excl, 0, out, onSkip) }()
	var entries []*FileEntry
	for fe := range out {
		entries = append(entries, fe)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("walkSource: %v", err)
	}
	return entries, skips
}

func relPaths(entries []*FileEntry) []string {
	out := make([]string, len(entries))
	for i, fe := range entries {
		out[i] = fe.RelPath
	}
	return out
}

func assertStringSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v, want %v", i, got, want)
		}
	}
}

// --- sanitizeRelPath ---

func TestSanitizeRelPath(t *testing.T) {
	valid := map[string]string{
		"a":                "a",
		"a/b/c":            "a/b/c",
		"dir/file.txt":     "dir/file.txt",
		"ünïcödé/файл.bin": "ünïcödé/файл.bin",
		"a b/c d.txt":      "a b/c d.txt",
		"weird;punct'name": "weird;punct'name",
		"CONTACT.txt":      "CONTACT.txt", // not literally "CON"
	}
	for in, want := range valid {
		got, err := sanitizeRelPath(in)
		if err != nil {
			t.Errorf("sanitizeRelPath(%q): unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("sanitizeRelPath(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"/etc/passwd",
		"//etc/passwd",
		"../etc/passwd",
		"a/../../b",
		"a/b/../../../c",
		"a/./b",
		"a//b",
		"a/",
		"C:\\Windows",
		"a\\b",
		"a\x00b",
		"CON",
		"con",
		"NUL.txt",
		"COM1",
		"lpt3",
		"trailing.",
		"trailing ",
	}
	for _, in := range invalid {
		if _, err := sanitizeRelPath(in); err == nil {
			t.Errorf("sanitizeRelPath(%q) unexpectedly accepted", in)
		}
	}

	// A component right at the length boundary and one over it.
	longOK := make([]byte, maxPathComponentLen)
	for i := range longOK {
		longOK[i] = 'a'
	}
	if _, err := sanitizeRelPath(string(longOK)); err != nil {
		t.Errorf("component of exactly %d bytes rejected: %v", maxPathComponentLen, err)
	}
	longBad := append(longOK, 'a')
	if _, err := sanitizeRelPath(string(longBad)); err == nil {
		t.Errorf("component of %d bytes unexpectedly accepted", len(longBad))
	}
}

// --- treeBlockSize ---

func TestTreeBlockSize(t *testing.T) {
	const cap10M = 10 * 1024 * 1024
	tests := []struct {
		size uint64
		cap  uint32
		want uint32
	}{
		{0, cap10M, minTreeBlock},
		{1, cap10M, minTreeBlock},
		{minTreeBlock, cap10M, minTreeBlock},
		{minTreeBlock + 1, cap10M, minTreeBlock * 2},
		{1 << 20, cap10M, 1 << 20}, // 1MB file: doubling from minTreeBlock lands exactly on size
		{cap10M, cap10M, cap10M},
		{cap10M * 10, cap10M, cap10M}, // large file clamps to the cap
		{100, 4096, 4096},             // cap below minTreeBlock: honor the explicit smaller cap
		{0, 4096, 4096},
	}
	for _, tt := range tests {
		got := treeBlockSize(tt.size, tt.cap)
		if got != tt.want {
			t.Errorf("treeBlockSize(%d, %d) = %d, want %d", tt.size, tt.cap, got, tt.want)
		}
		if got == 0 {
			t.Errorf("treeBlockSize(%d, %d) returned 0", tt.size, tt.cap)
		}
	}
}

func TestTreeBlockSizeZeroCapUsesPackageDefault(t *testing.T) {
	saved := blockSize
	defer func() { blockSize = saved }()
	blockSize = 5 * 1024 * 1024
	if got := treeBlockSize(100*1024*1024, 0); got != blockSize {
		t.Errorf("treeBlockSize with cap=0 = %d, want package default %d", got, blockSize)
	}
}

// --- walkSource: ordering, IDs, empty dirs ---

func TestWalkTreeOrderingAndIDs(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "a"))
	mustMkdirAll(t, filepath.Join(root, "b"))
	mustWriteFile(t, filepath.Join(root, "a", "1.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "a", "2.txt"), "y")
	mustWriteFile(t, filepath.Join(root, "b", "3.txt"), "z")
	mustWriteFile(t, filepath.Join(root, "top.txt"), "w")

	want := []string{"a", "a/1.txt", "a/2.txt", "b", "b/3.txt", "top.txt"}

	for i := 0; i < 2; i++ {
		entries, skips := drainWalk(t, root, nil)
		if len(skips) != 0 {
			t.Fatalf("run %d: unexpected skips: %v", i, skips)
		}
		assertStringSlicesEqual(t, relPaths(entries), want)
		for idx, fe := range entries {
			if fe.ID != uint32(idx) {
				t.Errorf("run %d: entry %d (%s) has ID %d, want %d", i, idx, fe.RelPath, fe.ID, idx)
			}
		}
	}
}

func TestWalkTreeRootNeverEmitted(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "f.txt"), "x")
	entries, _ := drainWalk(t, root, nil)
	for _, fe := range entries {
		if fe.RelPath == "" || fe.RelPath == "." || fe.AbsPath == root {
			t.Errorf("root was emitted as an entry: %+v", fe)
		}
	}
}

func TestWalkTreeEmptyDirs(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "empty"))
	mustMkdirAll(t, filepath.Join(root, "nested", "also-empty"))

	entries, skips := drainWalk(t, root, nil)
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	byPath := map[string]*FileEntry{}
	for _, fe := range entries {
		byPath[fe.RelPath] = fe
	}
	for _, want := range []string{"empty", "nested", "nested/also-empty"} {
		fe, ok := byPath[want]
		if !ok {
			t.Errorf("empty directory %q was not emitted", want)
			continue
		}
		if fe.Kind != KindDir {
			t.Errorf("%q: Kind = %v, want KindDir", want, fe.Kind)
		}
	}
}

func TestWalkTreeFileMetadataCaptured(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "f.txt"), "hello world")
	entries, skips := drainWalk(t, root, nil)
	if len(skips) != 0 {
		t.Fatalf("unexpected skips: %v", skips)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	fe := entries[0]
	if fe.Kind != KindFile {
		t.Errorf("Kind = %v, want KindFile", fe.Kind)
	}
	if fe.Size != 11 {
		t.Errorf("Size = %d, want 11", fe.Size)
	}
	if fe.ModTime.IsZero() {
		t.Error("ModTime is zero")
	}
	if fe.BlockSize == 0 {
		t.Error("BlockSize is 0 for a non-empty file")
	}
	if fe.Blocks == 0 {
		t.Error("Blocks is 0 for a non-empty file")
	}
}

// TestWalkTreeRespectsBlockSizeCap is a regression test for B1: walkSource
// used to compute every file's block size from the package-global
// blockSize directly, ignoring the blockSizeCap argument entirely -- so a
// caller-supplied cap silently had no effect on actual per-file block
// sizes (only on the TreeReader byte budget, which used blockSizeCap
// correctly all along). Kept well below the package global's own default
// (10 MB) so this can't pass by coincidence.
func TestWalkTreeRespectsBlockSizeCap(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "f.bin"), string(make([]byte, 200000))) // 200KB
	const blockCap = 8192
	out := make(chan *FileEntry, 8)
	errCh := make(chan error, 1)
	go func() { errCh <- walkSource(root, nil, blockCap, out, nil) }()
	var entries []*FileEntry
	for fe := range out {
		entries = append(entries, fe)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("walkSource: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].BlockSize != blockCap {
		t.Errorf("BlockSize = %d, want exactly %d (the cap, for a 200KB file that exceeds it)", entries[0].BlockSize, blockCap)
	}
}

func TestWalkTreeEmptyFileHasZeroBlocks(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "empty.txt"), "")
	entries, _ := drainWalk(t, root, nil)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Blocks != 0 {
		t.Errorf("Blocks = %d, want 0 for an empty file", entries[0].Blocks)
	}
}

// --- walkSource: skips ---

func TestWalkTreeSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "real.txt"), "x")
	mustMkdirAll(t, filepath.Join(root, "realdir"))
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link-to-file")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "realdir"), filepath.Join(root, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}

	entries, skips := drainWalk(t, root, nil)
	for _, fe := range entries {
		if fe.RelPath == "link-to-file" || fe.RelPath == "link-to-dir" || fe.RelPath == "dangling" {
			t.Errorf("symlink %q was emitted as a real entry", fe.RelPath)
		}
	}
	wantSkipped := map[string]bool{"link-to-file": false, "link-to-dir": false, "dangling": false}
	for _, s := range skips {
		for name := range wantSkipped {
			if len(s) >= len(name) && s[:len(name)] == name {
				wantSkipped[name] = true
			}
		}
	}
	for name, seen := range wantSkipped {
		if !seen {
			t.Errorf("expected a skip report for %q, skips were: %v", name, skips)
		}
	}
	// The dir symlink must not have been descended into.
	for _, fe := range entries {
		if fe.RelPath == "link-to-dir/real.txt" {
			t.Error("descended into a symlinked directory")
		}
	}
}

func TestWalkTreeSkipsFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs are unix-only")
	}
	root := t.TempDir()
	fifoPath := filepath.Join(root, "myfifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	entries, skips := drainWalk(t, root, nil)
	if len(entries) != 0 {
		t.Errorf("FIFO was emitted as a real entry: %+v", entries)
	}
	found := false
	for _, s := range skips {
		if len(s) >= 6 && s[:6] == "myfifo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a skip report for the FIFO, skips were: %v", skips)
	}
}

func TestWalkTreeExcluded(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "skip.log"), "x")
	mustMkdirAll(t, filepath.Join(root, "skipdir"))
	mustWriteFile(t, filepath.Join(root, "skipdir", "inside.txt"), "x")

	entries, skips := drainWalk(t, root, []string{"*.log", "skipdir"})
	got := relPaths(entries)
	for _, p := range got {
		if p == "skip.log" || p == "skipdir" || p == "skipdir/inside.txt" {
			t.Errorf("excluded path %q was emitted", p)
		}
	}
	foundKeep := false
	for _, p := range got {
		if p == "keep.txt" {
			foundKeep = true
		}
	}
	if !foundKeep {
		t.Errorf("non-excluded path missing from %v", got)
	}
	if len(skips) == 0 {
		t.Error("expected excluded paths to be reported via onSkip")
	}
}

func TestWalkTreeErrorInSubdirContinues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits behave differently on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't restrict access")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	mustMkdirAll(t, blocked)
	mustWriteFile(t, filepath.Join(blocked, "secret.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "visible.txt"), "x")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(blocked, 0700) // let TempDir cleanup remove it

	entries, skips := drainWalk(t, root, nil)
	got := relPaths(entries)
	foundVisible := false
	for _, p := range got {
		if p == "visible.txt" {
			foundVisible = true
		}
		if p == "blocked/secret.txt" {
			t.Error("read through a permission-denied directory")
		}
	}
	if !foundVisible {
		t.Errorf("walk did not continue past the unreadable directory: %v", got)
	}
	if len(skips) == 0 {
		t.Error("expected the unreadable directory to be reported via onSkip")
	}
}

// --- walkSource: whole-walk errors ---

func TestWalkTreeNonexistentRoot(t *testing.T) {
	out := make(chan *FileEntry, 1)
	err := walkSource(filepath.Join(t.TempDir(), "does-not-exist"), nil, 0, out, nil)
	if err == nil {
		t.Fatal("expected an error for a nonexistent root")
	}
	if _, ok := <-out; ok {
		t.Error("channel was not closed / had unexpected content")
	}
}

func TestWalkTreeRootIsAFile(t *testing.T) {
	path := writeTempFile(t, []byte("not a directory"))
	out := make(chan *FileEntry, 1)
	err := walkSource(path, nil, 0, out, nil)
	if err == nil {
		t.Fatal("expected an error when root is a regular file")
	}
}

// --- small local fixture helpers (kept local to this file; e2e_test.go's
// helpers are shaped around single flat files, not trees) ---

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
