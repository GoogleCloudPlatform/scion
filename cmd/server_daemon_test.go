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
	"sync"
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

// serveHealth starts an httptest.Server that answers every request with the
// given /healthz body.
func serveHealth(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fastReadyPoll shortens waitForServerReady's poll interval for a test.
func fastReadyPoll(t *testing.T) {
	t.Helper()
	old := serverReadyPollInterval
	serverReadyPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { serverReadyPollInterval = old })
}

// TestHealthProblemReason decodes actual JSON shapes (rather than
// constructing healthProbeResponse by hand) to guard against the struct tags
// drifting from what pkg/hub.HealthResponse/CompositeHealthResponse emit. In
// combined workstation mode (the default) the web server nests the Hub's
// checks under "hub"; a standalone Hub puts them at the top level. Both must
// yield the non-healthy checks, by name (ptone/scion#1094, #2154).
func TestHealthProblemReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"healthy composite", `{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy","checks":{"colocated_broker":"healthy","database":"healthy"}}}`, ""},
		{"healthy standalone", `{"status":"healthy","checks":{"database":"healthy"}}`, ""},
		{"empty body", `{}`, ""},
		{"standalone degraded on colocated_broker", `{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed","database":"healthy"}}`, "colocated_broker: unhealthy: registration failed"},
		{"composite degraded, nested under hub", `{"status":"degraded","version":"0.1.0","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`, "colocated_broker: unhealthy: registration pending"},
		{"standalone unhealthy, database", `{"status":"unhealthy","checks":{"database":"unhealthy"}}`, "database: unhealthy"},
		{"several checks sorted", `{"status":"unhealthy","checks":{"database":"unhealthy","colocated_broker":"unhealthy: registration failed"}}`, "colocated_broker: unhealthy: registration failed; database: unhealthy"},
		{"informational key", `{"status":"degraded","checks":{"workspace_storage":"healthy","workspace_storage_mount_verification":"unavailable: could not compare filesystem device IDs"}}`, "workspace_storage_mount_verification: unavailable: could not compare filesystem device IDs"},
		{"composite degraded by broker only, cause named", `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"healthy","checks":{"database":"healthy"}},"broker":{"status":"degraded","checks":{"docker":"available","nfs_mounts":"unhealthy: share1 not mounted"}}}`, "broker.nfs_mounts: unhealthy: share1 not mounted"},
		{"composite degraded by broker, no named check", `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"healthy","checks":{"database":"healthy"}},"broker":{"status":"degraded","checks":{"docker":"available"}}}`, "broker: degraded"},
		{"healthy broker checks are ignored", `{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy"},"broker":{"status":"healthy","checks":{"runtime":"unavailable"}}}`, ""},
		{"degraded with no named check", `{"status":"degraded"}`, "status: degraded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h healthProbeResponse
			require.NoError(t, json.Unmarshal([]byte(tt.body), &h))
			assert.Equal(t, tt.want, healthProblemReason(h))
		})
	}
}

func TestHealthProblemHint(t *testing.T) {
	assert.Equal(t, "see server log; restart after fixing the broker config",
		healthProblemHint("colocated_broker: unhealthy: registration failed"))
	assert.Equal(t, "see server log", healthProblemHint("database: unhealthy"))
}

// TestWaitForServerReady_DegradedAtDeadlineIsReady: a server that stays
// degraded is up and serving, so once the deadline passes waitForServerReady
// reports ready (not a timeout failure) and the last health response names
// the checks for the caller's warning (ptone/scion#1094, #2154).
func TestWaitForServerReady_DegradedAtDeadlineIsReady(t *testing.T) {
	fastReadyPoll(t)
	srv := serveHealth(t, `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}`)

	host, port := splitTestServerHostPort(t, srv.URL)
	start := time.Now()
	ready, lastHealth := waitForServerReady(host, port, 300*time.Millisecond)
	assert.True(t, ready, "degraded means up")
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond,
		"must keep polling for healthy until the deadline before settling for degraded")
	assert.Equal(t, "degraded", lastHealth.Status)
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", healthProblemReason(lastHealth))
}

// TestWaitForServerReady_DegradedThenHealthy covers the startup window: the
// co-located broker registers just after the listener starts, so a transient
// degraded must not end the wait early; healthy wins once it arrives.
func TestWaitForServerReady_DegradedThenHealthy(t *testing.T) {
	fastReadyPoll(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if atomic.AddInt32(&hits, 1) < 3 {
			_, _ = w.Write([]byte(`{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy"}}`))
	}))
	defer srv.Close()

	host, port := splitTestServerHostPort(t, srv.URL)
	ready, lastHealth := waitForServerReady(host, port, 5*time.Second)
	assert.True(t, ready)
	assert.Equal(t, "healthy", lastHealth.Status)
	assert.Empty(t, healthProblemReason(lastHealth))
}

