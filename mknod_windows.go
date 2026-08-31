//go:build windows

package main

import (
	"fmt"
	"os"
)

// makeSpecialNode always fails on Windows: it has no FIFO or unix-socket
// filesystem node to create. The caller records this as the one entry's
// environmental failure, so a tree containing them still syncs everything
// else -- the same shape as any other per-entry announce failure.
func makeSpecialNode(abs string, mode os.FileMode) error {
	return fmt.Errorf("special files are not supported on windows")
}
