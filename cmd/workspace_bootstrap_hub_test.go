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

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapHubStub is an httptest hub for a start with a non-git workspace
// bootstrap: create returns upload URLs on the stub, uploads are stored, and
// finalize is served only on the by-ID route for the created agent's ID.
type bootstrapHubStub struct {
	server    *httptest.Server
	projectID string
	agentName string
	agentID   string

	existingPhase  string // "" → the existing-agent GET returns 404
	existingID     string // ID of the existing agent ("" → agentID)
	createNoID     bool   // the create answer carries no agent ID
	uploadStatus   int    // non-zero → uploads fail with this status
	finalizeStatus int    // non-zero → finalize fails with this status
	deleteStatus   int    // non-zero → DELETE fails with this status

	mu         sync.Mutex
	createdFor []string          // workspace file paths sent with the create
	uploaded   map[string]string // path → uploaded content
	finalized  []string          // agent identifiers finalize was called with
	deleted    []string          // paths of DELETE requests
	unexpected []string          // requests the stub did not serve
}

func newBootstrapHubStub(t *testing.T, projectID, agentName, agentID string) *bootstrapHubStub {
	t.Helper()
	stub := &bootstrapHubStub{projectID: projectID, agentName: agentName, agentID: agentID, uploaded: map[string]string{}}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(stub.server.Close)
	return stub
}

func writeStubError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

func (s *bootstrapHubStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	projectPath := "/api/v1/projects/" + s.projectID
	finalizePrefix := "/api/v1/agents/"
	finalizeSuffix := "/workspace/sync-to/finalize"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == projectPath:
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": s.projectID, "name": "p"})
	case r.Method == http.MethodGet && r.URL.Path == projectPath+"/agents/"+s.agentName:
		if s.existingPhase == "" {
			writeStubError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		existingID := s.existingID
		if existingID == "" {
			existingID = s.agentID
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": existingID, "slug": s.agentName, "name": s.agentName,
			"phase": s.existingPhase, "status": s.existingPhase,
		})
	case r.Method == http.MethodPost && r.URL.Path == projectPath+"/agents":
		var req hubclient.CreateAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		urls := make([]transfer.UploadURLInfo, 0, len(req.WorkspaceFiles))
		for _, f := range req.WorkspaceFiles {
			s.createdFor = append(s.createdFor, f.Path)
			urls = append(urls, transfer.UploadURLInfo{Path: f.Path, URL: s.server.URL + "/upload/" + f.Path, Method: http.MethodPut})
		}
		createdID := s.agentID
		if s.createNoID {
			createdID = ""
		}
		_ = json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
			Agent: &hubclient.Agent{
				ID: createdID, Slug: s.agentName, Name: s.agentName,
				Status: "provisioning", Phase: "provisioning", Created: time.Now().UTC(),
			},
			UploadURLs: urls,
		})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/"):
		if s.uploadStatus != 0 {
			w.WriteHeader(s.uploadStatus)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.uploaded[strings.TrimPrefix(r.URL.Path, "/upload/")] = string(body)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, finalizePrefix) && strings.HasSuffix(r.URL.Path, finalizeSuffix):
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, finalizePrefix), finalizeSuffix)
		s.finalized = append(s.finalized, ref)
		if ref != s.agentID {
			writeStubError(w, http.StatusNotFound, "not_found", "agent not found")
			return
		}
		if s.finalizeStatus != 0 {
			writeStubError(w, s.finalizeStatus, "internal_error", "finalize failed")
			return
		}
		_ = json.NewEncoder(w).Encode(&hubclient.SyncToFinalizeResponse{FilesApplied: len(s.uploaded)})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, projectPath+"/agents/"):
		s.deleted = append(s.deleted, r.URL.Path)
		if s.deleteStatus != 0 {
			writeStubError(w, s.deleteStatus, "internal_error", "delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.unexpected = append(s.unexpected, r.Method+" "+r.URL.Path)
		writeStubError(w, http.StatusNotFound, "not_found", "not served by stub")
	}
}

// startMarkerLayoutAgent runs startAgentViaHub for a marker-layout project
// against stub without waiting for the launch. It runs from the workspace
// root, or, with fromElsewhere, from another directory with the workspace
// given as the explicit project path. It returns the start's stderr and
// error.
func startMarkerLayoutAgent(t *testing.T, stub *bootstrapHubStub, fromElsewhere bool) (stderr string, err error) {
	t.Helper()
	resetHubStartGlobals(t)
	origNoWait, origProjectPath := startNoWait, projectPath
	t.Cleanup(func() { startNoWait, projectPath = origNoWait, origProjectPath })
	startNoWait, projectPath = true, ""
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv(projectkeys.EnvProjectID, "")

	workspace, resolved := setupMarkerLayoutProject(t)
	if fromElsewhere {
		t.Chdir(t.TempDir())
		projectPath = workspace
	} else {
		t.Chdir(workspace)
	}
	client, cerr := hubclient.New(stub.server.URL)
	require.NoError(t, cerr)
	hubCtx := &HubContext{Client: client, Endpoint: stub.server.URL, ProjectID: stub.projectID, ProjectPath: resolved}
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, stub.agentName, "", false, nil) })
	})
	return stderr, err
}

