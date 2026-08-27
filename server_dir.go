package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dirIdlePeriod is how long a directory-mode server waits with no active
// connections before giving up on ever hearing from a client. Deliberately
// much longer than the legacy single-file server's 2-second idle timeout: a
// directory client can legitimately spend a long time hashing/reading a
// large file between connections, and the control connection staying open
// for the whole transfer (not any one worker socket) is the real liveness
// signal. A var, not a const, so tests can shrink it (see ioTimeout).
var dirIdlePeriod = 30 * time.Second

// staleRefCheckPeriod is how often WaitAllFinalized re-scans for a stale
// ref: a connection that acquired a file (OpFileOpen) and then vanished
// without ever sending OpFileClose, e.g. it redialed onto a fresh
// connection mid-file and the old one's ref was simply abandoned. Without
// this, one dropped connection could wedge finalization forever.
const staleRefCheckPeriod = 2 * time.Second

// staleRefAge is how long a ref may sit idle, once OpFileDone has already
// been seen for that file, before it's force-released. Generous relative to
// ioTimeout so a merely-slow (not dead) connection is never force-closed
// out from under it. A var (its initializer reads the var ioTimeout), so a
// test that shrinks ioTimeout must also reassign this if it wants the
// relationship to still hold -- it is NOT automatically recomputed.
var staleRefAge = 2 * ioTimeout

// ctrlIoTimeout is the read deadline used once a connection has shown
// itself to be the CONTROL connection (it has carried an OpAnnounce,
// OpAnnounceEnd, or OpFileDone -- opcodes the client's architecture only
// ever sends on that one connection). The control connection legitimately
// sends nothing between OpAnnounceEnd and the first OpFileDone, which for
// one large file or a slow link can easily exceed ioTimeout (5 minutes) --
// using ioTimeout uniformly would false-positive-kill a perfectly healthy
// transfer. Generous (not unbounded), so a truly-dead peer is still
// eventually reaped; a var so tests can shrink it instead of waiting on
// the real value.
var ctrlIoTimeout = 6 * time.Hour

// isWithinRoot reports whether path is root itself or a descendant of it,
// after both are cleaned. Used after joining a sanitized relative path to
// the destination root, as a second, independent check.
func isWithinRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// rejectSymlinkedAncestors walks every path component strictly above
// cleanRel (a sanitized, root-relative path — the entry's own final
// component is never checked here, only what's above it), from the top
// down, and refuses if any one of them already exists as a symlink.
//
// This exists because os.MkdirAll does not: MkdirAll succeeds silently
// through an existing symlinked directory (it Stats — following symlinks —
// and treats a resolved directory as "already there"), so without this
// check a symlink planted at ANY ancestor level, not just the entry's
// immediate parent, would let Announce's own MkdirAll — and therefore every
// later write through the file it opens — silently escape the destination
// root via that symlink's target. isWithinRoot cannot catch this either: it
// only inspects the joined path string, never what the filesystem actually
// resolves it to.
//
// Not fully TOCTOU-safe (that needs openat2(RESOLVE_BENEATH), Linux-only):
// a symlink planted between this check and the later MkdirAll/open is not
// caught. The peer is already trusted enough to have a binary piped to it
// over SSH, so this is defense in depth against a stale or adversarial
// pre-existing destination tree, not a complete guarantee.
func rejectSymlinkedAncestors(root, cleanRel string) error {
	dir := filepath.Dir(filepath.FromSlash(cleanRel))
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("ancestor %q already exists as a symlink", part)
		}
	}
	return nil
}

// destFile is one destination-side file or directory known to a fileTable,
// keyed by FileID.
type destFile struct {
	id      uint32
	relPath string
	absPath string
	size    uint64
	mode    os.FileMode
	mtime   time.Time
	isDir   bool

	mu        sync.Mutex
	file      *os.File
	blockSize uint32 // negotiated from the first message that names one; rejects a later mismatch
	refs      int    // connections currently bound (OpFileOpen minus OpFileClose)
	sawDone   bool   // OpFileDone received

	// finalized is flipped true in the SAME critical section as the
	// shouldFinalize decision (see Release/MarkDone) — the instant it's
	// true, no further Acquire may reopen this file. It does NOT mean the
	// actual I/O (finalizeIO: drain/truncate/close/chmod/chtimes) has run
	// yet; finalizeIO is always called after df.mu is released, so it can
	// still be in flight on another goroutine well after this flips.
	finalized bool

	// finalizeComplete is flipped true only once finalizeIO has actually
	// RETURNED (see the wrapper in Release/MarkDone), successfully or not.
	// This — not finalized — is what WaitAllFinalized must wait for: a
	// destination file isn't really done until its data is drained,
	// truncated, closed, and its mode/mtime applied, and the caller relies
	// on WaitAllFinalized's return to mean it is now safe to shut down
	// shared resources (the OrderedWriter in particular) that finalizeIO
	// still uses.
	finalizeComplete bool

	finalizeErr error
	lastActive  time.Time // last Acquire/Release/block activity; drives stale-ref reaping

	// pendingBlocks tracks in-flight checksum requests awaiting their
	// matching write, keyed by BlockIdx so multiple workers can be mid-flight
	// on different blocks of the same large file concurrently. This is the
	// per-file analogue of the legacy single-connection pendingWrite guard
	// (server.go) against an unsolicited or altered write.
	pendingBlocks map[uint32]struct{}
}

// negotiateBlockSize records bs as this file's block size on first use, or
// validates that a later message agrees with it. The destination adopts
// nothing after the first message — unlike the legacy single-file path,
// which re-adopts the package-global blockSize from every message — because
// two files in flight at once could otherwise stomp each other's size.
func (df *destFile) negotiateBlockSize(bs uint32) error {
	df.mu.Lock()
	defer df.mu.Unlock()
	if df.blockSize == 0 {
		df.blockSize = bs
		return nil
	}
	if df.blockSize != bs {
		return fmt.Errorf("file %d: block size changed mid-transfer (%d -> %d)", df.id, df.blockSize, bs)
	}
	return nil
}

