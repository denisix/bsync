package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EntryKind classifies a walked path for the wire's AnnounceRec.Kind field
// (added in a later phase): a regular file whose blocks get synced, a
// directory reproduced via MkdirAll, or something skipped outright.
type EntryKind uint8

const (
	KindFile EntryKind = iota
	KindDir
	KindSkipped
	// KindSymlink is a symlink synced as a symlink (-links): recreated at
	// the destination with the identical target string at announce time,
	// like a KindDir -- no block traffic. Without -links a symlink is a
	// KindSkipped placeholder instead, exactly as before this kind existed.
	KindSymlink
	// KindSpecial is a FIFO (named pipe) or a unix socket synced as itself
	// (-specials, rsync's --specials): recreated at the destination by
	// mkfifo/mknod at announce time, like a KindDir -- no block traffic.
	// The node TYPE travels in the record's existing Mode field, so no wire
	// layout change was needed for it. Device nodes are deliberately NOT
	// included: they additionally need a major/minor pair on the wire and
	// CAP_MKNOD on the destination (that is rsync's separate --devices).
	// Without -specials a FIFO or socket stays a KindSkipped placeholder,
	// exactly as before this kind existed.
	KindSpecial
)

// isSpecialMode reports whether mode is one of the types -specials syncs: a
// FIFO or a unix socket. Deliberately narrow -- every other irregular type
// (device, char device, and anything else) stays a KindSkipped placeholder.
func isSpecialMode(mode os.FileMode) bool {
	return mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0
}

// FileEntry describes one path discovered by walkSource, in walk order.
type FileEntry struct {
	ID      uint32 // dense, 0-based, assigned in emission order
	RelPath string // '/'-separated, sanitized, relative to the walk root
	// AbsPath is deliberately NOT stored here (it once was): it is always
	// filepath.Join(root, filepath.FromSlash(RelPath)), which TreeReader now
	// recomputes on demand when it actually opens a file. Retaining it
	// meant every walked entry -- not just the handful open at any instant
	// -- carried a second, separately allocated copy of its own path for
	// the life of the transfer.
	Size    uint64
	Mode    os.FileMode
	ModTime time.Time
	Kind    EntryKind

	BlockSize uint32 // per-file block size (treeBlockSize); 0 for dirs
	Blocks    uint32 // number of blocks; 0 for dirs and empty files

	// LinkTarget is a KindSymlink entry's target string, verbatim from
	// os.Readlink (relative stays relative, absolute stays absolute, a
	// dangling target is carried as-is). Empty for every other kind.
	LinkTarget string
}

const (
	// minTreeBlock is the smallest block size treeBlockSize will pick for a
	// non-empty file, before growing toward the cap.
	minTreeBlock = 64 << 10 // 64KB

	// maxRelPathLen and maxPathComponentLen bound a sanitized relative path
	// and a single path component within it.
	maxRelPathLen       = 4096
	maxPathComponentLen = 255
)

// treeBlockSize returns the block size to use for a file of the given size,
// capped at capBS. In directory mode -b is a CEILING, not a fixed size:
// syncing a tree of many small files at (say) the 10MB single-file default
// would cost a full block-size read, allocation, and compression pass per
// file for no benefit — latency-bound work, not bandwidth-bound. A small
// file instead gets a proportionally small block size, growing in powers of
// two up to the cap. capBS == 0 falls back to the package default (the -b
// value single-file mode already uses).
func treeBlockSize(size uint64, capBS uint32) uint32 {
	if capBS == 0 {
		capBS = blockSize
	}
	if size == 0 {
		if uint64(minTreeBlock) > uint64(capBS) {
			return capBS
		}
		return minTreeBlock
	}
	if size >= uint64(capBS) {
		return capBS
	}
	bs := uint64(minTreeBlock)
	for bs < size && bs < uint64(capBS) {
		bs <<= 1
	}
	if bs > uint64(capBS) {
		bs = uint64(capBS)
	}
	return uint32(bs)
}

