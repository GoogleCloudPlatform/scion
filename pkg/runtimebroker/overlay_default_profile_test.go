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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// These tests pin how a co-located broker resolves profiles once the Hub's
// stored profiles replace the settings file's through the settings overlay
// (ptone/scion#3906): an agent's own saved profile still wins; a saved
// profile the stored profiles lack fails start, restart and resume with no
// fallback to the default; a dispatch that needs the default profile, when
// the stored profiles do not define active_profile, fails with an error
// naming both; and none of this is a startup error.

// installStoredProfilesOverlay installs a global settings overlay whose
// stored profiles define only the kubernetes profile, so the fixture's
// file active_profile ("local") is not one of them.
func installStoredProfilesOverlay(t *testing.T) {
	t.Helper()
	prev := config.GetGlobalSettingsOverlay()
	o := config.NewSettingsOverlay()
	o.Update(
		map[string]config.V1RuntimeConfig{"kubernetes": {Type: "kubernetes"}},
		map[string]config.V1ProfileConfig{lifecycleK8sProfile: {Runtime: "kubernetes"}},
		nil, "")
	config.SetGlobalSettingsOverlay(o)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prev) })
}

// assertDispatchRefused checks a 503 runtime_unavailable whose body
// contains every want string.
func assertDispatchRefused(t *testing.T, w *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v: %s", err, w.Body.String())
	}
	if resp.Error.Code != ErrCodeRuntimeUnavailable {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeRuntimeUnavailable)
	}
	for _, s := range want {
		if !strings.Contains(resp.Error.Message, s) {
			t.Errorf("message does not contain %q: %s", s, resp.Error.Message)
		}
	}
}

// overlayDispatch is one lifecycle operation on an existing agent.
type overlayDispatch struct {
	name string
	// run performs the operation for agent name; setup registers whatever
	// the operation needs to find the agent.
	run func(t *testing.T, f *lifecycleFixture, name string) *httptest.ResponseRecorder
}

func overlayDispatches() []overlayDispatch {
	return []overlayDispatch{
		{name: "start", run: func(t *testing.T, f *lifecycleFixture, name string) *httptest.ResponseRecorder {
			return lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"projectPath": f.projectPath,
				"hubEndpoint": lifecycleHubEndpoint,
			})
		}},
		{name: "resume", run: func(t *testing.T, f *lifecycleFixture, name string) *httptest.ResponseRecorder {
			return lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"projectPath": f.projectPath,
				"hubEndpoint": lifecycleHubEndpoint,
				"resume":      true,
			})
		}},
		{name: "restart", run: func(t *testing.T, f *lifecycleFixture, name string) *httptest.ResponseRecorder {
			return lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
				"hubEndpoint": lifecycleHubEndpoint,
			})
		}},
	}
}

// An agent's saved profile resolves against the stored profiles exactly as
// before, even though the default profile does not resolve.
func TestOverlayProfiles_SavedProfileWinsOverUnresolvableDefault(t *testing.T) {
	for _, op := range overlayDispatches() {
		t.Run(op.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			installStoredProfilesOverlay(t)
			const name = "saved-k8s-agent"
			writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
			f.registerK8sAgents(lifecycleAgent(name, f.projectPath, ""))

			w := op.run(t, f, name)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			if runs, _ := f.k8sRun(); runs != 1 {
				t.Errorf("kubernetes runtime runs = %d, want 1", runs)
			}
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
		})
	}
}

// A saved profile the stored profiles lack fails start, restart and resume
// with the saved-profile error, and never falls back to the default profile
// or the default runtime.
func TestOverlayProfiles_MissingSavedProfileFailsWithoutFallback(t *testing.T) {
	for _, op := range overlayDispatches() {
		t.Run(op.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			installStoredProfilesOverlay(t)
			const name = "vanished-profile-agent"
			writeSavedAgentProfile(t, f.projectPath, name, "vanished")
			f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

			w := op.run(t, f, name)
			assertDispatchRefused(t, w, "saved runtime profile cannot be resolved", `profile "vanished" not found`)
			if f.defaultMgr.StartCalls() != 0 || f.defaultMgr.stopCalls != 0 {
				t.Errorf("default runtime used: start=%d stop=%d", f.defaultMgr.StartCalls(), f.defaultMgr.stopCalls)
			}
			if runs, _ := f.k8sRun(); runs != 0 {
				t.Errorf("kubernetes runtime runs = %d, want 0", runs)
			}
		})
	}
}

