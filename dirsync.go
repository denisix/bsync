package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// liveFile is one registered entry plus the transfer bookkeeping that used
// to live in dirTransferState's two per-FileID sync.Maps (remaining,
// doneOnce): the countdown of blocks left before OpFileDone, and (via
// SyncSession.claimDone) the exactly-one-completion guard. Bundling them
// here means all three pieces of a file's state are created and destroyed
// together -- released the instant the file finishes, instead of
// accumulating one node per file for the LIFE OF THE RUN in three separate
// unbounded structures (SyncSession.files, dirTransferState.remaining,
// dirTransferState.doneOnce). remaining is manipulated with
// atomic.AddInt64/LoadInt64 rather than atomic.Int64 purely so liveFile
// stays an ordinary, obviously-copyable-free struct; nothing here is ever
// copied by value.
type liveFile struct {
	fe        *FileEntry
	remaining int64 // atomic; blocks left before OpFileDone
}

// SyncSession tracks progress and per-file failure state for one
// directory-tree transfer. It replaces the package-level globals the
// single-file path uses for the same purpose (lastBlockNum, totCompSize,
// totOrigSize, diffs, v_skipIdx, v_fileSize, t0 in client.go; srvOnce,
// srvFileSize, srvLastBlockNum, etc. in server.go) — none of those can hold
// more than one file's worth of state at a time, and directory mode needs
// exactly that. All counters are atomics so the hot per-block path never
// takes a lock.
type SyncSession struct {
	startedAt time.Time

	filesMu sync.RWMutex
	// live holds only entries something can still look up: registered by
	// AddFile, removed by Release/claimDone the instant nothing will ever
	// need them again (see those methods, and the release points in
	// startDirClient/runAnnounceBatcherQuickCheck). This replaces a
	// []*FileEntry indexed by FileID that was append-only and NEVER
	// pruned -- at tree scale that retained every entry, plus two
	// separately-allocated path strings each (before AbsPath was removed,
	// one; the RelPath string itself remains), for the entire run
	// regardless of how quickly each file's own transfer actually
	// finished.
	live   map[uint32]*liveFile
	nextID uint32 // replaces len(files): AddFile's dense-ID check, and FileCount's total

	walkDone atomic.Bool

	blocksDone atomic.Uint64
	bytesOrig  atomic.Uint64
	bytesNet   atomic.Uint64
	diffs      atomic.Uint64

	// totalBytesKnown/totalBlocksKnown are running sums over every KindFile
	// entry AddFile has seen so far (updated there, O(1) per call) — a
	// growing, partial total until WalkDone, exactly like FileCount/
	// TotalBytes's existing "partial while walking" semantics, just no
	// longer requiring an O(files) rescan on every read (TotalBytes used to
	// scan s.files itself; see its comment below).
	//
	// bytesScanned is the sum of every block's actual byte length that has
	// been read+checksummed so far, REGARDLESS of whether it differed from
	// the destination (unlike bytesOrig/bytesNet above, which only count
	// CHANGED blocks) — this is what an overall "how far through the tree
	// are we" progress metric needs: on a mostly-unchanged re-run, almost
	// every block is skipped after a cheap checksum compare, and a metric
	// based on bytesNet alone would sit at ~0 for the whole run.
	totalBytesKnown  atomic.Uint64
	totalBlocksKnown atomic.Uint64
	bytesScanned     atomic.Uint64

	// totalFilesKnown counts only KindFile entries AddFile has seen -- the
	// correct denominator for a "files done/total" display, since dirs and
	// KindSkipped placeholders never get a MarkFileDone and would otherwise
	// keep the numerator permanently short of the denominator.
	totalFilesKnown atomic.Uint32

	// estFiles/estBytes are the display-only totals fed by the fast
	// metadata pre-scan (estimateTree): they converge on the true totals
	// within seconds, while totalFilesKnown/totalBytesKnown grow only at
	// transfer pace (the walk is back-pressured by the pipeline). Read via
	// resolveProgressTotals, which prefers the exact values once the real
	// walk finishes. Never used for transfer bookkeeping.
	estFiles atomic.Uint32
	estBytes atomic.Uint64

	filesDone    atomic.Uint32
	filesSkipped atomic.Uint32

	failed sync.Map // FileID (uint32) -> error
}

// NewSyncSession creates an empty session with startedAt set to now.
func NewSyncSession() *SyncSession {
	return &SyncSession{startedAt: time.Now(), live: make(map[uint32]*liveFile)}
}

// AddFile registers fe in the session's live table and returns its FileID.
// fe.ID must already be set by the caller (walkSource assigns dense,
// sequential IDs, guaranteed in the real pipeline; only a bug there, not
// any external input, could violate it); AddFile checks it matches the
// next expected ID. Returns an error rather than panicking: a panic in
// a server/transfer data path is a liveness hazard (it can bring down the
// whole process instead of just failing this one transfer), and every
// other error in this file is returned, not panicked.
func (s *SyncSession) AddFile(fe *FileEntry) (uint32, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if fe.ID != s.nextID {
		return 0, fmt.Errorf("SyncSession.AddFile: FileID %d does not match next slot %d", fe.ID, s.nextID)
	}
	// remaining is seeded from fe.Blocks, floored at 1: recordBlockDone/
	// forceFileDone are only ever invoked for a KindFile entry with
	// Size > 0 (TreeReader filters out everything else before either could
	// fire), which always has Blocks >= 1 in practice -- the floor exists
	// only so their arithmetic can't underflow if that invariant is ever
	// violated. Irrelevant for every other kind, whose remaining is never
	// read.
	remaining := int64(fe.Blocks)
	if remaining == 0 {
		remaining = 1
	}
	s.live[fe.ID] = &liveFile{fe: fe, remaining: remaining}
	s.nextID++
	if fe.Kind == KindFile {
		s.totalBytesKnown.Add(fe.Size)
		s.totalBlocksKnown.Add(uint64(fe.Blocks))
		s.totalFilesKnown.Add(1)
	}
	return fe.ID, nil
}

// FileByID returns the entry for id, or nil if it is not currently live
// (never registered, or already released -- see Release/claimDone).
func (s *SyncSession) FileByID(id uint32) *FileEntry {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	lf, ok := s.live[id]
	if !ok {
		return nil
	}
	return lf.fe
}

// liveEntry returns id's live bookkeeping record (for atomic manipulation
// of its remaining-block counter), or nil if id is not currently live. A
// nil return from a caller like recordBlockDone/forceFileDone is not an
// error: it means every block the file will ever produce has already been
// accounted for and the file has already completed.
func (s *SyncSession) liveEntry(id uint32) *liveFile {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	return s.live[id]
}

// Release drops id from the live table. Idempotent -- releasing an
// already-released or never-registered id is a harmless no-op. Call once
// nothing will ever look this entry up by FileID again (a directory,
// symlink, special file, or skipped placeholder right after it is handed
// off to the announce/tree-feed channels; an empty file once its
// synthetic OpFileDone has been sent).
func (s *SyncSession) Release(id uint32) {
	s.filesMu.Lock()
	delete(s.live, id)
	s.filesMu.Unlock()
}

// claimDone removes id from the live table and reports whether THIS call
// is the one that removed it. It is the exactly-one-completion guard that
// dirTransferState.doneOnce (a sync.Map of *sync.Once, one node per file,
// never deleted) used to provide: the map delete is atomic under filesMu,
// so of any number of racing callers exactly one observes ok==true. This is
// strictly stronger than the sync.Once it replaces in two ways: the
// completion state can never be re-created after the entry is gone (a
// straggler call simply finds nothing, rather than being able to
// resurrect a *sync.Once under a reused map key), and a losing caller
// returns immediately instead of blocking inside Once.Do until the
// winner's own work -- which used to include a network send -- completes.
func (s *SyncSession) claimDone(id uint32) (*FileEntry, bool) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	lf, ok := s.live[id]
	if !ok {
		return nil, false
	}
	delete(s.live, id)
	return lf.fe, true
}

// LiveCount returns the number of entries currently live (registered but
// not yet released) -- the number a memory-bounded design keeps small
// regardless of tree size. Exported for tests; not used in the transfer
// path itself.
func (s *SyncSession) LiveCount() int {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	return len(s.live)
}

// FileCount returns the number of files ever registered with the session
// (nextID), NOT the number currently live -- entries are released as their
// transfer completes, but the total assigned only ever grows. This grows as
// the walk discovers more entries, so a caller must not treat it as a final
// total until WalkDone reports true. See LiveCount for the currently-live
// count.
func (s *SyncSession) FileCount() int {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	return int(s.nextID)
}

