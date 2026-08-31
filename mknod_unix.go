//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// makeSpecialNode creates a FIFO or a unix socket at abs, reproducing the
// node type carried in mode. Only those two types are ever passed here (see
// isSpecialMode): a device node would additionally need a major/minor pair
// on the wire and CAP_MKNOD on the destination, which is rsync's separate
// --devices, not --specials.
//
// A socket is created with mknod(S_IFSOCK) rather than by binding and
// closing a listener: binding would make the path briefly live, and mknod
// needs no privilege for S_IFSOCK or S_IFIFO (unlike S_IFBLK/S_IFCHR).
// Permission bits are left to the caller's later chmod, matching how every
// other kind applies its mode.
func makeSpecialNode(abs string, mode os.FileMode) error {
	perm := uint32(mode.Perm())
	switch {
	case mode&os.ModeNamedPipe != 0:
		if err := syscall.Mkfifo(abs, perm); err != nil {
			return fmt.Errorf("mkfifo: %w", err)
		}
		return nil
	case mode&os.ModeSocket != 0:
		if err := syscall.Mknod(abs, syscall.S_IFSOCK|perm, 0); err != nil {
			return fmt.Errorf("mknod socket: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported special file type %v", mode.Type())
	}
}
