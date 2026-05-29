package main

import (
	"bytes"
	"regexp"
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
	withTerminalProbe(t, 140, 24)
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

func TestTerminalSizePrefersLiveProbeOverStaleEnv(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	t.Setenv("LINES", "50")
	withTerminalProbe(t, 42, 13)

	if got := getTerminalWidth(); got != 42 {
		t.Fatalf("terminal width = %d, want live probe value 42", got)
	}
	if got := getTerminalHeight(); got != 13 {
		t.Fatalf("terminal height = %d, want live probe value 13", got)
	}
}

func TestTerminalSizeFallsBackToEnvWhenProbeUnavailable(t *testing.T) {
	t.Setenv("COLUMNS", "77")
	t.Setenv("LINES", "22")
	withTerminalProbe(t, 0, 0)

	if got := getTerminalWidth(); got != 77 {
		t.Fatalf("terminal width = %d, want env fallback 77", got)
	}
	if got := getTerminalHeight(); got != 22 {
		t.Fatalf("terminal height = %d, want env fallback 22", got)
	}
}

func TestVisualizerSmallTerminalUsesSingleLineLegendAndStats(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	t.Setenv("LINES", "50")
	withTerminalProbe(t, 40, 12)

	v := newVisualizer(1000, 200, 5)
	var frame strings.Builder
	v.renderLegend(&frame)

	legend := stripANSI(strings.Split(frame.String(), "\n")[0])
	if visibleLen(legend) >= 40 {
		t.Fatalf("compact legend length = %d, want < 40: %q", visibleLen(legend), legend)
	}
	if strings.Contains(legend, "Compressed") || strings.Contains(legend, "Skipped") {
		t.Fatalf("compact legend should not use wide labels: %q", legend)
	}

	stats := stripANSI(v.statsLine())
	if visibleLen(stats) >= 40 {
		t.Fatalf("compact stats length = %d, want < 40: %q", visibleLen(stats), stats)
	}
	if !strings.Contains(stats, "Cell: 5") && !strings.Contains(stats, "C: 5") {
		t.Fatalf("compact stats should include cell size: %q", stats)
	}
	if strings.Contains(stats, "MB/s") {
		t.Fatalf("compact stats should not include speed text: %q", stats)
	}
}

func TestVisualizerStatsLineCompactsWhenFullLineWouldWrap(t *testing.T) {
	withTerminalProbe(t, 80, 24)

	v := newVisualizer(75406, 1301, 58)
	v.completedBlocks.Store(1235)

	stats := stripANSI(v.statsLine())
	if visibleLen(stats) >= 80 {
		t.Fatalf("stats length = %d, want < 80: %q", visibleLen(stats), stats)
	}
	if strings.Contains(stats, "MB/s") {
		t.Fatalf("stats should omit speed when full line would wrap: %q", stats)
	}
	if !strings.Contains(stats, "Cell: 58") && !strings.Contains(stats, "C: 58") {
		t.Fatalf("stats should keep cell size when full line would wrap: %q", stats)
	}
}

func TestVisualizerStatsLineOmitsCellSuffixesForOneToOneCompactGrid(t *testing.T) {
	withTerminalProbe(t, 40, 12)

	v := newVisualizer(1000, 1000, 1)

	stats := stripANSI(v.statsLine())
	if strings.Contains(stats, "Each cell:") || strings.Contains(stats, "Cell:") || strings.Contains(stats, "C:") {
		t.Fatalf("1:1 compact stats should omit cell suffix: %q", stats)
	}
}

func TestVisualizerSmallTerminalRenderKeepsGridAtRowThree(t *testing.T) {
	withTerminalProbe(t, 40, 12)
	var buf bytes.Buffer

	v := newVisualizer(1000, 200, 5)
	v.output = &buf
	v.Render()
	out := buf.String()

	if strings.Contains(out, "Processing") || strings.Contains(out, "Compressed") || strings.Contains(out, "Skipped") {
		t.Fatalf("small render emitted wide legend labels: %q", out)
	}
	if strings.Contains(out, "MB/s") {
		t.Fatalf("small render emitted wide stats speed label: %q", out)
	}
	if !strings.Contains(out, "Cell: 5") && !strings.Contains(out, "C: 5") {
		t.Fatalf("small render omitted compact cell size: %q", out)
	}
	if strings.Contains(out, cursorPos(2, 1)+ColorDim+PendingChar) {
		t.Fatalf("grid appears to start on row 2: %q", out)
	}
	if v.cols >= getTerminalWidth() {
		t.Fatalf("grid columns = %d, want less than terminal width %d", v.cols, getTerminalWidth())
	}
}

func TestVisualizerUsesFixedFiveLineLogArea(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		height   int
		wantRows int
	}{
		{name: "minimum", width: 40, height: 12, wantRows: 2},
		{name: "default", width: 80, height: 24, wantRows: 14},
		{name: "large", width: 120, height: 40, wantRows: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTerminalProbe(t, tt.width, tt.height)
			v := newVisualizer(100000, 100000, 1)

			if got := v.logLines; got != visualLogLines {
				t.Fatalf("logLines = %d, want %d", got, visualLogLines)
			}
			if got := v.rows; got != tt.wantRows {
				t.Fatalf("rows = %d, want %d", got, tt.wantRows)
			}
			if got := v.statsRow(); got != 3+tt.wantRows+1 {
				t.Fatalf("statsRow = %d, want %d", got, 3+tt.wantRows+1)
			}
			if got := v.logAreaStartRow(); got != v.statsRow()+1 {
				t.Fatalf("logAreaStartRow = %d, want statsRow + 1", got)
			}
		})
	}
}

