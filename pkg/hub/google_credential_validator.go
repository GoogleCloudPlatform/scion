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

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ---------------------------------------------------------------------------
// Google credential validator — verifies end-user Google ID tokens and access
// tokens for the GE exchange endpoint. Implements fail-closed validation per
// the auth-exchange-contract.
// ---------------------------------------------------------------------------

// Pinned Google OIDC constants.
const (
	// googleIssuerHTTPS is the canonical HTTPS issuer for Google OIDC.
	googleIssuerHTTPS = "https://accounts.google.com"
	// googleIssuerBare is the bare variant Google sometimes uses.
	googleIssuerBare = "accounts.google.com"
	// googleJWKSURL is the JWKS endpoint for Google's public signing keys.
	googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	// googleTokenInfoURL is the Google token info endpoint for access tokens.
	// Per https://googleapis.dev/nodejs/google-auth-library/latest/classes/OAuth2Client.html
	// this endpoint returns audience, issued_to, scope, expires_in, access_type.
	googleTokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"
	// googleUserInfoURL is the Google userinfo endpoint for profile/email.
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"
	// googleCanonicalIssuer is the canonical issuer form used for persistence.
	googleCanonicalIssuer = googleIssuerHTTPS
)

// Maximum allowed clock skew when validating Google token timestamps.
const googleClockSkew = 2 * time.Minute

// GoogleCredentialType is an explicit credential type for the exchange.
type GoogleCredentialType string

const (
	GoogleCredentialIDToken     GoogleCredentialType = "id_token"
	GoogleCredentialAccessToken GoogleCredentialType = "access_token"
)

// ValidatedGoogleIdentity holds the verified identity extracted from a Google
// credential after full validation. All fields are authoritative.
type ValidatedGoogleIdentity struct {
	// Subject is the stable Google account identifier (sub claim).
	Subject string
	// Email is the verified email address.
	Email string
	// EmailVerified indicates the email was verified by Google.
	EmailVerified bool
	// DisplayName from the identity provider (optional).
	DisplayName string
	// AvatarURL from the identity provider (optional).
	AvatarURL string
	// Issuer is the canonical Google issuer URL.
	Issuer string
	// Audience is the OAuth client ID the credential was issued to.
	Audience string
	// UpstreamExpiry is the authoritative expiry from Google.
	UpstreamExpiry time.Time
	// HostedDomain is the Google Workspace hd claim (empty for consumer accounts).
	HostedDomain string
	// IsServiceAccount indicates whether this is a service account identity.
	IsServiceAccount bool
}

// GoogleCredentialValidator validates Google credentials (ID tokens or access
// tokens) and extracts verified identity information.
type GoogleCredentialValidator interface {
	// ValidateIDToken cryptographically verifies a Google ID token and returns
	// the validated identity. It checks signature, issuer, audience (must be in
	// allowedClientIDs), expiry, stable sub, and verified email.
	ValidateIDToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error)

	// ValidateAccessToken validates a Google access token using the tokeninfo
	// endpoint and userinfo endpoint, cross-checks stable fields, and returns
	// the validated identity.
	ValidateAccessToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error)
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	ErrGoogleInvalidCredential = errors.New("invalid Google credential")
	ErrGoogleExpiredCredential = errors.New("expired Google credential")
	ErrGoogleUntrustedAudience = errors.New("untrusted Google client ID")
	ErrGoogleUntrustedIssuer   = errors.New("untrusted Google issuer")
	ErrGoogleUnverifiedEmail   = errors.New("google email not verified")
	ErrGoogleMissingSubject    = errors.New("missing Google subject")
	ErrGoogleFieldDisagreement = errors.New("google token metadata fields disagree")
	ErrGoogleMissingField      = errors.New("required field missing from Google response")
	ErrGoogleUpstreamError     = errors.New("google upstream validation failed")
	ErrGENotConfigured         = errors.New("GE Google exchange not configured")
	ErrGEUnsupportedCredType   = errors.New("unsupported credential type")
	ErrGENoRemainingLifetime   = errors.New("credential has no remaining usable lifetime")
)

