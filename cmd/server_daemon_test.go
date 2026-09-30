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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreachableHTTPURL returns an http:// URL that nothing is listening on, for
// probeServerStatus tests that need to simulate a component that is not
// running. It opens and immediately closes a loopback listener so the OS
// hands back a genuinely free ephemeral port rather than a guessed one.
func unreachableHTTPURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return "http://" + addr
}

// splitTestServerHostPort extracts the host and numeric port from an
// httptest.Server's URL, for tests that drive waitForServerReady (which
// takes host/port separately rather than a URL).
func splitTestServerHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return u.Hostname(), port
}

// newBoolFlagCmd builds a throwaway command with a single bool flag and parses
// setArgs into it, so cmd.Flags().Changed reflects whether the flag was given.
func newBoolFlagCmd(t *testing.T, name string, setArgs ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	var v bool
	c.Flags().BoolVar(&v, name, false, "")
	c.SetArgs(setArgs)
	if err := c.Execute(); err != nil {
		t.Fatalf("parse %v: %v", setArgs, err)
	}
	return c
}

// TestAppendDaemonBoolFlagForwardsExplicitDisable is the regression guard for
// the workstation-mode dev-auth bug: `scion server start --dev-auth=false`
// re-exec'd itself as a --foreground child WITHOUT --dev-auth, so the child's
// applyWorkstationDefaults silently re-enabled it. The daemon arg builder must
// forward an explicitly-set flag as --flag=<value> so the disable survives.
func TestAppendDaemonBoolFlagForwardsExplicitDisable(t *testing.T) {
	// Explicit --dev-auth=false → forwarded as --dev-auth=false (the fix).
	c := newBoolFlagCmd(t, "dev-auth", "--dev-auth=false")
	assert.Equal(t, []string{"--dev-auth=false"}, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Explicit --dev-auth=true → forwarded as --dev-auth=true.
	c = newBoolFlagCmd(t, "dev-auth", "--dev-auth=true")
	assert.Equal(t, []string{"--dev-auth=true"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value true (a workstation default) → historical bare form.
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Equal(t, []string{"--dev-auth"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value false → nothing appended (unchanged behavior).
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Nil(t, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Appends onto existing args rather than replacing them.
	c = newBoolFlagCmd(t, "enable-web", "--enable-web=false")
	assert.Equal(t, []string{"server", "start", "--enable-web=false"},
		appendDaemonBoolFlag(c, []string{"server", "start"}, "enable-web", false))
}

// TestBuildDaemonStartArgsForwardsExplicitFlags checks that buildDaemonStartArgs
// forwards every explicitly-set flag to the --foreground child — the workstation
// disable (--enable-web=false), the non-workstation bools, and the string/int
// flags that are otherwise lost across the re-exec — while omitting unset ones.
func TestBuildDaemonStartArgsForwardsExplicitFlags(t *testing.T) {
	resetServerFlags()
	// Globals resetServerFlags doesn't cover:
	noAutoMigrate, enableTestLogin, simulateRemoteBroker = false, false, false
	templateCacheDir, webAssetsDir, webBaseURL = "", "", ""
	adminEmails = nil
	templateCacheMax, globalMode = 0, false
	defer func() {
		resetServerFlags()
		noAutoMigrate, enableTestLogin, simulateRemoteBroker = false, false, false
		templateCacheDir, webAssetsDir, webBaseURL = "", "", ""
		adminEmails = nil
		templateCacheMax, globalMode = 0, false
	}()

	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	f := c.Flags()
	f.BoolVar(&hostedMode, "hosted", false, "")
	f.StringVar(&hubHost, "host", "0.0.0.0", "")
	f.BoolVar(&enableWeb, "enable-web", false, "")
	f.BoolVar(&enableDevAuth, "dev-auth", false, "")
	f.BoolVar(&noAutoMigrate, "no-auto-migrate", false, "")
	f.BoolVar(&enableTestLogin, "enable-test-login", false, "")
	f.BoolVar(&simulateRemoteBroker, "simulate-remote-broker", false, "")
	f.StringVar(&templateCacheDir, "template-cache-dir", "", "")
	f.Int64Var(&templateCacheMax, "template-cache-max", 100*1024*1024, "")
	f.StringVar(&webAssetsDir, "web-assets-dir", "", "")
	f.StringVar(&webSessionSecret, "session-secret", "", "")
	f.StringVar(&webBaseURL, "base-url", "", "")
	f.StringArrayVar(&adminEmails, "admin-emails", nil, "")

	c.SetArgs([]string{
		"--hosted=false",     // explicit mode disable must survive the re-exec
		"--enable-web=false", // explicit workstation-default disable (the core fix)
		"--no-auto-migrate",
		"--simulate-remote-broker=true",
		"--session-secret=topsecret", // set, but must NOT be forwarded (signing secret)
		"--base-url=https://scion.example.com",
		"--admin-emails=a@x.com,b@y.com",
		"--template-cache-max=42",
	})
	if err := c.Execute(); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := buildDaemonStartArgs(c)

	assert.Equal(t, []string{"server", "start", "--foreground"}, got[:3])
	for _, want := range []string{
		"--hosted=false",
		"--enable-web=false",
		"--no-auto-migrate=true", // helper normalizes bare --no-auto-migrate to =true
		"--simulate-remote-broker=true",
		"--base-url=https://scion.example.com",
		"--admin-emails=a@x.com,b@y.com",
		"--template-cache-max=42",
	} {
		assert.Contains(t, got, want)
	}
	// Unset flags are not forwarded; --host is guarded by Changed; the session
	// secret must never reach the child argv / saved-args file.
	assert.NotContains(t, got, "--enable-test-login")
	for _, a := range got {
		assert.False(t, strings.HasPrefix(a, "--web-assets-dir="), "web-assets-dir omitted when unset")
		assert.False(t, strings.HasPrefix(a, "--template-cache-dir="), "template-cache-dir omitted when unset")
		assert.False(t, strings.HasPrefix(a, "--host="), "host omitted when not explicitly set")
		assert.False(t, strings.HasPrefix(a, "--session-secret"), "session-secret never forwarded")
	}
	assert.NotContains(t, strings.Join(got, " "), "topsecret", "session secret must not leak into args")
}

// TestBuildDaemonStartArgsForwardsExplicitHost guards the positive side of the
// --host fix: an explicitly-set host must still be forwarded (only the
// unconditional default forwarding was removed).
func TestBuildDaemonStartArgsForwardsExplicitHost(t *testing.T) {
	resetServerFlags()
	defer resetServerFlags()

	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().StringVar(&hubHost, "host", "0.0.0.0", "")
	c.SetArgs([]string{"--host=1.2.3.4"})
	if err := c.Execute(); err != nil {
		t.Fatalf("parse: %v", err)
	}
	assert.Contains(t, buildDaemonStartArgs(c), "--host=1.2.3.4")
}

// TestColocatedBrokerReason guards a shape colocatedBrokerReason must
// handle: in combined workstation mode (the default and the case this exists
// for), the web server answers /healthz with a composite body that nests the
// Hub's checks under "hub" (pkg/hub.CompositeHealthResponse /
// WebServer.handleHealthz) instead of putting them at the top level like a
// standalone Hub's direct response does. colocatedBrokerReason must find the
// reason in both shapes.
func TestColocatedBrokerReason(t *testing.T) {
	tests := []struct {
		name   string
		health healthProbeResponse
		want   string
	}{
		{
			name:   "healthy top-level",
			health: healthProbeResponse{Status: "healthy"},
			want:   "",
		},
		{
			name: "degraded for another reason, not colocated_broker",
			health: healthProbeResponse{
				Status: "degraded",
				Checks: map[string]string{"database": "unhealthy"},
			},
			want: "",
		},
		{
			name: "standalone Hub body: degraded because of colocated_broker",
			health: healthProbeResponse{
				Status: "degraded",
				Checks: map[string]string{"colocated_broker": "unhealthy: registration failed"},
			},
			want: "unhealthy: registration failed",
		},
		{
			name: "composite (combined-mode) body: reason nested under hub",
			health: healthProbeResponse{
				Status: "degraded",
				Hub: &struct {
					Status string            `json:"status"`
					Checks map[string]string `json:"checks"`
				}{
					Status: "degraded",
					Checks: map[string]string{"colocated_broker": "unhealthy: registration pending"},
				},
			},
			want: "unhealthy: registration pending",
		},
		{
			name: "composite body: top-level degraded but hub healthy -> no colocated_broker reason",
			health: healthProbeResponse{
				Status: "degraded",
				Hub: &struct {
					Status string            `json:"status"`
					Checks map[string]string `json:"checks"`
				}{Status: "healthy"},
			},
			want: "",
		},
		{
			name:   "malformed/empty body",
			health: healthProbeResponse{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, colocatedBrokerReason(tt.health))
		})
	}
}

// TestColocatedBrokerReason_FromRealJSON decodes actual JSON shapes (rather
// than constructing healthProbeResponse by hand) to guard against the struct
// tags drifting from what pkg/hub.HealthResponse/CompositeHealthResponse
// actually emit.
func TestColocatedBrokerReason_FromRealJSON(t *testing.T) {
	standalone := `{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed","database":"healthy"}}`
	var h1 healthProbeResponse
	require.NoError(t, json.Unmarshal([]byte(standalone), &h1))
	assert.Equal(t, "unhealthy: registration failed", colocatedBrokerReason(h1))

	composite := `{"status":"degraded","version":"0.1.0","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`
	var h2 healthProbeResponse
	require.NoError(t, json.Unmarshal([]byte(composite), &h2))
	assert.Equal(t, "unhealthy: registration pending", colocatedBrokerReason(h2))

	healthyComposite := `{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy","checks":{"colocated_broker":"healthy"}}}`
	var h3 healthProbeResponse
	require.NoError(t, json.Unmarshal([]byte(healthyComposite), &h3))
	assert.Equal(t, "", colocatedBrokerReason(h3))
}

// TestWaitForServerReady_NamesColocatedBrokerReasonInCombinedMode drives
// waitForServerReady against an httptest.Server serving a combined-mode
// (composite) degraded body, and checks that the returned lastHealth lets
// the caller name the colocated_broker reason — the exact case
// printWorkstationQuickstart needs (it polls the web port, which is always
// combined-mode shaped when a hub health provider is registered).
func TestWaitForServerReady_NamesColocatedBrokerReasonInCombinedMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}`))
	}))
	defer srv.Close()

	host, port := splitTestServerHostPort(t, srv.URL)
	ready, lastHealth := waitForServerReady(host, port, 500*time.Millisecond)
	assert.False(t, ready)
	assert.Equal(t, "unhealthy: registration failed", colocatedBrokerReason(lastHealth))
}

// TestWaitForServerReady_ReturnsReadyOnHealthy is the healthy-path
// counterpart: once the composite body reports "healthy", waitForServerReady
// returns immediately rather than waiting out the full timeout.
func TestWaitForServerReady_ReturnsReadyOnHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy"}}`))
	}))
	defer srv.Close()

	host, port := splitTestServerHostPort(t, srv.URL)
	start := time.Now()
	ready, _ := waitForServerReady(host, port, 20*time.Second)
	assert.True(t, ready)
	assert.Less(t, time.Since(start), 5*time.Second, "should return promptly once healthy, not wait out the timeout")
}

