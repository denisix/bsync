package main

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeSysfsQueue(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	queue := filepath.Join(root, "block", "sda", "queue")
	if err := os.MkdirAll(queue, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"read_ahead_kb": "128",
		"scheduler":     "[mq-deadline] none",
		"nr_requests":   "128",
		"nomerges":      "0",
	} {
		if err := os.WriteFile(filepath.Join(queue, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSysfsTunerReadsAndOptimizesFakeTree(t *testing.T) {
	root := fakeSysfsQueue(t)
	tuner := NewSysfsTunerAt("/dev/sda", root)

	if got, err := tuner.GetReadAhead(); err != nil || got != 128 {
		t.Fatalf("GetReadAhead = (%d, %v), want (128, nil)", got, err)
	}
	if got, err := tuner.GetQueueDepth(); err != nil || got != 128 {
		t.Fatalf("GetQueueDepth = (%d, %v), want (128, nil)", got, err)
	}
	if got, err := tuner.GetNomerges(); err != nil || got != 0 {
		t.Fatalf("GetNomerges = (%d, %v), want (0, nil)", got, err)
	}
	if err := tuner.OptimizeForHDD(); err != nil {
		t.Fatalf("OptimizeForHDD: %v", err)
	}
	for name, want := range map[string]string{
		"read_ahead_kb": "4096",
		"scheduler":     "deadline",
		"nr_requests":   "256",
		"nomerges":      "1",
	} {
		got, err := os.ReadFile(tuner.queuePath(name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestSysfsTunerReportsInvalidOrMissingSettings(t *testing.T) {
	root := fakeSysfsQueue(t)
	tuner := NewSysfsTunerAt("sda", root)
	if err := os.WriteFile(tuner.queuePath("read_ahead_kb"), []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := tuner.GetReadAhead(); err == nil {
		t.Fatal("invalid numeric sysfs value unexpectedly parsed")
	}
	if err := os.RemoveAll(filepath.Dir(tuner.queuePath("scheduler"))); err != nil {
		t.Fatal(err)
	}
	if err := tuner.OptimizeForHDD(); err == nil {
		t.Fatal("optimization unexpectedly succeeded with missing scheduler entry")
	}
}
