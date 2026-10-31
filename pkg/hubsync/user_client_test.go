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

package hubsync

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Placeholder credential values. Each is distinct, so a header seen by the
// counting hub names the credential that was sent.
const (
	tpOAuthToken     = "tp-oauth-access-token"
	tpHubToken       = "tp-hub-token-pat"
	tpAgentToken     = "tp-agent-identity-token"
	tpTransportToken = "tp-transport-id-token"
	tpUserinfoSecret = "tp-userinfo-secret"
)

// countingHub records every request it receives with its credential
// headers, and answers the health and project calls with 200.
type countingHub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []http.Header
}

func newCountingHub(t *testing.T) *countingHub {
	t.Helper()
	h := &countingHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.reqs = append(h.reqs, r.Header.Clone())
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *countingHub) requests() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]http.Header(nil), h.reqs...)
}

// routeDialsTo makes http.DefaultTransport, which hub clients use when no
// transport is set, connect every dial to addr whatever host the URL names.
// A request to a non-loopback URL then reaches the counting hub, so a
// missing transport check shows as a recorded request, not a dial error.
func routeDialsTo(t *testing.T, addr string) {
	t.Helper()
	orig := http.DefaultTransport
	tr := orig.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// clearTransportPolicyEnv clears every hub credential and transport
// credential source and returns a fresh HOME.
func clearTransportPolicyEnv(t *testing.T) string {
	t.Helper()
	home := clearHubCredentialEnv(t)
	for _, name := range []string{
		"SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_TOKEN_FILE", "SCION_TRANSPORT_MODE",
		"SCION_TRANSPORT_AUDIENCE", "SCION_HUB_OIDC_AUDIENCE",
	} {
		t.Setenv(name, "")
	}
	return home
}

// setTransportCredential configures a transport (IAP) credential from the
// environment, which hubclient.New adds on its own.
func setTransportCredential(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_TRANSPORT_MODE", "iap")
	t.Setenv("SCION_TRANSPORT_TOKEN", tpTransportToken)
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", filepath.Join(t.TempDir(), "absent-transport-token"))
}

func writeAgentTokenFile(t *testing.T, home, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertNoRequests fails when any request reached the hub.
func assertNoRequests(t *testing.T, hub *countingHub) {
	t.Helper()
	if reqs := hub.requests(); len(reqs) != 0 {
		t.Fatalf("%d request(s) reached the hub (first headers: %v); want none", len(reqs), redactHeaders(reqs[0]))
	}
}

// redactHeaders lists the credential header names present, not values.
func redactHeaders(h http.Header) []string {
	var names []string
	for _, k := range []string{"Authorization", "Proxy-Authorization", "X-Scion-Agent-Token"} {
		if h.Get(k) != "" {
			names = append(names, k)
		}
	}
	return names
}

var refusedEndpoints = []string{
	"http://10.0.0.1:9810",
	"http://hub.example.com",
	"ws://hub.example.com",
	"http://[2001:db8::1]:9810",
	"http://host.docker.internal:9810",
	"http://user:" + tpUserinfoSecret + "@10.0.0.1:9810",
}

func TestNewUserHubClient_RefusesPlaintextNonLoopbackBeforeAnyRequest(t *testing.T) {
	for _, endpoint := range refusedEndpoints {
		t.Run(endpoint, func(t *testing.T) {
			clearTransportPolicyEnv(t)
			setTransportCredential(t)
			hub := newCountingHub(t)
			routeDialsTo(t, hub.Listener.Addr().String())

			c, err := NewUserHubClient(endpoint, hubclient.WithBearerToken(tpHubToken), hubclient.WithTimeout(5*time.Second))
			if err == nil {
				// Without the rule a client is built; prove it would send.
				_, _ = c.Health(context.Background())
				t.Fatalf("NewUserHubClient(%s) built a client", endpoint)
			}
			if !errors.Is(err, config.ErrPlaintextNonLoopback) {
				t.Fatalf("error %v does not wrap ErrPlaintextNonLoopback", err)
			}
			if strings.Contains(err.Error(), tpUserinfoSecret) {
				t.Fatalf("refusal echoes URL user info: %v", err)
			}
			assertNoRequests(t, hub)
		})
	}
}

func TestNewUserHubClient_LoopbackHTTPAndHTTPSAllowed(t *testing.T) {
	clearTransportPolicyEnv(t)
	hub := newCountingHub(t)
	for _, endpoint := range []string{hub.URL, strings.Replace(hub.URL, "127.0.0.1", "localhost", 1)} {
		c, err := NewUserHubClient(endpoint, hubclient.WithBearerToken(tpHubToken), hubclient.WithTimeout(5*time.Second))
		if err != nil {
			t.Fatalf("NewUserHubClient(%s): %v", endpoint, err)
		}
		if _, err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health(%s): %v", endpoint, err)
		}
	}
	if n := len(hub.requests()); n != 2 {
		t.Fatalf("loopback requests = %d, want 2", n)
	}

	tlsHub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(tlsHub.Close)
	c, err := NewUserHubClient(tlsHub.URL, hubclient.WithHTTPClient(tlsHub.Client()), hubclient.WithBearerToken(tpHubToken))
	if err != nil {
		t.Fatalf("NewUserHubClient(https): %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health(https): %v", err)
	}
}

// agentEnv sets up a hub-managed agent: SCION_AGENT_ID, the injected
// endpoint and an agent token in the token file.
func agentEnv(t *testing.T, injected string) string {
	t.Helper()
	home := clearTransportPolicyEnv(t)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HUB_ENDPOINT", injected)
	writeAgentTokenFile(t, home, tpAgentToken)
	return home
}

func TestNewInjectedAgentHubClient_AllConditionsSendsOnlyAgentToken(t *testing.T) {
	for _, variable := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		for _, viaEnv := range []bool{false, true} {
			name := variable + "/token-file"
			if viaEnv {
				name = variable + "/SCION_AUTH_TOKEN"
			}
			t.Run(name, func(t *testing.T) {
				const injected = "http://host.docker.internal:9810"
				home := agentEnv(t, "")
				t.Setenv(variable, injected)
				if viaEnv {
					_ = os.Remove(filepath.Join(home, ".scion", "scion-token"))
					t.Setenv("SCION_AUTH_TOKEN", tpAgentToken)
				}
				setTransportCredential(t)
				hub := newCountingHub(t)
				routeDialsTo(t, hub.Listener.Addr().String())

				c, ok := NewInjectedAgentHubClient(injected, 5*time.Second)
				if !ok {
					t.Fatal("NewInjectedAgentHubClient refused the broker-injected endpoint and agent token")
				}
				if _, err := c.Health(context.Background()); err != nil {
					t.Fatalf("Health: %v", err)
				}
				reqs := hub.requests()
				if len(reqs) != 1 {
					t.Fatalf("requests = %d, want 1", len(reqs))
				}
				if got := reqs[0].Get("X-Scion-Agent-Token"); got != tpAgentToken {
					t.Errorf("agent token header = %q, want the injected agent token", got)
				}
				if names := redactHeaders(reqs[0]); len(names) != 1 {
					t.Errorf("credential headers sent = %v, want only X-Scion-Agent-Token (no transport credential)", names)
				}
			})
		}
	}
}