// TestFormatServerStatusComponents guards two things: a standalone Hub
// without a web server must not have its Web Frontend line mislabeled
// "degraded" just because the Hub itself is degraded, and both the
// standalone and combined-mode deployments must name the colocated_broker
// reason instead of printing a bare "not detected".
func TestFormatServerStatusComponents(t *testing.T) {
	tests := []struct {
		name   string
		status serverStatusInfo
		want   []string
	}{
		{
			name:   "combined mode, healthy",
			status: serverStatusInfo{HubRunning: true, WebRunning: true, BrokerRunning: true},
			want: []string{
				"  Hub API:         running",
				"  Runtime Broker:  running",
				"  Web Frontend:    running",
			},
		},
		{
			name: "combined mode, degraded on colocated_broker",
			status: serverStatusInfo{
				HubDegradedReason: "unhealthy: registration failed",
				WebDegradedReason: "unhealthy: registration failed",
			},
			want: []string{
				"  Hub API:         degraded (colocated_broker: unhealthy: registration failed) — see server log; restart after fixing the broker config",
				"  Runtime Broker:  not detected",
				"  Web Frontend:    degraded (colocated_broker: unhealthy: registration failed) — see server log; restart after fixing the broker config",
			},
		},
		{
			name:   "standalone hub, healthy, no web server",
			status: serverStatusInfo{HubRunning: true, BrokerRunning: true},
			want: []string{
				"  Hub API:         running",
				"  Runtime Broker:  running",
				"  Web Frontend:    not detected",
			},
		},
		{
			// This is the state probeServerStatus actually produces for a real
			// standalone degraded Hub: the 9810 branch sets HubRunning=true
			// whenever the body parses at all, degraded or not. A test input with
			// HubRunning:false would not match what the probe produces, so it
			// would not guard the line a standalone operator actually sees.
			name: "standalone hub, degraded on colocated_broker, no web server: Web Frontend must stay not detected",
			status: serverStatusInfo{
				HubRunning:        true,
				HubDegradedReason: "unhealthy: registration pending",
				// WebDegradedReason intentionally unset: no web server ran at all.
			},
			want: []string{
				"  Hub API:         running, degraded (colocated_broker: unhealthy: registration pending) — see server log; restart after fixing the broker config",
				"  Runtime Broker:  not detected",
				"  Web Frontend:    not detected",
			},
		},
		{
			name:   "nothing detected",
			status: serverStatusInfo{},
			want: []string{
				"  Hub API:         not detected",
				"  Runtime Broker:  not detected",
				"  Web Frontend:    not detected",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatServerStatusComponents(tt.status))
		})
	}
}

