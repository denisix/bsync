# bsync

🚀 **Efficient block-level synchronization tool for large data transfer between servers**

`bsync` transfers data between block devices or files block-by-block. In upload mode it compares destination checksums and transmits only changed blocks; in download mode it streams source blocks to the local destination. Builds and runs on Linux and macOS; Windows is supported as a client for block-device/volume sources (see [WINDOWS.md](WINDOWS.md)).

## ✨ Features

- **Smart Upload**: only transfers blocks that differ (SHA-256, hardware-accelerated on ARMv8/x86)
- **Zero Block Support**: omits zero payloads on the network while correctly clearing stale destination data
- **Compression**: built-in zstd, configurable levels (fast/default/better/best)
- **Encryption**: optional ChaCha20-Poly1305 for SSH-automated transfers
- **SSH Integration**: automatic remote server deployment
- **Directory-Tree Sync**: recursive push with exclude patterns, quick-check, `--delete`, symlinks, specials
- **Multi-worker Support**: HDD-friendly sequential source reads and ordered destination writes
- **Resume Capability**: skip blocks to resume interrupted transfers
- **Progress Visualization**: Win98 Defragmenter-style real-time grid, speed and ETA on both ends
- **Download Mode**: transfer from server to client (reverse direction)
- **Connection Robustness**: keep-alive, per-operation deadlines, acknowledged + retried upload writes

## 🛠️ Installation

```bash
make          # binary for the current OS/arch
make windows  # Windows binary (bsync.exe)
make release  # cross-compiled linux/{amd64,arm64}, windows/amd64, darwin/{amd64,arm64} into dist/
```

Copy the resulting binary to source and destination. Both endpoints must be built from the same source commit — the binary protocol (`ver0.06`) is not forward/backward compatible.

## 📖 Usage

Upload mode (default): server receives, client sends. `-d` reverses it: server reads the source, client writes the destination.

### Command Line Options

| Option | Description | Default |
|--------|-------------|---------|
| `-a` | List drives where supported; on Linux prints `lsblk` guidance | false |
| `-f` | File or device path (e.g., `/dev/vda`) | `/dev/zero` |
| `-r` | Remote server address (`host:port`) | - |
| `-b` | Block size in bytes | 10485760 (10MB) |
| `-s` | Skip blocks (for resume) | 0 |
| `-p` | Server port | 8080 |
| `-i` | Bind to specific IP address | `0.0.0.0` |
| `-n` | Disable compression | false |
| `-e` | Enable encryption for SSH mode (auto-generates key) | false |
| `-L` | Compression level: `fast`, `default`, `better`, `best` | `default` |
| `-t` | SSH target (`user@host:/remote_path` or `user@host:port:/remote_path`) | - |
| `-l` | Custom log prefix | - |
| `-w` | Number of workers | 1 |
| `-prefetch` | Prefetch queue depth; `0` selects `workers*4` | 0 |
| `-auto` | Auto-tune parameters for direct client upload (`-r` only) | false |
| `-q` | Quiet mode | false |
| `-d` | Download mode (server → client) | false |
| `-v` | Win98-style visual progress grid | false |
| `-R` | Force directory-tree mode (auto-detected when `-f` is a directory) | false |
| `-exclude` | Comma-separated glob patterns, matched against relative path and base name | - |
| `-quick-check` | Directory mode: skip a file (no read/hash) if size+mtime already match | **true** |
| `-ignore-times` | Directory mode: never skip on size+mtime — compare every block (rsync `-I`) | false |
| `-delete` | Directory mode: remove destination entries not in the source, before transfer (rsync `--delete-before`) | false |
| `-links` | Directory mode: recreate source symlinks with identical targets (rsync `-l`) | false |
| `-specials` | Directory mode: recreate FIFOs/sockets (rsync `--specials`); device nodes still skipped | false |
| `-memlimit` | Soft memory limit in MB (`0` = off); safety valve, not a heap fix. Forwarded via `-t` | 0 |

`-K`: internal/manual 32-byte hex key for direct network encryption; normally prefer `-e` with `-t`.

