package main

import (
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// freeTCPPort asks the OS for an unused port on loopback and returns it as a
// string. There is a tiny race between closing the probe listener and the
// server re-binding, but it is acceptable for tests.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("freeTCPPort split: %v", err)
	}
	return port
}

// resetEngineGlobals resets the package-level state that the transfer engine
// mutates, so sequential in-process transfers in a single test run stay
// independent. It also silences logging.
func resetEngineGlobals() {
	atomic.StoreInt32(&doneReceived, 0)
	atomic.StoreInt64(&activeConns, 0)
	atomic.StoreUint64(&srvLastBlockNum, 0)
	atomic.StoreUint64(&srvBytesNet, 0)
	atomic.StoreUint64(&srvBytesOrig, 0)
	atomic.StoreInt64(&srvDiffs, 0)
	srvOnce = sync.Once{}
	shutdownTimer = nil
	shutdownResetChan = nil
	setSuppressProgress(true)
	setQuiet(true)
	setVisual(false)
	visualTransferStarted.Store(false)
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()

	// client-side stats globals
	totCompSize = 0
	totOrigSize = 0
	diffs = 0

	// crypto state
	encryptionKey = nil
	aeadOnce = sync.Once{}
}

// waitForListener dials addr until the connection succeeds or the deadline
// passes, so a test never races the server's listener startup.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener %s never came up", addr)
}

// writeTempFile creates a temp file seeded with data and returns its path.
func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "bsync-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			t.Fatalf("write temp: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return f.Name()
}

// --- deterministic content generators ---

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func zeroBytes(n int) []byte { return make([]byte, n) }

func ffBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// mixedBytes returns random data with zero "holes" punched at block boundaries.
func mixedBytes(n int, block int, seed int64) []byte {
	b := randomBytes(n, seed)
	for off := 0; off+block <= n; off += block * 2 { // zero every other block
		for i := off; i < off+block; i++ {
			b[i] = 0
		}
	}
	return b
}

// almostZeroBytes is all zeros except a single random block in the middle.
func almostZeroBytes(n int, block int, seed int64) []byte {
	b := zeroBytes(n)
	mid := (n / 2 / block) * block
	end := mid + block
	if end > n {
		end = n
	}
	copy(b[mid:end], randomBytes(end-mid, seed))
	return b
}

// almostFullBytes is random data except a single zero block in the middle.
func almostFullBytes(n int, block int, seed int64) []byte {
	b := randomBytes(n, seed)
	mid := (n / 2 / block) * block
	end := mid + block
	if end > n {
		end = n
	}
	for i := mid; i < end; i++ {
		b[i] = 0
	}
	return b
}

// alternating produces alternating zero / random blocks.
func alternating(n int, block int, seed int64) []byte {
	b := randomBytes(n, seed)
	zero := false
	for off := 0; off < n; off += block {
		if zero {
			end := off + block
			if end > n {
				end = n
			}
			for i := off; i < end; i++ {
				b[i] = 0
			}
		}
		zero = !zero
	}
	return b
}

// singleByteInZero is an all-zero buffer with exactly one non-zero byte,
// stressing the zero-detection fast path's boundary handling.
func singleByteInZero(n int, pos int) []byte {
	b := zeroBytes(n)
	if n > 0 {
		if pos < 0 || pos >= n {
			pos = n - 1
		}
		b[pos] = 0xAB
	}
	return b
}

// allByteValues repeats the 0..255 sequence to length n.
func allByteValues(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

// compressibleBytes is a repeating pattern that compresses well.
func compressibleBytes(n int) []byte {
	pattern := []byte("the quick brown fox 0123456789 ")
	b := make([]byte, n)
	for i := range b {
		b[i] = pattern[i%len(pattern)]
	}
	return b
}
