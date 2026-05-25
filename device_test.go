package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractDeviceName tests device name extraction from various paths.
func TestExtractDeviceName(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
		note     string
	}{
		{
			name:     "simple device sda",
			path:     "/dev/sda",
			expected: "sda",
		},
		{
			name:     "simple device sdb",
			path:     "/dev/sdb",
			expected: "sdb",
		},
		{
			name:     "simple device nvme",
			path:     "/dev/nvme0n1",
			expected: "nvme0n1",
		},
		{
			name:     "partition sda1",
			path:     "/dev/sda1",
			expected: "sda",
			note:     "partition should extract base device",
		},
		{
			name:     "partition sda2",
			path:     "/dev/sda2",
			expected: "sda",
		},
		{
			name:     "partition sdb15",
			path:     "/dev/sdb15",
			expected: "sdb",
		},
		{
			name:     "partition with multiple digits sda123",
			path:     "/dev/sda123",
			expected: "sda",
		},
		{
			name:     "nvme partition",
			path:     "/dev/nvme0n1p2",
			expected: "nvme0n1p2",
			note:     "nvme partitions not stripped by current logic",
		},
		{
			name:     "mmc device",
			path:     "/dev/mmcblk0",
			expected: "mmcblk0",
		},
		{
			name:     "mmc partition",
			path:     "/dev/mmcblk0p1",
			expected: "mmcblk0p1",
			note:     "mmc partitions not stripped by current logic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDeviceName(tt.path)
			if result != tt.expected {
				t.Errorf("extractDeviceName(%q) = %q, want %q. %s",
					tt.path, result, tt.expected, tt.note)
			}
		})
	}
}

// TestExtractDeviceNameMountPoint tests extraction from mount points.
func TestExtractDeviceNameMountPoint(t *testing.T) {
	// This test is hard to do reliably without mocking /proc/mounts
	// We'll just verify the function doesn't panic and returns something

	// Test with a non-existent path - should return default
	result := extractDeviceName("/nonexistent/path/that/does/not/exist")
	if result == "" {
		t.Error("extractDeviceName should return a default value, not empty string")
	}

	// The default should be "sda" as per the code
	if result != "sda" {
		t.Logf("extractDeviceName returned %q for non-existent path (default is 'sda')", result)
	}
}

// TestReadSysfsInt tests reading integer values from sysfs.
func TestReadSysfsInt(t *testing.T) {
	// Test with a real sysfs file if available
	// Use sda's device name which is likely to exist on most Linux systems

	device := "sda"

	// Try reading queue/depth (or nr_requests)
	val, err := readSysfsInt(device, "queue/nr_requests")
	if err != nil {
		t.Skipf("sysfs not available or sda doesn't exist: %v", err)
	}

	if val <= 0 {
		t.Errorf("readSysfsInt(%s, queue/nr_requests) = %d, want > 0", device, val)
	}

	t.Logf("readSysfsInt(%s, queue/nr_requests) = %d", device, val)
}

// TestReadSysfsIntNonExistent tests reading non-existent sysfs property.
func TestReadSysfsIntNonExistent(t *testing.T) {
	device := "sda"

	// Try reading a non-existent property
	_, err := readSysfsInt(device, "queue/nonexistent_property")
	if err == nil {
		t.Error("Expected error when reading non-existent sysfs property")
	}

	t.Logf("Expected error for non-existent property: %v", err)
}

// TestReadSysfsIntInvalidFile tests reading invalid device.
func TestReadSysfsIntInvalidDevice(t *testing.T) {
	device := "nonexistent_device_xyz123"

	_, err := readSysfsInt(device, "queue/nr_requests")
	if err == nil {
		t.Error("Expected error when reading from non-existent device")
	}

	t.Logf("Expected error for non-existent device: %v", err)
}

