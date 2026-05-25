# bsync

🚀 **Efficient block-level synchronization tool for large data transfer between servers**

`bsync` transfers data between block devices or files block-by-block. In upload mode it compares destination checksums and transmits only changed blocks; in download mode it streams source blocks to the local destination. It is currently supported on Linux.

## ✨ Features

- **Smart Upload**: Upload mode only transfers blocks that differ (checksum-based)
- **Zero Block Support**: Omits zero payloads on the network while correctly clearing stale destination data
- **Compression**: Built-in zstd compression with configurable levels (fast/default/better/best)
- **Encryption**: Optional ChaCha20-Poly1305 encryption for SSH-automated transfers
- **SSH Integration**: Automatic remote server deployment via SSH
- **Multi-worker Support**: Parallel processing with HDD-friendly sequential reads
- **Resume Capability**: Skip blocks to resume interrupted transfers
- **Progress Monitoring**: Real-time transfer progress with speed and ETA on both sender and receiver
- **Download Mode**: Transfer from server to client (reverse direction)
- **IP Binding**: Bind server to specific network interface
- **Connection Robustness**: TCP keep-alive, per-operation deadlines, acknowledged upload writes, and upload retry with reconnect

## 🛠️ Installation

```bash
make          # Linux binary
```

Copy the `bsync` binary to your source and destination servers.

## 📖 Usage

### Basic Operation

In normal upload mode:
- **Server mode**: Receives data (destination)
- **Client mode**: Sends data (source)

With `-d`, the direction reverses: the server reads the source and the client
writes the destination.

### Command Line Options

| Option | Description | Default |
|--------|-------------|---------|
| `-a` | List drives where supported; on Linux prints `lsblk` guidance | false |
| `-f` | File or device path (e.g., `/dev/vda`) | `/dev/zero` |
| `-r` | Remote server address (`host:port`) | - |
| `-b` | Block size in bytes (decimal integer) | 10485760 (10MB) |
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
| `-q` | Quiet mode (no output) | false |
| `-d` | Download mode: transfer from server to client | false |
| `-P` | Suppress server-side progress output (set automatically via `-t`) | false |

`-K` is an internal/manual input for a shared 32-byte hexadecimal key. Direct
network encryption requires the same key on both endpoints; for normal use,
prefer `-e` with SSH mode (`-t`).

## 🔄 Examples

### 1. Local Network Transfer

**Destination server:**
```bash
./bsync -f /dev/shm/test-dst -p 8080
```

**Source server:**
```bash
./bsync -b 209715200 -f /dev/shm/test-src -r 192.168.1.100:8080
```

### 2. SSH-Automated Transfer

**Single command (automatically starts remote server):**
```bash
./bsync -b 209715200 -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst
```

### 3. Encrypted Transfer

**Secure transfer with auto-generated encryption key:**
```bash
./bsync -e -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst
```

The `-e` flag enables ChaCha20-Poly1305 encryption in `-t` SSH mode. A 32-byte key is auto-generated and passed to the remote server through SSH. Direct `-r`/`-p` mode does not auto-negotiate a key and rejects `-e` without an explicitly shared internal key.

### 4. High-Performance Transfer

**Multi-worker (4 workers) transfer with custom block size:**
```bash
./bsync -b 524288000 -w 4 -f /dev/sda -r remote-server:8080
```

### 5. Compression Levels

**Fast compression for CPU-limited systems:**
```bash
./bsync -L fast -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst
```

The `-L` flag controls compression level:
- `fast`: Fastest compression, larger output (~25x faster, ~10% larger)
- `default`: Balanced speed and ratio
- `better`: Better compression ratio, slower
- `best`: Best compression ratio, slowest (~2-5x slower, ~10-15% smaller)

### 6. Resume Interrupted Transfer

**Skip first 10 blocks to resume:**
```bash
./bsync -f /dev/sda -r remote-server:8080 -s 10
```

`-s N` does not verify or transfer skipped blocks. Use it only when all blocks
before `N` are already correct at the destination.

### 7. Quiet Mode for Scripts

```bash
./bsync -f /dev/sda -r remote-server:8080 -q
```

### 8. Local File Sync

```bash
./bsync -n -f /tmp/src.img -t /tmp/dst.img
```

### 9. Download Mode (Server → Client)

**Source server:**
```bash
./bsync -f /dev/shm/test-src -p 8080 -d
```

**Client (download mode):**
```bash
./bsync -f /dev/shm/test-dst -r 192.168.1.100:8080 -d
```

**SSH-Automated Download:**
```bash
./bsync -f /dev/shm/test-dst -t user@remote-server:/dev/shm/test-src -d
```

In download mode, the server/source block size is authoritative. The
destination client adopts it from transfer metadata.

### 10. Bind to Specific IP

**Use specific network interface:**
```bash
./bsync -f /dev/shm/test-dst -p 8080 -i 192.168.1.50
```

### 11. Combined Options

**Encrypted, fast compression, multi-worker:**
```bash
./bsync -e -L fast -w 4 -f /dev/sda -t user@remote:/backup/disk.img
```

## 🕳️ Sparse File Support

`bsync` detects zero blocks and does not transfer their payload bytes over the network. For correctness, zero blocks overwrite stale non-zero destination ranges when needed. A new or expanded regular-file upload destination can remain sparse where zero blocks required no overwrite.

```bash
# Create sparse file
truncate -s 100G /tmp/sparse.img

# Transfer - zero block payloads are omitted
./bsync -f /tmp/sparse.img -t user@remote:/backup/sparse.img
```

## Transfer Safety and Compatibility

- A changed upload block completes only after the destination confirms its write; destination write failures cause a nonzero exit.
- Download writes clear zero ranges in existing destinations instead of leaving stale data behind.
- Empty source files are supported and truncate a regular-file destination to zero bytes.
- The current wire protocol is `ver0.02`; use matching newly built binaries on both endpoints because older `ver0.01` binaries are incompatible.

