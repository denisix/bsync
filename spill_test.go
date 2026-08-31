package main

import (
	"os"
	"testing"
)

func TestIDSpillRoundTrip(t *testing.T) {
	s, err := newIDSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	want := []uint32{0, 1, 2, 100, 1 << 20, 1<<32 - 1}
	for _, id := range want {
		if err := s.Append(id); err != nil {
			t.Fatalf("Append(%d): %v", id, err)
		}
	}
	if s.Count() != uint64(len(want)) {
		t.Fatalf("Count() = %d, want %d", s.Count(), len(want))
	}

	rd, err := s.Reader()
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for {
		id, ok, err := rd.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d ids, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

func TestIDSpillEmpty(t *testing.T) {
	s, err := newIDSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rd, err := s.Reader()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rd.Next(); ok || err != nil {
		t.Errorf("Next() on an empty spill = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// TestIDSpillCloseRemovesFile proves Close actually removes the temp file
// -- a leaked spill file per delete-mode transfer would defeat the whole
// point of moving this off the heap.
func TestIDSpillCloseRemovesFile(t *testing.T) {
	s, err := newIDSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file does not exist before Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("spill file still exists after Close (stat err = %v)", err)
	}
}

// TestIDSpillCloseIsIdempotentAndNilSafe: a caller unconditionally defers
// Close() even when no spill was ever created (see startDirClient), and a
// caller might reasonably call it more than once on a real error path.
func TestIDSpillCloseIsIdempotentAndNilSafe(t *testing.T) {
	var nilSpill *idSpill
	if err := nilSpill.Close(); err != nil {
		t.Errorf("Close on a nil *idSpill returned an error: %v", err)
	}

	s, err := newIDSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned an error, want a no-op: %v", err)
	}
}

// TestIDSpillPreservesOrderAtScale is a scale sanity check: many more ids
// than the internal 256 KiB buffer holds, still round-tripping in order.
func TestIDSpillPreservesOrderAtScale(t *testing.T) {
	s, err := newIDSpill(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const n = 200000
	for i := uint32(0); i < n; i++ {
		if err := s.Append(i); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	rd, err := s.Reader()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < n; i++ {
		id, ok, err := rd.Next()
		if err != nil {
			t.Fatalf("Next at %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("spill ended early at %d, want %d entries", i, n)
		}
		if id != i {
			t.Fatalf("entry %d: got id %d, want %d (order not preserved)", i, id, i)
		}
	}
	if _, ok, _ := rd.Next(); ok {
		t.Error("spill produced more entries than were appended")
	}
}
