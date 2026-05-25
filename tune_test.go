package main

import (
	"os"
	"testing"
)

// TestDefaultTuneConfig tests the default configuration values
func TestDefaultTuneConfig(t *testing.T) {
	cfg := DefaultTuneConfig()

	if cfg.Workers != 1 {
		t.Errorf("Workers = %d, want 1", cfg.Workers)
	}
	if cfg.PrefetchDepth != 4 {
		t.Errorf("PrefetchDepth = %d, want 4", cfg.PrefetchDepth)
	}
	if cfg.BlockSize != 10485760 {
		t.Errorf("BlockSize = %d, want 10485760 (10MB)", cfg.BlockSize)
	}
	// Default is to compress (NoCompress = false)
	if cfg.NoCompress {
		t.Errorf("NoCompress = true, want false (default is compression enabled)")
	}
	if cfg.CompLevel != "default" {
		t.Errorf("CompLevel = %q, want %q", cfg.CompLevel, "default")
	}
	if cfg.TCPBufSize != 4*1024*1024 {
		t.Errorf("TCPBufSize = %d, want %d (4MB)", cfg.TCPBufSize, 4*1024*1024)
	}
}

// TestConfigCompStr tests the config compression string formatter
func TestConfigCompStr(t *testing.T) {
	tests := []struct {
		name     string
		cfg      TuneConfig
		expected string
	}{
		{
			name:     "no compress",
			cfg:      TuneConfig{NoCompress: true},
			expected: "none",
		},
		{
			name:     "fast compress",
			cfg:      TuneConfig{NoCompress: false, CompLevel: "fast"},
			expected: "fast",
		},
		{
			name:     "default compress",
			cfg:      TuneConfig{NoCompress: false, CompLevel: "default"},
			expected: "default",
		},
		{
			name:     "better compress",
			cfg:      TuneConfig{NoCompress: false, CompLevel: "better"},
			expected: "better",
		},
		{
			name:     "best compress",
			cfg:      TuneConfig{NoCompress: false, CompLevel: "best"},
			expected: "best",
		},
		{
			name:     "empty comp level with compress",
			cfg:      TuneConfig{NoCompress: false, CompLevel: ""},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := configCompStr(tt.cfg)
			if result != tt.expected {
				t.Errorf("configCompStr() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestMin tests the min helper function
func TestMin(t *testing.T) {
	tests := []struct {
		name     string
		a        int64
		b        int64
		expected int64
	}{
		{"a less than b", 5, 10, 5},
		{"b less than a", 10, 5, 5},
		{"equal values", 7, 7, 7},
		{"a negative", -5, 10, -5},
		{"b negative", 10, -5, -5},
		{"both negative", -10, -5, -10},
		{"a zero", 0, 5, 0},
		{"b zero", 5, 0, 0},
		{"both zero", 0, 0, 0},
		{"large values", 1000000000, 2000000000, 1000000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := min(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("min(%d, %d) = %d, want %d", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

// TestTuneConfigFields tests TuneConfig struct fields
func TestTuneConfigFields(t *testing.T) {
	cfg := TuneConfig{
		Workers:       8,
		PrefetchDepth: 32,
		BlockSize:     20 * 1024 * 1024,
		NoCompress:    false,
		CompLevel:     "fast",
		TCPBufSize:    8 * 1024 * 1024,
	}

	if cfg.Workers != 8 {
		t.Errorf("Workers = %d, want 8", cfg.Workers)
	}
	if cfg.PrefetchDepth != 32 {
		t.Errorf("PrefetchDepth = %d, want 32", cfg.PrefetchDepth)
	}
	if cfg.BlockSize != 20*1024*1024 {
		t.Errorf("BlockSize = %d, want %d", cfg.BlockSize, 20*1024*1024)
	}
	if cfg.NoCompress {
		t.Errorf("NoCompress = true, want false")
	}
	if cfg.CompLevel != "fast" {
		t.Errorf("CompLevel = %q, want %q", cfg.CompLevel, "fast")
	}
	if cfg.TCPBufSize != 8*1024*1024 {
		t.Errorf("TCPBufSize = %d, want %d", cfg.TCPBufSize, 8*1024*1024)
	}
}

func TestRunQuickBenchmarkSmallFile(t *testing.T) {
	srcPath := writeTempFile(t, []byte("small benchmark input"))
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cfg := DefaultTuneConfig()
	throughput := runQuickBenchmark(cfg, src, t.TempDir(), 21)
	if throughput <= 0 {
		t.Fatalf("runQuickBenchmark throughput = %v, want positive value", throughput)
	}
}

func TestRunQuickBenchmarkReadFallback(t *testing.T) {
	srcPath := writeTempFile(t, []byte("fallback benchmark input"))
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cfg := DefaultTuneConfig()
	throughput := runQuickBenchmark(cfg, src, filepathThatDoesNotExist(t), 24)
	if throughput <= 0 {
		t.Fatalf("fallback throughput = %v, want positive value", throughput)
	}
	if second := runQuickBenchmark(cfg, src, filepathThatDoesNotExist(t), 24); second <= 0 {
		t.Fatalf("repeated fallback throughput = %v, want positive value", second)
	}
}

func TestRunQuickBenchmarkEmptyFileTerminates(t *testing.T) {
	srcPath := writeTempFile(t, nil)
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if got := runQuickBenchmark(DefaultTuneConfig(), src, t.TempDir(), 1024); got != 0 {
		t.Fatalf("empty benchmark throughput = %v, want 0", got)
	}
}

func filepathThatDoesNotExist(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/missing"
}
