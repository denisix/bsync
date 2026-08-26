package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestDirServerState builds a dirServerState with a fresh fileTable
// rooted at a temp dir, for driving dirHandleConn directly in tests without
// a real listener.
func newTestDirServerState(t *testing.T, workers int) *dirServerState {
	t.Helper()
	// See newTestFileTable's identical call for why this must be
	// unconditional: these tests drive the real block-decode/decrypt path
	// and must not inherit a stale encryption key from an unrelated test
	// that ran earlier in the same process.
	resetEngineGlobals()
	root := t.TempDir()
	ow := NewOrderedWriter(nil, workers)
	ow.EnablePerItemErrors()
	t.Cleanup(func() { ow.Close() })
	return &dirServerState{ft: newFileTable(root, workers, ow)}
}

// runDirHandleConn drives dirHandleConn against one side of a net.Pipe,
// returning the other side (the "client" the test drives by hand) and a
// channel that receives dirHandleConn's return value.
func runDirHandleConn(t *testing.T, st *dirServerState) (net.Conn, <-chan error) {
	t.Helper()
	server, client := net.Pipe()
	errCh := make(chan error, 1)
	doneChan := make(chan struct{})
	go func() { errCh <- dirHandleConn(server, st, doneChan) }()
	t.Cleanup(func() { client.Close() })
	return client, errCh
}

func writeDirMsg(t *testing.T, conn net.Conn, msg *Msg) {
	t.Helper()
	msg.MagicHead = stringToFixedSizeArray(magicHead)
	data, err := pack(msg)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readDirMsg(conn net.Conn) (*Msg, error) {
	buf := make([]byte, binary.Size(Msg{}))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return unpack(buf)
}

// drainDirError reads one Msg and, if it's an OpError, also reads and
// discards its trailing text. net.Pipe() writes block until a matching read
// fully drains them, so a test that reads only the header and stops would
// leave the server-side sendDirError call (header write, then a SEPARATE
// text-payload write) blocked forever on that second write -- hanging
// dirHandleConn rather than letting it return, and making the corresponding
// expectHandlerError call time out for a reason that has nothing to do with
// the behavior actually under test.
func drainDirError(t *testing.T, conn net.Conn) *Msg {
	t.Helper()
	msg, err := readDirMsg(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if Op(msg.Op) == OpError && msg.DataSize > 0 {
		buf := make([]byte, msg.DataSize)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("drain error text: %v", err)
		}
	}
	return msg
}

func expectHandlerError(t *testing.T, errCh <-chan error, wantSubstr string) {
	t.Helper()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected dirHandleConn to return an error, got nil")
		}
		if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("error = %q, want substring %q", err.Error(), wantSubstr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dirHandleConn did not return in time")
	}
}

func TestDirHandleConnRejectsBadMagic(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	msg := &Msg{Op: uint8(OpAnnounceEnd)}
	// writeDirMsg always sets the correct magic; overwrite it here.
	badMagic := stringToFixedSizeArray("not-the-real-magic")
	msg.MagicHead = badMagic
	data, err := pack(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	expectHandlerError(t, errCh, "invalid request magic")
}

func TestDirHandleConnRejectsNonzeroReserved(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	msg := &Msg{Op: uint8(OpAnnounceEnd), Reserved: [5]byte{1}}
	writeDirMsg(t, client, msg)
	expectHandlerError(t, errCh, "reserved")
}

func TestDirHandleConnRejectsUnannouncedFileOpen(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 999, BlockSize: 4096})

	resp := drainDirError(t, client)
	if Op(resp.Op) != OpError {
		t.Errorf("response op = %d, want OpError", resp.Op)
	}
	expectHandlerError(t, errCh, "acquire file 999")
}

func TestDirHandleConnRejectsUnsolicitedZeroWrite(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4096}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)

	// Open the file, but send a zero-write WITHOUT a preceding OpBlockCheck.
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4096})
	if _, err := readDirMsg(client); err != nil {
		t.Fatalf("read file-open ack: %v", err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockZero), FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: 4096})
	expectHandlerError(t, errCh, "unsolicited")
}

func TestDirHandleConnRejectsUnsolicitedDataWrite(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4096}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4096})
	if _, err := readDirMsg(client); err != nil {
		t.Fatalf("read file-open ack: %v", err)
	}
	// handleBlockData checks pendingBlocks before it ever reads the payload,
	// so no payload bytes need to follow this header for the rejection to
	// fire.
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockData), FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: 4096, DataSize: 100})
	expectHandlerError(t, errCh, "unsolicited")
}

func TestDirHandleConnRejectsBlockWriteWithoutFileOpen(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4096}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	// No OpFileOpen at all.
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 4096, FileSize: 4096})
	expectHandlerError(t, errCh, "without a matching OpFileOpen")
}

