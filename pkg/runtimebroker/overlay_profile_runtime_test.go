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
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// These tests pin the stored-profile runtime errors of ptone/scion#4889,
// #4888 and #4955: a stored profile whose runtime the stored settings do
// not define fails the dispatch instead of running on the default runtime;
// a create reports that before any env check; and every stored-profile
// refusal carries details naming the profile.

// danglingProfile is a stored profile whose runtime is not a stored runtime.
const danglingProfile = "dangling"

// installDanglingOverlay installs stored profiles in which danglingProfile
// names the undefined runtime "gke"; when activeDangles is true the file's
// active_profile ("local") is also stored with that runtime.
func installDanglingOverlay(t *testing.T, activeDangles bool) {
	t.Helper()
	profiles := map[string]config.V1ProfileConfig{
		lifecycleK8sProfile: {Runtime: "kubernetes"},
		danglingProfile:     {Runtime: "gke"},
	}
	if activeDangles {
		profiles["local"] = config.V1ProfileConfig{Runtime: "gke"}
	}
	installOverlayProfiles(t, profiles)
}

// profileErrorDetails decodes a refusal's error details.
func profileErrorDetails(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v: %s", err, w.Body.String())
	}
	return resp.Error.Details
}

// assertProfileUnresolvedDetails checks the profile_unresolved details.
func assertProfileUnresolvedDetails(t *testing.T, w *httptest.ResponseRecorder, profile string) {
	t.Helper()
	d := profileErrorDetails(t, w)
	if d[api.BrokerErrorDetailReason] != api.BrokerErrorReasonProfileUnresolved {
		t.Errorf("details reason = %v, want %q (details %v)", d[api.BrokerErrorDetailReason], api.BrokerErrorReasonProfileUnresolved, d)
	}
	if got, ok := d[api.BrokerErrorDetailProfile].(string); !ok || got != profile {
		t.Errorf("details profile = %v, want %q", d[api.BrokerErrorDetailProfile], profile)
	}
}

func (f *lifecycleFixture) assertNoRuntimeUsed(t *testing.T) {
	t.Helper()
	if f.defaultMgr.StartCalls() != 0 || f.defaultMgr.stopCalls != 0 {
		t.Errorf("default runtime used: start=%d stop=%d", f.defaultMgr.StartCalls(), f.defaultMgr.stopCalls)
	}
	if runs, _ := f.k8sRun(); runs != 0 {
		t.Errorf("kubernetes runtime runs = %d, want 0", runs)
	}
}

// A create naming a stored profile whose runtime is missing is refused,
// and does not run on the default runtime (ptone/scion#4889).
func TestOverlayProfileRuntime_NamedProfileCreateRefused(t *testing.T) {
	f := newLifecycleFixture(t)
	installDanglingOverlay(t, false)
	w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
		"name":        "dangling-agent",
		"slug":        "dangling-agent",
		"id":          "agent-uuid-dangling",
		"projectPath": f.projectPath,
		"config":      map[string]any{"template": "claude", "profile": danglingProfile},
	})
	assertDispatchRefused(t, w, "runtime profile's runtime cannot be resolved",
		`runtime "gke" not found for profile "dangling"`, "available runtimes: kubernetes",
		"retry once the profile's runtime is one of the hub's stored runtimes")
	assertProfileUnresolvedDetails(t, w, danglingProfile)
	f.assertNoRuntimeUsed(t)
}

// A create with no profile whose active_profile is a stored profile with a
// missing runtime is refused the same way.
func TestOverlayProfileRuntime_DefaultProfileCreateRefused(t *testing.T) {
	f := newLifecycleFixture(t)
	installDanglingOverlay(t, true)
	w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
		"name":        "default-dangling-agent",
		"slug":        "default-dangling-agent",
		"id":          "agent-uuid-default-dangling",
		"projectPath": f.projectPath,
		"config":      map[string]any{"template": "claude"},
	})
	assertDispatchRefused(t, w, "runtime profile's runtime cannot be resolved", `runtime "gke" not found for profile "local"`)
	assertProfileUnresolvedDetails(t, w, "local")
	f.assertNoRuntimeUsed(t)
}

// A provisioned profile whose stored runtime is missing fails start,
// restart and resume with the saved-profile error and never runs on the
// default runtime, including when agent-info.json records that the agent
// last ran there.
func TestOverlayProfileRuntime_SavedProfileFailsWithoutFallback(t *testing.T) {
	for _, op := range overlayDispatches() {
		t.Run(op.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			installDanglingOverlay(t, false)
			const name = "saved-dangling-agent"
			writeSavedAgentInfo(t, f.projectPath, name, danglingProfile, "docker")
			writeProvisionedProfile(t, f.projectPath, name, danglingProfile)
			f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

			w := op.run(t, f, name)
			assertDispatchRefused(t, w, "saved runtime profile cannot be resolved", `runtime "gke" not found for profile "dangling"`)
			assertProfileUnresolvedDetails(t, w, danglingProfile)
			f.assertNoRuntimeUsed(t)
		})
	}
}

// A named profile the stored profiles lack carries the details too
// (ptone/scion#4955).
func TestOverlayProfileRuntime_MissingNamedProfileCarriesDetails(t *testing.T) {
	f := newLifecycleFixture(t)
	installStoredProfilesOverlay(t)
	w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
		"name":        "missing-profile-agent",
		"slug":        "missing-profile-agent",
		"id":          "agent-uuid-missing",
		"projectPath": f.projectPath,
		"config":      map[string]any{"template": "claude", "profile": "vanished"},
	})
	assertDispatchRefused(t, w, `profile "vanished" not found`)
	assertProfileUnresolvedDetails(t, w, "vanished")
}

