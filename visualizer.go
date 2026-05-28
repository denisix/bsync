package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BlockStatus represents the transfer status of a single block
type BlockStatus int32

const (
	StatusPending    BlockStatus = iota // Not yet processed
	StatusProcessing                    // Currently being processed (blinks)
	StatusZero                          // Zero block (sparse, no data transferred)
	StatusCompressed                    // Compressed block transfer
	StatusRaw                           // Non-compressed block transfer
	StatusSkipped                       // Skipped/equal block (checksum match)
)

// ANSI color codes
const (
	ColorReset       = "\033[0m"
	ColorGrey        = "\033[90m"       // Zero blocks
	ColorBlue        = "\033[94m"       // Compressed / speed
	ColorOrange      = "\033[38;5;208m" // Non-compressed / ETA orange
	ColorGreen       = "\033[32m"       // Skipped/equal (green)
	ColorBrightGreen = "\033[92m"       // Highlighted percentage
	ColorWhite       = "\033[97m"       // Highlighted stat values
	ColorDim         = "\033[2m"        // Dim text for legend/pending
	ColorBlink       = "\033[5m"        // Slow blink (for processing indicator)
)

const (
	BlockChar   = "■" // Filled square for processed blocks
	PendingChar = "□" // Empty square for pending/processing blocks
)

const (
	visualRenderMinInterval = 100 * time.Millisecond
	etaSampleSize           = 100
	visualHeightRatio       = 0.9
)

type progressSample struct {
	completed uint32
	at        time.Time
}

var visualOutput io.Writer = os.Stdout

// Visualizer manages block-by-block progress visualization
type Visualizer struct {
	realBlockCount  uint32         // Actual number of source blocks
	visualCellCount uint32         // Number of visual cells in the grid
	blocksPerCell   uint32         // How many real blocks each visual cell represents
	cellStatus      []atomic.Int32 // Per-cell status (thread-safe)
	realBlockDone   []atomic.Bool
	cellDone        []atomic.Uint32
	currentBlock    atomic.Int32 // Real block index of most recently processing block
	completedBlocks atomic.Uint32
	lastRenderAt    atomic.Int64
	renderRequested atomic.Int32
	renderInFlight  atomic.Int32
	renderStopped   atomic.Int32
	progressMu      sync.Mutex
	progressSamples [etaSampleSize]progressSample
	progressSampleN int
	progressSampleI int
	mu              sync.Mutex
	enabled         bool
	rows            int
	cols            int
	logLines        int  // Lines reserved for log text at the bottom
	initialRendered bool // Whether the initial pending-state render was done
	screenRendered  bool
	renderedLines   int
	renderHook      func()
	output          io.Writer
	lastCellStatus  []BlockStatus  // Last displayed status for each cell
	lastWidth       int             // Last terminal width (for resize detection)
	lastHeight      int             // Last terminal height (for resize detection)
	lastLogLines    []string        // Last log lines displayed
	needsFullRender atomic.Bool     // Force full redraw on resize
}

// terminalGridSize returns the grid dimensions for the visual block area only.
// 90% of terminal height goes to the visual grid (legend + grid + stats),
// 10% is reserved for text logs below.
func terminalGridSize() (cols, maxGridRows, maxCells int) {
	width := getTerminalWidth()
	height := getTerminalHeight()
	if width < 20 {
		width = 80
	}
	if height < 10 {
		height = 24
	}

	cols = width - 2
	if cols < 10 {
		cols = 10
	}

	// 90% for visual grid, 10% for text logs
	visualLines := int(float64(height) * visualHeightRatio)
	if visualLines < 8 {
		visualLines = 8
	}

	// Overhead: legend(1) + blank(1) + pre-stats blank(1) + stats(1)
	maxGridRows = visualLines - 4
	if maxGridRows < 3 {
		maxGridRows = 3
	}

	maxCells = cols * maxGridRows
	return
}