// isFinalized reports whether df's finalizeIO has actually completed — not
// just been decided — matching what WaitAllFinalized itself waits for (see
// allFinalized). Safe to call (and to poll in a loop) concurrently
// with Release/MarkDone/reapStaleRefs, unlike reading a field directly.
func (df *destFile) isFinalized() bool {
	df.mu.Lock()
	defer df.mu.Unlock()
	return df.finalizeComplete
}

// handle returns df's currently-open file handle (nil if never Acquired).
// Safe to call concurrently with anything else touching df.
func (df *destFile) handle() *os.File {
	df.mu.Lock()
	defer df.mu.Unlock()
	return df.file
}

// touch refreshes df.lastActive to now, on any block-level activity that
// doesn't otherwise already update it as part of a larger critical section
// (Acquire/Release/reapStaleRefs each fold their own lastActive update into
// their existing refs/finalized critical section instead of calling this,
// since splitting that into two lock acquisitions would reopen exactly the
// TOCTOU window their own comments explain why they avoid).
func (df *destFile) touch() {
	df.mu.Lock()
	df.lastActive = time.Now()
	df.mu.Unlock()
}

// fileTable is the directory destination server's per-transfer state: every
// file/directory announced so far, an fd budget, and the single
// OrderedWriter shared across every file in the tree (see writer.go).
type fileTable struct {
	root string
	ow   *OrderedWriter

	mu   sync.Mutex
	cond *sync.Cond
	byID map[uint32]*destFile
	// byPath maps a clean relative path to the FileID that claimed it,
	// rejecting a second announce for the same path under a different
	// FileID (a malformed or hostile announce batch; a real walk never
	// produces two entries for one path) so two FileIDs can never silently
	// interleave their writes into the same destination file. Deliberately
	// exact-match, not case-folded: a case-insensitive check would also
	// flag two source paths differing only in case (e.g. "A.txt"/"a.txt")
	// as colliding, which is a real false positive on a same-OS
	// case-sensitive transfer (this server's own filesystem) even though
	// it would be a genuine hazard against a case-insensitive destination
	// this server doesn't otherwise know about.
	byPath     map[string]uint32
	open       int
	openBudget int
	announced  int
}

func newFileTable(root string, workers int, ow *OrderedWriter) *fileTable {
	openBudget := workers * 4
	if openBudget < 64 {
		openBudget = 64
	}
	ft := &fileTable{
		root:       root,
		ow:         ow,
		byID:       make(map[uint32]*destFile),
		byPath:     make(map[string]uint32),
		openBudget: openBudget,
	}
	ft.cond = sync.NewCond(&ft.mu)
	return ft
}

// Announce registers one announced entry: it sanitizes rec.Path (never
// trusting a path that arrived over the wire), resolves it against root,
// and — for a directory — creates it immediately (MkdirAll), so a later
// file announced under it can be opened without racing its own
// parent-directory creation. A regular file's parent directories are
// likewise created here; the file itself is opened lazily by Acquire.
func (ft *fileTable) Announce(rec AnnounceRec) error {
	clean, err := sanitizeRelPath(rec.Path)
	if err != nil {
		return fmt.Errorf("announce %d: unsafe path %q: %w", rec.FileID, rec.Path, err)
	}
	abs := filepath.Join(ft.root, filepath.FromSlash(clean))
	if !isWithinRoot(ft.root, abs) {
		return fmt.Errorf("announce %d: path %q escapes the destination root", rec.FileID, clean)
	}

	switch rec.Kind {
	case KindDir:
		// Symlinked-ancestor mitigation: refuse an existing symlink
		// standing in for a real directory rather than silently writing
		// through it. Not fully TOCTOU-safe (that needs
		// openat2(RESOLVE_BENEATH), Linux-only) but cheap and catches the
		// straightforward case; the peer is already trusted enough to have
		// a binary piped to it over SSH, so this is defense in depth, not
		// the whole story.
		//
		// This only covers abs itself; an ancestor further up the tree
		// (some already-existing "a" that "a/b" is announced under) is
		// covered separately by rejectSymlinkedAncestors below — MkdirAll
		// silently succeeds through an existing symlinked directory, so
		// without that check a symlink planted at any ancestor level
		// (not just the immediate one) would let a later entry write
		// through it and escape the destination root.
		if fi, statErr := os.Lstat(abs); statErr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("announce %d: %q already exists as a symlink", rec.FileID, clean)
		}
		if err := rejectSymlinkedAncestors(ft.root, clean); err != nil {
			return fmt.Errorf("announce %d: %w", rec.FileID, err)
		}
		if err := os.MkdirAll(abs, 0777); err != nil {
			return fmt.Errorf("announce %d: mkdir %q: %w", rec.FileID, clean, err)
		}
	case KindFile:
		if err := rejectSymlinkedAncestors(ft.root, clean); err != nil {
			return fmt.Errorf("announce %d: %w", rec.FileID, err)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0777); err != nil {
			return fmt.Errorf("announce %d: mkdir parent of %q: %w", rec.FileID, clean, err)
		}
	case KindSkipped:
		// Recorded below for a later --delete pass's keep-set, but nothing
		// is created on disk for it, so there is no MkdirAll/open to
		// mitigate here.
	default:
		return fmt.Errorf("announce %d: unknown entry kind %d", rec.FileID, rec.Kind)
	}

	df := &destFile{
		id:            rec.FileID,
		relPath:       clean,
		absPath:       abs,
		size:          rec.FileSize,
		mode:          os.FileMode(rec.Mode),
		mtime:         time.Unix(rec.MTimeSec, int64(rec.MTimeNs)),
		isDir:         rec.Kind == KindDir,
		lastActive:    time.Now(),
		pendingBlocks: make(map[uint32]struct{}),
	}
	// A directory or skipped entry has nothing further to transfer, so
	// treat it as immediately finalized: it must not block WaitAllFinalized
	// or be reachable via Acquire for block traffic. There is no finalizeIO
	// to run for it (no fd was ever opened), so finalizeComplete is set
	// alongside finalized rather than left for a finalizeIO call that will
	// never happen.
	if rec.Kind != KindFile {
		df.finalized = true
		df.finalizeComplete = true
	}

	ft.mu.Lock()
	if _, exists := ft.byID[rec.FileID]; exists {
		ft.mu.Unlock()
		return fmt.Errorf("announce %d: file ID already announced", rec.FileID)
	}
	if ownerID, exists := ft.byPath[clean]; exists {
		ft.mu.Unlock()
		return fmt.Errorf("announce %d: path %q was already announced as file %d", rec.FileID, clean, ownerID)
	}
	ft.byID[rec.FileID] = df
	ft.byPath[clean] = rec.FileID
	ft.announced++
	ft.mu.Unlock()
	return nil
}

