package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// e2eOpts controls a single in-process transfer scenario.
type e2eOpts struct {
	workers      int
	noCompress   bool
	level        string // "" -> default
	encrypt      bool
	blockSize    uint32
	showProgress bool   // exercise the progress-stat code paths
	skipIdx      uint32 // resume: skip blocks below this index
	serverBlock  uint32 // if set, server uses this blockSize (mismatch test)
	dstInit      []byte // optional initial download destination content
}

func (o e2eOpts) norm() e2eOpts {
	if o.workers == 0 {
		o.workers = 1
	}
	if o.blockSize == 0 {
		o.blockSize = 4096
	}
	if o.level == "" {
		o.level = "default"
	}
	return o
}

// setupTransfer applies shared global state for an in-process transfer and
// returns a cleanup that restores the default block size.
func setupTransfer(t *testing.T, o e2eOpts) {
	t.Helper()
	resetEngineGlobals()
	SetCompressionLevel(o.level)
	blockSize = o.blockSize
	if o.showProgress {
		setSuppressProgress(false) // run serverPrintStats; output still gated by quiet
	}
	if o.encrypt {
		encryptionKey = GenerateEncryptionKey()
		aeadOnce = sync.Once{}
	}
	t.Cleanup(func() {
		blockSize = 10485760
		// Without this, a live key set by an -e subtest leaks into every
		// later test in the process that doesn't itself call
		// resetEngineGlobals() — including tests that assert on plaintext
		// wire bytes, which then fail with a decrypt error instead of the
		// assertion they were actually testing.
		encryptionKey = nil
		aeadOnce = sync.Once{}
	})
}

// runUpload performs src -> dst over loopback using the real client/server and
// asserts the destination ends up byte-identical to the source.
func runUpload(t *testing.T, src, dstInit []byte, o e2eOpts) {
	t.Helper()
	o = o.norm()
	setupTransfer(t, o)

	srcPath := writeTempFile(t, src)
	dstPath := writeTempFile(t, dstInit)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer srcFile.Close()
	dstFile, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dstFile.Close()

	fileSize := uint64(len(src))

	// The destination hashes blocks after the sender's block size is known.
	dstCache := NewChecksumCache(0)

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port

	// Optionally start the server with a different global block size so the
	// first wire message forces the size-adaptation/realloc branch.
	if o.serverBlock != 0 {
		blockSize = o.serverBlock
	}

	srvDone := make(chan error, 1)
	go func() {
		srvDone <- startServer(dstFile, "127.0.0.1", port, dstCache, o.workers)
	}()
	waitForListener(t, addr)

	srcCache := NewChecksumCache(0)
	if fileSize > 0 {
		srcCache = NewChecksumCache(uint32((fileSize - 1) / uint64(o.blockSize)))
	}
	if err := startClient(srcFile, addr, o.skipIdx, fileSize, o.blockSize, o.noCompress, srcCache, o.workers); err != nil {
		t.Fatalf("client upload: %v", err)
	}

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("server upload destination: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit after DONE")
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("upload mismatch: dst %d bytes, src %d bytes", len(got), len(src))
	}
}

