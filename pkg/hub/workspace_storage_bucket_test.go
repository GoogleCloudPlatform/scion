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
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/require"
)

// Tests for handing the workspace upload's bucket to the broker
// (ptone/scion#3422): the hub uploads a hub-managed workspace to its GCS
// bucket, and a standalone broker with no bucket setting must still be able
// to download it, or refuse with an explicit error rather than a 502.

// TestDispatchAgentCreate_SendsWorkspaceStorageBucket covers the create
// dispatch carrying the bucket recorded with the workspace storage path.
func TestDispatchAgentCreate_SendsWorkspaceStorageBucket(t *testing.T) {
	f := newTZDispatchFixture(t, "")
	f.agent.AppliedConfig.WorkspaceStoragePath = "workspaces/p/a"
	f.agent.AppliedConfig.WorkspaceStorageBucket = "hub-bucket"

	_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	req := f.client.lastCreateReq
	require.NotNil(t, req)
	require.Equal(t, "workspaces/p/a", req.WorkspaceStoragePath)
	require.Equal(t, "hub-bucket", req.WorkspaceStorageBucket)
}

// TestDispatchAgentCreate_LegacyRowSendsNoBucket covers an agent recorded
// before the bucket was stored: the path is still sent and the bucket is
// left empty, so the broker falls back to its own setting.
func TestDispatchAgentCreate_LegacyRowSendsNoBucket(t *testing.T) {
	f := newTZDispatchFixture(t, "")
	f.agent.AppliedConfig.WorkspaceStoragePath = "workspaces/p/a"

	_, err := f.d.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	req := f.client.lastCreateReq
	require.NotNil(t, req)
	require.Equal(t, "workspaces/p/a", req.WorkspaceStoragePath)
	require.Empty(t, req.WorkspaceStorageBucket)
}

// TestDispatchCreateErrorResponse_WorkspaceStorageUnconfiguredRelays422
// covers the broker's refusal for a workspace upload it has no bucket for
// reaching the client as the broker's 422 and code, not a 502.
func TestDispatchCreateErrorResponse_WorkspaceStorageUnconfiguredRelays422(t *testing.T) {
	err := &brokerStatusError{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       `{"error":{"code":"workspace_storage_unconfigured","message":"Cannot download the uploaded workspace: no bucket"}}`,
	}

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err, "")

	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Equal(t, workspaceStorageUnconfiguredErrorCode, resp.Error.Code)
	require.Equal(t, "Failed to dispatch to runtime broker: Cannot download the uploaded workspace: no bucket", resp.Error.Message)
}

// TestDispatchCreateErrorResponse_Other422Stays502 keeps the relay narrow: a
// broker 422 with a different code still maps to the generic 502.
func TestDispatchCreateErrorResponse_Other422Stays502(t *testing.T) {
	err := &brokerStatusError{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       `{"error":{"code":"validation_error","message":"something else"}}`,
	}

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err, "")

	require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
}

// workspaceStorageUnconfiguredBrokerErr is the broker's refusal for a
// workspace upload it has no bucket for, as it arrives at the hub.
func workspaceStorageUnconfiguredBrokerErr() error {
	return &brokerStatusError{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       `{"error":{"code":"workspace_storage_unconfigured","message":"Cannot download the uploaded workspace: no bucket"}}`,
	}
}

// requireWorkspaceStorageUnconfigured422 asserts rec carries the relayed
// broker refusal with the same status, code and message as create.
func requireWorkspaceStorageUnconfigured422(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, workspaceStorageUnconfiguredErrorCode, resp.Error.Code)
	require.Equal(t, "Failed to dispatch to runtime broker: Cannot download the uploaded workspace: no bucket", resp.Error.Message)
}

