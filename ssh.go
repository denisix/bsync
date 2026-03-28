package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func isPortNumber(s string) bool {
	if s == "" {
		return false
	}
	// Check all characters are digits
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	// Parse and validate range (1-65535)
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	return true
}

func parseSSHTarget(input string) (user, host, port, file string) {
	// If input doesn't contain ':', it's a local file
	if !strings.Contains(input, ":") {
		return "", "", "", input
	}

	// Split the input by '@' to handle the optional user component
	parts := strings.SplitN(input, "@", 2)
	if len(parts) == 2 {
		user = parts[0]
		input = parts[1]
	}

	// Split by ':' - could be host:port:/path or host:/path or host:port:path (for Windows)
	parts = strings.SplitN(input, ":", 3)
	if len(parts) < 2 {
		Err("invalid format: %s\n", input)
		return
	}

	host = parts[0]

	// Check if middle part is a numeric port (format: host:port:/path)
	if len(parts) == 3 && isPortNumber(parts[1]) {
		port = parts[1]
		file = parts[2]
	} else {
		// No port: host:/path
		file = parts[1]
	}

	return user, host, port, file
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

func copyBinaryToRemote(sshTarget, sshPort string) (string, error) {
	// Get local binary path
	localPath, err := os.Executable()
	if err != nil {
		return "", err
	}

	// Generate unique remote path
	remotePath := fmt.Sprintf("/tmp/bsync-%d", os.Getpid())

	// Read local binary
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}

	// Build remote command: decode base64, write to file, make executable
	remoteCmd := fmt.Sprintf("base64 -d > %s && chmod +x %s", remotePath, remotePath)

	// Build SSH args
	sshArgs := []string{"ssh"}
	if sshPort != "" {
		sshArgs = append(sshArgs, "-p", sshPort)
	}
	sshArgs = append(sshArgs, sshTarget, remoteCmd)

	Log("copying binary to remote via SSH: %s\n", remotePath)

	// Run SSH with stdin receiving base64-encoded binary
	cmd := exec.Command(sshArgs[0], sshArgs[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", err
	}

	// Write base64-encoded binary to SSH stdin
	encoder := base64.NewEncoder(base64.StdEncoding, stdin)
	encoder.Write(data)
	encoder.Close()
	stdin.Close()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("binary transfer failed: %v", err)
	}

	return remotePath, nil
}


func startRemoteSSH(targetPath, port string, blockSize, skipIdx uint32, quiet bool, noCompress bool) (*exec.Cmd, error) {
	// split sshTarget "user@host:/remote/path" -> "user@host" and "/remote/path"
	user, host, sshPort, file := parseSSHTarget(targetPath)

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
		if noCompress {
			args = append(args, "-n")
		}
		// Pass encryption key if enabled
		if IsEncryptionEnabled() {
			args = append(args, "-K", GetEncryptionKeyHex())
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


	if (user != "") {
		host = user + "@" + host
	}

	// Copy binary to remote server
	remoteBinPath, err := copyBinaryToRemote(host, sshPort)
	if err != nil {
		return nil, err
	}

	// run SSH command
	args := []string{"ssh"}

	// Add custom SSH port if specified
	if sshPort != "" {
		args = append(args, "-p", sshPort)
	}

	args = append(args, host,
		remoteBinPath, "-f", file,
		"-p", port,
		"-b", strconv.FormatUint(uint64(blockSize), 10),
		"-s", strconv.FormatUint(uint64(skipIdx), 10),
	)

	if quiet {
		args = append(args, "-q")
	}

	if noCompress {
		args = append(args, "-n")
	}

	// Pass encryption key if enabled
	if IsEncryptionEnabled() {
		args = append(args, "-K", GetEncryptionKeyHex())
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

	if err := waitForReady(stdout, false); err != nil {
		return nil, err
	}

	cmd.Stdout = os.Stdout

	go io.Copy(os.Stdout, stdout) // keeps reading and printing

	return cmd, nil
}

func startRemoteSSHDownload(targetPath, port string, blockSize, skipIdx uint32, quiet bool, noCompress bool) (*exec.Cmd, error) {
	// split sshTarget "user@host:/remote/path" -> "user@host" and "/remote/path"
	user, host, sshPort, file := parseSSHTarget(targetPath)

	// If no host specified, run locally
	if host == "" {
		Log("run locally for download: %s\n", file)

		// Get the path to the current executable
		execPath, err := os.Executable()
		if err != nil {
			return nil, err
		}

		// Create command with absolute path to bsync with -d flag for download mode
		args := []string{execPath, "-f", file, "-p", port, "-b", strconv.FormatUint(uint64(blockSize), 10), "-d"}
		if quiet {
			args = append(args, "-q")
		}
		if noCompress {
			args = append(args, "-n")
		}
		// Pass encryption key if enabled
		if IsEncryptionEnabled() {
			args = append(args, "-K", GetEncryptionKeyHex())
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

	if (user != "") {
		host = user + "@" + host
	}

	// Copy binary to remote server
	remoteBinPath, err := copyBinaryToRemote(host, sshPort)
	if err != nil {
		return nil, err
	}

	// run SSH command with -d flag for download mode
	args := []string{"ssh"}

	// Add custom SSH port if specified
	if sshPort != "" {
		args = append(args, "-p", sshPort)
	}

	args = append(args, host,
		remoteBinPath, "-f", file,
		"-p", port,
		"-b", strconv.FormatUint(uint64(blockSize), 10),
		"-s", strconv.FormatUint(uint64(skipIdx), 10),
		"-d",
	)

	if quiet {
		args = append(args, "-q")
	}

	if noCompress {
		args = append(args, "-n")
	}

	// Pass encryption key if enabled
	if IsEncryptionEnabled() {
		args = append(args, "-K", GetEncryptionKeyHex())
	}

	Log("spawning ssh for download with args: %s\n", args)
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
