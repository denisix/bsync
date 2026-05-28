package main

import (
	"strings"
	"testing"
)

func resetVisualLogTestState(t *testing.T) {
	t.Helper()
	setVisual(true)
	visualTransferStarted.Store(false)
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
	SetLog("", "[test]", false)
	t.Cleanup(func() {
		setVisual(false)
		visualTransferStarted.Store(false)
		logBufferMu.Lock()
		logBuffer = nil
		logBufferMu.Unlock()
	})
}

func TestVisualLogsBufferNormalMessagesBeforeTransferStart(t *testing.T) {
	resetVisualLogTestState(t)

	Log("waiting for server\n")

	logs := GetRecentLogs(10)
	if len(logs) != 1 || !strings.Contains(logs[0], "waiting for server") {
		t.Fatalf("logs = %v, want startup log buffered", logs)
	}
}

func TestVisualLogsDropNormalMessagesAfterTransferStart(t *testing.T) {
	resetVisualLogTestState(t)
	Log("waiting for server\n")

	MarkVisualTransferStarted()
	Log("normal transfer chatter\n")

	if logs := GetRecentLogs(10); len(logs) != 0 {
		t.Fatalf("logs = %v, want normal visual logs hidden after transfer start", logs)
	}
}

func TestVisualLogsKeepImportantMessagesAfterTransferStart(t *testing.T) {
	resetVisualLogTestState(t)

	MarkVisualTransferStarted()
	Log("Warning: read-ahead tuning failed\n")
	Log("ERROR: retry failed\n")

	logs := GetRecentLogs(10)
	if len(logs) != 2 {
		t.Fatalf("logs = %v, want two important logs", logs)
	}
	if !strings.Contains(logs[0], "Warning") || !strings.Contains(logs[1], "ERROR") {
		t.Fatalf("logs = %v, want warning and error logs", logs)
	}
}

func TestVisualTransferStartKeepsExistingImportantMessages(t *testing.T) {
	resetVisualLogTestState(t)

	Log("waiting for server\n")
	Log("Warning: remote startup is slow\n")

	MarkVisualTransferStarted()

	logs := GetRecentLogs(10)
	if len(logs) != 1 || !strings.Contains(logs[0], "Warning") {
		t.Fatalf("logs = %v, want only existing warning retained", logs)
	}
}

func TestImportantVisualLogMatchingIsCaseInsensitive(t *testing.T) {
	for _, msg := range []string{
		"warning: slow disk",
		"WARNING: slow disk",
		"Warn: slow disk",
		"error: failed write",
		"FAILED after retries",
	} {
		if !isImportantVisualLog(msg) {
			t.Fatalf("%q was not classified as important", msg)
		}
	}
}