// windowsReservedNames are Windows device names that are reserved
// regardless of extension or case (e.g. "CON.txt" is still reserved).
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isWindowsReservedName reports whether name (a single path component) is a
// Windows reserved device name, ignoring any extension and case.
func isWindowsReservedName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return windowsReservedNames[strings.ToUpper(base)]
}

// errPathUnrepresentable marks a path that is structurally safe but cannot
// exist on THIS machine's filesystem -- today only the Windows-portability
// cases. It is deliberately distinguishable from every other sanitizeRelPath
// failure: a traversal or absolute path means a hostile or corrupt stream and
// must kill the connection, whereas a name Windows merely cannot represent is
// an ordinary fact about one entry, and must cost only that entry.
var errPathUnrepresentable = errors.New("path is not representable on this platform")

// enforceWindowsPathRules gates the portability-only component checks in
// sanitizeRelPath. It is true only on Windows, where such names genuinely
// cannot be created; a Linux or macOS destination stores them fine. A var,
// not a constant, so tests can exercise BOTH platforms' behavior on either.
var enforceWindowsPathRules = runtime.GOOS == "windows"

// sanitizeRelPath validates a source- or wire-supplied relative path and
// returns its canonical form. It REJECTS rather than repairs: filepath.Clean
// can resolve ".." components in ways that look safe but aren't (e.g.
// "a/../../etc/passwd"), so a suspicious path is refused outright instead of
// silently "fixed". Used both defensively on the source side (paths the
// walker builds itself should always pass) and, more importantly, on the
// destination side in a later phase against untrusted wire input — never
// trust a path that arrived over the network without re-validating it here.
//
// Two classes of check live here, and they are NOT interchangeable:
//
//   - Structural safety (traversal, absolute/UNC/drive paths, NUL bytes,
//     empty components) is enforced everywhere, always. A path failing
//     these is evidence of a hostile or corrupt stream.
//
//   - Windows PORTABILITY (reserved device names like NUL or AUX, trailing
//     dots or spaces, and backslashes in a component) is enforced only
//     where such a name genuinely cannot be created -- a Windows
//     destination, see enforceWindowsPathRules. These names are perfectly
//     ordinary on Linux: a DNS zone file is routinely called "example.net."
//     with the root label's trailing dot, and a Linux filename may
//     legitimately contain a literal backslash byte (it is not a
//     separator -- the wire format and every split above use '/' only).
//     Rejecting those on a Linux-to-Linux sync silently skipped real data
//     to satisfy a constraint neither end had. On a Windows destination a
//     backslash in a component is genuinely dangerous, not just
//     unrepresentable: Windows accepts '\' as a path separator, so
//     filepath.Join/MkdirAll would silently split the component into
//     nested directories. Such a path is returned CLEANED alongside
//     errPathUnrepresentable, so a Windows destination can register and
//     fail just that entry rather than drop it silently or kill the
//     transfer.
func sanitizeRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if len(p) > maxRelPathLen {
		return "", fmt.Errorf("path exceeds %d bytes", maxRelPathLen)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("path contains a NUL byte")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path is absolute")
	}
	if strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("path looks UNC-style")
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("path has a drive prefix")
	}

	parts := strings.Split(p, "/")
	for _, part := range parts {
		switch part {
		case "":
			return "", fmt.Errorf("path contains an empty component (double slash or trailing slash)")
		case ".":
			return "", fmt.Errorf("path contains a '.' component")
		case "..":
			return "", fmt.Errorf("path contains a '..' component")
		}
		if len(part) > maxPathComponentLen {
			return "", fmt.Errorf("path component %q exceeds %d bytes", part, maxPathComponentLen)
		}
	}

	// clean is p itself, not a rebuilt copy: strings.Join(strings.Split(p,
	// "/"), "/") is p byte-for-byte by construction (Split/Join with the
	// same separator always reconstructs the original), so building it was
	// a wasted []string allocation plus a full string copy on every single
	// call -- paid once per announced entry on BOTH the client (walk, via
	// this function) and the server (announce, via the same function), so
	// at tree scale it was two redundant copies of the whole path set.
	clean := p

	// Portability checks last, and only where they bite: the path is
	// already known structurally safe, so it is returned cleaned even when
	// it is unrepresentable here -- the caller needs it for bookkeeping.
	if enforceWindowsPathRules {
		for _, part := range parts {
			if strings.Contains(part, "\\") {
				return clean, fmt.Errorf("%w: path component %q contains a backslash, which Windows treats as a separator", errPathUnrepresentable, part)
			}
			if isWindowsReservedName(part) {
				return clean, fmt.Errorf("%w: path component %q is a reserved Windows device name", errPathUnrepresentable, part)
			}
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
				return clean, fmt.Errorf("%w: path component %q has a trailing dot or space, which Windows rejects", errPathUnrepresentable, part)
			}
		}
	}

	return clean, nil
}

