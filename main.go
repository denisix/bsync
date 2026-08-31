package main

import (
	"flag"
	"math/rand"
	"net"
	"os"
	"os/exec"
	rtdebug "runtime/debug" // aliased: this package already has a bool var named debug (server.go's -debug logging flag)
	"strconv"
	"strings"
	"sync/atomic"
)

var debug bool = false
var blockSize uint32 = 10485760
var mb1 float64 = 1048576.0
var prefetchDepth int // Configurable prefetch depth for SequentialReader
var vVisual atomic.Bool

func setVisual(enabled bool) {
	vVisual.Store(enabled)
}

func visualEnabled() bool {
	return vVisual.Load()
}

// pickHighPort returns a currently-free high (>10000) TCP port. It asks the OS
// for an unused port (so it won't collide with a still-lingering server), and
// falls back to a random high port if probing fails. There is a tiny window
// between probing and the server binding, but this is far more reliable than a
// blind random pick when launching transfers in rapid succession.
// parseExcludePatterns splits a comma-separated --exclude flag value into a
// clean pattern list for walkSource/excluded: each element trimmed, empty
// elements (from "", trailing/leading/doubled commas) dropped.
func parseExcludePatterns(raw string) []string {
	var patterns []string
	for _, pat := range strings.Split(raw, ",") {
		if pat = strings.TrimSpace(pat); pat != "" {
			patterns = append(patterns, pat)
		}
	}
	return patterns
}

func pickHighPort() string {
	for i := 0; i < 10; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			break
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if p > 10000 {
			return strconv.Itoa(p)
		}
	}
	return strconv.Itoa(10001 + rand.Intn(50000))
}

// dirOnlyFlags are the flags that only mean anything in directory-tree mode.
// A single list, rather than a validation branch per flag, so adding one
// cannot silently skip the check -- and so a test can assert the whole set
// (see TestDirOnlyFlagsRejectedInSingleFileMode).
var dirOnlyFlags = []string{"exclude", "quick-check", "ignore-times", "delete", "links", "specials"}

// firstDirOnlyFlagUsed returns the first directory-only flag the user
// actually PASSED, or "" if none. Keyed on what was passed rather than on the
// flags' values: quick-check now defaults to true, so testing its value here
// would reject every single-file transfer.
func firstDirOnlyFlagUsed(setFlags map[string]bool) string {
	for _, name := range dirOnlyFlags {
		if setFlags[name] {
			return name
		}
	}
	return ""
}

