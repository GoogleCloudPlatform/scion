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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project storage (ptone/scion#5813, ptone/scion#5815): the GCS
// upload of a hub-managed workspace on agent create is gone. Agents of a
// hub-managed or shared-workspace project mount the workspace the hub
// serves; a remote broker reaches it only through the hub's shared
// workspace storage. A remote broker without it is refused with 409 before
// anything is dispatched; a broker with a local path for the
// project, and per-agent workspace modes, are unaffected.

// setupRemoteBrokerWorkspaceServer is setupCreateAgentServer with its broker
// made remote (no local path for the project) and a local storage provider.
func setupRemoteBrokerWorkspaceServer(t *testing.T) (*Server, store.Store, *store.Project, *createAgentDispatcher) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetStorage(newContentMockStorage("local"))
	clearCreateAgentProviderPath(t, s, project)
	return srv, s, project, disp
}

// setBrokerWorkspaceStorage records the broker's reported workspace storage
// descriptor, as its heartbeat does.
func setBrokerWorkspaceStorage(t *testing.T, s store.Store, brokerID string, ws *api.BrokerWorkspaceStorage) {
	t.Helper()
	ctx := context.Background()
	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.WorkspaceStorage = ws
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
	got, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	if ws == nil {
		require.Nil(t, got.WorkspaceStorage, "fixture check: no descriptor")
	} else {
		require.NotNil(t, got.WorkspaceStorage, "fixture check: descriptor stored")
		require.Equal(t, ws.Backend, got.WorkspaceStorage.Backend)
	}
}

// requireRefusedCreate asserts a 409 naming wantText, no dispatch, and no
// agent row left behind.
func requireRefusedCreate(t *testing.T, s store.Store, project *store.Project, disp *createAgentDispatcher, code int, body, wantText string) {
	t.Helper()
	require.Equal(t, http.StatusConflict, code, "body: %s", body)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, wantText)
	assert.False(t, strings.Contains(resp.Error.Message, "/"), "the message names no host path: %q", resp.Error.Message)
	require.Nil(t, disp.capturedAgent, "nothing may be dispatched")
	agents, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, agents.Items, "the agent row must be cleaned up")
}

func TestCreateAgent_RemoteBroker_SharedWorkspaceGit_LocalHub_Refused409(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	project.GitRemote = "github.com/example/repo"
	project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}
	require.NoError(t, s.UpdateProject(context.Background(), project))
	require.True(t, project.IsSharedWorkspace(), "fixture check: shared-workspace project")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "t6-shared-git", ProjectID: project.ID,
	})
	requireRefusedCreate(t, s, project, disp, rec.Code, rec.Body.String(),
		"lives on the hub's local disk")
}

func TestCreateAgent_RemoteBroker_NoSharedStorageDescriptor_Refused409(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(t.TempDir()))
	for _, ws := range []*api.BrokerWorkspaceStorage{nil, {Backend: api.WorkspaceStorageBackendLocal}} {
		setBrokerWorkspaceStorage(t, s, project.DefaultRuntimeBrokerID, ws)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name: "t6-no-descriptor", ProjectID: project.ID,
		})
		requireRefusedCreate(t, s, project, disp, rec.Code, rec.Body.String(),
			"does not report the hub's shared workspace storage (nfs)")
	}
}

func TestCreateAgent_RemoteBroker_SharedStorage_DispatchesUUIDWorkspace(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	mountRoot := t.TempDir()
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755))
	setBrokerWorkspaceStorage(t, s, project.DefaultRuntimeBrokerID, &api.BrokerWorkspaceStorage{Backend: api.WorkspaceStorageBackendNFS})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "shared-storage-agent", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	assert.Empty(t, disp.capturedAgent.AppliedConfig.Workspace,
		"the hub's mount path is not sent to a remote broker: it resolves the workspace through its own mount of the share")
	assert.Empty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath, "no GCS copy of the workspace")
	assert.Empty(t, disp.capturedAgent.AppliedConfig.WorkspaceStorageBucket)
}