// AnnouncedCount returns the number of records announced so far.
func (ft *fileTable) AnnouncedCount() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.announced
}

// lookup returns the destFile for id, or an error if it was never
// announced.
func (ft *fileTable) lookup(id uint32) (*destFile, error) {
	ft.mu.Lock()
	df, ok := ft.byID[id]
	ft.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("file %d was never announced", id)
	}
	return df, nil
}

// waitForBudget blocks until fewer than openBudget files are currently
// open, then reserves a slot. Always pair with releaseBudget.
func (ft *fileTable) waitForBudget() {
	ft.mu.Lock()
	for ft.open >= ft.openBudget {
		ft.cond.Wait()
	}
	ft.open++
	ft.mu.Unlock()
}

func (ft *fileTable) releaseBudget() {
	ft.mu.Lock()
	ft.open--
	ft.mu.Unlock()
	ft.cond.Signal()
}

// Acquire opens (creating and truncating to size if necessary) the file for
// id and increments its reference count. Idempotent and safe to call
// concurrently for the same id from different connections — this is what
// makes soft binding safe: a connection that redials and re-sends
// OpFileOpen for a file it was already bound to just bumps refs again,
// never corrupts anything. created reports whether the file did not exist
// before this call (FlagCreated on the wire).
func (ft *fileTable) Acquire(id uint32) (df *destFile, created bool, err error) {
	df, err = ft.lookup(id)
	if err != nil {
		return nil, false, err
	}

	// The fd-budget wait must never happen while df.mu is held: cond.Wait()
	// can block for as long as the whole tree takes to make progress, and
	// holding a per-file lock across that would block every other
	// connection touching this same file too — including the finalize path
	// that would otherwise free up a slot. So: peek at whether opening is
	// even needed, unlocked; take the budget outside df.mu if so; then
	// re-check under df.mu, since another goroutine may have opened (or
	// finalized) the file while this one was waiting.
	df.mu.Lock()
	if df.finalized {
		df.mu.Unlock()
		return nil, false, fmt.Errorf("file %d already finalized", id)
	}
	alreadyOpen := df.file != nil
	df.mu.Unlock()

	tookBudget := false
	if !alreadyOpen {
		ft.waitForBudget()
		tookBudget = true
	}

	df.mu.Lock()
	defer df.mu.Unlock()
	if df.finalized {
		if tookBudget {
			ft.releaseBudget()
		}
		return nil, false, fmt.Errorf("file %d already finalized", id)
	}
	if df.file == nil {
		existing, statErr := os.Stat(df.absPath)
		created = os.IsNotExist(statErr)
		if statErr == nil && existing.Mode().Perm()&0200 == 0 {
			// A PREVIOUS run's finalize already applied this file's real
			// (possibly read-only) mode. The 0666 passed to OpenFile below
			// only governs creating a brand-new file — opening an EXISTING
			// file for O_RDWR is checked against ITS CURRENT permission
			// bits, so without this a read-only source file would sync
			// fine once and then fail with EACCES on every subsequent
			// (resume, idempotent no-op, or updated-content) run. finalize
			// always re-applies df.mode at the end regardless of what
			// happens here, so temporarily adding the owner-write bit
			// never leaves an incorrect final permission behind.
			os.Chmod(df.absPath, existing.Mode().Perm()|0200)
		}
		// 0666&^umask, not df.mode: the final permission (which may be
		// read-only) is applied at finalize, after every write; creating
		// with it up front could lock the transfer out of its own file.
		f, openErr := os.OpenFile(df.absPath, os.O_RDWR|os.O_CREATE, 0666)
		if openErr != nil {
			if tookBudget {
				ft.releaseBudget()
			}
			return nil, false, fmt.Errorf("open %s: %w", df.relPath, openErr)
		}
		if truncErr := truncateRegular(f, df.size); truncErr != nil {
			f.Close()
			if tookBudget {
				ft.releaseBudget()
			}
			return nil, false, fmt.Errorf("truncate %s: %w", df.relPath, truncErr)
		}
		df.file = f
	} else if tookBudget {
		// Lost the race: another goroutine opened it first while this one
		// waited on the budget. The slot was never used for our open —
		// give it back rather than leaking it.
		ft.releaseBudget()
	}
	df.refs++
	df.lastActive = time.Now()
	return df, created, nil
}

// tryFinalize is the shared double-latch pattern behind Release, MarkDone,
// and reapStaleRefs: decide, under df.mu, evaluates whatever combination of
// fields and side effects that specific caller needs (Release decrements
// refs; MarkDone sets sawDone; reapStaleRefs force-zeros refs) and returns
// whether df should finalize now. If so, tryFinalize flips df.finalized —
// in the SAME critical section decide ran in, not a decide-then-unlock-
// then-flip sequence — and only then runs finalizeIO.
//
// This ordering is load-bearing, not stylistic: unlocking between "the
// decision came back true" and "the flag is actually set" would leave a
// window where a stray Acquire (a buggy or hostile peer re-opening a file
// after its OpFileDone) could slip in, bump refs, and then race
// finalizeIO's close/chmod/chtimes against that caller's still-in-flight
// writes. decide must therefore never itself lock df.mu (it already holds
// it) and should be side-effect-free beyond mutating df's own fields.
func (ft *fileTable) tryFinalize(df *destFile, decide func() bool) error {
	df.mu.Lock()
	shouldFinalize := decide()
	var f *os.File
	if shouldFinalize {
		df.finalized = true
		f = df.file
	}
	df.mu.Unlock()
	if shouldFinalize {
		return ft.runFinalizeIO(df, f)
	}
	return nil
}

