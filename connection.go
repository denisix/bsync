package main

import (
	"io"
	"net"
	"time"
)

// Robust read with infinite retries
func connReadFullWithRetry(conn net.Conn, buf []byte) (int, error) {
	var n int
	var err error
	for {
		n, err = io.ReadFull(conn, buf)
		if err == nil {
			return n, nil
		}
		Log("Network read error: %v, retrying in 1s...\n", err)
		// If connection is AutoReconnectTCP, try to reconnect on EOF or permanent error
		if err == io.EOF || (err != nil && !isTemporaryNetErrConn(err)) {
			if artcp, ok := conn.(*AutoReconnectTCP); ok {
				Log("Triggering reconnect after read error...\n")
				artcp.Close()
				_ = artcp.connect()
			}
		}
		time.Sleep(time.Second)
	}
}

// Robust write with infinite retries
func connWriteWithRetry(conn net.Conn, data []byte) error {
	var err error
	for {
		_, err = conn.Write(data)
		if err == nil {
			return nil
		}
		Log("Network write error: %v, retrying in 1s...\n", err)
		// If connection is AutoReconnectTCP, try to reconnect on EOF or permanent error
		if err == io.EOF || (err != nil && !isTemporaryNetErrConn(err)) {
			if artcp, ok := conn.(*AutoReconnectTCP); ok {
				Log("Triggering reconnect after write error...\n")
				artcp.Close()
				_ = artcp.connect()
			}
		}
		time.Sleep(time.Second)
	}
}

// Helper to check for temporary network errors (local to connection.go)
func isTemporaryNetErrConn(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Temporary()
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

	var err error
	a.conn, err = net.DialTCP("tcp", nil, a.addr)
	return err
}

func (a *AutoReconnectTCP) handleErr(err error) {
	if netErr, ok := err.(net.Error); ok && (netErr.Timeout() || netErr.Temporary()) {
		if a.conn != nil {
			a.conn.Close()
		}
		a.conn = nil
	}
}

func (a *AutoReconnectTCP) Read(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}

	n, err := a.conn.Read(b)
	if err != nil {
		a.handleErr(err)
		return 0, err
	}
	return n, nil
}

// Write all bytes, with reconnect and error handling
func (a *AutoReconnectTCP) Write(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}
	total := 0
	for total < len(b) {
		n, err := a.conn.Write(b[total:])
		if err != nil {
			a.handleErr(err)
			return total, err
		}
		total += n
	}
	return total, nil
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
	return a.addr
}

func (a *AutoReconnectTCP) SetDeadline(t time.Time) error {
	if err := a.connect(); err != nil {
		return err
	}
	return a.conn.SetDeadline(t)
}

func (a *AutoReconnectTCP) SetReadDeadline(t time.Time) error {
	if err := a.connect(); err != nil {
		return err
	}
	return a.conn.SetReadDeadline(t)
}

func (a *AutoReconnectTCP) SetWriteDeadline(t time.Time) error {
	if err := a.connect(); err != nil {
		return err
	}
	return a.conn.SetWriteDeadline(t)
}
