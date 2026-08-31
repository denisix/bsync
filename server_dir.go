package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
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
func (ft *fileTable) rejectSymlinkedAncestors(cleanRel string) error {
	dir := path.Dir(cleanRel)
	if dir == "." || dir == "/" {
		return nil
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.verifiedDirs == nil {
		ft.verifiedDirs = make(map[string]struct{})
	}
	if _, ok := ft.verifiedDirs[dir]; ok {
		// Every component of dir was lstat'd clean earlier this run (or dir
		// was created by this run), so re-walking it would just repeat
		// those syscalls for each of a directory's siblings -- O(entries x
		// depth) lstats over a tree, which dominates announce on a cold
		// cache or a network filesystem.
		return nil
	}

	// Verify only the components not already known clean, marking each
	// prefix as we go so a sibling announced next skips the whole walk.
	var relPrefix string
	cur := ft.root
	for _, part := range strings.Split(dir, "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if relPrefix == "" {
			relPrefix = part
		} else {
			relPrefix += "/" + part
		}
		if _, ok := ft.verifiedDirs[relPrefix]; ok {
			continue
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("ancestor %q already exists as a symlink", part)
		}
		ft.verifiedDirs[relPrefix] = struct{}{}
	}
	return nil
}

// markVerifiedDir records a directory this run just created (or confirmed) as
// a clean, non-symlink ancestor, so rejectSymlinkedAncestors need not lstat
// it again for any of the entries announced beneath it.
func (ft *fileTable) markVerifiedDir(cleanRel string) {
	if cleanRel == "" || cleanRel == "." {
		return
	}
	ft.mu.Lock()
	if ft.verifiedDirs == nil {
		ft.verifiedDirs = make(map[string]struct{})
	}
	ft.verifiedDirs[cleanRel] = struct{}{}
	ft.mu.Unlock()
}

// errAlreadyFinalized marks an Acquire rejection whose outcome can never
// change: once finalized (whether by normal completion, a quick-check
// skip, or an announce-time failure), a file can never be reopened for
// block traffic. dirHandleConn translates it into an OpError carrying
// FlagErrPermanent so the client fails the file immediately -- unlike
// every other Acquire failure (most importantly "never announced", which
// the pipeline legitimately produces transiently when a worker's open
// races ahead of a still-buffering announce batch), which stays retryable.
var errAlreadyFinalized = errors.New("already finalized")