// Release decrements df's reference count and finalizes it if both halves
// of the double latch now hold: sawDone (OpFileDone seen) and refs == 0. A
// connection can still be mid-retry on a redialed link when OpFileDone
// arrives on the control connection, so neither event reliably comes last;
// both Release and MarkDone must re-check the pair.
func (ft *fileTable) Release(df *destFile) error {
	return ft.tryFinalize(df, func() bool {
		if df.refs > 0 {
			df.refs--
		}
		df.lastActive = time.Now()
		return df.sawDone && df.refs == 0 && !df.finalized
	})
}

// MarkDone records that OpFileDone was received for id (every block
// delivered) and finalizes it if refs is already 0. See tryFinalize for why
// the finalize decision and the finalized flag flip share one critical
// section.
func (ft *fileTable) MarkDone(id uint32) error {
	df, err := ft.lookup(id)
	if err != nil {
		return err
	}
	return ft.tryFinalize(df, func() bool {
		if df.finalized {
			return false
		}
		df.sawDone = true
		return df.refs == 0
	})
}

// runFinalizeIO calls finalizeIO and then, once it has actually returned,
// flips df.finalizeComplete under df.mu — unconditionally, whether
// finalizeIO succeeded or not, since a permanently-errored file must still
// eventually be observed as "done trying" by WaitAllFinalized rather than
// hang it forever (the error itself is separately recorded in
// df.finalizeErr and surfaced via FirstFinalizeErr).
func (ft *fileTable) runFinalizeIO(df *destFile, f *os.File) error {
	err := ft.finalizeIO(df, f)
	df.mu.Lock()
	df.finalizeComplete = true
	df.mu.Unlock()
	return err
}

// finalizeIO does the actual finalize work for df: drains any writes still
// in flight, truncates to its final size (in case the last block was short
// and never actually written, e.g. because it happened to be all zero and
// got clamped by the initial truncateRegular already — draining first
// still matters when blocks that DID write arrived after the initial
// open), closes the fd, then applies mode and mtime — in that order, and
// only in that order: closing before chmod/chtimes so no later write can
// reopen and dirty mtime again, and chtimes last of all since even chmod
// does not touch it.
//
// The caller must have already set df.finalized = true, atomically with
// whatever check decided finalization should happen (see Release/MarkDone);
// this function never checks or sets that flag itself, so it must never be
// called except immediately after such a check.
func (ft *fileTable) finalizeIO(df *destFile, f *os.File) error {
	if f == nil {
		// Never opened (a zero-byte file whose only block was implicit, or
		// a file announced but never acquired due to an earlier failure).
		// Create it directly so an empty destination file still exists.
		//
		// Deliberately does NOT take the fd budget: this fd is transient
		// (opened, truncated, and closed within this same function call,
		// never handed back to a caller), and finalizeIO commonly runs on
		// the control connection's single goroutine as it processes a
		// stream of OpFileDone messages. Blocking that goroutine in
		// waitForBudget's cond.Wait() when the budget is exhausted would be
		// self-defeating: releasing budget slots happens on this very same
		// finalize path, on files that were actually Acquire'd — so the one
		// goroutine that could free a slot would be the one stuck waiting
		// for one.
		var createErr error
		f, createErr = os.OpenFile(df.absPath, os.O_RDWR|os.O_CREATE, 0666)
		if createErr != nil {
			ft.recordFinalizeErr(df, createErr)
			return createErr
		}
	} else {
		defer ft.releaseBudget()
	}

	var err error
	if ft.ow != nil {
		err = ft.ow.DrainFile(f)
	}
	if err == nil {
		err = truncateRegular(f, df.size)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(df.absPath, df.mode.Perm())
	}
	if err == nil {
		err = os.Chtimes(df.absPath, df.mtime, df.mtime)
	}
	if err != nil {
		ft.recordFinalizeErr(df, err)
	}
	return err
}

func (ft *fileTable) recordFinalizeErr(df *destFile, err error) {
	df.mu.Lock()
	if df.finalizeErr == nil {
		df.finalizeErr = err
	}
	df.mu.Unlock()
}

// snapshot returns every destFile currently known to the table, as a plain
// slice safe to range over without holding ft.mu (the lock is only needed
// to safely copy the map's values out; each destFile then has its own
// mu for whatever the caller does with it next).
func (ft *fileTable) snapshot() []*destFile {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	files := make([]*destFile, 0, len(ft.byID))
	for _, df := range ft.byID {
		files = append(files, df)
	}
	return files
}

// reapStaleRefs force-releases any ref that has sat idle past staleRefAge
// on a file that has already seen OpFileDone — the connection that held it
// is presumed gone (redialed elsewhere without sending OpFileClose, or
// simply died) rather than legitimately still working. Returns how many
// refs were reaped, for logging.
func (ft *fileTable) reapStaleRefs() int {
	files := ft.snapshot()

	reaped := 0
	for _, df := range files {
		wasStale := false
		ft.tryFinalize(df, func() bool {
			stale := df.sawDone && !df.finalized && df.refs > 0 && time.Since(df.lastActive) > staleRefAge
			if stale {
				df.refs = 0
				df.lastActive = time.Now()
				wasStale = true
			}
			return stale
		})
		if wasStale {
			reaped++
			Log("directory server: reaped a stale reference on file %d (%s); its owning connection is presumed gone\n", df.id, df.relPath)
		}
	}
	return reaped
}

// WaitAllFinalized blocks until every announced regular file has been
// finalized, periodically reaping stale refs so a single vanished
// connection can't wedge this forever. Returns ctx.Err() if ctx is done
// first.
func (ft *fileTable) WaitAllFinalized(ctx context.Context) error {
	ticker := time.NewTicker(staleRefCheckPeriod)
	defer ticker.Stop()
	for {
		if ft.allFinalized() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ft.reapStaleRefs()
		}
	}
}