// ---------------------------------------------------------------------------
// Production implementation
// ---------------------------------------------------------------------------

// googleCredentialValidator implements GoogleCredentialValidator using real
// Google endpoints for production use.
type googleCredentialValidator struct {
	httpClient *http.Client
	jwksCache  *googleJWKSCache
}

// NewGoogleCredentialValidator creates a production Google credential validator.
func NewGoogleCredentialValidator(httpClient *http.Client) GoogleCredentialValidator {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 10 * time.Second,
			// Do not follow redirects — we call pinned Google endpoints only.
			// A redirect from these endpoints would indicate a misconfiguration
			// or MITM attempt.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("redirect not allowed to pinned Google endpoint: %s", req.URL)
			},
		}
	}
	return &googleCredentialValidator{
		httpClient: httpClient,
		jwksCache:  newGoogleJWKSCache(httpClient),
	}
}

// ValidateIDToken performs cryptographic signature verification of a Google
// ID token using Google's public JWKS keys. Verifies:
//   - RSA/EC signature via Google's published JWKS
//   - Issuer is accounts.google.com (both forms)
//   - Audience is in allowedClientIDs
//   - Expiry/not-before/issued-at with bounded skew
//   - Non-empty stable sub
//   - email_verified == true
//   - Classifies (does not reject) service accounts; see IsServiceAccount
func (v *googleCredentialValidator) ValidateIDToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error) {
	if len(allowedClientIDs) == 0 {
		return nil, fmt.Errorf("%w: no allowed client IDs configured", ErrGENotConfigured)
	}

	// Fetch Google JWKS for signature verification.
	jwks, err := v.jwksCache.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to fetch Google JWKS: %v", ErrGoogleUpstreamError, err)
	}

	// Parse and verify the JWT signature.
	parsedToken, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{
		jose.RS256, jose.ES256,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGoogleInvalidCredential, err)
	}

	// Try each key from the JWKS until one verifies.
	var claims googleIDTokenClaims
	verified := false
	for _, key := range jwks.Keys {
		if err := parsedToken.Claims(key, &claims); err == nil {
			verified = true
			break
		}
	}

	// If no cached key verified, force-refresh JWKS (the signing key may have
	// rotated) and retry once.
	if !verified {
		refreshedJWKS, err := v.jwksCache.forceRefresh(ctx)
		if err != nil {
			// The refresh itself failed (network error, 5xx, cancelled
			// context, ...) — that is an upstream fault, not evidence the
			// credential is invalid. Mislabeling it ErrGoogleInvalidCredential
			// would make it negatively cacheable (google_credential_cache.go),
			// refusing a validly-signed token for negTTL after a transient
			// JWKS blip (review r1 finding 2).
			return nil, fmt.Errorf("%w: signature verification failed and JWKS refresh failed: %v",
				ErrGoogleUpstreamError, err)
		}
		for _, key := range refreshedJWKS.Keys {
			if err := parsedToken.Claims(key, &claims); err == nil {
				verified = true
				break
			}
		}
	}
	if !verified {
		return nil, fmt.Errorf("%w: signature verification failed", ErrGoogleInvalidCredential)
	}

	// Validate issuer — accept both Google issuer forms.
	if claims.Issuer != googleIssuerHTTPS && claims.Issuer != googleIssuerBare {
		return nil, fmt.Errorf("%w: got %q", ErrGoogleUntrustedIssuer, claims.Issuer)
	}

	// Validate audience — must be exactly one of the allowed client IDs.
	if !isAllowedAudience(claims.Audience, allowedClientIDs) {
		return nil, fmt.Errorf("%w: audience not in allowed set", ErrGoogleUntrustedAudience)
	}

	// Require exp claim — ID tokens without expiry are invalid.
	if claims.Expiry == nil {
		return nil, fmt.Errorf("%w: missing exp claim", ErrGoogleInvalidCredential)
	}

	// Validate time claims with bounded skew.
	now := time.Now()
	expected := jwt.Expected{
		Time: now,
	}
	if err := claims.Validate(expected); err != nil {
		// Check if it's an expiry issue vs other issue
		if claims.Expiry.Time().Before(now.Add(-googleClockSkew)) {
			return nil, fmt.Errorf("%w: %v", ErrGoogleExpiredCredential, err)
		}
		return nil, fmt.Errorf("%w: time validation failed: %v", ErrGoogleInvalidCredential, err)
	}

	// Ensure remaining lifetime is positive. Tokens within the skew window
	// (validated above) still need positive remaining lifetime to be useful
	// for minting Hub tokens. The GE exchange further caps at
	// min(configured TTL, remaining) and rejects TTL ≤ 0.
	remaining := time.Until(claims.Expiry.Time())
	if remaining <= 0 {
		return nil, ErrGENoRemainingLifetime
	}

	// Validate stable subject.
	if claims.Subject == "" {
		return nil, ErrGoogleMissingSubject
	}

	// Validate verified email.
	if claims.Email == "" {
		return nil, fmt.Errorf("%w: no email claim", ErrGoogleMissingField)
	}
	if !bool(claims.EmailVerified) {
		return nil, ErrGoogleUnverifiedEmail
	}

	// Classify service accounts by the verified email claim. Rejection is the
	// caller's responsibility (design §4.2(i)): GEExchangeService rejects SA
	// identities immediately after validation to keep its behaviour unchanged,
	// while other callers (e.g. the external-bearer path) may admit them under
	// their own policy.
	isServiceAccount := isGoogleServiceAccount(claims.Email)

	expiry := claims.Expiry.Time()

	// Extract audience — use azp if present (authoritative issued-client),
	// otherwise the single audience element. Reject multi-valued aud without azp.
	var audience string
	if claims.AZP != "" {
		audience = claims.AZP
		// If aud is also present and differs from azp, reject (disagreement).
		if len(claims.Audience) > 0 {
			for _, aud := range claims.Audience {
				if string(aud) != claims.AZP {
					return nil, fmt.Errorf("%w: aud %q differs from azp %q",
						ErrGoogleFieldDisagreement, aud, claims.AZP)
				}
			}
		}
	} else if len(claims.Audience) == 1 {
		audience = string(claims.Audience[0])
	} else if len(claims.Audience) > 1 {
		return nil, fmt.Errorf("%w: multi-valued aud without azp", ErrGoogleInvalidCredential)
	}

	return &ValidatedGoogleIdentity{
		Subject:          claims.Subject,
		Email:            claims.Email,
		EmailVerified:    bool(claims.EmailVerified),
		DisplayName:      claims.Name,
		AvatarURL:        claims.Picture,
		Issuer:           googleCanonicalIssuer,
		Audience:         audience,
		UpstreamExpiry:   expiry,
		HostedDomain:     claims.HD,
		IsServiceAccount: isServiceAccount,
	}, nil
}

