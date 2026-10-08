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
	"net/http"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Tests for the hub-managed workspace upload on agent create running only
// on a GCS-backed hub (ptone/scion#3765): the upload is a GCS sync, so a hub
// on any other storage provider never runs it. For a project without a git
// remote it fails the create with a clear error; a shared-workspace project
// with a git remote is dispatched without the upload.

// newGCSContentMockStorage is a content mock storage that reports the GCS
// provider, for tests that exercise the workspace upload.
func newGCSContentMockStorage(bucket string) *contentMockStorage {
	stor := newContentMockStorage(bucket)
	stor.provider = storage.ProviderGCS
	return stor
}

// uploadRecorder replaces syncToGCSForWorkspaceUpload for the test and
// records each call's bucket.
func uploadRecorder(t *testing.T) *[]string {
	t.Helper()
	var buckets []string
	previous := syncToGCSForWorkspaceUpload
	syncToGCSForWorkspaceUpload = func(_ context.Context, _, bucket, _ string) error {
		buckets = append(buckets, bucket)
		return nil
	}
	t.Cleanup(func() { syncToGCSForWorkspaceUpload = previous })
	return &buckets
}

// setupWorkspaceUploadServer is setupCreateAgentServer (a hub-managed
// project with no git remote and a remote broker with no local path) with
// the given hub storage.
func setupWorkspaceUploadServer(t *testing.T, stor storage.Storage) (*Server, store.Store, *store.Project, *createAgentDispatcher) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetStorage(stor)
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})
	return srv, s, project, disp
}

// TestCreateAgent_LocalStorageRemoteBrokerFailsWithoutSync covers a hub on
// local storage creating an agent in a project with no git remote on a
// remote broker with no local path: no sync runs, the create answers 412
// with the clear error, and no agent row or dispatch is left behind.
func TestCreateAgent_LocalStorageRemoteBrokerFailsWithoutSync(t *testing.T) {
	srv, s, project, disp := setupWorkspaceUploadServer(t, newContentMockStorage("local"))
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "local-storage-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body: %s", rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, ErrCodeUnsupportedCapability, resp.Error.Code)
	require.Equal(t, `cannot send the project workspace to a remote runtime broker: `+
		`hub storage is "local"; use GCS hub storage, add a git remote to the project, `+
		`or link the project at a local path on that broker`,
		resp.Error.Message)

	require.Empty(t, *uploads, "no workspace sync may run on a hub without GCS storage")
	require.Nil(t, disp.capturedAgent, "nothing may be dispatched")
	agents, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, agents.Items, "the agent row must be cleaned up")
}

