package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

// --- runDirTransferWorker's dual-source receive loop ---

// runWorkerAgainstRefusedPort resolves a *net.TCPAddr for a port nothing is
// listening on (so every dial fails immediately, not via a timeout -- the
// bounded-failure path handleDirBlock takes still costs the fixed
// maxRetries backoff, ~3s per block, but that's deterministic and fast
// enough for a unit test) and runs runDirTransferWorker against it,
// returning once the worker's loop exits.
func runWorkerAgainstRefusedPort(t *testing.T, dt *dirTransferState, ded, shared <-chan PrecomputedBlock) {
	t.Helper()
	port := freeTCPPort(t)
	saddr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("ResolveTCPAddr: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDirTransferWorker(saddr, dt, ded, shared)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runDirTransferWorker did not return -- a channel was orphaned (dual-source exit-condition bug)")
	}
}

// sentinelFileID is registered in every TestRunDirTransferWorkerDrainsBothChannels
// subtest but is NEVER given a real block. PrecomputedBlock's zero value
// happens to have FileID 0, BlockIdx 0 -- exactly sentinelFileID -- so if a
// select branch's !ok handling is ever missing its continue (letting a
// closed channel's zero-value read fall through to processing instead of
// being discarded), the phantom block lands squarely on this sentinel's
// single-block counter, spuriously completing a file that was never sent
// a single real block. Without a sentinel, that same phantom decrement
// would (harmlessly by coincidence) land on a real, already-finished
// file's counter and go completely unobserved -- recordBlockDone's
// counter for an already-zero file just goes negative, sendFileDone's
// zero-check never re-fires, and isFailed's early-return means it never
// even attempts a second network op. This was verified empirically: a
// deliberately reintroduced missing-continue bug made this exact class of
// test pass right up until the sentinel assertions below were added.
const sentinelFileID = 0

// assertSentinelUntouched asserts the sentinel file never received a
// single recorded block -- see sentinelFileID's comment.
func assertSentinelUntouched(t *testing.T, log *sentMsgLog) {
	t.Helper()
	assertFileDoneCountStays(t, log, sentinelFileID, 0)
}

// TestRunDirTransferWorkerDrainsBothChannels proves the dual-source loop's
// three hazards can't happen: a buffered block still sitting in a channel
// at close time must still be processed (never silently dropped), the loop
// must terminate only once BOTH channels are closed AND drained -- never on
// just one closing, which would orphan the other channel's sender forever
// (the same shape of hang as the A1 pipeline deadlock) -- and a channel's
// closed-and-drained zero-value read must never be mistaken for a real
// block (see sentinelFileID). Every real FileID here has exactly one
// block, so exactly one recorded recordBlockDone call (success or the
// dial-refused failure path, either way) sends exactly one OpFileDone for
// it -- turning "was this block actually handled, and only it" into a
// directly observable, deterministic assertion.
func TestRunDirTransferWorkerDrainsBothChannels(t *testing.T) {
	newBlock := func(fileID uint32) PrecomputedBlock {
		return PrecomputedBlock{FileID: fileID, BlockIdx: 0, FileSize: 1, BlockSize: 1, Last: true}
	}

	t.Run("shared closes before dedicated, both have buffered blocks", func(t *testing.T) {
		t.Parallel() // independent: own sess/port/channels -- runs alongside its siblings to absorb the fixed maxRetries backoff cost concurrently instead of serially
		sess := NewSyncSession()
		sess.AddFile(newTestFileEntry(sentinelFileID, 1))
		sess.AddFile(newTestFileEntry(1, 1))
		sess.AddFile(newTestFileEntry(2, 1))
		dt, log := newTestDirTransferState(t, sess)

		ded := make(chan PrecomputedBlock, 1)
		shared := make(chan PrecomputedBlock, 1)
		ded <- newBlock(1)
		shared <- newBlock(2)
		close(shared)
		close(ded)

		runWorkerAgainstRefusedPort(t, dt, ded, shared)
		waitForFileDoneCount(t, log, 1, 1, 2*time.Second)
		waitForFileDoneCount(t, log, 2, 1, 2*time.Second)
		assertSentinelUntouched(t, log)
	})

	t.Run("dedicated closes before shared, both have buffered blocks", func(t *testing.T) {
		t.Parallel()
		sess := NewSyncSession()
		sess.AddFile(newTestFileEntry(sentinelFileID, 1))
		sess.AddFile(newTestFileEntry(1, 1))
		sess.AddFile(newTestFileEntry(2, 1))
		dt, log := newTestDirTransferState(t, sess)

		ded := make(chan PrecomputedBlock, 1)
		shared := make(chan PrecomputedBlock, 1)
		ded <- newBlock(1)
		shared <- newBlock(2)
		close(ded)
		close(shared)

		runWorkerAgainstRefusedPort(t, dt, ded, shared)
		waitForFileDoneCount(t, log, 1, 1, 2*time.Second)
		waitForFileDoneCount(t, log, 2, 1, 2*time.Second)
		assertSentinelUntouched(t, log)
	})

	t.Run("nil dedicated channel reduces to single-source behavior", func(t *testing.T) {
		t.Parallel()
		sess := NewSyncSession()
		sess.AddFile(newTestFileEntry(sentinelFileID, 1))
		sess.AddFile(newTestFileEntry(1, 1))
		dt, log := newTestDirTransferState(t, sess)

		shared := make(chan PrecomputedBlock, 1)
		shared <- newBlock(1)
		close(shared)

		runWorkerAgainstRefusedPort(t, dt, nil, shared)
		waitForFileDoneCount(t, log, 1, 1, 2*time.Second)
		assertSentinelUntouched(t, log)
	})

	t.Run("nil shared channel reduces to dedicated-only behavior", func(t *testing.T) {
		t.Parallel()
		sess := NewSyncSession()
		sess.AddFile(newTestFileEntry(sentinelFileID, 1))
		sess.AddFile(newTestFileEntry(1, 1))
		dt, log := newTestDirTransferState(t, sess)

		ded := make(chan PrecomputedBlock, 1)
		ded <- newBlock(1)
		close(ded)

		runWorkerAgainstRefusedPort(t, dt, ded, nil)
		waitForFileDoneCount(t, log, 1, 1, 2*time.Second)
		assertSentinelUntouched(t, log)
	})
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

// --- markFileSkippedDone ---

func TestDirTransferStateMarkFileSkippedDoneSendsNoFileDoneFrame(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	dt.markFileSkippedDone(0)

	// Unlike sendFileDone/forceFileDone, this must never put an OpFileDone
	// on the wire: the destination already finalized the file for itself
	// at announce time (fileTable.announce), so there is nothing left for
	// the server to wait for.
	assertFileDoneCountStays(t, log, 0, 0)
	if got := sess.filesDone.Load(); got != 1 {
		t.Errorf("filesDone = %d, want 1", got)
	}
	if got := sess.bytesScanned.Load(); got != newTestFileEntry(0, 3).Size {
		t.Errorf("bytesScanned = %d, want %d", got, newTestFileEntry(0, 3).Size)
	}
}

func TestDirTransferStateMarkFileSkippedDoneIsExactlyOnce(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dt.markFileSkippedDone(0)
		}()
	}
	wg.Wait()

	if got := sess.filesDone.Load(); got != 1 {
		t.Errorf("filesDone = %d, want 1 -- must count exactly once regardless of how many callers race", got)
	}
	assertFileDoneCountStays(t, log, 0, 0)
}

