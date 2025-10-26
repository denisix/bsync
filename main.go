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

	flag.StringVar(&device, "f", "/dev/zero", "specify file or device, i.e. '/dev/vda'")
	flag.StringVar(&remoteAddr, "r", "", "specify remote address of server")
	flag.UintVar(&bSize, "b", uint(blockSize), "block size, default 100M")
	flag.UintVar(&skipIdx, "s", 0, "skip blocks, default 0")
	flag.StringVar(&port, "p", "8080", "bind to port, default 8080")
	flag.StringVar(&bindIp, "i", "0.0.0.0", "bind to IP, default 0.0.0.0")
	flag.BoolVar(&noCompress, "n", false, "do not compress blocks (by default compress)")
	flag.StringVar(&sshTarget, "t", "", "launch remote server via ssh: user@host:/remote_path")
	flag.StringVar(&logPrefix, "l", "", "custom log prefix")
	flag.UintVar(&workers, "w", 1, "workers count, default 1")
	flag.BoolVar(&quiet, "q", false, "be quiet, without output")

	flag.Parse() // after declaring flags we need to call it

	blockSize = uint32(bSize)

	if blockSize == 0 {
		Err("Block size cannot be zero\n")
	}

	if sshTarget != "" {
		_, host, _ := parseSSHTarget(sshTarget)
		remoteAddr = host + ":" + port
	}

	if remoteAddr != "" {
		// CLIENT: source file
		SetLog(logPrefix, "[client]", quiet)
		Log("starting client, transfer: %s -> %s\n", device, remoteAddr)

		// launch SSH if sshTarget provided
		var sshCmd *exec.Cmd
		if sshTarget != "" {
			Log("launching remote server via SSH: %s\n", sshTarget)
			var err error
			sshCmd, err = startRemoteSSH(sshTarget, port, blockSize, uint32(skipIdx), quiet)
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
		SetLog(logPrefix, "[server]", quiet)
		Log("starting server, remote -> %s\n", device)
		file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			Err("Error opening file: %s\n", err.Error())
			return
		}
		defer file.Close()

		fileSize := getDeviceSize(file)
		lastBlockNum := uint32(fileSize / uint64(blockSize))

		checksumCache := NewChecksumCache(lastBlockNum)
		go precomputeChecksums(file, blockSize, lastBlockNum, checksumCache, uint32(skipIdx))

		startServer(file, bindIp, port, checksumCache)
	}
}