// TestCreateAgent_GCSStorageRemoteBrokerUploads covers the GCS-backed hub
// the provider check leaves unchanged: the upload runs and the agent is
// pointed at the workspace storage path and bucket.
func TestCreateAgent_GCSStorageRemoteBrokerUploads(t *testing.T) {
	srv, _, project, disp := setupWorkspaceUploadServer(t, newGCSContentMockStorage("hub-bucket"))
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "gcs-storage-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Equal(t, []string{"hub-bucket"}, *uploads)
	require.NotNil(t, disp.capturedAgent)
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	require.Empty(t, disp.capturedAgent.AppliedConfig.Workspace)
	require.Equal(t, storage.ProjectWorkspaceStoragePath(srv.HubID(), project.ID), disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
	require.Equal(t, "hub-bucket", disp.capturedAgent.AppliedConfig.WorkspaceStorageBucket)
}

// TestCreateAgent_LocalStorageGitRemoteUnaffected covers a project with a
// git remote: the broker clones it, so a hub on local storage still creates
// the agent and runs no upload.
func TestCreateAgent_LocalStorageGitRemoteUnaffected(t *testing.T) {
	srv, s, project, disp := setupWorkspaceUploadServer(t, newContentMockStorage("local"))
	project.GitRemote = "github.com/example/repo"
	require.NoError(t, s.UpdateProject(context.Background(), project))
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "git-remote-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Empty(t, *uploads)
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
}

// TestCreateAgent_LocalStorageBrokerLocalPathUnaffected covers a broker
// that has the project at a local path: it needs no upload, so a hub on
// local storage still creates the agent.
func TestCreateAgent_LocalStorageBrokerLocalPathUnaffected(t *testing.T) {
	srv, s, project, disp := setupWorkspaceUploadServer(t, newContentMockStorage("local"))
	ctx := context.Background()
	provider, err := s.GetProjectProvider(ctx, project.ID, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	provider.LocalPath = t.TempDir()
	require.NoError(t, s.AddProjectProvider(ctx, provider))
	got, err := s.GetProjectProvider(ctx, project.ID, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	require.NotEmpty(t, got.LocalPath, "fixture check: the broker must have a local path")
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "local-path-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Empty(t, *uploads)
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
}

// makeSharedGitProject turns the fixture project into a shared-workspace
// project with a git remote.
func makeSharedGitProject(t *testing.T, s store.Store, project *store.Project) {
	t.Helper()
	project.GitRemote = "github.com/example/repo"
	if project.Labels == nil {
		project.Labels = map[string]string{}
	}
	project.Labels[store.LabelWorkspaceMode] = store.WorkspaceModeShared
	require.NoError(t, s.UpdateProject(context.Background(), project))
	require.True(t, project.IsSharedWorkspace(), "fixture check: shared-workspace project")
}

// sharedGitDispatch is what a shared-workspace git project's create hands
// to the broker: the dispatched agent's AppliedConfig, the project facts the
// dispatcher adds to it, and the hub-managed project path of that run.
type sharedGitDispatch struct {
	config      *store.AgentAppliedConfig
	info        projectDispatchInfo
	spec        WorkspaceDispatchSpec
	projectPath string
	uploads     []string
}

// createSharedGitAgent creates an agent in a shared-workspace git project on
// a remote broker with no local path, with the given hub storage (nil for
// none), and returns what was dispatched.
func createSharedGitAgent(t *testing.T, stor storage.Storage) sharedGitDispatch {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	if stor != nil {
		srv.SetStorage(stor)
	}
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})
	makeSharedGitProject(t, s, project)
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "shared-git-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
	require.NotNil(t, disp.capturedAgent.AppliedConfig)

	info, err := (&HTTPAgentDispatcher{store: s}).resolveDispatchProjectInfo(context.Background(), disp.capturedAgent)
	require.NoError(t, err)
	projectPath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	return sharedGitDispatch{
		config:      disp.capturedAgent.AppliedConfig,
		info:        info,
		spec:        workspaceSpecFor(disp.capturedAgent, info),
		projectPath: projectPath,
		uploads:     *uploads,
	}
}

// TestCreateAgent_LocalStorageSharedWorkspaceGitRemoteDispatchesWithoutSync
// covers a shared-workspace project with a git remote on a hub on local
// storage and a remote broker with no local path: the broker builds the
// shared workspace from the git remote, so the create runs no sync and
// dispatches exactly what a hub with no storage dispatches.
func TestCreateAgent_LocalStorageSharedWorkspaceGitRemoteDispatchesWithoutSync(t *testing.T) {
	want := createSharedGitAgent(t, nil)
	got := createSharedGitAgent(t, newContentMockStorage("local"))

	require.Empty(t, got.uploads, "no workspace sync may run on a hub without GCS storage")

	// The hub-managed workspace path is dispatched, as with no storage.
	require.Equal(t, want.projectPath, want.config.Workspace, "fixture check: nil storage dispatches the hub-managed path")
	require.Equal(t, got.projectPath, got.config.Workspace)
	require.Empty(t, got.config.WorkspaceStoragePath)
	require.Empty(t, got.config.WorkspaceStorageBucket)
	require.Equal(t, want.config.WorkspaceStoragePath, got.config.WorkspaceStoragePath)
	require.Equal(t, want.config.WorkspaceStorageBucket, got.config.WorkspaceStorageBucket)
	require.Equal(t, want.config.GitClone, got.config.GitClone)

	// The shared-workspace flag and clone config travel as with no storage.
	require.True(t, got.info.sharedWorkspace)
	require.Equal(t, want.info.sharedWorkspace, got.info.sharedWorkspace)
	require.Equal(t, want.info.workspaceMode, got.info.workspaceMode)
	require.Equal(t, want.info.sharedWorkspaceClone, got.info.sharedWorkspaceClone)
	require.Equal(t, want.spec, got.spec)
}

// TestCreateAgent_GCSStorageSharedWorkspaceGitRemoteUploads covers a
// shared-workspace project with a git remote on a GCS-backed hub: the
// provider check leaves it unchanged, so the upload runs and the agent is
// pointed at the workspace storage path and bucket.
func TestCreateAgent_GCSStorageSharedWorkspaceGitRemoteUploads(t *testing.T) {
	srv, s, project, disp := setupWorkspaceUploadServer(t, newGCSContentMockStorage("hub-bucket"))
	makeSharedGitProject(t, s, project)
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "gcs-shared-git-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Equal(t, []string{"hub-bucket"}, *uploads)
	require.NotNil(t, disp.capturedAgent)
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	require.Empty(t, disp.capturedAgent.AppliedConfig.Workspace)
	require.Equal(t, storage.ProjectWorkspaceStoragePath(srv.HubID(), project.ID), disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
	require.Equal(t, "hub-bucket", disp.capturedAgent.AppliedConfig.WorkspaceStorageBucket)
}

// TestCreateAgent_LocalStorageUnrelatedCallerWorkspaceDispatches covers a
// caller-supplied workspace outside the project's managed path on a hub on
// local storage: the upload is skipped for it whatever the storage, so the
// create dispatches as before instead of failing with the storage error.
func TestCreateAgent_LocalStorageUnrelatedCallerWorkspaceDispatches(t *testing.T) {
	srv, _, project, disp := setupWorkspaceUploadServer(t, newContentMockStorage("local"))
	uploads := uploadRecorder(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "unrelated-workspace-agent",
		ProjectID: project.ID,
		Workspace: t.TempDir(),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Empty(t, *uploads)
	require.NotNil(t, disp.capturedAgent, "the agent must be dispatched")
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	require.Empty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
}