func TestDirTransferStateMarkFileSkippedDoneSharesGuardWithSendFileDone(t *testing.T) {
	// markFileSkippedDone and sendFileDone/forceFileDone must share the same
	// doneOnce guard (keyed by FileID): if a file was already reported done
	// through one path, the other must become a no-op rather than
	// double-counting filesDone or firing a redundant OpFileDone.
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 3))
	dt, log := newTestDirTransferState(t, sess)

	dt.sendFileDone(0)
	waitForFileDoneCount(t, log, 0, 1, 2*time.Second)

	dt.markFileSkippedDone(0)

	assertFileDoneCountStays(t, log, 0, 1)
	if got := sess.filesDone.Load(); got != 1 {
		t.Errorf("filesDone = %d, want 1 -- markFileSkippedDone must be a no-op once sendFileDone already fired", got)
	}
}

// --- Phase C: progress/ETA line ---

func TestResolveProgressTotals(t *testing.T) {
	cases := []struct {
		name       string
		knownFiles uint32
		knownBytes uint64
		estFiles   uint32
		estBytes   uint64
		walkDone   bool
		wantFiles  uint32
		wantBytes  uint64
	}{
		// The normal deep-tree case this exists for: the walk is
		// back-pressured, the pre-scan has raced ahead -- display the
		// estimate.
		{name: "estimate ahead while walking", knownFiles: 12, knownBytes: 1000, estFiles: 5000, estBytes: 900000, wantFiles: 5000, wantBytes: 900000},
		// Startup instant before the pre-scan has found anything.
		{name: "known ahead while walking", knownFiles: 12, knownBytes: 1000, estFiles: 3, estBytes: 50, wantFiles: 12, wantBytes: 1000},
		// max() is per-dimension: either side can win independently.
		{name: "mixed leaders", knownFiles: 100, knownBytes: 1000, estFiles: 50, estBytes: 900000, wantFiles: 100, wantBytes: 900000},
		// Walk finished: exact totals only, however large the (stale,
		// best-effort) estimate claims to be -- e.g. files deleted from the
		// source after the pre-scan passed them.
		{name: "walk done ignores estimate", knownFiles: 12, knownBytes: 1000, estFiles: 5000, estBytes: 900000, walkDone: true, wantFiles: 12, wantBytes: 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := NewSyncSession()
			sess.totalFilesKnown.Store(tc.knownFiles)
			sess.totalBytesKnown.Store(tc.knownBytes)
			sess.estFiles.Store(tc.estFiles)
			sess.estBytes.Store(tc.estBytes)
			if tc.walkDone {
				sess.MarkWalkDone()
			}
			files, bytes := resolveProgressTotals(sess)
			if files != tc.wantFiles || bytes != tc.wantBytes {
				t.Errorf("resolveProgressTotals = %d files / %d bytes, want %d / %d", files, bytes, tc.wantFiles, tc.wantBytes)
			}
		})
	}
}

// TestAddFileCountsOnlyKindFilesInTotalFilesKnown pins the files-only
// denominator: dirs and KindSkipped placeholders register with the session
// (they need IDs and announce records) but never get a MarkFileDone, so
// counting them in the display total would keep "files X/Y" permanently
// short of Y even on a fully successful sync.
func TestAddFileCountsOnlyKindFilesInTotalFilesKnown(t *testing.T) {
	sess := NewSyncSession()
	entries := []*FileEntry{
		{ID: 0, Kind: KindDir, RelPath: "d"},
		{ID: 1, Kind: KindFile, RelPath: "d/f1", Size: 10},
		{ID: 2, Kind: KindSkipped, RelPath: "d/link"},
		{ID: 3, Kind: KindFile, RelPath: "d/f2", Size: 20},
	}
	for _, fe := range entries {
		if _, err := sess.AddFile(fe); err != nil {
			t.Fatal(err)
		}
	}
	if got := sess.totalFilesKnown.Load(); got != 2 {
		t.Errorf("totalFilesKnown = %d, want 2 (KindFile entries only)", got)
	}
	if got := sess.FileCount(); got != 4 {
		t.Errorf("FileCount = %d, want 4 (every entry still registers)", got)
	}
}