// TotalBytes sums the Size of every KindFile entry known so far. Grows
// alongside FileCount as the walk discovers more entries. O(1): backed by
// totalBytesKnown, a running total AddFile maintains as entries arrive,
// rather than rescanning s.files on every call (a real cost on a
// large/still-growing tree if this were read on every tick of a progress
// renderer, which is exactly what it's now used for).
func (s *SyncSession) TotalBytes() uint64 {
	return s.totalBytesKnown.Load()
}

// TotalBlocks sums the Blocks of every KindFile entry known so far, the
// block-count counterpart to TotalBytes. Also O(1).
func (s *SyncSession) TotalBlocks() uint64 {
	return s.totalBlocksKnown.Load()
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

// AddBlockProgress records one block's outcome. blockLen is the block's
// actual byte length (its contribution to bytesScanned regardless of
// outcome); netBytes/origBytes are the compressed/raw byte counts (0 for a
// skipped/unchanged block), and changed reports whether the block actually
// differed and was transferred.
func (s *SyncSession) AddBlockProgress(changed bool, blockLen, netBytes, origBytes uint64) {
	s.blocksDone.Add(1)
	s.bytesScanned.Add(blockLen)
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

// --- client-side directory transfer (Phase 4: on-demand hashing only) ---

// announceBatchSize caps how many AnnounceRec accumulate before a batch is
// flushed as one OpAnnounce frame, so a 100k-file tree sends ~500 frames
// instead of 100k.
const announceBatchSize = 200

// The quick-check batcher's exchange is SYNCHRONOUS -- it sends a batch and
// waits for the skip list before moving on -- so its batch size trades two
// costs against each other, and the right answer depends entirely on the
// link:
//
//   - On a high-latency link the round trips dominate. Bigger batches mean
//     fewer of them: measured on a 20 ms simulated WAN, a 20k-file unchanged
//     re-sync went from 3.02 s at 200 records to 1.22 s at 1000.
//   - On loopback the round trip is nearly free, and a big batch instead
//     COSTS overlap: the server can't start stat-ing a batch until the whole
//     thing arrives, so the client's walk and the server's quick-check stop
//     running concurrently. The same re-sync measured 0.33 s at 200 but
//     0.53 s at 1000.
//
// So the size is adapted at run time from the observed ack latency rather
// than fixed: start at announceBatchSize and grow toward announceBatchMax
// only on a link slow enough for the round trips to be worth avoiding.
const (
	announceBatchMax = 2000
	// Grow when a batch's round trip costs more than this, shrink when it
	// costs less than announceShrinkRTT. The gap between the two is
	// deliberate: it stops a link hovering near one threshold from
	// oscillating between sizes on every batch.
	announceGrowRTT   = 8 * time.Millisecond
	announceShrinkRTT = 2 * time.Millisecond
)

// dirDedicatedChanDepth bounds each transfer worker's private routed-block
// queue (see affinityWorker). Deliberately small: it is not a throughput
// knob -- the shared channel is where bulk buffering lives -- and every
// block sitting here is (a) byteBudget the TreeReader cannot reclaim yet,
// hostage to one worker that may be mid-retry, and (b) an extra file
// potentially in flight ahead of its walk-order neighbours, which widens
// OrderedWriter's reorder window and its FileID-ascending-key destination
// write-locality assumption (writer.go). Raise only with a measured reason.
const dirDedicatedChanDepth = 2

// ctrlConn serializes writes to the control connection: the announce
// batcher, per-file OpFileDone signals from transfer workers, and
// startDirClient's own OpAnnounceEnd/OpTreeDone all write to it
// concurrently, and a single net.Conn is not safe for concurrent writers
// without this.
type ctrlConn struct {
	conn       net.Conn
	mu         sync.Mutex
	magicBytes [magicLen]byte
}

func newCtrlConn(conn net.Conn) *ctrlConn {
	return &ctrlConn{conn: conn, magicBytes: stringToFixedSizeArray(magicHead)}
}

func (c *ctrlConn) send(msg *Msg, trailer []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg.MagicHead = c.magicBytes
	data, err := pack(msg)
	if err != nil {
		return err
	}
	if err := connWrite(c.conn, data); err != nil {
		return err
	}
	if len(trailer) > 0 {
		return connWrite(c.conn, trailer)
	}
	return nil
}

// batchRecords is the accumulate/flush loop both announce batchers share:
// append until target() records have piled up, flush, then flush whatever is
// left once the channel closes. Only flush itself differs between the two --
// the plain batcher fires and forgets, the quick-check one waits for a skip
// list -- and target is a function because the quick-check batcher resizes
// its batch from the measured link (see adaptAnnounceBatch).
//
// On a flush error it DRAINS the rest of records before returning. Without
// that, the fan-out goroutine feeding this channel blocks forever on a send
// nobody will read -- and it is the same goroutine that drives walkSource, so
// walkDone never fires and the whole client wedges with no timeout. The
// quick-check batcher had this guard; the plain one did not, which left that
// wedge reachable on any -ignore-times run whose announce send failed.
func batchRecords(records <-chan AnnounceRec, batch *[]AnnounceRec, target func() int, flush func() error) error {
	drain := func() {
		for range records {
		}
	}
	for rec := range records {
		*batch = append(*batch, rec)
		if len(*batch) >= target() {
			if err := flush(); err != nil {
				drain()
				return err
			}
		}
	}
	if err := flush(); err != nil {
		drain()
		return err
	}
	return nil
}

// runAnnounceBatcher drains records, sending an OpAnnounce frame every time
// announceBatchSize records have accumulated, plus a final one for
// whatever's left when records closes.
func runAnnounceBatcher(ctrl *ctrlConn, records <-chan AnnounceRec) error {
	batch := make([]AnnounceRec, 0, announceBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		trailer, err := encodeAnnounceBatch(batch)
		if err != nil {
			return fmt.Errorf("encode announce batch: %w", err)
		}
		if err := ctrl.send(&Msg{Op: uint8(OpAnnounce), DataSize: uint32(len(trailer))}, trailer); err != nil {
			return fmt.Errorf("send announce batch: %w", err)
		}
		batch = batch[:0]
		return nil
	}
	return batchRecords(records, &batch, func() int { return announceBatchSize }, flush)
}

// adaptAnnounceBatch picks the next quick-check batch size from the round
// trip the last one actually cost (see announceBatchMax for why this is
// tuned rather than fixed). Doubling and halving, rather than tracking a
// target directly, keeps it converging in a handful of batches from either
// end while never leaving the [announceBatchSize, announceBatchMax] range.
func adaptAnnounceBatch(current int, rtt time.Duration) int {
	switch {
	case rtt > announceGrowRTT && current < announceBatchMax:
		if current *= 2; current > announceBatchMax {
			current = announceBatchMax
		}
	case rtt < announceShrinkRTT && current > announceBatchSize:
		if current /= 2; current < announceBatchSize {
			current = announceBatchSize
		}
	}
	return current
}

// runAnnounceBatcherQuickCheck is runAnnounceBatcher's quick-check
// counterpart, used only when quick-check is enabled -- the default path
// (runAnnounceBatcher plus startDirClient's ordinary fan-out) is left
// completely untouched. It batches records the same way, but sets
// FlagQuickCheck on every OpAnnounce and turns the exchange synchronous:
// after sending a batch it blocks on rawConn for the matching OpAnnounceAck
// before releasing any of that batch's entries downstream. A file the ack's
// skip-list names is marked done via dt.markFileSkippedDone WITHOUT ever
// reaching treeFileChan (see fileTable.announce for why that is safe: the
// destination has already finalized it); every other file is looked up by
// FileID via dt.sess.FileByID (populated by the fan-out goroutine's
// sess.AddFile call, which happens-before the corresponding AnnounceRec is
// sent on records) and handed to forward -- which the caller wires either
// as a live treeFileChan send (the normal quick-check pipeline) or as a
// spill.Append(fe.ID) onto a deferred to-transfer list (delete-before mode,
// where nothing may reach TreeReader until the destination's delete pass
// has completed; see startDirClient). forward can fail (a spill write is
// disk I/O), which is why it returns an error -- treated exactly like any
// other flush failure, draining the rest of records via batchRecords rather
// than leaving the fan-out goroutine blocked. Closing treeFileChan is
// therefore the CALLER's responsibility now, tied to whichever forward it
// chose.
func runAnnounceBatcherQuickCheck(ctrl *ctrlConn, rawConn io.Reader, dt *dirTransferState, records <-chan AnnounceRec, forward func(*FileEntry) error) error {
	batch := make([]AnnounceRec, 0, announceBatchMax)
	batchTarget := announceBatchSize

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		trailer, err := encodeAnnounceBatch(batch)
		if err != nil {
			return fmt.Errorf("encode announce batch: %w", err)
		}
		sentAt := time.Now()
		if err := ctrl.send(&Msg{Op: uint8(OpAnnounce), Flags: FlagQuickCheck, DataSize: uint32(len(trailer))}, trailer); err != nil {
			return fmt.Errorf("send announce batch: %w", err)
		}
		ack, err := readMsg(rawConn)
		if err != nil {
			return fmt.Errorf("read announce ack: %w", err)
		}
		// Adapt to the link before doing anything with the reply (see
		// announceBatchMax): this measures send-to-ack, which on a slow link
		// is dominated by the round trip and on loopback is dominated by the
		// server's own stat work -- exactly the quantity worth batching
		// against in each case.
		batchTarget = adaptAnnounceBatch(batchTarget, time.Since(sentAt))
		switch Op(ack.Op) {
		case OpAnnounceAck:
		case OpError:
			return fmt.Errorf("server reported an error announcing a batch")
		default:
			return fmt.Errorf("unexpected reply to announce: op %d", ack.Op)
		}
		ackTrailer := make([]byte, ack.DataSize)
		if _, err := io.ReadFull(rawConn, ackTrailer); err != nil {
			return fmt.Errorf("read announce ack trailer: %w", err)
		}
		skipped, err := decodeSkipList(ackTrailer)
		if err != nil {
			return fmt.Errorf("decode announce ack: %w", err)
		}
		skipSet := make(map[uint32]struct{}, len(skipped))
		for _, id := range skipped {
			skipSet[id] = struct{}{}
		}
		for _, rec := range batch {
			if _, isSkipped := skipSet[rec.FileID]; isSkipped {
				dt.markFileSkippedDone(rec.FileID) // already releases via claimDone
				continue
			}
			fe := dt.sess.FileByID(rec.FileID)
			if fe == nil {
				return fmt.Errorf("announce ack: file %d not found in session", rec.FileID)
			}
			if err := forward(fe); err != nil {
				return fmt.Errorf("forward file %d: %w", rec.FileID, err)
			}
			// A dir/symlink/special/skipped-placeholder is finalized by the
			// destination at announce time (see fileTable.announce) and
			// never receives an OpFileDone of its own -- forward just
			// handed it to treeFileChan/qcSurvivors, where TreeReader
			// silently discards non-KindFile entries, so nothing will ever
			// look this one up by FileID again. Released HERE, not in the
			// fan-out loop above, because this is the first point after
			// that loop where it's guaranteed nothing still needs the
			// lookup this function just performed. An empty (Size==0)
			// KindFile is deliberately exempt: it still needs the
			// dedicated empty-file loop in startDirClient to find it and
			// send its synthetic OpFileDone.
			if fe.Kind != KindFile {
				dt.sess.Release(fe.ID)
			}
		}
		batch = batch[:0]
		return nil
	}

	return batchRecords(records, &batch, func() int { return batchTarget }, flush)
}

// dirTransferState is the shared, read-mostly state every transfer worker
// needs: the session, the control connection (for per-file OpFileDone), the
// per-file remaining-block counters, and transfer options.
type dirTransferState struct {
	sess       *SyncSession
	ctrl       *ctrlConn
	magicBytes [magicLen]byte
	noCompress bool
	// The per-file remaining-block counter and the exactly-one-completion
	// guard that used to live here as two separate, never-pruned sync.Maps
	// (remaining, doneOnce) now live on SyncSession's live table itself
	// (see liveFile, claimDone): they are created with AddFile and
	// destroyed with Release/claimDone, so they vanish the instant the
	// file does instead of accumulating a node per file for the life of
	// the run.
}

// sendFileDone marks fileID done in the session and sends OpFileDone,
// exactly once, regardless of which caller (a block reaching zero
// remaining, or forceFileDone below) triggers it. claimDone is the guard:
// of any number of racing callers, exactly one sees ok==true and gets to
// finalize.
func (dt *dirTransferState) sendFileDone(fileID uint32) {
	if _, ok := dt.sess.claimDone(fileID); !ok {
		return
	}
	dt.sess.MarkFileDone()
	if err := dt.ctrl.send(&Msg{Op: uint8(OpFileDone), FileID: fileID}, nil); err != nil {
		Log("file %d: failed to send file-done: %v\n", fileID, err)
	}
}

// markFileSkippedDone marks fileID done for a file the destination's
// quick-check already reported as unchanged (see fileTable.announce and
// runAnnounceBatcherQuickCheck). Unlike sendFileDone, it does NOT send an
// OpFileDone frame: the destination already finalized the file for itself
// at announce time and has nothing further to wait for, so sending one
// would be redundant at best. It still funnels through the same claimDone
// guard as sendFileDone/forceFileDone, so a file can never be double-counted
// regardless of which path reaches it first, and it credits the file's full
// size to bytesScanned so the progress line's scanned/total ratio still
// reaches 100% for a skipped file exactly as if its blocks had actually
// been read.
func (dt *dirTransferState) markFileSkippedDone(fileID uint32) {
	fe, ok := dt.sess.claimDone(fileID)
	if !ok {
		return
	}
	dt.sess.MarkFileDone()
	dt.sess.bytesScanned.Add(fe.Size)
}

// recordBlockDone accounts for one block of fileID having been fully
// processed (whether transferred or found already in sync — both count
// toward "this file is done"). Whichever worker's call brings the count to
// zero sends OpFileDone; blocks complete out of order across workers, so
// this is not necessarily the worker that opened the file. A nil liveEntry
// means the file has already completed (claimDone already removed it) --
// nothing left to count, and nothing to do.
func (dt *dirTransferState) recordBlockDone(fileID uint32) {
	lf := dt.sess.liveEntry(fileID)
	if lf == nil {
		return
	}
	if atomic.AddInt64(&lf.remaining, -1) == 0 {
		dt.sendFileDone(fileID)
	}
}

// forceFileDone corrects fileID's remaining-block counter for a file
// TreeReader could not fully read: an open failure emits zero blocks, and a
// mid-file read failure emits fewer than the file's expected total, so
// recordBlockDone's counter would otherwise never reach zero on its own —
// stranding OpFileDone forever and hanging the server's WaitAllFinalized
// for its full timeout, failing the WHOLE tree even though every other file
// completed normally. (A source file being deleted, renamed, or losing
// read permission between the walk and the read is the ordinary case for a
// live source tree, not a contrived one.)
//
// This does NOT send OpFileDone directly — that would be unsafe. Some of
// blocksEmitted's blocks may still be in flight through the transfer
// pipeline (opened, checksummed, or mid-write) when this fires; declaring
// the file done before they actually complete risks the server
// truncating/closing/chmod-ing it out from under an in-progress write.
// Instead, this subtracts exactly the blocks that will now NEVER arrive
// (expectedTotal - blocksEmitted) from the SAME counter recordBlockDone
// uses, through the SAME atomic Add-and-check-zero operation — so whoever
// (a legitimately completing block, or this correction) brings the counter
// to zero is the one that sends OpFileDone, with the same
// exactly-one-caller-observes-zero guarantee already relied on for every
// ordinary block. Every block that WAS emitted still counts down normally
// through its own recordBlockDone call; only the blocks that will never
// exist are cancelled out. A nil liveEntry means the file already
// completed by some other means; nothing left to correct.
func (dt *dirTransferState) forceFileDone(fileID uint32, blocksEmitted uint32) {
	lf := dt.sess.liveEntry(fileID)
	if lf == nil {
		return
	}
	total := int64(lf.fe.Blocks)
	if total == 0 {
		total = 1
	}
	neverComing := total - int64(blocksEmitted)
	if neverComing <= 0 {
		return // every expected block was actually emitted; nothing to correct
	}
	if atomic.AddInt64(&lf.remaining, -neverComing) == 0 {
		dt.sendFileDone(fileID)
	}
}

// formatBytesShort renders n bytes as a short human-readable size (e.g.
// "3.2 GB", "512.4 MB", "870.0 KB", "42 B") for the directory-tree progress
// line. Not shared with visualizer.go's stats line, which deliberately
// always reports a fixed "MB/s" unit for its throughput figure -- this one
// needs an adaptive unit for TOTAL size, which ranges from a few bytes to
// many terabytes across real trees.
func formatBytesShort(n uint64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	switch {
	case n >= tb:
		return fmt.Sprintf("%.2f TB", float64(n)/tb)
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// dirProgressRenderer periodically prints an overall progress/ETA summary
// line for a directory-tree transfer -- directory mode is otherwise
// completely silent from start to finish. Deliberately a single persistent
// TEXT line, not a redesign of visualizer.go's per-block grid: the grid
// needs a fixed total block count up front, which isn't known until the
// walk finishes (dead air on a large tree before the first render), and a
// tree has per-file block-index spaces that don't map onto the grid's flat
// index space anyway. Output goes through Log(), so it automatically gets
// the same quiet/-v routing every other status line in this codebase
// already has (see log.go's Log — -v routes it into the (currently
// unrendered, for directory mode) visual log ring buffer instead of
// stdout, which is why directory mode rejects -v at the CLI layer rather
// than silently swallowing this line there).
type dirProgressRenderer struct {
	sess *SyncSession
	stop chan struct{}
	done chan struct{}

	// lastLen is the width of the line rendered last tick. Every render
	// returns to column 0 with \r and overwrites in place, so a line that
	// is SHORTER than its predecessor leaves that predecessor's tail on
	// screen -- which is where output like "ETA ~79h02mm3m" comes from: a
	// correctly formatted "79h02m" sitting on top of the leftovers of a
	// longer previous line. Padding each render out to the previous width
	// erases them. Deliberately spaces rather than an ANSI erase sequence
	// (\033[K): this line goes through Log(), which is routinely redirected
	// to a file, and escape codes would corrupt that.
	lastLen int
}

// startDirProgressRenderer starts rendering immediately and returns a
// handle whose Stop renders one final line and waits for the goroutine to
// exit. Safe to use even before the walk has produced any entries (renders
// "files 0/0~ ...", matching the design's own "~ while the walk is
// incomplete" notation).
func startDirProgressRenderer(sess *SyncSession) *dirProgressRenderer {
	r := &dirProgressRenderer{sess: sess, stop: make(chan struct{}), done: make(chan struct{})}
	go r.run()
	return r
}

func (r *dirProgressRenderer) Stop() {
	close(r.stop)
	<-r.done
}

// dirProgressSample is one bytesScanned reading at a point in time, used to
// derive a smoothed recent throughput instead of an instantaneous (jittery)
// per-tick rate.
type dirProgressSample struct {
	bytes uint64
	// net is bytesNet at the same instant: the COMPRESSED bytes actually
	// put on the wire, as opposed to bytes (bytesScanned), which counts
	// source bytes read and includes blocks that turned out to be identical
	// and quick-check-skipped files that were never opened at all. The two
	// differ enormously on a real run -- a mostly-unchanged re-sync scans
	// terabytes while sending almost nothing -- so reporting only the first
	// makes it impossible to tell a saturated link from an idle one.
	net uint64
	at  time.Time
}

const dirProgressTick = time.Second
const dirProgressWindow = 5 // ticks averaged for the displayed MB/s and ETA

// resolveProgressTotals picks the file/byte totals the progress display
// should show right now: the session's exact running totals once the walk
// has finished, and max(known, pre-scan estimate) while it's still going —
// the walk is back-pressured by the transfer pipeline, so its own totals
// grow at transfer pace, while estimateTree's converge on the real totals
// within seconds. max (not "estimate until walkDone") because either side
// can legitimately be ahead: the estimate is behind briefly at startup, and
// the exact count pulls ahead if files were created after the pre-scan
// passed their directory. Pure given the session's atomics, so it's
// directly unit-testable like dirProgressLine.
func resolveProgressTotals(sess *SyncSession) (filesTotal uint32, bytesTotal uint64) {
	filesTotal = sess.totalFilesKnown.Load()
	bytesTotal = sess.totalBytesKnown.Load()
	if sess.WalkDone() {
		return filesTotal, bytesTotal
	}
	if est := sess.estFiles.Load(); est > filesTotal {
		filesTotal = est
	}
	if est := sess.estBytes.Load(); est > bytesTotal {
		bytesTotal = est
	}
	return filesTotal, bytesTotal
}

// TransferSummary renders the end-of-run figures that matter for judging a
// transfer: how much was scanned, how much actually went on the wire, and
// what compression bought. The source total alone cannot distinguish a run
// that sent everything from one that sent almost nothing, which is the whole
// question on a re-sync or a bandwidth-limited link.
func (s *SyncSession) TransferSummary() string {
	scanned := formatBytesShort(s.TotalBytes())
	net := s.bytesNet.Load()
	if net == 0 {
		return fmt.Sprintf("%s scanned, nothing sent (destination already matched)", scanned)
	}
	orig := s.bytesOrig.Load()
	if orig > net {
		return fmt.Sprintf("%s scanned, %s sent (×%.2f compression)",
			scanned, formatBytesShort(net), float64(orig)/float64(net))
	}
	return fmt.Sprintf("%s scanned, %s sent", scanned, formatBytesShort(net))
}

// netSegment renders the wire-rate part of the progress line: what is
// actually leaving the machine, and how much compression is buying. Empty
// until something has actually been sent, so an unchanged re-sync (which
// scans everything and sends nothing) doesn't carry a permanent
// "net 0.0 MB/s" that says nothing.
//
// This exists because the headline MB/s is the SCAN rate, and on this
// codebase's own workloads the two can differ by an order of magnitude --
// without the wire figure there is no way to tell "the link is saturated"
// from "the link is idle and something upstream is the bottleneck", which
// is the first question worth asking about any slow transfer.
func netSegment(window []dirProgressSample) string {
	if len(window) < 2 {
		return ""
	}
	oldest, newest := window[0], window[len(window)-1]
	if newest.net <= oldest.net {
		return ""
	}
	elapsed := newest.at.Sub(oldest.at).Seconds()
	if elapsed <= 0 {
		return ""
	}
	netMBps := float64(newest.net-oldest.net) / elapsed / mb1
	// Compression ratio over the same window: scanned bytes are what the
	// source produced, net bytes are what the wire carried.
	if newest.bytes > oldest.bytes {
		if ratio := float64(newest.bytes-oldest.bytes) / float64(newest.net-oldest.net); ratio >= 1.005 {
			return fmt.Sprintf(" · net %.1f MB/s ×%.2f", netMBps, ratio)
		}
	}
	return fmt.Sprintf(" · net %.1f MB/s", netMBps)
}

// dirProgressLine computes the exact progress/ETA summary line text from a
// sample window (oldest to newest, already trimmed to at most
// dirProgressWindow entries) and the session's current totals — fileCount
// is the files-only denominator (KindFile entries; dirs and skipped
// placeholders never complete, so counting them would keep the display
// permanently short of N/N), typically resolveProgressTotals' result. Pure
// and deterministic given its inputs — no clock or global state read
// directly — so it's directly unit-testable without needing to capture
// stdout or fake time.Now(); dirProgressRenderer.run is a thin
// timing/sampling/IO wrapper around it.
func dirProgressLine(window []dirProgressSample, total uint64, filesDone, fileCount uint32, walkDone bool) string {
	mbps := 0.0
	eta := "--"
	if len(window) >= 2 {
		oldest := window[0]
		newest := window[len(window)-1]
		elapsed := newest.at.Sub(oldest.at).Seconds()
		if elapsed > 0 && newest.bytes >= oldest.bytes {
			bytesPerSec := float64(newest.bytes-oldest.bytes) / elapsed
			mbps = bytesPerSec / mb1
			switch {
			case bytesPerSec > 0 && total > newest.bytes:
				eta = formatDuration(time.Duration(float64(total-newest.bytes)/bytesPerSec) * time.Second)
			case total > 0 && newest.bytes >= total:
				eta = "0s"
			}
		}
	}

	walkTilde := ""
	if !walkDone {
		walkTilde = "~"
	}
	scanned := uint64(0)
	if len(window) > 0 {
		scanned = window[len(window)-1].bytes
	}
	return fmt.Sprintf("files %d/%d%s · %s/%s · %.1f MB/s%s · ETA ~%s",
		filesDone, fileCount, walkTilde, formatBytesShort(scanned), formatBytesShort(total),
		mbps, netSegment(window), eta)
}

// padOverPrevious pads line with trailing spaces so it is at least as wide
// as the previous render, and returns it together with the width to remember
// for the next one. Returning the width of the UNPADDED line would let the
// remembered width shrink while stale characters from an even earlier,
// longer line were still on screen.
func padOverPrevious(line string, prev int) (string, int) {
	width := len(line)
	if pad := prev - width; pad > 0 {
		line += strings.Repeat(" ", pad)
		width = prev
	}
	return line, width
}

func (r *dirProgressRenderer) run() {
	defer close(r.done)
	ticker := time.NewTicker(dirProgressTick)
	defer ticker.Stop()

	var window []dirProgressSample
	render := func() {
		window = append(window, dirProgressSample{
			bytes: r.sess.bytesScanned.Load(),
			net:   r.sess.bytesNet.Load(),
			at:    time.Now(),
		})
		if len(window) > dirProgressWindow {
			window = window[len(window)-dirProgressWindow:]
		}
		filesTotal, bytesTotal := resolveProgressTotals(r.sess)
		line := dirProgressLine(window, bytesTotal, r.sess.filesDone.Load(), filesTotal, r.sess.WalkDone())
		// line is passed as an argument, never as the format string itself:
		// it's runtime-computed text (a formatted byte size could in
		// principle contain a stray '%' in some future unit), and Log's
		// first parameter is a Printf-style format string.
		line, r.lastLen = padOverPrevious(line, r.lastLen)
		Log("%s\r", line)
	}

	for {
		select {
		case <-ticker.C:
			render()
		case <-r.stop:
			render()
			Log("\n") // leave the final in-place-updated line intact
			return
		}
	}
}

// maxSkipLinesPerReason caps how many individual "skip" lines the walk
// prints for any one reason before it goes quiet about that reason. A backup
// tree routinely contains a container's whole /dev -- hundreds or thousands
// of device nodes, none of them syncable -- and one line each buries every
// other message in the transfer. The count is still reported in full by
// skipLogger.summary, so nothing is hidden, just not repeated.
const maxSkipLinesPerReason = 10

// skipLogger bounds the walk's per-path skip logging and replaces the tail
// with a single aggregated summary. Entries are still ANNOUNCED exactly as
// before (as KindSkipped placeholders, keeping them protected from
// -delete) -- this changes only what reaches the log.
type skipLogger struct {
	mu     sync.Mutex
	shown  map[string]int
	counts map[string]int
	order  []string // reasons in first-seen order, so the summary is stable
	total  int
}

func newSkipLogger() *skipLogger {
	return &skipLogger{shown: map[string]int{}, counts: map[string]int{}}
}

// skipReasonKey collapses a formatted reason to the part before its first
// colon ("unsupported file type: Dc---------" -> "unsupported file type"),
// so every device node in a /dev tree shares one bucket instead of each
// distinct mode string getting its own allowance.
func skipReasonKey(reason string) string {
	if i := strings.IndexByte(reason, ':'); i >= 0 {
		return reason[:i]
	}
	return reason
}

func (s *skipLogger) log(relPath, reason string) {
	key := skipReasonKey(reason)

	s.mu.Lock()
	if _, seen := s.counts[key]; !seen {
		s.order = append(s.order, key)
	}
	s.counts[key]++
	s.total++
	shown := s.shown[key]
	if shown < maxSkipLinesPerReason {
		s.shown[key] = shown + 1
	}
	s.mu.Unlock()

	// Log outside the lock: it can block on a slow terminal, and the walk
	// is the only caller that would then be stalled behind it.
	if shown < maxSkipLinesPerReason {
		Log("skip %s: %s\n", relPath, reason)
		if shown+1 == maxSkipLinesPerReason {
			Log("skip: further %q skips will be counted, not listed\n", key)
		}
	}
}

// summary logs one aggregated line for everything the walk skipped, or
// nothing at all if it skipped nothing.
func (s *skipLogger) summary() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total == 0 {
		return
	}
	parts := make([]string, 0, len(s.order))
	for _, key := range s.order {
		parts = append(parts, fmt.Sprintf("%d %s", s.counts[key], key))
	}
	Log("skipped %d path(s): %s\n", s.total, strings.Join(parts, ", "))
}

// startDirClient walks srcRoot, announces it to serverAddress, and
// transfers every differing block over workers parallel connections. excl
// is a list of glob patterns (matched by walkSource/excluded against each
// entry's relative path) to skip entirely; nil/empty excludes nothing.
// deleteExtra requests a `-delete` pass: once the tree is fully announced
// -- and BEFORE any file content is transferred -- the destination removes
// anything not among this run's announced entries (an
// excluded/symlinked/special-file path is still announced, as KindSkipped,
// so it and anything under it is protected from this, not swept away just
// because it wasn't transferred). syncLinks (-links) recreates source
// symlinks at the destination with identical target strings instead of
// skipping them (see walkSource); syncSpecials (-specials) does the same for
// FIFOs and unix sockets, recreating them via mkfifo/mknod. The returned
// SyncSession's
// FailedCount/FailedError report per-file failures that did not abort the
// transfer; the returned error is non-nil only for a failure of the
// transfer as a whole (the walk itself, or the control connection).
func startDirClient(srcRoot, serverAddress string, blockSizeCap uint32, noCompress bool, workers int, excl []string, quickCheck, deleteExtra, syncLinks, syncSpecials bool) (*SyncSession, error) {
	if workers < 1 {
		return nil, fmt.Errorf("worker count must be at least 1")
	}
	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		return nil, fmt.Errorf("resolving: %w", err)
	}

	// Control connection: opened first, held for the whole transfer. Unlike
	// a worker connection, losing it is fatal in this phase — it carries
	// announce/file-done/tree-done, none of which has a safe way to resume
	// from scratch the way a block-request retry does.
	rawConn, err := net.DialTimeout("tcp", saddr.String(), dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connecting control channel: %w", err)
	}
	if tc, ok := rawConn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(keepAlivePeriod)
	}
	defer rawConn.Close()
	ctrl := newCtrlConn(rawConn)

	sess := NewSyncSession()
	dt := &dirTransferState{sess: sess, ctrl: ctrl, magicBytes: ctrl.magicBytes, noCompress: noCompress}

	capBS := blockSizeCap
	if capBS == 0 {
		capBS = blockSize
	}

	fileChan := make(chan *FileEntry, 256)
	treeFileChan := make(chan *FileEntry, 256)
	announceChan := make(chan AnnounceRec, 256)

	var walkErr error
	walkDone := make(chan struct{})
	skips := newSkipLogger()
	go func() {
		defer close(walkDone)
		walkErr = walkSource(srcRoot, excl, capBS, syncLinks, syncSpecials, fileChan, skips.log)
		skips.summary()
	}()

	// Fast metadata-only pre-scan feeding the progress display's totals
	// (see estimateTree/resolveProgressTotals): the real walk above is
	// back-pressured by the transfer pipeline, so without this the
	// files/bytes denominators -- and the ETA derived from them -- would
	// track the transfer instead of the tree. Fire-and-forget: it touches
	// nothing but two session atomics (no channels, so no ordering
	// constraint against the pipeline below), and nothing ever waits on it
	// -- if the transfer finishes first, the leftover goroutine just
	// finishes its lstat sweep and exits.
	go estimateTree(srcRoot, excl, func(size uint64) {
		sess.estFiles.Add(1)
		sess.estBytes.Add(size)
	})

	// Single consumer of the walk: registering each entry with the session
	// IS what assigns it there (walkSource's IDs are dense in emission
	// order, matching AddFile's contract), then fans it out to the tree
	// reader and the announce queue. Two modes suppress the live
	// treeFileChan feed here: with quick-check enabled it's fed by
	// runAnnounceBatcherQuickCheck's forward instead -- only AFTER the
	// destination has confirmed a given file actually needs transfer --
	// and with -delete enabled NOTHING feeds it until the destination's
	// delete pass has completed (see the feeder goroutine below), since
	// delete-before means no file content moves until stale data is gone.
	// The announce queue itself is fed and closed here in every mode.
	feedLive := !quickCheck && !deleteExtra
	var fanOutErr error
	go func() {
		defer close(announceChan)
		if feedLive {
			defer close(treeFileChan)
		}
		for fe := range fileChan {
			if _, err := sess.AddFile(fe); err != nil {
				// A bug in walkSource's ID assignment, not anything a
				// remote peer or malformed input could trigger -- still
				// handled as a controlled failure of this transfer (checked
				// below, once announceChan's resulting close unblocks
				// announceErrCh) rather than a panic that would take down
				// the whole process.
				fanOutErr = err
				return
			}
			if fe.Kind == KindSkipped {
				sess.MarkFileSkipped()
			}
			if feedLive {
				treeFileChan <- fe
			}
			announceChan <- AnnounceRec{
				FileID: fe.ID, FileSize: fe.Size, Mode: uint32(fe.Mode),
				MTimeSec: fe.ModTime.Unix(), MTimeNs: int32(fe.ModTime.Nanosecond()),
				Kind: fe.Kind, Path: fe.RelPath, LinkTarget: fe.LinkTarget,
			}
			// A dir/symlink/special/skipped-placeholder is finalized by the
			// destination at announce time and never receives an
			// OpFileDone of its own (see fileTable.announce) -- nothing
			// downstream will ever look it up by FileID again, so release
			// it now rather than let it sit in the live table for the rest
			// of the run. NOT done here when quickCheck is set: the
			// quick-check batcher (runAnnounceBatcherQuickCheck) still
			// needs to look every one of these up by FileID once its
			// announce batch is acked, so it releases them itself, one
			// step later, instead (see its flush). An empty (Size==0)
			// KindFile is exempt from BOTH release points: it still needs
			// its own synthetic OpFileDone sent once every announce is
			// confirmed flushed (see the empty-file loop below), which
			// needs to find it here.
			if fe.Kind != KindFile && !quickCheck {
				sess.Release(fe.ID)
			}
		}
	}()

	// spill holds the deferred to-transfer list in delete-before mode: no
	// file content may reach TreeReader until the destination confirms its
	// delete pass completed (see below), so on a large tree the FULL
	// survivor list is known before a single byte moves. Spilled to disk
	// (idSpill) rather than an in-memory []*FileEntry -- the FileEntry
	// values themselves stay alive via SyncSession.live regardless (a real
	// file isn't released until its own transfer completes), so the saving
	// here is the list of pointers itself, not the entries they point to.
	// Created only when actually needed; nil otherwise, and Close is nil-
	// receiver-safe. Ownership: if the feeder goroutine below is ever
	// spawned, IT closes the spill (in its own defer, once it has drained
	// every id); if something fails before that (walk/announce/delete-pass
	// error), the treeFeederSpawned-gated defer further down closes it
	// instead -- the same pattern already used for treeFileChan's
	// ownership, so the two can never both think they own it.
	var spill *idSpill
	if deleteExtra {
		var spillErr error
		spill, spillErr = newIDSpill("")
		if spillErr != nil {
			return sess, fmt.Errorf("create deferred transfer list: %w", spillErr)
		}
	}

	announceErrCh := make(chan error, 1)
	switch {
	case quickCheck && deleteExtra:
		go func() {
			announceErrCh <- runAnnounceBatcherQuickCheck(ctrl, rawConn, dt, announceChan,
				func(fe *FileEntry) error { return spill.Append(fe.ID) })
		}()
	case quickCheck:
		go func() {
			err := runAnnounceBatcherQuickCheck(ctrl, rawConn, dt, announceChan,
				func(fe *FileEntry) error { treeFileChan <- fe; return nil })
			// Sole writer of treeFileChan in this mode, so also its closer
			// -- on the clean AND the error path, or TreeReader (and the
			// whole worker chain behind it) would wait forever.
			close(treeFileChan)
			announceErrCh <- err
		}()
	default:
		go func() { announceErrCh <- runAnnounceBatcher(ctrl, announceChan) }()
	}

	// The tree reader, precompute pool, and transfer workers must start
	// consuming treeFileChan/precompChan CONCURRENTLY with the walk, not
	// after it finishes: treeFileChan is only 256-deep, and the fan-out
	// goroutine above writes to it synchronously for every entry (files
	// AND dirs). On any tree past that threshold, the fan-out goroutine
	// blocks on `treeFileChan <- fe`, which stalls announceChan too (same
	// goroutine), which stalls the announce batcher, which stalls
	// walkSource's own output channel, which stalls walkDone below —
	// deadlocking the whole client forever with no timeout. Starting the
	// downstream pipeline first, before the walkDone/announceErrCh waits,
	// is what makes this a real pipeline instead of "walk fully, then
	// read" — which is also what the design's "announce while still
	// walking" intent always required.
	tr := NewTreeReader(srcRoot, treeFileChan, sess, int64(workers)*4*int64(capBS))
	tr.onFileFailed = dt.forceFileDone
	tr.Start()
	// One shared FCFS channel -- for files with more blocks than workers,
	// cooperatively drained by every worker exactly as before affinity
	// existed -- plus one dedicated channel per worker, for files small
	// enough (see affinityWorker) that streaming all of their blocks
	// through a single already-bound connection beats letting them race
	// across several. Constructed here and consumed by workers started on
	// the very next statement, with nothing blocking in between -- the
	// same A1 invariant as treeFileChan/announceChan above: never let a
	// producer start filling a channel before its consumer goroutine
	// exists.
	// workers*8, not workers*2: this is the FCFS pool that "files with more
	// blocks than workers" drain from (see the comment above), and its
	// small depth was part of why the pipeline's real in-flight ceiling sat
	// far below what byteBudget was sized to allow (see
	// treeReaderOutDepth). Unlike dirDedicatedChanDepth below, widening
	// this queue doesn't single out one file ahead of its walk-order
	// neighbours -- any free worker drains whichever block is next -- so it
	// carries none of that comment's write-locality caveat.
	sharedChan := make(chan PrecomputedBlock, workers*8)
	dedChans := make([]chan PrecomputedBlock, workers)
	sendDed := make([]chan<- PrecomputedBlock, workers)
	for i := range dedChans {
		dedChans[i] = make(chan PrecomputedBlock, dirDedicatedChanDepth)
		sendDed[i] = dedChans[i]
	}
	precomputeTreeBlocksRouted(tr, sess, sharedChan, sendDed, workers, noCompress)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runDirTransferWorker(saddr, dt, dedChans[i], sharedChan)
		}()
	}
	// Declared BEFORE the wg.Wait() defer below so it runs AFTER it (defers
	// are LIFO): the progress line keeps updating while workers are still
	// active and only renders its final state, then stops, once they've
	// all actually finished (or the function is returning early for any
	// other reason -- Stop still renders once and exits cleanly either
	// way).
	progress := startDirProgressRenderer(sess)
	defer progress.Stop()
	// Guarantees the reader/precompute/worker chain always drains via the
	// natural channel-close cascade (walkSource closes its output on any
	// return, error or not) on EVERY exit path below, including the early
	// returns on a walk/announce/send error -- without this, an early
	// return here would leak the goroutines still blocked on a channel
	// that will now never be read from again.
	defer wg.Wait()
	// Delete-before mode only: until the feeder goroutine below exists,
	// treeFileChan has NO owner that would ever close it, so an early
	// return (walk error, announce error, delete-pass refusal) would leave
	// TreeReader -- and the whole chain the wg.Wait() defer above waits on
	// -- blocked forever. Declared AFTER that defer so it runs BEFORE it
	// (LIFO); once the feeder is spawned, close-ownership is its (via its
	// own defer), and this becomes a no-op.
	treeFeederSpawned := false
	defer func() {
		if deleteExtra && !treeFeederSpawned {
			close(treeFileChan)
			// The feeder that would otherwise own spill's cleanup was
			// never spawned -- nothing else will ever close it.
			spill.Close()
		}
	}()

	<-walkDone
	if walkErr != nil {
		return sess, fmt.Errorf("walking %s: %w", srcRoot, walkErr)
	}
	sess.MarkWalkDone()

	if err := <-announceErrCh; err != nil {
		return sess, fmt.Errorf("announcing tree: %w", err)
	}
	// Safe to read here without synchronization beyond what already
	// happened above: announceChan only closes (unblocking announceErrCh)
	// AFTER the fan-out goroutine's assignment to fanOutErr, on the same
	// goroutine, in program order -- so this read happens-after that write.
	if fanOutErr != nil {
		return sess, fmt.Errorf("registering walked entry: %w", fanOutErr)
	}

	fileCount := uint32(sess.FileCount())
	var announceEndFlags uint8
	if deleteExtra {
		announceEndFlags |= FlagDelete
	}
	if err := ctrl.send(&Msg{Op: uint8(OpAnnounceEnd), Flags: announceEndFlags, BlockIdx: fileCount, FileSize: sess.TotalBytes()}, nil); err != nil {
		return sess, fmt.Errorf("send announce end: %w", err)
	}

	if deleteExtra {
		// Delete-before: block here until the destination confirms its
		// delete pass completed (or refused/failed -- OpError), and only
		// then release the tree to the reader. Safe to read rawConn
		// synchronously: the quick-check batcher, the only other reader,
		// has already finished (announceErrCh above), and the next read
		// after this one is the tree ack at the very end.
		ack, err := readMsg(rawConn)
		if err != nil {
			return sess, fmt.Errorf("waiting for delete-pass ack: %w", err)
		}
		switch Op(ack.Op) {
		case OpAnnounceEndAck:
		case OpError:
			return sess, fmt.Errorf("server reported an error running the delete pass")
		default:
			return sess, fmt.Errorf("unexpected reply to announce end: op %d", ack.Op)
		}
		if ack.BlockIdx > 0 {
			Log("delete pass removed %d extraneous destination entr(y/ies)\n", ack.BlockIdx)
		}

		// The destination is clean; NOW feed the reader. spill already holds
		// the quick-check survivor list when quick-check ran (populated by
		// runAnnounceBatcherQuickCheck's forward above); otherwise every
		// live KindFile entry with content is spilled here instead of
		// collected into an in-memory slice (TreeReader would skip a
		// non-KindFile or Size==0 entry anyway, so there is no reason to
		// spill one). Either way the result streams back in ID order,
		// preserving the walk-order assumptions TreeReader and the
		// destination's OrderedWriter both rely on.
		if !quickCheck {
			for id := uint32(0); id < fileCount; id++ {
				fe := sess.FileByID(id)
				if fe == nil || fe.Kind != KindFile || fe.Size == 0 {
					continue
				}
				if err := spill.Append(id); err != nil {
					return sess, fmt.Errorf("spill deferred transfer list: %w", err)
				}
			}
		}
		spillReader, err := spill.Reader()
		if err != nil {
			return sess, fmt.Errorf("rewind deferred transfer list: %w", err)
		}
		treeFeederSpawned = true
		go func() {
			defer spill.Close()
			defer close(treeFileChan)
			for {
				id, ok, err := spillReader.Next()
				if err != nil {
					Log("deferred transfer list: %v\n", err)
					return
				}
				if !ok {
					return
				}
				fe := sess.FileByID(id)
				if fe == nil {
					// Already released (shouldn't happen -- a spilled
					// KindFile entry is only ever released by its own
					// eventual transfer completion, which cannot have
					// happened yet since TreeReader hasn't seen it). Skip
					// rather than fail the whole tree over one entry.
					continue
				}
				treeFileChan <- fe
			}
		}()
	}

	// Every announce is now confirmed sent, so it's safe to immediately
	// finish any zero-byte file: it produces no blocks (TreeReader skips
	// Size==0 entries entirely), so recordBlockDone's block-counting path
	// never reaches it. This is also the FIRST safe point to release such
	// an entry from the live table -- both release points above (the
	// fan-out loop, and runAnnounceBatcherQuickCheck's flush) deliberately
	// leave a Size==0 KindFile alone specifically so it is still here to
	// be found.
	for id := uint32(0); id < fileCount; id++ {
		fe := sess.FileByID(id)
		if fe != nil && fe.Kind == KindFile && fe.Size == 0 {
			sess.MarkFileDone()
			if err := ctrl.send(&Msg{Op: uint8(OpFileDone), FileID: fe.ID}, nil); err != nil {
				return sess, fmt.Errorf("send file-done for empty file %d: %w", fe.ID, err)
			}
			sess.Release(fe.ID)
		}
	}

	wg.Wait()

	if err := ctrl.send(&Msg{Op: uint8(OpTreeDone), BlockIdx: fileCount, FileSize: sess.TotalBytes()}, nil); err != nil {
		return sess, fmt.Errorf("send tree done: %w", err)
	}
	ack, err := readMsg(rawConn)
	if err != nil {
		return sess, fmt.Errorf("waiting for tree ack: %w", err)
	}
	switch Op(ack.Op) {
	case OpTreeAck:
	case OpError:
		return sess, fmt.Errorf("server reported an error finishing the tree")
	default:
		return sess, fmt.Errorf("unexpected reply to tree done: op %d", ack.Op)
	}

	return sess, nil
}