// destFile is one destination-side file or directory known to a fileTable,
// keyed by FileID.
type destFile struct {
	id      uint32
	relPath string
	// absPath is deliberately NOT stored: it is always
	// filepath.Join(ft.root, filepath.FromSlash(relPath)), recomputed on
	// demand by fileTable.abs. Retaining it cost a second, separately
	// allocated ~144-byte string per announced entry purely to avoid one
	// Join call in the two places (Acquire, finalizeIO) that already make a
	// filesystem syscall right after -- at tree scale that was gigabytes of
	// duplicate path data for no benefit.
	size  uint64
	mode  os.FileMode
	mtime time.Time
	// kind records what this entry was announced as -- used only by
	// deleteExtraneous, to tell a real synced directory (safe to recurse
	// into during a -delete pass) apart from a file or a KindSkipped
	// placeholder (a protected leaf, never recursed into even if it
	// happens to be a directory on the destination).
	kind EntryKind

	mu        sync.Mutex
	file      *os.File
	blockSize uint32 // negotiated from the first message that names one; rejects a later mismatch
	refs      int    // connections currently bound (OpFileOpen minus OpFileClose)
	sawDone   bool   // OpFileDone received

	// createdFresh records that this transfer's own Acquire created the
	// destination file (it did not exist before). Such a file was empty at
	// creation and truncated straight to its announced size, so its entire
	// content is owned by this transfer -- which is why
	// takePendingOrFresh accepts writes to it WITHOUT a prior OpBlockCheck
	// (the client's fresh-file fast path skips that round trip; see
	// processDirBlock). Never set for a pre-existing file: those keep the
	// full unsolicited-write guard.
	createdFresh bool

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

// abs reconstructs df's absolute destination path from ft.root and
// df.relPath. Identical by construction to what announce computed when it
// created df (filepath.Join(ft.root, filepath.FromSlash(clean))) -- see the
// destFile.absPath field comment for why this is recomputed rather than
// stored.
func (ft *fileTable) abs(df *destFile) string {
	return filepath.Join(ft.root, filepath.FromSlash(df.relPath))
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
	// byID holds only LIVE entries: announced but not yet retired (see
	// retire). An entry is removed the instant its finalizeIO has actually
	// completed (born-finalized entries -- a dir, symlink, special, skipped
	// placeholder, or quick-check skip -- never enter it at all, see
	// announce). This is what keeps the table's memory O(files genuinely in
	// flight) instead of O(whole tree): on a mostly-unchanged quick-check
	// re-sync, nearly every entry retires at announce time and byID stays
	// small for the whole run. "Absent from byID" is therefore NOT the same
	// as "never announced" -- see announcedBits and lookup, which exist
	// precisely to keep those two cases distinguishable once byID no longer
	// retains everything.
	byID map[uint32]*destFile

	// announcedBits is a dense bitmap, one bit per FileID, set the moment an
	// entry registers (announce) and NEVER cleared -- unlike byID, which
	// drops an entry once it retires. lookup consults it on a byID miss to
	// tell "retired" (bit set) apart from "not yet announced" (bit clear);
	// conflating the two is exactly the failure mode pendingDone below
	// guards against for the latter case, and errAlreadyFinalized exists to
	// signal the former as a permanent, non-retryable rejection. Grown by
	// doubling; ~4 MB at 32M entries, pointer-free.
	announcedBits []uint64

	// keep is the `-delete` keep-set AND, until announceClosed, the
	// duplicate-path detector that used to be byPath's job (see
	// keepSet's own doc comment for why a 128-bit digest set replaces a
	// map[string]uint32 of every announced path). Built during announce,
	// consulted by deleteExtraneous, then discarded by freeAnnounceScratch
	// once the announce stream is known closed -- nothing after
	// OpAnnounceEnd calls announce or deleteExtraneous again for this tree,
	// so its ~1.1 GB/32M-path peak is confined to the announce phase.
	keep *keepSet

	// announceClosed is set once OpAnnounceEnd has been processed. It gates
	// freeAnnounceScratch: an announce arriving after this is a stream
	// error (a strengthening over the old behavior, which silently accepted
	// a straggler announce with degraded duplicate detection once byPath
	// forgot nothing).
	announceClosed bool

	// verifiedDirs memoizes clean relative directory paths already proven
	// (or created) free of symlinked components this run, so
	// rejectSymlinkedAncestors stops re-lstat'ing the same ancestors once
	// per sibling entry. Guarded by mu like the maps above. Freed by
	// freeAnnounceScratch alongside keep -- every caller
	// (rejectSymlinkedAncestors, markVerifiedDir) already tolerates a nil
	// map by lazily recreating it, which only a stray post-close announce
	// could ever trigger.
	verifiedDirs map[string]struct{}

	// createdDirs memoizes clean relative directory paths already known to
	// exist on disk because THIS run's own os.MkdirAll succeeded for them
	// -- distinct from verifiedDirs, whose membership only means "confirmed
	// not a symlink (or confirmed absent)" and is set even for a directory
	// that turned out NOT to exist yet (rejectSymlinkedAncestors's lstat
	// walk). Every file in a directory of N siblings otherwise repeated the
	// same MkdirAll (itself a Stat before any Mkdir) for every one of the
	// N-1 after the first: this is what lets mkdirAllOnce skip it once the
	// first reference -- the directory's own KindDir record if the stream
	// is well-formed, or the first file needing it if not -- has already
	// created the path. Guarded by mu like verifiedDirs; freed alongside it.
	createdDirs map[string]struct{}

	// pendingDone holds FileIDs whose OpFileDone arrived BEFORE the announce
	// that registers them. That ordering is legitimate: announces are
	// batched (announceBatchSize), while a file whose source cannot be read
	// fails instantly and forceFileDone sends its OpFileDone straight away,
	// so the done can overtake the batch still buffered on the client. The
	// zero-byte-file path in startDirClient sidesteps this by waiting until
	// "every announce is now confirmed sent"; the failure path has no such
	// barrier. Dropping the early done (as this used to) left the file
	// permanently unfinalized and hung WaitAllFinalized until its timeout,
	// failing the whole tree. Guarded by mu like the maps above.
	pendingDone map[uint32]struct{}
	open        int
	openBudget  int
	announced   int

	// retiredCount and firstErr back allFinalized/FirstFinalizeErr in O(1),
	// replacing a full snapshot()-and-scan of the whole table on every one
	// of WaitAllFinalized's polls -- which, at tree scale, allocated a
	// pointer-dense slice of the whole table and locked every destFile in
	// it, repeatedly, for as long as the last file took to finish. Both are
	// updated only from retire, which runs exactly once per announced
	// entry (see its own comment), so retiredCount == announced is exactly
	// "every entry has reached its terminal state."
	retiredCount int
	firstErr     error

	// announceErrShown bounds how many per-entry "skipping this entry"
	// lines announce() logs (see its use below): a destination with, say, a
	// full disk can reject thousands of entries in a row, and logging every
	// one is thousands of synchronous terminal writes on the single
	// announce goroutine for what is, after the first few, the same
	// message repeated. atomic even though announce() only ever runs
	// single-threaded in practice (see its own doc comment) -- the cost of
	// making that not matter is one atomic add.
	announceErrShown atomic.Int32
}

// maxAnnounceErrLines caps individually-logged announce-time rejections
// per fileTable; see announceErrShown.
const maxAnnounceErrLines = 10

func newFileTable(root string, workers int, ow *OrderedWriter) *fileTable {
	openBudget := workers * 4
	if openBudget < 64 {
		openBudget = 64
	}
	ft := &fileTable{
		root:         root,
		ow:           ow,
		byID:         make(map[uint32]*destFile),
		keep:         newKeepSet(),
		verifiedDirs: make(map[string]struct{}),
		pendingDone:  make(map[uint32]struct{}),
		openBudget:   openBudget,
	}
	ft.cond = sync.NewCond(&ft.mu)
	return ft
}

// maxAnnounceFileID bounds a FileID's value as a sanity check against a
// corrupt or hostile announce stream. FileIDs are dense and sequential --
// assigned 0-based, one per walked entry, by the client's walk (see
// walk.go) -- so a legitimate tree of even 100M entries stays far below
// this. Keeps announcedBits's backing array at a fixed 16 MB no matter what
// a hostile peer claims, rather than letting one malformed FileID force an
// enormous allocation.
const maxAnnounceFileID = 1 << 27

// announcedSeen reports whether id has ever been announced (its bit is set
// in announcedBits) -- including one that has since retired and left byID.
// Caller must hold ft.mu.
func (ft *fileTable) announcedSeen(id uint32) bool {
	word := id / 64
	if int(word) >= len(ft.announcedBits) {
		return false
	}
	return ft.announcedBits[word]&(1<<(id%64)) != 0
}

// markAnnounced sets id's bit, growing the bitmap if needed. Caller must
// hold ft.mu. A bit, once set, is NEVER cleared -- that permanence is what
// lets byID safely drop a retired entry without lookup mistaking "retired"
// for "never announced" (see lookup).
func (ft *fileTable) markAnnounced(id uint32) {
	word := int(id / 64)
	if word >= len(ft.announcedBits) {
		// Double (at least), never grow to exact size: IDs arrive densely
		// ascending, so exact-size growth reallocated and copied the whole
		// bitmap every 64 entries -- quadratic in the file count, on the
		// single announce goroutine.
		newLen := 2 * len(ft.announcedBits)
		if newLen < word+1 {
			newLen = word + 1
		}
		grown := make([]uint64, newLen)
		copy(grown, ft.announcedBits)
		ft.announcedBits = grown
	}
	ft.announcedBits[word] |= 1 << (id % 64)
}

// retire is called once finalizeIO has actually returned for df (see
// runFinalizeIO), removing it from byID and rolling its outcome into the
// O(1) counters allFinalized/FirstFinalizeErr use instead of a full-table
// scan. Runs exactly once per destFile that ever entered byID: the
// tryFinalize/decide double latch (see its own comment) flips df.finalized
// under a single critical section shared by Release/MarkDone/reapStaleRefs,
// so exactly one of them ever proceeds into runFinalizeIO for a given df.
// A born-finalized entry (see announce) never enters byID at all and so
// never reaches retire either; its counters are updated inline in announce
// instead, under the same ft.mu section that would otherwise insert it.
func (ft *fileTable) retire(df *destFile) {
	df.mu.Lock()
	ferr := df.finalizeErr
	df.mu.Unlock()

	ft.mu.Lock()
	delete(ft.byID, df.id)
	ft.retiredCount++
	if ferr != nil && ft.firstErr == nil {
		ft.firstErr = fmt.Errorf("file %d (%s): %w", df.id, df.relPath, ferr)
	}
	ft.mu.Unlock()
}

// closeAnnounce marks the announce stream complete: no further call to
// announce may succeed after this. Called from the OpAnnounceEnd handler,
// unconditionally (whether or not -delete was requested), before
// freeAnnounceScratch discards the keep-set and verifiedDirs memo -- both
// built and consulted only during announce. Without this gate, a straggler
// announce arriving after OpAnnounceEnd could silently rebuild an empty
// keep-set missing every earlier entry, which a -delete pass using it
// would then treat as newly extraneous.
func (ft *fileTable) closeAnnounce() {
	ft.mu.Lock()
	ft.announceClosed = true
	ft.mu.Unlock()
}

// freeAnnounceScratch releases the announce-phase-only structures: the
// keep-set (duplicate-path detection, plus any -delete pass, which has
// already run by the time this is called) and the symlinked-ancestor
// memoization cache. Safe the instant the announce stream is known closed
// (closeAnnounce) and any -delete pass has returned: nothing after
// OpAnnounceEnd calls announce, rejectSymlinkedAncestors, or
// deleteExtraneous again for this tree. This turns their combined
// announce-phase peak into a transient confined to that phase rather than a
// steady-state cost for the rest of the transfer.
func (ft *fileTable) freeAnnounceScratch() {
	ft.mu.Lock()
	ft.keep = nil
	ft.verifiedDirs = nil
	ft.createdDirs = nil
	ft.mu.Unlock()
}

// mkdirAllOnce creates absDir (relDir's absolute form) via os.MkdirAll the
// first time relDir is seen this run, and is a no-op every time after --
// see createdDirs. relDir == "." (a top-level entry's parent, or the root's
// own KindDir if one is ever announced) is always treated as already
// created: the destination root is MkdirAll'd once before startDirServer
// starts accepting connections, so every announce sees it already in place.
func (ft *fileTable) mkdirAllOnce(relDir, absDir string) error {
	if relDir == "" || relDir == "." {
		return nil
	}
	ft.mu.Lock()
	if ft.createdDirs == nil {
		ft.createdDirs = make(map[string]struct{})
	}
	_, done := ft.createdDirs[relDir]
	ft.mu.Unlock()
	if done {
		return nil
	}
	if err := os.MkdirAll(absDir, 0777); err != nil {
		return err
	}
	ft.mu.Lock()
	ft.createdDirs[relDir] = struct{}{}
	ft.mu.Unlock()
	return nil
}

// Announce registers one announced entry: it sanitizes rec.Path (never
// trusting a path that arrived over the wire), resolves it against root,
// and — for a directory — creates it immediately (MkdirAll), so a later
// file announced under it can be opened without racing its own
// parent-directory creation. A regular file's parent directories are
// likewise created here; the file itself is opened lazily by Acquire.
// Announce registers rec with ft, matching the original (no quick-check)
// behaviour: announce is called with quickCheck=false, and its skip result
// is always false so is discarded here.
func (ft *fileTable) Announce(rec AnnounceRec) error {
	_, err := ft.announce(rec, false)
	return err
}

// announce is Announce's full implementation. When quickCheck is true and
// rec is a non-empty KindFile entry, it stats the destination path first: if
// a regular file already exists there with the same size and the same
// modification time (whole-second precision -- see the quick-check plan's
// rationale for not requiring nanosecond precision), the file is skipped
// entirely -- no fd is ever opened for it, and it is marked finalized
// immediately, the same way a KindDir/KindSkipped entry already is below.
// Zero-byte files are deliberately excluded from this check: they already
// have their own always-skip mechanism (startDirClient's empty-file loop),
// so there is nothing left for quick-check to add for them and rec.MTimeSec
// wouldn't be meaningfully comparable against a file quick-check never
// bothers to look at anyway.
func (ft *fileTable) announce(rec AnnounceRec, quickCheck bool) (skip bool, err error) {
	clean, sanErr := sanitizeRelPath(rec.Path)
	if sanErr != nil && !errors.Is(sanErr, errPathUnrepresentable) {
		// Structurally unsafe (traversal, absolute, NUL, ...): a hostile or
		// corrupt stream, so kill the connection.
		return false, fmt.Errorf("announce %d: unsafe path %q: %w", rec.FileID, rec.Path, sanErr)
	}
	abs := filepath.Join(ft.root, filepath.FromSlash(clean))
	if !isWithinRoot(ft.root, abs) {
		return false, fmt.Errorf("announce %d: path %q escapes the destination root", rec.FileID, clean)
	}

	// announceErr collects an ENVIRONMENTAL failure (a MkdirAll hitting
	// ENOSPC/ENOTDIR/EACCES on this one entry) instead of returning it: a
	// per-entry environment problem must degrade to a per-file failure,
	// not kill the connection announces arrive on -- which is the CONTROL
	// connection, whose death silently orphans the entire transfer (every
	// later OpFileDone hits a broken pipe and OpTreeDone has nowhere to
	// travel; observed in production when one full-disk mkdir cost a
	// multi-hour sync). The entry is still registered below -- keeping
	// AnnouncedCount consistent with the client's claimed total and the
	// path in the -delete keep-set -- but immediately finalized with
	// finalizeErr set, so Acquire rejects it (the client fails just that
	// file and moves on) and FirstFinalizeErr reports it at the end.
	//
	// Validation failures (unsafe/escaping path, an empty symlink target,
	// duplicate ID/path, unknown kind) still return errors and kill the
	// connection: those indicate a hostile or corrupt STREAM, not the
	// environment. Destination-side conflicts are deliberately NOT in that
	// set -- a stale symlink where the source now has a directory is
	// ordinary out-of-date state, and failing the connection over it used
	// to abort the entire tree (every later file failing "was never
	// announced") rather than just the one entry involved.
	var announceErr error

	// A structurally-safe name this filesystem cannot represent (a Windows
	// destination handed a Linux path like "example.net.") costs exactly one
	// entry: it is registered below so AnnouncedCount still matches the
	// client's total and the path stays in the -delete keep-set, but nothing
	// is created for it and Acquire will reject it. Skipping the switch is
	// what guarantees no MkdirAll/mknod/symlink ever runs on a name the OS
	// would mangle (Windows silently strips a trailing dot, which could
	// collide with a sibling).
	if sanErr != nil {
		announceErr = fmt.Errorf("announce %d: %q: %w", rec.FileID, clean, sanErr)
	}

	switch {
	case announceErr != nil:
		// Nothing to create; fall through to registration.
	case rec.Kind == KindDir:
		// Symlinked-ancestor mitigation: never write THROUGH a symlink
		// standing in for a real directory. Two distinct cases, and the
		// difference matters:
		//
		//   - abs itself is a stale link (the source turned a symlink into
		//     a real directory since the last run). That is ordinary
		//     out-of-date destination state, not an attack: the link is
		//     removed and the directory created in its place, so the sync
		//     converges. MkdirAll alone would silently succeed THROUGH the
		//     link, putting this directory's whole subtree outside the
		//     destination root.
		//
		//   - an ancestor further up the tree ("a" already exists as a link
		//     and "a/b" is announced under it) is rejectSymlinkedAncestors'
		//     job below. Because WalkDir is parent-first, a stale "a" is
		//     announced as its own KindDir record and repaired HERE before
		//     "a/b" is ever announced, so that check firing now means an
		//     anomalous stream rather than a merely stale destination.
		//
		// Still not fully TOCTOU-safe (that needs openat2(RESOLVE_BENEATH),
		// Linux-only); openNoFollow on the Acquire open is what actually
		// hardens the path where content gets written.
		// Ancestors FIRST, always: clearConflictingDest lstats (and may
		// remove) abs itself, and if an ancestor were a symlink out of the
		// tree, abs would resolve through it -- removing a file outside the
		// destination root entirely. Verifying the ancestors first means abs
		// is known to be a genuine path inside the root before anything
		// touches it.
		if err := ft.rejectSymlinkedAncestors(clean); err != nil {
			announceErr = fmt.Errorf("announce %d: %w", rec.FileID, err)
			break
		}
		if err := clearConflictingDest(abs, KindDir); err != nil {
			announceErr = fmt.Errorf("announce %d: %q: %w", rec.FileID, clean, err)
			break
		}
		if err := ft.mkdirAllOnce(clean, abs); err != nil {
			announceErr = fmt.Errorf("announce %d: mkdir %q: %w", rec.FileID, clean, err)
			break
		}
		ft.markVerifiedDir(clean)
	case rec.Kind == KindFile:
		if err := ft.rejectSymlinkedAncestors(clean); err != nil {
			announceErr = fmt.Errorf("announce %d: %w", rec.FileID, err)
			break
		}
		if err := ft.mkdirAllOnce(path.Dir(clean), filepath.Dir(abs)); err != nil {
			announceErr = fmt.Errorf("announce %d: mkdir parent of %q: %w", rec.FileID, clean, err)
			break
		}
		// Drop a stale symlink sitting at the file's own path BEFORE the
		// quick-check stat below, so quick-check can never match against
		// the link's TARGET (same size and mtime) and skip the file,
		// leaving the link in place forever.
		if err := clearConflictingDest(abs, KindFile); err != nil {
			announceErr = fmt.Errorf("announce %d: %q: %w", rec.FileID, clean, err)
			break
		}
		if announceErr == nil && quickCheck && rec.FileSize > 0 {
			if fi, statErr := os.Stat(abs); statErr == nil && fi.Mode().IsRegular() &&
				uint64(fi.Size()) == rec.FileSize && fi.ModTime().Unix() == rec.MTimeSec {
				skip = true
				// rsync's quick-check still corrects a permission mismatch
				// on an otherwise-skipped file rather than leaving it
				// stale; chmod is idempotent and cheap, so no error here is
				// worth failing the whole skip decision over.
				if fi.Mode().Perm() != os.FileMode(rec.Mode).Perm() {
					os.Chmod(abs, os.FileMode(rec.Mode).Perm())
				}
			}
		}
	case rec.Kind == KindSymlink:
		// -links: recreate the source symlink here, at announce time, with
		// the identical (verbatim) target string -- like a KindDir, there is
		// no block traffic for it. The target's CONTENT is deliberately not
		// validated (rsync semantics: relative stays relative, absolute
		// stays absolute, dangling stays dangling): this server never
		// follows it, and a later entry announced BENEATH a symlink --
		// including one this very transfer just created -- is rejected by
		// rejectSymlinkedAncestors, so a link can never become a write
		// path out of the root. An empty target, though, is a corrupt
		// record: the walker never emits one, so reject the stream.
		if rec.LinkTarget == "" {
			return false, fmt.Errorf("announce %d: symlink %q has an empty target", rec.FileID, clean)
		}
		if err := ft.rejectSymlinkedAncestors(clean); err != nil {
			announceErr = fmt.Errorf("announce %d: %w", rec.FileID, err)
			break
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0777); err != nil {
			announceErr = fmt.Errorf("announce %d: mkdir parent of %q: %w", rec.FileID, clean, err)
			break
		}
		if err := createSymlinkAt(abs, rec.LinkTarget); err != nil {
			announceErr = fmt.Errorf("announce %d: symlink %q: %w", rec.FileID, clean, err)
		}
	case rec.Kind == KindSpecial:
		// -specials: recreate the FIFO or socket here, at announce time,
		// with no block traffic -- like a KindDir or KindSymlink. The node
		// type rides in rec.Mode (the walker only ever emits the two types
		// isSpecialMode accepts), so a corrupt or hostile Mode simply makes
		// makeSpecialNode refuse, which is this one entry's failure.
		if err := ft.rejectSymlinkedAncestors(clean); err != nil {
			announceErr = fmt.Errorf("announce %d: %w", rec.FileID, err)
			break
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0777); err != nil {
			announceErr = fmt.Errorf("announce %d: mkdir parent of %q: %w", rec.FileID, clean, err)
			break
		}
		if err := createSpecialAt(abs, os.FileMode(rec.Mode)); err != nil {
			announceErr = fmt.Errorf("announce %d: special %q: %w", rec.FileID, clean, err)
		}
	case rec.Kind == KindSkipped:
		// Recorded below for a later --delete pass's keep-set, but nothing
		// is created on disk for it, so there is no MkdirAll/open to
		// mitigate here.
	default:
		return false, fmt.Errorf("announce %d: unknown entry kind %d", rec.FileID, rec.Kind)
	}

	df := &destFile{
		id:         rec.FileID,
		relPath:    clean,
		size:       rec.FileSize,
		mode:       os.FileMode(rec.Mode),
		mtime:      time.Unix(rec.MTimeSec, int64(rec.MTimeNs)),
		kind:       rec.Kind,
		lastActive: time.Now(),
		// pendingBlocks is allocated lazily, on first use, by
		// handleBlockCheck -- not here. Every directory, symlink, special
		// file, skipped placeholder, and quick-check-skipped file (nearly
		// the whole tree on a mostly-unchanged re-sync) never has a block
		// checked against it, so allocating this map unconditionally cost a
		// map header on every one of those entries for nothing.
	}
	// A directory or skipped entry has nothing further to transfer, so
	// treat it as immediately finalized: it must not block WaitAllFinalized
	// or be reachable via Acquire for block traffic. There is no finalizeIO
	// to run for it (no fd was ever opened), so finalizeComplete is set
	// alongside finalized rather than left for a finalizeIO call that will
	// never happen. A quick-check-skipped KindFile entry gets the exact
	// same treatment for the exact same reason: its content is already
	// correct, so there is nothing left for Acquire/block traffic/finalizeIO
	// to do for it either. An entry whose mkdir failed (announceErr) gets
	// the same shape too, plus finalizeErr: nothing can be transferred for
	// it, Acquire must reject it, and the failure must surface via
	// FirstFinalizeErr rather than hang WaitAllFinalized.
	if rec.Kind != KindFile || skip || announceErr != nil {
		df.finalized = true
		df.finalizeComplete = true
	}
	if announceErr != nil {
		df.finalizeErr = announceErr
		if ft.announceErrShown.Add(1) <= maxAnnounceErrLines {
			Log("directory server: %v -- skipping this entry, transfer continues\n", announceErr)
		}
	}

	ft.mu.Lock()
	if ft.announceClosed {
		ft.mu.Unlock()
		return false, fmt.Errorf("announce %d: announce stream already closed by OpAnnounceEnd", rec.FileID)
	}
	if rec.FileID >= maxAnnounceFileID {
		ft.mu.Unlock()
		return false, fmt.Errorf("announce %d: FileID exceeds the maximum of %d", rec.FileID, maxAnnounceFileID-1)
	}
	if ft.announcedSeen(rec.FileID) {
		ft.mu.Unlock()
		return false, fmt.Errorf("announce %d: file ID already announced", rec.FileID)
	}
	if ft.keep.add(clean, rec.Kind) {
		ft.mu.Unlock()
		return false, fmt.Errorf("announce %d: path %q was already announced", rec.FileID, clean)
	}
	ft.markAnnounced(rec.FileID)
	ft.announced++
	if df.finalizeComplete {
		// Born finalized (a dir, symlink, special, skipped placeholder, a
		// quick-check skip, or an announce-time failure): nothing will ever
		// Acquire it, send it an OpFileDone, or need its destFile object
		// again, so retire it inline instead of publishing it to byID at
		// all and immediately removing it again. df is still a private,
		// unpublished value here (nothing else can reach it yet), so
		// reading its fields needs no df.mu -- unlike retire, which is used
		// for the OTHER path, where a live df is genuinely shared.
		ft.retiredCount++
		if df.finalizeErr != nil && ft.firstErr == nil {
			ft.firstErr = fmt.Errorf("file %d (%s): %w", df.id, df.relPath, df.finalizeErr)
		}
	} else {
		ft.byID[rec.FileID] = df
	}
	_, doneEarly := ft.pendingDone[rec.FileID]
	if doneEarly {
		delete(ft.pendingDone, rec.FileID)
	}
	ft.mu.Unlock()

	// Replay an OpFileDone that beat this announce. Harmless for an entry
	// already finalized above (a directory, a skipped placeholder, a
	// quick-check skip, or one whose announce failed): MarkDone's
	// shouldFinalize sees df.finalized and does nothing.
	if doneEarly {
		if err := ft.MarkDone(rec.FileID); err != nil {
			Log("directory server: replaying early file-done for %d: %v\n", rec.FileID, err)
		}
	}
	return skip, nil
}

// clearConflictingDest removes a destination SYMLINK standing where this run
// needs a real file (KindFile) or directory (KindDir) -- the mirror image of
// createSymlinkAt, which replaces a file/link standing where a symlink is
// wanted. The path is source-managed now, so the stale link is removed rather
// than written through; rsync replaces a symlink with a file or directory the
// same way.
//
// This is the announce-time half of the write-through defense. It matters
// even when the current run has no -links: a link an EARLIER -links run
// created is already on disk, and following it would take the transfer
// outside the destination root entirely (openNoFollow closes the remaining
// TOCTOU window at the open itself).
//
// Anything that is NOT a symlink is left completely alone -- an existing
// regular file is the ordinary resume/update case, and an existing directory
// where a file is wanted is reported so the caller can fail just that entry
// rather than silently destroying a tree.
func clearConflictingDest(abs string, want EntryKind) error {
	fi, statErr := os.Lstat(abs)
	if statErr != nil {
		return nil // missing (the common case), or unreadable -- the caller's own operation reports it
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("remove stale symlink: %w", err)
		}
		return nil
	}
	if want == KindFile && fi.IsDir() {
		return fmt.Errorf("a directory occupies the file's path")
	}
	return nil
}

