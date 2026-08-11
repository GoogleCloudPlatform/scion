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

package teams

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// -------------------------------------------------------------------
// TokenProvider — outbound OAuth2 token acquisition (client_credentials)
// -------------------------------------------------------------------

const (
	// defaultTokenEndpointTemplate is the Azure AD v2 token endpoint.
	defaultTokenEndpointTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	// botFrameworkScope is the scope requested for Bot Connector API calls.
	botFrameworkScope = "https://api.botframework.com/.default"
	// tokenRefreshWindow is how far before expiry we proactively refresh.
	tokenRefreshWindow = 5 * time.Minute
)

// TokenProvider acquires and caches OAuth2 access tokens for outbound
// Bot Connector REST API calls using the client_credentials grant.
type TokenProvider struct {
	mu        sync.RWMutex
	token     string
	expiresAt time.Time

	appID     string
	appSecret string
	tenantID  string

	log *slog.Logger

	// httpClient is injectable for testing.
	httpClient *http.Client
	// tokenEndpoint allows override for testing.
	tokenEndpoint string
}

// NewTokenProvider creates a new TokenProvider for the given Azure app.
func NewTokenProvider(appID, appSecret, tenantID string) *TokenProvider {
	return &TokenProvider{
		appID:         appID,
		appSecret:     appSecret,
		tenantID:      tenantID,
		log:           slog.Default(),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		tokenEndpoint: fmt.Sprintf(defaultTokenEndpointTemplate, tenantID),
	}
}

// InvalidateToken clears the cached token so that the next GetToken call
// forces a refresh. Used after receiving a 401 to discard a stale token.
func (tp *TokenProvider) InvalidateToken() {
	tp.mu.Lock()
	tp.token = ""
	tp.expiresAt = time.Time{}
	tp.mu.Unlock()
}

// GetToken returns a valid access token, refreshing if necessary.
func (tp *TokenProvider) GetToken(ctx context.Context) (string, error) {
	tp.mu.RLock()
	if tp.token != "" && time.Now().Add(tokenRefreshWindow).Before(tp.expiresAt) {
		t := tp.token
		tp.mu.RUnlock()
		return t, nil
	}
	tp.mu.RUnlock()

	return tp.refresh(ctx)
}

