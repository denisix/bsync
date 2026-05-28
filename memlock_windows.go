//go:build windows

package main

// LockBuffer is a no-op on Windows.
func LockBuffer(data []byte) error {
	return nil
}

// UnlockBuffer is a no-op on Windows.
func UnlockBuffer(data []byte) error {
	return nil
}

// TryLockBuffer is a no-op on Windows.
func TryLockBuffer(data []byte) {
	// No-op on Windows
}

// LockAllMemory is not supported on Windows.
func LockAllMemory() error {
	return nil // No-op on Windows
}

// UnlockAllMemory is not supported on Windows.
func UnlockAllMemory() error {
	return nil // No-op on Windows
}

// AdviseHugePage is a no-op on Windows.
func AdviseHugePage(data []byte) error {
	return nil
}