### Directory-Tree Mode

`-f` a directory (or force with `-R`) walks the source and syncs every file and empty directory, push only:

```bash
./bsync -f /data/src -t user@host:/data/dst -w 8 -exclude "*.log,node_modules,*tmp*"
```

- **`-exclude`**: shell globs (`*`, `?`, `[...]`) checked against both relative path and base name; `*` never crosses `/`, so `node_modules` prunes any dir by that name at any depth.
- **Quick check** (default): skips a file entirely — no open/read/hash, no round trip — when destination size and mtime (whole seconds) already match. A stale permission is still corrected. Measured: 3 000 unchanged files over loopback, 2.17s → 0.07s; over a simulated 20ms link, 500 files, 23.4s → 0.21s. Trusts size+mtime like rsync's default; `-ignore-times` compares every block instead.
- **`-delete`** (rsync `--delete-before`): once the whole tree is announced — before any content transfers — sweeps the destination for anything not in the source and removes it (files and directories). Refuses to run if the announced count doesn't match the client's claim, so a truncated stream can't trigger deletion. Excluded paths and un-synced types (symlinks without `-links`, specials without `-specials`, device nodes always) are protected, contents included. Every removal is logged; transfer waits for the walk to finish only when `-delete` is set.
- **`-links`** (rsync `-l`): recreates symlinks with identical targets (relative stays relative, dangling stays dangling); retargets a wrong link, replaces a regular file, refuses over a directory. The reverse — source replaces a link with real content — always removes the stale destination link rather than writing through it, with or without `-links`. Without `-links`, symlinks are skipped and protected from `-delete`.
- **`-specials`** (rsync `--specials`): recreates FIFOs/sockets via `mkfifo`/`mknod` at announce time (no block traffic, no root needed); leaves a correct-type node alone, replaces a wrong-type one. Device nodes are never synced.
- **Path safety**: traversal, absolute/UNC/drive-prefixed paths, NUL bytes, backslashes are always refused. Windows-only-invalid names (reserved device names, trailing dot/space) are refused only when the **destination** is Windows — an ordinary Linux file like `example.net.` fails cleanly for that one entry rather than being silently dropped.
- **Skip logging**: a bounded tail per distinct reason, then one aggregated line (`skipped 4213 path(s): 4198 unsupported file type, 15 excluded`).
- **Unwritable destination**: out of space, over quota, or a read-only mount fails the affected file on its first refused open — no retries, no reconnects — while the rest of the tree continues. Such files are left untouched at the destination rather than created empty or truncated. Transient conditions (a momentarily exhausted file-descriptor table) are still retried normally.
- **Destination-side failures**: if the destination can't create a path (a file where a directory must go, permissions, no space), that entry and everything under it fail there. The destination reports them in its announce reply, so the client fails them locally without attempting a transfer — no per-file connection churn — and logs a bounded tail plus one summary (`N path(s) rejected by the destination: ...`). `FailedError(id)` and the log carry the reason; the destination's own `skipping this entry` lines name the exact cause. Affected files are left untouched at the destination rather than created empty.
- **Completion summary**: logs `N/M file(s) changed` alongside bytes scanned/sent — how many regular files actually differed and transferred, distinct from how many were scanned or how many bytes moved (a few large changed files and thousands of small ones can move the same byte total). A quick-check-skipped file and a file whose every block matched via checksum both count as unchanged.

## 🔄 Examples

