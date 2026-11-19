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

package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResolver returns fixed IP addresses without real DNS.
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

// recordingDialer records dialed addresses and optionally forwards to target.
type recordingDialer struct {
	mu     sync.Mutex
	addrs  []string
	target string
	inner  ssrfclient.Dialer
}

func (d *recordingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	if d.inner == nil {
		return nil, errors.New("unexpected dial")
	}
	dialAddr := addr
	if d.target != "" {
		dialAddr = d.target
	}
	return d.inner.DialContext(ctx, network, dialAddr)
}

func (d *recordingDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.addrs))
	copy(out, d.addrs)
	return out
}

// tlsClientConfigForServer extracts a tls.Config trusting ts's self-signed cert
// with ServerName set to serverName so https://public.example:<port> verifies.
func tlsClientConfigForServer(t *testing.T, ts *httptest.Server, serverName string) *tls.Config {
	t.Helper()
	tr, ok := ts.Client().Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.TLSClientConfig)
	cfg := tr.TLSClientConfig.Clone()
	cfg.ServerName = serverName
	return cfg
}

// ---------------------------------------------------------------------------
// AC-4: Private, loopback, and link-local destinations are refused
// ---------------------------------------------------------------------------

func TestEgress_RefusesPrivateLoopbackAndLinkLocalDestinations(t *testing.T) {
	const secretKey = "sk-secret-do-not-leak-998877"

	cases := []struct {
		name string
		ips  []net.IPAddr
	}{
		{"loopback IPv4", []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{"loopback IPv6", []net.IPAddr{{IP: net.ParseIP("::1")}}},
		{"private RFC1918 10/8", []net.IPAddr{{IP: net.ParseIP("10.1.2.3")}}},
		{"private RFC1918 172.16/12", []net.IPAddr{{IP: net.ParseIP("172.20.0.5")}}},
		{"private RFC1918 192.168/16", []net.IPAddr{{IP: net.ParseIP("192.168.10.20")}}},
		{"private IPv6 ULA", []net.IPAddr{{IP: net.ParseIP("fd00::1234")}}},
		{"shared CGNAT 100.64/10", []net.IPAddr{{IP: net.ParseIP("100.64.1.2")}}},
		{"link-local cloud metadata", []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}},
		{"link-local IPv6", []net.IPAddr{{IP: net.ParseIP("fe80::1")}}},
		{"unspecified IPv4", []net.IPAddr{{IP: net.ParseIP("0.0.0.0")}}},
		{"IETF protocol 192.0.0/24", []net.IPAddr{{IP: net.ParseIP("192.0.0.1")}}},
		{"benchmarking 198.18/15", []net.IPAddr{{IP: net.ParseIP("198.18.0.1")}}},
		{"TEST-NET-1 192.0.2/24", []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}},
		{"TEST-NET-2 198.51.100/24", []net.IPAddr{{IP: net.ParseIP("198.51.100.1")}}},
		{"TEST-NET-3 203.0.113/24", []net.IPAddr{{IP: net.ParseIP("203.0.113.1")}}},
		{"IPv6 doc 2001:db8::/32", []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}}},
		{"IPv4-mapped metadata", []net.IPAddr{{IP: net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 169, 254, 169, 254}}}},
		{"all blocked mixed", []net.IPAddr{
			{IP: net.ParseIP("10.0.0.1")},
			{IP: net.ParseIP("127.0.0.1")},
			{IP: net.ParseIP("169.254.169.254")},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialer := &recordingDialer{}
			client := NewClient(
				StaticBearer(secretKey),
				WithDialerForTesting(&fakeResolver{ips: tc.ips}, dialer),
			)

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://provider.example/v1/systemone", strings.NewReader(`{}`))
			require.NoError(t, err)

			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
			assert.True(t, IsBlockedDestination(err), "expected blocked destination error, got: %v", err)
			assert.ErrorIs(t, err, ErrBlockedDestination)
			assert.Empty(t, dialer.dialed(), "raw dialer must never be invoked for blocked IPs")
			assert.NotContains(t, err.Error(), secretKey, "secret key must never appear in error")
		})
	}
}