// allFinalized reports whether every announced file has actually completed
// finalizeIO. (Named without the "Locked" suffix Go convention reserves for
// "caller must hold the lock" — this takes and releases ft.mu itself, via
// snapshot, and holds no lock on return.)
func (ft *fileTable) allFinalized() bool {
	files := ft.snapshot()
	for _, df := range files {
		df.mu.Lock()
		// finalizeComplete, not finalized: the latter flips before
		// finalizeIO's actual drain/truncate/close/chmod/chtimes work has
		// run (see the destFile field comments), so waiting on it here
		// would let WaitAllFinalized return — and OpTreeDone ack, and the
		// server shut down its shared OrderedWriter — while another
		// goroutine's finalizeIO call is still in flight.
		done := df.finalizeComplete
		df.mu.Unlock()
		if !done {
			return false
		}
	}
	return true
}

// FirstFinalizeErr returns the first per-file finalize error recorded
// across the whole table, if any, for surfacing as the transfer's overall
// result.
func (ft *fileTable) FirstFinalizeErr() error {
	files := ft.snapshot()
	for _, df := range files {
		df.mu.Lock()
		err := df.finalizeErr
		df.mu.Unlock()
		if err != nil {
			return fmt.Errorf("file %d (%s): %w", df.id, df.relPath, err)
		}
	}
	return nil
}

// dirServerState is the whole-tree bookkeeping for one startDirServer
// invocation, deliberately struct-scoped (not package globals) so multiple
// directory transfers in the same test binary never interfere with each
// other the way the legacy single-file path's globals require an explicit
// reset between runs to avoid.
type dirServerState struct {
	// activeConns and totalBytes are the struct's 64-bit atomics and must
	// stay first: sync/atomic only guarantees 8-byte alignment for the
	// FIRST word of an allocated struct on 32-bit platforms (ARM, x86-32,
	// 32-bit MIPS) -- a *fileTable pointer (4 bytes on those platforms)
	// preceding an int64/uint64 field would misalign it, and
	// atomic.AddInt64/LoadUint64 on a misaligned word can panic there. This
	// project currently only ships amd64/windows-amd64 builds, so this was
	// latent rather than observed, but correct regardless of today's build
	// targets.
	activeConns int64  // atomic
	totalBytes  uint64 // atomic

	ft *fileTable

	treeDoneSeen int32 // atomic

	// fileCount/announceEndSeen are set by OpAnnounceEnd and read by
	// OpTreeDone's cross-check (totalBytes above too). In practice both
	// opcodes arrive on the same (control) connection, but the wire
	// protocol does not itself enforce that — soft binding means no
	// connection's identity is authoritative, so nothing here should rely
	// on connection affinity for correctness. Kept atomic accordingly,
	// matching treeDoneSeen above.
	fileCount       uint32 // atomic
	announceEndSeen int32  // atomic; 0/1
}

// startDirServer listens on bindIp:port and serves as the destination for a
// directory-tree upload: it accepts connections, dispatches OpAnnounce /
// OpFileOpen / OpBlockCheck / OpBlockZero / OpBlockData / OpFileClose /
// OpFileDone / OpTreeDone per the v3 protocol, and returns once the tree
// has been fully received and every file finalized (or an unrecoverable
// error occurs).
func startDirServer(root, bindIp, port string, workers int, quiet bool) error {
	if err := os.MkdirAll(root, 0777); err != nil {
		return fmt.Errorf("create destination root: %w", err)
	}

	bindTo := ":" + port
	if bindIp != "0.0.0.0" {
		bindTo = bindIp + ":" + port
	}
	listener, err := net.Listen("tcp", bindTo)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	defer listener.Close()

	Log("READY, listening on %s\n", bindTo)

	ow := NewOrderedWriter(nil, workers)
	// A tree's files are independent: one file's write failure must not
	// poison every other file's writes (see writeOne's perItemErr comment).
	ow.EnablePerItemErrors()
	defer ow.Close()
	st := &dirServerState{ft: newFileTable(root, workers, ow)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idleTimer := time.NewTimer(dirIdlePeriod)
	connChan := make(chan net.Conn)
	errChan := make(chan error, 1)
	doneChan := make(chan struct{})
	// connClosed is how a per-connection goroutine tells the main loop it
	// finished, WITHOUT touching idleTimer itself: Timer.Reset from
	// multiple goroutines on a possibly-fired timer is documented-unsafe
	// (the exact class of bug Phase 0 already fixed once for the legacy
	// single-file server's shutdownTimer). All Timer mutation stays on
	// this one goroutine; connClosed just triggers the decision.
	connClosed := make(chan struct{}, 1)

	// connWG tracks every dirHandleConn goroutine. startDirServer must not
	// return (and therefore must not run its deferred ow.Close(), which
	// closes the OrderedWriter's channel) while a handler could still be
	// mid-write -- see the defer ordering note below.
	var connWG sync.WaitGroup
	// Declared AFTER `defer ow.Close()` above: Go defers run LIFO, so this
	// Wait() executes FIRST on return, guaranteeing every handler has
	// exited before the writer is closed out from under it.
	defer connWG.Wait()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case errChan <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case connChan <- conn:
			case <-ctx.Done():
				conn.Close()
				return
			}
		}
	}()

	// stopIdleTimer and resetIdleTimer are the standard drain-before-reuse
	// idiom for time.Timer: Stop (or an unconsumed prior fire) can leave a
	// value sitting in idleTimer.C, and Reset is only documented-safe on a
	// timer whose channel is known-empty. Both are called only from this
	// function's own single goroutine, alongside every other read of
	// idleTimer.C, so there is no concurrent-access race to guard against
	// beyond this drain.
	stopIdleTimer := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
	}
	resetIdleTimer := func() {
		stopIdleTimer()
		idleTimer.Reset(dirIdlePeriod)
	}

	for {
		select {
		case conn := <-connChan:
			stopIdleTimer()
			atomic.AddInt64(&st.activeConns, 1)
			connWG.Add(1)
			go func(c net.Conn) {
				defer connWG.Done()
				defer atomic.AddInt64(&st.activeConns, -1)
				defer func() {
					select {
					case connClosed <- struct{}{}:
					default:
						// A signal is already pending; the main loop will
						// re-check activeConns when it processes that one,
						// so a dropped duplicate here loses nothing.
					}
				}()
				if handlerErr := dirHandleConn(c, st, doneChan); handlerErr != nil {
					// A per-connection error (a dropped link, a malformed
					// frame, a rejected protocol violation) must not abort
					// the whole multi-connection, multi-file transfer: the
					// client already retries transparently on connection
					// failure exactly as if this were a network blip, and
					// releaseBinding's deferred Release already unwound
					// whatever ref this connection held. Log it and keep
					// serving every other connection. Only listener.Accept
					// failing (below) is treated as fatal to the server as
					// a whole.
					Log("directory server: connection error: %v\n", handlerErr)
				}
			}(conn)

		case err := <-errChan:
			return fmt.Errorf("accept: %w", err)

		case <-idleTimer.C:
			if atomic.LoadInt64(&st.activeConns) == 0 && atomic.LoadInt32(&st.treeDoneSeen) == 0 {
				return fmt.Errorf("directory server idle timeout: no client ever connected")
			}

		case <-connClosed:
			// A connection just finished. If every connection is now
			// closed and the client hasn't sent OpTreeDone yet, restart
			// the idle clock -- without this, a client that connects and
			// then vanishes (crash, network partition, or any bug that
			// kills its connections) leaves this server blocked in this
			// select forever: idleTimer.Stop() above only ever fires
			// once, on the FIRST connection, and nothing else was
			// resetting it.
			if atomic.LoadInt64(&st.activeConns) == 0 && atomic.LoadInt32(&st.treeDoneSeen) == 0 {
				resetIdleTimer()
			}

		case <-doneChan:
			// OpTreeDone handled successfully and acked; wait for that
			// connection's handler goroutine to finish closing out, then
			// exit cleanly.
			if ferr := st.ft.FirstFinalizeErr(); ferr != nil {
				return ferr
			}
			return nil
		}
	}
}

