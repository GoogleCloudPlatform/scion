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

package runtimebroker

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
)

func TestBrokerAuthMiddleware_Disabled(t *testing.T) {
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_AllowUnauthenticated(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: true,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 for unauthenticated request, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_RequireAuth(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Request without any HMAC headers
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()

	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for missing auth headers, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_ValidSignature(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a properly signed request
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", secret)

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 for valid signature, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBrokerAuthMiddleware_InvalidSignature(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with wrong signature
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, timestamp)
	req.Header.Set(apiclient.HeaderNonce, "test-nonce")
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString([]byte("invalid-signature")))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for invalid signature, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_ExpiredTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with old timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	oldTimestamp := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	nonce := "test-nonce"

	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, oldTimestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	// Compute valid signature with old timestamp
	canonical := apiclient.BuildCanonicalString(req, oldTimestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for expired timestamp, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_FutureTimestamp(t *testing.T) {
	secret := []byte("test-secret-key-32bytes!12345678")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create a request with future timestamp
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	futureTimestamp := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	nonce := "test-nonce"

	req.Header.Set(apiclient.HeaderBrokerID, "test-host")
	req.Header.Set(apiclient.HeaderTimestamp, futureTimestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	// Compute valid signature with future timestamp
	canonical := apiclient.BuildCanonicalString(req, futureTimestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401 for future timestamp, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_MissingHeaders(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{
			name: "missing timestamp",
			headers: map[string]string{
				apiclient.HeaderBrokerID:    "host-id",
				apiclient.HeaderNonce:     "nonce",
				apiclient.HeaderSignature: "sig",
			},
		},
		{
			name: "missing signature",
			headers: map[string]string{
				apiclient.HeaderBrokerID:    "host-id",
				apiclient.HeaderTimestamp: "123",
				apiclient.HeaderNonce:     "nonce",
			},
		},
		{
			name: "missing host ID only",
			headers: map[string]string{
				apiclient.HeaderTimestamp: "123",
				apiclient.HeaderNonce:     "nonce",
				apiclient.HeaderSignature: "sig",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			rr := httptest.NewRecorder()
			middleware.Middleware(handler).ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("Expected status 401, got %d", rr.Code)
			}
		})
	}
}

func TestBrokerAuthMiddleware_InvalidTimestampFormat(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(apiclient.HeaderBrokerID, "host-id")
	req.Header.Set(apiclient.HeaderTimestamp, "not-a-number")
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "sig")

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_InvalidSignatureEncoding(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(apiclient.HeaderBrokerID, "host-id")
	req.Header.Set(apiclient.HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "not-valid-base64!!!")

	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_UpdateSecretKey(t *testing.T) {
	oldSecret := []byte("old-secret-key")
	newSecret := []byte("new-secret-key-32bytes!12345678")

	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            oldSecret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Create request signed with new secret
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", newSecret)

	// Should fail with old secret
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 before key update, got %d", rr.Code)
	}

	// Update to new secret
	middleware.UpdateSecretKey(newSecret)

	// Should succeed now
	req = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	signRequest(req, "test-host", newSecret)
	rr = httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 after key update, got %d", rr.Code)
	}
}

func TestBrokerAuthMiddleware_SetEnabled(t *testing.T) {
	secret := []byte("test-secret-key")
	middleware := NewBrokerAuthMiddleware(BrokerAuthConfig{
		Enabled:              false,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Should pass when disabled
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr := httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("Expected 200 when disabled, got %d", rr.Code)
	}

	// Enable auth
	middleware.SetEnabled(true)

	// Should fail without auth
	req = httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rr = httptest.NewRecorder()
	middleware.Middleware(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 when enabled, got %d", rr.Code)
	}
}

func TestDefaultBrokerAuthConfig(t *testing.T) {
	cfg := DefaultBrokerAuthConfig()

	if cfg.Enabled {
		t.Error("Expected Enabled to be false by default")
	}
	if cfg.MaxClockSkew != 5*time.Minute {
		t.Errorf("Expected MaxClockSkew 5m, got %v", cfg.MaxClockSkew)
	}
	if !cfg.AllowUnauthenticated {
		t.Error("Expected AllowUnauthenticated to be true by default")
	}
}

// signRequest signs an HTTP request with HMAC authentication.
func signRequest(req *http.Request, brokerID string, secret []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "test-nonce-" + timestamp

	req.Header.Set(apiclient.HeaderBrokerID, brokerID)
	req.Header.Set(apiclient.HeaderTimestamp, timestamp)
	req.Header.Set(apiclient.HeaderNonce, nonce)

	canonical := apiclient.BuildCanonicalString(req, timestamp, nonce)
	sig := apiclient.ComputeHMAC(secret, canonical)
	req.Header.Set(apiclient.HeaderSignature, base64.StdEncoding.EncodeToString(sig))
}
