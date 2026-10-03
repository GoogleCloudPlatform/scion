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
// runtime that is neither a local-container runtime nor Kubernetes, must
// downgrade to block. srv.resolveAuxiliaryRuntime is overridden (the same pattern
// newTestServerForSavedProfileRemap uses) to a fictitious runtime name
// ("other") rather than "kubernetes": block is not offered on Kubernetes
// (ptone/scion#2328), so Kubernetes is excluded from this downgrade (see
// TestBuildStartContext_HubDefaultPassthroughKeptOnKubernetesRemap below), so
// a real remap to Kubernetes would no longer exercise the downgrade path this
// test is pinning.
func TestBuildStartContext_HubDefaultPassthroughDowngradedOnRuntimeRemap(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "other")
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return &runtime.MockRuntime{NameFunc: func() string { return "other" }}
	}

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

// TestBuildStartContext_HubDefaultPassthroughKeptOnKubernetesRemap: a
// hub-default passthrough grant (RequireLocalRuntime) dispatched to a
// profile that resolves to Kubernetes must NOT downgrade to block. Block is
// not offered on Kubernetes (ptone/scion#2328) — Kubernetes could not accept
// this downgrade's explicit "block" even if it produced one — and the
// broker's own runtime-aware default for Kubernetes is already "passthrough"
// anyway, so keeping the grant's passthrough unchanged produces the
// identical outcome a downgrade-to-unset would. This is reconciled with
// upstream's RequireLocalRuntime downgrade mechanism (main #2186).
// srv.resolveAuxiliaryRuntime is overridden so settings resolving to "kubernetes"
// returns a mock runtime rather than attempting a real cluster client (see
// TestExtractRequiredEnvKeys_KubernetesImplicitPassthroughSkipsADC for the
// same need in a different test file).
func TestBuildStartContext_HubDefaultPassthroughKeptOnKubernetesRemap(t *testing.T) {
	srv, _ := newTestServerForRuntimeRemap(t)
	projectPath := writeRemapSettings(t, "kubernetes")
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	}

	r := httptest.NewRequest("POST", "/api/v1/agents", nil)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-remap-kubernetes-kept",
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
		t.Errorf("expected SCION_METADATA_MODE='passthrough' to survive a remap to Kubernetes unchanged, got %q", sc.Opts.Env["SCION_METADATA_MODE"])
	}
	if sc.Opts.Env["GCE_METADATA_HOST"] != "" {
		t.Errorf("expected no GCE_METADATA_HOST redirect for a kept Kubernetes passthrough, got %q", sc.Opts.Env["GCE_METADATA_HOST"])
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
	// Remap to a fictitious non-local, non-Kubernetes runtime with a mock
	// resolver, as TestBuildStartContext_HubDefaultPassthroughDowngradedOnRuntimeRemap
	// does. "kubernetes" no longer exercises the downgrade (block is not
	// offered on Kubernetes, ptone/scion#2328), and without the override the
	// resolver builds a real cluster client: where one is reachable the
	// dispatch resolves to Kubernetes and keeps passthrough.
	projectPath := writeRemapSettings(t, "other")
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return &runtime.MockRuntime{NameFunc: func() string { return "other" }}
	}

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
// second profile, "local", is mapped to remapRuntimeName and set as
// agentName's own saved profile (agent-info.json). Callers pass "kubernetes"
// to exercise the carve-out that keeps passthrough rather than downgrading
// it (block is not offered on Kubernetes, ptone/scion#2328) or a
// non-local/non-Kubernetes name like "other" to exercise the ordinary
// downgrade-to-block path.
//
// No ForceRuntime is set, so both resolveManagerForOpts calls in
// startAgent/restartAgent consult settings for real: the first (before the
// saved-profile lookup, inside buildStartContext) sees the empty profile
// and falls back to the active one, matching the broker's own default; the
// second (after it, in the handler itself) sees the agent's saved profile
// and does not. Production always constructs a fresh agent.Manager around a
// freshly resolved runtime for that second resolution; this fixture
// overrides srv.resolveAuxiliaryRuntime so that resolution returns this test's own
// mock runtime (remapRuntime, returned to the caller) instead of attempting
// a real cluster client, without changing what resolveManagerForOpts does
// for any real dispatch or how its result is wired up afterward. The
// second resolution's Start() call therefore runs for real against
// remapRuntime; tests assert on the env its RunFunc observes, not on the
// outer mockManager (which only ever sees a dispatch that keeps the
// broker's default runtime).
func newTestServerForSavedProfileRemap(t *testing.T, agentName, remapRuntimeName string) (*Server, *mockManager, *runtime.MockRuntime) {
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
		"        runtime: " + remapRuntimeName + "\n" +
		"runtimes:\n" +
		"    docker:\n" +
		"        type: docker\n" +
		"    " + remapRuntimeName + ":\n" +
		"        type: " + remapRuntimeName + "\n"
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
	remapRuntime := &runtime.MockRuntime{NameFunc: func() string { return remapRuntimeName }}
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
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
// different runtime that is neither a local-container runtime nor
// Kubernetes ("other" — see
// TestStartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes
// below for the Kubernetes carve-out), and the re-check that runs after that
// second resolution must still downgrade to block. Removing the downgrade
// call after that second resolution (handlers.go) makes this test fail; the
// earlier, first-resolution check alone is not enough here.
func TestStartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "other")
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

// TestStartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes
// pins the same carve-out as
// TestBuildStartContext_HubDefaultPassthroughKeptOnKubernetesRemap above, one
// resolution later: at the startAgent late-recheck layer, not just
// buildStartContext's own earlier one. When the agent's own saved profile
// resolves to Kubernetes rather than docker, the re-check that runs after
// that second, authoritative resolution must NOT downgrade to block —
// Kubernetes keeps its own runtime-aware default of passthrough instead.
func TestStartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "kubernetes")
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
	if !slices.Contains(capturedEnv, "SCION_METADATA_MODE=passthrough") {
		t.Errorf("expected the saved-profile resolution to Kubernetes to keep passthrough, got env %v", capturedEnv)
	}
	if slices.ContainsFunc(capturedEnv, func(e string) bool { return strings.HasPrefix(e, "SCION_METADATA_MODE=block") }) {
		t.Errorf("expected no block mode once resolved to Kubernetes, got env %v", capturedEnv)
	}
}

// TestDowngradeUnverifiedHubDefaultPassthrough_NilEnv proves a nil env does
// not panic even when every other condition would otherwise trigger the
// write-the-block-bundle path (requireLocalRuntime true, mode passthrough,
// and a non-local-container resolved runtime type). No production call site
// currently passes a nil env — buildStartContext always allocates one — but
// the function is meant to degrade safely rather than rely on that.
func TestDowngradeUnverifiedHubDefaultPassthrough_NilEnv(t *testing.T) {
	downgradeUnverifiedHubDefaultPassthrough(nil, nil, store.GCPMetadataModePassthrough, true, "other")
}

// TestRestartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers is
// the restart-path twin of the start-path test above.
func TestRestartAgent_HubDefaultPassthroughDowngradedWhenSavedProfileDiffers(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "other")
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

// TestRestartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes
// is the restart-path twin of
// TestStartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes
// above.
func TestRestartAgent_HubDefaultPassthroughKeptWhenSavedProfileResolvesToKubernetes(t *testing.T) {
	srv, _, remapRuntime := newTestServerForSavedProfileRemap(t, "test-agent-1", "kubernetes")
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
	if !slices.Contains(capturedEnv, "SCION_METADATA_MODE=passthrough") {
		t.Errorf("expected the saved-profile resolution to Kubernetes to keep passthrough, got env %v", capturedEnv)
	}
	if slices.ContainsFunc(capturedEnv, func(e string) bool { return strings.HasPrefix(e, "SCION_METADATA_MODE=block") }) {
		t.Errorf("expected no block mode once resolved to Kubernetes, got env %v", capturedEnv)
	}
}

