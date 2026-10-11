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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	err := waitForServerListening(&http.Client{Timeout: time.Second}, targets, 5*time.Second, readyTestInterval, alwaysAlive)
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
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, timeout, readyTestInterval, alwaysAlive)
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
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 10*time.Second, readyTestInterval, alive)
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

func TestWaitForServerListening_CombinedPortNeedsHub(t *testing.T) {
	// The web port answers, but without the hub's health: not ready.
	webOnly := delayedHealthServer(t, 0, false)
	target := serverReadyTarget{Name: "hub API and web", URL: webOnly.URL + "/healthz", NeedsHub: true}
	err := waitForServerListening(&http.Client{Timeout: time.Second}, []serverReadyTarget{target}, 50*time.Millisecond, readyTestInterval, alwaysAlive)
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

	base := []string{"server", "start", "--foreground"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	tests := []struct {
		name string
		args []string
		mode string
		cfg  *config.GlobalConfig
		want []serverReadyTarget
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
			name: "hosted with nothing enabled",
			args: with("--hosted"),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serverReadyTargetsFromArgs(tt.args, tt.mode, tt.cfg)
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
