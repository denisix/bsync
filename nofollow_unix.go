//go:build !windows

package main

import "syscall"

// openNoFollow is OR'd into the destination-file open in fileTable.Acquire so
// a symlink sitting at the destination path is never written THROUGH. Without
// it, a stale link left by an earlier -links run (say "foo" -> /etc/hostname)
// standing where the source now has a regular file would send that file's
// content straight to the link's target, outside the destination root --
// reachable even when the current run doesn't pass -links at all, since the
// link is already on disk. announce removes such a link up front
// (clearConflictingDest); this closes the window between that check and the
// open, where the check alone cannot help.
//
// Opening a symlink with O_NOFOLLOW fails with ELOOP, which Acquire surfaces
// as this one file's error. Replacing (rather than following) a symlink at a
// destination path is also what rsync does.
const openNoFollow = syscall.O_NOFOLLOW