```bash
# Local network transfer (start destination, then source)
./bsync -f /dev/shm/test-dst -p 8080
./bsync -b 209715200 -f /dev/shm/test-src -r 192.168.1.100:8080

# SSH-automated (starts the remote server for you)
./bsync -b 209715200 -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst

# Encrypted (SSH mode only -- key generated and shared automatically)
./bsync -e -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst

# Multi-worker, large blocks
./bsync -b 524288000 -w 4 -f /dev/sda -r remote-server:8080

# Compression level
./bsync -L fast -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst

# Resume: skip the first 10 blocks (only if already correct at the destination)
./bsync -f /dev/sda -r remote-server:8080 -s 10

# Quiet (scripts) / local sync without compression
./bsync -f /dev/sda -r remote-server:8080 -q
./bsync -n -f /tmp/src.img -t /tmp/dst.img

# Download mode (-d: server reads source, client writes destination)
./bsync -f /dev/shm/test-src -p 8080 -d
./bsync -f /dev/shm/test-dst -r 192.168.1.100:8080 -d
./bsync -f /dev/shm/test-dst -t user@remote-server:/dev/shm/test-src -d   # SSH-automated

# Bind to a specific interface
./bsync -f /dev/shm/test-dst -p 8080 -i 192.168.1.50

# Combined: encrypted, fast compression, multi-worker
./bsync -e -L fast -w 4 -f /dev/sda -t user@remote:/backup/disk.img
```

- `-e` needs SSH mode (`-t`) to auto-share the key; direct `-r`/`-p` rejects `-e` without an explicit `-K`.
- `-L`: `fast` ~25x faster/~10% larger, `better` ~2x slower/~10% smaller, `best` ~4x slower/~10-15% smaller than `default`. zstd window derives from `-b`, capped at 4 MiB.
- `-s N` skips verifying/transferring blocks before `N` — use only when they're already correct at the destination.
- In download mode the source's block size is authoritative; the client adopts it from transfer metadata.

## 🕳️ Sparse File Support

Zero blocks are detected and their payload is never sent; a stale non-zero destination range is still cleared correctly. A new or expanded regular-file upload destination stays sparse where a zero block required no overwrite.

```bash
truncate -s 100G /tmp/sparse.img
./bsync -f /tmp/sparse.img -t user@remote:/backup/sparse.img
```

## Transfer Safety and Compatibility

- A changed upload block completes only after the destination confirms its write; a destination write failure is a nonzero exit.
- Download writes clear zero ranges in existing destinations instead of leaving stale data.
- Empty source files are supported and truncate a regular-file destination to zero bytes.
- Wire protocol is `ver0.06`; both endpoints need matching newly built binaries.

## 🔍 Verification

```bash
md5sum /dev/shm/test-src /dev/shm/test-dst   # manual check

make test          # full suite + edge-case matrix
make test-fault     # protocol-failure, CLI-validation, sysfs-simulation, vector-I/O tests
make test-stress    # race detector + concurrent local -t stress matrix
make bench           # transfer-level benchmarks (real client/server, loopback); compare with benchstat
make test-remote REMOTE=user@host   # live SSH transfer test, needs non-interactive SSH auth
```

`test-remote` drives a broad `-t` upload/download matrix: workers 1/2/4/8, block sizes 4KB/64KB/1MB, data patterns (zero, solid, random+zero-interleaved, non-block-aligned), and compression modes (default/off/fast/best, each with and without encryption). Every case pre-fills the destination with stale data first and verifies by checksum (upload) or byte comparison (download).

## 📊 Performance

### Block Size

| Network | Block Size | Why |
|---|---|---|
| 1 Gbps | 100-200 MB | balances throughput and memory |
| 10 Gbps+ | 500 MB-1 GB | maximizes throughput |
| 100 Mbps | 10-50 MB | finer progress granularity |

### Workers (`-w`)

- HDD source: `1` (default) — sequential reads avoid head thrashing
- HDD destination: higher is safe — writes are reordered into a near-sequential stream
- SSD/NVMe: `4`-`8` — parallel I/O helps
- Multiple devices: match worker count to device count
- Large directory trees: `16`-`64` is the practical ceiling. Past that, workers stop buying throughput (the disk and the link are the bottleneck, not connection count) and start costing real resources: each one holds a socket on both sides with its own TCP buffers, and the destination allows up to 4 concurrently open files per worker. A server needs roughly `workers × 5 + 32` file descriptors — `-w 512` wants ~2600 against a typical `ulimit -n` of 1024. bsync clamps its own open-file budget to fit `RLIMIT_NOFILE` and warns when `-w` is above what the system can support, but it never overrides your `-w`; raise the limit with `ulimit -n` or lower `-w`.

