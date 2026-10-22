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
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestResolveManagerForOpts_NoProfile(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{Name: "test-agent"}
	mgr, _ := managerAndType(srv.resolveManagerForOpts(opts))

	// With no profile, should return the default manager
	if mgr != srv.manager {
		t.Error("expected default manager when no profile is set")
	}
}

func TestResolveManagerForOpts_ProfileNotInSettings(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:    "test-agent",
		Profile: "nonexistent-profile",
	}
	mgr, _ := managerAndType(srv.resolveManagerForOpts(opts))

	// Profile not found in settings should return the default manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile not found in settings")
	}
}

func TestResolveManagerForOpts_ProfileWithDifferentRuntime(t *testing.T) {
	// Create a temp project directory with settings that specify a different runtime
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings.yaml with a profile that specifies runtime "container"
	// (which differs from the broker's "docker" runtime)
	settingsYAML := `schema_version: "1"
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "apple",
		ProjectPath: projectPath,
	}
	mgr, _ := managerAndType(srv.resolveManagerForOpts(opts))

	// Profile specifies "container" runtime which differs from mock's "mock",
	// so we should get a different manager
	if mgr == srv.manager {
		t.Error("expected a different manager when profile specifies a different runtime")
	}
}

// TestResolveManagerForOpts_NilRuntimeResolverFallsBack proves that a nil
// srv.resolveAuxiliaryRuntime does not panic when settings resolve to a
// runtime other than the broker's default. New() always sets
// resolveAuxiliaryRuntime to agent.ResolveRuntimeWithRecord, so this only matters for
// a Server built without New() (e.g. a test literal, or some future
// construction path) — but resolveManagerForOpts falls back to that exact
// same function rather than a stand-in, so the fallback resolves
// identically to production.
//
// Isolated from ambient SCION_* env and HOME: without that isolation, an
// ambient SCION_AUTO_EXPOSE_PORTS collides with the struct-typed
// auto_expose_ports settings key, LoadEffectiveSettings fails to decode,
// and resolveManagerForOpts returns the default manager before ever
// reaching the nil-resolver fallback this test is meant to exercise.
func TestResolveManagerForOpts_NilRuntimeResolverFallsBack(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	settingsYAML := `schema_version: "1"
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	srv.resolveAuxiliaryRuntime = nil

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "apple",
		ProjectPath: projectPath,
	}

	// Must not panic.
	mgr, _ := managerAndType(srv.resolveManagerForOpts(opts))

	// Profile specifies "container" runtime which differs from mock's
	// "mock", so the fallback must have actually resolved a new manager
	// rather than silently keeping the default.
	if mgr == srv.manager {
		t.Error("expected a different manager when the nil-resolver fallback resolves a different runtime")
	}
}

