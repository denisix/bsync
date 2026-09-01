package main

// Pipelined directory-transfer worker: the windowed replacement for the
// strictly synchronous per-block exchange in handleDirBlock/processDirBlock
// (which remain as the reference implementation, the unit-test surface, and
// the BSYNC_NOPIPELINE fallback).
//
// The insight that makes this work with ZERO server changes: the directory
// server processes each connection's frames strictly in order and replies in
// that same order, every request and reply carries FileID+BlockIdx, and
// destFile.pendingBlocks is a per-block-index set built to tolerate many
// outstanding checks. So one worker connection can keep a whole window of
// requests in flight -- across file boundaries too -- and consume the
// replies as a FIFO, instead of paying one full round trip per protocol
// step. On a WAN link this collapses the 3 sequential RTTs of a small
// changed file (open-ack, check-hash, data-ack) into a continuously busy
// pipe, and an unchanged N-block file from N+1 RTTs toward ~1.
//
// Send order == server processing order == reply order, so the expectation
// FIFO below is always consumed in exactly the order entries were pushed;
// every push happens at the moment its request frame is written, which is
// what keeps the two orders identical by construction.
//
// One deliberate speculation: a file's first OpBlockCheck is sent in the
// same burst as its OpFileOpen, before the open-ack (whose FlagCreated bit
// the strict path waits for) has been seen. This is safe for a created-
// fresh file because Acquire truncates it straight to its announced size,
// so handleBlockCheck reads whole blocks of zeros and replies the
// zero-block hash: a zero source block then matches (resolved unchanged, no
// write -- the same end state as the strict created fast path) and any
// other block mismatches and sends its data exactly as it must. Once the
// open-ack has been consumed, later blocks of a created file skip the
// check entirely, as the strict path does.
//
// Failure handling is window-scoped where the strict path's was
// block-scoped: a connection or protocol error tears down everything in
// flight, requeues each unresolved block with one more retry charged, and
// re-drives them (rebinding as needed) on a fresh dial -- so a block still
// gets maxRetries attempts before its file is failed, they just come one
// window-abort apart instead of one withRetry loop apart. A deterministic
// per-file open rejection (FlagErrPermanent) dooms only that file and
// requeues everyone else without charging them an attempt.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
)

// dirPipelineWindow caps how many blocks one worker connection retains at
// once: on the wire awaiting a reply (in w.expects), or parked in w.held by
// the file-switch gate. This is a per-worker memory/re-drive ceiling, not a
// throughput guarantee -- the TreeReader's own byte budget
// (workers*4*capBS, see dirsync.go's sharedChan setup) is usually the
// tighter bound for cap-size blocks, so this window is fully reachable only
// for smaller blocks (small files), which is exactly the WAN small-file
// case the pipeline targets. inflight() counts both expects and held so
// the cap below is exact, not off by the held slot.
const dirPipelineWindow = 32

// dirPipelineDisabled falls back to the strict one-request-one-reply worker
// loop (handleDirBlock), e.g. to rule the pipeline out when debugging:
// BSYNC_NOPIPELINE=1. A var, not a constant, so tests can drive both paths.
var dirPipelineDisabled = os.Getenv("BSYNC_NOPIPELINE") != ""

type expectKind uint8

const (
	expOpenAck expectKind = iota // OpFileAck for an OpFileOpen
	expHash                      // OpHash + digest for an OpBlockCheck
	expAck                       // OpAck for an OpBlockData/OpBlockZero
)

// dirExpect is one entry of the reply FIFO: which reply comes next on the
// wire and, for block-carrying kinds, the block it settles (plus that
// block's accumulated retry count, so a window abort can requeue it with
// its history intact).
type dirExpect struct {
	kind     expectKind
	fileID   uint32
	pb       PrecomputedBlock // valid when hasPB
	hasPB    bool
	retries  int
	netBytes uint32 // expAck: wire bytes sent for this block (0 for a zero block)
}

// pendingPB is an unresolved block queued for (re-)admission.
type pendingPB struct {
	pb      PrecomputedBlock
	retries int
}