func TestEgress_ProductionDefaultRefusesDirectLoopbackServer(t *testing.T) {
	const secretKey = "sk-live-secret-never-echo"

	var hit bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Production NewClient with NO test options must refuse 127.0.0.1.
	client := NewClient(StaticBearer(secretKey))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/systemone", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, IsBlockedDestination(err), "expected blocked destination error, got: %v", err)
	assert.False(t, hit, "loopback server must never be reached with production defaults")
	assert.NotContains(t, err.Error(), secretKey)
}

// ---------------------------------------------------------------------------
// AC-4: Non-HTTPS schemes refused
// ---------------------------------------------------------------------------

func TestEgress_RefusesNonHTTPS(t *testing.T) {
	const secretKey = "sk-scheme-secret-abc123"

	dialer := &recordingDialer{}
	client := NewClient(
		StaticBearer(secretKey),
		WithDialerForTesting(&fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}, dialer),
	)

	for _, rawURL := range []string{
		"http://api.typesafe.ai/v1/systemone",
		"ws://api.typesafe.ai/v1/systemone",
		"ftp://api.typesafe.ai/v1/systemone",
	} {
		t.Run(rawURL, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, rawURL, strings.NewReader(`{}`))
			require.NoError(t, err)

			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
			assert.True(t, IsSchemeNotAllowed(err), "expected scheme error, got: %v", err)
			assert.ErrorIs(t, err, ErrSchemeNotAllowed)
			assert.Empty(t, dialer.dialed(), "non-https request must fail before dialing")
			assert.NotContains(t, err.Error(), secretKey)
		})
	}
}

func TestEgress_RefusesURLUserinfo(t *testing.T) {
	const secretKey = "sk-userinfo-secret-xyz"

	dialer := &recordingDialer{}
	client := NewClient(
		StaticBearer(secretKey),
		WithDialerForTesting(&fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}, dialer),
	)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://user:pw@api.typesafe.ai/v1/systemone", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEndpoint)
	assert.Empty(t, dialer.dialed())
	assert.NotContains(t, err.Error(), secretKey)
}

// ---------------------------------------------------------------------------
// AC-4: Redirects refused
// ---------------------------------------------------------------------------

func TestEgress_RefusesRedirects(t *testing.T) {
	const secretKey = "sk-redirect-secret-4567"

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			var targetHit bool
			targetSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHit = true
				w.WriteHeader(http.StatusOK)
			}))
			defer targetSrv.Close()

			redirectSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", targetSrv.URL+"/internal-secret-hop")
				w.WriteHeader(status)
			}))
			defer redirectSrv.Close()

			_, port, err := net.SplitHostPort(redirectSrv.Listener.Addr().String())
			require.NoError(t, err)

			dialer := &recordingDialer{
				target: redirectSrv.Listener.Addr().String(),
				inner:  &net.Dialer{Timeout: 5 * time.Second},
			}
			client := NewClient(
				StaticBearer(secretKey),
				WithDialerForTesting(&fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}, dialer),
				WithTLSConfigForTesting(tlsClientConfigForServer(t, redirectSrv, "127.0.0.1")),
			)

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("https://public.example:%s/v1/systemone", port), strings.NewReader(`{}`))
			require.NoError(t, err)

			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
			assert.True(t, IsRedirectBlocked(err), "expected redirect blocked, got: %v", err)
			assert.ErrorIs(t, err, ErrRedirectBlocked)
			assert.False(t, targetHit, "redirect target must never be contacted")
			assert.NotContains(t, err.Error(), secretKey)
			assert.NotContains(t, err.Error(), "internal-secret-hop", "redirect Location header must not be echoed in errors")
		})
	}
}

// ---------------------------------------------------------------------------
// AC-4: Response body capped at 1 MiB (distinguishable error -> malformed_response)
// ---------------------------------------------------------------------------

