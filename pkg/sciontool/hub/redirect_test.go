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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var redirectCredentialHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Serverless-Authorization",
	"X-Scion-Agent-Token",
	"X-Scion-Run-Id",
	"X-Custom",
}

type redirectHopRecorder struct {
	mu   sync.Mutex
	hops map[string]map[string]string
}

func (r *redirectHopRecorder) record(key string, req *http.Request) {
	got := map[string]string{}
	for _, name := range redirectCredentialHeaders {
		if v := req.Header.Get(name); v != "" {
			got[name] = v
		}
	}
	r.mu.Lock()
	if r.hops == nil {
		r.hops = map[string]map[string]string{}
	}
	r.hops[key] = got
	r.mu.Unlock()
}

func (r *redirectHopRecorder) headers(key string) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hops[key]
	return h, ok
}

func sortedHeaderNames(h map[string]string) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

type staticTransportSource struct {
	tok string
}

func (s staticTransportSource) Token() (string, error) { return s.tok, nil }
func (s staticTransportSource) SetToken(string, time.Time) {
}
func (s staticTransportSource) Expiry() time.Time { return time.Time{} }

type namedOriginNet struct {
	addrs map[string]string
}

func newNamedOriginNet(t *testing.T, routes map[string]http.HandlerFunc) *namedOriginNet {
	t.Helper()
	n := &namedOriginNet{addrs: map[string]string{}}
	for hostport, h := range routes {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		n.addrs[hostport] = srv.Listener.Addr().String()
	}
	return n
}

func (n *namedOriginNet) transport() *http.Transport {
	var d net.Dialer
	rt := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target, ok := n.addrs[addr]
			if !ok {
				return nil, fmt.Errorf("no test server for %s", addr)
			}
			return d.DialContext(ctx, network, target)
		},
	}
	return rt
}

