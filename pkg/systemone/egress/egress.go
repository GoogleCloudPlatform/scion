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

// Package egress constructs the authenticated, SSRF-safe outbound *http.Client
// (provider Transport) used for System One model provider invocations.
//
// Security invariants:
//   - HTTPS only; any non-https scheme or embedded URL userinfo is refused
//     before DNS resolution or dialing.
//   - Private (RFC 1918, IPv6 ULA, CGNAT), loopback, link-local (including
//     169.254.169.254), multicast, unspecified, and translation-prefix IPs are
//     refused via pkg/util/ssrfclient, dialing the validated IP directly.
//   - All HTTP redirects (3xx) are refused without following or echoing the
//     Location header.
//   - Authentication (default Authorization: Bearer <key>, or adapter-declared
//     header placement via WithCredentialPlacement) is injected inside RoundTrip
//     onto a cloned request; the credential is never mutated onto the caller's
//     *http.Request, never logged, and never echoed in errors. With header
//     placement, Authorization is never set.
//   - Response bodies are capped at MaxResponseBytes (1 MiB); exceeding the cap
//     returns ErrResponseTooLarge so the runtime/adapter maps it to
//     malformed_response.
package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
)

// MaxResponseBytes is the hard cap on provider HTTP response bodies (1 MiB).
// Any response whose Content-Length or streamed body exceeds MaxResponseBytes
// fails with ErrResponseTooLarge, which callers classify as malformed_response.
const MaxResponseBytes int64 = 1 << 20

// DefaultIdleConnTimeout bounds how long idle HTTP keep-alive sockets remain
// open in an egress transport pool (egress-side only; ssrfclient defaults
// remain unchanged).
const DefaultIdleConnTimeout = 90 * time.Second

var (
	// ErrResponseTooLarge indicates the provider response body exceeded
	// MaxResponseBytes (1 MiB). Runtime and adapters map this error to
	// FailReason malformed_response.
	ErrResponseTooLarge = errors.New("systemone egress: response body exceeds 1 MiB limit")

	// ErrRedirectBlocked indicates the provider returned a 3xx redirect, which
	// is refused.
	ErrRedirectBlocked = errors.New("systemone egress: redirects are blocked")

	// ErrSchemeNotAllowed indicates a request URL did not use the https scheme.
	ErrSchemeNotAllowed = errors.New("systemone egress: only https endpoints are allowed")

	// ErrInvalidEndpoint indicates a malformed request URL (missing host,
	// opaque URL, embedded userinfo, or invalid test loopback configuration).
	ErrInvalidEndpoint = errors.New("systemone egress: invalid endpoint URL")

	// ErrBlockedDestination indicates DNS resolved solely to blocked private,
	// loopback, link-local, or reserved IP addresses.
	ErrBlockedDestination = errors.New("systemone egress: destination address is blocked")

	// ErrAuth indicates the bearer credential was missing, empty, or malformed
	// before sending the request.
	ErrAuth = errors.New("systemone egress: bearer credential unavailable or invalid")

	// ErrCredentialUnavailable indicates the BearerSource callback failed to
	// resolve a credential (for example, due to a transient secret-store
	// outage) before sending the request. Callers map this to FailUnavailable.
	ErrCredentialUnavailable = errors.New("systemone egress: bearer credential source unavailable")

	// ErrInvalidPlacement indicates WithCredentialPlacement was called with an
	// unsupported auth kind or an invalid/reserved HTTP header name. Every
	// request fails closed without dialing or falling back to Bearer.
	ErrInvalidPlacement = errors.New("systemone egress: invalid credential placement")
)

// IsResponseTooLarge reports whether err was caused by a response body
// exceeding MaxResponseBytes (1 MiB).
func IsResponseTooLarge(err error) bool {
	return errors.Is(err, ErrResponseTooLarge)
}

// IsRedirectBlocked reports whether err was caused by a refused HTTP redirect.
func IsRedirectBlocked(err error) bool {
	return errors.Is(err, ErrRedirectBlocked)
}

// IsSchemeNotAllowed reports whether err was caused by a non-https request URL.
func IsSchemeNotAllowed(err error) bool {
	return errors.Is(err, ErrSchemeNotAllowed)
}

