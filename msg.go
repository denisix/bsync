package main
import (
  "bytes"
  "encoding/binary"
)

const magicLen = 17
const magicHead = "blockSync-ver0.01"

type Msg struct {
  MagicHead  [magicLen]byte
  BlockIdx   uint32
  BlockSize  uint32
  FileSize   uint64
  DataSize   uint32
  Compressed bool
	Zero       bool
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
