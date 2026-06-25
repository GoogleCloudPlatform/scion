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

package hub

import (
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
)

// ProxyUserInfo is the verified identity extracted from proxy headers/assertions.
type ProxyUserInfo struct {
	Subject     string // stable provider subject (IdP prefix stripped)
	Email       string // verified email (IdP prefix stripped, lowercased)
	DisplayName string // best-effort; may be empty for IAP
	Domain      string // hd claim, if present
}

// ProxyAuthenticator verifies proxy-supplied auth on a request and returns the
// verified user. (nil, nil) = "no proxy assertion present" (fall through);
// (nil, err) = assertion present but invalid (reject).
type ProxyAuthenticator interface {
	Authenticate(r *http.Request) (*ProxyUserInfo, error)
	Name() string // for logging/metrics, e.g. "iap"
}

// ---- Google IAP Authenticator ----

const (
	// IAPAssertionHeader is the header containing the IAP signed JWT.
	IAPAssertionHeader = "X-Goog-IAP-JWT-Assertion"

	// DefaultIAPIssuer is the expected issuer for IAP JWTs.
	DefaultIAPIssuer = "https://cloud.google.com/iap"

	// DefaultIAPJWKSURL is the URL for IAP public keys.
	DefaultIAPJWKSURL = "https://www.gstatic.com/iap/verify/public_key-jwk"

	// iapClockSkew is the allowed clock skew for exp/iat validation.
	iapClockSkew = 30 * time.Second

	// jwksRefreshInterval is how often the JWKS cache proactively refreshes.
	jwksRefreshInterval = 1 * time.Hour

	// iapIdPPrefix is the IdP prefix stripped from IAP sub/email claims.
	iapIdPPrefix = "accounts.google.com:"
)

// IAPAuthenticator verifies Google IAP signed JWTs (X-Goog-IAP-JWT-Assertion).
type IAPAuthenticator struct {
	// Audience is the expected audience claim — MANDATORY.
	Audience string

	// Issuer is the expected issuer (defaults to DefaultIAPIssuer).
	Issuer string

	// JWKSURL is the JWKS endpoint (defaults to DefaultIAPJWKSURL).
	JWKSURL string

	// HTTPClient is the HTTP client for fetching JWKS (defaults to http.DefaultClient).
	HTTPClient *http.Client

	jwksCache *jwksCache
	initOnce  sync.Once
}

// Name returns "iap" for logging/metrics.
func (a *IAPAuthenticator) Name() string { return "iap" }

// Authenticate reads the IAP assertion header, verifies the JWT, and returns
// the verified ProxyUserInfo. Returns (nil, nil) if no assertion is present.
func (a *IAPAuthenticator) Authenticate(r *http.Request) (*ProxyUserInfo, error) {
	a.initOnce.Do(a.init)

	assertion := r.Header.Get(IAPAssertionHeader)
	if assertion == "" {
		return nil, nil // no assertion present, fall through
	}

	// Parse the JWT (compact serialization)
	tok, err := jwt.ParseSigned(assertion, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return nil, fmt.Errorf("iap: failed to parse JWT: %w", err)
	}

	// Look up the signing key by kid
	if len(tok.Headers) == 0 {
		return nil, fmt.Errorf("iap: JWT has no headers")
	}
	kid := tok.Headers[0].KeyID
	if kid == "" {
		return nil, fmt.Errorf("iap: JWT has no kid")
	}

	key, err := a.jwksCache.GetKey(kid)
	if err != nil {
		return nil, fmt.Errorf("iap: JWKS key lookup failed for kid %q: %w", kid, err)
	}

	// Verify signature and extract claims
	var claims iapClaims
	if err := tok.Claims(key, &claims); err != nil {
		return nil, fmt.Errorf("iap: JWT signature verification failed: %w", err)
	}

	// Validate standard claims
	expectedIssuer := a.resolveIssuer()
	now := time.Now()

	if err := a.validateClaims(&claims, expectedIssuer, now); err != nil {
		return nil, err
	}

	// Strip IdP prefix and build ProxyUserInfo
	return &ProxyUserInfo{
		Subject:     stripIAPPrefix(claims.Subject),
		Email:       strings.ToLower(stripIAPPrefix(claims.Email)),
		DisplayName: "", // IAP does not provide display name
		Domain:      claims.HD,
	}, nil
}

// iapClaims are the JWT claims from an IAP assertion.
type iapClaims struct {
	Issuer   string           `json:"iss"`
	Subject  string           `json:"sub"`
	Audience jwt.Audience     `json:"aud"`
	IssuedAt *jwt.NumericDate `json:"iat"`
	Expiry   *jwt.NumericDate `json:"exp"`
	Email    string           `json:"email"`
	HD       string           `json:"hd,omitempty"` // hosted domain
}

func (a *IAPAuthenticator) validateClaims(claims *iapClaims, expectedIssuer string, now time.Time) error {
	// Issuer
	if claims.Issuer != expectedIssuer {
		return fmt.Errorf("iap: invalid issuer %q, expected %q", claims.Issuer, expectedIssuer)
	}

	// Audience (mandatory binding)
	if !claims.Audience.Contains(a.Audience) {
		return fmt.Errorf("iap: audience mismatch: got %v, expected %q", claims.Audience, a.Audience)
	}

	// Expiry
	if claims.Expiry == nil {
		return fmt.Errorf("iap: missing exp claim")
	}
	if now.After(claims.Expiry.Time().Add(iapClockSkew)) {
		return fmt.Errorf("iap: token expired at %v", claims.Expiry.Time())
	}

	// Issued-at (with skew: reject if iat is too far in the future)
	if claims.IssuedAt != nil {
		if claims.IssuedAt.Time().After(now.Add(iapClockSkew)) {
			return fmt.Errorf("iap: token issued in the future: iat=%v", claims.IssuedAt.Time())
		}
	}

	// Subject and email must be present
	if claims.Subject == "" {
		return fmt.Errorf("iap: missing sub claim")
	}
	if claims.Email == "" {
		return fmt.Errorf("iap: missing email claim")
	}

	return nil
}