// IsInvalidEndpoint reports whether err was caused by an invalid endpoint URL.
func IsInvalidEndpoint(err error) bool {
	return errors.Is(err, ErrInvalidEndpoint)
}

// IsBlockedDestination reports whether err was caused by SSRF destination
// address blocking.
func IsBlockedDestination(err error) bool {
	return errors.Is(err, ErrBlockedDestination) || errors.Is(err, ssrfclient.ErrBlockedAddress)
}

// IsAuthError reports whether err was caused by a missing, empty, or invalid
// bearer credential before sending the request.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrAuth)
}

// IsCredentialUnavailable reports whether err was caused by a BearerSource
// callback failure before sending the request.
func IsCredentialUnavailable(err error) bool {
	return errors.Is(err, ErrCredentialUnavailable)
}

// IsInvalidPlacement reports whether err was caused by an invalid credential
// placement configuration (invalid/reserved header name or unsupported auth kind).
func IsInvalidPlacement(err error) bool {
	return errors.Is(err, ErrInvalidPlacement)
}

// maxPlacementHeaderNameBytes bounds custom API-key header names declared by adapters.
const maxPlacementHeaderNameBytes = 128

// HeaderQuotaProject is the canonical HTTP header name carrying the GCP billing
// / quota project ID on keyless OAuth/ADC bearer requests (PV3 brief Addendum B
// §§B3, B5). It is reserved so adapters cannot set or override it independently.
const HeaderQuotaProject = "X-Goog-User-Project"

const (
	minGCPProjectIDLen = 6
	maxGCPProjectIDLen = 30
)

