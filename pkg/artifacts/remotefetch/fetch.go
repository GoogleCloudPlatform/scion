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

// Package remotefetch fetches remote images for the artifact service at
// publish time. Every request it makes is constrained so that a URL taken
// from a published document cannot reach anything but a public HTTPS image
// server:
//
//   - https only, on port 443, with no userinfo, checked on the first request
//     and on every redirect hop (at most MaxRedirects);
//   - the host name is resolved once per hop and the fetch fails if ANY
//     resolved address is denied (loopback, private, link-local including
//     the cloud metadata address, multicast, unspecified and other special
//     ranges, in IPv4, IPv6 and IPv4-mapped forms);
//   - the connection goes to the first vetted address only (no other
//     address is tried and the name is not resolved again), and TLS is
//     verified against the hop's own host name;
//   - no proxy, cookies, credentials or authorization headers;
//   - connect, per-fetch and caller-supplied (total) deadlines;
//   - the body is capped while streaming and must sniff as PNG, JPEG, GIF or
//     WebP; SVG and everything else are refused. The returned content type
//     is the sniffed one, never the server's.
//
// Errors carry a Reason for server-side logs. Callers must not show the
// reason to the publisher or to readers: a refused address and a missing
// image must look the same from outside.
package remotefetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Defaults for a zero Config field.
const (
	DefaultMaxBytes       int64 = 5 << 20
	DefaultTimeout              = 10 * time.Second
	DefaultConnectTimeout       = 5 * time.Second
	DefaultMaxRedirects         = 3
)

// Reason classifies why a fetch failed. It is for server-side logs only.
type Reason string

// Failure reasons.
const (
	ReasonBadURL        Reason = "bad_url"
	ReasonScheme        Reason = "scheme_not_https"
	ReasonUserinfo      Reason = "userinfo"
	ReasonPort          Reason = "port_not_443"
	ReasonHost          Reason = "bad_host"
	ReasonResolve       Reason = "resolve_failed"
	ReasonDeniedAddress Reason = "denied_address"
	ReasonConnect       Reason = "connect_failed"
	ReasonTimeout       Reason = "timeout"
	ReasonRedirects     Reason = "too_many_redirects"
	ReasonStatus        Reason = "bad_status"
	ReasonTooLarge      Reason = "too_large"
	ReasonType          Reason = "type_not_allowed"
)

// Error is a failed fetch.
type Error struct {
	Reason Reason
	// Detail is free text for logs; it may name hosts and addresses.
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "remote fetch failed: " + string(e.Reason)
	}
	return "remote fetch failed: " + string(e.Reason) + ": " + e.Detail
}

func fail(r Reason, format string, args ...any) *Error {
	return &Error{Reason: r, Detail: fmt.Sprintf(format, args...)}
}

// Result is a fetched image.
type Result struct {
	Body []byte
	// ContentType is the sniffed media type: image/png, image/jpeg,
	// image/gif or image/webp.
	ContentType string
	// SHA256 is the lowercase hex digest of Body.
	SHA256 string
}

// Resolver resolves host names. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Config tunes a Fetcher. Zero fields take the defaults above.
type Config struct {
	MaxBytes       int64
	Timeout        time.Duration
	ConnectTimeout time.Duration
	MaxRedirects   int
	Logger         *slog.Logger
}

// Fetcher fetches remote images. It is safe for concurrent use.
type Fetcher struct {
	cfg      Config
	resolver Resolver

	// Test seams, unexported so production code cannot relax them. In
	// production denied is isDenied, port is "443" and rootCAs is nil (the
	// system roots).
	denied  func(netip.Addr) bool
	port    string
	rootCAs *x509.CertPool
}

// New returns a Fetcher with the system resolver.
func New(cfg Config) *Fetcher {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = DefaultMaxRedirects
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Fetcher{
		cfg:      cfg,
		resolver: &net.Resolver{PreferGo: true},
		denied:   isDenied,
		port:     "443",
	}
}

// Fetch downloads one image. The per-fetch timeout applies on top of any
// deadline ctx already carries (the caller's total budget).
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, f.cfg.Timeout)
	defer cancel()
	res, err := f.fetch(ctx, rawURL)
	if err != nil {
		var fe *Error
		if !errors.As(err, &fe) {
			fe = fail(ReasonConnect, "%v", err)
		}
		if ctx.Err() != nil && fe.Reason != ReasonDeniedAddress {
			fe = fail(ReasonTimeout, "%v", err)
		}
		f.cfg.Logger.WarnContext(ctx, "artifacts: remote image not fetched",
			"reason", string(fe.Reason), "detail", fe.Detail, "url", redactURL(rawURL))
		return nil, fe
	}
	return res, nil
}