// runDownload performs remote-src -> local-dst over loopback (reverse mode).
func runDownload(t *testing.T, src []byte, o e2eOpts) {
	t.Helper()
	o = o.norm()
	setupTransfer(t, o)

	srcPath := writeTempFile(t, src)
	// For a resumed download the local file must already hold the leading
	// blocks [0, skipIdx); seed them from the source.
	dstSeed := append([]byte{}, o.dstInit...)
	if o.skipIdx > 0 {
		end := int(uint64(o.skipIdx) * uint64(o.blockSize))
		if end > len(src) {
			end = len(src)
		}
		if len(dstSeed) < end {
			dstSeed = append(dstSeed, make([]byte, end-len(dstSeed))...)
		}
		copy(dstSeed[:end], src[:end])
	}
	dstPath := writeTempFile(t, dstSeed)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer srcFile.Close()
	dstFile, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dstFile.Close()

	fileSize := uint64(len(src))
	serverBlock := o.blockSize
	if o.serverBlock != 0 {
		serverBlock = o.serverBlock
	}
	blockSize = serverBlock
	srcCache := NewChecksumCache(0)
	if fileSize > 0 {
		lastBlock := uint32((fileSize - 1) / uint64(serverBlock))
		srcCache = NewChecksumCache(lastBlock)
		go precomputeChecksums(srcFile, serverBlock, lastBlock, srcCache, 0, o.workers)
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port

	srvDone := make(chan error, 1)
	go func() {
		srvDone <- startServerUpload(srcFile, "127.0.0.1", port, fileSize, srcCache, o.workers, o.noCompress)
	}()
	waitForListener(t, addr)

	if err := startClientDownload(dstFile, addr, o.skipIdx, o.blockSize, o.noCompress, o.workers); err != nil {
		t.Fatalf("client download: %v", err)
	}

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("download source server: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("upload server did not idle-exit")
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("download mismatch: dst %d bytes, src %d bytes", len(got), len(src))
	}
}

// --- upload scenarios ---

func TestE2EUploadSizes(t *testing.T) {
	const bs = 4096
	sizes := map[string]int{
		"sub-block":        bs / 2,
		"one-block":        bs,
		"block-plus-one":   bs + 1,
		"three-blocks":     bs * 3,
		"three-blocks-odd": bs*3 + 123,
		"many-blocks":      bs*8 + 7,
	}
	for name, sz := range sizes {
		t.Run(name, func(t *testing.T) {
			runUpload(t, randomBytes(sz, int64(sz)), nil, e2eOpts{blockSize: bs, workers: 1})
		})
	}
}

func TestE2EUploadContent(t *testing.T) {
	const bs = 4096
	n := bs*4 + 200
	contents := map[string][]byte{
		"random": randomBytes(n, 1),
		"zeros":  zeroBytes(n),
		"ff":     ffBytes(n),
		"mixed":  mixedBytes(n, bs, 2),
	}
	for name, data := range contents {
		t.Run(name, func(t *testing.T) {
			runUpload(t, data, nil, e2eOpts{blockSize: bs, workers: 4})
		})
	}
}

func TestE2EUploadDstStates(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*4, 3)
	modified := append([]byte{}, src...)
	copy(modified[bs:bs*2], randomBytes(bs, 99)) // change one block
	larger := append(append([]byte{}, src...), randomBytes(bs*2, 4)...)
	smaller := src[:bs*2]

	cases := map[string][]byte{
		"empty":     nil,
		"identical": append([]byte{}, src...),
		"modified":  modified,
		"all-zero":  zeroBytes(bs * 4),
		"larger":    larger,
		"smaller":   smaller,
	}
	for name, dst := range cases {
		t.Run(name, func(t *testing.T) {
			runUpload(t, src, dst, e2eOpts{blockSize: bs, workers: 4})
		})
	}
}

func TestE2EUploadFlags(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*4+55, 5)
	flagCases := map[string]e2eOpts{
		"nocompress":         {blockSize: bs, workers: 4, noCompress: true},
		"encrypt":            {blockSize: bs, workers: 4, encrypt: true},
		"nocompress+encrypt": {blockSize: bs, workers: 4, noCompress: true, encrypt: true},
		"level-fast":         {blockSize: bs, workers: 2, level: "fast"},
		"level-best":         {blockSize: bs, workers: 2, level: "best"},
		"single-worker":      {blockSize: bs, workers: 1},
	}
	for name, o := range flagCases {
		t.Run(name, func(t *testing.T) {
			runUpload(t, src, nil, o)
		})
	}
}

// Zero blocks in the source against a non-zero destination exercise the
// server's zero-overwrite path and the client's zero-block send path.
func TestE2EUploadZeroOverwrite(t *testing.T) {
	const bs = 4096
	src := mixedBytes(bs*4, bs, 70) // has zero holes
	dst := ffBytes(bs * 4)          // all non-zero, same size
	runUpload(t, src, dst, e2eOpts{blockSize: bs, workers: 4})
}

func TestE2EUploadWriteFailureReported(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/dev/full is Linux-specific")
	}
	const bs = 4096
	o := e2eOpts{blockSize: bs, workers: 1}.norm()
	setupTransfer(t, o)

	srcPath := writeTempFile(t, randomBytes(bs, 204))
	srcFile, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer srcFile.Close()
	dstFile, err := os.OpenFile("/dev/full", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open /dev/full: %v", err)
	}
	defer dstFile.Close()

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + port
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- startServer(dstFile, "127.0.0.1", port, NewChecksumCache(0), 1)
	}()
	waitForListener(t, addr)

	clientErr := startClient(srcFile, addr, 0, bs, bs, false, NewChecksumCache(0), 1)
	if clientErr == nil {
		t.Fatal("client upload unexpectedly succeeded after destination write failure")
	}
	select {
	case err := <-srvDone:
		if err == nil {
			t.Fatal("destination server unexpectedly succeeded after write failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("destination server did not report write failure")
	}
}

// Run with progress stats enabled to cover serverPrintStats / printStats.
func TestE2EUploadWithProgress(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*4, 71)
	runUpload(t, src, nil, e2eOpts{blockSize: bs, workers: 2, showProgress: true})
}