// dirSession is one connection's local view of the directory protocol: the
// file it's currently bound to (if any) and the block it has an outstanding
// checksum request for, mirroring the legacy single-file server's
// pendingWrite guard against an unsolicited or altered write.
type dirSession struct {
	bound    bool
	fileID   uint32
	df       *destFile
	blockBuf []byte
}

// dirHandleConn services one connection for the lifetime of a directory
// transfer. doneChan is closed exactly once, by whichever connection
// successfully processes OpTreeDone, to tell startDirServer's main loop the
// transfer is complete.
func dirHandleConn(conn net.Conn, st *dirServerState, doneChan chan struct{}) error {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(keepAlivePeriod)
	}

	magicBytes := stringToFixedSizeArray(magicHead)
	msgBuf := make([]byte, msgSize)
	c := bufio.NewReader(conn)
	sess := &dirSession{}

	// releaseBinding unbinds sess from whatever file it currently holds,
	// telling fileTable so the ref count (and, eventually, finalization)
	// reflects it. Used both for an explicit OpFileClose and for cleanup
	// when the connection ends without one.
	releaseBinding := func() {
		if sess.bound && sess.df != nil {
			st.ft.Release(sess.df)
		}
		sess.bound = false
		sess.df = nil
	}
	defer releaseBinding()

	// isControlConn tracks whether this connection has carried a
	// control-only opcode (OpAnnounce, OpAnnounceEnd, OpFileDone) -- by the
	// client's own architecture, those are only ever sent on the control
	// connection, which legitimately falls silent between OpAnnounceEnd
	// and the first OpFileDone for as long as the first file in flight
	// takes to complete. Applying the short worker-oriented ioTimeout
	// uniformly here would false-positive-kill a perfectly healthy but
	// slow transfer (see ctrlIoTimeout's doc comment).
	isControlConn := false

	for {
		deadline := ioTimeout
		if isControlConn {
			deadline = ctrlIoTimeout
		}
		conn.SetDeadline(time.Now().Add(deadline))
		if _, err := io.ReadFull(c, msgBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("read request: %w", err)
		}
		msg, err := unpack(msgBuf)
		if err != nil {
			return fmt.Errorf("unpack request: %w", err)
		}
		if msg.MagicHead != magicBytes {
			return fmt.Errorf("invalid request magic")
		}
		// Covers Reserved (as the old inline check here did) plus every
		// other directory-mode opcode's shape: unexpected flags, and sanity
		// caps on wire-driven allocation sizes (DataSize/BlockSize) that
		// this loop previously trusted unconditionally before handing them
		// to handleAnnounce's make([]byte, msg.DataSize) or
		// negotiateBlockSize's unconditional first-use acceptance.
		if err := validateDirClientFrame(msg); err != nil {
			return fmt.Errorf("invalid request: %w", err)
		}

		switch Op(msg.Op) {
		case OpAnnounce:
			isControlConn = true
			if err := handleAnnounce(conn, c, msg, st.ft); err != nil {
				return err
			}

		case OpAnnounceEnd:
			isControlConn = true
			atomic.StoreUint32(&st.fileCount, msg.BlockIdx)
			atomic.StoreUint64(&st.totalBytes, msg.FileSize)
			atomic.StoreInt32(&st.announceEndSeen, 1)

		case OpFileOpen:
			if sess.bound && sess.fileID != msg.FileID {
				releaseBinding()
			}
			df, created, err := st.ft.Acquire(msg.FileID)
			if err != nil {
				sendDirError(conn, magicBytes, err)
				return fmt.Errorf("acquire file %d: %w", msg.FileID, err)
			}
			if err := df.negotiateBlockSize(msg.BlockSize); err != nil {
				st.ft.Release(df)
				sendDirError(conn, magicBytes, err)
				return err
			}
			sess.bound = true
			sess.fileID = msg.FileID
			sess.df = df
			var flags uint8
			if created {
				flags |= FlagCreated
			}
			ack := &Msg{MagicHead: magicBytes, Op: uint8(OpFileAck), Flags: flags, FileID: msg.FileID, BlockSize: df.blockSize, FileSize: df.size}
			if err := sendMsg(conn, ack); err != nil {
				return err
			}

		case OpFileClose:
			if sess.bound && sess.fileID == msg.FileID {
				releaseBinding()
			}

		case OpBlockCheck:
			if err := handleBlockCheck(conn, magicBytes, sess, msg); err != nil {
				return err
			}

		case OpBlockZero:
			if err := handleBlockZero(conn, magicBytes, sess, st.ft, msg); err != nil {
				return err
			}

		case OpBlockData:
			if err := handleBlockData(conn, c, magicBytes, sess, st.ft, msg); err != nil {
				return err
			}

		case OpFileDone:
			isControlConn = true
			// OpFileDone is fire-and-forget from the client (see
			// recordBlockDone: it sends and moves on, never reads a
			// reply) — unlike OpFileOpen/OpBlockCheck/OpTreeDone, which
			// are real request/response pairs. A finalize failure here
			// (e.g. the destination path collides with an existing
			// directory) must therefore neither write an unsolicited
			// response frame — which would desync the NEXT genuinely
			// expected response on this same connection (OpTreeDone's
			// OpTreeAck), since the client never reads anything back for
			// OpFileDone and that stray frame would just sit in the
			// stream ahead of it — nor kill the connection: for the
			// control connection specifically, "kill the connection" means
			// killing the one channel OpTreeDone still needs to arrive on,
			// silently orphaning the whole transfer (WaitAllFinalized was
			// never even reached, so nothing ever unblocks
			// startDirServer's main loop). The error is already recorded
			// on df.finalizeErr by finalizeIO and surfaces via
			// FirstFinalizeErr() once the tree actually finishes — that is
			// the correct, honest way for one file's unrecoverable failure
			// to reach the overall transfer result, not by aborting the
			// control connection out from under every other file still in
			// flight.
			if err := st.ft.MarkDone(msg.FileID); err != nil {
				Log("directory server: finalize error for file %d: %v\n", msg.FileID, err)
			}

		case OpTreeDone:
			return handleTreeDone(conn, magicBytes, st, msg, doneChan)

		default:
			return fmt.Errorf("unexpected op %d", msg.Op)
		}
	}
}

