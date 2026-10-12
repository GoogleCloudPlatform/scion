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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

const readyTestInterval = 10 * time.Millisecond

func alwaysAlive() bool { return true }

// delayedHealthServer answers 503 until delay has passed, then a healthy
// /healthz body. withHub adds the nested hub health of the combined web port.
func delayedHealthServer(t *testing.T, delay time.Duration, withHub bool) *httptest.Server {
	t.Helper()
	readyAt := time.Now().Add(delay)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || time.Now().Before(readyAt) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if withHub {
			_, _ = w.Write([]byte(`{"status":"healthy","hub":{"status":"healthy"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedURL returns a /healthz URL on a port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/healthz"
	srv.Close()
	return url
}

func TestWaitForServerListening_ReturnsOnceServerAnswers(t *testing.T) {
	const delay = 150 * time.Millisecond
	hubSrv := delayedHealthServer(t, delay, true)
	brokerSrv := delayedHealthServer(t, delay/2, false)
	targets := []serverReadyTarget{
		{Name: "hub API and web", URL: hubSrv.URL + "/healthz", NeedsHub: true},
		{Name: "runtime broker", URL: brokerSrv.URL + "/healthz"},
	}

	start := time.Now()
	err := waitForServerListening(&http.Client{Timeout: time.Second}, targets, 5*time.Second, readyTestInterval, 0, alwaysAlive)
	if err != nil {
		t.Fatalf("waitForServerListening: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("returned after %s, before the server answered (%s)", elapsed, delay)
	}
	// A request made right after the wait succeeds.
	resp, err := http.Get(hubSrv.URL + "/healthz")
	if err != nil {
		t.Fatalf("request after wait: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request after wait: HTTP %d", resp.StatusCode)
	}
}

func TestWaitForServerListening_TimesOutWhenNeverAnswering(t *testing.T) {
	never := delayedHealthServer(t, time.Hour, false)
	target := serverReadyTarget{Name: "hub API", URL: never.URL + "/healthz"}
	const timeout = 150 * time.Millisecond

	start := time.Now()
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, timeout, readyTestInterval, 0, alwaysAlive)
	elapsed := time.Since(start)

	var notReady *serverNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("err = %v, want *serverNotReadyError", err)
	}
	if elapsed < timeout || elapsed > timeout+2*time.Second {
		t.Fatalf("returned after %s, want about %s", elapsed, timeout)
	}
	msg := daemonReadyError(err, "/logs/server.log", alwaysAlive).Error()
	for _, want := range []string{"did not become ready within 150ms", "hub API", target.URL, "HTTP 503", "still running", "/logs/server.log", "scion server stop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestWaitForServerListening_ProcessExitsEarly(t *testing.T) {
	var calls atomic.Int32
	alive := func() bool { return calls.Add(1) < 3 }
	target := serverReadyTarget{Name: "hub API", URL: closedURL(t)}

	start := time.Now()
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 10*time.Second, readyTestInterval, 0, alive)
	if !errors.Is(err, errServerExitedDuringStartup) {
		t.Fatalf("err = %v, want errServerExitedDuringStartup", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s; an exited process must end the wait early", elapsed)
	}
	msg := daemonReadyError(err, "/logs/server.log", alive).Error()
	if !strings.Contains(msg, "exited during startup") || !strings.Contains(msg, "/logs/server.log") || strings.Contains(msg, "did not become ready within") {
		t.Errorf("unexpected error message: %q", msg)
	}
}

func TestWaitForServerListening_StaleServerAnswersThenProcessExits(t *testing.T) {
	// Something already on the port answers at once, but the started process
	// exits shortly after (its own port check failed): not ready.
	stale := delayedHealthServer(t, 0, false)
	target := serverReadyTarget{Name: "hub API", URL: stale.URL + "/healthz"}
	const settle = 100 * time.Millisecond
	exitAt := time.Now().Add(settle / 2)
	alive := func() bool { return time.Now().Before(exitAt) }

	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 5*time.Second, readyTestInterval, settle, alive)
	if !errors.Is(err, errServerExitedDuringStartup) {
		t.Fatalf("err = %v, want errServerExitedDuringStartup", err)
	}
}

func TestWaitForServerListening_SettleWithLiveProcessIsReady(t *testing.T) {
	srv := delayedHealthServer(t, 0, false)
	target := serverReadyTarget{Name: "hub API", URL: srv.URL + "/healthz"}
	const settle = 50 * time.Millisecond

	start := time.Now()
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 5*time.Second, readyTestInterval, settle, alwaysAlive)
	if err != nil {
		t.Fatalf("waitForServerListening: %v", err)
	}
	if elapsed := time.Since(start); elapsed < settle {
		t.Fatalf("returned after %s, before the settle delay (%s)", elapsed, settle)
	}
}

func TestWaitForServerListening_CombinedPortNeedsHub(t *testing.T) {
	// The web port answers, but without the hub's health: not ready.
	webOnly := delayedHealthServer(t, 0, false)
	target := serverReadyTarget{Name: "hub API and web", URL: webOnly.URL + "/healthz", NeedsHub: true}
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 50*time.Millisecond, readyTestInterval, 0, alwaysAlive)
	var notReady *serverNotReadyError
	if !errors.As(err, &notReady) || !strings.Contains(err.Error(), "hub API not yet mounted") {
		t.Fatalf("err = %v, want a timeout naming the missing hub", err)
	}
}

func TestServerReadyTargetsFromArgs(t *testing.T) {
	cfgWithPorts := config.DefaultGlobalConfig()
	cfgWithPorts.Hub.Port = 9911
	cfgWithPorts.Hub.Host = "0.0.0.0"
	cfgWithPorts.RuntimeBroker.Port = 9901
	cfgWithPorts.RuntimeBroker.Host = "0.0.0.0"

	cfgHosted := config.DefaultGlobalConfig()
	cfgHosted.Mode = "hosted"
	cfgHosted.Hub.Port = 9811

	cfgHostedBroker := config.DefaultGlobalConfig()
	cfgHostedBroker.Hub.Host = "127.0.0.1"
	cfgHostedBroker.RuntimeBroker.Enabled = true
	cfgHostedBroker.RuntimeBroker.Host = "127.0.0.1"
	cfgHostedBroker.RuntimeBroker.Port = 9802

	base := []string{"server", "start", "--foreground"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	tests := []struct {
		name string
		args []string
		mode string
		cfg  *config.GlobalConfig
		// noRegistry: no image registry is configured.
		noRegistry bool
		want       []serverReadyTarget
	}{
		{
			name: "workstation combo",
			args: with("--enable-hub", "--enable-runtime-broker", "--enable-web", "--dev-auth"),
			want: []serverReadyTarget{
				{Name: "hub API and web", URL: "http://127.0.0.1:8080/healthz", NeedsHub: true},
				{Name: "runtime broker", URL: "http://127.0.0.1:9800/healthz"},
			},
		},
		{
			name: "workstation defaults from a minimal restart argv",
			args: base,
			cfg:  &cfgWithPorts,
			want: []serverReadyTarget{
				{Name: "hub API and web", URL: "http://127.0.0.1:8080/healthz", NeedsHub: true},
				{Name: "runtime broker", URL: "http://127.0.0.1:9901/healthz"},
			},
		},
		{
			name: "workstation without web uses the hub port",
			args: with("--enable-web=false", "--web-port=9000"),
			cfg:  &cfgWithPorts,
			want: []serverReadyTarget{
				{Name: "hub API", URL: "http://127.0.0.1:9911/healthz"},
				{Name: "runtime broker", URL: "http://127.0.0.1:9901/healthz"},
			},
		},
		{
			name: "hosted hub only from config ports, wildcard bind probed on loopback",
			args: with("--hosted", "--enable-hub"),
			cfg:  &cfgWithPorts,
			want: []serverReadyTarget{{Name: "hub API", URL: "http://127.0.0.1:9911/healthz"}},
		},
		{
			name: "hosted via server.mode, hub only, flag port and host",
			args: with("--enable-hub", "--port=9500", "--host=10.1.2.3"),
			mode: "hosted",
			want: []serverReadyTarget{{Name: "hub API", URL: "http://10.1.2.3:9500/healthz"}},
		},
		{
			name: "hosted broker only",
			args: with("--hosted", "--enable-runtime-broker", "--runtime-broker-port=9700"),
			want: []serverReadyTarget{{Name: "runtime broker", URL: "http://127.0.0.1:9700/healthz"}},
		},
		{
			name: "hosted web only, IPv6 host",
			args: with("--hosted", "--enable-web", "--host=::1", "--web-port=8443"),
			want: []serverReadyTarget{{Name: "web", URL: "http://[::1]:8443/healthz"}},
		},
		{
			name: "explicit --hosted=false overrides server.mode",
			args: with("--hosted=false"),
			mode: "hosted",
			want: []serverReadyTarget{
				{Name: "hub API and web", URL: "http://127.0.0.1:8080/healthz", NeedsHub: true},
				{Name: "runtime broker", URL: "http://127.0.0.1:9800/healthz"},
			},
		},
		{
			name:       "workstation without an image registry skips the broker",
			args:       with("--enable-hub", "--enable-runtime-broker", "--enable-web"),
			noRegistry: true,
			want: []serverReadyTarget{
				{Name: "hub API and web", URL: "http://127.0.0.1:8080/healthz", NeedsHub: true},
			},
		},
		{
			name:       "workstation broker only without an image registry still waits on the broker",
			args:       with("--enable-hub=false", "--enable-web=false"),
			noRegistry: true,
			want:       []serverReadyTarget{{Name: "runtime broker", URL: "http://127.0.0.1:9800/healthz"}},
		},
		{
			name:       "hosted ignores the image registry",
			args:       with("--hosted", "--enable-hub", "--enable-runtime-broker"),
			noRegistry: true,
			want: []serverReadyTarget{
				{Name: "hub API", URL: "http://127.0.0.1:9810/healthz"},
				{Name: "runtime broker", URL: "http://127.0.0.1:9800/healthz"},
			},
		},
		{
			name: "hosted from cfg.Mode (env or --config), fallback mode ignored",
			args: with("--enable-hub"),
			cfg:  &cfgHosted,
			mode: "workstation",
			want: []serverReadyTarget{{Name: "hub API", URL: "http://127.0.0.1:9811/healthz"}},
		},
		{
			name: "hosted broker enabled only in config",
			args: with("--hosted", "--enable-hub"),
			cfg:  &cfgHostedBroker,
			want: []serverReadyTarget{
				{Name: "hub API", URL: "http://127.0.0.1:9810/healthz"},
				{Name: "runtime broker", URL: "http://127.0.0.1:9802/healthz"},
			},
		},
		{
			name: "hosted --enable-runtime-broker=false overrides config",
			args: with("--hosted", "--enable-hub", "--enable-runtime-broker=false"),
			cfg:  &cfgHostedBroker,
			want: []serverReadyTarget{{Name: "hub API", URL: "http://127.0.0.1:9810/healthz"}},
		},
		{
			name: "hosted --host applies to the hub only",
			args: with("--hosted", "--enable-hub", "--host=10.1.2.3"),
			cfg:  &cfgHostedBroker,
			want: []serverReadyTarget{
				{Name: "hub API", URL: "http://10.1.2.3:9810/healthz"},
				{Name: "runtime broker", URL: "http://127.0.0.1:9802/healthz"},
			},
		},
		{
			name: "workstation --host applies to the hub only",
			args: with("--host=10.1.2.3"),
			cfg:  &cfgWithPorts,
			want: []serverReadyTarget{
				{Name: "hub API and web", URL: "http://10.1.2.3:8080/healthz", NeedsHub: true},
				{Name: "runtime broker", URL: "http://127.0.0.1:9901/healthz"},
			},
		},
		{
			name: "hosted with nothing enabled",
			args: with("--hosted"),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registryCheck := func() error { return nil }
			if tt.noRegistry {
				registryCheck = func() error { return errors.New("image registry not configured") }
			}
			got := serverReadyTargetsFromArgs(tt.args, tt.cfg, tt.mode, registryCheck)
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("target %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestServerConfigPathFromArgs(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"server", "start", "--foreground"}, ""},
		{[]string{"server", "start", "--foreground", "--config=/etc/scion/settings.yaml"}, "/etc/scion/settings.yaml"},
		{[]string{"server", "start", "-c", "/x.yaml"}, "/x.yaml"},
	} {
		if got := serverConfigPathFromArgs(tt.args); got != tt.want {
			t.Errorf("serverConfigPathFromArgs(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// TestDaemonRegistryCheck_UsesGlobalDirNotCwdProject: the daemon child runs
// in the global directory, so an image registry set only in the project the
// user started from does not keep its broker on. The wait must use that
// global-scoped answer and not wait for a broker the child skips.
func TestDaemonRegistryCheck_UsesGlobalDirNotCwdProject(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	for _, k := range []string{"SCION_IMAGE_REGISTRY", "SCION_MAINTENANCE_IMAGE_REGISTRY"} {
		t.Setenv(k, "")
	}
	project := t.TempDir()
	projectScion := filepath.Join(project, ".scion")
	if err := os.MkdirAll(projectScion, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nimage_registry: registry.example.com/project-only\n"
	if err := os.WriteFile(filepath.Join(projectScion, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	// The working-directory check sees the project-only registry ...
	if err := requireImageRegistryForBroker(); err != nil {
		t.Fatalf("cwd-scoped check should see the project registry: %v", err)
	}
	// ... the child's (global-scoped) check does not.
	check := daemonRegistryCheck(globalDir)
	if err := check(); err == nil {
		t.Fatal("global-scoped check must not see a project-only registry")
	}
	args := []string{"server", "start", "--foreground", "--enable-hub", "--enable-runtime-broker", "--enable-web"}
	targets := serverReadyTargetsFromArgs(args, nil, "", check)
	for _, tg := range targets {
		if tg.Name == "runtime broker" {
			t.Fatalf("targets %+v include the broker the child skips", targets)
		}
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only the web port", targets)
	}
}

// healthBodyServer serves body on /healthz and returns its port.
func healthBodyServer(t *testing.T, body string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// runServerStatusJSON runs 'server status --json' and returns its answer.
func runServerStatusJSON(t *testing.T) serverStatusInfo {
	t.Helper()
	savedJSON := serverStatusJSON
	t.Cleanup(func() { serverStatusJSON = savedJSON })
	serverStatusJSON = true
	out := captureStdout(t, func() {
		if err := runServerStatus(serverStatusCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	var info serverStatusInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("status output %q: %v", out, err)
	}
	return info
}

// markServerDaemonRunning makes the server daemon look running: its PID file
// names this test process.
func markServerDaemonRunning(t *testing.T, globalDir string) {
	t.Helper()
	if err := daemon.WritePIDComponent(serverDaemonComponent, globalDir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
}

// TestServerStatus_ProbesStartedWebAndBrokerPorts: 'server status' finds a
// running daemon started with non-default --web-port and
// --runtime-broker-port (its saved start args), not only one on 8080.
func TestServerStatus_ProbesStartedWebAndBrokerPorts(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	t.Setenv("SCION_SERVER_MODE", "")
	markServerDaemonRunning(t, globalDir)
	webPort := healthBodyServer(t, `{"status":"healthy","hub":{"status":"healthy"}}`)
	brokerPort := newFakeBrokerHealthServer(t, "healthy")
	args := []string{"server", "start", "--foreground",
		"--web-port=" + strconv.Itoa(webPort), "--runtime-broker-port=" + strconv.Itoa(brokerPort)}
	if err := daemon.SaveArgs(serverDaemonComponent, globalDir, args); err != nil {
		t.Fatal(err)
	}

	info := runServerStatusJSON(t)
	if !info.WebRunning || !info.HubRunning {
		t.Errorf("web and hub on web port %d not found: %+v", webPort, info)
	}
	if !info.BrokerRunning {
		t.Errorf("broker on port %d not found: %+v", brokerPort, info)
	}
}

// TestServerStatus_ProbesStartedHubPort: a standalone hub daemon started with a
// non-default --port (web disabled) is found on that port, not only on 9810.
func TestServerStatus_ProbesStartedHubPort(t *testing.T) {
	_, globalDir := brokerTestHome(t)
	t.Setenv("SCION_SERVER_MODE", "")
	markServerDaemonRunning(t, globalDir)
	hubPort := healthBodyServer(t, `{"status":"healthy"}`)
	args := []string{"server", "start", "--foreground", "--enable-web=false",
		"--port=" + strconv.Itoa(hubPort), "--web-port=" + strconv.Itoa(unusedPort(t))}
	if err := daemon.SaveArgs(serverDaemonComponent, globalDir, args); err != nil {
		t.Fatal(err)
	}

	info := runServerStatusJSON(t)
	if !info.HubRunning {
		t.Errorf("hub on port %d not found: %+v", hubPort, info)
	}
	if info.WebRunning {
		t.Errorf("web reported running with nothing on the web port: %+v", info)
	}
}

func TestServerStatusBaseURLs(t *testing.T) {
	saveArgs := func(t *testing.T, globalDir string, extra ...string) {
		t.Helper()
		args := append([]string{"server", "start", "--foreground"}, extra...)
		if err := daemon.SaveArgs(serverDaemonComponent, globalDir, args); err != nil {
			t.Fatal(err)
		}
	}
	allPorts := []string{"--web-port=18080", "--port=19810", "--runtime-broker-port=19800"}
	tests := []struct {
		name     string
		settings string
		args     []string // saved start args; nil saves none
		running  bool
		want     [3]string // web, hub, broker
	}{
		{"no saved args: defaults", "", nil, false,
			[3]string{"http://127.0.0.1:8080", "http://127.0.0.1:9810", "http://127.0.0.1:9800"}},
		{"running daemon: saved args set every port", "", allPorts, true,
			[3]string{"http://127.0.0.1:18080", "http://127.0.0.1:19810", "http://127.0.0.1:19800"}},
		// The args of an earlier daemon outlive 'server stop'; a foreground
		// server started since then listens on the default ports.
		{"saved args but no running daemon: defaults", "", allPorts, false,
			[3]string{"http://127.0.0.1:8080", "http://127.0.0.1:9810", "http://127.0.0.1:9800"}},
		{"saved args but no running daemon: settings ports",
			"schema_version: \"1\"\nserver:\n  hub:\n    port: 19811\n  broker:\n    port: 19801\n", allPorts, false,
			[3]string{"http://127.0.0.1:8080", "http://127.0.0.1:19811", "http://127.0.0.1:19801"}},
		{"settings ports without saved args",
			"schema_version: \"1\"\nserver:\n  hub:\n    port: 19811\n  broker:\n    port: 19801\n", nil, false,
			[3]string{"http://127.0.0.1:8080", "http://127.0.0.1:19811", "http://127.0.0.1:19801"}},
		{"running daemon: --host ::1", "", []string{"--host=::1"}, true,
			[3]string{"http://[::1]:8080", "http://[::1]:9810", "http://127.0.0.1:9800"}},
		{"hosted: settings hub host",
			"schema_version: \"1\"\nserver:\n  mode: hosted\n  hub:\n    host: 192.0.2.10\n    port: 19812\n", nil, false,
			[3]string{"http://192.0.2.10:8080", "http://192.0.2.10:19812", "http://127.0.0.1:9800"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, globalDir := brokerTestHome(t)
			t.Setenv("SCION_SERVER_MODE", "")
			if tt.settings != "" {
				writeGlobalSettings(t, globalDir, tt.settings)
			}
			if tt.args != nil {
				saveArgs(t, globalDir, tt.args...)
			}
			if tt.running {
				markServerDaemonRunning(t, globalDir)
			}
			web, hub, broker := serverStatusBaseURLs(globalDir, tt.running)
			if got := [3]string{web, hub, broker}; got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