// ValidateAccessToken validates a Google access token by:
// 1. Calling Google's tokeninfo endpoint to get audience/issued_to/expiry
// 2. Calling Google's userinfo endpoint to get email/sub/profile
// 3. Cross-checking sub between both responses
// 4. Validating the issued-to client is in allowedClientIDs
//
// Per contract-review point 3: aud and azp/issued_to are not interchangeable.
// We pin to Google's tokeninfo endpoint which returns:
//   - azp: the client that obtained the token (authoritative issued-to)
//   - aud: the intended audience
//   - sub: stable subject identifier
//   - exp: expiry in seconds since epoch
//   - email: email address
//   - email_verified: boolean
//
// The authoritative issued-client field is `azp` (authorized party).
// If `aud` is also present and differs from `azp`, both must be in
// allowedClientIDs or the request fails closed.
func (v *googleCredentialValidator) ValidateAccessToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error) {
	if len(allowedClientIDs) == 0 {
		return nil, fmt.Errorf("%w: no allowed client IDs configured", ErrGENotConfigured)
	}

	// Step 1: Call Google tokeninfo endpoint. getTokenInfo already classifies
	// the failure (ErrGoogleInvalidCredential for a credential Google itself
	// rejected, ErrGoogleUpstreamError for a network/5xx/decode fault) — do
	// not re-wrap it as ErrGoogleUpstreamError here, or every tokeninfo 400
	// (the most common real rejection: an expired or revoked access token)
	// would be misreported as an upstream outage and never negatively cached
	// (review r1 finding 2).
	tokenInfo, err := v.getTokenInfo(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("tokeninfo call failed: %w", err)
	}

	// Validate that tokeninfo returned required fields.
	if tokenInfo.AZP == "" {
		return nil, fmt.Errorf("%w: azp (authorized party) missing from tokeninfo", ErrGoogleMissingField)
	}

	// The authoritative issued-client is azp. It must be in allowedClientIDs.
	if !containsString(allowedClientIDs, tokenInfo.AZP) {
		return nil, fmt.Errorf("%w: azp %q not in allowed set", ErrGoogleUntrustedAudience, tokenInfo.AZP)
	}

	// Per contract-review: reject disagreement among client-identifying fields.
	// If aud is present and differs from azp, reject unconditionally — even if
	// both are individually allowlisted. This prevents confused-deputy attacks
	// where a token with split aud/azp passes validation.
	if tokenInfo.AUD != "" && tokenInfo.AUD != tokenInfo.AZP {
		return nil, fmt.Errorf("%w: aud %q differs from azp %q",
			ErrGoogleFieldDisagreement, tokenInfo.AUD, tokenInfo.AZP)
	}

	// Validate expiry from tokeninfo.
	if int64(tokenInfo.ExpiresIn) <= 0 {
		return nil, fmt.Errorf("%w: no remaining lifetime from tokeninfo", ErrGENoRemainingLifetime)
	}
	upstreamExpiry := time.Now().Add(time.Duration(int64(tokenInfo.ExpiresIn)) * time.Second)

	// Step 2: Call Google userinfo for profile data. Same reasoning as
	// getTokenInfo above: preserve its classification rather than forcing
	// ErrGoogleUpstreamError.
	userInfo, err := v.getUserInfo(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("userinfo call failed: %w", err)
	}

	// Validate required userinfo fields.
	if userInfo.Sub == "" {
		return nil, fmt.Errorf("%w: sub missing from userinfo", ErrGoogleMissingField)
	}
	if userInfo.Email == "" {
		return nil, fmt.Errorf("%w: email missing from userinfo", ErrGoogleMissingField)
	}
	if !bool(userInfo.EmailVerified) {
		return nil, ErrGoogleUnverifiedEmail
	}

	// Step 3: Cross-check sub between tokeninfo and userinfo.
	// Per contract-review: cross-check stable sub, not only email.
	if tokenInfo.Sub != "" && tokenInfo.Sub != userInfo.Sub {
		return nil, fmt.Errorf("%w: sub from tokeninfo (%q) != userinfo (%q)",
			ErrGoogleFieldDisagreement, tokenInfo.Sub, userInfo.Sub)
	}
	// Cross-check email where both provide it.
	if tokenInfo.Email != "" && !strings.EqualFold(tokenInfo.Email, userInfo.Email) {
		return nil, fmt.Errorf("%w: email from tokeninfo (%q) != userinfo (%q)",
			ErrGoogleFieldDisagreement, tokenInfo.Email, userInfo.Email)
	}
	// Cross-check email_verified where tokeninfo provides it.
	if tokenInfo.EmailVerified != nil && bool(*tokenInfo.EmailVerified) != bool(userInfo.EmailVerified) {
		return nil, fmt.Errorf("%w: email_verified disagrees between tokeninfo and userinfo",
			ErrGoogleFieldDisagreement)
	}

	return &ValidatedGoogleIdentity{
		Subject:        userInfo.Sub,
		Email:          userInfo.Email,
		EmailVerified:  bool(userInfo.EmailVerified),
		DisplayName:    userInfo.Name,
		AvatarURL:      userInfo.Picture,
		Issuer:         googleCanonicalIssuer,
		Audience:       tokenInfo.AZP,
		UpstreamExpiry: upstreamExpiry,
		HostedDomain:   userInfo.HD,
		// Classify service accounts by the verified email claim. Rejection is
		// the caller's responsibility (design §4.2(i)); see ValidateIDToken.
		IsServiceAccount: isGoogleServiceAccount(userInfo.Email),
	}, nil
}

