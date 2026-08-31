package main

import (
	"fmt"
	"testing"
)

func TestKeepSetAddAndContains(t *testing.T) {
	k := newKeepSet()

	if dup := k.add("a/b/c.txt", KindFile); dup {
		t.Fatal("first add reported a duplicate")
	}
	if dup := k.add("a/b", KindDir); dup {
		t.Fatal("first add of a distinct path reported a duplicate")
	}

	if isDir, ok := k.contains("a/b/c.txt"); !ok || isDir {
		t.Errorf("contains(file) = (%v, %v), want (false, true)", isDir, ok)
	}
	if isDir, ok := k.contains("a/b"); !ok || !isDir {
		t.Errorf("contains(dir) = (%v, %v), want (true, true)", isDir, ok)
	}
	if _, ok := k.contains("never/added"); ok {
		t.Error("contains reported a path that was never added")
	}
}

func TestKeepSetAddReportsDuplicate(t *testing.T) {
	k := newKeepSet()
	if dup := k.add("same/path", KindFile); dup {
		t.Fatal("first add reported a duplicate")
	}
	if dup := k.add("same/path", KindFile); !dup {
		t.Error("second add of the identical path did not report a duplicate")
	}
	// A different kind at the same path is still the same digest -- still a
	// duplicate. deleteExtraneous's isDir bit reflects whichever add came
	// first, matching byPath's old first-writer-wins semantics (announce
	// would have already rejected the second announce as an error before a
	// second add could ever run in practice).
	if dup := k.add("same/path", KindDir); !dup {
		t.Error("duplicate add with a different kind was not reported")
	}
}

func TestKeepSetGrowsAndStaysExact(t *testing.T) {
	k := newKeepSet()
	const n = 20000 // several growths past keepSetInitialSlots
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		paths[i] = fmt.Sprintf("dir%d/file%d.bin", i%97, i)
		kind := KindFile
		if i%13 == 0 {
			kind = KindDir
		}
		if dup := k.add(paths[i], kind); dup {
			t.Fatalf("path %d reported as a spurious duplicate", i)
		}
	}
	if k.count != n {
		t.Fatalf("count = %d, want %d", k.count, n)
	}
	for i, p := range paths {
		wantDir := i%13 == 0
		isDir, ok := k.contains(p)
		if !ok {
			t.Fatalf("path %d (%q) missing after growth", i, p)
		}
		if isDir != wantDir {
			t.Errorf("path %d (%q): isDir = %v, want %v", i, p, isDir, wantDir)
		}
	}
	if _, ok := k.contains("dir0/file999999.bin"); ok {
		t.Error("contains reported a path that was never added, after growth")
	}
}

func TestKeepSetEmptyContainsNothing(t *testing.T) {
	k := newKeepSet()
	if _, ok := k.contains("anything"); ok {
		t.Error("empty keepSet reported containing a path")
	}
}

func TestPathDigestIsDeterministicAndDistinct(t *testing.T) {
	hi1, lo1 := pathDigest("a/b/c")
	hi2, lo2 := pathDigest("a/b/c")
	if hi1 != hi2 || lo1 != lo2 {
		t.Error("pathDigest is not deterministic for the same input")
	}
	hi3, lo3 := pathDigest("a/b/d")
	if hi1 == hi3 && lo1 == lo3 {
		t.Error("pathDigest produced identical digests for different paths (extraordinarily unlikely)")
	}
}
