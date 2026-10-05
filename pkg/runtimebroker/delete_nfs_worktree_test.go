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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// worktreeRemovingManager records RemoveNFSAgentFiles calls and returns err.
type worktreeRemovingManager struct {
	*filteringMockManager
	calls []string
	err   error
}

func (m *worktreeRemovingManager) RemoveNFSAgentFiles(_ context.Context, projectPath, projectID, agentName string) ([]string, error) {
	m.calls = append(m.calls, projectPath+"|"+projectID+"|"+agentName)
	return []string{
		"/export/projects/" + projectID + "/workspace/worktrees/" + agentName,
		"/export/projects/" + projectID + "/agents/" + agentName,
	}, m.err
}

func newWorktreeRemovalServer(t *testing.T, mgr *worktreeRemovingManager) (*Server, string, *bytes.Buffer) {
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

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))
	return srv, home, &logBuf
}

// A delete with files asks the manager to remove the agent's worktree and
// own workspace on the NFS workspace; a failure there is logged with the
// paths and the delete still succeeds.
func TestDeleteAgent_NFSWorktreeRemovalFailureDoesNotFailDelete(t *testing.T) {
	mgr := &worktreeRemovingManager{filteringMockManager: &filteringMockManager{}, err: errors.New("provisioning lock busy")}
	srv, home, logBuf := newWorktreeRemovalServer(t, mgr)
	scionDir, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-a", scopeProjA, scionDir)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjA+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 delete, got %d", mgr.DeleteCalls())
	}
	if len(mgr.calls) != 1 || mgr.calls[0] != scionDir+"|"+scopeProjA+"|dev" {
		t.Fatalf("RemoveNFSAgentFiles calls = %q", mgr.calls)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "left in place") || !strings.Contains(logs, "/export/projects/"+scopeProjA+"/workspace/worktrees/dev") ||
		!strings.Contains(logs, "/export/projects/"+scopeProjA+"/agents/dev") {
		t.Errorf("expected a warning with the worktree and agent directory paths, got: %s", logs)
	}
}

// A delete that keeps the agent's files keeps its worktree and own
// workspace too.
func TestDeleteAgent_KeepFilesKeepsNFSWorktree(t *testing.T) {
	mgr := &worktreeRemovingManager{filteringMockManager: &filteringMockManager{}}
	srv, home, _ := newWorktreeRemovalServer(t, mgr)
	scionDir, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-a", scopeProjA, scionDir)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjA+"&deleteFiles=false")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mgr.calls) != 0 {
		t.Fatalf("RemoveNFSAgentFiles called with deleteFiles=false: %q", mgr.calls)
	}
}

// An empty-per-agent agent (design #2703 P3) goes through the same
// name-keyed removal on delete with files: its own workspace on the NFS
// export is removed by RemoveNFSAgentFiles, with no separate remover.
func TestDeleteAgent_EmptyPerAgentRemovesNFSAgentFiles(t *testing.T) {
	mgr := &worktreeRemovingManager{filteringMockManager: &filteringMockManager{}}
	srv, home, _ := newWorktreeRemovalServer(t, mgr)
	scionDir, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	info := labelled("dev", "cid-a", scopeProjA, scionDir)
	info.Labels["scion.dev/workspace-mode"] = "empty-per-agent"
	mgr.agents = []api.AgentInfo{info}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjA+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(mgr.calls) != 1 || mgr.calls[0] != scionDir+"|"+scopeProjA+"|dev" {
		t.Fatalf("RemoveNFSAgentFiles calls = %q", mgr.calls)
	}
}