// blockOutcome reports what processDirBlock actually did, for progress
// accounting.
type blockOutcome struct {
	changed   bool
	netBytes  uint32
	origBytes uint32
}

// dirWorkerBinding is one transfer worker's soft file binding: which
// FileID its connection currently holds a server-side ref for, and on
// which connection generation. Extracted from runDirTransferWorker's
// former loop locals (bound/boundFileID/boundGen) so its per-block logic
// can be shared between a single-source and (later) dual-source receive
// loop without duplicating it; the binding rules themselves are unchanged
// (see dirWorkerBind).
type dirWorkerBinding struct {
	bound  bool
	fileID uint32
	gen    uint64
	// created mirrors the FlagCreated bit from the OpFileAck of the CURRENT
	// binding: the destination created this file fresh (empty, truncated
	// straight to full size -- guaranteed all-zeros), so processDirBlock
	// takes its fast path: no checksum round trip for a data block, no wire
	// op at all for a zero block. Refreshed on every (re)bind, which is what
	// keeps it safe across redials: a re-open of the same file reports
	// created=false (the file exists now), and the fast path self-disables
	// back to the full checksum exchange.
	created bool
}

// handleDirBlock processes exactly one block on conn, using and updating
// b to track this worker's current file binding. This is
// runDirTransferWorker's entire former per-iteration loop body, moved out
// verbatim so it can be called from more than one receive loop shape.
//
// Two invariants every caller depends on and must never break: every block
// passed in results in exactly one dt.recordBlockDone(pb.FileID) call
// (succeeded, permanently failed, or skipped because its file is already
// doomed), and exactly one pb.Release() call.
func (dt *dirTransferState) handleDirBlock(conn *AutoReconnectTCP, pb PrecomputedBlock, b *dirWorkerBinding) {
	if dt.sess.isFailed(pb.FileID) {
		// A prior block of this file already exhausted its retries.
		// This block is skipped, not attempted, but it must still be
		// accounted for: recordBlockDone's counter tracks blocks
		// resolved (attempted-and-settled OR skipped-because-doomed),
		// not blocks that succeeded, or a permanently failing file
		// would never reach zero remaining, OpFileDone would never be
		// sent for it, and the server would wait forever for it to
		// finalize in WaitAllFinalized -- one bad block would hang the
		// entire tree, not just its own file.
		dt.recordBlockDone(pb.FileID)
		pb.Release()
		return
	}

	var outcome blockOutcome
	lastErr := withRetry(
		func(retry int) error {
			if err := conn.Dial(); err != nil {
				return err
			}
			needRebind := !b.bound || b.fileID != pb.FileID || b.gen != conn.Gen()
			if needRebind {
				sameConnDifferentFile := b.bound && b.gen == conn.Gen() && b.fileID != pb.FileID
				created, err := dirWorkerBind(conn, dt, pb, sameConnDifferentFile, b.fileID)
				if err != nil {
					b.bound = false
					return err
				}
				b.bound = true
				b.fileID = pb.FileID
				b.gen = conn.Gen()
				b.created = created
			}
			var err error
			outcome, err = processDirBlock(conn, dt, pb, b.created)
			if err != nil {
				b.bound = false
			}
			return err
		},
		func(retry int, lastErr error) {
			Log("file %d block %d: retry %d/%d after: %v\n", pb.FileID, pb.BlockIdx, retry, maxRetries-1, lastErr)
			conn.Close()
			b.bound = false
		},
	)

	if lastErr != nil {
		if errors.Is(lastErr, errPermanent) {
			// A deterministic rejection resolved on the FIRST attempt --
			// withRetry never ran onRetry, so this connection was never
			// closed, yet the server killed its own side when it sent the
			// rejection (and its unread error-text payload is still sitting
			// in the receive buffer). Close it here so the next block starts
			// on a clean redial instead of tripping over a dead, dirty
			// connection and burning a retry to discover that.
			Log("file %d block %d: %v\n", pb.FileID, pb.BlockIdx, lastErr)
			conn.Close()
			b.bound = false
		} else {
			Log("file %d block %d: failed after %d retries: %v\n", pb.FileID, pb.BlockIdx, maxRetries, lastErr)
		}
		dt.sess.failFile(pb.FileID, fmt.Errorf("block %d: %w", pb.BlockIdx, lastErr))
	} else {
		blockLen := uint64(blockPayloadSize(pb.FileSize, pb.BlockSize, pb.BlockIdx))
		dt.sess.AddBlockProgress(outcome.changed, blockLen, uint64(outcome.netBytes), uint64(outcome.origBytes))
	}
	// Every block handled here, succeeded or not, counts toward the file
	// being "resolved" -- see the comment above for why a failure must
	// still be counted.
	dt.recordBlockDone(pb.FileID)
	pb.Release()
}

