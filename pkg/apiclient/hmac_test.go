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

package apiclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHMACAuth_ApplyAuth(t *testing.T) {
	secret := []byte("test-secret-key-12345678901234567890")
	auth := &HMACAuth{
		BrokerID:    "test-host-id",
		SecretKey: secret,
	}

	req, err := http.NewRequest(http.MethodGet, "http://localhost/api/v1/test", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	err = auth.ApplyAuth(req)
	if err != nil {
		t.Fatalf("ApplyAuth failed: %v", err)
	}

	// Verify headers are set
	if req.Header.Get(HeaderBrokerID) != "test-host-id" {
		t.Errorf("Expected Host-ID header to be 'test-host-id', got %q", req.Header.Get(HeaderBrokerID))
	}

	timestamp := req.Header.Get(HeaderTimestamp)
	if timestamp == "" {
		t.Error("Expected Timestamp header to be set")
	}

	// Verify timestamp is a valid Unix timestamp within reasonable range
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		t.Errorf("Timestamp is not a valid integer: %v", err)
	}
	now := time.Now().Unix()
	if ts < now-60 || ts > now+60 {
		t.Errorf("Timestamp %d is not within 60 seconds of current time %d", ts, now)
	}

	nonce := req.Header.Get(HeaderNonce)
	if nonce == "" {
		t.Error("Expected Nonce header to be set")
	}
	// Verify nonce is valid base64
	_, err = base64.URLEncoding.DecodeString(nonce)
	if err != nil {
		t.Errorf("Nonce is not valid URL-safe base64: %v", err)
	}

	signature := req.Header.Get(HeaderSignature)
	if signature == "" {
		t.Error("Expected Signature header to be set")
	}
	// Verify signature is valid base64
	_, err = base64.StdEncoding.DecodeString(signature)
	if err != nil {
		t.Errorf("Signature is not valid base64: %v", err)
	}
}

