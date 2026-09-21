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
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// External bearer authentication.
//
// Clients such as the Gemini Enterprise A2A bridge forward the end user's own
// Google credential to the Hub as a plain `Authorization: Bearer` token instead
// of minting a Hub credential on the user's behalf. Two token shapes arrive:
//
//   - OIDC ID tokens (JWTs): verified by the FederationAuthenticator against
//     server.federation.trusted_issuers (JWKS signature, iss, aud, exp,
//     allowed_emails).
//   - Opaque Google OAuth2 access tokens (ya29.*): verified with Google's
//     tokeninfo endpoint and bound to the accounts.google.com trusted issuer
//     entry — the token's audience must equal that issuer's expected_audience
//     (the OAuth client ID registered in Gemini Enterprise) and the verified
//     email must satisfy allowed_emails.
//
// A verified email is then turned into a Hub user through the same sign-in
// policy as interactive OAuth (admin_emails, authorized_domains,
// user_access_mode), so external callers never bypass the Hub's own gating and
// never receive more than the role that policy assigns.

const (
	// googleIssuerURL is the OIDC issuer for Google accounts.
	googleIssuerURL = "https://accounts.google.com"
	// googleTokenInfoURL (Google's token introspection endpoint) is shared
	// with google_credential_validator.go.
	// googleAccessTokenPrefix identifies opaque Google OAuth2 access tokens.
	googleAccessTokenPrefix = "ya29."

	// googleTokenInfoTimeout bounds a single tokeninfo call.
	googleTokenInfoTimeout = 5 * time.Second
	// googleTokenInfoMaxCacheTTL caps how long a verified access token is
	// remembered; the token's own remaining lifetime is used when shorter.
	googleTokenInfoMaxCacheTTL = 5 * time.Minute
	// googleTokenInfoMaxCacheEntries bounds the verification cache.
	googleTokenInfoMaxCacheEntries = 10000
)

// errExternalBearerNotApplicable reports that the token is not something the
// external bearer path can vouch for (no federation configured, no issuer for
// the token shape, or the token failed verification). Callers fall back to
// their normal rejection message.
var errExternalBearerNotApplicable = errors.New("external bearer: not applicable")

// googleTokenInfo is the subset of Google's tokeninfo response we rely on.
type googleTokenInfo struct {
	Aud           string `json:"aud"`
	Azp           string `json:"azp"`
	Email         string `json:"email"`
	EmailVerified string `json:"email_verified"`
	ExpiresIn     string `json:"expires_in"`
}

type googleTokenInfoCacheEntry struct {
	info      *googleTokenInfo
	expiresAt time.Time
}

// googleTokenInfoCache remembers verified access tokens keyed by SHA-256 of
// the token so a chatty client does not trigger a tokeninfo round-trip per
// request. Entries are dropped when the token expires.
type googleTokenInfoCache struct {
	mu      sync.Mutex
	entries map[[32]byte]googleTokenInfoCacheEntry
}

var tokenInfoCache = &googleTokenInfoCache{entries: make(map[[32]byte]googleTokenInfoCacheEntry)}

func (c *googleTokenInfoCache) get(key [32]byte) (*googleTokenInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.info, true
}

func (c *googleTokenInfoCache) set(key [32]byte, info *googleTokenInfo, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= googleTokenInfoMaxCacheEntries {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= googleTokenInfoMaxCacheEntries {
			// Still full of live entries: skip caching rather than evict
			// arbitrarily; correctness does not depend on the cache.
			return
		}
	}
	c.entries[key] = googleTokenInfoCacheEntry{info: info, expiresAt: time.Now().Add(ttl)}
}

// serveExternalBearer attempts to authenticate the request with an external
// bearer token. It returns true when it has written a response or served the
// request, and false when the token is not an external bearer token the Hub
// can vouch for (the caller then emits its usual rejection).
func serveExternalBearer(w http.ResponseWriter, r *http.Request, next http.Handler,
	ctx context.Context, token string, cfg AuthConfig, log *slog.Logger) bool {

	user, err := authenticateExternalBearer(ctx, token, cfg)
	if err != nil {
		switch {
		case errors.Is(err, ErrAccessDenied):
			log.Warn("External bearer rejected: email not authorized by sign-in policy",
				"error", err)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"access denied: email not authorized", nil)
			return true
		case errors.Is(err, ErrUserSuspended):
			log.Warn("External bearer rejected: user is suspended", "error", err)
			writeError(w, http.StatusForbidden, "user_suspended",
				"access denied: user account is suspended", nil)
			return true
		case errors.Is(err, errExternalBearerNotApplicable):
			if cfg.Debug {
				log.Debug("External bearer not applicable", "error", err)
			}
			return false
		default:
			// The token targeted a trusted issuer but failed verification or
			// policy (bad signature, wrong audience, email not allowed, ...).
			log.Info("External bearer rejected", "error", err)
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
				"invalid external bearer token", nil)
			return true
		}
	}

	ctx = context.WithValue(ctx, userContextKey{}, user)
	ctx = contextWithIdentity(ctx, user)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(user))
	ctx = contextWithAuthType(ctx, AuthTypeExternalBearer)
	if cfg.Debug {
		log.Debug("External bearer authenticated", "email", user.Email(), "user_id", user.ID())
	}
	next.ServeHTTP(w, r.WithContext(ctx))
	return true
}