// runDirTransferWorker pulls blocks from two sources and sends them via
// handleDirBlock, maintaining its own connection and soft file-binding
// across calls: dedChan carries every block of the files routed
// exclusively to THIS worker (see affinityWorker), so a small/medium file
// is always streamed through one already-bound connection instead of
// racing across several; sharedChan carries files with more blocks than
// there are workers, cooperatively drained by every worker exactly as
// before affinity existed. Either may be nil, which in a select simply
// means that case is never ready -- a nil dedChan reduces this to the
// pre-affinity single-source behavior.
func runDirTransferWorker(saddr *net.TCPAddr, dt *dirTransferState, dedChan <-chan PrecomputedBlock, sharedChan <-chan PrecomputedBlock) {
	conn := NewAutoReconnectTCP(saddr)
	defer conn.Close()

	var b dirWorkerBinding

	// Local copies, nil'd out once each channel is closed AND drained (Go
	// delivers every buffered value before ok reports false, so nil-ing
	// only on !ok never drops a final buffered block). A nil channel is
	// never ready in a select, which is exactly "stop considering this
	// source" -- so the loop below terminates iff BOTH sources are closed
	// and drained, and can never busy-spin re-selecting an already-closed
	// (permanently ready) channel.
	ded, shared := dedChan, sharedChan

	for ded != nil || shared != nil {
		var pb PrecomputedBlock

		// Priority receive: drain this worker's own routed file first.
		// Without this, select's uniform-random choice among ready cases
		// would let a shared-channel block land between two consecutive
		// blocks of a dedicated file, forcing an extra dirWorkerBind
		// rebind on the switch back -- defeating the entire point of
		// routing. The non-blocking probe with a default case cannot
		// busy-spin: when ded is nil or has nothing ready, it falls
		// straight through to the blocking select below, which parks
		// until either source has something.
		select {
		case v, ok := <-ded:
			if !ok {
				ded = nil
				continue
			}
			pb = v
		default:
			select {
			case v, ok := <-ded:
				if !ok {
					ded = nil
					continue
				}
				pb = v
			case v, ok := <-shared:
				if !ok {
					shared = nil
					continue
				}
				pb = v
			}
		}

		dt.handleDirBlock(conn, pb, &b)
	}
}