func sendMsg(conn net.Conn, msg *Msg) error {
	data, err := pack(msg)
	if err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	return connWrite(conn, data)
}

func sendDirError(conn net.Conn, magicBytes [magicLen]byte, cause error) {
	text := cause.Error()
	if len(text) > 4096 {
		text = text[:4096]
	}
	hdr := &Msg{MagicHead: magicBytes, Op: uint8(OpError), DataSize: uint32(len(text))}
	data, err := pack(hdr)
	if err != nil {
		return
	}
	if connWrite(conn, data) != nil {
		return
	}
	connWrite(conn, []byte(text))
}

func handleAnnounce(conn net.Conn, r *bufio.Reader, msg *Msg, ft *fileTable) error {
	if msg.DataSize == 0 {
		return fmt.Errorf("empty announce batch")
	}
	conn.SetDeadline(time.Now().Add(ioTimeout))
	trailer := make([]byte, msg.DataSize)
	if _, err := io.ReadFull(r, trailer); err != nil {
		return fmt.Errorf("read announce trailer: %w", err)
	}
	recs, err := decodeAnnounceBatch(trailer)
	if err != nil {
		return fmt.Errorf("decode announce batch: %w", err)
	}
	for _, rec := range recs {
		if err := ft.Announce(rec); err != nil {
			return err
		}
	}
	return nil
}

func handleBlockCheck(conn net.Conn, magicBytes [magicLen]byte, sess *dirSession, msg *Msg) error {
	if !sess.bound || sess.fileID != msg.FileID {
		return fmt.Errorf("checksum request for file %d without a matching OpFileOpen", msg.FileID)
	}
	df := sess.df
	if err := df.negotiateBlockSize(msg.BlockSize); err != nil {
		sendDirError(conn, magicBytes, err)
		return err
	}
	expected := blockPayloadSize(df.size, msg.BlockSize, msg.BlockIdx)
	if expected == 0 {
		return fmt.Errorf("invalid block %d for file %d of size %d", msg.BlockIdx, msg.FileID, df.size)
	}
	if cap(sess.blockBuf) < int(expected)+int(encryptOverhead) {
		sess.blockBuf = make([]byte, expected+encryptOverhead)
	}
	buf := sess.blockBuf[:expected]

	f := df.handle()
	df.touch()
	n, err := f.ReadAt(buf, int64(msg.BlockIdx)*int64(msg.BlockSize))
	if err != nil && err != io.EOF {
		return fmt.Errorf("read destination block %d of file %d: %w", msg.BlockIdx, msg.FileID, err)
	}
	if n != int(expected) {
		return fmt.Errorf("read destination block %d of file %d: %w", msg.BlockIdx, msg.FileID, io.ErrUnexpectedEOF)
	}

	var hash []byte
	if isZeroBlock(buf) {
		hash = zeroBlockHash
	} else {
		hash = checksum(buf)
	}
	hashMsg := &Msg{MagicHead: magicBytes, Op: uint8(OpHash), FileID: msg.FileID, BlockIdx: msg.BlockIdx, BlockSize: msg.BlockSize, FileSize: df.size, DataSize: uint32(len(hash))}
	if err := sendMsg(conn, hashMsg); err != nil {
		// A write failure here (e.g. a dropped connection) self-heals: the
		// caller's next read will hit the connection's own read deadline
		// and the client redials, exactly as any other connection loss
		// does. But silently swallowing it (bare "return nil", no log)
		// previously converted a clean, immediately-diagnosable error into
		// an up-to-5-minute silent stall from the operator's point of view.
		Log("directory server: send hash for file %d block %d: %v\n", msg.FileID, msg.BlockIdx, err)
		return nil
	}
	if err := connWrite(conn, hash); err != nil {
		Log("directory server: send hash digest for file %d block %d: %v\n", msg.FileID, msg.BlockIdx, err)
		return nil
	}

	df.mu.Lock()
	df.pendingBlocks[msg.BlockIdx] = struct{}{}
	df.mu.Unlock()
	return nil
}

