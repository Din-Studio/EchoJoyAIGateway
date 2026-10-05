package encryption

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"gpt-load/internal/platform/utils"
)

func TestServiceUsesDomainSeparatedFingerprintKey(t *testing.T) {
	const (
		masterKey = "domain-separated-master-key"
		plaintext = "sk-secret-value"
	)
	service, err := NewService(masterKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	legacyMAC := hmac.New(sha256.New, utils.DeriveAESKey(masterKey))
	_, _ = legacyMAC.Write([]byte(plaintext))
	legacyHash := hex.EncodeToString(legacyMAC.Sum(nil))

	if got := service.Hash(plaintext); got == legacyHash {
		t.Fatal("Hash() reuses the AES key; fingerprinting requires a domain-separated subkey")
	}
}

func TestServiceEncryptDecryptAndStableHash(t *testing.T) {
	service, err := NewService("a-test-master-key")
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	ciphertext, err := service.Encrypt("sk-secret-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if ciphertext == "sk-secret-value" {
		t.Fatal("Encrypt() returned plaintext")
	}

	plaintext, err := service.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if plaintext != "sk-secret-value" {
		t.Fatalf("Decrypt() = %q", plaintext)
	}

	first := service.Hash("sk-secret-value")
	second := service.Hash("sk-secret-value")
	if first == "" || first != second {
		t.Fatalf("Hash() is not stable: %q != %q", first, second)
	}
}

func TestNewServiceRejectsEmptyMasterKey(t *testing.T) {
	if _, err := NewService(""); err == nil {
		t.Fatal("NewService() error = nil, want error")
	}
}