// pipeWorker is one transfer worker's pipelining state: the reply FIFO, the
// re-drive queue, and the admission-side view of the connection's current
// file binding (which file new sends belong to, and whether its open-ack
// has been seen yet).
type pipeWorker struct {
	dt   *dirTransferState
	conn *AutoReconnectTCP

	expects []dirExpect
	retryQ  []pendingPB

	bound        bool
	gen          uint64
	fileID       uint32
	createdKnown bool
	created      bool

	failStreak int // consecutive charged window aborts; reset on any settled block

	// held parks one admitted-but-not-yet-sendable block: switching the
	// connection to a new file (Close+Open) is illegal while any OpBlockCheck
	// of the CURRENT file is still awaiting its hash, because consuming that
	// hash may have to send the block's data -- which would then arrive at
	// the server after the Close, on a session bound to the new file. Only
	// hash expectations gate this; an outstanding OpAck is receive-only and
	// crosses file boundaries freely.
	held *pendingPB
}

// pendingHashForCurrent reports whether any in-flight OpBlockCheck belongs
// to the connection's current file binding. By induction only the current
// binding can have one: a switch away is exactly what this gate forbids
// until the count reaches zero.
func (w *pipeWorker) pendingHashForCurrent() bool {
	if !w.bound {
		return false
	}
	for _, e := range w.expects {
		if e.kind == expHash && e.fileID == w.fileID {
			return true
		}
	}
	return false
}

// inflight counts blocks currently retained by this worker: on the wire
// awaiting a reply, plus the one (if any) parked in w.held by the
// file-switch gate -- both are memory the worker is holding onto, so both
// count against dirPipelineWindow.
func (w *pipeWorker) inflight() int {
	n := 0
	for _, e := range w.expects {
		if e.hasPB {
			n++
		}
	}
	if w.held != nil {
		n++
	}
	return n
}

// resolveOK settles one block successfully, mirroring handleDirBlock's
// success accounting: exactly one AddBlockProgress + recordBlockDone +
// Release per block, on every path.
func (w *pipeWorker) resolveOK(pb PrecomputedBlock, changed bool, netBytes uint32) {
	blockLen := uint64(blockPayloadSize(pb.FileSize, pb.BlockSize, pb.BlockIdx))
	w.dt.sess.AddBlockProgress(changed, blockLen, uint64(netBytes), blockLen)
	w.dt.recordBlockDone(pb.FileID, changed)
	pb.Release()
	w.failStreak = 0
}

// resolveFailed settles one block whose file is doomed (failFile already
// called by the site that decided so, or the file was failed earlier).
// Never reports the block as changed -- if an earlier block of the same
// file already transferred, the file still correctly counts as changed via
// liveFile.changed's latch.
func (w *pipeWorker) resolveFailed(pb PrecomputedBlock) {
	w.dt.recordBlockDone(pb.FileID, false)
	pb.Release()
}

// abortWindow tears the window down after a connection or protocol error:
// every unresolved block (extra, if non-nil, is the one being processed
// when the error hit, and goes FIRST to preserve admission order) is either
// requeued for re-drive or, once out of retries, failed; only the window
// head is charged an attempt (see below). chargeRetry is false for a
// deterministic per-file rejection, where the connection was deliberately
// killed by the server and nothing should lose an attempt over it.
func (w *pipeWorker) abortWindow(err error, extra *dirExpect, extraFirst, chargeRetry bool) {
	w.conn.Close()
	w.bound = false
	w.createdKnown = false

	// Re-drive order mirrors admission order: a block popped off the FIFO
	// head when the error hit (extraFirst) predates everything still in
	// the FIFO; a block that failed while being ADMITTED postdates it.
	var unresolved []pendingPB
	if extra != nil && extra.hasPB && extraFirst {
		unresolved = append(unresolved, pendingPB{pb: extra.pb, retries: extra.retries})
	}
	for _, e := range w.expects {
		if e.hasPB {
			unresolved = append(unresolved, pendingPB{pb: e.pb, retries: e.retries})
		}
	}
	if extra != nil && extra.hasPB && !extraFirst {
		unresolved = append(unresolved, pendingPB{pb: extra.pb, retries: extra.retries})
	}
	w.expects = w.expects[:0]

	// Only the window HEAD is charged a retry: it is the block whose reply
	// the failure interrupted, and charging everything in flight would give
	// all pipelined blocks shared-fate budgets -- three connection blips
	// with a full window would doom every file at once, where the strict
	// path burned one block's budget per three CONSECUTIVE failures. With
	// head-only charging a persistent failure still converges (each head
	// exhausts its budget in turn and is failed), and a transient blip
	// costs the re-driven blocks nothing.
	requeued := 0
	for i, p := range unresolved {
		if chargeRetry && i == 0 {
			p.retries++
		}
		if p.retries >= maxRetries {
			Log("file %d block %d: failed after %d retries: %v\n", p.pb.FileID, p.pb.BlockIdx, maxRetries, err)
			w.dt.sess.failFile(p.pb.FileID, fmt.Errorf("block %d: %w", p.pb.BlockIdx, err))
			w.resolveFailed(p.pb)
			continue
		}
		w.retryQ = append(w.retryQ, p)
		requeued++
	}

	if chargeRetry {
		w.failStreak++
		Log("transfer window aborted (%d block(s) requeued): %v\n", requeued, err)
		// Same backoff shape as withRetry: 0s before the 2nd attempt's
		// dial has already been paid once here as 1s, growing linearly,
		// capped so a long outage doesn't sleep unboundedly.
		streak := w.failStreak
		if streak > maxRetries {
			streak = maxRetries
		}
		time.Sleep(time.Duration(streak-1) * time.Second)
	}
}