// createSymlinkAt idempotently makes abs a symlink pointing at target:
// a missing path is created; an existing symlink with the SAME target is a
// no-op (the idempotent-rerun case); an existing symlink with a different
// target, or an existing regular file, is removed and replaced (the path
// is source-managed now -- rsync does the same). An existing DIRECTORY is
// refused: silently destroying a directory tree to plant a link is never
// the right call (rsync refuses too); the caller treats that as this one
// entry's environmental failure.
func createSymlinkAt(abs, target string) error {
	fi, statErr := os.Lstat(abs)
	switch {
	case os.IsNotExist(statErr):
		// Nothing there: fall through to create.
	case statErr != nil:
		return fmt.Errorf("lstat: %w", statErr)
	case fi.Mode()&os.ModeSymlink != 0:
		existing, readErr := os.Readlink(abs)
		if readErr == nil && existing == target {
			return nil // already correct
		}
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("remove old symlink: %w", err)
		}
	case fi.IsDir():
		return fmt.Errorf("a directory occupies the symlink's path")
	default:
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("remove old file: %w", err)
		}
	}
	return os.Symlink(target, abs)
}

// createSpecialAt idempotently makes abs a FIFO or socket matching mode, the
// -specials counterpart to createSymlinkAt: a node of the RIGHT type already
// there is a no-op (the idempotent-rerun case); a symlink, a regular file, or
// a node of the wrong type is removed and replaced (the path is
// source-managed now); a DIRECTORY is refused, exactly as createSymlinkAt
// refuses one, because silently destroying a tree to plant a node is never
// the right call. The caller treats a refusal as this entry's environmental
// failure.
func createSpecialAt(abs string, mode os.FileMode) error {
	if !isSpecialMode(mode) {
		return fmt.Errorf("not a syncable special type (%v)", mode.Type())
	}
	fi, statErr := os.Lstat(abs)
	switch {
	case os.IsNotExist(statErr):
		// Nothing there: fall through to create.
	case statErr != nil:
		return fmt.Errorf("lstat: %w", statErr)
	case fi.IsDir():
		return fmt.Errorf("a directory occupies the special file's path")
	case fi.Mode().Type() == mode.Type():
		// Already the right type of node; leave it alone. Recreating it
		// would be pointless churn, and for a FIFO would break any reader
		// or writer currently holding it open.
		return nil
	default:
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("remove conflicting %v: %w", fi.Mode().Type(), err)
		}
	}
	return makeSpecialNode(abs, mode)
}

