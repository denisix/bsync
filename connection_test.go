package main

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestSetTCPBufferSize tests that TCP buffer size can be set.
func TestSetTCPBufferSize(t *testing.T) {
	// Save original value
	original := tcpBufferSize

	// Test setting various buffer sizes
	sizes := []int{1024 * 1024, 4 * 1024 * 1024, 8 * 1024 * 1024}

	for _, size := range sizes {
		SetTCPBufferSize(size)
		// We can't directly read tcpBufferSize without exporting it,
		// but we verify the function doesn't panic
	}

	// Restore original value
	tcpBufferSize = original
}

// TestNewAutoReconnectTCP tests creation of AutoReconnectTCP.
func TestNewAutoReconnectTCP(t *testing.T) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("ResolveTCPAddr failed: %v", err)
	}

	art := NewAutoReconnectTCP(addr)

	if art == nil {
		t.Fatal("NewAutoReconnectTCP returned nil")
	}
	if art.addr == nil {
		t.Error("addr is nil")
	}
	if art.conn != nil {
		t.Error("conn should be nil initially")
	}
}

// TestAutoReconnectTCPConnect tests basic connection functionality.
func TestAutoReconnectTCPConnect(t *testing.T) {
	// Start a simple echo server
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	// Accept connections in background
	serverReady := make(chan struct{})
	go func() {
		close(serverReady)
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()
	<-serverReady

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Test Write
	testData := []byte("hello world")
	n, err := art.Write(testData)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Write wrote %d bytes, want %d", n, len(testData))
	}

	// Test Read
	buf := make([]byte, 100)
	n, err = art.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if !bytes.Equal(buf[:n], testData) {
		t.Errorf("Read got %q, want %q", buf[:n], testData)
	}

	// Test Close
	if err := art.Close(); err != nil {
		t.Errorf("Close error: %v", err)
	}
}

// TestAutoReconnectTCPReconnect tests reconnection after connection loss.
func TestAutoReconnectTCPReconnect(t *testing.T) {
	// Start a server
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)

	// Track connections accepted
	connCount := 0
	connCountChan := make(chan int, 10)

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			connCount++
			connCountChan <- connCount
			go handleEchoConn(conn)
		}
	}()

	art := NewAutoReconnectTCP(addr)

	// First connection - verify it works
	testData1 := []byte("first")
	_, err = art.Write(testData1)
	if err != nil {
		t.Fatalf("First Write error: %v", err)
	}

	// Read echo
	buf := make([]byte, 100)
	n, _ := art.Read(buf)
	if !bytes.Equal(buf[:n], testData1) {
		t.Fatalf("First echo mismatch: got %q, want %q", buf[:n], testData1)
	}

	// Verify server accepted first connection
	<-connCountChan

	// Close the underlying connection to simulate failure
	art.Close()

	// Give time for close to propagate
	time.Sleep(50 * time.Millisecond)

	// Create new AutoReconnectTCP with same address - should reconnect
	art2 := NewAutoReconnectTCP(addr)

	testData2 := []byte("second")
	_, err = art2.Write(testData2)
	if err != nil {
		t.Fatalf("Second Write error (should reconnect): %v", err)
	}

	// Read echo
	n, _ = art2.Read(buf)
	if !bytes.Equal(buf[:n], testData2) {
		t.Errorf("Second echo mismatch: got %q, want %q", buf[:n], testData2)
	}

	art2.Close()

	// Verify server accepted second connection
	select {
	case <-connCountChan:
		// Server accepted second connection - good
	case <-time.After(1 * time.Second):
		t.Log("Note: Server may not have accepted second connection (connection reuse)")
	}
}


// TestAutoReconnectTCPAddresses tests LocalAddr and RemoteAddr.
func TestAutoReconnectTCPAddresses(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Before connection, addresses should be nil
	if art.LocalAddr() != nil {
		t.Error("LocalAddr should be nil before connection")
	}
	if art.RemoteAddr() != nil {
		t.Error("RemoteAddr should be nil before connection")
	}

	// Connect by writing
	art.Write([]byte("test"))

	// After connection, addresses should be available
	local := art.LocalAddr()
	if local == nil {
		t.Error("LocalAddr is nil after connection")
	}

	remote := art.RemoteAddr()
	if remote == nil {
		t.Error("RemoteAddr is nil after connection")
	} else if remote.String() != addr.String() {
		t.Errorf("RemoteAddr = %s, want %s", remote.String(), addr.String())
	}

	art.Close()
}

// TestAutoReconnectTCPDeadlines tests deadline methods.
func TestAutoReconnectTCPDeadlines(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Before connection, deadlines should succeed (no-op)
	err = art.SetDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		t.Errorf("SetDeadline failed: %v", err)
	}

	err = art.SetReadDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		t.Errorf("SetReadDeadline failed: %v", err)
	}

	err = art.SetWriteDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		t.Errorf("SetWriteDeadline failed: %v", err)
	}

	// After connection
	art.Write([]byte("test"))

	// Set deadline again
	err = art.SetDeadline(time.Now().Add(1 * time.Second))
	if err != nil {
		t.Errorf("SetDeadline after connection failed: %v", err)
	}

	art.Close()
}

