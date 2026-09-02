//go:build !windows

package main

import "syscall"

// maxOpenFiles returns the process's soft limit on open file descriptors
// (RLIMIT_NOFILE) and whether it could be determined. Used to keep the
// directory server's open-file budget inside what the process can actually
// hold: the budget is derived from the worker count, so a high -w can
// otherwise ask for more descriptors than the limit allows and turn every
// file open into EMFILE.
func maxOpenFiles() (uint64, bool) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false
	}
	// RLIM_INFINITY comes back as a huge value; treat it as "no useful
	// limit" so the caller keeps its own sizing.
	if lim.Cur == 0 || lim.Cur > 1<<31 {
		return 0, false
	}
	return uint64(lim.Cur), true
}
