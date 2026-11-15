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

// Package ssrfclient provides an HTTP transport and client whose dialer
// resolves hostnames once, blocks disallowed IP ranges (SSRF protection),
// dials the validated IP directly to close DNS-rebinding TOCTOU windows, and
// refuses HTTP redirects.
package ssrfclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

var (
	// ErrBlockedAddress is returned when every resolved IP address for a host
	// falls within a blocked SSRF address range.
	ErrBlockedAddress = errors.New("SSRF protection: blocked address")

	// ErrRedirectBlocked is the default error returned by CheckRedirect when an
	// outbound HTTP response attempts a redirect.
	ErrRedirectBlocked = errors.New("SSRF protection: redirects are blocked")
)

// Resolver abstracts DNS resolution for the SSRF-safe dialer.
// Production uses net.DefaultResolver; tests can inject a fake to control
// which IPs a hostname resolves to without real DNS.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Dialer abstracts the raw TCP dial for the SSRF-safe dialer.
// Production uses a net.Dialer; tests can inject a fake to verify which
// IP:port pairs are actually dialed.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// defaultResolver wraps net.DefaultResolver to satisfy Resolver.
type defaultResolver struct{}

func (defaultResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// Config configures an SSRF-safe HTTP transport or client.
// The zero value enforces strict defaults: private, loopback, link-local,
// multicast, unspecified and special-purpose ranges are blocked, environment
// proxies are ignored, and all redirects are refused.
type Config struct {
	// AllowPrivateNetworks, when true, permits RFC 1918 private addresses
	// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and IPv6 ULA while still
	// blocking loopback, link-local (unicast + multicast), and unspecified
	// addresses. Defaults to false (private networks blocked).
	AllowPrivateNetworks bool

	// Resolver overrides DNS resolution (defaults to net.DefaultResolver).
	Resolver Resolver

	// Dialer overrides the underlying TCP dialer (defaults to a net.Dialer
	// with 30s Timeout and 30s KeepAlive).
	Dialer Dialer

	// TLSClientConfig sets TLS configuration on the underlying http.Transport.
	TLSClientConfig *tls.Config

	// RedirectError overrides the error returned by CheckRedirect when non-nil.
	// Defaults to ErrRedirectBlocked.
	RedirectError error
}

// NewTransport creates an SSRF-safe *http.Transport from cfg.
//
// The transport resolves the target hostname, selects the first non-blocked IP,
// and dials THAT SPECIFIC IP — never the original hostname — closing the
// DNS-rebinding TOCTOU window. TLS SNI and the HTTP Host header remain derived
// from req.URL.Host. Environment proxies are disabled (Proxy is nil).
func NewTransport(cfg Config) *http.Transport {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = defaultResolver{}
	}
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
	}
	allowPrivate := cfg.AllowPrivateNetworks

	var tlsCfg *tls.Config
	if cfg.TLSClientConfig != nil {
		tlsCfg = cfg.TLSClientConfig.Clone()
	}

	return &http.Transport{
		Proxy:           nil,
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("SSRF protection: invalid address %q: %w", addr, err)
			}
			ips, err := resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("SSRF protection: DNS lookup failed for %q: %w", host, err)
			}

			for _, ipAddr := range ips {
				if len(ipAddr.IP) == 0 {
					continue
				}
				if allowPrivate {
					if !IsBlockedLoopbackOrLinkLocal(ipAddr.IP) {
						return dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
					}
					continue
				}
				if ipAddr.Zone == "" && !IsBlockedPrivateOrLocal(ipAddr.IP) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
				}
			}

			if allowPrivate {
				return nil, fmt.Errorf("%w: all resolved IPs for %q are blocked (loopback/link-local)", ErrBlockedAddress, host)
			}
			return nil, fmt.Errorf("%w: all resolved IPs for %q are blocked (private/loopback/link-local)", ErrBlockedAddress, host)
		},
	}
}

// NewClient creates an *http.Client with NewTransport(cfg) and redirect
// blocking. No http.Client.Timeout is set so caller context deadlines act as
// the single timeout mechanism.
func NewClient(cfg Config) *http.Client {
	redirectErr := cfg.RedirectError
	if redirectErr == nil {
		redirectErr = ErrRedirectBlocked
	}
	return &http.Client{
		Transport: NewTransport(cfg),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return redirectErr
		},
	}
}

// IsBlockedLoopbackOrLinkLocal reports whether ip is a loopback (127.0.0.0/8,
// ::1), link-local unicast or multicast (169.254.0.0/16, fe80::/10, 224.0.0.0/24,
// ff02::/16), or unspecified (0.0.0.0, ::) address. RFC 1918 private addresses
// are NOT blocked by this predicate; callers that must also refuse private
// networks should use IsBlockedPrivateOrLocal (the default in Config{}).
func IsBlockedLoopbackOrLinkLocal(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// specialBlockedPrefixes covers non-global IPv4 and IPv6 ranges not flagged by
// netip.Addr's built-in IsLoopback/IsPrivate/IsLinkLocal*/IsMulticast/IsUnspecified
// predicates, including CGNAT (RFC 6598) and IPv6-to-IPv4 translation prefixes.
var specialBlockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network" (RFC 1122; routes to loopback on Linux)
		"100.64.0.0/10",   // shared address space / carrier-grade NAT (RFC 6598)
		"240.0.0.0/4",     // reserved for future use + limited broadcast
		"::/96",           // deprecated IPv4-compatible IPv6
		"::ffff:0:0:0/96", // IPv4-translated (SIIT)
		"64:ff9b::/96",    // NAT64 well-known prefix
		"64:ff9b:1::/48",  // NAT64 local-use prefix
		"100::/64",        // discard-only prefix
		"2001::/32",       // Teredo tunneling
		"2002::/16",       // 6to4 tunneling
		"fec0::/10",       // deprecated site-local
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// IsBlockedPrivateOrLocal reports whether ip is nil/invalid or falls in any
// private, loopback, link-local, multicast, unspecified, CGNAT, or special
// translation range (evaluating IPv4-mapped IPv6 addresses as IPv4).
func IsBlockedPrivateOrLocal(ip net.IP) bool {
	if len(ip) == 0 {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	if addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return true
	}
	for _, p := range specialBlockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
