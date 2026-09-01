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
//
// ver0.04 extends the OpAnnounce trailer's record layout with a LinkTarget
// field (see encodeAnnounceBatch) for -links symlink syncing. Bumped so a
// version-mismatched pair fails with a clean "invalid magic" instead of a
// garbled trailer decode.
//
// ver0.06 replaces the block hash (FNV-128a -> SHA-256 truncated to 16
// bytes). The digest WIDTH is unchanged, so nothing about the frame layout
// moves -- but the bytes two endpoints compare no longer agree across the
// change, and a mismatched pair would silently conclude every block differs
// and retransmit the entire tree. Bumped so they fail up front instead.
//
// ver0.05 adds the KindSpecial record kind (-specials FIFO/socket syncing).
// The record LAYOUT is unchanged -- the node type rides in the existing Mode
// field -- but an older server rejects the unknown kind mid-stream and kills
// the control connection, so the version is bumped to turn that into the
// same clean up-front "invalid magic" failure.
var magicHead = "blockSync-ver0.06"

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
	OpAnnounceAck Op = 25 // response to an OpAnnounce carrying FlagQuickCheck;
	// trailer: a skip-list of FileIDs the destination already has (see
	// encodeSkipList) -- sent even when the list is empty, since the
	// client blocks on exactly one reply per quick-check batch.
	OpAnnounceEndAck Op = 26 // response to an OpAnnounceEnd carrying
	// FlagDelete: the destination's delete pass (deleteExtraneous) has
	// completed, so it is now safe for the client to start transferring
	// file content. BlockIdx carries the removed-entry count (informational).
	// Never sent for a plain OpAnnounceEnd -- without FlagDelete the control
	// stream stays strictly one-way until OpTreeAck.
)

