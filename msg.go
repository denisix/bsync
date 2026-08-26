package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const magicLen = 17

// magicHead is a var, not a const, so tests can construct/compare against a
// foreign or older magic value (e.g. to exercise a version-mismatch path)
// without a second build.
//
// ver0.03 replaces the old implicit message "kind" (DataSize==0 plus three
// bool flags) with an explicit Op byte and a Flags byte, and adds a FileID
// field. Single-file transfers (the only mode implemented so far) always use
// FileID 0; a future directory-tree mode reuses the same header with
// non-zero FileIDs and the currently-unused announce/tree opcodes below.
// README documents that both endpoints must be built from the same commit,
// so there is no v0.02 interop path.
var magicHead = "blockSync-ver0.03"

// Op identifies the kind of a message. Reserved is 5 zero bytes so a future
// field can be added without bumping magicHead again.
type Op uint8

const (
	// client -> server (upload: client is the source, server is the destination)
	OpBlockCheck Op = 4 // checksum request for (FileID, BlockIdx); DataSize==0
	OpBlockZero  Op = 5 // write an all-zero block; no payload
	OpBlockData  Op = 6 // write payload; DataSize bytes follow
	OpFileDone   Op = 8 // upload complete (single-file mode: also ends the transfer)

	// server -> client (upload)
	OpHash Op = 20 // trailer: a 16-byte digest
	OpAck  Op = 21 // echoes FileID + BlockIdx; write accepted

	// client -> server (download: client is the destination, server is the source)
	OpDownReq Op = 40 // request; reused for the metadata probe (BlockIdx=0,
	// FileSize=0) and for ordinary block requests
	// server -> client (download)
	OpDownData Op = 41 // a block (DataSize>0), or with DataSize==0 on the very
	// first reply, file metadata (BlockSize/FileSize only)
	OpDownDone Op = 42 // past the last block, or an empty source

	// Directory-tree mode (not yet implemented): reserved so the opcode
	// space is settled ahead of time and doesn't require another magic bump.
	OpAnnounce    Op = 1  // trailer: a batch of announce records (control conn)
	OpAnnounceEnd Op = 2  // walk complete; BlockIdx=fileCount, FileSize=totalBytes
	OpFileOpen    Op = 3  // warm-up hint: about to send blocks for FileID
	OpFileClose   Op = 7  // done sending blocks for FileID on this connection
	OpTreeDone    Op = 9  // end of tree (control conn)
	OpFileAck     Op = 22 // response to OpFileOpen
	OpTreeAck     Op = 23 // destination is consistent; safe to exit
	OpError       Op = 24 // trailer: a UTF-8 error message
)

const (
	// FlagCompressed marks an OpBlockData/OpDownData payload as zstd-compressed.
	FlagCompressed uint8 = 1 << 0
	// FlagZero marks an OpDownData reply as a zero block: DataSize is 0 and
	// no payload follows. (The upload direction has a dedicated OpBlockZero
	// instead, since the client always initiates that write.)
	FlagZero uint8 = 1 << 1
	// FlagNoDest and FlagCreated and FlagDelete are reserved for
	// directory-tree mode (not yet implemented).
	FlagNoDest  uint8 = 1 << 2
	FlagCreated uint8 = 1 << 3
	FlagDelete  uint8 = 1 << 4
)

const msgSize = 48

type Msg struct {
	MagicHead [magicLen]byte // 17
	Op        uint8          //  1
	Flags     uint8          //  1
	Reserved  [5]byte        //  5  must be zero
	FileID    uint32         //  4  0 = the implicit single file
	BlockIdx  uint32         //  4
	BlockSize uint32         //  4
	FileSize  uint64         //  8
	DataSize  uint32         //  4  payload bytes following this header
} // binary.Size == msgSize (48), little-endian, no padding

func blockPayloadSize(fileSize uint64, blockSize uint32, blockIdx uint32) uint32 {
	if blockSize == 0 {
		return 0
	}
	offset := uint64(blockIdx) * uint64(blockSize)
	if offset >= fileSize {
		return 0
	}
	remaining := fileSize - offset
	if remaining < uint64(blockSize) {
		return uint32(remaining)
	}
	return blockSize
}

var zeroReserved [5]byte

// validateClientFrame validates a message received by the upload server (the
// destination) from the upload client (the source). It is the Op-based
// successor to the old validateUploadMessage, which inferred a message's
// kind from DataSize plus three bool flags; that kind is now the explicit
// Op field, so each case only needs to reject flags/sizes that don't belong
// to it.
func validateClientFrame(msg *Msg) error {
	if msg.Reserved != zeroReserved {
		return fmt.Errorf("invalid message: reserved bytes must be zero")
	}
	switch Op(msg.Op) {
	case OpFileDone:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid done message flags")
		}
		return nil
	case OpBlockZero:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid zero message flags")
		}
		return nil
	case OpBlockCheck:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid checksum request flags")
		}
		return nil
	case OpBlockData:
		if msg.DataSize == 0 {
			return fmt.Errorf("invalid data message: DataSize must be nonzero")
		}
		if msg.Flags&^FlagCompressed != 0 {
			return fmt.Errorf("invalid data message flags")
		}
		return nil
	default:
		return fmt.Errorf("unexpected client op %d", msg.Op)
	}
}

