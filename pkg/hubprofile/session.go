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

package hubprofile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Wire headers of the target hub's workload issuer delegation arm.
const (
	HeaderIssuerDelegation = "X-Scion-Issuer-Delegation"
	HeaderRunLabel         = "X-Scion-Run-Label"

	// ReissueMargin is how close to its expiry a cached member-tier token
	// is re-issued.
	ReissueMargin = transportauth.RefreshMargin

	testInfraStatusPath = "/api/v1/test-infra/status"

	// IssuerAudiencePrefix prefixes every hub's issuer audience
	// ("scion-test-issuer:<hub id>", computed by the hub).
	IssuerAudiencePrefix = "scion-test-issuer:"
)

func hasControlOrSpace(s string) bool {
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return true
		}
	}
	return false
}

// Bearer sources reported by a session.
const (
	SourceIssuer      = "profile:issuer"
	sourceAliasPrefix = "profile:"
)

// credentialHeaders are the app-layer credential headers a profile request
// must never carry besides its own Authorization.
var credentialHeaders = []string{"X-Scion-Agent-Token", "X-Scion-Broker-Token", "X-Scion-Federation-Token", "X-Scion-Broker-ID", "X-Scion-Run-Id", "X-Scion-Dev-Token", "X-API-Key"}

// Deps are a session's external dependencies. Zero values use the real
// ones.
type Deps struct {
	// Getenv reads the environment (default os.Getenv).
	Getenv func(string) string
	// BaseTransport is the HTTP transport for target hub requests
	// (default http.DefaultTransport).
	BaseTransport http.RoundTripper
	// AccessTokens returns the provider of the assigned service account's
	// access token (default: the metadata emulator in assign mode).
	AccessTokens func(getenv func(string) string) (transportauth.AccessTokenProvider, error)
	// IAMCredentialsURL overrides the IAM Credentials base URL.
	IAMCredentialsURL string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (d *Deps) fill() {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.BaseTransport == nil {
		d.BaseTransport = http.DefaultTransport
	}
	if d.AccessTokens == nil {
		d.AccessTokens = func(getenv func(string) string) (transportauth.AccessTokenProvider, error) {
			base, err := transportauth.AssignModeMetadataBaseURL(getenv)
			if err != nil {
				return nil, err
			}
			return transportauth.NewMetadataAccessTokenSource(base), nil
		}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
}

// Options select what a session acts as.
type Options struct {
	Home    string
	Profile *Profile
	// Alias, when set, makes the session act as that cached test identity
	// of Run. Otherwise it acts as the profile's member issuer delegation.
	Alias string
	// Run is the run label: required with Alias, optional otherwise (sent
	// as the issuance audit's run label).
	Run string
}

// Session is an open target-hub profile: the in-process token sources and
// the HTTP plumbing for one CLI invocation. It never writes a token
// anywhere except the identity cache.
type Session struct {
	opts Options
	deps Deps

	endpointURL *url.URL

	accessOnce sync.Once
	access     transportauth.AccessTokenProvider
	accessErr  error

	transport *transportauth.IAMIDTokenSource // nil for transport none

	issuerMu       sync.Mutex
	issuerAudience string
	issuer         *transportauth.IAMIDTokenSource

	entryMu sync.Mutex
	entry   *CacheEntry

	recorder *wireRecorder
	hc       *http.Client
}

// Open validates the profile and options and prepares a session. It does
// not contact the target hub or mint any token yet.
func Open(opts Options, deps Deps) (*Session, error) {
	deps.fill()
	p := opts.Profile
	if p == nil {
		return nil, errors.New("no hub profile")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	endpoint, err := NormalizeEndpoint(p.Endpoint)
	if err != nil {
		return nil, err
	}
	if err := CheckNotOwnHub(endpoint, deps.Getenv); err != nil {
		return nil, fmt.Errorf("hub profile %q: %w", p.Name, err)
	}
	eu, _ := url.Parse(endpoint)
	if opts.Alias != "" {
		if err := ValidateName("alias", opts.Alias); err != nil {
			return nil, err
		}
		if opts.Run == "" {
			return nil, fmt.Errorf("--as %s needs --run <label> (or %s): cached identities belong to a run", opts.Alias, EnvHubRun)
		}
	}
	if opts.Run != "" {
		if err := ValidateName("run label", opts.Run); err != nil {
			return nil, err
		}
	}
	s := &Session{opts: opts, deps: deps, endpointURL: eu, recorder: &wireRecorder{}}
	s.opts.Profile = &Profile{}
	*s.opts.Profile = *p
	s.opts.Profile.Endpoint = endpoint

	if p.Transport == TransportIAP {
		src, err := transportauth.NewIAMIDTokenSource(p.UATServiceAccount, p.IAPAudience, lazyAccess{s}, s.iamOpts()...)
		if err != nil {
			return nil, err
		}
		s.transport = src
	}
	s.recorder.base = deps.BaseTransport
	s.hc = &http.Client{Transport: &profileTransport{s: s, base: s.recorder}}

	if opts.Alias != "" {
		e, err := ReadEntry(opts.Home, p.Name, opts.Run, opts.Alias)
		if err != nil {
			return nil, err
		}
		if !sameEndpointExact(e.Endpoint, endpoint) {
			return nil, fmt.Errorf("cached identity %q was issued on another hub (%s), not %s", opts.Alias, e.Endpoint, endpoint)
		}
		s.entry = e
	} else if p.MemberDelegation() == "" {
		return nil, fmt.Errorf("hub profile %q has no member issuer delegation and no --as identity was given: no profile credential applies (this command never falls back to another credential)", p.Name)
	}
	return s, nil
}

func (s *Session) iamOpts() []transportauth.IAMIDTokenSourceOption {
	opts := []transportauth.IAMIDTokenSourceOption{transportauth.WithClock(s.deps.Now)}
	if s.deps.IAMCredentialsURL != "" {
		opts = append(opts, transportauth.WithIAMCredentialsURL(s.deps.IAMCredentialsURL))
	}
	return opts
}

// lazyAccess resolves the access token provider on first use, so a session
// that needs no Google token (an alias on a hub without IAP) never touches
// the metadata emulator.
type lazyAccess struct{ s *Session }

func (l lazyAccess) AccessToken() (string, error) {
	l.s.accessOnce.Do(func() {
		l.s.access, l.s.accessErr = l.s.deps.AccessTokens(l.s.deps.Getenv)
	})
	if l.s.accessErr != nil {
		return "", l.s.accessErr
	}
	return l.s.access.AccessToken()
}

// Profile returns the session's profile.
func (s *Session) Profile() *Profile { return s.opts.Profile }

// Endpoint returns the normalized target endpoint.
func (s *Session) Endpoint() string { return s.opts.Profile.Endpoint }

// Alias returns the alias the session acts as ("" for the issuer).
func (s *Session) Alias() string { return s.opts.Alias }

// Run returns the run label.
func (s *Session) Run() string { return s.opts.Run }

// BearerSource names where the session's bearer comes from.
func (s *Session) BearerSource() string {
	if s.opts.Alias != "" {
		return sourceAliasPrefix + s.opts.Alias
	}
	return SourceIssuer
}

// Client returns a hub client for the target that authenticates only with
// the profile credential. Environment transport auto-detection is
// suppressed; the transport token, when the profile has one, is added by
// the session's own strict transport.
func (s *Session) Client() (hubclient.Client, error) {
	return s.clientAs(s.opts.Alias != "")
}

// IssuerClient returns a hub client that acts as the profile's member
// issuer delegation, whatever the session's alias.
func (s *Session) IssuerClient() (hubclient.Client, error) {
	if s.opts.Profile.MemberDelegation() == "" {
		return nil, fmt.Errorf("hub profile %q has no member issuer delegation (add one with --issuer-delegation member=<id>)", s.opts.Profile.Name)
	}
	return s.clientAs(false)
}

func (s *Session) clientAs(alias bool) (hubclient.Client, error) {
	hc := &http.Client{Transport: s.hc.Transport, Timeout: 30 * time.Second}
	return hubclient.New(s.Endpoint(),
		hubclient.WithHTTPClient(hc),
		hubclient.WithTransportAuth(nil, transportauth.HeaderAuthorization),
		hubclient.WithAuthenticator(&bearerAuth{s: s, alias: alias}),
	)
}

// HTTPClient returns an HTTP client that sends the profile credential to
// the target hub, for callers that make raw requests.
func (s *Session) HTTPClient() *http.Client {
	return &http.Client{Transport: &authRoundTripper{s: s, alias: s.opts.Alias != "", base: s.hc.Transport}, Timeout: 30 * time.Second}
}

// isTarget reports whether u is on the target hub (same scheme, host and
// port). Profile credentials are attached only to such requests.
func (s *Session) isTarget(u *url.URL) bool {
	if u == nil {
		return false
	}
	return strings.EqualFold(u.Scheme, s.endpointURL.Scheme) && strings.EqualFold(u.Host, s.endpointURL.Host)
}

// IssuerAudience returns the target's issuer audience, read once from its
// public GET /api/v1/test-infra/status (sent with the transport token
// only).
func (s *Session) IssuerAudience(ctx context.Context) (string, error) {
	s.issuerMu.Lock()
	defer s.issuerMu.Unlock()
	if s.issuerAudience != "" {
		return s.issuerAudience, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Endpoint()+testInfraStatusPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the target's issuer audience: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the target's issuer audience: %s returned %d", testInfraStatusPath, resp.StatusCode)
	}
	var st struct {
		TestIdentities     bool   `json:"testIdentities"`
		TestIssuerAudience string `json:"testIssuerAudience"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil {
		return "", fmt.Errorf("reading the target's issuer audience: invalid %s response", testInfraStatusPath)
	}
	if st.TestIssuerAudience == "" {
		return "", errors.New("the target hub does not accept workload issuer delegation (its test-infra status has no issuer audience)")
	}
	// The target names the audience the runner mints an issuer token for:
	// accept only the issuer audience form, never the IAP audience, so a
	// target cannot obtain an issuer token for another purpose.
	if !strings.HasPrefix(st.TestIssuerAudience, IssuerAudiencePrefix) || len(st.TestIssuerAudience) == len(IssuerAudiencePrefix) || hasControlOrSpace(st.TestIssuerAudience) {
		return "", fmt.Errorf("the target hub's issuer audience is not of the form %s<hub id>; refusing to mint an issuer token for it", IssuerAudiencePrefix)
	}
	if st.TestIssuerAudience == s.opts.Profile.IAPAudience {
		return "", errors.New("the target hub's issuer audience equals the profile's IAP audience; refusing to mint an issuer token for it")
	}
	s.issuerAudience = st.TestIssuerAudience
	return s.issuerAudience, nil
}

func (s *Session) issuerSource(ctx context.Context) (*transportauth.IAMIDTokenSource, error) {
	aud, err := s.IssuerAudience(ctx)
	if err != nil {
		return nil, err
	}
	s.issuerMu.Lock()
	defer s.issuerMu.Unlock()
	if s.issuer == nil {
		src, err := transportauth.NewIAMIDTokenSource(s.opts.Profile.IssuerServiceAccount, aud, lazyAccess{s}, s.iamOpts()...)
		if err != nil {
			return nil, err
		}
		s.issuer = src
	}
	return s.issuer, nil
}

// bearer returns the Authorization token and extra headers for a request
// to the target, acting as the alias (alias true) or the issuer.
func (s *Session) bearer(ctx context.Context, alias bool) (string, map[string]string, error) {
	if alias {
		e, err := s.freshEntry(ctx)
		if err != nil {
			return "", nil, err
		}
		return e.Token, nil, nil
	}
	if s.opts.Profile.MemberDelegation() == "" {
		return "", nil, fmt.Errorf("hub profile %q has no member issuer delegation: no profile credential applies", s.opts.Profile.Name)
	}
	src, err := s.issuerSource(ctx)
	if err != nil {
		return "", nil, err
	}
	tok, err := src.Token()
	if err != nil {
		return "", nil, err
	}
	extra := map[string]string{HeaderIssuerDelegation: s.opts.Profile.MemberDelegation()}
	if s.opts.Run != "" {
		extra[HeaderRunLabel] = s.opts.Run
	}
	return tok, extra, nil
}

// freshEntry returns the cached identity, re-issuing a member-tier token
// through the issuer delegation when it is within ReissueMargin of its
// expiry. An expired identity, or an admin-tier identity whose token is
// expiring, is refused.
func (s *Session) freshEntry(ctx context.Context) (*CacheEntry, error) {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	e := s.entry
	if e == nil {
		return nil, errors.New("no cached identity")
	}
	now := s.deps.Now()
	if !now.Before(e.ExpiresAt) {
		return nil, fmt.Errorf("identity %q expired at %s; issue a new one", e.Alias, e.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if now.Before(e.TokenExpiresAt.Add(-ReissueMargin)) {
		return e, nil
	}
	if !e.IsMemberTier() {
		if now.Before(e.TokenExpiresAt) {
			return e, nil
		}
		return nil, fmt.Errorf("identity %q (role %s) has an expired token; admin-tier tokens are not re-issued, issue a new identity", e.Alias, e.Role)
	}
	// A new token cannot outlive the identity: once the token already
	// reaches the identity's expiry, keep it until it expires.
	if !e.TokenExpiresAt.Before(e.ExpiresAt) {
		if now.Before(e.TokenExpiresAt) {
			return e, nil
		}
		return nil, fmt.Errorf("identity %q expired; issue a new one", e.Alias)
	}
	ne, err := s.reissueLocked(ctx, e, 0)
	if err != nil {
		if now.Before(e.TokenExpiresAt) {
			return e, nil
		}
		return nil, fmt.Errorf("identity %q has an expired token and could not be re-issued: %w", e.Alias, err)
	}
	return ne, nil
}

// Reissue re-issues the token of the cached identity now, through the
// issuer delegation, and stores it in the cache. Admin-tier identities are
// refused. ttlSeconds 0 means the hub default.
func (s *Session) Reissue(ctx context.Context, ttlSeconds int64) (*CacheEntry, error) {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	e := s.entry
	if e == nil {
		return nil, errors.New("no cached identity: name one with --as <alias>")
	}
	if !e.IsMemberTier() {
		return nil, fmt.Errorf("identity %q (role %s): admin-tier tokens are not re-issued, issue a new identity", e.Alias, e.Role)
	}
	if !s.deps.Now().Before(e.ExpiresAt) {
		return nil, fmt.Errorf("identity %q expired at %s; issue a new one", e.Alias, e.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return s.reissueLocked(ctx, e, ttlSeconds)
}

// reissueLocked calls the token route as the issuer and replaces the cache
// entry. s.entryMu must be held.
func (s *Session) reissueLocked(ctx context.Context, e *CacheEntry, ttlSeconds int64) (*CacheEntry, error) {
	client, err := s.IssuerClient()
	if err != nil {
		return nil, err
	}
	var req *hubclient.TestIdentityTokenRequest
	if ttlSeconds > 0 {
		req = &hubclient.TestIdentityTokenRequest{TokenTTLSeconds: ttlSeconds}
	}
	resp, err := client.TestIdentities().Token(ctx, e.IdentityID, req)
	if err != nil {
		return nil, fmt.Errorf("re-issuing the token of identity %q: %w", e.Alias, err)
	}
	if resp.AccessToken == "" || resp.Identity.ID != e.IdentityID {
		return nil, fmt.Errorf("re-issuing the token of identity %q: unexpected response", e.Alias)
	}
	ne := *e
	ne.Token = resp.AccessToken
	ne.TokenExpiresAt = resp.TokenExpiresAt
	if !resp.Identity.ExpiresAt.IsZero() {
		ne.ExpiresAt = resp.Identity.ExpiresAt
	}
	if err := ReplaceEntry(s.opts.Home, &ne); err != nil {
		return nil, fmt.Errorf("storing the re-issued token of identity %q: %w", e.Alias, err)
	}
	s.entry = &ne
	return &ne, nil
}

// Entry returns the cached identity the session acts as (nil for the
// issuer). Its token must not be printed.
func (s *Session) Entry() *CacheEntry {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.entry
}

// bearerAuth is the apiclient.Authenticator of a profile client. It
// replaces every credential header with the profile's own, only on
// requests to the target hub, and fails the request when no profile
// credential is available.
type bearerAuth struct {
	s     *Session
	alias bool
}

func (a *bearerAuth) ApplyAuth(req *http.Request) error {
	for _, h := range credentialHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("Authorization")
	req.Header.Del(HeaderIssuerDelegation)
	req.Header.Del(HeaderRunLabel)
	if !a.s.isTarget(req.URL) {
		return nil
	}
	tok, extra, err := a.s.bearer(req.Context(), a.alias)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return nil
}

func (a *bearerAuth) Refresh() (bool, error) { return false, nil }

// authRoundTripper applies bearerAuth to raw requests.
type authRoundTripper struct {
	s     *Session
	alias bool
	base  http.RoundTripper
}

func (rt *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if err := (&bearerAuth{s: rt.s, alias: rt.alias}).ApplyAuth(clone); err != nil {
		return nil, err
	}
	return rt.base.RoundTrip(clone)
}

// profileTransport adds the transport token to requests for the target
// and fails them when it cannot be minted (no request leaves without it).
// Requests to other hosts never get the transport token.
type profileTransport struct {
	s    *Session
	base http.RoundTripper
}

func (t *profileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Del("Proxy-Authorization")
	clone.Header.Del("X-Serverless-Authorization")
	if t.s.transport != nil && t.s.isTarget(req.URL) {
		tok, err := t.s.transport.Token()
		if err != nil {
			return nil, fmt.Errorf("IAP transport token for %s: %w", t.s.opts.Profile.UATServiceAccount, err)
		}
		clone.Header.Set("Proxy-Authorization", "Bearer "+tok)
	}
	if !t.s.isTarget(req.URL) {
		// Never send a profile credential off the target hub.
		clone.Header.Del("Authorization")
		clone.Header.Del(HeaderIssuerDelegation)
		clone.Header.Del(HeaderRunLabel)
	}
	return t.base.RoundTrip(clone)
}

// wireRecorder records non-secret facts of the last request sent to the
// target, for hub status: a fingerprint of its Authorization token and the
// decoded claims of its transport token.
type wireRecorder struct {
	base http.RoundTripper
	mu   sync.Mutex
	last wireFacts
}

type wireFacts struct {
	seen            bool
	authorization   string // the token itself, kept in memory only for comparison
	otherCredential bool
	transport       string
}

func (w *wireRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	f := wireFacts{seen: true}
	f.authorization = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	f.transport = strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "Bearer ")
	for _, h := range credentialHeaders {
		if req.Header.Get(h) != "" {
			f.otherCredential = true
		}
	}
	w.mu.Lock()
	w.last = f
	w.mu.Unlock()
	return w.base.RoundTrip(req)
}

func (w *wireRecorder) take() wireFacts {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.last
	w.last = wireFacts{}
	return f
}

// HubTokenClaims are the non-secret claims of a hub user token.
type HubTokenClaims struct {
	UserID string
	Email  string
	Role   string
	Expiry time.Time
}

// DecodeHubTokenClaims decodes a hub user token's payload without
// verifying it. Errors never include the token.
func DecodeHubTokenClaims(token string) (HubTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return HubTokenClaims{}, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return HubTokenClaims{}, errors.New("token payload is not base64url")
	}
	var c struct {
		UID   string `json:"uid"`
		Email string `json:"email"`
		Role  string `json:"role"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return HubTokenClaims{}, errors.New("token payload is not JSON")
	}
	out := HubTokenClaims{UserID: c.UID, Email: c.Email, Role: c.Role}
	if c.Exp > 0 {
		out.Expiry = time.Unix(c.Exp, 0)
	}
	return out, nil
}

var _ apiclient.Authenticator = (*bearerAuth)(nil)
