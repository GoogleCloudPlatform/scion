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

package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
)

// Placeholder credential values; each is distinct, so a header the counting
// hub sees names the credential that was sent.
const (
	htpOAuthToken     = "htp-oauth-access-token"
	htpHubToken       = "htp-hub-token-pat"
	htpAgentToken     = "htp-agent-identity-token"
	htpTransportToken = "htp-transport-id-token"
	htpUserinfoSecret = "htp-userinfo-secret"
	htpInjected       = "http://host.docker.internal:9810"
	htpRefused        = "http://hub.example.com:9810"
)

// htpHub records every request it receives, with its credential headers.
type htpHub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []http.Header
}

func newHTPHub(t *testing.T) *htpHub {
	t.Helper()
	h := &htpHub{}
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

func (h *htpHub) requests() []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]http.Header(nil), h.reqs...)
}

// credentialHeaders names the credential headers present in h (no values).
func credentialHeaders(h http.Header) []string {
	var names []string
	for _, k := range []string{"Authorization", "Proxy-Authorization", "X-Scion-Agent-Token"} {
		if h.Get(k) != "" {
			names = append(names, k)
		}
	}
	return names
}

func (h *htpHub) assertNoRequests(t *testing.T) {
	t.Helper()
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("%d request(s) reached the hub (credential headers %v); want none", len(reqs), credentialHeaders(reqs[0]))
	}
}

// htpRouteDials makes http.DefaultTransport connect every dial to the
// counting hub, whatever host the URL names, so a request to a refused URL
// would arrive there (a missing check shows as a recorded request, not as a
// dial error).
func htpRouteDials(t *testing.T, hub *htpHub) {
	t.Helper()
	addr := hub.Listener.Addr().String()
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

// htpEnv clears every credential, transport and hub-selection source and
// returns a fresh HOME. With user set, an OAuth login for endpoint, a
// SCION_HUB_TOKEN and a transport (IAP) credential are all present.
func htpEnv(t *testing.T, endpoint string, user bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	credPath := filepath.Join(home, "credentials.json")
	origPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string { return credPath })
	t.Cleanup(func() { credentials.SetCredentialsPath(origPath) })
	for _, name := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_AGENT_ID", "SCION_HUB_TOKEN", "SCION_AUTH_TOKEN",
		"SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HUB_PROFILE",
		"SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_MODE", "SCION_TRANSPORT_AUDIENCE", "SCION_HUB_OIDC_AUDIENCE",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", filepath.Join(home, "absent-transport-token"))
	savedEndpoint, savedSource := hubEndpoint, hubSource
	hubEndpoint, hubSource = "", ""
	t.Cleanup(func() { hubEndpoint, hubSource = savedEndpoint, savedSource })
	if user {
		if err := credentials.Store(endpoint, &credentials.TokenResponse{AccessToken: htpOAuthToken, ExpiresIn: time.Hour}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		t.Setenv("SCION_TRANSPORT_MODE", "iap")
		t.Setenv("SCION_TRANSPORT_TOKEN", htpTransportToken)
	}
	return home
}

func assertTransportRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error; want the transport refusal")
	}
	if !errors.Is(err, config.ErrPlaintextNonLoopback) && !strings.Contains(err.Error(), config.ErrPlaintextNonLoopback.Error()) {
		t.Fatalf("error %v is not the transport refusal", err)
	}
	if strings.Contains(err.Error(), htpUserinfoSecret) {
		t.Fatalf("refusal echoes URL user info: %v", err)
	}
}

var htpRefusedEndpoints = []string{
	htpRefused,
	"http://10.0.0.1:9810",
	"http://[2001:db8::1]:9810",
	htpInjected,
	"http://user:" + htpUserinfoSecret + "@10.0.0.1:9810",
}