const (
	// dirAnnounceMaxDataSize sanity-bounds an OpAnnounce trailer's claimed
	// DataSize before handleAnnounce allocates a buffer for it: the batcher
	// caps a real batch at announceBatchSize (200) records, and even a
	// worst-case record (the maximum 0xFFFF-byte path encodeAnnounceBatch
	// allows) is a few tens of KB, so a legitimate batch never gets close
	// to this; it exists to stop a corrupted or hostile DataSize (up to
	// 4 GiB, being a uint32) from causing a multi-gigabyte allocation.
	dirAnnounceMaxDataSize = 64 << 20 // 64 MiB
	// dirBlockSizeMax sanity-bounds BlockSize on every directory-mode
	// opcode that carries one. Generous relative to any real -b value
	// (10 MiB default, users occasionally go much higher for huge files)
	// so it never rejects a legitimate transfer; it exists only to stop a
	// corrupted or hostile BlockSize (also a uint32, up to 4 GiB) from
	// sizing a destination-side read/write buffer absurdly large.
	dirBlockSizeMax = 1 << 30 // 1 GiB
)

// validateDirClientFrame validates a message received by the directory-mode
// destination server from the directory-mode client, covering the opcode
// set validateClientFrame (the legacy single-file validator) does not:
// OpAnnounce, OpAnnounceEnd, OpFileOpen, OpBlockCheck, OpBlockZero,
// OpBlockData, OpFileClose, OpFileDone, OpTreeDone. Same "reject rather
// than repair" posture as validateClientFrame, plus explicit sanity caps on
// wire-driven allocation sizes (DataSize, BlockSize) that dirHandleConn
// previously trusted unconditionally.
func validateDirClientFrame(msg *Msg) error {
	if msg.Reserved != zeroReserved {
		return fmt.Errorf("invalid message: reserved bytes must be zero")
	}
	blockSizeOp := func(name string) error {
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid %s message flags", name)
		}
		if msg.BlockSize == 0 {
			return fmt.Errorf("invalid %s message: BlockSize must be nonzero", name)
		}
		if msg.BlockSize > dirBlockSizeMax {
			return fmt.Errorf("invalid %s message: BlockSize %d exceeds sanity limit %d", name, msg.BlockSize, dirBlockSizeMax)
		}
		return nil
	}
	switch Op(msg.Op) {
	case OpAnnounce:
		if msg.Flags != 0 {
			return fmt.Errorf("invalid announce message flags")
		}
		if msg.DataSize == 0 {
			return fmt.Errorf("invalid announce message: DataSize must be nonzero")
		}
		if msg.DataSize > dirAnnounceMaxDataSize {
			return fmt.Errorf("invalid announce message: DataSize %d exceeds sanity limit %d", msg.DataSize, dirAnnounceMaxDataSize)
		}
		return nil
	case OpAnnounceEnd, OpTreeDone:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid announce-end/tree-done message flags")
		}
		return nil
	case OpFileOpen:
		return blockSizeOp("file-open")
	case OpBlockCheck:
		return blockSizeOp("checksum request")
	case OpBlockZero:
		return blockSizeOp("zero-write")
	case OpBlockData:
		if msg.DataSize == 0 {
			return fmt.Errorf("invalid data message: DataSize must be nonzero")
		}
		if msg.Flags&^FlagCompressed != 0 {
			return fmt.Errorf("invalid data message flags")
		}
		if msg.BlockSize == 0 {
			return fmt.Errorf("invalid data message: BlockSize must be nonzero")
		}
		if msg.BlockSize > dirBlockSizeMax {
			return fmt.Errorf("invalid data message: BlockSize %d exceeds sanity limit %d", msg.BlockSize, dirBlockSizeMax)
		}
		if uint64(msg.DataSize) > uint64(msg.BlockSize)+uint64(encryptOverhead) {
			return fmt.Errorf("invalid data message: DataSize %d exceeds BlockSize+overhead", msg.DataSize)
		}
		return nil
	case OpFileClose, OpFileDone:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid file-close/file-done message flags")
		}
		return nil
	default:
		return fmt.Errorf("unexpected directory client op %d", msg.Op)
	}
}

