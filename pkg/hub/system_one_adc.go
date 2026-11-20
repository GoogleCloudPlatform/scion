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
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

const (
	// SystemOneADCReuseExpiry is the early-refresh window passed to
	// oauth2.ReuseTokenSourceWithExpiry for Hub-side Google ADC token sources
	// (PV3 brief Addendum B §B4).
	SystemOneADCReuseExpiry = 5 * time.Minute

	// Allowed Google ADC credential type categories (PV3 brief Addendum B §B4).
	SystemOneADCTypeServiceAccount             = "service_account"
	SystemOneADCTypeExternalAccount            = "external_account"
	SystemOneADCTypeImpersonatedServiceAccount = "impersonated_service_account"
	SystemOneADCTypeAuthorizedUser             = "authorized_user"
	SystemOneADCTypeMetadata                   = "metadata"

	// SystemOneADCUnresolved is rendered on instance create/get/list when ADC
	// credential metadata has not yet been resolved in-process by an evaluation
	// or probe, avoiding any ADC discovery or metadata I/O on read/create paths.
	SystemOneADCUnresolved = "unresolved (run probe)"

	maxSystemOneADCEmailBytes = 128
)

// SystemOneADCCredentials pairs an oauth2.TokenSource with its non-secret
// credential category metadata (PV3 brief Addendum B §§B4, B6). Token values
// and credential file contents are never stored here.
type SystemOneADCCredentials struct {
	TokenSource         oauth2.TokenSource
	CredentialType      string
	ServiceAccountEmail string
}

// SystemOneADCMetadata holds non-secret identity metadata exposed on instance
// get/list/create and probe provenance responses.
type SystemOneADCMetadata struct {
	CredentialType      string
	ServiceAccountEmail string
}

// SystemOneADCProvider discovers Google Application Default Credentials for a
// canonical scope set. Production uses google.FindDefaultCredentials (via the
// Google library's own HTTP client, outside provider egress); tests inject a
// fake implementation via SetSystemOneADCForTesting.
type SystemOneADCProvider func(ctx context.Context, scopes []string) (*SystemOneADCCredentials, error)

// isAllowedSystemOneADCType reports whether credType is one of the allowed
// Google ADC credential categories in PV3 brief Addendum B §B4.
func isAllowedSystemOneADCType(credType string) bool {
	switch credType {
	case SystemOneADCTypeServiceAccount,
		SystemOneADCTypeExternalAccount,
		SystemOneADCTypeImpersonatedServiceAccount,
		SystemOneADCTypeAuthorizedUser,
		SystemOneADCTypeMetadata:
		return true
	default:
		return false
	}
}

// defaultSystemOneADCProvider resolves Application Default Credentials using
// golang.org/x/oauth2/google.FindDefaultCredentials. Token minting uses a
// background context so the cached oauth2.TokenSource uses the Google library's
// own HTTP path (never provider egress) and outlives the first request's
// deadline.
func defaultSystemOneADCProvider(ctx context.Context, scopes []string) (*SystemOneADCCredentials, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	creds, err := google.FindDefaultCredentials(context.Background(), scopes...)
	if err != nil {
		return nil, classifySystemOneADCError(ctx, err)
	}
	if creds == nil || creds.TokenSource == nil {
		return nil, egress.ErrAuth
	}

	if len(creds.JSON) > 0 {
		credType, saEmail, inspectErr := inspectGoogleCredentialsJSON(creds.JSON)
		if inspectErr != nil {
			return nil, egress.ErrAuth
		}
		return &SystemOneADCCredentials{
			TokenSource:         creds.TokenSource,
			CredentialType:      credType,
			ServiceAccountEmail: saEmail,
		}, nil
	}

	var saEmail string
	emailCtx := ctx
	if emailCtx == nil {
		emailCtx = context.Background()
	}
	if email, emailErr := metadata.EmailWithContext(emailCtx, "default"); emailErr == nil {
		saEmail = sanitizeSystemOneServiceAccountEmail(email)
	}
	return &SystemOneADCCredentials{
		TokenSource:         creds.TokenSource,
		CredentialType:      SystemOneADCTypeMetadata,
		ServiceAccountEmail: saEmail,
	}, nil
}

// inspectGoogleCredentialsJSON extracts ONLY the non-secret credential type
// category and service-account email (when exposed) from a Google ADC JSON
// payload, rejecting disallowed credential types with egress.ErrAuth. Secret
// fields (private_key, client_secret, refresh_token) are never decoded or
// retained.
func inspectGoogleCredentialsJSON(raw []byte) (string, string, error) {
	var meta struct {
		Type                           string `json:"type"`
		ClientEmail                    string `json:"client_email"`
		ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", "", egress.ErrAuth
	}

	switch strings.TrimSpace(meta.Type) {
	case SystemOneADCTypeServiceAccount:
		return SystemOneADCTypeServiceAccount, sanitizeSystemOneServiceAccountEmail(meta.ClientEmail), nil

	case SystemOneADCTypeExternalAccount:
		email := extractImpersonatedServiceAccountEmail(meta.ServiceAccountImpersonationURL)
		if email == "" {
			email = sanitizeSystemOneServiceAccountEmail(meta.ClientEmail)
		}
		return SystemOneADCTypeExternalAccount, email, nil

	case SystemOneADCTypeImpersonatedServiceAccount:
		email := extractImpersonatedServiceAccountEmail(meta.ServiceAccountImpersonationURL)
		if email == "" {
			email = sanitizeSystemOneServiceAccountEmail(meta.ClientEmail)
		}
		return SystemOneADCTypeImpersonatedServiceAccount, email, nil

	case SystemOneADCTypeAuthorizedUser:
		return SystemOneADCTypeAuthorizedUser, "", nil

	default:
		return "", "", egress.ErrAuth
	}
}