// NewVisualizer creates a new visualizer instance.
// The number of visual cells is dynamically computed from the current terminal
// size so the grid fills the screen. For small block counts every block gets
// its own cell (1:1); for large counts blocks are aggregated into cells.
func NewVisualizer(blockCount uint32) *Visualizer {
	if blockCount == 0 {
		return &Visualizer{enabled: false}
	}

	_, _, maxCells := terminalGridSize()

	bpc := uint32(1)
	if blockCount > uint32(maxCells) {
		// How many real blocks per visual cell, rounding up
		bpc = (blockCount + uint32(maxCells) - 1) / uint32(maxCells)
		// How many visual cells needed = ceil(blockCount / bpc).
		// This is ≤ maxCells by construction; for blockCount just over
		// maxCells it avoids allocating cells that never get mapped.
		visualCells := (blockCount + bpc - 1) / bpc
		return newVisualizer(blockCount, visualCells, bpc)
	}

	return newVisualizer(blockCount, blockCount, 1)

}

// newVisualizer is the shared constructor that builds the Visualizer struct with
// the given parameters. It exists so NewVisualizer can compute aggregation in
// two branches but share the struct setup code.
func newVisualizer(blockCount, visualCells, bpc uint32) *Visualizer {
	v := &Visualizer{
		realBlockCount:  blockCount,
		visualCellCount: visualCells,
		blocksPerCell:   bpc,
		cellStatus:      make([]atomic.Int32, visualCells),
		realBlockDone:   make([]atomic.Bool, blockCount),
		cellDone:        make([]atomic.Uint32, visualCells),
		enabled:         true,
		output:          visualOutput,
	}

	for i := uint32(0); i < visualCells; i++ {
		v.cellStatus[i].Store(int32(StatusPending))
	}
	v.lastCellStatus = make([]BlockStatus, visualCells)
	for i := uint32(0); i < visualCells; i++ {
		v.lastCellStatus[i] = StatusPending
	}

	v.calcGridDimensions()
	v.lastWidth = getTerminalWidth()
	v.lastHeight = getTerminalHeight()
	v.needsFullRender.Store(true)
	return v
}

// renderInitial renders the all-pending grid once at startup.
// Must be called after NewVisualizer before any block processing.
func (v *Visualizer) renderInitial() {
	if !v.enabled || v.initialRendered {
		return
	}
	v.initialRendered = true
	go v.Render()
}

// calcGridDimensions recalculates grid layout from the current terminal size.
// Called on every Render() to handle terminal resize. The cell array size
// (visualCellCount) is fixed at construction — only cols/rows/logLines adapt.
func (v *Visualizer) calcGridDimensions() {
	width := getTerminalWidth()
	height := getTerminalHeight()
	if width < 20 {
		width = 80
	}
	if height < 10 {
		height = 24
	}

	// 90% for visual grid section, 10% for text log area
	visualLines := int(float64(height) * visualHeightRatio)
	if visualLines < 8 {
		visualLines = 8
	}
	v.logLines = height - visualLines
	if v.logLines < 2 {
		v.logLines = 2
		visualLines = height - v.logLines
	}

	// Overhead: legend(1) + blank(1) + pre-stats blank(1) + stats(1)
	overhead := 4
	maxGridRows := visualLines - overhead
	if maxGridRows < 3 {
		maxGridRows = 3
	}

	v.cols = width - 2
	if v.cols < 10 {
		v.cols = 10
	}
	if v.cols > int(v.visualCellCount) {
		v.cols = int(v.visualCellCount)
	}

	neededRows := int((v.visualCellCount + uint32(v.cols) - 1) / uint32(v.cols))
	if neededRows > maxGridRows {
		neededRows = maxGridRows
	}
	v.rows = neededRows
}

// getTerminalWidth attempts to get the terminal width.
func getTerminalWidth() int {
	if cols := getEnvInt("COLUMNS"); cols > 0 {
		return cols
	}
	if width := getTerminalWidthIoctl(); width > 0 {
		return width
	}
	return 80
}

// getTerminalHeight attempts to get the terminal height.
func getTerminalHeight() int {
	if rows := getEnvInt("LINES"); rows > 0 {
		return rows
	}
	if height := getTerminalHeightIoctl(); height > 0 {
		return height
	}
	return 24
}

// getEnvInt reads an integer from environment variable
func getEnvInt(key string) int {
	val := os.Getenv(key)
	if val == "" {
		return 0
	}
	var result int
	if _, err := fmt.Sscanf(val, "%d", &result); err == nil && result > 0 {
		return result
	}
	return 0
}