func (f *Fetcher) fetch(ctx context.Context, rawURL string) (*Result, error) {
	current := rawURL
	for hop := 0; ; hop++ {
		u, err := f.checkURL(current)
		if err != nil {
			return nil, err
		}
		addr, err := f.vet(ctx, u.Hostname())
		if err != nil {
			return nil, err
		}
		resp, err := f.get(ctx, u, addr)
		if err != nil {
			return nil, err
		}
		if isRedirect(resp.StatusCode) {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hop >= f.cfg.MaxRedirects {
				return nil, fail(ReasonRedirects, "more than %d redirects", f.cfg.MaxRedirects)
			}
			next, err := u.Parse(loc)
			if err != nil || loc == "" {
				return nil, fail(ReasonBadURL, "unusable redirect location")
			}
			current = next.String()
			continue
		}
		defer func() { _ = resp.Body.Close() }()
		return f.readImage(resp)
	}
}

// checkURL enforces the per-hop URL rules: https, port 443, no userinfo, a
// host that is either a canonical IP literal or a DNS name.
func (f *Fetcher) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return nil, fail(ReasonBadURL, "not an absolute URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fail(ReasonScheme, "scheme %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fail(ReasonUserinfo, "userinfo in URL")
	}
	if p := u.Port(); p != "" && p != f.port {
		return nil, fail(ReasonPort, "port %q", p)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fail(ReasonHost, "empty host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return nil, fail(ReasonHost, "zone in IP literal")
		}
		return u, nil
	}
	if !plausibleDNSName(host) {
		return nil, fail(ReasonHost, "host %q is neither an IP literal nor a DNS name", host)
	}
	return u, nil
}

// plausibleDNSName rejects host strings that some resolvers would read as a
// numeric address (decimal, octal or hex forms such as 2130706433, 0x7f.1
// or 127.1) and other non-hostnames. A real top-level label always has a
// letter and is never hex-prefixed.
func plausibleDNSName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "%[]") {
		return false
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return false
		}
		for _, r := range l {
			if !isHostnameRune(r) {
				return false
			}
		}
	}
	tld := strings.ToLower(labels[len(labels)-1])
	if strings.HasPrefix(tld, "0x") {
		return false
	}
	return strings.IndexFunc(tld, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
}

func isHostnameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

// vet resolves host once and returns the address to connect to. It fails if
// any resolved address is denied; it never picks an allowed address out of
// a mixed answer. Denials are logged here, before any connection.
func (f *Fetcher) vet(ctx context.Context, host string) (netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = f.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return netip.Addr{}, fail(ReasonResolve, "resolve %s: %v", host, err)
		}
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fail(ReasonResolve, "no addresses for %s", host)
	}
	for _, a := range addrs {
		if f.denied(a) {
			f.cfg.Logger.WarnContext(ctx, "artifacts: remote image address denied before connecting",
				"host", host, "address", a.String())
			return netip.Addr{}, fail(ReasonDeniedAddress, "%s resolves to denied address %s", host, a)
		}
	}
	return addrs[0].Unmap(), nil
}

// get sends one request to addr, the vetted address of u's host. The
// transport dials that address only and verifies TLS against u's host.
func (f *Fetcher) get(ctx context.Context, u *url.URL, addr netip.Addr) (*http.Response, error) {
	target := net.JoinHostPort(addr.String(), f.port)
	dialer := &net.Dialer{Timeout: f.cfg.ConnectTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// The requested address is ignored: the connection always
			// goes to the address vetted for this hop.
			return dialer.DialContext(ctx, "tcp", target)
		},
		TLSClientConfig: &tls.Config{
			ServerName: u.Hostname(),
			RootCAs:    f.rootCAs,
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:    f.cfg.ConnectTimeout,
		ResponseHeaderTimeout:  f.cfg.Timeout,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      false,
	}
	client := &http.Client{
		Transport: transport,
		Jar:       nil,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fail(ReasonBadURL, "build request: %v", err)
	}
	req.Header.Set("User-Agent", "scion-artifacts/1")
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif, image/webp")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fail(ReasonConnect, "%s via %s: %v", u.Hostname(), target, err)
	}
	return resp, nil
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// readImage reads a 200 response under the size cap and sniffs its type.
func (f *Fetcher) readImage(resp *http.Response) (*Result, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, fail(ReasonStatus, "status %d", resp.StatusCode)
	}
	if resp.ContentLength > f.cfg.MaxBytes {
		return nil, fail(ReasonTooLarge, "content-length %d over %d", resp.ContentLength, f.cfg.MaxBytes)
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(resp.Body, f.cfg.MaxBytes+1))
	if err != nil {
		return nil, fail(ReasonConnect, "read body: %v", err)
	}
	if n > f.cfg.MaxBytes {
		return nil, fail(ReasonTooLarge, "body over %d bytes", f.cfg.MaxBytes)
	}
	ctype := SniffImage(buf.Bytes())
	if ctype == "" {
		return nil, fail(ReasonType, "body is not an allowed image type")
	}
	sum := sha256.Sum256(buf.Bytes())
	return &Result{Body: buf.Bytes(), ContentType: ctype, SHA256: hex.EncodeToString(sum[:])}, nil
}

// SniffImage returns the media type of an allowed raster image from its
// leading bytes, or "" for anything else (SVG included).
func SniffImage(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// redactURL drops the query and fragment, which may carry tokens, from a URL
// before it is logged.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}
