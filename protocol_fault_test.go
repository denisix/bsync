package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func writeTestMsg(t *testing.T, conn net.Conn, msg *Msg) {
	t.Helper()
	data, err := pack(msg)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func readTestMsg(conn net.Conn) (*Msg, error) {
	buf := make([]byte, msgSize)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return unpack(buf)
}

func scriptedDownloadServer(t *testing.T, respond func(net.Conn) error) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	errCh := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		errCh <- respond(conn)
	}()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("scripted server: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("scripted server did not terminate")
		}
	})
	return l.Addr().String()
}

func sendDownloadMetadata(conn net.Conn, fileSize uint64, blockSize uint32) error {
	if _, err := readTestMsg(conn); err != nil {
		return err
	}
	data, err := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownData), BlockSize: blockSize, FileSize: fileSize})
	if err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}

func TestFaultDownloadRejectsPrematureCompletion(t *testing.T) {
	resetEngineGlobals()
	const bs = uint32(4096)
	addr := scriptedDownloadServer(t, func(conn net.Conn) error {
		if err := sendDownloadMetadata(conn, uint64(bs*2), bs); err != nil {
			return err
		}
		req, err := readTestMsg(conn)
		if err != nil {
			return err
		}
		done, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownDone), BlockIdx: req.BlockIdx, BlockSize: bs, FileSize: uint64(bs * 2)})
		_, err = conn.Write(done)
		return err
	})
	dst, err := os.CreateTemp(t.TempDir(), "dst-*")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	err = startClientDownload(dst, addr, 0, bs, false, 1)
	if err == nil || !strings.Contains(err.Error(), "premature completion") {
		t.Fatalf("expected premature completion error, got %v", err)
	}
}

func TestFaultDownloadRejectsMalformedResponses(t *testing.T) {
	const bs = uint32(4096)
	cases := map[string]func(net.Conn) error{
		"zero-with-payload": func(conn net.Conn) error {
			msg, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownData), Flags: FlagZero, BlockSize: bs, FileSize: uint64(bs), DataSize: 1})
			_, err := conn.Write(msg)
			return err
		},
		"payload-missing": func(conn net.Conn) error {
			msg, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownData), BlockSize: bs, FileSize: uint64(bs)})
			_, err := conn.Write(msg)
			return err
		},
		"truncated-payload": func(conn net.Conn) error {
			msg, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownData), BlockSize: bs, FileSize: uint64(bs), DataSize: bs})
			if _, err := conn.Write(msg); err != nil {
				return err
			}
			_, err := conn.Write([]byte("short"))
			return err
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			resetEngineGlobals()
			addr := scriptedDownloadServer(t, func(conn net.Conn) error {
				if err := sendDownloadMetadata(conn, uint64(bs), bs); err != nil {
					return err
				}
				if _, err := readTestMsg(conn); err != nil {
					return err
				}
				return response(conn)
			})
			dst, err := os.CreateTemp(t.TempDir(), "dst-*")
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			if err := startClientDownload(dst, addr, 0, bs, false, 1); err == nil {
				t.Fatal("malformed block response unexpectedly succeeded")
			}
		})
	}
}

func TestFaultDownloadReportsDestinationWriteFailure(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full unavailable")
	}
	const bs = uint32(4096)
	addr := scriptedDownloadServer(t, func(conn net.Conn) error {
		if err := sendDownloadMetadata(conn, uint64(bs), bs); err != nil {
			return err
		}
		if _, err := readTestMsg(conn); err != nil {
			return err
		}
		msg, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownData), BlockSize: bs, FileSize: uint64(bs), DataSize: bs})
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		_, err := conn.Write(bytes.Repeat([]byte{0x7f}, int(bs)))
		return err
	})
	dst, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("open /dev/full: %v", err)
	}
	defer dst.Close()
	if err := startClientDownload(dst, addr, 0, bs, false, 1); err == nil {
		t.Fatal("download into /dev/full unexpectedly succeeded")
	}
}

func TestFaultServerRejectsContradictoryUploadMessages(t *testing.T) {
	const bs = uint32(4096)
	for name, bad := range map[string]*Msg{
		// DONE must not carry a payload.
		"done-payload": {Op: uint8(OpFileDone), BlockSize: bs, FileSize: uint64(bs), DataSize: 1},
		// A zero-block write can't also carry a payload.
		"zero-payload": {Op: uint8(OpBlockZero), BlockSize: bs, FileSize: uint64(bs), DataSize: 1},
		// A checksum request (no payload) can't carry the compressed flag.
		"checksum-compress": {Op: uint8(OpBlockCheck), BlockSize: bs, FileSize: uint64(bs), Flags: FlagCompressed},
	} {
		t.Run(name, func(t *testing.T) {
			resetEngineGlobals()
			initial := bytes.Repeat([]byte{0x55}, int(bs))
			path := writeTempFile(t, initial)
			file, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			server, client := net.Pipe()
			errCh := make(chan error, 1)
			go func() { errCh <- serverHandleReq(server, file, NewChecksumCache(0), nil) }()
			bad.MagicHead = stringToFixedSizeArray(magicHead)
			writeTestMsg(t, client, bad)
			client.Close()
			if err := <-errCh; err == nil {
				t.Fatal("invalid upload message unexpectedly succeeded")
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, initial) {
				t.Fatal("invalid upload message modified destination")
			}
		})
	}
}

