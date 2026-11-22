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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserService_ExecContainer(t *testing.T) {
	const agentID = "11111111-2222-3333-4444-555555555555"

	var currentID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/users/me/exec-container", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if currentID == "" {
				_ = json.NewEncoder(w).Encode(ExecContainerStatus{Configured: false})
				return
			}
			_ = json.NewEncoder(w).Encode(ExecContainerStatus{
				Configured: true,
				State:      "ready",
				Agent: &ExecContainerAgent{
					ID:          currentID,
					Slug:        "ops",
					Name:        "ops",
					ProjectID:   "proj-1",
					ProjectName: "My Project",
					Phase:       "running",
					Activity:    "idle",
				},
			})
		case http.MethodPut:
			var req setExecContainerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			currentID = req.AgentID
			_ = json.NewEncoder(w).Encode(ExecContainerStatus{
				Configured: true,
				State:      "ready",
				Agent: &ExecContainerAgent{
					ID:          currentID,
					Slug:        "ops",
					Name:        "ops",
					ProjectID:   "proj-1",
					ProjectName: "My Project",
					Phase:       "running",
					Activity:    "idle",
				},
			})
		case http.MethodDelete:
			currentID = ""
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)
	ctx := context.Background()

	got, err := c.Users().GetExecContainer(ctx)
	require.NoError(t, err)
	assert.False(t, got.Configured)

	setResp, err := c.Users().SetExecContainer(ctx, agentID)
	require.NoError(t, err)
	assert.True(t, setResp.Configured)
	assert.Equal(t, "ready", setResp.State)
	require.NotNil(t, setResp.Agent)
	assert.Equal(t, agentID, setResp.Agent.ID)

	got2, err := c.Users().GetExecContainer(ctx)
	require.NoError(t, err)
	assert.True(t, got2.Configured)
	assert.Equal(t, "My Project", got2.Agent.ProjectName)

	require.NoError(t, c.Users().ClearExecContainer(ctx))

	got3, err := c.Users().GetExecContainer(ctx)
	require.NoError(t, err)
	assert.False(t, got3.Configured)
}
