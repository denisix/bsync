package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"time"
)

// ioTimeout is a var, not a const, purely so tests can shrink it instead of
// waiting on the real 5-minute value (matching the same var-not-const
// treatment magicHead got in Phase 0 for the same reason).
var ioTimeout = 5 * time.Minute // generous for large blocks on slow links

const (
	dialTimeout     = 30 * time.Second
	keepAlivePeriod = 30 * time.Second
	maxRetries      = 3

	// Shutdown timing. After the client sends DONE the server should exit
	// quickly when no connections remain; the longer linger period is only a
	// cap used while stragglers are still active. These dominate per-transfer
	// teardown time, so keep the idle exit short.
	shutdownGracePeriod  = 250 * time.Millisecond // exit this fast after DONE when idle
	shutdownLingerPeriod = 10 * time.Second       // cap while connections still active
	doneFlushDelay       = 200 * time.Millisecond // client flush margin after DONE
	uploadIdlePeriod     = 1 * time.Second        // upload server exits this long after last conn
)

// TCP buffer size, can be overridden by auto-tuning
var tcpBufferSize int = 4 * 1024 * 1024 // Default 4MB

// errPermanent marks a failure that retrying can never fix: a
// deterministic peer-state rejection (e.g. the server refusing OpFileOpen
// for a file it has already finalized or failed at announce time), as
// opposed to a transient network fault. An attempt that wraps its error
// with %w-errPermanent makes withRetry return it immediately instead of
// burning maxRetries attempts plus backoff on an outcome that cannot
// change -- which matters enormously when MANY entries are doomed at once
// (a full destination disk failing thousands of files would otherwise cost
// ~3s of retry churn EACH).
var errPermanent = errors.New("permanent failure")

// withRetry calls attempt up to maxRetries times total, with a linear
// backoff (retry*1s) between attempts, and returns the last error (nil once
// attempt succeeds; immediately if it wraps errPermanent — see above).
// Before every attempt after the first, it calls onRetry (if non-nil) with
// the retry number and the previous attempt's error, so a caller can reset
// its own per-attempt state (closing/redialing a connection, clearing a
// "currently bound to X" flag) before trying again — exactly the shape both
// the legacy single-file client's block-transfer retry loop and the
// directory-tree transfer worker's retry loop needed independently,
// differing only in what that per-attempt state actually is.
func withRetry(attempt func(retry int) error, onRetry func(retry int, lastErr error)) error {
	var lastErr error
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			if onRetry != nil {
				onRetry(retry, lastErr)
			}
			time.Sleep(time.Duration(retry) * time.Second)
		}
		if lastErr = attempt(retry); lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, errPermanent) {
			return lastErr
		}
	}
	return lastErr
}

// SetTCPBufferSize sets the TCP buffer size for new connections.
func SetTCPBufferSize(size int) {
	tcpBufferSize = size
}

// connWrite writes all bytes to conn, retrying on partial writes.
// Sets a per-iteration write deadline to detect stalled peers.
func connWrite(conn net.Conn, data []byte) error {
	var start, c int
	var err error
	for {
		conn.SetWriteDeadline(time.Now().Add(ioTimeout))
		if c, err = conn.Write(data[start:]); err != nil {
			if start > 0 {
				// Earlier bytes of this same frame were already written by a
				// prior iteration of this loop, on the same conn, before
				// this call failed. If conn is an AutoReconnectTCP, a write
				// error silently redials it, so those earlier bytes are now
				// stranded on a connection that no longer exists — the
				// frame cannot be completed by resuming; the caller must
				// resend it whole, from the start, on the fresh connection.
				return fmt.Errorf("write: frame partially sent (%d/%d bytes) before error, cannot resume: %w", start, len(data), err)
			}
			return err
		}
		start += c
		if start == len(data) {
			break
		}
		if c == 0 {
			return fmt.Errorf("write stalled: 0 bytes written without error")
		}
	}
	return nil
}