// takePending validates and clears an outstanding checksum request for
// (fileID, blockIdx), the per-file analogue of the legacy single-connection
// pendingWrite guard: a write is accepted only if it matches a checksum
// request this file has actually seen, keyed by block index so several
// workers can be mid-flight on different blocks of the same file at once.
func takePending(df *destFile, blockIdx uint32) bool {
	df.mu.Lock()
	defer df.mu.Unlock()
	if _, ok := df.pendingBlocks[blockIdx]; !ok {
		return false
	}
	delete(df.pendingBlocks, blockIdx)
	return true
}

// writeAndAck writes payload at msg's block offset (via the shared
// OrderedWriter, ordered across the whole tree by writeKey), refreshes
// df.lastActive, and sends the OpAck response — the epilogue
// handleBlockZero and handleBlockData both need identically, once each has
// separately decided what payload to write (an all-zero buffer, or the
// decoded wire payload).
func writeAndAck(conn net.Conn, magicBytes [magicLen]byte, df *destFile, ft *fileTable, msg *Msg, payload []byte) error {
	offset := int64(msg.BlockIdx) * int64(msg.BlockSize)
	key := writeKey(msg.FileID, msg.BlockIdx)
	if err := ft.ow.SubmitAndWaitFile(df.handle(), key, offset, payload); err != nil {
		return fmt.Errorf("write file %d block %d: %w", msg.FileID, msg.BlockIdx, err)
	}
	df.touch()
	ack := &Msg{MagicHead: magicBytes, Op: uint8(OpAck), FileID: msg.FileID, BlockIdx: msg.BlockIdx, BlockSize: msg.BlockSize, FileSize: df.size}
	return sendMsg(conn, ack)
}

func handleBlockZero(conn net.Conn, magicBytes [magicLen]byte, sess *dirSession, ft *fileTable, msg *Msg) error {
	if !sess.bound || sess.fileID != msg.FileID {
		return fmt.Errorf("zero write for file %d without a matching OpFileOpen", msg.FileID)
	}
	df := sess.df
	if !takePending(df, msg.BlockIdx) {
		return fmt.Errorf("unsolicited zero write for file %d block %d", msg.FileID, msg.BlockIdx)
	}
	expected := blockPayloadSize(df.size, msg.BlockSize, msg.BlockIdx)
	if expected == 0 {
		return fmt.Errorf("invalid zero block %d for file %d", msg.BlockIdx, msg.FileID)
	}
	return writeAndAck(conn, magicBytes, df, ft, msg, getZeroBuf(int(expected)))
}

func handleBlockData(conn net.Conn, r *bufio.Reader, magicBytes [magicLen]byte, sess *dirSession, ft *fileTable, msg *Msg) error {
	if !sess.bound || sess.fileID != msg.FileID {
		return fmt.Errorf("data write for file %d without a matching OpFileOpen", msg.FileID)
	}
	df := sess.df
	if !takePending(df, msg.BlockIdx) {
		return fmt.Errorf("unsolicited data write for file %d block %d", msg.FileID, msg.BlockIdx)
	}
	expected := blockPayloadSize(df.size, msg.BlockSize, msg.BlockIdx)
	if expected == 0 {
		return fmt.Errorf("invalid data block %d for file %d", msg.BlockIdx, msg.FileID)
	}
	if uint64(msg.DataSize) > uint64(msg.BlockSize)+uint64(encryptOverhead) {
		return fmt.Errorf("payload for file %d block %d is too large", msg.FileID, msg.BlockIdx)
	}
	conn.SetDeadline(time.Now().Add(ioTimeout))
	raw := make([]byte, msg.DataSize)
	if _, err := io.ReadFull(r, raw); err != nil {
		return fmt.Errorf("read payload for file %d block %d: %w", msg.FileID, msg.BlockIdx, err)
	}

	var payload []byte
	if msg.Flags&FlagCompressed != 0 {
		decompressed, err := decompressData(raw)
		if err != nil {
			return fmt.Errorf("decompress file %d block %d: %w", msg.FileID, msg.BlockIdx, err)
		}
		payload = decompressed
	} else if IsEncryptionEnabled() {
		plain, err := decryptBlock(raw)
		if err != nil {
			return fmt.Errorf("decrypt file %d block %d: %w", msg.FileID, msg.BlockIdx, err)
		}
		payload = plain
	} else {
		payload = raw
	}
	if uint32(len(payload)) != expected {
		return fmt.Errorf("file %d block %d has invalid decoded size %d", msg.FileID, msg.BlockIdx, len(payload))
	}

	return writeAndAck(conn, magicBytes, df, ft, msg, payload)
}

func handleTreeDone(conn net.Conn, magicBytes [magicLen]byte, st *dirServerState, msg *Msg, doneChan chan struct{}) error {
	atomic.StoreInt32(&st.treeDoneSeen, 1)

	if atomic.LoadInt32(&st.announceEndSeen) == 0 {
		err := fmt.Errorf("tree done received before announce end — refusing to treat the tree as complete")
		sendDirError(conn, magicBytes, err)
		return err
	}
	fileCount := atomic.LoadUint32(&st.fileCount)
	announced := uint32(st.ft.AnnouncedCount())
	if announced != fileCount {
		err := fmt.Errorf("tree done: client claims %d announced entries, server saw %d — refusing to treat the tree as complete", fileCount, announced)
		sendDirError(conn, magicBytes, err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := st.ft.WaitAllFinalized(ctx); err != nil {
		sendDirError(conn, magicBytes, err)
		return fmt.Errorf("waiting for files to finalize: %w", err)
	}

	ack := &Msg{MagicHead: magicBytes, Op: uint8(OpTreeAck), BlockIdx: msg.BlockIdx, FileSize: msg.FileSize}
	if err := sendMsg(conn, ack); err != nil {
		return err
	}
	select {
	case <-doneChan:
	default:
		close(doneChan)
	}
	return nil
}