// A dispatch needing an unset default profile carries the details with an
// empty profile.
func TestOverlayProfileRuntime_UnresolvableDefaultCarriesDetails(t *testing.T) {
	err := &config.ActiveProfileError{Available: []string{"a", "b"}}
	d := storedProfileErrorDetails(errors.Join(errDefaultProfileUnresolved, err))
	if d[api.BrokerErrorDetailReason] != api.BrokerErrorReasonProfileUnresolved || d[api.BrokerErrorDetailProfile] != "" {
		t.Errorf("details = %v, want profile_unresolved with an empty profile", d)
	}
}

// A saved profile that cannot be resolved for a reason other than the
// stored profiles carries no details, so the Hub keeps its generic text.
func TestOverlayProfileRuntime_NonStoredErrorHasNoDetails(t *testing.T) {
	err := errors.New("saved runtime profile cannot be resolved: project settings could not be loaded")
	if d := storedProfileErrorDetails(err); d != nil {
		t.Errorf("details = %v, want nil", d)
	}
	w := httptest.NewRecorder()
	writeProfileUnresolved(w, errSavedProfileUnresolved)
	if d := profileErrorDetails(t, w); d != nil {
		t.Errorf("response details = %v, want none", d)
	}
}

// A create that gathers env reports the stored-profile error before the
// env check, not a missing-env answer (ptone/scion#4888).
func TestOverlayProfileRuntime_GatherEnvCreateReportsProfileFirst(t *testing.T) {
	for _, tc := range []struct {
		name, profile, wantProfile string
		activeDangles              bool
		want                       string
	}{
		{name: "default profile runtime missing", activeDangles: true, wantProfile: "local", want: `runtime "gke" not found for profile "local"`},
		{name: "named profile runtime missing", profile: danglingProfile, wantProfile: danglingProfile, want: `runtime "gke" not found for profile "dangling"`},
		{name: "named profile missing", profile: "vanished", wantProfile: "vanished", want: `profile "vanished" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			// A harness-config env key the request does not supply, so
			// the env check alone would answer with requirements.
			writeLifecycleProject(t, f.projectPath, "schema_version: \"1\"\n"+
				"active_profile: local\n"+
				"harness_configs:\n"+
				"    claude:\n"+
				"        harness: claude\n"+
				"        env:\n"+
				"            SECRET_TOKEN: \"\"\n"+
				"profiles:\n"+
				"    local:\n"+
				"        runtime: docker\n"+
				"runtimes:\n"+
				"    docker:\n"+
				"        type: docker\n")
			installDanglingOverlay(t, tc.activeDangles)
			cfg := map[string]any{"template": "claude"}
			if tc.profile != "" {
				cfg["profile"] = tc.profile
			}
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":        "gather-agent",
				"slug":        "gather-agent",
				"id":          "agent-uuid-gather",
				"projectPath": f.projectPath,
				"gatherEnv":   true,
				"config":      cfg,
			})
			assertDispatchRefused(t, w, tc.want)
			assertProfileUnresolvedDetails(t, w, tc.wantProfile)
			if strings.Contains(w.Body.String(), "SECRET_TOKEN") {
				t.Errorf("response is an env answer: %s", w.Body.String())
			}
			f.assertNoRuntimeUsed(t)
		})
	}
}

// An agent without image provenance whose Hub-sent profile is a stored
// profile with a missing runtime fails start, restart and resume with the
// saved-profile error and the profile_unresolved details, and never runs
// on the default runtime, including when agent-info.json records that the
// agent last ran there.
func TestOverlayProfileRuntime_HubSentProfileFailsWithoutFallback(t *testing.T) {
	for _, op := range []struct {
		name, action string
		body         func(f *lifecycleFixture) map[string]any
	}{
		{name: "start", action: "start", body: func(f *lifecycleFixture) map[string]any {
			return map[string]any{"projectPath": f.projectPath, "hubEndpoint": lifecycleHubEndpoint, "profile": danglingProfile}
		}},
		{name: "resume", action: "start", body: func(f *lifecycleFixture) map[string]any {
			return map[string]any{"projectPath": f.projectPath, "hubEndpoint": lifecycleHubEndpoint, "profile": danglingProfile, "resume": true}
		}},
		{name: "restart", action: "restart", body: func(f *lifecycleFixture) map[string]any {
			return map[string]any{"hubEndpoint": lifecycleHubEndpoint, "profile": danglingProfile}
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			installDanglingOverlay(t, false)
			const name = "hub-sent-dangling-agent"
			writeSavedAgentInfo(t, f.projectPath, name, danglingProfile, "docker")
			f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/"+op.action, op.body(f))
			assertDispatchRefused(t, w, "saved runtime profile cannot be resolved", `runtime "gke" not found for profile "dangling"`)
			if strings.Contains(w.Body.String(), "provisioned with") {
				t.Errorf("refusal names a provisioned profile, want the Hub-sent one: %s", w.Body.String())
			}
			assertProfileUnresolvedDetails(t, w, danglingProfile)
			f.assertNoRuntimeUsed(t)
		})
	}
}
