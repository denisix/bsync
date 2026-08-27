//go:build darwin

package main

import "syscall"

// LockBuffer prevents a buffer from being paged out to swap.
// Returns error on failure (e.g., RLIMIT_MEMLOCK exceeded).
func LockBuffer(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return syscall.Mlock(data)
}

// UnlockBuffer releases the memory lock.
func UnlockBuffer(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return syscall.Munlock(data)
}

// TryLockBuffer attempts to lock a buffer, logs warning but doesn't fail.
// Memory locking is best-effort - the application will work without it.
func TryLockBuffer(data []byte) {
	if err := LockBuffer(data); err != nil {
		Log("Warning: failed to lock buffer (%d bytes): %s\n", len(data), err)
	}
}

// LockAllMemory locks all current and future memory mappings.
// Use with extreme caution - can cause system-wide memory pressure.
// Requires elevated RLIMIT_MEMLOCK.
func LockAllMemory() error {
	return syscall.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE)
}

// UnlockAllMemory releases all memory locks.
func UnlockAllMemory() error {
	return syscall.Munlockall()
}

// AdviseHugePage is a no-op on Darwin: XNU has no user-directed huge-page
// hint equivalent to Linux's MADV_HUGEPAGE (transparent huge pages aren't a
// concept on this platform).
func AdviseHugePage(data []byte) error {
	return nil
}
