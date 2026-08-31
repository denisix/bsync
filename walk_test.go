package main

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// drainWalk runs walkSource to completion and returns everything it
// emitted, plus every skip reason reported along the way (formatted as
// "relPath: reason" for easy substring assertions).
func drainWalk(t *testing.T, root string, excl []string) ([]*FileEntry, []string) {
	t.Helper()
	return drainWalkLinks(t, root, excl, false)
}

// drainWalkLinks is drainWalk with explicit control over -links symlink
// syncing, for the tests that need it enabled. specials is variadic so the
// existing -links call sites stay untouched; pass a single true to enable
// -specials too.
func drainWalkLinks(t *testing.T, root string, excl []string, links bool, specials ...bool) ([]*FileEntry, []string) {
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
	syncSpecials := len(specials) > 0 && specials[0]
	go func() { errCh <- walkSource(root, excl, 0, links, syncSpecials, out, onSkip) }()
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
		"a\x00b",
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

// TestWalkTreeIDsStayDenseAcrossSkippedEntries proves a KindSkipped entry
// (from skipKept) consumes an ID from the exact same counter every other
// branch uses -- SyncSession.AddFile requires IDs to be dense and
// sequential in emission order regardless of Kind, so a skipped entry
// stealing or skipping a slot would break every subsequent real entry's ID.
func TestWalkTreeIDsStayDenseAcrossSkippedEntries(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "skip.log"), "x")
	mustWriteFile(t, filepath.Join(root, "b.txt"), "y")

	entries, _ := drainWalk(t, root, []string{"*.log"})
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (2 real + 1 skipped placeholder): %+v", len(entries), entries)
	}
	for idx, fe := range entries {
		if fe.ID != uint32(idx) {
			t.Errorf("entry %d (%s, kind %v) has ID %d, want %d", idx, fe.RelPath, fe.Kind, fe.ID, idx)
		}
	}
}

// TestWalkTreeLinksEmitsSymlinksWithVerbatimTargets pins -links walking:
// relative, absolute, and dangling symlinks are all emitted as KindSymlink
// entries carrying the EXACT os.Readlink string -- never resolved, never
// followed (a symlinked directory's contents must not be walked), and IDs
// stay dense across the mix of kinds.
func TestWalkTreeLinksEmitsSymlinksWithVerbatimTargets(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "target.txt"), "content")
	mustMkdirAll(t, filepath.Join(root, "realdir"))
	mustWriteFile(t, filepath.Join(root, "realdir", "inner.txt"), "x")
	if err := os.Symlink("target.txt", filepath.Join(root, "rel-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("does-not-exist", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realdir", filepath.Join(root, "dir-link")); err != nil {
		t.Fatal(err)
	}

	entries, skips := drainWalkLinks(t, root, nil, true)
	if len(skips) != 0 {
		t.Fatalf("unexpected skips with -links: %v", skips)
	}
	byPath := map[string]*FileEntry{}
	for idx, fe := range entries {
		byPath[fe.RelPath] = fe
		if fe.ID != uint32(idx) {
			t.Errorf("entry %d (%s) has ID %d, want %d -- IDs must stay dense", idx, fe.RelPath, fe.ID, idx)
		}
	}
	wantLinks := map[string]string{
		"rel-link": "target.txt",
		"abs-link": "/etc/hostname",
		"dangling": "does-not-exist",
		"dir-link": "realdir",
	}
	for rel, wantTarget := range wantLinks {
		fe, ok := byPath[rel]
		if !ok {
			t.Errorf("symlink %q was not emitted", rel)
			continue
		}
		if fe.Kind != KindSymlink {
			t.Errorf("%q: Kind = %v, want KindSymlink", rel, fe.Kind)
		}
		if fe.LinkTarget != wantTarget {
			t.Errorf("%q: LinkTarget = %q, want %q (verbatim)", rel, fe.LinkTarget, wantTarget)
		}
	}
	// The symlinked directory's CONTENTS must not have been walked through
	// the link (only through the real path).
	if _, ok := byPath["dir-link/inner.txt"]; ok {
		t.Error("walked through a symlinked directory")
	}
	if fe, ok := byPath["realdir/inner.txt"]; !ok || fe.Kind != KindFile {
		t.Error("the real directory's content is missing")
	}
}

