package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

var logPrefix = "[main]"
var quiet = false

func SetLog(pre1, pre2 string, q bool) {
	quiet = q
	if pre1 != "" {
		logPrefix = pre1 + " " + pre2
	} else {
		logPrefix = pre2
	}
}

func Log(format string, args ...interface{}) {
	if quiet && !strings.Contains(format, "READY") {
		return
	}

	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s %s", ts, logPrefix, msg)
}

func Err(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s ERROR: %s", ts, logPrefix, msg)
	os.Exit(1)
}

