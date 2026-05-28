//go:build !windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize matches the Unix termios TIOCGWINSZ struct.
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// getTerminalWidthIoctl uses TIOCGWINSZ ioctl to get terminal width on Unix-like systems.
func getTerminalWidthIoctl() int {
	ws := &winsize{}
	retCode, _, _ := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(os.Stdout.Fd()),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if int(retCode) == -1 {
		return 0
	}
	return int(ws.Col)
}

// getTerminalHeightIoctl uses TIOCGWINSZ ioctl to get terminal height on Unix-like systems.
func getTerminalHeightIoctl() int {
	ws := &winsize{}
	retCode, _, _ := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(os.Stdout.Fd()),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(ws)),
	)
	if int(retCode) == -1 {
		return 0
	}
	return int(ws.Row)
}