// TestWaitForServerReady_UnhealthyIsNotReady: a failed critical check
// (database) is not "up", even though the process answers.
func TestWaitForServerReady_UnhealthyIsNotReady(t *testing.T) {
	fastReadyPoll(t)
	srv := serveHealth(t, `{"status":"unhealthy","web":{"status":"ok"},"hub":{"status":"unhealthy","checks":{"database":"unhealthy"}}}`)

	host, port := splitTestServerHostPort(t, srv.URL)
	ready, lastHealth := waitForServerReady(host, port, 200*time.Millisecond)
	assert.False(t, ready)
	assert.Equal(t, "database: unhealthy", healthProblemReason(lastHealth))
}

// TestWaitForServerReady_Unreachable: nothing answering is not ready, with
// a zero lastHealth.
func TestWaitForServerReady_Unreachable(t *testing.T) {
	fastReadyPoll(t)
	host, port := splitTestServerHostPort(t, unreachableHTTPURL(t))
	ready, lastHealth := waitForServerReady(host, port, 100*time.Millisecond)
	assert.False(t, ready)
	assert.Empty(t, healthProblemReason(lastHealth))
}

// TestWaitForServerReady_ReturnsReadyOnHealthy is the healthy-path
// counterpart: once the composite body reports "healthy", waitForServerReady
// returns immediately rather than waiting out the full timeout.
func TestWaitForServerReady_ReturnsReadyOnHealthy(t *testing.T) {
	srv := serveHealth(t, `{"status":"healthy","web":{"status":"ok"},"hub":{"status":"healthy"}}`)

	host, port := splitTestServerHostPort(t, srv.URL)
	start := time.Now()
	ready, _ := waitForServerReady(host, port, 20*time.Second)
	assert.True(t, ready)
	assert.Less(t, time.Since(start), 5*time.Second, "should return promptly once healthy, not wait out the timeout")
}

