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

// HostAuthConfig holds host authentication configuration.
type HostAuthConfig struct {
	// Enabled controls whether host authentication is active.
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

// DefaultHostAuthConfig returns the default host authentication configuration.
func DefaultHostAuthConfig() HostAuthConfig {
	return HostAuthConfig{
		Enabled:          true,
		MaxClockSkew:     5 * time.Minute,
		EnableNonceCache: true, // Enabled by default for replay attack prevention
		NonceCacheTTL:    10 * time.Minute,
		JoinTokenExpiry:  1 * time.Hour,
		JoinTokenLength:  32,
		SecretKeyLength:  32, // 256 bits
	}
}

// HostAuthService handles host registration and HMAC-based authentication.
type HostAuthService struct {
	config HostAuthConfig
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

// NewHostAuthService creates a new host authentication service.
func NewHostAuthService(config HostAuthConfig, s store.Store) *HostAuthService {
	svc := &HostAuthService{
		config: config,
		store:  s,
	}
	if config.EnableNonceCache {
		svc.nonces = NewNonceCache(config.NonceCacheTTL)
	}
	return svc
}

// =============================================================================
// Host Registration
// =============================================================================

// CreateHostRegistrationRequest is the request body for POST /api/v1/hosts.
type CreateHostRegistrationRequest struct {
	Name         string            `json:"name"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// CreateHostRegistrationResponse is the response for POST /api/v1/hosts.
type CreateHostRegistrationResponse struct {
	HostID    string    `json:"hostId"`
	JoinToken string    `json:"joinToken"` // scion_join_<base64>
	ExpiresAt time.Time `json:"expiresAt"`
}

// HostJoinRequest is the request body for POST /api/v1/hosts/join.
type HostJoinRequest struct {
	HostID       string   `json:"hostId"`
	JoinToken    string   `json:"joinToken"`
	Hostname     string   `json:"hostname"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// HostJoinResponse is the response for POST /api/v1/hosts/join.
type HostJoinResponse struct {
	SecretKey   string `json:"secretKey"` // Base64-encoded 256-bit key
	HubEndpoint string `json:"hubEndpoint"`
	HostID      string `json:"hostId"`
}

// JoinTokenPrefix is the prefix for join tokens.
const JoinTokenPrefix = "scion_join_"

// CreateHostRegistration creates a new host with a join token.
// Requires admin authentication.
func (s *HostAuthService) CreateHostRegistration(ctx context.Context, req CreateHostRegistrationRequest, createdBy string) (*CreateHostRegistrationResponse, error) {
	if req.Name == "" {
		return nil, errors.New("name is required")
	}

	// Generate host ID
	hostID := uuid.New().String()

	// Create the runtime host record
	host := &store.RuntimeHost{
		ID:          hostID,
		Name:        req.Name,
		Slug:        slugify(req.Name),
		Mode:        store.HostModeConnected,
		Status:      store.HostStatusOffline,
		Labels:      req.Labels,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := s.store.CreateRuntimeHost(ctx, host); err != nil {
		return nil, fmt.Errorf("failed to create runtime host: %w", err)
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
	joinTokenRecord := &store.HostJoinToken{
		HostID:    hostID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
		CreatedBy: createdBy,
	}

	if err := s.store.CreateJoinToken(ctx, joinTokenRecord); err != nil {
		// Clean up the host record on failure
		_ = s.store.DeleteRuntimeHost(ctx, hostID)
		return nil, fmt.Errorf("failed to create join token: %w", err)
	}

	return &CreateHostRegistrationResponse{
		HostID:    hostID,
		JoinToken: joinToken,
		ExpiresAt: expiresAt,
	}, nil
}

// CompleteHostJoin completes host registration with join token exchange.
// Returns the shared secret for HMAC authentication.
func (s *HostAuthService) CompleteHostJoin(ctx context.Context, req HostJoinRequest, hubEndpoint string) (*HostJoinResponse, error) {
	if req.HostID == "" {
		return nil, errors.New("hostId is required")
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

	// Verify host ID matches
	if joinToken.HostID != req.HostID {
		return nil, fmt.Errorf("join token does not match host")
	}

	// Check expiry
	if time.Now().After(joinToken.ExpiresAt) {
		// Delete expired token
		_ = s.store.DeleteJoinToken(ctx, joinToken.HostID)
		return nil, fmt.Errorf("join token has expired")
	}

	// Generate shared secret
	secretKey := make([]byte, s.config.SecretKeyLength)
	if _, err := rand.Read(secretKey); err != nil {
		return nil, fmt.Errorf("failed to generate secret key: %w", err)
	}

	// Store the host secret
	hostSecret := &store.HostSecret{
		HostID:    req.HostID,
		SecretKey: secretKey,
		Algorithm: store.HostSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.HostSecretStatusActive,
	}

	if err := s.store.CreateHostSecret(ctx, hostSecret); err != nil {
		return nil, fmt.Errorf("failed to store host secret: %w", err)
	}

	// Update the runtime host with connection info
	host, err := s.store.GetRuntimeHost(ctx, req.HostID)
	if err != nil {
		return nil, fmt.Errorf("failed to get runtime host: %w", err)
	}

	host.Version = req.Version
	host.Status = store.HostStatusOnline
	host.ConnectionState = "connected"
	host.LastHeartbeat = time.Now()
	host.Updated = time.Now()

	if err := s.store.UpdateRuntimeHost(ctx, host); err != nil {
		return nil, fmt.Errorf("failed to update runtime host: %w", err)
	}

	// Delete the used join token
	_ = s.store.DeleteJoinToken(ctx, joinToken.HostID)

	return &HostJoinResponse{
		SecretKey:   base64.StdEncoding.EncodeToString(secretKey),
		HubEndpoint: hubEndpoint,
		HostID:      req.HostID,
	}, nil
}

// =============================================================================
// HMAC Signature Validation
// =============================================================================

// HMAC authentication headers as per runtime-host-auth.md
const (
	HeaderHostID        = "X-Scion-Host-ID"
	HeaderTimestamp     = "X-Scion-Timestamp"
	HeaderNonce         = "X-Scion-Nonce"
	HeaderSignature     = "X-Scion-Signature"
	HeaderSignedHeaders = "X-Scion-Signed-Headers"
)

// ValidateHostSignature validates an HMAC-signed request from a Runtime Host.
func (s *HostAuthService) ValidateHostSignature(ctx context.Context, r *http.Request) (HostIdentity, error) {
	// Extract required headers
	hostID := r.Header.Get(HeaderHostID)
	if hostID == "" {
		return nil, errors.New("missing X-Scion-Host-ID header")
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

	// Get the host's secret
	hostSecret, err := s.store.GetHostSecret(ctx, hostID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("unknown host: %s", hostID)
		}
		return nil, fmt.Errorf("failed to get host secret: %w", err)
	}

	// Check if secret is active
	if hostSecret.Status != store.HostSecretStatusActive {
		return nil, fmt.Errorf("host secret is %s", hostSecret.Status)
	}

	// Check expiry
	if !hostSecret.ExpiresAt.IsZero() && time.Now().After(hostSecret.ExpiresAt) {
		return nil, errors.New("host secret has expired")
	}

	// Build canonical string and verify signature
	canonicalString := s.buildCanonicalString(r, timestamp, nonce)
	expectedSig := computeHMAC(hostSecret.SecretKey, canonicalString)
	expectedSigB64 := base64.StdEncoding.EncodeToString(expectedSig)

	if !hmac.Equal([]byte(signature), []byte(expectedSigB64)) {
		return nil, errors.New("invalid signature")
	}

	return NewHostIdentity(hostID), nil
}

// buildCanonicalString builds the canonical string for HMAC signing.
// Format: METHOD\nPATH\nQUERY\nTIMESTAMP\nNONCE\nSIGNED_HEADERS\nBODY_HASH
func (s *HostAuthService) buildCanonicalString(r *http.Request, timestamp, nonce string) []byte {
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
// Used by Runtime Hosts when calling the Hub API.
func (s *HostAuthService) SignRequest(r *http.Request, hostID string, secret []byte) error {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Generate nonce
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	nonce := base64.URLEncoding.EncodeToString(nonceBytes)

	// Set headers
	r.Header.Set(HeaderHostID, hostID)
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

// RotateSecretRequest is the request body for POST /api/v1/hosts/{id}/rotate-secret.
type RotateSecretRequest struct {
	// GracePeriod is how long the old secret remains valid after rotation.
	// Defaults to 5 minutes if not specified.
	GracePeriod time.Duration `json:"gracePeriod,omitempty"`
}

// RotateSecretResponse is the response for POST /api/v1/hosts/{id}/rotate-secret.
type RotateSecretResponse struct {
	SecretKey   string    `json:"secretKey"`   // Base64-encoded new secret
	RotatedAt   time.Time `json:"rotatedAt"`
	GracePeriod string    `json:"gracePeriod"` // Duration string
}

// RotateHostSecret generates a new secret for a host.
// The old secret is marked as deprecated and remains valid for the grace period.
// Note: Current schema only supports one secret per host, so this replaces immediately.
// TODO: Add schema migration to support multiple secrets per host for true dual-secret rotation.
func (s *HostAuthService) RotateHostSecret(ctx context.Context, hostID string, gracePeriod time.Duration) (*RotateSecretResponse, error) {
	if gracePeriod <= 0 {
		gracePeriod = 5 * time.Minute
	}

	// Get existing secret
	existingSecret, err := s.store.GetHostSecret(ctx, hostID)
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
	existingSecret.Status = store.HostSecretStatusActive

	if err := s.store.UpdateHostSecret(ctx, existingSecret); err != nil {
		return nil, fmt.Errorf("failed to update secret: %w", err)
	}

	return &RotateSecretResponse{
		SecretKey:   base64.StdEncoding.EncodeToString(newSecretKey),
		RotatedAt:   now,
		GracePeriod: gracePeriod.String(),
	}, nil
}

// ValidateHostSignatureWithRotation validates a request trying multiple secrets.
// This supports the grace period during secret rotation where both old and new
// secrets are valid.
func (s *HostAuthService) ValidateHostSignatureWithRotation(ctx context.Context, r *http.Request) (HostIdentity, error) {
	// Extract required headers
	hostID := r.Header.Get(HeaderHostID)
	if hostID == "" {
		return nil, errors.New("missing X-Scion-Host-ID header")
	}

	// Get all active secrets for this host
	secrets, err := s.store.GetActiveSecrets(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("failed to get host secrets: %w", err)
	}

	if len(secrets) == 0 {
		return nil, fmt.Errorf("unknown host: %s", hostID)
	}

	// Try each secret until one validates
	var lastErr error
	for _, secret := range secrets {
		// Skip expired secrets
		if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
			continue
		}

		identity, err := s.validateWithSecret(ctx, r, hostID, secret.SecretKey)
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
func (s *HostAuthService) validateWithSecret(ctx context.Context, r *http.Request, hostID string, secretKey []byte) (HostIdentity, error) {
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

	return NewHostIdentity(hostID), nil
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

// HostAuthMiddleware creates middleware for HMAC-based host authentication.
// This runs AFTER UnifiedAuthMiddleware and checks for X-Scion-Host-ID header.
func HostAuthMiddleware(svc *HostAuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip if host auth service is not configured
			if svc == nil || !svc.config.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			// Skip if not a host-authenticated request
			hostID := r.Header.Get(HeaderHostID)
			if hostID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Validate HMAC signature
			identity, err := svc.ValidateHostSignature(r.Context(), r)
			if err != nil {
				writeHostAuthError(w, err.Error())
				return
			}

			// Set both host-specific and generic identity contexts
			ctx := contextWithHostIdentity(r.Context(), identity)
			ctx = contextWithIdentity(ctx, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeHostAuthError writes a host authentication error response.
func writeHostAuthError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusUnauthorized, ErrCodeHostAuthFailed, message, nil)
}