### Compression (`-L`)

| Level | Speed | Size | Best for |
|---|---|---|---|
| `fast` | ~25x faster | ~10% larger | fast networks, CPU-limited |
| `default` | 1x | 1x | general use |
| `better` | ~2x slower | ~10% smaller | slow networks |
| `best` | ~4x slower | ~10-15% smaller | very slow networks |

### What's optimized internally

- **Memory**: exact-size buffer pools (no over-allocation for small files); single-concurrency zstd encoders — parallelism comes from `-w`, not per-encoder; directory mode keeps only in-flight files in memory; `-delete`'s deferred file list spills to disk; `-memlimit`/`GOMEMLIMIT` as a soft GC ceiling
- **I/O**: sequential-access advice and read-ahead tuning on transfer files, skipped under 1MB in directory mode; page cache dropped after each read so a large transfer doesn't evict useful read-ahead; a single sequential reader drives both hashing and sending, never a redundant second pass; directory-mode `mkdir` is checked once per directory, not once per file, and a quick-check-skipped file costs a single stat, not two; the progress pre-scan only runs at all when the walk is still going after 2s, so short re-syncs never pay for a second metadata sweep
- **HDD**: configurable prefetch depth; all destination writes reordered into a near-sequential offset-ordered stream regardless of `-w`; steady `sync_file_range` writeback instead of bursty flushes
- **Network**: large TCP buffers, keep-alive, per-operation deadlines; acknowledged + retried upload writes (3 attempts); buffered per-connection reads and coalesced small frames cut syscalls per block; a block ready to reuse is handed to the write/network path directly instead of through an extra copy
- **Latency (directory mode)**: the transfer protocol is pipelined — each worker connection keeps a window of up to 32 requests in flight and consumes replies as a stream, instead of one blocking round trip per protocol step; on a high-latency link this is worth ~5x on small-file trees (measured at 4 ms RTT), and a network blip only re-drives the affected window (`BSYNC_NOPIPELINE=1` falls back to the strict lock-step exchange)
- **Compression**: payloads under 512 bytes skip zstd entirely — frame overhead alone would exceed any savings
- **Sparse files**: zero blocks never cross the network; stale non-zero destination ranges are still cleared correctly
- **Concurrency**: full test suite passes under `-race`

## 🖥️ Progress Visualization

`-v` renders a real-time Win98 Defragmenter-style grid: each block is a colored square.

```ansi
Legend: □ Pending □ Processing ■ Zero ■ Compressed ■ Raw ■ Skipped

□□□□□□□□□□□□□□□■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■
■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■■□□□□□□□□□□□□□□□
■■■■■■■■■■■■■■■■■■■■■■□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□□

Progress:  37.2% block 3047/8192 89.20 MB/s ETA: 4m12s Each cell: 7 blocks
```

| Color | Meaning |
|-------|---------|
| Dim `□` | Not yet transferred |
| Grey `■` | Zero block — payload omitted |
| Blue `■` | Transferred with compression |
| Orange `■` | Transferred without compression |
| Green `■` | Skipped — already equal at destination |

See [VISUAL.md](VISUAL.md) for the design (grid aggregation, resize handling, render architecture).

## 🔧 Technical Details

- **Checksum**: SHA-256 (truncated to 16 bytes), hardware-accelerated on ARMv8/x86
- **Compression**: Zstandard (zstd), configurable levels
- **Encryption**: ChaCha20-Poly1305 AEAD
- **Protocol**: custom binary over TCP (`ver0.06`; matching binaries required at both endpoints)
- **Reliability**: TCP keep-alive (30s), 5-minute I/O deadlines, acknowledged + retried (3x) upload writes

## 🚨 Requirements

- Go 1.25+ to build from source
- Linux or macOS to run either endpoint; Windows can act as a client for block-device/volume sources (see [WINDOWS.md](WINDOWS.md))
- Network connectivity between servers; SSH access if using `-t`
- Read/write permissions on source/destination files

## 📝 License

See [LICENSE](LICENSE) file for details.
