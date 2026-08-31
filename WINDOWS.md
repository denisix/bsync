# bsync Windows Support

`bsync.exe` is a native Windows binary that reads physical drives and partitions at the block level and syncs them to a Linux server. No SSH auto-deploy needed (`-t`) since binaries are platform-specific — run the Linux side manually and connect with `-r`.

## Building

```bash
make windows   # cross-compile from Linux -> bsync.exe (amd64)
# or directly:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync.exe
```

Both the Windows `.exe` and the Linux `bsync` binary must be built from the same source — the wire protocol is not version-tolerant; a mismatched pair desyncs and corrupts the transfer.

## Usage

```
bsync.exe -a   # list drives (Administrator)
```
```
Physical drives (use with -f, requires Administrator):
  -f \\.\PhysicalDrive0            476.94 GB  (sector: 512 B)

Logical volumes (use with -f):
  -f \\.\C:                        476.94 GB total,  120.30 GB free  [fixed]
```

```bash
# Linux server (destination)
./bsync -f /dev/vda -p 8080

# Windows client (source), run as Administrator
bsync.exe -f \\.\PhysicalDrive0 -r 192.168.1.100:8080

# Logical volume
bsync.exe -f \\.\C: -r 192.168.1.100:8080

# All standard flags work
bsync.exe -f \\.\PhysicalDrive0 -r 192.168.1.100:8080 -b 50M -w 4 -L fast
```

## Requirements

- Administrator, to access physical drives (`\\.\PhysicalDriveN`) and locked volumes (`\\.\C:`)
- Windows 10 / Windows Server 2016+
- The Linux server must run a `bsync` binary built from the same source version

## Implementation

| File | Build tag | Purpose |
|------|-----------|---------|
| `util_unix.go` | `!windows` | `getDeviceSize` (seek-based), `truncateIfRegularFile` |
| `util_windows.go` | `windows` | `getDeviceSize` tries `Seek` then falls back to `IOCTL_DISK_GET_DRIVE_GEOMETRY_EX` for physical drives; `truncateIfRegularFile` skips `\\.\` device paths |
| `drives_windows.go` | `windows` | `listDrives()` — enumerates `\\.\PhysicalDrive0..15` via `DeviceIoControl`, then logical volumes via `GetLogicalDriveStrings`/`GetDiskFreeSpaceEx` |
| `drives_stub.go` | `!windows` | Stub `listDrives()` pointing to `lsblk` |

`Seek(0, io.SeekEnd)` returns 0 for raw physical drives on Windows, hence the `IOCTL_DISK_GET_DRIVE_GEOMETRY_EX` (`0x000700A0`) fallback for exact drive/sector size. Opening (`os.OpenFile` → `CreateFile` with `FILE_SHARE_READ|FILE_SHARE_WRITE`) and reading (`ReadAt` → `ReadFile` with `OVERLAPPED`) need no special calls beyond the size query — block sizes and offsets are always 512-byte-aligned, so reads are naturally sector-aligned.

## Notes

- `-t` compiles and links on Windows (OpenSSH ships with Windows 10+) but isn't the intended workflow — use `-r` against a manually started Linux server.
- NTFS metadata blocks (`$LogFile`, `$MFT`, volume bitmap) can change slightly between passes on a live Windows install; this is expected and affects only a few blocks.
