package main

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

func TestUnpackTooShort(t *testing.T) {
	// A buffer shorter than a packed Msg must error.
	if _, err := unpack([]byte{1, 2, 3}); err == nil {
		t.Error("expected error unpacking a too-short buffer")
	}
}

func TestPackedSizeConstant(t *testing.T) {
	msg := &Msg{MagicHead: stringToFixedSizeArray(magicHead)}
	data, err := pack(msg)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	want := binary.Size(Msg{})
	if len(data) != want {
		t.Errorf("packed size = %d, want %d", len(data), want)
	}
	// magic(17)+op(1)+flags(1)+reserved(5)+fileID(4)+blockIdx(4)+blockSize(4)+fileSize(8)+dataSize(4) = 48
	if want != 48 {
		t.Errorf("binary.Size(Msg) = %d, want 48", want)
	}
	if want != msgSize {
		t.Errorf("binary.Size(Msg) = %d, want msgSize = %d", want, msgSize)
	}
}

func TestPackUnpackMaxValues(t *testing.T) {
	orig := &Msg{
		MagicHead: stringToFixedSizeArray(magicHead),
		Op:        ^uint8(0),
		Flags:     ^uint8(0),
		Reserved:  [5]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		FileID:    ^uint32(0),
		BlockIdx:  ^uint32(0),
		BlockSize: ^uint32(0),
		FileSize:  ^uint64(0),
		DataSize:  ^uint32(0),
	}
	data, err := pack(orig)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	got, err := unpack(data)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if *got != *orig {
		t.Errorf("max-value round-trip mismatch:\n got %+v\nwant %+v", got, orig)
	}
}

func TestZeroBlockHashLength(t *testing.T) {
	if len(zeroBlockHash) != 16 {
		t.Errorf("zeroBlockHash length = %d, want 16 (FNV-128a)", len(zeroBlockHash))
	}
}

func TestStringToFixedSizeArrayTruncates(t *testing.T) {
	long := "this string is definitely longer than seventeen bytes"
	arr := stringToFixedSizeArray(long)
	if len(arr) != magicLen {
		t.Fatalf("len = %d, want %d", len(arr), magicLen)
	}
	if string(arr[:]) != long[:magicLen] {
		t.Errorf("truncation mismatch: %q", string(arr[:]))
	}
}

func TestBlockPayloadSizeBoundaries(t *testing.T) {
	tests := []struct {
		fileSize  uint64
		blockSize uint32
		blockIdx  uint32
		want      uint32
	}{
		{0, 4096, 0, 0},
		{4096, 4096, 0, 4096},
		{4097, 4096, 1, 1},
		{4096, 4096, 1, 0},
		{4096, 0, 0, 0},
	}
	for _, tt := range tests {
		if got := blockPayloadSize(tt.fileSize, tt.blockSize, tt.blockIdx); got != tt.want {
			t.Errorf("blockPayloadSize(%d, %d, %d) = %d, want %d", tt.fileSize, tt.blockSize, tt.blockIdx, got, tt.want)
		}
	}
}

