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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// dispatchTestEnv prepares a fresh broker test environment with a
// provisionCapturingManager and isolated cwd/$HOME. Harness-config dirs
// written via writeHarnessConfig land in $HOME/.scion/harness-configs/
// (the global dir resolved by config.GetGlobalDir), which is the lookup
// path used by createAgent when the request has no projectPath — matching
// the broker's behavior for hub-dispatched agents on a remote broker.
//
// Returns the server, manager, and the global .scion dir so tests can
// inspect/mutate state.
func dispatchTestEnv(t *testing.T, allowContainerScript bool) (*Server, *provisionCapturingManager, string) {
	t.Helper()

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	// CWD .scion is needed because LoadEffectiveSettings is called with the
	// resolved global dir; we drop a minimal settings file there so the
	// test does not hit "no settings" code paths.
	cwdScion := filepath.Join(cwd, ".scion")
	if err := os.MkdirAll(cwdScion, 0755); err != nil {
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
	if err := os.WriteFile(filepath.Join(cwdScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Global .scion dir — where the broker resolves harness-configs when no
	// grove path is supplied.
	globalScion := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(globalScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	cfg.AllowContainerScriptHarnesses = allowContainerScript

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "mock" }}

	return New(cfg, mgr, rt), mgr, globalScion
}

// writeHarnessConfig writes a minimal harness-config dir under .scion/harness-configs/<name>.
func writeHarnessConfig(t *testing.T, dotScion, name, body string) {
	t.Helper()
	dir := filepath.Join(dotScion, "harness-configs", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// dispatchAgent posts a CreateAgentRequest with the given harness-config and
// captures the response.
func dispatchAgent(t *testing.T, srv *Server, harnessConfig string) (int, string) {
	t.Helper()
	body := `{
		"name": "dispatch-test-agent",
		"id": "agent-uuid-dispatch",
		"slug": "dispatch-test-agent",
		"provisionOnly": true,
		"config": {"template": "` + harnessConfig + `", "harnessConfig": "` + harnessConfig + `"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestDispatchLegacyBuiltinConfigAllowed(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "claude-builtin", `harness: claude
image: scion-claude:test
provisioner:
  type: builtin
  interface_version: 1
`)

	code, body := dispatchAgent(t, srv, "claude-builtin")
	if code != http.StatusCreated {
		t.Fatalf("legacy built-in dispatch expected 201, got %d: %s", code, body)
	}
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called for built-in dispatch")
	}
}

func TestDispatchContainerScriptConfigBlockedByDefault(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "scripted", `harness: claude
image: scion-claude:test
provisioner:
  type: container-script
  interface_version: 1
  command: ["python3", "/home/scion/.scion/harness/provision.py"]
`)

	code, body := dispatchAgent(t, srv, "scripted")
	if code != http.StatusForbidden {
		t.Fatalf("container-script dispatch should be 403 by default, got %d: %s", code, body)
	}
	if mgr.provisionCalled {
		t.Error("Provision must not be called when policy blocks the dispatch")
	}
	if !strings.Contains(body, "allow_container_script_harnesses") {
		t.Errorf("error body should mention the broker setting, got: %s", body)
	}
	if !strings.Contains(body, "scripted") {
		t.Errorf("error body should mention the harness-config name, got: %s", body)
	}
}

func TestDispatchContainerScriptConfigAllowedWhenEnabled(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, true)
	writeHarnessConfig(t, dotScion, "scripted-ok", `harness: claude
image: scion-claude:test
provisioner:
  type: container-script
  interface_version: 1
  command: ["python3", "/home/scion/.scion/harness/provision.py"]
`)

	code, body := dispatchAgent(t, srv, "scripted-ok")
	if code != http.StatusCreated {
		t.Fatalf("container-script dispatch with allow=true expected 201, got %d: %s", code, body)
	}
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called when policy permits the dispatch")
	}
}

// TestDispatchContainerScriptResponseBody confirms the 403 body uses the
// standard error envelope so Hub clients can parse it.
func TestDispatchContainerScriptResponseBody(t *testing.T) {
	srv, _, dotScion := dispatchTestEnv(t, false)
	writeHarnessConfig(t, dotScion, "scripted-envelope", `harness: claude
image: scion-claude:test
provisioner:
  type: container-script
  interface_version: 1
`)

	code, body := dispatchAgent(t, srv, "scripted-envelope")
	if code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", code, body)
	}

	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not a valid ErrorResponse envelope: %v\nbody: %s", err, body)
	}
	if resp.Error.Code != ErrCodeForbidden {
		t.Errorf("expected error code %q, got %q", ErrCodeForbidden, resp.Error.Code)
	}
}

// TestDispatchMissingRequiredImageTools simulates a container-script
// harness whose image is missing the declared interpreter. The current
// broker scope (Phase 3) does not pre-validate image contents — that runs
// inside `sciontool harness provision` from Phase 2. This test pins the
// expected behavior: the broker accepts the dispatch when allow=true and
// defers tool validation to the in-container provisioner. If a future
// broker-side preflight is added, this test should be updated to expect a
// pre-launch failure instead.
func TestDispatchMissingRequiredImageTools(t *testing.T) {
	srv, mgr, dotScion := dispatchTestEnv(t, true)
	writeHarnessConfig(t, dotScion, "needs-tools", `harness: claude
image: scion-claude:bare
provisioner:
  type: container-script
  interface_version: 1
  command: ["nonexistent-interpreter", "/home/scion/.scion/harness/provision.py"]
  required_image_tools:
    - nonexistent-interpreter
`)

	code, body := dispatchAgent(t, srv, "needs-tools")
	if code != http.StatusCreated {
		t.Fatalf("dispatch expected 201 (broker defers tool validation to in-container script), got %d: %s", code, body)
	}
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
}

// TestDispatchNoHarnessConfigStillWorks verifies the policy gate is a
// no-op when no harness-config is involved (e.g. legacy template-only
// dispatches). Refusing those would break every legacy create request.
func TestDispatchNoHarnessConfigStillWorks(t *testing.T) {
	srv, mgr, _ := dispatchTestEnv(t, false)

	body := `{
		"name": "no-harness-cfg",
		"provisionOnly": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("dispatch without harness-config should not trigger policy: got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
}

// TestExtractRequiredEnvKeys_ConfigDriven verifies that when a harness-
// config carries declarative auth metadata, the broker uses
// RequiredAuthEnvKeysFromConfig (Phase 3) instead of the compiled
// per-harness tables. This guards against silent regressions when the
// config-driven path diverges from the compiled fallback.
func TestExtractRequiredEnvKeys_ConfigDriven(t *testing.T) {
	srv, _, dotScion := dispatchTestEnv(t, false)

	// Harness-config with custom auth metadata: requires FOOBAR_TOKEN, not
	// the canonical ANTHROPIC_API_KEY. If the broker still used the compiled
	// table, it would ask for ANTHROPIC_API_KEY and the test would catch the
	// regression.
	writeHarnessConfig(t, dotScion, "custom-auth", `harness: claude
image: scion-claude:test
provisioner:
  type: builtin
  interface_version: 1
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["FOOBAR_TOKEN"]
`)

	req := CreateAgentRequest{
		Name: "x",
		Config: &CreateAgentConfig{
			Template:      "custom-auth",
			HarnessConfig: "custom-auth",
		},
	}
	required, _ := srv.extractRequiredEnvKeys(req)

	// Build a set for stable lookup.
	got := make(map[string]bool)
	for _, k := range required {
		got[k] = true
	}
	if !got["FOOBAR_TOKEN"] {
		t.Errorf("config-driven required keys missing FOOBAR_TOKEN, got %v", required)
	}
	if got["ANTHROPIC_API_KEY"] {
		t.Errorf("compiled-table key ANTHROPIC_API_KEY leaked into config-driven dispatch: %v", required)
	}
}

// TestExtractRequiredEnvKeys_CompiledFallback verifies the compiled table
// still drives preflight when the harness-config has no `auth:` block. This
// is the legacy path required during migration so older configs do not
// suddenly stop reporting missing credentials.
func TestExtractRequiredEnvKeys_CompiledFallback(t *testing.T) {
	srv, _, dotScion := dispatchTestEnv(t, false)

	writeHarnessConfig(t, dotScion, "claude-legacy", `harness: claude
image: scion-claude:test
`)

	req := CreateAgentRequest{
		Name: "x",
		Config: &CreateAgentConfig{
			Template:      "claude-legacy",
			HarnessConfig: "claude-legacy",
		},
	}
	required, _ := srv.extractRequiredEnvKeys(req)

	got := make(map[string]bool)
	for _, k := range required {
		got[k] = true
	}
	if !got["ANTHROPIC_API_KEY"] {
		t.Errorf("legacy config should fall back to compiled table requiring ANTHROPIC_API_KEY, got %v", required)
	}
}

// TestAgentResponseHarnessConfigRevision verifies that the broker echoes
// the harness-config revision back in the AgentResponse. This is the
// Phase 3 audit hook that lets the Hub correlate an agent with the exact
// bundle it ran.
func TestAgentResponseHarnessConfigRevision(t *testing.T) {
	resp := AgentInfoToResponse(api.AgentInfo{
		ID:                    "abc",
		Slug:                  "x",
		Name:                  "x",
		HarnessConfig:         "claude",
		HarnessConfigRevision: "sha256:deadbeef",
	})
	if resp.HarnessConfigRevision != "sha256:deadbeef" {
		t.Errorf("HarnessConfigRevision lost in response conversion: got %q", resp.HarnessConfigRevision)
	}
}
