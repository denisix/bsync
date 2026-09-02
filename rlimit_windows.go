//go:build windows

package main

// maxOpenFiles reports that no descriptor limit is discoverable on Windows.
// Windows has no RLIMIT_NOFILE: handles are bounded by system resources
// rather than a per-process soft limit the program can read, so the
// directory server keeps its worker-derived open-file budget as-is.
func maxOpenFiles() (uint64, bool) {
	return 0, false
}
