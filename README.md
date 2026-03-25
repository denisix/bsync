# bsync

🚀 **Efficient block-level synchronization tool for large data transfer between servers**

`bsync` transfers data between block devices or files block-by-block, comparing checksums and only transmitting differences with compression. Perfect for syncing large datasets, disk images, or virtual machine storage.

## ✨ Features

- **Smart Transfer**: Only transfers blocks that differ (checksum-based)
- **Sparse File Support**: Efficiently handles zero blocks - preserves holes, no data transfer
- **Compression**: Built-in zstd compression with configurable levels (fast/default/better/best)
- **Encryption**: Optional ChaCha20-Poly1305 encryption for secure transfers
- **SSH Integration**: Automatic remote server deployment via SSH
- **Multi-worker Support**: Parallel processing with HDD-friendly sequential reads
- **Resume Capability**: Skip blocks to resume interrupted transfers
- **Progress Monitoring**: Real-time transfer progress with speed and ETA
- **Download Mode**: Transfer from server to client (reverse direction)
- **IP Binding**: Bind server to specific network interface

## 🛠️ Installation

```bash
make
```

Copy the `bsync` binary to your source and destination servers.

## 📖 Usage

### Basic Operation

`bsync` operates in two modes:
- **Server mode**: Receives data (destination)
- **Client mode**: Sends data (source)

### Command Line Options

| Option | Description | Default |
|--------|-------------|---------|
| `-f` | File or device path (e.g., `/dev/vda`, `/path/to/file`) | `/dev/zero` |
| `-r` | Remote server address (`host:port`) | - |
| `-b` | Block size in bytes | 10485760 (10MB) |
| `-s` | Skip blocks (for resume) | 0 |
| `-p` | Server port | 8080 |
| `-i` | Bind to specific IP address | `0.0.0.0` |
| `-n` | Disable compression | false |
| `-e` | Enable encryption (auto-generates key) | false |
| `-L` | Compression level: `fast`, `default`, `better`, `best` | `default` |
| `-t` | SSH target (`user@host:/remote_path` or `user@host:port:/remote_path`) | - |
| `-l` | Custom log prefix | - |
| `-w` | Number of workers | 1 |
| `-q` | Quiet mode (no output) | false |
| `-d` | Download mode: transfer from server to client | false |

## 🔄 Examples

### 1. Local Network Transfer

**Destination server:**
```bash
./bsync -f /dev/shm/test-dst -p 8080
```

**Source server:**
```bash
./bsync -b 200M -f /dev/shm/test-src -r 192.168.1.100:8080
```

### 2. SSH-Automated Transfer

**Single command (automatically starts remote server):**
```bash
./bsync -b 200M -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst
```

### 3. Encrypted Transfer

**Secure transfer with auto-generated encryption key:**
```bash
./bsync -e -f /dev/shm/test-src -t user@remote-server:/dev/shm/test-dst
```

The `-e` flag enables ChaCha20-Poly1305 encryption. A 32-byte key is auto-generated and securely passed to the remote server via SSH. Data is encrypted after compression and decrypted before decompression.

### 4. High-Performance Transfer

**Multi-worker (4 workers) transfer with custom block size:**
```bash
./bsync -b 500M -w 4 -f /dev/sda -r remote-server:8080
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

### 7. Quiet Mode for Scripts

```bash
./bsync -f /dev/sda -r remote-server:8080 -q
```

### 8. Local File Sync

```bash
./bsync -n -f /tmp/src.img -t /tmp/dst.img
```

### 9. Download Mode (Server → Client)

**Server (upload mode):**
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

`bsync` efficiently handles sparse files:
- Zero blocks are detected and not transferred over the network
- Sparse holes are preserved on the destination
- Saves bandwidth and disk space for files with lots of zeros

```bash
# Create sparse file
truncate -s 100G /tmp/sparse.img

# Transfer - only actual data is sent
./bsync -f /tmp/sparse.img -t user@remote:/backup/sparse.img
```

## 🔍 Verification

Verify successful transfer:
```bash
md5sum /dev/shm/test-src /dev/shm/test-dst
```

Run comprehensive tests:
```bash
make test
```

## 📊 Performance Tips

- **Block Size**: Larger blocks (200M-500M) for fast networks, smaller for slow connections
- **Workers**: Increase worker count (`-w 4` or `-w 8`) for parallel processing on SSDs
- **Compression**:
  - Use `-L fast` for high-speed networks where CPU is the bottleneck
  - Use `-L best` for slow networks to minimize data transfer
  - Disable (`-n`) only if data is uncompressible (video, already compressed)
- **SSH**: Use `-t` for automatic remote server management
- **Bind IP**: Use `-i` to select specific network interface for multi-homed servers

## 🔧 Technical Details

- **Checksum**: FNV-128a hash for block comparison
- **Compression**: Zstandard (zstd) with configurable levels
- **Encryption**: ChaCha20-Poly1305 AEAD cipher
- **Protocol**: Custom binary protocol over TCP
- **Concurrency**: Parallel checksum computation and compression

## 🚨 Requirements

- Go 1.16+ (for compilation)
- Network connectivity between servers
- SSH access (if using `-t` option)
- Read/write permissions on source/destination files

## 📝 License

See [LICENSE](LICENSE) file for details.