// TestCreateAgent_EmbeddedBroker_SharedStorage_SendsExistingUUIDWorkspace:
// the co-located (embedded) broker shares the hub's mount, so it is given
// the uuid workspace path, which the hub creates if missing (the hub no
// longer creates a workspace copy at project create).
func TestCreateAgent_EmbeddedBroker_SharedStorage_SendsExistingUUIDWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	clearCreateAgentProviderPath(t, s, project)
	srv.SetEmbeddedBrokerID(project.DefaultRuntimeBrokerID)
	mountRoot := t.TempDir()
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "embedded-shared-agent", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
	want := filepath.Join(mountRoot, "share1", "projects", project.ID, "workspace")
	assert.Equal(t, want, disp.capturedAgent.AppliedConfig.Workspace, "the workspace is the uuid tree agents mount")
	assert.DirExists(t, want, "the hub creates the workspace leaf before dispatch")
}

func TestCreateAgent_BrokerWithLocalPath_Dispatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	srv.SetStorage(newContentMockStorage("local"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "local-path-agent", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	assert.Empty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
}

func TestCreateAgent_RemoteBroker_ClonePerAgentGit_Dispatches(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	project.GitRemote = "github.com/example/repo"
	require.NoError(t, s.UpdateProject(context.Background(), project))
	require.False(t, mountsHubProjectWorkspace(project), "fixture check: per-agent git mode")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "clone-per-agent", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
}

// TestWorkspaceFinalize_RelaysBrokerBootstrapSkippedWarning: when the broker
// skips a workspace bootstrap because the project workspace already exists
// (its create answer carries the warning), the hub relays the warning into
// the finalize response the CLI prints.
func TestWorkspaceFinalize_RelaysBrokerBootstrapSkippedWarning(t *testing.T) {
	const skipped = "The project workspace already exists on the runtime broker, so the uploaded files were not applied; use `scion sync to <agent>` to push changes"
	srv, f, c := newLandingCompletionServer(t)
	c.warnings = []string{skipped}
	agent := f.agent(t, "fin-skip", string(state.PhaseProvisioning), false)

	rec := doBootstrapRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agent.ID),
		SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp SyncToFinalizeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Warnings, skipped)
}

// markLinkedOrigin sets the server-owned project-origin label as a
// registration from an existing checkout does.
func markLinkedOrigin(t *testing.T, s store.Store, project *store.Project) {
	t.Helper()
	if project.Labels == nil {
		project.Labels = map[string]string{}
	}
	project.Labels[store.LabelProjectOrigin] = store.ProjectOriginLinked
	require.NoError(t, s.UpdateProject(context.Background(), project))
	require.True(t, project.IsLinkedOrigin(), "fixture check")
}

// TestCreateAgent_RemoteBroker_LocalHubWorkspace_Refused409: a HUB-MANAGED
// non-git project on a local-backend hub sent to a remote broker without a
// local path is refused: the hub holds its workspace.
func TestCreateAgent_RemoteBroker_LocalHubWorkspace_Refused409(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	require.False(t, project.IsLinkedOrigin(), "fixture check: hub-managed project")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "t6-local-hub", ProjectID: project.ID,
	})
	requireRefusedCreate(t, s, project, disp, rec.Code, rec.Body.String(),
		"lives on the hub's local disk, which a remote runtime broker cannot reach")
}

// TestCreateAgent_RemoteBroker_LinkedNonGitLocalHub_CarveOut pins the
// linked non-git exception: a
// LINKED non-git project on a local-backend hub dispatched to a remote
// broker without a local path is NOT refused, also without a task or
// workspace files (scion start/create, an empty directory); the broker uses
// its own project directory, so no hub path is recorded.
func TestCreateAgent_RemoteBroker_LinkedNonGitLocalHub_CarveOut(t *testing.T) {
	srv, s, project, disp := setupRemoteBrokerWorkspaceServer(t)
	require.Empty(t, project.GitRemote, "fixture check: non-git project")
	markLinkedOrigin(t, s, project)

	for _, req := range []CreateAgentRequest{
		{Name: "carve-no-task", ProjectID: project.ID},
		{Name: "carve-task", ProjectID: project.ID, Task: "work"},
	} {
		disp.capturedAgent = nil
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
		require.Equal(t, http.StatusCreated, rec.Code, "%s: %s", req.Name, rec.Body.String())
		require.NotNil(t, disp.capturedAgent, "dispatched")
		assert.Empty(t, disp.capturedAgent.AppliedConfig.Workspace, "the hub's local path is not sent to a remote broker")
	}
}