// cursorPos returns ANSI escape to move cursor to row, col (1-based).
func cursorPos(row, col int) string {
	return fmt.Sprintf("\033[%d;%dH", row, col)
}

// cellPosition returns screen (row, col) for visual cell at idx (1-based).
// Grid starts at row 3 (after legend + blank line) with 2-column left margin.
func (v *Visualizer) cellPosition(idx uint32) (row, col int) {
	// +3 for legend(1) + blank(1) + baseline offset(1)
	// +2 for left margin
	row = 3 + int(idx)/v.cols
	col = 2 + int(idx)%v.cols
	return
}

// renderCellAt renders a single cell at its screen position.
func (v *Visualizer) renderCellAt(frame *strings.Builder, row, col int, status BlockStatus) {
	frame.WriteString(cursorPos(row, col))
	v.renderBlock(frame, status)
}

// statsRow returns the screen row where stats line is displayed (1-based).
func (v *Visualizer) statsRow() int {
	// After legend(1) + blank(1) + grid rows(v.rows) + blank(1)
	return 3 + v.rows + 1
}

// logAreaStartRow returns the starting row for the log area (1-based).
func (v *Visualizer) logAreaStartRow() int {
	return v.statsRow() + 1
}

// mapToCell converts a real block index to a visual cell index.
func (v *Visualizer) mapToCell(blockIdx uint32) uint32 {
	cellIdx := blockIdx / v.blocksPerCell
	if cellIdx >= v.visualCellCount {
		cellIdx = v.visualCellCount - 1
	}
	return cellIdx
}

func isFinalBlockStatus(status BlockStatus) bool {
	return status == StatusZero || status == StatusCompressed || status == StatusRaw || status == StatusSkipped
}

func (v *Visualizer) realBlocksInCell(cellIdx uint32) uint32 {
	start := cellIdx * v.blocksPerCell
	if start >= v.realBlockCount {
		return 0
	}
	end := start + v.blocksPerCell
	if end > v.realBlockCount {
		end = v.realBlockCount
	}
	return end - start
}

// UpdateBlockStatus updates the status for the visual cell covering the given block (thread-safe).
// If the new status is StatusProcessing, currentBlock is updated atomically.
func (v *Visualizer) UpdateBlockStatus(blockIdx uint32, status BlockStatus) {
	if !v.enabled || blockIdx >= v.realBlockCount {
		return
	}
	cellIdx := v.mapToCell(blockIdx)
	if status == StatusProcessing {
		v.cellStatus[cellIdx].CompareAndSwap(int32(StatusPending), int32(StatusProcessing))
		v.currentBlock.Store(int32(blockIdx))
	}
	if isFinalBlockStatus(status) {
		if v.realBlockDone[blockIdx].CompareAndSwap(false, true) {
			v.recordBlockComplete(time.Now())
			doneInCell := v.cellDone[cellIdx].Add(1)
			if doneInCell >= v.realBlocksInCell(cellIdx) {
				v.cellStatus[cellIdx].Store(int32(status))
			} else {
				v.cellStatus[cellIdx].Store(int32(StatusProcessing))
			}
		}
	}
	if status != StatusPending {
		MarkVisualTransferStarted()
		v.RenderIfNeeded(blockIdx)
	}
}

func (v *Visualizer) recordBlockComplete(now time.Time) {
	completed := v.completedBlocks.Add(1)
	if completed > v.realBlockCount {
		v.completedBlocks.Store(v.realBlockCount)
		completed = v.realBlockCount
	}

	v.progressMu.Lock()
	v.progressSamples[v.progressSampleI] = progressSample{
		completed: completed,
		at:        now,
	}
	v.progressSampleI = (v.progressSampleI + 1) % etaSampleSize
	if v.progressSampleN < etaSampleSize {
		v.progressSampleN++
	}
	v.progressMu.Unlock()
}

func (v *Visualizer) writeFrame(frame string) {
	if v.output == nil {
		v.output = os.Stdout
	}
	_, _ = io.WriteString(v.output, frame)
	if syncer, ok := v.output.(interface{ Sync() error }); ok {
		_ = syncer.Sync()
	}
}