// --- download scenarios ---

func TestE2EDownload(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*4+77, 6)
	cases := map[string]e2eOpts{
		"default":            {blockSize: bs, workers: 1},
		"multi-worker":       {blockSize: bs, workers: 4},
		"nocompress":         {blockSize: bs, workers: 4, noCompress: true},
		"encrypt":            {blockSize: bs, workers: 1, encrypt: true},
		"nocompress+encrypt": {blockSize: bs, workers: 4, noCompress: true, encrypt: true},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			runDownload(t, src, o)
		})
	}
}

func TestE2EDownloadContent(t *testing.T) {
	const bs = 4096
	n := bs*3 + 10
	contents := map[string][]byte{
		"zeros":        zeroBytes(n),
		"mixed":        mixedBytes(n, bs, 8),
		"compressible": bytes.Repeat([]byte("zzzz"), n/4), // server sends compressed
	}
	for name, data := range contents {
		t.Run(name, func(t *testing.T) {
			runDownload(t, data, e2eOpts{blockSize: bs, workers: 2})
		})
	}
}

func TestE2EDownloadOverwritesExistingZeroBlocks(t *testing.T) {
	const bs = 4096
	src := mixedBytes(bs*3+51, bs, 200)
	runDownload(t, src, e2eOpts{blockSize: bs, workers: 2, dstInit: ffBytes(len(src))})
}

func TestE2EDownloadBlockSizeMismatch(t *testing.T) {
	const clientBlock = 1024
	const serverBlock = 4096
	src := randomBytes(serverBlock*2+19, 201)
	runDownload(t, src, e2eOpts{blockSize: clientBlock, serverBlock: serverBlock, workers: 2})
}

func TestE2EEmptyFiles(t *testing.T) {
	t.Run("upload", func(t *testing.T) {
		runUpload(t, nil, randomBytes(100, 202), e2eOpts{blockSize: 4096, workers: 1})
	})
	t.Run("download", func(t *testing.T) {
		runDownload(t, nil, e2eOpts{blockSize: 4096, workers: 1, dstInit: randomBytes(100, 203)})
	})
}

func TestE2EDownloadWithProgress(t *testing.T) {
	const bs = 4096
	runDownload(t, randomBytes(bs*3, 9), e2eOpts{blockSize: bs, workers: 2, showProgress: true})
}

// --- resume (-s / skipIdx) ---

func TestE2EUploadResume(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*5, 100)
	// dst already holds the first 2 blocks correctly; the rest is corrupted.
	dst := append([]byte{}, src...)
	copy(dst[2*bs:], randomBytes(bs*3, 101))
	runUpload(t, src, dst, e2eOpts{blockSize: bs, workers: 4, skipIdx: 2})
}

func TestE2EDownloadResume(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*5+33, 102)
	runDownload(t, src, e2eOpts{blockSize: bs, workers: 2, skipIdx: 2})
}

// --- idempotent re-sync (all blocks already in sync) ---

func TestE2EUploadIdempotent(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*4+17, 103)
	// destination already identical -> every block hashes equal, nothing sent
	runUpload(t, src, append([]byte{}, src...), e2eOpts{blockSize: bs, workers: 4})
}

// --- block-size mismatch (server adapts from the wire) ---

func TestE2EUploadBlockSizeMismatch(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*3+11, 104)
	runUpload(t, src, nil, e2eOpts{blockSize: bs, workers: 2, serverBlock: 10485760})
}

func TestE2EUploadBlockSizeMismatchExistingDestination(t *testing.T) {
	const bs = 4096
	src := randomBytes(bs*3+11, 105)
	runUpload(t, src, ffBytes(len(src)), e2eOpts{blockSize: bs, workers: 2, serverBlock: 10485760})
}

// --- block-boundary sizes across several block sizes ---