// TestUserHubPaths_RefusePlaintextNonLoopbackBeforeAnyRequest runs every CLI
// path that sends (or receives) a user credential against a refused URL,
// with every user credential present, and checks that nothing reached the
// hub and that a hub client was never built.
func TestUserHubPaths_RefusePlaintextNonLoopbackBeforeAnyRequest(t *testing.T) {
	paths := map[string]func(t *testing.T, endpoint string) error{
		"P1 getHubClient (hub.go)": func(t *testing.T, endpoint string) error {
			hubEndpoint, hubSource = endpoint, "" // --hub flag
			c, err := getHubClient(nil)
			if err == nil {
				_, _ = c.Health(context.Background())
			}
			return err
		},
		"P1 hubClientForEndpoint": func(t *testing.T, endpoint string) error {
			c, err := hubClientForEndpoint(nil, endpoint)
			if err == nil {
				_, _ = c.Health(context.Background())
			}
			return err
		},
		"P2 requireHub (hubsync.EnsureHubReady)": func(t *testing.T, endpoint string) error {
			hubEndpoint, hubSource = endpoint, "" // --hub flag
			saved := autoConfirm
			autoConfirm = true
			t.Cleanup(func() { autoConfirm = saved })
			t.Chdir(t.TempDir())
			_, err := requireHubWith("", true)
			return err
		},
		"P3 hub auth login flow": func(t *testing.T, endpoint string) error {
			saved := hubAuthNoBrowser
			hubAuthNoBrowser = true
			t.Cleanup(func() { hubAuthNoBrowser = saved })
			c := &cobra.Command{}
			c.SetContext(context.Background())
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			_, err := hubAuthLoginFlow(c, endpoint, "google")
			return err
		},
		"P4 hub health check (hub add/use)": func(t *testing.T, endpoint string) error {
			return hubResponds(endpoint, false)
		},
		"P4 hub health check fan-out (hub list --check)": func(t *testing.T, endpoint string) error {
			return hubResponds(endpoint, true)
		},
		"P4 hub credential verification": func(t *testing.T, endpoint string) error {
			// hubAccepts reports no error, only ok; the error below is made
			// here. What kills this case is assertNoRequests (zero requests).
			if _, ok := hubAccepts(endpoint, htpHubToken, false); ok {
				return nil
			}
			return config.ErrPlaintextNonLoopback
		},
		"P5 hub fan-out client": func(t *testing.T, endpoint string) error {
			c, err := hubClientFor(nil, endpoint, true)
			if err == nil {
				_, _ = c.Health(context.Background())
			}
			return err
		},
		"P6 completion": func(t *testing.T, endpoint string) error {
			_, err := fetchHubAgents(context.Background(), endpoint, &config.Settings{})
			return err
		},
		"P7 tunnel client config (tunnel, ssh, vscode)": func(t *testing.T, endpoint string) error {
			_, _, err := tunnelClientConfig(endpoint, io.Discard)
			return err
		},
		"P7 tunnel REST client": func(t *testing.T, endpoint string) error {
			h := &tunnelHub{endpoint: endpoint}
			c, err := h.client(tunnelclient.Credential{Token: htpHubToken, Kind: tunnelclient.CredentialToken})
			if err == nil {
				_, _ = c.Health(context.Background())
			}
			return err
		},
		"P8 ssh hub connection": func(t *testing.T, endpoint string) error {
			_, err := newSSHHubConn(endpoint, htpHubToken)
			return err
		},
		"P8 ssh hub resolution": func(t *testing.T, endpoint string) error {
			_, err := resolveSSHHub(&sshCommandEnv{home: t.TempDir(), endpoint: endpoint, token: htpHubToken})
			return err
		},
		"P8 ssh command env reads no credential": func(t *testing.T, endpoint string) error {
			// The env builder returns no error for a refused hub; the check is
			// that no credential was read (t.Fatal below) and no request was
			// sent (assertNoRequests). The error returned is made here.
			hubEndpoint, hubSource = endpoint, "" // --hub flag
			env, err := defaultSSHCommandEnv()
			if err != nil {
				return err
			}
			if env.token != "" {
				t.Fatal("defaultSSHCommandEnv read a credential for a refused hub")
			}
			return config.ErrPlaintextNonLoopback
		},
		"P9 attach": func(t *testing.T, endpoint string) error {
			origTransport, origAttach := resolveAttachTransportFn, attachToAgentFn
			t.Cleanup(func() { resolveAttachTransportFn, attachToAgentFn = origTransport, origAttach })
			resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
				t.Fatal("attach resolved the transport credential for a refused hub")
				return nil, 0, nil
			}
			attachToAgentFn = func(context.Context, string, string, string, ...wsclient.AttachOption) error {
				t.Fatal("attach dialed a refused hub")
				return nil
			}
			return attachHubSession(context.Background(), &HubContext{Endpoint: endpoint}, hubAttachTarget{Name: "a", ID: "a"})
		},
		"P10 onboarding status": func(t *testing.T, endpoint string) error {
			// quickstartStatusAllowed returns a bool; the error is made here,
			// and assertNoRequests checks that nothing was sent.
			if quickstartStatusAllowed(endpoint) {
				return nil
			}
			return config.ErrPlaintextNonLoopback
		},
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, endpoint := range htpRefusedEndpoints {
			t.Run(name+"/"+endpoint, func(t *testing.T) {
				htpEnv(t, endpoint, true)
				hub := newHTPHub(t)
				htpRouteDials(t, hub)
				err := paths[name](t, endpoint)
				if strings.Contains(endpoint, "@") && err != nil && !errors.Is(err, config.ErrPlaintextNonLoopback) {
					// Some paths refuse URL user info before the transport
					// rule; that refusal also sends nothing. It must not
					// echo the user info either.
					if strings.Contains(err.Error(), htpUserinfoSecret) {
						t.Fatalf("refusal echoes URL user info: %v", err)
					}
				} else {
					assertTransportRefusal(t, err)
				}
				hub.assertNoRequests(t)
			})
		}
	}
}

