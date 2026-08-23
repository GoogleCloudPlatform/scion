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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// encryptedPrefix is prepended to every AES-256-GCM ciphertext stored by the
// local backend. Its presence distinguishes encrypted values from legacy
// plaintext, enabling transparent migration: if a stored value does not start
// with this prefix it is treated as unencrypted plaintext.
const encryptedPrefix = "enc:v1:"

// deriveLocalEncryptionKey deterministically derives a 32-byte AES-256 key
// from the deployment-wide shared signing secret using SHA-256 with a
// domain-specific prefix. This parallels the derivation used for JWT signing
// keys in pkg/hub/server.go (deriveSharedSigningKey) but uses a distinct
// domain separator to ensure cryptographic independence.
func deriveLocalEncryptionKey(sharedSecret string) []byte {
	sum := sha256.Sum256([]byte("scion-hub-local-secret-encryption:" + sharedSecret))
	return sum[:]
}

// encryptValue encrypts plaintext using AES-256-GCM and returns a string
// suitable for storage in the encrypted_value column. The format is:
//
//	enc:v1:<base64(nonce + ciphertext)>
//
// The nonce is a 12-byte random value prepended to the ciphertext.
func encryptValue(plaintext string, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encryptedPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptValue decrypts a value produced by encryptValue. If the value does
// not carry the enc:v1: prefix it is assumed to be legacy plaintext and
// returned as-is (with isLegacy = true) so that callers can re-encrypt on
// the next write.
func decryptValue(stored string, key []byte) (plaintext string, isLegacy bool, err error) {
	if len(stored) <= len(encryptedPrefix) || stored[:len(encryptedPrefix)] != encryptedPrefix {
		// Legacy plaintext — return as-is.
		return stored, true, nil
	}

	data, err := base64.StdEncoding.DecodeString(stored[len(encryptedPrefix):])
	if err != nil {
		return "", false, fmt.Errorf("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", false, fmt.Errorf("aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false, fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", false, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintextBytes, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", false, fmt.Errorf("gcm.Open: %w", err)
	}
	return string(plaintextBytes), false, nil
}
