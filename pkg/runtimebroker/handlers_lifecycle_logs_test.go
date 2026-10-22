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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestStopAgent(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/stop", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status %d, got %d", http.StatusAccepted, w.Code)
	}
}

func TestRestartAgent(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
	}
	if mgr.LastStartOpts().Name != "test-agent-1" {
		t.Fatalf("expected restart to start agent 'test-agent-1', got %q", mgr.LastStartOpts().Name)
	}
}

func TestRestartAgent_StartFailure(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("boom")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
	}
}

func TestRestartAgent_StopFailureTolerated(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	// Simulate podman returning an error when stopping an already-exited container
	mgr.stopErr = fmt.Errorf("podman stop test-agent-1 failed: exit status 125: Error: can only stop running containers: test-agent-1 is not running")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Restart should succeed despite the stop error — it's tolerable
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
	}
}

func TestRestartAgent_BrokerModeSet(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.LastStartOpts().BrokerMode {
		t.Fatalf("expected BrokerMode to be true in restart start options")
	}
}

func TestIsContainerStopTolerable(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"not found", fmt.Errorf("podman stop foo failed: exit status 1: Error: no container with name or ID \"foo\" found: no such container"), true},
		{"no such", fmt.Errorf("docker stop foo failed: exit status 1: Error response from daemon: No such container: foo"), true},
		{"exit status 125", fmt.Errorf("podman stop foo failed: exit status 125"), true},
		{"not running", fmt.Errorf("podman stop foo failed: exit status 125: Error: can only stop running containers: foo is not running"), true},
		{"generic failure", fmt.Errorf("podman stop foo failed: exit status 1: unexpected error"), false},
		{"permission denied", fmt.Errorf("podman stop foo failed: exit status 1: Error: permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isContainerStopTolerable(tt.err)
			if result != tt.expected {
				t.Errorf("isContainerStopTolerable(%q) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)

	// PUT on /api/v1/agents should not be allowed
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestAgentLogsAllowsGet(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent-1/logs", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "mock logs" {
		t.Fatalf("expected body %q, got %q", "mock logs", body)
	}
}

func TestAgentLogsReadsFileWhenSlugEmpty(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Create agent.log at the path derived from Name (not Slug)
	agentHome := filepath.Join(dotScion, "agents", "my-agent", "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	logContent := "hello from agent.log"
	if err := os.WriteFile(filepath.Join(agentHome, "agent.log"), []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}

	getLogsCalled := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		GetLogsFunc: func(_ context.Context, _ string) (string, error) {
			getLogsCalled = true
			return "", fmt.Errorf("should not be called")
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:          "container-abc",
				Name:        "my-agent",
				Slug:        "",       // empty slug — handler must fall back to Name
				ProjectPath: dotScion, // matches production: ProjectPath is the resolved .scion directory
				Phase:       "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/my-agent/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != logContent {
		t.Fatalf("expected body %q, got %q", logContent, body)
	}
	if getLogsCalled {
		t.Fatal("runtime.GetLogs should not have been called when agent.log is readable")
	}
}

func TestAgentLogsFallbackUsesContainerID(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// No agent.log on disk — forces fallback to container logs
	var receivedID string
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		GetLogsFunc: func(_ context.Context, id string) (string, error) {
			receivedID = id
			return "container log output", nil
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:          "myproject--foo", // project-prefixed container name
				ContainerID: "myproject--foo",
				Name:        "foo",
				Slug:        "",
				Phase:       "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	// Request uses the slug "foo", not the full container ID
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/foo/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if receivedID != "myproject--foo" {
		t.Fatalf("expected GetLogs to receive container ID %q, got %q", "myproject--foo", receivedID)
	}
	if body := w.Body.String(); body != "container log output" {
		t.Fatalf("expected body %q, got %q", "container log output", body)
	}
}

// TestAgentLogsRuntimeNotSupported verifies that a runtime declining logs
// outright (runtime.ErrLogsNotSupported — a runtime's contract) maps to an
// explicit 501 with the runtime_logs_unsupported code and the
// sentinel's own fixed message body, even when the runtime wraps the
// sentinel with extra identifying text: errors.Is matches through a wrap,
// so the response must use the sentinel's own Error() text, never the
// wrapped error's, or a wrapper could leak an id through this path.
func TestAgentLogsRuntimeNotSupported(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// No agent.log on disk — forces fallback to the runtime's GetLogs, the
	// logs-declined path this fixture stands in for. The wrap carries text
	// that must never reach the response body.
	const wrappedIdentifier = "scion-hostile-namespace/leaked-actor-name"
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "noattach" },
		GetLogsFunc: func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("noattach: get actor %s: %w", wrappedIdentifier, runtime.ErrLogsNotSupported)
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:    "noattach-actor",
				Name:  "noattach-actor",
				Slug:  "",
				Phase: "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/noattach-actor/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusNotImplemented, w.Code, w.Body.String())
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body: %s", err, w.Body.String())
	}
	if resp.Error.Code != ErrCodeRuntimeLogsUnsupported {
		t.Errorf("expected code %q, got %q", ErrCodeRuntimeLogsUnsupported, resp.Error.Code)
	}
	if resp.Error.Message != runtime.ErrLogsNotSupported.Error() {
		t.Errorf("expected message %q, got %q", runtime.ErrLogsNotSupported.Error(), resp.Error.Message)
	}

	body := w.Body.String()
	if strings.Contains(body, "noattach-actor") {
		t.Errorf("response body leaks the agent/actor name: %s", body)
	}
	if strings.Contains(body, wrappedIdentifier) {
		t.Errorf("response body leaks the wrapped identifier: %s", body)
	}
}