// excluded reports whether relPath (already '/'-separated) matches any of
// the given shell glob patterns, tested against both the full relative path
// and its basename. Uses the "path" package (not "path/filepath") so
// matching is portable regardless of the host OS's separator.
func excluded(relPath string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := path.Base(relPath)
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if ok, _ := path.Match(pat, relPath); ok {
			return true
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// walkSource recursively walks root and emits one FileEntry per discovered
// path (files and directories) on out, in walk order, with dense sequential
// IDs starting at 0. The root itself is never emitted: a directory transfer
// syncs root's CONTENTS into the destination, not root as a named entry.
//
// walkSource owns out and closes it before returning, whether it returns an
// error or not, so a caller can always range over out to know the walk is
// complete.
//
// excl is a list of shell glob patterns (see excluded); a match excludes
// the entry, reported via onSkip like any other skip.
//
// Non-fatal problems — a symlink, a special file, an unreadable
// subdirectory, a path that fails sanitizeRelPath, an excluded path — are
// reported via onSkip (which may be nil) and the walk continues past them.
// walkSource returns a non-nil error only for a problem with the walk as a
// whole: root doesn't exist, root isn't a directory, or an unexpected
// lower-level failure.
//
// blockSizeCap bounds treeBlockSize's per-file block size, exactly as the
// caller intends (typically startDirClient's own resolved capBS, itself
// derived from -b); 0 means "use the package-global blockSize", matching
// the same zero-means-default convention startDirClient already uses for
// blockSizeCap itself, so existing callers passing 0 see no behavior
// change.
//
// links (-links) switches symlink handling: false emits a symlink as a
// KindSkipped placeholder exactly as always; true emits it as a
// KindSymlink entry carrying its verbatim os.Readlink target, to be
// recreated at the destination (falling back to KindSkipped if the target
// can't be read or is implausibly long).
func walkSource(root string, excl []string, blockSizeCap uint32, links, specials bool, out chan<- *FileEntry, onSkip func(relPath, reason string)) error {
	defer close(out)
	if blockSizeCap == 0 {
		blockSizeCap = blockSize
	}

	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("stat root: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}

	var nextID uint32

	// skip only logs, via onSkip -- used for the two cases where relPath
	// itself isn't trustworthy enough to announce (a filepath.Rel failure,
	// or a path sanitizeRelPath just rejected as unsafe): the server
	// re-validates every announced path anyway, so sending either up would
	// hard-fail the whole transfer instead of just skipping this one entry.
	skip := func(relPath, reason string) {
		if onSkip != nil {
			onSkip(relPath, reason)
		}
	}

	// skipKept logs like skip, and additionally announces relPath as a
	// KindSkipped entry: nothing is created on the destination for it (see
	// fileTable.Announce's KindSkipped case), but it IS recorded in the
	// destination's keep-set, so a `-delete` pass knows this path was
	// intentionally left alone by the source -- not simply absent from it
	// -- and must leave it (and, if it's actually a directory on the
	// destination, everything under it) untouched. Used for exactly the
	// three categories AnnounceRec's own doc comment already names:
	// excluded paths, symlinks, and unsupported file types.
	skipKept := func(relPath, reason string) {
		skip(relPath, reason)
		out <- &FileEntry{ID: nextID, RelPath: relPath, Kind: KindSkipped}
		nextID++
	}

	walkErr := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if absPath == root {
			// Never emitted; descend into it unless even stat-ing it failed.
			return err
		}

		// Shared with estimateTree so the two walks cannot disagree about
		// which entries count (see walkGate).
		relPath, reason, gate := walkGate(root, absPath, d, err, excl)
		if gate != gateAccept {
			// An excluded path is announced as a KindSkipped placeholder so
			// -delete protects it; everything else the gate rejects is a
			// path we cannot safely name, so it is only logged.
			if reason == "excluded" {
				skipKept(relPath, reason)
			} else {
				skip(relPath, reason)
			}
			if gate == gateSkipDir {
				return fs.SkipDir
			}
			return nil
		}

		typ := d.Type()
		switch {
		case typ&fs.ModeSymlink != 0:
			// WalkDir uses Lstat semantics, so this is never followed —
			// true for a dangling symlink too, since its target is never
			// touched here.
			if !links {
				skipKept(relPath, "symlink")
				return nil
			}
			target, linkErr := os.Readlink(absPath)
			if linkErr != nil {
				skipKept(relPath, fmt.Sprintf("readlink: %v", linkErr))
				return nil
			}
			if target == "" || len(target) > maxRelPathLen {
				// Nothing legitimate is empty or past PATH_MAX; fall back to
				// the skipped placeholder rather than announce garbage.
				skipKept(relPath, fmt.Sprintf("symlink target length %d out of range", len(target)))
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				skipKept(relPath, fmt.Sprintf("lstat: %v", infoErr))
				return nil
			}
			out <- &FileEntry{
				ID:         nextID,
				RelPath:    relPath,
				Mode:       info.Mode(),
				ModTime:    info.ModTime(),
				Kind:       KindSymlink,
				LinkTarget: target,
			}
			nextID++
			return nil

		case d.IsDir():
			info, infoErr := d.Info()
			if infoErr != nil {
				skip(relPath, fmt.Sprintf("stat: %v", infoErr))
				return fs.SkipDir
			}
			out <- &FileEntry{
				ID:      nextID,
				RelPath: relPath,
				Mode:    info.Mode(),
				ModTime: info.ModTime(),
				Kind:    KindDir,
			}
			nextID++
			return nil

		case typ.IsRegular():
			// One lstat (d.Info), NOT open+fstat+close. A single stat is
			// already a mutually-consistent Size/Mode/ModTime snapshot, so
			// the fd bought nothing but syscalls: it was closed immediately
			// and TreeReader opens the file again when it actually reads it.
			// At three syscalls per file instead of one, that was the
			// single largest fixed cost of walking a large tree -- and on a
			// quick-check run most of these files are never read at all, so
			// the walk was opening millions of files just to discard them.
			//
			// What the open did provide was early detection of an
			// unreadable file. That now surfaces in TreeReader.readOneFile,
			// which already fails the file individually via
			// sess.failFile -- and the path stays announced, so -delete
			// still treats it as intentionally present rather than absent.
			info, infoErr := d.Info()
			if infoErr != nil {
				skip(relPath, fmt.Sprintf("stat: %v", infoErr))
				return nil
			}
			if !info.Mode().IsRegular() {
				// Raced with something replacing the file between the
				// directory-entry read (d_type) and this stat, e.g. a
				// symlink swap. Don't trust metadata for a type that
				// changed under us.
				skip(relPath, "not a regular file after stat")
				return nil
			}
			size := uint64(info.Size())
			bs := treeBlockSize(size, blockSizeCap)
			var blocks uint32
			if size > 0 {
				blocks = uint32((size + uint64(bs) - 1) / uint64(bs))
			}
			out <- &FileEntry{
				ID:        nextID,
				RelPath:   relPath,
				Size:      size,
				Mode:      info.Mode(),
				ModTime:   info.ModTime(),
				Kind:      KindFile,
				BlockSize: bs,
				Blocks:    blocks,
			}
			nextID++
			return nil

		default:
			// Socket, device, char device, named pipe, or any other
			// irregular type. With -specials the first two of those are
			// reproducible as themselves; everything else stays a skipped
			// placeholder.
			if specials && isSpecialMode(typ) {
				info, infoErr := d.Info()
				if infoErr != nil {
					skipKept(relPath, fmt.Sprintf("lstat: %v", infoErr))
					return nil
				}
				out <- &FileEntry{
					ID:      nextID,
					RelPath: relPath,
					Mode:    info.Mode(),
					ModTime: info.ModTime(),
					Kind:    KindSpecial,
				}
				nextID++
				return nil
			}
			skipKept(relPath, fmt.Sprintf("unsupported file type: %v", typ))
			return nil
		}
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return nil
}

// estimateTree is walkSource's fast, metadata-only shadow: it traverses
// root applying the same gates (sanitizeRelPath, excluded, symlink and
// irregular-type skips, never descending into an excluded or unreadable
// directory) and calls onFile(size) once for every entry walkSource would
// emit as a KindFile -- but it never opens a file (d.Info() is an lstat),
// never blocks on a channel, and treats every error as a silent skip.
//
// Its purpose is the progress display: walkSource's own output is
// back-pressured by the transfer pipeline (it can only run a few hundred
// entries ahead of what has actually been read and sent), so on a large
// tree the session's known totals grow at TRANSFER pace, not discovery
// pace, and the files/bytes denominators -- and the ETA computed from
// them -- stay uselessly small until the very end. This scan runs
// concurrently and unthrottled, converging on the true totals within
// seconds, so the display can show max(known, estimate) instead (see
// resolveProgressTotals).
//
// Best-effort by design: a file that appears/disappears/changes size
// mid-run, or a subtree that becomes unreadable, makes the estimate
// approximate -- which is exactly why callers must treat it as a display
// hint (the progress line keeps its '~' marker until the real walk
// finishes) and never as transfer bookkeeping.
// walkGateResult says what the shared entry gate decided about one path.
type walkGateResult int

const (
	gateAccept  walkGateResult = iota // usable; use the returned clean relPath
	gateSkip                          // skip this entry, keep walking siblings
	gateSkipDir                       // skip this entry AND its whole subtree
)

// walkGate is the entry filter shared by walkSource and estimateTree: resolve
// the path relative to root, sanitize it, and apply the exclude patterns.
//
// Shared deliberately. The two walks must agree on which entries "count" --
// estimateTree feeds the progress denominators that walkSource's own totals
// are compared against -- and they previously each reimplemented this,
// leaving nothing to stop them drifting apart. reason is non-empty whenever
// the gate rejects, so walkSource can report it; estimateTree ignores it.
func walkGate(root, absPath string, d fs.DirEntry, err error, excl []string) (path string, reason string, res walkGateResult) {
	dirLike := d != nil && d.IsDir()
	reject := func(path, reason string) (string, string, walkGateResult) {
		if dirLike {
			return path, reason, gateSkipDir
		}
		return path, reason, gateSkip
	}

	relPath, relErr := filepath.Rel(root, absPath)
	if relErr != nil {
		// Should not happen given absPath is always under root during a
		// WalkDir traversal, but fail closed rather than emit garbage. The
		// absolute path is the only usable thing to name it by.
		return reject(absPath, fmt.Sprintf("relative path: %v", relErr))
	}
	relPath = filepath.ToSlash(relPath)

	if err != nil {
		// A directory we couldn't read, or an entry we couldn't stat during
		// the walk itself.
		return reject(relPath, fmt.Sprintf("walk error: %v", err))
	}
	cleanPath, sanErr := sanitizeRelPath(relPath)
	if sanErr != nil {
		return reject(relPath, fmt.Sprintf("unsafe path: %v", sanErr))
	}
	if excluded(cleanPath, excl) {
		if dirLike {
			return cleanPath, "excluded", gateSkipDir
		}
		return cleanPath, "excluded", gateSkip
	}
	return cleanPath, "", gateAccept
}

func estimateTree(root string, excl []string, onFile func(size uint64)) {
	filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if absPath == root {
			return err
		}
		_, _, res := walkGate(root, absPath, d, err, excl)
		switch res {
		case gateSkipDir:
			return fs.SkipDir
		case gateSkip:
			return nil
		}
		if d.Type().IsRegular() {
			if info, infoErr := d.Info(); infoErr == nil && info.Mode().IsRegular() {
				onFile(uint64(info.Size()))
			}
		}
		return nil
	})
}
