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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newTestServerForRuntimeRemap builds a server whose broker-default runtime
// reports "docker" (matching hubDefaultPassthroughRuntimeTypes) but whose
// ForceRuntime is deliberately left unset, so resolveManagerForOpts always
// consults project settings instead of short-circuiting to the default
// manager — the same trick TestResolveManagerForOpts_ProfileWithDifferentRuntime
// uses. Tests write their own settings.yaml under a per-test project dir and
// pass its path as opts.ProjectPath / the request's "projectPath", so one
// broker fixture can serve both the "matches broker default" and "remapped"
// cases.
func newTestServerForRuntimeRemap(t *testing.T) (*Server, *mockManager) {
	t.Helper()
	// Isolate both halves of resolveManagerForOpts's settings read: HOME
	// (so "global settings" never sees a real ~/.scion) and every ambient
	// SCION_* variable (so koanf's env provider never merges a colliding
	// process var into VersionedSettings — e.g. SCION_AUTO_EXPOSE_PORTS
	// maps to the struct-typed "auto_expose_ports" key and makes
	// LoadEffectiveSettings fail to decode, which resolveManagerForOpts
	// silently treats as "no settings" and falls back to the broker's own
	// default runtime instead of this test's remap). See clearSCIONEnv.
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	// Deliberately NOT set to "mock"/"docker": an empty ForceRuntime means
	// resolveManagerForOpts never takes its early-return branch and always
	// resolves against project-effective settings, exactly like a real
	// broker with no forced runtime override.

	mgr := &mockManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	return New(cfg, mgr, rt), mgr
}

// writeRemapSettings writes a minimal settings.yaml under dir/.scion whose
// "local" profile (and active_profile) resolves to runtimeType, so a test
// can set the resolved runtime type for this dispatch independently of
// whatever type the hub's own default-passthrough gate assumed.
func writeRemapSettings(t *testing.T, runtimeType string) string {
	t.Helper()
	dir := t.TempDir()
	dotScion := filepath.Join(dir, ".scion")
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := "schema_version: \"1\"\n" +
		"active_profile: local\n" +
		"profiles:\n" +
		"    local:\n" +
		"        runtime: " + runtimeType + "\n" +
		"runtimes:\n" +
		"    " + runtimeType + ":\n" +
		"        type: " + runtimeType + "\n"
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	return dotScion
}

// TestBuildStartContext_HubDefaultPassthroughDowngradedOnRuntimeRemap covers
// the create path: a passthrough grant flagged as RequireLocalRuntime, where
// this dispatch's project-effective settings resolve the profile to a
// non-local-container runtime, must downgrade to block.
func TestBuildStartContext_HubDefaultPassthroughDowngradedOnRuntimeRemap(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-remap-downgrade",
		ProjectPath: projectPath,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode:        "passthrough",
				RequireLocalRuntime: true,
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block' after the runtime remap, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_HOST='localhost:18380' once downgraded, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
	if sc.Opts.Env["GCE_METADATA_ROOT"] != "localhost:18380" {
		t.Errorf("expected GCE_METADATA_ROOT='localhost:18380' once downgraded, got %q", sc.Opts.Env["GCE_METADATA_ROOT"])
	}
}

