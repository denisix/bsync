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
	var noCompress bool
	var sshTarget string
	var logPrefix string
	var workers uint

	flag.StringVar(&device, "f", "/dev/zero", "specify file or device, i.e. '/dev/vda'")
	flag.StringVar(&remoteAddr, "r", "", "specify remote address of server")
	flag.UintVar(&bSize, "b", uint(blockSize), "block size, default 100M")
	flag.UintVar(&skipIdx, "s", 0, "skip blocks, default 0")
	flag.StringVar(&port, "p", "8080", "bind to port, default 8080")
	flag.BoolVar(&noCompress, "n", false, "do not compress blocks (by default compress)")
	flag.StringVar(&sshTarget, "t", "", "launch remote server via ssh: user@host:/remote_path")
	flag.StringVar(&logPrefix, "l", "", "custom log prefix")
	flag.UintVar(&workers, "w", 1, "workers count, default 1")

	flag.Parse() // after declaring flags we need to call it

	blockSize = uint32(bSize)

	if sshTarget != "" {
		_, host, _ := parseSSHTarget(sshTarget)
		remoteAddr = host + ":" + port
	}

	if remoteAddr != "" {
		// CLIENT: source file
		SetLogPrefix(logPrefix, "[client]")
		Log("starting client, transfer: %s -> %s\n", device, remoteAddr)

		// launch SSH if sshTarget provided
		var sshCmd *exec.Cmd
		if sshTarget != "" {
			Log("launching remote server via SSH: %s\n", sshTarget)
			var err error
			sshCmd, err = startRemoteSSH(sshTarget, port, blockSize, uint32(skipIdx))
			if err != nil {
				Log("Error: SSH launch failed: %s\n", err)
				return
			}
			defer sshCmd.Wait()
		}

		file, err := os.OpenFile(device, os.O_RDONLY, 0666)
		if err != nil {
			Log("Error: opening file: %s\n", err.Error())
			return
		}
		defer file.Close()

		fileSize := getDeviceSize(file)
		lastBlockNum := uint32(fileSize / uint64(blockSize))

		checksumCache := NewChecksumCache(lastBlockNum)
		go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx))

		startClient(file, remoteAddr, uint32(skipIdx), fileSize, blockSize, noCompress, checksumCache, int(workers))

		// cleanup SSH
		if sshCmd != nil {
			Log("waiting for remote process to finish\n")
			sshCmd.Wait()
			Log("DONE, exiting\n")
		}

	} else {
		// SERVER: destination file
		SetLogPrefix(logPrefix, "[server]")
		Log("starting server, remote -> %s\n", device)
		file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			Log("[server] Error opening file: %s\n", err.Error())
			return
		}
		defer file.Close()

		fileSize := getDeviceSize(file)
		lastBlockNum := uint32(fileSize / uint64(blockSize))

		checksumCache := NewChecksumCache(lastBlockNum)
		go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx))

		startServer(file, port, checksumCache)
	}
}
