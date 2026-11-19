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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for create/provision/reprovision profile resolution without a Hub DB
// settings overlay (ptone/scion#3817): an unknown requested profile, an
// unknown non-empty active_profile, or a profile whose runtime entry is
// missing is refused with 503 runtime_unavailable and reason
// profile_unresolved (listing the valid names), instead of falling back to the
// default runtime and saving a profile that later starts refuse.

// lifecycleAvailableProfiles is the sorted list of profiles configured on
// newLifecycleFixture (embedded defaults "local" and "remote", plus project
// profile lifecycleK8sProfile).
const lifecycleAvailableProfiles = lifecycleK8sProfile + ", local, remote"

// TestCreateProfileResolution_UnknownProfileRefused reproduces ptone/scion#3817
// across full create, gatherEnv create, and provisionOnly create: an unknown
// profile is refused with 503 runtime_unavailable + profile_unresolved and
// valid profile names, without starting or provisioning the agent.
func TestCreateProfileResolution_UnknownProfileRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		provisionOnly bool
		gatherEnv     bool
	}{
		{name: "create"},
		{name: "gatherEnv", gatherEnv: true},
		{name: "provisionOnly", provisionOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			const agentName = "typo-profile-agent"
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":          agentName,
				"slug":          agentName,
				"id":            "agent-uuid-typo",
				"projectPath":   f.projectPath,
				"provisionOnly": tc.provisionOnly,
				"gatherEnv":     tc.gatherEnv,
				"noAuth":        !tc.gatherEnv,
				"config":        map[string]any{"template": "claude", "profile": "no-such-profile"},
			})
			assertDispatchRefused(t, w,
				"requested runtime profile cannot be resolved",
				`profile "no-such-profile" not found`,
				"available profiles: "+lifecycleAvailableProfiles,
			)
			assertProfileUnresolvedDetails(t, w, "no-such-profile")
			f.assertNoRuntimeUsed(t)
			if agent.IsProvisioned(f.projectPath, agentName, false, "") {
				t.Fatalf("agent %q was provisioned on disk despite refused profile", agentName)
			}
		})
	}
}

// TestCreateProfileResolution_UnknownActiveProfileAndMissingRuntimeRefused
// verifies that an unknown non-empty active_profile (with no explicit request
// profile) and a profile whose runtime entry is missing are both refused with
// 503 runtime_unavailable + profile_unresolved and valid names.
func TestCreateProfileResolution_UnknownActiveProfileAndMissingRuntimeRefused(t *testing.T) {
	t.Run("unknown active_profile", func(t *testing.T) {
		f := newLifecycleFixture(t)
		writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
			"active_profile: vanished-default\n"+
			"profiles:\n"+
			"    local:\n"+
			"        runtime: docker\n"+
			"runtimes:\n"+
			"    docker:\n"+
			"        type: docker\n")

		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":        "bad-active-agent",
			"slug":        "bad-active-agent",
			"id":          "agent-uuid-bad-active",
			"projectPath": f.projectPath,
			"config":      map[string]any{"template": "claude"},
		})
		assertDispatchRefused(t, w,
			"default runtime profile cannot be resolved",
			`active_profile "vanished-default" not found`,
			"available profiles: local, remote",
		)
		assertProfileUnresolvedDetails(t, w, "vanished-default")
		f.assertNoRuntimeUsed(t)
	})

	t.Run("missing profile runtime", func(t *testing.T) {
		f := newLifecycleFixture(t)
		writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
			"active_profile: local\n"+
			"profiles:\n"+
			"    local:\n"+
			"        runtime: docker\n"+
			"    dangling:\n"+
			"        runtime: no-such-runtime\n"+
			"runtimes:\n"+
			"    docker:\n"+
			"        type: docker\n")

		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":        "dangling-rt-agent",
			"slug":        "dangling-rt-agent",
			"id":          "agent-uuid-dangling-rt",
			"projectPath": f.projectPath,
			"config":      map[string]any{"template": "claude", "profile": "dangling"},
		})
		assertDispatchRefused(t, w,
			"runtime profile's runtime cannot be resolved",
			`runtime "no-such-runtime" not found for profile "dangling"`,
			"available runtimes: ",
		)
		assertProfileUnresolvedDetails(t, w, "dangling")
		f.assertNoRuntimeUsed(t)
	})
}

