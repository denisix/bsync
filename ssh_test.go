package main

import (
	"io"
	"strings"
	"testing"
)

func TestWaitForReady(t *testing.T) {
	// A stream that eventually emits a READY line should return nil.
	r := io.NopCloser(strings.NewReader("starting\nbinding socket\nREADY, listening on :8080\nmore\n"))
	if err := waitForReady(r, true); err != nil {
		t.Errorf("waitForReady returned error: %v", err)
	}
}

func TestWaitForReadyNoReady(t *testing.T) {
	r := io.NopCloser(strings.NewReader("line1\nline2\n"))
	if err := waitForReady(r, false); err == nil {
		t.Error("server exit before READY must fail")
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote("/tmp/name with 'quote'.img")
	want := "'/tmp/name with '\"'\"'quote'\"'\"'.img'"
	if got != want {
		t.Fatalf("shellQuote() = %q, want %q", got, want)
	}
}

func TestRemoteBinaryCommandQuotesAndCleans(t *testing.T) {
	cmd := remoteBinaryCommand("/tmp/bsync-1", []string{"-f", "/tmp/dst file; touch /tmp/bad", "-d"})
	for _, part := range []string{"trap", "rm -f --", "'/tmp/bsync-1'", "'/tmp/dst file; touch /tmp/bad'", "'-d'"} {
		if !strings.Contains(cmd, part) {
			t.Fatalf("remote command %q missing %q", cmd, part)
		}
	}
}

func TestParseSSHTargetExtra(t *testing.T) {
	tests := []struct {
		name                       string
		input                      string
		wUser, wHost, wPort, wFile string
	}{
		{"host only path", "myhost:/data/disk.img", "", "myhost", "", "/data/disk.img"},
		{"user host port path", "admin@10.0.0.5:2200:/srv/x", "admin", "10.0.0.5", "2200", "/srv/x"},
		{"plain local path", "/var/lib/disk.img", "", "", "", "/var/lib/disk.img"},
		{"relative local", "disk.img", "", "", "", "disk.img"},
		{"windows style path", "user@host:2222:C:/data", "user", "host", "2222", "C:/data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, h, p, f := parseSSHTarget(tt.input)
			if u != tt.wUser || h != tt.wHost || p != tt.wPort || f != tt.wFile {
				t.Errorf("parseSSHTarget(%q) = (%q,%q,%q,%q), want (%q,%q,%q,%q)",
					tt.input, u, h, p, f, tt.wUser, tt.wHost, tt.wPort, tt.wFile)
			}
		})
	}
}