func TestHMACAuth_ApplyAuth_WithBody(t *testing.T) {
	secret := []byte("test-secret-key-12345678901234567890")
	auth := &HMACAuth{
		BrokerID:    "test-host-id",
		SecretKey: secret,
	}

	body := `{"key": "value"}`
	req, err := http.NewRequest(http.MethodPost, "http://localhost/api/v1/test", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.ContentLength = int64(len(body))

	err = auth.ApplyAuth(req)
	if err != nil {
		t.Fatalf("ApplyAuth failed: %v", err)
	}

	// Verify body is still readable after signing
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if string(bodyBytes) != body {
		t.Errorf("Body was modified: expected %q, got %q", body, string(bodyBytes))
	}
}

func TestHMACAuth_ApplyAuth_MissingCredentials(t *testing.T) {
	tests := []struct {
		name      string
		auth      *HMACAuth
		expectErr bool
	}{
		{
			name: "missing host ID",
			auth: &HMACAuth{
				BrokerID:    "",
				SecretKey: []byte("secret"),
			},
			expectErr: true,
		},
		{
			name: "missing secret key",
			auth: &HMACAuth{
				BrokerID:    "host-id",
				SecretKey: nil,
			},
			expectErr: true,
		},
		{
			name: "empty secret key",
			auth: &HMACAuth{
				BrokerID:    "host-id",
				SecretKey: []byte{},
			},
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			err := tc.auth.ApplyAuth(req)
			if tc.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestBuildCanonicalString(t *testing.T) {
	// Test canonical string format matches expected structure
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/api/v1/test?foo=bar", nil)
	timestamp := "1234567890"
	nonce := "test-nonce-value"

	canonical := BuildCanonicalString(req, timestamp, nonce)

	// Canonical format: METHOD\nPATH\nQUERY\nTIMESTAMP\nNONCE\nSIGNED_HEADERS\nBODY_HASH
	parts := strings.Split(string(canonical), "\n")

	if len(parts) < 5 {
		t.Fatalf("Expected at least 5 parts in canonical string, got %d", len(parts))
	}

	if parts[0] != "POST" {
		t.Errorf("Expected method 'POST', got %q", parts[0])
	}
	if parts[1] != "/api/v1/test" {
		t.Errorf("Expected path '/api/v1/test', got %q", parts[1])
	}
	if parts[2] != "foo=bar" {
		t.Errorf("Expected query 'foo=bar', got %q", parts[2])
	}
	if parts[3] != timestamp {
		t.Errorf("Expected timestamp %q, got %q", timestamp, parts[3])
	}
	if parts[4] != nonce {
		t.Errorf("Expected nonce %q, got %q", nonce, parts[4])
	}
}

func TestBuildCanonicalString_WithBody(t *testing.T) {
	body := `{"test": "data"}`
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/api/v1/test", strings.NewReader(body))
	req.ContentLength = int64(len(body))

	canonical := BuildCanonicalString(req, "123", "nonce")

	// The canonical string should include the body hash
	expectedHash := sha256.Sum256([]byte(body))
	expectedHashB64 := base64.StdEncoding.EncodeToString(expectedHash[:])

	if !strings.Contains(string(canonical), expectedHashB64) {
		t.Errorf("Canonical string should contain body hash %q, but got: %s", expectedHashB64, canonical)
	}

	// Verify body is still readable
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("Failed to read body: %v", err)
	}
	if string(bodyBytes) != body {
		t.Errorf("Body was modified: expected %q, got %q", body, string(bodyBytes))
	}
}

func TestBuildCanonicalString_PathNormalization(t *testing.T) {
	// This test documents the behavior of path normalization in the canonical string.
	// The canonical string MUST match what the server sees.
	// If a request is made to http://localhost//api/v1/test, the server (Go http.ServeMux)
	// will often see it as /api/v1/test due to path cleaning, OR it might see the double slash
	// depending on the middleware.

	tests := []struct {
		name         string
		url          string
		expectedPath string
	}{
		{
			name:         "standard path",
			url:          "http://localhost/api/v1/test",
			expectedPath: "/api/v1/test",
		},
		{
			name:         "trailing slash in host (single slash path)",
			url:          "http://localhost//api/v1/test",
			expectedPath: "//api/v1/test", // http.NewRequest preserves double slashes in Path
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, tc.url, nil)
			canonical := BuildCanonicalString(req, "123", "nonce")
			parts := strings.Split(string(canonical), "\n")
			if parts[1] != tc.expectedPath {
				t.Errorf("Expected path %q, got %q", tc.expectedPath, parts[1])
			}
		})
	}
}

func TestComputeHMAC(t *testing.T) {
	secret := []byte("test-secret")
	data := []byte("test-data")

	sig1 := ComputeHMAC(secret, data)
	sig2 := ComputeHMAC(secret, data)

	// Same input should produce same output
	if !bytes.Equal(sig1, sig2) {
		t.Error("HMAC computation is not deterministic")
	}

	// Different data should produce different signature
	sig3 := ComputeHMAC(secret, []byte("different-data"))
	if bytes.Equal(sig1, sig3) {
		t.Error("Different data produced same signature")
	}

	// Different secret should produce different signature
	sig4 := ComputeHMAC([]byte("different-secret"), data)
	if bytes.Equal(sig1, sig4) {
		t.Error("Different secret produced same signature")
	}
}

func TestVerifyHMAC(t *testing.T) {
	secret := []byte("test-secret")
	data := []byte("test-data")

	sig := ComputeHMAC(secret, data)

	if !VerifyHMAC(secret, data, sig) {
		t.Error("Valid signature was rejected")
	}

	// Tampered signature should fail
	sig[0] ^= 0xFF
	if VerifyHMAC(secret, data, sig) {
		t.Error("Tampered signature was accepted")
	}
}

func TestHMACAuth_Refresh(t *testing.T) {
	auth := &HMACAuth{
		BrokerID:    "host-id",
		SecretKey: []byte("secret"),
	}

	supported, err := auth.Refresh()
	if err != nil {
		t.Errorf("Refresh returned error: %v", err)
	}
	if supported {
		t.Error("Refresh should return false for HMAC auth")
	}
}
