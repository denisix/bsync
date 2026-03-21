package main

import (
	"sync"
	"github.com/klauspost/compress/zstd"
)

var (
	encoderPool = sync.Pool{
		New: func() interface{} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(zstd.SpeedBestCompression),
				zstd.WithWindowSize(1<<18),
			)
			if err != nil {
				panic(err)
			}
			return enc
		},
	}
	decoderPool = sync.Pool{
		New: func() interface{} {
			dec, err := zstd.NewReader(nil)
			if err != nil {
				panic(err)
			}
			return dec
		},
	}
)

func compressData(data []byte) ([]byte, error) {
	encoder := encoderPool.Get().(*zstd.Encoder)
	defer encoderPool.Put(encoder)

	buffer := make([]byte, 0, len(data))
	return encoder.EncodeAll(data, buffer), nil
}

func decompressData(data []byte) ([]byte, error) {
	decoder := decoderPool.Get().(*zstd.Decoder)
	defer decoderPool.Put(decoder)

	buffer := make([]byte, 0, len(data)*2)
	decompressed, err := decoder.DecodeAll(data, buffer)
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}
