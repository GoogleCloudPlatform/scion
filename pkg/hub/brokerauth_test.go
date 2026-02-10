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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
)

func setupTestBrokerAuthService(t *testing.T) (*BrokerAuthService, store.Store) {
	t.Helper()

	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	config := DefaultBrokerAuthConfig()
	svc := NewBrokerAuthService(config, s)

	return svc, s
}

func TestBrokerRegistrationAndJoin(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker registration
	req := CreateBrokerRegistrationRequest{
		Name: "test-host",
		Labels: map[string]string{
			"env": "test",
		},
	}

	resp, err := svc.CreateBrokerRegistration(ctx, req, "admin-user-id")
	if err != nil {
		t.Fatalf("CreateBrokerRegistration failed: %v", err)
	}

	if resp.BrokerID == "" {
		t.Error("BrokerID should not be empty")
	}
	if resp.JoinToken == "" {
		t.Error("JoinToken should not be empty")
	}
	if !strings.HasPrefix(resp.JoinToken, JoinTokenPrefix) {
		t.Errorf("JoinToken should have prefix %s, got: %s", JoinTokenPrefix, resp.JoinToken)
	}
	if resp.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be set")
	}
	if resp.ExpiresAt.Before(time.Now()) {
		t.Error("ExpiresAt should be in the future")
	}

	// Complete the join
	joinReq := BrokerJoinRequest{
		BrokerID:    resp.BrokerID,
		JoinToken: resp.JoinToken,
		Hostname:  "test-hostname",
		Version:   "1.0.0",
	}

	joinResp, err := svc.CompleteBrokerJoin(ctx, joinReq, "http://localhost:9810")
	if err != nil {
		t.Fatalf("CompleteBrokerJoin failed: %v", err)
	}

	if joinResp.BrokerID != resp.BrokerID {
		t.Errorf("BrokerID mismatch: got %s, want %s", joinResp.BrokerID, resp.BrokerID)
	}
	if joinResp.SecretKey == "" {
		t.Error("SecretKey should not be empty")
	}
	if joinResp.HubEndpoint != "http://localhost:9810" {
		t.Errorf("HubEndpoint mismatch: got %s, want http://localhost:9810", joinResp.HubEndpoint)
	}

	// Verify the secret key is valid base64
	secretBytes, err := base64.StdEncoding.DecodeString(joinResp.SecretKey)
	if err != nil {
		t.Errorf("SecretKey should be valid base64: %v", err)
	}
	if len(secretBytes) != 32 {
		t.Errorf("SecretKey should be 32 bytes, got %d", len(secretBytes))
	}
}

func TestJoinWithInvalidToken(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker registration
	req := CreateBrokerRegistrationRequest{Name: "test-host"}
	resp, err := svc.CreateBrokerRegistration(ctx, req, "admin")
	if err != nil {
		t.Fatalf("CreateBrokerRegistration failed: %v", err)
	}

	// Try to join with wrong token
	joinReq := BrokerJoinRequest{
		BrokerID:    resp.BrokerID,
		JoinToken: JoinTokenPrefix + "invalid-token",
		Hostname:  "test",
		Version:   "1.0.0",
	}

	_, err = svc.CompleteBrokerJoin(ctx, joinReq, "http://localhost:9810")
	if err == nil {
		t.Error("Expected error for invalid token")
	}
	if !strings.Contains(err.Error(), "invalid join token") {
		t.Errorf("Expected 'invalid join token' error, got: %v", err)
	}
}

