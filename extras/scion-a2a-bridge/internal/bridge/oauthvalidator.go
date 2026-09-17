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

package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	defaultOAuthUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	defaultOAuthCacheTTL    = 60 * time.Second
	maxOAuthResponseBytes   = 64 * 1024 // 64 KB
)

// AgentUserAuthHeader is the HTTP header used by Gemini Enterprise / Vertex Agent Engine
// to propagate the end-user's OAuth 2.0 access token when the standard Authorization
// header carries a Cloud Run invoker or service-to-service ID token.
const AgentUserAuthHeader = "X-Goog-Agent-User-Authorization"

type oauthCacheEntry struct {
	identity  *CallerIdentity
	expiresAt time.Time
}

// OAuthValidator validates OAuth 2.0 access tokens and caches verified CallerIdentity entries.
type OAuthValidator struct {
	userInfoURL string
	cacheTTL    time.Duration
	client      *http.Client

	mu     sync.RWMutex
	cache  map[string]*oauthCacheEntry
	flight singleflight.Group
}

// NewOAuthValidator creates a new OAuthValidator from OAuthConfig.
func NewOAuthValidator(cfg OAuthConfig) *OAuthValidator {
	userInfoURL := strings.TrimSpace(cfg.UserInfoURL)
	if userInfoURL == "" {
		userInfoURL = defaultOAuthUserInfoURL
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultOAuthCacheTTL
	}
	return &OAuthValidator{
		userInfoURL: userInfoURL,
		cacheTTL:    cacheTTL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache: make(map[string]*oauthCacheEntry),
	}
}

// extractOAuthToken extracts an OAuth bearer token from a request.
// It checks X-Goog-Agent-User-Authorization first (for Gemini Enterprise / Cloud Run
// environments where Authorization carries the service invoker token), falling back
// to Authorization: Bearer and X-API-Key.
func extractOAuthToken(r *http.Request) string {
	if val := strings.TrimSpace(r.Header.Get(AgentUserAuthHeader)); val != "" {
		if strings.HasPrefix(val, "Bearer ") || strings.HasPrefix(val, "bearer ") {
			return strings.TrimSpace(val[7:])
		}
		return val
	}
	if token := extractBearerToken(r); token != "" {
		return token
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// Validate verifies the OAuth access token and returns the associated CallerIdentity.
func (v *OAuthValidator) Validate(ctx context.Context, token string) (*CallerIdentity, error) {
	if token == "" {
		return nil, fmt.Errorf("oauth: empty token")
	}

	sum := sha256.Sum256([]byte(token))
	cacheKey := hex.EncodeToString(sum[:])

	// Check cache first.
	v.mu.RLock()
	if entry, ok := v.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		v.mu.RUnlock()
		return entry.identity, nil
	}
	v.mu.RUnlock()

	val, err, _ := v.flight.Do(cacheKey, func() (interface{}, error) {
		var identity *CallerIdentity
		var err error

		if v.userInfoURL == "jwt_decode" {
			identity, err = decodeOAuthJWT(token)
		} else {
			identity, err = v.fetchUserInfo(ctx, token)
		}
		if err != nil {
			return nil, err
		}

		v.mu.Lock()
		v.cache[cacheKey] = &oauthCacheEntry{
			identity:  identity,
			expiresAt: time.Now().Add(v.cacheTTL),
		}
		v.mu.Unlock()

		return identity, nil
	})
	if err != nil {
		return nil, err
	}
	return val.(*CallerIdentity), nil
}

// fetchUserInfo queries the OIDC UserInfo or TokenInfo endpoint to verify the token.
func (v *OAuthValidator) fetchUserInfo(ctx context.Context, token string) (*CallerIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.userInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: calling userinfo endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("oauth: reading userinfo response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: userinfo returned HTTP %d", resp.StatusCode)
	}

	return parseOAuthClaims(body, token)
}

// decodeOAuthJWT decodes a JWT access/ID token payload locally without network calls.
func decodeOAuthJWT(tokenString string) (*CallerIdentity, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oauth: token is not a valid JWT (expected 3 parts, got %d)", len(parts))
	}
	payloadStr := parts[1]
	if l := len(payloadStr) % 4; l > 0 {
		payloadStr += strings.Repeat("=", 4-l)
	}
	payloadBytes, err := base64.URLEncoding.DecodeString(payloadStr)
	if err != nil {
		payloadBytes, err = base64.StdEncoding.DecodeString(payloadStr)
		if err != nil {
			return nil, fmt.Errorf("oauth: failed to decode JWT payload: %w", err)
		}
	}
	return parseOAuthClaims(payloadBytes, tokenString)
}

func parseOAuthClaims(data []byte, rawToken string) (*CallerIdentity, error) {
	var claims struct {
		Sub    string `json:"sub"`
		ID     string `json:"id"`
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil, fmt.Errorf("oauth: parsing user claims: %w", err)
	}

	userID := claims.Sub
	if userID == "" {
		userID = claims.UserID
	}
	if userID == "" {
		userID = claims.ID
	}
	email := claims.Email
	if email == "" && strings.Contains(userID, "@") {
		email = userID
	}
	if userID == "" && email != "" {
		userID = email
	}

	if userID == "" && email == "" {
		return nil, fmt.Errorf("oauth: verified claims missing both sub and email")
	}

	return &CallerIdentity{
		UserID:    userID,
		Email:     email,
		Role:      "user",
		RawToken:  rawToken,
		TokenType: "oauth",
	}, nil
}
