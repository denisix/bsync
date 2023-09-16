package main

import (
  "github.com/pierrec/lz4"
)

func compressData(data []byte) ([]byte, error) {
  buf := make([]byte, lz4.CompressBlockBound(len(data)))

  var c lz4.Compressor
  n, err := c.CompressBlock(data, buf)
  if err != nil {
    return nil, err
  }
  return buf[:n], nil
}

func decompressData(data []byte) ([]byte, error) {
  out := make([]byte, blockSize)
  n, err := lz4.UncompressBlock(data, out)
  if err != nil {
    return nil, err
  }
  return out[:n], nil // uncompressed data
}