func TestJoinWithExpiredToken(t *testing.T) {
	// Create service with short token expiry
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	config := DefaultBrokerAuthConfig()
	config.JoinTokenExpiry = -1 * time.Hour // Already expired
	svc := NewBrokerAuthService(config, s)
	ctx := context.Background()

	// Create a broker registration (token will already be expired)
	req := CreateBrokerRegistrationRequest{Name: "test-host"}
	resp, err := svc.CreateBrokerRegistration(ctx, req, "admin")
	if err != nil {
		t.Fatalf("CreateBrokerRegistration failed: %v", err)
	}

	// Try to join
	joinReq := BrokerJoinRequest{
		BrokerID:    resp.BrokerID,
		JoinToken: resp.JoinToken,
		Hostname:  "test",
		Version:   "1.0.0",
	}

	_, err = svc.CompleteBrokerJoin(ctx, joinReq, "http://localhost:9810")
	if err == nil {
		t.Error("Expected error for expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("Expected 'expired' error, got: %v", err)
	}
}

func TestJoinTokenSingleUse(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create and complete a broker registration
	req := CreateBrokerRegistrationRequest{Name: "test-host"}
	resp, err := svc.CreateBrokerRegistration(ctx, req, "admin")
	if err != nil {
		t.Fatalf("CreateBrokerRegistration failed: %v", err)
	}

	joinReq := BrokerJoinRequest{
		BrokerID:    resp.BrokerID,
		JoinToken: resp.JoinToken,
		Hostname:  "test",
		Version:   "1.0.0",
	}

	// First join should succeed
	_, err = svc.CompleteBrokerJoin(ctx, joinReq, "http://localhost:9810")
	if err != nil {
		t.Fatalf("First CompleteBrokerJoin failed: %v", err)
	}

	// Second join with same token should fail
	_, err = svc.CompleteBrokerJoin(ctx, joinReq, "http://localhost:9810")
	if err == nil {
		t.Error("Expected error for reused token")
	}
}

func TestValidateBrokerSignature(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker and set up its secret
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secretKey := []byte("test-secret-key-32-bytes-long!!")
	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	// Create a signed request
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "test-nonce-123"
	body := []byte(`{"test": "data"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderBrokerID, brokerID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	// Build canonical string and compute signature
	canonicalString := svc.buildCanonicalString(req, timestamp, nonce)

	// Reset body for validation
	req.Body = io.NopCloser(bytes.NewReader(body))

	h := hmac.New(sha256.New, secretKey)
	h.Write(canonicalString)
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))
	req.Header.Set(HeaderSignature, signature)

	// Validate the signature
	identity, err := svc.ValidateBrokerSignature(ctx, req)
	if err != nil {
		t.Fatalf("ValidateBrokerSignature failed: %v", err)
	}

	if identity.BrokerID() != brokerID {
		t.Errorf("BrokerID mismatch: got %s, want %s", identity.BrokerID(), brokerID)
	}
	if identity.Type() != "broker" {
		t.Errorf("Type mismatch: got %s, want broker", identity.Type())
	}
}

func TestValidateBrokerSignature_InvalidSignature(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker with secret
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: []byte("correct-secret-key-32-bytes-ok!"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	// Create a request with wrong signature
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(HeaderBrokerID, brokerID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(HeaderNonce, "test-nonce")
	req.Header.Set(HeaderSignature, "invalid-signature")

	_, err := svc.ValidateBrokerSignature(ctx, req)
	if err == nil {
		t.Error("Expected error for invalid signature")
	}
	if !strings.Contains(err.Error(), "invalid signature") {
		t.Errorf("Expected 'invalid signature' error, got: %v", err)
	}
}

func TestValidateBrokerSignature_ClockSkew(t *testing.T) {
	// Create service with short clock skew tolerance
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate store: %v", err)
	}

	config := DefaultBrokerAuthConfig()
	config.MaxClockSkew = 1 * time.Second
	svc := NewBrokerAuthService(config, s)
	ctx := context.Background()

	// Create a broker with secret
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: []byte("test-secret-key-32-bytes-long!!"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	// Create a request with old timestamp
	oldTimestamp := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(HeaderBrokerID, brokerID)
	req.Header.Set(HeaderTimestamp, oldTimestamp)
	req.Header.Set(HeaderNonce, "test-nonce")
	req.Header.Set(HeaderSignature, "some-signature")

	_, err = svc.ValidateBrokerSignature(ctx, req)
	if err == nil {
		t.Error("Expected error for clock skew")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Errorf("Expected timestamp error, got: %v", err)
	}
}

func TestValidateBrokerSignature_MissingHeaders(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		setupReq    func(*http.Request)
		expectedErr string
	}{
		{
			name: "missing broker ID",
			setupReq: func(r *http.Request) {
				r.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
				r.Header.Set(HeaderSignature, "sig")
			},
			expectedErr: "missing X-Scion-Broker-ID",
		},
		{
			name: "missing timestamp",
			setupReq: func(r *http.Request) {
				r.Header.Set(HeaderBrokerID, "host-id")
				r.Header.Set(HeaderSignature, "sig")
			},
			expectedErr: "missing X-Scion-Timestamp",
		},
		{
			name: "missing signature",
			setupReq: func(r *http.Request) {
				r.Header.Set(HeaderBrokerID, "host-id")
				r.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
			},
			expectedErr: "missing X-Scion-Signature",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
			tc.setupReq(req)

			_, err := svc.ValidateBrokerSignature(ctx, req)
			if err == nil {
				t.Error("Expected error")
			}
			if !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("Expected error containing '%s', got: %v", tc.expectedErr, err)
			}
		})
	}
}

func TestBrokerAuthMiddleware(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker with secret
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "middleware-test-host",
		Slug:    "middleware-test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	secretKey := []byte("middleware-secret-key-32-bytes!!")
	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("failed to create broker secret: %v", err)
	}

	// Create a handler that checks for broker identity
	var gotIdentity BrokerIdentity
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetBrokerIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// Wrap with middleware
	wrapped := BrokerAuthMiddleware(svc)(handler)

	// Test 1: Request without broker ID header should pass through
	t.Run("no broker header passes through", func(t *testing.T) {
		gotIdentity = nil
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}
		if gotIdentity != nil {
			t.Error("Expected no identity for unauthenticated request")
		}
	})

	// Test 2: Request with valid signature should set identity
	t.Run("valid signature sets identity", func(t *testing.T) {
		gotIdentity = nil
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := "test-nonce"

		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		req.Header.Set(HeaderBrokerID, brokerID)
		req.Header.Set(HeaderTimestamp, timestamp)
		req.Header.Set(HeaderNonce, nonce)

		// Compute signature
		canonicalString := svc.buildCanonicalString(req, timestamp, nonce)
		h := hmac.New(sha256.New, secretKey)
		h.Write(canonicalString)
		signature := base64.StdEncoding.EncodeToString(h.Sum(nil))
		req.Header.Set(HeaderSignature, signature)

		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", w.Code)
		}
		if gotIdentity == nil {
			t.Fatal("Expected identity to be set")
		}
		if gotIdentity.BrokerID() != brokerID {
			t.Errorf("BrokerID mismatch: got %s, want %s", gotIdentity.BrokerID(), brokerID)
		}
	})

	// Test 3: Request with invalid signature should return 401
	t.Run("invalid signature returns 401", func(t *testing.T) {
		gotIdentity = nil
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		req.Header.Set(HeaderBrokerID, brokerID)
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set(HeaderNonce, "nonce")
		req.Header.Set(HeaderSignature, "invalid-signature")

		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401, got %d", w.Code)
		}
	})
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Test Host", "test-host"},
		{"My-Host-Name", "my-host-name"},
		{"host123", "host123"},
		{"Host With   Spaces", "host-with---spaces"},
		{"Special!@#$Characters", "specialcharacters"},
		{"UPPERCASE", "uppercase"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := slugify(tc.input)
			if result != tc.expected {
				t.Errorf("slugify(%q) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

// TestGenerateAndStoreSecret tests the simplified secret generation for grove registration.
func TestGenerateAndStoreSecret(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker first (GenerateAndStoreSecret requires an existing broker)
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Generate secret
	secretKey, err := svc.GenerateAndStoreSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("GenerateAndStoreSecret failed: %v", err)
	}

	// Verify secret is valid base64
	secretBytes, err := base64.StdEncoding.DecodeString(secretKey)
	if err != nil {
		t.Fatalf("SecretKey should be valid base64: %v", err)
	}
	if len(secretBytes) != 32 {
		t.Errorf("SecretKey should be 32 bytes, got %d", len(secretBytes))
	}

	// Verify secret was stored
	storedSecret, err := s.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("failed to get stored secret: %v", err)
	}
	if storedSecret == nil {
		t.Fatal("expected secret to be stored")
	}
	if !bytes.Equal(storedSecret.SecretKey, secretBytes) {
		t.Error("stored secret doesn't match returned secret")
	}
}

// TestGenerateAndStoreSecret_ReturnsExistingSecret tests that calling GenerateAndStoreSecret
// multiple times for the same broker returns the existing secret.
func TestGenerateAndStoreSecret_ReturnsExistingSecret(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Generate secret first time
	secretKey1, err := svc.GenerateAndStoreSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("First GenerateAndStoreSecret failed: %v", err)
	}

	// Generate secret second time - should return same secret
	secretKey2, err := svc.GenerateAndStoreSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("Second GenerateAndStoreSecret failed: %v", err)
	}

	if secretKey1 != secretKey2 {
		t.Errorf("Expected same secret on re-registration, got different:\n  first:  %s\n  second: %s", secretKey1, secretKey2)
	}
}

// TestGenerateAndStoreSecret_RequiresBrokerID tests that empty brokerID is rejected.
func TestGenerateAndStoreSecret_RequiresBrokerID(t *testing.T) {
	svc, _ := setupTestBrokerAuthService(t)
	ctx := context.Background()

	_, err := svc.GenerateAndStoreSecret(ctx, "")
	if err == nil {
		t.Error("Expected error for empty brokerID")
	}
	if !strings.Contains(err.Error(), "brokerId is required") {
		t.Errorf("Expected 'brokerId is required' error, got: %v", err)
	}
}

// TestGenerateAndStoreSecret_CanBeUsedForHMACAuth tests the full flow:
// generate secret, then use it to authenticate a request.
func TestGenerateAndStoreSecret_CanBeUsedForHMACAuth(t *testing.T) {
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()

	// Create a broker
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "auth-test-host",
		Slug:    "auth-test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Generate secret
	secretKeyB64, err := svc.GenerateAndStoreSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("GenerateAndStoreSecret failed: %v", err)
	}

	secretKey, err := base64.StdEncoding.DecodeString(secretKeyB64)
	if err != nil {
		t.Fatalf("failed to decode secret: %v", err)
	}

	// Create a signed request using the secret
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "test-nonce-abc"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req.Header.Set(HeaderBrokerID, brokerID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	// Build canonical string and compute signature
	canonicalString := svc.buildCanonicalString(req, timestamp, nonce)
	h := hmac.New(sha256.New, secretKey)
	h.Write(canonicalString)
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))
	req.Header.Set(HeaderSignature, signature)

	// Validate the signature
	identity, err := svc.ValidateBrokerSignature(ctx, req)
	if err != nil {
		t.Fatalf("ValidateBrokerSignature failed: %v", err)
	}

	if identity.BrokerID() != brokerID {
		t.Errorf("BrokerID mismatch: got %s, want %s", identity.BrokerID(), brokerID)
	}
}
