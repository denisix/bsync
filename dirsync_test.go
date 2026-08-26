package main

import (
	"net"
	"sync"
	"testing"
	"time"
)

// sentMsgLog collects every Msg written to one side of a net.Pipe by a
// background drain goroutine, so a test can synchronously call
// dirTransferState methods (which themselves synchronously call
// ctrl.send) without deadlocking against net.Pipe's unbuffered rendezvous
// semantics.
type sentMsgLog struct {
	mu   sync.Mutex
	msgs []*Msg
}

func (l *sentMsgLog) add(m *Msg) {
	l.mu.Lock()
	l.msgs = append(l.msgs, m)
	l.mu.Unlock()
}

func (l *sentMsgLog) fileDoneCount(fileID uint32) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if Op(m.Op) == OpFileDone && m.FileID == fileID {
			n++
		}
	}
	return n
}

// waitForFileDoneCount polls (rather than trusting that a caller's
// synchronous ctrl.send has necessarily been recorded by the drain
// goroutine yet -- net.Pipe's Write unblocking and the reader goroutine's
// subsequent bookkeeping happen on different goroutines with no ordering
// guarantee beyond the byte handoff itself) until fileID has want recorded
// OpFileDone sends, or fails after timeout.
func waitForFileDoneCount(t *testing.T, log *sentMsgLog, fileID uint32, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if log.fileDoneCount(fileID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("fileDoneCount(%d) = %d after %v, want %d", fileID, log.fileDoneCount(fileID), timeout, want)
}

// assertFileDoneCountStays asserts fileID's recorded OpFileDone count is
// (and remains, for a short grace window) want -- used to prove a send did
// NOT happen, which polling-until-match can't do by itself.
func assertFileDoneCountStays(t *testing.T, log *sentMsgLog, fileID uint32, want int) {
	t.Helper()
	time.Sleep(30 * time.Millisecond) // let any wrongly-fired send land
	if got := log.fileDoneCount(fileID); got != want {
		t.Fatalf("fileDoneCount(%d) = %d, want %d", fileID, got, want)
	}
}

// newTestDirTransferState builds a dirTransferState whose ctrl connection
// is one side of a net.Pipe; a background goroutine drains the other side
// into a sentMsgLog so ctrl.send (called synchronously by
// dirTransferState's methods) never blocks against nothing reading it.
func newTestDirTransferState(t *testing.T, sess *SyncSession) (*dirTransferState, *sentMsgLog) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	log := &sentMsgLog{}
	go func() {
		for {
			msg, err := readDirMsg(client)
			if err != nil {
				return
			}
			log.add(msg)
		}
	}()
	ctrl := newCtrlConn(server)
	return &dirTransferState{sess: sess, ctrl: ctrl, magicBytes: ctrl.magicBytes}, log
}

func newTestFileEntry(id uint32, blocks uint32) *FileEntry {
	return &FileEntry{
		ID: id, Kind: KindFile, RelPath: "f.bin",
		Size: uint64(blocks) * 4096, BlockSize: 4096, Blocks: blocks,
	}
}

// --- recordBlockDone ---

func TestDirTransferStateRecordBlockDoneSendsExactlyOneFileDone(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	// Three blocks of the same file completing concurrently, from
	// different simulated workers -- exactly one of them must observe the
	// counter hit zero and send OpFileDone.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dt.recordBlockDone(0)
		}()
	}
	wg.Wait()

	waitForFileDoneCount(t, log, 0, 1, 2*time.Second)
	assertFileDoneCountStays(t, log, 0, 1)
}

func TestDirTransferStateRecordBlockDoneDoesNotFireEarly(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	done := make(chan struct{})
	go func() { dt.recordBlockDone(0); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recordBlockDone did not return")
	}
	assertFileDoneCountStays(t, log, 0, 0)
}

// --- forceFileDone: the open-failure case (0 emitted) ---

func TestDirTransferStateForceFileDoneOnOpenFailureSendsImmediately(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	done := make(chan struct{})
	go func() { dt.forceFileDone(0, 0); close(done) }() // 0 blocksEmitted = open failure
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forceFileDone(0 emitted) did not return")
	}
	waitForFileDoneCount(t, log, 0, 1, 2*time.Second)
}

// --- forceFileDone: the mid-file-failure case (this is the load-bearing
// correctness property -- forceFileDone must never declare a file done
// while blocks that WERE emitted are still uncompleted, since they may
// still be mid-write on the server; only their own recordBlockDone calls
// may retire them) ---

func TestDirTransferStateForceFileDoneWaitsForEmittedBlocksBeforeSending(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 5))
	dt, log := newTestDirTransferState(t, sess)

	// TreeReader emitted 2 of the file's 5 blocks before hitting a mid-file
	// read error.
	forceDone := make(chan struct{})
	go func() { dt.forceFileDone(0, 2); close(forceDone) }()
	select {
	case <-forceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("forceFileDone did not return")
	}
	assertFileDoneCountStays(t, log, 0, 0)

	// First of the 2 emitted blocks completes -- still not done (1 of 2).
	b1 := make(chan struct{})
	go func() { dt.recordBlockDone(0); close(b1) }()
	select {
	case <-b1:
	case <-time.After(2 * time.Second):
		t.Fatal("recordBlockDone (1st) did not return")
	}
	assertFileDoneCountStays(t, log, 0, 0)

	// Second (last) emitted block completes -- NOW it must fire, exactly
	// once.
	b2 := make(chan struct{})
	go func() { dt.recordBlockDone(0); close(b2) }()
	select {
	case <-b2:
	case <-time.After(2 * time.Second):
		t.Fatal("recordBlockDone (2nd) did not return")
	}
	waitForFileDoneCount(t, log, 0, 1, 2*time.Second)
	assertFileDoneCountStays(t, log, 0, 1)
}

func TestDirTransferStateForceFileDoneNoOpWhenAllBlocksAlreadyEmitted(t *testing.T) {
	// A close()-only failure after a fully successful read reports the
	// FULL emitted count -- forceFileDone must be a no-op here (nothing to
	// correct), letting the ordinary recordBlockDone calls converge on
	// zero by themselves without a premature or duplicate send.
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 2))
	dt, log := newTestDirTransferState(t, sess)

	done := make(chan struct{})
	go func() { dt.forceFileDone(0, 2); close(done) }() // all 2 blocks were emitted
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forceFileDone did not return")
	}
	assertFileDoneCountStays(t, log, 0, 0)
}

func TestDirTransferStateForceFileDoneThenNormalCompletionSendsExactlyOnce(t *testing.T) {
	// Whichever of forceFileDone/recordBlockDone happens to be the one
	// that brings the counter to zero, exactly one OpFileDone must result
	// -- covers the case where forceFileDone's correction itself is what
	// hits zero (rather than a later recordBlockDone, as in the "waits
	// for" test above).
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	b1 := make(chan struct{})
	go func() { dt.recordBlockDone(0); close(b1) }()
	<-b1
	assertFileDoneCountStays(t, log, 0, 0)

	// 1 of 3 blocks emitted and completed; TreeReader now reports only 1
	// was ever emitted (a read failure right after the first block) --
	// forceFileDone's correction (3-1=2) brings the counter straight to
	// zero itself.
	fd := make(chan struct{})
	go func() { dt.forceFileDone(0, 1); close(fd) }()
	select {
	case <-fd:
	case <-time.After(2 * time.Second):
		t.Fatal("forceFileDone did not return")
	}
	waitForFileDoneCount(t, log, 0, 1, 2*time.Second)
	assertFileDoneCountStays(t, log, 0, 1)
}
