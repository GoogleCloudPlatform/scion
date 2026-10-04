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

package hubsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Acceptance (j), ptone/scion#2483: a DELETE the hub answers with 202 (slow
// delete still running, or a join that hit the deadline) is 2xx, so the sync
// carries on with the next removal and still records its watermark.
func TestExecuteSync_DeleteAnswered202DoesNotAbort(t *testing.T) {
	const projectID = "project-sync-202"
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		deleted = append(deleted, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/id-slow":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"agentId":"id-slow","deletion":{"state":"deleting","claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID, ProjectPath: projectDir, Settings: &config.Settings{}}
	serverTime := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	result := &SyncResult{
		ToRemove:   []AgentRef{{Name: "slow", ID: "id-slow"}, {Name: "fast", ID: "id-fast"}},
		ServerTime: serverTime,
	}

	require.NoError(t, ExecuteSync(context.Background(), hubCtx, result, true))
	assert.Equal(t, []string{
		"/api/v1/projects/" + projectID + "/agents/id-slow",
		"/api/v1/projects/" + projectID + "/agents/id-fast",
	}, deleted, "the 202 does not stop the next removal")

	st, err := config.LoadProjectState(projectDir)
	require.NoError(t, err)
	assert.Equal(t, serverTime.Format(time.RFC3339Nano), st.LastSyncedAt, "watermark recorded after a sync that saw a 202")
}