func TestNewInjectedAgentHubClient_EachConditionRequired(t *testing.T) {
	const injected = "http://host.docker.internal:9810"
	tests := []struct {
		name     string
		setup    func(t *testing.T, home string)
		endpoint string
	}{
		{
			name:     "not a hub-managed agent",
			setup:    func(t *testing.T, _ string) { t.Setenv("SCION_AGENT_ID", "") },
			endpoint: injected,
		},
		{
			name: "no agent token in the injected source",
			setup: func(t *testing.T, home string) {
				_ = os.Remove(filepath.Join(home, ".scion", "scion-token"))
				t.Setenv("SCION_AUTH_TOKEN", "")
				// A user credential is present but must not qualify.
				t.Setenv("SCION_HUB_TOKEN", tpHubToken)
			},
			endpoint: injected,
		},
		{
			name:     "another bridge spelling of the injected host",
			endpoint: "http://host.docker.internal:9811",
		},
		{
			name:     "another bridge host",
			endpoint: "http://host.containers.internal:9810",
		},
		{
			name:     "trailing slash is not the injected value",
			endpoint: injected + "/",
		},
		{
			name:     "an arbitrary non-loopback host",
			endpoint: "http://10.0.0.1:9810",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := agentEnv(t, injected)
			if tc.setup != nil {
				tc.setup(t, home)
			}
			hub := newCountingHub(t)
			routeDialsTo(t, hub.Listener.Addr().String())

			if c, ok := NewInjectedAgentHubClient(tc.endpoint, 5*time.Second); ok {
				_, _ = c.Health(context.Background())
				t.Fatalf("NewInjectedAgentHubClient(%s) allowed it", tc.endpoint)
			}
			assertNoRequests(t, hub)
		})
	}
}

func TestNewInjectedAgentHubClient_NotUsedWhenRuleAccepts(t *testing.T) {
	// An https or loopback injected endpoint goes through NewUserHubClient
	// and keeps the transport credential; the exception never applies.
	for _, endpoint := range []string{"https://hub.example.com", "http://127.0.0.1:9810"} {
		agentEnv(t, endpoint)
		if _, ok := NewInjectedAgentHubClient(endpoint, time.Second); ok {
			t.Errorf("NewInjectedAgentHubClient(%s) applied to an endpoint the rule accepts", endpoint)
		}
	}
}