// TestUserHubPaths_LoopbackAndHTTPSStillWork checks the controls: the same
// clients reach a loopback http hub and an https hub.
func TestUserHubPaths_LoopbackAndHTTPSStillWork(t *testing.T) {
	hub := newHTPHub(t)
	for _, endpoint := range []string{hub.URL, strings.Replace(hub.URL, "127.0.0.1", "localhost", 1)} {
		htpEnv(t, endpoint, false)
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		c, err := hubClientForEndpoint(nil, endpoint)
		if err != nil {
			t.Fatalf("hubClientForEndpoint(%s): %v", endpoint, err)
		}
		if _, err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health(%s): %v", endpoint, err)
		}
		if err := hubResponds(endpoint, false); err != nil {
			t.Fatalf("hubResponds(%s): %v", endpoint, err)
		}
		if _, _, err := tunnelClientConfig(endpoint, io.Discard); err != nil {
			t.Fatalf("tunnelClientConfig(%s): %v", endpoint, err)
		}
		if !quickstartStatusAllowed(endpoint) {
			t.Fatalf("quickstartStatusAllowed(%s) = false", endpoint)
		}
	}
	if n := len(hub.requests()); n != 4 {
		t.Fatalf("loopback requests = %d, want 4", n)
	}

	for _, endpoint := range []string{"https://hub.example.com", "https://hub.example.com/scion"} {
		htpEnv(t, endpoint, false)
		if _, err := hubClientForEndpoint(nil, endpoint); err != nil {
			t.Fatalf("hubClientForEndpoint(%s): %v", endpoint, err)
		}
		if _, _, err := tunnelClientConfig(endpoint, io.Discard); err != nil {
			t.Fatalf("tunnelClientConfig(%s): %v", endpoint, err)
		}
		if _, err := newSSHHubConn(endpoint, htpHubToken); err != nil {
			t.Fatalf("newSSHHubConn(%s): %v", endpoint, err)
		}
	}
}