// TestFormatServerStatusComponents guards: a standalone Hub without a web
// server must not have its Web Frontend line mislabeled "degraded" just
// because the Hub itself is degraded; a degraded component is "running" and
// names its checks (with the broker-specific recovery hint for
// colocated_broker, ptone/scion#2154); an unhealthy one is not running but
// still names its checks instead of a bare "not detected".
func TestFormatServerStatusComponents(t *testing.T) {
	const brokerReason = "colocated_broker: unhealthy: registration failed"
	const brokerHint = " — see server log; restart after fixing the broker config"
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
				HubRunning: true, WebRunning: true,
				HubStatus: "degraded", WebStatus: "degraded",
				HubHealthReason: brokerReason,
				WebHealthReason: brokerReason,
			},
			want: []string{
				"  Hub API:         running, degraded (" + brokerReason + ")" + brokerHint,
				"  Runtime Broker:  not detected",
				"  Web Frontend:    running, degraded (" + brokerReason + ")" + brokerHint,
			},
		},
		{
			name: "combined mode, unhealthy database",
			status: serverStatusInfo{
				HubStatus: "unhealthy", WebStatus: "unhealthy",
				HubHealthReason: "database: unhealthy",
				WebHealthReason: "database: unhealthy",
			},
			want: []string{
				"  Hub API:         unhealthy (database: unhealthy) — see server log",
				"  Runtime Broker:  not detected",
				"  Web Frontend:    unhealthy (database: unhealthy) — see server log",
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
			name: "standalone hub, degraded on colocated_broker, no web server: Web Frontend must stay not detected",
			status: serverStatusInfo{
				HubRunning:      true,
				HubStatus:       "degraded",
				HubHealthReason: "colocated_broker: unhealthy: registration pending",
				// Web fields intentionally unset: no web server ran at all.
			},
			want: []string{
				"  Hub API:         running, degraded (colocated_broker: unhealthy: registration pending)" + brokerHint,
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
// answering degraded because of colocated_broker): degraded is up, so
// HubRunning=true with the checks named, and the Web fields stay empty — a
// standalone Hub's Web Frontend must not be labeled degraded.
func TestProbeServerStatus_StandaloneHubDegraded(t *testing.T) {
	hubSrv := serveHealth(t, `{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}`)

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, unreachableHTTPURL(t), hubSrv.URL, unreachableHTTPURL(t))

	assert.True(t, status.HubRunning)
	assert.False(t, status.WebRunning)
	assert.Equal(t, "degraded", status.HubStatus)
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", status.HubHealthReason)
	assert.Empty(t, status.WebHealthReason, "no web server ran, so Web Frontend must not be labeled degraded")
	assert.Empty(t, status.WebStatus)
}

// TestProbeServerStatus_StandaloneHubUnhealthy: a failed critical check is
// not "up" — HubRunning stays false — but the checks are still named.
func TestProbeServerStatus_StandaloneHubUnhealthy(t *testing.T) {
	hubSrv := serveHealth(t, `{"status":"unhealthy","checks":{"database":"unhealthy"}}`)

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, unreachableHTTPURL(t), hubSrv.URL, unreachableHTTPURL(t))

	assert.False(t, status.HubRunning)
	assert.Equal(t, "unhealthy", status.HubStatus)
	assert.Equal(t, "database: unhealthy", status.HubHealthReason)
	assert.Empty(t, status.WebStatus)
}

// TestProbeServerStatus_CombinedModeDegraded covers the other realistic
// shape: the web server answers on the combined port with a degraded
// composite body (colocated_broker nested under "hub"). Degraded is up, so
// HubRunning and WebRunning are true, and both reason fields are set from
// the single 8080 probe.
//
// The Hub URL is a request-counting server rather than merely an unreachable
// one: combined mode (--enable-web) never starts the standalone Hub listener
// (port 9810), so once the web port has answered with the combined composite
// body (it carries a nested "hub" object), a follow-up probe to the
// standalone hub port is redundant. See
// TestProbeServerStatus_UnrelatedServiceOnWebPort for the case where the web
// port answers but is not a scion combined server.
func TestProbeServerStatus_CombinedModeDegraded(t *testing.T) {
	webSrv := serveHealth(t, `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration pending"}}}`)

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

	assert.True(t, status.HubRunning, "degraded means up")
	assert.True(t, status.WebRunning, "degraded means up")
	assert.Equal(t, "degraded", status.HubStatus)
	assert.Equal(t, "degraded", status.WebStatus)
	assert.Equal(t, "colocated_broker: unhealthy: registration pending", status.HubHealthReason)
	assert.Equal(t, "colocated_broker: unhealthy: registration pending", status.WebHealthReason)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hubHits),
		"the 8080 probe already answered (degraded), so the standalone hub port must not be probed at all")
}

// TestProbeServerStatus_CombinedModeUnhealthy: an unhealthy composite is not
// up, names its checks, and still suppresses the redundant 9810 probe.
func TestProbeServerStatus_CombinedModeUnhealthy(t *testing.T) {
	webSrv := serveHealth(t, `{"status":"unhealthy","web":{"status":"ok"},"hub":{"status":"unhealthy","checks":{"database":"unhealthy"}}}`)

	var hubHits int32
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hubHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer hubSrv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, hubSrv.URL, unreachableHTTPURL(t))

	assert.False(t, status.HubRunning)
	assert.False(t, status.WebRunning)
	assert.Equal(t, "unhealthy", status.HubStatus)
	assert.Equal(t, "unhealthy", status.WebStatus)
	assert.Equal(t, "database: unhealthy", status.HubHealthReason)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hubHits))
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
	assert.Empty(t, status.HubHealthReason)
	assert.Empty(t, status.WebHealthReason)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hubHits),
		"the 8080 probe already reported healthy, so the standalone hub port must not be probed at all")
}