// newTestServerForLateCheckOrdering builds a server for pinning that the
// start/restart handlers' later, authoritative resolution — not just
// buildStartContext's own earlier one — is what the Kubernetes/block
// rejection runs against. Unlike newTestServerForSavedProfileRemap, this
// fixture deliberately makes the two resolutions see different saved
// profiles:
//
//   - The project's active profile ("other") resolves to docker; "local"
//     resolves to remapRuntimeName (Kubernetes).
//   - The mock agent record's Name is agentName, but its ContainerID and
//     scion.name label are urlID — a different string. The HTTP request
//     addresses the agent by urlID.
//   - A saved profile of "local" (Kubernetes) exists only under urlID, not
//     under agentName.
//
// On restart, the handler's own pre-buildStartContext lookup finds the
// record by its scion.name label and passes its Name (agentName) to
// buildStartContext, whose early resolution reads the saved profile under
// that Name — finding nothing, so it falls back to the active profile
// (docker) and does not reject. The handler's later, authoritative
// resolution reads the saved profile under the URL id itself
// (agent.GetSavedProfile(id, ...), handlers.go) — urlID — and finds
// Kubernetes.
//
// On start, buildStartContext's Name input is the URL id on both reads, and
// startAgent recovers the project path from the record before
// buildStartContext runs, so the early resolution already finds the
// Kubernetes profile saved under urlID and rejects there. The start test
// therefore pins that the rejection comes before Start and before the
// inline config is written, whichever resolution raises it.
func newTestServerForLateCheckOrdering(t *testing.T, agentName, urlID, remapRuntimeName string) (*Server, *mockManager, *runtime.MockRuntime) {
	t.Helper()
	// Isolate HOME and every ambient SCION_* variable, as
	// newTestServerForSavedProfileRemap does. Deliberately no os.Chdir: see
	// the doc comment above for why the start-path test depends on this
	// fixture's working directory not matching the project directory below.
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	tmpDir := t.TempDir()
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
		"        runtime: " + remapRuntimeName + "\n" +
		"runtimes:\n" +
		"    docker:\n" +
		"        type: docker\n" +
		"    " + remapRuntimeName + ":\n" +
		"        type: " + remapRuntimeName + "\n"
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

	// The saved profile lives under urlID only — not under agentName — so
	// only a resolution that looks the agent up by urlID finds Kubernetes.
	writeSavedAgentProfile(t, dotScion, urlID, "local")

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	// The scion.name label carries urlID, as a runtime labels a container
	// with the slug the hub addresses it by; the broker's agent lookup
	// filters on that label.
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: agentName, Name: agentName, ContainerID: urlID, ProjectPath: dotScion, Phase: "running",
				Labels: map[string]string{"scion.name": urlID}},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)
	remapRuntime := &runtime.MockRuntime{NameFunc: func() string { return remapRuntimeName }}
	srv.resolveAuxiliaryRuntime = func(projectPath, agentName, profileFlag string) runtime.Runtime {
		return remapRuntime
	}
	return srv, mgr, remapRuntime
}

// TestStartAgent_LateKubernetesBlockRejectionRunsBeforeStart pins the
// ordering of the start path's later, saved-profile resolution: see
// newTestServerForLateCheckOrdering's doc comment for how this fixture makes
// only that later resolution see Kubernetes. An explicit "block" must still
// be rejected with 400, before Start ever runs.
func TestStartAgent_LateKubernetesBlockRejectionRunsBeforeStart(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("Start must not run once the late Kubernetes/block rejection fires")
		return "", nil
	}

	// Seed an existing scion-agent.json so an inlineConfig update (sent
	// below) has something to write over if applyInlineConfigUpdate runs.
	// The late rejection must fire before that write, so this must come back
	// unchanged.
	dotScion := mgr.agents[0].ProjectPath
	agentDir := config.GetAgentDir(dotScion, "url-id", false)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(api.ScionConfig{Image: "original-image"})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(cfgPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_METADATA_MODE": "block",
		},
		"inlineConfig": map[string]any{
			"image": "changed-image",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/start", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read scion-agent.json after rejection: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("expected scion-agent.json to stay unchanged by applyInlineConfigUpdate once the late rejection fires first, got %s, want %s", after, original)
	}
}

// TestRestartAgent_LateKubernetesBlockRejectionRunsBeforeStop pins the same
// ordering on the restart path, and additionally that the rejection runs
// before the agent is stopped: see newTestServerForLateCheckOrdering's doc
// comment for how this fixture makes only the later resolution see
// Kubernetes. An explicit "block" must be rejected with 400 before Stop is
// ever called.
func TestRestartAgent_LateKubernetesBlockRejectionRunsBeforeStop(t *testing.T) {
	srv, mgr, remapRuntime := newTestServerForLateCheckOrdering(t, "actual-agent-name", "url-id", "kubernetes")
	remapRuntime.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		t.Fatal("Start must not run once the late Kubernetes/block rejection fires")
		return "", nil
	}

	body, err := json.Marshal(map[string]any{
		"resolvedEnv": map[string]string{
			"SCION_METADATA_MODE": "block",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/url-id/restart", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 0 {
		t.Errorf("expected the agent not to be stopped once the late rejection fires before the stop, got %d stop calls", mgr.stopCalls)
	}
}