// validateDownloadRequest validates a message received by the download
// server (the source) from the download client (the destination). Every
// download request, whether the initial metadata probe or an ordinary block
// request, is OpDownReq with no payload; the two roles are distinguished by
// field values (BlockIdx==0 && FileSize==0), exactly as before.
func validateDownloadRequest(msg *Msg) error {
	if msg.Reserved != zeroReserved {
		return fmt.Errorf("invalid download request: reserved bytes must be zero")
	}
	if Op(msg.Op) != OpDownReq {
		return fmt.Errorf("unexpected download client op %d", msg.Op)
	}
	if msg.DataSize != 0 || msg.Flags != 0 {
		return fmt.Errorf("invalid download request flags")
	}
	return nil
}

func stringToFixedSizeArray(s string) [magicLen]byte {
	var arr [magicLen]byte
	copy(arr[:], s)
	return arr
}

func pack(data *Msg) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unpack(dataBytes []byte) (*Msg, error) {
	data := &Msg{}
	buf := bytes.NewReader(dataBytes)
	if err := binary.Read(buf, binary.LittleEndian, data); err != nil {
		return nil, err
	}
	return data, nil
}

// AnnounceRec is one record inside an OpAnnounce trailer: a file, directory,
// or skipped path discovered by the walker. Kind reuses walk.go's EntryKind
// (KindFile/KindDir/KindSkipped) — kindSkipped entries (symlinks, sockets,
// FIFOs, devices) are announced without being created, so a later --delete
// pass knows not to remove their destination-side namesakes if any exist.
type AnnounceRec struct {
	FileID   uint32
	FileSize uint64
	Mode     uint32
	MTimeSec int64
	MTimeNs  int32
	Kind     EntryKind
	Path     string // '/'-separated, sanitized, relative to the source root
}

// announceBatchMaxRecs bounds decodeAnnounceBatch's record count against a
// corrupt or hostile claim that would otherwise drive a huge allocation
// before any record is actually validated. Real batches are expected to
// hold at most a few hundred records (see the client-side announce
// batcher); a million is already generous headroom.
const announceBatchMaxRecs = 1_000_000

// encodeAnnounceBatch serializes recs into the trailer bytes that follow an
// OpAnnounce header's DataSize: a uint32 count, then each record as
// FileID(4) FileSize(8) Mode(4) MTimeSec(8) MTimeNs(4) Kind(1) PathLen(2)
// Path(PathLen), all little-endian.
func encodeAnnounceBatch(recs []AnnounceRec) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(recs))); err != nil {
		return nil, fmt.Errorf("encode announce count: %w", err)
	}
	for i, r := range recs {
		if len(r.Path) > 0xFFFF {
			return nil, fmt.Errorf("record %d: path is %d bytes, exceeds the uint16 length field", i, len(r.Path))
		}
		fields := []any{r.FileID, r.FileSize, r.Mode, r.MTimeSec, r.MTimeNs, uint8(r.Kind), uint16(len(r.Path))}
		for _, f := range fields {
			if err := binary.Write(buf, binary.LittleEndian, f); err != nil {
				return nil, fmt.Errorf("encode record %d: %w", i, err)
			}
		}
		if _, err := buf.WriteString(r.Path); err != nil {
			return nil, fmt.Errorf("encode record %d path: %w", i, err)
		}
	}
	return buf.Bytes(), nil
}

// decodeAnnounceBatch is encodeAnnounceBatch's inverse. It never trusts the
// claimed count to size an allocation beyond announceBatchMaxRecs, and every
// short read is a hard error — the trailer is either well-formed in full or
// rejected, never partially accepted.
func decodeAnnounceBatch(b []byte) ([]AnnounceRec, error) {
	buf := bytes.NewReader(b)
	var count uint32
	if err := binary.Read(buf, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("decode announce count: %w", err)
	}
	if count > announceBatchMaxRecs {
		return nil, fmt.Errorf("announce batch claims %d records, exceeds sanity limit %d", count, announceBatchMaxRecs)
	}
	recs := make([]AnnounceRec, 0, count)
	for i := uint32(0); i < count; i++ {
		var r AnnounceRec
		var kind uint8
		var pathLen uint16
		for _, f := range []any{&r.FileID, &r.FileSize, &r.Mode, &r.MTimeSec, &r.MTimeNs, &kind, &pathLen} {
			if err := binary.Read(buf, binary.LittleEndian, f); err != nil {
				return nil, fmt.Errorf("decode record %d: %w", i, err)
			}
		}
		r.Kind = EntryKind(kind)
		pathBuf := make([]byte, pathLen)
		if _, err := io.ReadFull(buf, pathBuf); err != nil {
			return nil, fmt.Errorf("decode record %d path: %w", i, err)
		}
		r.Path = string(pathBuf)
		recs = append(recs, r)
	}
	return recs, nil
}
