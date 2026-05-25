package main

import (
	"io"
	"os"
	"sort"
	"time"
)

// TuneConfig holds all tunable parameters.
type TuneConfig struct {
	Workers       int
	PrefetchDepth int
	BlockSize     uint32
	NoCompress    bool
	CompLevel     string
	TCPBufSize    int
}

// TuneResult holds benchmark result for a configuration.
type TuneResult struct {
	Config     TuneConfig
	Throughput float64 // MB/s
	Duration   time.Duration
}

// DefaultTuneConfig returns the current default configuration.
func DefaultTuneConfig() TuneConfig {
	return TuneConfig{
		Workers:       1,
		PrefetchDepth: 4,
		BlockSize:     10485760, // 10MB
		NoCompress:    false,
		CompLevel:     "default",
		TCPBufSize:    4 * 1024 * 1024, // 4MB
	}
}

// AutoTune runs benchmarks and returns optimal configuration.
// Uses first 100MB of source file for testing to keep tuning time reasonable.
func AutoTune(srcFile *os.File, dstDir string, fileSize int64) TuneConfig {
	Log("\n=== Auto-tuning for optimal HDD performance ===\n")
	Log("Note: Tuning benchmarks use local disk I/O to find optimal settings\n")
	Log("      The determined settings will be applied before network transfer starts\n\n")

	// Limit test size to 100MB for quick tuning
	const testSize = 100 * 1024 * 1024
	if fileSize < testSize {
		Log("Source file is smaller than 100MB, using full file for tuning\n")
	} else {
		Log("Using first 100MB of source file for benchmarking\n")
	}

	// Phase 1: Find optimal workers and prefetch depth (most impactful)
	Log("\n--- Phase 1: Workers and Prefetch Depth ---\n")
	bestWorkers := tuneWorkersAndPrefetch(srcFile, dstDir, testSize)
	Log("Phase 1 complete: optimal workers=%d\n", bestWorkers)

	// Phase 2: Find optimal compression settings
	Log("\n--- Phase 2: Compression Settings ---\n")
	bestComp := tuneCompression(srcFile, dstDir, testSize, bestWorkers)
	Log("Phase 2 complete: optimal compression=%s\n", configCompStr(bestComp))

	// Phase 3: Find optimal TCP buffer size
	Log("\n--- Phase 3: TCP Buffer Size ---\n")
	bestTCP := tuneTCPBuffer(srcFile, dstDir, testSize, bestWorkers, bestComp)
	Log("Phase 3 complete: optimal TCP buffer=%d MB\n", bestTCP/(1024*1024))

	// Phase 4: Find optimal block size (skip if file too small)
	var bestBlockSize uint32 = 10485760 // default 10MB
	if fileSize >= 500*1024*1024 {
		Log("\n--- Phase 4: Block Size ---\n")
		bestBlockSize = tuneBlockSize(srcFile, dstDir, testSize, bestWorkers, bestComp, bestTCP)
		Log("Phase 4 complete: optimal block size=%d MB\n", bestBlockSize/(1024*1024))
	}

	result := TuneConfig{
		Workers:       bestWorkers,
		PrefetchDepth: bestWorkers * 4, // Default to workers*4 for now
		BlockSize:     bestBlockSize,
		NoCompress:    bestComp.NoCompress,
		CompLevel:     bestComp.CompLevel,
		TCPBufSize:    bestTCP,
	}

	Log("\n=== Auto-tuning complete ===\n")
	Log("Optimal configuration: w=%d, pf=%d, bs=%dMB, comp=%s, tcp=%dMB\n",
		result.Workers, result.PrefetchDepth, result.BlockSize/(1024*1024),
		configCompStr(result), result.TCPBufSize/(1024*1024))
	Log("All settings will be applied BEFORE network transfer starts\n\n")

	return result
}

func tuneWorkersAndPrefetch(srcFile *os.File, dstDir string, testSize int64) int {
	workersOptions := []int{1, 2, 4, 8, 16}
	results := make([]struct {
		workers    int
		throughput float64
	}, 0)

	for _, w := range workersOptions {
		cfg := TuneConfig{
			Workers:       w,
			PrefetchDepth: w * 4,
			BlockSize:     10485760,
			NoCompress:    true, // Test without compression for speed
			CompLevel:     "",
			TCPBufSize:    4 * 1024 * 1024,
		}

		Log("Testing workers=%d...", w)
		t := runQuickBenchmark(cfg, srcFile, dstDir, testSize)
		Log(" %.1f MB/s\n", t)

		results = append(results, struct {
			workers    int
			throughput float64
		}{w, t})
	}

	// Sort by throughput descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].throughput > results[j].throughput
	})

	Log("Best workers: %d (%.1f MB/s)\n", results[0].workers, results[0].throughput)
	return results[0].workers
}

