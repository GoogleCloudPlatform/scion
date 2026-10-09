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
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// These tests pin that a restart of an agent with no saved profile starts it
// on the runtime its container was found on (and stopped on), not on the
// runtime of the project's active profile (ptone/scion#2658).

// newActiveK8sProject writes an agent project whose active profile selects
// the kubernetes runtime, while docker stays the broker default, and returns
// its .scion directory.
func newActiveK8sProject(t *testing.T) string {
	t.Helper()
	dotScion := filepath.Join(t.TempDir(), "active-k8s-project", ".scion")
	writeLifecycleProject(t, dotScion, "schema_version: \"1\"\n"+
		"active_profile: "+lifecycleK8sProfile+"\n"+
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
	return dotScion
}

// TestRestartAgent_NoSavedProfile_StartsOnFoundRuntime: an agent running on
// the default runtime with no saved profile, in a project whose active
// profile points at an auxiliary runtime, is stopped and started on the
// default runtime, and the start is classified for that runtime (docker's
// default GCP metadata mode and bridged hub endpoint).
func TestRestartAgent_NoSavedProfile_StartsOnFoundRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "pinned-agent"
	projectPath := newActiveK8sProject(t)
	f.defaultMgr.agents = []api.AgentInfo{lifecycleAgent(name, projectPath, "")}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if f.defaultMgr.stopCalls != 1 {
		t.Errorf("default runtime stop calls = %d, want 1", f.defaultMgr.stopCalls)
	}
	if f.defaultMgr.StartCalls() != 1 {
		t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
	}
	if runs, _ := f.k8sRun(); runs != 0 {
		t.Errorf("kubernetes runtime runs = %d, want 0", runs)
	}
	env := f.defaultMgr.LastStartOpts().Env
	if got := env["SCION_METADATA_MODE"]; got != "block" {
		t.Errorf("SCION_METADATA_MODE = %q, want block (classified for docker)", got)
	}
	if got := env["SCION_HUB_ENDPOINT"]; got != lifecycleBridgedHubEP {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (classified for docker)", got, lifecycleBridgedHubEP)
	}
}

// TestRestartAgent_SavedProfile_NotPinnedToFoundRuntime: with a saved
// profile the restart still starts on that profile's runtime, as before,
// even though the container was found on the default runtime.
func TestRestartAgent_SavedProfile_NotPinnedToFoundRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "saved-agent"
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)
	f.defaultMgr.agents = []api.AgentInfo{lifecycleAgent(name, f.projectPath, "")}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if f.defaultMgr.stopCalls != 1 {
		t.Errorf("default runtime stop calls = %d, want 1", f.defaultMgr.stopCalls)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
	runs, env := f.k8sRun()
	if runs != 1 {
		t.Fatalf("kubernetes runtime runs = %d, want 1", runs)
	}
	if !slices.Contains(env, "SCION_HUB_ENDPOINT="+lifecycleHubEndpoint) {
		t.Errorf("want SCION_HUB_ENDPOINT=%s (classified for kubernetes), got env %v", lifecycleHubEndpoint, env)
	}
}

// TestRestartAgent_NoContainer_FollowsActiveProfile: when the agent's entry
// is found without a container, nothing is stopped or pinned and the start
// follows the project's active profile, as before.
func TestRestartAgent_NoContainer_FollowsActiveProfile(t *testing.T) {
	f := newLifecycleFixture(t)
	const (
		name      = "files-only-agent"
		projectID = "11111111-2222-3333-4444-555555555555"
	)
	projectPath := newActiveK8sProject(t)
	entry := lifecycleAgent(name, projectPath, projectID)
	entry.ID, entry.ContainerID = "", ""
	f.defaultMgr.agents = []api.AgentInfo{entry}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart?projectId="+projectID, map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if f.defaultMgr.stopCalls != 0 {
		t.Errorf("default runtime stop calls = %d, want 0", f.defaultMgr.stopCalls)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Errorf("kubernetes runtime runs = %d, want 1", runs)
	}
}
