package main

import (
	"bytes"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"crypto/sha256"
	"golang.org/x/crypto/blake2b"
)

func TestSetCompressionLevel(t *testing.T) {
	cases := map[string]zstd.EncoderLevel{
		"fast":    zstd.SpeedFastest,
		"default": zstd.SpeedDefault,
		"better":  zstd.SpeedBetterCompression,
		"best":    zstd.SpeedBestCompression,
		"unknown": zstd.SpeedDefault, // falls through to default
	}
	orig := encoderLevel
	defer func() { encoderLevel = orig }()

	for name, want := range cases {
		SetCompressionLevel(name)
		if encoderLevel != want {
			t.Errorf("SetCompressionLevel(%q) = %v, want %v", name, encoderLevel, want)
		}
	}
}

func TestCompressRoundTripAllLevels(t *testing.T) {
	orig := encoderLevel
	defer func() { encoderLevel = orig }()

	// Compressible data (repeating) so compression actually shrinks it.
	data := bytes.Repeat([]byte("the quick brown fox 0123456789 "), 4096)

	for _, level := range []string{"fast", "default", "better", "best"} {
		t.Run(level, func(t *testing.T) {
			SetCompressionLevel(level)
			comp, err := compressData(data)
			if err != nil {
				t.Fatalf("compress: %v", err)
			}
			if len(comp) >= len(data) {
				t.Errorf("compressible data did not shrink: %d -> %d", len(data), len(comp))
			}
			got, err := decompressData(comp)
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Error("round-trip mismatch")
			}
		})
	}
}

func TestCompressIncompressible(t *testing.T) {
	data := randomBytes(65536, 7)
	comp, err := compressData(data)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("round-trip mismatch on random data")
	}
}

func TestCompressEmpty(t *testing.T) {
	comp, err := compressData([]byte{})
	if err != nil {
		t.Fatalf("compress empty: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty round-trip produced %d bytes", len(got))
	}
}

func TestDecompressCorrupt(t *testing.T) {
	// Garbage is not a valid zstd stream.
	if _, err := decompressData([]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02}); err == nil {
		t.Error("expected error decompressing garbage")
	}
	// A truncated valid stream should also fail.
	good, err := compressData(bytes.Repeat([]byte("data"), 1000))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(good) > 4 {
		if _, err := decompressData(good[:len(good)-3]); err == nil {
			t.Error("expected error decompressing a truncated stream")
		}
	}
}

// compressData encrypts after compressing when a key is set; decompressData
// must decrypt then decompress. Regression coverage for the -e path.
func TestCompressWithEncryption(t *testing.T) {
	resetCrypto(t)
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}

	data := bytes.Repeat([]byte("encrypted-and-compressed "), 2048)
	comp, err := compressData(data)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	got, err := decompressData(comp)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("encrypted+compressed round-trip mismatch")
	}

	// With the wrong key, decompressData should fail at the decrypt stage.
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	if _, err := decompressData(comp); err == nil {
		t.Error("decompress with wrong key should fail")
	}
}

// --- zstd window sizing ---

func TestZstdWindowSizeTracksBlockSizeWithinBounds(t *testing.T) {
	saved := blockSize
	t.Cleanup(func() { blockSize = saved })

	for _, tc := range []struct {
		block uint32
		want  int
	}{
		{1 << 10, zstdMinWindow},     // tiny block: never below the old fixed value
		{1 << 18, 1 << 18},           // exactly the floor
		{1 << 20, 1 << 20},           // 1 MiB block -> 1 MiB window
		{1 << 22, zstdMaxWindow},     // 4 MiB block -> the cap
		{10 << 20, zstdMaxWindow},    // 10 MiB default block -> capped at 4 MiB
		{1 << 30, zstdMaxWindow},     // absurd block -> still capped
		{(1 << 21) + 12345, 1 << 21}, // rounded DOWN to a power of two
	} {
		blockSize = tc.block
		if got := zstdWindowSize(); got != tc.want {
			t.Errorf("blockSize=%d: window = %d, want %d", tc.block, got, tc.want)
		}
	}
}

// TestZstdWindowSizeIsAlwaysAPowerOfTwo: WithWindowSize requires one, and a
// non-power-of-two would make zstd.NewWriter fail inside encoderPool.New,
// which panics.
func TestZstdWindowSizeIsAlwaysAPowerOfTwo(t *testing.T) {
	saved := blockSize
	t.Cleanup(func() { blockSize = saved })
	for _, b := range []uint32{1, 999, 1 << 17, (1 << 19) + 1, 3 << 20, 7 << 20, 1<<31 - 1} {
		blockSize = b
		w := zstdWindowSize()
		if w&(w-1) != 0 {
			t.Errorf("blockSize=%d: window %d is not a power of two", b, w)
		}
		if w < zstdMinWindow || w > zstdMaxWindow {
			t.Errorf("blockSize=%d: window %d out of bounds", b, w)
		}
	}
}