// TestProbeServerStatus_StandaloneHubDegraded guards the realistic
// standalone-deployment shape (a Hub with no web server running at all,
// answering degraded because of colocated_broker): it must produce
// HubRunning=true, HubDegradedReason set, and WebDegradedReason empty — the
// input formatServerStatusComponents' standalone test case exercises.
// Without this test, setting WebDegradedReason from the 9810 branch (which
// would mislabel a standalone Hub's Web Frontend as degraded) would pass
// silently.
func TestProbeServerStatus_StandaloneHubDegraded(t *testing.T) {
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}`))
	}))
	defer hubSrv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, unreachableHTTPURL(t), hubSrv.URL, unreachableHTTPURL(t))

	assert.True(t, status.HubRunning)
	assert.False(t, status.WebRunning)
	assert.Equal(t, "unhealthy: registration failed", status.HubDegradedReason)
	assert.Empty(t, status.WebDegradedReason, "no web server ran, so Web Frontend must not be labeled degraded")
}

// TestProbeServerStatus_CombinedModeDegraded covers the other realistic
// shape: the web server answers on the combined port with a degraded
// composite body (colocated_broker nested under "hub"). Both HubRunning and
// WebRunning stay false (status is not "healthy"), and both degraded-reason
// fields are set from the single 8080 probe.
//
// The Hub URL is a request-counting server rather than merely an unreachable
// one: combined mode (--enable-web) never starts the standalone Hub listener
// (port 9810), so once the web port has answered with the combined composite
// body (it carries a nested "hub" object) — healthy or degraded — a
// follow-up probe to the standalone hub port is redundant. If that fallback
// guard were ever weakened to probe the standalone port whenever the web
// port merely failed to report "healthy" (rather than whenever it answered
// with a composite body), this test fails on the hit-count assertion even
// though the final status fields would still come out looking correct. See
// TestProbeServerStatus_UnrelatedServiceOnWebPort for the case where the web
// port answers but is not a scion combined server.
func TestProbeServerStatus_CombinedModeDegraded(t *testing.T) {
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`))
	}))
	defer webSrv.Close()

	var hubHits int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hubHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer hubSrv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, hubSrv.URL, unreachableHTTPURL(t))

	assert.False(t, status.HubRunning)
	assert.False(t, status.WebRunning)
	assert.Equal(t, "unhealthy: registration pending", status.HubDegradedReason)
	assert.Equal(t, "unhealthy: registration pending", status.WebDegradedReason)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hubHits),
		"the 8080 probe already answered (degraded), so the standalone hub port must not be probed at all")
}

