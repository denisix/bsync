# 🖥️ bsync Visualizer — Win98 Defragmenter-Style Progress Display

## The Idea

Instead of a plain text progress bar (`block 3047/8192 37% ETA: 4m12s`),
`bsync -v` renders a real-time grid of coloured squares directly in the
terminal — inspired by the classic **Windows 98 Disk Defragmenter**.

Each square represents a block of the transfer. Its colour tells you at a
glance what happened to that block:

| Display | Colour     | ANSI            | Meaning |
|---------|------------|-----------------|---------|
| `□`     | Dim        | `\033[2m`       | Not yet transferred (static) |
| `□`     | Blink dim  | `\033[5m\033[2m` | Currently being processed (blinks) |
| `■`     | Grey       | `\033[90m`      | Zero block — payload omitted from network (sparse) |
| `■`     | Blue       | `\033[94m`      | Transferred with zstd compression |
| `■`     | Orange     | `\033[91m`      | Transferred without compression |
| `■`     | Green      | `\033[32m`      | Skipped — block already identical at destination |

Only **one cell** blinks at a time — the cell the worker is currently
sending or receiving. This gives a clear visual pulse showing the transfer
is actively making progress.

## Layout

The terminal is split **90% visual / 10% text logs**:

```
┌─ Legend: □ Pending □ Processing ■ Zero ■ Compressed ■ Raw ■ Skipped  ─┐
│                                                                        │
│ □□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□  │  ~90%
│ □□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□  │  visual
│ □□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□  │  blocks
│ ■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■□□□□□□□□□□□□□□□□□□□□□□□  │
│ ■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■□□□□□□□□□□□  │
│                                                                        │
│                                                                        │
│ Progress:  45.2% block 3047/8192 82.40 MB/s ETA: 4m12s Each cell: 7 blocks │
├────────────────────────────────────────────────────────────────────────┤
│ 13:33:49 [client] startClient()                                        │  ~10%
│ 13:33:49 [client] source size: 52428800 bytes, blockNum: 49           │  text
│ 13:33:49 [client] starting 1 transfer workers                          │  logs
│ 13:33:49 [client] DONE, waiting for the workers                        │
└────────────────────────────────────────────────────────────────────────┘
```

### Visual section (~90% of terminal height)

1. **Legend** — colour samples with labels (1 line)
2. **Block grid** — rows of coloured squares filling available width
3. **Blank spacer** — one empty line before the stats
4. **Stats line** — compact `Progress: XX.X% block current/total speed ETA: value [Each cell: N blocks]` (1 line)

### Log section (~10% of terminal height)

Before transfer starts, the last N log messages (up to 200 buffered) are
displayed below the grid in dim text so connection/setup problems are visible.
Once the first block starts processing or completes, normal log messages are
hidden and only warning/error/failure messages remain visible. In `-v` mode,
`Log()` writes into a ring buffer instead of stdout; the visualizer's
`renderLogs()` method reads the buffer and prints it below the grid. The newest
entries appear at the bottom.

## Stats Line

The stats line shows a compact single-line summary:

```
Progress:  45.2% block 3047/8192 82.40 MB/s ETA: 4m12s Each cell: 7 blocks
```

Labels are dim/grey, the percentage is bright green, the current block is
bright white, the total block count is dim/grey, speed is bright blue, and
ETA is bright orange.

- **`45.2%`** — percentage of real transfer blocks completed
- **`3047`** — real block index of the most recently started processing
  block (tracked atomically via `currentBlock` in the Visualizer struct)
- **`8192`** — total real transfer blocks
- **`82.40 MB/s`** — recent throughput, calculated from the last 100
  completed block samples and the active block size
- **`4m12s`** — ETA from the same last-100-block speed window
- **`Each cell: 7 blocks`** — shown only when visual cells aggregate multiple
  real transfer blocks

## Architecture — Visual vs Physical Blocks

The key design decision: **visual cells ≠ physical transfer blocks**.

```
realBlockCount  — actual transfer blocks (e.g. 10,000)
visualCellCount — grid cells on screen (e.g. 1,429)
blocksPerCell   — how many real blocks each cell represents (e.g. 7)
```

### Block aggregation formula

```
maxCells = (terminalWidth - 2) × (terminalHeight × 0.9 - overhead)

if realBlockCount > maxCells:
    blocksPerCell = ceil(realBlockCount / maxCells)
    visualCellCount = ceil(realBlockCount / blocksPerCell)   // ≤ maxCells
else:
    visualCellCount = realBlockCount, blocksPerCell = 1       // 1:1 mapping
```

This ensures:
- **Small transfers** (< terminal capacity): every block gets its own cell
- **Large transfers**: blocks are aggregated into cells that fill the screen
- **No orphan cells**: `visualCellCount × blocksPerCell ≥ realBlockCount`,
  but the last real block `(realBlockCount-1) / blocksPerCell ≤ visualCellCount - 1`,
  so every visual cell maps to at least one real block

