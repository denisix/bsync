package main

import (
	"fmt"
	"strings"
	"time"
)

var logPrefix = "[main]"
var quiet = false

func SetLog(p string, q bool) {
	logPrefix = p
	quiet = q
}

func Log(format string, args ...interface{}) {
	if quiet && !strings.Contains(format, "READY") {
		return
	}

	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s %s", ts, logPrefix, msg)
}
