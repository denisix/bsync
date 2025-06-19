package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func parseSSHTarget(input string) (user, host, file string) {
	// If input doesn't contain ':', it's a local file
	if !strings.Contains(input, ":") {
		return "", "", input
	}

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

func waitForReady(stdout io.ReadCloser, isLocal bool) error {
	Log("waiting for server..\n")
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		Log("server-> %s\n", line)
		if strings.Contains(line, "READY") {
			serverType := "remote"
			if isLocal {
				serverType = "local"
			}
			Log("%s server is ready\n", serverType)
			break
		}
	}
	return scanner.Err()
}

func startRemoteSSH(targetPath, port string, blockSize uint64, quiet bool) (*exec.Cmd, error) {
	// split sshTarget "user@host:/remote/path" -> "user@host" and "/remote/path"
	user, host, file := parseSSHTarget(targetPath)

	// If no host specified, run locally
	if host == "" {
		Log("run locally: %s\n", file)

		// Get the path to the current executable
		execPath, err := os.Executable()
		if err != nil {
			return nil, err
		}

		// Create command with absolute path to bsync
		args := []string{execPath, "-f", file, "-p", port, "-b", strconv.FormatUint(uint64(blockSize), 10)}
		if quiet {
			args = append(args, "-q")
		}
		cmd := exec.Command(args[0], args[1:]...)

		// Set up stdout pipe to capture READY signal
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			return nil, err
		}

		if err := waitForReady(stdout, true); err != nil {
			return nil, err
		}
		cmd.Stdout = os.Stdout

		go io.Copy(os.Stdout, stdout) // keeps reading and printing

		return cmd, nil
	}

	if user != "" {
		host = user + "@" + host
	}

	// run SSH command
	args := []string{"ssh", host, "bsync", "-f", file, "-p", port, "-b", strconv.FormatUint(uint64(blockSize), 10)}
	if quiet {
		args = append(args, "-q")
	}
	Log("spawning ssh with args: %s\n", args)
	cmd := exec.Command(args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // pass stderr through

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	if err := waitForReady(stdout, false); err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stdout

	go io.Copy(os.Stdout, stdout) // keeps reading and printing

	return cmd, nil
}