// TestWalkTreeLinksOffKeepsSkippedPlaceholders: without -links the symlink
// behavior is byte-for-byte the pre-existing one -- a KindSkipped
// placeholder, no target carried.
func TestWalkTreeLinksOffKeepsSkippedPlaceholders(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "target.txt"), "content")
	if err := os.Symlink("target.txt", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	entries, _ := drainWalkLinks(t, root, nil, false)
	for _, fe := range entries {
		if fe.RelPath == "link" {
			if fe.Kind != KindSkipped {
				t.Errorf("link Kind = %v, want KindSkipped without -links", fe.Kind)
			}
			if fe.LinkTarget != "" {
				t.Errorf("link LinkTarget = %q, want empty without -links", fe.LinkTarget)
			}
		}
	}
}

func TestWalkTreeRootNeverEmitted(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "f.txt"), "x")
	entries, _ := drainWalk(t, root, nil)
	for _, fe := range entries {
		if fe.RelPath == "" || fe.RelPath == "." {
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
	go func() { errCh <- walkSource(root, nil, blockCap, false, false, out, nil) }()
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
	// A symlink is announced as a KindSkipped placeholder (so a later
	// -delete pass knows to leave its destination-side namesake alone), but
	// never as a real KindFile/KindDir entry -- its blocks are never synced
	// and MkdirAll/open is never called for it.
	for _, fe := range entries {
		if fe.RelPath == "link-to-file" || fe.RelPath == "link-to-dir" || fe.RelPath == "dangling" {
			if fe.Kind != KindSkipped {
				t.Errorf("symlink %q was emitted as Kind %v, want KindSkipped", fe.RelPath, fe.Kind)
			}
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
	if len(entries) != 1 || entries[0].RelPath != "myfifo" || entries[0].Kind != KindSkipped {
		t.Errorf("entries = %+v, want exactly one KindSkipped entry for myfifo", entries)
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
	// An excluded path is announced as a KindSkipped placeholder (protecting
	// it from a later -delete pass), never as a real KindFile/KindDir entry
	// whose content actually gets synced. "skipdir/inside.txt" must not
	// appear at all -- excluding a directory means never descending into
	// it, not emitting a placeholder for every path underneath it too.
	byPath := map[string]*FileEntry{}
	for _, fe := range entries {
		byPath[fe.RelPath] = fe
	}
	for _, p := range []string{"skip.log", "skipdir"} {
		fe, ok := byPath[p]
		if !ok {
			t.Errorf("excluded path %q missing a KindSkipped placeholder entry", p)
			continue
		}
		if fe.Kind != KindSkipped {
			t.Errorf("excluded path %q emitted as Kind %v, want KindSkipped", p, fe.Kind)
		}
	}
	if _, ok := byPath["skipdir/inside.txt"]; ok {
		t.Error("descended into an excluded directory and emitted an entry for its content")
	}
	if fe, ok := byPath["keep.txt"]; !ok || fe.Kind != KindFile {
		t.Errorf("non-excluded path missing or wrong kind: %+v", byPath["keep.txt"])
	}
	if len(skips) == 0 {
		t.Error("expected excluded paths to be reported via onSkip")
	}
}

// TestWalkTreeExcludedSubstringGlob proves the exact syntax a --exclude user
// would reach for to match "anything containing this substring" --
// *some_file_name* -- actually works, at both the top level (matched via
// excluded's full-path arm, which for a root-level entry is identical to its
// basename) and nested (matched via the basename arm only, since path.Match's
// '*' does not cross '/').
func TestWalkTreeExcludedSubstringGlob(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "my_some_file_name_v2.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "unrelated.txt"), "x")
	mustMkdirAll(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "sub", "also_some_file_name.log"), "x")

	entries, skips := drainWalk(t, root, []string{"*some_file_name*"})
	byPath := map[string]*FileEntry{}
	for _, fe := range entries {
		byPath[fe.RelPath] = fe
	}
	for _, p := range []string{"my_some_file_name_v2.txt", "sub/also_some_file_name.log"} {
		fe, ok := byPath[p]
		if !ok {
			t.Errorf("excluded path %q missing a KindSkipped placeholder entry", p)
			continue
		}
		if fe.Kind != KindSkipped {
			t.Errorf("excluded path %q emitted as Kind %v, want KindSkipped", p, fe.Kind)
		}
	}
	if fe, ok := byPath["unrelated.txt"]; !ok || fe.Kind != KindFile {
		t.Errorf("non-excluded path missing or wrong kind: %+v", byPath["unrelated.txt"])
	}
	wantSkipReasons := 2 // my_some_file_name_v2.txt + sub/also_some_file_name.log
	if len(skips) != wantSkipReasons {
		t.Errorf("skips = %v, want %d entries", skips, wantSkipReasons)
	}
}

// TestExcludedFullPathArmDoesNotCrossSlash directly exercises excluded()
// (not through a walk) to prove its two matching arms -- full relative path,
// and basename -- are independent, and that '*' correctly does not cross
// '/': a pattern anchored to a subdirectory must not accidentally match a
// same-named file living elsewhere in the tree.
func TestExcludedFullPathArmDoesNotCrossSlash(t *testing.T) {
	pattern := []string{"sub/*.tmp"}
	if !excluded("sub/x.tmp", pattern) {
		t.Error(`excluded("sub/x.tmp", ["sub/*.tmp"]) = false, want true (full-path arm)`)
	}
	if excluded("x.tmp", pattern) {
		t.Error(`excluded("x.tmp", ["sub/*.tmp"]) = true, want false (pattern is anchored to sub/, must not match the root-level file)`)
	}
	if excluded("other/sub/x.tmp", pattern) {
		t.Error(`excluded("other/sub/x.tmp", ["sub/*.tmp"]) = true, want false ('*' in the glob must not cross '/', and the full path doesn't match either)`)
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
	err := walkSource(filepath.Join(t.TempDir(), "does-not-exist"), nil, 0, false, false, out, nil)
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
	err := walkSource(path, nil, 0, false, false, out, nil)
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

// TestEstimateTreeMatchesWalkSource pins estimateTree's contract: on a
// stable tree it must count exactly the entries walkSource emits as
// KindFile, and sum exactly their sizes -- dirs, excluded paths, symlinks,
// and the contents of excluded directories all filtered identically. If the
// two ever drift, the progress display's estimated totals silently stop
// meaning "what this transfer will actually process".
func TestEstimateTreeMatchesWalkSource(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sub", "deeper"))
	mustWriteFile(t, filepath.Join(root, "a.txt"), "aaaa")
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), "bbbbbbbb")
	mustWriteFile(t, filepath.Join(root, "sub", "deeper", "c.txt"), "c")
	mustWriteFile(t, filepath.Join(root, "empty.txt"), "") // zero-byte files still count as files
	mustWriteFile(t, filepath.Join(root, "skip.log"), "excluded")
	mustMkdirAll(t, filepath.Join(root, "skipdir"))
	mustWriteFile(t, filepath.Join(root, "skipdir", "hidden.txt"), "never counted")
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link.txt")); err == nil {
		// Symlinks are skipped by both walkers; created best-effort so the
		// test still runs where symlinks are unsupported.
		_ = err
	}

	excl := []string{"*.log", "skipdir"}

	entries, _ := drainWalk(t, root, excl)
	var wantFiles, wantBytes uint64
	for _, fe := range entries {
		if fe.Kind == KindFile {
			wantFiles++
			wantBytes += fe.Size
		}
	}
	if wantFiles == 0 {
		t.Fatal("test fixture problem: walkSource emitted no files")
	}

	var gotFiles, gotBytes uint64
	estimateTree(root, excl, func(size uint64) {
		gotFiles++
		gotBytes += size
	})
	if gotFiles != wantFiles || gotBytes != wantBytes {
		t.Errorf("estimateTree = %d files / %d bytes, walkSource emitted %d files / %d bytes",
			gotFiles, gotBytes, wantFiles, wantBytes)
	}
}

// TestEstimateTreeNonexistentRootIsANoOp: best-effort means best-effort --
// a root that doesn't exist must produce zero callbacks, not a panic or
// error (the real walk reports that failure; the estimator never does).
func TestEstimateTreeNonexistentRoot(t *testing.T) {
	calls := 0
	estimateTree(filepath.Join(t.TempDir(), "does-not-exist"), nil, func(uint64) { calls++ })
	if calls != 0 {
		t.Errorf("estimateTree called onFile %d times for a nonexistent root, want 0", calls)
	}
}

// mkFifo creates a named pipe, skipping the test where the platform or
// filesystem won't allow one.
func mkFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
}

// mkSocket creates a unix socket file by binding a listener and closing it,
// which leaves the filesystem node behind -- deliberately a DIFFERENT
// mechanism from the mknod the server under test uses, so the test doesn't
// simply mirror the implementation.
func mkSocket(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unsupported: %v", err)
	}
	// Go unlinks a unix listener's socket file on Close by default, which
	// would delete the very node this helper exists to leave behind.
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	l.Close()
}

