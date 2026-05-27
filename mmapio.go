//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// MMapReader provides memory-mapped file reading.
// Best for local-to-local sync scenarios where the file is read sequentially.
type MMapReader struct {
	data     []byte
	file     *os.File
	fileSize int64
}

// NewMMapReader creates a new memory-mapped reader for the given file.
// The file remains open for the lifetime of the MMapReader.
func NewMMapReader(file *os.File) (*MMapReader, error) {
	// Get file size
	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat failed: %w", err)
	}
	size := stat.Size()
	if size == 0 {
		return nil, fmt.Errorf("empty file")
	}

	// Memory map the file
	data, err := syscall.Mmap(
		int(file.Fd()),
		0,
		int(size),
		syscall.PROT_READ,
		syscall.MAP_SHARED,
	)
	if err != nil {
		return nil, fmt.Errorf("mmap failed: %w", err)
	}

	// Advise kernel about sequential access pattern
	_ = syscall.Madvise(data, syscall.MADV_SEQUENTIAL)

	return &MMapReader{
		data:     data,
		file:     file,
		fileSize: size,
	}, nil
}

// ReadAt reads data at the given offset without allocation.
// Returns a slice directly into the mmap'd region.
func (m *MMapReader) ReadAt(offset int64, size int) []byte {
	if offset < 0 || offset >= m.fileSize {
		return nil
	}
	end := offset + int64(size)
	if end > m.fileSize {
		end = m.fileSize
	}
	return m.data[offset:end]
}

// Size returns the file size.
func (m *MMapReader) Size() int64 {
	return m.fileSize
}

// Close unmaps the memory and closes the file.
func (m *MMapReader) Close() error {
	err1 := syscall.Munmap(m.data)
	if m.file != nil {
		err2 := m.file.Close()
		if err1 != nil {
			return err1
		}
		return err2
	}
	return err1
}
