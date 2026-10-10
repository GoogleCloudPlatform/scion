// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// These tests pin that a start or restart of an existing agent whose saved
// profile cannot be resolved fails with a retryable 503 instead of running
// the agent on the broker's default runtime, while a start that names no
// profile keeps the default (ptone/scion#2709).

// lockedBuffer is a bytes.Buffer safe for a logger and a test to share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLifecycleLog sends srv's agent lifecycle log to the returned buffer.
func captureLifecycleLog(srv *Server) *lockedBuffer {
	buf := &lockedBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// secretSettingsPath stands in for the absolute settings path an OS error
// from the real loader carries.
const secretSettingsPath = "/secret/dir/settings.yaml"

// unresolvedCase is one way a saved profile can fail to resolve.
type unresolvedCase struct {
	name string
	// settings replaces the settings loader; nil uses the real loader
	// against the fixture's project, which does not define "vanished".
	settings func(string) (*config.VersionedSettings, []string, error)
	// noSettingsFile removes the project's settings.yaml first.
	noSettingsFile bool
	wantMsg        string
	// absent must not appear in the response body.
	absent []string
	// wantLog must appear in the Warn log entry.
	wantLog string
}

func unresolvedCases() []unresolvedCase {
	return []unresolvedCase{
		{name: "profile missing", wantMsg: `profile \"vanished\" not found`, wantLog: "not found"},
		{
			name: "settings load fails",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, errors.New("open " + secretSettingsPath + ": permission denied")
			},
			wantMsg: "project settings could not be loaded",
			absent:  []string{"/secret/dir", "permission denied"},
			wantLog: secretSettingsPath,
		},
		{
			name: "no settings",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, nil
			},
			wantMsg: "no project settings found",
			wantLog: "no project settings found",
		},
		{
			// The real loader layers the embedded defaults, so a project
			// with no settings file on disk fails as "profile not found".
			name:           "no settings file on disk",
			noSettingsFile: true,
			wantMsg:        `profile \"vanished\" not found`,
			wantLog:        "not found",
		},
	}
}

func (tc unresolvedCase) apply(t *testing.T, f *lifecycleFixture) {
	t.Helper()
	if tc.settings != nil {
		f.srv.loadSettings = tc.settings
	}
	if tc.noSettingsFile {
		if err := os.Remove(filepath.Join(f.projectPath, "settings.yaml")); err != nil {
			t.Fatal(err)
		}
	}
}

// assertSavedProfileUnresolved checks the 503 shape, the body and the
// single server-side log entry for tc.
func assertSavedProfileUnresolved(t *testing.T, tc unresolvedCase, w *httptest.ResponseRecorder, logs *lockedBuffer, projectPath string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != recordedRuntimeRetryAfterSeconds {
		t.Errorf("Retry-After = %q, want %q", got, recordedRuntimeRetryAfterSeconds)
	}
	body := w.Body.String()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeRuntimeUnavailable)
	}
	if !strings.Contains(body, "vanished") || !strings.Contains(body, tc.wantMsg) {
		t.Errorf("body does not name the profile and cause %q: %s", tc.wantMsg, body)
	}
	for _, s := range append([]string{projectPath}, tc.absent...) {
		if strings.Contains(body, s) {
			t.Errorf("body contains %q: %s", s, body)
		}
	}
	entries := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "saved runtime profile cannot be resolved") {
			entries++
			for _, want := range []string{"level=WARN", "agent=", "profile=vanished", "projectDir=", tc.wantLog} {
				if !strings.Contains(line, want) {
					t.Errorf("log entry missing %q: %s", want, line)
				}
			}
		}
	}
	if entries != 1 {
		t.Errorf("unresolved profile log entries = %d, want 1:\n%s", entries, logs.String())
	}
}

func TestStartAgent_UnresolvableSavedProfileReturns503(t *testing.T) {
	for _, tc := range unresolvedCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			logs := captureLifecycleLog(f.srv)
			const name = "saved-profile-agent"
			writeSavedAgentProfile(t, f.projectPath, name, "vanished")
			tc.apply(t, f)

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"projectPath": f.projectPath,
			})
			assertSavedProfileUnresolved(t, tc, w, logs, f.projectPath)
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
			if runs, _ := f.k8sRun(); runs != 0 {
				t.Errorf("kubernetes runtime runs = %d, want 0", runs)
			}
		})
	}
}

// TestRestartAgent_UnresolvableSavedProfileReturns503: the broker restart
// endpoint fails before its stop, so the running agent is left untouched.
func TestRestartAgent_UnresolvableSavedProfileReturns503(t *testing.T) {
	for _, tc := range unresolvedCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			logs := captureLifecycleLog(f.srv)
			const name = "restart-saved-profile"
			writeSavedAgentProfile(t, f.projectPath, name, "vanished")
			f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))
			tc.apply(t, f)

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{})
			assertSavedProfileUnresolved(t, tc, w, logs, f.projectPath)
			if f.defaultMgr.stopCalls != 0 || f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime used: stop=%d start=%d", f.defaultMgr.stopCalls, f.defaultMgr.StartCalls())
			}
		})
	}
}