func TestEgress_ResponseBodyCapAt1MiB(t *testing.T) {
	const secretKey = "sk-cap-secret-7890"

	t.Run("exact_1MiB_succeeds", func(t *testing.T) {
		payload := bytes.Repeat([]byte("a"), int(MaxResponseBytes))
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
		}))
		defer ts.Close()

		client := NewClient(StaticBearer(secretKey), WithLoopbackServerForTesting(ts.URL, ts.Client()))
		resp, err := client.Get(ts.URL + "/v1/models")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Len(t, got, int(MaxResponseBytes))
	})

	t.Run("over_1MiB_with_content_length_fails", func(t *testing.T) {
		payload := bytes.Repeat([]byte("b"), int(MaxResponseBytes)+1)
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
		}))
		defer ts.Close()

		client := NewClient(StaticBearer(secretKey), WithLoopbackServerForTesting(ts.URL, ts.Client()))
		resp, err := client.Get(ts.URL + "/v1/systemone")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.True(t, IsResponseTooLarge(err), "expected ErrResponseTooLarge, got: %v", err)
		assert.ErrorIs(t, err, ErrResponseTooLarge)
		assert.NotContains(t, err.Error(), secretKey)
	})

	t.Run("over_1MiB_chunked_without_content_length_fails", func(t *testing.T) {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			require.True(t, ok)
			w.WriteHeader(http.StatusOK)
			chunk := bytes.Repeat([]byte("c"), 64<<10)
			// Write 16 * 64 KiB + 1 byte = 1 MiB + 1 byte as chunked stream.
			for range 16 {
				_, _ = w.Write(chunk)
				flusher.Flush()
			}
			_, _ = w.Write([]byte("x"))
			flusher.Flush()
		}))
		defer ts.Close()

		client := NewClient(StaticBearer(secretKey), WithLoopbackServerForTesting(ts.URL, ts.Client()))
		resp, err := client.Get(ts.URL + "/v1/systemone")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.True(t, IsResponseTooLarge(err), "expected ErrResponseTooLarge on chunked stream, got: %v", err)
		assert.ErrorIs(t, err, ErrResponseTooLarge)
		assert.NotContains(t, err.Error(), secretKey)
	})
}

// ---------------------------------------------------------------------------
// AC-2 & AC-4: Bearer auth present ONLY in Authorization, never mutated onto
// caller's Request, and NEVER echoed in errors
// ---------------------------------------------------------------------------

func TestEgress_BearerHeaderOnlyInWireAuthorizationAndNeverEchoedInErrors(t *testing.T) {
	const secretKey = "sk-ultra-sensitive-bearer-key-31415926"

	var (
		gotHeaders http.Header
		gotBody    string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	client := NewClient(
		func(ctx context.Context) (string, error) {
			return secretKey, nil
		},
		WithLoopbackServerForTesting(ts.URL, ts.Client()),
	)

	callerReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/systemone", strings.NewReader(`{"model":"jev-1.14.0"}`))
	require.NoError(t, err)
	callerReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(callerReq)
	require.NoError(t, err)
	_ = resp.Body.Close()

	// 1. Key appears on the wire in Authorization: Bearer <key>.
	assert.Equal(t, "Bearer "+secretKey, gotHeaders.Get("Authorization"))

	// 2. Key appears NOWHERE else in wire headers or wire body.
	for name, vals := range gotHeaders {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, secretKey, "header %s leaked secret", name)
		}
	}
	assert.NotContains(t, gotBody, secretKey)

	// 3. Caller's original *http.Request was NOT mutated with the credential.
	assert.Empty(t, callerReq.Header.Get("Authorization"), "RoundTrip must not mutate caller request headers")
}