// AnnouncedCount returns the number of records announced so far.
func (ft *fileTable) AnnouncedCount() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.announced
}

// lookup returns the destFile for id if it is still live (announced and not
// yet retired). A miss now has two genuinely different causes, which
// announcedBits is what makes distinguishable now that byID no longer
// retains a retired entry forever:
//
//   - id was announced and has since fully retired (see retire): permanent,
//     never becoming true again. Reported as errAlreadyFinalized, the same
//     sentinel Acquire already returns for a live-but-finalized entry
//     (dirHandleConn maps both to the identical FlagErrPermanent OpError,
//     so a caller cannot tell them apart on the wire, and does not need to).
//   - id has genuinely never been announced: transient, and expected to
//     resolve itself once a still-buffering announce batch lands (see
//     MarkDone's pendingDone path, which this case still feeds).
func (ft *fileTable) lookup(id uint32) (*destFile, error) {
	ft.mu.Lock()
	df, ok := ft.byID[id]
	announced := ft.announcedSeen(id)
	ft.mu.Unlock()
	if ok {
		return df, nil
	}
	if announced {
		return nil, fmt.Errorf("file %d: %w", id, errAlreadyFinalized)
	}
	return nil, fmt.Errorf("file %d was never announced", id)
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
		return nil, false, fmt.Errorf("file %d: %w", id, errAlreadyFinalized)
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
		return nil, false, fmt.Errorf("file %d: %w", id, errAlreadyFinalized)
	}
	if df.file == nil {
		abs := ft.abs(df)
		// Lstat, not Stat: a symlink at the destination path must be seen
		// as itself, never as whatever it points at -- otherwise the chmod
		// below would widen the permissions of the link's TARGET, outside
		// the destination root. announce removes such a link before we get
		// here; openNoFollow covers one planted in between.
		existing, statErr := os.Lstat(abs)
		created = os.IsNotExist(statErr)
		if created {
			df.createdFresh = true // already under df.mu; see the field's doc
		}
		if statErr == nil && existing.Mode().IsRegular() && existing.Mode().Perm()&0200 == 0 {
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
			os.Chmod(abs, existing.Mode().Perm()|0200)
		}
		// 0666&^umask, not df.mode: the final permission (which may be
		// read-only) is applied at finalize, after every write; creating
		// with it up front could lock the transfer out of its own file.
		f, openErr := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|openNoFollow, 0666)
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
		// Released here, atomically with the finalized flip, rather than
		// waiting for finalizeIO to close it: the handle is dead to any
		// OTHER caller the instant finalization is decided (the invariant
		// finalizeIO's own doc comment already states -- nothing may touch
		// f after df.finalized flips), and retaining it anyway cost ~112
		// bytes of live *os.File per transferred file for the rest of the
		// run. A stale df.handle()/negotiateBlockSize caller now fails its
		// read/write against a nil receiver (os.File's nil-receiver methods
		// return ErrInvalid, never panic) instead of racing finalizeIO's own
		// close of the SAME fd it captured into f just above.
		df.file = nil
		df.pendingBlocks = nil
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
// maxPendingDone bounds pendingDone so a peer sending a flood of OpFileDone
// frames for IDs it never announces cannot grow the map without limit. Far
// above any real in-flight window: only files whose done overtakes their own
// announce batch land here, so in practice this holds a handful of entries.
const maxPendingDone = 1 << 16

