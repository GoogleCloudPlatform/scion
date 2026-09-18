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
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/sync/singleflight"
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

	// minJWKSRefreshInterval is the minimum time between JWKS fetches to
	// prevent DoS via unknown kid stampede — if the JWKS was fetched recently
	// and the kid is still not found, return an error immediately without
	// re-fetching.
	minJWKSRefreshInterval = 1 * time.Minute

	// defaultJWKSFetchTimeout is the HTTP timeout for JWKS endpoint fetches.
	defaultJWKSFetchTimeout = 10 * time.Second

	// maxJWKSResponseBytes limits the JWKS response body size to prevent
	// memory exhaustion from a compromised or malicious endpoint.
	maxJWKSResponseBytes = 1 << 20 // 1 MiB
)

// GoogleIDTokenValidatorConfig configures the concrete Google ID token
// validator for the bridge's gRPC server.
type GoogleIDTokenValidatorConfig struct {
	// Audience is the expected audience claim — typically the bridge's service
	// URL. Required; tokens with a different audience are rejected.
	Audience string

	// AuthorizedSubjects is the set of authorized service account emails
	// (matched against the "email" claim, not "sub"). For Google Cloud
	// service accounts, the email is the stable human-readable identifier
	// (e.g., "hub-sa@project.iam.gserviceaccount.com"), while "sub" is
	// the SA's opaque numeric unique ID. Authorization is performed against
	// the email because it is the meaningful principal identifier in IAM
	// policy and audit logs.
	//
	// Required — tokens from principals not in this list are rejected.
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
	jwksFetchTimeout   time.Duration

	// mu protects jwks and fetchedAt for short cache reads/writes.
	// Network I/O (JWKS fetch) happens OUTSIDE this lock.
	mu        sync.RWMutex
	jwks      *jose.JSONWebKeySet
	fetchedAt time.Time

	// fetchGroup coalesces concurrent JWKS refreshes so that only one
	// network request is in-flight at a time; all waiters share the result.
	fetchGroup singleflight.Group
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
		jwksFetchTimeout:   defaultJWKSFetchTimeout,
		// Google ID tokens are signed with RS256 per Google's OIDC documentation.
		// No other algorithms are accepted to prevent algorithm confusion attacks.
		algorithms: []jose.SignatureAlgorithm{
			jose.RS256,
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

	// 7. Validate time (expiry is mandatory; not-before is optional).
	if claims.Expiry == nil {
		return status.Error(codes.Unauthenticated, "token has no expiry (exp claim is required)")
	}
	now := time.Now()
	if now.After(claims.Expiry.Time()) {
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

// getSigningKey returns the public keys matching kid from the cached JWKS.
//
// Fast path: a read lock checks the cache; if the kid is found, keys are
// returned immediately without blocking, regardless of cache age. A cache
// hit with a stale timestamp is always safe because JWT signature verification
// will fail if the key material has been rotated by Google.
//
// Refresh path: when the kid is NOT in the cache (or no cache exists), a
// singleflight call coalesces concurrent refreshes so only one goroutine
// performs the HTTP fetch. The fetch runs OUTSIDE any mutex; only the short
// cache-swap happens under a write lock.
//
// Cooldown: if the JWKS was fetched within minJWKSRefreshInterval and the kid
// is still not found, the error is returned immediately to prevent DoS via
// unknown-kid stampede.
func (v *GoogleIDTokenValidator) getSigningKey(ctx context.Context, kid string) ([]interface{}, error) {
	// 1. Fast path — read lock only, no network I/O. If the kid is in the
	// cache, return immediately regardless of cache age.
	v.mu.RLock()
	cachedJWKS := v.jwks
	fetchAge := time.Since(v.fetchedAt)
	v.mu.RUnlock()

	if cachedJWKS != nil {
		keys := cachedJWKS.Key(kid)
		if len(keys) > 0 {
			return keysToPublicKeys(keys), nil
		}
		// Key not found. If within cooldown, refuse to re-fetch.
		if fetchAge < minJWKSRefreshInterval {
			return nil, fmt.Errorf("no key found for kid %q (JWKS cache is fresh; next refresh in %v)",
				kid, minJWKSRefreshInterval-fetchAge)
		}
		// Cooldown elapsed — fall through to refresh.
	}

	// 2. Refresh path — coalesce concurrent fetches via singleflight.
	// Network I/O happens here, OUTSIDE any mutex.
	resultCh := v.fetchGroup.DoChan("jwks", func() (interface{}, error) {
		// Re-check cache inside singleflight — another caller may have
		// already refreshed while we were waiting to enter.
		v.mu.RLock()
		innerAge := time.Since(v.fetchedAt)
		innerJWKS := v.jwks
		v.mu.RUnlock()

		if innerJWKS != nil && innerAge < minJWKSRefreshInterval {
			// Another goroutine just refreshed; return that cache.
			return innerJWKS, nil
		}

		// The shared fetch must not inherit any one caller's cancellation.
		// Bound the detached work so it cannot outlive all waiters indefinitely.
		fetchCtx, cancel := context.WithTimeout(context.Background(), v.fetchTimeout())
		defer cancel()
		jwks, err := v.fetchJWKS(fetchCtx)
		if err != nil {
			return nil, fmt.Errorf("JWKS fetch failed: %w", err)
		}

		// Short write lock to swap the cache.
		v.mu.Lock()
		v.jwks = jwks
		v.fetchedAt = time.Now()
		v.mu.Unlock()

		return jwks, nil
	})

	var result singleflight.Result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result = <-resultCh:
	}
	if result.Err != nil {
		return nil, result.Err
	}

	jwks := result.Val.(*jose.JSONWebKeySet)
	keys := jwks.Key(kid)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key found for kid %q", kid)
	}
	return keysToPublicKeys(keys), nil
}

// fetchJWKS fetches the JWKS from the configured URL.
func (v *GoogleIDTokenValidator) fetchJWKS(ctx context.Context) (*jose.JSONWebKeySet, error) {
	client := &http.Client{Timeout: v.fetchTimeout()}

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

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read JWKS response: %w", err)
	}

	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}

	return &jwks, nil
}

