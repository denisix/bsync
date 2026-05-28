package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var logPrefix = "[main]"
var quiet = false

var (
	logStateMu   sync.RWMutex
	logBuffer    []string
	logBufferMu  sync.Mutex
	logBufferMax = 200

	visualTransferStarted atomic.Bool
)

func SetLog(pre1, pre2 string, q bool) {
	logStateMu.Lock()
	defer logStateMu.Unlock()
	quiet = q
	if pre1 != "" {
		logPrefix = pre1 + " " + pre2
	} else {
		logPrefix = pre2
	}
}

func getLogState() (string, bool) {
	logStateMu.RLock()
	defer logStateMu.RUnlock()
	return logPrefix, quiet
}

func setQuiet(q bool) {
	logStateMu.Lock()
	quiet = q
	logStateMu.Unlock()
}

func Log(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	prefix, isQuiet := getLogState()
	formatted := fmt.Sprintf("%s %s %s", ts, prefix, msg)

	if visualEnabled() {
		if !visualTransferStarted.Load() || isImportantVisualLog(formatted) {
			logAppend(formatted)
		}
		return
	}

	if isQuiet && !strings.Contains(format, "READY") {
		return
	}

	fmt.Print(formatted)
}

func Err(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	prefix, _ := getLogState()
	formatted := fmt.Sprintf("%s %s ERROR: %s", ts, prefix, msg)
	if visualEnabled() {
		logAppend(formatted)
		fmt.Fprint(os.Stderr, formatted)
	} else {
		fmt.Print(formatted)
	}
	os.Exit(1)
}

func MarkVisualTransferStarted() {
	if !visualEnabled() || !visualTransferStarted.CompareAndSwap(false, true) {
		return
	}

	logBufferMu.Lock()
	defer logBufferMu.Unlock()

	kept := logBuffer[:0]
	for _, msg := range logBuffer {
		if isImportantVisualLog(msg) {
			kept = append(kept, msg)
		}
	}
	logBuffer = kept
}

func isImportantVisualLog(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "error") ||
		strings.Contains(lower, "warning") ||
		strings.Contains(lower, "warn") ||
		strings.Contains(lower, "failed")
}

func logAppend(msg string) {
	logBufferMu.Lock()
	defer logBufferMu.Unlock()
	logBuffer = append(logBuffer, msg)
	if len(logBuffer) > logBufferMax {
		logBuffer = logBuffer[len(logBuffer)-logBufferMax:]
	}
}

// GetRecentLogs returns up to n most recent log messages.
func GetRecentLogs(n int) []string {
	logBufferMu.Lock()
	defer logBufferMu.Unlock()
	if len(logBuffer) <= n {
		result := make([]string, len(logBuffer))
		copy(result, logBuffer)
		return result
	}
	result := make([]string, n)
	copy(result, logBuffer[len(logBuffer)-n:])
	return result
}