// tokenResponse is the JSON response from the Azure AD token endpoint.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func (tp *TokenProvider) refresh(ctx context.Context) (string, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	// Double-check after acquiring write lock.
	if tp.token != "" && time.Now().Add(tokenRefreshWindow).Before(tp.expiresAt) {
		return tp.token, nil
	}

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {tp.appID},
		"client_secret": {tp.appSecret},
		"scope":         {botFrameworkScope},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tp.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return tp.fallbackOrError(fmt.Errorf("create token request: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tp.httpClient.Do(req)
	if err != nil {
		return tp.fallbackOrError(fmt.Errorf("token request failed: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return tp.fallbackOrError(fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body)))
	}

	var result tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return tp.fallbackOrError(fmt.Errorf("decode token response: %w", err))
	}

	tp.token = result.AccessToken
	tp.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)

	return tp.token, nil
}

// fallbackOrError returns the cached token if it is still valid (not yet
// expired), even if a refresh attempt failed. This allows the broker to
// ride out transient Azure AD outages using a cached token that hasn't
// actually expired yet (it's just past the proactive refresh window).
// Must be called while tp.mu is held.
func (tp *TokenProvider) fallbackOrError(refreshErr error) (string, error) {
	if tp.token != "" && time.Now().Before(tp.expiresAt) {
		// Token is still valid — use it and log a warning.
		tp.log.Warn("Token refresh failed but cached token still valid, using cached token",
			"error", refreshErr,
			"expires_at", tp.expiresAt,
		)
		return tp.token, nil
	}
	return "", refreshErr
}

// -------------------------------------------------------------------
// JWTValidator — inbound JWT validation for Bot Framework activities
// -------------------------------------------------------------------

const (
	// defaultOpenIDMetadataURL is the Bot Framework OpenID metadata endpoint.
	defaultOpenIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// expectedIssuer is the required JWT issuer for Bot Framework tokens.
	expectedIssuer = "https://api.botframework.com"
	// jwksRefreshInterval controls how often the JWKS keys are refreshed.
	jwksRefreshInterval = 24 * time.Hour
	// jwksRefreshCooldown is the minimum interval between JWKS refreshes to
	// prevent cache stampede and DoS via forged kid values.
	jwksRefreshCooldown = 10 * time.Second
)

// JWTValidator validates inbound JWTs from the Bot Framework Service.
type JWTValidator struct {
	appID string

	mu             sync.RWMutex
	keys           map[string]*rsa.PublicKey
	keysLoadedAt   time.Time
	jwksURL        string
	jwksURLFetched bool

	// openIDMetadataURL allows override for testing.
	openIDMetadataURL string
	// httpClient is injectable for testing.
	httpClient *http.Client
}

// NewJWTValidator creates a new JWTValidator for the given app ID.
func NewJWTValidator(appID string) *JWTValidator {
	return &JWTValidator{
		appID:             appID,
		keys:              make(map[string]*rsa.PublicKey),
		openIDMetadataURL: defaultOpenIDMetadataURL,
		httpClient:        &http.Client{Timeout: 10 * time.Second},
	}
}

// ValidateToken validates a JWT token string and returns the parsed token.
func (v *JWTValidator) ValidateToken(ctx context.Context, tokenString string) (*jwt.Token, error) {
	keyFunc := func(token *jwt.Token) (interface{}, error) {
		// Verify signing method is RSA.
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, fmt.Errorf("missing kid in token header")
		}

		key, err := v.getKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	token, err := jwt.Parse(tokenString, keyFunc,
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(v.appID),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	return token, nil
}

// getKey retrieves a signing key by kid, fetching/refreshing JWKS as needed.
func (v *JWTValidator) getKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	needsRefresh := time.Since(v.keysLoadedAt) > jwksRefreshInterval
	v.mu.RUnlock()

	if ok && !needsRefresh {
		return key, nil
	}

	// Key not found or keys are stale — refresh, passing kid for
	// double-check and cooldown enforcement inside the write lock.
	if err := v.refreshKeys(ctx, kid); err != nil {
		return nil, fmt.Errorf("refresh JWKS: %w", err)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	key, ok = v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("signing key %q not found in JWKS", kid)
	}
	return key, nil
}

// openIDConfig is the relevant part of the OpenID Connect metadata.
type openIDConfig struct {
	JWKSURI string `json:"jwks_uri"`
}

// jwksResponse is the JSON Web Key Set response.
type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

// jwkKey is a single key in a JWKS.
type jwkKey struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *JWTValidator) refreshKeys(ctx context.Context, kid string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Double-check: if the requested kid is already present and keys were
	// loaded recently, another goroutine already refreshed — skip.
	if _, ok := v.keys[kid]; ok && time.Since(v.keysLoadedAt) <= jwksRefreshInterval {
		return nil
	}

	// Cooldown: refuse to refresh if we fetched very recently, preventing
	// an attacker from flooding the JWKS endpoint with random kid values.
	if !v.keysLoadedAt.IsZero() && time.Since(v.keysLoadedAt) < jwksRefreshCooldown {
		return nil
	}

	// Fetch JWKS URL from OpenID metadata if we haven't yet.
	if !v.jwksURLFetched {
		jwksURL, err := v.fetchJWKSURL(ctx)
		if err != nil {
			return err
		}
		v.jwksURL = jwksURL
		v.jwksURLFetched = true
	}

	// Fetch JWKS.
	req, err := http.NewRequestWithContext(ctx, "GET", v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("create JWKS request: %w", err)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	var jwks jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pubKey, err := parseRSAKey(k)
		if err != nil {
			continue // Skip invalid keys.
		}
		keys[k.Kid] = pubKey
	}

	v.keys = keys
	v.keysLoadedAt = time.Now()
	return nil
}

func (v *JWTValidator) fetchJWKSURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", v.openIDMetadataURL, nil)
	if err != nil {
		return "", fmt.Errorf("create OpenID metadata request: %w", err)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch OpenID metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenID metadata endpoint returned %d", resp.StatusCode)
	}

	var config openIDConfig
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return "", fmt.Errorf("decode OpenID metadata: %w", err)
	}

	if config.JWKSURI == "" {
		return "", fmt.Errorf("OpenID metadata missing jwks_uri")
	}

	return config.JWKSURI, nil
}

// parseRSAKey converts a JWK RSA key to an *rsa.PublicKey.
func parseRSAKey(k jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}