// admit drives one block into the window: (re)bind the connection to its
// file if needed (Close(prev)+Open coalesced into one write, expectation
// pushed, NOT waited for), then send the block's first frame -- a check,
// or straight data for a known-created file, or nothing at all for a
// known-created zero block. Any send error aborts the whole window with
// this block included for re-drive.
func (w *pipeWorker) admit(p pendingPB) {
	pb := p.pb
	if w.dt.sess.isFailed(pb.FileID) {
		// A prior block of this file exhausted its retries; this one is
		// skipped but still accounted, or the file would never reach zero
		// remaining and the server would wait forever in WaitAllFinalized.
		w.resolveFailed(pb)
		return
	}
	if err := w.conn.Dial(); err != nil {
		w.abortWindow(err, &dirExpect{pb: pb, hasPB: true, retries: p.retries}, false, true)
		return
	}
	if !w.bound || w.fileID != pb.FileID || w.gen != w.conn.Gen() {
		sameConnDifferentFile := w.bound && w.gen == w.conn.Gen() && w.fileID != pb.FileID
		openMsg := &Msg{MagicHead: w.dt.magicBytes, Op: uint8(OpFileOpen), FileID: pb.FileID, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
		openData, err := pack(openMsg)
		if err == nil {
			if sameConnDifferentFile {
				closeMsg := &Msg{MagicHead: w.dt.magicBytes, Op: uint8(OpFileClose), FileID: w.fileID}
				if closeData, cerr := pack(closeMsg); cerr == nil {
					err = connWriteFrame(w.conn, closeData, openData)
				} else {
					err = connWrite(w.conn, openData)
				}
			} else {
				err = connWrite(w.conn, openData)
			}
		}
		if err != nil {
			w.abortWindow(fmt.Errorf("send file-open: %w", err), &dirExpect{pb: pb, hasPB: true, retries: p.retries}, false, true)
			return
		}
		w.expects = append(w.expects, dirExpect{kind: expOpenAck, fileID: pb.FileID})
		w.bound = true
		w.gen = w.conn.Gen()
		w.fileID = pb.FileID
		w.createdKnown = false
		w.created = false
	}

	if w.createdKnown && w.created {
		// Known-created file: its whole content is guaranteed zeros
		// (truncated straight to size at open), so a zero block needs no
		// wire op at all, and a data block always differs -- skip the
		// checksum exchange, exactly like the strict created fast path.
		if pb.IsZero {
			w.resolveOK(pb, false, 0)
			return
		}
		w.sendBlockPayload(p, false)
		return
	}

	checkMsg := &Msg{MagicHead: w.dt.magicBytes, Op: uint8(OpBlockCheck), FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
	data, err := pack(checkMsg)
	if err == nil {
		err = connWrite(w.conn, data)
	}
	if err != nil {
		w.abortWindow(fmt.Errorf("send check: %w", err), &dirExpect{pb: pb, hasPB: true, retries: p.retries}, false, true)
		return
	}
	w.expects = append(w.expects, dirExpect{kind: expHash, fileID: pb.FileID, pb: pb, hasPB: true, retries: p.retries})
}

// sendBlockPayload writes the block's data (or zero marker) frame and
// pushes its ack expectation, identical wire behavior to the strict path's
// second half of processDirBlock.
func (w *pipeWorker) sendBlockPayload(p pendingPB, atFront bool) {
	pb := p.pb
	if pb.IsZero {
		zmsg := &Msg{MagicHead: w.dt.magicBytes, Op: uint8(OpBlockZero), FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize}
		data, err := pack(zmsg)
		if err == nil {
			err = connWrite(w.conn, data)
		}
		if err != nil {
			w.abortWindow(fmt.Errorf("send zero: %w", err), &dirExpect{pb: pb, hasPB: true, retries: p.retries}, atFront, true)
			return
		}
		w.expects = append(w.expects, dirExpect{kind: expAck, fileID: pb.FileID, pb: pb, hasPB: true, retries: p.retries, netBytes: 0})
		return
	}

	var dataToSend []byte
	var flags uint8
	if w.dt.noCompress || !pb.UseCompressed {
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
		// Corrupt precompute; deterministic, so retrying cannot help.
		Log("file %d block %d: no data to send\n", pb.FileID, pb.BlockIdx)
		w.dt.sess.failFile(pb.FileID, fmt.Errorf("block %d: no data to send", pb.BlockIdx))
		w.resolveFailed(pb)
		return
	}
	dmsg := &Msg{MagicHead: w.dt.magicBytes, Op: uint8(OpBlockData), Flags: flags, FileID: pb.FileID, BlockIdx: pb.BlockIdx, BlockSize: pb.BlockSize, FileSize: pb.FileSize, DataSize: uint32(len(dataToSend))}
	hdr, err := pack(dmsg)
	if err == nil {
		err = connWriteFrame(w.conn, hdr, dataToSend)
	}
	if err != nil {
		w.abortWindow(fmt.Errorf("send data block: %w", err), &dirExpect{pb: pb, hasPB: true, retries: p.retries}, atFront, true)
		return
	}
	w.expects = append(w.expects, dirExpect{kind: expAck, fileID: pb.FileID, pb: pb, hasPB: true, retries: p.retries, netBytes: uint32(len(dataToSend))})
}

// consumeOne pops the FIFO head and reads its reply off the wire, settling
// or advancing the block it carries. Returns false only when there was
// nothing to consume.
func (w *pipeWorker) consumeOne() bool {
	if len(w.expects) == 0 {
		return false
	}
	e := w.expects[0]
	w.expects = w.expects[1:]

	reply, err := readMsg(w.conn)
	if err != nil {
		w.abortWindow(fmt.Errorf("read reply: %w", err), &e, true, true)
		return true
	}

	switch e.kind {
	case expOpenAck:
		if Op(reply.Op) == OpError {
			if reply.Flags&FlagErrPermanent != 0 {
				// Deterministic rejection of THIS file (already finalized,
				// failed at announce, ...): doom it, and requeue everything
				// else without charging an attempt -- the server kills its
				// side of the connection after sending an error, so the
				// window cannot continue, but nothing else was at fault.
				// The server's reason text is still in the receive buffer;
				// read it first so both the log and the recorded per-file
				// error say WHY, then bound the logging (one destination
				// problem cascades to every file under it).
				reason := readDirErrorText(w.conn, reply.DataSize)
				failErr := fmt.Errorf("file-open: %w", errPermanent)
				logLine := "server rejected file-open permanently"
				if reason != "" {
					failErr = fmt.Errorf("file-open rejected: %s: %w", reason, errPermanent)
					logLine = "server rejected file-open permanently: " + reason
				}
				w.dt.logReject(e.fileID, logLine)
				w.dt.sess.failFile(e.fileID, failErr)
				w.abortWindow(fmt.Errorf("file %d: %w", e.fileID, errPermanent), nil, false, false)
				return true
			}
			// Most often a worker's open racing a still-buffering announce
			// batch; the retry re-drive absorbs it exactly as withRetry did.
			w.abortWindow(fmt.Errorf("server rejected file-open for file %d", e.fileID), nil, false, true)
			return true
		}
		if Op(reply.Op) != OpFileAck || reply.FileID != e.fileID {
			w.abortWindow(fmt.Errorf("unexpected reply to file-open for file %d: op %d", e.fileID, reply.Op), nil, false, true)
			return true
		}
		if w.bound && w.fileID == e.fileID {
			// Still the admission-side binding: later blocks of this file
			// can now use the created fast path.
			w.createdKnown = true
			w.created = reply.Flags&FlagCreated != 0
		}
		w.failStreak = 0

	case expHash:
		if Op(reply.Op) == OpError {
			w.abortWindow(fmt.Errorf("server error on checksum request for file %d block %d", e.fileID, e.pb.BlockIdx), &e, true, true)
			return true
		}
		if Op(reply.Op) != OpHash || reply.FileID != e.pb.FileID || reply.BlockIdx != e.pb.BlockIdx {
			w.abortWindow(fmt.Errorf("unexpected hash response for file %d block %d: op %d", e.pb.FileID, e.pb.BlockIdx, reply.Op), &e, true, true)
			return true
		}
		serverHash := make([]byte, reply.DataSize)
		if _, err := io.ReadFull(w.conn, serverHash); err != nil {
			w.abortWindow(fmt.Errorf("read hash: %w", err), &e, true, true)
			return true
		}
		if bytes.Equal(e.pb.Hash, serverHash) {
			w.resolveOK(e.pb, false, 0)
			return true
		}
		w.sendBlockPayload(pendingPB{pb: e.pb, retries: e.retries}, true)

	case expAck:
		if Op(reply.Op) == OpError {
			w.abortWindow(fmt.Errorf("server error acking file %d block %d", e.pb.FileID, e.pb.BlockIdx), &e, true, true)
			return true
		}
		if Op(reply.Op) != OpAck || reply.FileID != e.pb.FileID || reply.BlockIdx != e.pb.BlockIdx {
			w.abortWindow(fmt.Errorf("invalid ack for file %d block %d: op %d", e.pb.FileID, e.pb.BlockIdx, reply.Op), &e, true, true)
			return true
		}
		w.resolveOK(e.pb, true, e.netBytes)
	}
	return true
}

// runDirTransferWorkerPipelined is runDirTransferWorker's windowed loop:
// admit blocks (retry queue first, then the dedicated channel, then the
// shared one -- the same priority order as the strict loop) until the
// window is full or nothing is immediately available, then consume one
// reply; park blocking on the channels only when the window is empty.
func runDirTransferWorkerPipelined(conn *AutoReconnectTCP, dt *dirTransferState, dedChan <-chan PrecomputedBlock, sharedChan <-chan PrecomputedBlock) {
	w := &pipeWorker{dt: dt, conn: conn}
	ded, shared := dedChan, sharedChan

	// tryAdmit routes one candidate through the file-switch gate: a block
	// for a different file than the current binding must wait (in w.held)
	// until no hash reply is outstanding for the current file. Returns
	// false when the candidate was parked rather than admitted.
	tryAdmit := func(p pendingPB) bool {
		if w.bound && p.pb.FileID != w.fileID && w.pendingHashForCurrent() {
			w.held = &p
			return false
		}
		w.admit(p)
		return true
	}

	for {
		// Admission: top the window up without blocking.
		for w.inflight() < dirPipelineWindow && w.held == nil {
			if len(w.retryQ) > 0 {
				p := w.retryQ[0]
				w.retryQ = w.retryQ[1:]
				tryAdmit(p)
				continue
			}
			if ded == nil && shared == nil {
				break
			}
			// Priority probe: dedicated first, then shared, never blocking
			// here -- with work in flight the reply loop below must run.
			admitted := false
			select {
			case v, ok := <-ded:
				if !ok {
					ded = nil
				} else {
					tryAdmit(pendingPB{pb: v})
					admitted = true
				}
			default:
				select {
				case v, ok := <-ded:
					if !ok {
						ded = nil
					} else {
						tryAdmit(pendingPB{pb: v})
						admitted = true
					}
				case v, ok := <-shared:
					if !ok {
						shared = nil
					} else {
						tryAdmit(pendingPB{pb: v})
						admitted = true
					}
				default:
				}
			}
			if !admitted && len(w.retryQ) == 0 {
				break
			}
		}

		if len(w.expects) > 0 {
			w.consumeOne()
			// A consumed reply may have cleared the file-switch gate.
			if w.held != nil && !(w.bound && w.held.pb.FileID != w.fileID && w.pendingHashForCurrent()) {
				p := *w.held
				w.held = nil
				w.admit(p)
			}
			continue
		}
		if w.held != nil {
			// Nothing in flight, so the gate cannot be blocked anymore.
			p := *w.held
			w.held = nil
			w.admit(p)
			continue
		}
		if len(w.retryQ) > 0 {
			continue
		}
		if ded == nil && shared == nil {
			return
		}

		// Window empty, nothing queued: park until a source produces
		// (dedicated first, same as the strict loop's priority receive).
		select {
		case v, ok := <-ded:
			if !ok {
				ded = nil
				continue
			}
			tryAdmit(pendingPB{pb: v})
		default:
			select {
			case v, ok := <-ded:
				if !ok {
					ded = nil
					continue
				}
				tryAdmit(pendingPB{pb: v})
			case v, ok := <-shared:
				if !ok {
					shared = nil
					continue
				}
				tryAdmit(pendingPB{pb: v})
			}
		}
	}
}
