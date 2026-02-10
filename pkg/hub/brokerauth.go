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

// Package hub provides the Scion Hub API server.
package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/store"
)

// BrokerAuthConfig holds broker authentication configuration.
type BrokerAuthConfig struct {
	// Enabled controls whether broker authentication is active.
	Enabled bool
	// MaxClockSkew is the maximum allowed time difference between client and server.
	MaxClockSkew time.Duration
	// EnableNonceCache enables replay attack prevention via nonce caching.
	EnableNonceCache bool
	// NonceCacheTTL is how long nonces are cached (should be > MaxClockSkew).
	NonceCacheTTL time.Duration
	// JoinTokenExpiry is how long join tokens remain valid.
	JoinTokenExpiry time.Duration
	// JoinTokenLength is the length of generated join tokens in bytes.
	JoinTokenLength int
	// SecretKeyLength is the length of generated secret keys in bytes.
	SecretKeyLength int
}

// DefaultBrokerAuthConfig returns the default broker authentication configuration.
func DefaultBrokerAuthConfig() BrokerAuthConfig {
	return BrokerAuthConfig{
		Enabled:          true,
		MaxClockSkew:     5 * time.Minute,
		EnableNonceCache: true, // Enabled by default for replay attack prevention
		NonceCacheTTL:    10 * time.Minute,
		JoinTokenExpiry:  1 * time.Hour,
		JoinTokenLength:  32,
		SecretKeyLength:  32, // 256 bits
	}
}

// BrokerAuthService handles broker registration and HMAC-based authentication.
type BrokerAuthService struct {
	config BrokerAuthConfig
	store  store.Store
	nonces *NonceCache
}

// NonceCache provides replay attack prevention by caching used nonces.
type NonceCache struct {
	mu     sync.RWMutex
	nonces map[string]time.Time
	ttl    time.Duration
}

// NewNonceCache creates a new nonce cache.
func NewNonceCache(ttl time.Duration) *NonceCache {
	nc := &NonceCache{
		nonces: make(map[string]time.Time),
		ttl:    ttl,
	}
	// Start cleanup goroutine
	go nc.cleanup()
	return nc
}

// Add adds a nonce to the cache. Returns false if nonce already exists.
func (nc *NonceCache) Add(nonce string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()

	if _, exists := nc.nonces[nonce]; exists {
		return false
	}
	nc.nonces[nonce] = time.Now()
	return true
}

// cleanup periodically removes expired nonces.
func (nc *NonceCache) cleanup() {
	ticker := time.NewTicker(nc.ttl / 2)
	for range ticker.C {
		nc.mu.Lock()
		cutoff := time.Now().Add(-nc.ttl)
		for nonce, addedAt := range nc.nonces {
			if addedAt.Before(cutoff) {
				delete(nc.nonces, nonce)
			}
		}
		nc.mu.Unlock()
	}
}

// NewBrokerAuthService creates a new broker authentication service.
func NewBrokerAuthService(config BrokerAuthConfig, s store.Store) *BrokerAuthService {
	svc := &BrokerAuthService{
		config: config,
		store:  s,
	}
	if config.EnableNonceCache {
		svc.nonces = NewNonceCache(config.NonceCacheTTL)
	}
	return svc
}

// =============================================================================
// Broker Registration
// =============================================================================

