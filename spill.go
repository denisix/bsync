package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// idSpill is an on-disk, write-once-then-read-once queue of FileIDs: the
// list of files a delete-before transfer (-delete) must hold back until the
// destination confirms its delete pass has completed (see startDirClient).
// Delete-before means the WHOLE tree is announced, and its survivor list
// fully known, before a single byte of content may move -- so on a large
// tree that list used to be a plain in-memory []*FileEntry (qcSurvivors, or
// the equivalent reconstruction for the non-quick-check case), costing an
// 8-byte pointer per file for the entire announce-to-delete-ack window.
//
// Only the FileID is spilled, not the FileEntry itself: the corresponding
// *FileEntry is already retained by SyncSession.live for as long as that
// file's own transfer is still pending (a real KindFile entry with content
// is only released once its transfer completes -- see claimDone), so
// re-encoding its RelPath/Size/BlockSize here would just be a second,
// redundant copy of data already resident in memory. This holds only the
// ~128 MB of plain 4-byte integers a 32M-entry tree's survivor list would
// otherwise cost as pointers -- on disk instead of in RAM -- and it is
// freed (the temp file removed) as soon as the feeder goroutine that reads
// it back finishes.
type idSpill struct {
	path string
	f    *os.File
	w    *bufio.Writer
	n    uint64
}

// newIDSpill creates a spill file in dir (os.TempDir() if dir is empty).
func newIDSpill(dir string) (*idSpill, error) {
	f, err := os.CreateTemp(dir, "bsync-tree-*")
	if err != nil {
		return nil, fmt.Errorf("create spill file: %w", err)
	}
	return &idSpill{path: f.Name(), f: f, w: bufio.NewWriterSize(f, 256*1024)}, nil
}

// Append writes id to the spill.
func (s *idSpill) Append(id uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], id)
	if _, err := s.w.Write(b[:]); err != nil {
		return fmt.Errorf("spill: write id: %w", err)
	}
	s.n++
	return nil
}

// Count returns how many IDs have been appended so far.
func (s *idSpill) Count() uint64 { return s.n }

// Reader flushes any buffered writes and returns a fresh reader positioned
// at the start of the spill, in append order. Call once, after every
// Append -- the returned idSpillReader is single-pass.
func (s *idSpill) Reader() (*idSpillReader, error) {
	if err := s.w.Flush(); err != nil {
		return nil, fmt.Errorf("spill: flush: %w", err)
	}
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("spill: seek: %w", err)
	}
	return &idSpillReader{r: bufio.NewReaderSize(s.f, 256*1024), left: s.n}, nil
}

// Close closes and removes the spill file. Safe to call on a nil *idSpill
// (a no-op, for the common case of an unconditional `defer spill.Close()`
// in a caller where the spill is only sometimes created) and safe to call
// more than once.
func (s *idSpill) Close() error {
	if s == nil || s.f == nil {
		return nil
	}
	closeErr := s.f.Close()
	rmErr := os.Remove(s.path)
	s.f = nil
	if closeErr != nil {
		return closeErr
	}
	return rmErr
}

// idSpillReader streams IDs back out of an idSpill, in the order they were
// appended.
type idSpillReader struct {
	r    *bufio.Reader
	left uint64
}

// Next returns the next ID, or ok=false once every appended ID has been
// returned.
func (r *idSpillReader) Next() (id uint32, ok bool, err error) {
	if r.left == 0 {
		return 0, false, nil
	}
	var b [4]byte
	if _, err := io.ReadFull(r.r, b[:]); err != nil {
		return 0, false, fmt.Errorf("spill: read id: %w", err)
	}
	r.left--
	return binary.LittleEndian.Uint32(b[:]), true, nil
}
