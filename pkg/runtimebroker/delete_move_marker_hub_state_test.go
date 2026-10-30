// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#5955: a forced move's rollback delete passes the
// Hub project ID it resolves the agent's hub-project state directory with
// (the request's projectId) to the move-marker check, so the check and the
// delete name the same directories.
//
// For canonical project layouts, the check's hub-project state directory
// branch (agent.CheckMoveMarkers via hubStateDirToRemove) is not reachable
// through this handler: that directory is a directory of its own only when
// the project's recorded project ID is absent or differs from the Hub
// project ID, and the handler uses a project path only when it identifies
// as the request's projectId (resolveDeleteTarget), which is also the Hub
// project ID it passes. A project with such a directory therefore has no
// project path here, and its rollback delete is refused (409 when a runtime
// entry matched, else 404) before the check runs. A non-canonical layout
// can still name a directory of its own, and there the check is what
// refuses: TestDeleteAgent_ForcedMoveRollback_NonCanonicalMarkerHubDirRefused
// builds one (a non-git project whose marker slug is not in the slugified
// form). The branch is also covered in pkg/agent (move_marker_test.go).
// The other tests pin the wiring and a refusal that comes from the check
// itself.

const hubStateMoveID = "reincarnation-5955"

const hubStateMoveDeleteQuery = "deleteFiles=true&removeBranch=false&projectId=" + scopeProjB + "&" + api.MoveReincarnationIDQueryParam + "=" + hubStateMoveID

// hubIDMoveManager is a filteringMockManager that records the Hub project
// ID the handler passes to CheckMoveMarkers and puts in DeleteTarget's
// context. With a checker, CheckMoveMarkers is the real
// agent.AgentManager check; without one it reports no paths and no error.
type hubIDMoveManager struct {
	filteringMockManager
	checker          *agent.AgentManager
	checkCalls       int
	checkProjectPath string
	checkHubProject  string
	deleteHubProject string
}

func (m *hubIDMoveManager) CheckMoveMarkers(ctx context.Context, projectPath, projectID, hubProjectID, agentName, sharerKey, moveID string) ([]string, error) {
	m.checkCalls++
	m.checkProjectPath = projectPath
	m.checkHubProject = hubProjectID
	if m.checker == nil {
		return nil, nil
	}
	return m.checker.CheckMoveMarkers(ctx, projectPath, projectID, hubProjectID, agentName, sharerKey, moveID)
}

func (m *hubIDMoveManager) DeleteTarget(ctx context.Context, agentName, sharerKey string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.deleteHubProject = api.HubProjectIDFromContext(ctx)
	return m.filteringMockManager.DeleteTarget(ctx, agentName, sharerKey, ref, deleteFiles, projectPath, removeBranch)
}

// newHubIDMoveFixture is a hub-native project proj-b (project ID
// scopeProjB, which the delete resolves) with agent dev running in a
// container labelled with scopeProjB, behind a broker over mgr whose
// lifecycle log is returned. It returns the server, the project's .scion
// dir and the agent's home.
func newHubIDMoveFixture(t *testing.T, mgr *hubIDMoveManager) (*Server, string, string, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	scionDir, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-dev", scopeProjB, scionDir)}
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	logs := &bytes.Buffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	return srv, scionDir, config.GetAgentHomePath(scionDir, "dev"), logs
}

// The rollback delete passes the request's projectId as the Hub project ID
// both to the move-marker check and in DeleteTarget's context.
func TestDeleteAgent_ForcedMoveRollback_HubProjectIDWiring(t *testing.T) {
	mgr := &hubIDMoveManager{}
	srv, scionDir, _, _ := newHubIDMoveFixture(t, mgr)

	rec := doDelete(t, srv, "dev", hubStateMoveDeleteQuery)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if mgr.checkCalls != 1 {
		t.Fatalf("CheckMoveMarkers calls = %d, want 1", mgr.checkCalls)
	}
	if mgr.checkHubProject != scopeProjB {
		t.Errorf("CheckMoveMarkers hub project ID = %q, want the request's %q", mgr.checkHubProject, scopeProjB)
	}
	if !sameDirPath(t, mgr.checkProjectPath, scionDir) {
		t.Errorf("CheckMoveMarkers project path = %q, want %q", mgr.checkProjectPath, scionDir)
	}
	if mgr.deleteCalls != 1 {
		t.Fatalf("DeleteTarget calls = %d, want 1", mgr.deleteCalls)
	}
	if mgr.deleteHubProject != scopeProjB {
		t.Errorf("DeleteTarget context hub project ID = %q, want the request's %q", mgr.deleteHubProject, scopeProjB)
	}
}