// TestHubTransportRefusal_NamesTheSetting checks that the refusal names
// where the endpoint came from.
func TestHubTransportRefusal_NamesTheSetting(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		htpEnv(t, htpRefused, false)
		t.Setenv("SCION_HUB_ENDPOINT", htpRefused)
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		_, err := getHubClient(nil)
		assertTransportRefusal(t, err)
		if !strings.Contains(err.Error(), "hub endpoint from SCION_HUB_ENDPOINT") {
			t.Errorf("refusal %q does not name SCION_HUB_ENDPOINT", err)
		}
	})
	t.Run("environment with user info", func(t *testing.T) {
		// The refusal carries the endpoint redacted; the origin still matches.
		withInfo := "http://user:" + htpUserinfoSecret + "@10.0.0.1:9810"
		htpEnv(t, withInfo, false)
		t.Setenv("SCION_HUB_ENDPOINT", withInfo)
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		_, err := getHubClient(nil)
		assertTransportRefusal(t, err)
		if !strings.Contains(err.Error(), "hub endpoint from SCION_HUB_ENDPOINT") {
			t.Errorf("refusal %q does not name SCION_HUB_ENDPOINT", err)
		}
	})
	t.Run("flag", func(t *testing.T) {
		htpEnv(t, htpRefused, false)
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		hubEndpoint, hubSource = htpRefused, "" // --hub flag
		_, err := getHubClient(nil)
		assertTransportRefusal(t, err)
		if !strings.Contains(err.Error(), "hub endpoint from --hub flag") {
			t.Errorf("refusal %q does not name the --hub flag", err)
		}
	})
}

// The origin names the endpoint the refusal carries, not the one the caller
// passed (EnsureHubReady may refuse another endpoint than the invocation hub).
func TestHubTransportRefusal_NamesTheRefusedEndpoint(t *testing.T) {
	htpEnv(t, htpRefused, false)
	t.Setenv("SCION_HUB_ENDPOINT", htpRefused)
	refusal := hubsync.CheckUserHubTransport(htpRefused)
	err := hubTransportRefusal("http://other.example.com:1", refusal)
	assertTransportRefusal(t, err)
	if !strings.Contains(err.Error(), "hub endpoint from SCION_HUB_ENDPOINT") {
		t.Errorf("refusal %q does not name SCION_HUB_ENDPOINT", err)
	}
}