func TestWalkTreeSpecialsEmitsFifosAndSockets(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "real.txt"), "data")
	mkFifo(t, filepath.Join(root, "pipe"))
	mkSocket(t, filepath.Join(root, "sock"))

	entries, _ := drainWalkLinks(t, root, nil, false, true)

	kinds := map[string]EntryKind{}
	modes := map[string]os.FileMode{}
	for _, fe := range entries {
		kinds[fe.RelPath] = fe.Kind
		modes[fe.RelPath] = fe.Mode
	}
	if kinds["pipe"] != KindSpecial {
		t.Errorf("pipe kind = %v, want KindSpecial", kinds["pipe"])
	}
	if kinds["sock"] != KindSpecial {
		t.Errorf("sock kind = %v, want KindSpecial", kinds["sock"])
	}
	// The node type must ride in Mode -- that is the whole reason no wire
	// layout change was needed.
	if modes["pipe"]&os.ModeNamedPipe == 0 {
		t.Errorf("pipe mode %v lost its ModeNamedPipe bit", modes["pipe"])
	}
	if modes["sock"]&os.ModeSocket == 0 {
		t.Errorf("sock mode %v lost its ModeSocket bit", modes["sock"])
	}
	if kinds["real.txt"] != KindFile {
		t.Errorf("real.txt kind = %v, want KindFile", kinds["real.txt"])
	}
}

