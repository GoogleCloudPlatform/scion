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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The hub's test identity purge (ptone/scion#4240, Phase 3b) deletes every
// agent with deleteFiles=true and removeBranch=true. On the broker that
// delete removes the agent's home directory (with the rest of its agent
// directory and its worktree); the same delete with deleteFiles=false
// keeps it.
func TestDeleteAgent_DeleteFilesRemovesAgentHome(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleteFiles bool
	}{
		{"deleteFiles=true removes the home", true},
		{"deleteFiles=false keeps the home", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_HOST_UID", "")
			var deleted []string
			entries := []api.AgentInfo{}
			srv, home := newRealFilesServer(t, labelFilterRuntime(&entries, &deleted))
			scionDir, _, agentDir, _ := makeGitHubProject(t, home, "proj-b", scopeProjB, "dev", "run-cur")
			entries = []api.AgentInfo{withRun(labelled("dev", "cid-cur", scopeProjB, scionDir), "run-cur")}
			agentHome := config.GetAgentHomePath(scionDir, "dev")
			if err := os.WriteFile(filepath.Join(agentHome, "notes.txt"), []byte("agent state"), 0o644); err != nil {
				t.Fatal(err)
			}

			query := "projectId=" + scopeProjB + "&runId=run-cur&removeBranch=true"
			if tc.deleteFiles {
				query += "&deleteFiles=true"
			}
			rec := doDelete(t, srv, "dev", query)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(deleted) != 1 || deleted[0] != "cid-cur" {
				t.Fatalf("runtime deletes = %v, want [cid-cur]", deleted)
			}
			_, homeErr := os.Stat(agentHome)
			_, dirErr := os.Stat(agentDir)
			if tc.deleteFiles {
				if !os.IsNotExist(homeErr) {
					t.Errorf("deleteFiles=true left the agent home (stat err %v)", homeErr)
				}
				if !os.IsNotExist(dirErr) {
					t.Errorf("deleteFiles=true left the agent dir (stat err %v)", dirErr)
				}
				return
			}
			if homeErr != nil {
				t.Errorf("deleteFiles=false removed the agent home: %v", homeErr)
			}
		})
	}
}