func (v *GoogleIDTokenValidator) fetchTimeout() time.Duration {
	if v.jwksFetchTimeout > 0 {
		return v.jwksFetchTimeout
	}
	return defaultJWKSFetchTimeout
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
// the bridge and Hub share a symmetric signing key. This validator is not
// wired into the production standalone bridge config path (no matching
// client-side HMAC JWT minting exists in the factory). It is retained for
// testing and potential future use with an explicit HMAC client credential.
type HMACTokenValidator struct {
	key                []byte
	expectedIssuer     string
	expectedAudience   string
	authorizedSubjects map[string]bool
}

// HMACTokenValidatorConfig configures an HMAC-based token validator.
type HMACTokenValidatorConfig struct {
	// Key is the shared HMAC signing key (raw bytes).
	Key []byte

	// Issuer is the expected issuer claim. Required.
	Issuer string

	// Audience is the expected audience claim. Required.
	Audience string

	// AuthorizedSubjects limits which subjects (sub claim) are accepted.
	// Required — at least one authorized subject must be specified.
	AuthorizedSubjects []string
}

// NewHMACTokenValidator creates a validator for HMAC-signed JWTs.
func NewHMACTokenValidator(cfg HMACTokenValidatorConfig) (*HMACTokenValidator, error) {
	if len(cfg.Key) == 0 {
		return nil, fmt.Errorf("HMAC key is required")
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("issuer is required for HMAC validator")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("audience is required")
	}
	if len(cfg.AuthorizedSubjects) == 0 {
		return nil, fmt.Errorf("at least one authorized subject is required for HMAC validator")
	}
	subjects := make(map[string]bool, len(cfg.AuthorizedSubjects))
	for _, s := range cfg.AuthorizedSubjects {
		subjects[s] = true
	}
	return &HMACTokenValidator{
		key:                cfg.Key,
		expectedIssuer:     cfg.Issuer,
		expectedAudience:   cfg.Audience,
		authorizedSubjects: subjects,
	}, nil
}

// ValidateToken verifies an HMAC-signed JWT's signature, issuer, audience,
// expiry, and authorized subject.
func (v *HMACTokenValidator) ValidateToken(_ context.Context, tokenString string) error {
	tok, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "invalid token format: %v", err)
	}

	var claims jwt.Claims
	if err := tok.Claims(v.key, &claims); err != nil {
		return status.Errorf(codes.Unauthenticated, "token signature verification failed: %v", err)
	}

	// Validate issuer.
	if claims.Issuer != v.expectedIssuer {
		return status.Errorf(codes.Unauthenticated, "issuer mismatch: token has %q, expected %q",
			claims.Issuer, v.expectedIssuer)
	}

	// Validate audience.
	aud := jwt.Audience(claims.Audience)
	if !aud.Contains(v.expectedAudience) {
		return status.Errorf(codes.Unauthenticated, "audience mismatch: token has %v, expected %q",
			claims.Audience, v.expectedAudience)
	}

	// Validate time (expiry is mandatory).
	if claims.Expiry == nil {
		return status.Error(codes.Unauthenticated, "token has no expiry (exp claim is required)")
	}
	now := time.Now()
	if now.After(claims.Expiry.Time()) {
		return status.Error(codes.Unauthenticated, "token is expired")
	}
	if claims.NotBefore != nil && now.Before(claims.NotBefore.Time()) {
		return status.Error(codes.Unauthenticated, "token is not yet valid")
	}

	// Authorize subject (mandatory).
	sub := claims.Subject
	if sub == "" {
		return status.Error(codes.PermissionDenied, "token has no subject claim")
	}
	if !v.authorizedSubjects[sub] {
		return status.Errorf(codes.PermissionDenied, "subject %q is not authorized", sub)
	}

	return nil
}