// A marker-layout start uploads the workspace-root files and finalizes the
// agent create returned, by its ID.
func TestStartAgentViaHub_MarkerLayoutBootstrapFinalizesCreatedAgent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fromElsewhere bool
	}{{"from the workspace", false}, {"project path given from another directory", true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newBootstrapHubStub(t, "proj-ws", "ws-agent", "0a1b2c3d-0000-4000-8000-00000000a001")
			_, err := startMarkerLayoutAgent(t, stub, tc.fromElsewhere)
			require.NoError(t, err)

			created := slices.Clone(stub.createdFor)
			slices.Sort(created)
			assert.Equal(t, []string{"main.go", "sub/notes.txt"}, created, "workspace files sent with the create")
			assert.Equal(t, map[string]string{"main.go": "main.go", "sub/notes.txt": "sub/notes.txt"}, stub.uploaded, "uploaded workspace files")
			assert.Equal(t, []string{stub.agentID}, stub.finalized, "finalize identifiers")
			assert.Empty(t, stub.deleted, "no agent deleted")
			assert.Empty(t, stub.unexpected, "unexpected requests")
		})
	}
}

func TestStartAgentViaHub_BootstrapWithoutAgentID(t *testing.T) {
	stub := newBootstrapHubStub(t, "proj-ws", "ws-agent", "0a1b2c3d-0000-4000-8000-00000000a003")
	stub.createNoID = true
	_, err := startMarkerLayoutAgent(t, stub, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the hub returned no agent ID for 'ws-agent'")
	assert.Empty(t, stub.uploaded, "nothing uploaded")
	assert.Empty(t, stub.finalized, "no finalize")
}

func TestStartAgentViaHub_FailedBootstrapLeavesNoProvisioningAgent(t *testing.T) {
	const projectID, agentName, agentID = "proj-ws", "ws-agent", "0a1b2c3d-0000-4000-8000-00000000a002"
	deletePath := "/api/v1/projects/" + projectID + "/agents/" + agentID

	t.Run("finalize fails", func(t *testing.T) {
		stub := newBootstrapHubStub(t, projectID, agentName, agentID)
		stub.finalizeStatus = http.StatusInternalServerError
		_, err := startMarkerLayoutAgent(t, stub, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to finalize workspace bootstrap")
		assert.Equal(t, []string{deletePath}, stub.deleted, "the created agent is removed")
	})

	t.Run("upload fails", func(t *testing.T) {
		stub := newBootstrapHubStub(t, projectID, agentName, agentID)
		stub.uploadStatus = http.StatusForbidden
		_, err := startMarkerLayoutAgent(t, stub, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to upload workspace files")
		assert.Empty(t, stub.finalized, "no finalize after a failed upload")
		assert.Equal(t, []string{deletePath}, stub.deleted, "the created agent is removed")
	})

	t.Run("existing agent is kept", func(t *testing.T) {
		stub := newBootstrapHubStub(t, projectID, agentName, agentID)
		stub.existingPhase = "stopped"
		stub.finalizeStatus = http.StatusInternalServerError
		_, err := startMarkerLayoutAgent(t, stub, false)
		require.Error(t, err)
		assert.Empty(t, stub.deleted, "an agent that existed before the start is not deleted")
	})

	t.Run("existing provisioning agent replaced by the create", func(t *testing.T) {
		stub := newBootstrapHubStub(t, projectID, agentName, agentID)
		stub.existingPhase = "provisioning"
		stub.existingID = "0a1b2c3d-0000-4000-8000-0000000000ff"
		stub.finalizeStatus = http.StatusInternalServerError
		_, err := startMarkerLayoutAgent(t, stub, false)
		require.Error(t, err)
		assert.Equal(t, []string{deletePath}, stub.deleted, "the agent the create returned is removed")
	})

	t.Run("cleanup failure is reported", func(t *testing.T) {
		stub := newBootstrapHubStub(t, projectID, agentName, agentID)
		stub.finalizeStatus = http.StatusInternalServerError
		stub.deleteStatus = http.StatusInternalServerError
		stderr, err := startMarkerLayoutAgent(t, stub, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to finalize workspace bootstrap")
		assert.Equal(t, []string{deletePath}, stub.deleted, "the delete was attempted")
		assert.Contains(t, stderr, "Warning: failed to clean up provisioning agent 'ws-agent'")
	})
}
