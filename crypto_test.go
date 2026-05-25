package main

import (
	"bytes"
	"sync"
	"testing"
)

// resetCrypto clears the global encryption state between tests.
func resetCrypto(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		encryptionKey = nil
		aeadOnce = sync.Once{}
	})
}

func TestGenerateEncryptionKey(t *testing.T) {
	resetCrypto(t)
	k1 := GenerateEncryptionKey()
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}
	k2 := GenerateEncryptionKey()
	if bytes.Equal(k1, k2) {
		t.Error("two generated keys are identical (not random)")
	}
}

func TestSetEncryptionKey(t *testing.T) {
	resetCrypto(t)

	// valid 32-byte hex
	validHex := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if err := SetEncryptionKey(validHex); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if !IsEncryptionEnabled() {
		t.Error("IsEncryptionEnabled false after SetEncryptionKey")
	}
	if GetEncryptionKeyHex() != validHex {
		t.Errorf("GetEncryptionKeyHex = %q, want %q", GetEncryptionKeyHex(), validHex)
	}

	// invalid hex
	if err := SetEncryptionKey("not-hex-zz"); err == nil {
		t.Error("expected error for non-hex key")
	}

	// wrong length (16 bytes)
	if err := SetEncryptionKey("00112233445566778899aabbccddeeff"); err == nil {
		t.Error("expected error for wrong-length key")
	}
}

func TestEncryptionDisabledPassthrough(t *testing.T) {
	resetCrypto(t)
	encryptionKey = nil
	aeadOnce = sync.Once{}

	if IsEncryptionEnabled() {
		t.Fatal("encryption should be disabled")
	}
	if GetEncryptionKeyHex() != "" {
		t.Error("GetEncryptionKeyHex should be empty when disabled")
	}
	data := []byte("hello")
	if got := encryptBlock(data); !bytes.Equal(got, data) {
		t.Error("encryptBlock should pass through when disabled")
	}
	out, err := decryptBlock(data)
	if err != nil || !bytes.Equal(out, data) {
		t.Errorf("decryptBlock passthrough failed: %v", err)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	resetCrypto(t)
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}

	plain := randomBytes(4096, 1)
	ct := encryptBlock(plain)

	if bytes.Equal(ct, plain) {
		t.Error("ciphertext equals plaintext")
	}
	if len(ct) != len(plain)+encryptOverhead {
		t.Errorf("ciphertext len = %d, want %d", len(ct), len(plain)+encryptOverhead)
	}

	// nonce randomness: encrypting the same plaintext twice differs
	ct2 := encryptBlock(plain)
	if bytes.Equal(ct, ct2) {
		t.Error("two encryptions of same plaintext are identical (nonce reuse)")
	}

	got, err := decryptBlock(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Error("round-trip mismatch")
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	resetCrypto(t)
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	ct := encryptBlock([]byte("secret data here"))

	// switch to a different key
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	if _, err := decryptBlock(ct); err == nil {
		t.Error("decrypt with wrong key should fail")
	}
}

func TestDecryptTooShort(t *testing.T) {
	resetCrypto(t)
	encryptionKey = GenerateEncryptionKey()
	aeadOnce = sync.Once{}
	if _, err := decryptBlock([]byte{1, 2, 3}); err == nil {
		t.Error("decrypt of too-short input should fail")
	}
}
