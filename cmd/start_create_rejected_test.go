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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3430: a create the Hub rejects (non-2xx, no agent record) must
// end `scion start` at once with a non-zero exit and the Hub's message. It
// must not enter the launch wait or poll the agent afterwards.
func TestStartAgentViaHub_RejectedCreateExitsImmediately(t *testing.T) {
	const projectID, agentName = "proj-rejected", "rejected-agent"
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"validation 400", http.StatusBadRequest, "validation_error", "GCP service account not available in this project"},
		{"forbidden 403", http.StatusForbidden, "forbidden", "not allowed to create agents in this project"},
		{"unprocessable 422", http.StatusUnprocessableEntity, "validation_error", "template not found"},
		{"server error 500", http.StatusInternalServerError, "internal_error", "database unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origSA, origNoWait, origWaitTimeout := serviceAccountFlag, startNoWait, startWaitTimeout
			t.Cleanup(func() {
				serviceAccountFlag, startNoWait, startWaitTimeout = origSA, origNoWait, origWaitTimeout
			})
			serviceAccountFlag, startNoWait, startWaitTimeout = "sa-unavailable", false, 0

			var (
				mu            sync.Mutex
				createCalls   int
				afterCreate   []string // requests seen after the rejected create
				createBodyGCP interface{}
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if createCalls > 0 {
					afterCreate = append(afterCreate, r.Method+" "+r.URL.Path)
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
					createCalls++
					var body map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&body)
					createBodyGCP = body["gcp_identity"]
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{"code": tc.code, "message": tc.message},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "p"})
				default:
					// Existing-agent check (before create) and any poll (after):
					// the agent does not exist.
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{"code": "not_found", "message": "agent not found"},
					})
				}
			}))
			t.Cleanup(srv.Close)
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

			// Guard against a hang: a wait loop would block here until its
			// budget (minutes) runs out.
			done := make(chan struct{})
			var stdout, stderr string
			var startErr error
			go func() {
				defer close(done)
				stdout, stderr = captureStdIO(t, func() {
					startErr = startAgentViaHub(nil, hubCtx, agentName, "do it", false, nil)
				})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("startAgentViaHub did not return after the Hub rejected the create")
			}

			require.Error(t, startErr)
			assert.Contains(t, startErr.Error(), tc.message, "the Hub's error message must be surfaced")
			assert.NotZero(t, exitCodeFor(startErr), "a rejected create must exit non-zero")
			assert.True(t, isHubFailure(startErr), "a rejected create is a Hub failure, not a usage error")

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, 1, createCalls, "the create must be sent exactly once")
			assert.Equal(t, map[string]interface{}{"metadata_mode": "assign", "service_account_id": "sa-unavailable"}, createBodyGCP)
			assert.Empty(t, afterCreate, "no request (wait/poll) may follow a rejected create")
			assert.NotContains(t, stdout+stderr, "Waiting for agent", "a rejected create must not enter the launch wait")
		})
	}
}