### Examples

| real blocks | Terminal (W×H) | maxCells | bpc | visualCells | rows |
|-------------|----------------|----------|-----|-------------|------|
| 50          | 80×24          | 858      | 1   | 50          | 1    |
| 5,000       | 80×24          | 858      | 6   | 834         | 11   |
| 10,000      | 80×24          | 858      | 12  | 834         | 11   |
| 10,000,000  | 80×24          | 858      | 11656 | 858       | 11   |
| 10,000      | 200×50         | 6960     | 2   | 5000        | 26   |

## Block States

Each visual cell transitions through at most two states:

```
StatusPending ──→ StatusProcessing ──→ Status{Skipped,Zero,Compressed,Raw}
     (dim □)          (blink dim □)          (coloured ■)
```

- **`StatusPending`** (static dim `□`): the worker hasn't reached this cell yet
- **`StatusProcessing`** (blinking dim `□`): the worker is actively sending
  or receiving this cell's blocks. Only one cell has this state at a time.
  The `currentBlock` atomic is updated whenever this state is set.
- **Final status** (coloured `■`): the cell is done

## Terminal Size Detection

### Unix (Linux / macOS)

Uses `TIOCGWINSZ` ioctl on stdout — returns both width (`ws.Col`) and
height (`ws.Row`). Falls back to `$COLUMNS` / `$LINES` env vars, then
80×24.

### Windows

Uses `GetConsoleScreenBufferInfoEx` (kernel32) — reads `Window.Right -
Window.Left + 1` for width and `Window.Bottom - Window.Top + 1` for
height. Falls back to `GetConsoleScreenBufferInfo` on older Windows.

## Terminal Resize Handling

- `calcGridDimensions()` runs **every `Render()`** call
- Only `cols`, `rows`, and `logLines` adapt — the cell status array
  (`visualCellCount` / `blocksPerCell`) and `currentBlock` are **fixed
  at construction**
- If the terminal shrinks below the grid, trailing cells are clipped
  (data preserved, just not visible). If it grows, more cells become
  visible.
- The first render uses `\033[2J\033[H` to clear the screen and move home.
  Later renders use only `\033[H` plus per-line `\033[K` clears to avoid
  visible full-screen blinking.

## Performance — Non-Blocking Render

The render must never block the transfer worker goroutines. Four
mechanisms ensure this:

1. **Async rendering** — `RenderIfNeeded()` launches `go v.Render()`
   instead of calling `v.Render()` synchronously. The worker returns to
   processing blocks immediately.

2. **Mutex serialization** — `v.mu` inside `Render()` serialises
   concurrent render goroutines. Only one screen draw at a time.

3. **Single-frame output** — each render is built in memory and written to
   the terminal once, avoiding per-cell terminal writes and reducing visible
   paint/flicker.

4. **Time-bounded refresh** — every non-pending status update requests a
   render, including `StatusProcessing` and skipped blocks. The first
   transfer update renders immediately; later updates are coalesced to at
   most one render every 500ms while work is active. A single in-flight
   render loop prevents unbounded `go v.Render()` buildup.

5. **Final render** — `RenderFinal()` is synchronous (no more work
   after it), ensuring the final state is displayed before exit.

## Concurrency Safety

| Resource | Protection | Used by |
|----------|------------|---------|
| `cellStatus[]` | `atomic.Int32` per entry | `UpdateBlockStatus()` (workers), `Render()` (goroutine) |
| `currentBlock` | `atomic.Int32` | `UpdateBlockStatus()` when setting `StatusProcessing` |
| `completedBlocks` | `atomic.Uint32` | `UpdateBlockStatus()` final states, `renderStats()` |
| `progressSamples` | `progressMu sync.Mutex` | ETA and MB/s from last 100 completed blocks |
| `lastRenderAt` / `renderRequested` / `renderInFlight` | atomics | `RenderIfNeeded()` (workers), render scheduler |
| Screen output | `v.mu sync.Mutex` | `Render()` (goroutine) |
| Log buffer | `logBufferMu sync.Mutex` | `Log()` (workers), `GetRecentLogs()` (render goroutine) |
| Visual log phase | `visualTransferStarted atomic.Bool` | `UpdateBlockStatus()`, `Log()` filtering |

## Implementation Files

| File | Purpose |
|------|---------|
| `visualizer.go` | Core: Visualizer struct, grid render, legend, stats, aggregation |
| `visualizer_unix.go` | Terminal size via `TIOCGWINSZ` ioctl (Linux/macOS) |
| `visualizer_windows.go` | Terminal size via `GetConsoleScreenBufferInfoEx` (Windows) |
| `log.go` | Log buffer ring, `GetRecentLogs()` for log-area display |
| `client.go` | Wires `cliVisualizer` into upload/download worker path |
| `server.go` | Wires `srvVisualizer` into server receive path |
| `main.go` | `-v` flag parsing, `v_visual` global |