func TestValidateProtocolMessageStates(t *testing.T) {
	validUpload := []*Msg{
		{Op: uint8(OpFileDone)},
		{Op: uint8(OpBlockZero)},
		{Op: uint8(OpBlockCheck)},
		{Op: uint8(OpBlockData), DataSize: 1},
		{Op: uint8(OpBlockData), DataSize: 1, Flags: FlagCompressed},
	}
	for _, msg := range validUpload {
		if err := validateClientFrame(msg); err != nil {
			t.Errorf("valid upload message %+v rejected: %v", msg, err)
		}
	}
	invalidUpload := []*Msg{
		{Op: uint8(OpFileDone), DataSize: 1},             // DONE must not carry a payload
		{Op: uint8(OpBlockZero), Flags: FlagCompressed},  // zero write can't also be compressed
		{Op: uint8(OpBlockZero), DataSize: 1},            // zero write can't carry a payload
		{Op: uint8(OpBlockCheck), Flags: FlagCompressed}, // checksum request can't carry flags
		{Op: uint8(OpBlockData)},                         // data write needs DataSize > 0
		{Op: uint8(OpAck)},                               // a server-only op sent by the client
		{Op: uint8(OpBlockCheck), Reserved: [5]byte{9}},  // Reserved must be all-zero
	}
	for _, msg := range invalidUpload {
		if err := validateClientFrame(msg); err == nil {
			t.Errorf("invalid upload message %+v accepted", msg)
		}
	}

	if err := validateDownloadRequest(&Msg{Op: uint8(OpDownReq)}); err != nil {
		t.Fatalf("valid download request rejected: %v", err)
	}
	invalidDownload := []*Msg{
		{Op: uint8(OpFileDone)},                       // wrong op for the download direction
		{Op: uint8(OpDownReq), Flags: FlagCompressed}, // request can't carry flags
		{Op: uint8(OpDownReq), DataSize: 1},           // request can't carry a payload
		{Op: uint8(OpDownReq), Reserved: [5]byte{1}},  // Reserved must be all-zero
	}
	for _, msg := range invalidDownload {
		if err := validateDownloadRequest(msg); err == nil {
			t.Errorf("invalid download request %+v accepted", msg)
		}
	}
}