func (ft *fileTable) MarkDone(id uint32) error {
	df, err := ft.lookup(id)
	if err != nil {
		if errors.Is(err, errAlreadyFinalized) {
			// Already retired (see lookup): this done is moot -- a
			// duplicate, or one that simply lost the race with an entry
			// that was already finalized for some other reason (a
			// quick-check skip, an announce-time failure, ...). Nothing to
			// finalize and nothing to remember; storing it in pendingDone
			// would just leak forever, since the announce that would ever
			// clear it has already happened.
			return nil
		}
		// Not announced YET. Remember the done and replay it when the
		// announce lands (see the tail of announce); returning the error
		// here would drop it, and nothing would ever finalize the file.
		ft.mu.Lock()
		if len(ft.pendingDone) < maxPendingDone {
			if ft.pendingDone == nil {
				ft.pendingDone = make(map[uint32]struct{})
			}
			ft.pendingDone[id] = struct{}{}
			ft.mu.Unlock()
			return nil
		}
		ft.mu.Unlock()
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
	ft.retire(df)
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
	abs := ft.abs(df)
	if f == nil {
		// Never opened. Two very different reasons land here, and only one
		// of them may create a file:
		//
		//   - A genuinely EMPTY source file (size 0). Its only block is
		//     implicit, so no Acquire ever happened; the destination still
		//     needs the empty file to exist, so create it below.
		//
		//   - A NON-EMPTY file that was announced but never acquired,
		//     because its transfer failed before a single block moved (the
		//     source file could not be opened or read). Creating it here
		//     would fabricate a file of the announced size filled entirely
		//     with ZEROS -- right name, right size, right mode, and content
		//     that never existed. Worse, when the destination already holds
		//     a good copy from an earlier run, the truncate below would
		//     destroy it whenever the size changed. Nothing was transferred,
		//     so nothing here is known to be correct: leave the destination
		//     exactly as it was and report the failure instead.
		if df.size > 0 {
			err := fmt.Errorf("no data was ever transferred for %s; destination left untouched", df.relPath)
			ft.recordFinalizeErr(df, err)
			return err
		}
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
		f, createErr = os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0666)
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
		err = os.Chmod(abs, df.mode.Perm())
	}
	if err == nil {
		err = os.Chtimes(abs, df.mtime, df.mtime)
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

// snapshot returns every currently LIVE destFile (announced, not yet
// retired -- see byID's own comment), as a plain slice safe to range over
// without holding ft.mu (the lock is only needed to safely copy the map's
// values out; each destFile then has its own mu for whatever the caller
// does with it next). Used only by reapStaleRefs: allFinalized and
// FirstFinalizeErr no longer need a scan at all (see their own comments),
// so this is naturally bounded by files genuinely in flight rather than the
// whole tree, without needing any change here.
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

// finalizePollPeriod is how often WaitAllFinalized re-checks allFinalized.
// Deliberately much shorter than staleRefCheckPeriod: OpTreeDone (control
// connection) routinely races ahead of a worker connection's own teardown
// (its OpFileClose/Release is fire-and-forget on a DIFFERENT connection),
// so the last file's finalize typically lands milliseconds after the first
// allFinalized check fails -- polling at the reap cadence instead used to
// add up to a full staleRefCheckPeriod (2s) of pure tail latency to every
// transfer that lost that race. Cheap to poll this fast: allFinalized is an
// O(1) counter comparison (see its own comment), not a table scan, and this
// loop runs exactly once per tree, at the very end of it.
const finalizePollPeriod = 10 * time.Millisecond

// WaitAllFinalized blocks until every announced regular file has been
// finalized, periodically reaping stale refs so a single vanished
// connection can't wedge this forever. Returns ctx.Err() if ctx is done
// first.
func (ft *fileTable) WaitAllFinalized(ctx context.Context) error {
	poll := time.NewTicker(finalizePollPeriod)
	defer poll.Stop()
	lastReap := time.Now()
	for {
		if ft.allFinalized() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			// Reaping keeps its own, much slower cadence: it force-releases
			// refs that have been idle for staleRefAge, and running it on
			// every fast poll would just take ft.mu/df.mu more often for no
			// benefit.
			if time.Since(lastReap) >= staleRefCheckPeriod {
				ft.reapStaleRefs()
				lastReap = time.Now()
			}
		}
	}
}

// allFinalized reports whether every announced entry has actually completed
// finalizeIO -- an O(1) counter comparison, not a table scan. (Named
// without the "Locked" suffix Go convention reserves for "caller must hold
// the lock" — this takes and releases ft.mu itself and holds no lock on
// return.)
//
// retiredCount, not a scan of "finalized" flags: retiredCount is
// incremented ONLY from retire (called from runFinalizeIO, strictly AFTER
// finalizeIO's drain/truncate/close/chmod/chtimes has actually returned)
// and from announce's born-finalized path (which never had any I/O to run
// in the first place). So retiredCount == announced means exactly what the
// old scan checked -- every entry's finalizeComplete, not merely
// finalized, is true — without ever allocating or walking a
// snapshot of the table. See the destFile field comments for why that
// distinction (finalizeComplete vs. the earlier finalized) matters: it is
// what lets WaitAllFinalized's return safely trigger shutting down the
// shared OrderedWriter.
func (ft *fileTable) allFinalized() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.retiredCount == ft.announced
}