// TestCompressRoundTripAtDefaultBlockSize exercises the real encoder built
// with the derived window, and the decoder that must accept its frames.
func TestCompressRoundTripAtDefaultBlockSize(t *testing.T) {
	saved := blockSize
	t.Cleanup(func() { blockSize = saved })
	blockSize = 10 << 20

	// Compressible, and larger than the old 256 KiB window so the change is
	// actually exercised.
	unit := []byte("bsync directory-tree sync compressible payload 0123456789 ")
	data := make([]byte, 0, 2<<20)
	for len(data) < 2<<20 {
		data = append(data, unit...)
	}

	compressed, err := compressData(data)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(compressed) >= len(data) {
		t.Fatalf("compressed to %d bytes from %d; expected real compression", len(compressed), len(data))
	}
	out, err := decompressData(compressed)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(out) != string(data) {
		t.Error("round trip did not reproduce the input")
	}
}

// --- per-byte CPU cost of the two hot stages ---
// These answer "what saturates the cores at high -w": every block is hashed
// on BOTH ends and compressed on the sender, so their per-byte costs are the
// per-worker throughput ceiling.

func benchPayload(n int) []byte {
	unit := []byte("bsync tree sync payload, moderately compressible 0123456789abcdef ")
	b := make([]byte, 0, n)
	for len(b) < n {
		b = append(b, unit...)
	}
	return b[:n]
}

func BenchmarkChecksumBlock(b *testing.B) {
	data := benchPayload(4 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = checksum(data)
	}
}

func BenchmarkCompressBlock(b *testing.B) {
	saved := blockSize
	blockSize = 10 << 20
	defer func() { blockSize = saved }()
	data := benchPayload(4 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := compressData(data); err != nil {
			b.Fatal(err)
		}
	}
}

// Candidate replacements for FNV-128a, measured on the same payload.
func BenchmarkHashCandidateSHA256(b *testing.B) {
	data := benchPayload(4 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := sha256.Sum256(data)
		_ = h
	}
}

func BenchmarkHashCandidateBlake2b128(b *testing.B) {
	data := benchPayload(4 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, err := blake2b.New(16, nil)
		if err != nil {
			b.Fatal(err)
		}
		h.Write(data)
		_ = h.Sum(nil)
	}
}

// TestResetEncoderPoolPicksUpNewWindow: pooled encoders carry the window they
// were built with, so changing blockSize without rebuilding the pool would
// silently keep compressing with the old one. Production sets blockSize once,
// but tests change it constantly -- this is what keeps them honest.
func TestResetEncoderPoolPicksUpNewWindow(t *testing.T) {
	saved := blockSize
	t.Cleanup(func() { blockSize = saved; resetEncoderPool() })

	// Build a pooled encoder at the smallest window.
	blockSize = 1 << 18
	resetEncoderPool()
	if got := zstdWindowSize(); got != 1<<18 {
		t.Fatalf("window = %d, want %d", got, 1<<18)
	}
	// One megabyte of incompressible noise, repeated. The second copy can
	// only be matched against the first from 1 MiB away -- reachable with a
	// 4 MiB window, invisible to a 256 KiB one. A merely repetitive payload
	// would NOT distinguish the two: a small window already captures all of
	// its redundancy.
	noise := randomBytes(1<<20, 4242)
	payload := append(append([]byte{}, noise...), noise...)
	small, err := compressData(payload)
	if err != nil {
		t.Fatal(err)
	}

	// Same data, larger window: must compress better, which can only happen
	// if the pool actually rebuilt.
	blockSize = 10 << 20
	resetEncoderPool()
	if got := zstdWindowSize(); got != zstdMaxWindow {
		t.Fatalf("window = %d, want %d", got, zstdMaxWindow)
	}
	large, err := compressData(payload)
	if err != nil {
		t.Fatal(err)
	}
	// The 4 MiB window should find the repeat and roughly halve the output.
	if len(large) >= len(small)*9/10 {
		t.Errorf("larger window produced %d bytes vs %d for the small window; "+
			"the pool appears not to have been rebuilt", len(large), len(small))
	}

	// Both must still decode.
	for name, blob := range map[string][]byte{"small-window": small, "large-window": large} {
		out, derr := decompressData(blob)
		if derr != nil {
			t.Errorf("%s: decompress: %v", name, derr)
			continue
		}
		if !bytes.Equal(out, payload) {
			t.Errorf("%s: round trip mismatch", name)
		}
	}
}

