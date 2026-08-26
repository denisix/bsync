package main

import (
	"encoding/binary"
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
	encoded, err := encodeAnnounceBatch([]AnnounceRec{{FileID: 1, Kind: KindFile, Path: "a/b.txt"}})
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