// TestStartAgent_UnresolvableSavedProfileOnKubernetesDefaultBroker: on a
// broker whose default runtime is Kubernetes, a vanished saved profile
// with an explicit "block" GCP metadata mode returns the 503, not the
// Kubernetes/"block" 400 that classifying against the default runtime
// would give.
func TestStartAgent_UnresolvableSavedProfileOnKubernetesDefaultBroker(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	projectPath := newDockerProject(t)
	const name = "k8s-default-saved-profile"
	writeSavedAgentProfile(t, projectPath, name, "vanished")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.StateDir = t.TempDir()
	cfg.ContainerHubEndpoint = lifecycleBridge
	mgr := &filteringMockManager{}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }})
	logs := captureLifecycleLog(srv)

	w := lifecyclePost(t, srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"projectPath": projectPath,
		"hubEndpoint": lifecycleHubEndpoint,
		"resolvedEnv": map[string]string{"SCION_METADATA_MODE": "block"},
	})
	tc := unresolvedCase{wantMsg: `profile \"vanished\" not found`, wantLog: "not found"}
	assertSavedProfileUnresolved(t, tc, w, logs, projectPath)
	if mgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", mgr.StartCalls())
	}
}

// TestStartAgent_NoSavedProfileKeepsDefault: a start that names no profile
// still falls back to the default runtime, including when settings cannot
// be loaded.
func TestStartAgent_NoSavedProfileKeepsDefault(t *testing.T) {
	cases := []struct {
		name     string
		settings func(string) (*config.VersionedSettings, []string, error)
	}{
		{name: "settings resolve"},
		{
			name: "settings load fails",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, errors.New("malformed settings")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			if tc.settings != nil {
				f.srv.loadSettings = tc.settings
			}
			w := lifecyclePost(t, f.srv, "/api/v1/agents/fresh-agent/start", map[string]any{
				"projectPath": f.projectPath,
			})
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			if f.defaultMgr.StartCalls() != 1 {
				t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
			}
		})
	}
}

// TestStartAgent_ResolvableSavedProfileUsesItsRuntime: a saved profile the
// settings define still starts on that profile's runtime.
func TestStartAgent_ResolvableSavedProfileUsesItsRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "k8s-saved-agent"
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"projectPath": f.projectPath,
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Errorf("kubernetes runtime runs = %d, want 1", runs)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}

// writeSavedAgentInfo writes agent-info.json with a saved profile and the
// runtime the agent is recorded as last running on.
func writeSavedAgentInfo(t *testing.T, dotScionDir, agentName, profile, recordedRuntime string) {
	t.Helper()
	agentHome := config.GetAgentHomePath(dotScionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(api.AgentInfo{Name: agentName, Profile: profile, Runtime: recordedRuntime})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStartAgent_UnresolvableSavedProfileRecordedRuntime: when the saved
// profile cannot be resolved, an agent whose agent-info.json records the
// broker's default runtime ("docker" in the fixture) starts on the default
// runtime; a recorded non-default runtime or no recorded runtime still
// gets the 503.
func TestStartAgent_UnresolvableSavedProfileRecordedRuntime(t *testing.T) {
	for _, recorded := range []string{"docker", "kubernetes", ""} {
		for _, tc := range unresolvedCases() {
			t.Run("recorded="+recorded+"/"+tc.name, func(t *testing.T) {
				f := newLifecycleFixture(t)
				logs := captureLifecycleLog(f.srv)
				const name = "recorded-runtime-agent"
				writeSavedAgentInfo(t, f.projectPath, name, "vanished", recorded)
				tc.apply(t, f)

				w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
					"projectPath": f.projectPath,
				})
				if runs, _ := f.k8sRun(); runs != 0 {
					t.Errorf("kubernetes runtime runs = %d, want 0", runs)
				}
				if recorded != "docker" {
					assertSavedProfileUnresolved(t, tc, w, logs, f.projectPath)
					if f.defaultMgr.StartCalls() != 0 {
						t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
					}
					return
				}
				if w.Code != http.StatusAccepted {
					t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
				}
				if f.defaultMgr.StartCalls() != 1 {
					t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
				}
				out := logs.String()
				assertFallbackLoggedOnceAtWarn(t, out)
				if !strings.Contains(out, "agent last ran on the broker default runtime") ||
					!strings.Contains(out, "runtime=docker") || !strings.Contains(out, "profile=vanished") {
					t.Errorf("default-runtime fallback not logged:\n%s", out)
				}
				if strings.Contains(out, "refusing to use the broker default runtime") {
					t.Errorf("fallback logged as a refusal:\n%s", out)
				}
			})
		}
	}
}