// dirWorkerBind (re)binds conn to pb.FileID: if the same physical
// connection genuinely held a ref for a DIFFERENT file (sameConnDifferentFile),
// it releases that ref first via OpFileClose; otherwise (first bind, or a
// redial happened) there is no ref on this connection to close — the old
// one, if any, is on a now-dead connection and becomes the server's stale
// ref to reap, not this connection's problem. Then sends OpFileOpen and
// waits for the ack. The returned created reports the ack's FlagCreated
// bit: the destination created this file fresh during this very open (see
// dirWorkerBinding.created for what the caller does with it).
func dirWorkerBind(conn *AutoReconnectTCP, dt *dirTransferState, pb PrecomputedBlock, sameConnDifferentFile bool, prevFileID uint32) (created bool, err error) {
	if sameConnDifferentFile {
		closeMsg := &Msg{MagicHead: dt.magicBytes, Op: uint8(OpFileClose), FileID: prevFileID}
		data, err := pack(closeMsg)
		if err == nil {
			connWrite(conn, data) // best-effort: a failure here just means an extra stale ref for the server to reap
		}
	}

	openMsg := &Msg{MagicHead: dt.magicBytes, Op: uint8(OpFileOpen), FileID: pb.FileID, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
	data, err := pack(openMsg)
	if err != nil {
		return false, err
	}
	if err := connWrite(conn, data); err != nil {
		return false, fmt.Errorf("send file-open: %w", err)
	}
	ack, err := readMsg(conn)
	if err != nil {
		return false, fmt.Errorf("read file-open ack: %w", err)
	}
	if Op(ack.Op) == OpError {
		// The server distinguishes deterministic rejections (file already
		// finalized -- including one that failed at announce time) from
		// possibly-transient ones with FlagErrPermanent. Only the former
		// short-circuits withRetry: a plain rejection is most often "file
		// was never announced", the ordinary race of a worker's open
		// beating a still-buffering announce batch, which the retry loop
		// has always absorbed and must keep absorbing.
		if ack.Flags&FlagErrPermanent != 0 {
			return false, fmt.Errorf("server rejected file-open for file %d: %w", pb.FileID, errPermanent)
		}
		return false, fmt.Errorf("server rejected file-open for file %d", pb.FileID)
	}
	if Op(ack.Op) != OpFileAck || ack.FileID != pb.FileID {
		return false, fmt.Errorf("unexpected reply to file-open for file %d: op %d", pb.FileID, ack.Op)
	}
	return ack.Flags&FlagCreated != 0, nil
}

// processDirBlock runs the checksum-request/compare/write exchange for one
// block on a connection already bound to its file, mirroring
// processPrecomputedBlock's single-file logic.
//
// created short-circuits that exchange for a file the destination created
// fresh during this binding's own OpFileOpen (FlagCreated on the ack):
// Acquire truncates a created file straight to its announced size, so its
// entire content is guaranteed zeros. A zero block therefore needs no wire
// op at all -- the destination hole already reads as zeros, and finalize's
// own truncate keeps it that way -- and a data block always differs, so
// the checksum round trip (whose hash could only ever come back as the
// zero hash) is skipped and the data sent directly. The server accepts
// these check-less writes only for created-fresh files (see
// takePendingOrFresh); everything else keeps the full exchange.
func processDirBlock(conn *AutoReconnectTCP, dt *dirTransferState, pb PrecomputedBlock, created bool) (blockOutcome, error) {
	magicBytes := dt.magicBytes
	expected := blockPayloadSize(pb.FileSize, pb.BlockSize, pb.BlockIdx)

	if created && pb.IsZero {
		return blockOutcome{changed: false}, nil
	}

	if !created {
		checkMsg := &Msg{MagicHead: magicBytes, Op: uint8(OpBlockCheck), FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
		data, err := pack(checkMsg)
		if err != nil {
			return blockOutcome{}, err
		}
		if err := connWrite(conn, data); err != nil {
			return blockOutcome{}, fmt.Errorf("send check: %w", err)
		}

		hashHdr, err := readMsg(conn)
		if err != nil {
			return blockOutcome{}, fmt.Errorf("read hash header: %w", err)
		}
		if Op(hashHdr.Op) == OpError {
			return blockOutcome{}, fmt.Errorf("server error on checksum request for file %d block %d", pb.FileID, pb.BlockIdx)
		}
		if Op(hashHdr.Op) != OpHash || hashHdr.FileID != pb.FileID || hashHdr.BlockIdx != pb.BlockIdx {
			return blockOutcome{}, fmt.Errorf("unexpected hash response for file %d block %d: op %d", pb.FileID, pb.BlockIdx, hashHdr.Op)
		}
		serverHash := make([]byte, hashHdr.DataSize)
		if _, err := io.ReadFull(conn, serverHash); err != nil {
			return blockOutcome{}, fmt.Errorf("read hash: %w", err)
		}

		if bytes.Equal(pb.Hash, serverHash) {
			return blockOutcome{changed: false}, nil
		}
	}

	if pb.IsZero {
		zmsg := &Msg{MagicHead: magicBytes, Op: uint8(OpBlockZero), FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
		data, err := pack(zmsg)
		if err != nil {
			return blockOutcome{}, err
		}
		if err := connWrite(conn, data); err != nil {
			return blockOutcome{}, fmt.Errorf("send zero: %w", err)
		}
		if err := waitDirAck(conn, pb.FileID, pb.BlockIdx); err != nil {
			return blockOutcome{}, fmt.Errorf("ack zero: %w", err)
		}
		return blockOutcome{changed: true, netBytes: 0, origBytes: expected}, nil
	}

	var dataToSend []byte
	var flags uint8
	if dt.noCompress || !pb.UseCompressed {
		if IsEncryptionEnabled() {
			dataToSend = encryptBlock(pb.Data)
		} else {
			dataToSend = pb.Data
		}
	} else {
		flags |= FlagCompressed
		dataToSend = pb.Compressed
	}
	if len(dataToSend) == 0 {
		return blockOutcome{}, fmt.Errorf("no data for file %d block %d", pb.FileID, pb.BlockIdx)
	}

	dmsg := &Msg{MagicHead: magicBytes, Op: uint8(OpBlockData), Flags: flags, FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize, DataSize: uint32(len(dataToSend))}
	data2, err := pack(dmsg)
	if err != nil {
		return blockOutcome{}, err
	}
	if err := connWriteFrame(conn, data2, dataToSend); err != nil {
		return blockOutcome{}, fmt.Errorf("send data block: %w", err)
	}
	if err := waitDirAck(conn, pb.FileID, pb.BlockIdx); err != nil {
		return blockOutcome{}, fmt.Errorf("ack data: %w", err)
	}
	return blockOutcome{changed: true, netBytes: uint32(len(dataToSend)), origBytes: expected}, nil
}

// waitDirAck takes io.Reader, not *AutoReconnectTCP, even though every
// production caller passes one: it only ever reads, and pinning the
// parameter type to the concrete connection type is unnecessary coupling
// (also matches the legacy path's equivalent, waitForBlockAck, which has
// always taken io.Reader).
func waitDirAck(conn io.Reader, fileID, blockIdx uint32) error {
	ack, err := readMsg(conn)
	if err != nil {
		return err
	}
	if Op(ack.Op) == OpError {
		return fmt.Errorf("server error acking file %d block %d", fileID, blockIdx)
	}
	if Op(ack.Op) != OpAck || ack.FileID != fileID || ack.BlockIdx != blockIdx {
		return fmt.Errorf("invalid ack for file %d block %d: op %d", fileID, blockIdx, ack.Op)
	}
	return nil
}
