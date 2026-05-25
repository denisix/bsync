package main

import (
	"sync"
	"time"
)

var (
	statsMutex sync.Mutex
	statsT0    time.Time
	statsTotOrigSize uint64
	statsTotCompSize uint64
	statsLastBlockNum uint32
	statsDiffs uint32
)

// TransferStats holds final transfer statistics.
type TransferStats struct {
	TotalBytes      uint64
	CompressedBytes uint64
	TotalBlocks     uint32
	Diffs           uint32
	StartTime       time.Time
}

// InitStats initializes global stats for a transfer.
func InitStats(t0 time.Time, lastBlockNum uint32) {
	statsMutex.Lock()
	statsT0 = t0
	statsTotOrigSize = 0
	statsTotCompSize = 0
	statsLastBlockNum = lastBlockNum
	statsDiffs = 0
	statsMutex.Unlock()
}

// UpdateStats updates the stats with new data.
func UpdateStats(addDiffs uint32, addBytes uint32, addCompBytes uint64) {
	statsMutex.Lock()
	statsDiffs += addDiffs
	if addBytes > 0 {
		statsTotOrigSize += uint64(addBytes)
	}
	if addCompBytes > 0 {
		statsTotCompSize += addCompBytes
	}
	statsMutex.Unlock()
}

// GetStats returns a snapshot of current transfer stats.
func GetStats() TransferStats {
	statsMutex.Lock()
	defer statsMutex.Unlock()
	return TransferStats{
		TotalBytes:      statsTotOrigSize,
		CompressedBytes: statsTotCompSize,
		TotalBlocks:     statsLastBlockNum,
		Diffs:           statsDiffs,
		StartTime:       statsT0,
	}
}

// ThroughputMBps returns current throughput in MB/s.
func ThroughputMBps() float64 {
	s := GetStats()
	elapsed := time.Since(s.StartTime).Seconds()
	if elapsed <= 0 {
		return 0
	}
	mbsDone := float64(statsTotOrigSize) / (1024 * 1024)
	return mbsDone / elapsed
}