func TestWalkTreeSpecialsOffKeepsSkippedPlaceholders(t *testing.T) {
	root := t.TempDir()
	mkFifo(t, filepath.Join(root, "pipe"))
	mkSocket(t, filepath.Join(root, "sock"))

	entries, skips := drainWalkLinks(t, root, nil, false)

	for _, fe := range entries {
		if fe.RelPath == "pipe" || fe.RelPath == "sock" {
			if fe.Kind != KindSkipped {
				t.Errorf("%s kind = %v, want KindSkipped without -specials", fe.RelPath, fe.Kind)
			}
		}
	}
	joined := strings.Join(skips, "\n")
	if !strings.Contains(joined, "pipe: unsupported file type") ||
		!strings.Contains(joined, "sock: unsupported file type") {
		t.Errorf("skips did not report both special files:\n%s", joined)
	}
}

// TestWalkTreeSpecialsLeavesDeviceNodesSkipped: -specials is FIFOs and
// sockets only. A device node needs a major/minor on the wire and CAP_MKNOD
// at the destination (rsync's separate --devices), so it stays a placeholder
// even with -specials on.
func TestWalkTreeSpecialsLeavesDeviceNodesSkipped(t *testing.T) {
	if !isSpecialMode(os.ModeNamedPipe) || !isSpecialMode(os.ModeSocket) {
		t.Fatal("isSpecialMode must accept FIFOs and sockets")
	}
	for _, mode := range []os.FileMode{os.ModeDevice, os.ModeDevice | os.ModeCharDevice, os.ModeIrregular} {
		if isSpecialMode(mode) {
			t.Errorf("isSpecialMode(%v) = true, want false", mode)
		}
	}
}