func TestEgress_AuthSourceErrorsAndInvalidTokensNeverLeakCredential(t *testing.T) {
	const secretKey = "sk-leaked-in-source-error-271828"

	t.Run("nil_bearer_source", func(t *testing.T) {
		client := NewClient(nil)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.True(t, IsAuthError(err))
		assert.ErrorIs(t, err, ErrAuth)
	})

	t.Run("empty_token", func(t *testing.T) {
		client := NewClient(StaticBearer("   "))
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.True(t, IsAuthError(err))
	})

	t.Run("control_chars_in_token_never_echoed", func(t *testing.T) {
		badKey := secretKey + "\r\nX-Injected: evil"
		client := NewClient(StaticBearer(badKey))
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.True(t, IsAuthError(err))
		assert.NotContains(t, err.Error(), secretKey)
		assert.NotContains(t, err.Error(), "X-Injected")
	})

	t.Run("bearer_source_error_containing_secret_is_scrubbed_as_credential_unavailable", func(t *testing.T) {
		client := NewClient(func(context.Context) (string, error) {
			return "", fmt.Errorf("vault failed for key %s", secretKey)
		})
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.True(t, IsCredentialUnavailable(err))
		assert.ErrorIs(t, err, ErrCredentialUnavailable)
		assert.False(t, IsAuthError(err))
		assert.NotContains(t, err.Error(), secretKey, "bearer source error text must never propagate secret material")
	})

	t.Run("bearer_source_returning_err_auth_preserves_auth", func(t *testing.T) {
		client := NewClient(func(context.Context) (string, error) {
			return "", ErrAuth
		})
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.True(t, IsAuthError(err))
		assert.False(t, IsCredentialUnavailable(err))
	})

	t.Run("transport_error_containing_secret_is_scrubbed", func(t *testing.T) {
		dialer := &recordingDialer{
			inner: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
				return nil, fmt.Errorf("synthetic transport failure mentioning %s", secretKey)
			}),
		}
		client := NewClient(
			StaticBearer(secretKey),
			WithDialerForTesting(&fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}, dialer),
		)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.typesafe.ai/v1/models", nil)
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretKey)
	})
}

func TestEgress_ProdTransportSetsIdleConnTimeoutWithoutChangingSSRFDefault(t *testing.T) {
	client := NewClient(StaticBearer("sk-test"))
	rt, ok := client.Transport.(*egressRoundTripper)
	require.True(t, ok)
	prodTr, ok := rt.prodTransport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, 90*time.Second, prodTr.IdleConnTimeout)
	assert.Equal(t, DefaultIdleConnTimeout, prodTr.IdleConnTimeout)

	// Verify ssrfclient default IdleConnTimeout remains untouched so lifecycle hooks
	// preserve exact existing behavior.
	ssrfTr := ssrfclient.NewTransport(ssrfclient.Config{})
	assert.Zero(t, ssrfTr.IdleConnTimeout)
}

type dialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

func TestEgress_LoopbackTestSeamConfinedToExactLoopbackServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("other loopback server must not be reachable")
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	client := NewClient(StaticBearer("sk-test"), WithLoopbackServerForTesting(ts.URL, ts.Client()))

	// Allowed: the exact configured httptest server.
	resp, err := client.Get(ts.URL + "/v1/models")
	require.NoError(t, err)
	_ = resp.Body.Close()

	// Refused: a DIFFERENT loopback port on http://127.0.0.1.
	resp2, err := client.Get(other.URL + "/v1/models")
	if resp2 != nil {
		_ = resp2.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, IsSchemeNotAllowed(err) || IsBlockedDestination(err), "unexpected error: %v", err)

	// Refused: passing a non-loopback URL into WithLoopbackServerForTesting fails closed.
	badClient := NewClient(StaticBearer("sk-test"), WithLoopbackServerForTesting("https://api.typesafe.ai", ts.Client()))
	resp3, err := badClient.Get("https://api.typesafe.ai/v1/models")
	if resp3 != nil {
		_ = resp3.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEndpoint)
	assert.True(t, IsInvalidEndpoint(err))
}

