package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SyncSession tracks progress and per-file failure state for one
// directory-tree transfer. It replaces the package-level globals the
// single-file path uses for the same purpose (lastBlockNum, totCompSize,
// totOrigSize, diffs, v_skipIdx, v_fileSize, t0 in client.go; srvOnce,
// srvFileSize, srvLastBlockNum, etc. in server.go) — none of those can hold
// more than one file's worth of state at a time, and directory mode needs
// exactly that. All counters are atomics so the hot per-block path never
// takes a lock; files is append-only and guarded by a lock only because it
// can grow (as the walk discovers more entries) concurrently with reads of
// already-appended ones.
//
// This is the client-side counterpart introduced now (Phase 2, no network
// yet); startDirClient/TreeReader/the transfer workers that actually drive
// a SyncSession are added in later phases.
type SyncSession struct {
	startedAt time.Time

	filesMu sync.RWMutex
	files   []*FileEntry // index == FileID, append-only

	walkDone atomic.Bool

	blocksDone atomic.Uint64
	bytesOrig  atomic.Uint64
	bytesNet   atomic.Uint64
	diffs      atomic.Uint64

	filesDone    atomic.Uint32
	filesSkipped atomic.Uint32

	failed sync.Map // FileID (uint32) -> error
}

// NewSyncSession creates an empty session with startedAt set to now.
func NewSyncSession() *SyncSession {
	return &SyncSession{startedAt: time.Now()}
}

// AddFile appends fe to the session's file table and returns its FileID.
// fe.ID must already be set by the caller (walkSource assigns dense,
// sequential IDs); AddFile asserts it matches the next available slot, so a
// caller that skips or reorders entries fails loudly instead of silently
// desynchronizing the ID space from the table.
func (s *SyncSession) AddFile(fe *FileEntry) uint32 {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if int(fe.ID) != len(s.files) {
		panic(fmt.Sprintf("SyncSession.AddFile: FileID %d does not match next slot %d", fe.ID, len(s.files)))
	}
	s.files = append(s.files, fe)
	return fe.ID
}

// FileByID returns the entry for id, or nil if id is out of range.
func (s *SyncSession) FileByID(id uint32) *FileEntry {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	if int(id) >= len(s.files) {
		return nil
	}
	return s.files[id]
}

// FileCount returns the number of files currently known to the session.
// This grows as the walk discovers more entries, so a caller must not treat
// it as a final total until WalkDone reports true.
func (s *SyncSession) FileCount() int {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	return len(s.files)
}

// MarkWalkDone records that the walk has finished discovering files; the
// file count and any totals derived from it are final from this point on.
func (s *SyncSession) MarkWalkDone() {
	s.walkDone.Store(true)
}

// WalkDone reports whether the walk has finished.
func (s *SyncSession) WalkDone() bool {
	return s.walkDone.Load()
}

// failFile records that FileID id failed with err. Only the first failure
// per file is kept, so a file that fails again on a later retry doesn't
// overwrite the original cause.
func (s *SyncSession) failFile(id uint32, err error) {
	s.failed.LoadOrStore(id, err)
}

// isFailed reports whether FileID id has already been marked failed, so a
// reader or transfer worker can stop doing further I/O for it instead of
// retrying a doomed file block by block.
func (s *SyncSession) isFailed(id uint32) bool {
	_, ok := s.failed.Load(id)
	return ok
}

// FailedError returns the recorded error for FileID id, or nil if it has
// not failed.
func (s *SyncSession) FailedError(id uint32) error {
	v, ok := s.failed.Load(id)
	if !ok {
		return nil
	}
	return v.(error)
}

// FailedCount returns the number of files currently marked failed.
func (s *SyncSession) FailedCount() int {
	n := 0
	s.failed.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// AddBlockProgress records one block's outcome. netBytes/origBytes are the
// compressed/raw byte counts (0 for a skipped/unchanged block), and changed
// reports whether the block actually differed and was transferred.
func (s *SyncSession) AddBlockProgress(changed bool, netBytes, origBytes uint64) {
	s.blocksDone.Add(1)
	if changed {
		s.diffs.Add(1)
		s.bytesNet.Add(netBytes)
		s.bytesOrig.Add(origBytes)
	}
}

// MarkFileDone increments the completed-file counter. Call once per file,
// after all of its blocks (or its failure) have been accounted for.
func (s *SyncSession) MarkFileDone() {
	s.filesDone.Add(1)
}

// MarkFileSkipped increments the skipped-file counter (symlinks, special
// files, excluded paths — anything walkSource reported via onSkip).
func (s *SyncSession) MarkFileSkipped() {
	s.filesSkipped.Add(1)
}
