package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DeviceInfo contains information about a block device.
type DeviceInfo struct {
	Path              string // Device name (e.g., "sda")
	PhysicalSectorSize int    // Physical block size in bytes
	LogicalSectorSize  int    // Logical block size in bytes
	OptimalIOSize      int    // Optimal I/O size in bytes
	MinIOSize         int    // Minimum I/O size in bytes
	QueueDepth        int    // Queue depth
}

// extractDeviceName extracts device name from path.
// e.g., "/dev/sda" -> "sda", "/dev/nvme0n1" -> "nvme0n1"
// For mount points, reads /proc/mounts to find backing device.
func extractDeviceName(path string) string {
	// If it's a direct device path
	if strings.HasPrefix(path, "/dev/") {
		base := filepath.Base(path)
		// Handle partition devices (sda1 -> sda)
		if len(base) > 3 && base[0:3] == "sda" || base[0:3] == "sdb" || base[0:3] == "sdc" || base[0:3] == "sdd" || base[0:3] == "sde" || base[0:3] == "sdf" {
			// Extract base device name from partition
			for i := len(base); i > 0; i-- {
				if base[i-1] < '0' || base[i-1] > '9' {
					return base[:i]
				}
			}
		}
		return base
	}

	// For mount points, try to find backing device from /proc/mounts
	if mounts, err := os.ReadFile("/proc/mounts"); err == nil {
		lines := strings.Split(string(mounts), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == path {
				device := fields[0]
				// Handle /dev/mapper/* devices
				if strings.HasPrefix(device, "/dev/mapper/") {
					// For LVM, try to get the underlying device
					dmName := filepath.Base(device)
					// Try to read from sysfs for dm devices
					if slaves, err := os.ReadFile(fmt.Sprintf("/sys/block/dm-%s/slaves", dmName)); err == nil {
						devices := strings.Fields(string(slaves))
						if len(devices) > 0 {
							return devices[0]
						}
					}
					return dmName
				}
				return extractDeviceName(device)
			}
		}
	}

	// Default fallback
	return "sda"
}

// readSysfsInt reads an integer value from sysfs.
func readSysfsInt(device, property string) (int, error) {
	path := fmt.Sprintf("/sys/block/%s/%s", device, property)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return val, nil
}

// GetDeviceInfo retrieves device information from sysfs.
// Returns device information with sensible defaults for unavailable properties.
func GetDeviceInfo(path string) (*DeviceInfo, error) {
	device := extractDeviceName(path)
	info := &DeviceInfo{Path: device}

	// Read physical sector size
	if size, err := readSysfsInt(device, "queue/physical_block_size"); err == nil {
		info.PhysicalSectorSize = size
	} else {
		info.PhysicalSectorSize = 4096 // Default to 4K
	}

	// Read logical sector size
	if size, err := readSysfsInt(device, "queue/logical_block_size"); err == nil {
		info.LogicalSectorSize = size
	} else {
		info.LogicalSectorSize = 512 // Default to 512
	}

	// Read optimal I/O size (may not be available on all devices)
	if size, err := readSysfsInt(device, "queue/optimal_io_size"); err == nil && size > 0 {
		info.OptimalIOSize = size
	}

	// Read minimum I/O size
	if size, err := readSysfsInt(device, "queue/min_io_size"); err == nil && size > 0 {
		info.MinIOSize = size
	}

	// Read queue depth
	if depth, err := readSysfsInt(device, "queue/nr_requests"); err == nil {
		info.QueueDepth = depth
	}

	return info, nil
}

// GetOptimalAlignment returns the optimal alignment for I/O operations
// on the given device. Returns physical sector size or 4K default.
func GetOptimalAlignment(devicePath string) int {
	info, err := GetDeviceInfo(devicePath)
	if err != nil {
		Log("Warning: failed to get device info for %s: %s\n", devicePath, err)
		return 4096 // Default to 4K
	}
	if info.PhysicalSectorSize > 0 {
		return info.PhysicalSectorSize
	}
	return 4096
}