// authenticateExternalBearer verifies token against the trusted issuer
// configuration and resolves the verified email to a Hub user. It returns
// errExternalBearerNotApplicable when the token cannot be attributed to a
// trusted issuer at all; other errors describe a token that targeted a
// trusted issuer but failed verification or policy.
func authenticateExternalBearer(ctx context.Context, token string, cfg AuthConfig) (UserIdentity, error) {
	if cfg.FederationAuth == nil {
		return nil, errExternalBearerNotApplicable
	}
	fedAuth := cfg.FederationAuth.Load()
	if fedAuth == nil {
		return nil, errExternalBearerNotApplicable
	}

	var email, displayName string
	switch {
	case strings.HasPrefix(token, googleAccessTokenPrefix):
		issuerCfg, ok := fedAuth.IssuerConfig(googleIssuerURL)
		if !ok || IssuerType(issuerCfg.IssuerType) != IssuerTypeUser {
			return nil, fmt.Errorf("%w: no trusted user issuer configured for %s",
				errExternalBearerNotApplicable, googleIssuerURL)
		}
		info, err := verifyGoogleAccessToken(ctx, token, cfg.GoogleTokenInfoURL, issuerCfg)
		if err != nil {
			return nil, err
		}
		email = info.Email
	case looksLikeJWT(token):
		identity, err := fedAuth.Authenticate(token)
		if err != nil {
			// The FederationAuthenticator rejects untrusted issuers and
			// malformed tokens with the same error type; treat every JWT
			// failure as "not ours" so Hub-token error messages are preserved.
			return nil, fmt.Errorf("%w: %v", errExternalBearerNotApplicable, err)
		}
		switch ident := identity.(type) {
		case *FederatedUserIdentity:
			email, displayName = ident.Email(), ident.DisplayName()
		case *FederatedServiceIdentity:
			email = ident.Email()
		default:
			return nil, fmt.Errorf("external bearer: issuer %q does not identify a user", identity.IssuerURL())
		}
		if email == "" {
			return nil, fmt.Errorf("external bearer: token from %q carries no email claim", identity.IssuerURL())
		}
	default:
		return nil, errExternalBearerNotApplicable
	}

	email = strings.ToLower(strings.TrimSpace(email))

	if cfg.ExternalUserProvisioner != nil {
		return cfg.ExternalUserProvisioner(ctx, &ProxyUserInfo{Email: email, DisplayName: displayName})
	}

	// No provisioner wired: accept only users that already exist and are active.
	if cfg.UserStore == nil {
		return nil, fmt.Errorf("external bearer: user store not configured")
	}
	u, err := cfg.UserStore.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("external bearer: %w: %s is not a registered user", ErrAccessDenied, email)
		}
		return nil, fmt.Errorf("external bearer: user lookup: %w", err)
	}
	switch u.Status {
	case store.UserStatusActive:
		return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), nil
	case store.UserStatusSuspended:
		return nil, fmt.Errorf("external bearer: %w", ErrUserSuspended)
	default:
		return nil, fmt.Errorf("external bearer: %w: %s has status %q", ErrAccessDenied, email, u.Status)
	}
}

// verifyGoogleAccessToken introspects an opaque Google OAuth2 access token and
// enforces the trusted issuer policy: the token must have been issued to the
// configured expected_audience, carry a verified email, and (when configured)
// match allowed_emails.
func verifyGoogleAccessToken(ctx context.Context, token, endpoint string, issuerCfg config.TrustedIssuerConfig) (*googleTokenInfo, error) {
	// Audience binding is mandatory for opaque tokens: without it any Google
	// access token minted for any application would authenticate its owner.
	if issuerCfg.ExpectedAudience == "" {
		return nil, fmt.Errorf("external bearer: trusted issuer %s has no expected_audience; refusing opaque access tokens", googleIssuerURL)
	}

	key := sha256.Sum256([]byte(token))
	info, cached := tokenInfoCache.get(key)
	if !cached {
		var err error
		info, err = fetchGoogleTokenInfo(ctx, token, endpoint)
		if err != nil {
			return nil, err
		}
	}

	if info.Aud != issuerCfg.ExpectedAudience && info.Azp != issuerCfg.ExpectedAudience {
		return nil, fmt.Errorf("external bearer: access token audience %q does not match expected_audience", info.Aud)
	}
	if info.Email == "" || info.EmailVerified != "true" {
		return nil, fmt.Errorf("external bearer: access token has no verified email")
	}
	if len(issuerCfg.AllowedEmails) > 0 && !matchesAllowedEmails(issuerCfg.AllowedEmails, info.Email) {
		return nil, fmt.Errorf("external bearer: email %q not in allowed_emails for %s", info.Email, googleIssuerURL)
	}

	if !cached {
		ttl := googleTokenInfoMaxCacheTTL
		if secs, err := strconv.Atoi(info.ExpiresIn); err == nil && time.Duration(secs)*time.Second < ttl {
			ttl = time.Duration(secs) * time.Second
		}
		tokenInfoCache.set(key, info, ttl)
	}
	return info, nil
}

// fetchGoogleTokenInfo calls Google's tokeninfo endpoint for an access token.
func fetchGoogleTokenInfo(ctx context.Context, token, endpoint string) (*googleTokenInfo, error) {
	if endpoint == "" {
		endpoint = googleTokenInfoURL
	}
	reqCtx, cancel := context.WithTimeout(ctx, googleTokenInfoTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		endpoint+"?access_token="+url.QueryEscape(token), nil)
	if err != nil {
		return nil, fmt.Errorf("external bearer: build tokeninfo request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("external bearer: tokeninfo request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Google answers 400 for expired, revoked, or malformed tokens.
		return nil, fmt.Errorf("external bearer: tokeninfo rejected access token (HTTP %d)", resp.StatusCode)
	}
	var info googleTokenInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return nil, fmt.Errorf("external bearer: decode tokeninfo response: %w", err)
	}
	return &info, nil
}
