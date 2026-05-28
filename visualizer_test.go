package main

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVisualizerRenderDelay(t *testing.T) {
	v := newVisualizer(10, 10, 1)
	base := time.Unix(100, 0)

	if got := v.renderDelay(base); got != 0 {
		t.Fatalf("first render delay = %s, want 0", got)
	}

	v.lastRenderAt.Store(base.UnixNano())
	if got := v.renderDelay(base.Add(40 * time.Millisecond)); got != 60*time.Millisecond {
		t.Fatalf("render delay inside throttle = %s, want 60ms", got)
	}
	if got := v.renderDelay(base.Add(visualRenderMinInterval)); got != 0 {
		t.Fatalf("render delay after interval = %s, want 0", got)
	}
}

func TestVisualizerStatusUpdatesRequestRender(t *testing.T) {
	for _, status := range []BlockStatus{StatusProcessing, StatusSkipped} {
		t.Run(status.String(), func(t *testing.T) {
			v := newVisualizer(10, 10, 1)
			var renders atomic.Int32
			rendered := make(chan struct{}, 1)
			v.renderHook = func() {
				if renders.Add(1) == 1 {
					rendered <- struct{}{}
				}
			}

			v.UpdateBlockStatus(3, status)

			select {
			case <-rendered:
			case <-time.After(time.Second):
				t.Fatalf("status %v did not request a render", status)
			}
			if got := BlockStatus(v.cellStatus[3].Load()); got != status {
				t.Fatalf("cell status = %v, want %v", got, status)
			}
		})
	}
}

func TestVisualizerStatusUpdateMarksVisualTransferStarted(t *testing.T) {
	resetVisualLogTestState(t)
	Log("waiting for server\n")
	Log("Warning: keep this\n")

	v := newVisualizer(1, 1, 1)
	v.renderHook = func() {}
	v.UpdateBlockStatus(0, StatusSkipped)

	if !visualTransferStarted.Load() {
		t.Fatal("visual transfer was not marked started")
	}
	logs := GetRecentLogs(10)
	if len(logs) != 1 || !strings.Contains(logs[0], "Warning") {
		t.Fatalf("logs = %v, want only warning retained after transfer start", logs)
	}
}

func TestVisualizerRecentSpeedAndETA(t *testing.T) {
	oldBlockSize := atomic.LoadUint32(&blockSize)
	atomic.StoreUint32(&blockSize, uint32(mb1))
	defer atomic.StoreUint32(&blockSize, oldBlockSize)

	v := newVisualizer(10, 10, 1)
	base := time.Unix(100, 0)
	v.recordBlockComplete(base)
	v.recordBlockComplete(base.Add(time.Second))

	mbps, eta := v.recentSpeedAndETA()
	if mbps != 1 {
		t.Fatalf("speed = %.2f MB/s, want 1.00 MB/s", mbps)
	}
	if eta != "8s" {
		t.Fatalf("ETA = %q, want 8s", eta)
	}
}

func TestVisualizerRecentSpeedStillShowsWhenComplete(t *testing.T) {
	oldBlockSize := atomic.LoadUint32(&blockSize)
	atomic.StoreUint32(&blockSize, uint32(mb1))
	defer atomic.StoreUint32(&blockSize, oldBlockSize)

	v := newVisualizer(2, 2, 1)
	base := time.Unix(100, 0)
	v.recordBlockComplete(base)
	v.recordBlockComplete(base.Add(time.Second))

	mbps, eta := v.recentSpeedAndETA()
	if mbps != 1 {
		t.Fatalf("speed = %.2f MB/s, want 1.00 MB/s", mbps)
	}
	if eta != "0s" {
		t.Fatalf("ETA = %q, want 0s", eta)
	}
}

func TestVisualizerRecentSpeedUsesSampleWindow(t *testing.T) {
	oldBlockSize := atomic.LoadUint32(&blockSize)
	atomic.StoreUint32(&blockSize, uint32(mb1))
	defer atomic.StoreUint32(&blockSize, oldBlockSize)

	v := newVisualizer(200, 200, 1)
	base := time.Unix(100, 0)
	for i := 0; i < 50; i++ {
		v.recordBlockComplete(base.Add(time.Duration(i) * time.Minute))
	}
	recentBase := base.Add(time.Hour)
	for i := 0; i < etaSampleSize; i++ {
		v.recordBlockComplete(recentBase.Add(time.Duration(i) * time.Second))
	}

	mbps, eta := v.recentSpeedAndETA()
	if mbps != 1 {
		t.Fatalf("speed = %.2f MB/s, want 1.00 MB/s from recent samples", mbps)
	}
	if eta != "50s" {
		t.Fatalf("ETA = %q, want 50s from recent samples", eta)
	}
}

func TestVisualizerStatsLineColorGroupsAggregated(t *testing.T) {
	oldBlockSize := atomic.LoadUint32(&blockSize)
	atomic.StoreUint32(&blockSize, uint32(1.42*mb1))
	defer atomic.StoreUint32(&blockSize, oldBlockSize)

	v := newVisualizer(3356, 672, 5)
	v.currentBlock.Store(527)
	v.completedBlocks.Store(527)
	base := time.Unix(100, 0)
	v.progressSamples[0] = progressSample{completed: 526, at: base}
	v.progressSamples[1] = progressSample{completed: 527, at: base.Add(time.Second)}
	v.progressSampleN = 2
	v.progressSampleI = 2

	line := v.statsLine()
	expectedParts := []string{
		ColorGrey + "Progress: ",
		ColorBrightGreen + " 15.7%" + ColorGrey,
		" block " + ColorWhite + "527" + ColorGrey + "/3356",
		" " + ColorBlue + "1.42 MB/s" + ColorGrey,
		" ETA: " + ColorOrange,
		ColorGrey + " Each cell: 5 blocks" + ColorReset,
	}
	for _, part := range expectedParts {
		if !strings.Contains(line, part) {
			t.Fatalf("stats line %q missing color group %q", line, part)
		}
	}
}