// windowsPortabilityPaths are structurally safe names that only WINDOWS
// cannot represent: reserved device names, trailing dots or spaces, and a
// literal backslash in a component (Windows treats it as a separator).
var windowsPortabilityPaths = []string{
	"CON", "con", "NUL.txt", "COM1", "lpt3",
	"trailing.", "trailing ",
	"dir/example.net.", // the real-world case: a DNS zone file
	"a\\b",             // literal backslash byte, ordinary on Linux
}

// TestSanitizeRelPathWindowsRulesOnlyApplyOnWindows is the regression for
// Linux-to-Linux syncs silently skipping real data. A name like
// "example.net." (a DNS zone file, trailing root label and all) is ordinary
// on Linux and must sync; the same name genuinely cannot be created on
// Windows, so it is rejected only there -- and even then as a
// distinguishable, per-entry failure rather than a corrupt-stream error.
func TestSanitizeRelPathWindowsRulesOnlyApplyOnWindows(t *testing.T) {
	restore := enforceWindowsPathRules
	t.Cleanup(func() { enforceWindowsPathRules = restore })

	enforceWindowsPathRules = false
	for _, p := range windowsPortabilityPaths {
		got, err := sanitizeRelPath(p)
		if err != nil {
			t.Errorf("with Windows rules off, sanitizeRelPath(%q) rejected it: %v", p, err)
			continue
		}
		if got != p {
			t.Errorf("sanitizeRelPath(%q) = %q, want it unchanged", p, got)
		}
	}

	enforceWindowsPathRules = true
	for _, p := range windowsPortabilityPaths {
		got, err := sanitizeRelPath(p)
		if err == nil {
			t.Errorf("with Windows rules on, sanitizeRelPath(%q) accepted it", p)
			continue
		}
		if !errors.Is(err, errPathUnrepresentable) {
			t.Errorf("sanitizeRelPath(%q) error %v is not errPathUnrepresentable; a "+
				"structurally-safe name must not read as a hostile stream", p, err)
		}
		// The cleaned path still comes back, so the caller can register and
		// fail just this entry instead of dropping it silently.
		if got != p {
			t.Errorf("sanitizeRelPath(%q) returned path %q, want the cleaned path even on rejection", p, got)
		}
	}
}

// TestSanitizeRelPathStructuralRulesAlwaysApply: the safety checks are not
// platform-scoped, on any OS, ever.
func TestSanitizeRelPathStructuralRulesAlwaysApply(t *testing.T) {
	restore := enforceWindowsPathRules
	t.Cleanup(func() { enforceWindowsPathRules = restore })

	for _, enforce := range []bool{false, true} {
		enforceWindowsPathRules = enforce
		for _, p := range []string{"", "/etc/passwd", "../etc/passwd", "a/../../b", "a\x00b", "a//b"} {
			_, err := sanitizeRelPath(p)
			if err == nil {
				t.Errorf("enforceWindowsPathRules=%v: sanitizeRelPath(%q) accepted an unsafe path", enforce, p)
				continue
			}
			if errors.Is(err, errPathUnrepresentable) {
				t.Errorf("enforceWindowsPathRules=%v: sanitizeRelPath(%q) marked an UNSAFE path as merely "+
					"unrepresentable, which would downgrade it to a per-entry failure", enforce, p)
			}
		}
	}
}

// TestSanitizeRelPathDoesNotCopyOnAccept pins that the accepted path is the
// SAME string as the input, not a rebuilt copy: strings.Join(strings.
// Split(p, "/"), "/") is byte-identical to p by construction, so building
// it cost a []string allocation plus a full string copy per call for
// nothing -- paid once per announced entry on the client (walk) and again
// on the server (announce), doubling the path-data cost at tree scale.
// unsafe.StringData gives the actual backing-array pointer, so this
// catches a regression back to rebuilding even though Go strings are
// otherwise incomparable by identity.
func TestSanitizeRelPathDoesNotCopyOnAccept(t *testing.T) {
	for _, p := range []string{"a", "a/b", "a/b/c.txt", "dir with spaces/file.txt", "日本語/ファイル.txt"} {
		got, err := sanitizeRelPath(p)
		if err != nil {
			t.Fatalf("sanitizeRelPath(%q): %v", p, err)
		}
		if got != p {
			t.Fatalf("sanitizeRelPath(%q) = %q, want it unchanged", p, got)
		}
		if unsafe.StringData(got) != unsafe.StringData(p) {
			t.Errorf("sanitizeRelPath(%q) returned a COPY (different backing array), want the same string value returned verbatim", p)
		}
	}
}

