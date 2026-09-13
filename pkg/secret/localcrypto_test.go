// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secret

import (
	"strings"
	"testing"
)

func TestDeriveLocalEncryptionKey_Deterministic(t *testing.T) {
	key1 := DeriveLocalEncryptionKey("my-secret")
	key2 := DeriveLocalEncryptionKey("my-secret")
	if len(key1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key1))
	}
	for i := range key1 {
		if key1[i] != key2[i] {
			t.Fatal("same input must produce the same key")
		}
	}
}

func TestDeriveLocalEncryptionKey_DifferentInputs(t *testing.T) {
	key1 := DeriveLocalEncryptionKey("secret-a")
	key2 := DeriveLocalEncryptionKey("secret-b")
	same := true
	for i := range key1 {
		if key1[i] != key2[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different inputs must produce different keys")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := DeriveLocalEncryptionKey("test-secret")

	tests := []string{
		"hello world",
		"",
		"sk-test-1234567890",
		strings.Repeat("x", 10000),
	}
	for _, plaintext := range tests {
		encrypted, err := EncryptValue(plaintext, key)
		if err != nil {
			t.Fatalf("EncryptValue(%q) failed: %v", plaintext, err)
		}
		if !strings.HasPrefix(encrypted, EncryptedPrefix) {
			t.Errorf("expected prefix %q, got %q", EncryptedPrefix, encrypted[:len(EncryptedPrefix)])
		}
		if encrypted == plaintext {
			t.Error("encrypted value should differ from plaintext")
		}

		decrypted, isLegacy, err := DecryptValue(encrypted, key)
		if err != nil {
			t.Fatalf("DecryptValue failed: %v", err)
		}
		if isLegacy {
			t.Error("expected isLegacy=false for encrypted value")
		}
		if decrypted != plaintext {
			t.Errorf("round-trip mismatch: got %q, want %q", decrypted, plaintext)
		}
	}
}

func TestDecryptValue_LegacyPlaintext(t *testing.T) {
	key := DeriveLocalEncryptionKey("test-secret")

	// Values without the enc:v1: prefix are treated as legacy plaintext.
	legacyValues := []string{
		"sk-test-123",
		"plain-text-secret",
		"",
		"some random value",
	}
	for _, val := range legacyValues {
		decrypted, isLegacy, err := DecryptValue(val, key)
		if err != nil {
			t.Fatalf("DecryptValue(%q) failed: %v", val, err)
		}
		if !isLegacy {
			t.Errorf("expected isLegacy=true for %q", val)
		}
		if decrypted != val {
			t.Errorf("expected %q, got %q", val, decrypted)
		}
	}
}

func TestEncryptValue_UniqueNonces(t *testing.T) {
	key := DeriveLocalEncryptionKey("test-secret")

	// Encrypting the same plaintext twice should produce different ciphertexts
	// due to random nonces.
	enc1, err := EncryptValue("same-value", key)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := EncryptValue("same-value", key)
	if err != nil {
		t.Fatal(err)
	}
	if enc1 == enc2 {
		t.Error("two encryptions of the same plaintext should differ (random nonce)")
	}

	// Both should decrypt to the same value.
	dec1, _, _ := DecryptValue(enc1, key)
	dec2, _, _ := DecryptValue(enc2, key)
	if dec1 != "same-value" || dec2 != "same-value" {
		t.Error("both ciphertexts should decrypt to the same plaintext")
	}
}

func TestDecryptValue_WrongKey(t *testing.T) {
	key1 := DeriveLocalEncryptionKey("secret-1")
	key2 := DeriveLocalEncryptionKey("secret-2")

	encrypted, err := EncryptValue("my-secret-data", key1)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = DecryptValue(encrypted, key2)
	if err == nil {
		t.Error("decrypting with wrong key should fail")
	}
}

func TestDecryptValue_CorruptedCiphertext(t *testing.T) {
	key := DeriveLocalEncryptionKey("test-secret")

	// Valid prefix but invalid base64 data
	_, _, err := DecryptValue(EncryptedPrefix+"!!!invalid-base64!!!", key)
	if err == nil {
		t.Error("corrupted base64 should fail")
	}

	// Valid prefix and base64, but too short
	_, _, err = DecryptValue(EncryptedPrefix+"AAAA", key)
	if err == nil {
		t.Error("truncated ciphertext should fail")
	}
}
