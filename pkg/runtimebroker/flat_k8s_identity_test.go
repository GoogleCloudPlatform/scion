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
	"testing"
)

// TestFlatInstanceStart_NoKubernetesIdentityOnDockerTarget: a flat Docker
// instance resolves its single Docker target for every start, so the start
// context never builds Kubernetes identity material (a ServiceAccount name
// or a block identity), even when the settings' active profile names a
// Kubernetes runtime and the start carries GCP "assign" or "block" identity
// env. The Kubernetes consistency checks after the saved-profile read run
// for flat instances too; with no Kubernetes material on a Docker target
// they have nothing to refuse, and the start runs on the Docker target.
func TestFlatInstanceStart_NoKubernetesIdentityOnDockerTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{name: "assign", env: assignResolvedEnv},
		{name: "block", env: map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true, activeProfile: "batch"})
			// The existing agent's ownership record, its project ID and its
			// agent ID, as a flat instance's start requires them (the
			// existing-agent arrangement of the other flat start tests).
			f.seedOwnedAgent(t)
			env := map[string]string{"SCION_AGENT_ID": flatTestAgentID}
			for k, v := range tc.env {
				env[k] = v
			}
			body, err := json.Marshal(map[string]any{
				"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
				"resolvedEnv":             env,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start"+flatStartQuery, string(body))
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
			}
			f.mgr.mu.Lock()
			defer f.mgr.mu.Unlock()
			if f.mgr.startCalls != 1 {
				t.Fatalf("startCalls = %d, want 1", f.mgr.startCalls)
			}
			if got := f.mgr.lastStartOpts.ResolvedKubernetesServiceAccountName; got != "" {
				t.Fatalf("a Kubernetes ServiceAccount reached the Docker target: %q", got)
			}
			if f.mgr.lastStartOpts.KubernetesBlockIdentity != nil {
				t.Fatalf("a Kubernetes block identity reached the Docker target: %+v", f.mgr.lastStartOpts.KubernetesBlockIdentity)
			}
		})
	}
}