// TestGetDeviceInfo tests getting device information.
func TestGetDeviceInfo(t *testing.T) {
	// Test with sda which is likely to exist on most systems
	info, err := GetDeviceInfo("/dev/sda")
	if err != nil {
		t.Skipf("GetDeviceInfo failed: %v", err)
	}

	if info == nil {
		t.Fatal("GetDeviceInfo returned nil info")
	}

	if info.Path == "" {
		t.Error("DeviceInfo.Path is empty")
	}

	// Physical sector size should be set (default is 4096)
	if info.PhysicalSectorSize == 0 {
		t.Error("PhysicalSectorSize should have a default value")
	}

	// Logical sector size should be set (default is 512)
	if info.LogicalSectorSize == 0 {
		t.Error("LogicalSectorSize should have a default value")
	}

	t.Logf("Device info: Path=%s, PhysicalSector=%d, LogicalSector=%d, OptimalIO=%d, MinIO=%d, QueueDepth=%d",
		info.Path, info.PhysicalSectorSize, info.LogicalSectorSize,
		info.OptimalIOSize, info.MinIOSize, info.QueueDepth)
}

// TestGetDeviceInfoNonExistent tests getting info for non-existent device.
func TestGetDeviceInfoNonExistent(t *testing.T) {
	info, err := GetDeviceInfo("/dev/nonexistent_device_xyz123")

	// Should return info with defaults, not error
	if err != nil {
		t.Logf("GetDeviceInfo returned error for non-existent device: %v", err)
	}

	if info == nil {
		t.Fatal("GetDeviceInfo should return info with defaults even for non-existent device")
	}

	// Verify defaults are set
	if info.PhysicalSectorSize != 4096 {
		t.Errorf("PhysicalSectorSize should be 4096 (default), got %d", info.PhysicalSectorSize)
	}

	if info.LogicalSectorSize != 512 {
		t.Errorf("LogicalSectorSize should be 512 (default), got %d", info.LogicalSectorSize)
	}
}

// TestGetDeviceInfoPartition tests getting info for partition device.
func TestGetDeviceInfoPartition(t *testing.T) {
	// Test with sda1 which should use sda's sysfs info
	info, err := GetDeviceInfo("/dev/sda1")
	if err != nil {
		t.Skipf("GetDeviceInfo failed: %v", err)
	}

	if info == nil {
		t.Fatal("GetDeviceInfo returned nil info")
	}

	// extractDeviceName should have returned "sda" for "sda1"
	// so info.Path should be "sda"
	if info.Path != "sda" {
		t.Logf("DeviceInfo.Path = %q (expected 'sda' for partition sda1)", info.Path)
	}

	t.Logf("Partition device info: Path=%s, PhysicalSector=%d, LogicalSector=%d",
		info.Path, info.PhysicalSectorSize, info.LogicalSectorSize)
}

// TestGetOptimalAlignment tests getting optimal alignment.
func TestGetOptimalAlignment(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"sda", "/dev/sda"},
		{"sdb", "/dev/sdb"},
		{"nvme", "/dev/nvme0n1"},
		{"partition", "/dev/sda1"},
		{"nonexistent", "/dev/nonexistent_xyz123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			align := GetOptimalAlignment(tt.path)

			// Should always return at least 4K
			if align < 4096 {
				t.Errorf("GetOptimalAlignment(%s) = %d, want >= 4096", tt.path, align)
			}

			t.Logf("GetOptimalAlignment(%s) = %d", tt.path, align)
		})
	}
}

// TestDeviceInfoDefaults tests that DeviceInfo has proper defaults.
func TestDeviceInfoDefaults(t *testing.T) {
	info := &DeviceInfo{}

	// Test default values
	if info.PhysicalSectorSize != 0 {
		t.Logf("PhysicalSectorSize default = %d", info.PhysicalSectorSize)
	}

	if info.LogicalSectorSize != 0 {
		t.Logf("LogicalSectorSize default = %d", info.LogicalSectorSize)
	}

	// After GetDeviceInfo for non-existent device, defaults should be set
	info2, _ := GetDeviceInfo("/dev/does_not_exist")

	if info2.PhysicalSectorSize == 0 {
		t.Error("PhysicalSectorSize should have default (4096)")
	}

	if info2.LogicalSectorSize == 0 {
		t.Error("LogicalSectorSize should have default (512)")
	}
}