// ---------------------------------------------------------------------------
// Google API response types
// ---------------------------------------------------------------------------

// googleIDTokenClaims are the JWT claims in a Google ID token.
type googleIDTokenClaims struct {
	jwt.Claims
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
	HD            string   `json:"hd"`  // Hosted domain for Workspace accounts
	AZP           string   `json:"azp"` // Authorized party (may differ from aud in some flows)
}

// googleTokenInfoResponse is the response from Google's tokeninfo endpoint.
// Documented at https://googleapis.dev/nodejs/google-auth-library/latest/classes/OAuth2Client.html
// and https://developers.google.com/resources/api-libraries/documentation/oauth2/v2/java/latest/com/google/api/services/oauth2/model/Tokeninfo.html
//
// Per contract-review: azp is the authoritative issued-client field.
// aud and azp are distinct documented fields and are not interchangeable.
type googleTokenInfoResponse struct {
	// AZP is the authorized party — the client that obtained the token.
	// This is the authoritative issued-to client field.
	AZP string `json:"azp"`
	// AUD is the intended audience. May differ from AZP in some flows.
	AUD string `json:"aud"`
	// Sub is the stable subject identifier.
	Sub string `json:"sub"`
	// Email is the email address.
	Email string `json:"email"`
	// EmailVerified decodes from either boolean or string ("true"/"false")
	// because Google's tokeninfo endpoint may return either form.
	EmailVerified *flexBool `json:"email_verified,omitempty"`
	// ExpiresIn is the remaining lifetime in seconds.
	// Google's tokeninfo endpoint may return this as a JSON number or string.
	ExpiresIn flexInt64 `json:"expires_in"`
	// Scope is the granted OAuth scopes.
	Scope string `json:"scope"`
	// AccessType is "online" or "offline".
	AccessType string `json:"access_type"`
	// Error description if the token is invalid.
	Error string `json:"error_description"`
}

