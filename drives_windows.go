//go:build windows

package main

import (
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ioctlDiskGetDriveGeometryExDrives matches the IOCTL code defined in util_windows.go
const ioctlDiskGeomForList = 0x000700A0

type driveListGeometry struct {
	Cylinders         int64
	MediaType         uint32
	TracksPerCylinder uint32
	SectorsPerTrack   uint32
	BytesPerSector    uint32
}

type driveListGeometryEx struct {
	Geometry driveListGeometry
	DiskSize int64
	Data     [1]byte
}

const (
	winDriveRemovable = 2
	winDriveFixed     = 3
	winDriveRemote    = 4
	winDriveCDROM     = 5
	winDriveRAMDisk   = 6
)

func driveTypeStr(t uint32) string {
	switch t {
	case winDriveRemovable:
		return "removable"
	case winDriveFixed:
		return "fixed"
	case winDriveRemote:
		return "network"
	case winDriveCDROM:
		return "cdrom"
	case winDriveRAMDisk:
		return "ramdisk"
	default:
		return "unknown"
	}
}

func queryPhysicalDriveSize(path string) (int64, uint32, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(handle)

	var geom driveListGeometryEx
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		handle,
		ioctlDiskGeomForList,
		nil, 0,
		(*byte)(unsafe.Pointer(&geom)),
		uint32(unsafe.Sizeof(geom)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		return 0, 0, err
	}
	return geom.DiskSize, uint32(geom.Geometry.BytesPerSector), nil
}

func listDrives() {
	fmt.Println("Physical drives (use with -f, requires Administrator):")
	found := false
	for i := 0; i < 16; i++ {
		path := fmt.Sprintf(`\\.\PhysicalDrive%d`, i)
		size, sectorSize, err := queryPhysicalDriveSize(path)
		if err != nil {
			break
		}
		found = true
		sizeGB := float64(size) / (1024 * 1024 * 1024)
		fmt.Printf("  -f %-30s  %8.2f GB  (sector: %d B)\n", path, sizeGB, sectorSize)
	}
	if !found {
		fmt.Println("  (none found — run as Administrator to access physical drives)")
	}

	fmt.Println("\nLogical volumes (use with -f):")
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil || n == 0 {
		fmt.Println("  (none found)")
		return
	}

	// Parse multi-string: consecutive null-terminated UTF-16 strings ending with empty string
	for i := 0; i < int(n); {
		j := i
		for j < int(n) && buf[j] != 0 {
			j++
		}
		if j == i {
			break // double-null terminator
		}
		drive := string(utf16.Decode(buf[i:j])) // e.g. "C:\"
		i = j + 1

		if len(drive) < 2 || drive[1] != ':' {
			continue
		}
		devicePath := fmt.Sprintf(`\\.\%c:`, drive[0])

		drivePtr, _ := windows.UTF16PtrFromString(drive)
		driveType := windows.GetDriveType(drivePtr)
		typeStr := driveTypeStr(driveType)

		var total, free uint64
		sizePtr, _ := windows.UTF16PtrFromString(drive)
		err := windows.GetDiskFreeSpaceEx(sizePtr, nil, &total, &free)
		if err == nil && total > 0 {
			totalGB := float64(total) / (1024 * 1024 * 1024)
			freeGB := float64(free) / (1024 * 1024 * 1024)
			fmt.Printf("  -f %-30s  %8.2f GB total, %8.2f GB free  [%s]\n",
				devicePath, totalGB, freeGB, typeStr)
		} else {
			fmt.Printf("  -f %-30s  [%s]\n", devicePath, typeStr)
		}
	}
}
