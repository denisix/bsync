# 🖥️ bsync Visualizer — Win98 Defragmenter-Style Progress Display

`bsync -v` renders a real-time grid of colored squares instead of a plain text progress line — inspired by the classic Windows 98 Disk Defragmenter. Each square is one block; its color shows what happened to it.

| Display | Color | ANSI | Meaning |
|---------|-------|------|---------|
| `□` | Dim | `\033[2m` | Not yet transferred |
| `□` | Blink dim | `\033[5m\033[2m` | Currently being sent/received (only one cell at a time) |
| `■` | Grey | `\033[90m` | Zero block — payload omitted (sparse) |
| `■` | Blue | `\033[94m` | Transferred with zstd compression |
| `■` | Orange | `\033[38;5;208m` | Transferred without compression |
| `■` | Green | `\033[32m` | Skipped — already identical at destination |

## Layout

5 lines at the bottom are reserved for text logs, plus one guard line to prevent scroll; the rest is legend, block grid, spacer, and stats:

```
┌─ Legend: □ Pending □ Processing ■ Zero ■ Compressed ■ Raw ■ Skipped  ─┐
│ □□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□  │  visual
│ ■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■□□□□□□□□□□□□□□□□□□□□□□□  │  blocks
│ Progress:  45.2% block 3047/8192 82.40 MB/s ETA: 4m12s Each cell: 7 blocks │
├────────────────────────────────────────────────────────────────────────┤
│ 13:33:49 [client] starting 1 transfer workers                          │  5 log
│ 13:33:49 [client] DONE, waiting for the workers                        │  lines
└────────────────────────────────────────────────────────────────────────┘
```

Before the transfer starts, the last buffered log lines (up to 200) show below the grid in dim text; once the first block starts, only warnings/errors stay visible there. `Log()` writes to a ring buffer instead of stdout while `-v` is active; the visualizer reads it for that area.

## Stats Line

```
Progress:  45.2% block 3047/8192 82.40 MB/s ETA: 4m12s Each cell: 7 blocks
```

Percentage/completed/total real blocks, throughput and ETA from the last 100 completed-block samples, and — only when cells aggregate multiple blocks — how many blocks each cell represents (`Each cell: 7`, or `Cell: 7`/`C: 7` on a narrow terminal).

## Visual vs Physical Blocks

Grid cells don't map 1:1 to transfer blocks once the transfer is larger than the terminal can show:

```
gridCols = terminalWidth - 1
gridRows = terminalHeight - 5 log lines - 4 visual overhead - 1 scroll guard
maxCells = gridCols × gridRows

if realBlockCount > maxCells:
    blocksPerCell = ceil(realBlockCount / maxCells)
    visualCellCount = ceil(realBlockCount / blocksPerCell)   // ≤ maxCells
else:
    visualCellCount = realBlockCount, blocksPerCell = 1       // 1:1
```

A small transfer gets one cell per block; a large one aggregates blocks so the grid always fills the screen without orphan cells.

## Block States

```
StatusPending ──→ StatusProcessing ──→ Status{Skipped,Zero,Compressed,Raw}
   (dim □)          (blink dim □)             (colored ■)
```

Only one cell is ever `StatusProcessing` at a time — the block a worker is actively sending or receiving.

## Terminal Size & Resize

- **Unix**: `TIOCGWINSZ` ioctl on stdout, falling back to `$COLUMNS`/`$LINES`, then 80×24.
- **Windows**: `GetConsoleScreenBufferInfoEx`, falling back to `GetConsoleScreenBufferInfo`.
- `calcGridDimensions()` runs on every render; `visualCellCount`/`blocksPerCell` are recalculated on a significant capacity change, and per-block completion state is remapped into the new cells. The first render and any resize do a full `\033[2J\033[H` clear; normal renders use cursor positioning plus per-line `\033[K`.

## Non-Blocking Render

Render never blocks a transfer worker:

1. `RenderIfNeeded()` launches `go v.Render()` — the worker returns immediately.
2. `v.mu` serializes concurrent render goroutines to one draw at a time.
3. Each frame is built in memory and written to the terminal in one shot.
4. Renders are coalesced to at most one per 100ms while work is active (first update renders immediately); a single in-flight render loop prevents unbounded goroutine buildup.
5. `RenderFinal()` is synchronous, so the final state is on screen before exit.

Shared state (`cellStatus[]`, `currentBlock`, `completedBlocks`) is atomics-only; the last-100-samples window, screen output, and log buffer each have their own mutex.

## Implementation Files

| File | Purpose |
|------|---------|
| `visualizer.go` | Core: struct, grid render, legend, stats, aggregation |
| `visualizer_unix.go` / `visualizer_windows.go` | Terminal size, per-OS |
| `log.go` | Log ring buffer for the log-area display |
| `client.go` / `server.go` | Wire `cliVisualizer`/`srvVisualizer` into the transfer path |
| `main.go` | `-v` flag parsing |