// TestProjectWorkspaceFiles_LinkedNonGitRemoteOnly_Conflict: the Files tab
// for the carve-out case answers 409 and never creates a hub copy; with an
// embedded provider it is served as before (workstation unchanged).
func TestProjectWorkspaceFiles_LinkedNonGitRemoteOnly_Conflict(t *testing.T) {
	srv, s, project, _ := setupRemoteBrokerWorkspaceServer(t)
	markLinkedOrigin(t, s, project)
	local, err := localProjectPath(project.Slug)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(local))
	url := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	rec := doRequest(t, srv, http.MethodGet, url, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "remote runtime brokers without shared storage")
	rec = doRequest(t, srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"})
	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	_, err = os.Stat(local)
	assert.True(t, os.IsNotExist(err), "no hub-side copy is created")

	srv.SetEmbeddedBrokerID(project.DefaultRuntimeBrokerID)
	rec = doRequest(t, srv, http.MethodGet, url, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "with the co-located broker the workspace is served as before: %s", rec.Body.String())
}

// TestProjectOriginLabel_ServerOwned: the project-origin label is set only
// by registration (a new project from an existing checkout is linked), is
// refused from clients on create and register, cannot be changed or set by
// PATCH (and survives a labels replacement), and is not copied by clone.
func TestProjectOriginLabel_ServerOwned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s := testServer(t)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Origin Create", Labels: map[string]string{store.LabelProjectOrigin: store.ProjectOriginLinked},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "create refuses the label: %s", rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "origin-register-bad", Labels: map[string]string{store.LabelProjectOrigin: "hub"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "register refuses the label: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "origin-register"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var reg RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	require.NotNil(t, reg.Project)
	linked, err := s.GetProject(ctx, reg.Project.ID)
	require.NoError(t, err)
	assert.True(t, linked.IsLinkedOrigin(), "a registered project is linked")

	hub, _ := createTestHubManagedProject(t, srv, "Origin Hub")
	assert.False(t, hub.IsLinkedOrigin(), "a created project is hub-managed")

	patch := func(id string, labels map[string]string) int {
		return doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+id, map[string]interface{}{"labels": labels}).Code
	}
	assert.Equal(t, http.StatusBadRequest, patch(hub.ID, map[string]string{store.LabelProjectOrigin: store.ProjectOriginLinked}), "PATCH cannot set it")
	assert.Equal(t, http.StatusBadRequest, patch(linked.ID, map[string]string{store.LabelProjectOrigin: "other"}), "PATCH cannot change it")
	assert.Less(t, patch(linked.ID, map[string]string{"team": "a"}), 300, "a labels replacement without the key is accepted")
	after, err := s.GetProject(ctx, linked.ID)
	require.NoError(t, err)
	assert.True(t, after.IsLinkedOrigin(), "the label survives a labels replacement")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+linked.ID+"/clone", map[string]interface{}{"name": "Origin Clone"})
	require.Less(t, rec.Code, 300, "the clone succeeds: %s", rec.Body.String())
	var clone store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &clone))
	require.NotEmpty(t, clone.ID)
	storedClone, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)
	assert.False(t, storedClone.IsLinkedOrigin(), "a clone is hub-managed")
}