// TestCreateProfileResolution_ReprovisionDeletedProfileFailsAtProvisioning
// verifies that reprovisioning an agent whose profile was deleted from
// settings fails at the provisioning step with 503 runtime_unavailable +
// profile_unresolved before Manager.Reprovision runs.
func TestCreateProfileResolution_ReprovisionDeletedProfileFailsAtProvisioning(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""

	projectPath := filepath.Join(t.TempDir(), ".scion")
	writeLifecycleProject(t, projectPath, "schema_version: \"1\"\n"+
		"active_profile: local\n"+
		"profiles:\n"+
		"    local:\n"+
		"        runtime: docker\n"+
		"    staging:\n"+
		"        runtime: docker\n"+
		"runtimes:\n"+
		"    docker:\n"+
		"        type: docker\n")

	// Initial provision with valid profile "staging" succeeds.
	w := lifecyclePost(t, srv, "/api/v1/agents", map[string]any{
		"name":          "reinc-agent",
		"slug":          "reinc-agent",
		"id":            "agent-uuid-reinc",
		"projectPath":   projectPath,
		"provisionOnly": true,
		"config":        map[string]any{"template": "claude", "profile": "staging"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("initial provision status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if !mgr.provisionCalled {
		t.Fatal("expected initial Provision to be called")
	}

	// Remove "staging" from project settings, then attempt reprovision.
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte("schema_version: \"1\"\n"+
		"active_profile: local\n"+
		"profiles:\n"+
		"    local:\n"+
		"        runtime: docker\n"+
		"runtimes:\n"+
		"    docker:\n"+
		"        type: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w = lifecyclePost(t, srv, "/api/v1/agents", map[string]any{
		"name":          "reinc-agent",
		"slug":          "reinc-agent",
		"id":            "agent-uuid-reinc",
		"projectPath":   projectPath,
		"provisionOnly": true,
		"reprovision":   true,
		"config":        map[string]any{"template": "claude", "profile": "staging"},
	})
	assertDispatchRefused(t, w,
		"requested runtime profile cannot be resolved",
		`profile "staging" not found`,
		"available profiles: local, remote",
	)
	assertProfileUnresolvedDetails(t, w, "staging")
	if mgr.reprovisionCalled {
		t.Fatal("Manager.Reprovision must not be called when the profile no longer resolves")
	}
}

// TestCreateProfileResolution_ValidDefaultZeroAndFlatUnchanged verifies that:
//   - valid explicit and default-profile creates still succeed;
//   - zero-profile fallback (no requested profile and empty active_profile)
//     still uses the default runtime;
//   - flat instances and ForceRuntime brokers remain unaffected.
func TestCreateProfileResolution_ValidDefaultZeroAndFlatUnchanged(t *testing.T) {
	t.Run("valid explicit and default profile creates succeed", func(t *testing.T) {
		f := newLifecycleFixture(t)
		for _, prof := range []string{"", "local"} {
			name := "ok-agent-" + prof
			cfg := map[string]any{"template": "claude"}
			if prof != "" {
				cfg["profile"] = prof
			}
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":          name,
				"slug":          name,
				"id":            "uuid-" + name,
				"projectPath":   f.projectPath,
				"provisionOnly": true,
				"config":        cfg,
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("profile=%q: status = %d, want %d: %s", prof, w.Code, http.StatusCreated, w.Body.String())
			}
		}
	})

	t.Run("zero-profile fallback when requested and active_profile are both empty", func(t *testing.T) {
		f := newBRFixture(t)
		f.srv.loadSettings = func(string) (*config.VersionedSettings, []string, error) {
			return &config.VersionedSettings{
				ActiveProfile: "",
				Runtimes:      map[string]config.V1RuntimeConfig{"docker": {Type: "docker"}},
			}, nil, nil
		}
		res, err := f.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "zero-prof"}, profileCreate)
		if err != nil {
			t.Fatalf("unexpected error on zero-profile fallback: %v", err)
		}
		if res.How != resolutionDefaultFallback || res.RuntimeType != "docker" {
			t.Fatalf("res = %+v, want default fallback on docker", res)
		}
		rtName, err := f.srv.resolveRuntimeNameForOpts(api.StartOptions{Name: "zero-prof"})
		if err != nil || rtName != "docker" {
			t.Fatalf("resolveRuntimeNameForOpts = %q, %v; want docker, nil", rtName, err)
		}
	})

	t.Run("flat instance and ForceRuntime unaffected", func(t *testing.T) {
		ff := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
		if _, err := ff.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "flat-a", Profile: "ignored"}, profileCreate); err != nil {
			t.Fatalf("flat resolveManagerForOptsStrict: %v", err)
		}
		if _, err := ff.srv.resolveRuntimeNameForOpts(api.StartOptions{Name: "flat-a", Profile: "ignored"}); err != nil {
			t.Fatalf("flat resolveRuntimeNameForOpts: %v", err)
		}

		forced := newBRFixture(t)
		forced.srv.config.ForceRuntime = "docker"
		if _, err := forced.srv.resolveManagerForOptsStrict(forced.opts("unknown-under-force"), profileCreate); err != nil {
			t.Fatalf("ForceRuntime resolveManagerForOptsStrict: %v", err)
		}
		if _, err := forced.srv.resolveRuntimeNameForOpts(forced.opts("unknown-under-force")); err != nil {
			t.Fatalf("ForceRuntime resolveRuntimeNameForOpts: %v", err)
		}
	})
}
