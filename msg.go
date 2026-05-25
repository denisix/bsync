package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const magicLen = 17
const magicHead = "blockSync-ver0.02"

type Msg struct {
	MagicHead  [magicLen]byte
	BlockIdx   uint32
	BlockSize  uint32
	FileSize   uint64
	DataSize   uint32
	Compressed bool
	Zero       bool
	Done       bool
}

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

// validateUploadMessage validates the source-to-destination protocol states.
// DataSize == 0 without flags is a checksum request; otherwise a message is
// exactly one of DONE, a zero-block write, or a payload write.
func validateUploadMessage(msg *Msg) error {
	if msg.Done {
		if msg.DataSize != 0 || msg.Zero || msg.Compressed {
			return fmt.Errorf("invalid done message flags")
		}
		return nil
	}
	if msg.Zero {
		if msg.DataSize != 0 || msg.Compressed {
			return fmt.Errorf("invalid zero message flags")
		}
		return nil
	}
	if msg.DataSize == 0 {
		if msg.Compressed {
			return fmt.Errorf("invalid checksum request flags")
		}
		return nil
	}
	return nil
}

func validateDownloadRequest(msg *Msg) error {
	if msg.Done || msg.Zero || msg.Compressed || msg.DataSize != 0 {
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