// TestWaitForServerReady_StaleDegradedIsNotReady (review a1 N2): a server
// that answered degraded once and then stopped answering before the deadline
// must not be reported ready on that stale answer; lastHealth still names
// the checks it last reported.
func TestWaitForServerReady_StaleDegradedIsNotReady(t *testing.T) {
	fastReadyPoll(t)
	answered := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}`))
		w.(http.Flusher).Flush()
		once.Do(func() { close(answered) })
	}))
	host, port := splitTestServerHostPort(t, srv.URL)
	// Shut the server down once it has answered once (degraded), so every
	// later poll before the deadline gets connection refused.
	go func() {
		<-answered
		srv.Close()
	}()
	t.Cleanup(srv.Close)

	ready, lastHealth := waitForServerReady(host, port, 400*time.Millisecond)
	assert.False(t, ready, "the last poll got no answer, so a stale degraded response must not count as up")
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", healthProblemReason(lastHealth))
}

// TestProbeServerStatus_StandaloneNonScionStatus (review a1 N3): a 9810
// answer whose status is not a scion hub status, or that has no status at
// all, is reported as not detected rather than echoing the status.
func TestProbeServerStatus_StandaloneNonScionStatus(t *testing.T) {
	for _, body := range []string{`{"status":"ok"}`, `{}`, `{"status":""}`} {
		t.Run(body, func(t *testing.T) {
			hubSrv := serveHealth(t, body)
			client := &http.Client{Timeout: 2 * time.Second}
			status := probeServerStatus(client, unreachableHTTPURL(t), hubSrv.URL, unreachableHTTPURL(t))

			assert.False(t, status.HubRunning)
			assert.Empty(t, status.HubStatus)
			assert.Empty(t, status.HubHealthReason)
			assert.Equal(t, "  Hub API:         not detected", formatServerStatusComponents(status)[0])
		})
	}
}

// TestProbeServerStatus_UnrelatedDegradedOnWebPort (review a1 N4): an
// unrelated service on 8080 answering {"status":"degraded"} (no nested
// "hub") must not be reported as the scion web server/hub, and the
// standalone hub port is still probed.
func TestProbeServerStatus_UnrelatedDegradedOnWebPort(t *testing.T) {
	webSrv := serveHealth(t, `{"status":"degraded"}`)
	hubSrv := serveHealth(t, `{"status":"healthy","checks":{"database":"healthy"}}`)

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, hubSrv.URL, unreachableHTTPURL(t))

	assert.False(t, status.WebRunning)
	assert.Empty(t, status.WebStatus)
	assert.Empty(t, status.WebHealthReason)
	assert.True(t, status.HubRunning, "the real standalone hub must still be found")
	assert.Empty(t, status.HubHealthReason)
}

// TestQuickstartReadyMessage (review a1 N6) covers the message and browser
// decision printWorkstationQuickstart makes after waitForServerReady.
func TestQuickstartReadyMessage(t *testing.T) {
	parse := func(body string) healthProbeResponse {
		var h healthProbeResponse
		require.NoError(t, json.Unmarshal([]byte(body), &h))
		return h
	}
	tests := []struct {
		name     string
		ready    bool
		health   healthProbeResponse
		wantMsg  string
		wantOpen bool
	}{
		{"healthy", true, parse(`{"status":"healthy","hub":{"status":"healthy"}}`), "", true},
		{"degraded at deadline", true, parse(`{"status":"degraded","hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}`),
			"  Warning: server is up but degraded: colocated_broker: unhealthy: registration failed — see server log; restart after fixing the broker config", true},
		{"unhealthy", false, parse(`{"status":"unhealthy","hub":{"status":"unhealthy","checks":{"database":"unhealthy"}}}`),
			"  (server is unhealthy: database: unhealthy — see server log)", false},
		{"stale degraded (answered, then stopped)", false, parse(`{"status":"degraded","hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}`),
			"  (server stopped answering /healthz; last status degraded: colocated_broker: unhealthy: registration failed — see server log)", false},
		{"never answered", false, healthProbeResponse{}, "  (server not yet ready — open the URL manually once it starts)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, open := quickstartReadyMessage(tt.ready, tt.health)
			assert.Equal(t, tt.wantMsg, msg)
			assert.Equal(t, tt.wantOpen, open)
		})
	}
}

// TestServerStatusInfoJSON (review a1 N5/N6) pins the `scion server status
// --json` keys: hubRunning/webRunning mean up (healthy or degraded),
// hubStatus/webStatus carry a non-healthy status, hubHealthReason/
// webHealthReason name the checks, and all are omitted when empty.
func TestServerStatusInfoJSON(t *testing.T) {
	data, err := json.Marshal(serverStatusInfo{
		DaemonRunning:   true,
		HubRunning:      true,
		WebRunning:      true,
		HubStatus:       "degraded",
		WebStatus:       "degraded",
		HubHealthReason: "colocated_broker: unhealthy: registration failed",
		WebHealthReason: "colocated_broker: unhealthy: registration failed",
	})
	require.NoError(t, err)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, true, got["hubRunning"])
	assert.Equal(t, true, got["webRunning"])
	assert.Equal(t, "degraded", got["hubStatus"])
	assert.Equal(t, "degraded", got["webStatus"])
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", got["hubHealthReason"])
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", got["webHealthReason"])
	assert.NotContains(t, got, "hubDegradedReason")

	data, err = json.Marshal(serverStatusInfo{BrokerRunning: true, BrokerStatus: "degraded", BrokerHealthReason: "nfs_mounts: unhealthy: x"})
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "degraded", got["brokerStatus"])
	assert.Equal(t, "nfs_mounts: unhealthy: x", got["brokerHealthReason"])

	data, err = json.Marshal(serverStatusInfo{DaemonRunning: true, HubRunning: true, WebRunning: true})
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(data, &got))
	for _, k := range []string{"hubStatus", "webStatus", "brokerStatus", "hubHealthReason", "webHealthReason", "brokerHealthReason"} {
		assert.NotContains(t, got, k, "a healthy server omits %s", k)
	}
}

