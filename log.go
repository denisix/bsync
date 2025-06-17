package main

import (
	"fmt"
	"time"
)

var logPrefix = "[main]"

func SetLogPrefix(p string) {
	logPrefix = p
}

func Log(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s %s", ts, logPrefix, msg)
}
