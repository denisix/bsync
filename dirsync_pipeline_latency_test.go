package main

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// delayProxy relays TCP both ways, sleeping delay before forwarding each
// read chunk -- a crude propagation-delay model: lock-step protocols pay it
// per frame, pipelined bursts pay it once per burst.
func delayProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				pipe := func(dst, src net.Conn) {
					buf := make([]byte, 256*1024)
					for {
						n, err := src.Read(buf)
						if n > 0 {
							time.Sleep(delay)
							if _, werr := dst.Write(buf[:n]); werr != nil {
								break
							}
						}
						if err != nil {
							break
						}
					}
					dst.Close()
				}
				go pipe(up, c)
				pipe(c, up)
			}(c)
		}
	}()
	return l.Addr().String()
}

func TestPipelinedBeatsStrictUnderLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison; skipped under -short")
	}
	build := func(seed int64) string {
		src := t.TempDir()
		for i := 0; i < 60; i++ {
			mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%03d.bin", i)), string(randomBytes(1500, seed+int64(i))))
		}
		return src
	}
	run := func(disabled bool) time.Duration {
		restore := dirPipelineDisabled
		defer func() { dirPipelineDisabled = restore }()
		dirPipelineDisabled = disabled

		src := build(500)
		resetEngineGlobals()
		port := freeTCPPort(t)
		realAddr := "127.0.0.1:" + port
		dst := t.TempDir()
		srvErrCh := make(chan error, 1)
		go func() { srvErrCh <- startDirServer(dst, "127.0.0.1", port, 2, true) }()
		waitForListener(t, realAddr)
		proxy := delayProxy(t, realAddr, 2*time.Millisecond)

		start := time.Now()
		sess, err := startDirClient(src, proxy, 0, true, 2, nil, false, false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		el := time.Since(start)
		if err := <-srvErrCh; err != nil {
			t.Fatal(err)
		}
		if sess.FailedCount() != 0 {
			t.Fatalf("%d failed", sess.FailedCount())
		}
		assertTreesEqual(t, src, dst)
		return el
	}
	strict := run(true)
	pipe := run(false)
	speedup := float64(strict) / float64(pipe)
	t.Logf("4ms-RTT, 60 small changed files, 2 workers: strict=%v pipelined=%v speedup=%.1fx", strict, pipe, speedup)
	// Measured ~5x on a quiet machine; 2x is the loose floor that still
	// catches "the pipeline silently degraded to lock-step" regressions
	// without flaking on busy CI.
	if speedup < 2.0 {
		t.Errorf("pipelined transfer only %.1fx faster than strict under 4ms RTT, want >= 2x", speedup)
	}
}