func TestEgress_PreservesContextCancelAndDeadlineAndNon2xxResponsesUnderCap(t *testing.T) {
	const secretKey = "sk-ctx-secret-999"

	hangDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang" {
			select {
			case <-hangDone:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("X-Typesafe-Request-Id", "req-abc")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited"}}`))
	}))
	defer ts.Close()
	defer close(hangDone)

	client := NewClient(StaticBearer(secretKey), WithLoopbackServerForTesting(ts.URL, ts.Client()))

	t.Run("non_2xx_under_1MiB_returns_response_intact", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/v1/systemone")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.Equal(t, "req-abc", resp.Header.Get("X-Typesafe-Request-Id"))
		assert.Equal(t, "2", resp.Header.Get("Retry-After"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `{"error":{"code":"rate_limited"}}`, string(body))
	})

	t.Run("context_deadline_exceeded_preserved", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/hang", strings.NewReader(`{}`))
		require.NoError(t, err)
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotContains(t, err.Error(), secretKey)
	})

	t.Run("context_canceled_preserved", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/hang", strings.NewReader(`{}`))
		require.NoError(t, err)
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), secretKey)
	})
}

// ---------------------------------------------------------------------------
// PV3 G0: Credential placement (WithCredentialPlacement: header vs bearer)
// ---------------------------------------------------------------------------

func TestEgress_HeaderCredentialPlacement_OnlyInConfiguredHeaderAndNeverInAuthorization(t *testing.T) {
	const canaryKey = "AIzaSy-synthetic-canary-gemini-key-884422"

	var (
		gotHeaders http.Header
		gotQuery   string
		gotPath    string
		gotBody    string
	)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer ts.Close()

	client := NewClient(
		StaticBearer(canaryKey),
		WithCredentialPlacement(Placement{
			Kind:       provider.AuthKindHeader,
			HeaderName: "x-goog-api-key",
		}),
		WithLoopbackServerForTesting(ts.URL, ts.Client()),
	)

	callerReq, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		ts.URL+"/v1beta/decisions",
		strings.NewReader(`{"model":"gemini-decision-1"}`),
	)
	require.NoError(t, err)
	callerReq.Header.Set("Content-Type", "application/json")
	// Even if a caller accidentally pre-populated Authorization, header placement
	// must strip it so Authorization is never sent.
	callerReq.Header.Set("Authorization", "Bearer pre-existing-should-be-stripped")

	resp, err := client.Do(callerReq)
	require.NoError(t, err)
	_ = resp.Body.Close()

	// 1. Key appears on the wire ONLY in x-goog-api-key.
	assert.Equal(t, canaryKey, gotHeaders.Get("x-goog-api-key"))
	assert.Empty(t, gotHeaders.Get("Authorization"), "Authorization must never be set with header placement")

	// 2. Key appears NOWHERE else in headers, URL path, query, or body.
	for name, vals := range gotHeaders {
		if strings.EqualFold(name, "x-goog-api-key") {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, canaryKey, "header %s leaked secret", name)
		}
	}
	assert.NotContains(t, gotPath, canaryKey)
	assert.NotContains(t, gotQuery, canaryKey)
	assert.NotContains(t, gotBody, canaryKey)

	// 3. Caller's original *http.Request was NOT mutated with the credential.
	assert.Empty(t, callerReq.Header.Get("x-goog-api-key"), "RoundTrip must not mutate caller request headers")
}