func TestDirHandleConnRejectsCheckForWrongBoundFile(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "a.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	if err := st.ft.Announce(AnnounceRec{FileID: 2, Kind: KindFile, Path: "b.txt", FileSize: 10}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 10})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	// Request a checksum for FileID 2 without ever opening it on this
	// connection (still bound to FileID 1).
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 2, BlockIdx: 0, BlockSize: 10, FileSize: 10})
	expectHandlerError(t, errCh, "without a matching OpFileOpen")
}

func TestDirHandleConnRejectsBlockSizeMismatch(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4096}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4096})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	// Same file, different block size on the very next message.
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 2048, FileSize: 4096})
	drainDirError(t, client)
	expectHandlerError(t, errCh, "block size changed")
}

func TestDirHandleConnRejectsUnexpectedOp(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: 250}) // not any defined opcode
	expectHandlerError(t, errCh, "unexpected op")
}

func TestDirHandleConnAnnounceRejectsPathTraversalOverTheWire(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	trailer, err := encodeAnnounceBatch([]AnnounceRec{{FileID: 1, Kind: KindFile, Path: "../../etc/passwd"}})
	if err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounce), DataSize: uint32(len(trailer))})
	if _, err := client.Write(trailer); err != nil {
		t.Fatal(err)
	}
	expectHandlerError(t, errCh, "unsafe path")
	if st.ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0 -- the traversal attempt must not have registered", st.ft.AnnouncedCount())
	}
}

func TestDirHandleConnAnnounceRejectsAbsolutePathOverTheWire(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	trailer, err := encodeAnnounceBatch([]AnnounceRec{{FileID: 1, Kind: KindFile, Path: "/etc/passwd"}})
	if err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounce), DataSize: uint32(len(trailer))})
	if _, err := client.Write(trailer); err != nil {
		t.Fatal(err)
	}
	expectHandlerError(t, errCh, "unsafe path")
}

func TestDirHandleConnFullExchangeWritesCorrectContent(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4})
	ack, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(ack.Op) != OpFileAck || ack.Flags&FlagCreated == 0 {
		t.Fatalf("expected OpFileAck with FlagCreated, got %+v", ack)
	}

	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 4, FileSize: 4})
	hashHdr, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}
	// New destination file is all zero; a "test" (0x74,0x65,0x73,0x74)
	// payload will not match, so the server should accept the write below.

	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockData), FileID: 1, BlockIdx: 0, BlockSize: 4, FileSize: 4, DataSize: 4})
	if _, err := client.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	dataAck, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(dataAck.Op) != OpAck || dataAck.FileID != 1 || dataAck.BlockIdx != 0 {
		t.Fatalf("unexpected data ack: %+v", dataAck)
	}

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileDone), FileID: 1})
	client.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("dirHandleConn returned an error on clean close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dirHandleConn did not return after client close")
	}

	df, err := st.ft.lookup(1)
	if err != nil {
		t.Fatal(err)
	}
	ctxDeadline := time.Now().Add(3 * time.Second)
	for !df.isFinalized() && time.Now().Before(ctxDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !df.isFinalized() {
		t.Fatal("file never finalized after OpFileDone and connection close (refs never released)")
	}
	content, err := os.ReadFile(filepath.Join(st.ft.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "test" {
		t.Errorf("content = %q, want %q", content, "test")
	}
}

func TestDirHandleConnZeroBlockWriteRoundTrip(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 4, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	// Pre-seed the destination with non-zero content so a passing test
	// actually proves OpBlockZero wrote real zeros, not just that the file
	// started out zero from Acquire's truncate.
	if err := os.WriteFile(filepath.Join(st.ft.root, "f.txt"), []byte("xxxx"), 0644); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 4, FileSize: 4})
	hashHdr := drainDirError(t, client)
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}

	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockZero), FileID: 1, BlockIdx: 0, BlockSize: 4, FileSize: 4})
	ack, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(ack.Op) != OpAck || ack.FileID != 1 || ack.BlockIdx != 0 {
		t.Fatalf("unexpected zero-write ack: %+v", ack)
	}

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileDone), FileID: 1})
	client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("dirHandleConn returned an error on clean close: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(st.ft.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, make([]byte, 4)) {
		t.Errorf("content = %q, want four zero bytes", content)
	}
}