## 🔍 Verification

Verify successful transfer:
```bash
md5sum /dev/shm/test-src /dev/shm/test-dst
```

Run comprehensive tests:
```bash
make test
```

Run deterministic protocol-failure, CLI-validation, sysfs-simulation, and
strict vector-I/O tests:
```bash
make test-fault
```

Run the opt-in race detector and concurrent local `-t` stress transfer matrix:
```bash
make test-stress
```

Run the opt-in live SSH transfer test against an accessible temporary-file
host:
```bash
make test-remote REMOTE=cloud@IP
```

The remote test requires non-interactive SSH authentication. It uses a unique
directory under `/tmp` on the remote host; exercises `-t` uploads and
downloads with default, explicit-level, disabled, and encrypted compression,
multiple workers, zero-block overwrites, empty-file truncation, and `-s`
resume behavior. It also covers quoted remote paths, concurrent moderate-size
traffic, remote executable cleanup, and verifies transfer correctness with
checksums for uploads and byte-for-byte comparisons for downloads before
removing the temporary artifacts it creates.

## 📊 Performance Tips

### Block Size Selection

| Network Speed | Recommended Block Size | Reason |
|--------------|----------------------|---------|
| 1 Gbps | 100-200 MB | Balances throughput and memory overhead |
| 10 Gbps+ | 500-1000 MB | Maximizes throughput with sufficient bandwidth |
| 100 Mbps | 10-50 MB | Smaller blocks for better progress granularity |

### Worker Count

- **HDD**: Use `-w 1` (default) - sequential reads prevent head thrashing
- **SSD/NVMe**: Use `-w 4` to `-w 8` - parallel I/O beneficial
- **Multiple Devices**: Worker count should match device count for optimal throughput

### Compression Levels

| Level | Speed vs Default | Size vs Default | Best For |
|-------|------------------|-----------------|----------|
| `fast` | ~25x faster | ~10% larger | High-speed networks, CPU-limited systems |
| `default` | 1x | 1x | General use, balanced performance |
| `better` | ~2x slower | ~10% smaller | Slow networks, bandwidth-sensitive |
| `best` | ~4x slower | ~15% smaller | Very slow networks, minimum transfer |

### Other Tips

- **Encryption**: use `-e` with `-t` so the generated key is supplied to both endpoints
- **SSH mode**: Use `-t` for automatic remote server management and secure key exchange
- **Bind IP**: Use `-i` to select specific network interface on multi-homed servers
- **Quiet mode**: Use `-q` when scripting to suppress progress output
- **Resume capability**: Use `-s N` to skip N blocks and resume interrupted transfers

## ⚡ Performance Optimizations

`bsync` uses the following optimizations in its active transfer path:

### Memory Optimizations

- **Memory Locking**: Reader pool buffers attempt `mlock()` on Linux; transfer continues if locking is unavailable
- **Huge Pages**: Large reader pool buffers request `MADV_HUGEPAGE` on Linux
- **Buffer Pools**: Reusable buffer pools minimize allocation overhead and GC pressure

### I/O Optimizations

- **Sequential Advice**: Uses Linux sequential-access advice for opened transfer files
- **Sequential Reader**: Reads source blocks in order, directly into the buffer that is handed downstream (no extra per-block copy), and prefetches into a bounded queue
- **Read-Ahead Tuning**: Best-effort raises the kernel read-ahead window to 4MB for block-device endpoints; silently skipped on regular files or without root
- **Cache Hygiene**: Advises the kernel to drop already-read source pages (`POSIX_FADV_DONTNEED`) so large transfers don't evict useful read-ahead pages

### HDD-Friendly Design

- **Prefetch Window**: Configurable prefetch depth allows overlapping I/O and computation
- **Ordered Writes**: Destination writes from all concurrent connection handlers are serialized through a single writer into a near-sequential, offset-ordered stream, so a spinning destination disk avoids seek thrashing under `-w > 1` (per-block write acknowledgement is preserved)
- **Steady Writeback**: Periodic `sync_file_range` writeback keeps dirty pages flushing smoothly instead of in large bursts

### Network Optimizations

- **TCP Buffer Tuning**: Client connections request large send/receive buffers
- **Keep-Alive**: 30-second TCP keep-alive to detect stale connections early
- **Acknowledged Upload Writes**: A changed uploaded block succeeds only after destination write confirmation
- **Upload Retry**: Upload retries changed blocks after connection failure (up to 3 attempts per block)
- **Per-Operation Deadlines**: 5-minute I/O timeout prevents indefinite hangs

### Sparse File Handling

- **Zero Block Detection**: Zero payload bytes are omitted from the network
- **Destination Correctness**: Existing non-zero ranges are cleared when the source block is zero

### Concurrency Safety

- **Race Detection**: Full test suite passes with `-race` flag enabled

## 🔧 Technical Details

- **Checksum**: FNV-128a hash for block comparison
- **Compression**: Zstandard (zstd) with configurable levels
- **Encryption**: ChaCha20-Poly1305 AEAD cipher
- **Protocol**: Custom binary protocol over TCP (`ver0.02`; use matching binaries at both endpoints)
- **Concurrency**: Parallel upload checksum computation and compression
- **Reliability**: TCP keep-alive (30s), 5-minute I/O deadlines, acknowledged upload writes, and upload retry with reconnect (up to 3 attempts per block)

## 🚨 Requirements

- Go 1.25+ (as declared by `go.mod`)
- Network connectivity between servers
- SSH access (if using `-t` option)
- Read/write permissions on source/destination files

## 📝 License

See [LICENSE](LICENSE) file for details.
