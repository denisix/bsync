//go:build windows

package main

import (
	"fmt"
	"os"
)

// MMapReader provides a fallback reader on Windows (no mmap support).
type MMapReader struct {
	file     *os.File
	fileSize int64
}

// NewMMapReader creates a reader on Windows (fallback to regular file access).
func NewMMapReader(file *os.File) (*MMapReader, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat failed: %w", err)
	}
	size := stat.Size()
	if size == 0 {
		return nil, fmt.Errorf("empty file")
	}

	return &MMapReader{
		file:     file,
		fileSize: size,
	}, nil
}

// ReadAt reads data at the given offset (allocates new buffer).
func (m *MMapReader) ReadAt(offset int64, size int) []byte {
	if offset < 0 || offset >= m.fileSize {
		return nil
	}
	end := offset + int64(size)
	if end > m.fileSize {
		end = m.fileSize
	}
	buf := make([]byte, end-offset)
	n, err := m.file.ReadAt(buf, offset)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// Size returns the file size.
func (m *MMapReader) Size() int64 {
	return m.fileSize
}

// Close closes the file.
func (m *MMapReader) Close() error {
	if m.file != nil {
		return m.file.Close()
	}
	return nil
}