// --- the shared walk/estimate gate ---

// TestWalkGateAgreesWithEstimateTree is the drift regression. walkSource and
// estimateTree each used to reimplement this filter, and estimateTree feeds
// the progress denominators that walkSource's own totals are compared
// against -- so any disagreement showed up as a progress bar that never
// reached 100%, or reached it early. Sharing walkGate is what prevents that;
// this pins that both really do go through it.
func TestWalkGateAgreesWithEstimateTree(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "keep", "inner"))
	mustMkdirAll(t, filepath.Join(root, "skipme", "inner"))
	mustWriteFile(t, filepath.Join(root, "keep", "a.txt"), "12345")
	mustWriteFile(t, filepath.Join(root, "keep", "inner", "b.txt"), "678")
	mustWriteFile(t, filepath.Join(root, "skipme", "inner", "c.txt"), "not counted")
	mustWriteFile(t, filepath.Join(root, "top.log"), "excluded too")

	excl := []string{"skipme", "*.log"}

	// What estimateTree counts.
	var estFiles int
	var estBytes uint64
	estimateTree(root, excl, func(size uint64) {
		estFiles++
		estBytes += size
	})

	// What the real walk emits as transferable files.
	entries, _ := drainWalkLinks(t, root, excl, false)
	var walkFiles int
	var walkBytes uint64
	for _, fe := range entries {
		if fe.Kind == KindFile {
			walkFiles++
			walkBytes += fe.Size
		}
	}

	if estFiles != walkFiles {
		t.Errorf("estimateTree counted %d files, walkSource emitted %d", estFiles, walkFiles)
	}
	if estBytes != walkBytes {
		t.Errorf("estimateTree summed %d bytes, walkSource summed %d", estBytes, walkBytes)
	}
	if walkFiles != 2 {
		t.Errorf("walkFiles = %d, want 2 (the two files under keep/)", walkFiles)
	}
}

func TestWalkGateClassifies(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "sub", "ok.txt"), "x")
	mustWriteFile(t, filepath.Join(root, "skip.log"), "x")

	entries, err := os.ReadDir(filepath.Join(root, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	okEntry := entries[0]

	// Accepted: clean relative path, no reason, gateAccept.
	path, reason, res := walkGate(root, filepath.Join(root, "sub", "ok.txt"), okEntry, nil, nil)
	if res != gateAccept || path != "sub/ok.txt" || reason != "" {
		t.Errorf("accept case: path=%q reason=%q res=%v", path, reason, res)
	}

	// Excluded: still returns the CLEAN path, because the caller announces
	// it as a KindSkipped placeholder to protect it from -delete.
	path, reason, res = walkGate(root, filepath.Join(root, "sub", "ok.txt"), okEntry, nil, []string{"*.txt"})
	if res != gateSkip || path != "sub/ok.txt" || reason != "excluded" {
		t.Errorf("exclude case: path=%q reason=%q res=%v", path, reason, res)
	}

	// A walk error on a directory must skip the whole subtree, not just the
	// entry -- otherwise its unreadable children are walked anyway.
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirEntry fs.DirEntry
	for _, e := range dirEntries {
		if e.IsDir() {
			dirEntry = e
		}
	}
	_, reason, res = walkGate(root, filepath.Join(root, "sub"), dirEntry, errors.New("permission denied"), nil)
	if res != gateSkipDir {
		t.Errorf("walk error on a directory: res=%v, want gateSkipDir", res)
	}
	if !strings.Contains(reason, "permission denied") {
		t.Errorf("reason %q does not carry the underlying error", reason)
	}
}