func extractImpersonatedServiceAccountEmail(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	const marker = "/serviceAccounts/"
	idx := strings.Index(u.Path, marker)
	if idx < 0 {
		return ""
	}
	rest := u.Path[idx+len(marker):]
	candidate, _, _ := strings.Cut(rest, ":")
	candidate, _, _ = strings.Cut(candidate, "/")
	if decoded, uErr := url.PathUnescape(candidate); uErr == nil {
		candidate = decoded
	}
	return sanitizeSystemOneServiceAccountEmail(candidate)
}

func sanitizeSystemOneServiceAccountEmail(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxSystemOneADCEmailBytes {
		return ""
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

func canonicalADCScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, egress.ErrAuth
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, egress.ErrAuth
	}
	sort.Strings(out)
	return out, nil
}

func canonicalADCScopesKey(scopes []string) string {
	var b strings.Builder
	for _, s := range scopes {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
		b.WriteByte(';')
	}
	return b.String()
}

// hubSystemOneADCCache caches one oauth2.ReuseTokenSourceWithExpiry(..., 5m)
// per canonical scope set in memory for the lifetime of the Hub process
// (PV3 brief Addendum B §B4). Quota project is never part of the cache key.
type hubSystemOneADCCache struct {
	mu       sync.Mutex
	provider SystemOneADCProvider
	entries  map[string]*SystemOneADCCredentials
}

func newHubSystemOneADCCache(provider SystemOneADCProvider) *hubSystemOneADCCache {
	return &hubSystemOneADCCache{
		provider: provider,
		entries:  make(map[string]*SystemOneADCCredentials),
	}
}

func (c *hubSystemOneADCCache) resolve(ctx context.Context, scopes []string) (*SystemOneADCCredentials, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if c == nil {
		return nil, egress.ErrAuth
	}

	canonScopes, err := canonicalADCScopes(scopes)
	if err != nil {
		return nil, err
	}
	key := canonicalADCScopesKey(canonScopes)

	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	prov := c.provider
	c.mu.Unlock()

	if prov == nil {
		return nil, egress.ErrAuth
	}

	resolved, err := prov(ctx, slices.Clone(canonScopes))
	if err != nil {
		return nil, classifySystemOneADCError(ctx, err)
	}
	if resolved == nil || resolved.TokenSource == nil || !isAllowedSystemOneADCType(resolved.CredentialType) {
		return nil, egress.ErrAuth
	}

	entry := &SystemOneADCCredentials{
		TokenSource:         oauth2.ReuseTokenSourceWithExpiry(nil, resolved.TokenSource, SystemOneADCReuseExpiry),
		CredentialType:      resolved.CredentialType,
		ServiceAccountEmail: sanitizeSystemOneServiceAccountEmail(resolved.ServiceAccountEmail),
	}

	c.mu.Lock()
	if existing, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.entries[key] = entry
	c.mu.Unlock()
	return entry, nil
}

// peek reads already-resolved non-secret ADC metadata from the in-memory scope
// cache without invoking provider discovery or performing any file/network I/O.
func (c *hubSystemOneADCCache) peek(scopes []string) (SystemOneADCMetadata, bool) {
	if c == nil {
		return SystemOneADCMetadata{}, false
	}
	canonScopes, err := canonicalADCScopes(scopes)
	if err != nil {
		return SystemOneADCMetadata{}, false
	}
	key := canonicalADCScopesKey(canonScopes)
	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()
	if !ok || cached == nil {
		return SystemOneADCMetadata{}, false
	}
	return SystemOneADCMetadata{
		CredentialType:      cached.CredentialType,
		ServiceAccountEmail: cached.ServiceAccountEmail,
	}, true
}

// SetSystemOneADCForTesting replaces the Hub's Google Application Default
// Credentials provider and clears any in-memory cached ADC token sources so
// offline tests inject a fake oauth2.TokenSource without touching live ADC
// files, gcloud credentials, or metadata servers.
//
// For tests only; production code must never call it.
func (s *Server) SetSystemOneADCForTesting(tb interface{ Helper() }, provider SystemOneADCProvider) {
	if tb == nil {
		panic("SetSystemOneADCForTesting requires a non-nil testing.TB")
	}
	tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systemOneADCCache = newHubSystemOneADCCache(provider)
}

