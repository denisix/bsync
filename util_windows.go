//go:build windows

package main

import (
	"io"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// IOCTL_DISK_GET_DRIVE_GEOMETRY_EX = CTL_CODE(0x7, 0x28, 0, 0)
const ioctlDiskGetDriveGeometryEx = 0x000700A0

type winDiskGeometry struct {
	Cylinders         int64
	MediaType         uint32
	TracksPerCylinder uint32
	SectorsPerTrack   uint32
	BytesPerSector    uint32
}

type winDiskGeometryEx struct {
	Geometry winDiskGeometry
	DiskSize int64
	Data     [1]byte
}

// getDeviceSize returns file or physical drive size in bytes.
// For regular files, uses Seek. For Windows device paths (\\.\PhysicalDriveN, \\.\C:),
// falls back to IOCTL_DISK_GET_DRIVE_GEOMETRY_EX.
func getDeviceSize(file *os.File) uint64 {
	pos, err := file.Seek(0, io.SeekEnd)
	if err == nil && pos >= 0 {
		file.Seek(0, io.SeekStart)
		Log("file size -> %d bytes.\n", pos)
		return uint64(pos)
	}
	file.Seek(0, io.SeekStart)

	// Physical drives on Windows return 0 or error from Seek — use DeviceIoControl
	var geom winDiskGeometryEx
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		windows.Handle(file.Fd()),
		ioctlDiskGetDriveGeometryEx,
		nil, 0,
		(*byte)(unsafe.Pointer(&geom)),
		uint32(unsafe.Sizeof(geom)),
		&bytesReturned,
		nil,
	)
	if err == nil && geom.DiskSize > 0 {
		size := uint64(geom.DiskSize)
		Log("device size -> %d bytes.\n", size)
		return size
	}

	Log("Error: cannot determine size of %s: %v\n", file.Name(), err)
	return 0
}

func isWindowsDevicePath(name string) bool {
	return strings.HasPrefix(name, `\\.\`)
}

func truncateIfRegularFile(file *os.File, size uint64) {
	// Never truncate raw device paths
	if isWindowsDevicePath(file.Name()) {
		return
	}

	info, err := file.Stat()
	if err != nil {
		Log("Error: file stat failed: %v\n", err)
		return
	}

	if info.Mode().IsRegular() {
		currentSize := uint64(info.Size())
		if currentSize != size {
			if err := file.Truncate(int64(size)); err != nil {
				Err("Error: truncate failed: %v\n", err)
			}
			Log("file truncated to %d bytes\n", size)
		}
	}
}