func TestDirHandleConnCompressedBlockRoundTrip(t *testing.T) {
	st := newTestDirServerState(t, 1)
	// A long, repetitive payload compresses well; compressData only
	// reports UseCompressed on the SENDER side normally, but the server's
	// handleBlockData just trusts FlagCompressed and decodes accordingly,
	// so any real compressed payload exercises the same path.
	original := bytes.Repeat([]byte("compress-me "), 200)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: uint64(len(original)), Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	comp, err := compressData(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp) >= len(original) {
		t.Fatalf("test payload did not compress (comp=%d orig=%d) -- adjust the fixture", len(comp), len(original))
	}
	client, errCh := runDirHandleConn(t, st)

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: uint32(len(original))})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: uint32(len(original)), FileSize: uint64(len(original))})
	hashHdr := drainDirError(t, client)
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}

	writeDirMsg(t, client, &Msg{
		Op: uint8(OpBlockData), Flags: FlagCompressed, FileID: 1, BlockIdx: 0,
		BlockSize: uint32(len(original)), FileSize: uint64(len(original)), DataSize: uint32(len(comp)),
	})
	if _, err := client.Write(comp); err != nil {
		t.Fatal(err)
	}
	ack, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(ack.Op) != OpAck {
		t.Fatalf("expected OpAck, got op %d", ack.Op)
	}

	writeDirMsg(t, client, &Msg{Op: uint8(OpFileDone), FileID: 1})
	client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("dirHandleConn returned an error on clean close: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(st.ft.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, original) {
		t.Error("content mismatch after a compressed block round trip")
	}
}

func TestDirHandleConnRejectsCorruptCompressedPayload(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 8, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 8})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 8, FileSize: 8})
	hashHdr := drainDirError(t, client)
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}

	garbage := []byte("not-real-zstd!!")
	writeDirMsg(t, client, &Msg{
		Op: uint8(OpBlockData), Flags: FlagCompressed, FileID: 1, BlockIdx: 0,
		BlockSize: 8, FileSize: 8, DataSize: uint32(len(garbage)),
	})
	if _, err := client.Write(garbage); err != nil {
		t.Fatal(err)
	}
	expectHandlerError(t, errCh, "decompress")

	// Nothing must have been written -- the destination stays at its
	// Acquire-time zero-filled state.
	content, err := os.ReadFile(filepath.Join(st.ft.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, make([]byte, 8)) {
		t.Errorf("content = %q, a corrupt compressed payload must never be written", content)
	}
}

func TestDirHandleConnRejectsOversizedPayload(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 8, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 8})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 8, FileSize: 8})
	hashHdr := drainDirError(t, client)
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}
	// DataSize claims far more than BlockSize+encryptOverhead could ever
	// legitimately be -- handleBlockData must reject this before even
	// trying to read that many bytes off the wire.
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockData), FileID: 1, BlockIdx: 0, BlockSize: 8, FileSize: 8, DataSize: 1 << 20})
	expectHandlerError(t, errCh, "too large")
}

func TestDirHandleConnRejectsBlockIndexBeyondFileSize(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 8, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 8})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	// BlockIdx 5 at BlockSize 8 starts at offset 40, far past the 8-byte
	// announced file size.
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 5, BlockSize: 8, FileSize: 8})
	expectHandlerError(t, errCh, "invalid block")
}

func TestDirHandleConnBlockCheckOnFreshFileReportsZeroHash(t *testing.T) {
	// Directory mode's Acquire always creates and truncates to the
	// announced size before any block traffic is possible, so a "brand
	// new destination" checksum request never hits an absent/short file --
	// unlike the legacy single-file server, there is no FlagNoDest case
	// here; the common new-tree case is simply a zero-filled read that
	// hashes to zeroBlockHash. Worth pinning explicitly since it's the
	// overwhelmingly common case on a fresh destination tree.
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 16, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	writeDirMsg(t, client, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 16})
	if _, err := readDirMsg(client); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: 0, BlockSize: 16, FileSize: 16})
	hashHdr := drainDirError(t, client)
	if Op(hashHdr.Op) != OpHash {
		t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
	}
	hashBuf := make([]byte, hashHdr.DataSize)
	if _, err := io.ReadFull(client, hashBuf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hashBuf, zeroBlockHash) {
		t.Errorf("hash = %x, want the zero-block hash for a fresh destination", hashBuf)
	}
	client.Close()
	<-errCh
}

func TestDirHandleConnRejectsTreeDoneBeforeAnnounceEnd(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	// OpTreeDone with no preceding OpAnnounceEnd at all.
	writeDirMsg(t, client, &Msg{Op: uint8(OpTreeDone), BlockIdx: 0})
	// handleTreeDone's rejection path calls sendDirError, a header write
	// followed by a SEPARATE text write; drain both or the server-side
	// second write blocks forever on net.Pipe() and dirHandleConn never
	// returns (see drainDirError's comment).
	drainDirError(t, client)
	expectHandlerError(t, errCh, "before announce end")
}