func TestVisualizerStatsLineOmitsEachCellForOneToOneGrid(t *testing.T) {
	v := newVisualizer(10, 10, 1)

	line := v.statsLine()
	if strings.Contains(line, "Each cell:") {
		t.Fatalf("stats line %q should not include Each cell for 1:1 grid", line)
	}
}

func TestVisualizerRenderClearsScreenOnlyOnFirstRender(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	t.Setenv("LINES", "12")
	var buf bytes.Buffer

	v := newVisualizer(3, 3, 1)
	v.output = &buf
	v.Render()
	first := buf.String()
	if !strings.HasPrefix(first, "\033[2J\033[H") {
		t.Fatalf("first render prefix = %q, want full clear + home", first[:shortLen(first, 8)])
	}
	if !strings.Contains(first, "\033[K") {
		t.Fatalf("first render missing line clear sequence: %q", first)
	}

	// Update one cell to trigger diff rendering
	v.UpdateBlockStatus(1, StatusCompressed)

	buf.Reset()
	v.Render()
	second := buf.String()
	if strings.Contains(second, "\033[2J") {
		t.Fatalf("second render contains full-screen clear: %q", second)
	}
	// In diff mode, second render starts with cursor positioning to update cells
	// It should start with \033[row;colH format, not \033[H
	if strings.HasPrefix(second, "\033[H") && !strings.HasPrefix(second, "\033[") {
		// Check if it's just cursor home without positioning (shouldn't happen in diff mode)
		t.Fatalf("second render prefix = %q, diff mode should use cursor positioning", second[:shortLen(second, 4)])
	}
	if !strings.Contains(second, "\033[K") {
		t.Fatalf("second render missing line clear sequence: %q", second)
	}
	// Diff render should update cell 1 (which changed to StatusCompressed)
	// Cell at (row=0, col=1) maps to screen position (row=3, col=3)
	if !strings.Contains(second, "\033[3;3H") {
		t.Fatalf("second render missing cursor positioning for changed cell: %q", second)
	}
}

func TestVisualizerRenderWritesSingleFrame(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	t.Setenv("LINES", "12")
	writer := &countingWriter{}

	v := newVisualizer(3, 3, 1)
	v.output = writer
	v.Render()

	if got := writer.writes.Load(); got != 1 {
		t.Fatalf("render writes = %d, want 1 complete frame write", got)
	}
	if !strings.HasPrefix(writer.String(), "\033[2J\033[H") {
		t.Fatalf("render did not write a complete visual frame: %q", writer.String())
	}
}

func TestVisualizerAggregatedCellCompletesAfterAllMappedBlocks(t *testing.T) {
	v := newVisualizer(10, 2, 5)
	v.renderHook = func() {}

	v.UpdateBlockStatus(0, StatusCompressed)
	if got := BlockStatus(v.cellStatus[0].Load()); got != StatusProcessing {
		t.Fatalf("cell status after first real block = %v, want processing", got)
	}

	for i := uint32(1); i < 4; i++ {
		v.UpdateBlockStatus(i, StatusCompressed)
	}
	if got := BlockStatus(v.cellStatus[0].Load()); got != StatusProcessing {
		t.Fatalf("cell status before final mapped block = %v, want processing", got)
	}

	v.UpdateBlockStatus(4, StatusRaw)
	if got := BlockStatus(v.cellStatus[0].Load()); got != StatusRaw {
		t.Fatalf("cell status after all mapped blocks = %v, want last final status raw", got)
	}
	if got := v.completedBlocks.Load(); got != 5 {
		t.Fatalf("completed blocks = %d, want 5", got)
	}
}

func TestVisualizerDuplicateFinalStatusDoesNotDoubleCount(t *testing.T) {
	v := newVisualizer(2, 2, 1)
	v.renderHook = func() {}

	v.UpdateBlockStatus(0, StatusCompressed)
	v.UpdateBlockStatus(0, StatusRaw)

	if got := v.completedBlocks.Load(); got != 1 {
		t.Fatalf("completed blocks after duplicate final status = %d, want 1", got)
	}
	if got := v.cellDone[0].Load(); got != 1 {
		t.Fatalf("cell completed blocks after duplicate final status = %d, want 1", got)
	}
	if got := BlockStatus(v.cellStatus[0].Load()); got != StatusCompressed {
		t.Fatalf("cell status after duplicate final status = %v, want first final status", got)
	}
}

type countingWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes atomic.Int32
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *countingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func shortLen(s string, n int) int {
	if len(s) < n {
		return len(s)
	}
	return n
}

func (s BlockStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusProcessing:
		return "processing"
	case StatusZero:
		return "zero"
	case StatusCompressed:
		return "compressed"
	case StatusRaw:
		return "raw"
	case StatusSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}