// TestBuildStartContext_HubDefaultPassthroughKeptWhenRuntimeMatches is the
// control case: the same flagged grant, but this dispatch's project-effective
// settings resolve the profile to the same local container runtime the hub
// believed it would — passthrough must survive unchanged.
func TestBuildStartContext_HubDefaultPassthroughKeptWhenRuntimeMatches(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "docker")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-remap-kept",
		ProjectPath: projectPath,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode:        "passthrough",
				RequireLocalRuntime: true,
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected SCION_METADATA_MODE='passthrough' when the runtime matches, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST redirect for a kept passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_UnflaggedPassthroughUnaffectedByRuntimeRemap covers
// the scope boundary explicitly: a passthrough grant that is NOT flagged
// (explicit request or project-level default, never the hub-default rung)
// must survive a runtime remap completely unaffected — the broker-side
// re-check is scoped to the hub-default rung only, by construction.
func TestBuildStartContext_UnflaggedPassthroughUnaffectedByRuntimeRemap(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-unflagged-passthrough",
		ProjectPath: projectPath,
		Config: &CreateAgentConfig{
			GCPIdentity: &GCPIdentityConfig{
				MetadataMode: "passthrough",
				// RequireLocalRuntime deliberately left false/unset.
			},
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "passthrough" {
		t.Errorf("expected an unflagged passthrough to survive a runtime remap unchanged, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST redirect for an unflagged passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
	}
}

// TestBuildStartContext_HubDefaultPassthroughDowngradedFromEnvFlag covers the
// start/restart shape directly: no Config struct at all (Config is nil on
// these paths), the flag and mode arrive as resolvedEnv values instead — the
// same struct-or-env precedence SCION_METADATA_MODE itself already has.
func TestBuildStartContext_HubDefaultPassthroughDowngradedFromEnvFlag(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "kubernetes")

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-env-flag-downgrade",
		ProjectPath: projectPath,
		ResolvedEnv: map[string]string{
			"SCION_METADATA_MODE":                  "passthrough",
			"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
		},
		HTTPRequest: r,
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if sc.Opts.Env["SCION_METADATA_MODE"] != "block" {
		t.Errorf("expected the env-carried flag to downgrade to block after the remap, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
}

// newTestServerForSavedProfileRemap builds a full HTTP-routable server
// (mockManager as the broker's default, so a dispatch that never remaps
// runtime is captured without exercising a real container runtime) whose
// broker-default runtime is "docker", matching its project's active profile
// — following newTestServer's own recipe (cwd-relative .scion) for the
// template/harness-config scaffolding startAgent/restartAgent need. A
// second profile, "local", is mapped to "kubernetes" and set as agentName's
// own saved profile (agent-info.json).
//
// No ForceRuntime is set, so both resolveManagerForOpts calls in
// startAgent/restartAgent consult settings for real: the first (before the
// saved-profile lookup, inside buildStartContext) sees the empty profile
// and falls back to the active one, matching the broker's own default; the
// second (after it, in the handler itself) sees the agent's saved profile
// and does not. Production always constructs a fresh agent.Manager around a
// freshly resolved runtime for that second resolution; this fixture
// overrides srv.runtimeResolver so that resolution returns this test's own
// mock runtime (remapRuntime, returned to the caller) instead of attempting
// a real cluster client, without changing what resolveManagerForOpts does
// for any real dispatch or how its result is wired up afterward. The
// second resolution's Start() call therefore runs for real against
// remapRuntime; tests assert on the env its RunFunc observes, not on the
// outer mockManager (which only ever sees a dispatch that keeps the
// broker's default runtime).
func newTestServerForSavedProfileRemap(t *testing.T, agentName string) (*Server, *mockManager, *runtime.MockRuntime) {
	t.Helper()
	// See the comment in newTestServerForRuntimeRemap: isolate HOME and every
	// ambient SCION_* variable before either of this fixture's two
	// resolveManagerForOpts calls reads settings.
	clearSCIONEnv(t)
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
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := "schema_version: \"1\"\n" +
		"active_profile: other\n" +
		"profiles:\n" +
		"    other:\n" +
		"        runtime: docker\n" +
		"    local:\n" +
		"        runtime: kubernetes\n" +
		"runtimes:\n" +
		"    docker:\n" +
		"        type: docker\n" +
		"    kubernetes:\n" +
		"        type: kubernetes\n"
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range []string{"default", "claude"} {
		tplDir := filepath.Join(dotScion, "templates", tpl)
		if err := os.MkdirAll(tplDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: "+tpl+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, hc := range []string{"default", "claude"} {
		hcDir := filepath.Join(dotScion, "harness-configs", hc)
		if err := os.MkdirAll(hcDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: "+hc+"\nimage: test-image:"+hc+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// agentName's own saved profile is "local" — different from the
	// project's active profile ("other") that the first resolution sees.
	agentHome := config.GetAgentHomePath(dotScion, agentName)
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	infoData, err := json.Marshal(api.AgentInfo{Profile: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	// No ForceRuntime: see the doc comment above.

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: agentName, Name: agentName, ProjectPath: dotScion, Phase: "running"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)
	remapRuntime := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.runtimeResolver = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return remapRuntime
	}
	return srv, mgr, remapRuntime
}

// TestStartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers
// covers the re-check that runs after startAgent resolves the agent's own
// saved profile — a later, more specific resolution than the one
// buildStartContext itself sees. The project's active profile resolves to
// docker, matching the broker's own default, so buildStartContext's own
// resolution does not downgrade; this agent's saved profile resolves to a
// different, non-local-container runtime, and the re-check that runs after
// that second resolution must still downgrade to block. Removing the
// downgrade call after that second resolution (handlers.go) makes this
// test fail; the earlier, first-resolution check alone is not enough here.
func TestStartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1")
	var capturedEnv []string
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedEnv = config.Env
		return "mock-id", nil
	}

	body, err := json.Marshal(map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_METADATA_MODE":                  "passthrough",
			"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !slices.Contains(capturedEnv, "SCION_METADATA_MODE=block") {
		t.Errorf("expected the saved-profile resolution to downgrade to block, got env %v", capturedEnv)
	}
}

// TestDowngradeUnverifiedHubDefaultPassthrough_NilEnv proves a nil env does
// not panic even when every other condition would otherwise trigger the
// write-the-block-bundle path (requireLocalRuntime true, mode passthrough,
// and a non-local-container resolved runtime type). No production call site
// currently passes a nil env — buildStartContext always allocates one — but
// the function is meant to degrade safely rather than rely on that.
func TestDowngradeUnverifiedHubDefaultPassthrough_NilEnv(t *testing.T) {
	downgradeUnverifiedHubDefaultPassthrough(nil, nil, store.GCPMetadataModePassthrough, true, "kubernetes")
}

// TestRestartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers is
// the restart-path twin of the start-path test above.
func TestRestartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1")
	var capturedEnv []string
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedEnv = config.Env
		return "mock-id", nil
	}

	body, err := json.Marshal(map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_METADATA_MODE":                  "passthrough",
			"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !slices.Contains(capturedEnv, "SCION_METADATA_MODE=block") {
		t.Errorf("expected the saved-profile resolution to downgrade to block, got env %v", capturedEnv)
	}
}