// --- Startup validation ---

// StandaloneAuthMode describes the authentication mode for a standalone bridge.
type StandaloneAuthMode string

const (
	// AuthModeGoogleIDToken uses Google OIDC ID tokens validated via JWKS.
	// This is the production auth mode for Cloud Run and Kubernetes.
	AuthModeGoogleIDToken StandaloneAuthMode = "google_id_token"

	// AuthModeLocalDev allows unauthenticated access for localhost-only dev.
	AuthModeLocalDev StandaloneAuthMode = "local_dev"
)

// StandaloneServerConfig holds the full configuration for a standalone bridge's
// gRPC server, including authentication mode, TLS, and authorized principals.
type StandaloneServerConfig struct {
	// AuthMode selects the authentication mode. Supported:
	//   - "google_id_token" — JWKS-based Google OIDC ID token validation (production)
	//   - "local_dev" — unauthenticated, localhost only (development)
	//   - "" — empty is equivalent to local_dev for local addresses, rejected for remote
	AuthMode StandaloneAuthMode

	// Audience is the expected audience claim. Required for google_id_token.
	Audience string

	// AuthorizedSubjects is the set of authorized service account emails.
	// Required for google_id_token mode.
	AuthorizedSubjects []string

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

// isLocalListenAddress determines whether a server listen address binds only
// to the loopback interface. Unlike isLocalAddress (used for client dial
// targets), this function treats an empty host, "0.0.0.0", and "::" as
// NON-local because they are wildcard binds that accept connections from any
// network interface. Only explicit loopback addresses (localhost, 127.0.0.1,
// ::1) are considered local.
func isLocalListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	// Empty host in a listen address (e.g. ":50051") means bind all
	// interfaces — this is NOT local.
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateStandaloneServerConfig validates the standalone server config
// for security correctness. Returns an error if the config would create
// an insecure deployment.
func ValidateStandaloneServerConfig(cfg StandaloneServerConfig) error {
	var errs []string

	isLocal := isLocalListenAddress(cfg.ListenAddress)

	switch cfg.AuthMode {
	case AuthModeGoogleIDToken:
		if cfg.Audience == "" {
			errs = append(errs, "audience is required for google_id_token auth mode")
		}
		if len(cfg.AuthorizedSubjects) == 0 {
			errs = append(errs, "authorized_subjects is required for google_id_token auth mode; "+
				"specify the Hub service account email(s)")
		}

	case AuthModeLocalDev:
		if !isLocal {
			errs = append(errs, fmt.Sprintf("local_dev auth mode is only allowed for "+
				"local addresses, but listen address is %q", cfg.ListenAddress))
		}

	case "":
		if !isLocal {
			errs = append(errs, "auth_mode is required for non-local listen addresses; "+
				"set to google_id_token or use a local address for development")
		}
		// Empty + local is allowed (implicit local_dev).

	default:
		errs = append(errs, fmt.Sprintf("unsupported auth_mode %q; supported: "+
			"google_id_token, local_dev", cfg.AuthMode))
	}

	// Validate TLS field consistency.
	if err := validateTLSFields(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile); err != nil {
		errs = append(errs, err.Error())
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