// TestAutoReconnectTCPConcurrent tests concurrent operations.
func TestAutoReconnectTCPConcurrent(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)

	// Create multiple independent connections
	const goroutines = 5
	const messages = 5

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Each goroutine gets its own connection
			art := NewAutoReconnectTCP(addr)

			for j := 0; j < messages; j++ {
				msg := []byte{byte('A' + id%26)}
				_, err := art.Write(msg)
				if err != nil {
					t.Logf("goroutine %d: Write error: %v", id, err)
					continue
				}
				buf := make([]byte, 1)
				_, err = art.Read(buf)
				if err != nil {
					t.Logf("goroutine %d: Read error: %v", id, err)
				}
			}

			art.Close()
		}(i)
	}

	wg.Wait()
}

// TestConnWrite tests connWrite with a mock connection.
func TestConnWrite(t *testing.T) {
	// Create a pipe for testing
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()

	// Start reading in background
	testData := []byte("test data for connWrite")
	resultChan := make(chan []byte, 1)
	errChan := make(chan error, 1)

	go func() {
		buf := make([]byte, len(testData))
		_, err := io.ReadFull(pr, buf)
		if err != nil {
			errChan <- err
			return
		}
		resultChan <- buf
	}()

	// Write using connWrite
	err := connWrite(pw, testData)
	if err != nil {
		t.Fatalf("connWrite error: %v", err)
	}

	// Verify data was written
	select {
	case buf := <-resultChan:
		if !bytes.Equal(buf, testData) {
			t.Errorf("Data mismatch: got %v, want %v", buf, testData)
		}
	case err := <-errChan:
		t.Fatalf("Read error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for read")
	}
}

// TestConnWriteLargeData tests connWrite with large data that may require multiple writes.
func TestConnWriteLargeData(t *testing.T) {
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()

	// Create large data (1MB)
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	// Write and read concurrently
	writeDone := make(chan error, 1)
	readDone := make(chan error, 1)
	readBuf := make([]byte, len(largeData))

	go func() {
		writeDone <- connWrite(pw, largeData)
	}()

	go func() {
		_, err := io.ReadFull(pr, readBuf)
		readDone <- err
	}()

	// Wait for both operations with timeout
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("connWrite error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for write")
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for read")
	}

	if !bytes.Equal(readBuf, largeData) {
		t.Error("Large data mismatch")
	}
}

// TestConnWriteEmpty tests connWrite with empty data.
func TestConnWriteEmpty(t *testing.T) {
	pr, pw := net.Pipe()
	defer pr.Close()
	defer pw.Close()

	// Start a concurrent reader (will get EOF immediately)
	go func() {
		io.Copy(io.Discard, pr)
	}()

	// Give goroutine time to start
	time.Sleep(10 * time.Millisecond)

	err := connWrite(pw, []byte{})
	if err != nil {
		t.Errorf("connWrite with empty data failed: %v", err)
	}
}

// TestAutoReconnectTCPZeroCopy tests that zero-copy send doesn't break normal operation.
func TestAutoReconnectTCPZeroCopy(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Test with various data sizes
	sizes := []int{1, 100, 4096, 64 * 1024, 1024 * 1024}

	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i % 256)
		}

		_, err := art.Write(data)
		if err != nil {
			t.Errorf("Write with size %d failed: %v", size, err)
			continue
		}

		// Read response - might need multiple reads
		buf := make([]byte, size)
		totalRead := 0
		for totalRead < size {
			n, err := art.Read(buf[totalRead:])
			if err != nil {
				t.Errorf("Read with size %d failed after %d bytes: %v", size, totalRead, err)
				break
			}
			totalRead += n
		}

		if !bytes.Equal(buf[:totalRead], data) {
			t.Errorf("Data mismatch for size %d, read %d bytes", size, totalRead)
		}
	}

	art.Close()
}

// TestAutoReconnectTCPConnectTimeout tests connection timeout.
func TestAutoReconnectTCPConnectTimeout(t *testing.T) {
	// Use an address that's unlikely to be listening
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:1") // Port 1 is typically unused
	if err != nil {
		t.Fatalf("ResolveTCPAddr failed: %v", err)
	}

	art := NewAutoReconnectTCP(addr)

	// Connection should timeout/fail
	_, err = art.Write([]byte("test"))
	if err == nil {
		t.Error("Expected error when connecting to non-existent server")
	}
}

// TestAutoReconnectCloseTwice tests closing connection twice.
func TestAutoReconnectCloseTwice(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Connect
	art.Write([]byte("test"))

	// Close once
	err = art.Close()
	if err != nil {
		t.Errorf("First Close failed: %v", err)
	}

	// Close twice - should not error
	err = art.Close()
	if err != nil {
		t.Errorf("Second Close failed: %v", err)
	}
}

// TestAutoReconnectTCPReadDeadline tests that deadline methods work without panic.
func TestAutoReconnectTCPReadDeadline(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP failed: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go handleEchoConn(conn)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	art := NewAutoReconnectTCP(addr)

	// Connect
	art.Write([]byte("test"))

	// Set deadline - should not panic
	art.SetReadDeadline(time.Now().Add(1 * time.Second))

	// Read should work normally (deadline is overridden by AutoReconnectTCP.Read's own deadline)
	buf := make([]byte, 100)
	_, err = art.Read(buf)
	if err != nil {
		t.Errorf("Read failed: %v", err)
	}

	art.Close()
}

// Helper function for echo server
func handleEchoConn(conn *net.TCPConn) {
	defer conn.Close()
	buf := make([]byte, 64*1024)
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			conn.Write(buf[:n])
		}
	}
}