// htpAgentEnv sets up a hub-managed agent: SCION_AGENT_ID, the injected
// endpoint and an agent token in the token file, and no user credential.
func htpAgentEnv(t *testing.T) string {
	t.Helper()
	home := htpEnv(t, htpInjected, false)
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_HUB_ENDPOINT", htpInjected)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(htpAgentToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestInjectedAgentTokenException covers the one exception and its
// conditions: allowed only for a hub-managed agent AND the broker-injected
// agent-token source AND exactly the injected endpoint AND a host alias or
// link-local host;
// a user credential is refused in every mode, including inside an agent.
func TestInjectedAgentTokenException(t *testing.T) {
	t.Run("all conditions: only the agent token is sent", func(t *testing.T) {
		htpAgentEnv(t)
		t.Setenv("SCION_TRANSPORT_MODE", "iap")
		t.Setenv("SCION_TRANSPORT_TOKEN", htpTransportToken)
		hub := newHTPHub(t)
		htpRouteDials(t, hub)
		for _, run := range []func() error{
			func() error { // P1 through getHubClient (the invocation hub)
				c, err := getHubClient(nil)
				if err != nil {
					return err
				}
				_, err = c.Health(context.Background())
				return err
			},
			func() error { // P1 for an explicit endpoint (hubClientAndAuthSourceForEndpoint); the
				// P2 positive case (createHubClient) is pkg/hubsync's
				// TestCreateHubClient_UserCredentialsGatedInsideAgent "agent token" case.
				c, _, err := hubClientAndAuthSourceForEndpoint(nil, htpInjected)
				if err != nil {
					return err
				}
				_, err = c.Health(context.Background())
				return err
			},
		} {
			if err := run(); err != nil {
				t.Fatalf("agent token to the injected endpoint: %v", err)
			}
		}
		reqs := hub.requests()
		if len(reqs) != 2 {
			t.Fatalf("requests = %d, want 2", len(reqs))
		}
		for _, r := range reqs {
			if names := credentialHeaders(r); len(names) != 1 || r.Get("X-Scion-Agent-Token") != htpAgentToken {
				t.Fatalf("credential headers %v; want only the injected agent token", names)
			}
		}
	})

	negatives := map[string]func(t *testing.T) string{
		"not a hub-managed agent": func(t *testing.T) string {
			t.Setenv("SCION_AGENT_ID", "")
			return htpInjected
		},
		"no broker-injected agent token (SCION_HUB_TOKEN only)": func(t *testing.T) string {
			_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".scion", "scion-token"))
			t.Setenv("SCION_HUB_TOKEN", htpHubToken)
			return htpInjected
		},
		"OAuth login inside the agent": func(t *testing.T) string {
			if err := credentials.Store(htpInjected, &credentials.TokenResponse{AccessToken: htpOAuthToken, ExpiresIn: time.Hour}); err != nil {
				t.Fatal(err)
			}
			return htpInjected
		},
		"transport credential only inside the agent": func(t *testing.T) string {
			_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".scion", "scion-token"))
			t.Setenv("SCION_TRANSPORT_MODE", "iap")
			t.Setenv("SCION_TRANSPORT_TOKEN", htpTransportToken)
			return htpInjected
		},
		"endpoint is not the injected value (other port)": func(t *testing.T) string {
			return "http://host.docker.internal:9811"
		},
		"endpoint is another bridge host": func(t *testing.T) string {
			return "http://host.containers.internal:9810"
		},
		"endpoint is another non-loopback host": func(t *testing.T) string {
			return htpRefused
		},
		"injected endpoint is not a container-to-host address": func(t *testing.T) string {
			t.Setenv("SCION_HUB_ENDPOINT", "http://10.0.0.1:9810")
			return "http://10.0.0.1:9810"
		},
	}
	for name, setup := range negatives {
		t.Run(name, func(t *testing.T) {
			htpAgentEnv(t)
			endpoint := setup(t)
			hub := newHTPHub(t)
			htpRouteDials(t, hub)
			c, _, err := hubClientAndAuthSourceForEndpoint(nil, endpoint)
			if err == nil {
				_, _ = c.Health(context.Background())
			}
			assertTransportRefusal(t, err)
			hub.assertNoRequests(t)
		})
	}

	t.Run("user-credential paths stay gated inside the agent", func(t *testing.T) {
		htpAgentEnv(t)
		t.Setenv("SCION_HUB_TOKEN", htpHubToken)
		hub := newHTPHub(t)
		htpRouteDials(t, hub)
		_, _, err := tunnelClientConfig(htpInjected, io.Discard)
		assertTransportRefusal(t, err)
		err = attachHubSession(context.Background(), &HubContext{Endpoint: htpInjected}, hubAttachTarget{Name: "a", ID: "a"})
		assertTransportRefusal(t, err)
		_, err = newSSHHubConn(htpInjected, htpHubToken)
		assertTransportRefusal(t, err)
		hub.assertNoRequests(t)
	})
}

