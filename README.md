# bsync

🚀 **Efficient block-level synchronization tool for large data transfer between servers**

`bsync` transfers data between block devices or files block-by-block, comparing checksums and only transmitting differences with compression. Perfect for syncing large datasets, disk images, or virtual machine storage.

## ✨ Features

- **Smart Transfer**: Only transfers blocks that differ (checksum-based)
- **Compression**: Built-in compression for efficient network usage
- **SSH Integration**: Automatic remote server deployment via SSH
- **Multi-worker Support**: Parallel processing for faster transfers
- **Resume Capability**: Skip blocks to resume interrupted transfers
- **Progress Monitoring**: Real-time transfer progress with speed and ETA

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
| `-n` | Disable compression | false |
| `-t` | SSH target (`user@host:/remote_path`) | - |
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

### 3. High-Performance Transfer

**Multi-worker (4 workers selected) transfer with custom block size and label `backup`:**
```bash
./bsync -b 500M -w 4 -l backup -f /dev/sda -r remote-server:8080
```

### 4. Resume Interrupted Transfer

**Skip first 10 blocks to resume:**
```bash
./bsync -f /dev/sda -r remote-server:8080 -s 10
```

### 5. Quiet Mode for Scripts

```bash
./bsync -f /dev/sda -r remote-server:8080  -q
```

### 6. Bonus: it's also possible to sync local files
```bash
./bsync -n -f /tmp/src.img -t /tmp/dst.img
```

### 7. Download Mode Examples

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

## 🔍 Verification

Verify successful transfer:
```bash
md5sum /dev/shm/test-src /dev/shm/test-dst
```

Run tests:
```bash
make test
```

## 📊 Performance Tips

- **Block Size**: Larger blocks (200M-500M) for fast networks, smaller for slow connections
- **Workers**: Increase worker count (`-w`) for parallel processing
- **Compression**: Disable (`-n`) only if network is very fast and CPU is limited, or data is uncompressable (like video)
- **SSH**: Use `-t` for automatic remote server management

## 🚨 Requirements

- Go 1.16+ (for compilation)
- Network connectivity between servers
- SSH access (if using `-t` option)
- Read/write permissions on source/destination files

## 📝 License

See [LICENSE](LICENSE) file for details.