// Render draws the current state of the block grid
func (v *Visualizer) Render() {
	if !v.enabled {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Recalc grid dimensions every render (handles terminal resize)
	v.calcGridDimensions()

	// Check for terminal resize
	currentWidth := getTerminalWidth()
	currentHeight := getTerminalHeight()
	resized := v.lastWidth != currentWidth || v.lastHeight != currentHeight
	if resized {
		v.lastWidth = currentWidth
		v.lastHeight = currentHeight
		v.needsFullRender.Store(true)
	}

	fullRender := v.needsFullRender.Load()
	if fullRender {
		v.renderFull()
		v.needsFullRender.Store(false)
	} else {
		v.renderDiff()
	}

	// Update tracking state after rendering
	v.updateLastCellStatus()
	v.updateLastLogLines()
}

// renderFull performs a complete screen redraw.
func (v *Visualizer) renderFull() {
	var frame strings.Builder
	frame.Grow((v.cols*12 + 8) * (v.rows + v.logLines + 4))

	if !v.screenRendered {
		frame.WriteString("\033[2J\033[H")
		v.screenRendered = true
	} else {
		frame.WriteString("\033[H")
	}

	// Draw legend
	v.renderLegend(&frame)

	totalCells := v.visualCellCount

	// Draw grid
	for row := 0; row < v.rows; row++ {
		for col := 0; col < v.cols; col++ {
			idx := uint32(row*v.cols + col)
			if idx >= totalCells {
				frame.WriteString(" ")
				continue
			}
			status := BlockStatus(v.cellStatus[idx].Load())
			v.renderBlock(&frame, status)
		}
		frame.WriteString("\033[K\n")
	}

	// Draw stats
	v.renderStats(&frame)

	// Draw log messages in the bottom 10%
	v.renderLogs(&frame)

	renderedLines := 2 + v.rows + 2 + v.logLines
	for i := renderedLines; i < v.renderedLines; i++ {
		frame.WriteString("\033[K\n")
	}
	v.renderedLines = renderedLines

	v.writeFrame(frame.String())
}

// renderDiff performs incremental updates - only changed cells.
func (v *Visualizer) renderDiff() {
	var frame strings.Builder
	frame.Grow(256)

	totalCells := v.visualCellCount
	if uint32(len(v.lastCellStatus)) < totalCells {
		// State mismatch, fall back to full render
		v.needsFullRender.Store(true)
		return
	}

	// Update only changed cells
	for idx := uint32(0); idx < totalCells; idx++ {
		currentStatus := BlockStatus(v.cellStatus[idx].Load())
		if currentStatus != v.lastCellStatus[idx] {
			row, col := v.cellPosition(idx)
			v.renderCellAt(&frame, row, col, currentStatus)
		}
	}

	// Always update stats line (changes frequently)
	frame.WriteString(cursorPos(v.statsRow(), 1))
	frame.WriteString("\033[K")
	frame.WriteString(v.statsLine())

	// Update log area if changed
	currentLogs := GetRecentLogs(v.logLines)
	if !v.logsEqual(currentLogs) {
		v.renderLogsDiff(&frame, currentLogs)
	}

	v.writeFrame(frame.String())
}

// updateLastCellStatus syncs lastCellStatus with current cellStatus.
func (v *Visualizer) updateLastCellStatus() {
	totalCells := v.visualCellCount
	if uint32(len(v.lastCellStatus)) < totalCells {
		// Resize happened, reallocate
		v.lastCellStatus = make([]BlockStatus, totalCells)
	}
	for idx := uint32(0); idx < totalCells; idx++ {
		v.lastCellStatus[idx] = BlockStatus(v.cellStatus[idx].Load())
	}
}

// updateLastLogLines syncs lastLogLines with current log buffer.
func (v *Visualizer) updateLastLogLines() {
	v.lastLogLines = GetRecentLogs(v.logLines)
}

// logsEqual checks if current logs match lastLogLines.
func (v *Visualizer) logsEqual(current []string) bool {
	if len(v.lastLogLines) != len(current) {
		return false
	}
	for i := range current {
		if v.lastLogLines[i] != current[i] {
			return false
		}
	}
	return true
}

// renderLogsDiff performs incremental log area update.
func (v *Visualizer) renderLogsDiff(frame *strings.Builder, current []string) {
	startRow := v.logAreaStartRow()
	maxRow := startRow + v.logLines

	// Clear and redraw log area
	for row := startRow; row < maxRow; row++ {
		frame.WriteString(cursorPos(row, 1))
		frame.WriteString("\033[K")
	}

	// Display logs
	display := current
	if len(display) > v.logLines {
		display = display[len(display)-v.logLines:]
	}

	for i, line := range display {
		trimmed := strings.TrimRight(line, "\n\r")
		frame.WriteString(cursorPos(startRow+i, 1))
		frame.WriteString(ColorDim)
		frame.WriteString(trimmed)
		frame.WriteString(ColorReset)
	}
}

// renderLegend draws the color legend at the top
func (v *Visualizer) renderLegend(frame *strings.Builder) {
	frame.WriteString(ColorDim + "Legend: " + ColorReset)
	frame.WriteString(ColorDim + PendingChar + ColorReset + " Pending ")
	frame.WriteString(ColorBlink + ColorDim + PendingChar + ColorReset + " Processing ")
	frame.WriteString(ColorGrey + BlockChar + ColorReset + " Zero ")
	frame.WriteString(ColorBlue + BlockChar + ColorReset + " Compressed ")
	frame.WriteString(ColorOrange + BlockChar + ColorReset + " Raw ")
	frame.WriteString(ColorGreen + BlockChar + ColorReset + " Skipped\n\n")

}

// renderStats draws the compact stats line.
func (v *Visualizer) renderStats(frame *strings.Builder) {
	frame.WriteString("\033[K\n")
	frame.WriteString(v.statsLine())
	frame.WriteString("\033[K\n")
}

func (v *Visualizer) statsLine() string {
	done := v.completedBlocks.Load()
	if done > v.realBlockCount {
		done = v.realBlockCount
	}
	pctDone := 0.0
	if v.realBlockCount > 0 {
		pctDone = 100.0 * float64(done) / float64(v.realBlockCount)
	}

	current := v.currentBlock.Load()
	mbps, eta := v.recentSpeedAndETA()
	line := fmt.Sprintf(ColorGrey+"Progress: %s%5.1f%%%s block %s%d%s/%d %s%0.2f MB/s%s ETA: %s%s%s",
		ColorBrightGreen, pctDone, ColorGrey,
		ColorWhite, current, ColorGrey,
		v.realBlockCount,
		ColorBlue, mbps, ColorGrey,
		ColorOrange, eta, ColorGrey)
	if v.blocksPerCell > 1 {
		line += fmt.Sprintf(" Each cell: %d blocks", v.blocksPerCell)
	}
	return line + ColorReset
}

func (v *Visualizer) recentSpeedAndETA() (float64, string) {
	done := v.completedBlocks.Load()

	v.progressMu.Lock()
	n := v.progressSampleN
	if n < 2 {
		v.progressMu.Unlock()
		if done >= v.realBlockCount {
			return 0, "0s"
		}
		return 0, "--"
	}
	newestIdx := (v.progressSampleI - 1 + etaSampleSize) % etaSampleSize
	oldestIdx := (v.progressSampleI - n + etaSampleSize) % etaSampleSize
	newest := v.progressSamples[newestIdx]
	oldest := v.progressSamples[oldestIdx]
	v.progressMu.Unlock()

	blocksDone := newest.completed - oldest.completed
	elapsed := newest.at.Sub(oldest.at)
	if blocksDone == 0 || elapsed <= 0 {
		return 0, "--"
	}

	blocksPerSecond := float64(blocksDone) / elapsed.Seconds()
	if blocksPerSecond <= 0 {
		if done >= v.realBlockCount {
			return 0, "0s"
		}
		return 0, "--"
	}
	mbps := blocksPerSecond * float64(atomic.LoadUint32(&blockSize)) / mb1
	if done >= v.realBlockCount {
		return mbps, "0s"
	}
	remaining := v.realBlockCount - done
	eta := formatDuration(time.Duration(float64(remaining)/blocksPerSecond) * time.Second)
	return mbps, eta
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
}

// renderLogs draws recent log messages in the log area at the bottom.
func (v *Visualizer) renderLogs(frame *strings.Builder) {
	lines := GetRecentLogs(v.logLines)
	if len(lines) == 0 {
		// Fill remaining lines with blank cleared lines
		for i := 0; i < v.logLines; i++ {
			frame.WriteString("\033[K\n")
		}
		return
	}

	// Show newest logs first (most recent at bottom, but we print top-to-bottom)
	// Trim to fit
	display := lines
	if len(display) > v.logLines {
		display = display[len(display)-v.logLines:]
	}

	for _, line := range display {
		// Log messages already end with \n from their format string;
		// strip it since we add our own newline below.
		trimmed := strings.TrimRight(line, "\n\r")
		frame.WriteString(ColorDim)
		frame.WriteString(trimmed)
		frame.WriteString(ColorReset)
		frame.WriteString("\033[K\n")
	}

	// Fill any remaining lines
	for i := len(display); i < v.logLines; i++ {
		frame.WriteString("\033[K\n")
	}
}

// renderBlock prints a single block with proper coloring
func (v *Visualizer) renderBlock(frame *strings.Builder, status BlockStatus) {
	switch status {
	case StatusPending:
		frame.WriteString(ColorDim + PendingChar + ColorReset)
	case StatusProcessing:
		// Only the currently-processing cell blinks
		frame.WriteString(ColorBlink + ColorDim + PendingChar + ColorReset)
	case StatusZero:
		frame.WriteString(ColorGrey + BlockChar + ColorReset)
	case StatusCompressed:
		frame.WriteString(ColorBlue + BlockChar + ColorReset)
	case StatusRaw:
		frame.WriteString(ColorOrange + BlockChar + ColorReset)
	case StatusSkipped:
		frame.WriteString(ColorGreen + BlockChar + ColorReset)
	default:
		frame.WriteString(ColorDim + PendingChar + ColorReset)
	}
}

func (v *Visualizer) renderOnce() {
	if v.renderHook != nil {
		v.renderHook()
		return
	}
	v.Render()
}

func (v *Visualizer) renderDelay(now time.Time) time.Duration {
	last := v.lastRenderAt.Load()
	if last == 0 {
		return 0
	}

	elapsed := now.Sub(time.Unix(0, last))
	if elapsed >= visualRenderMinInterval {
		return 0
	}
	return visualRenderMinInterval - elapsed
}

func (v *Visualizer) requestRender() {
	if v.renderStopped.Load() != 0 {
		return
	}
	v.renderRequested.Store(1)
	if !v.renderInFlight.CompareAndSwap(0, 1) {
		return
	}
	go v.renderLoop()
}

func (v *Visualizer) renderLoop() {
	for {
		if v.renderStopped.Load() != 0 {
			v.renderInFlight.Store(0)
			return
		}
		if v.renderRequested.Swap(0) == 0 {
			v.renderInFlight.Store(0)
			if v.renderRequested.Load() == 0 || !v.renderInFlight.CompareAndSwap(0, 1) {
				return
			}
			continue
		}

		if delay := v.renderDelay(time.Now()); delay > 0 {
			time.Sleep(delay)
		}
		if v.renderStopped.Load() != 0 {
			v.renderInFlight.Store(0)
			return
		}
		v.renderOnce()
		v.lastRenderAt.Store(time.Now().UnixNano())
	}
}

// RenderIfNeeded renders the visualization on a time-bounded schedule.
func (v *Visualizer) RenderIfNeeded(lastProcessed uint32) {
	if !v.enabled {
		return
	}

	// Always render on last block to ensure final state is shown.
	// This one is synchronous so RenderFinal can follow cleanly.
	if lastProcessed >= v.realBlockCount-1 {
		v.renderRequested.Store(0)
		v.renderOnce()
		v.lastRenderAt.Store(time.Now().UnixNano())
		return
	}

	v.requestRender()
}

// RenderFinal renders the final state and resets cursor
func (v *Visualizer) RenderFinal() {
	if !v.enabled {
		return
	}

	v.renderStopped.Store(1)
	v.renderRequested.Store(0)
	for v.renderInFlight.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	v.Render()

	// Move cursor below the visualization and clear the note lines
	if v.output == nil {
		v.output = os.Stdout
	}
	_, _ = io.WriteString(v.output, "\033[0m\n")
}

// Disable disables the visualization (for quiet mode)
func (v *Visualizer) Disable() {
	v.enabled = false
}
