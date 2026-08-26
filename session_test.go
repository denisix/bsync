package main

import (
	"errors"
	"sync"
	"testing"
)

func TestSyncSessionAddFileAndLookup(t *testing.T) {
	s := NewSyncSession()
	if s.FileCount() != 0 {
		t.Fatalf("new session FileCount = %d, want 0", s.FileCount())
	}
	fe0 := &FileEntry{ID: 0, RelPath: "a.txt", Kind: KindFile}
	fe1 := &FileEntry{ID: 1, RelPath: "b.txt", Kind: KindFile}

	if got, err := s.AddFile(fe0); got != 0 || err != nil {
		t.Errorf("AddFile(fe0) = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := s.AddFile(fe1); got != 1 || err != nil {
		t.Errorf("AddFile(fe1) = (%d, %v), want (1, nil)", got, err)
	}
	if s.FileCount() != 2 {
		t.Errorf("FileCount = %d, want 2", s.FileCount())
	}
	if got := s.FileByID(0); got != fe0 {
		t.Errorf("FileByID(0) = %+v, want %+v", got, fe0)
	}
	if got := s.FileByID(1); got != fe1 {
		t.Errorf("FileByID(1) = %+v, want %+v", got, fe1)
	}
	if got := s.FileByID(2); got != nil {
		t.Errorf("FileByID(2) = %+v, want nil (out of range)", got)
	}
}

// TestSyncSessionAddFileWrongIDReturnsError is a regression test for B3:
// AddFile used to panic on an ID/slot mismatch -- a panic in a
// server/transfer data path is a liveness hazard, able to take down the
// whole process over what should just fail this one transfer. It now
// returns an error instead, matching every other error in this file.
func TestSyncSessionAddFileWrongIDReturnsError(t *testing.T) {
	s := NewSyncSession()
	if _, err := s.AddFile(&FileEntry{ID: 0}); err != nil {
		t.Fatalf("AddFile(ID 0): %v", err)
	}
	// Next expected slot is 1; ID 5 must return an error, not panic, and
	// must not silently desynchronize the ID space from the file table.
	if _, err := s.AddFile(&FileEntry{ID: 5}); err == nil {
		t.Fatal("AddFile with a skipped ID unexpectedly succeeded")
	}
	if s.FileCount() != 1 {
		t.Errorf("FileCount = %d, want 1 -- the rejected entry must not have been appended", s.FileCount())
	}
}

func TestSyncSessionWalkDone(t *testing.T) {
	s := NewSyncSession()
	if s.WalkDone() {
		t.Fatal("new session reports WalkDone = true")
	}
	s.MarkWalkDone()
	if !s.WalkDone() {
		t.Fatal("MarkWalkDone did not stick")
	}
}

func TestSyncSessionFailFileFirstWriteWins(t *testing.T) {
	s := NewSyncSession()
	if s.isFailed(0) {
		t.Fatal("fresh session reports file 0 as failed")
	}
	errA := errors.New("first failure")
	errB := errors.New("second failure")
	s.failFile(0, errA)
	s.failFile(0, errB)
	if !s.isFailed(0) {
		t.Fatal("failFile did not mark the file failed")
	}
	if got := s.FailedError(0); got != errA {
		t.Errorf("FailedError(0) = %v, want the first error %v", got, errA)
	}
	if s.FailedCount() != 1 {
		t.Errorf("FailedCount = %d, want 1", s.FailedCount())
	}
	if s.isFailed(1) {
		t.Error("an unrelated file was reported as failed")
	}
	if s.FailedError(1) != nil {
		t.Error("FailedError for a healthy file should be nil")
	}
}

func TestSyncSessionBlockProgress(t *testing.T) {
	s := NewSyncSession()
	s.AddBlockProgress(false, 4096, 0, 0) // unchanged block: counted, incl. in bytesScanned, but no net/orig bytes
	s.AddBlockProgress(true, 100, 100, 400)
	s.AddBlockProgress(true, 50, 50, 400)

	if got := s.blocksDone.Load(); got != 3 {
		t.Errorf("blocksDone = %d, want 3", got)
	}
	if got := s.diffs.Load(); got != 2 {
		t.Errorf("diffs = %d, want 2", got)
	}
	if got := s.bytesNet.Load(); got != 150 {
		t.Errorf("bytesNet = %d, want 150", got)
	}
	if got := s.bytesOrig.Load(); got != 800 {
		t.Errorf("bytesOrig = %d, want 800", got)
	}
	// bytesScanned (Phase C): every block's actual length counts, changed
	// or not -- the unchanged 4096-byte block must be included even though
	// it contributed nothing to bytesNet/bytesOrig above.
	if got := s.bytesScanned.Load(); got != 4096+100+50 {
		t.Errorf("bytesScanned = %d, want %d", got, 4096+100+50)
	}
}

func TestSyncSessionFileDoneAndSkippedCounters(t *testing.T) {
	s := NewSyncSession()
	s.MarkFileDone()
	s.MarkFileDone()
	s.MarkFileSkipped()
	if got := s.filesDone.Load(); got != 2 {
		t.Errorf("filesDone = %d, want 2", got)
	}
	if got := s.filesSkipped.Load(); got != 1 {
		t.Errorf("filesSkipped = %d, want 1", got)
	}
}

// TestSyncSessionCountersConcurrent hammers every counter-mutating method
// from many goroutines at once; run with -race, this is the test that would
// catch a missing atomic or an unguarded map/slice access.
func TestSyncSessionCountersConcurrent(t *testing.T) {
	s := NewSyncSession()
	const workers = 32
	const perWorker = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := uint32(w*perWorker + i)
				fe := &FileEntry{ID: id, RelPath: "f"}
				// AddFile requires sequential IDs from a single logical
				// producer; exercise the rest of the API concurrently
				// instead, which is the realistic multi-worker access
				// pattern (many transfer workers, one shared session).
				_ = fe
				s.AddBlockProgress(i%2 == 0, uint64(i+1), uint64(i), uint64(i*2))
				if i%7 == 0 {
					s.failFile(id, errors.New("boom"))
				}
				_ = s.isFailed(id)
				if i%5 == 0 {
					s.MarkFileDone()
				} else {
					s.MarkFileSkipped()
				}
			}
		}(w)
	}
	wg.Wait()

	total := uint64(workers * perWorker)
	if got := s.blocksDone.Load(); got != total {
		t.Errorf("blocksDone = %d, want %d", got, total)
	}
	if s.FailedCount() == 0 {
		t.Error("expected at least one failed file to be recorded")
	}
}

func TestSyncSessionAddFileConcurrentWithReads(t *testing.T) {
	s := NewSyncSession()
	const n = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			s.AddFile(&FileEntry{ID: uint32(i), RelPath: "f"})
		}
	}()
	// Concurrently poll FileCount/FileByID while entries are still being
	// appended; must never race or panic.
	for {
		select {
		case <-done:
			if s.FileCount() != n {
				t.Fatalf("FileCount = %d, want %d", s.FileCount(), n)
			}
			return
		default:
			c := s.FileCount()
			if c > 0 {
				if fe := s.FileByID(uint32(c - 1)); fe == nil {
					t.Fatalf("FileByID(%d) returned nil while FileCount reports %d", c-1, c)
				}
			}
		}
	}
}