func TestE2EUploadBoundaries(t *testing.T) {
	blockSizes := []uint32{512, 1000, 4096}
	for _, bs := range blockSizes {
		for _, k := range []int{1, 2, 8} {
			for _, delta := range []int{-1, 0, 1} {
				sz := k*int(bs) + delta
				if sz <= 0 {
					continue
				}
				name := fmt.Sprintf("bs%d-k%d-d%d", bs, k, delta)
				t.Run(name, func(t *testing.T) {
					runUpload(t, randomBytes(sz, int64(sz)+int64(bs)), nil,
						e2eOpts{blockSize: bs, workers: 4})
				})
			}
		}
	}
	// one large block size
	t.Run("bs1M", func(t *testing.T) {
		bs := uint32(1 << 20)
		runUpload(t, randomBytes(int(bs)*2+5, 7), nil, e2eOpts{blockSize: bs, workers: 4})
	})
}

// --- content patterns through both directions ---

func TestE2EPatterns(t *testing.T) {
	const bs = 4096
	n := bs*4 + 200
	patterns := map[string][]byte{
		"almost-zero":      almostZeroBytes(n, bs, 1),
		"almost-full":      almostFullBytes(n, bs, 2),
		"alternating":      alternating(n, bs, 3),
		"single-byte-zero": singleByteInZero(n, n/2),
		"all-byte-values":  allByteValues(n),
		"compressible":     compressibleBytes(n),
		"ff":               ffBytes(n),
	}
	for name, data := range patterns {
		data := data
		t.Run("up/"+name, func(t *testing.T) {
			runUpload(t, data, nil, e2eOpts{blockSize: bs, workers: 4})
		})
		t.Run("down/"+name, func(t *testing.T) {
			runDownload(t, data, e2eOpts{blockSize: bs, workers: 2})
		})
	}
}

// --- many small blocks (ordering / window stress in a real transfer) ---

func TestE2EManySmallBlocks(t *testing.T) {
	const bs = 512
	src := randomBytes(bs*64, 55)
	runUpload(t, src, nil, e2eOpts{blockSize: bs, workers: 8})
}

// --- randomized fuzz: random size/content/flags, byte-identical every time ---

func fuzzContent(r *rand.Rand, n, block int) []byte {
	switch r.Intn(7) {
	case 0:
		return randomBytes(n, r.Int63())
	case 1:
		return zeroBytes(n)
	case 2:
		return mixedBytes(n, block, r.Int63())
	case 3:
		return almostZeroBytes(n, block, r.Int63())
	case 4:
		return alternating(n, block, r.Int63())
	case 5:
		return compressibleBytes(n)
	default:
		return ffBytes(n)
	}
}

func TestE2EFuzzUpload(t *testing.T) {
	master := rand.New(rand.NewSource(0xB5C))
	blockChoices := []uint32{512, 1000, 4096, 8192}
	levels := []string{"fast", "default", "better", "best"}

	for i := 0; i < 25; i++ {
		seed := master.Int63()
		r := rand.New(rand.NewSource(seed))
		bs := blockChoices[r.Intn(len(blockChoices))]
		nblocks := 1 + r.Intn(10)
		sz := nblocks*int(bs) + r.Intn(int(bs))
		if sz == 0 {
			sz = int(bs)
		}
		data := fuzzContent(r, sz, int(bs))
		o := e2eOpts{
			blockSize:  bs,
			workers:    1 + r.Intn(8),
			noCompress: r.Intn(2) == 0,
			encrypt:    r.Intn(2) == 0,
			level:      levels[r.Intn(len(levels))],
		}
		t.Run(fmt.Sprintf("up-%d", i), func(t *testing.T) {
			t.Logf("seed=%d bs=%d sz=%d %+v", seed, bs, sz, o)
			runUpload(t, data, nil, o)
		})
	}
}

func TestE2EFuzzDownload(t *testing.T) {
	master := rand.New(rand.NewSource(0xD09))
	blockChoices := []uint32{512, 1000, 4096}
	for i := 0; i < 8; i++ {
		seed := master.Int63()
		r := rand.New(rand.NewSource(seed))
		bs := blockChoices[r.Intn(len(blockChoices))]
		nblocks := 1 + r.Intn(6)
		sz := nblocks*int(bs) + r.Intn(int(bs))
		if sz == 0 {
			sz = int(bs)
		}
		data := fuzzContent(r, sz, int(bs))
		o := e2eOpts{
			blockSize:  bs,
			workers:    1 + r.Intn(4),
			noCompress: r.Intn(2) == 0,
			encrypt:    r.Intn(2) == 0,
		}
		t.Run(fmt.Sprintf("down-%d", i), func(t *testing.T) {
			t.Logf("seed=%d bs=%d sz=%d %+v", seed, bs, sz, o)
			runDownload(t, data, o)
		})
	}
}
