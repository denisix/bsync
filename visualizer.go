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
	visualLogLines          = 5
	visualScrollGuardLines  = 1
	minTerminalWidth        = 40 // Minimum terminal width for visual mode
	minTerminalHeight       = 12 // Minimum terminal height for visual mode
	resizeCellThresholdPct  = 10
)

type progressSample struct {
	completed uint32
	at        time.Time
}

var visualOutput io.Writer = os.Stdout
var terminalWidthProbe = getTerminalWidthIoctl
var terminalHeightProbe = getTerminalHeightIoctl

// isTerminalTooSmall checks if terminal is too small for visual rendering.
func isTerminalTooSmall() bool {
	width := getTerminalWidth()
	height := getTerminalHeight()
	return width < minTerminalWidth || height < minTerminalHeight
}

// Visualizer manages block-by-block progress visualization
type Visualizer struct {
	realBlockCount  uint32         // Actual number of source blocks
	visualCellCount uint32         // Number of visual cells in the grid
	blocksPerCell   uint32         // How many real blocks each visual cell represents
	cellStatus      []atomic.Int32 // Per-cell status (thread-safe)
	realBlockDone   []atomic.Bool
	realBlockStatus []atomic.Int32
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
	lastCellStatus  []BlockStatus // Last displayed status for each cell
	lastWidth       int           // Last terminal width (for resize detection)
	lastHeight      int           // Last terminal height (for resize detection)
	lastLogLines    []string      // Last log lines displayed
	needsFullRender atomic.Bool   // Force full redraw on resize
}

// terminalGridSize returns the grid dimensions for the visual block area only.
// Five terminal lines are reserved for logs below the visual section.
func terminalGridSize() (cols, maxGridRows, maxCells int) {
	width := getTerminalWidth()
	height := getTerminalHeight()
	if width < 20 {
		width = 80
	}
	if height < 10 {
		height = 24
	}

	cols = width - 1
	if cols < 1 {
		cols = 1
	}

	// Overhead: legend(1) + blank(1) + pre-stats blank(1) + stats(1).
	// Leave one guard line so full renders do not scroll when they reach the
	// terminal bottom.
	maxGridRows = height - visualLogLines - 4 - visualScrollGuardLines
	if maxGridRows < 1 {
		maxGridRows = 1
	}

	maxCells = cols * maxGridRows
	return
}