// TestProjectWorkspaceFiles_LinkedNonGitRemoteOnly_SharedBackendServesUUIDTree:
// on a shared backend a linked non-git project served only by remote
// brokers runs its agents on projects/<uuid>/workspace, so the Files tab
// serves that tree (PUT writes there, GET lists it) instead of answering
// 409, and a remote broker on the same storage is not refused.
func TestProjectWorkspaceFiles_LinkedNonGitRemoteOnly_SharedBackendServesUUIDTree(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	srv, s, project, _ := setupRemoteBrokerWorkspaceServer(t)
	markLinkedOrigin(t, s, project)
	mountRoot := t.TempDir()
	shareBase := filepath.Join(mountRoot, "share1")
	require.NoError(t, os.MkdirAll(shareBase, 0o755))
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(mountRoot))
	stored, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	require.True(t, stored.IsLinkedOrigin(), "fixture check")
	assert.False(t, srv.workspaceOnRemoteBrokers(stored), "on a shared backend the workspace is the uuid tree")

	url := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
	rec := doRequest(t, srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(shareBase, "projects", project.ID, "workspace", "a.txt"))
	require.NoError(t, err, "PUT writes into projects/<uuid>/workspace")
	assert.Equal(t, "a", string(got))

	rec = doRequest(t, srv, http.MethodGet, url, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"a.txt"`, "GET lists the uuid tree")
}

// TestProjectRegister_BackfillsOriginForPreviouslyLinkedNonGitProject: on
// a local-backend hub, a non-git project linked from a checkout before the
// origin label existed (no label; the registering broker already has a
// provider row with that checkout's path; no hub workspace directory) is
// labeled linked once the broker registers the same checkout again. No other
// project is relabeled: a project created on the hub (origin hub), even with
// its workspace directory missing and registered twice; an unlabeled project
// the hub holds a workspace for; and an unlabeled project registered from a
// different path than the broker's existing one.
func TestProjectRegister_BackfillsOriginForPreviouslyLinkedNonGitProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s := testServer(t)
	ctx := context.Background()
	checkouts := t.TempDir()

	broker := &store.RuntimeBroker{ID: "aaaaaaaa-0000-4000-8000-0000000000f1", Name: "Backfill Broker", Slug: "backfill-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	register := func(p *store.Project, path string) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
			ID: p.ID, Name: p.Name, BrokerID: broker.ID, Path: path,
		})
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", p.Name, rec.Body.String())
	}
	legacyProject := func(id, name, slug string, providerPath string) *store.Project {
		t.Helper()
		p := &store.Project{ID: id, Name: name, Slug: slug}
		require.NoError(t, s.CreateProject(ctx, p))
		if providerPath != "" {
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: p.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: providerPath,
			}))
		}
		return p
	}
	origin := func(p *store.Project) string {
		t.Helper()
		got, err := s.GetProject(ctx, p.ID)
		require.NoError(t, err)
		return got.Labels[store.LabelProjectOrigin]
	}

	// Linked before the label: same broker, same checkout path.
	legacyPath := filepath.Join(checkouts, "legacy-linked")
	legacy := legacyProject("aaaaaaaa-0000-4000-8000-0000000000f3", "Legacy Linked", "legacy-linked", legacyPath)
	register(legacy, legacyPath)
	assert.Equal(t, store.ProjectOriginLinked, origin(legacy), "a project linked before the label existed is backfilled")

	// Created on the hub: origin hub, never relabeled, even with its
	// workspace directory missing and a checkout registered twice.
	hubCreated, _ := createTestHubManagedProject(t, srv, "Hub Created")
	assert.Equal(t, store.ProjectOriginHub, origin(hubCreated), "a project created on the hub records origin hub")
	hubDir, err := localProjectPath(hubCreated.Slug)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(hubDir))
	hubPath := filepath.Join(checkouts, hubCreated.Slug)
	register(hubCreated, hubPath)
	register(hubCreated, hubPath)
	assert.Equal(t, store.ProjectOriginHub, origin(hubCreated), "a hub-created project is never relabeled")

	// Unlabeled, but the hub holds its workspace.
	heldPath := filepath.Join(checkouts, "held")
	held := legacyProject("aaaaaaaa-0000-4000-8000-0000000000f4", "Held", "held-backfill", heldPath)
	heldDir, err := localProjectPath(held.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(heldDir, 0o755))
	register(held, heldPath)
	assert.Empty(t, origin(held), "a project the hub holds a workspace for is not relabeled")

	// Unlabeled, registered from a different path than the broker's.
	moved := legacyProject("aaaaaaaa-0000-4000-8000-0000000000f5", "Moved", "moved-backfill", filepath.Join(checkouts, "old"))
	register(moved, filepath.Join(checkouts, "new"))
	assert.NotEqual(t, store.ProjectOriginLinked, origin(moved), "no evidence: a different checkout path")
}
