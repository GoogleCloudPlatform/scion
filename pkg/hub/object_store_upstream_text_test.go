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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The object-store failure paths answer the client fixed text with the same
// status (502) and code (runtime_error) as before, and log the object-store
// detail once, rendered by upstreamText.
//
// These tests must not run in parallel: they swap the default logger.

// errUpstreamMarker is an object-store error carrying upstreamMarkerBody.
var errUpstreamMarker = errors.New(upstreamMarkerBody)

// upstreamFaultStorage fails every signed URL, upload and existence check
// with errUpstreamMarker; the rest is served by the wrapped storage.
type upstreamFaultStorage struct {
	storage.Storage
}

func (upstreamFaultStorage) GenerateSignedURL(context.Context, string, storage.SignedURLOptions) (*storage.SignedURL, error) {
	return nil, errUpstreamMarker
}

func (upstreamFaultStorage) Upload(context.Context, string, io.Reader, storage.UploadOptions) (*storage.Object, error) {
	return nil, errUpstreamMarker
}

func (upstreamFaultStorage) Exists(context.Context, string) (bool, error) {
	return false, errUpstreamMarker
}

// failStorage swaps srv's storage for one that fails with errUpstreamMarker.
func failStorage(srv *Server) {
	srv.SetStorage(upstreamFaultStorage{srv.GetStorage()})
}

// assertObjectStoreFailureFixed asserts the fixed 502 runtime_error answer
// without the marker, and one logMsg record carrying it rendered.
func assertObjectStoreFailureFixed(t *testing.T, rec *httptest.ResponseRecorder, logs *bytes.Buffer, logMsg string) {
	t.Helper()
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeRuntimeError)
	assert.NotContains(t, rec.Body.String(), upstreamMarker)
	assertLoggedOnceRendered(t, logs, logMsg)
	assert.Equal(t, 1, strings.Count(logs.String(), upstreamMarker), "the marker reaches exactly one log record")
}

func TestTemplateUpload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload",
		UploadRequest{Files: uploadFiles("CLAUDE.md")})

	assertObjectStoreFailureFixed(t, rec, logs, "template upload URLs failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate upload URLs")
}

func TestTemplateDownload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/download", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "template download URLs failed")
	assert.Contains(t, rec.Body.String(), "scion template validate "+tmpl.Name)
}

func TestHarnessConfigUpload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, _, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/upload",
		UploadRequest{Files: uploadFiles("config.yaml")})

	assertObjectStoreFailureFixed(t, rec, logs, "harness-config upload URLs failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate upload URLs")
}

func TestHarnessConfigDownload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, _, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/download", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "harness-config download URLs failed")
	assert.Contains(t, rec.Body.String(), "scion harness-config validate "+hc.Name)
}

func TestSkillUpload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, _, alice, skill, stor := setupLocalSkill(t)
	publishLocalSkillDraft(t, srv, stor, alice, skill)
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload",
		map[string]interface{}{"version": "1.0.0", "files": uploadFiles("SKILL.md")})

	assertObjectStoreFailureFixed(t, rec, logs, "skill upload URLs failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate upload URLs")
}

func TestSkillMultipartPublish_ObjectStoreFailureFixedText(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "1.0.0"},
		map[string][]byte{"SKILL.md": []byte("---\nname: test\n---\n# Test Skill")})

	assertObjectStoreFailureFixed(t, rec, logs, "skill file upload failed")
	assert.Contains(t, rec.Body.String(), "Failed to upload files")
}

func TestSkillDownload_ObjectStoreFailureFixedText(t *testing.T) {
	srv, _, alice, skill, stor := setupLocalSkill(t)
	addLocalSkillVersion(t, srv, stor, skill, "1.0.0", map[string][]byte{"SKILL.md": []byte("# skill")})
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "skill download URLs failed")
	assert.Contains(t, rec.Body.String(), "failed to generate download URLs")
}

// workspaceSyncAgent creates a provisioning agent on the bootstrap server's
// broker, for the workspace sync-to routes.
func workspaceSyncAgent(t *testing.T, s store.Store, name string) *store.Agent {
	t.Helper()
	projectID, _ := setupProjectAndBroker(t, s)
	agent := &store.Agent{
		ID:              tid("agent_" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       projectID,
		RuntimeBrokerID: tid("broker_bootstrap_test"),
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig:   &store.AgentAppliedConfig{Task: "test task"},
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

func TestWorkspaceSyncTo_ObjectStoreFailureFixedText(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	agent := workspaceSyncAgent(t, s, "sync-to-upstream")
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doBootstrapRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to", agent.ID), SyncToRequest{
		Files: []transfer.FileInfo{{Path: "main.go", Size: 10, Hash: "sha256:abc"}},
	})

	assertObjectStoreFailureFixed(t, rec, logs, "workspace upload URL failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate upload URL")
}

func TestWorkspaceSyncToFinalize_ObjectStoreFailureFixedText(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	agent := workspaceSyncAgent(t, s, "finalize-upstream")
	failStorage(srv)
	logs := captureUpstreamLog(t)

	rec := doBootstrapRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/workspace/sync-to/finalize", agent.ID), SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0", Files: []transfer.FileInfo{{Path: "main.go", Size: 10, Hash: "sha256:abc"}}},
	})

	assertObjectStoreFailureFixed(t, rec, logs, "workspace file verify failed")
	assert.Contains(t, rec.Body.String(), "Failed to verify file")
}

func TestWorkspaceSyncFrom_ObjectStoreFailureFixedText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	srv.SetStorage(upstreamFaultStorage{newContentMockStorage("test-bucket")})
	agent := createSiteAgent(t, s, project, "sync-from-upstream", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	// The broker answers the tunneled upload with a one-file manifest, so
	// the hub signs a download URL for that file.
	go func() {
		for {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			body, _ := json.Marshal(RuntimeBrokerWorkspaceUploadResponse{Manifest: &transfer.Manifest{
				Version: "1.0",
				Files:   []transfer.FileInfo{{Path: "main.go", Size: 10, Hash: "sha256:abc"}},
			}})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			return
		}
	}()
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "workspace download URL failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate download URL")
}

// Agent create with workspace files: the upload URL failure answers fixed
// text, logs the detail once, and the rolled-back create's audit record
// carries the fixed cause, not the object-store text.
func TestCreateAgentWorkspaceUploadURL_ObjectStoreFailureFixedText(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	clearCreateAgentProviderPath(t, s, project)
	srv.SetStorage(upstreamFaultStorage{newMockStorage("test-bucket")})
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:           "upstream-text",
		ProjectID:      project.ID,
		Task:           "do something",
		WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
	})

	assertObjectStoreFailureFixed(t, rec, logs, "agent create: workspace upload URLs failed")
	assert.Contains(t, rec.Body.String(), "Failed to generate upload URLs")

	failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
	require.NoError(t, err)
	require.Len(t, failed, 1, "the rolled-back create is recorded once")
	var sum compensationSummary
	require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
	assert.Equal(t, createStageUploadURL, sum.Stage)
	assert.Equal(t, errWorkspaceUploadURLs.Error(), sum.Error)
	assert.NotContains(t, failed[0].AfterSummary, upstreamMarker)
}