func TestFormatBytesShort(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{42, "42 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{3*(1<<20) + (1 << 19), "3.5 MB"},
		{1 << 30, "1.00 GB"},
		{1 << 40, "1.00 TB"},
	}
	for _, tt := range cases {
		if got := formatBytesShort(tt.n); got != tt.want {
			t.Errorf("formatBytesShort(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// dirProgressLine is pure (no clock/global reads), so these use fixed
// timestamps for fully deterministic assertions rather than sleeping on a
// real ticker.

func TestDirProgressLineNoRateWithFewerThanTwoSamples(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	line := dirProgressLine([]dirProgressSample{{bytes: 100, at: base}}, 1000, 1, 4, false)
	if !strings.Contains(line, "0.0 MB/s") {
		t.Errorf("line = %q, want a 0.0 MB/s rate with only one sample", line)
	}
	if !strings.Contains(line, "ETA ~--") {
		t.Errorf("line = %q, want ETA ~-- with only one sample", line)
	}
	if !strings.Contains(line, "files 1/4~") {
		t.Errorf("line = %q, want \"files 1/4~\" (walk not done)", line)
	}
}

func TestDirProgressLineComputesRateAndETA(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// 10 MB scanned over 5 seconds = 2 MB/s; 40 MB remaining of a 50 MB
	// total at that rate = 20s.
	window := []dirProgressSample{
		{bytes: 0, at: base},
		{bytes: 10 << 20, at: base.Add(5 * time.Second)},
	}
	line := dirProgressLine(window, 50<<20, 2, 10, true)
	if !strings.Contains(line, "2.0 MB/s") {
		t.Errorf("line = %q, want \"2.0 MB/s\"", line)
	}
	if !strings.Contains(line, "ETA ~20s") {
		t.Errorf("line = %q, want \"ETA ~20s\"", line)
	}
	if !strings.Contains(line, "files 2/10 ") {
		t.Errorf("line = %q, want \"files 2/10 \" (no ~, walk is done)", line)
	}
	if !strings.Contains(line, "10.0 MB/50.0 MB") {
		t.Errorf("line = %q, want the scanned/total sizes", line)
	}
}

func TestDirProgressLineNoProgressBetweenSamplesShowsNoETA(t *testing.T) {
	// A real plateau (e.g. between files, waiting on a checksum round trip
	// with no new bytes read) must not show a stale or fabricated ETA.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	window := []dirProgressSample{
		{bytes: 5000, at: base},
		{bytes: 5000, at: base.Add(3 * time.Second)},
	}
	line := dirProgressLine(window, 10000, 1, 3, false)
	if !strings.Contains(line, "0.0 MB/s") {
		t.Errorf("line = %q, want 0.0 MB/s with no movement", line)
	}
	if !strings.Contains(line, "ETA ~--") {
		t.Errorf("line = %q, want ETA ~-- with no movement (not a stale/fabricated value)", line)
	}
}

func TestDirProgressLineCompleteShowsZeroETA(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	window := []dirProgressSample{
		{bytes: 100, at: base},
		{bytes: 1000, at: base.Add(time.Second)},
	}
	line := dirProgressLine(window, 1000, 5, 5, true)
	if !strings.Contains(line, "ETA ~0s") {
		t.Errorf("line = %q, want ETA ~0s once scanned has reached the total", line)
	}
}

func TestDirProgressLineWalkTildeReflectsWalkDone(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	window := []dirProgressSample{{bytes: 0, at: base}}
	if line := dirProgressLine(window, 100, 0, 1, false); !strings.Contains(line, "0/1~") {
		t.Errorf("line = %q, want a ~ suffix while walking", line)
	}
	// "ETA ~" always carries its own tilde regardless of walk state, so
	// check the files-count segment specifically ("0/1 " with a trailing
	// space, not "0/1~") rather than a bare "~" anywhere in the line.
	if line := dirProgressLine(window, 100, 0, 1, true); !strings.Contains(line, "0/1 ") {
		t.Errorf("line = %q, want \"0/1 \" (no ~ suffix) once the walk is done", line)
	}
}

func TestDirProgressLineEmptyWindowDoesNotPanic(t *testing.T) {
	// startDirProgressRenderer can in principle render before its first
	// sample is even appended (defensive only; run() always appends
	// before calling this in practice).
	line := dirProgressLine(nil, 0, 0, 0, false)
	if !strings.Contains(line, "files 0/0~") {
		t.Errorf("line = %q, want \"files 0/0~\"", line)
	}
}

// TestDirProgressRendererStartStopDoesNotPanicOrHang is a smoke test of the
// goroutine/timing wrapper itself (dirProgressLine's own tests above cover
// the computation precisely): start, let it tick at least once, stop, and
// confirm Stop returns promptly.
func TestDirProgressRendererStartStopDoesNotPanicOrHang(t *testing.T) {
	sess := NewSyncSession()
	sess.AddFile(newTestFileEntry(0, 1))
	r := startDirProgressRenderer(sess)
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return")
	}
}

// --- runAnnounceBatcherQuickCheck ---
//
// fakeAnnounceAckServer drives the SERVER side of the OpAnnounce{
// FlagQuickCheck}/OpAnnounceAck exchange by hand over one net.Pipe: for
// every OpAnnounce batch it receives, it replies with an OpAnnounceAck
// whose skip-list is exactly the subset of that batch's FileIDs present in
// skip. It also records every Msg it sees into a sentMsgLog, so a test can
// assert on what runAnnounceBatcherQuickCheck actually put on the wire
// (in particular, that a skipped file never produces an OpFileDone).
// Returns once conn is closed.
func fakeAnnounceAckServer(t *testing.T, conn net.Conn, skip map[uint32]bool) *sentMsgLog {
	t.Helper()
	log := &sentMsgLog{}
	go func() {
		for {
			msg, err := readDirMsg(conn)
			if err != nil {
				return
			}
			log.add(msg)
			if Op(msg.Op) != OpAnnounce {
				continue
			}
			trailer := make([]byte, msg.DataSize)
			if _, err := io.ReadFull(conn, trailer); err != nil {
				return
			}
			recs, err := decodeAnnounceBatch(trailer)
			if err != nil {
				return
			}
			var skipped []uint32
			for _, rec := range recs {
				if skip[rec.FileID] {
					skipped = append(skipped, rec.FileID)
				}
			}
			ackTrailer, err := encodeSkipList(skipped)
			if err != nil {
				return
			}
			ack := &Msg{Op: uint8(OpAnnounceAck), DataSize: uint32(len(ackTrailer))}
			ack.MagicHead = stringToFixedSizeArray(magicHead)
			data, err := pack(ack)
			if err != nil {
				return
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
			if len(ackTrailer) > 0 {
				if _, err := conn.Write(ackTrailer); err != nil {
					return
				}
			}
		}
	}()
	return log
}

func TestRunAnnounceBatcherQuickCheckSkipsMatchAndForwardsRest(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	log := fakeAnnounceAckServer(t, server, map[uint32]bool{0: true})

	sess := NewSyncSession()
	feMatch := &FileEntry{ID: 0, Kind: KindFile, RelPath: "match.txt", Size: 123}
	feDiffer := &FileEntry{ID: 1, Kind: KindFile, RelPath: "differ.txt", Size: 10}
	if _, err := sess.AddFile(feMatch); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AddFile(feDiffer); err != nil {
		t.Fatal(err)
	}
	ctrl := newCtrlConn(client)
	dt := &dirTransferState{sess: sess, ctrl: ctrl, magicBytes: ctrl.magicBytes}

	records := make(chan AnnounceRec, 2)
	records <- AnnounceRec{FileID: 0, Kind: KindFile, Path: "match.txt", FileSize: feMatch.Size}
	records <- AnnounceRec{FileID: 1, Kind: KindFile, Path: "differ.txt", FileSize: feDiffer.Size}
	close(records)

	// Mirrors startDirClient's live (non-delete) quick-check wiring: forward
	// is a treeFileChan send, and the wrapper closes the channel once the
	// batcher returns.
	treeFileChan := make(chan *FileEntry, 2)
	errDone := make(chan error, 1)
	go func() {
		err := runAnnounceBatcherQuickCheck(ctrl, client, dt, records, func(fe *FileEntry) error { treeFileChan <- fe; return nil })
		close(treeFileChan)
		errDone <- err
	}()

	var forwarded []*FileEntry
	for fe := range treeFileChan {
		forwarded = append(forwarded, fe)
	}
	select {
	case err := <-errDone:
		if err != nil {
			t.Fatalf("runAnnounceBatcherQuickCheck: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runAnnounceBatcherQuickCheck did not return in time")
	}

	if len(forwarded) != 1 || forwarded[0].ID != 1 {
		t.Fatalf("forwarded = %v, want exactly file 1", forwarded)
	}
	if got := sess.filesDone.Load(); got != 1 {
		t.Errorf("filesDone = %d, want 1 (the skipped file)", got)
	}
	if got := sess.bytesScanned.Load(); got != feMatch.Size {
		t.Errorf("bytesScanned = %d, want %d (the skipped file's size)", got, feMatch.Size)
	}
	// The skipped file (0) must never get an OpFileDone -- the destination
	// already finalized it for itself at announce time (see
	// fileTable.announce); only the non-skipped file would ever get one,
	// and only once real block traffic (not exercised by this unit test)
	// completes it.
	if n := log.fileDoneCount(0); n != 0 {
		t.Errorf("OpFileDone count for skipped file 0 = %d, want 0", n)
	}
	if n := log.fileDoneCount(1); n != 0 {
		t.Errorf("OpFileDone count for non-skipped file 1 = %d, want 0 (no block traffic ran in this test)", n)
	}
}

func TestRunAnnounceBatcherQuickCheckClosesTreeFileChanOnAckError(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	// Server that replies with an OpError instead of OpAnnounceAck.
	go func() {
		msg, err := readDirMsg(server)
		if err != nil {
			return
		}
		trailer := make([]byte, msg.DataSize)
		io.ReadFull(server, trailer)
		sendDirError(server, stringToFixedSizeArray(magicHead), fmt.Errorf("fake rejection"))
	}()

	sess := NewSyncSession()
	fe := &FileEntry{ID: 0, Kind: KindFile, RelPath: "f.txt", Size: 10}
	if _, err := sess.AddFile(fe); err != nil {
		t.Fatal(err)
	}
	ctrl := newCtrlConn(client)
	dt := &dirTransferState{sess: sess, ctrl: ctrl, magicBytes: ctrl.magicBytes}

	records := make(chan AnnounceRec, 1)
	records <- AnnounceRec{FileID: 0, Kind: KindFile, Path: "f.txt", FileSize: fe.Size}
	close(records)

	treeFileChan := make(chan *FileEntry, 1)
	errDone := make(chan error, 1)
	go func() {
		err := runAnnounceBatcherQuickCheck(ctrl, client, dt, records, func(fe *FileEntry) error { treeFileChan <- fe; return nil })
		close(treeFileChan)
		errDone <- err
	}()

	// treeFileChan must still be closed (by the wrapper, mirroring
	// startDirClient's wiring) so a downstream TreeReader ranging over it
	// is never left hanging even on the error path.
	select {
	case _, ok := <-treeFileChan:
		if ok {
			t.Fatal("expected treeFileChan to be closed with nothing sent")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("treeFileChan was not closed in time")
	}
	select {
	case err := <-errDone:
		if err == nil {
			t.Fatal("expected an error from runAnnounceBatcherQuickCheck")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runAnnounceBatcherQuickCheck did not return in time")
	}
}

// --- processDirBlock: fresh-file (created) fast path ---

// fakeBlockServer is a scripted single-connection TCP server for driving
// processDirBlock (or a whole handleDirBlock call) directly: it records
// every frame it receives into a sentMsgLog and replies per opcode --
// OpFileOpen gets an OpFileAck carrying openFlags (FlagCreated or 0),
// OpBlockCheck gets an OpHash carrying checkReply, OpBlockData (payload
// drained) and OpBlockZero get an OpAck; OpFileClose is fire-and-forget and
// just logged. A real TCP listener, not a net.Pipe, because these paths
// take an *AutoReconnectTCP, which dials an address.
func fakeBlockServer(t *testing.T, openFlags uint8, checkReply []byte) (*net.TCPAddr, *sentMsgLog) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	log := &sentMsgLog{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		magic := stringToFixedSizeArray(magicHead)
		for {
			msg, err := readDirMsg(conn)
			if err != nil {
				return
			}
			log.add(msg)
			switch Op(msg.Op) {
			case OpFileOpen:
				ackMsg := &Msg{MagicHead: magic, Op: uint8(OpFileAck), Flags: openFlags, FileID: msg.FileID, BlockSize: msg.BlockSize, FileSize: msg.FileSize}
				data, _ := pack(ackMsg)
				conn.Write(data)
			case OpBlockCheck:
				hdr := &Msg{MagicHead: magic, Op: uint8(OpHash), FileID: msg.FileID, BlockIdx: msg.BlockIdx, BlockSize: msg.BlockSize, FileSize: msg.FileSize, DataSize: uint32(len(checkReply))}
				data, _ := pack(hdr)
				conn.Write(data)
				conn.Write(checkReply)
			case OpBlockData:
				payload := make([]byte, msg.DataSize)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
				ackMsg := &Msg{MagicHead: magic, Op: uint8(OpAck), FileID: msg.FileID, BlockIdx: msg.BlockIdx, BlockSize: msg.BlockSize, FileSize: msg.FileSize}
				data, _ := pack(ackMsg)
				conn.Write(data)
			case OpBlockZero:
				ackMsg := &Msg{MagicHead: magic, Op: uint8(OpAck), FileID: msg.FileID, BlockIdx: msg.BlockIdx, BlockSize: msg.BlockSize, FileSize: msg.FileSize}
				data, _ := pack(ackMsg)
				conn.Write(data)
			}
		}
	}()
	addr, err := net.ResolveTCPAddr("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return addr, log
}

func (l *sentMsgLog) opCount(op Op) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if Op(m.Op) == op {
			n++
		}
	}
	return n
}

func TestProcessDirBlockCreatedZeroBlockSendsNothing(t *testing.T) {
	addr, log := fakeBlockServer(t, 0, nil)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()
	if err := conn.Dial(); err != nil {
		t.Fatal(err)
	}
	dt := &dirTransferState{sess: NewSyncSession(), magicBytes: stringToFixedSizeArray(magicHead)}

	pb := PrecomputedBlock{FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: 4096, IsZero: true, Hash: zeroBlockHash}
	outcome, err := processDirBlock(conn, dt, pb, true)
	if err != nil {
		t.Fatalf("processDirBlock: %v", err)
	}
	if outcome.changed {
		t.Error("created zero block reported changed=true, want false (dest hole already reads as zeros)")
	}
	// Nothing must have gone over the wire at all -- give any wrongly-sent
	// frame a moment to land before asserting.
	time.Sleep(30 * time.Millisecond)
	log.mu.Lock()
	n := len(log.msgs)
	log.mu.Unlock()
	if n != 0 {
		t.Errorf("created zero block put %d frame(s) on the wire, want 0", n)
	}
}

func TestProcessDirBlockCreatedDataBlockSkipsChecksumExchange(t *testing.T) {
	addr, log := fakeBlockServer(t, 0, nil)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()
	if err := conn.Dial(); err != nil {
		t.Fatal(err)
	}
	dt := &dirTransferState{sess: NewSyncSession(), magicBytes: stringToFixedSizeArray(magicHead), noCompress: true}

	data := []byte("some payload")
	pb := PrecomputedBlock{FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: uint64(len(data)), Data: data, Hash: checksum(data)}
	outcome, err := processDirBlock(conn, dt, pb, true)
	if err != nil {
		t.Fatalf("processDirBlock: %v", err)
	}
	if !outcome.changed {
		t.Error("created data block reported changed=false, want true")
	}
	if n := log.opCount(OpBlockCheck); n != 0 {
		t.Errorf("created data block sent %d OpBlockCheck frame(s), want 0 -- the whole point of the fast path", n)
	}
	if n := log.opCount(OpBlockData); n != 1 {
		t.Errorf("created data block sent %d OpBlockData frame(s), want 1", n)
	}
}

// rejectingOpenServer accepts connections and rejects every OpFileOpen the
// way dirHandleConn does for an unacquirable file -- sendDirErrorFlags with
// the given flags, then kill the connection -- logging every frame seen.
func rejectingOpenServer(t *testing.T, errFlags uint8) (*net.TCPAddr, *sentMsgLog) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	log := &sentMsgLog{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					msg, err := readDirMsg(c)
					if err != nil {
						return
					}
					log.add(msg)
					if Op(msg.Op) == OpFileOpen {
						sendDirErrorFlags(c, stringToFixedSizeArray(magicHead), fmt.Errorf("rejected"), errFlags)
						return
					}
				}
			}(conn)
		}
	}()
	addr, err := net.ResolveTCPAddr("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return addr, log
}

// TestHandleDirBlockFailsFastOnRejectedFileOpen pins the errPermanent fast
// path end to end: an OpError carrying FlagErrPermanent in reply to
// OpFileOpen (the file is already finalized -- including one that failed
// at announce time) must fail the file after EXACTLY ONE attempt, with no
// retry backoff: with thousands of files doomed at once (a full
// destination disk), the old 3-attempts-plus-backoff-each behavior turned
// the failure into hours of churn.
func TestHandleDirBlockFailsFastOnRejectedFileOpen(t *testing.T) {
	addr, log := rejectingOpenServer(t, FlagErrPermanent)

	sess := NewSyncSession()
	if _, err := sess.AddFile(newTestFileEntry(0, 1)); err != nil {
		t.Fatal(err)
	}
	dt, ctrlLog := newTestDirTransferState(t, sess)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()

	var b dirWorkerBinding
	pb := PrecomputedBlock{FileID: 0, BlockIdx: 0, BlockSize: 4096, FileSize: 4096, Data: []byte("x"), Hash: checksum([]byte("x")), Last: true}
	start := time.Now()
	dt.handleDirBlock(conn, pb, &b)
	elapsed := time.Since(start)

	if !sess.isFailed(0) {
		t.Error("file 0 not marked failed after a rejected open")
	}
	if elapsed > time.Second {
		t.Errorf("handleDirBlock took %v -- retry backoff ran despite the permanent rejection", elapsed)
	}
	if n := log.opCount(OpFileOpen); n != 1 {
		t.Errorf("OpFileOpen count = %d, want exactly 1 (no retries)", n)
	}
	// The failed file must still resolve normally: exactly one OpFileDone.
	waitForFileDoneCount(t, ctrlLog, 0, 1, 2*time.Second)
}

// TestHandleDirBlockRetriesTransientRejectedFileOpen is the counterpart
// guarding the OTHER direction: an OpError WITHOUT FlagErrPermanent must
// keep the full retry behavior. This is load-bearing, not just politeness:
// a worker's OpFileOpen legitimately races ahead of a still-buffering
// announce batch, the server transiently rejects it as "never announced",
// and the retry loop is what absorbs that race -- treating it as permanent
// spuriously failed the file AND (because its OpFileDone then preceded its
// announce) hung the server's WaitAllFinalized on a file no one would ever
// finish. Caught by exactly this scenario in TestE2EDirLargeTreeManyFiles.
func TestHandleDirBlockRetriesTransientRejectedFileOpen(t *testing.T) {
	addr, log := rejectingOpenServer(t, 0)

	sess := NewSyncSession()
	if _, err := sess.AddFile(newTestFileEntry(0, 1)); err != nil {
		t.Fatal(err)
	}
	dt, _ := newTestDirTransferState(t, sess)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()

	var b dirWorkerBinding
	pb := PrecomputedBlock{FileID: 0, BlockIdx: 0, BlockSize: 4096, FileSize: 4096, Data: []byte("x"), Hash: checksum([]byte("x")), Last: true}
	dt.handleDirBlock(conn, pb, &b)

	if n := log.opCount(OpFileOpen); n != maxRetries {
		t.Errorf("OpFileOpen count = %d, want %d -- a transient rejection must be retried", n, maxRetries)
	}
}

// TestHandleDirBlockCapturesCreatedFlagFromBindAck drives the full
// per-block path (handleDirBlock: bind via dirWorkerBind, then
// processDirBlock) rather than processDirBlock alone, pinning the one link
// the direct tests above can't: the FlagCreated bit on the OpFileOpen ack
// must actually reach processDirBlock through the worker's binding. If that
// capture is ever dropped, everything still syncs CORRECTLY (the client
// just falls back to the checksum exchange), so only this frame-count
// assertion -- an OpBlockData with no preceding OpBlockCheck -- can catch
// the fast path silently disengaging.
func TestHandleDirBlockCapturesCreatedFlagFromBindAck(t *testing.T) {
	addr, log := fakeBlockServer(t, FlagCreated, nil)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()

	sess := NewSyncSession()
	data := []byte("payload")
	if _, err := sess.AddFile(&FileEntry{ID: 0, Kind: KindFile, RelPath: "f.bin", Size: uint64(len(data)), BlockSize: 4096, Blocks: 1}); err != nil {
		t.Fatal(err)
	}
	dt, ctrlLog := newTestDirTransferState(t, sess)
	dt.noCompress = true

	var b dirWorkerBinding
	pb := PrecomputedBlock{FileID: 0, BlockIdx: 0, BlockSize: 4096, FileSize: uint64(len(data)), Data: data, Hash: checksum(data), Last: true}
	dt.handleDirBlock(conn, pb, &b)

	if !b.created {
		t.Error("binding.created = false after an OpFileAck carrying FlagCreated")
	}
	if n := log.opCount(OpFileOpen); n != 1 {
		t.Errorf("OpFileOpen count = %d, want 1", n)
	}
	if n := log.opCount(OpBlockCheck); n != 0 {
		t.Errorf("OpBlockCheck count = %d, want 0 -- the created flag was not carried from the bind ack into processDirBlock", n)
	}
	if n := log.opCount(OpBlockData); n != 1 {
		t.Errorf("OpBlockData count = %d, want 1", n)
	}
	// The single-block file must complete normally through the fast path.
	waitForFileDoneCount(t, ctrlLog, 0, 1, 2*time.Second)
}

func TestProcessDirBlockNotCreatedKeepsChecksumExchange(t *testing.T) {
	data := []byte("some payload")
	hash := checksum(data)
	// The fake server replies with the SAME hash the client computed, so
	// the block compares equal and no data follows -- proving the checksum
	// exchange both happened and did its job.
	addr, log := fakeBlockServer(t, 0, hash)
	conn := NewAutoReconnectTCP(addr)
	defer conn.Close()
	if err := conn.Dial(); err != nil {
		t.Fatal(err)
	}
	dt := &dirTransferState{sess: NewSyncSession(), magicBytes: stringToFixedSizeArray(magicHead), noCompress: true}

	pb := PrecomputedBlock{FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: uint64(len(data)), Data: data, Hash: hash}
	outcome, err := processDirBlock(conn, dt, pb, false)
	if err != nil {
		t.Fatalf("processDirBlock: %v", err)
	}
	if outcome.changed {
		t.Error("matching-hash block reported changed=true, want false")
	}
	if n := log.opCount(OpBlockCheck); n != 1 {
		t.Errorf("created=false sent %d OpBlockCheck frame(s), want 1", n)
	}
	if n := log.opCount(OpBlockData); n != 0 {
		t.Errorf("created=false with matching hash sent %d OpBlockData frame(s), want 0", n)
	}
}

// --- walk skip logging (bounded, then summarized) ---

func TestSkipReasonKeyCollapsesFormattedReasons(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"unsupported file type: Dc---------", "unsupported file type"},
		{"unsupported file type: S---------", "unsupported file type"},
		{"excluded", "excluded"},
		{"symlink", "symlink"},
		{"readlink: permission denied", "readlink"},
	} {
		if got := skipReasonKey(tc.reason); got != tc.want {
			t.Errorf("skipReasonKey(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// TestSkipLoggerBoundsLinesAndCountsAll: every device node in a container's
// /dev shares one reason bucket, so only the first maxSkipLinesPerReason are
// listed -- but the total must still be complete and accurate.
func TestSkipLoggerBoundsLinesAndCountsAll(t *testing.T) {
	s := newSkipLogger()
	const devices = 500
	for i := 0; i < devices; i++ {
		s.log(fmt.Sprintf("dev/tty%d", i), "unsupported file type: Dc---------")
	}
	for i := 0; i < 3; i++ {
		s.log(fmt.Sprintf("logs/%d.log", i), "excluded")
	}

	if got := s.shown["unsupported file type"]; got != maxSkipLinesPerReason {
		t.Errorf("listed %d device lines, want the cap of %d", got, maxSkipLinesPerReason)
	}
	if got := s.counts["unsupported file type"]; got != devices {
		t.Errorf("counted %d devices, want %d", got, devices)
	}
	// A reason under the cap is never truncated.
	if got := s.shown["excluded"]; got != 3 {
		t.Errorf("listed %d excluded lines, want all 3", got)
	}
	if s.total != devices+3 {
		t.Errorf("total = %d, want %d", s.total, devices+3)
	}
	// The summary orders reasons by first appearance, so it is stable
	// across runs rather than following map iteration order.
	if len(s.order) != 2 || s.order[0] != "unsupported file type" || s.order[1] != "excluded" {
		t.Errorf("order = %v, want first-seen order", s.order)
	}
}

func TestSkipLoggerSummaryIsSilentWhenNothingSkipped(t *testing.T) {
	s := newSkipLogger()
	if s.total != 0 {
		t.Fatalf("fresh logger total = %d", s.total)
	}
	s.summary() // must not panic and must log nothing
}

// --- adaptive quick-check announce batching ---

func TestAdaptAnnounceBatchConvergesPerLink(t *testing.T) {
	// A fast link (loopback) must stay at the small batch: a big one costs
	// the overlap between the client's walk and the server's stat work.
	size := announceBatchSize
	for i := 0; i < 10; i++ {
		size = adaptAnnounceBatch(size, 100*time.Microsecond)
	}
	if size != announceBatchSize {
		t.Errorf("on a fast link the batch drifted to %d, want it pinned at %d", size, announceBatchSize)
	}

	// A slow link must grow to the cap, and get there quickly.
	size = announceBatchSize
	for i := 0; i < 10; i++ {
		size = adaptAnnounceBatch(size, 20*time.Millisecond)
	}
	if size != announceBatchMax {
		t.Errorf("on a 20ms link the batch reached %d, want the %d cap", size, announceBatchMax)
	}

	// And back down when the link improves.
	for i := 0; i < 10; i++ {
		size = adaptAnnounceBatch(size, 100*time.Microsecond)
	}
	if size != announceBatchSize {
		t.Errorf("batch stuck at %d after the link got fast, want %d", size, announceBatchSize)
	}
}

func TestAdaptAnnounceBatchStaysInRange(t *testing.T) {
	for _, rtt := range []time.Duration{0, time.Microsecond, 5 * time.Millisecond, time.Second} {
		size := announceBatchSize
		for i := 0; i < 40; i++ {
			size = adaptAnnounceBatch(size, rtt)
			if size < announceBatchSize || size > announceBatchMax {
				t.Fatalf("rtt=%v produced out-of-range batch %d", rtt, size)
			}
		}
	}
	// An RTT between the two thresholds must not move the size at all --
	// that dead band is what stops it oscillating batch to batch.
	mid := (announceGrowRTT + announceShrinkRTT) / 2
	for _, start := range []int{announceBatchSize, 800, announceBatchMax} {
		if got := adaptAnnounceBatch(start, mid); got != start {
			t.Errorf("rtt=%v moved the batch %d -> %d; the dead band must hold it", mid, start, got)
		}
	}
}

// --- progress line redraw ---

// TestPadOverPreviousErasesLeftovers is the "ETA ~79h02mm3m" regression: the
// progress line is redrawn in place with \r, so a render shorter than the one
// before it leaves that one's tail visible unless it is padded over.
func TestPadOverPreviousErasesLeftovers(t *testing.T) {
	long := "files 15297705/15944363~ · 44.16 TB/64.66 TB · 175.5 MB/s · ETA ~179h02m"
	short := "files 15297705/15944363~ · 44.16 TB/64.66 TB · 75.5 MB/s · ETA ~79h02m"

	out1, w1 := padOverPrevious(long, 0)
	if out1 != long || w1 != len(long) {
		t.Fatalf("first render was altered: %q (width %d)", out1, w1)
	}

	out2, w2 := padOverPrevious(short, w1)
	if len(out2) != len(long) {
		t.Errorf("shorter render is %d wide, want %d so it covers the previous line", len(out2), len(long))
	}
	if strings.TrimRight(out2, " ") != short {
		t.Errorf("padding altered the content: %q", out2)
	}
	// The remembered width must stay at the widest line still on screen --
	// dropping to the short line's own width would let an even earlier,
	// longer line's tail survive the next render.
	if w2 != len(long) {
		t.Errorf("remembered width = %d, want %d (the widest line drawn so far)", w2, len(long))
	}

	// A third, equally short render must still cover the original long one.
	out3, _ := padOverPrevious(short, w2)
	if len(out3) != len(long) {
		t.Errorf("third render is %d wide, want %d", len(out3), len(long))
	}
}

func TestPadOverPreviousGrowingLineIsUntouched(t *testing.T) {
	short := "files 1/2"
	long := "files 100/200 · 1.0 MB/2.0 MB"
	_, w := padOverPrevious(short, 0)
	out, w2 := padOverPrevious(long, w)
	if out != long {
		t.Errorf("a longer line must not be padded: %q", out)
	}
	if w2 != len(long) {
		t.Errorf("width = %d, want %d", w2, len(long))
	}
}

// --- wire-rate reporting ---

// TestNetSegmentReportsWireRateAndRatio: the headline MB/s is the SCAN rate,
// which on a compressed or mostly-unchanged run says nothing about whether
// the link is saturated. The net segment is what makes that answerable.
func TestNetSegmentReportsWireRateAndRatio(t *testing.T) {
	base := time.Now()
	// 100 MB scanned, 25 MB on the wire, over 10s -> 2.5 MB/s net, x4.
	window := []dirProgressSample{
		{bytes: 0, net: 0, at: base},
		{bytes: 100 << 20, net: 25 << 20, at: base.Add(10 * time.Second)},
	}
	seg := netSegment(window)
	if !strings.Contains(seg, "net 2.5 MB/s") {
		t.Errorf("segment %q does not report the wire rate", seg)
	}
	if !strings.Contains(seg, "×4.00") {
		t.Errorf("segment %q does not report the compression ratio", seg)
	}

	// It must reach the full line too.
	line := dirProgressLine(window, 200<<20, 5, 10, true)
	if !strings.Contains(line, "net 2.5 MB/s") {
		t.Errorf("progress line lost the wire rate: %q", line)
	}
	// ...without disturbing the scan rate that was always there.
	if !strings.Contains(line, "10.0 MB/s") {
		t.Errorf("progress line lost the scan rate: %q", line)
	}
}

// TestNetSegmentSilentWhenNothingSent: an unchanged re-sync scans everything
// and sends nothing; a permanent "net 0.0 MB/s" would be pure noise.
func TestNetSegmentSilentWhenNothingSent(t *testing.T) {
	base := time.Now()
	window := []dirProgressSample{
		{bytes: 0, net: 0, at: base},
		{bytes: 500 << 20, net: 0, at: base.Add(5 * time.Second)},
	}
	if seg := netSegment(window); seg != "" {
		t.Errorf("segment = %q, want empty when nothing was sent", seg)
	}
	line := dirProgressLine(window, 1<<30, 3, 3, true)
	if strings.Contains(line, "net ") {
		t.Errorf("progress line carries a net segment with no traffic: %q", line)
	}
}

// TestNetSegmentOmitsRatioWhenIncompressible: sending as many bytes as were
// read means compression bought nothing; printing "x1.00" implies otherwise.
func TestNetSegmentOmitsRatioWhenIncompressible(t *testing.T) {
	base := time.Now()
	window := []dirProgressSample{
		{bytes: 0, net: 0, at: base},
		{bytes: 50 << 20, net: 50 << 20, at: base.Add(5 * time.Second)},
	}
	seg := netSegment(window)
	if !strings.Contains(seg, "net 10.0 MB/s") {
		t.Errorf("segment %q lost the wire rate", seg)
	}
	if strings.Contains(seg, "×") {
		t.Errorf("segment %q claims a compression ratio for incompressible data", seg)
	}
}

// --- shared announce batching ---

func TestBatchRecordsFlushesAtTargetAndAtClose(t *testing.T) {
	records := make(chan AnnounceRec, 16)
	for i := 0; i < 7; i++ {
		records <- AnnounceRec{FileID: uint32(i)}
	}
	close(records)

	var batch []AnnounceRec
	var flushes [][]uint32
	err := batchRecords(records, &batch, func() int { return 3 }, func() error {
		ids := make([]uint32, len(batch))
		for i, r := range batch {
			ids[i] = r.FileID
		}
		flushes = append(flushes, ids)
		batch = batch[:0]
		return nil
	})
	if err != nil {
		t.Fatalf("batchRecords: %v", err)
	}
	// 7 records at a target of 3: two full batches, then the remainder at
	// close. An empty final flush is the caller's business, not the loop's.
	want := [][]uint32{{0, 1, 2}, {3, 4, 5}, {6}}
	if len(flushes) != len(want) {
		t.Fatalf("flushes = %v, want %v", flushes, want)
	}
	for i := range want {
		if len(flushes[i]) != len(want[i]) {
			t.Fatalf("flushes = %v, want %v", flushes, want)
		}
		for j := range want[i] {
			if flushes[i][j] != want[i][j] {
				t.Fatalf("flushes = %v, want %v", flushes, want)
			}
		}
	}
}

// TestBatchRecordsDrainsOnFlushError is the wedge regression. The producer
// feeding this channel is the same goroutine that drives walkSource, so if a
// flush failure made the loop return while records were still pending, that
// goroutine would block forever on a send nobody reads and the client would
// hang with no timeout. The quick-check batcher guarded against this; the
// plain one did not until the loop was shared.
func TestBatchRecordsDrainsOnFlushError(t *testing.T) {
	records := make(chan AnnounceRec)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(records)
		for i := 0; i < 50; i++ {
			records <- AnnounceRec{FileID: uint32(i)}
		}
	}()

	var batch []AnnounceRec
	wantErr := errors.New("flush exploded")
	err := batchRecords(records, &batch, func() int { return 2 }, func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	select {
	case <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("producer is still blocked sending: the loop returned without draining")
	}
}

// TestBatchRecordsDrainsOnFinalFlushError: the same hazard exists for the
// flush that happens after the channel closes -- there the producer is
// already done, so this pins that the error still propagates.
func TestBatchRecordsDrainsOnFinalFlushError(t *testing.T) {
	records := make(chan AnnounceRec, 2)
	records <- AnnounceRec{FileID: 1}
	close(records)

	var batch []AnnounceRec
	wantErr := errors.New("final flush exploded")
	err := batchRecords(records, &batch, func() int { return 100 }, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