func TestEgress_HeaderCredentialPlacement_ScrubsSecretAndDefaultsZeroPlacementToBearer(t *testing.T) {
	const canaryKey = "AIzaSy-scrub-canary-secret-551199"

	t.Run("transport_error_scrubs_header_secret", func(t *testing.T) {
		dialer := &recordingDialer{
			inner: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
				return nil, fmt.Errorf("dial failed with echoed header %s", canaryKey)
			}),
		}
		client := NewClient(
			StaticBearer(canaryKey),
			WithCredentialPlacement(HeaderPlacement("x-goog-api-key")),
			WithDialerForTesting(&fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}, dialer),
		)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/decisions", strings.NewReader(`{}`))
		require.NoError(t, err)
		_, err = client.Do(req)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), canaryKey)
	})

	t.Run("zero_placement_defaults_to_bearer", func(t *testing.T) {
		var gotAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		client := NewClient(
			StaticBearer(canaryKey),
			WithCredentialPlacement(Placement{}),
			WithLoopbackServerForTesting(ts.URL, ts.Client()),
		)
		resp, err := client.Get(ts.URL + "/v1/models")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, "Bearer "+canaryKey, gotAuth)
	})

	t.Run("none_placement_attaches_no_auth_header_without_calling_source", func(t *testing.T) {
		var gotHeaders http.Header
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		sourceCalled := false
		client := NewClient(
			func(context.Context) (string, error) {
				sourceCalled = true
				return canaryKey, nil
			},
			WithCredentialPlacement(Placement{Kind: provider.AuthKindNone}),
			WithLoopbackServerForTesting(ts.URL, ts.Client()),
		)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/models", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer should-not-be-sent")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.False(t, sourceCalled, "AuthKindNone must not invoke credential source")
		assert.Empty(t, gotHeaders.Get("Authorization"))
	})
}