// NewVisualizer creates a new visualizer instance.
// The number of visual cells is dynamically computed from the current terminal
// size so the grid fills the screen. For small block counts every block gets
// its own cell (1:1); for large counts blocks are aggregated into cells.
func NewVisualizer(blockCount uint32) *Visualizer {
	if blockCount == 0 || isTerminalTooSmall() {
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
		realBlockStatus: make([]atomic.Int32, blockCount),
		cellDone:        make([]atomic.Uint32, visualCells),
		enabled:         true,
		output:          visualOutput,
	}

	for i := uint32(0); i < blockCount; i++ {
		v.realBlockStatus[i].Store(int32(StatusPending))
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
// Called on every Render() to handle terminal resize.
func (v *Visualizer) calcGridDimensions() {
	width := getTerminalWidth()
	height := getTerminalHeight()
	if width < 20 {
		width = 80
	}
	if height < 10 {
		height = 24
	}

	v.logLines = visualLogLines

	// Overhead: legend(1) + blank(1) + pre-stats blank(1) + stats(1).
	overhead := 4
	maxGridRows := height - v.logLines - overhead - visualScrollGuardLines
	if maxGridRows < 1 {
		maxGridRows = 1
	}

	v.cols = width - 1
	if v.cols < 1 {
		v.cols = 1
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

func visualCellsForCapacity(blockCount uint32, maxCells int) (visualCells, bpc uint32) {
	if blockCount == 0 || maxCells <= 0 {
		return 0, 1
	}
	if blockCount <= uint32(maxCells) {
		return blockCount, 1
	}
	bpc = (blockCount + uint32(maxCells) - 1) / uint32(maxCells)
	visualCells = (blockCount + bpc - 1) / bpc
	return visualCells, bpc
}

func (v *Visualizer) shouldRecalculateCells(maxCells int) bool {
	targetCells, targetBPC := visualCellsForCapacity(v.realBlockCount, maxCells)
	if targetCells == 0 {
		return false
	}
	if targetBPC == v.blocksPerCell && targetCells == v.visualCellCount {
		return false
	}
	current := int(v.visualCellCount)
	target := int(targetCells)
	diff := current - target
	if diff < 0 {
		diff = -diff
	}
	if current == 0 {
		return true
	}
	return diff*100/current >= resizeCellThresholdPct
}

func (v *Visualizer) recalculateCells(maxCells int) {
	visualCells, bpc := visualCellsForCapacity(v.realBlockCount, maxCells)
	if visualCells == 0 {
		return
	}

	newCellStatus := make([]atomic.Int32, visualCells)
	newCellDone := make([]atomic.Uint32, visualCells)
	for cellIdx := uint32(0); cellIdx < visualCells; cellIdx++ {
		start := cellIdx * bpc
		end := start + bpc
		if end > v.realBlockCount {
			end = v.realBlockCount
		}

		var done uint32
		status := StatusPending
		for blockIdx := start; blockIdx < end; blockIdx++ {
			blockStatus := BlockStatus(v.realBlockStatus[blockIdx].Load())
			if isFinalBlockStatus(blockStatus) {
				done++
				status = blockStatus
			} else if blockStatus == StatusProcessing && status == StatusPending {
				status = StatusProcessing
			}
		}
		if done > 0 && done < end-start {
			status = StatusProcessing
		}

		newCellDone[cellIdx].Store(done)
		newCellStatus[cellIdx].Store(int32(status))
	}

	v.visualCellCount = visualCells
	v.blocksPerCell = bpc
	v.cellStatus = newCellStatus
	v.cellDone = newCellDone
	v.lastCellStatus = make([]BlockStatus, visualCells)
	v.needsFullRender.Store(true)
}

// getTerminalWidth attempts to get the terminal width.
func getTerminalWidth() int {
	if width := terminalWidthProbe(); width > 0 {
		return width
	}
	if cols := getEnvInt("COLUMNS"); cols > 0 {
		return cols
	}
	return 80
}

// getTerminalHeight attempts to get the terminal height.
func getTerminalHeight() int {
	if height := terminalHeightProbe(); height > 0 {
		return height
	}
	if rows := getEnvInt("LINES"); rows > 0 {
		return rows
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

func visibleLen(s string) int {
	n := 0
	inEscape := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inEscape {
			if ch >= '@' && ch <= '~' {
				inEscape = false
			}
			continue
		}
		if ch == '\033' {
			inEscape = true
			continue
		}
		n++
	}
	return n
}

func fitsTerminalWidth(s string, width int) bool {
	return visibleLen(s) < width
}

// cursorPos returns ANSI escape to move cursor to row, col (1-based).
func cursorPos(row, col int) string {
	return fmt.Sprintf("\033[%d;%dH", row, col)
}

// cellPosition returns screen (row, col) for visual cell at idx (1-based).
// Grid starts at row 3 (after legend + blank line), column 1 (no margin).
func (v *Visualizer) cellPosition(idx uint32) (row, col int) {
	// Row: +2 for legend(1) + blank(1), grid rows start at row 3
	// Col: +1 for 1-based ANSI coordinates, no left margin in actual layout
	row = 3 + int(idx)/v.cols
	col = 1 + int(idx)%v.cols
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

	v.mu.Lock()
	cellIdx := v.mapToCell(blockIdx)
	if status == StatusProcessing {
		v.realBlockStatus[blockIdx].CompareAndSwap(int32(StatusPending), int32(StatusProcessing))
		v.cellStatus[cellIdx].CompareAndSwap(int32(StatusPending), int32(StatusProcessing))
		v.currentBlock.Store(int32(blockIdx))
	}
	if isFinalBlockStatus(status) {
		if v.realBlockDone[blockIdx].CompareAndSwap(false, true) {
			v.realBlockStatus[blockIdx].Store(int32(status))
			v.recordBlockComplete(time.Now())
			doneInCell := v.cellDone[cellIdx].Add(1)
			if doneInCell >= v.realBlocksInCell(cellIdx) {
				v.cellStatus[cellIdx].Store(int32(status))
			} else {
				v.cellStatus[cellIdx].Store(int32(StatusProcessing))
			}
		}
	}
	v.mu.Unlock()

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

	if isTerminalTooSmall() {
		v.Disable()
		v.writeFrame("\033[2J\033[HTerminal too small for visual mode (min 40x12)\n")
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Check for terminal resize
	currentWidth := getTerminalWidth()
	currentHeight := getTerminalHeight()
	resized := v.lastWidth != currentWidth || v.lastHeight != currentHeight
	if resized {
		v.lastWidth = currentWidth
		v.lastHeight = currentHeight
		_, _, maxCells := terminalGridSize()
		if v.shouldRecalculateCells(maxCells) {
			v.recalculateCells(maxCells)
		}
		v.screenRendered = false
		v.renderedLines = 0
		v.needsFullRender.Store(true)
	}

	// Recalc grid dimensions every render (handles terminal resize)
	v.calcGridDimensions()

	fullRender := v.needsFullRender.Load()
	if fullRender {
		v.renderFull()
		v.needsFullRender.Store(false)
	} else {
		v.renderDiff()
	}

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
				break
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

	// Update tracking state with what we actually displayed
	displayedStatus := make([]BlockStatus, totalCells)
	for idx := uint32(0); idx < totalCells; idx++ {
		displayedStatus[idx] = BlockStatus(v.cellStatus[idx].Load())
	}
	v.lastCellStatus = displayedStatus
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

	// Track what we're actually displaying to avoid race condition
	displayedStatus := make([]BlockStatus, totalCells)

	// Update only changed cells
	for idx := uint32(0); idx < totalCells; idx++ {
		currentStatus := BlockStatus(v.cellStatus[idx].Load())
		displayedStatus[idx] = currentStatus

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

	// Update tracking state with what we actually displayed
	v.lastCellStatus = displayedStatus
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
	width := getTerminalWidth()
	full := ColorDim + "Legend: " + ColorReset +
		ColorDim + PendingChar + ColorReset + " Pending " +
		ColorBlink + ColorDim + PendingChar + ColorReset + " Processing " +
		ColorGrey + BlockChar + ColorReset + " Zero " +
		ColorBlue + BlockChar + ColorReset + " Compressed " +
		ColorOrange + BlockChar + ColorReset + " Raw " +
		ColorGreen + BlockChar + ColorReset + " Skipped"
	if fitsTerminalWidth(full, width) {
		frame.WriteString(full)
		frame.WriteString("\033[K\n\n")
		return
	}

	compact := ColorDim + "Legend: " + ColorReset +
		ColorDim + PendingChar + ColorReset + " Pending " +
		ColorBlink + ColorDim + PendingChar + ColorReset + " Active " +
		ColorGreen + BlockChar + ColorReset + " Done"
	frame.WriteString(compact)
	frame.WriteString("\033[K\n\n")
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

	width := getTerminalWidth()
	mbps, eta := v.recentSpeedAndETA()
	line := fmt.Sprintf(ColorGrey+"Progress: %s%5.1f%%%s block %s%d%s/%d %s%0.2f MB/s%s ETA: %s%s%s",
		ColorBrightGreen, pctDone, ColorGrey,
		ColorWhite, done, ColorGrey,
		v.realBlockCount,
		ColorBlue, mbps, ColorGrey,
		ColorOrange, eta, ColorGrey)
	if v.blocksPerCell > 1 {
		line += fmt.Sprintf(" Each cell: %d blocks", v.blocksPerCell)
	}
	line += ColorReset
	if fitsTerminalWidth(line, width) {
		return line
	}

	compact := fmt.Sprintf(ColorGrey+"Progress: %s%5.1f%%%s %s%d%s/%d ETA: %s%s%s",
		ColorBrightGreen, pctDone, ColorGrey,
		ColorWhite, done, ColorGrey,
		v.realBlockCount,
		ColorOrange, eta, ColorGrey)
	if v.blocksPerCell > 1 {
		compact += fmt.Sprintf(" Cell: %d", v.blocksPerCell)
	}
	compact += ColorReset
	if fitsTerminalWidth(compact, width) {
		return compact
	}

	short := fmt.Sprintf(ColorGrey+"%s%5.1f%%%s %s%d%s/%d ETA %s%s%s",
		ColorBrightGreen, pctDone, ColorGrey,
		ColorWhite, done, ColorGrey,
		v.realBlockCount,
		ColorOrange, eta, ColorGrey)
	if v.blocksPerCell > 1 {
		short += fmt.Sprintf(" C: %d", v.blocksPerCell)
	}
	return short + ColorReset
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
	display := lines
	if len(display) > v.logLines {
		display = display[len(display)-v.logLines:]
	}

	for row := 0; row < v.logLines; row++ {
		if row < len(display) {
			trimmed := strings.TrimRight(display[row], "\n\r")
			frame.WriteString(ColorDim)
			frame.WriteString(trimmed)
			frame.WriteString(ColorReset)
		}
		frame.WriteString("\033[K")
		if row < v.logLines-1 {
			frame.WriteString("\n")
		}
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