// CreateBrokerRegistrationRequest is the request body for POST /api/v1/brokers.
type CreateBrokerRegistrationRequest struct {
	Name         string            `json:"name"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// CreateBrokerRegistrationResponse is the response for POST /api/v1/brokers.
type CreateBrokerRegistrationResponse struct {
	BrokerID string    `json:"brokerId"`
	JoinToken string    `json:"joinToken"` // scion_join_<base64>
	ExpiresAt time.Time `json:"expiresAt"`
}

// BrokerJoinRequest is the request body for POST /api/v1/brokers/join.
type BrokerJoinRequest struct {
	BrokerID     string                `json:"brokerId"`
	JoinToken    string                `json:"joinToken"`
	Hostname     string                `json:"hostname"`
	Version      string                `json:"version"`
	Capabilities []string              `json:"capabilities,omitempty"`
	Profiles     []store.BrokerProfile `json:"profiles,omitempty"`
}

// BrokerJoinResponse is the response for POST /api/v1/brokers/join.
type BrokerJoinResponse struct {
	SecretKey   string `json:"secretKey"` // Base64-encoded 256-bit key
	HubEndpoint string `json:"hubEndpoint"`
	BrokerID string `json:"brokerId"`
}

// JoinTokenPrefix is the prefix for join tokens.
const JoinTokenPrefix = "scion_join_"

// CreateBrokerRegistration creates a new broker with a join token.
// Requires admin authentication.
func (s *BrokerAuthService) CreateBrokerRegistration(ctx context.Context, req CreateBrokerRegistrationRequest, createdBy string) (*CreateBrokerRegistrationResponse, error) {
	if req.Name == "" {
		return nil, errors.New("name is required")
	}

	// Generate broker ID
	brokerID := uuid.New().String()

	// Create the runtime broker record
	broker := &store.RuntimeBroker{
		ID:        brokerID,
		Name:      req.Name,
		Slug:      slugify(req.Name),
		Status:    store.BrokerStatusOffline,
		Labels:    req.Labels,
		Created:   time.Now(),
		Updated:   time.Now(),
		CreatedBy: createdBy,
	}

	if err := s.store.CreateRuntimeBroker(ctx, broker); err != nil {
		return nil, fmt.Errorf("failed to create runtime broker: %w", err)
	}

	// Generate join token
	tokenBytes := make([]byte, s.config.JoinTokenLength)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("failed to generate join token: %w", err)
	}
	joinToken := JoinTokenPrefix + base64.URLEncoding.EncodeToString(tokenBytes)

	// Hash the token for storage
	tokenHash := sha256Hash(joinToken)

	// Calculate expiry
	expiresAt := time.Now().Add(s.config.JoinTokenExpiry)

	// Store the join token
	joinTokenRecord := &store.BrokerJoinToken{
		BrokerID:    brokerID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
		CreatedBy: createdBy,
	}

	if err := s.store.CreateJoinToken(ctx, joinTokenRecord); err != nil {
		// Clean up the broker record on failure
		_ = s.store.DeleteRuntimeBroker(ctx, brokerID)
		return nil, fmt.Errorf("failed to create join token: %w", err)
	}

	return &CreateBrokerRegistrationResponse{
		BrokerID:    brokerID,
		JoinToken: joinToken,
		ExpiresAt: expiresAt,
	}, nil
}

// CompleteBrokerJoin completes broker registration with join token exchange.
// Returns the shared secret for HMAC authentication.
func (s *BrokerAuthService) CompleteBrokerJoin(ctx context.Context, req BrokerJoinRequest, hubEndpoint string) (*BrokerJoinResponse, error) {
	if req.BrokerID == "" {
		return nil, errors.New("brokerId is required")
	}
	if req.JoinToken == "" {
		return nil, errors.New("joinToken is required")
	}

	// Hash the provided token
	tokenHash := sha256Hash(req.JoinToken)

	// Look up the join token
	joinToken, err := s.store.GetJoinToken(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("invalid join token")
		}
		return nil, fmt.Errorf("failed to validate join token: %w", err)
	}

	// Verify broker ID matches
	if joinToken.BrokerID != req.BrokerID {
		return nil, fmt.Errorf("join token does not match broker")
	}

	// Check expiry
	if time.Now().After(joinToken.ExpiresAt) {
		// Delete expired token
		_ = s.store.DeleteJoinToken(ctx, joinToken.BrokerID)
		return nil, fmt.Errorf("join token has expired")
	}

	// Generate shared secret
	secretKey := make([]byte, s.config.SecretKeyLength)
	if _, err := rand.Read(secretKey); err != nil {
		return nil, fmt.Errorf("failed to generate secret key: %w", err)
	}

	// Store the broker secret
	brokerSecret := &store.BrokerSecret{
		BrokerID:    req.BrokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.BrokerSecretStatusActive,
	}

	if err := s.store.CreateBrokerSecret(ctx, brokerSecret); err != nil {
		return nil, fmt.Errorf("failed to store broker secret: %w", err)
	}

	// Update the runtime broker with connection info
	broker, err := s.store.GetRuntimeBroker(ctx, req.BrokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get runtime broker: %w", err)
	}

	broker.Version = req.Version
	broker.Status = store.BrokerStatusOnline
	broker.ConnectionState = "connected"
	broker.LastHeartbeat = time.Now()
	broker.Updated = time.Now()

	// Update profiles if provided in the join request
	if len(req.Profiles) > 0 {
		broker.Profiles = req.Profiles
	}

	if err := s.store.UpdateRuntimeBroker(ctx, broker); err != nil {
		return nil, fmt.Errorf("failed to update runtime broker: %w", err)
	}

	// Delete the used join token
	_ = s.store.DeleteJoinToken(ctx, joinToken.BrokerID)

	return &BrokerJoinResponse{
		SecretKey:   base64.StdEncoding.EncodeToString(secretKey),
		HubEndpoint: hubEndpoint,
		BrokerID:      req.BrokerID,
	}, nil
}

// GenerateAndStoreSecret generates a new HMAC secret for an existing broker.
// This is used for simplified registration flows where a join token is not required.
// Returns the base64-encoded secret key.
func (s *BrokerAuthService) GenerateAndStoreSecret(ctx context.Context, brokerID string) (string, error) {
	if brokerID == "" {
		return "", errors.New("brokerId is required")
	}

	// Check if broker already has a secret
	existingSecret, err := s.store.GetBrokerSecret(ctx, brokerID)
	if err == nil && existingSecret != nil {
		// Broker already has a secret - return it (re-registration case)
		return base64.StdEncoding.EncodeToString(existingSecret.SecretKey), nil
	}

	// Generate shared secret
	secretKey := make([]byte, s.config.SecretKeyLength)
	if _, err := rand.Read(secretKey); err != nil {
		return "", fmt.Errorf("failed to generate secret key: %w", err)
	}

	// Store the broker secret
	brokerSecret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: secretKey,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.BrokerSecretStatusActive,
	}

	if err := s.store.CreateBrokerSecret(ctx, brokerSecret); err != nil {
		return "", fmt.Errorf("failed to store broker secret: %w", err)
	}

	return base64.StdEncoding.EncodeToString(secretKey), nil
}

// =============================================================================
// HMAC Signature Validation
// =============================================================================

// HMAC authentication headers as per runtime-broker-auth.md
const (
	HeaderBrokerID        = "X-Scion-Broker-ID"
	HeaderTimestamp     = "X-Scion-Timestamp"
	HeaderNonce         = "X-Scion-Nonce"
	HeaderSignature     = "X-Scion-Signature"
	HeaderSignedHeaders = "X-Scion-Signed-Headers"
)

// ValidateBrokerSignature validates an HMAC-signed request from a Runtime Broker.
func (s *BrokerAuthService) ValidateBrokerSignature(ctx context.Context, r *http.Request) (BrokerIdentity, error) {
	// Extract required headers
	brokerID := r.Header.Get(HeaderBrokerID)
	if brokerID == "" {
		return nil, errors.New("missing X-Scion-Broker-ID header")
	}

	timestamp := r.Header.Get(HeaderTimestamp)
	if timestamp == "" {
		return nil, errors.New("missing X-Scion-Timestamp header")
	}

	signature := r.Header.Get(HeaderSignature)
	if signature == "" {
		return nil, errors.New("missing X-Scion-Signature header")
	}

	// Parse and validate timestamp
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp format: %w", err)
	}

	requestTime := time.Unix(ts, 0)
	clockSkew := time.Since(requestTime)
	if clockSkew < 0 {
		clockSkew = -clockSkew
	}
	if clockSkew > s.config.MaxClockSkew {
		return nil, fmt.Errorf("timestamp outside acceptable range (skew: %v)", clockSkew)
	}

	// Validate nonce if enabled
	nonce := r.Header.Get(HeaderNonce)
	if s.nonces != nil && nonce != "" {
		if !s.nonces.Add(nonce) {
			return nil, errors.New("nonce already used (possible replay attack)")
		}
	}

	// Get the broker's secret
	brokerSecret, err := s.store.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown broker: %s", brokerID)
		}
		return nil, fmt.Errorf("failed to get broker secret: %w", err)
	}

	// Check if secret is active
	if brokerSecret.Status != store.BrokerSecretStatusActive {
		return nil, fmt.Errorf("broker secret is %s", brokerSecret.Status)
	}

	// Check expiry
	if !brokerSecret.ExpiresAt.IsZero() && time.Now().After(brokerSecret.ExpiresAt) {
		return nil, errors.New("broker secret has expired")
	}

	// Build canonical string and verify signature
	canonicalString := s.buildCanonicalString(r, timestamp, nonce)
	expectedSig := computeHMAC(brokerSecret.SecretKey, canonicalString)
	expectedSigB64 := base64.StdEncoding.EncodeToString(expectedSig)

	if !hmac.Equal([]byte(signature), []byte(expectedSigB64)) {
		return nil, errors.New("invalid signature")
	}

	return NewBrokerIdentity(brokerID), nil
}

// buildCanonicalString builds the canonical string for HMAC signing.
// Format: METHOD\nPATH\nQUERY\nTIMESTAMP\nNONCE\nSIGNED_HEADERS\nBODY_HASH
func (s *BrokerAuthService) buildCanonicalString(r *http.Request, timestamp, nonce string) []byte {
	var buf bytes.Buffer

	// HTTP method
	buf.WriteString(r.Method)
	buf.WriteByte('\n')

	// Request path
	buf.WriteString(r.URL.Path)
	buf.WriteByte('\n')

	// Query string (sorted)
	buf.WriteString(r.URL.RawQuery)
	buf.WriteByte('\n')

	// Timestamp
	buf.WriteString(timestamp)
	buf.WriteByte('\n')

	// Nonce
	buf.WriteString(nonce)
	buf.WriteByte('\n')

	// Signed headers (if specified)
	signedHeaders := r.Header.Get(HeaderSignedHeaders)
	if signedHeaders != "" {
		// Headers are listed as semicolon-separated names
		headerNames := strings.Split(signedHeaders, ";")
		for _, name := range headerNames {
			name = strings.TrimSpace(name)
			value := r.Header.Get(name)
			buf.WriteString(strings.ToLower(name))
			buf.WriteByte(':')
			buf.WriteString(strings.TrimSpace(value))
			buf.WriteByte('\n')
		}
	}

	// Body hash (SHA-256 of request body)
	if r.Body != nil && r.ContentLength > 0 {
		// We need to read and restore the body
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			bodyHash := sha256.Sum256(bodyBytes)
			buf.WriteString(base64.StdEncoding.EncodeToString(bodyHash[:]))
		}
	}

	return buf.Bytes()
}

// SignRequest signs an outgoing HTTP request with HMAC.
// Used by Runtime Brokers when calling the Hub API.
func (s *BrokerAuthService) SignRequest(r *http.Request, brokerID string, secret []byte) error {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Generate nonce
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	nonce := base64.URLEncoding.EncodeToString(nonceBytes)

	// Set headers
	r.Header.Set(HeaderBrokerID, brokerID)
	r.Header.Set(HeaderTimestamp, timestamp)
	r.Header.Set(HeaderNonce, nonce)

	// Build canonical string and compute signature
	canonicalString := s.buildCanonicalString(r, timestamp, nonce)
	sig := computeHMAC(secret, canonicalString)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	r.Header.Set(HeaderSignature, sigB64)

	return nil
}

// =============================================================================
// Secret Rotation
// =============================================================================

// RotateSecretRequest is the request body for POST /api/v1/brokers/{id}/rotate-secret.
type RotateSecretRequest struct {
	// GracePeriod is how long the old secret remains valid after rotation.
	// Defaults to 5 minutes if not specified.
	GracePeriod time.Duration `json:"gracePeriod,omitempty"`
}

// RotateSecretResponse is the response for POST /api/v1/brokers/{id}/rotate-secret.
type RotateSecretResponse struct {
	SecretKey   string    `json:"secretKey"`   // Base64-encoded new secret
	RotatedAt   time.Time `json:"rotatedAt"`
	GracePeriod string    `json:"gracePeriod"` // Duration string
}

// RotateBrokerSecret generates a new secret for a broker.
// The old secret is marked as deprecated and remains valid for the grace period.
// Note: Current schema only supports one secret per broker, so this replaces immediately.
// TODO: Add schema migration to support multiple secrets per broker for true dual-secret rotation.
func (s *BrokerAuthService) RotateBrokerSecret(ctx context.Context, brokerID string, gracePeriod time.Duration) (*RotateSecretResponse, error) {
	if gracePeriod <= 0 {
		gracePeriod = 5 * time.Minute
	}

	// Get existing secret
	existingSecret, err := s.store.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get existing secret: %w", err)
	}

	// Generate new secret
	newSecretKey := make([]byte, s.config.SecretKeyLength)
	if _, err := rand.Read(newSecretKey); err != nil {
		return nil, fmt.Errorf("failed to generate new secret: %w", err)
	}

	now := time.Now()

	// Update the secret with new key
	// Note: In a full implementation with multi-secret support, we would:
	// 1. Mark old secret as deprecated with expiry = now + gracePeriod
	// 2. Create new secret with status active
	existingSecret.SecretKey = newSecretKey
	existingSecret.RotatedAt = now
	existingSecret.Status = store.BrokerSecretStatusActive

	if err := s.store.UpdateBrokerSecret(ctx, existingSecret); err != nil {
		return nil, fmt.Errorf("failed to update secret: %w", err)
	}

	return &RotateSecretResponse{
		SecretKey:   base64.StdEncoding.EncodeToString(newSecretKey),
		RotatedAt:   now,
		GracePeriod: gracePeriod.String(),
	}, nil
}

// ValidateBrokerSignatureWithRotation validates a request trying multiple secrets.
// This supports the grace period during secret rotation where both old and new
// secrets are valid.
func (s *BrokerAuthService) ValidateBrokerSignatureWithRotation(ctx context.Context, r *http.Request) (BrokerIdentity, error) {
	// Extract required headers
	brokerID := r.Header.Get(HeaderBrokerID)
	if brokerID == "" {
		return nil, errors.New("missing X-Scion-Broker-ID header")
	}

	// Get all active secrets for this broker
	secrets, err := s.store.GetActiveSecrets(ctx, brokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get broker secrets: %w", err)
	}

	if len(secrets) == 0 {
		return nil, fmt.Errorf("unknown broker: %s", brokerID)
	}

	// Try each secret until one validates
	var lastErr error
	for _, secret := range secrets {
		// Skip expired secrets
		if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
			continue
		}

		identity, err := s.validateWithSecret(ctx, r, brokerID, secret.SecretKey)
		if err == nil {
			return identity, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no valid secrets found")
}

// validateWithSecret validates a request using a specific secret key.
func (s *BrokerAuthService) validateWithSecret(ctx context.Context, r *http.Request, brokerID string, secretKey []byte) (BrokerIdentity, error) {
	timestamp := r.Header.Get(HeaderTimestamp)
	if timestamp == "" {
		return nil, errors.New("missing X-Scion-Timestamp header")
	}

	signature := r.Header.Get(HeaderSignature)
	if signature == "" {
		return nil, errors.New("missing X-Scion-Signature header")
	}

	// Parse and validate timestamp
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp format: %w", err)
	}

	requestTime := time.Unix(ts, 0)
	clockSkew := time.Since(requestTime)
	if clockSkew < 0 {
		clockSkew = -clockSkew
	}
	if clockSkew > s.config.MaxClockSkew {
		return nil, fmt.Errorf("timestamp outside acceptable range (skew: %v)", clockSkew)
	}

	// Validate nonce if enabled (only check once, not per-secret)
	nonce := r.Header.Get(HeaderNonce)

	// Build canonical string and verify signature
	canonicalString := s.buildCanonicalString(r, timestamp, nonce)
	expectedSig := computeHMAC(secretKey, canonicalString)
	expectedSigB64 := base64.StdEncoding.EncodeToString(expectedSig)

	if !hmac.Equal([]byte(signature), []byte(expectedSigB64)) {
		return nil, errors.New("invalid signature")
	}

	// Only add nonce to cache after successful validation
	if s.nonces != nil && nonce != "" {
		if !s.nonces.Add(nonce) {
			return nil, errors.New("nonce already used (possible replay attack)")
		}
	}

	return NewBrokerIdentity(brokerID), nil
}

// =============================================================================
// Helper Functions
// =============================================================================

// computeHMAC computes HMAC-SHA256.
func computeHMAC(secret, data []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hash returns the hex-encoded SHA-256 hash of a string.
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

// slugify converts a name to a URL-safe slug.
func slugify(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	// Remove non-alphanumeric characters except hyphens
	var result strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// =============================================================================
// Middleware
// =============================================================================

// BrokerAuthMiddleware creates middleware for HMAC-based broker authentication.
// This runs AFTER UnifiedAuthMiddleware and checks for X-Scion-Broker-ID header.
func BrokerAuthMiddleware(svc *BrokerAuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip if broker auth service is not configured
			if svc == nil || !svc.config.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			// Skip if not a broker-authenticated request
			brokerID := r.Header.Get(HeaderBrokerID)
			if brokerID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Validate HMAC signature
			identity, err := svc.ValidateBrokerSignature(r.Context(), r)
			if err != nil {
				writeBrokerAuthError(w, err.Error())
				return
			}

			// Set both broker-specific and generic identity contexts
			ctx := contextWithBrokerIdentity(r.Context(), identity)
			ctx = contextWithIdentity(ctx, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeBrokerAuthError writes a broker authentication error response.
func writeBrokerAuthError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, message, nil)
}
