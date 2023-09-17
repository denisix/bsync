package main

import (
  "sync"
  "github.com/klauspost/compress/zstd"
)

var (
	encoder *zstd.Encoder
	decoder *zstd.Decoder
	initOnce sync.Once
)

func initEncoderDecoder() {
	var err error
	encoder, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		//zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(1<<18), // Setting a window size of 1MB. Adjust as needed.
	)
	if err != nil {
		panic(err)
	}

	decoder, err = zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}
}

func compressData(data []byte) ([]byte, error) {
	initOnce.Do(initEncoderDecoder)

	// Pre-allocate a buffer with the same size as the input data.
	// This is a basic heuristic; you might want to adjust based on your data characteristics.
	buffer := make([]byte, 0, len(data))

	return encoder.EncodeAll(data, buffer), nil
}

func decompressData(data []byte) ([]byte, error) {
	initOnce.Do(initEncoderDecoder)

	// Pre-allocate a buffer with the same size as the input data for decompression.
	buffer := make([]byte, 0, len(data)*2) // Assuming a conservative 2:1 compression ratio.

	decompressed, err := decoder.DecodeAll(data, buffer)
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}
