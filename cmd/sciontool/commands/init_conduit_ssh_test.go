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

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
)

// countSSHBuilds counts newConduitSSH calls for the test.
func countSSHBuilds(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := newConduitSSH
	newConduitSSH = func(u conduit.PTYUser, env func() ([]string, error), getenv func(string) string) (*sshd.Config, error) {
		n.Add(1)
		return orig(u, env, getenv)
	}
	t.Cleanup(func() { newConduitSSH = orig })
	return &n
}

// runPortForwarding runs p until the test ends.
func runPortForwarding(t *testing.T, p portForwarding) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(pfWait):
			t.Error("run did not return after cancel")
		}
	})
}

// TestConduitSSHOnlyWithConduit: with hub.conduit off no SSH server (and
// no host key) is created, nothing is reported and nothing is advertised.
// With it on, one server is created, its host key is reported in exactly
// one status POST carrying only sshHostKey (the server's public line),
// and the Hello advertises the ssh kind.
func TestConduitSSHOnlyWithConduit(t *testing.T) {
	t.Run("conduit off", func(t *testing.T) {
		n := countSSHBuilds(t)
		h := newPFHub(t, true)
		client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
		env := map[string]string{projectkeys.EnvProjectID: "project-1", conduit.EnvLaunchID: "launch-1"}
		getenv := func(k string) string { return env[k] }
		ssh := prepareConduitSSH(client, true, conduit.PTYUser{}, nil, getenv)
		if ssh != nil {
			t.Fatal("SSH server prepared with conduit off")
		}
		runPortForwarding(t, newPortForwarding(client, false, conduit.PTYUser{}, ssh, getenv))
		waitSignal(t, h.tunnel, "the legacy tunnel")
		if got := n.Load(); got != 0 {
			t.Fatalf("SSH server built %d times with conduit off", got)
		}
		if h.conduitHits.Load() != 0 {
			t.Fatal("conduit dialed with conduit off")
		}
		if posts := h.statusPosts(); len(posts) != 0 {
			t.Fatalf("%d status POSTs with conduit off, want none", len(posts))
		}
	})
	t.Run("conduit disabled", func(t *testing.T) {
		n := countSSHBuilds(t)
		h := newPFHub(t, true)
		client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
		env := map[string]string{conduit.EnvHubExperiments: conduit.ExperimentConduit}
		if ssh := prepareConduitSSH(client, false, conduit.PTYUser{}, nil, func(k string) string { return env[k] }); ssh != nil || n.Load() != 0 {
			t.Fatal("SSH server prepared with conduit disabled")
		}
		if posts := h.statusPosts(); len(posts) != 0 {
			t.Fatalf("%d status POSTs with conduit disabled, want none", len(posts))
		}
	})
	t.Run("conduit on", func(t *testing.T) {
		t.Cleanup(hub.SetTokenHome(t.TempDir()))
		n := countSSHBuilds(t)
		h := newPFHub(t, true)
		client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
		env := map[string]string{
			conduit.EnvHubExperiments: conduit.ExperimentConduit,
			projectkeys.EnvProjectID:  "project-1",
			conduit.EnvLaunchID:       "launch-1",
		}
		getenv := func(k string) string { return env[k] }
		ssh := prepareConduitSSH(client, true, conduit.PTYUser{}, nil, getenv)
		if ssh == nil {
			t.Fatal("no SSH server with conduit on")
		}
		if got := n.Load(); got != 1 {
			t.Fatalf("SSH server built %d times, want 1", got)
		}
		// The first report is made before prepareConduitSSH returns (the
		// hub answers at once).
		posts := h.statusPosts()
		if len(posts) != 1 {
			t.Fatalf("%d status POSTs, want exactly 1", len(posts))
		}
		want := sshd.AuthorizedKey(ssh.HostKey)
		if len(posts[0]) != 1 || posts[0]["sshHostKey"] != want {
			t.Fatalf("status POST %v, want only sshHostKey=%q", posts[0], want)
		}
		runPortForwarding(t, newPortForwarding(client, false, conduit.PTYUser{}, ssh, getenv))
		select {
		case hello := <-h.hellos:
			if kinds := hello.GetCapabilities().GetStreamKinds(); !slices.Contains(kinds, grant.StreamKindSSH) {
				t.Fatalf("Hello stream kinds %v lack ssh", kinds)
			}
		case <-time.After(pfWait):
			t.Fatal("no conduit Hello")
		}
		if posts := h.statusPosts(); len(posts) != 1 {
			t.Fatalf("%d status POSTs after the dialer started, want still 1", len(posts))
		}
	})
}

