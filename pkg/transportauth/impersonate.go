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

package transportauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Assign-mode ID tokens for another service account (ptone/scion#4240).
//
// In "assign" metadata mode the scion metadata emulator serves the agent's
// assigned service account: an OAuth access token at
// /computeMetadata/v1/instance/service-accounts/default/token. FromEnv
// does not use the emulator (it is a redirected metadata server, see
// IsMetadataRedirected), so an assign-mode CLI had no way to obtain ID
// tokens for a target hub.
//
// IAMIDTokenSource fills that gap for an explicit caller (the CLI's target
// hub profile): it takes the assigned account's access token from the
// emulator and calls IAM Credentials generateIdToken AS another service
// account (the target's issuer or IAP account) for one audience. This only
// works when the operator granted the assigned account
// roles/iam.serviceAccountOpenIdTokenCreator on that target account.
//
// Tokens live only in memory. Errors never carry token material.

const (
	// EnvGCEMetadataHost is where the broker points the agent at the
	// metadata emulator in assign and block modes.
	EnvGCEMetadataHost = "GCE_METADATA_HOST"

	// metadataModeAssign mirrors store.GCPMetadataModeAssign (see the note
	// on metadataModePassthrough).
	metadataModeAssign = "assign"

	// DefaultIAMCredentialsURL is the IAM Credentials API base URL.
	DefaultIAMCredentialsURL = "https://iamcredentials.googleapis.com"

	// iamFetchTimeout bounds one emulator or IAM call.
	iamFetchTimeout = 15 * time.Second

	// maxTokenResponseBytes bounds a token response body.
	maxTokenResponseBytes = 64 << 10
)

// ErrNotAssignMode is returned by AssignModeMetadataBaseURL when the agent
// does not run in assign metadata mode.
var ErrNotAssignMode = errors.New("the assigned service account is not available: SCION_METADATA_MODE is not \"assign\"")

// AssignModeMetadataBaseURL returns the metadata emulator base URL
// ("http://<GCE_METADATA_HOST>") when getenv reports assign mode and a
// loopback emulator address, and an error otherwise. The emulator always
// runs in the agent's own network namespace, so a non-loopback host is
// refused.
func AssignModeMetadataBaseURL(getenv func(string) string) (string, error) {
	if getenv(EnvMetadataMode) != metadataModeAssign {
		return "", ErrNotAssignMode
	}
	host := strings.TrimSpace(getenv(EnvGCEMetadataHost))
	if host == "" {
		return "", fmt.Errorf("the metadata emulator is not available: %s is not set", EnvGCEMetadataHost)
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h != "localhost" {
		ip := net.ParseIP(h)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("refusing metadata emulator address %q: it must be a loopback address", host)
		}
	}
	return "http://" + host, nil
}

// AccessTokenProvider yields OAuth access tokens for the caller's own
// service account.
type AccessTokenProvider interface {
	AccessToken() (string, error)
}

// MetadataAccessTokenSource fetches and caches the assigned service
// account's OAuth access token from the metadata emulator. Thread-safe.
type MetadataAccessTokenSource struct {
	baseURL    string
	httpClient *http.Client
	now        func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewMetadataAccessTokenSource returns a source reading access tokens from
// the metadata server at baseURL (see AssignModeMetadataBaseURL).
func NewMetadataAccessTokenSource(baseURL string) *MetadataAccessTokenSource {
	return &MetadataAccessTokenSource{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: iamFetchTimeout},
		now:        time.Now,
	}
}

// AccessToken returns a cached access token, fetching a new one once the
// cached one is within RefreshMargin of its expiry.
func (s *MetadataAccessTokenSource) AccessToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expiresAt.Add(-RefreshMargin)) {
		return s.token, nil
	}
	req, err := http.NewRequest(http.MethodGet, s.baseURL+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", fmt.Errorf("assigned service account token: build request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("assigned service account token: metadata emulator unreachable: %w", scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return "", fmt.Errorf("assigned service account token: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("assigned service account token: metadata emulator returned %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("assigned service account token: malformed metadata emulator response")
	}
	if tr.ExpiresIn <= 0 {
		return "", errors.New("assigned service account token: metadata emulator response has no expiry")
	}
	s.token = tr.AccessToken
	s.expiresAt = s.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return s.token, nil
}

// IAMIDTokenSource is a TokenSource that mints Google ID tokens AS
// TargetServiceAccount for one audience, through IAM Credentials
// generateIdToken, authenticated with the caller's own access token. It
// caches the ID token and refreshes it RefreshMargin before expiry. Every
// minted token is checked locally: its aud must equal the audience and its
// email the target account. Thread-safe.
type IAMIDTokenSource struct {
	targetSA   string
	audience   string
	access     AccessTokenProvider
	iamBaseURL string
	httpClient *http.Client
	now        func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// IAMIDTokenSourceOption configures an IAMIDTokenSource.
type IAMIDTokenSourceOption func(*IAMIDTokenSource)

// WithIAMCredentialsURL overrides the IAM Credentials base URL (tests).
func WithIAMCredentialsURL(u string) IAMIDTokenSourceOption {
	return func(s *IAMIDTokenSource) { s.iamBaseURL = strings.TrimRight(u, "/") }
}

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) IAMIDTokenSourceOption {
	return func(s *IAMIDTokenSource) { s.now = now }
}

// NewIAMIDTokenSource returns a source minting ID tokens as targetSA for
// audience. Both must be non-empty and access non-nil.
func NewIAMIDTokenSource(targetSA, audience string, access AccessTokenProvider, opts ...IAMIDTokenSourceOption) (*IAMIDTokenSource, error) {
	targetSA = strings.TrimSpace(targetSA)
	audience = strings.TrimSpace(audience)
	if targetSA == "" || !strings.Contains(targetSA, "@") || strings.ContainsAny(targetSA, "/?#%: ") {
		return nil, fmt.Errorf("invalid target service account %q", targetSA)
	}
	if audience == "" {
		return nil, errors.New("an ID token audience is required")
	}
	if access == nil {
		return nil, errors.New("an access token provider is required")
	}
	s := &IAMIDTokenSource{
		targetSA:   targetSA,
		audience:   audience,
		access:     access,
		iamBaseURL: DefaultIAMCredentialsURL,
		httpClient: &http.Client{Timeout: iamFetchTimeout},
		now:        time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Token returns a valid ID token, minting a new one when the cached token
// is within RefreshMargin of its expiry. Any failure is returned: there is
// no fallback credential.
func (s *IAMIDTokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expiresAt.Add(-RefreshMargin)) {
		return s.token, nil
	}
	access, err := s.access.AccessToken()
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{"audience": s.audience, "includeEmail": true})
	endpoint := s.iamBaseURL + "/v1/projects/-/serviceAccounts/" + url.PathEscape(s.targetSA) + ":generateIdToken"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("generateIdToken as %s: build request: %w", s.targetSA, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("generateIdToken as %s: IAM Credentials unreachable: %w", s.targetSA, scrubURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return "", fmt.Errorf("generateIdToken as %s: read response: %w", s.targetSA, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("generateIdToken as %s: IAM Credentials returned %d%s", s.targetSA, resp.StatusCode, iamErrorStatus(body))
	}
	var tr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.Token == "" {
		return "", fmt.Errorf("generateIdToken as %s: malformed response", s.targetSA)
	}
	claims, err := DecodeIDTokenClaims(tr.Token)
	if err != nil {
		return "", fmt.Errorf("generateIdToken as %s: %w", s.targetSA, err)
	}
	if !claims.HasAudience(s.audience) {
		return "", fmt.Errorf("generateIdToken as %s: token audience does not match the requested audience", s.targetSA)
	}
	if !strings.EqualFold(claims.Email, s.targetSA) {
		return "", fmt.Errorf("generateIdToken as %s: token email does not match the service account", s.targetSA)
	}
	if claims.Expiry.IsZero() || !claims.Expiry.After(s.now()) {
		return "", fmt.Errorf("generateIdToken as %s: token has no future expiry", s.targetSA)
	}
	s.token = tr.Token
	s.expiresAt = claims.Expiry
	return s.token, nil
}

// SetToken is part of TokenSource. An IAMIDTokenSource mints its own
// tokens; a pushed token replaces the cache like for the other sources.
func (s *IAMIDTokenSource) SetToken(token string, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	s.expiresAt = expiry
}

// Expiry returns the cached token's expiry (zero before the first mint).
func (s *IAMIDTokenSource) Expiry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

// TargetServiceAccount returns the service account tokens are minted as.
func (s *IAMIDTokenSource) TargetServiceAccount() string { return s.targetSA }

// Audience returns the audience tokens are minted for.
func (s *IAMIDTokenSource) Audience() string { return s.audience }

// IDTokenClaims are the non-secret claims of a Google ID token, decoded
// locally without verifying the signature (for display and local
// consistency checks only; the receiving server verifies).
type IDTokenClaims struct {
	Issuer    string
	Subject   string
	Email     string
	Audiences []string
	Expiry    time.Time
	IssuedAt  time.Time
}

// HasAudience reports whether aud is one of the token's audiences.
func (c IDTokenClaims) HasAudience(aud string) bool {
	for _, a := range c.Audiences {
		if a == aud {
			return true
		}
	}
	return false
}

// DecodeIDTokenClaims decodes the payload of a JWT. It never returns the
// token or any part of it in an error.
func DecodeIDTokenClaims(token string) (IDTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return IDTokenClaims{}, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return IDTokenClaims{}, errors.New("token payload is not base64url")
	}
	var raw struct {
		Iss   string          `json:"iss"`
		Sub   string          `json:"sub"`
		Email string          `json:"email"`
		Aud   json.RawMessage `json:"aud"`
		Exp   int64           `json:"exp"`
		Iat   int64           `json:"iat"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return IDTokenClaims{}, errors.New("token payload is not JSON")
	}
	c := IDTokenClaims{Issuer: raw.Iss, Subject: raw.Sub, Email: raw.Email}
	if len(raw.Aud) > 0 {
		var one string
		if json.Unmarshal(raw.Aud, &one) == nil {
			c.Audiences = []string{one}
		} else {
			_ = json.Unmarshal(raw.Aud, &c.Audiences)
		}
	}
	if raw.Exp > 0 {
		c.Expiry = time.Unix(raw.Exp, 0)
	}
	if raw.Iat > 0 {
		c.IssuedAt = time.Unix(raw.Iat, 0)
	}
	return c, nil
}

// iamErrorStatus extracts the Google API error status and message (never
// token material: the request carried none in the body, and a response
// error does not echo the bearer) for an error message.
func iamErrorStatus(body []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || (e.Error.Status == "" && e.Error.Message == "") {
		return ""
	}
	msg := e.Error.Message
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Sprintf(" (%s: %s)", e.Error.Status, msg)
}

// scrubURLError drops the URL from a *url.Error so a query string never
// reaches an error message.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
