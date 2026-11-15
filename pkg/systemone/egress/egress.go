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
//   - Bearer authentication (Authorization: Bearer <key>) is injected inside
//     RoundTrip onto a cloned request; the credential is never mutated onto the
//     caller's *http.Request, never logged, and never echoed in errors.
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

	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
)

// MaxResponseBytes is the hard cap on provider HTTP response bodies (1 MiB).
// Any response whose Content-Length or streamed body exceeds MaxResponseBytes
// fails with ErrResponseTooLarge, which callers classify as malformed_response.
const MaxResponseBytes int64 = 1 << 20

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

	// ErrAuth indicates the bearer credential could not be resolved or was
	// empty or malformed before sending the request.
	ErrAuth = errors.New("systemone egress: bearer credential unavailable or invalid")
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

// IsAuthError reports whether err was caused by a missing, failing, or invalid
// bearer credential before sending the request.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrAuth)
}

// BearerSource supplies the raw bearer API key (without the "Bearer " prefix)
// at request time. The returned value is never logged or echoed in errors.
type BearerSource func(ctx context.Context) (string, error)

// StaticBearer returns a BearerSource that yields key on every request.
func StaticBearer(key string) BearerSource {
	return func(context.Context) (string, error) {
		return key, nil
	}
}

// Option configures test seams on NewClient. Production callers pass no
// options.
type Option func(*clientConfig)

type clientConfig struct {
	resolver     ssrfclient.Resolver
	dialer       ssrfclient.Dialer
	tlsConfig    *tls.Config
	loopbackSeam *loopbackTestSeam
	configErr    error
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
			c.configErr = fmt.Errorf("%w: invalid test server URL", ErrInvalidEndpoint)
			return
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			c.configErr = fmt.Errorf("%w: unsupported test server scheme", ErrInvalidEndpoint)
			return
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || u.Port() == "" {
			c.configErr = fmt.Errorf("%w: test server must bind a literal loopback IP and port", ErrInvalidEndpoint)
			return
		}
		var tr http.RoundTripper = http.DefaultTransport
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
// Transport. Production callers invoke NewClient(bearer) with no options.
func NewClient(bearer BearerSource, opts ...Option) *http.Client {
	cfg := &clientConfig{}
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
		RedirectError:        ErrRedirectBlocked,
	})

	rt := &egressRoundTripper{
		bearer:        bearer,
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

	token, err := rt.resolveToken(req.Context())
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}

	outReq := req.Clone(req.Context())
	outReq.Header.Set("Authorization", "Bearer "+token)

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
		// Never propagate raw error text from a credential callback in case it
		// echoes secret material.
		return "", ErrAuth
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