func main() {
	var device string
	var remoteAddr string
	var bSize uint
	var skipIdx uint
	var port string
	var bindIp string
	var noCompress bool
	var sshTarget string
	var logPrefix string
	var workers uint
	var quiet bool
	var reverse bool
	var encrypt bool
	var encKeyReceived string
	var compLevel string
	var listAllDrives bool
	var visual bool
	var suppressProgressFlag bool
	var forceDirMode bool
	var excludeFlag string
	var quickCheck bool
	var ignoreTimes bool
	var deleteFlag bool
	var syncLinks bool
	var syncSpecials bool
	var memLimitMB uint64

	flag.BoolVar(&listAllDrives, "a", false, "list available drives and partitions")
	flag.StringVar(&device, "f", "/dev/zero", "specify file or device, i.e. '/dev/vda'")
	flag.StringVar(&remoteAddr, "r", "", "specify remote address of server")
	flag.UintVar(&bSize, "b", uint(blockSize), "block size, default 100M")
	flag.UintVar(&skipIdx, "s", 0, "skip blocks, default 0")
	flag.StringVar(&port, "p", "8080", "bind to port, default 8080")
	flag.StringVar(&bindIp, "i", "0.0.0.0", "bind to IP, default 0.0.0.0")
	flag.BoolVar(&noCompress, "n", false, "do not compress blocks (by default compress)")
	flag.StringVar(&compLevel, "L", "default", "compression level: fast, default, better, best")
	flag.StringVar(&sshTarget, "t", "", "launch remote server via ssh: user@host:/remote_path")
	flag.StringVar(&logPrefix, "l", "", "custom log prefix")
	flag.UintVar(&workers, "w", 1, "workers count, default 1")
	flag.BoolVar(&quiet, "q", false, "be quiet, without output")
	flag.BoolVar(&reverse, "d", false, "download mode: transfer from server to client")
	flag.BoolVar(&encrypt, "e", false, "enable SSH-mode encryption (auto-generates key)")
	flag.StringVar(&encKeyReceived, "K", "", "encryption key (internal use)")
	flag.BoolVar(&suppressProgressFlag, "P", false, "suppress server progress output (set automatically when launched via -t)")
	flag.IntVar(&prefetchDepth, "prefetch", 0, "prefetch depth (default: workers*4)")
	var autoTune bool
	flag.BoolVar(&autoTune, "auto", false, "auto-tune parameters for optimal HDD performance")
	flag.BoolVar(&visual, "v", false, "enable visual block-by-block progress display")
	flag.BoolVar(&forceDirMode, "R", false, "force recursive directory-tree mode (auto-detected from -f when it's already a directory)")
	flag.StringVar(&excludeFlag, "exclude", "", "comma-separated glob patterns to skip in directory mode (matched against each entry's full relative path and its base name)")
	flag.BoolVar(&quickCheck, "quick-check", true, "directory mode only: skip a file entirely if the destination already has the same size and modification time (second precision), like rsync's quick check; on by default -- use -ignore-times to compare every block instead")
	flag.BoolVar(&ignoreTimes, "ignore-times", false, "directory mode only: never skip a file on matching size+modification time; compare every block of every file (like rsync's -I). Slower, but catches a file rewritten within the same second at the same size")
	flag.BoolVar(&deleteFlag, "delete", false, "directory mode only: before syncing, remove destination files/directories not present in the source (like rsync's --delete-before); excluded and symlinked/special-file paths are protected, not removed")
	flag.BoolVar(&syncLinks, "links", false, "directory mode only: recreate source symlinks at the destination with identical target paths (like rsync -l); without it symlinks are skipped")
	flag.BoolVar(&syncSpecials, "specials", false, "directory mode only: recreate source FIFOs (named pipes) and unix sockets at the destination (like rsync --specials); device nodes are still skipped, and without it all special files are skipped")
	flag.Uint64Var(&memLimitMB, "memlimit", 0, "soft memory limit in MB for this process (0 = off; the GOMEMLIMIT environment variable is honored either way). A safety valve against an unexpectedly large tree, NOT a fix for a large live heap -- against one of those it makes the GC work harder rather than freeing memory that's still genuinely in use")

	flag.Parse() // after declaring flags we need to call it

	// Applied as early as possible, before any allocation-heavy path
	// branches below. debug.SetMemoryLimit is a SOFT ceiling: the GC works
	// harder as the heap approaches it, and only refuses to grow past it if
	// it can -- it is a safety valve against an unexpectedly large tree,
	// not a substitute for the structural fixes that keep steady-state
	// memory bounded in the first place. Applying it against a genuinely
	// large live heap produces a GC death spiral (throughput collapses
	// while memory stays high) rather than relief, so this is deliberately
	// opt-in (default 0 = off) rather than a derived default. Go already
	// honors the GOMEMLIMIT environment variable natively regardless of
	// this flag, so operators have that escape hatch even without it.
	if memLimitMB > 0 {
		rtdebug.SetMemoryLimit(int64(memLimitMB) << 20)
		Log("soft memory limit set to %d MB\n", memLimitMB)
	}

	// Which flags the user actually PASSED. quick-check now defaults to true,
	// so the "directory mode only" validation below can no longer test its
	// value -- that would reject every single-file transfer. Only an explicit
	// mention is an error there.
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	// -ignore-times is the opt-out: it wins over -quick-check however the two
	// are combined, so a script that passes both gets the conservative
	// (compare-everything) behavior it asked for.
	if ignoreTimes {
		quickCheck = false
	}

	if listAllDrives {
		listDrives()
		return
	}

	// BSYNC_CPUPROFILE=<path> writes a CPU profile covering the whole
	// transfer, for `go tool pprof`. Env var rather than a flag: it is a
	// developer tool, and -t forwards flags to the remote side, where a
	// profile path would be meaningless. Best-effort -- a profile that
	// cannot be started must never abort a transfer.
	if stop := startCPUProfile(os.Getenv("BSYNC_CPUPROFILE")); stop != nil {
		defer stop()
	}

	// Set compression level
	SetCompressionLevel(compLevel)

	// Set visual mode
	setVisual(visual)

	// suppressProgress is mutex-guarded (server.go); flag.BoolVar can't write
	// through that guard, so parse into a local and set it properly here,
	// before any goroutine that might read it starts.
	setSuppressProgress(suppressProgressFlag)

	if bSize == 0 || uint64(bSize) > uint64(^uint32(0)) {
		Err("Block size must be between 1 and %d bytes\n", ^uint32(0))
	}
	blockSize = uint32(bSize)
	// The zstd window is derived from blockSize (see zstdWindowSize), and
	// SetCompressionLevel ran above, before blockSize was known. Rebuild the
	// encoder pool now that both inputs are final.
	resetEncoderPool()
	if workers == 0 {
		Err("Worker count must be at least 1\n")
	}
	if prefetchDepth < 0 {
		Err("Prefetch depth cannot be negative\n")
	}

	// Directory-tree mode: -R forces it; otherwise auto-detected from -f
	// already being a directory. os.Stat (not Lstat) so a symlinked source
	// root is followed. On the destination side of a push to a path that
	// doesn't exist yet, Stat can't help -- that's exactly why the client
	// side (which DOES know: its own -f must exist) forwards -R to the
	// remote/local server process explicitly (see ssh.go's startRemoteSSH),
	// rather than every invocation re-deriving it independently.
	dirMode := forceDirMode
	if !dirMode {
		if st, statErr := os.Stat(device); statErr == nil && st.IsDir() {
			dirMode = true
		}
	}
	if dirMode {
		// Directory mode is upload/push only, always block-level (no -s
		// shortcut), has its own always-on progress line (not the -v grid,
		// which assumes a single flat block-index space one file wide),
		// and doesn't support HDD auto-tuning (auto-tune profiles a single
		// file's access pattern, which doesn't generalize across a tree of
		// arbitrarily different files).
		if reverse {
			Err("directory mode does not support -d (download); only upload/push is supported\n")
		}
		if skipIdx != 0 {
			Err("directory mode does not support -s (every file is resumed automatically and independently)\n")
		}
		if autoTune {
			Err("directory mode does not support -auto\n")
		}
		if visual {
			Err("directory mode does not support -v (visual block-grid display); its own progress line is always shown instead\n")
		}
	} else {
		if name := firstDirOnlyFlagUsed(setFlags); name != "" {
			Err("-%s is only supported in directory mode (-R or a directory -f target)\n", name)
		}
	}
	excludePatterns := parseExcludePatterns(excludeFlag)

	// Handle encryption
	if encKeyReceived != "" {
		if err := SetEncryptionKey(encKeyReceived); err != nil {
			Err("Invalid encryption key: %v\n", err)
		}
	} else if encrypt && sshTarget != "" {
		// Auto-generate key for SSH transfer
		encryptionKey = GenerateEncryptionKey()
		Log("encryption enabled with auto-generated key\n")
	} else if encrypt {
		Err("The -e option requires SSH mode (-t); direct network mode requires a shared key via -K\n")
	}

	// Set default prefetch depth if not specified
	if prefetchDepth == 0 {
		prefetchDepth = int(workers) * 4 // Default: workers * 4
	}

	// Auto-tune parameters if requested
	if autoTune {
		// Determine source file for tuning
		var srcFile *os.File
		var err error
		var dstDir string

		// For client mode (upload), use the source file
		if remoteAddr != "" && !reverse {
			srcFile, err = os.Open(device)
			if err != nil {
				Err("cannot open source file for tuning: %s\n", err.Error())
				return
			}
			defer srcFile.Close()
			AdviseSequential(srcFile) // Optimise for sequential access
			TuneHDDReadAhead(device)  // Raise HDD read-ahead (best-effort)
			dstDir = "/tmp"           // Default temp directory
		} else {
			// Auto-tuning not supported in server/download modes yet
			Log("Auto-tuning is currently only supported in client upload mode\n")
			Log("Run with -auto on the client side when pushing data to a server\n")
			autoTune = false
		}

		if autoTune {
			fileSize := getDeviceSize(srcFile)
			tunedConfig := AutoTune(srcFile, dstDir, int64(fileSize))

			// Apply auto-tuned settings only if user didn't explicitly set them
			bSizeSet := false
			workersSet := false
			prefetchSet := false
			compSet := false
			flag.Visit(func(f *flag.Flag) {
				if f.Name == "b" {
					bSizeSet = true
				}
				if f.Name == "w" {
					workersSet = true
				}
				if f.Name == "prefetch" {
					prefetchSet = true
				}
				if f.Name == "n" || f.Name == "L" {
					compSet = true
				}
			})

			if !bSizeSet {
				blockSize = tunedConfig.BlockSize
				Log("Auto-tuned block size: %d MB\n", blockSize/(1024*1024))
			}
			if !workersSet {
				workers = uint(tunedConfig.Workers)
				Log("Auto-tuned workers: %d\n", workers)
			}
			if !prefetchSet {
				prefetchDepth = tunedConfig.PrefetchDepth
				Log("Auto-tuned prefetch depth: %d\n", prefetchDepth)
			}
			if !compSet {
				noCompress = tunedConfig.NoCompress
				if !noCompress {
					compLevel = tunedConfig.CompLevel
					SetCompressionLevel(compLevel)
				}
				Log("Auto-tuned compression: %s\n", configCompStr(tunedConfig))
			}

			// Always apply TCP buffer size (no user flag for it yet)
			// IMPORTANT: This sets the global variable before ANY connections are created
			SetTCPBufferSize(tunedConfig.TCPBufSize)
			Log("Applying auto-tuned TCP buffer: %d MB (will be used for all new connections)\n", tunedConfig.TCPBufSize/(1024*1024))
			Log("\n")
		}
	}

	// When launching the remote side via -t and the user didn't explicitly set
	// -p, pick a free high port so multiple concurrent transfers from the same
	// host don't collide on the default 8080.
	if sshTarget != "" {
		portSet := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "p" {
				portSet = true
			}
		})
		if !portSet {
			port = pickHighPort()
			Log("using transfer port %s\n", port)
		}
	}

	if sshTarget != "" {
		_, host, _, _ := parseSSHTarget(sshTarget)
		remoteAddr = host + ":" + port
	}

	if remoteAddr != "" {
		if reverse {
			// CLIENT in download mode: receive from server
			SetLog(logPrefix, "[client-download]", quiet)
			Log("starting client download mode, transfer: %s <- %s\n", device, remoteAddr)

			// launch SSH if sshTarget provided (for download mode, server needs different args)
			var sshCmd *exec.Cmd
			if sshTarget != "" {
				Log("launching remote server via SSH: %s\n", sshTarget)
				var err error
				sshCmd, err = startRemoteSSHDownload(sshTarget, port, blockSize, uint32(skipIdx), quiet, noCompress, compLevel)
				if err != nil {
					Err("Error: SSH launch failed: %s\n", err)
					return
				}
				defer sshCmd.Wait()
			}

			file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				if sshCmd != nil {
					sshCmd.Process.Kill()
					sshCmd.Wait()
				}
				Err("opening file: %s\n", err.Error())
				return
			}
			defer file.Close()
			AdviseSequential(file)   // Optimise for sequential access
			TuneHDDReadAhead(device) // Raise HDD read-ahead (best-effort)

			if err := startClientDownload(file, remoteAddr, uint32(skipIdx), blockSize, noCompress, int(workers)); err != nil {
				Err("download failed: %s\n", err)
			}

			// cleanup SSH
			if sshCmd != nil {
				Log("waiting for remote process to finish\n")
				sshCmd.Wait()
				Log("DONE, exiting\n")
			}
		} else if dirMode {
			// CLIENT: directory-tree source (upload/push)
			SetLog(logPrefix, "[client]", quiet)
			Log("starting client, tree transfer: %s -> %s\n", device, remoteAddr)

			var sshCmd *exec.Cmd
			if sshTarget != "" {
				Log("launching remote server via SSH: %s\n", sshTarget)
				var err error
				sshCmd, err = startRemoteSSH(sshTarget, port, blockSize, uint32(skipIdx), quiet, noCompress, true, int(workers), memLimitMB)
				if err != nil {
					Err("Error: SSH launch failed: %s\n", err)
					return
				}
				defer sshCmd.Wait()
			}

			sess, err := startDirClient(device, remoteAddr, blockSize, noCompress, int(workers), excludePatterns, quickCheck, deleteFlag, syncLinks, syncSpecials)
			if err != nil {
				if sshCmd != nil {
					sshCmd.Process.Kill()
					sshCmd.Wait()
				}
				Err("directory sync failed: %s\n", err.Error())
				return
			}
			if sess.FailedCount() > 0 {
				Log("synced with %d failed file(s); see earlier log lines for details\n", sess.FailedCount())
			} else {
				Log("directory sync complete: %d files, %s\n", sess.FileCount(), sess.TransferSummary())
			}

			if sshCmd != nil {
				Log("waiting for remote process to finish\n")
				sshCmd.Wait()
				Log("DONE, exiting\n")
			}
			// Exit-code contract: 0 = clean, 1 = anything wrong, including a
			// partial per-file failure that did not abort the transfer as a
			// whole (matching Err()'s exit code for every other failure
			// path in this file).
			if sess.FailedCount() > 0 {
				os.Exit(1)
			}
		} else {
			// CLIENT: source file (original upload mode)
			SetLog(logPrefix, "[client]", quiet)
			Log("starting client, transfer: %s -> %s\n", device, remoteAddr)

			// launch SSH if sshTarget provided
			var sshCmd *exec.Cmd
			if sshTarget != "" {
				Log("launching remote server via SSH: %s\n", sshTarget)
				var err error
				sshCmd, err = startRemoteSSH(sshTarget, port, blockSize, uint32(skipIdx), quiet, noCompress, false, int(workers), memLimitMB)
				if err != nil {
					Err("Error: SSH launch failed: %s\n", err)
					return
				}
				defer sshCmd.Wait()
			}

			file, err := os.OpenFile(device, os.O_RDONLY, 0666)
			if err != nil {
				if sshCmd != nil {
					sshCmd.Process.Kill()
					sshCmd.Wait()
				}
				Err("opening file: %s\n", err.Error())
				return
			}
			defer file.Close()
			AdviseSequential(file)   // Optimise for sequential access
			TuneHDDReadAhead(device) // Raise HDD read-ahead (best-effort)

			fileSize := getDeviceSize(file)
			checksumCache := NewChecksumCache(0)
			if fileSize > 0 {
				lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))
				checksumCache = NewChecksumCache(lastBlockNum)
			}
			// Note: startClient now handles checksum precomputation with sequential reader

			if err := startClient(file, remoteAddr, uint32(skipIdx), fileSize, blockSize, noCompress, checksumCache, int(workers)); err != nil {
				Err("upload failed: %s\n", err)
			}

			// cleanup SSH
			if sshCmd != nil {
				Log("waiting for remote process to finish\n")
				sshCmd.Wait()
				Log("DONE, exiting\n")
			}
		}
	} else {
		if reverse {
			// SERVER in upload mode: send to client
			SetLog(logPrefix, "[server-upload]", quiet)
			Log("starting server upload mode, %s -> remote\n", device)
			file, err := os.OpenFile(device, os.O_RDONLY, 0666)
			if err != nil {
				Err("Error opening file: %s\n", err.Error())
				return
			}
			defer file.Close()
			AdviseSequential(file)   // Optimise for sequential access
			TuneHDDReadAhead(device) // Raise HDD read-ahead (best-effort)

			fileSize := getDeviceSize(file)
			// No checksum precompute here: a download source never compares
			// hashes (the client writes every block it receives), and the
			// precompute pass used to run a SECOND full sequential read of
			// the device concurrently with the serving reader -- two seek
			// streams on one spindle -- while the serving loop blocked on
			// its (discarded) result. The cache is passed empty only to keep
			// startServerUpload's signature shared with the tests.
			checksumCache := NewChecksumCache(0)

			if err := startServerUpload(file, bindIp, port, fileSize, checksumCache, int(workers), noCompress); err != nil {
				Err("download source failed: %s\n", err)
			}
		} else if dirMode {
			// SERVER: directory-tree destination
			SetLog(logPrefix, "[server]", quiet)
			Log("starting server, tree destination: remote -> %s\n", device)
			if err := startDirServer(device, bindIp, port, int(workers), quiet); err != nil {
				Err("directory destination failed: %s\n", err.Error())
			}
		} else {
			// SERVER: destination file (original download mode)
			SetLog(logPrefix, "[server]", quiet)
			Log("starting server, remote -> %s\n", device)
			file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				Err("Error opening file: %s\n", err.Error())
				return
			}
			defer file.Close()
			AdviseSequential(file)   // Optimise for sequential access
			TuneHDDReadAhead(device) // Raise HDD read-ahead (best-effort)

			// The sender's first request defines the block size. Hash blocks
			// on request after negotiation rather than caching with a local default.
			checksumCache := NewChecksumCache(0)
			if err := startServer(file, bindIp, port, checksumCache, int(workers)); err != nil {
				Err("upload destination failed: %s\n", err)
			}
		}
	}
}