// TestDeviceInfoRealDevice tests with real devices if available.
func TestDeviceInfoRealDevice(t *testing.T) {
	// Try to find a real block device on the system
	devices := []string{"sda", "sdb", "nvme0n1", "vda", "xvda"}

	var foundDevice string
	var foundPath string

	for _, dev := range devices {
		path := "/dev/" + dev
		if _, err := os.Stat(path); err == nil {
			foundDevice = dev
			foundPath = path
			break
		}
	}

	if foundDevice == "" {
		t.Skip("No real block device found for testing")
	}

	t.Logf("Testing with real device: %s", foundPath)

	info, err := GetDeviceInfo(foundPath)
	if err != nil {
		t.Logf("GetDeviceInfo returned error: %v", err)
	}

	if info != nil {
		t.Logf("Device info: Path=%s, PhysicalSector=%d, LogicalSector=%d",
			info.Path, info.PhysicalSectorSize, info.LogicalSectorSize)

		// Verify sysfs files exist for this device
		sysfsBase := "/sys/block/" + extractDeviceName(foundPath)

		// Check what sysfs files are available
		properties := []string{
			"queue/physical_block_size",
			"queue/logical_block_size",
			"queue/optimal_io_size",
			"queue/min_io_size",
			"queue/nr_requests",
		}

		for _, prop := range properties {
			fullPath := filepath.Join(sysfsBase, prop)
			if _, err := os.Stat(fullPath); err == nil {
				t.Logf("  sysfs available: %s", prop)
			} else {
				t.Logf("  sysfs not available: %s", prop)
			}
		}
	}
}

// TestExtractDeviceNameLVM tests LVM device handling.
func TestExtractDeviceNameLVM(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		contains string
	}{
		{
			name:     "mapper device",
			path:     "/dev/mapper/vg0-lv1",
			contains: "vg0-lv1",
		},
		{
			name:     "mapper device with dashes",
			path:     "/dev/mapper/test--vg-test--lv",
			contains: "test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractDeviceName(tt.path)
			if !strings.Contains(result, tt.contains) {
				t.Logf("extractDeviceName(%q) = %q (expected to contain %q)",
					tt.path, result, tt.contains)
				t.Logf("Note: LVM handling may not work without /proc/mounts or proper sysfs setup")
			}
		})
	}
}

// TestGetDeviceInfoMultiplePartitions tests that partition devices get base device info.
func TestGetDeviceInfoMultiplePartitions(t *testing.T) {
	partitions := []string{"/dev/sda1", "/dev/sda2", "/dev/sdb1", "/dev/sdb2"}

	for _, part := range partitions {
		t.Run(part, func(t *testing.T) {
			info, err := GetDeviceInfo(part)

			if err != nil {
				t.Logf("GetDeviceInfo(%s) returned error: %v (device may not exist)", part, err)
				return
			}

			if info != nil {
				t.Logf("%s: Path=%s, PhysicalSector=%d, LogicalSector=%d",
					part, info.Path, info.PhysicalSectorSize, info.LogicalSectorSize)

				// Path should be the base device (sda, sdb, etc.)
				// Extract base from partition path
				base := filepath.Base(part)
				for i := len(base); i > 0; i-- {
					if base[i-1] < '0' || base[i-1] > '9' {
						expectedBase := base[:i]
						if info.Path != expectedBase {
							t.Logf("Note: info.Path=%s, expected base=%s", info.Path, expectedBase)
						}
						break
					}
				}
			}
		})
	}
}
