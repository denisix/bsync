package main

import (
	"sync"
	"testing"
	"time"
)

// TestInitStats tests initialization of stats
func TestInitStats(t *testing.T) {
	t0 := time.Now()
	InitStats(t0, 100)

	stats := GetStats()
	if stats.TotalBlocks != 100 {
		t.Errorf("TotalBlocks = %d, want 100", stats.TotalBlocks)
	}
	if stats.Diffs != 0 {
		t.Errorf("Diffs = %d, want 0", stats.Diffs)
	}
	if stats.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", stats.TotalBytes)
	}
	if stats.CompressedBytes != 0 {
		t.Errorf("CompressedBytes = %d, want 0", stats.CompressedBytes)
	}
	if !stats.StartTime.Equal(t0) {
		t.Errorf("StartTime = %v, want %v", stats.StartTime, t0)
	}
}

// TestUpdateStats tests updating stats with new data
func TestUpdateStats(t *testing.T) {
	InitStats(time.Now(), 100)

	UpdateStats(1, 1024, 512)
	UpdateStats(2, 2048, 1024)

	stats := GetStats()
	if stats.Diffs != 3 {
		t.Errorf("Diffs = %d, want 3", stats.Diffs)
	}
	if stats.TotalBytes != 3072 {
		t.Errorf("TotalBytes = %d, want 3072", stats.TotalBytes)
	}
	if stats.CompressedBytes != 1536 {
		t.Errorf("CompressedBytes = %d, want 1536", stats.CompressedBytes)
	}
}

// TestThroughputMBps tests throughput calculation
func TestThroughputMBps(t *testing.T) {
	t.Run("normal throughput", func(t *testing.T) {
		t0 := time.Now()
		InitStats(t0, 100)

		// Update with 10MB of data
		UpdateStats(0, 10*1024*1024, 5*1024*1024)

		// Small sleep to ensure elapsed time > 0
		time.Sleep(10 * time.Millisecond)

		throughput := ThroughputMBps()
		if throughput <= 0 {
			t.Errorf("ThroughputMBps = %f, want > 0", throughput)
		}
		// Throughput should be reasonable (10MB in at least 10ms = max ~1000 MB/s)
		if throughput > 2000 {
			t.Errorf("ThroughputMBps = %f, seems unreasonably high", throughput)
		}
	})

	t.Run("zero elapsed time", func(t *testing.T) {
		t0 := time.Now()
		InitStats(t0, 100)

		// Don't sleep, elapsed time will be 0 or very close
		throughput := ThroughputMBps()
		if throughput < 0 {
			t.Errorf("ThroughputMBps = %f, want >= 0", throughput)
		}
	})

	t.Run("no data transferred", func(t *testing.T) {
		t0 := time.Now()
		InitStats(t0, 100)

		time.Sleep(10 * time.Millisecond)

		throughput := ThroughputMBps()
		if throughput != 0 {
			t.Errorf("ThroughputMBps = %f, want 0 when no data transferred", throughput)
		}
	})
}

// TestStatsConcurrency tests concurrent stats updates
func TestStatsConcurrency(t *testing.T) {
	InitStats(time.Now(), 1000)

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				UpdateStats(1, 1024, 512)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	stats := GetStats()
	// 10 goroutines * 100 iterations = 1000
	if stats.Diffs != 1000 {
		t.Errorf("Diffs = %d, want 1000", stats.Diffs)
	}
	// 10 * 100 * 1024 = 1024000
	if stats.TotalBytes != 1024000 {
		t.Errorf("TotalBytes = %d, want 1024000", stats.TotalBytes)
	}
	// 10 * 100 * 512 = 512000
	if stats.CompressedBytes != 512000 {
		t.Errorf("CompressedBytes = %d, want 512000", stats.CompressedBytes)
	}
}

// TestStatsZeroValues tests stats with zero values
func TestStatsZeroValues(t *testing.T) {
	t0 := time.Now()
	InitStats(t0, 0)

	stats := GetStats()
	if stats.TotalBlocks != 0 {
		t.Errorf("TotalBlocks = %d, want 0", stats.TotalBlocks)
	}

	// Update with zero values
	UpdateStats(0, 0, 0)
	stats = GetStats()
	if stats.Diffs != 0 {
		t.Errorf("Diffs = %d, want 0", stats.Diffs)
	}
}

// TestStatsGetSnapshot tests that GetStats returns a snapshot
func TestStatsGetSnapshot(t *testing.T) {
	InitStats(time.Now(), 100)

	stats1 := GetStats()
	UpdateStats(1, 1024, 512)
	stats2 := GetStats()

	// stats1 should not have changed
	if stats1.Diffs != 0 {
		t.Errorf("stats1.Diffs = %d, want 0 (snapshot should not change)", stats1.Diffs)
	}

	// stats2 should reflect the update
	if stats2.Diffs != 1 {
		t.Errorf("stats2.Diffs = %d, want 1", stats2.Diffs)
	}
}

// TestStatsMutexProtected tests that stats are properly protected by mutex
func TestStatsMutexProtected(t *testing.T) {
	// This test is similar to TestStatsConcurrency but focuses on
	// verifying no race conditions occur
	InitStats(time.Now(), 10000)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				UpdateStats(1, 1024, 512)
				_ = GetStats()
			}
		}()
	}

	wg.Wait()

	stats := GetStats()
	if stats.Diffs != 5000 {
		t.Errorf("Diffs = %d, want 5000", stats.Diffs)
	}
}