// flexBool decodes JSON values that may be boolean (true/false) or string
// ("true"/"false"). Google's tokeninfo endpoint sometimes returns
// email_verified as a string rather than a native boolean.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	switch s {
	case "true":
		*b = true
	case "false":
		*b = false
	default:
		return fmt.Errorf("flexBool: cannot decode %s", string(data))
	}
	return nil
}

// flexInt64 decodes JSON values that may be a number (3600) or a string
// ("3600"). Google's tokeninfo endpoint sometimes returns expires_in as
// a string rather than a native JSON number.
type flexInt64 int64

func (i *flexInt64) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("flexInt64: cannot decode %s: %w", string(data), err)
	}
	*i = flexInt64(n)
	return nil
}

// googleUserInfoResponse is the response from Google's userinfo endpoint.
type googleUserInfoResponse struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
	HD            string   `json:"hd"` // Hosted domain
}

// ---------------------------------------------------------------------------
// Google API calls
// ---------------------------------------------------------------------------

// isGoogleClientErrorStatus reports whether status is an HTTP status Google's
// tokeninfo/userinfo endpoints use to say "this credential is bad" (as
// opposed to "we're having trouble right now"). Google answers an invalid,
// expired or revoked access token with tokeninfo 400 {"error":"invalid_token"};
// 401 is included defensively for the same class of rejection. Everything
// else non-2xx (5xx, unexpected 3xx/4xx) is treated as an upstream fault, not
// a credential verdict (review r1 finding 2).
func isGoogleClientErrorStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnauthorized
}