// TestClient_NewClient_CrossOriginRedirectDropsCredentials exercises an
// untouched NewClient() built purely from environment variables (nil base
// RoundTripper -> http.DefaultTransport) when the hub redirects to another
// loopback origin.
func TestClient_NewClient_CrossOriginRedirectDropsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		header string
	}{
		{"authorization", "", "Authorization"},
		{"proxy_authorization", "iap", "Proxy-Authorization"},
		{"serverless_authorization", "cloudrun_invoker", "X-Serverless-Authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrubHubEnv(t)
			t.Cleanup(SetTokenHome(t.TempDir()))
			t.Cleanup(overrideGCPDetection(false))

			rec := &redirectHopRecorder{}
			otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record("other"+r.URL.Path, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"phase":"running"}`)
			}))
			t.Cleanup(otherSrv.Close)

			hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record("hub"+r.URL.Path, r)
				http.Redirect(w, r, otherSrv.URL+"/landed", http.StatusFound)
			}))
			t.Cleanup(hubSrv.Close)

			transportJWT := makeTestJWT(time.Now().Add(time.Hour))
			t.Setenv(EnvHubEndpoint, hubSrv.URL)
			t.Setenv(EnvHubToken, "secret-agent-token")
			t.Setenv(EnvAgentID, "agent-1")
			t.Setenv(EnvLaunchID, "run-42")
			t.Setenv(transportauth.EnvTransportToken, transportJWT)
			t.Setenv(transportauth.EnvTransportMode, tc.mode)

			c := NewClient()
			require.NotNil(t, c)
			c.maxRetries = 0

			_, err := c.GetSelf(context.Background())
			require.NoError(t, err)

			hubHeaders, ok := rec.headers("hub/api/v1/agents/agent-1")
			require.True(t, ok, "hub must receive initial request")
			assert.Equal(t, "secret-agent-token", hubHeaders["X-Scion-Agent-Token"])
			assert.Equal(t, "run-42", hubHeaders["X-Scion-Run-Id"])
			assert.Equal(t, "Bearer "+transportJWT, hubHeaders[tc.header])

			otherHeaders, ok := rec.headers("other/landed")
			require.True(t, ok, "redirect target must be reached")
			assert.Empty(t, sortedHeaderNames(otherHeaders),
				"cross-origin redirect hop must carry no agent or transport credentials, got %v", otherHeaders)
		})
	}
}

// TestClient_CrossOriginRedirectDropsCredentials covers redirects to another
// host, a subdomain of the hub host, and another port on the hub host across
// all three transportauth header modes.
func TestClient_CrossOriginRedirectDropsCredentials(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	targets := []struct {
		name string
		loc  string
		hop  string
	}{
		{"other_host", "http://storage.test/landed", "storage.test/landed"},
		{"subdomain", "http://api.hub.test/landed", "api.hub.test/landed"},
		{"other_port", "http://hub.test:8080/landed", "hub.test:8080/landed"},
	}
	modes := []struct {
		name   string
		mode   transportauth.HeaderMode
		header string
	}{
		{"authorization", transportauth.HeaderAuthorization, "Authorization"},
		{"proxy_authorization", transportauth.HeaderProxyAuthorization, "Proxy-Authorization"},
		{"serverless_authorization", transportauth.HeaderServerlessAuthorization, "X-Serverless-Authorization"},
	}

	for _, target := range targets {
		for _, m := range modes {
			t.Run(target.name+"-"+m.name, func(t *testing.T) {
				rec := &redirectHopRecorder{}
				handler := func(w http.ResponseWriter, r *http.Request) {
					rec.record(r.Host+r.URL.Path, r)
					if r.Host == "hub.test" && strings.HasPrefix(r.URL.Path, "/api/") {
						http.Redirect(w, r, target.loc, http.StatusFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"phase":"running"}`)
				}
				net := newNamedOriginNet(t, map[string]http.HandlerFunc{
					"hub.test:80":     handler,
					"hub.test:8080":   handler,
					"api.hub.test:80": handler,
					"storage.test:80": handler,
				})
				rt := net.transport()
				t.Cleanup(rt.CloseIdleConnections)

				c := NewClientWithConfig("http://hub.test", "secret-agent-token", "agent-1")
				c.maxRetries = 0
				c.runID = "run-42"
				c.client.Transport = rt
				c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, m.mode)

				_, err := c.GetSelf(context.Background())
				require.NoError(t, err)

				start, ok := rec.headers("hub.test/api/v1/agents/agent-1")
				require.True(t, ok, "initial hub request not recorded")
				assert.Equal(t, "secret-agent-token", start["X-Scion-Agent-Token"])
				assert.Equal(t, "run-42", start["X-Scion-Run-Id"])
				assert.Equal(t, "Bearer secret-transport-token", start[m.header])

				landed, ok := rec.headers(target.hop)
				require.True(t, ok, "redirect to %s was not followed", target.loc)
				assert.Empty(t, sortedHeaderNames(landed),
					"hop to %s leaked credentials: %v", target.loc, landed)
			})
		}
	}
}