const (
	// FlagCompressed marks an OpBlockData/OpDownData payload as zstd-compressed.
	FlagCompressed uint8 = 1 << 0
	// FlagZero marks an OpDownData reply as a zero block: DataSize is 0 and
	// no payload follows. (The upload direction has a dedicated OpBlockZero
	// instead, since the client always initiates that write.)
	FlagZero uint8 = 1 << 1
	// FlagNoDest is reserved for directory-tree mode (not yet implemented).
	FlagNoDest uint8 = 1 << 2
	// FlagCreated marks an OpFileAck reply as being for a file the
	// destination created fresh (as opposed to one that already existed).
	FlagCreated uint8 = 1 << 3
	// FlagDelete marks an OpAnnounceEnd as requesting a `-delete` pass:
	// with the announce stream now complete (which is exactly what
	// OpAnnounceEnd means), the destination sweeps its own root for
	// anything not among the announced files/directories/skipped-
	// placeholders, removes it (see fileTable.deleteExtraneous), and
	// replies OpAnnounceEndAck -- BEFORE the client transfers any file
	// content (rsync's --delete-before), so a full destination is cleared
	// of stale data before new data needs the space. Purely a wire-level,
	// per-tree signal -- the destination server needs no matching CLI flag.
	FlagDelete uint8 = 1 << 4
	// FlagErrPermanent marks an OpError reply as a PERMANENT rejection:
	// retrying the request can never change the outcome (e.g. OpFileOpen
	// for a file the destination has already finalized -- including one
	// that failed at announce time). The client fails the affected file
	// immediately instead of burning its retry budget. Its absence means
	// the rejection may be transient -- most importantly "file was never
	// announced", which the pipeline legitimately produces when a worker's
	// open races ahead of the announce batch still buffering client-side
	// -- and the client retries exactly as it always has.
	FlagErrPermanent uint8 = 1 << 6
	// FlagQuickCheck marks an OpAnnounce batch as requesting rsync-style
	// "quick check" evaluation: the destination stats each announced
	// KindFile entry and, if an existing file there already matches its
	// size and modification time (whole-second precision), reports it back
	// in an OpAnnounceAck skip-list instead of accepting block traffic for
	// it. Purely a wire-level, per-batch signal -- the destination server
	// needs no matching CLI flag.
	FlagQuickCheck uint8 = 1 << 5
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
		if msg.Flags&^FlagQuickCheck != 0 {
			return fmt.Errorf("invalid announce message flags")
		}
		if msg.DataSize == 0 {
			return fmt.Errorf("invalid announce message: DataSize must be nonzero")
		}
		if msg.DataSize > dirAnnounceMaxDataSize {
			return fmt.Errorf("invalid announce message: DataSize %d exceeds sanity limit %d", msg.DataSize, dirAnnounceMaxDataSize)
		}
		return nil
	case OpAnnounceEnd:
		if msg.DataSize != 0 || msg.Flags&^FlagDelete != 0 {
			return fmt.Errorf("invalid announce-end message flags")
		}
		return nil
	case OpTreeDone:
		if msg.DataSize != 0 || msg.Flags != 0 {
			return fmt.Errorf("invalid tree-done message flags")
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

// pack encodes m into its 48-byte little-endian wire form. Hand-rolled
// rather than binary.Write: Msg contains fixed-size arrays, which sends
// encoding/binary down its reflection path, and a bytes.Buffer was
// allocated per call -- for something done three to six times per block (or
// per small file) on each side. The layout is byte-for-byte identical to
// what binary.Write produced (TestPackMatchesBinaryWrite pins that), so the
// wire format is unchanged. The error return is kept so the many call sites
// need no change; it is always nil.
func pack(m *Msg) ([]byte, error) {
	buf := make([]byte, msgSize)
	m.encode(buf)
	return buf, nil
}

// encode writes m's wire form into buf, which must be at least msgSize long.
func (m *Msg) encode(buf []byte) {
	_ = buf[msgSize-1]
	copy(buf[0:17], m.MagicHead[:])
	buf[17] = m.Op
	buf[18] = m.Flags
	copy(buf[19:24], m.Reserved[:])
	binary.LittleEndian.PutUint32(buf[24:28], m.FileID)
	binary.LittleEndian.PutUint32(buf[28:32], m.BlockIdx)
	binary.LittleEndian.PutUint32(buf[32:36], m.BlockSize)
	binary.LittleEndian.PutUint64(buf[36:44], m.FileSize)
	binary.LittleEndian.PutUint32(buf[44:48], m.DataSize)
}

// unpack decodes one wire header. A buffer shorter than msgSize is an
// error, exactly as binary.Read reported it (io.ErrUnexpectedEOF).
func unpack(dataBytes []byte) (*Msg, error) {
	if len(dataBytes) < msgSize {
		return nil, io.ErrUnexpectedEOF
	}
	m := &Msg{}
	m.decode(dataBytes)
	return m, nil
}

// decode is encode's inverse; buf must be at least msgSize long.
func (m *Msg) decode(buf []byte) {
	_ = buf[msgSize-1]
	copy(m.MagicHead[:], buf[0:17])
	m.Op = buf[17]
	m.Flags = buf[18]
	copy(m.Reserved[:], buf[19:24])
	m.FileID = binary.LittleEndian.Uint32(buf[24:28])
	m.BlockIdx = binary.LittleEndian.Uint32(buf[28:32])
	m.BlockSize = binary.LittleEndian.Uint32(buf[32:36])
	m.FileSize = binary.LittleEndian.Uint64(buf[36:44])
	m.DataSize = binary.LittleEndian.Uint32(buf[44:48])
}

// readMsg reads and unpacks exactly one Msg header from r: the
// make([]byte, msgSize) / io.ReadFull / unpack sequence repeated at every
// ONE-SHOT read site in this codebase (waiting for a single ack, a single
// hash header, a single metadata probe reply). A persistent per-connection
// read loop that reuses one buffer across many iterations instead
// (server.go's serverHandleReq, server_dir.go's dirHandleConn) intentionally
// does NOT use this — that reuse avoids a fresh allocation per message in a
// hot path, which this helper, allocating fresh every call, would undo.
func readMsg(r io.Reader) (*Msg, error) {
	buf := make([]byte, msgSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return unpack(buf)
}

// AnnounceRec is one record inside an OpAnnounce trailer: a file, directory,
// symlink, special file, or skipped path discovered by the walker. Kind
// reuses walk.go's EntryKind. KindSkipped entries (an excluded path, and --
// unless -links/-specials asked for them -- symlinks, sockets, FIFOs and
// devices) are announced without being created, so a later --delete pass
// knows not to remove their destination-side namesakes if any exist.
type AnnounceRec struct {
	FileID   uint32
	FileSize uint64
	Mode     uint32
	MTimeSec int64
	MTimeNs  int32
	Kind     EntryKind
	Path     string // '/'-separated, sanitized, relative to the source root
	// LinkTarget is a KindSymlink record's target string, verbatim (see
	// FileEntry.LinkTarget); empty for every other kind.
	LinkTarget string
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
// Path(PathLen) TargetLen(2) LinkTarget(TargetLen), all little-endian.
// TargetLen is 0 for every kind but KindSymlink (2 bytes/record overhead
// keeps the codec branch-free; added in ver0.04).
func encodeAnnounceBatch(recs []AnnounceRec) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(recs))); err != nil {
		return nil, fmt.Errorf("encode announce count: %w", err)
	}
	for i, r := range recs {
		if len(r.Path) > 0xFFFF {
			return nil, fmt.Errorf("record %d: path is %d bytes, exceeds the uint16 length field", i, len(r.Path))
		}
		if len(r.LinkTarget) > 0xFFFF {
			return nil, fmt.Errorf("record %d: link target is %d bytes, exceeds the uint16 length field", i, len(r.LinkTarget))
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
		if err := binary.Write(buf, binary.LittleEndian, uint16(len(r.LinkTarget))); err != nil {
			return nil, fmt.Errorf("encode record %d target length: %w", i, err)
		}
		if _, err := buf.WriteString(r.LinkTarget); err != nil {
			return nil, fmt.Errorf("encode record %d link target: %w", i, err)
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
		var targetLen uint16
		if err := binary.Read(buf, binary.LittleEndian, &targetLen); err != nil {
			return nil, fmt.Errorf("decode record %d target length: %w", i, err)
		}
		if targetLen > 0 {
			targetBuf := make([]byte, targetLen)
			if _, err := io.ReadFull(buf, targetBuf); err != nil {
				return nil, fmt.Errorf("decode record %d link target: %w", i, err)
			}
			r.LinkTarget = string(targetBuf)
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// skipListMaxRecs bounds decodeSkipList's claimed count the same way
// announceBatchMaxRecs bounds decodeAnnounceBatch's: a skip list can never
// legitimately exceed the batch that provoked it (at most announceBatchSize
// records), so this is already generous headroom against a corrupted or
// hostile count driving a huge allocation.
const skipListMaxRecs = announceBatchMaxRecs

// encodeSkipList serializes an OpAnnounceAck trailer: a uint32 count
// followed by that many little-endian uint32 FileIDs -- the destination's
// quick-check skip decision for a just-received OpAnnounce batch.
func encodeSkipList(ids []uint32) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, uint32(len(ids))); err != nil {
		return nil, fmt.Errorf("encode skip-list count: %w", err)
	}
	for i, id := range ids {
		if err := binary.Write(buf, binary.LittleEndian, id); err != nil {
			return nil, fmt.Errorf("encode skip-list entry %d: %w", i, err)
		}
	}
	return buf.Bytes(), nil
}

// decodeSkipList is encodeSkipList's inverse, with the same
// never-trust-the-claimed-count-before-validating posture as
// decodeAnnounceBatch. It reads only the FIRST section, so it is also the
// skip-list half of decodeAnnounceAck.
func decodeSkipList(b []byte) ([]uint32, error) {
	ids, _, err := readIDSection(b, "skip list")
	return ids, err
}

// readIDSection decodes one [uint32 count][count x uint32 id] section from
// the head of b and returns the ids plus the remaining bytes.
func readIDSection(b []byte, what string) (ids []uint32, rest []byte, err error) {
	buf := bytes.NewReader(b)
	var count uint32
	if err := binary.Read(buf, binary.LittleEndian, &count); err != nil {
		return nil, nil, fmt.Errorf("decode %s count: %w", what, err)
	}
	if count > skipListMaxRecs {
		return nil, nil, fmt.Errorf("%s claims %d entries, exceeds sanity limit %d", what, count, skipListMaxRecs)
	}
	ids = make([]uint32, count)
	for i := range ids {
		if err := binary.Read(buf, binary.LittleEndian, &ids[i]); err != nil {
			return nil, nil, fmt.Errorf("decode %s entry %d: %w", what, i, err)
		}
	}
	return ids, b[len(b)-buf.Len():], nil
}

// encodeAnnounceAck serializes the full OpAnnounceAck trailer: the
// quick-check skip list, then the list of entries whose announce FAILED
// environmentally on the destination. Two independent sections rather than
// one tagged list because they mean opposite things -- a skipped file is
// already correct at the destination, a failed one will never exist there.
func encodeAnnounceAck(skipped, failed []uint32) ([]byte, error) {
	head, err := encodeSkipList(skipped)
	if err != nil {
		return nil, err
	}
	// Omit the second section entirely when empty: the overwhelmingly
	// common case is zero announce failures, and decodeAnnounceAck treats a
	// missing section as empty, so the ordinary ack stays exactly the
	// bytes it has always been.
	if len(failed) == 0 {
		return head, nil
	}
	tail, err := encodeSkipList(failed)
	if err != nil {
		return nil, fmt.Errorf("encode announce-failed list: %w", err)
	}
	return append(head, tail...), nil
}

// decodeAnnounceAck is encodeAnnounceAck's inverse. A trailer carrying only
// the skip-list section (nothing failed) decodes with an empty failed list.
func decodeAnnounceAck(b []byte) (skipped, failed []uint32, err error) {
	skipped, rest, err := readIDSection(b, "skip list")
	if err != nil {
		return nil, nil, err
	}
	if len(rest) == 0 {
		return skipped, nil, nil
	}
	failed, rest, err = readIDSection(rest, "announce-failed list")
	if err != nil {
		return nil, nil, err
	}
	if len(rest) != 0 {
		return nil, nil, fmt.Errorf("announce ack has %d trailing byte(s)", len(rest))
	}
	return skipped, failed, nil
}