// htpGuarded lists the constructors and dialers that send or receive a hub
// credential (import path, then name), each with the only places in cmd that
// may reference it: "<file>:<enclosing function>" or "<file>:var <name>" for a
// package-level variable. Every such place applies the transport rule first
// or is gated by its only caller:
//   - broker.go getHubClientForConnection and broker_join.go
//     newUnauthenticatedHubClient: broker-side clients that apply
//     config.CheckHubTransport themselves (the latter's only caller is
//     gated by checkBrokerHubURL);
//   - newUserHubClient (the CLI's constructor) and
//     hubClientAndAuthSourceForEndpoint (the injected agent-token exception);
//   - ssh_hostkey.go newSSHHubConn and tunnel.go tunnelClientConfig/runTunnel:
//     checkUserHubTransport first;
//   - attach.go var attachToAgentFn and vscode.go var vscodePreflight:
//     function values whose callers (attachHubSession; tunnelClientConfig
//     inside vscodePreflight) check first.
//
// A method is named "<file>:<receiver type>.<method>". Raw net/http requests
// that set a credential themselves (server_onboarding.go) are outside this
// scan except where htpGuardedLocal lists their wrappers; their path tests
// cover them.
var htpGuarded = map[string][]string{
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient.New":                     {"broker.go:getHubClientForConnection", "broker_join.go:newUnauthenticatedHubClient"},
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient.NewTransport":            {"ssh_hostkey.go:newSSHHubConn"},
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient.AttachToAgent":            {"attach.go:var attachToAgentFn"},
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient.NewPTYClient":             nil,
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient.Run":                  {"tunnel.go:runTunnel", "vscode.go:var vscodePreflight"},
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient.Config":               {"tunnel.go:tunnelClientConfig"},
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync.NewUserHubClient":          {"hub_transport_policy.go:newUserHubClient"},
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync.NewInjectedAgentHubClient": {"hub.go:hubClientAndAuthSourceForEndpoint"},
}

// htpGuardedLocal lists the package-level cmd wrappers that do NOT apply the
// transport rule themselves, each with the only places that may reference
// them; those places check first (or, for newUnauthenticatedHubClient, its
// caller applies checkBrokerHubURL and the client sends no user credential;
// for the onboarding status request, the caller applies
// quickstartStatusAllowed; for the dev sign-in link request, quickstartStatusAllowed
// or devSignInAllowed).
// Tests may still replace attachToAgentFn (test files are not scanned).
var htpGuardedLocal = map[string][]string{
	"attachToAgentFn":             {"attach.go:attachHubSession"},
	"newUnauthenticatedHubClient": {"broker_join.go:runBrokerJoin"},
	// The workstation onboarding status request carries the server's token:
	// fetchOnboardingStatus only through the quickstartOnboardingStatus seam,
	// and that only from printWorkstationQuickstart, after
	// quickstartStatusAllowed.
	"fetchOnboardingStatus":      {"server_onboarding.go:var quickstartOnboardingStatus"},
	"quickstartOnboardingStatus": {"server_daemon.go:printWorkstationQuickstart"},
	// The dev sign-in link request carries the server's dev token:
	// requestDevSignInLink only through the quickstartDevSignIn and
	// serverOpenRequestLink seams; quickstartDevSignIn only from
	// printWorkstationQuickstart, after quickstartStatusAllowed;
	// serverOpenRequestLink only from runServerOpen, after devSignInAllowed.
	"requestDevSignInLink":  {"server_onboarding.go:var quickstartDevSignIn", "server_open.go:var serverOpenRequestLink"},
	"quickstartDevSignIn":   {"server_daemon.go:printWorkstationQuickstart"},
	"serverOpenRequestLink": {"server_open.go:runServerOpen"},
}