func TestDirHandleConnRejectsTreeDoneWithWrongFileCount(t *testing.T) {
	st := newTestDirServerState(t, 1)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 1}); err != nil {
		t.Fatal(err)
	}
	client, errCh := runDirHandleConn(t, st)
	// Claims 5 announced files; the server only ever saw 1. This is the
	// cross-check that guards --delete against acting on a truncated
	// announce stream.
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounceEnd), BlockIdx: 5})
	writeDirMsg(t, client, &Msg{Op: uint8(OpTreeDone), BlockIdx: 5})
	drainDirError(t, client) // see the identical comment in the announce-end test above
	expectHandlerError(t, errCh, "claims 5 announced entries, server saw 1")
}

func TestDirHandleConnAnnounceEmptyBatchIsAccepted(t *testing.T) {
	st := newTestDirServerState(t, 1)
	client, errCh := runDirHandleConn(t, st)
	trailer, err := encodeAnnounceBatch(nil)
	if err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounce), DataSize: uint32(len(trailer))})
	if _, err := client.Write(trailer); err != nil {
		t.Fatal(err)
	}
	// Immediately follow with something that must succeed if the empty
	// batch didn't wedge or error the connection.
	writeDirMsg(t, client, &Msg{Op: uint8(OpAnnounceEnd), BlockIdx: 0})
	writeDirMsg(t, client, &Msg{Op: uint8(OpTreeDone), BlockIdx: 0})
	resp, err := readDirMsg(client)
	if err != nil {
		t.Fatal(err)
	}
	if Op(resp.Op) != OpTreeAck {
		t.Fatalf("expected OpTreeAck after an empty announce batch, got op %d", resp.Op)
	}
	client.Close()
	<-errCh
	if st.ft.AnnouncedCount() != 0 {
		t.Errorf("AnnouncedCount = %d, want 0", st.ft.AnnouncedCount())
	}
}

// TestDirHandleConnTwoConnectionsWriteSameFileConcurrently is the soft-
// binding promise asserted directly on the wire (not just through
// fileTable's Go API): two independent connections both OpFileOpen the
// same file and each write a different block of it, exactly as two workers
// in the same worker pool would.
func TestDirHandleConnTwoConnectionsWriteSameFileConcurrently(t *testing.T) {
	st := newTestDirServerState(t, 2)
	if err := st.ft.Announce(AnnounceRec{FileID: 1, Kind: KindFile, Path: "f.txt", FileSize: 8, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	connA, errChA := runDirHandleConn(t, st)
	connB, errChB := runDirHandleConn(t, st)

	writeDirMsg(t, connA, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4})
	if _, err := readDirMsg(connA); err != nil {
		t.Fatal(err)
	}
	writeDirMsg(t, connB, &Msg{Op: uint8(OpFileOpen), FileID: 1, BlockSize: 4})
	if _, err := readDirMsg(connB); err != nil {
		t.Fatal(err)
	}

	writeBlock := func(conn net.Conn, idx uint32, data []byte) {
		t.Helper()
		writeDirMsg(t, conn, &Msg{Op: uint8(OpBlockCheck), FileID: 1, BlockIdx: idx, BlockSize: 4, FileSize: 8})
		hashHdr := drainDirError(t, conn)
		if Op(hashHdr.Op) != OpHash {
			t.Fatalf("expected OpHash, got op %d", hashHdr.Op)
		}
		hashBuf := make([]byte, hashHdr.DataSize)
		if _, err := io.ReadFull(conn, hashBuf); err != nil {
			t.Fatal(err)
		}
		writeDirMsg(t, conn, &Msg{Op: uint8(OpBlockData), FileID: 1, BlockIdx: idx, BlockSize: 4, FileSize: 8, DataSize: uint32(len(data))})
		if _, err := conn.Write(data); err != nil {
			t.Fatal(err)
		}
		ack, err := readDirMsg(conn)
		if err != nil {
			t.Fatal(err)
		}
		if Op(ack.Op) != OpAck {
			t.Fatalf("expected OpAck, got op %d", ack.Op)
		}
	}
	writeBlock(connA, 0, []byte("AAAA"))
	writeBlock(connB, 1, []byte("BBBB"))

	if err := st.ft.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	connA.Close()
	connB.Close()
	if err := <-errChA; err != nil {
		t.Fatalf("connection A: %v", err)
	}
	if err := <-errChB; err != nil {
		t.Fatalf("connection B: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := st.ft.WaitAllFinalized(ctx); err != nil {
		t.Fatalf("WaitAllFinalized: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(st.ft.root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "AAAABBBB" {
		t.Errorf("content = %q, want %q", content, "AAAABBBB")
	}
}
