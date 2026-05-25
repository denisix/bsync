package main

import (
	"flag"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
)

var debug bool = false
var blockSize uint32 = 10485760
var mb1 float64 = 1048576.0
var prefetchDepth int // Configurable prefetch depth for SequentialReader

// pickHighPort returns a currently-free high (>10000) TCP port. It asks the OS
// for an unused port (so it won't collide with a still-lingering server), and
// falls back to a random high port if probing fails. There is a tiny window
// between probing and the server binding, but this is far more reliable than a
// blind random pick when launching transfers in rapid succession.
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
	flag.BoolVar(&suppressProgress, "P", false, "suppress server progress output (set automatically when launched via -t)")
	flag.IntVar(&prefetchDepth, "prefetch", 0, "prefetch depth (default: workers*4)")
	var autoTune bool
	flag.BoolVar(&autoTune, "auto", false, "auto-tune parameters for optimal HDD performance")

	flag.Parse() // after declaring flags we need to call it

	if listAllDrives {
		listDrives()
		return
	}

	// Set compression level
	SetCompressionLevel(compLevel)

	if bSize == 0 || uint64(bSize) > uint64(^uint32(0)) {
		Err("Block size must be between 1 and %d bytes\n", ^uint32(0))
	}
	blockSize = uint32(bSize)
	if workers == 0 {
		Err("Worker count must be at least 1\n")
	}
	if prefetchDepth < 0 {
		Err("Prefetch depth cannot be negative\n")
	}

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
		} else {
			// CLIENT: source file (original upload mode)
			SetLog(logPrefix, "[client]", quiet)
			Log("starting client, transfer: %s -> %s\n", device, remoteAddr)

			// launch SSH if sshTarget provided
			var sshCmd *exec.Cmd
			if sshTarget != "" {
				Log("launching remote server via SSH: %s\n", sshTarget)
				var err error
				sshCmd, err = startRemoteSSH(sshTarget, port, blockSize, uint32(skipIdx), quiet, noCompress)
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
			checksumCache := NewChecksumCache(0)
			if fileSize > 0 {
				lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))
				checksumCache = NewChecksumCache(lastBlockNum)
				go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx), int(workers))
			}

			if err := startServerUpload(file, bindIp, port, fileSize, checksumCache, int(workers), noCompress); err != nil {
				Err("download source failed: %s\n", err)
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