// TestHubClientConstructionGoesThroughTransportRule fails when a cmd file
// references a guarded hub constructor or dialer (a call, a function value or
// a composite literal; through any import name) outside its allowed places,
// imports a guarded package with a dot import, references a guarded local
// wrapper (htpGuardedLocal) outside its allowed places, or when an allowed
// place no longer references it (a stale entry).
func TestHubClientConstructionGoesThroughTransportRule(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	guardedPkgs := map[string]bool{}
	for k := range htpGuarded {
		guardedPkgs[k[:strings.LastIndex(k, ".")]] = true
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		scanned++
		local := map[string]string{} // import name in this file -> import path
		for _, im := range f.Imports {
			path, err := strconv.Unquote(im.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s: %v", file, im.Path.Value, err)
			}
			name := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			if name == "." && guardedPkgs[path] {
				t.Errorf("%s: dot import of %s hides its constructors from this guard", file, path)
			}
			local[name] = path
		}
		for _, decl := range f.Decls {
			var places []string
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					rt := d.Recv.List[0].Type
					if st, ok := rt.(*ast.StarExpr); ok {
						rt = st.X
					}
					if id, ok := rt.(*ast.Ident); ok {
						name = id.Name + "." + name
					} else {
						name = "(receiver)." + name
					}
				}
				places = []string{file + ":" + name}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							places = append(places, file+":var "+n.Name)
						}
					}
				}
				if len(places) == 0 {
					places = []string{file + ":(declaration)"}
				}
			}
			declNames := map[*ast.Ident]bool{} // the declared names themselves are not references
			switch d := decl.(type) {
			case *ast.FuncDecl:
				declNames[d.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							declNames[n] = true
						}
					}
				}
			}
			selNames := map[*ast.Ident]bool{} // x.Name selectors are fields or package members, not locals
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && !declNames[id] && !selNames[id] {
					if allowed, guarded := htpGuardedLocal[id.Name]; guarded {
						key := "cmd." + id.Name
						for _, place := range places {
							ok := false
							for _, a := range allowed {
								if a == place {
									ok = true
									seen[key+" "+a] = true
								}
							}
							if !ok {
								t.Errorf("%s: %s referenced in %s, outside its allowed places %v (it does not apply the transport rule itself)", fset.Position(id.Pos()), id.Name, place, allowed)
							}
						}
					}
					return true
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				selNames[sel.Sel] = true
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				path, ok := local[id.Name]
				if !ok {
					return true
				}
				key := path + "." + sel.Sel.Name
				allowed, guarded := htpGuarded[key]
				if !guarded {
					return true
				}
				for _, place := range places {
					ok := false
					for _, a := range allowed {
						if a == place {
							ok = true
							seen[key+" "+a] = true
						}
					}
					if !ok {
						t.Errorf("%s: %s referenced in %s, outside the transport rule's allowed places %v", fset.Position(sel.Pos()), key, place, allowed)
					}
				}
				return true
			})
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned %d cmd files; the scan did not find the package sources", scanned)
	}
	for key, allowed := range htpGuarded {
		for _, a := range allowed {
			if !seen[key+" "+a] {
				t.Errorf("stale allowed place %s for %s: it no longer references it", a, key)
			}
		}
	}
	for name, allowed := range htpGuardedLocal {
		for _, a := range allowed {
			if !seen["cmd."+name+" "+a] {
				t.Errorf("stale allowed place %s for %s: it no longer references it", a, name)
			}
		}
	}
}

// TestServerOpen_TransportRule: 'scion server open' requests a sign-in link
// with the server's dev token, so the transport rule applies: a plaintext
// non-loopback web URL is refused before the token is read or anything is
// sent, and a loopback literal or an https URL gets the link.
func TestServerOpen_TransportRule(t *testing.T) {
	setWebBase := func(t *testing.T, base string) {
		t.Helper()
		orig := serverOpenWebBase
		t.Cleanup(func() { serverOpenWebBase = orig })
		serverOpenWebBase = func(string, bool) string { return base }
	}
	stubTokenRead := func(t *testing.T, read func(*config.GlobalConfig, string) string) {
		t.Helper()
		orig := readServerDevToken
		t.Cleanup(func() { readServerDevToken = orig })
		readServerDevToken = read
	}
	for _, endpoint := range htpRefusedEndpoints {
		t.Run("refused "+endpoint, func(t *testing.T) {
			stubServerOpen(t, false)
			serverOpenPrintOnly = true
			setWebBase(t, endpoint)
			serverOpenRequestLink = func(string, string, string) (devSignInLink, error) {
				t.Fatal("server open requested a sign-in link from a refused URL")
				return devSignInLink{}, nil
			}
			stubTokenRead(t, func(*config.GlobalConfig, string) string {
				t.Fatal("server open read the dev token for a refused URL")
				return ""
			})
			err := runServerOpen(serverOpenCmd, nil)
			assertTransportRefusal(t, err)
			if strings.Contains(err.Error(), fixtureDevToken) {
				t.Fatalf("refusal echoes the dev token: %v", err)
			}
		})
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "https://hub.example:8443"} {
		t.Run("allowed "+endpoint, func(t *testing.T) {
			base, token, _ := stubServerOpen(t, false)
			serverOpenPrintOnly = true
			setWebBase(t, endpoint)
			var out strings.Builder
			serverOpenCmd.SetOut(&out)
			t.Cleanup(func() { serverOpenCmd.SetOut(nil) })
			if err := runServerOpen(serverOpenCmd, nil); err != nil {
				t.Fatalf("runServerOpen(%s): %v", endpoint, err)
			}
			if *base != endpoint || *token != fixtureDevToken {
				t.Fatalf("link requested from %q with token %q; want %q with the dev token", *base, *token, endpoint)
			}
		})
	}
}