// TestSyncToFinalize_WorkspaceStorageUnconfiguredRelays422 covers the
// sync-to finalize dispatch relaying the broker's no-bucket refusal as the
// same 422 the create path gives, not a 500. With local hub storage no
// bucket is recorded, so a broker without its own bucket answers this way.
func TestSyncToFinalize_WorkspaceStorageUnconfiguredRelays422(t *testing.T) {
	srv, s, stor, disp := testBootstrapServer(t)
	disp.returnErr = workspaceStorageUnconfiguredBrokerErr()
	projectID, _ := setupProjectAndBroker(t, s)
	ctx := context.Background()

	agentID := tid("agent_finalize_no_bucket")
	agent := &store.Agent{
		ID:              agentID,
		Slug:            "finalize-no-bucket",
		Name:            "Finalize No Bucket",
		ProjectID:       projectID,
		RuntimeBrokerID: tid("broker_bootstrap_test"),
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig:   &store.AgentAppliedConfig{Task: "test task"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	storagePath := "workspaces/" + projectID + "/" + agentID
	stor.objects[storagePath+"/files/main.go"] = &storage.Object{Name: storagePath + "/files/main.go"}

	rec := doBootstrapRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agentID), SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0", Files: []transfer.FileInfo{{Path: "main.go", Size: 10, Hash: "sha256:abc"}}},
	})
	requireWorkspaceStorageUnconfigured422(t, rec)
}

// workspaceStorageUnconfiguredFinalizeDispatcher answers finalize-env with
// the broker's no-bucket refusal.
type workspaceStorageUnconfiguredFinalizeDispatcher struct {
	createAgentDispatcher
}

func (d *workspaceStorageUnconfiguredFinalizeDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return nil, workspaceStorageUnconfiguredBrokerErr()
}

// TestSubmitAgentEnv_WorkspaceStorageUnconfiguredRelays422 covers
// finalize-env, which creates the agent on the broker after an env gather
// and so can meet the same refusal: it is relayed as create's 422, not a 500.
func TestSubmitAgentEnv_WorkspaceStorageUnconfiguredRelays422(t *testing.T) {
	disp := &workspaceStorageUnconfiguredFinalizeDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	agent := &store.Agent{
		ID:              tid("agent-env-no-bucket"),
		Name:            "env-no-bucket",
		Slug:            "env-no-bucket",
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/env-no-bucket/env",
		SubmitEnvRequest{Env: map[string]string{"API_KEY": "v"}})
	requireWorkspaceStorageUnconfigured422(t, rec)
}

func TestWorkspaceDownloadBucket(t *testing.T) {
	gcs := newMockStorage("hub-bucket")
	gcs.provider = storage.ProviderGCS
	require.Equal(t, "hub-bucket", workspaceDownloadBucket(gcs))
	require.Empty(t, workspaceDownloadBucket(newMockStorage("local-bucket")), "local storage is not downloadable by a broker")
	require.Empty(t, workspaceDownloadBucket(nil))
}

// TestCreateAgent_HubManagedUploadRecordsBucket covers the create path that
// uploads a hub-managed project workspace for a remote broker: the bucket
// it uploaded to is recorded with the storage path the dispatch sends.
func TestCreateAgent_HubManagedUploadRecordsBucket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
	srv.SetStorage(newContentMockStorage("test-bucket"))
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})

	var uploadedBucket string
	previous := syncToGCSForWorkspaceUpload
	syncToGCSForWorkspaceUpload = func(_ context.Context, _, bucket, _ string) error {
		uploadedBucket = bucket
		return nil
	}
	t.Cleanup(func() { syncToGCSForWorkspaceUpload = previous })

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "upload-bucket-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.Equal(t, "test-bucket", uploadedBucket, "fixture check: the upload must have run")
	require.NotNil(t, disp.capturedAgent)
	require.NotNil(t, disp.capturedAgent.AppliedConfig)
	require.NotEmpty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
	require.Equal(t, "test-bucket", disp.capturedAgent.AppliedConfig.WorkspaceStorageBucket)
}