// TestClient_SchemeDowngradeRedirectDropsCredentials verifies that an HTTPS ->
// HTTP redirect on the same host is treated as cross-origin and drops every
// agent and transport credential header.
func TestClient_SchemeDowngradeRedirectDropsCredentials(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	httpsRec, httpRec := &redirectHopRecorder{}, &redirectHopRecorder{}
	net := newNamedOriginNet(t, map[string]http.HandlerFunc{
		"hub.test:80": func(w http.ResponseWriter, r *http.Request) {
			httpRec.record(r.Host+r.URL.Path, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"phase":"running"}`)
		},
	})
	tlsHub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpsRec.record(r.Host+r.URL.Path, r)
		http.Redirect(w, r, "http://hub.test/landed", http.StatusFound)
	}))
	t.Cleanup(tlsHub.Close)
	net.addrs["hub.test:443"] = tlsHub.Listener.Addr().String()

	roots := x509.NewCertPool()
	roots.AddCert(tlsHub.Certificate())
	rt := net.transport()
	rt.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com"}
	t.Cleanup(rt.CloseIdleConnections)

	c := NewClientWithConfig("https://hub.test", "secret-agent-token", "agent-1")
	c.maxRetries = 0
	c.runID = "run-42"
	c.client.Transport = rt
	c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, transportauth.HeaderAuthorization)

	_, err := c.GetSelf(context.Background())
	require.NoError(t, err)

	start, ok := httpsRec.headers("hub.test/api/v1/agents/agent-1")
	require.True(t, ok, "https hub request not recorded")
	assert.Equal(t, "Authorization,X-Scion-Agent-Token,X-Scion-Run-Id", sortedHeaderNames(start))

	landed, ok := httpRec.headers("hub.test/landed")
	require.True(t, ok, "redirect to http://hub.test/landed was not followed")
	assert.Empty(t, sortedHeaderNames(landed),
		"scheme-downgrade hop to http://hub.test leaked credentials: %v", landed)
}

// TestClient_SameOriginRedirectKeepsCredentials verifies that a same-origin
// redirect (both relative and explicit default port) preserves the agent
// credential, run ID, and transport credential in all three header modes.
func TestClient_SameOriginRedirectKeepsCredentials(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	locs := []struct {
		name string
		loc  string
	}{
		{"relative", "/landed"},
		{"explicit_port_80", "http://hub.test:80/landed"},
	}
	modes := []struct {
		name   string
		mode   transportauth.HeaderMode
		header string
	}{
		{"authorization", transportauth.HeaderAuthorization, "Authorization"},
		{"proxy_authorization", transportauth.HeaderProxyAuthorization, "Proxy-Authorization"},
		{"serverless_authorization", transportauth.HeaderServerlessAuthorization, "X-Serverless-Authorization"},
	}

	for _, loc := range locs {
		for _, m := range modes {
			t.Run(loc.name+"-"+m.name, func(t *testing.T) {
				rec := &redirectHopRecorder{}
				handler := func(w http.ResponseWriter, r *http.Request) {
					rec.record(r.Host+r.URL.Path, r)
					if strings.HasPrefix(r.URL.Path, "/api/") {
						http.Redirect(w, r, loc.loc, http.StatusTemporaryRedirect)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"phase":"running"}`)
				}
				net := newNamedOriginNet(t, map[string]http.HandlerFunc{
					"hub.test:80": handler,
				})
				rt := net.transport()
				t.Cleanup(rt.CloseIdleConnections)

				c := NewClientWithConfig("http://hub.test", "secret-agent-token", "agent-1")
				c.maxRetries = 0
				c.runID = "run-42"
				c.client.Transport = rt
				c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, m.mode)

				self, err := c.GetSelf(context.Background())
				require.NoError(t, err)
				assert.Equal(t, "running", self.Phase)

				start, ok := rec.headers("hub.test/api/v1/agents/agent-1")
				require.True(t, ok)
				landed, ok := rec.headers("hub.test/landed")
				if !ok {
					landed, ok = rec.headers("hub.test:80/landed")
				}
				require.True(t, ok, "same-origin redirect was not followed")
				assert.Equal(t, "secret-agent-token", landed["X-Scion-Agent-Token"])
				assert.Equal(t, "run-42", landed["X-Scion-Run-Id"])
				assert.Equal(t, "Bearer secret-transport-token", landed[m.header])
				assert.Equal(t, start, landed)
			})
		}
	}
}