// TestPrintWorkstationQuickstart_SignInTransportRule: the quickstart's dev
// sign-in link request carries the server's token, so it is sent only to a
// loopback (or TLS) URL; for another bind host neither the onboarding status
// nor a sign-in link is requested, the token is not read, and the quickstart
// says why.
func TestPrintWorkstationQuickstart_SignInTransportRule(t *testing.T) {
	countTokenReads := func(t *testing.T, refuse bool) *int {
		t.Helper()
		orig := readServerDevToken
		t.Cleanup(func() { readServerDevToken = orig })
		n := new(int)
		readServerDevToken = func(cfg *config.GlobalConfig, globalDir string) string {
			if refuse {
				t.Fatal("the quickstart read the dev token for a non-loopback plaintext URL")
			}
			*n++
			return orig(cfg, globalDir)
		}
		return n
	}
	for _, host := range []string{"10.0.0.5", "hub.example"} {
		t.Run("refused "+host, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "dev-token"), []byte(fixtureDevToken+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
			quickstartDevSignIn = func(string, string, string) (devSignInLink, error) {
				t.Fatal("the quickstart requested a sign-in link from a non-loopback plaintext URL")
				return devSignInLink{}, nil
			}
			countTokenReads(t, true)
			output := captureStdout(t, func() {
				printWorkstationQuickstart(false, dir, nil, host, 8080, true, true, true, true)
			})
			if rec.statusCalls != 0 {
				t.Fatalf("onboarding status asked %d times for %s", rec.statusCalls, host)
			}
			if !strings.Contains(output, "Sign-in links are requested only over a loopback or TLS connection, so no sign-in link was requested.") ||
				!strings.Contains(output, "SSH forward to http://127.0.0.1:8080") {
				t.Fatalf("no notice in output:\n%s", output)
			}
			if strings.Contains(output, fixtureDevToken) {
				t.Fatal("output contains the dev token")
			}
		})
	}
	t.Run("allowed 127.0.0.1", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "dev-token"), []byte(fixtureDevToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
		var gotBase, gotToken string
		quickstartDevSignIn = func(baseURL, token, next string) (devSignInLink, error) {
			rec.signInCalls++
			gotBase, gotToken = baseURL, token
			return devSignInLink{URL: baseURL + "/auth/dev-signin?code=xyz", ExpiresAt: time.Now().Add(2 * time.Minute)}, nil
		}
		reads := countTokenReads(t, false)
		output := captureStdout(t, func() {
			printWorkstationQuickstart(false, dir, nil, "127.0.0.1", 8080, true, true, true, true)
		})
		if *reads != 1 {
			t.Fatalf("dev token read %d times, want 1", *reads)
		}
		if rec.signInCalls != 1 || gotBase != "http://127.0.0.1:8080" || gotToken != fixtureDevToken {
			t.Fatalf("sign-in calls %d, base %q, token %q; want 1 call to http://127.0.0.1:8080 with the dev token", rec.signInCalls, gotBase, gotToken)
		}
		if rec.statusCalls != 1 {
			t.Fatalf("onboarding status asked %d times, want 1", rec.statusCalls)
		}
		if strings.Contains(output, "requested only over a loopback") {
			t.Fatal("loopback quickstart printed the transport notice")
		}
	})
}