type AutoReconnectTCP struct {
	addr *net.TCPAddr
	conn *net.TCPConn
	br   *bufio.Reader // wraps conn; replaced on every (re)dial, see Read

	// frameBroken records that the most recent Write wrote some bytes
	// (n > 0) and then failed, so a redial happened mid-frame. It is purely
	// diagnostic today — every current caller already resends a frame whole
	// from the start on retry rather than trying to resume it, which is safe
	// by construction — but it documents the invariant explicitly for any
	// future caller (e.g. request pipelining) that might be tempted to trust
	// a partial byte count across a reconnect.
	frameBroken bool

	// gen counts successful dials, starting at 0 (never connected). It is
	// how a directory-mode transfer worker notices "this connection was
	// silently redialed under me" and knows to re-send OpFileOpen before
	// its next block — the server resolves every block by FileID regardless
	// of which physical connection sends it (see server_dir.go), so a
	// missed rebind never corrupts data, but it does cost the server an
	// extra file acquire it wouldn't otherwise need, and the client uses a
	// generation mismatch to avoid that where it can.
	gen uint64
}

// FrameBroken reports whether the last Write left a partial frame stranded
// on a connection that has since been closed and redialed.
func (a *AutoReconnectTCP) FrameBroken() bool {
	return a.frameBroken
}

// ClearFrameBroken acknowledges a stranded frame once the caller has
// discarded it and is about to resend it whole on the fresh connection.
func (a *AutoReconnectTCP) ClearFrameBroken() {
	a.frameBroken = false
}

func NewAutoReconnectTCP(addr *net.TCPAddr) *AutoReconnectTCP {
	return &AutoReconnectTCP{addr: addr}
}

func (a *AutoReconnectTCP) connect() error {
	if a.conn != nil {
		return nil
	}
	Log("connecting to %s ..\n", a.addr)

	c, err := net.DialTimeout("tcp", a.addr.String(), dialTimeout)
	if err != nil {
		return err
	}
	a.conn = c.(*net.TCPConn)
	a.br = bufio.NewReaderSize(a.conn, connReadBufSize)
	a.conn.SetKeepAlive(true)
	a.conn.SetKeepAlivePeriod(keepAlivePeriod)
	// Increase TCP buffers for better throughput on fast networks
	a.conn.SetReadBuffer(tcpBufferSize)
	a.conn.SetWriteBuffer(tcpBufferSize)
	// Disable Nagle's algorithm for immediate sending of small packets
	a.conn.SetNoDelay(true)
	a.gen++
	return nil
}

// Dial establishes the connection if it isn't already up, exactly like the
// implicit dial inside Read/Write, but lets a caller do so explicitly and
// then read Gen() before writing anything — needed to notice a redial that
// happens mid-handshake rather than only after the fact.
func (a *AutoReconnectTCP) Dial() error {
	return a.connect()
}

// Gen returns the number of successful dials so far (0 before the first).
// It increases by exactly 1 on every (re)connect, including one triggered
// transparently inside Read/Write, so comparing it before and after an
// operation reveals a silent redial.
func (a *AutoReconnectTCP) Gen() uint64 {
	return a.gen
}

// Connected reports whether the connection is currently up. False right
// after construction, and after any error until the next successful redial.
func (a *AutoReconnectTCP) Connected() bool {
	return a.conn != nil
}

// handleErr closes and nils the connection on any error so the next
// Read/Write call triggers a reconnect.
func (a *AutoReconnectTCP) handleErr(err error) {
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
		a.br = nil
	}
}

// connReadBufSize is the bufio.Reader size for a client connection. Replies
// are tiny (header + digest + ack ≈ 112 bytes per block), so a small buffer
// is enough; large payloads on the download path are read with io.ReadFull
// straight through it and never copied twice for being bigger than this.
const connReadBufSize = 4096