func TestEgress_RejectsInvalidAndReservedCredentialPlacements(t *testing.T) {
	const secretKey = "AIzaSy-never-send-on-invalid-placement"
	const badHeaderCanary = "CANARY_BAD_HEADER_NAME_DO_NOT_ECHO"

	invalidPlacements := []struct {
		name      string
		placement Placement
	}{
		{name: "header_empty_name", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: ""}},
		{name: "header_whitespace_only", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "   "}},
		{name: "header_leading_space", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: " x-goog-api-key"}},
		{name: "header_trailing_space", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key "}},
		{name: "header_internal_space", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x goog api key"}},
		{name: "header_crlf_injection", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key\r\nX-Injected: " + badHeaderCanary}},
		{name: "header_lf_injection", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key\n" + badHeaderCanary}},
		{name: "header_nul_byte", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-key\x00bad"}},
		{name: "header_del_byte", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-key\x7fbad"}},
		{name: "header_colon_delimiter", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key: " + badHeaderCanary}},
		{name: "header_slash_delimiter", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog/api-key"}},
		{name: "header_quotes_delimiter", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: `"x-goog-api-key"`}},
		{name: "header_non_ascii", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-ключ"}},
		{name: "header_oversize_name", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-" + strings.Repeat("a", 200)}},
		{name: "reserved_authorization", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Authorization"}},
		{name: "reserved_authorization_lower", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "authorization"}},
		{name: "reserved_proxy_authorization", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Proxy-Authorization"}},
		{name: "reserved_host", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Host"}},
		{name: "reserved_cookie", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Cookie"}},
		{name: "reserved_set_cookie", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Set-Cookie"}},
		{name: "reserved_content_length", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Content-Length"}},
		{name: "reserved_content_type", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Content-Type"}},
		{name: "reserved_content_encoding", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Content-Encoding"}},
		{name: "hop_by_hop_transfer_encoding", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Transfer-Encoding"}},
		{name: "hop_by_hop_connection", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Connection"}},
		{name: "hop_by_hop_keep_alive", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Keep-Alive"}},
		{name: "hop_by_hop_proxy_authenticate", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Proxy-Authenticate"}},
		{name: "hop_by_hop_proxy_connection", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Proxy-Connection"}},
		{name: "hop_by_hop_te", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "TE"}},
		{name: "hop_by_hop_trailer", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Trailer"}},
		{name: "hop_by_hop_trailers", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Trailers"}},
		{name: "hop_by_hop_upgrade", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Upgrade"}},
		{name: "reserved_expect", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Expect"}},
		{name: "reserved_via", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Via"}},
		{name: "reserved_forwarded", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "Forwarded"}},
		{name: "reserved_x_forwarded_for", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "X-Forwarded-For"}},
		{name: "reserved_x_goog_user_project", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-user-project"}},
		{name: "reserved_x_goog_user_project_canonical", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "X-Goog-User-Project"}},
		{name: "bearer_with_header_name", placement: Placement{Kind: provider.AuthKindBearer, HeaderName: "x-goog-api-key"}},
		{name: "empty_kind_with_header_name", placement: Placement{Kind: "", HeaderName: "x-goog-api-key"}},
		{name: "none_with_header_name", placement: Placement{Kind: provider.AuthKindNone, HeaderName: "x-goog-api-key"}},
		{name: "header_with_quota_project", placement: Placement{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key", QuotaProject: "valid-project-123"}},
		{name: "none_with_quota_project", placement: Placement{Kind: provider.AuthKindNone, QuotaProject: "valid-project-123"}},
		{name: "bearer_quota_project_too_short", placement: ADCBearerPlacement("abcde")},
		{name: "bearer_quota_project_too_long", placement: ADCBearerPlacement("a" + strings.Repeat("b", 30))},
		{name: "bearer_quota_project_starts_with_digit", placement: ADCBearerPlacement("1project")},
		{name: "bearer_quota_project_starts_with_hyphen", placement: ADCBearerPlacement("-project")},
		{name: "bearer_quota_project_ends_with_hyphen", placement: ADCBearerPlacement("project-")},
		{name: "bearer_quota_project_uppercase", placement: ADCBearerPlacement("MyProject")},
		{name: "bearer_quota_project_underscore", placement: ADCBearerPlacement("my_project")},
		{name: "bearer_quota_project_colon", placement: ADCBearerPlacement("google.com:my-proj")},
		{name: "bearer_quota_project_crlf_canary", placement: ADCBearerPlacement("proj-12\r\nX-Evil: " + badHeaderCanary)},
		{name: "unsupported_google_access_token", placement: Placement{Kind: provider.AuthKindGoogleAccessToken}},
		{name: "unsupported_unknown_kind", placement: Placement{Kind: provider.AuthKind("digest_" + badHeaderCanary)}},
	}

	for _, tc := range invalidPlacements {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, ValidatePlacement(tc.placement))

			var hit bool
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			// Order WithLoopbackServerForTesting BOTH after and before WithCredentialPlacement
			// to confirm a later option never clears an earlier configErr.
			for _, opts := range [][]Option{
				{WithCredentialPlacement(tc.placement), WithLoopbackServerForTesting(ts.URL, ts.Client())},
				{WithLoopbackServerForTesting(ts.URL, ts.Client()), WithCredentialPlacement(tc.placement)},
			} {
				client := NewClient(StaticBearer(secretKey), opts...)
				req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1beta/decisions", strings.NewReader(`{}`))
				require.NoError(t, err)

				resp, err := client.Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				require.Error(t, err)
				assert.True(t, IsInvalidPlacement(err), "expected ErrInvalidPlacement, got: %v", err)
				assert.ErrorIs(t, err, ErrInvalidPlacement)
				assert.False(t, hit, "invalid placement must never contact server or fall back to bearer")
				assert.NotContains(t, err.Error(), secretKey)
				assert.NotContains(t, err.Error(), badHeaderCanary, "invalid header/kind text must not be echoed in errors")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Import Guard (P1 brief §4 & common-dev-context)
// ---------------------------------------------------------------------------

func TestEgress_ImportGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	fset := token.NewFileSet()
	nonTestCount := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		nonTestCount++
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		require.NoError(t, err)

		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(path, "github.com/GoogleCloudPlatform/scion/") {
				// Must be Go standard library (no dots in first path element).
				first, _, _ := strings.Cut(path, "/")
				assert.NotContains(t, first, ".", "non-stdlib external import %q not allowed in egress", path)
				continue
			}
			// Within this module, egress may depend ONLY on pkg/util/ssrfclient
			// and pkg/systemone/provider (never pkg/hub, pkg/store, pkg/secret,
			// pkg/messaging, pkg/ruleengine, any trigger package, or
			// pkg/systemone/runtime).
			allowed := path == "github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient" ||
				path == "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
			assert.True(t, allowed, "forbidden internal import %q in %s", path, file)
		}
	}
	require.Greater(t, nonTestCount, 0, "expected at least one non-test Go file")
}

func TestEgress_ADCBearerQuotaProjectPlacementAndCanaryIsolation(t *testing.T) {
	const canaryToken = "fake-adc-token-CANARY-egress-001"
	const quotaProject = "paid-gemini-proj-42"

	var (
		mu         sync.Mutex
		gotHeaders http.Header
		gotQuery   string
		gotBody    []byte
	)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotHeaders = r.Header.Clone()
		gotQuery = r.URL.RawQuery
		gotBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	t.Run("adc_bearer_with_quota_project_sets_authorization_and_user_project_only", func(t *testing.T) {
		require.NoError(t, ValidateQuotaProject(quotaProject))
		require.NoError(t, ValidateQuotaProject("abcdef"))
		require.NoError(t, ValidateQuotaProject("a"+strings.Repeat("b", 28)+"9"))

		client := NewClient(
			StaticBearer(canaryToken),
			WithCredentialPlacement(ADCBearerPlacement(quotaProject)),
			WithLoopbackServerForTesting(ts.URL, ts.Client()),
		)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1beta/decisions", strings.NewReader(`{"model":"gemini-decision-1"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		// Adapter-set x-goog-user-project must be overwritten/controlled solely by egress.
		req.Header.Set("x-goog-user-project", "adapter-spoofed-proj")

		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		// Caller's original request must not have been mutated with token or quota project.
		assert.Empty(t, req.Header.Get("Authorization"))
		assert.Equal(t, "adapter-spoofed-proj", req.Header.Get("x-goog-user-project"))

		mu.Lock()
		hdrs := gotHeaders.Clone()
		query := gotQuery
		body := string(gotBody)
		mu.Unlock()

		assert.Equal(t, "Bearer "+canaryToken, hdrs.Get("Authorization"))
		assert.Equal(t, []string{quotaProject}, hdrs.Values("X-Goog-User-Project"))
		assert.Empty(t, hdrs.Get("x-goog-api-key"))
		assert.Empty(t, query)
		assert.NotContains(t, body, canaryToken)
		for name, vals := range hdrs {
			if strings.EqualFold(name, "Authorization") {
				continue
			}
			for _, v := range vals {
				assert.NotContains(t, v, canaryToken, "canary token leaked into header %s", name)
			}
		}
	})

	t.Run("adc_bearer_without_quota_project_omits_and_strips_user_project", func(t *testing.T) {
		client := NewClient(
			StaticBearer(canaryToken),
			WithCredentialPlacement(ADCBearerPlacement("")),
			WithLoopbackServerForTesting(ts.URL, ts.Client()),
		)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1beta/decisions", strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("x-goog-user-project", "adapter-spoofed-proj")

		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		mu.Lock()
		hdrs := gotHeaders.Clone()
		mu.Unlock()

		assert.Equal(t, "Bearer "+canaryToken, hdrs.Get("Authorization"))
		assert.Empty(t, hdrs.Values("X-Goog-User-Project"), "x-goog-user-project must be omitted when QuotaProject is unset and stripped if pre-set")
		assert.Empty(t, hdrs.Get("x-goog-api-key"))
	})

	t.Run("header_placement_strips_any_adapter_set_user_project", func(t *testing.T) {
		client := NewClient(
			StaticBearer("AIzaSy-fake-key-canary"),
			WithCredentialPlacement(HeaderPlacement("x-goog-api-key")),
			WithLoopbackServerForTesting(ts.URL, ts.Client()),
		)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1beta/decisions", strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("x-goog-user-project", "adapter-spoofed-proj")

		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		mu.Lock()
		hdrs := gotHeaders.Clone()
		mu.Unlock()

		assert.Equal(t, "AIzaSy-fake-key-canary", hdrs.Get("x-goog-api-key"))
		assert.Empty(t, hdrs.Get("Authorization"))
		assert.Empty(t, hdrs.Values("X-Goog-User-Project"))
	})
}