// TestClient_AllMethodsDropCredentialsOnCrossOriginRedirect exercises every
// HTTP-sending method on Client against a hub that redirects cross-origin and
// proves none of them leak agent, run-scope, or transport credentials.
func TestClient_AllMethodsDropCredentialsOnCrossOriginRedirect(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)

	cases := []struct {
		name       string
		statusCode int
		respBody   string
		call       func(ctx context.Context, c *Client) error
	}{
		{
			name:       "UpdateStatus",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.UpdateStatus(ctx, StatusUpdate{Status: "running"})
			},
		},
		{
			name:       "ReportSSHHostKey",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.ReportSSHHostKey(ctx, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest")
			},
		},
		{
			name:       "Heartbeat",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.Heartbeat(ctx)
			},
		},
		{
			name:       "ReportState",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.ReportState(ctx, state.PhaseRunning, state.ActivityThinking, "working")
			},
		},
		{
			name:       "ReportMetrics",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.ReportMetrics(ctx, MetricsPayload{
					Type:    "agent_metrics",
					AgentID: "agent-1",
					Session: SessionMetrics{ID: "sess-1"},
				})
			},
		},
		{
			name:       "RegisterPort",
			statusCode: http.StatusCreated,
			respBody:   `{"port":8080,"url":"/p/8080"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.RegisterPort(ctx, RegisterPortRequest{Port: 8080})
				return err
			},
		},
		{
			name:       "ListPorts",
			statusCode: http.StatusOK,
			respBody:   `{"ports":[{"port":8080,"url":"/p/8080"}]}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListPorts(ctx)
				return err
			},
		},
		{
			name:       "DeletePort",
			statusCode: http.StatusNoContent,
			respBody:   ``,
			call: func(ctx context.Context, c *Client) error {
				return c.DeletePort(ctx, 8080)
			},
		},
		{
			name:       "SetSecret",
			statusCode: http.StatusCreated,
			respBody:   `{"key":"K","scope":"project","scopeId":"p1"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.SetSecret(ctx, "K", "dmFs", "environment", "K", "project", true, nil)
				return err
			},
		},
		{
			name:       "GetSecret",
			statusCode: http.StatusOK,
			respBody:   `{"key":"K","value":"dmFs","type":"environment","target":"K"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetSecret(ctx, "K")
				return err
			},
		},
		{
			name:       "ListSecrets",
			statusCode: http.StatusOK,
			respBody:   `{"secrets":[{"key":"K","type":"environment","target":"K"}]}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ListSecrets(ctx)
				return err
			},
		},
		{
			name:       "RefreshToken",
			statusCode: http.StatusOK,
			respBody:   fmt.Sprintf(`{"token":"refreshed-tok","expires_at":%q}`, exp),
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.RefreshToken(ctx)
				return err
			},
		},
		{
			name:       "ExchangeDelegation",
			statusCode: http.StatusOK,
			respBody:   fmt.Sprintf(`{"token":"%sabc","expiresAt":%q,"grantId":"grant-1","credentialId":"cred-1","audience":"scion-hub"}`, DelegatedTokenPrefix, exp),
			call: func(ctx context.Context, c *Client) error {
				_, err := c.ExchangeDelegation(ctx, "grant-1", "scion-hub")
				return err
			},
		},
		{
			name:       "RefreshGitHubToken",
			statusCode: http.StatusOK,
			respBody:   fmt.Sprintf(`{"token":"ghs_test","expires_at":%q}`, exp),
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.RefreshGitHubToken(ctx)
				return err
			},
		},
		{
			name:       "SendSelfMessage",
			statusCode: http.StatusOK,
			respBody:   `{"status":"ok"}`,
			call: func(ctx context.Context, c *Client) error {
				return c.SendSelfMessage(ctx, &messages.StructuredMessage{Msg: "hi"})
			},
		},
		{
			name:       "FetchGCPToken",
			statusCode: http.StatusOK,
			respBody:   `{"access_token":"ya29.test","expires_in":3600,"token_type":"Bearer"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.FetchGCPToken(ctx, []string{"scope-a"})
				return err
			},
		},
		{
			name:       "FetchGCPIdentityToken",
			statusCode: http.StatusOK,
			respBody:   `{"token":"oidc.id.token"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.FetchGCPIdentityToken(ctx, "https://aud.example")
				return err
			},
		},
		{
			name:       "RequestIdentityToken",
			statusCode: http.StatusOK,
			respBody:   fmt.Sprintf(`{"token":"scion.id.token","expires_at":%q}`, exp),
			call: func(ctx context.Context, c *Client) error {
				_, err := c.RequestIdentityToken(ctx, "https://aud.example")
				return err
			},
		},
		{
			name:       "GetSelf",
			statusCode: http.StatusOK,
			respBody:   `{"phase":"running"}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetSelf(ctx)
				return err
			},
		},
		{
			name:       "GetExperiments",
			statusCode: http.StatusOK,
			respBody:   `{"experiments":{"hub.conduit":true}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetExperiments(ctx)
				return err
			},
		},
		{
			name:       "FetchSecrets",
			statusCode: http.StatusOK,
			respBody:   `{"secrets":[{"key":"K","value":"V","status":"ok"}]}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.FetchSecrets(ctx, []string{"K"})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetTokenHome(t.TempDir()))
			rec := &redirectHopRecorder{}

			evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record("evil"+r.URL.Path, r)
				if tc.statusCode != http.StatusNoContent {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.statusCode)
				if tc.respBody != "" {
					_, _ = io.WriteString(w, tc.respBody)
				}
			}))
			t.Cleanup(evilSrv.Close)

			var hubSawPath string
			var hubMu sync.Mutex
			hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hubMu.Lock()
				hubSawPath = r.URL.Path
				hubMu.Unlock()
				rec.record("hub"+r.URL.Path, r)
				http.Redirect(w, r, evilSrv.URL+"/landed", http.StatusFound)
			}))
			t.Cleanup(hubSrv.Close)

			c := NewClientWithConfig(hubSrv.URL, "secret-agent-token", "agent-1")
			c.maxRetries = 0
			c.runID = "run-42"
			c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, transportauth.HeaderAuthorization)

			err := tc.call(context.Background(), c)
			require.NoError(t, err)

			hubMu.Lock()
			path := hubSawPath
			hubMu.Unlock()
			require.NotEmpty(t, path, "hub did not receive request")

			hubHeaders, ok := rec.headers("hub" + path)
			require.True(t, ok)
			assert.Equal(t, "secret-agent-token", hubHeaders["X-Scion-Agent-Token"])
			assert.Equal(t, "run-42", hubHeaders["X-Scion-Run-Id"])
			assert.Equal(t, "Bearer secret-transport-token", hubHeaders["Authorization"])

			evilHeaders, ok := rec.headers("evil/landed")
			require.True(t, ok, "redirect target was not reached for %s", tc.name)
			assert.Empty(t, sortedHeaderNames(evilHeaders),
				"%s leaked credentials to cross-origin redirect target: %v", tc.name, evilHeaders)
		})
	}
}

