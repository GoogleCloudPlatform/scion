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

package ssrfclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SSRF Protection — IsBlockedLoopbackOrLinkLocal unit tests
// (moved unchanged from pkg/hub/lifecycle_hook_executor_test.go)
// ---------------------------------------------------------------------------

func TestIsBlockedSSRFTarget(t *testing.T) {
	tests := []struct {
		name    string
		ip      net.IP
		blocked bool
	}{
		// Loopback — MUST block
		{"loopback IPv4", net.ParseIP("127.0.0.1"), true},
		{"loopback IPv4 other", net.ParseIP("127.0.0.2"), true},
		{"loopback IPv6", net.ParseIP("::1"), true},

		// Link-local — MUST block (includes metadata service 169.254.169.254)
		{"link-local IPv4 metadata", net.ParseIP("169.254.169.254"), true},
		{"link-local IPv4 base", net.ParseIP("169.254.0.1"), true},
		{"link-local IPv6", net.ParseIP("fe80::1"), true},

		// Link-local multicast — MUST block
		{"link-local multicast IPv4", net.ParseIP("224.0.0.1"), true},
		{"link-local multicast IPv6", net.ParseIP("ff02::1"), true},

		// Unspecified/any — MUST block (routes to loopback on many platforms)
		{"unspecified IPv4", net.ParseIP("0.0.0.0"), true},
		{"unspecified IPv6", net.ParseIP("::"), true},

		// RFC1918 — MUST ALLOW when AllowPrivateNetworks is true (internal service registries)
		{"RFC1918 10.x", net.ParseIP("10.0.0.1"), false},
		{"RFC1918 172.16.x", net.ParseIP("172.16.0.1"), false},
		{"RFC1918 192.168.x", net.ParseIP("192.168.1.1"), false},

		// Public IPs — MUST ALLOW
		{"public IPv4", net.ParseIP("8.8.8.8"), false},
		{"public IPv4 other", net.ParseIP("203.0.113.1"), false},
		{"public IPv6", net.ParseIP("2001:4860:4860::8888"), false},

		// nil IP
		{"nil IP", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsBlockedLoopbackOrLinkLocal(tc.ip)
			assert.Equal(t, tc.blocked, got, "IsBlockedLoopbackOrLinkLocal(%s)", tc.ip)
		})
	}
}

// ---------------------------------------------------------------------------
// SSRF Protection — IsBlockedPrivateOrLocal unit tests (strict default)
// ---------------------------------------------------------------------------

func TestIsBlockedPrivateOrLocal(t *testing.T) {
	tests := []struct {
		name    string
		ip      net.IP
		blocked bool
	}{
		// Loopback
		{"loopback IPv4", net.ParseIP("127.0.0.1"), true},
		{"loopback IPv4 high", net.ParseIP("127.255.255.254"), true},
		{"loopback IPv6", net.ParseIP("::1"), true},
		{"loopback IPv4-mapped IPv6", net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 127, 0, 0, 1}, true},

		// Private (RFC 1918 + IPv6 ULA + CGNAT)
		{"RFC1918 10.x", net.ParseIP("10.0.0.1"), true},
		{"RFC1918 172.16.x", net.ParseIP("172.16.0.1"), true},
		{"RFC1918 172.31.x", net.ParseIP("172.31.255.254"), true},
		{"RFC1918 192.168.x", net.ParseIP("192.168.1.1"), true},
		{"RFC1918 IPv4-mapped IPv6", net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}, true},
		{"IPv6 ULA fc00", net.ParseIP("fc00::1"), true},
		{"IPv6 ULA fd00", net.ParseIP("fd12:3456:789a::1"), true},
		{"CGNAT 100.64.x", net.ParseIP("100.64.0.1"), true},
		{"CGNAT 100.127.x", net.ParseIP("100.127.255.254"), true},

		// Link-local & cloud metadata
		{"link-local metadata", net.ParseIP("169.254.169.254"), true},
		{"link-local IPv4", net.ParseIP("169.254.0.1"), true},
		{"link-local IPv6", net.ParseIP("fe80::1"), true},
		{"metadata IPv4-mapped IPv6", net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 169, 254, 169, 254}, true},

		// Multicast, unspecified, reserved, translation prefixes
		{"multicast IPv4", net.ParseIP("224.0.0.1"), true},
		{"multicast IPv6", net.ParseIP("ff02::1"), true},
		{"unspecified IPv4", net.ParseIP("0.0.0.0"), true},
		{"this-network IPv4", net.ParseIP("0.1.2.3"), true},
		{"unspecified IPv6", net.ParseIP("::"), true},
		{"broadcast IPv4", net.ParseIP("255.255.255.255"), true},
		{"NAT64", net.ParseIP("64:ff9b::a00:1"), true},
		{"6to4", net.ParseIP("2002:7f00:1::1"), true},
		{"site-local deprecated", net.ParseIP("fec0::1"), true},
		{"nil IP", nil, true},

		// Public IPs — MUST ALLOW
		{"public IPv4 8.8.8.8", net.ParseIP("8.8.8.8"), false},
		{"public IPv4 172.32.x", net.ParseIP("172.32.0.1"), false},
		{"public IPv4 100.128.x", net.ParseIP("100.128.0.1"), false},
		{"public IPv4 203.0.113.1", net.ParseIP("203.0.113.1"), false},
		{"public IPv6", net.ParseIP("2001:4860:4860::8888"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsBlockedPrivateOrLocal(tc.ip)
			assert.Equal(t, tc.blocked, got, "IsBlockedPrivateOrLocal(%s)", tc.ip)
		})
	}
}

