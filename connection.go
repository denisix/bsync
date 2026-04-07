package main

import (
	"fmt"
	"net"
	"time"
)

const (
	dialTimeout     = 30 * time.Second
	ioTimeout       = 5 * time.Minute  // generous for large blocks on slow links
	keepAlivePeriod = 30 * time.Second
	maxRetries      = 3
)

// connWrite writes all bytes to conn, retrying on partial writes.
// Sets a per-iteration write deadline to detect stalled peers.
func connWrite(conn net.Conn, data []byte) error {
	var start, c int
	var err error
	for {
		conn.SetWriteDeadline(time.Now().Add(ioTimeout))
		if c, err = conn.Write(data[start:]); err != nil {
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
	a.conn.SetKeepAlive(true)
	a.conn.SetKeepAlivePeriod(keepAlivePeriod)
	return nil
}

// handleErr closes and nils the connection on any error so the next
// Read/Write call triggers a reconnect.
func (a *AutoReconnectTCP) handleErr(err error) {
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
}

func (a *AutoReconnectTCP) Read(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}
	a.conn.SetReadDeadline(time.Now().Add(ioTimeout))
	n, err := a.conn.Read(b)
	if err != nil {
		a.handleErr(err)
		return 0, err
	}
	return n, nil
}

func (a *AutoReconnectTCP) Write(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}
	// Deadline is managed by connWrite (the canonical write path); don't double-set here.
	n, err := a.conn.Write(b)
	if err != nil {
		a.handleErr(err)
		return 0, err
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