// TestPrepareConduitSSHFirstReportBounded: the first host key report runs
// before prepareConduitSSH returns (and so before the harness starts), but
// waits at most conduitSSHFirstReportWait for a hub that does not answer;
// the SSH server is still returned.
func TestPrepareConduitSSHFirstReportBounded(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hung.Close() })
	orig, origBg := conduitSSHFirstReportWait, reportSSHHostKeyTimeout
	conduitSSHFirstReportWait, reportSSHHostKeyTimeout = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { conduitSSHFirstReportWait, reportSSHHostKeyTimeout = orig, origBg })
	client := hub.NewClientWithConfig(hung.URL, "token-1", "agent-1")
	env := map[string]string{conduit.EnvHubExperiments: conduit.ExperimentConduit, conduit.EnvLaunchID: "launch-1"}
	start := time.Now()
	ssh := prepareConduitSSH(client, true, conduit.PTYUser{}, nil, func(k string) string { return env[k] })
	if ssh == nil {
		t.Fatal("no SSH server when the hub does not answer")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("prepareConduitSSH took %v with a first-report bound of 300ms", d)
	}
}

// TestSSHMaxChannelsEnv: SCION_CONDUIT_SSH_MAX_CHANNELS sets the cap; an
// unset or invalid value keeps the default.
func TestSSHMaxChannelsEnv(t *testing.T) {
	for raw, want := range map[string]int{"": 32, "8": 8, "64": 64, "0": 32, "-1": 32, "x": 32} {
		got := sshMaxChannels(func(k string) string {
			if k == envSSHMaxChannels {
				return raw
			}
			return ""
		})
		if got != want {
			t.Errorf("%s=%q: cap %d, want %d", envSSHMaxChannels, raw, got, want)
		}
	}
}