func (a *IAPAuthenticator) resolveIssuer() string {
	if a.Issuer != "" {
		return a.Issuer
	}
	return DefaultIAPIssuer
}

func (a *IAPAuthenticator) resolveJWKSURL() string {
	if a.JWKSURL != "" {
		return a.JWKSURL
	}
	return DefaultIAPJWKSURL
}

// defaultJWKSHTTPClient is used for JWKS fetches when no custom client is provided.
// It has a reasonable timeout to prevent hanging on unresponsive endpoints.
var defaultJWKSHTTPClient = &http.Client{Timeout: 10 * time.Second}

func (a *IAPAuthenticator) resolveHTTPClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return defaultJWKSHTTPClient
}

func (a *IAPAuthenticator) init() {
	a.jwksCache = &jwksCache{
		url:    a.resolveJWKSURL(),
		client: a.resolveHTTPClient(),
	}
}

// stripIAPPrefix removes the "accounts.google.com:" prefix from IAP claims.
func stripIAPPrefix(s string) string {
	return strings.TrimPrefix(s, iapIdPPrefix)
}

// ---- JWKS Cache ----

// jwksCache manages a cached set of JWKS keys with lazy fetch, periodic refresh,
// and on-miss refresh for unknown key IDs.
type jwksCache struct {
	url    string
	client *http.Client

	mu            sync.RWMutex
	keys          map[string]jose.JSONWebKey // kid -> key
	lastFetched   time.Time                  // last successful fetch
	lastAttempted time.Time                  // last fetch attempt (success or failure), for stampede prevention
	refreshing    bool                       // true while a refresh is in-flight
}

// GetKey returns the public key for the given kid. If the kid is not found
// in the cache, a refresh is triggered. If the JWKS endpoint is temporarily
// unavailable, the last-good keys are served.
func (c *jwksCache) GetKey(kid string) (interface{}, error) {
	// Try cached key first
	c.mu.RLock()
	if c.keys != nil {
		if k, ok := c.keys[kid]; ok {
			needsRefresh := time.Since(c.lastFetched) > jwksRefreshInterval
			c.mu.RUnlock()
			// Proactive background refresh (non-blocking)
			if needsRefresh {
				go func() { _ = c.refresh() }()
			}
			return k.Key, nil
		}
	}
	c.mu.RUnlock()

	// Kid not found — refresh and retry
	if err := c.refresh(); err != nil {
		// If we have stale keys but not this kid, it's a genuine miss
		c.mu.RLock()
		hasKeys := len(c.keys) > 0
		c.mu.RUnlock()
		if !hasKeys {
			return nil, fmt.Errorf("jwks fetch failed and no cached keys: %w", err)
		}
		// Stale keys but kid still not found after failed refresh
		return nil, fmt.Errorf("unknown kid %q (jwks refresh failed: %v)", kid, err)
	}

	// Check again after refresh
	c.mu.RLock()
	defer c.mu.RUnlock()
	if k, ok := c.keys[kid]; ok {
		return k.Key, nil
	}
	return nil, fmt.Errorf("unknown kid %q after JWKS refresh", kid)
}

// jwksDebounceInterval is the minimum time between refresh attempts (success or failure)
// to prevent stampedes during JWKS endpoint outages.
const jwksDebounceInterval = 5 * time.Second

// refresh fetches the JWKS from the endpoint and updates the cache.
// On transient failure, the last-good keys are preserved.
// Concurrent calls are coalesced: if a refresh is already in-flight, subsequent
// callers return immediately (nil error) and rely on cached keys.
func (c *jwksCache) refresh() error {
	c.mu.Lock()

	// Debounce: skip if a refresh was attempted (success OR failure) very recently.
	if time.Since(c.lastAttempted) < jwksDebounceInterval {
		c.mu.Unlock()
		return nil
	}

	// Prevent concurrent in-flight refreshes.
	if c.refreshing {
		c.mu.Unlock()
		return nil
	}
	c.refreshing = true
	c.lastAttempted = time.Now()
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	// All network I/O and response processing happens with no lock held.
	resp, err := c.client.Get(c.url)
	if err != nil {
		slog.Warn("jwks fetch failed, serving last-good keys", "url", c.url, "error", err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Warn("jwks fetch non-200", "url", c.url, "status", resp.StatusCode, "body", string(body))
		return fmt.Errorf("jwks fetch returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return fmt.Errorf("jwks read body: %w", err)
	}

	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("jwks parse: %w", err)
	}

	newKeys := make(map[string]jose.JSONWebKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.KeyID != "" {
			newKeys[k.KeyID] = k
		}
	}

	// Re-acquire lock only to swap the cached keys.
	c.mu.Lock()
	c.keys = newKeys
	c.lastFetched = time.Now()
	c.mu.Unlock()

	slog.Debug("jwks cache refreshed", "url", c.url, "keyCount", len(newKeys))
	return nil
}