// An agent directory without the move's marker, in a project the delete
// resolves: the real check refuses with move_marker_mismatch, the refusal
// names the paths it checked (not the missing project path), and nothing
// is removed.
func TestDeleteAgent_ForcedMoveRollback_HubProjectUnmarkedDirRefused(t *testing.T) {
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	mgr := &hubIDMoveManager{checker: agent.NewManager(rt).(*agent.AgentManager)}
	srv, scionDir, agentHome, logs := newHubIDMoveFixture(t, mgr)
	agentDir := filepath.Join(scionDir, "agents", "dev")
	unmarked := filepath.Dir(agentHome)
	if sameDirPath(t, unmarked, agentDir) {
		t.Fatalf("setup: the agent home's parent %s is the project agent directory", unmarked)
	}
	// The project agent directory carries the marker; the home's parent
	// under the external agents root does not.
	if err := os.WriteFile(filepath.Join(agentDir, agent.MoveMarkerFile), []byte(hubStateMoveID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", hubStateMoveDeleteQuery)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"`+api.BrokerErrCodeMoveMarkerMismatch+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
	}
	if mgr.checkCalls != 1 {
		t.Fatalf("CheckMoveMarkers calls = %d, want 1", mgr.checkCalls)
	}
	if mgr.checkHubProject != scopeProjB {
		t.Errorf("CheckMoveMarkers hub project ID = %q, want the request's %q", mgr.checkHubProject, scopeProjB)
	}
	out := logs.String()
	if !strings.Contains(out, "refusing a forced move's rollback delete") {
		t.Fatalf("no rollback refusal logged: %s", out)
	}
	if strings.Contains(out, "no project path to check the move markers in") {
		t.Errorf("refused for the missing project path, not by the move-marker check: %s", out)
	}
	if strings.Contains(out, "paths=[]") || !strings.Contains(out, unmarked) {
		t.Errorf("the refusal does not name the checked paths, including the unmarked %s: %s", unmarked, out)
	}
	if !strings.Contains(out, agent.ErrMoveMarkerMismatch.Error()) {
		t.Errorf("the refusal is not the move-marker mismatch: %s", out)
	}
	if mgr.deleteCalls != 0 {
		t.Errorf("DeleteTarget calls = %d on a refused rollback delete, want 0", mgr.deleteCalls)
	}
	for _, dir := range []string{agentDir, agentHome} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s removed on a refused rollback delete: %v", dir, err)
		}
	}
}

// A non-git project whose marker slug is not in the slugified form
// (Proj-B): the agent's project path is the project's external config dir
// <global>/project-configs/Proj-B__<short id>/.scion, which identifies as
// the project by its name, while the Hub project ID resolves the agent's
// state directory under project-configs/proj-b__<short id>, a directory of
// its own. That directory carries another move's marker: the check refuses
// on it with move_marker_mismatch, and nothing is removed.
func TestDeleteAgent_ForcedMoveRollback_NonCanonicalMarkerHubDirRefused(t *testing.T) {
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	mgr := &hubIDMoveManager{checker: agent.NewManager(rt).(*agent.AgentManager)}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	marker := &config.ProjectMarker{ProjectID: scopeProjB, ProjectName: "Proj-B", ProjectSlug: "Proj-B"}
	if marker.HasCanonicalDirName() {
		t.Fatalf("setup: the marker slug %q is canonical", marker.ProjectSlug)
	}
	workspace := filepath.Join(home, "work", "proj-b")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectMarker(filepath.Join(workspace, config.DotScion), marker); err != nil {
		t.Fatal(err)
	}
	extScion, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(extScion, "agents", "dev")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, agent.MoveMarkerFile), []byte(hubStateMoveID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hubDir, err := config.AgentDirForProject(extScion, "dev", true, scopeProjB)
	if err != nil {
		t.Fatalf("hub-project state dir: %v", err)
	}
	if strings.HasPrefix(filepath.Clean(hubDir), filepath.Clean(extScion)+string(filepath.Separator)) {
		t.Fatalf("setup: the hub-project state dir %s is not a directory of its own", hubDir)
	}
	if err := os.MkdirAll(hubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if sameDirPath(t, hubDir, agentDir) {
		t.Skip("case-insensitive filesystem: the hub-project state dir is the agent dir")
	}
	if err := os.WriteFile(filepath.Join(hubDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, agent.MoveMarkerFile), []byte("reincarnation-other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-dev", scopeProjB, extScion)}
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	logs := &bytes.Buffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))

	rec := doDelete(t, srv, "dev", hubStateMoveDeleteQuery)
	if mgr.checkCalls != 1 {
		t.Fatalf("CheckMoveMarkers calls = %d, want 1 (status %d: %s)", mgr.checkCalls, rec.Code, rec.Body.String())
	}
	if mgr.checkHubProject != scopeProjB {
		t.Errorf("CheckMoveMarkers hub project ID = %q, want the request's %q", mgr.checkHubProject, scopeProjB)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 for the hub-project state dir with another move's marker: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"`+api.BrokerErrCodeMoveMarkerMismatch+`"`) {
		t.Fatalf("body = %s, want code %s", rec.Body.String(), api.BrokerErrCodeMoveMarkerMismatch)
	}
	out := logs.String()
	if strings.Contains(out, "no project path to check the move markers in") {
		t.Errorf("refused for the missing project path, not by the move-marker check: %s", out)
	}
	if strings.Contains(out, "paths=[]") || !strings.Contains(out, hubDir) {
		t.Errorf("the refusal does not name the checked hub-project state dir %s: %s", hubDir, out)
	}
	if !strings.Contains(out, hubDir+" is marked by another move") {
		t.Errorf("the refusal is not for the hub-project state dir %s marked by another move: %s", hubDir, out)
	}
	if !strings.Contains(out, agent.ErrMoveMarkerMismatch.Error()) {
		t.Errorf("the refusal is not the move-marker mismatch: %s", out)
	}
	if mgr.deleteCalls != 0 {
		t.Errorf("DeleteTarget calls = %d on a refused rollback delete, want 0", mgr.deleteCalls)
	}
	for _, f := range []string{filepath.Join(hubDir, "scion-agent.json"), filepath.Join(agentDir, "scion-agent.json")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed on a refused rollback delete: %v", f, err)
		}
	}
}

// sameDirPath reports whether a and b name the same existing directory.
func sameDirPath(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