// TestProbeServerStatus_UnrelatedServiceOnWebPort guards against a regression
// where the standalone-hub-port skip keys off "any parseable 200 JSON body"
// on the web port rather than specifically the scion combined composite body
// (identified by a non-nil "hub" key, see healthProbeResponse). Port 8080 is
// a common dev port; some unrelated service could easily be listening there
// and answer /healthz-shaped requests with an unrelated 200 JSON object. In
// that case a real standalone Hub on 9810 must still be found.
func TestProbeServerStatus_UnrelatedServiceOnWebPort(t *testing.T) {
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer webSrv.Close()

	var hubHits int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hubHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer hubSrv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, hubSrv.URL, unreachableHTTPURL(t))

	assert.True(t, status.HubRunning, "the unrelated 8080 responder must not suppress the standalone hub probe")
	assert.Equal(t, int32(1), atomic.LoadInt32(&hubHits),
		"the standalone hub port must still be probed when the web port's body has no \"hub\" key")
}

// TestProbeServerStatus_CombinedModeHealthy is the happy-path counterpart:
// a healthy composite response marks both HubRunning and WebRunning, with no
// degraded reasons and no fallback probe to the standalone hub port. The Hub
// URL is a request-counting server rather than merely an unreachable one, so
// a fallback probe that silently failed (rather than never being attempted)
// would still be caught: if the "already found on the web port" guard were
// ever weakened to always probe the standalone port too, this test fails on
// the hit-count assertion even though the final status fields would still
// come out looking correct.
func TestProbeServerStatus_CombinedModeHealthy(t *testing.T) {
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy"}}`))
	}))
	defer webSrv.Close()

	var hubHits int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hubHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer hubSrv.Close()

	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer brokerSrv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, hubSrv.URL, brokerSrv.URL)

	assert.True(t, status.HubRunning)
	assert.True(t, status.WebRunning)
	assert.True(t, status.BrokerRunning)
	assert.Empty(t, status.HubDegradedReason)
	assert.Empty(t, status.WebDegradedReason)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hubHits),
		"the 8080 probe already reported healthy, so the standalone hub port must not be probed at all")
}
