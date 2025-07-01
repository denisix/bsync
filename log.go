package main

import (
	"fmt"
	"time"
)

var logPrefix = "[main]"

func SetLogPrefix(pre1, pre2 string) {
	if pre1 != "" {
		logPrefix = pre1 + " " + pre2
	} else {
		logPrefix = pre2
	}
}

func Log(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s %s", ts, logPrefix, msg)
}