// TestZstdWindowSizeReadsBlockSizeAtomically is the race regression. blockSize
// is a runtime-mutable global -- the legacy single-file server adopts it from
// incoming messages via atomic.StoreUint32 while transfers are in flight --
// and zstdWindowSize runs from the encoder pool's New closure on worker
// goroutines. Reading it non-atomically was a genuine data race in production
// code, not just under test. Run with -race for this to mean anything.
func TestZstdWindowSizeReadsBlockSizeAtomically(t *testing.T) {
	saved := atomic.LoadUint32(&blockSize)
	t.Cleanup(func() { atomic.StoreUint32(&blockSize, saved); resetEncoderPool() })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: the server-side adoption pattern.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			atomic.StoreUint32(&blockSize, uint32(1<<18+(i%16)*4096))
		}
	}()

	// Readers: what the encoder pool does on worker goroutines.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				w := zstdWindowSize()
				if w < zstdMinWindow || w > zstdMaxWindow || w&(w-1) != 0 {
					t.Errorf("window %d out of range or not a power of two", w)
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// internalEncoderCount returns how many internal encoders enc's pool holds,
// via the unexported "encoders" channel's capacity -- forcing a first
// EncodeAll to make sure initialize() has actually run and populated it.
// Without WithEncoderConcurrency(1), that count defaults to
// runtime.GOMAXPROCS(0): each pooled *zstd.Encoder then carries that many
// internal encoders, each with its own multi-megabyte history buffer, so on
// a many-core box a handful of pooled encoders (one per hashing/compression
// goroutine, see checksum.go) multiplies into gigabytes.
func internalEncoderCount(t *testing.T, enc *zstd.Encoder) int {
	t.Helper()
	enc.EncodeAll([]byte("warm up the pool"), nil)
	v := reflect.ValueOf(enc).Elem().FieldByName("encoders")
	if !v.IsValid() || v.Kind() != reflect.Chan {
		t.Skip("klauspost/compress internals changed; re-verify WithEncoderConcurrency by hand")
	}
	return v.Cap()
}

// TestEncoderPoolUsesSingleInternalEncoder pins that every encoder built by
// encoderPool.New (and by resetEncoderPool, which must build identically)
// caps its internal encoder count at 1, regardless of GOMAXPROCS. This
// program always gets its parallelism from having up to -w pooled
// *zstd.Encoder values live at once, one per hashing goroutine -- never
// from asking one Encoder to internally fan a single EncodeAll call across
// multiple cores, which is the only thing the library's own concurrency
// option buys. Fails without zstd.WithEncoderConcurrency(1) on any host
// with GOMAXPROCS > 1.
func TestEncoderPoolUsesSingleInternalEncoder(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS > 1 to distinguish from the library default")
	}
	t.Cleanup(resetEncoderPool)

	pooled := encoderPool.Get().(*zstd.Encoder)
	if got := internalEncoderCount(t, pooled); got != 1 {
		t.Errorf("encoderPool: internal encoders = %d, want 1", got)
	}
	encoderPool.Put(pooled)

	resetEncoderPool()
	rebuilt := encoderPool.Get().(*zstd.Encoder)
	if got := internalEncoderCount(t, rebuilt); got != 1 {
		t.Errorf("after resetEncoderPool: internal encoders = %d, want 1", got)
	}
	encoderPool.Put(rebuilt)
}

// TestDecompressDataDoesNotOverAllocate is the regression for the removed
// decompressBufPool, whose destination buffer was returned to the pool ONLY
// on the error path (see decompressData) -- on the success path, the only
// one that ever ran in production, the pool was permanently empty and every
// single call paid for a fresh, fixed 10 MiB allocation regardless of the
// block's real size. Passing nil to DecodeAll instead lets it size the
// allocation to the frame's actual content size. Measured via TotalAlloc
// (monotonic, unaffected by GC or the scavenger) rather than HeapAlloc,
// which a background GC cycle could shrink out from under the assertion.
func TestDecompressDataDoesNotOverAllocate(t *testing.T) {
	small := make([]byte, 4096)
	for i := range small {
		small[i] = byte(i)
	}
	compressed, err := compressData(small)
	if err != nil {
		t.Fatalf("compressData: %v", err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const iterations = 50
	for i := 0; i < iterations; i++ {
		out, derr := decompressData(compressed)
		if derr != nil {
			t.Fatalf("decompressData: %v", derr)
		}
		if len(out) != len(small) {
			t.Fatalf("decompressed length = %d, want %d", len(out), len(small))
		}
	}
	runtime.ReadMemStats(&after)

	// The old fixed-10 MiB pool cost >= 10 MiB of TotalAlloc per call, ~500
	// MiB over 50 iterations. Sizing to content caps each call near 4 KiB;
	// generous headroom (1 MiB/iteration) keeps this robust to zstd's own
	// internal scratch allocations while still failing hard against a
	// regression back to a fixed 10 MiB destination buffer.
	perCall := (after.TotalAlloc - before.TotalAlloc) / iterations
	const ceiling = 1 << 20
	if perCall > ceiling {
		t.Errorf("decompressData allocated %d bytes/call, want < %d (looks like a fixed-size buffer regression)", perCall, ceiling)
	}
}
