package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"time"
)

type serverChecksum struct {
	hash     []byte
	received bool
}

// hashReceiver handles receiving hashes from server
type hashReceiver struct {
	conn         *AutoReconnectTCP
	hashes       []serverChecksum
	ready        chan struct{} // signals when a hash is ready
	lastBlockNum uint64
}

func newHashReceiver(conn *AutoReconnectTCP, lastBlockNum uint64) *hashReceiver {
	hr := &hashReceiver{
		conn:         conn,
		hashes:       make([]serverChecksum, lastBlockNum+1),
		ready:        make(chan struct{}, 100),
		lastBlockNum: lastBlockNum,
	}

	go hr.receiveHashes()
	return hr
}

func (hr *hashReceiver) receiveHashes() {
	hashBuf := make([]byte, 16)
	for i := uint64(0); i <= hr.lastBlockNum; i++ {
		if _, err := io.ReadFull(hr.conn, hashBuf); err != nil {
			Log("Error reading hash: %v\n", err)
			break
		}
		hr.hashes[i].hash = make([]byte, 16)
		copy(hr.hashes[i].hash, hashBuf)
		// Log("received hash [%d]: %x\n", i, hashBuf)
		hr.hashes[i].received = true
		hr.ready <- struct{}{}
		if i == hr.lastBlockNum {
			break
		}
	}
	close(hr.ready)
}

func (hr *hashReceiver) waitForHash(blockIdx uint64) []byte {
	if !hr.hashes[blockIdx].received {
		select {
		case _, ok := <-hr.ready:
			if !ok {
				return nil // channel closed, error occurred
			}
		case <-time.After(30 * time.Second):
			Log("Timeout waiting for hash %d\n", blockIdx)
			return nil
		}
	}
	return hr.hashes[blockIdx].hash
}

func startClient(serverAddress string, skipIdx uint64, fileSize uint64, blockSize uint64, lastBlockNum uint64, noCompress bool, checksumCache *ChecksumCache) {
	magicBytes := stringToFixedSizeArray(magicHead)

	saddr, err := net.ResolveTCPAddr("tcp", serverAddress)
	if err != nil {
		Log("Error resolving address: %v\n", err)
		os.Exit(1)
	}

	conn := NewAutoReconnectTCP(saddr)
	defer conn.Close()

	// Send initial message with file info
	msg, err := pack(&Msg{
		MagicHead:  magicBytes,
		BlockIdx:   0,
		BlockSize:  blockSize,
		FileSize:   fileSize,
		DataSize:   0,
		Compressed: false,
	})
	if err != nil {
		Log("Error packing initial message: %v\n", err)
		os.Exit(1)
	}

	Log("send init message\n")
	if _, err := conn.Write(msg); err != nil {
		Log("Error sending initial message: %v\n", err)
		os.Exit(1)
	}

	// Initialize hash receiver
	Log("initialize hash receiver\n")
	hashReceiver := newHashReceiver(conn, lastBlockNum)

	var totCompSize uint64 = 0
	var totOrigSize uint64 = 0
	t0 := time.Now()

	// Process blocks
	Log("processing blocks: %d .. %d\n", skipIdx, lastBlockNum)
	for blockIdx := uint64(skipIdx); blockIdx <= lastBlockNum; blockIdx++ {
		serverHash := hashReceiver.waitForHash(blockIdx)

		if serverHash == nil {
			Log("Error: serverHash nil\n")
			os.Exit(1)
		}

		localHash := checksumCache.WaitFor(blockIdx)
		totOrigSize += uint64(blockSize)
		secs := time.Since(t0).Seconds()
		mbs := float64(0)
		if secs > 0 {
			mbs = float64(totOrigSize) / mb1 / secs
		}

		if bytes.Equal(localHash, serverHash) || bytes.Equal(localHash, zeroBlockHash) {
			totCompSize += uint64(blockSize)
			ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))
			Log("block %d/%d (%0.2f%%) [-] size=%d ratio=%0.2f %0.2f MB/s\r", blockIdx, lastBlockNum, ratio, fileSize, ratio, mbs)
			continue
		}

		blockData := checksumCache.WaitForBlockData(blockIdx)
		if blockData == nil {
			Log("Error getting block data for block %d\n", blockIdx)
			os.Exit(1)
		}

		totCompSize += uint64(len(blockData.Data))
		ratio := float32(100 * (float64(totCompSize) / float64(totOrigSize)))

		// Send block
		msg, err := pack(&Msg{
			MagicHead:  magicBytes,
			BlockIdx:   blockIdx,
			BlockSize:  blockSize,
			FileSize:   fileSize,
			DataSize:   uint64(len(blockData.Data)),
			Compressed: blockData.IsCompressed,
		})
		if err != nil {
			Log("Error packing block message: %v\n", blockIdx)
			os.Exit(1)
		}

		if _, err := conn.Write(msg); err != nil {
			Log("Error sending block message: %v\n", err)
			os.Exit(1)
		}
		if _, err := conn.Write(blockData.Data); err != nil {
			Log("Error sending block data: %v\n", err)
			os.Exit(1)
		}

		Log("block %d/%d (%0.2f%%) [%s] size=%d ratio=%0.2f %0.2f MB/s\r",
			blockIdx, lastBlockNum, ratio, map[bool]string{true: "c", false: "w"}[blockData.IsCompressed],
			fileSize, ratio, mbs)
	}

	// Send completion message
	msg, err = pack(&Msg{
		MagicHead: magicBytes,
		BlockIdx:  0,
		BlockSize: blockSize,
		FileSize:  fileSize,
		DataSize:  0,
		LastBlock: true,
	})
	if err != nil {
		Log("Error packing completion message: %v\n", err)
		return
	}
	if _, err := conn.Write(msg); err != nil {
		Log("Error sending completion message: %v\n", err)
		return
	}
	time.Sleep(time.Second)
}