// A dispatch that needs the default profile fails with an error naming the
// configured active profile and the stored profiles, instead of running on
// the broker's default runtime.
func TestOverlayProfiles_DispatchNeedingDefaultFailsClearly(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		f := newLifecycleFixture(t)
		installStoredProfilesOverlay(t)
		w := lifecyclePost(t, f.srv, "/api/v1/agents/fresh-agent/start", map[string]any{
			"projectPath": f.projectPath,
		})
		assertDispatchRefused(t, w, `active_profile "local"`, "available profiles: "+lifecycleK8sProfile)
		if f.defaultMgr.StartCalls() != 0 {
			t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
		}
	})
	t.Run("create", func(t *testing.T) {
		f := newLifecycleFixture(t)
		installStoredProfilesOverlay(t)
		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":          "prov-agent",
			"slug":          "prov-agent",
			"id":            "agent-uuid-prov",
			"projectPath":   f.projectPath,
			"provisionOnly": true,
			"config":        map[string]any{"template": "claude"},
		})
		assertDispatchRefused(t, w, `active_profile "local"`, "available profiles: "+lifecycleK8sProfile)
	})
	t.Run("create with explicit profile", func(t *testing.T) {
		f := newLifecycleFixture(t)
		installStoredProfilesOverlay(t)
		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":          "prov-agent",
			"slug":          "prov-agent",
			"id":            "agent-uuid-prov",
			"projectPath":   f.projectPath,
			"provisionOnly": true,
			"config":        map[string]any{"template": "claude", "profile": lifecycleK8sProfile},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
	})
}

// Resolution that does not dispatch (resolveManagerForOpts, used for agents
// already running) still returns the default manager, and a restart of a
// running agent with no saved profile stays on the runtime it was found on.
func TestOverlayProfiles_RunningAgentsUnaffected(t *testing.T) {
	t.Run("non-dispatch resolution", func(t *testing.T) {
		f := newLifecycleFixture(t)
		installStoredProfilesOverlay(t)
		mgr, name := f.srv.resolveManagerForOpts(api.StartOptions{Name: "running", ProjectPath: f.projectPath})
		if mgr == nil || name != "docker" {
			t.Errorf("resolveManagerForOpts = %v, %q; want the default docker manager", mgr, name)
		}
	})
	t.Run("restart pinned to found runtime", func(t *testing.T) {
		f := newLifecycleFixture(t)
		installStoredProfilesOverlay(t)
		const name = "running-no-profile"
		writeSavedAgentProfile(t, f.projectPath, name, "")
		f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

		w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
			"hubEndpoint": lifecycleHubEndpoint,
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
		}
		if f.defaultMgr.StartCalls() != 1 {
			t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
		}
	})
}

// A Kubernetes-only broker with no active_profile, whose agents carry their
// own profile, still starts and dispatches those agents; only a dispatch
// that needs the default profile fails.
func TestOverlayProfiles_KubernetesOnlyBrokerWithoutActiveProfileStarts(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	projectPath := filepath.Join(t.TempDir(), "k8s-project", ".scion")
	writeLifecycleProject(t, projectPath, "schema_version: \"1\"\n"+
		"profiles:\n"+
		"    "+lifecycleK8sProfile+":\n"+
		"        runtime: kubernetes\n"+
		"runtimes:\n"+
		"    kubernetes:\n"+
		"        type: kubernetes\n")
	installStoredProfilesOverlay(t)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.StateDir = t.TempDir()
	cfg.ContainerHubEndpoint = lifecycleBridge
	mgr := &filteringMockManager{}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }})
	srv.discoverAuxiliaryRuntimesForProjects([]string{projectPath})

	health := httptest.NewRecorder()
	srv.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d: %s", health.Code, http.StatusOK, health.Body.String())
	}

	const name = "k8s-agent"
	writeSavedAgentProfile(t, projectPath, name, lifecycleK8sProfile)
	w := lifecyclePost(t, srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"projectPath": projectPath,
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if mgr.StartCalls() != 1 {
		t.Errorf("kubernetes default runtime Start calls = %d, want 1", mgr.StartCalls())
	}

	w = lifecyclePost(t, srv, "/api/v1/agents/no-profile-agent/start", map[string]any{
		"projectPath": projectPath,
		"hubEndpoint": lifecycleHubEndpoint,
	})
	// The embedded defaults supply active_profile "local" when no settings
	// file sets one; either way it is not a stored profile.
	assertDispatchRefused(t, w, "active_profile ", "is not one of the hub's stored profiles", "available profiles: "+lifecycleK8sProfile)
	if mgr.StartCalls() != 1 {
		t.Errorf("Start calls after refused dispatch = %d, want 1", mgr.StartCalls())
	}
}
