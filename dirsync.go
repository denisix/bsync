package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
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

// TotalBytes sums the Size of every KindFile entry known so far. Grows
// alongside FileCount as the walk discovers more entries.
func (s *SyncSession) TotalBytes() uint64 {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	var total uint64
	for _, fe := range s.files {
		if fe.Kind == KindFile {
			total += fe.Size
		}
	}
	return total
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

// --- client-side directory transfer (Phase 4: on-demand hashing only) ---

// announceBatchSize caps how many AnnounceRec accumulate before a batch is
// flushed as one OpAnnounce frame, so a 100k-file tree sends ~500 frames
// instead of 100k.
const announceBatchSize = 200

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
	for rec := range records {
		batch = append(batch, rec)
		if len(batch) >= announceBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// dirTransferState is the shared, read-mostly state every transfer worker
// needs: the session, the control connection (for per-file OpFileDone), the
// per-file remaining-block counters, and transfer options.
type dirTransferState struct {
	sess       *SyncSession
	ctrl       *ctrlConn
	magicBytes [magicLen]byte
	noCompress bool
	remaining  sync.Map // FileID -> *atomic.Int64, blocks left before OpFileDone
	// doneOnce guards OpFileDone to exactly one send per file, independent
	// of the remaining-block counter above: forceFileDone (below) can fire
	// for a file that never had all its blocks flow through
	// recordBlockDone at all (TreeReader failed to open or fully read it),
	// so the counter alone cannot be trusted to converge on zero. Both
	// paths funnel through sendFileDone, so whichever fires first wins and
	// the other becomes a no-op.
	doneOnce sync.Map // FileID -> *sync.Once
}

// sendFileDone marks fileID done in the session and sends OpFileDone,
// exactly once, regardless of which caller (a block reaching zero
// remaining, or forceFileDone below) triggers it.
func (dt *dirTransferState) sendFileDone(fileID uint32) {
	v, _ := dt.doneOnce.LoadOrStore(fileID, &sync.Once{})
	v.(*sync.Once).Do(func() {
		dt.sess.MarkFileDone()
		if err := dt.ctrl.send(&Msg{Op: uint8(OpFileDone), FileID: fileID}, nil); err != nil {
			Log("file %d: failed to send file-done: %v\n", fileID, err)
		}
	})
}

// remainingCounter returns fileID's per-file remaining-block counter
// (lazily seeded from its expected total block count on first touch, by
// whichever caller — a real completing block via recordBlockDone, or a
// correction via forceFileDone — touches it first) alongside that expected
// total, so a caller needing both doesn't have to look fe.Blocks up twice.
func (dt *dirTransferState) remainingCounter(fileID uint32) (counter *atomic.Int64, total int64) {
	total = 1
	if fe := dt.sess.FileByID(fileID); fe != nil && fe.Blocks > 0 {
		total = int64(fe.Blocks)
	}
	fresh := new(atomic.Int64)
	fresh.Store(total)
	v, _ := dt.remaining.LoadOrStore(fileID, fresh)
	return v.(*atomic.Int64), total
}

// recordBlockDone accounts for one block of fileID having been fully
// processed (whether transferred or found already in sync — both count
// toward "this file is done"). Whichever worker's call brings the count to
// zero sends OpFileDone; blocks complete out of order across workers, so
// this is not necessarily the worker that opened the file.
func (dt *dirTransferState) recordBlockDone(fileID uint32) {
	counter, _ := dt.remainingCounter(fileID)
	if counter.Add(-1) == 0 {
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
// exist are cancelled out.
func (dt *dirTransferState) forceFileDone(fileID uint32, blocksEmitted uint32) {
	counter, total := dt.remainingCounter(fileID)
	neverComing := total - int64(blocksEmitted)
	if neverComing <= 0 {
		return // every expected block was actually emitted; nothing to correct
	}
	if counter.Add(-neverComing) == 0 {
		dt.sendFileDone(fileID)
	}
}

// startDirClient walks srcRoot, announces it to serverAddress, and
// transfers every differing block over workers parallel connections. The
// returned SyncSession's FailedCount/FailedError report per-file failures
// that did not abort the transfer; the returned error is non-nil only for a
// failure of the transfer as a whole (the walk itself, or the control
// connection).
func startDirClient(srcRoot, serverAddress string, blockSizeCap uint32, noCompress bool, workers int) (*SyncSession, error) {
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
	go func() {
		defer close(walkDone)
		walkErr = walkSource(srcRoot, nil, fileChan, func(relPath, reason string) {
			Log("skip %s: %s\n", relPath, reason)
		})
	}()

	// Single consumer of the walk: registering each entry with the session
	// IS what assigns it there (walkSource's IDs are dense in emission
	// order, matching AddFile's contract), then fans it out to the tree
	// reader and the announce queue.
	go func() {
		defer close(treeFileChan)
		defer close(announceChan)
		for fe := range fileChan {
			sess.AddFile(fe)
			if fe.Kind == KindSkipped {
				sess.MarkFileSkipped()
			}
			treeFileChan <- fe
			announceChan <- AnnounceRec{
				FileID: fe.ID, FileSize: fe.Size, Mode: uint32(fe.Mode),
				MTimeSec: fe.ModTime.Unix(), MTimeNs: int32(fe.ModTime.Nanosecond()),
				Kind: fe.Kind, Path: fe.RelPath,
			}
		}
	}()

	announceErrCh := make(chan error, 1)
	go func() { announceErrCh <- runAnnounceBatcher(ctrl, announceChan) }()

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
	tr := NewTreeReader(treeFileChan, sess, int64(workers)*4*int64(capBS))
	tr.onFileFailed = dt.forceFileDone
	tr.Start()
	precompChan := make(chan PrecomputedBlock, workers*2)
	precomputeTreeBlocks(tr, sess, precompChan, workers, noCompress)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runDirTransferWorker(saddr, dt, precompChan)
		}()
	}
	// Guarantees the reader/precompute/worker chain always drains via the
	// natural channel-close cascade (walkSource closes its output on any
	// return, error or not) on EVERY exit path below, including the early
	// returns on a walk/announce/send error -- without this, an early
	// return here would leak the goroutines still blocked on a channel
	// that will now never be read from again.
	defer wg.Wait()

	<-walkDone
	if walkErr != nil {
		return sess, fmt.Errorf("walking %s: %w", srcRoot, walkErr)
	}
	sess.MarkWalkDone()

	if err := <-announceErrCh; err != nil {
		return sess, fmt.Errorf("announcing tree: %w", err)
	}

	fileCount := uint32(sess.FileCount())
	if err := ctrl.send(&Msg{Op: uint8(OpAnnounceEnd), BlockIdx: fileCount, FileSize: sess.TotalBytes()}, nil); err != nil {
		return sess, fmt.Errorf("send announce end: %w", err)
	}

	// Every announce is now confirmed sent, so it's safe to immediately
	// finish any zero-byte file: it produces no blocks (TreeReader skips
	// Size==0 entries entirely), so recordBlockDone's block-counting path
	// never reaches it.
	for id := uint32(0); id < fileCount; id++ {
		fe := sess.FileByID(id)
		if fe != nil && fe.Kind == KindFile && fe.Size == 0 {
			sess.MarkFileDone()
			if err := ctrl.send(&Msg{Op: uint8(OpFileDone), FileID: fe.ID}, nil); err != nil {
				return sess, fmt.Errorf("send file-done for empty file %d: %w", fe.ID, err)
			}
		}
	}

	wg.Wait()

	if err := ctrl.send(&Msg{Op: uint8(OpTreeDone), BlockIdx: fileCount, FileSize: sess.TotalBytes()}, nil); err != nil {
		return sess, fmt.Errorf("send tree done: %w", err)
	}
	ackBuf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(rawConn, ackBuf); err != nil {
		return sess, fmt.Errorf("waiting for tree ack: %w", err)
	}
	ack, err := unpack(ackBuf)
	if err != nil {
		return sess, fmt.Errorf("unpack tree ack: %w", err)
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

// runDirTransferWorker pulls blocks from precompChan and sends them,
// maintaining its own connection and soft file-binding: it resolves a
// block's target purely by the FileID carried on every frame (see
// server_dir.go), so a silently redialed connection never risks
// corruption, only the cost of one extra OpFileOpen round trip.
func runDirTransferWorker(saddr *net.TCPAddr, dt *dirTransferState, precompChan <-chan PrecomputedBlock) {
	conn := NewAutoReconnectTCP(saddr)
	defer conn.Close()

	var bound bool
	var boundFileID uint32
	var boundGen uint64

	for pb := range precompChan {
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
			continue
		}

		var lastErr error
		var outcome blockOutcome
		for retry := 0; retry < maxRetries; retry++ {
			if retry > 0 {
				Log("file %d block %d: retry %d/%d after: %v\n", pb.FileID, pb.BlockIdx, retry, maxRetries-1, lastErr)
				conn.Close()
				bound = false
				time.Sleep(time.Duration(retry) * time.Second)
			}
			if err := conn.Dial(); err != nil {
				lastErr = err
				continue
			}

			needRebind := !bound || boundFileID != pb.FileID || boundGen != conn.Gen()
			if needRebind {
				sameConnDifferentFile := bound && boundGen == conn.Gen() && boundFileID != pb.FileID
				if err := dirWorkerBind(conn, dt, pb, sameConnDifferentFile, boundFileID); err != nil {
					lastErr = err
					bound = false
					continue
				}
				bound = true
				boundFileID = pb.FileID
				boundGen = conn.Gen()
			}

			outcome, lastErr = processDirBlock(conn, dt, pb)
			if lastErr == nil {
				break
			}
			bound = false
		}

		if lastErr != nil {
			Log("file %d block %d: failed after %d retries: %v\n", pb.FileID, pb.BlockIdx, maxRetries, lastErr)
			dt.sess.failFile(pb.FileID, fmt.Errorf("block %d: %w", pb.BlockIdx, lastErr))
		} else {
			dt.sess.AddBlockProgress(outcome.changed, uint64(outcome.netBytes), uint64(outcome.origBytes))
		}
		// Every block that leaves this loop, succeeded or not, counts
		// toward the file being "resolved" -- see the comment above for
		// why a failure must still be counted.
		dt.recordBlockDone(pb.FileID)
		pb.Release()
	}
}

// dirWorkerBind (re)binds conn to pb.FileID: if the same physical
// connection genuinely held a ref for a DIFFERENT file (sameConnDifferentFile),
// it releases that ref first via OpFileClose; otherwise (first bind, or a
// redial happened) there is no ref on this connection to close — the old
// one, if any, is on a now-dead connection and becomes the server's stale
// ref to reap, not this connection's problem. Then sends OpFileOpen and
// waits for the ack.
func dirWorkerBind(conn *AutoReconnectTCP, dt *dirTransferState, pb PrecomputedBlock, sameConnDifferentFile bool, prevFileID uint32) error {
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
		return err
	}
	if err := connWrite(conn, data); err != nil {
		return fmt.Errorf("send file-open: %w", err)
	}
	ackBuf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, ackBuf); err != nil {
		return fmt.Errorf("read file-open ack: %w", err)
	}
	ack, err := unpack(ackBuf)
	if err != nil {
		return fmt.Errorf("unpack file-open ack: %w", err)
	}
	if Op(ack.Op) == OpError {
		return fmt.Errorf("server rejected file-open for file %d", pb.FileID)
	}
	if Op(ack.Op) != OpFileAck || ack.FileID != pb.FileID {
		return fmt.Errorf("unexpected reply to file-open for file %d: op %d", pb.FileID, ack.Op)
	}
	return nil
}

// processDirBlock runs the checksum-request/compare/write exchange for one
// block on a connection already bound to its file, mirroring
// processPrecomputedBlock's single-file logic.
func processDirBlock(conn *AutoReconnectTCP, dt *dirTransferState, pb PrecomputedBlock) (blockOutcome, error) {
	magicBytes := dt.magicBytes

	checkMsg := &Msg{MagicHead: magicBytes, Op: uint8(OpBlockCheck), FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
	data, err := pack(checkMsg)
	if err != nil {
		return blockOutcome{}, err
	}
	if err := connWrite(conn, data); err != nil {
		return blockOutcome{}, fmt.Errorf("send check: %w", err)
	}

	hdrBuf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, hdrBuf); err != nil {
		return blockOutcome{}, fmt.Errorf("read hash header: %w", err)
	}
	hashHdr, err := unpack(hdrBuf)
	if err != nil {
		return blockOutcome{}, fmt.Errorf("unpack hash header: %w", err)
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

	expected := blockPayloadSize(pb.FileSize, pb.BlockSize, pb.BlockIdx)

	if bytes.Equal(pb.Hash, serverHash) {
		return blockOutcome{changed: false}, nil
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
	if err := connWrite(conn, data2); err != nil {
		return blockOutcome{}, fmt.Errorf("send data header: %w", err)
	}
	if err := connWrite(conn, dataToSend); err != nil {
		return blockOutcome{}, fmt.Errorf("send data payload: %w", err)
	}
	if err := waitDirAck(conn, pb.FileID, pb.BlockIdx); err != nil {
		return blockOutcome{}, fmt.Errorf("ack data: %w", err)
	}
	return blockOutcome{changed: true, netBytes: uint32(len(dataToSend)), origBytes: expected}, nil
}

func waitDirAck(conn *AutoReconnectTCP, fileID, blockIdx uint32) error {
	buf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	ack, err := unpack(buf)
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
