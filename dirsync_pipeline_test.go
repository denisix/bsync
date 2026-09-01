package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// withPipeline runs fn with dirPipelineDisabled forced to the given state,
// restoring the environment-derived default afterwards.
func withPipeline(t *testing.T, disabled bool, fn func()) {
	t.Helper()
	restore := dirPipelineDisabled
	t.Cleanup(func() { dirPipelineDisabled = restore })
	dirPipelineDisabled = disabled
	fn()
}

// TestPipelinedMatchesStrictOnChangedTree is the equivalence pin: the same
// source tree, synced onto the same kind of half-stale destination, must
// produce identical results whether the worker uses the strict lock-step
// exchange or the windowed pipeline. This exercises every wire path the
// pipeline reorders: open bursts, speculative first checks, hash-match
// short-circuits, hash-mismatch data sends, zero blocks, and the
// Close+Open file switches between them.
func TestPipelinedMatchesStrictOnChangedTree(t *testing.T) {
	buildSrc := func(seed int64) string {
		src := t.TempDir()
		mustMkdirAll(t, filepath.Join(src, "sub", "deeper"))
		for i := 0; i < 20; i++ {
			mustWriteFile(t, filepath.Join(src, fmt.Sprintf("small%02d.bin", i)), string(randomBytes(700+i*13, seed+int64(i))))
		}
		// One multi-block file (window-within-file), with a zero region.
		big := make([]byte, 300*1024)
		copy(big, randomBytes(100*1024, seed+100))
		copy(big[200*1024:], randomBytes(100*1024, seed+101))
		mustWriteFile(t, filepath.Join(src, "sub", "big.bin"), string(big))
		mustWriteFile(t, filepath.Join(src, "sub", "deeper", "leaf.txt"), string(randomBytes(3000, seed+200)))
		return src
	}

	for _, disabled := range []bool{false, true} {
		disabled := disabled
		name := "pipelined"
		if disabled {
			name = "strict"
		}
		t.Run(name, func(t *testing.T) {
			withPipeline(t, disabled, func() {
				src := buildSrc(4242)
				// First pass onto an empty destination (created-fresh path),
				// then mutate roughly half the small files and one region of
				// the big file and sync again (check/mismatch/data path).
				dst, _ := runDirTransferOpts(t, src, 2, false, false)
				assertTreesEqual(t, src, dst)

				for i := 0; i < 20; i += 2 {
					mustWriteFile(t, filepath.Join(src, fmt.Sprintf("small%02d.bin", i)), string(randomBytes(700+i*13, 9000+int64(i))))
				}
				bigPath := filepath.Join(src, "sub", "big.bin")
				big, err := os.ReadFile(bigPath)
				if err != nil {
					t.Fatal(err)
				}
				copy(big[150*1024:], randomBytes(50*1024, 9999))
				mustWriteFile(t, bigPath, string(big))

				sess := startDirClientAgainst(t, src, dst, 2, false, nil, false, false, false)
				assertTreesEqual(t, src, dst)
				if sess.FailedCount() != 0 {
					t.Fatalf("%d file(s) failed", sess.FailedCount())
				}
			})
		})
	}
}

// TestPipelinedSpeculativeCheckOnCreatedFreshSparseFile pins the pipeline's
// one deliberate speculation: a file's first OpBlockCheck goes out before
// its open-ack (and thus before FlagCreated) is known. For a created-fresh
// file the destination is truncated straight to size, so that check comes
// back as the zero-block hash -- a zero first block must then resolve as
// unchanged with NO bytes sent for it, and a data block must mismatch and
// transfer. A sparse-leading file exercises exactly that first-block case.
func TestPipelinedSpeculativeCheckOnCreatedFreshSparseFile(t *testing.T) {
	withPipeline(t, false, func() {
		src := t.TempDir()
		// Leading zero block, data after it.
		content := make([]byte, 200*1024)
		copy(content[128*1024:], randomBytes(64*1024, 777))
		mustWriteFile(t, filepath.Join(src, "sparse.bin"), string(content))

		dst, sess := runDirTransferOpts(t, src, 1, false, false)
		assertTreesEqual(t, src, dst)
		if sess.FailedCount() != 0 {
			t.Fatalf("%d file(s) failed", sess.FailedCount())
		}
		// The zero region must not have traveled: net bytes stay well under
		// the file size (compression also shrinks the data region, so just
		// pin "less than the data region's raw size").
		if net := sess.bytesNet.Load(); net >= 128*1024 {
			t.Errorf("bytesNet = %d, want < %d -- zero blocks of a created-fresh file must not be transferred", net, 128*1024)
		}
	})
}

// TestPipelinedManySmallChangedFilesOneWorker is the regression stress for
// the file-switch gate: with a single worker connection, a run of small
// changed files forces a Close+Open switch after every block, each with a
// hash->data transition that must complete BEFORE the switch frame goes
// out (a data frame arriving after its file's Close hits a session bound
// to the next file and kills the connection). Before the gate existed this
// failed most files within milliseconds.
func TestPipelinedManySmallChangedFilesOneWorker(t *testing.T) {
	withPipeline(t, false, func() {
		src := t.TempDir()
		for i := 0; i < 40; i++ {
			mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%03d.bin", i)), string(randomBytes(1500, int64(3000+i))))
		}
		dst, _ := runDirTransferOpts(t, src, 1, false, false)
		assertTreesEqual(t, src, dst)

		// Change every file, keep sizes, resync with one worker: every
		// block takes the check -> mismatch -> data -> ack path and every
		// file boundary exercises the gate.
		for i := 0; i < 40; i++ {
			mustWriteFile(t, filepath.Join(src, fmt.Sprintf("f%03d.bin", i)), string(randomBytes(1500, int64(7000+i))))
		}
		sess := startDirClientAgainst(t, src, dst, 1, false, nil, false, false, false)
		assertTreesEqual(t, src, dst)
		if sess.FailedCount() != 0 {
			t.Fatalf("%d file(s) failed", sess.FailedCount())
		}
	})
}