func TestFaultServerRejectsInvalidDownloadRequest(t *testing.T) {
	resetEngineGlobals()
	const bs = uint32(4096)
	srcPath := writeTempFile(t, bytes.Repeat([]byte("a"), int(bs)))
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- serverHandleUpload(server, src, uint64(bs), NewChecksumCache(0), 1, false) }()
	// A well-formed OpDownReq must never carry a payload.
	writeTestMsg(t, client, &Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpDownReq), BlockSize: bs, DataSize: 1})
	client.Close()
	if err := <-errCh; err == nil {
		t.Fatal("invalid download request unexpectedly succeeded")
	}
}

func TestFaultServerRejectsUnsolicitedOrChangedUploadWrite(t *testing.T) {
	const bs = uint32(4096)
	for name, messages := range map[string][]*Msg{
		"unsolicited": {
			{Op: uint8(OpBlockData), BlockIdx: 0, BlockSize: bs, FileSize: uint64(bs), DataSize: bs},
		},
		"changed-metadata": {
			{Op: uint8(OpBlockCheck), BlockIdx: 0, BlockSize: bs, FileSize: uint64(bs)},
			{Op: uint8(OpBlockData), BlockIdx: 0, BlockSize: bs, FileSize: uint64(bs) * 2, DataSize: bs},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resetEngineGlobals()
			path := writeTempFile(t, bytes.Repeat([]byte{0x31}, int(bs)))
			file, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			server, client := net.Pipe()
			errCh := make(chan error, 1)
			go func() { errCh <- serverHandleReq(server, file, NewChecksumCache(0), nil) }()
			for _, msg := range messages {
				msg.MagicHead = stringToFixedSizeArray(magicHead)
				writeTestMsg(t, client, msg)
				if msg.DataSize > 0 {
					_, _ = client.Write(bytes.Repeat([]byte{0x72}, int(msg.DataSize)))
				} else {
					// A checksum request now gets a framed OpHash reply
					// (header + payload), not bare bytes; drain both.
					hashHdr, err := readTestMsg(client)
					if err != nil {
						t.Fatal(err)
					}
					hash := make([]byte, hashHdr.DataSize)
					if _, err := io.ReadFull(client, hash); err != nil {
						t.Fatal(err)
					}
				}
			}
			client.Close()
			if err := <-errCh; err == nil {
				t.Fatal("invalid upload write unexpectedly succeeded")
			}
		})
	}
}

func TestFaultUploadRetriesAfterLostAcknowledgement(t *testing.T) {
	const bs = uint32(4096)
	o := e2eOpts{blockSize: bs, workers: 1, noCompress: true}.norm()
	setupTransfer(t, o)
	data := bytes.Repeat([]byte{0x4a}, int(bs))
	srcPath := writeTempFile(t, data)
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	received := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			conn, err := l.Accept()
			if err != nil {
				errCh <- err
				return
			}
			req, err := readTestMsg(conn)
			if err != nil {
				errCh <- err
				return
			}
			if Op(req.Op) == OpFileDone {
				errCh <- fmt.Errorf("unexpected DONE before retry")
				return
			}
			// Framed OpHash reply: header, then the 16-byte digest. All
			// zero bytes forces a mismatch against the block's real FNV
			// hash, so the client proceeds to send the payload.
			hashMsg, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpHash), BlockIdx: req.BlockIdx, BlockSize: req.BlockSize, FileSize: req.FileSize, DataSize: 16})
			conn.Write(hashMsg)
			conn.Write(make([]byte, 16))
			payloadHeader, err := readTestMsg(conn)
			if err != nil {
				errCh <- err
				return
			}
			payload := make([]byte, payloadHeader.DataSize)
			if _, err := io.ReadFull(conn, payload); err != nil {
				errCh <- err
				return
			}
			if attempt == 0 {
				conn.Close() // Force retry after the sender has transmitted data.
				continue
			}
			received <- payload
			ack, _ := pack(&Msg{MagicHead: stringToFixedSizeArray(magicHead), Op: uint8(OpAck), BlockIdx: 0, BlockSize: bs, FileSize: uint64(bs)})
			conn.Write(ack)
			conn.Close()
		}
		conn, err := l.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		done, err := readTestMsg(conn)
		if err != nil || Op(done.Op) != OpFileDone {
			errCh <- fmt.Errorf("missing DONE: %v", err)
			return
		}
		errCh <- nil
	}()
	if err := startClient(src, l.Addr().String(), 0, uint64(bs), bs, true, NewChecksumCache(0), 1); err != nil {
		t.Fatalf("upload did not recover: %v", err)
	}
	if got := <-received; !bytes.Equal(got, data) {
		t.Fatal("retried data mismatch")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}