// ---------------------------------------------------------------------------
// SSRF dialer hardening — DNS-rebinding TOCTOU closure
// (moved unchanged from pkg/hub/lifecycle_hook_executor_test.go)
// ---------------------------------------------------------------------------

// fakeResolver returns a fixed set of IPs for any hostname lookup.
type fakeResolver struct {
	ips []net.IPAddr
	err error
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.ips, nil
}

// capturingDialer records the addr passed to DialContext, then delegates to a
// real dialer. This lets us verify the dialer is called with an IP, not a host.
type capturingDialer struct {
	mu       sync.Mutex
	addrs    []string
	delegate Dialer
}

func (d *capturingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return d.delegate.DialContext(ctx, network, addr)
}

func (d *capturingDialer) getAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.addrs))
	copy(out, d.addrs)
	return out
}

// rewritingDialer always dials a fixed target address, regardless of the
// addr argument. This lets tests verify what address the SSRF transport
// INTENDED to dial while still reaching an actual httptest server.
type rewritingDialer struct {
	target string
	inner  Dialer
}

func (d *rewritingDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return d.inner.DialContext(ctx, network, d.target)
}

func TestSSRFDialer_DialsByValidatedIP(t *testing.T) {
	// Start a test server to accept the connection.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Parse the httptest server's address to get its actual IP and port.
	_, tsPort, err := net.SplitHostPort(ts.Listener.Addr().String())
	require.NoError(t, err)

	// We use 10.0.0.1 as the "resolved" IP (RFC1918, allowed when
	// AllowPrivateNetworks is true) and route the actual dial back to the
	// httptest server via capturingDialer.
	allowedIP := net.ParseIP("10.0.0.1")

	resolver := &fakeResolver{
		ips: []net.IPAddr{{IP: allowedIP}},
	}

	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	capturing := &capturingDialer{
		delegate: &rewritingDialer{
			target: ts.Listener.Addr().String(),
			inner:  realDialer,
		},
	}

	client := NewClient(Config{
		AllowPrivateNetworks: true,
		Resolver:             resolver,
		Dialer:               capturing,
	})

	resp, err := client.Get(fmt.Sprintf("http://some-host.example.com:%s/api", tsPort))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the dialer was called with the IP address, not the hostname.
	addrs := capturing.getAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, net.JoinHostPort(allowedIP.String(), tsPort), addrs[0],
		"dialer must be called with the validated IP, not the hostname")
}

func TestSSRFDialer_AllBlockedIPsRefused(t *testing.T) {
	// A resolver that returns only blocked IPs (loopback + link-local).
	resolver := &fakeResolver{
		ips: []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("::1")},
			{IP: net.ParseIP("169.254.169.254")},
		},
	}

	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	client := NewClient(Config{
		AllowPrivateNetworks: true,
		Resolver:             resolver,
		Dialer:               realDialer,
	})

	_, err := client.Get("http://evil-host.example.com:8080/steal")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSRF protection")
	assert.Contains(t, err.Error(), "all resolved IPs")
	assert.ErrorIs(t, err, ErrBlockedAddress)
}

func TestSSRFDialer_MixedIPsDialsFirstAllowed(t *testing.T) {
	// Resolver returns a blocked IP first, then an allowed IP.
	allowedIP := net.ParseIP("10.0.0.5")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	_, tsPort, err := net.SplitHostPort(ts.Listener.Addr().String())
	require.NoError(t, err)

	resolver := &fakeResolver{
		ips: []net.IPAddr{
			{IP: net.ParseIP("127.0.0.1")}, // blocked
			{IP: allowedIP},                // allowed — should be dialed
		},
	}

	realDialer := &net.Dialer{Timeout: 5 * time.Second}
	capturing := &capturingDialer{
		delegate: &rewritingDialer{
			target: ts.Listener.Addr().String(),
			inner:  realDialer,
		},
	}

	client := NewClient(Config{
		AllowPrivateNetworks: true,
		Resolver:             resolver,
		Dialer:               capturing,
	})
	resp, err := client.Get(fmt.Sprintf("http://mixed-host.example.com:%s/api", tsPort))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the dialer was called with the allowed IP, skipping the blocked one.
	addrs := capturing.getAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, net.JoinHostPort(allowedIP.String(), tsPort), addrs[0])
}

func TestSSRFDialer_DefaultConfigRefusesPrivateAndZonedIPs(t *testing.T) {
	resolver := &fakeResolver{
		ips: []net.IPAddr{
			{IP: net.ParseIP("10.0.0.1")},
			{IP: net.ParseIP("172.16.0.1")},
			{IP: net.ParseIP("192.168.1.1")},
			{IP: net.ParseIP("2001:4860:4860::8888"), Zone: "eth0"},
		},
	}
	capturing := &capturingDialer{
		delegate: &net.Dialer{Timeout: 5 * time.Second},
	}

	// Default Config{} has AllowPrivateNetworks == false.
	client := NewClient(Config{
		Resolver: resolver,
		Dialer:   capturing,
	})

	_, err := client.Get("http://internal.example.com:8080/api")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBlockedAddress)
	assert.Empty(t, capturing.getAddrs(), "dialer must never be invoked when all IPs are blocked")
}

func TestNewClient_BlocksRedirects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1234/internal", http.StatusFound)
	}))
	defer ts.Close()

	_, tsPort, err := net.SplitHostPort(ts.Listener.Addr().String())
	require.NoError(t, err)

	client := NewClient(Config{
		Resolver: &fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}},
		Dialer: &rewritingDialer{
			target: ts.Listener.Addr().String(),
			inner:  &net.Dialer{Timeout: 5 * time.Second},
		},
	})

	resp, err := client.Get(fmt.Sprintf("http://public.example.com:%s/redirect", tsPort))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRedirectBlocked))
}