func TestResolveManagerForOpts_ProfileWithSameRuntime(t *testing.T) {
	// Create a temp project directory with settings that specify the same runtime as the broker
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings with profile whose runtime matches the broker's runtime ("docker")
	settingsYAML := `schema_version: "1"
profiles:
  local:
    runtime: docker
runtimes:
  docker:
    type: docker
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "local",
		ProjectPath: projectPath,
	}
	mgr, _ := managerAndType(srv.resolveManagerForOpts(opts))

	// Profile specifies "docker" runtime which matches the broker's runtime,
	// so we should get the same manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile resolves to same runtime")
	}
}

// TestResolveManagerForOpts_EnvCollision_HonoursProjectRuntime is the
// end-to-end regression test for
// https://github.com/ptone/scion/issues/2447 ("runtimebroker: project
// runtime settings silently ignored when a colliding SCION_* env var is
// set"). SCION_AUTO_EXPOSE_PORTS is a variable the hub sets inside agent
// containers; a broker started inside one (e.g. `scion server start` run
// from an agent) inherits it in its own process environment.
// config.LoadEffectiveSettings merges every SCION_*-prefixed process env var
// through koanf, and SCION_AUTO_EXPOSE_PORTS used to map to the
// struct-typed "auto_expose_ports" key, making Unmarshal fail. Before the
// fix, resolveManagerForOpts discarded that error silently (`vs, _, _ :=`)
// and fell back to the broker's default manager/runtime — so the project's
// configured runtime was never honoured.
//
// This asserts the actual runtime of the returned manager, not merely that
// it differs from the broker's default, since a weaker `mgr != srv.manager`
// check also passes if resolution lands on some other wrong runtime.
func TestResolveManagerForOpts_EnvCollision_HonoursProjectRuntime(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// A profile remapped to a non-default runtime ("container"), active by
	// default so opts.Profile can stay empty, matching how the broker
	// resolves a project's own settings with no explicit --profile flag.
	settingsYAML := `schema_version: "1"
active_profile: apple
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	opts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectPath,
	}
	mgr, rtName := managerAndType(srv.resolveManagerForOpts(opts))

	// The broker's default runtime is "docker" (see
	// newTestServerWithProvisionCapture); the project's active profile
	// resolves to "container", so the manager actually returned must run on
	// "container" — not merely be some manager other than the default.
	if rtName != "container" {
		t.Errorf("expected resolveManagerForOpts to report runtime %q, got %q (the SCION_AUTO_EXPOSE_PORTS collision was not handled)", "container", rtName)
	}
	am, ok := mgr.(*agent.AgentManager)
	if !ok {
		t.Fatalf("expected *agent.AgentManager, got %T", mgr)
	}
	if got := am.Runtime.Name(); got != "container" {
		t.Errorf("expected the project's configured runtime %q, got %q (the SCION_AUTO_EXPOSE_PORTS collision was not handled)", "container", got)
	}

	// Decoding succeeds once the colliding var is excluded from the env
	// provider, so no warning should be logged on this path.
	if logged := logBuf.String(); strings.Contains(logged, "level=WARN") {
		t.Errorf("expected no WARN log on the successful collision path, got: %s", logged)
	}
}

// TestResolveManagerForOpts_SettingsLoadErrorIsLogged proves
// resolveManagerForOpts no longer swallows a settings load error
// silently: it must log a warning identifying the project directory and the
// error, then fall back to the broker's default manager. A malformed
// settings.yaml (not the SCION_AUTO_EXPOSE_PORTS collision, which the
// mapper fix above avoids entirely) is used to force a real load error
// deterministically.
func TestResolveManagerForOpts_SettingsLoadErrorIsLogged(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}
	// The file is not valid YAML, so the load fails. A wrong-typed value
	// (profiles as a scalar, say) no longer does: the settings loader falls
	// back to the default for that key and the load succeeds.
	malformedYAML := `schema_version: "1"
profiles: [not valid yaml
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(malformedYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	opts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectPath,
	}
	mgr, rtName := managerAndType(srv.resolveManagerForOpts(opts))

	if mgr != srv.manager {
		t.Error("expected default manager when settings fail to load")
	}
	if rtName != srv.runtime.Name() {
		t.Errorf("expected the broker's default runtime %q when settings fail to load, got %q", srv.runtime.Name(), rtName)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected a WARN-level log entry for the settings load error, got: %s", logged)
	}
	if !strings.Contains(logged, projectPath) {
		t.Errorf("expected the logged warning to include the project dir %q, got: %s", projectPath, logged)
	}
}

func TestCreateAgentWithProfile(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "profiled-agent",
		"config": {"template": "claude", "profile": "custom-profile"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.Profile != "custom-profile" {
		t.Errorf("expected Profile 'custom-profile', got %q", mgr.lastOpts.Profile)
	}
}

func TestCreateAgentWithoutProfile(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "no-profile-agent",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.Profile != "" {
		t.Errorf("expected empty Profile, got %q", mgr.lastOpts.Profile)
	}
}
