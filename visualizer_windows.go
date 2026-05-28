//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandle                 = kernel32.NewProc("GetStdHandle")
	procGetConsoleScreenBufferInfoEx = kernel32.NewProc("GetConsoleScreenBufferInfoEx")
)

const stdOutputHandle = ^uintptr(10) // uintptr(-11) for STD_OUTPUT_HANDLE

type coordSmall struct {
	X, Y int16
}

type smallRect struct {
	Left, Top, Right, Bottom int16
}

type consoleScreenBufferInfoEx struct {
	cbSize               uint32
	Size                 coordSmall
	CursorPosition       coordSmall
	Attributes           uint16
	Window               smallRect
	MaximumWindowSize    coordSmall
	Attributes2          uint16
	ColorTable           [16]uint32
}

// getConsoleSize retrieves both terminal width and height via Windows Console API.
func getConsoleSize() (width, height int) {
	handle, _, _ := procGetStdHandle.Call(uintptr(stdOutputHandle))
	if handle == 0 || handle == ^uintptr(0) {
		return 0, 0
	}

	var info consoleScreenBufferInfoEx
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, _ := procGetConsoleScreenBufferInfoEx.Call(handle, uintptr(unsafe.Pointer(&info)))
	if ret != 0 {
		w := int(info.Window.Right - info.Window.Left + 1)
		h := int(info.Window.Bottom - info.Window.Top + 1)
		return w, h
	}

	// Fallback to GetConsoleScreenBufferInfo (older Windows)
	var info2 consoleScreenBufferInfo
	ret2, _, _ := procGetConsoleScreenBufferInfo.Call(handle, uintptr(unsafe.Pointer(&info2)))
	if ret2 == 0 {
		return 0, 0
	}
	w := int(info2.Window.Right - info2.Window.Left + 1)
	h := int(info2.Window.Bottom - info2.Window.Top + 1)
	return w, h
}

// getTerminalWidthIoctl returns terminal width via Windows Console API.
func getTerminalWidthIoctl() int {
	w, _ := getConsoleSize()
	return w
}

// getTerminalHeightIoctl returns terminal height via Windows Console API.
func getTerminalHeightIoctl() int {
	_, h := getConsoleSize()
	return h
}