func (v *googleCredentialValidator) getTokenInfo(ctx context.Context, token string) (*googleTokenInfoResponse, error) {
	// POST to tokeninfo endpoint with access_token in the body.
	// Do NOT pass the token as a query parameter to avoid logging exposure.
	// Use proper URL encoding for the form body value.
	formData := url.Values{"access_token": {token}}
	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenInfoURL,
		strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: tokeninfo request failed: %v", ErrGoogleUpstreamError, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: tokeninfo read failed: %v", ErrGoogleUpstreamError, err)
	}

	// A 400/401 means Google rejected the credential itself (e.g. expired,
	// revoked, malformed) — that is ErrGoogleInvalidCredential, mapped to 401
	// and eligible for the negative cache. Any other non-200 (5xx, or an
	// unexpected status) is an upstream fault: ErrGoogleUpstreamError, mapped
	// to 503 and never negatively cached.
	if isGoogleClientErrorStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%w: tokeninfo returned %d", ErrGoogleInvalidCredential, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: tokeninfo returned %d", ErrGoogleUpstreamError, resp.StatusCode)
	}

	var info googleTokenInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("%w: tokeninfo decode failed: %v", ErrGoogleUpstreamError, err)
	}

	if info.Error != "" {
		// A 200 response can still carry a body-level error field for an
		// invalid credential — also a credential verdict, not an upstream
		// fault.
		return nil, fmt.Errorf("%w: tokeninfo error: %s", ErrGoogleInvalidCredential, info.Error)
	}

	return &info, nil
}

func (v *googleCredentialValidator) getUserInfo(ctx context.Context, token string) (*googleUserInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", googleUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo request failed: %v", ErrGoogleUpstreamError, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo read failed: %v", ErrGoogleUpstreamError, err)
	}

	if isGoogleClientErrorStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%w: userinfo returned %d", ErrGoogleInvalidCredential, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: userinfo returned %d", ErrGoogleUpstreamError, resp.StatusCode)
	}

	var info googleUserInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("%w: userinfo decode failed: %v", ErrGoogleUpstreamError, err)
	}

	return &info, nil
}

// ---------------------------------------------------------------------------
// JWKS cache for Google ID token verification
// ---------------------------------------------------------------------------

type googleJWKSCache struct {
	mu         sync.RWMutex
	keys       *jose.JSONWebKeySet
	fetchedAt  time.Time
	ttl        time.Duration
	maxStale   time.Duration // Maximum age of stale cache before hard-failing.
	httpClient *http.Client
}

func newGoogleJWKSCache(client *http.Client) *googleJWKSCache {
	return &googleJWKSCache{
		ttl:        time.Hour,
		maxStale:   24 * time.Hour, // Bounded stale fallback: max 24h.
		httpClient: client,
	}
}

// get returns the cached JWKS, refreshing if the cache is expired.
// Uses a bounded stale fallback: stale keys are accepted for up to maxStale
// duration when a refresh fails, after which the cache hard-fails.
func (c *googleJWKSCache) get(ctx context.Context) (*jose.JSONWebKeySet, error) {
	c.mu.RLock()
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		keys := c.keys
		c.mu.RUnlock()
		return keys, nil
	}
	staleKeys := c.keys
	staleAge := time.Since(c.fetchedAt)
	c.mu.RUnlock()

	return c.refresh(ctx, staleKeys, staleAge)
}

