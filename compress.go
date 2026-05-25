package main

import (
	"sync"
	"github.com/klauspost/compress/zstd"
)

var (
	encoderLevel zstd.EncoderLevel = zstd.SpeedFastest
	encoderPool = sync.Pool{
		New: func() interface{} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(encoderLevel),
				zstd.WithWindowSize(1<<18),
			)
			if err != nil {
				panic(err)
			}
			return enc
		},
	}
	// Buffer pool for compression output (reduces GC pressure)
	compressOutputPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 10*1024*1024)
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
	// Buffer pool for decompression output (reduces GC pressure)
	decompressBufPool = sync.Pool{
		New: func() interface{} {
			// Pre-allocate buffer for typical block size (10MB decompressed)
			return make([]byte, 0, 10*1024*1024)
		},
	}
)

// SetCompressionLevel sets the compression level for new encoders
// Must be called before any compression is done
func SetCompressionLevel(level string) {
	switch level {
	case "fast":
		encoderLevel = zstd.SpeedFastest
	case "better":
		encoderLevel = zstd.SpeedBetterCompression
	case "best":
		encoderLevel = zstd.SpeedBestCompression
	default:
		encoderLevel = zstd.SpeedDefault
	}
}

func compressData(data []byte) ([]byte, error) {
	encoder := encoderPool.Get().(*zstd.Encoder)
	defer encoderPool.Put(encoder)

	// Get buffer from pool to reduce allocations
	buf := compressOutputPool.Get().([]byte)
	buf = buf[:cap(buf)] // Use full capacity
	result := encoder.EncodeAll(data, buf[:0])

	// Copy result since we need to return the buffer to pool
	output := make([]byte, len(result))
	copy(output, result)
	compressOutputPool.Put(buf[:0])

	// Encrypt after compression if enabled
	if IsEncryptionEnabled() {
		output = encryptBlock(output)
	}

	return output, nil
}

func decompressData(data []byte) ([]byte, error) {
	// Decrypt before decompression if enabled
	if IsEncryptionEnabled() {
		decrypted, err := decryptBlock(data)
		if err != nil {
			return nil, err
		}
		data = decrypted
	}

	decoder := decoderPool.Get().(*zstd.Decoder)
	defer decoderPool.Put(decoder)

	// Get buffer from pool to reduce allocations
	buf := decompressBufPool.Get().([]byte)
	buf = buf[:0] // Reset length but keep capacity

	decompressed, err := decoder.DecodeAll(data, buf)
	if err != nil {
		decompressBufPool.Put(decompressed[:0]) // Return to pool on error
		return nil, err
	}
	// Note: Buffer is not returned to pool since caller uses it
	// This is a tradeoff - we get reduced initial allocation but don't fully reuse
	return decompressed, nil
}
