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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A soft delete with retained files (the hub's soft_delete_retain_files:
// it sends deleteFiles=false with softDelete=true) keeps a linked project's
// agent files on the broker, as it does for a hub-managed project
// (ptone/scion#1854). These tests use the real agent manager, so a file
// deletion would really remove the files; the deleteFiles=true case shows
// that it does.

// newRealManagerScopeServer is newScopeTestServer with the real agent
// manager over a mock runtime listing *entries.
func newRealManagerScopeServer(t *testing.T, entries *[]api.AgentInfo) *Server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return *entries, nil
		},
	}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	return New(cfg, agent.NewManager(rt), rt)
}

// writeLinkedAgentFiles puts an agent-info.json in the agent's home and a
// file in its workspace under the linked project's resolved directory, and
// returns the agent directory and the agent-info.json path.
func writeLinkedAgentFiles(t *testing.T, projectDir, agentName string) (string, string) {
	t.Helper()
	agentDir := filepath.Join(projectDir, "agents", agentName)
	if err := os.MkdirAll(filepath.Join(agentDir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "workspace", "work.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(info, []byte(`{"name":"`+agentName+`","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return agentDir, info
}

func TestDeleteAgent_SoftDeleteRetainFiles_LinkedProjectKeepsFiles(t *testing.T) {
	type linked struct {
		root       string // the path the hub sends as projectPath
		projectDir string // the resolved project directory
	}
	gitLinked := func(t *testing.T) linked {
		root := makeLinkedGitProject(t, scopeProjB, "dev")
		return linked{root: root, projectDir: resolvedScionDir(t, root)}
	}
	markerLinked := func(t *testing.T) linked {
		root := t.TempDir()
		marker := &config.ProjectMarker{ProjectID: scopeProjB, ProjectName: "linked", ProjectSlug: "linked"}
		if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), marker); err != nil {
			t.Fatal(err)
		}
		ext, err := marker.ExternalProjectPath()
		if err != nil {
			t.Fatal(err)
		}
		return linked{root: root, projectDir: ext}
	}

	tests := []struct {
		name      string
		project   func(t *testing.T) linked
		container bool
	}{
		{name: "git linked project, running container", project: gitLinked, container: true},
		{name: "git linked project, files only", project: gitLinked},
		{name: "non-git linked project, running container", project: markerLinked, container: true},
		{name: "non-git linked project, files only", project: markerLinked},
	}
	for _, tc := range tests {
		for _, deleteFiles := range []bool{false, true} {
			name := tc.name + "/retain files"
			if deleteFiles {
				name = tc.name + "/delete files"
			}
			t.Run(name, func(t *testing.T) {
				// HOME is set before the project is made: a non-git linked
				// project resolves its directory under it.
				var entries []api.AgentInfo
				srv := newRealManagerScopeServer(t, &entries)
				p := tc.project(t)
				if tc.container {
					entries = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, p.projectDir)}
				}
				agentDir, info := writeLinkedAgentFiles(t, p.projectDir, "dev")

				// The query the hub sends for a soft delete
				// (deleteAgentQuery): retained files mean deleteFiles=false.
				q := "deleteFiles=false&removeBranch=true&projectId=" + scopeProjB +
					"&projectPath=" + url.QueryEscape(p.root) +
					"&softDelete=true&deletedAt=2026-10-08T00:00:00Z"
				if deleteFiles {
					q = strings.Replace(q, "deleteFiles=false", "deleteFiles=true", 1)
				}
				rec := doDelete(t, srv, "dev", q)
				if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
					t.Fatalf("expected success, got %d: %s", rec.Code, rec.Body.String())
				}

				_, statErr := os.Stat(filepath.Join(agentDir, "workspace", "work.txt"))
				if deleteFiles {
					if !os.IsNotExist(statErr) {
						t.Fatalf("deleteFiles=true must remove the agent's files (stat: %v)", statErr)
					}
					return
				}
				if statErr != nil {
					t.Fatalf("retained soft delete removed the linked project's agent files: %v", statErr)
				}
				data, err := os.ReadFile(info)
				if err != nil {
					t.Fatalf("retained soft delete removed agent-info.json: %v", err)
				}
				if !strings.Contains(string(data), `"deleted"`) {
					t.Errorf("agent-info.json not marked deleted: %s", data)
				}
			})
		}
	}
}
