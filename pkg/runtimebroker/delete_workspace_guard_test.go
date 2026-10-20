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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Agent deletion never removes a shared project workspace: deleting one
// agent of a hub-native project, through the broker's delete handler with a
// real agent manager, removes only that agent's directory. Another agent of
// the project and the project's workspace keep their files, for an
// ordinary delete and for the localOnly delete the hub sends to an agent's
// previous broker after a move.
func TestDeleteAgent_SiblingAndProjectWorkspaceSurvive(t *testing.T) {
	for _, mode := range []string{"deleteFiles=true", "deleteFiles=true&localOnly=true"} {
		t.Run(mode, func(t *testing.T) {
			var entries []api.AgentInfo
			var deleted []string
			srv, home := newProvisionDirsServer(t, &entries, &deleted)
			_, agentDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "dev")
			_, siblingDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "sib")
			projectFile := filepath.Join(home, ".scion", "projects", "proj-b", "README.md")
			if err := os.WriteFile(projectFile, []byte("shared"), 0o644); err != nil {
				t.Fatal(err)
			}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&"+mode)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
				t.Errorf("agent directory %s not removed (stat err %v)", agentDir, err)
			}
			for _, p := range []string{filepath.Join(siblingDir, "scion-agent.json"), projectFile} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("%s removed: %v", p, err)
				}
			}
		})
	}
}

// An agent directory that resolves into the project workspace is refused:
// the delete fails with the refusal in its error, for an ordinary and a
// localOnly delete, and the project workspace keeps its files. The agent
// runs (it has a runtime entry), so the delete finds it through the
// runtime, not the agent directory.
func TestDeleteAgent_RefusesAgentDirResolvingIntoProjectWorkspace(t *testing.T) {
	for _, mode := range []string{"deleteFiles=true", "deleteFiles=true&localOnly=true"} {
		t.Run(mode, func(t *testing.T) {
			var entries []api.AgentInfo
			var deleted []string
			srv, home := newProvisionDirsServer(t, &entries, &deleted)
			projectRoot := filepath.Join(home, ".scion", "projects", "proj-b")
			scionDir := filepath.Join(projectRoot, ".scion")
			if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := config.WriteProjectID(scionDir, scopeProjB); err != nil {
				t.Fatal(err)
			}
			projectFile := filepath.Join(projectRoot, "README.md")
			if err := os.WriteFile(projectFile, []byte("shared"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(projectRoot, filepath.Join(scionDir, "agents", "dev")); err != nil {
				t.Fatal(err)
			}
			entries = []api.AgentInfo{{
				Name: "dev", ContainerID: "cid-b", ProjectID: scopeProjB, ProjectPath: scionDir,
				Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_id": scopeProjB},
			}}

			rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&"+mode)
			if rec.Code == http.StatusNoContent {
				t.Fatalf("expected the delete to fail, got 204")
			}
			if !strings.Contains(rec.Body.String(), "refusing to delete agent files") {
				t.Errorf("response does not carry the refusal: %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), projectRoot) {
				t.Errorf("response names a path: %s", rec.Body.String())
			}
			if _, err := os.Stat(projectFile); err != nil {
				t.Errorf("project workspace file removed: %v", err)
			}
		})
	}
}