// reservedPlacementHeaders lists lowercased hop-by-hop, framing, routing,
// cookie, and bearer/proxy-auth HTTP header names that WithCredentialPlacement
// must never overwrite or use as an API-key header.
var reservedPlacementHeaders = map[string]struct{}{
	"accept-encoding":     {},
	"authorization":       {},
	"connection":          {},
	"content-encoding":    {},
	"content-length":      {},
	"content-type":        {},
	"cookie":              {},
	"expect":              {},
	"forwarded":           {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"set-cookie":          {},
	"te":                  {},
	"trailer":             {},
	"trailers":            {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"user-agent":          {},
	"via":                 {},
	"www-authenticate":    {},
	"x-forwarded-for":     {},
	"x-forwarded-host":    {},
	"x-forwarded-proto":   {},
	"x-goog-user-project": {},
}

// Placement specifies where egress attaches the per-request credential inside
// RoundTrip. Zero value defaults to Bearer (Authorization: Bearer <token>).
// HeaderName comes from an adapter's declared Capabilities.AuthScheme, never
// from admin or untrusted input, and is validated at construction. QuotaProject
// is attached as X-Goog-User-Project solely on ADC Bearer placement and is
// validated against the GCP project-ID grammar.
type Placement struct {
	Kind         provider.AuthKind
	HeaderName   string
	QuotaProject string
}

// BearerPlacement attaches the credential as "Authorization: Bearer <token>".
var BearerPlacement = Placement{Kind: provider.AuthKindBearer}

// ADCBearerPlacement constructs a Bearer Placement for Google OAuth/ADC access
// tokens, optionally attaching "X-Goog-User-Project: <quotaProject>" when
// quotaProject is non-empty.
func ADCBearerPlacement(quotaProject string) Placement {
	return Placement{Kind: provider.AuthKindBearer, QuotaProject: quotaProject}
}

// HeaderPlacement constructs a Placement that attaches the credential as
// "<headerName>: <token>" instead of Authorization.
func HeaderPlacement(headerName string) Placement {
	return Placement{Kind: provider.AuthKindHeader, HeaderName: headerName}
}

// ValidateQuotaProject checks that projectID conforms to the GCP project-ID
// grammar (6..30 characters: lowercase ASCII letter, followed by lowercase
// ASCII letters/digits/hyphens, ending with a lowercase ASCII letter or digit).
// Returned errors are content-free and never echo projectID.
func ValidateQuotaProject(projectID string) error {
	if !isGCPProjectID(projectID) {
		return fmt.Errorf("%w: quota project must be a valid GCP project ID", ErrInvalidPlacement)
	}
	return nil
}

func isLowerAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func isGCPProjectID(s string) bool {
	if len(s) < minGCPProjectIDLen || len(s) > maxGCPProjectIDLen {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	if !isLowerAlnum(s[len(s)-1]) {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		b := s[i]
		if !isLowerAlnum(b) && b != '-' {
			return false
		}
	}
	return true
}

// ValidateHeaderName verifies that name is a valid, non-reserved RFC 7230 token
// suitable for header-based API-key placement. Returned errors are content-free
// and never echo name.
func ValidateHeaderName(name string) error {
	if len(name) == 0 || len(name) > maxPlacementHeaderNameBytes || !isRFC7230Token(name) {
		return fmt.Errorf("%w: header name must be a non-empty RFC 7230 token", ErrInvalidPlacement)
	}
	if _, reserved := reservedPlacementHeaders[strings.ToLower(name)]; reserved {
		return fmt.Errorf("%w: header name is reserved or hop-by-hop", ErrInvalidPlacement)
	}
	return nil
}

// ValidatePlacement checks that p uses a supported AuthKind and valid header
// metadata. Zero Placement{} (empty Kind and empty HeaderName) is valid and
// normalizes to BearerPlacement. Returned errors are content-free.
func ValidatePlacement(p Placement) error {
	_, err := normalizePlacement(p)
	return err
}

func normalizePlacement(p Placement) (Placement, error) {
	switch p.Kind {
	case "", provider.AuthKindBearer:
		if p.HeaderName != "" {
			return Placement{}, fmt.Errorf("%w: bearer placement must not specify a header name", ErrInvalidPlacement)
		}
		if p.QuotaProject != "" {
			if err := ValidateQuotaProject(p.QuotaProject); err != nil {
				return Placement{}, err
			}
		}
		return Placement{
			Kind:         provider.AuthKindBearer,
			QuotaProject: p.QuotaProject,
		}, nil
	case provider.AuthKindHeader:
		if p.QuotaProject != "" {
			return Placement{}, fmt.Errorf("%w: header placement must not specify a quota project", ErrInvalidPlacement)
		}
		if err := ValidateHeaderName(p.HeaderName); err != nil {
			return Placement{}, err
		}
		return Placement{
			Kind:       provider.AuthKindHeader,
			HeaderName: http.CanonicalHeaderKey(p.HeaderName),
		}, nil
	case provider.AuthKindNone:
		if p.HeaderName != "" {
			return Placement{}, fmt.Errorf("%w: none placement must not specify a header name", ErrInvalidPlacement)
		}
		if p.QuotaProject != "" {
			return Placement{}, fmt.Errorf("%w: none placement must not specify a quota project", ErrInvalidPlacement)
		}
		return Placement{Kind: provider.AuthKindNone}, nil
	default:
		return Placement{}, fmt.Errorf("%w: unsupported credential placement kind", ErrInvalidPlacement)
	}
}

func isRFC7230Token(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isRFC7230TChar(s[i]) {
			return false
		}
	}
	return true
}

func isRFC7230TChar(b byte) bool {
	if (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

// BearerSource supplies the raw API key (without any "Bearer " prefix) at
// request time. The returned value is never logged or echoed in errors.
type BearerSource func(ctx context.Context) (string, error)

// StaticBearer returns a BearerSource that yields key on every request.
func StaticBearer(key string) BearerSource {
	return func(context.Context) (string, error) {
		return key, nil
	}
}

// Option configures NewClient (such as WithCredentialPlacement in production
// and *ForTesting seams in offline tests).
type Option func(*clientConfig)

type clientConfig struct {
	placement    Placement
	resolver     ssrfclient.Resolver
	dialer       ssrfclient.Dialer
	tlsConfig    *tls.Config
	loopbackSeam *loopbackTestSeam
	configErr    error
}

func (c *clientConfig) setConfigErr(err error) {
	if err != nil && c.configErr == nil {
		c.configErr = err
	}
}

// WithCredentialPlacement configures where RoundTrip places the resolved API
// key on the cloned outbound request. When omitted, NewClient defaults to
// BearerPlacement ("Authorization: Bearer <token>"). With HeaderPlacement,
// RoundTrip sets "<HeaderName>: <token>" and never sets Authorization. An
// invalid placement fails closed on every request with ErrInvalidPlacement.
func WithCredentialPlacement(p Placement) Option {
	return func(c *clientConfig) {
		norm, err := normalizePlacement(p)
		if err != nil {
			c.setConfigErr(err)
			return
		}
		c.placement = norm
	}
}

type loopbackTestSeam struct {
	scheme    string
	hostPort  string
	transport http.RoundTripper
}

// WithDialerForTesting overrides the DNS resolver and raw TCP dialer used by
// the underlying SSRF-safe transport while keeping all production IP-blocking,
// https-only, redirect-blocking, auth, and body-cap rules active.
func WithDialerForTesting(resolver ssrfclient.Resolver, dialer ssrfclient.Dialer) Option {
	return func(c *clientConfig) {
		c.resolver = resolver
		c.dialer = dialer
	}
}

// WithTLSConfigForTesting sets the TLSClientConfig on the underlying SSRF
// transport (for example, to trust an httptest.NewTLSServer certificate pool).
func WithTLSConfigForTesting(tlsCfg *tls.Config) Option {
	return func(c *clientConfig) {
		if tlsCfg != nil {
			c.tlsConfig = tlsCfg.Clone()
		}
	}
}

// WithLoopbackServerForTesting permits offline unit tests to route requests
// targeting a specific in-process httptest server (http://127.0.0.1:<port> or
// https://127.0.0.1:<port>) through serverClient's transport while keeping
// Bearer auth injection, redirect refusal, the 1 MiB response body cap, and
// refusal of all other destinations intact.
//
// If serverURL is not a literal loopback IP with an explicit port, the client
// fails closed on every request with ErrInvalidEndpoint.
func WithLoopbackServerForTesting(serverURL string, serverClient *http.Client) Option {
	return func(c *clientConfig) {
		u, err := url.Parse(serverURL)
		if err != nil || u.Opaque != "" || u.User != nil {
			c.setConfigErr(fmt.Errorf("%w: invalid test server URL", ErrInvalidEndpoint))
			return
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			c.setConfigErr(fmt.Errorf("%w: unsupported test server scheme", ErrInvalidEndpoint))
			return
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || u.Port() == "" {
			c.setConfigErr(fmt.Errorf("%w: test server must bind a literal loopback IP and port", ErrInvalidEndpoint))
			return
		}
		tr := http.DefaultTransport
		if serverClient != nil && serverClient.Transport != nil {
			tr = serverClient.Transport
		}
		c.loopbackSeam = &loopbackTestSeam{
			scheme:    scheme,
			hostPort:  net.JoinHostPort(ip.String(), u.Port()),
			transport: tr,
		}
	}
}

// NewClient constructs an *http.Client configured as the System One provider
// Transport. Without WithCredentialPlacement, NewClient defaults to
// Authorization: Bearer authentication.
func NewClient(bearer BearerSource, opts ...Option) *http.Client {
	cfg := &clientConfig{
		placement: BearerPlacement,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.tlsConfig != nil {
		tlsCfg = cfg.tlsConfig.Clone()
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS12
		}
	}

	prodTransport := ssrfclient.NewTransport(ssrfclient.Config{
		AllowPrivateNetworks: false,
		Resolver:             cfg.resolver,
		Dialer:               cfg.dialer,
		TLSClientConfig:      tlsCfg,
	})
	prodTransport.IdleConnTimeout = DefaultIdleConnTimeout

	rt := &egressRoundTripper{
		bearer:        bearer,
		placement:     cfg.placement,
		prodTransport: prodTransport,
		loopbackSeam:  cfg.loopbackSeam,
		configErr:     cfg.configErr,
	}

	return &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectBlocked
		},
	}
}

type egressRoundTripper struct {
	bearer        BearerSource
	placement     Placement
	prodTransport http.RoundTripper
	loopbackSeam  *loopbackTestSeam
	configErr     error
}

func (rt *egressRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.configErr != nil {
		closeRequestBody(req)
		return nil, rt.configErr
	}
	if req == nil || req.URL == nil {
		closeRequestBody(req)
		return nil, ErrInvalidEndpoint
	}
	if req.URL.Opaque != "" || req.URL.User != nil || req.URL.Host == "" || req.URL.Hostname() == "" {
		closeRequestBody(req)
		return nil, ErrInvalidEndpoint
	}

	transport, err := rt.selectTransport(req.URL)
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}

	var token string
	if rt.placement.Kind != provider.AuthKindNone {
		token, err = rt.resolveToken(req.Context())
		if err != nil {
			closeRequestBody(req)
			return nil, err
		}
	}

	outReq := req.Clone(req.Context())
	outReq.Header.Del(HeaderQuotaProject)
	switch rt.placement.Kind {
	case provider.AuthKindNone:
		outReq.Header.Del("Authorization")
	case provider.AuthKindHeader:
		outReq.Header.Del("Authorization")
		outReq.Header.Set(rt.placement.HeaderName, token)
	default:
		outReq.Header.Set("Authorization", "Bearer "+token)
		if rt.placement.QuotaProject != "" {
			outReq.Header.Set(HeaderQuotaProject, rt.placement.QuotaProject)
		}
	}

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		safeErr := scrubSecret(err, token)
		if errors.Is(err, ssrfclient.ErrBlockedAddress) {
			return nil, fmt.Errorf("%w: %w", ErrBlockedDestination, safeErr)
		}
		return nil, safeErr
	}

	// Refuse any 3xx redirect before net/http inspects or echoes Location.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrRedirectBlocked, resp.StatusCode)
	}

	if req.Method != http.MethodHead {
		if resp.ContentLength > MaxResponseBytes {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return nil, fmt.Errorf("%w: content-length %d exceeds %d bytes", ErrResponseTooLarge, resp.ContentLength, MaxResponseBytes)
		}
		if resp.Body != nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, scrubSecret(readErr, token)
			}
			if int64(len(body)) > MaxResponseBytes {
				return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrResponseTooLarge, MaxResponseBytes)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
		}
	}

	return resp, nil
}

