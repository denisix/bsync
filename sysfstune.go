package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SysfsTuner handles Linux sysfs tuning for HDD optimization.
// These operations require root access and affect system-wide behavior.
type SysfsTuner struct {
	diskDevice string // e.g., "sda" from /dev/sda
	sysfsRoot  string
}

// NewSysfsTuner creates a new tuner for the specified disk device.
func NewSysfsTuner(devicePath string) *SysfsTuner {
	return NewSysfsTunerAt(devicePath, "/sys")
}

// NewSysfsTunerAt creates a tuner rooted at a caller-provided sysfs tree.
// Tests use this to avoid modifying system-wide kernel tuning.
func NewSysfsTunerAt(devicePath, sysfsRoot string) *SysfsTuner {
	// Extract device name from path (e.g., "/dev/sda" -> "sda")
	device := devicePath
	if len(devicePath) > 5 && devicePath[:5] == "/dev/" {
		device = devicePath[5:]
	}
	return &SysfsTuner{diskDevice: device, sysfsRoot: sysfsRoot}
}

func (t *SysfsTuner) queuePath(name string) string {
	return filepath.Join(t.sysfsRoot, "block", t.diskDevice, "queue", name)
}

// SetReadAhead sets the kernel read-ahead size in KB.
// Default is usually 128KB; optimal for HDD is 1024-4096KB (1-4MB).
func (t *SysfsTuner) SetReadAhead(kb int) error {
	path := t.queuePath("read_ahead_kb")
	return os.WriteFile(path, []byte(strconv.Itoa(kb)), 0644)
}

// GetReadAhead returns the current read-ahead size in KB.
func (t *SysfsTuner) GetReadAhead() (int, error) {
	path := t.queuePath("read_ahead_kb")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

// SetIOScheduler sets the I/O scheduler.
// Recommended for HDD: "deadline" or "bfq"
// Avoid: "cfq" (can starve sequential I/O with random)
func (t *SysfsTuner) SetIOScheduler(scheduler string) error {
	path := t.queuePath("scheduler")
	data := fmt.Sprintf("%s", scheduler)
	return os.WriteFile(path, []byte(data), 0644)
}

// GetIOScheduler returns the current I/O scheduler.
func (t *SysfsTuner) GetIOScheduler() (string, error) {
	path := t.queuePath("scheduler")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Format: "noop deadline [cfq]"
	// We need to find the one in brackets
	// Simple parsing: return the whole string for now
	return string(data), nil
}

// SetQueueDepth sets the request queue depth.
// Default is usually 128; optimal for HDD is 64-256.
func (t *SysfsTuner) SetQueueDepth(depth int) error {
	path := t.queuePath("nr_requests")
	return os.WriteFile(path, []byte(strconv.Itoa(depth)), 0644)
}

// GetQueueDepth returns the current queue depth.
func (t *SysfsTuner) GetQueueDepth() (int, error) {
	path := t.queuePath("nr_requests")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

// SetNomerges controls request merging.
// 0 = enable merge, 1 = disable merge, 2 = disable merge only for simple requests
// For large sequential I/O, disabling merges can reduce overhead.
func (t *SysfsTuner) SetNomerges(value int) error {
	path := t.queuePath("nomerges")
	return os.WriteFile(path, []byte(strconv.Itoa(value)), 0644)
}

// GetNomerges returns the current nomerges setting.
func (t *SysfsTuner) GetNomerges() (int, error) {
	path := t.queuePath("nomerges")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

// TuneHDDReadAhead raises the kernel read-ahead window to 4MB for a block
// device, which lets large sequential transfers stream off a spinning disk
// instead of being throttled by the ~128KB default. It is best-effort: it only
// acts on /dev/ block-device paths and silently does nothing when the path is a
// regular file or the process lacks the privilege to write sysfs (non-root).
func TuneHDDReadAhead(path string) {
	if len(path) < 6 || path[:5] != "/dev/" {
		return
	}
	tuner := NewSysfsTuner(path)
	if prev, err := tuner.GetReadAhead(); err == nil && prev >= 4096 {
		return // already at/above target
	}
	if err := tuner.SetReadAhead(4096); err == nil {
		Log("tuned read-ahead to 4MB for %s\n", tuner.diskDevice)
	}
}

// OptimizeForHDD applies recommended settings for HDD optimization.
// This is a convenience function that sets all optimal values.
func (t *SysfsTuner) OptimizeForHDD() error {
	// 4MB read-ahead
	if err := t.SetReadAhead(4096); err != nil {
		return err
	}

	// Use deadline scheduler for predictable latency
	if err := t.SetIOScheduler("deadline"); err != nil {
		return err
	}

	// Increase queue depth for better throughput
	if err := t.SetQueueDepth(256); err != nil {
		return err
	}

	// Disable merges for large sequential I/O
	if err := t.SetNomerges(1); err != nil {
		return err
	}

	return nil
}

// PrintCurrentSettings outputs current sysfs settings for diagnostics.
func (t *SysfsTuner) PrintCurrentSettings() {
	fmt.Printf("Sysfs settings for %s:\n", t.diskDevice)

	if ra, err := t.GetReadAhead(); err == nil {
		fmt.Printf("  read_ahead_kb: %d KB\n", ra)
	}

	if sched, err := t.GetIOScheduler(); err == nil {
		fmt.Printf("  scheduler: %s", sched)
	}

	if qd, err := t.GetQueueDepth(); err == nil {
		fmt.Printf("  nr_requests: %d\n", qd)
	}

	if nm, err := t.GetNomerges(); err == nil {
		fmt.Printf("  nomerges: %d\n", nm)
	}
}