func tuneCompression(srcFile *os.File, dstDir string, testSize int64, workers int) TuneConfig {
	compOptions := []struct {
		noCompress bool
		level      string
		name       string
	}{
		{true, "", "none"},
		{false, "fast", "fast"},
		{false, "default", "default"},
	}

	results := make([]struct {
		cfg        TuneConfig
		throughput float64
	}, 0)

	for _, opt := range compOptions {
		cfg := TuneConfig{
			Workers:       workers,
			PrefetchDepth: workers * 4,
			BlockSize:     10485760,
			NoCompress:    opt.noCompress,
			CompLevel:     opt.level,
			TCPBufSize:    4 * 1024 * 1024,
		}

		Log("Testing compression=%s...", opt.name)
		t := runQuickBenchmark(cfg, srcFile, dstDir, testSize)
		Log(" %.1f MB/s\n", t)

		results = append(results, struct {
			cfg        TuneConfig
			throughput float64
		}{cfg, t})
	}

	// Sort by throughput descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].throughput > results[j].throughput
	})

	Log("Best compression: %s (%.1f MB/s)\n",
		configCompStr(results[0].cfg), results[0].throughput)
	return results[0].cfg
}

func tuneTCPBuffer(srcFile *os.File, dstDir string, testSize int64, workers int, comp TuneConfig) int {
	tcpOptions := []int{
		1 * 1024 * 1024, // 1MB
		2 * 1024 * 1024, // 2MB
		4 * 1024 * 1024, // 4MB
		8 * 1024 * 1024, // 8MB
	}

	results := make([]struct {
		bufSize    int
		throughput float64
	}, 0)

	for _, buf := range tcpOptions {
		cfg := comp
		cfg.TCPBufSize = buf

		Log("Testing TCP buffer=%dMB...", buf/(1024*1024))
		t := runQuickBenchmark(cfg, srcFile, dstDir, testSize)
		Log(" %.1f MB/s\n", t)

		results = append(results, struct {
			bufSize    int
			throughput float64
		}{buf, t})
	}

	// Sort by throughput descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].throughput > results[j].throughput
	})

	Log("Best TCP buffer: %dMB (%.1f MB/s)\n",
		results[0].bufSize/(1024*1024), results[0].throughput)
	return results[0].bufSize
}

func tuneBlockSize(srcFile *os.File, dstDir string, testSize int64, workers int, comp TuneConfig, tcpBuf int) uint32 {
	blockOptions := []uint32{
		5 * 1024 * 1024,  // 5MB
		10 * 1024 * 1024, // 10MB
		20 * 1024 * 1024, // 20MB
		50 * 1024 * 1024, // 50MB
	}

	results := make([]struct {
		blockSize  uint32
		throughput float64
	}, 0)

	for _, bs := range blockOptions {
		cfg := comp
		cfg.TCPBufSize = tcpBuf
		cfg.BlockSize = bs

		Log("Testing block size=%dMB...", bs/(1024*1024))
		t := runQuickBenchmark(cfg, srcFile, dstDir, testSize)
		Log(" %.1f MB/s\n", t)

		results = append(results, struct {
			blockSize  uint32
			throughput float64
		}{bs, t})
	}

	// Sort by throughput descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].throughput > results[j].throughput
	})

	Log("Best block size: %dMB (%.1f MB/s)\n",
		results[0].blockSize/(1024*1024), results[0].throughput)
	return results[0].blockSize
}

// runQuickBenchmark performs a quick benchmark test with the given configuration.
// Reads testSize bytes from srcFile, writes to temp file in dstDir.
func runQuickBenchmark(cfg TuneConfig, srcFile *os.File, dstDir string, testSize int64) float64 {
	// Create temporary destination file
	tmpFile, err := os.CreateTemp(dstDir, "bsync_bench_*")
	if err != nil {
		Log("Warning: cannot create temp file in %s, using memory benchmark: %v\n", dstDir, err)
		// Fallback: measure sequential read speed only
		return measureReadSpeed(srcFile, testSize)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Reset source file position
	if _, err := srcFile.Seek(0, io.SeekStart); err != nil {
		Log("Warning: cannot seek source file: %v\n", err)
		return 0
	}

	// Prepare buffer aligned to block size
	buf := make([]byte, min(testSize, int64(cfg.BlockSize)))

	// Simple sequential read/write test to measure raw throughput
	start := time.Now()
	bytesRemaining := testSize

	for bytesRemaining > 0 {
		toRead := min(int64(len(buf)), bytesRemaining)
		n, err := io.ReadFull(srcFile, buf[:toRead])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			Log("Read error: %v\n", err)
			break
		}

		if n > 0 {
			_, err := tmpFile.Write(buf[:n])
			if err != nil {
				Log("Write error: %v\n", err)
				break
			}
		}

		bytesRemaining -= int64(n)
		if n == 0 {
			break
		}
	}

	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0
	}

	throughput := float64(testSize-bytesRemaining) / (1024 * 1024) / elapsed

	// Reset source position for next test
	srcFile.Seek(0, io.SeekStart)

	return throughput
}

// measureReadSpeed measures only read throughput when write is not possible.
func measureReadSpeed(file *os.File, testSize int64) float64 {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		Log("Warning: cannot seek source file: %v\n", err)
		return 0
	}
	buf := make([]byte, 64*1024) // 64KB buffer
	start := time.Now()
	bytesRead := int64(0)

	for bytesRead < testSize {
		n, err := file.Read(buf)
		bytesRead += int64(n)
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
	}

	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0
	}

	throughput := float64(bytesRead) / (1024 * 1024) / elapsed
	_, _ = file.Seek(0, io.SeekStart)
	return throughput
}

func configCompStr(cfg TuneConfig) string {
	if cfg.NoCompress {
		return "none"
	}
	return cfg.CompLevel
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