func (s *Server) systemOneADC() *hubSystemOneADCCache {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	cache := s.systemOneADCCache
	s.mu.RUnlock()
	return cache
}

// peekSystemOneADCMetadata reads non-secret ADC credential metadata from the
// in-memory scope cache if already resolved in-process by a prior evaluation or
// probe, performing zero ADC discovery or metadata I/O on cold cache misses.
func (s *Server) peekSystemOneADCMetadata(scopes []string) (SystemOneADCMetadata, bool) {
	cache := s.systemOneADC()
	if cache == nil {
		return SystemOneADCMetadata{}, false
	}
	return cache.peek(scopes)
}

// classifySystemOneADCError maps ADC discovery and OAuth/STS/metadata token
// minting errors to content-free egress sentinels per PV3 brief Addendum B §B7:
//   - No ADC found, unreadable/invalid credentials file, disallowed credential
//     type, or token refresh invalid_grant / HTTP 400 / 401 / 403 -> egress.ErrAuth
//   - Metadata server, STS, or token endpoint timeout, connection failure, or
//     HTTP 5xx / 408 / 429 -> egress.ErrCredentialUnavailable
//
// Raw error text, token values, and key-file contents are never propagated.
func classifySystemOneADCError(ctx context.Context, err error) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, egress.ErrAuth) {
		return egress.ErrAuth
	}
	if errors.Is(err, egress.ErrCredentialUnavailable) {
		return egress.ErrCredentialUnavailable
	}

	var retErr *oauth2.RetrieveError
	if errors.As(err, &retErr) && retErr != nil {
		switch strings.ToLower(strings.TrimSpace(retErr.ErrorCode)) {
		case "invalid_grant",
			"invalid_client",
			"unauthorized_client",
			"invalid_scope",
			"invalid_request",
			"access_denied",
			"unsupported_grant_type":
			return egress.ErrAuth
		}
		if retErr.Response != nil {
			sc := retErr.Response.StatusCode
			if sc == http.StatusBadRequest || sc == http.StatusUnauthorized || sc == http.StatusForbidden {
				return egress.ErrAuth
			}
			if sc >= 500 || sc == http.StatusRequestTimeout || sc == http.StatusTooManyRequests {
				return egress.ErrCredentialUnavailable
			}
		}
		return egress.ErrAuth
	}

	var metaNotDef metadata.NotDefinedError
	if errors.As(err, &metaNotDef) {
		return egress.ErrAuth
	}
	var metaErr *metadata.Error
	if errors.As(err, &metaErr) && metaErr != nil {
		if metaErr.Code >= 500 || metaErr.Code == http.StatusRequestTimeout || metaErr.Code == http.StatusTooManyRequests {
			return egress.ErrCredentialUnavailable
		}
		return egress.ErrAuth
	}

	// Local credential-file read / parse failures map to ErrAuth before any
	// generic io/net check.
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return egress.ErrAuth
	}
	var synErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &synErr) || errors.As(err, &typeErr) {
		return egress.ErrAuth
	}

	// Token endpoint, STS, or metadata server timeout / connection failure ->
	// ErrCredentialUnavailable (caller ctx was checked above and is still live).
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return egress.ErrCredentialUnavailable
	}
	var urlErr *url.Error
	var netErr net.Error
	if errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return egress.ErrCredentialUnavailable
	}

	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return egress.ErrAuth
}

// GoogleAccessToken resolves an OAuth2 access token for a google_access_token
// System One instance from the Hub's in-memory ADC cache, enforcing the
// authorized_user -> quota_project requirement and recording non-secret
// credential category metadata in Evaluation provenance.
func (c *hubSystemOneCredentialSource) GoogleAccessToken(ctx context.Context, inst systemoneruntime.Instance) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if inst.Auth.Kind != store.SystemOneAuthKindGoogleAccessToken {
		return "", egress.ErrAuth
	}
	if c.srv == nil {
		return "", egress.ErrAuth
	}
	if !c.srv.isPinnedInstanceEndpointAllowed(inst.Adapter, inst.Endpoint) {
		return "", egress.ErrAuth
	}
	cache := c.srv.systemOneADC()
	if cache == nil {
		return "", egress.ErrAuth
	}

	adc, err := cache.resolve(ctx, inst.Auth.Scopes)
	if err != nil {
		return "", classifySystemOneADCError(ctx, err)
	}
	systemoneruntime.RecordAuthIdentity(ctx, adc.CredentialType, adc.ServiceAccountEmail)

	if adc.CredentialType == SystemOneADCTypeAuthorizedUser &&
		strings.TrimSpace(inst.Options["quota_project"]) == "" {
		return "", egress.ErrAuth
	}
	if adc.TokenSource == nil {
		return "", egress.ErrAuth
	}

	tok, err := adc.TokenSource.Token()
	if err != nil {
		return "", classifySystemOneADCError(ctx, err)
	}
	if tok == nil || !isValidSystemOneAPIKey(tok.AccessToken) {
		return "", egress.ErrAuth
	}
	return tok.AccessToken, nil
}