func (rt *egressRoundTripper) selectTransport(u *url.URL) (http.RoundTripper, error) {
	if rt.loopbackSeam != nil &&
		strings.EqualFold(u.Scheme, rt.loopbackSeam.scheme) &&
		canonicalLoopbackHostPort(u) == rt.loopbackSeam.hostPort {
		return rt.loopbackSeam.transport, nil
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("%w: scheme %q", ErrSchemeNotAllowed, u.Scheme)
	}
	return rt.prodTransport, nil
}

func canonicalLoopbackHostPort(u *url.URL) string {
	ip := net.ParseIP(u.Hostname())
	if ip == nil || u.Port() == "" {
		return ""
	}
	return net.JoinHostPort(ip.String(), u.Port())
}

func (rt *egressRoundTripper) resolveToken(ctx context.Context) (string, error) {
	if rt.bearer == nil {
		return "", ErrAuth
	}
	token, err := rt.bearer(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		if errors.Is(err, ErrAuth) {
			return "", ErrAuth
		}
		// Never propagate raw error text from a credential callback in case it
		// echoes secret material; surface ErrCredentialUnavailable so callers
		// distinguish transient store outages from missing/malformed tokens.
		return "", ErrCredentialUnavailable
	}
	if strings.TrimSpace(token) == "" || hasInvalidHeaderByte(token) {
		return "", ErrAuth
	}
	return token, nil
}

func hasInvalidHeaderByte(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

func closeRequestBody(req *http.Request) {
	if req != nil && req.Body != nil {
		_ = req.Body.Close()
	}
}

// scrubSecret ensures token never appears in any error returned by RoundTrip,
// even if an injected dialer or transport echoed it.
func scrubSecret(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "[REDACTED]")
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", msg, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
	case errors.Is(err, ssrfclient.ErrBlockedAddress):
		return fmt.Errorf("%s: %w", msg, ssrfclient.ErrBlockedAddress)
	default:
		return errors.New(msg)
	}
}