// TestProbeServerStatus_CombinedModeBrokerOnlyDegraded (review a3 R2): a
// composite degraded only by the co-located broker, with a healthy nested
// hub, must not pin the broker's problem on the Hub line. The Hub is
// running with no reason, the Web line carries the composite, and the
// Runtime Broker line names the broker's problem from its own /healthz.
func TestProbeServerStatus_CombinedModeBrokerOnlyDegraded(t *testing.T) {
	webSrv := serveHealth(t, `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"healthy","checks":{"database":"healthy","colocated_broker":"healthy"}},"broker":{"status":"degraded","checks":{"docker":"available","nfs_mounts":"unhealthy: share1 not mounted"}}}`)
	brokerSrv := serveHealth(t, `{"status":"degraded","version":"0.1.0","uptime":"1m0s","checks":{"docker":"available","nfs_mounts":"unhealthy: share1 not mounted"}}`)

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, unreachableHTTPURL(t), brokerSrv.URL)

	assert.True(t, status.HubRunning)
	assert.Empty(t, status.HubStatus, "the hub itself is healthy")
	assert.Empty(t, status.HubHealthReason, "a broker-only problem must not be reported against the Hub")
	assert.True(t, status.WebRunning)
	assert.Equal(t, "degraded", status.WebStatus)
	assert.Equal(t, "broker.nfs_mounts: unhealthy: share1 not mounted", status.WebHealthReason)
	assert.True(t, status.BrokerRunning)
	assert.Equal(t, "degraded", status.BrokerStatus)
	assert.Equal(t, "nfs_mounts: unhealthy: share1 not mounted", status.BrokerHealthReason)

	assert.Equal(t, []string{
		"  Hub API:         running",
		"  Runtime Broker:  running, degraded (nfs_mounts: unhealthy: share1 not mounted) — see server log",
		"  Web Frontend:    running, degraded (broker.nfs_mounts: unhealthy: share1 not mounted) — see server log",
	}, formatServerStatusComponents(status))
}

// TestProbeServerStatus_BrokerProbe: the broker's own /healthz answer gives
// its line — healthy is plain running; degraded with no qualifying check
// falls back to the bare status.
func TestProbeServerStatus_BrokerProbe(t *testing.T) {
	client := &http.Client{Timeout: 2 * time.Second}

	healthy := serveHealth(t, `{"status":"healthy","checks":{"docker":"available"}}`)
	st := probeServerStatus(client, unreachableHTTPURL(t), unreachableHTTPURL(t), healthy.URL)
	assert.True(t, st.BrokerRunning)
	assert.Empty(t, st.BrokerStatus)
	assert.Empty(t, st.BrokerHealthReason)

	bare := serveHealth(t, `{"status":"degraded","checks":{"docker":"available"}}`)
	st = probeServerStatus(client, unreachableHTTPURL(t), unreachableHTTPURL(t), bare.URL)
	assert.True(t, st.BrokerRunning)
	assert.Equal(t, "degraded", st.BrokerStatus)
	assert.Equal(t, "status: degraded", st.BrokerHealthReason)
}

// TestProbeServerStatus_CombinedModeHubDegradedBrokerHealthy: the converse —
// a hub-side problem stays on the Hub line (from the nested hub) and the Web
// line, not the broker's.
func TestProbeServerStatus_CombinedModeHubDegradedBrokerHealthy(t *testing.T) {
	webSrv := serveHealth(t, `{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"database":"healthy","colocated_broker":"unhealthy: registration failed"}},"broker":{"status":"healthy","checks":{"docker":"available"}}}`)
	brokerSrv := serveHealth(t, `{"status":"healthy","checks":{"docker":"available"}}`)

	client := &http.Client{Timeout: 2 * time.Second}
	status := probeServerStatus(client, webSrv.URL, unreachableHTTPURL(t), brokerSrv.URL)

	assert.True(t, status.HubRunning)
	assert.Equal(t, "degraded", status.HubStatus)
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", status.HubHealthReason)
	assert.Equal(t, "colocated_broker: unhealthy: registration failed", status.WebHealthReason)
	assert.Empty(t, status.BrokerHealthReason)
}
