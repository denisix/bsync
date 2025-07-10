package main

import (
	"os"
	"os/exec"
	"strings"
	"bufio"
	"io"
	"strconv"
)

func parseSSHTarget(input string) (user, host, file string) {
	// Split the input by '@' to handle the optional user component
	parts := strings.SplitN(input, "@", 2)
	if len(parts) == 2 {
		user = parts[0]
		input = parts[1]
	}

	// Split the remaining part by ':' to separate host and file
	parts = strings.SplitN(input, ":", 2)
	if len(parts) != 2 {
		Log("invalid format: %s\n", input)
		os.Exit(1)
	}

	host = parts[0]
	file = parts[1]

	return user, host, file
}

func startRemoteSSH(targetPath, port string, blockSize, skipIdx uint32) (*exec.Cmd, error) {
	// split sshTarget "user@host:/remote/path" -> "user@host" and "/remote/path"
	user, host, file := parseSSHTarget(targetPath)

	if (user != "") {
		host = user + "@" + host
	}

	// run SSH command
	args := []string{
		"ssh", host,
		"bsync", "-f", file,
		"-p", port,
		"-b", strconv.FormatUint(uint64(blockSize), 10),
		"-s", strconv.FormatUint(uint64(skipIdx), 10),
	}
	Log("spawning ssh with args: %s\n", args)
	cmd := exec.Command(args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr // pass stderr through

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// wait for the "READY" signal from server
	Log("waiting for server..\n")
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		// Log("SSH server output: %s\n", line)
		if strings.Contains(line, "READY") {
			Log("remote server is READY\n")
			break
		}
	}
	cmd.Stdout = os.Stdout

	go io.Copy(os.Stdout, stdout) // keeps reading and printing

	return cmd, nil
}