// forceRefresh unconditionally fetches fresh JWKS (used when a token's kid
// doesn't match any cached key). Only calls the remote if the cache is older
// than a minimum interval to avoid excessive fetches.
func (c *googleJWKSCache) forceRefresh(ctx context.Context) (*jose.JSONWebKeySet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Rate-limit force refreshes: don't refetch if we fetched within the last 30s.
	if c.keys != nil && time.Since(c.fetchedAt) < 30*time.Second {
		return c.keys, nil
	}

	return c.fetchLocked(ctx)
}

func (c *googleJWKSCache) refresh(ctx context.Context, staleKeys *jose.JSONWebKeySet, staleAge time.Duration) (*jose.JSONWebKeySet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock.
	if c.keys != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.keys, nil
	}

	keys, err := c.fetchLocked(ctx)
	if err != nil {
		// Bounded stale fallback: accept stale keys only up to maxStale.
		if staleKeys != nil && staleAge < c.maxStale {
			slog.Warn("Google JWKS refresh failed, using bounded stale cache",
				"error", err, "stale_age", staleAge)
			return staleKeys, nil
		}
		return nil, err
	}
	return keys, nil
}

// fetchLocked fetches JWKS from Google. Must be called with mu held.
func (c *googleJWKSCache) fetchLocked(ctx context.Context) (*jose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", googleJWKSURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("JWKS fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS fetch returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return nil, fmt.Errorf("JWKS read failed: %w", err)
	}

	var jwks jose.JSONWebKeySet
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("JWKS decode failed: %w", err)
	}

	c.keys = &jwks
	c.fetchedAt = time.Now()
	return c.keys, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// canonicalizeGoogleIssuer normalizes both known Google issuer forms to the
// canonical HTTPS form for persistence. This prevents one Google account from
// acquiring two external identity rows via different issuer spellings.
func canonicalizeGoogleIssuer(issuer string) string {
	if issuer == googleIssuerBare || issuer == googleIssuerHTTPS {
		return googleCanonicalIssuer
	}
	return issuer
}

// isAllowedAudience checks if any audience in the token matches an allowed
// client ID.
func isAllowedAudience(audiences jwt.Audience, allowed []string) bool {
	for _, aud := range audiences {
		if containsString(allowed, string(aud)) {
			return true
		}
	}
	return false
}

// containsString checks if a string slice contains the target.
func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}

// isGoogleServiceAccount detects Google service account emails by their
// characteristic suffixes.
func isGoogleServiceAccount(email string) bool {
	email = strings.ToLower(email)
	return strings.HasSuffix(email, ".iam.gserviceaccount.com") ||
		strings.HasSuffix(email, "@appspot.gserviceaccount.com") ||
		strings.HasSuffix(email, "@developer.gserviceaccount.com") ||
		strings.HasSuffix(email, "@system.gserviceaccount.com")
}

// isAuthoritativeEmailDomain checks if the email domain is authoritative per
// contract-review point 1: only Gmail or verified Google Workspace (hd claim)
// emails are authoritative for automatic first-time user bootstrap.
//
// Third-party email domains (e.g. a custom domain on a personal Google account
// without Workspace) are NOT authoritative because email_verified can remain
// true even after the mailbox ownership changes.
func isAuthoritativeEmailDomain(email, hostedDomain string) bool {
	email = strings.ToLower(email)

	// Gmail addresses are authoritative — Google controls the domain.
	if strings.HasSuffix(email, "@gmail.com") || strings.HasSuffix(email, "@googlemail.com") {
		return true
	}

	// Google Workspace accounts with a verified hd claim are authoritative —
	// the organization controls the domain through Workspace.
	if hostedDomain != "" {
		atIdx := strings.LastIndex(email, "@")
		if atIdx >= 0 {
			domain := email[atIdx+1:]
			if strings.EqualFold(domain, hostedDomain) {
				return true
			}
		}
	}

	return false
}