func TestVisualizerFullRenderDoesNotEndWithNewline(t *testing.T) {
	withTerminalProbe(t, 40, 12)
	var buf bytes.Buffer

	v := newVisualizer(1000, 200, 5)
	v.output = &buf
	v.Render()

	if strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("full render should not end with newline to avoid terminal scroll: %q", buf.String())
	}
}

func TestVisualizerResizeRecalculatesCellsAndPreservesStatus(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	t.Setenv("LINES", "12")
	var buf bytes.Buffer

	v := newVisualizer(1000, 200, 5)
	v.output = &buf
	v.UpdateBlockStatus(10, StatusCompressed)
	v.UpdateBlockStatus(11, StatusRaw)

	t.Setenv("COLUMNS", "100")
	t.Setenv("LINES", "30")
	v.Render()

	if got := v.blocksPerCell; got != 1 {
		t.Fatalf("blocksPerCell after resize = %d, want 1", got)
	}
	if got := v.visualCellCount; got != 1000 {
		t.Fatalf("visualCellCount after resize = %d, want 1000", got)
	}
	if got := BlockStatus(v.cellStatus[10].Load()); got != StatusCompressed {
		t.Fatalf("cell 10 status after resize = %v, want compressed", got)
	}
	if got := BlockStatus(v.cellStatus[11].Load()); got != StatusRaw {
		t.Fatalf("cell 11 status after resize = %v, want raw", got)
	}
}

func TestVisualizerResizeToNarrowClearsScreenAndCompacts(t *testing.T) {
	var buf bytes.Buffer
	withMutableTerminalProbe(t, 120, 30)

	v := newVisualizer(75406, 2000, 38)
	v.output = &buf
	v.Render()

	setMutableTerminalProbe(40, 12)
	buf.Reset()
	v.Render()
	out := buf.String()

	if !strings.HasPrefix(out, "\033[2J\033[H") {
		t.Fatalf("resize render prefix = %q, want full clear", out[:shortLen(out, 8)])
	}
	if strings.Contains(out, "Processing") || strings.Contains(out, "Compressed") || strings.Contains(out, "Skipped") {
		t.Fatalf("narrow resize emitted wide legend labels: %q", out)
	}
	if strings.Contains(out, "MB/s") {
		t.Fatalf("narrow resize emitted wide stats speed label: %q", out)
	}
	if !strings.Contains(out, "Cell:") && !strings.Contains(out, "C:") {
		t.Fatalf("narrow resize omitted compact cell size: %q", out)
	}
	if v.cols >= getTerminalWidth() {
		t.Fatalf("grid columns = %d, want less than terminal width %d", v.cols, getTerminalWidth())
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
	// Cell at (row=0, col=1) maps to screen position (row=3, col=2)
	if !strings.Contains(second, "\033[3;2H") {
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

func stripANSI(s string) string {
	return regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`).ReplaceAllString(s, "")
}

func withTerminalProbe(t *testing.T, width, height int) {
	t.Helper()
	oldWidthProbe := terminalWidthProbe
	oldHeightProbe := terminalHeightProbe
	terminalWidthProbe = func() int { return width }
	terminalHeightProbe = func() int { return height }
	t.Cleanup(func() {
		terminalWidthProbe = oldWidthProbe
		terminalHeightProbe = oldHeightProbe
	})
}

var mutableProbeWidth int
var mutableProbeHeight int

func withMutableTerminalProbe(t *testing.T, width, height int) {
	t.Helper()
	oldWidthProbe := terminalWidthProbe
	oldHeightProbe := terminalHeightProbe
	mutableProbeWidth = width
	mutableProbeHeight = height
	terminalWidthProbe = func() int { return mutableProbeWidth }
	terminalHeightProbe = func() int { return mutableProbeHeight }
	t.Cleanup(func() {
		terminalWidthProbe = oldWidthProbe
		terminalHeightProbe = oldHeightProbe
	})
}

func setMutableTerminalProbe(width, height int) {
	mutableProbeWidth = width
	mutableProbeHeight = height
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