// TestCreateHubClient_UserCredentialsGatedInsideAgent covers the CLI's main
// client path (createHubClient, behind EnsureHubReady): inside a hub-managed
// agent, with the injected plaintext endpoint, a USER credential is still
// refused before any request; only the injected agent token passes.
func TestCreateHubClient_UserCredentialsGatedInsideAgent(t *testing.T) {
	const injected = "http://host.docker.internal:9810"
	tests := []struct {
		name    string
		setup   func(t *testing.T, home string)
		allowed bool
	}{
		{name: "agent token", allowed: true},
		{
			name: "OAuth login",
			setup: func(t *testing.T, _ string) {
				if err := credentials.Store(injected, &credentials.TokenResponse{AccessToken: tpOAuthToken, ExpiresIn: time.Hour}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "SCION_HUB_TOKEN",
			setup: func(t *testing.T, home string) {
				_ = os.Remove(filepath.Join(home, ".scion", "scion-token"))
				t.Setenv("SCION_HUB_TOKEN", tpHubToken)
			},
		},
		{
			name: "transport credential only",
			setup: func(t *testing.T, home string) {
				_ = os.Remove(filepath.Join(home, ".scion", "scion-token"))
				setTransportCredential(t)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := agentEnv(t, injected)
			if tc.setup != nil {
				tc.setup(t, home)
			}
			hub := newCountingHub(t)
			routeDialsTo(t, hub.Listener.Addr().String())

			c, _, err := createHubClient(nil, injected)
			if !tc.allowed {
				if err == nil {
					_, _ = c.Health(context.Background())
					t.Fatal("createHubClient built a client for a user credential")
				}
				if !errors.Is(err, config.ErrPlaintextNonLoopback) {
					t.Fatalf("error %v does not wrap ErrPlaintextNonLoopback", err)
				}
				assertNoRequests(t, hub)
				return
			}
			if err != nil {
				t.Fatalf("createHubClient: %v", err)
			}
			if _, err := c.Health(context.Background()); err != nil {
				t.Fatalf("Health: %v", err)
			}
			if reqs := hub.requests(); len(reqs) != 1 || reqs[0].Get("X-Scion-Agent-Token") != tpAgentToken {
				t.Fatalf("requests = %d; want 1 with the agent token", len(reqs))
			}
		})
	}
}

func TestEnsureHubReady_PlaintextNonLoopbackRefusedBeforeAnyRequest(t *testing.T) {
	home := clearTransportPolicyEnv(t)
	t.Chdir(home)
	t.Setenv("SCION_HUB_TOKEN", tpHubToken)
	hub := newCountingHub(t)
	routeDialsTo(t, hub.Listener.Addr().String())

	_, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: "http://hub.example.com:9810", AutoConfirm: true})
	if err == nil || !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("EnsureHubReady error = %v, want the transport refusal", err)
	}
	if strings.Contains(err.Error(), "scion hub status") {
		t.Errorf("refusal carries the hub-check hint: %v", err)
	}
	assertNoRequests(t, hub)
}

// TestNewInjectedAgentHubClient_InjectedHostMustBeHostAddress checks the
// host-class condition: even when it is exactly the injected value, an
// endpoint qualifies only when its host is a host alias or an IPv4
// link-local address (loopback is accepted by the rule itself).
func TestNewInjectedAgentHubClient_InjectedHostMustBeHostAddress(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"http://host.docker.internal:9810":      true,
		"http://HOST.CONTAINERS.INTERNAL:9810":  true,
		"http://scion-hub.internal:9810":        true,
		"http://169.254.169.2:8080":             true,
		"http://10.0.0.1:9810":                  false,
		"http://hub.example.com":                false,
		"http://scion-hub.internal.example.com": false,
		"http://[fe80::1]:9810":                 false,
		"http://192.168.1.10:9810":              false,
	} {
		t.Run(endpoint, func(t *testing.T) {
			agentEnv(t, endpoint)
			hub := newCountingHub(t)
			routeDialsTo(t, hub.Listener.Addr().String())

			c, ok := NewInjectedAgentHubClient(endpoint, 5*time.Second)
			if ok != want {
				t.Fatalf("NewInjectedAgentHubClient(%s) ok = %v, want %v", endpoint, ok, want)
			}
			if !ok {
				assertNoRequests(t, hub)
				return
			}
			if _, err := c.Health(context.Background()); err != nil {
				t.Fatalf("Health: %v", err)
			}
		})
	}
}

// The refusal carries the endpoint it refused, redacted (for the caller's origin
// label), and never shows its user info.
func TestCheckUserHubTransport_RefusalCarriesEndpoint(t *testing.T) {
	endpoint := "http://user:" + tpUserinfoSecret + "@10.0.0.1:9810"
	err := CheckUserHubTransport(endpoint)
	var refused *TransportRefusalError
	if !errors.As(err, &refused) {
		t.Fatalf("error %v is not a *TransportRefusalError", err)
	}
	if refused.Endpoint != config.RedactURL(endpoint) {
		t.Errorf("Endpoint = %q, want the refused endpoint redacted", refused.Endpoint)
	}
	if strings.Contains(refused.Endpoint, tpUserinfoSecret) {
		t.Errorf("Endpoint %q holds the URL's user info", refused.Endpoint)
	}
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("error %v does not wrap ErrPlaintextNonLoopback", err)
	}
	if strings.Contains(err.Error(), tpUserinfoSecret) {
		t.Errorf("refusal echoes URL user info: %v", err)
	}
	if err := CheckUserHubTransport("https://hub.example.com"); err != nil {
		t.Errorf("https endpoint refused: %v", err)
	}
}
