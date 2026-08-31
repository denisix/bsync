package main

import (
	"github.com/klauspost/compress/zstd"
	"sync"
	"sync/atomic"
)

// compressMinSize is the smallest payload worth offering to zstd. A zstd
// frame carries its own header/footer overhead (magic number, frame
// header, checksum), so below a few hundred bytes a frame is essentially
// guaranteed to come back no smaller than the input -- callers already fall
// back to sending raw whenever compression doesn't shrink the block, so
// skipping the attempt below this floor is a pure win: no encoder pool
// round trip, no scratch-buffer copy, for an outcome that was never in
// doubt. Matters most in directory mode, where files well under this size
// are common.
const compressMinSize = 512

var (
	encoderLevel zstd.EncoderLevel = zstd.SpeedFastest
	encoderPool                    = sync.Pool{
		New: func() interface{} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(encoderLevel),
				zstd.WithWindowSize(zstdWindowSize()),
				// Parallelism already comes from having up to -w encoders
				// live at once (one per hashing/compression goroutine, see
				// checksum.go), each pulled from this pool. Without this,
				// zstd.NewWriter defaults concurrent to runtime.GOMAXPROCS(0)
				// and eagerly builds that many INTERNAL encoders per
				// *zstd.Encoder (encoder.go's initialize), each with its own
				// ~8 MiB history buffer -- multiplying the pool's memory by
				// the host's core count for a form of parallelism this
				// program never uses (EncodeAll always runs as a single call
				// on one goroutine's data, never as a concurrent stream).
				zstd.WithEncoderConcurrency(1),
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
			dec, err := zstd.NewReader(nil,
				// Matches WithEncoderConcurrency(1) above: this program
				// never hands one decoder a concurrent multi-block stream,
				// so there is nothing for extra internal decode goroutines
				// to do. WithDecoderMaxWindow caps what a frame may declare
				// -- without it a hostile or corrupt frame could claim the
				// library default of 512 MiB (zstd's MaxWindowSize) and the
				// decoder would honor it.
				zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxWindow(zstdMaxWindow),
			)
			if err != nil {
				panic(err)
			}
			return dec
		},
	}
)

// zstd window bounds. The window caps how far back zstd can match, so a
// window smaller than the block being compressed simply discards ratio --
// and the old fixed 1<<18 (256 KiB) was ~40x smaller than the 10 MiB default
// block. Measured on a 191 MB corpus of real source and binaries, at 10 MiB
// blocks, raising it is free on BOTH axes (longer matches mean fewer, longer
// sequences to emit, so it also encodes faster):
//
//	level    window   ratio    MB/s
//	fastest    256K   3.508   227.6
//	fastest   4096K   3.627   273.6     (+3.4% ratio, +20% speed)
//	default    256K   3.688   190.1
//	default   4096K   3.864   232.3     (+4.8% ratio, +22% speed)
//
// 8 MiB measured identically to 4 MiB at those block sizes, so the cap stops
// at 4 MiB: past that it is pure memory cost (the window is per ENCODER, and
// up to -w of them are live at once) for no measured gain.
const (
	zstdMinWindow = 1 << 18 // 256 KiB, the previous fixed value
	zstdMaxWindow = 1 << 22 // 4 MiB, where the measured gains stop
)

// zstdWindowSize picks the encoder window from the configured block size:
// there is nothing to gain from a window larger than the data ever handed to
// a single EncodeAll call. Rounded DOWN to a power of two, which
// WithWindowSize requires, and clamped to the bounds above.
//
// Read lazily inside encoderPool.New (which first runs during the transfer,
// long after flag parsing), so it sees the real -b value rather than the
// package default.
func zstdWindowSize() int {
	// atomic: blockSize is a runtime-mutable global -- the legacy
	// single-file server adopts it from incoming messages
	// (atomic.StoreUint32 in server.go) while transfers are in flight, and
	// this is read from the encoder pool's New closure on worker
	// goroutines. Matches how every other reader accesses it (client.go,
	// server.go, visualizer.go).
	w := int(atomic.LoadUint32(&blockSize))
	if w < zstdMinWindow {
		return zstdMinWindow
	}
	if w > zstdMaxWindow {
		return zstdMaxWindow
	}
	// Round down to a power of two.
	p := zstdMinWindow
	for p<<1 <= w {
		p <<= 1
	}
	return p
}

// resetEncoderPool discards every pooled encoder so the next one is rebuilt
// from the CURRENT encoderLevel and zstdWindowSize().
//
// Needed because the window is no longer a constant: it is derived from
// blockSize inside encoderPool.New, which runs lazily at first use. A pooled
// encoder therefore carries whatever window was in force when it was built,
// and would happily keep serving after blockSize changed. In production that
// never happens (blockSize is set once, before any transfer), but tests
// change it freely, so without this a pooled encoder silently outlives the
// configuration it was built for.
func resetEncoderPool() {
	encoderPool = sync.Pool{
		New: func() interface{} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(encoderLevel),
				zstd.WithWindowSize(zstdWindowSize()),
				zstd.WithEncoderConcurrency(1),
			)
			if err != nil {
				panic(err)
			}
			return enc
		},
	}
}

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

	// No pooled destination buffer: DecodeAll's result is handed straight to
	// the caller and never returned here (it goes on to be written to disk),
	// so a pool backing it would need every caller to give the buffer back
	// once it's done -- and nothing did. That made decompressBufPool
	// permanently empty in practice (its Put only ran on the error path
	// below), so every call already paid for a fresh 10 MiB allocation
	// regardless of the actual block size. Passing nil lets DecodeAll size
	// the allocation to the frame's real content size instead.
	decompressed, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	return decompressed, nil
}
