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
	"os"
	"path/filepath"
	"strings"
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
			if tc.gatherEnv {
				// Declare a required empty-valued env key in harness_configs so a
				// lenient preflight in resolveRuntimeNameForOpts would answer 202
				// env-gather instead of refusing with 503 before the env prompt
				// (ptone/scion#4888, ptone/scion#3817).
				t.Setenv("SCION_TEST_MISSING_ENV_3817", "")
				_ = os.Unsetenv("SCION_TEST_MISSING_ENV_3817")
				writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
					"active_profile: local\n"+
					"harness_configs:\n"+
					"    claude:\n"+
					"        harness: claude\n"+
					"        env:\n"+
					"            SCION_TEST_MISSING_ENV_3817: \"\"\n"+
					"profiles:\n"+
					"    local:\n"+
					"        runtime: docker\n"+
					"    "+lifecycleK8sProfile+":\n"+
					"        runtime: kubernetes\n"+
					"runtimes:\n"+
					"    docker:\n"+
					"        type: docker\n"+
					"    kubernetes:\n"+
					"        type: kubernetes\n")
			}
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

// TestCreateProfileResolution_GatherEnvPreflightAndSettingsLoadError verifies:
//   - a valid profile with a missing required env key returns 202 (control),
//     whereas an unresolvable profile/active_profile/runtime is refused by
//     resolveRuntimeNameForOpts with 503 before the 202 env-gather prompt;
//   - when project settings fail to load (malformed YAML) or return nil,
//     missingSettingsProfileError reports that project settings could not be
//     loaded rather than claiming the requested profile does not exist.
func TestCreateProfileResolution_GatherEnvPreflightAndSettingsLoadError(t *testing.T) {
	t.Setenv("SCION_TEST_MISSING_ENV_3817", "")
	_ = os.Unsetenv("SCION_TEST_MISSING_ENV_3817")

	t.Run("gatherEnv control returns 202 for valid profile", func(t *testing.T) {
		f := newLifecycleFixture(t)
		writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
			"active_profile: local\n"+
			"harness_configs:\n"+
			"    claude:\n"+
			"        harness: claude\n"+
			"        env:\n"+
			"            SCION_TEST_MISSING_ENV_3817: \"\"\n"+
			"profiles:\n"+
			"    local:\n"+
			"        runtime: docker\n"+
			"runtimes:\n"+
			"    docker:\n"+
			"        type: docker\n")
		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":        "gather-control",
			"slug":        "gather-control",
			"id":          "agent-uuid-gather-control",
			"projectPath": f.projectPath,
			"gatherEnv":   true,
			"config":      map[string]any{"template": "claude", "profile": "local"},
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d (202 env-gather control): %s", w.Code, http.StatusAccepted, w.Body.String())
		}
	})

	t.Run("gatherEnv preflight refuses unknown active_profile before 202", func(t *testing.T) {
		f := newLifecycleFixture(t)
		writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
			"active_profile: vanished-default\n"+
			"harness_configs:\n"+
			"    claude:\n"+
			"        harness: claude\n"+
			"        env:\n"+
			"            SCION_TEST_MISSING_ENV_3817: \"\"\n"+
			"profiles:\n"+
			"    local:\n"+
			"        runtime: docker\n"+
			"runtimes:\n"+
			"    docker:\n"+
			"        type: docker\n")
		w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
			"name":        "gather-bad-active",
			"slug":        "gather-bad-active",
			"id":          "agent-uuid-gather-bad-active",
			"projectPath": f.projectPath,
			"gatherEnv":   true,
			"config":      map[string]any{"template": "claude"},
		})
		assertDispatchRefused(t, w,
			"default runtime profile cannot be resolved",
			`active_profile "vanished-default" not found`,
		)
		assertProfileUnresolvedDetails(t, w, "vanished-default")
		f.assertNoRuntimeUsed(t)
	})

	t.Run("malformed settings reports load failure rather than unknown profile", func(t *testing.T) {
		for _, gatherEnv := range []bool{false, true} {
			f := newLifecycleFixture(t)
			if err := os.WriteFile(filepath.Join(f.projectPath, "settings.yaml"),
				[]byte("schema_version: \"1\"\nprofiles: [not valid yaml\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"template": "claude", "profile": "local"}
			if gatherEnv {
				cfg["env"] = []string{"SCION_TEST_MISSING_ENV_3817"}
			}
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":        "malformed-settings-agent",
				"slug":        "malformed-settings-agent",
				"id":          "agent-uuid-malformed",
				"projectPath": f.projectPath,
				"gatherEnv":   gatherEnv,
				"noAuth":      !gatherEnv,
				"config":      cfg,
			})
			assertDispatchRefused(t, w,
				"requested runtime profile cannot be resolved",
				`profile "local" cannot be verified: project settings could not be loaded`,
				"retry once project settings can be loaded",
			)
			if strings.Contains(w.Body.String(), "not found in settings") {
				t.Fatalf("body falsely claims profile is unknown when settings failed to load: %s", w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp.Error.Details != nil {
				t.Fatalf("details = %#v, want nil when settings failed to load", resp.Error.Details)
			}
			f.assertNoRuntimeUsed(t)
		}
	})

	t.Run("nil settings without load error reports no settings loaded", func(t *testing.T) {
		f := newBRFixture(t)
		f.srv.loadSettings = func(string) (*config.VersionedSettings, []string, error) {
			return nil, nil, nil
		}
		_, err := f.srv.resolveManagerForOptsStrict(api.StartOptions{Name: "nil-vs", Profile: "local"}, profileCreate)
		if err == nil || !strings.Contains(err.Error(), "no project settings loaded") {
			t.Fatalf("resolveManagerForOptsStrict err = %v, want 'no project settings loaded'", err)
		}
		_, err = f.srv.resolveRuntimeNameForOpts(api.StartOptions{Name: "nil-vs", Profile: "local"})
		if err == nil || !strings.Contains(err.Error(), "no project settings loaded") {
			t.Fatalf("resolveRuntimeNameForOpts err = %v, want 'no project settings loaded'", err)
		}
	})
}