// TestHarnessEnvIsSupervisorChildEnv: SSH sessions do not receive secret
// overrides; other environment matches the harness child. The SSH env is
// the harness env (supervisor.ChildEnv, same configuration) minus the
// secret-override keys: SCION_EXTRA_PATH applied, HOME, USER and LOGNAME
// for the agent user, the env overlay merged.
func TestHarnessEnvIsSupervisorChildEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SCION_EXTRA_PATH", "/opt/extra/bin")
	t.Setenv("API_SECRET", "")
	cfg := supervisor.Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"OVERLAY_ONLY": "o"},
		SecretOverrides: map[string]string{"API_SECRET": "s", "OTHER_SECRET": "t"},
	}
	cfgSSH, err := newConduitSSH(conduit.PTYUser{UID: 1000, GID: 1000, Username: "scion"}, sshSessionEnv(cfg), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfgSSH.Env()
	if err != nil {
		t.Fatal(err)
	}
	harness, err := supervisor.ChildEnv(cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	// The harness env still has the secret overrides.
	for _, kv := range []string{"API_SECRET=s", "OTHER_SECRET=t"} {
		if !slices.Contains(harness, kv) {
			t.Fatalf("harness env lacks secret override %s", envKey(kv))
		}
	}
	// The SSH env has none of their values, and no key the overrides added.
	for _, kv := range got {
		if strings.HasPrefix(kv, "OTHER_SECRET=") || kv == "API_SECRET=s" {
			t.Errorf("SSH env has secret override %s", envKey(kv))
		}
	}
	// Everything else matches the harness env.
	var want []string
	for _, kv := range harness {
		if kv == "API_SECRET=s" {
			want = append(want, "API_SECRET=")
			continue
		}
		if strings.HasPrefix(kv, "OTHER_SECRET=") {
			continue
		}
		want = append(want, kv)
	}
	if !slices.Equal(got, want) {
		// Key names only: the environment holds the test process's values.
		t.Fatalf("SSH env differs from the harness env without secret overrides; differing keys: %v", envKeyDiff(got, want))
	}
	for _, kv := range []string{"PATH=/opt/extra/bin:/usr/bin:/bin", "HOME=/home/scion", "USER=scion", "LOGNAME=scion", "OVERLAY_ONLY=o"} {
		if !slices.Contains(got, kv) {
			t.Errorf("SSH env lacks the expected value of %s", envKey(kv))
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "SCION_EXTRA_PATH=") {
			t.Errorf("SSH env keeps %s", envKey(kv))
		}
	}
}

// envKey is the key of a KEY=VALUE entry. Failure messages name keys
// only, never values.
func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// envKeyDiff lists the keys whose presence, value or position differs
// between got and want, by name only (never values).
func envKeyDiff(got, want []string) []string {
	index := func(env []string) map[string]int {
		m := make(map[string]int, len(env))
		for i, kv := range env {
			m[envKey(kv)] = i
		}
		return m
	}
	gi, wi := index(got), index(want)
	var diff []string
	for k, i := range gi {
		j, ok := wi[k]
		switch {
		case !ok:
			diff = append(diff, k+" (only in SSH env)")
		case got[i] != want[j]:
			diff = append(diff, k+" (value differs)")
		case i != j:
			diff = append(diff, k+" (position differs)")
		}
	}
	for k := range wi {
		if _, ok := gi[k]; !ok {
			diff = append(diff, k+" (missing from SSH env)")
		}
	}
	if len(diff) == 0 {
		diff = append(diff, "(duplicate keys)")
	}
	slices.Sort(diff)
	return diff
}

// TestConduitSSHHostKeyPerLaunch: the SSH server's host key is kept per
// agent launch: the same SCION_LAUNCH_ID gets the same key back (a
// container restart), a new one gets a new key.
func TestConduitSSHHostKeyPerLaunch(t *testing.T) {
	orig := conduitHostKeyDir
	conduitHostKeyDir = filepath.Join(t.TempDir(), "conduit")
	t.Cleanup(func() { conduitHostKeyDir = orig })
	build := func(launch string) string {
		t.Helper()
		cfg, err := newConduitSSH(conduit.PTYUser{}, nil, func(k string) string {
			if k == conduit.EnvLaunchID {
				return launch
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		return sshd.AuthorizedKey(cfg.HostKey)
	}
	first := build("launch-1")
	if again := build("launch-1"); again != first {
		t.Fatal("the same launch id got a different host key")
	}
	if next := build("launch-2"); next == first {
		t.Fatal("a new launch id kept the old host key")
	}
}

// fakeKeyReporter records reported keys.
type fakeKeyReporter struct {
	keys []string
	err  error
}

func (f *fakeKeyReporter) ReportSSHHostKey(_ context.Context, key string) error {
	f.keys = append(f.keys, key)
	return f.err
}

// TestReportSSHHostKey: the public line is reported once; a failed report
// is only logged.
func TestReportSSHHostKey(t *testing.T) {
	r := &fakeKeyReporter{}
	reportSSHHostKey(r, "ssh-ed25519 AAAA")
	if len(r.keys) != 1 || r.keys[0] != "ssh-ed25519 AAAA" {
		t.Fatalf("reported %v", r.keys)
	}
	failing := &fakeKeyReporter{err: errors.New("hub unavailable")}
	reportSSHHostKey(failing, "ssh-ed25519 AAAA")
	if len(failing.keys) != 1 {
		t.Fatalf("reported %d times, want 1", len(failing.keys))
	}
}

// TestRunInit_ReportsSSHHostKeyBeforeHarnessStarts: with hub.conduit on,
// RunInit reports the conduit SSH host key to the hub before it starts the
// harness child. The fake hub and the child append to one order file.
func TestRunInit_ReportsSSHHostKeyBeforeHarnessStarts(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)
	withRunGitCloneWorkspace(t, func(uid, gid int, home string, requirePrivilegeDrop bool) error { return nil })
	orderFile := filepath.Join(t.TempDir(), "order")
	var mu sync.Mutex
	appendOrder := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(orderFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("order file: %v", err)
			return
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/status") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["sshHostKey"]; ok {
				appendOrder("host-key-report")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/v1/conduit" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hubServer.Close)
	t.Setenv("SCION_HUB_ENDPOINT", hubServer.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent")
	t.Setenv(conduit.EnvHubExperiments, conduit.ExperimentConduit)
	t.Setenv(conduit.EnvLaunchID, "launch-1")

	rc := RunInit([]string{"sh", "-c", "echo harness-child >> " + orderFile}, InitRunOptions{DisableTermSignalForwarding: true})
	if rc != 0 {
		t.Fatalf("RunInit() = %d, want 0", rc)
	}
	raw, err := os.ReadFile(orderFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) < 2 || lines[0] != "host-key-report" || !slices.Contains(lines, "harness-child") {
		t.Fatalf("order = %v, want the host key report first, then the harness child", lines)
	}
}
