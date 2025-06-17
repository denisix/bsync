package main

import (
	"os"
	"os/exec"
	"strings"
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

func startRemoteSSH(targetPath, port string) (*exec.Cmd, error) {
	// split sshTarget "user@host:/remote/path" -> "user@host" and "/remote/path"
	user, host, file := parseSSHTarget(targetPath)

	if (user != "") {
		host = user + "@" + host
	}

	// run SSH command
	args := []string{"ssh", host, "bsync", "-f", file, "-p", port}
	Log("spawning ssh with args: %s\n", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}
