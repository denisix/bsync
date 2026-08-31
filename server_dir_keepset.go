package main

import (
	"crypto/sha256"
	"encoding/binary"
)

// keepSet is the directory server's substitute for retaining every
// announced path string for the whole run. It serves two jobs that used
// to be served by fileTable.byPath (a map[string]uint32 keeping every
// announced relative path alive):
//
//   - duplicate-path detection during announce: a malformed or hostile
//     announce batch handing two different FileIDs the same path;
//   - the `-delete` keep-set deleteExtraneous consults once, at
//     OpAnnounceEnd, to tell a stale destination entry from a live one.
//
// Neither job ever needs the path bytes back -- only membership, plus (for
// the second) whether the matching entry is a KindDir deleteExtraneous
// should recurse into (see its own comment). So this stores a 128-bit
// digest of each path instead of the path itself: an open-addressed hash
// set with linear probing, in four pointer-free slices, that a garbage
// collector scans in O(1) regardless of how many entries it holds --
// unlike a map[string]uint32, which is a live heap object (plus a separate
// string allocation) per entry. At 32M paths this is roughly a quarter the
// size of a literal path arena and needs no sort.
//
// Exactness: a collision (two distinct paths hashing to the same 128-bit
// digest) can only ever make deleteExtraneous KEEP a stale destination
// entry it should have removed -- contains only ever answers "is this
// exact digest present", never resolves a hit against real path bytes, so
// it can produce a false positive (wrongly kept) but never a false
// negative (wrongly deleted). At 32M entries the collision probability is
// approximately 3e-24 -- the same order of magnitude the wire protocol
// already trusts a 128-bit (16-byte) block hash to mean "these two blocks
// are identical" (see checksum.go's checksumSize). This was a deliberate,
// confirmed trade-off: see the plan history for the discussion of a
// literal path arena (~4x more memory, zero collision possibility of any
// kind) that this replaced.
type keepSet struct {
	hi, lo   []uint64 // the digest, split into two 64-bit halves
	occupied []uint64 // 1 bit per slot
	isDir    []uint64 // 1 bit per slot; only meaningful where occupied is set
	mask     uint64   // len(hi)-1; len(hi) is always a power of two
	count    int
}

// keepSetInitialSlots is the starting table size. Small on purpose: the
// server does not know the tree's final size at announce time (the client
// only reveals it at OpAnnounceEnd), and growth is amortized O(1) either
// way, so starting large just wastes memory on a small tree.
const keepSetInitialSlots = 1024

// newKeepSet creates an empty keep-set.
func newKeepSet() *keepSet {
	return &keepSet{
		hi:       make([]uint64, keepSetInitialSlots),
		lo:       make([]uint64, keepSetInitialSlots),
		occupied: make([]uint64, keepSetInitialSlots/64),
		isDir:    make([]uint64, keepSetInitialSlots/64),
		mask:     keepSetInitialSlots - 1,
	}
}

// pathDigest hashes rel into two 64-bit halves. rel must already be the
// clean, '/'-separated relative path (sanitizeRelPath's output) -- the
// same string both add and contains are always called with, so an
// identical path always produces an identical digest regardless of which
// call computed it.
func pathDigest(rel string) (hi, lo uint64) {
	sum := sha256.Sum256([]byte(rel))
	return binary.LittleEndian.Uint64(sum[0:8]), binary.LittleEndian.Uint64(sum[8:16])
}

func bitGet(bits []uint64, i uint64) bool {
	return bits[i/64]&(1<<(i%64)) != 0
}

func bitSet(bits []uint64, i uint64) {
	bits[i/64] |= 1 << (i % 64)
}

// add inserts rel's digest, recording whether kind is KindDir for
// deleteExtraneous's later recursion decision, and reports whether an
// identical path was already present -- the caller (announce) treats a
// true return as a duplicate-path stream error, exactly as byPath's
// lookup-then-insert used to.
func (k *keepSet) add(rel string, kind EntryKind) (dup bool) {
	if k.count*2 >= len(k.hi) {
		k.grow()
	}
	hi, lo := pathDigest(rel)
	idx := hi & k.mask
	for {
		if !bitGet(k.occupied, idx) {
			k.hi[idx], k.lo[idx] = hi, lo
			bitSet(k.occupied, idx)
			if kind == KindDir {
				bitSet(k.isDir, idx)
			}
			k.count++
			return false
		}
		if k.hi[idx] == hi && k.lo[idx] == lo {
			return true
		}
		idx = (idx + 1) & k.mask
	}
}

// contains reports whether rel's digest is present, and if so, whether it
// was recorded as a KindDir.
func (k *keepSet) contains(rel string) (isDir, ok bool) {
	hi, lo := pathDigest(rel)
	idx := hi & k.mask
	for {
		if !bitGet(k.occupied, idx) {
			return false, false
		}
		if k.hi[idx] == hi && k.lo[idx] == lo {
			return bitGet(k.isDir, idx), true
		}
		idx = (idx + 1) & k.mask
	}
}

// grow doubles the table and rehashes every occupied slot into it. No
// tombstones exist (nothing is ever removed from a keepSet in its
// lifetime -- see fileTable.freeAnnounceScratch, which discards the whole
// structure instead), so linear probing here and in add/contains stays
// correct without one.
func (k *keepSet) grow() {
	old := *k
	n := len(old.hi) * 2
	k.hi = make([]uint64, n)
	k.lo = make([]uint64, n)
	k.occupied = make([]uint64, n/64)
	k.isDir = make([]uint64, n/64)
	k.mask = uint64(n - 1)
	k.count = 0 // re-incremented below as entries are reinserted

	for i := range old.hi {
		if !bitGet(old.occupied, uint64(i)) {
			continue
		}
		idx := old.hi[i] & k.mask
		for bitGet(k.occupied, idx) {
			idx = (idx + 1) & k.mask
		}
		k.hi[idx], k.lo[idx] = old.hi[i], old.lo[i]
		bitSet(k.occupied, idx)
		if bitGet(old.isDir, uint64(i)) {
			bitSet(k.isDir, idx)
		}
		k.count++
	}
}
