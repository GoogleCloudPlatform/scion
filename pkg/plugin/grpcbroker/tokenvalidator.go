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

package grpcbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Google OIDC constants.
const (
	// GoogleIssuerV1 and GoogleIssuerV2 are the two issuers Google uses for
	// ID tokens. Both must be accepted per Google's documentation.
	GoogleIssuerV1 = "accounts.google.com"
	GoogleIssuerV2 = "https://accounts.google.com"

	// GoogleJWKSURL is the endpoint for Google's public JWKS keys.
	GoogleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

	// defaultJWKSRefreshInterval is how often the JWKS key set is refreshed.
	defaultJWKSRefreshInterval = 1 * time.Hour

	// defaultJWKSFetchTimeout is the HTTP timeout for JWKS endpoint fetches.
	defaultJWKSFetchTimeout = 10 * time.Second
)

// GoogleIDTokenValidatorConfig configures the concrete Google ID token
// validator for the bridge's gRPC server.
type GoogleIDTokenValidatorConfig struct {
	// Audience is the expected audience claim — typically the bridge's service
	// URL. Required; tokens with a different audience are rejected.
	Audience string

	// AuthorizedSubjects is the set of authorized service account emails
	// (the "sub" or "email" claim). When non-empty, only tokens from these
	// principals are accepted. When empty, any valid Google token with the
	// correct audience is accepted (suitable only when the audience is
	// purpose-specific and cannot be obtained by unrelated services).
	AuthorizedSubjects []string

	// JWKSURL overrides the Google JWKS endpoint (for testing).
	JWKSURL string

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

// GoogleIDTokenValidator validates Google OIDC ID tokens by verifying the
// signature against Google's JWKS, then checking issuer, audience, expiry,
// and authorized subject (email). This is the concrete server-side validator
// for the bridge's gRPC auth interceptor.
type GoogleIDTokenValidator struct {
	audience           string
	authorizedSubjects map[string]bool
	jwksURL            string
	logger             *slog.Logger
	algorithms         []jose.SignatureAlgorithm

	mu        sync.RWMutex
	jwks      *jose.JSONWebKeySet
	fetchedAt time.Time
}

// googleIDTokenClaims is the claims shape for Google ID tokens.
type googleIDTokenClaims struct {
	jwt.Claims

	// Email is the service account email for service-to-service tokens.
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"email_verified,omitempty"`

	// AZP is the authorized party — the OAuth2 client ID that obtained
	// the token. For service accounts, this is the SA's unique ID.
	AZP string `json:"azp,omitempty"`
}

// NewGoogleIDTokenValidator creates a validator that cryptographically
// verifies Google ID tokens using Google's public JWKS keys.
func NewGoogleIDTokenValidator(cfg GoogleIDTokenValidatorConfig) (*GoogleIDTokenValidator, error) {
	if cfg.Audience == "" {
		return nil, fmt.Errorf("audience is required for Google ID token validator")
	}

	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		jwksURL = GoogleJWKSURL
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	subjects := make(map[string]bool, len(cfg.AuthorizedSubjects))
	for _, s := range cfg.AuthorizedSubjects {
		subjects[s] = true
	}

	return &GoogleIDTokenValidator{
		audience:           cfg.Audience,
		authorizedSubjects: subjects,
		jwksURL:            jwksURL,
		logger:             logger.With("component", "google-id-token-validator"),
		algorithms: []jose.SignatureAlgorithm{
			jose.RS256,
			jose.ES256,
		},
	}, nil
}

// ValidateToken verifies a Google ID token's signature, issuer, audience,
// expiry, and authorized subject.
func (v *GoogleIDTokenValidator) ValidateToken(ctx context.Context, tokenString string) error {
	// 1. Parse JWT with algorithm pinning.
	tok, err := jwt.ParseSigned(tokenString, v.algorithms)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "invalid token format: %v", err)
	}

	// 2. Extract key ID from header.
	if len(tok.Headers) == 0 {
		return status.Error(codes.Unauthenticated, "token has no headers")
	}
	kid := tok.Headers[0].KeyID
	if kid == "" {
		return status.Error(codes.Unauthenticated, "token has no key ID (kid)")
	}

	// 3. Fetch JWKS and find the signing key.
	keys, err := v.getSigningKey(ctx, kid)
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "key resolution failed: %v", err)
	}

	// 4. Try each matching key for signature verification and claims extraction.
	var claims googleIDTokenClaims
	var verifyErr error
	for _, key := range keys {
		verifyErr = tok.Claims(key, &claims)
		if verifyErr == nil {
			break
		}
	}
	if verifyErr != nil {
		return status.Errorf(codes.Unauthenticated, "token signature verification failed: %v", verifyErr)
	}

	// 5. Validate issuer — must be Google.
	iss := claims.Issuer
	if iss != GoogleIssuerV1 && iss != GoogleIssuerV2 {
		return status.Errorf(codes.Unauthenticated, "invalid issuer %q: expected %q or %q",
			iss, GoogleIssuerV1, GoogleIssuerV2)
	}

	// 6. Validate audience.
	if !claims.Audience.Contains(v.audience) {
		return status.Errorf(codes.Unauthenticated, "audience mismatch: token has %v, expected %q",
			claims.Audience, v.audience)
	}

	// 7. Validate time (expiry and not-before).
	now := time.Now()
	if claims.Expiry != nil && now.After(claims.Expiry.Time()) {
		return status.Error(codes.Unauthenticated, "token is expired")
	}
	if claims.NotBefore != nil && now.Before(claims.NotBefore.Time()) {
		return status.Error(codes.Unauthenticated, "token is not yet valid")
	}

	// 8. Authorize subject — check email claim against authorized principals.
	if len(v.authorizedSubjects) > 0 {
		email := claims.Email
		if email == "" {
			return status.Error(codes.PermissionDenied,
				"token has no email claim; cannot authorize service principal")
		}
		if !v.authorizedSubjects[email] {
			return status.Errorf(codes.PermissionDenied,
				"service principal %q is not authorized", email)
		}
		if !claims.EmailVerified {
			return status.Errorf(codes.PermissionDenied,
				"email %q is not verified", email)
		}
	}

	return nil
}

// getSigningKey fetches and caches Google's JWKS, returning keys matching kid.
func (v *GoogleIDTokenValidator) getSigningKey(ctx context.Context, kid string) ([]interface{}, error) {
	v.mu.RLock()
	if v.jwks != nil && time.Since(v.fetchedAt) < defaultJWKSRefreshInterval {
		keys := v.jwks.Key(kid)
		v.mu.RUnlock()
		if len(keys) > 0 {
			return keysToPublicKeys(keys), nil
		}
		// Key not found — may need refresh, fall through.
	} else {
		v.mu.RUnlock()
	}

	// Fetch fresh JWKS.
	v.mu.Lock()
	defer v.mu.Unlock()

	// Double-check after acquiring write lock.
	if v.jwks != nil && time.Since(v.fetchedAt) < defaultJWKSRefreshInterval {
		keys := v.jwks.Key(kid)
		if len(keys) > 0 {
			return keysToPublicKeys(keys), nil
		}
	}

	jwks, err := v.fetchJWKS(ctx)
	if err != nil {
		return nil, fmt.Errorf("JWKS fetch failed: %w", err)
	}
	v.jwks = jwks
	v.fetchedAt = time.Now()

	keys := jwks.Key(kid)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key found for kid %q", kid)
	}
	return keysToPublicKeys(keys), nil
}

// fetchJWKS fetches the JWKS from the configured URL.
func (v *GoogleIDTokenValidator) fetchJWKS(ctx context.Context) (*jose.JSONWebKeySet, error) {
	client := &http.Client{Timeout: defaultJWKSFetchTimeout}

	req, err := http.NewRequestWithContext(ctx, "GET", v.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read JWKS response: %w", err)
	}

	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}

	return &jwks, nil
}

// keysToPublicKeys extracts the public key from each JSONWebKey.
func keysToPublicKeys(keys []jose.JSONWebKey) []interface{} {
	result := make([]interface{}, 0, len(keys))
	for _, k := range keys {
		result = append(result, k.Key)
	}
	return result
}

// HMACTokenValidator validates HMAC-signed JWT tokens for environments where
// the bridge and Hub share a symmetric signing key. This is the simpler
// alternative to Google ID tokens for localhost/dev deployments.
type HMACTokenValidator struct {
	key                []byte
	expectedAudience   string
	authorizedSubjects map[string]bool
}

// HMACTokenValidatorConfig configures an HMAC-based token validator.
type HMACTokenValidatorConfig struct {
	// Key is the shared HMAC signing key (raw bytes).
	Key []byte

	// Audience is the expected audience claim.
	Audience string

	// AuthorizedSubjects limits which subjects (sub claim) are accepted.
	// When empty, any valid token with the correct audience is accepted.
	AuthorizedSubjects []string
}

// NewHMACTokenValidator creates a validator for HMAC-signed JWTs.
func NewHMACTokenValidator(cfg HMACTokenValidatorConfig) (*HMACTokenValidator, error) {
	if len(cfg.Key) == 0 {
		return nil, fmt.Errorf("HMAC key is required")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("audience is required")
	}
	subjects := make(map[string]bool, len(cfg.AuthorizedSubjects))
	for _, s := range cfg.AuthorizedSubjects {
		subjects[s] = true
	}
	return &HMACTokenValidator{
		key:                cfg.Key,
		expectedAudience:   cfg.Audience,
		authorizedSubjects: subjects,
	}, nil
}

// ValidateToken verifies an HMAC-signed JWT's signature, audience, expiry,
// and authorized subject.
func (v *HMACTokenValidator) ValidateToken(_ context.Context, tokenString string) error {
	tok, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "invalid token format: %v", err)
	}

	var claims jwt.Claims
	if err := tok.Claims(v.key, &claims); err != nil {
		return status.Errorf(codes.Unauthenticated, "token signature verification failed: %v", err)
	}

	// Validate audience.
	aud := jwt.Audience(claims.Audience)
	if !aud.Contains(v.expectedAudience) {
		return status.Errorf(codes.Unauthenticated, "audience mismatch: token has %v, expected %q",
			claims.Audience, v.expectedAudience)
	}

	// Validate time.
	now := time.Now()
	if claims.Expiry != nil && now.After(claims.Expiry.Time()) {
		return status.Error(codes.Unauthenticated, "token is expired")
	}
	if claims.NotBefore != nil && now.Before(claims.NotBefore.Time()) {
		return status.Error(codes.Unauthenticated, "token is not yet valid")
	}

	// Authorize subject.
	if len(v.authorizedSubjects) > 0 {
		sub := claims.Subject
		if sub == "" {
			return status.Error(codes.PermissionDenied, "token has no subject claim")
		}
		if !v.authorizedSubjects[sub] {
			return status.Errorf(codes.PermissionDenied, "subject %q is not authorized", sub)
		}
	}

	return nil
}

// --- Startup validation ---

// StandaloneAuthMode describes the authentication mode for a standalone bridge.
type StandaloneAuthMode string

const (
	// AuthModeGoogleIDToken uses Google OIDC ID tokens validated via JWKS.
	AuthModeGoogleIDToken StandaloneAuthMode = "google_id_token"

	// AuthModeHMAC uses HMAC-signed JWTs with a shared signing key.
	AuthModeHMAC StandaloneAuthMode = "hmac"

	// AuthModeLocalDev allows unauthenticated access for localhost-only dev.
	AuthModeLocalDev StandaloneAuthMode = "local_dev"
)

// StandaloneServerConfig holds the full configuration for a standalone bridge's
// gRPC server, including authentication mode, TLS, and authorized principals.
type StandaloneServerConfig struct {
	// AuthMode selects the authentication mode.
	AuthMode StandaloneAuthMode

	// Audience is the expected audience claim (required for all auth modes
	// except local_dev).
	Audience string

	// AuthorizedSubjects is the set of authorized service account emails
	// (for google_id_token) or subjects (for hmac). Required for
	// google_id_token mode.
	AuthorizedSubjects []string

	// HMACKey is the shared signing key for hmac mode.
	HMACKey []byte

	// JWKSURL overrides the Google JWKS endpoint (for testing).
	JWKSURL string

	// ListenAddress is the gRPC listen address (for fail-closed validation).
	ListenAddress string

	// TLS configuration.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string

	// Logger is optional.
	Logger *slog.Logger
}

// ValidateStandaloneServerConfig validates the standalone server config
// for security correctness. Returns an error if the config would create
// an insecure deployment.
func ValidateStandaloneServerConfig(cfg StandaloneServerConfig) error {
	var errs []string

	isLocal := isLocalAddress(cfg.ListenAddress)

	switch cfg.AuthMode {
	case AuthModeGoogleIDToken:
		if cfg.Audience == "" {
			errs = append(errs, "audience is required for google_id_token auth mode")
		}
		if len(cfg.AuthorizedSubjects) == 0 {
			errs = append(errs, "authorized_subjects is required for google_id_token auth mode; "+
				"specify the Hub service account email(s)")
		}

	case AuthModeHMAC:
		if cfg.Audience == "" {
			errs = append(errs, "audience is required for hmac auth mode")
		}
		if len(cfg.HMACKey) == 0 {
			errs = append(errs, "hmac_key is required for hmac auth mode")
		}

	case AuthModeLocalDev:
		if !isLocal {
			errs = append(errs, fmt.Sprintf("local_dev auth mode is only allowed for "+
				"local addresses, but listen address is %q", cfg.ListenAddress))
		}

	case "":
		if !isLocal {
			errs = append(errs, "auth_mode is required for non-local listen addresses; "+
				"set to google_id_token, hmac, or use a local address for development")
		}
		// Empty + local is allowed (implicit local_dev).

	default:
		errs = append(errs, fmt.Sprintf("unsupported auth_mode %q; supported: "+
			"google_id_token, hmac, local_dev", cfg.AuthMode))
	}

	// Validate TLS field consistency.
	if err := validateTLSFields(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile); err != nil {
		errs = append(errs, err.Error())
	}

	// For non-local addresses without TLS, warn or error.
	if !isLocal && cfg.TLSCertFile == "" {
		// On Cloud Run, TLS is terminated by the platform — no server TLS needed.
		// On Kubernetes, either native TLS or ingress/sidecar is required.
		// We don't error here since Cloud Run h2c is valid, but the
		// deployment docs make this explicit.
	}

	// mTLS client CA without server cert is meaningless.
	if cfg.TLSClientCAFile != "" && cfg.TLSCertFile == "" {
		errs = append(errs, "tls_client_ca_file requires tls_cert_file and tls_key_file")
	}

	if len(errs) > 0 {
		return fmt.Errorf("standalone server config validation failed:\n  - %s",
			strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateTLSFields checks that TLS cert/key are provided together.
func validateTLSFields(certFile, keyFile, clientCAFile string) error {
	hasCert := certFile != ""
	hasKey := keyFile != ""

	if hasCert && !hasKey {
		return fmt.Errorf("tls_cert_file requires tls_key_file")
	}
	if hasKey && !hasCert {
		return fmt.Errorf("tls_key_file requires tls_cert_file")
	}
	if clientCAFile != "" && !hasCert {
		return fmt.Errorf("tls_client_ca_file requires tls_cert_file and tls_key_file")
	}
	return nil
}

// BuildStandaloneServerOptions creates grpc.ServerOption slices and a
// TokenValidator from a validated StandaloneServerConfig. Call
// ValidateStandaloneServerConfig first.
func BuildStandaloneServerOptions(cfg StandaloneServerConfig) ([]grpc.ServerOption, error) {
	var validator TokenValidator

	switch cfg.AuthMode {
	case AuthModeGoogleIDToken:
		v, err := NewGoogleIDTokenValidator(GoogleIDTokenValidatorConfig{
			Audience:           cfg.Audience,
			AuthorizedSubjects: cfg.AuthorizedSubjects,
			JWKSURL:            cfg.JWKSURL,
			Logger:             cfg.Logger,
		})
		if err != nil {
			return nil, fmt.Errorf("google ID token validator: %w", err)
		}
		validator = v

	case AuthModeHMAC:
		v, err := NewHMACTokenValidator(HMACTokenValidatorConfig{
			Key:                cfg.HMACKey,
			Audience:           cfg.Audience,
			AuthorizedSubjects: cfg.AuthorizedSubjects,
		})
		if err != nil {
			return nil, fmt.Errorf("HMAC token validator: %w", err)
		}
		validator = v

	case AuthModeLocalDev, "":
		// No validator for local dev.
		validator = nil
	}

	return ServerOptions(ServerAuthConfig{
		Validator:       validator,
		TLSCertFile:     cfg.TLSCertFile,
		TLSKeyFile:      cfg.TLSKeyFile,
		TLSClientCAFile: cfg.TLSClientCAFile,
	})
}
