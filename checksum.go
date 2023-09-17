package main

import (
	"hash/fnv"
)

// compute FNV1a checksum
func checksum(data []byte) []byte {
	hasher := fnv.New128a() // Using 128-bit FNV-1a. You can also use New32a(), New64a() for smaller sizes.
	hasher.Write(data)
	return hasher.Sum(nil)
}