// connWriteFrame sends one protocol frame -- a header followed by its
// payload -- coalescing the two into a single write when the payload is
// small. Header and payload were always two separate conn.Write calls, i.e.
// two syscalls and, on the wire, often two segments; for the 16-byte digest
// reply and for small-file blocks in directory mode that doubled the
// per-frame cost for nothing. Above frameCoalesceMax the copy would cost
// more than the syscall it saves, so large payloads keep the two writes.
func connWriteFrame(conn net.Conn, hdr, payload []byte) error {
	if len(payload) == 0 {
		return connWrite(conn, hdr)
	}
	if len(payload) <= frameCoalesceMax {
		buf := make([]byte, 0, len(hdr)+len(payload))
		buf = append(buf, hdr...)
		buf = append(buf, payload...)
		return connWrite(conn, buf)
	}
	if err := connWrite(conn, hdr); err != nil {
		return err
	}
	return connWrite(conn, payload)
}

const frameCoalesceMax = 64 * 1024

// Read returns bytes from the connection through a per-connection
// bufio.Reader: the protocol's replies are several small frames in a row (a
// 48-byte header, a 16-byte digest, a 48-byte ack), and reading them through
// bare conn.Read cost one syscall each. The buffer belongs to exactly one
// physical connection and is dropped with it on any error (handleErr), so a
// redial can never replay bytes from a dead connection -- and since every
// exchange on a connection is strictly request/response, the buffer never
// holds anything the caller is not about to read.
func (a *AutoReconnectTCP) Read(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}
	a.conn.SetReadDeadline(time.Now().Add(ioTimeout))
	n, err := a.br.Read(b)
	if err != nil {
		a.handleErr(err)
		return 0, err
	}
	return n, nil
}

// AwaitClose waits up to d for the peer to close the connection (a read
// that returns EOF), then closes this side. It is how the client lets the
// server consume its final frame before tearing down, without a fixed
// sleep: the server closes as soon as it has processed DONE, so this
// normally returns almost immediately. Any other outcome -- a timeout, an
// unexpected byte, an error -- is treated the same way; nothing depends on
// it beyond bounding the wait.
func (a *AutoReconnectTCP) AwaitClose(d time.Duration) {
	if a.conn == nil {
		return
	}
	a.conn.SetReadDeadline(time.Now().Add(d))
	var scratch [1]byte
	a.br.Read(scratch[:])
	a.handleErr(nil)
}

func (a *AutoReconnectTCP) Write(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}
	// Deadline is managed by connWrite (the canonical write path); don't double-set here.
	n, err := a.conn.Write(b)
	if err != nil {
		if n > 0 {
			a.frameBroken = true
		}
		a.handleErr(err)
		// Return the true byte count, not 0: io.Writer callers are entitled
		// to trust n on a partial write, and lying about it (this used to
		// always return 0 here) breaks that contract even though no current
		// caller in this codebase happens to rely on it.
		return n, err
	}
	return n, nil
}

func (a *AutoReconnectTCP) Close() error {
	if a.conn != nil {
		err := a.conn.Close()
		a.conn = nil
		return err
	}
	return nil
}

func (a *AutoReconnectTCP) LocalAddr() net.Addr {
	if a.conn != nil {
		return a.conn.LocalAddr()
	}
	return nil
}

func (a *AutoReconnectTCP) RemoteAddr() net.Addr {
	if a.conn != nil {
		return a.conn.RemoteAddr()
	}
	return nil
}

func (a *AutoReconnectTCP) SetDeadline(t time.Time) error {
	if a.conn != nil {
		return a.conn.SetDeadline(t)
	}
	return nil
}

func (a *AutoReconnectTCP) SetReadDeadline(t time.Time) error {
	if a.conn != nil {
		return a.conn.SetReadDeadline(t)
	}
	return nil
}

func (a *AutoReconnectTCP) SetWriteDeadline(t time.Time) error {
	if a.conn != nil {
		return a.conn.SetWriteDeadline(t)
	}
	return nil
}

// Ensure AutoReconnectTCP satisfies net.Conn at compile time
var _ net.Conn = (*AutoReconnectTCP)(nil)
