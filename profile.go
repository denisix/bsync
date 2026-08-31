package main

import (
	"os"
	"runtime/pprof"
)

// startCPUProfile starts a CPU profile written to path and returns the
// function that stops it and closes the file. An empty path, or any failure
// to create the file or start profiling, returns nil -- profiling is a
// developer aid and must never affect a transfer. Wired to the
// BSYNC_CPUPROFILE environment variable in main.
func startCPUProfile(path string) func() {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		Log("cpu profile: %v\n", err)
		return nil
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		Log("cpu profile: %v\n", err)
		f.Close()
		return nil
	}
	return func() {
		pprof.StopCPUProfile()
		f.Close()
	}
}
