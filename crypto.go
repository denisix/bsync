package main

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	encryptionKey []byte
	aeadCipher    cipher.AEAD
	aeadOnce      sync.Once
)

// GenerateEncryptionKey creates a new random 32-byte key
func GenerateEncryptionKey() []byte {
	key := make([]byte, chacha20poly1305.KeySize)
	rand.Read(key)
	return key
}

// SetEncryptionKey sets the encryption key from hex string
func SetEncryptionKey(hexKey string) error {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return err
	}
	if len(key) != chacha20poly1305.KeySize {
		return errors.New("encryption key must be 32 bytes")
	}
	encryptionKey = key
	// Reset aeadOnce so getAEAD creates new cipher with new key
	aeadOnce = sync.Once{}
	return nil
}

// GetEncryptionKeyHex returns the current key as hex string
func GetEncryptionKeyHex() string {
	if encryptionKey == nil {
		return ""
	}
	return hex.EncodeToString(encryptionKey)
}

// IsEncryptionEnabled returns true if encryption is active
func IsEncryptionEnabled() bool {
	return encryptionKey != nil
}

// getAEAD returns cached AEAD cipher instance
func getAEAD() cipher.AEAD {
	aeadOnce.Do(func() {
		aeadCipher, _ = chacha20poly1305.New(encryptionKey)
	})
	return aeadCipher
}

// encryptBlock encrypts data with ChaCha20-Poly1305
// Returns: nonce (12 bytes) + ciphertext + tag (16 bytes)
func encryptBlock(plaintext []byte) []byte {
	if encryptionKey == nil {
		return plaintext
	}

	aead := getAEAD()

	// Generate random nonce
	nonce := make([]byte, chacha20poly1305.NonceSize)
	rand.Read(nonce)

	// Encrypt (seal appends auth tag)
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	// Return nonce + ciphertext
	result := make([]byte, len(nonce)+len(ciphertext))
	copy(result, nonce)
	copy(result[len(nonce):], ciphertext)

	return result
}

// decryptBlock decrypts ChaCha20-Poly1305 encrypted data
func decryptBlock(data []byte) ([]byte, error) {
	if encryptionKey == nil {
		return data, nil
	}

	aead := getAEAD()

	nonceSize := chacha20poly1305.NonceSize
	if len(data) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]

	return aead.Open(nil, nonce, ciphertext, nil)
}
