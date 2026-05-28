# bsync Windows Support

## Overview

`bsync.exe` is a native Windows binary that can read physical drives and partitions
at the block level and sync them to a Linux server — without requiring `-t` (SSH
auto-deploy is not needed since the binaries are platform-specific).

## Building

```bash
# Cross-compile from Linux:
make windows
# Produces: bsync.exe (amd64)

# Or directly:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync.exe
```

**Important:** Both the Windows `.exe` and the Linux `bsync` binary must be compiled
from the same source. The binary protocol includes NTFS-related fields in the `Msg`
struct — an old Linux binary will desync with the new Windows client, causing
corrupted transfers and connection aborts.

## Usage

### List available drives

```
bsync.exe -a
```

Output example:
```
Physical drives (use with -f, requires Administrator):
  -f \\.\PhysicalDrive0            476.94 GB  (sector: 512 B)
  -f \\.\PhysicalDrive1            931.51 GB  (sector: 512 B)

Logical volumes (use with -f):
  -f \\.\C:                        476.94 GB total,  120.30 GB free  [fixed]
  -f \\.\D:                        931.51 GB total,  600.00 GB free  [fixed]
```

### Sync a physical drive to a Linux server

**Linux server (destination):**
```bash
./bsync -f /dev/vda -p 8080
```

**Windows client (source), run as Administrator:**
```
bsync.exe -f \\.\PhysicalDrive0 -r 192.168.1.100:8080
```

### Sync a logical volume

```
bsync.exe -f \\.\C: -r 192.168.1.100:8080
```

### All standard flags work

```
bsync.exe -f \\.\PhysicalDrive0 -r 192.168.1.100:8080 -b 50M -w 4 -L fast
```

## Requirements

- Run as **Administrator** to access physical drives (`\\.\PhysicalDriveN`) and
  locked volumes (`\\.\C:`)
- Windows 10 / Windows Server 2016 or later (for modern TCP stack behaviour)
- The Linux server must run a `bsync` binary built from the **same source version**

## Implementation Details

### New files added

| File | Build tag | Purpose |
|------|-----------|---------|
| `util_unix.go` | `!windows` | `getDeviceSize` (seek-based) and `truncateIfRegularFile` with `os.ModeDevice` check — original Linux implementation |
| `util_windows.go` | `windows` | `getDeviceSize` tries `Seek` first (regular files), falls back to `IOCTL_DISK_GET_DRIVE_GEOMETRY_EX` for physical drives; `truncateIfRegularFile` skips `\\.\` device paths |
| `drives_windows.go` | `windows` | `listDrives()` — enumerates `\\.\PhysicalDrive0..15` via `DeviceIoControl`, then logical volumes via `GetLogicalDriveStrings` + `GetDiskFreeSpaceEx` |
| `drives_stub.go` | `!windows` | Stub `listDrives()` pointing users to `lsblk` |

### Modified files

- **`util.go`** — removed `getDeviceSize` and `truncateIfRegularFile` (now in OS-specific files)
- **`main.go`** — added `-a` flag: calls `listDrives()` and exits
- **`Makefile`** — added `windows` target

### How device size is detected on Windows

`Seek(0, io.SeekEnd)` returns 0 for raw physical drives on Windows. The Windows
implementation falls back to:

```
IOCTL_DISK_GET_DRIVE_GEOMETRY_EX (0x000700A0)
```

which returns the exact drive size and sector size via `DeviceIoControl`.

### How physical drives are opened

Standard `os.OpenFile("\\\\.\\\PhysicalDrive0", os.O_RDONLY, 0)` works — Go's
runtime maps this to `CreateFile` with `FILE_SHARE_READ|FILE_SHARE_WRITE`, which
is the correct share mode for raw disk access. No special Windows API calls are
needed beyond the size query.

Block reads use `file.ReadAt(buf, offset)` which maps to `ReadFile` with an
`OVERLAPPED` offset. Since bsync block sizes are always multiples of 512 bytes and
block offsets are aligned to block size, reads are naturally sector-aligned.

## Notes

- The `-t` SSH flag compiles and links on Windows (OpenSSH is available on Windows 10+)
  but is not the intended workflow — use `-r` to connect to a manually started Linux server
- The `-x ntfs` flag (NTFS-aware transfer using ntfs-3g) is Linux-only; on Windows the
  OS handles NTFS natively and standard block reads work correctly
- NTFS metadata blocks (journal `$LogFile`, `$MFT`, volume bitmap) may change slightly
  between sync passes on an active Windows installation — this is expected and only
  affects a small number of blocks