func TestAnnounceBatchRoundTrip(t *testing.T) {
	recs := []AnnounceRec{
		{FileID: 0, FileSize: 12345, Mode: 0644, MTimeSec: 1700000000, MTimeNs: 123456789, Kind: KindFile, Path: "a/b/c.txt"},
		{FileID: 1, Mode: 0755, Kind: KindDir, Path: "a/b"},
		{FileID: 2, Kind: KindSkipped, Path: "a/symlink"},
		{FileID: 3, Kind: KindFile, Path: "ünïcödé/файл with spaces.bin"},
		{FileID: 4, Kind: KindFile, Path: ""}, // encoder doesn't validate paths; that's sanitizeRelPath's job
		{FileID: 5, Kind: KindSymlink, Path: "a/rel-link", LinkTarget: "b/c.txt"},
		{FileID: 6, Kind: KindSymlink, Path: "abs-link", LinkTarget: "/etc/hostname"},
		{FileID: 7, Kind: KindSymlink, Path: "weird-link", LinkTarget: "тарget with spaces/../x"},
	}
	encoded, err := encodeAnnounceBatch(recs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeAnnounceBatch(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(recs) {
		t.Fatalf("got %d records, want %d", len(decoded), len(recs))
	}
	for i := range recs {
		if decoded[i] != recs[i] {
			t.Errorf("record %d mismatch: got %+v, want %+v", i, decoded[i], recs[i])
		}
	}
}

func TestAnnounceBatchEmpty(t *testing.T) {
	encoded, err := encodeAnnounceBatch(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeAnnounceBatch(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 0 {
		t.Errorf("got %d records, want 0", len(decoded))
	}
}

func TestDecodeAnnounceBatchRejectsTruncated(t *testing.T) {
	// A symlink record (with a target trailer) so every byte of the
	// extended ver0.04 layout -- including TargetLen and the target bytes
	// -- is covered by the per-byte truncation sweep.
	encoded, err := encodeAnnounceBatch([]AnnounceRec{{FileID: 1, Kind: KindSymlink, Path: "a/b.txt", LinkTarget: "c/d.txt"}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for cut := 0; cut < len(encoded); cut++ {
		if _, err := decodeAnnounceBatch(encoded[:cut]); err == nil {
			t.Errorf("truncated to %d/%d bytes was unexpectedly accepted", cut, len(encoded))
		}
	}
}

func TestDecodeAnnounceBatchRejectsAbsurdCount(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, announceBatchMaxRecs+1)
	if _, err := decodeAnnounceBatch(buf); err == nil {
		t.Error("a record count over the sanity limit was unexpectedly accepted")
	}
}

func TestSkipListRoundTrip(t *testing.T) {
	ids := []uint32{0, 1, 7, 1000000, 4294967295}
	encoded, err := encodeSkipList(ids)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeSkipList(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(ids) {
		t.Fatalf("got %d ids, want %d", len(decoded), len(ids))
	}
	for i := range ids {
		if decoded[i] != ids[i] {
			t.Errorf("id %d mismatch: got %d, want %d", i, decoded[i], ids[i])
		}
	}
}

func TestSkipListEmpty(t *testing.T) {
	encoded, err := encodeSkipList(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeSkipList(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 0 {
		t.Errorf("got %d ids, want 0", len(decoded))
	}
}

func TestDecodeSkipListRejectsTruncated(t *testing.T) {
	encoded, err := encodeSkipList([]uint32{1, 2, 3})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for cut := 0; cut < len(encoded); cut++ {
		if _, err := decodeSkipList(encoded[:cut]); err == nil {
			t.Errorf("truncated to %d/%d bytes was unexpectedly accepted", cut, len(encoded))
		}
	}
}

func TestDecodeSkipListRejectsAbsurdCount(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, skipListMaxRecs+1)
	if _, err := decodeSkipList(buf); err == nil {
		t.Error("a count over the sanity limit was unexpectedly accepted")
	}
}

func TestValidateDirClientFrameAnnounceFlags(t *testing.T) {
	valid := []*Msg{
		{Op: uint8(OpAnnounce), DataSize: 1},
		{Op: uint8(OpAnnounce), DataSize: 1, Flags: FlagQuickCheck},
	}
	for _, msg := range valid {
		if err := validateDirClientFrame(msg); err != nil {
			t.Errorf("valid announce message %+v rejected: %v", msg, err)
		}
	}
	invalid := []*Msg{
		{Op: uint8(OpAnnounce), DataSize: 1, Flags: FlagCompressed},
		{Op: uint8(OpAnnounce), DataSize: 1, Flags: FlagQuickCheck | FlagCompressed},
	}
	for _, msg := range invalid {
		if err := validateDirClientFrame(msg); err == nil {
			t.Errorf("invalid announce message %+v accepted", msg)
		}
	}
}

func TestValidateDirClientFrameTreeDoneAndAnnounceEndFlags(t *testing.T) {
	valid := []*Msg{
		{Op: uint8(OpTreeDone)},
		{Op: uint8(OpAnnounceEnd)},
		// FlagDelete rides on OpAnnounceEnd (delete-before-transfer): the
		// delete pass runs the moment the announce stream is complete,
		// before any file content moves.
		{Op: uint8(OpAnnounceEnd), Flags: FlagDelete},
	}
	for _, msg := range valid {
		if err := validateDirClientFrame(msg); err != nil {
			t.Errorf("valid message %+v rejected: %v", msg, err)
		}
	}
	invalid := []*Msg{
		// OpTreeDone never accepts flags -- FlagDelete moved to
		// OpAnnounceEnd when -delete became delete-before.
		{Op: uint8(OpTreeDone), Flags: FlagDelete},
		{Op: uint8(OpTreeDone), Flags: FlagQuickCheck},
		{Op: uint8(OpTreeDone), DataSize: 1},
		{Op: uint8(OpAnnounceEnd), Flags: FlagQuickCheck},
		{Op: uint8(OpAnnounceEnd), Flags: FlagDelete | FlagCompressed},
		{Op: uint8(OpAnnounceEnd), DataSize: 1},
	}
	for _, msg := range invalid {
		if err := validateDirClientFrame(msg); err == nil {
			t.Errorf("invalid message %+v accepted", msg)
		}
	}
}

// TestPackMatchesBinaryWrite pins the hand-rolled encoder/decoder to the
// exact byte layout encoding/binary produced for Msg, so replacing the
// reflective binary.Write/Read (a per-frame reflection walk plus a
// bytes.Buffer allocation, several times per block) could not change the
// wire format.
func TestPackMatchesBinaryWrite(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		m := &Msg{
			Op:        uint8(r.Intn(256)),
			Flags:     uint8(r.Intn(256)),
			FileID:    r.Uint32(),
			BlockIdx:  r.Uint32(),
			BlockSize: r.Uint32(),
			FileSize:  r.Uint64(),
			DataSize:  r.Uint32(),
		}
		r.Read(m.MagicHead[:])
		r.Read(m.Reserved[:])

		got, err := pack(m)
		if err != nil {
			t.Fatalf("pack: %v", err)
		}
		var want bytes.Buffer
		if err := binary.Write(&want, binary.LittleEndian, m); err != nil {
			t.Fatalf("binary.Write: %v", err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("pack layout differs from binary.Write:\n got %x\nwant %x", got, want.Bytes())
		}

		back, err := unpack(got)
		if err != nil {
			t.Fatalf("unpack: %v", err)
		}
		if *back != *m {
			t.Fatalf("unpack round trip: got %+v, want %+v", *back, *m)
		}
		ref := &Msg{}
		if err := binary.Read(bytes.NewReader(got), binary.LittleEndian, ref); err != nil {
			t.Fatalf("binary.Read: %v", err)
		}
		if *ref != *back {
			t.Fatalf("unpack differs from binary.Read: got %+v, want %+v", *back, *ref)
		}
	}
}

// --- OpAnnounceAck: skip list + announce-failed list ---

// TestAnnounceAckRoundTrip covers the two-section trailer: both lists
// present, and the wire-compat case where nothing failed (only the
// skip-list section is emitted, exactly as before the failed list existed).
func TestAnnounceAckRoundTrip(t *testing.T) {
	cases := []struct {
		name            string
		skipped, failed []uint32
	}{
		{"both", []uint32{1, 2, 900}, []uint32{7, 8}},
		{"only skipped", []uint32{3, 4}, nil},
		{"only failed", nil, []uint32{5}},
		{"neither", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeAnnounceAck(tc.skipped, tc.failed)
			if err != nil {
				t.Fatalf("encodeAnnounceAck: %v", err)
			}
			skipped, failed, err := decodeAnnounceAck(encoded)
			if err != nil {
				t.Fatalf("decodeAnnounceAck: %v", err)
			}
			assertUint32SlicesEqual(t, skipped, tc.skipped)
			assertUint32SlicesEqual(t, failed, tc.failed)
		})
	}
}

// TestAnnounceAckWithNoFailuresIsByteIdenticalToSkipList pins the
// compatibility property the encoder relies on: the overwhelmingly common
// ack (nothing failed) is exactly the bytes encodeSkipList always produced,
// so the second section costs nothing on a healthy run and decodeSkipList
// still reads such a trailer.
func TestAnnounceAckWithNoFailuresIsByteIdenticalToSkipList(t *testing.T) {
	ids := []uint32{11, 22, 33}
	withFailed, err := encodeAnnounceAck(ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	skipOnly, err := encodeSkipList(ids)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withFailed, skipOnly) {
		t.Errorf("encodeAnnounceAck(ids, nil) = %v, want byte-identical to encodeSkipList(ids) = %v", withFailed, skipOnly)
	}
	decoded, err := decodeSkipList(withFailed)
	if err != nil {
		t.Fatalf("decodeSkipList on a no-failures ack: %v", err)
	}
	assertUint32SlicesEqual(t, decoded, ids)
}

// TestDecodeAnnounceAckRejectsTrailingGarbage: a trailer with bytes beyond
// the two sections is a corrupt/hostile frame, not something to silently
// ignore.
func TestDecodeAnnounceAckRejectsTrailingGarbage(t *testing.T) {
	encoded, err := encodeAnnounceAck([]uint32{1}, []uint32{2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeAnnounceAck(append(encoded, 0xFF)); err == nil {
		t.Error("decodeAnnounceAck accepted a trailer with trailing bytes")
	}
}

// TestDecodeAnnounceAckRejectsTruncatedFailedSection: every prefix that cuts
// into the second section must error rather than return a partial list.
func TestDecodeAnnounceAckRejectsTruncatedFailedSection(t *testing.T) {
	encoded, err := encodeAnnounceAck([]uint32{1, 2}, []uint32{3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	skipLen := 4 + 2*4 // count + 2 ids
	for cut := skipLen + 1; cut < len(encoded); cut++ {
		if _, _, err := decodeAnnounceAck(encoded[:cut]); err == nil {
			t.Errorf("decodeAnnounceAck accepted a trailer truncated to %d bytes", cut)
		}
	}
}

func assertUint32SlicesEqual(t *testing.T, got, want []uint32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v, want %v", i, got, want)
		}
	}
}
