//go:build windows

package main

// openNoFollow is 0 on Windows: there is no O_NOFOLLOW, and the Go runtime's
// CreateFile-based open does not offer an equivalent reparse-point flag
// through os.OpenFile. The announce-time clearConflictingDest check still
// applies there; only the TOCTOU hardening is unavailable.
const openNoFollow = 0
