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
	"time"
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
	ready := false
	for scanner.Scan() {
		line := scanner.Text()
		Log("server-> %s\n", line)
		if strings.Contains(line, "READY") {
			ready = true
			serverType := "remote"
			if isLocal {
				serverType = "local"
			}
			Log("%s server is ready\n", serverType)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("server exited before signalling readiness")
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func remoteBinaryCommand(remotePath string, args []string) string {
	quotedArgs := make([]string, 0, len(args)+1)
	quotedArgs = append(quotedArgs, shellQuote(remotePath))
	for _, arg := range args {
		quotedArgs = append(quotedArgs, shellQuote(arg))
	}
	cleanup := "rm -f -- " + shellQuote(remotePath)
	return "trap " + shellQuote(cleanup) + " EXIT; " + strings.Join(quotedArgs, " ")
}

func removeRemoteBinary(sshTarget, sshPort, remotePath string) {
	args := []string{}
	if sshPort != "" {
		args = append(args, "-p", sshPort)
	}
	args = append(args, sshTarget, "rm -f -- "+shellQuote(remotePath))
	_ = exec.Command("ssh", args...).Run()
}

func copyBinaryToRemote(sshTarget, sshPort string) (string, error) {
	// Get local binary path
	localPath, err := os.Executable()
	if err != nil {
		return "", err
	}

	// Generate unique remote path
	remotePath := fmt.Sprintf("/tmp/bsync-%d-%d", os.Getpid(), time.Now().UnixNano())

	// Read local binary
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", err
	}

	// Build remote command: decode base64, write to file, make executable
	remoteCmd := fmt.Sprintf("base64 -d > %s && chmod +x %s", shellQuote(remotePath), shellQuote(remotePath))

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
	if _, err := encoder.Write(data); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return "", err
	}
	if err := encoder.Close(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return "", err
	}
	if err := stdin.Close(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return "", err
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("binary transfer failed: %v", err)
	}

	return remotePath, nil
}

// startRemoteSSH launches the destination side of an upload transfer,
// either locally (no host in targetPath) or over ssh. dirMode forwards -R
// so the spawned process runs startDirServer even against a destination
// path that doesn't exist yet (directory auto-detection via os.Stat can't
// help there — the whole point of a fresh destination). workers forwards
// -w; unlike blockSize/skipIdx/quiet/noCompress, this was never forwarded
// at all before directory mode needed it to size startDirServer's fd
// budget and worker-facing connection handling correctly, not just the
// legacy path's OrderedWriter reorder window.
func startRemoteSSH(targetPath, port string, blockSize, skipIdx uint32, quiet bool, noCompress bool, dirMode bool, workers int) (*exec.Cmd, error) {
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
		args := []string{execPath, "-f", file, "-p", port, "-b", strconv.FormatUint(uint64(blockSize), 10), "-s", strconv.FormatUint(uint64(skipIdx), 10), "-w", strconv.Itoa(workers)}
		if quiet {
			args = append(args, "-q")
		}
		if noCompress {
			args = append(args, "-n")
		}
		if dirMode {
			args = append(args, "-R")
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

	if user != "" {
		host = user + "@" + host
	}

	// Copy binary to remote server
	remoteBinPath, err := copyBinaryToRemote(host, sshPort)
	if err != nil {
		return nil, err
	}

	// run SSH command. Keep all remote arguments in one shell-quoted command:
	// ssh servers execute their command through a shell.
	args := []string{"ssh"}

	// Add custom SSH port if specified
	if sshPort != "" {
		args = append(args, "-p", sshPort)
	}

	remoteArgs := []string{"-f", file,
		"-p", port,
		"-b", strconv.FormatUint(uint64(blockSize), 10),
		"-s", strconv.FormatUint(uint64(skipIdx), 10),
		"-w", strconv.Itoa(workers),
		"-P", // suppress server-side progress (client shows its own)
	}

	if quiet {
		remoteArgs = append(remoteArgs, "-q")
	}

	if noCompress {
		remoteArgs = append(remoteArgs, "-n")
	}

	if dirMode {
		remoteArgs = append(remoteArgs, "-R")
	}

	// Pass encryption key if enabled
	if IsEncryptionEnabled() {
		remoteArgs = append(remoteArgs, "-K", GetEncryptionKeyHex())
	}
	args = append(args, host, remoteBinaryCommand(remoteBinPath, remoteArgs))

	Log("spawning SSH server on %s\n", host)
	cmd := exec.Command(args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}
	// cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr // pass stderr through

	if err := cmd.Start(); err != nil {
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}

	if err := waitForReady(stdout, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}

	cmd.Stdout = os.Stdout

	go io.Copy(os.Stdout, stdout) // keeps reading and printing

	return cmd, nil
}

func startRemoteSSHDownload(targetPath, port string, blockSize, skipIdx uint32, quiet bool, noCompress bool, compLevel string) (*exec.Cmd, error) {
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
		args := []string{execPath, "-f", file, "-p", port, "-b", strconv.FormatUint(uint64(blockSize), 10), "-s", strconv.FormatUint(uint64(skipIdx), 10), "-d", "-L", compLevel}
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

	if user != "" {
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

	remoteArgs := []string{"-f", file,
		"-p", port,
		"-b", strconv.FormatUint(uint64(blockSize), 10),
		"-s", strconv.FormatUint(uint64(skipIdx), 10),
		"-d",
		"-L", compLevel,
		"-P", // suppress server-side progress (client shows its own)
	}

	if quiet {
		remoteArgs = append(remoteArgs, "-q")
	}

	if noCompress {
		remoteArgs = append(remoteArgs, "-n")
	}

	// Pass encryption key if enabled
	if IsEncryptionEnabled() {
		remoteArgs = append(remoteArgs, "-K", GetEncryptionKeyHex())
	}
	args = append(args, host, remoteBinaryCommand(remoteBinPath, remoteArgs))

	Log("spawning SSH download server on %s\n", host)
	cmd := exec.Command(args[0], args[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}
	cmd.Stderr = os.Stderr // pass stderr through

	if err := cmd.Start(); err != nil {
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}

	if err := waitForReady(stdout, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		removeRemoteBinary(host, sshPort, remoteBinPath)
		return nil, err
	}

	cmd.Stdout = os.Stdout

	go io.Copy(os.Stdout, stdout) // keeps reading and printing

	return cmd, nil
}
