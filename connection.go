package main

import (
  "net"
)

func connWrite(conn net.Conn, data []byte) error {
    var start,c int
    var err error
    for {
      if c, err = conn.Write(data[start:]); err != nil {
          return err
      }
      start += c
      if c == 0 || start == len(data) {
          break
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

func (a *AutoReconnectTCP) Write(b []byte) (int, error) {
	if err := a.connect(); err != nil {
		return 0, err
	}

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