// TestRestartAgent_UnresolvableSavedProfileRecordedDefaultRuntime: restart
// uses the same resolution, so a recorded default runtime restarts there.
func TestRestartAgent_UnresolvableSavedProfileRecordedDefaultRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	logs := captureLifecycleLog(f.srv)
	const name = "restart-recorded-default"
	writeSavedAgentInfo(t, f.projectPath, name, "vanished", "docker")
	f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	assertFallbackLoggedOnceAtWarn(t, logs.String())
	if f.defaultMgr.StartCalls() != 1 {
		t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
	}
	if runs, _ := f.k8sRun(); runs != 0 {
		t.Errorf("kubernetes runtime runs = %d, want 0", runs)
	}
}

// fallbackLogMsg is the recorded-runtime fallback's log message.
const fallbackLogMsg = "agent last ran on the broker default runtime, using it"

// assertFallbackLoggedOnceAtWarn checks that out has exactly one fallback
// log line and that it is at Warn. The capture handler drops Debug, so a
// handler-level recheck that logged at Warn would show up as a second line.
func assertFallbackLoggedOnceAtWarn(t *testing.T, out string) {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, fallbackLogMsg) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("fallback logged %d times, want 1:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("fallback not logged at Warn:\n%s", lines[0])
	}
}

// TestStartAgent_RecordedRuntimeNoFallbackForInstanceRuntimes: agent-info.json
// records only the runtime type. On a broker whose default runtime's
// identity is more than its type (Kubernetes context/namespace) or that
// has per-profile instances, a matching recorded type does not show the
// agent ran on that instance, so an unresolvable saved profile still gets
// the 503 instead of starting on the default. Restart gets the same 503
// before its stop.
func TestStartAgent_RecordedRuntimeNoFallbackForInstanceRuntimes(t *testing.T) {
	cases := []struct {
		name     string
		rt       runtime.Runtime
		recorded string
	}{
		{
			name:     "kubernetes default",
			rt:       &runtime.KubernetesRuntime{DefaultNamespace: "ns-a"},
			recorded: "kubernetes",
		},
		{
			name: "per-profile-instance default",
			rt: &perProfileRuntime{
				MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "docker" }},
				perProfile:  true,
			},
			recorded: "docker",
		},
	}
	for _, c := range cases {
		for _, action := range []string{"start", "restart"} {
			t.Run(c.name+"/"+action, func(t *testing.T) {
				clearSCIONEnv(t)
				t.Setenv("HOME", t.TempDir())
				projectPath := newDockerProject(t)
				const name = "instance-runtime-agent"
				writeSavedAgentInfo(t, projectPath, name, "vanished", c.recorded)

				cfg := DefaultServerConfig()
				cfg.BrokerID = "test-broker-id"
				cfg.BrokerName = "test-host"
				cfg.StateDir = t.TempDir()
				cfg.ContainerHubEndpoint = lifecycleBridge
				mgr := &filteringMockManager{}
				body := map[string]any{
					"projectPath": projectPath,
					"hubEndpoint": lifecycleHubEndpoint,
				}
				if action == "restart" {
					// Restart acts on a running agent and must fail
					// before it stops it.
					mgr.agents = append(mgr.agents, lifecycleAgent(name, projectPath, ""))
					body = map[string]any{}
				}
				srv := New(cfg, mgr, c.rt)
				logs := captureLifecycleLog(srv)

				w := lifecyclePost(t, srv, "/api/v1/agents/"+name+"/"+action, body)
				tc := unresolvedCase{wantMsg: `profile \"vanished\" not found`, wantLog: "not found"}
				assertSavedProfileUnresolved(t, tc, w, logs, projectPath)
				if mgr.StopCalls() != 0 || mgr.StartCalls() != 0 {
					t.Errorf("default runtime used: stop=%d start=%d", mgr.StopCalls(), mgr.StartCalls())
				}
				if strings.Contains(logs.String(), fallbackLogMsg) {
					t.Errorf("fallback taken:\n%s", logs.String())
				}
			})
		}
	}
}

// TestRestartAgent_HandlerStrictCheckWhenContainerNameDiffers: the matched
// container's Name differs from the restart id, and only id has the saved
// profile. buildStartContext reads the profile by Name, finds none and
// stays non-strict, so only restartAgent's own strict resolution (which
// reads the profile by id) can return the 503.
func TestRestartAgent_HandlerStrictCheckWhenContainerNameDiffers(t *testing.T) {
	f := newLifecycleFixture(t)
	logs := captureLifecycleLog(f.srv)
	const id = "restart-by-id"
	writeSavedAgentProfile(t, f.projectPath, id, "vanished")
	entry := lifecycleAgent(id, f.projectPath, "")
	entry.Name = "container-name"
	f.defaultMgr.agents = append(f.defaultMgr.agents, entry)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+id+"/restart", map[string]any{})
	tc := unresolvedCase{wantMsg: `profile \"vanished\" not found`, wantLog: "not found"}
	assertSavedProfileUnresolved(t, tc, w, logs, f.projectPath)
	if f.defaultMgr.stopCalls != 0 || f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime used: stop=%d start=%d", f.defaultMgr.stopCalls, f.defaultMgr.StartCalls())
	}
}
