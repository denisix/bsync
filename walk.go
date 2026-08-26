package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
)

// FileEntry describes one path discovered by walkSource, in walk order.
type FileEntry struct {
	ID      uint32 // dense, 0-based, assigned in emission order
	RelPath string // '/'-separated, sanitized, relative to the walk root
	AbsPath string // source-side only; never sent over the wire
	Size    uint64
	Mode    os.FileMode
	ModTime time.Time
	Kind    EntryKind

	BlockSize uint32 // per-file block size (treeBlockSize); 0 for dirs
	Blocks    uint32 // number of blocks; 0 for dirs and empty files
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

// sanitizeRelPath validates a source- or wire-supplied relative path and
// returns its canonical form. It REJECTS rather than repairs: filepath.Clean
// can resolve ".." components in ways that look safe but aren't (e.g.
// "a/../../etc/passwd"), so a suspicious path is refused outright instead of
// silently "fixed". Used both defensively on the source side (paths the
// walker builds itself should always pass) and, more importantly, on the
// destination side in a later phase against untrusted wire input — never
// trust a path that arrived over the network without re-validating it here.
//
// The Windows-reserved-name and trailing-dot/space checks run
// unconditionally, not just when GOOS=="windows": the source and
// destination need not run the same OS (this repo ships bsync.exe), so a
// path that's fine on the sender's filesystem can still be unsafe to create
// on the receiver's.
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
	if strings.Contains(p, "\\") {
		return "", fmt.Errorf("path contains a backslash; the wire format is always '/'-separated")
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
		if isWindowsReservedName(part) {
			return "", fmt.Errorf("path component %q is a reserved Windows device name", part)
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", fmt.Errorf("path component %q has a trailing dot or space, which Windows rejects", part)
		}
	}

	return strings.Join(parts, "/"), nil
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
func walkSource(root string, excl []string, out chan<- *FileEntry, onSkip func(relPath, reason string)) error {
	defer close(out)

	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("stat root: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}

	skip := func(relPath, reason string) {
		if onSkip != nil {
			onSkip(relPath, reason)
		}
	}

	var nextID uint32

	walkErr := filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if absPath == root {
			// Never emitted; descend into it unless even stat-ing it failed.
			return err
		}

		relPath, relErr := filepath.Rel(root, absPath)
		if relErr != nil {
			// Should not happen given absPath is always under root during a
			// WalkDir traversal, but fail closed rather than emit garbage.
			skip(absPath, fmt.Sprintf("relative path: %v", relErr))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		if err != nil {
			// A directory we couldn't read, or an entry we couldn't stat
			// during the walk itself. Skip it and keep walking siblings.
			skip(relPath, fmt.Sprintf("walk error: %v", err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		clean, sanErr := sanitizeRelPath(relPath)
		if sanErr != nil {
			skip(relPath, fmt.Sprintf("unsafe path: %v", sanErr))
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relPath = clean

		if excluded(relPath, excl) {
			skip(relPath, "excluded")
			if d.IsDir() {
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
			skip(relPath, "symlink")
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
				AbsPath: absPath,
				Mode:    info.Mode(),
				ModTime: info.ModTime(),
				Kind:    KindDir,
			}
			nextID++
			return nil

		case typ.IsRegular():
			// Open and Stat via the fd (rather than trusting the DirEntry,
			// which is a readdir-time snapshot) so Size/Mode/ModTime are at
			// least mutually consistent with each other, captured by one
			// syscall close to when the file will actually be read.
			f, openErr := os.Open(absPath)
			if openErr != nil {
				skip(relPath, fmt.Sprintf("open: %v", openErr))
				return nil
			}
			info, statErr := f.Stat()
			closeErr := f.Close()
			if statErr != nil {
				skip(relPath, fmt.Sprintf("stat: %v", statErr))
				return nil
			}
			if closeErr != nil {
				skip(relPath, fmt.Sprintf("close: %v", closeErr))
				return nil
			}
			if !info.Mode().IsRegular() {
				// Raced with something replacing the file between the
				// directory-entry read and the open (e.g. a symlink swap).
				// Don't trust stale metadata for a type that changed.
				skip(relPath, "not a regular file after open")
				return nil
			}
			size := uint64(info.Size())
			bs := treeBlockSize(size, blockSize)
			var blocks uint32
			if size > 0 {
				blocks = uint32((size + uint64(bs) - 1) / uint64(bs))
			}
			out <- &FileEntry{
				ID:        nextID,
				RelPath:   relPath,
				AbsPath:   absPath,
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
			// irregular type.
			skip(relPath, fmt.Sprintf("unsupported file type: %v", typ))
			return nil
		}
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return nil
}