// TestClient_StructLiteralStillAppliesOriginRule ensures that even a Client
// constructed directly as a struct literal in tests or internal helpers
// applies the hub origin rule when sending requests.
func TestClient_StructLiteralStillAppliesOriginRule(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	rec := &redirectHopRecorder{}
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("evil"+r.URL.Path, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"phase":"running"}`)
	}))
	t.Cleanup(evilSrv.Close)

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record("hub"+r.URL.Path, r)
		http.Redirect(w, r, evilSrv.URL+"/landed", http.StatusFound)
	}))
	t.Cleanup(hubSrv.Close)

	c := &Client{
		hubURL:  hubSrv.URL,
		token:   "secret-agent-token",
		agentID: "agent-1",
		runID:   "run-42",
		client: &http.Client{
			Timeout:   DefaultTimeout,
			Transport: transportauth.Wrap(nil, staticTransportSource{tok: "secret-transport-token"}, transportauth.HeaderAuthorization),
		},
	}

	_, err := c.GetSelf(context.Background())
	require.NoError(t, err)

	evilHeaders, ok := rec.headers("evil/landed")
	require.True(t, ok)
	assert.Empty(t, sortedHeaderNames(evilHeaders),
		"struct-literal Client leaked credentials on cross-origin redirect: %v", evilHeaders)
}

// TestClient_HTTPClientAndSubCallerOverrides verifies:
//  1. Callers using c.HTTPClient() directly without overriding CheckRedirect
//     inherit the hub origin rule.
//  2. Sub-callers that copy *c.HTTPClient() and install their own stricter
//     CheckRedirect (such as CheckCredentialAccepted, conduit RefreshKeys, and
//     the delegation proxy) continue to stop at the 3xx without following it,
//     while still sending the transport credential to the hub on hop 1.
func TestClient_HTTPClientAndSubCallerOverrides(t *testing.T) {
	scrubHubEnv(t)
	t.Cleanup(SetTokenHome(t.TempDir()))

	t.Run("direct_HTTPClient_drops_cross_origin_credentials", func(t *testing.T) {
		rec := &redirectHopRecorder{}
		otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record("other"+r.URL.Path, r)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(otherSrv.Close)

		hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record("hub"+r.URL.Path, r)
			http.Redirect(w, r, otherSrv.URL+"/landed", http.StatusFound)
		}))
		t.Cleanup(hubSrv.Close)

		c := NewClientWithConfig(hubSrv.URL, "secret-agent-token", "agent-1")
		c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, transportauth.HeaderProxyAuthorization)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, hubSrv.URL+"/probe", nil)
		require.NoError(t, err)
		req.Header.Set("X-Scion-Agent-Token", c.AuthToken())
		req.Header.Set("X-Custom", "kept")

		resp, err := c.HTTPClient().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()

		hubHeaders, ok := rec.headers("hub/probe")
		require.True(t, ok)
		assert.Equal(t, "Proxy-Authorization,X-Custom,X-Scion-Agent-Token", sortedHeaderNames(hubHeaders))

		otherHeaders, ok := rec.headers("other/landed")
		require.True(t, ok)
		assert.Equal(t, "X-Custom", sortedHeaderNames(otherHeaders))
	})

	t.Run("subcaller_CheckRedirect_override_stops_at_first_hop", func(t *testing.T) {
		rec := &redirectHopRecorder{}
		otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record("other"+r.URL.Path, r)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(otherSrv.Close)

		hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.record("hub"+r.URL.Path, r)
			http.Redirect(w, r, otherSrv.URL+"/landed", http.StatusFound)
		}))
		t.Cleanup(hubSrv.Close)

		c := NewClientWithConfig(hubSrv.URL, "secret-agent-token", "agent-1")
		c.runID = "run-42"
		c.SetTransportAuth(staticTransportSource{tok: "secret-transport-token"}, transportauth.HeaderAuthorization)

		// CheckCredentialAccepted uses c.directClient() with ErrUseLastResponse.
		err := c.CheckCredentialAccepted(context.Background(), c.AuthToken())
		assert.ErrorIs(t, err, ErrCandidateUnchecked, "302 redirect must not count as credential acceptance")

		hubHeaders, ok := rec.headers("hub/api/v1/agents/agent-1")
		require.True(t, ok)
		assert.Equal(t, "Bearer secret-transport-token", hubHeaders["Authorization"])
		assert.Equal(t, "secret-agent-token", hubHeaders["X-Scion-Agent-Token"])
		assert.Equal(t, "run-42", hubHeaders["X-Scion-Run-Id"])

		_, redirected := rec.headers("other/landed")
		assert.False(t, redirected, "CheckCredentialAccepted must not follow redirect")

		// Sub-caller copy pattern used by conduit/keys.go and delegation/proxy.go.
		sub := *c.HTTPClient()
		sub.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, hubSrv.URL+"/sub", nil)
		require.NoError(t, err)
		resp, err := sub.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode)

		subHubHeaders, ok := rec.headers("hub/sub")
		require.True(t, ok)
		assert.Equal(t, "Bearer secret-transport-token", subHubHeaders["Authorization"])
		_, redirected = rec.headers("other/landed")
		assert.False(t, redirected, "sub-caller CheckRedirect override must not follow redirect")
	})
}
