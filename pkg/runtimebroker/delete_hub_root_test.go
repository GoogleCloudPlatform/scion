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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#4769: the broker's file deletions resolve an
// agent's state directory with the same Hub project ID as lookup.

const (
	hubRootLocalID = "33333333-cccc-4ccc-8ccc-333333333333"
	hubRootHubID   = "44444444-dddd-4ddd-8ddd-444444444444"
)

// A failed synchronous create removes the agent's state directory as
// lookup resolves it from the Hub project ID, when that differs from the
// directory resolved from the project-id marker.
func TestSyncCreateFailure_RemovesStateDirOfHubProjectID(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	srv, mgr, _, _ := newSyncStartTestServer(t)

	const agentName = "same-name"
	root, dotScion := identityProjectDir(t, hubRootLocalID)
	// Keep the project-id marker on the local ID, so the two roots differ.
	localCfg, err := config.GetGitProjectExternalConfigDir(dotScion)
	if err != nil || localCfg == "" {
		t.Fatalf("GetGitProjectExternalConfigDir: dir=%q err=%v", localCfg, err)
	}
	if err := os.MkdirAll(localCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir, err := config.AgentDirForProject(dotScion, agentName, true, hubRootHubID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if dir, _, err := agent.AgentStateDir(dotScion, agentName, false, hubRootHubID, true); err != nil || dir != stateDir {
		t.Fatalf("lookup state dir = %q, %v; want %q", dir, err, stateDir)
	}

	var gotHubID string
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		gotHubID = opts.HubProjectID
		if err := os.WriteFile(filepath.Join(stateDir, "prompt.md"), []byte("t\n"), 0o644); err != nil {
			t.Errorf("write prompt.md: %v", err)
		}
		return nil, errors.New("container start failed")
	}
	body := `{"id":"agent-a-id","name":"` + agentName + `","projectId":"` + hubRootHubID +
		`","projectPath":` + strconvQuote(root) + `,"runId":"run-a","config":{"task":"t"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatalf("expected the create to fail, got %d: %s", w.Code, w.Body.String())
	}
	if gotHubID != hubRootHubID {
		t.Fatalf("Start got HubProjectID %q, want %q", gotHubID, hubRootHubID)
	}
	if id, err := config.ReadProjectID(dotScion); err != nil || id != hubRootLocalID {
		t.Fatalf("project-id marker = %q, %v; want %q", id, err, hubRootLocalID)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("failed create left the state dir of the Hub project ID (stat err %v)", err)
	}
}

// The async launch's failure cleanup (cleanupAbortedLaunch) also removes
// the agent's state directory as lookup resolves it from the launch's Hub
// project ID, when that differs from the directory resolved from the
// project-id marker.
func TestCleanupAbortedLaunch_RemovesStateDirOfHubProjectID(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	const agentName = "agent-x"
	_, dotScion := identityProjectDir(t, hubRootLocalID)
	stateDir, err := config.AgentDirForProject(dotScion, agentName, true, hubRootHubID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	extAgents, err := config.GetGitProjectExternalAgentsDir(dotScion)
	if err != nil || extAgents == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgents, err)
	}
	if markerDir := filepath.Join(extAgents, agentName); markerDir == stateDir {
		t.Fatalf("test setup: the Hub-ID dir and the marker-root dir are the same: %s", stateDir)
	}
	if err := writeLaunchMarker(dotScion, false, agentName, "L1"); err != nil {
		t.Fatal(err)
	}
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: agentName, ProjectPath: dotScion, RunID: "run-a", HubProjectID: hubRootHubID},
		mgr:  mgr,
		key:  launchKey{ProjectID: hubRootHubID, Slug: agentName},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("aborted launch cleanup left the state dir of the Hub project ID (stat err %v)", err)
	}
}

// hubIDRecordingManager records the Hub project ID in the context of each
// DeleteTarget call and then runs the real delete.
type hubIDRecordingManager struct {
	agent.Manager
	mu  sync.Mutex
	got []string
}

func (m *hubIDRecordingManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.mu.Lock()
	m.got = append(m.got, api.HubProjectIDFromContext(ctx))
	m.mu.Unlock()
	return m.Manager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
}

// The delete handler passes the request's project ID to the manager's
// file deletion as the Hub project ID, the one lookup uses.
func TestDeleteAgent_PassesRequestProjectIDAsHubProjectID(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	var deleted []string
	entries := []api.AgentInfo{}
	rt := labelFilterRuntime(&entries, &deleted)
	mgr := &hubIDRecordingManager{Manager: agent.NewManager(rt)}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, mgr, rt)

	scionDir, _, agentDir, _ := makeGitHubProject(t, home, "proj-b", scopeProjB, "dev", "run-cur")
	entries = []api.AgentInfo{withRun(labelled("dev", "cid-cur", scopeProjB, scionDir), "run-cur")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-cur&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	mgr.mu.Lock()
	got := append([]string(nil), mgr.got...)
	mgr.mu.Unlock()
	if len(got) != 1 || got[0] != scopeProjB {
		t.Fatalf("DeleteTarget Hub project IDs = %q, want [%s]", got, scopeProjB)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("delete left the agent dir (stat err %v)", err)
	}
}