// FirstFinalizeErr returns the first per-file finalize error recorded
// across the whole table, if any, for surfacing as the transfer's overall
// result. O(1): firstErr is set at most once, by whichever of retire's
// (possibly concurrent) callers is first to see a non-nil finalizeErr —
// see retire and announce's born-finalized path, the only two writers.
func (ft *fileTable) FirstFinalizeErr() error {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.firstErr
}

// deleteExtraneous is the `-delete` pass: it removes every destination-side
// entry not among the files/directories/skipped-placeholders announced this
// run. Called only once, from the OpAnnounceEnd handler -- the announce
// stream is complete (and cross-checked against the client's claimed entry
// count) but no file content has been transferred yet, so a full destination
// is cleared of stale data BEFORE new data needs the space, matching rsync's
// --delete-before.
//
// A KindDir entry is safe to recurse into: its children are independently
// checked against the same set. Every other kind (KindFile, KindSymlink,
// KindSpecial, KindSkipped) is a protected LEAF -- present in the set, so never removed, but also never
// recursed into even if it happens to be a directory on the destination.
// This is what keeps an excluded directory's untouched contents untouched
// (only its own placeholder path is in the set, none of what's under it),
// and what stops a KindSkipped symlink/special-file placeholder from being
// treated as a real, walkable directory.
//
// Per-entry removal (or stat) failures are logged and skipped -- one
// unremovable file must not abort the rest of the sweep. Only a failure of
// the walk itself (root unreadable) is returned; the caller treats that the
// same as a WaitAllFinalized failure.
func (ft *fileTable) deleteExtraneous() (removed int, err error) {
	// Grabbed once, under lock: deleteExtraneous runs from the single
	// control-connection goroutine handling OpAnnounceEnd, strictly after
	// closeAnnounce (see the handler), so nothing else can still be
	// writing to keep by the time this reads it -- no per-entry locking
	// needed for the walk below. (This replaces a real data race the old
	// implementation had: it read len(ft.byPath) one line before taking
	// ft.mu.)
	ft.mu.Lock()
	keep := ft.keep
	ft.mu.Unlock()
	if keep == nil {
		return 0, fmt.Errorf("delete pass: keep-set unavailable (announce not yet closed)")
	}

	// deleteExtraneous runs single-threaded on the control-connection
	// goroutine (see the locking note above), so this bounded-tail counter
	// needs no lock, unlike skipLogger's concurrent version: log the first
	// maxSkipLinesPerReason removals/failures of each kind individually,
	// then go quiet and let the returned count (and this summary line)
	// stand in for the rest -- a `-delete` sweep over a large stale
	// destination is otherwise millions of synchronous terminal writes.
	shownRemoved, shownFailed := 0, 0

	walkErr := filepath.WalkDir(ft.root, func(path string, d fs.DirEntry, statErr error) error {
		if path == ft.root {
			return statErr
		}
		if statErr != nil {
			if shownFailed < maxSkipLinesPerReason {
				shownFailed++
				Log("delete: skipping %s: %v\n", path, statErr)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(ft.root, path)
		if relErr != nil {
			return nil // unreachable: WalkDir guarantees path is under ft.root
		}
		rel = filepath.ToSlash(rel)

		if isDir, kept := keep.contains(rel); kept {
			if d.IsDir() && isDir {
				return nil // a real synced directory: recurse, check its children
			}
			if d.IsDir() {
				return fs.SkipDir // kept but not managed (e.g. excluded) -- leave its contents alone
			}
			return nil
		}

		// Not announced this run: stale.
		if d.IsDir() {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				if shownFailed < maxSkipLinesPerReason {
					shownFailed++
					Log("delete: failed to remove %s/: %v\n", rel, rmErr)
				}
				return fs.SkipDir
			}
			if shownRemoved < maxSkipLinesPerReason {
				shownRemoved++
				Log("delete: removed %s/\n", rel)
			}
			removed++
			return fs.SkipDir
		}
		if rmErr := os.Remove(path); rmErr != nil {
			if shownFailed < maxSkipLinesPerReason {
				shownFailed++
				Log("delete: failed to remove %s: %v\n", rel, rmErr)
			}
			return nil
		}
		if shownRemoved < maxSkipLinesPerReason {
			shownRemoved++
			Log("delete: removed %s\n", rel)
		}
		removed++
		return nil
	})
	if removed > shownRemoved {
		Log("delete: ... and %d more removed (%d total)\n", removed-shownRemoved, removed)
	}
	return removed, walkErr
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
			// Closed unconditionally, before any -delete pass below: no
			// OpAnnounce may succeed after this point regardless of
			// whether -delete was requested, and closing first is what
			// makes freeAnnounceScratch's release of the keep-set and
			// verifiedDirs (below) safe.
			st.ft.closeAnnounce()
			if shown := st.ft.announceErrShown.Load(); shown > maxAnnounceErrLines {
				Log("directory server: ... and %d more announce-time rejections\n", shown-maxAnnounceErrLines)
			}
			if msg.Flags&FlagDelete != 0 {
				// The `-delete` pass runs HERE -- announce stream complete,
				// no file content transferred yet (the client holds its
				// TreeReader until the ack below) -- so a full destination
				// is cleared of stale data BEFORE new data needs the space
				// (rsync's --delete-before).
				//
				// Safety gate first: a truncated announce stream must never
				// trigger deletion -- every path the server failed to hear
				// about would look extraneous and be wiped. The client's
				// claimed entry count rides in BlockIdx, exactly as
				// handleTreeDone re-validates it at the end.
				if announced := uint32(st.ft.AnnouncedCount()); announced != msg.BlockIdx {
					err := fmt.Errorf("delete pass refused: client claims %d announced entries, server saw %d", msg.BlockIdx, announced)
					sendDirError(conn, magicBytes, err)
					return err
				}
				removed, delErr := st.ft.deleteExtraneous()
				if delErr != nil {
					sendDirError(conn, magicBytes, delErr)
					return fmt.Errorf("delete pass: %w", delErr)
				}
				if removed > 0 {
					Log("directory server: delete pass removed %d extraneous entr(y/ies)\n", removed)
				}
				ack := &Msg{MagicHead: magicBytes, Op: uint8(OpAnnounceEndAck), BlockIdx: uint32(removed)}
				if err := sendMsg(conn, ack); err != nil {
					return err
				}
			}
			// The announce-phase scratch structures have done their job:
			// the keep-set answered every duplicate-path check during
			// announce and (if requested) the delete pass just above;
			// verifiedDirs will never be consulted again either. Freeing
			// them here confines their combined peak to the announce phase
			// instead of letting it ride out the rest of the transfer.
			st.ft.freeAnnounceScratch()

		case OpFileOpen:
			if sess.bound && sess.fileID != msg.FileID {
				releaseBinding()
			}
			df, created, err := st.ft.Acquire(msg.FileID)
			if err != nil {
				// FlagErrPermanent only for the finalized case: that state
				// can never change, so the client should fail the file
				// immediately. Every other rejection -- above all "never
				// announced", which the pipeline produces TRANSIENTLY when a
				// worker's open races ahead of a still-buffering announce
				// batch -- must stay retryable, or that ordinary race turns
				// into a spurious permanent file failure.
				var errFlags uint8
				if errors.Is(err, errAlreadyFinalized) {
					errFlags = FlagErrPermanent
				}
				sendDirErrorFlags(conn, magicBytes, err, errFlags)
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
	sendDirErrorFlags(conn, magicBytes, cause, 0)
}

// sendDirErrorFlags is sendDirError with an explicit Flags value on the
// OpError frame -- FlagErrPermanent when the rejection is deterministic and
// retrying is pointless (see errAlreadyFinalized), 0 otherwise.
func sendDirErrorFlags(conn net.Conn, magicBytes [magicLen]byte, cause error, flags uint8) {
	text := cause.Error()
	if len(text) > 4096 {
		text = text[:4096]
	}
	hdr := &Msg{MagicHead: magicBytes, Op: uint8(OpError), Flags: flags, DataSize: uint32(len(text))}
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
	quickCheck := msg.Flags&FlagQuickCheck != 0
	var skipped []uint32
	for _, rec := range recs {
		skip, err := ft.announce(rec, quickCheck)
		if err != nil {
			return err
		}
		if skip {
			skipped = append(skipped, rec.FileID)
		}
	}
	if !quickCheck {
		return nil
	}
	// One OpAnnounceAck per OpAnnounce that requested quick-check, always
	// sent (even with an empty skip list): the client blocks on exactly one
	// reply per batch, so a batch with nothing to skip still needs one to
	// unblock it.
	ackTrailer, err := encodeSkipList(skipped)
	if err != nil {
		return fmt.Errorf("encode announce ack: %w", err)
	}
	magicBytes := stringToFixedSizeArray(magicHead)
	ack := &Msg{MagicHead: magicBytes, Op: uint8(OpAnnounceAck), DataSize: uint32(len(ackTrailer))}
	data, err := pack(ack)
	if err != nil {
		return fmt.Errorf("pack announce ack: %w", err)
	}
	if err := connWrite(conn, data); err != nil {
		return fmt.Errorf("send announce ack: %w", err)
	}
	if len(ackTrailer) > 0 {
		if err := connWrite(conn, ackTrailer); err != nil {
			return fmt.Errorf("send announce ack trailer: %w", err)
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
	hashHdr, _ := pack(hashMsg)
	// Header and digest go out as ONE write (connWriteFrame): 64 bytes in a
	// single syscall and segment, instead of a 48-byte and a 16-byte write.
	if err := connWriteFrame(conn, hashHdr, hash); err != nil {
		// A write failure here (e.g. a dropped connection) self-heals: the
		// caller's next read will hit the connection's own read deadline
		// and the client redials, exactly as any other connection loss
		// does. But silently swallowing it (bare "return nil", no log)
		// previously converted a clean, immediately-diagnosable error into
		// an up-to-5-minute silent stall from the operator's point of view.
		Log("directory server: send hash for file %d block %d: %v\n", msg.FileID, msg.BlockIdx, err)
		return nil
	}

	df.mu.Lock()
	if df.pendingBlocks == nil {
		df.pendingBlocks = make(map[uint32]struct{})
	}
	df.pendingBlocks[msg.BlockIdx] = struct{}{}
	df.mu.Unlock()
	return nil
}

// takePendingOrFresh validates and clears an outstanding checksum request
// for (fileID, blockIdx), the per-file analogue of the legacy
// single-connection pendingWrite guard: a write is accepted if it matches a
// checksum request this file has actually seen, keyed by block index so
// several workers can be mid-flight on different blocks of the same file at
// once. A file this transfer's own Acquire created fresh (createdFresh) is
// the one exception: the client's fast path deliberately skips the
// checksum round trip for it (there is nothing on the destination to
// compare against -- the file was empty at creation), so its writes arrive
// without a pending entry and are accepted as-is. The guard stays fully
// intact for any pre-existing file, whose prior content an unsolicited
// write could actually corrupt; a created-fresh file's entire content is
// owned by this same transfer. Bounds and payload-size validation are the
// callers' separate checks and run either way.
func takePendingOrFresh(df *destFile, blockIdx uint32) bool {
	df.mu.Lock()
	defer df.mu.Unlock()
	if _, ok := df.pendingBlocks[blockIdx]; ok {
		delete(df.pendingBlocks, blockIdx)
		return true
	}
	return df.createdFresh
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
	if !takePendingOrFresh(df, msg.BlockIdx) {
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
	if !takePendingOrFresh(df, msg.BlockIdx) {
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
	// Read the wire payload into the session's reusable block buffer (the
	// same one handleBlockCheck hashes from) instead of a fresh allocation
	// per block. It stays valid for as long as it is needed: writeAndAck ->
	// SubmitAndWaitFile blocks until the bytes are on disk, and nothing
	// touches sess.blockBuf again before the next frame is read.
	if cap(sess.blockBuf) < int(msg.DataSize) {
		sess.blockBuf = make([]byte, msg.DataSize)
	}
	raw := sess.blockBuf[:msg.DataSize]
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
