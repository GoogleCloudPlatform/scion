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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#1819: a broker delete must act only on the agent in
// the requested project, on every runtime, and must return 404 with no side
// effects when the requested project has no such agent.

const (
	scopeProjA = "11111111-aaaa-aaaa-aaaa-111111111111"
	scopeProjB = "22222222-bbbb-bbbb-bbbb-222222222222"
)

func labelled(name, cid, projectID, projectPath string) api.AgentInfo {
	return api.AgentInfo{
		Name:        name,
		ContainerID: cid,
		ProjectID:   projectID,
		ProjectPath: projectPath,
		Labels: map[string]string{
			"scion.agent":      "true",
			"scion.name":       name,
			"scion.project_id": projectID,
		},
	}
}

// newScopeTestServer builds a broker whose default runtime is docker and
// isolates HOME/CWD so no real project directories are touched.
func newScopeTestServer(t *testing.T, mgr *filteringMockManager) (*Server, string) {
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
	return New(cfg, mgr, rt), home
}

// makeHubProject creates ~/.scion/projects/<slug>/.scion with a project-id
// file and an agent directory holding an agent-info.json. It returns the
// .scion dir and the agent-info.json path.
func makeHubProject(t *testing.T, home, slug, projectID, agentName string) (string, string) {
	t.Helper()
	scionDir := filepath.Join(home, ".scion", "projects", slug, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scionDir, projectID); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(info, []byte(`{"name":"`+agentName+`","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return scionDir, info
}

func doDelete(t *testing.T, srv *Server, agentName, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentName+"?"+query, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func assertUntouched(t *testing.T, scionDir, agentName, infoPath string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(scionDir, "agents", agentName)); err != nil {
		t.Errorf("other project's agent dir was touched: %v", err)
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatalf("read agent-info.json: %v", err)
	}
	if strings.Contains(string(data), "deleted") {
		t.Errorf("other project's agent-info.json was soft-delete marked: %s", data)
	}
}

func TestDeleteAgent_SameSlugDifferentProjects_DeletesOnlyRequested(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	// projA's agent is listed first: a first-match resolver would pick it.
	mgr.agents = []api.AgentInfo{
		labelled("dev", "cid-a", scopeProjA, scionA),
		labelled("dev", "cid-b", scopeProjB, scionB),
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected exactly 1 delete, got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteContainerID() != "cid-b" {
		t.Errorf("deleted container %q, want cid-b", mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteProjectPath() != scionB {
		t.Errorf("file deletion project path %q, want projB's %q", mgr.LastDeleteProjectPath(), scionB)
	}
}

func TestDeleteAgent_NoMatchInProject_404NoSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-a", scopeProjA, scionA)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&removeBranch=true&softDelete=true&deletedAt=2026-09-23T00:00:00Z")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete call, got %d (container %q)", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
	}
	assertUntouched(t, scionA, "dev", infoA)
}

// The case reported by substrate-lead: a named substrate profile is an
// auxiliary runtime on a broker whose default is docker. deleteAgent for
// projB, when only projA has "dev", must not fall through to the default
// runtime, the project-blind hub-managed directory scan, or soft-delete
// marking. The gate is the project match, not the runtime type.
func TestDeleteAgent_AuxiliarySubstrateProfile_NoMatchInProject_404(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	// projA's "dev" is only on disk (e.g. its container was pruned), so a
	// project-blind file scan would find it.
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{labelled("other", "actor-1", scopeProjB, "")}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["substrate-eu"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "substrate" }},
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&removeBranch=true&softDelete=true&deletedAt=2026-09-23T00:00:00Z")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 || auxMgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete calls, got default=%d aux=%d", mgr.DeleteCalls(), auxMgr.DeleteCalls())
	}
	assertUntouched(t, scionA, "dev", infoA)
}

func TestDeleteAgent_AuxiliaryRuntimeMatch_DeletesOnThatRuntime(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-a", scopeProjA, "/projects/a/.scion")}
	srv, _ := newScopeTestServer(t, mgr)
	auxMgr := &filteringMockManager{}
	auxMgr.agents = []api.AgentInfo{labelled("dev", "actor-b", scopeProjB, "/projects/b/.scion")}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["substrate"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "substrate" }},
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("default runtime (projA) must not be touched, got %d deletes", mgr.DeleteCalls())
	}
	if auxMgr.DeleteCalls() != 1 || auxMgr.LastDeleteContainerID() != "actor-b" {
		t.Errorf("expected aux delete of actor-b, got %d calls, container %q", auxMgr.DeleteCalls(), auxMgr.LastDeleteContainerID())
	}
}

func TestDeleteAgent_FileOnlyAgentInRequestedProject_DeletesFiles(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 delete call, got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteContainerID() != "" {
		t.Errorf("file-only delete must not target a container, got %q", mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteProjectPath() != scionB {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), scionB)
	}
	assertUntouched(t, scionA, "dev", infoA)
}

func TestDeleteAgent_MatchedEntryWithoutProjectPath_ResolvesOnlyOwnProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	// Both projects have a "dev" directory; projA sorts first.
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "actor-b", scopeProjB, "")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "actor-b" || mgr.LastDeleteProjectPath() != scionB {
		t.Errorf("got container %q path %q, want actor-b / %q", mgr.LastDeleteContainerID(), mgr.LastDeleteProjectPath(), scionB)
	}
}

func TestDeleteAgent_MatchedEntryWithoutProjectPath_NoProjectDir_SkipsFiles(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "actor-b", scopeProjB, "")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&softDelete=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "actor-b" {
		t.Errorf("deleted container %q, want actor-b", mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteFiles() || mgr.LastDeleteProjectPath() != "" {
		t.Errorf("expected file cleanup skipped, got deleteFiles=%v path=%q", mgr.LastDeleteFiles(), mgr.LastDeleteProjectPath())
	}
	assertUntouched(t, scionA, "dev", infoA)
}

func TestDeleteAgent_UnlabelledLegacyContainerAccepted(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{legacyEntry("dev", "cid-legacy", scionB)}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-legacy" {
		t.Errorf("deleted container %q, want cid-legacy", mgr.LastDeleteContainerID())
	}
}

func TestDeleteAgent_AmbiguousInProject_ConflictNoSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{
		labelled("dev", "cid-1", scopeProjB, "/projects/b/.scion"),
		labelled("dev", "cid-2", scopeProjB, "/projects/b/.scion"),
	}
	srv, _ := newScopeTestServer(t, mgr)

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete call, got %d", mgr.DeleteCalls())
	}
}

func TestFindAgentInHubManagedProjects_ProjectScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionA, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")

	for projectID, want := range map[string]string{scopeProjA: scionA, scopeProjB: scionB, "33333333-cccc": ""} {
		got, err := findAgentInHubManagedProjects("dev", projectID)
		if err != nil {
			t.Fatalf("project %s: %v", projectID, err)
		}
		if got != want {
			t.Errorf("project %s: got %q, want %q", projectID, got, want)
		}
	}

	if _, err := findAgentInHubManagedProjects("dev", ""); err == nil {
		t.Error("unscoped lookup of a slug present in two projects must fail closed")
	}
}

// erroringListManager fails every List call.
type erroringListManager struct{ filteringMockManager }

func (m *erroringListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return nil, errors.New("runtime unavailable")
}

func TestDeleteAgent_ListFailure_NotReportedAs404(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	auxMgr := &erroringListManager{}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB)
	if rec.Code == http.StatusNotFound || rec.Code/100 == 2 {
		t.Fatalf("expected an error status when a runtime cannot be listed, got %d", rec.Code)
	}
	if mgr.DeleteCalls() != 0 || auxMgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete calls")
	}
}

// makeLinkedGitProject creates a linked (non hub-managed) git-style project
// outside ~/.scion: <root>/.scion is a directory with a project-id file and an
// in-project agent directory. It returns the project root.
func makeLinkedGitProject(t *testing.T, projectID, agentName string) string {
	t.Helper()
	root := t.TempDir()
	scionDir := filepath.Join(root, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scionDir, projectID); err != nil {
		t.Fatal(err)
	}
	return root
}

func resolvedScionDir(t *testing.T, path string) string {
	t.Helper()
	dir, err := config.GetResolvedProjectDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// UAT regression for #1846: a linked project's agent whose container is gone
// but whose files remain must be found through the project path the hub
// sends, not reported as 404.
func TestDeleteAgent_FileOnlyAgentInLinkedProject_UsesHubPathHint(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	root := makeLinkedGitProject(t, scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&projectPath="+url.QueryEscape(root))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 || mgr.LastDeleteContainerID() != "" {
		t.Fatalf("expected 1 file-only delete, got %d calls, container %q", mgr.DeleteCalls(), mgr.LastDeleteContainerID())
	}
	if want := resolvedScionDir(t, root); mgr.LastDeleteProjectPath() != want {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), want)
	}
}

// A hint whose project identity differs from the requested project must be
// ignored, so a wrong or stale path can never redirect deletion.
func TestDeleteAgent_LinkedProjectHintForOtherProject_404NoSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	rootA := makeLinkedGitProject(t, scopeProjA, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&projectPath="+url.QueryEscape(rootA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no delete calls, got %d", mgr.DeleteCalls())
	}
	if _, err := os.Stat(filepath.Join(rootA, ".scion", "agents", "dev")); err != nil {
		t.Errorf("other project's agent dir was touched: %v", err)
	}
}

// Without a hint, an embedded broker running inside the linked project still
// finds the agent through its own working project (identity-checked).
func TestDeleteAgent_FileOnlyAgentInBrokerWorkingProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	if err := os.Chdir(root); err != nil { // restored by newScopeTestServer's cleanup
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 || mgr.LastDeleteProjectPath() == "" {
		t.Fatalf("expected a file-only delete with a project path, got %d calls, path %q", mgr.DeleteCalls(), mgr.LastDeleteProjectPath())
	}

	// A different project ID does not match the working project.
	mgr.SetDeleteCalls(0)
	rec = doDelete(t, srv, "dev", "projectId="+scopeProjA+"&deleteFiles=true")
	if rec.Code != http.StatusNotFound || mgr.DeleteCalls() != 0 {
		t.Fatalf("expected 404 with no delete for another project, got %d / %d calls", rec.Code, mgr.DeleteCalls())
	}
}

// Non-git linked projects use a .scion marker file pointing at an external
// config dir under ~/.scion/project-configs/.
func TestDeleteAgent_FileOnlyAgentInLinkedMarkerProject(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	root := t.TempDir()
	marker := &config.ProjectMarker{ProjectID: scopeProjB, ProjectName: "linked", ProjectSlug: "linked"}
	if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), marker); err != nil {
		t.Fatal(err)
	}
	extDir, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(extDir, "agents", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&projectPath="+url.QueryEscape(root))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() != extDir {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), extDir)
	}
}

// legacyEntry is a pre-label container: no project ID in labels or fields.
func legacyEntry(name, cid, projectPath string) api.AgentInfo {
	return api.AgentInfo{
		Name:        name,
		ContainerID: cid,
		ProjectPath: projectPath,
		Labels:      map[string]string{"scion.agent": "true", "scion.name": name},
	}
}

// UAT F2: a legacy container belonging to another project must not be
// deleted just because it carries no project ID.
func TestDeleteAgent_LegacyContainerFromOtherProject_404NoSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "worker")

	for _, tc := range []struct {
		name  string
		entry api.AgentInfo
	}{
		{"path of other project", legacyEntry("worker", "cid-legacy-a", scionA)},
		{"no project path", legacyEntry("worker", "cid-legacy-x", "")},
		{"path without identity", legacyEntry("worker", "cid-legacy-y", t.TempDir())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr.agents = []api.AgentInfo{tc.entry}
			mgr.SetDeleteCalls(0)
			rec := doDelete(t, srv, "worker", "projectId="+scopeProjB+"&deleteFiles=true")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
			}
			if mgr.DeleteCalls() != 0 {
				t.Fatalf("expected no delete calls, got %d", mgr.DeleteCalls())
			}
		})
	}
	assertUntouched(t, scionA, "worker", infoA)
}

// UAT F3: a project path from a runtime label is used for file deletion only
// if it verifiably belongs to the requested project.
func TestDeleteAgent_UntrustedEntryProjectPathIgnored(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionA, infoA := makeHubProject(t, home, "proj-a", scopeProjA, "dev")

	// Label points at another project's dir: no project dir of our own
	// exists, so file cleanup is skipped but the container is removed.
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, scionA)}
	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&softDelete=true&deletedAt=2026-01-01T00:00:00Z")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("expected success, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-b" {
		t.Errorf("deleted container %q, want cid-b", mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteProjectPath() != "" || mgr.LastDeleteFiles() {
		t.Errorf("file deletion must not use an untrusted path, got path %q files=%v", mgr.LastDeleteProjectPath(), mgr.LastDeleteFiles())
	}
	assertUntouched(t, scionA, "dev", infoA)

	// A crafted path outside any project is ignored and the broker's own
	// identity-checked resolution finds the real project dir.
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, t.TempDir())}
	rec = doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() != scionB {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), scionB)
	}
}

// Non-git linked projects record the external config dir as the agent's
// project path. Its <slug>__<short-id> name ties it to a project, so it is
// trusted for its own project only.
func TestDeleteAgent_ExternalConfigDirProjectPath(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extDir := func(projectID string) string {
		d, err := (config.ProjectMarker{ProjectID: projectID, ProjectSlug: "linked"}).ExternalProjectPath()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(d, home) {
			t.Fatalf("external dir %q not under test HOME %q", d, home)
		}
		if err := os.MkdirAll(filepath.Join(d, "agents", "dev"), 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	extB := extDir(scopeProjB)
	extA := extDir(scopeProjA)

	// Labelled entry, own project's external dir: used for files.
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, extB)}
	if rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true"); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() != extB {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), extB)
	}

	// Labelled entry, another project's external dir: ignored.
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, extA)}
	if rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true"); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() == extA {
		t.Errorf("file deletion used another project's external dir %q", extA)
	}

	// Legacy entry: accepted only for the project its external dir encodes.
	mgr.agents = []api.AgentInfo{legacyEntry("dev", "cid-legacy", extA)}
	mgr.SetDeleteCalls(0)
	if rec := doDelete(t, srv, "dev", "projectId="+scopeProjB); rec.Code != http.StatusNotFound || mgr.DeleteCalls() != 0 {
		t.Fatalf("expected 404 with no delete, got %d / %d calls", rec.Code, mgr.DeleteCalls())
	}
	if rec := doDelete(t, srv, "dev", "projectId="+scopeProjA); rec.Code != http.StatusNoContent || mgr.LastDeleteContainerID() != "cid-legacy" {
		t.Fatalf("expected 204 deleting cid-legacy, got %d / %q", rec.Code, mgr.LastDeleteContainerID())
	}
}

// A legacy container's recorded external config dir may still name an entry
// moved out of the pre-rename ~/.scion/grove-configs root:
// MigrateLegacyGlobalLayout leaves a per-entry symlink at the old name
// (grove-configs/<name> -> ../project-configs/<name>), so
// externalConfigShortID (via pathIdentifiesAs) must still recognise it.
func TestDeleteAgent_ExternalConfigDirProjectPath_ThroughLegacySymlink(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)

	marker := config.ProjectMarker{ProjectID: scopeProjB, ProjectSlug: "linked"}

	// Build the pre-migration layout directly under the legacy root, then
	// run the real migrator. config.MigrateLegacyGlobalLayout symlinks each
	// entry it moves individually (grove-configs/<name> ->
	// ../project-configs/<name>); the legacy root itself stays a real
	// directory, so the agent's recorded path below is reached through a
	// per-entry symlink, not a symlinked root.
	legacyEntryScionDir := filepath.Join(home, ".scion", "grove-configs", marker.DirName(), config.DotScion)
	if err := os.MkdirAll(filepath.Join(legacyEntryScionDir, "agents", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	config.MigrateLegacyGlobalLayout(filepath.Join(home, ".scion"), noopMigrationReporter{})

	realDir, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(realDir); err != nil {
		t.Fatalf("migration did not create the canonical external config dir: %v", err)
	}

	mgr.agents = []api.AgentInfo{legacyEntry("dev", "cid-legacy", legacyEntryScionDir)}
	if rec := doDelete(t, srv, "dev", "projectId="+scopeProjB); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-legacy" {
		t.Errorf("deleted container %q, want cid-legacy", mgr.LastDeleteContainerID())
	}
}

// Upstream review (#1875): a soft delete without deleteFiles must still
// resolve the project path so agent-info.json is marked deleted, while
// leaving the files in place.
func TestDeleteAgent_SoftDeleteWithoutFiles_MarksAgentInfo(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	// The runtime entry carries no project path.
	mgr.agents = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, "")}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&softDelete=true&deletedAt=2026-01-01T00:00:00Z")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("expected success, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-b" || mgr.LastDeleteFiles() {
		t.Errorf("got container %q files=%v, want cid-b without file deletion", mgr.LastDeleteContainerID(), mgr.LastDeleteFiles())
	}
	data, err := os.ReadFile(infoB)
	if err != nil {
		t.Fatalf("read agent-info.json: %v", err)
	}
	if !strings.Contains(string(data), `"deleted"`) {
		t.Errorf("agent-info.json not marked deleted: %s", data)
	}
	if _, err := os.Stat(filepath.Join(scionB, "agents", "dev")); err != nil {
		t.Errorf("agent dir must remain on soft delete without deleteFiles: %v", err)
	}
}

// Upstream review (#1875): if the global dir cannot be resolved, the agent's
// absence is unknown, so the delete must fail rather than return 404 (which
// the hub treats as a completed delete).
func TestDeleteAgent_GlobalDirUnresolvable_NotReportedAs404(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, _ := newScopeTestServer(t, mgr)
	t.Setenv("HOME", "") // os.UserHomeDir fails

	if _, err := findAgentInHubManagedProjects("dev", scopeProjB); !errors.Is(err, errDeleteTargetUnknown) {
		t.Fatalf("expected errDeleteTargetUnknown, got %v", err)
	}
	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code == http.StatusNotFound || rec.Code < 400 {
		t.Fatalf("expected a failure status other than 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no delete calls, got %d", mgr.DeleteCalls())
	}
}

// UAT N1 (#1846): with the agent's files present but a runtime unlistable,
// a file-only delete would orphan a possibly running container. The delete
// must fail with no side effects instead.
func TestDeleteAgent_ListFailureWithFilesPresent_FailsWithoutSideEffects(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	scionB, infoB := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	auxMgr := &erroringListManager{}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{
		Runtime: &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
		Manager: auxMgr,
	}
	srv.auxiliaryRuntimesMu.Unlock()

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&softDelete=true&deletedAt=2026-01-01T00:00:00Z")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected a runtime error (500) when a runtime cannot be listed, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 || auxMgr.DeleteCalls() != 0 {
		t.Errorf("expected no delete calls, got %d / %d", mgr.DeleteCalls(), auxMgr.DeleteCalls())
	}
	assertUntouched(t, scionB, "dev", infoB)
}

// makeHubMarkerProject creates a hub-managed project the way the broker's
// start path does for a project with no git remote: ~/.scion/projects/<slug>/.scion
// is a marker file, and the agent's provision directory lives under the
// external project-configs agents directory. It returns the external .scion
// dir and the agent's provision dir.
func makeHubMarkerProject(t *testing.T, home, slug, projectID, agentName string) (string, string) {
	t.Helper()
	root := filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := &config.ProjectMarker{ProjectID: projectID, ProjectName: slug, ProjectSlug: slug}
	if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), marker); err != nil {
		t.Fatal(err)
	}
	extDir, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(extDir, "agents", agentName)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return extDir, agentDir
}

// ptone/scion#2839: an agent in a hub-managed (marker-file) project that has
// no runtime entry -- provisioned but never started, or its container already
// gone -- must still resolve to its project so its provision directory under
// the project configs agents directory is removed.
func TestDeleteAgent_FileOnlyAgentInHubMarkerProject_DeletesFiles(t *testing.T) {
	mgr := &filteringMockManager{}
	srv, home := newScopeTestServer(t, mgr)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	extB, _ := makeHubMarkerProject(t, home, "proj-b", scopeProjB, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteProjectPath() != extB {
		t.Errorf("file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), extB)
	}
	if mgr.LastDeleteProjectPath() == extA {
		t.Errorf("delete resolved to the other project's dir %q", extA)
	}
}

func TestFindAgentInHubManagedProjects_MarkerProjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	extA, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	extB, _ := makeHubMarkerProject(t, home, "proj-b", scopeProjB, "dev")

	for projectID, want := range map[string]string{scopeProjA: extA, scopeProjB: extB, "33333333-cccc": ""} {
		got, err := findAgentInHubManagedProjects("dev", projectID)
		if err != nil {
			t.Fatalf("project %s: %v", projectID, err)
		}
		if got != want {
			t.Errorf("project %s: got %q, want %q", projectID, got, want)
		}
	}
	if got, err := findAgentInHubManagedProjects("other", scopeProjA); err != nil || got != "" {
		t.Errorf("absent agent: got %q, %v; want empty", got, err)
	}
	// Without a project ID, a marker's identity cannot be checked against
	// anything trusted, so marker projects are not considered at all.
	if got, err := findAgentInHubManagedProjects("dev", ""); err != nil || got != "" {
		t.Errorf("unscoped lookup: got %q, %v; want marker projects skipped", got, err)
	}
}
