package main

import (
	"flag"
	"os"
	"os/exec"
)

var debug bool = false
var blockSize uint32 = 10485760
var mb1 float64 = 1048576.0

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
	flag.BoolVar(&encrypt, "e", false, "enable encryption (auto-generates key)")
	flag.StringVar(&encKeyReceived, "K", "", "encryption key (internal use)")

	flag.Parse() // after declaring flags we need to call it

	if listAllDrives {
		listDrives()
		return
	}

	// Set compression level
	SetCompressionLevel(compLevel)

	blockSize = uint32(bSize)

	if blockSize == 0 {
		Err("Block size cannot be zero\n")
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
				sshCmd, err = startRemoteSSHDownload(sshTarget, port, blockSize, uint32(skipIdx), quiet, noCompress)
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

			startClientDownload(file, remoteAddr, uint32(skipIdx), blockSize, noCompress, int(workers))

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

			fileSize := getDeviceSize(file)
			if fileSize == 0 {
				if sshCmd != nil {
					sshCmd.Process.Kill()
					sshCmd.Wait()
				}
				Err("Error: zero source file: %s\n", device)
			}

			lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

			checksumCache := NewChecksumCache(lastBlockNum)
			// Note: startClient now handles checksum precomputation with sequential reader

			startClient(file, remoteAddr, uint32(skipIdx), fileSize, blockSize, noCompress, checksumCache, int(workers))

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

			fileSize := getDeviceSize(file)
			if fileSize == 0 {
				Err("Error: zero source file: %s\n", device)
			}
			lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

			checksumCache := NewChecksumCache(lastBlockNum)
			go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx), int(workers))

			startServerUpload(file, bindIp, port, fileSize, checksumCache, int(workers))
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

			fileSize := getDeviceSize(file)
			lastBlockNum := uint32((fileSize - 1) / uint64(blockSize))

			checksumCache := NewChecksumCache(lastBlockNum)
			go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx), int(workers))

			startServer(file, bindIp, port, checksumCache)
		}
	}
}
