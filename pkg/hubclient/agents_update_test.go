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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateBody sends req through Agents().Update against a test server and
// returns the PATCH body the server received, decoded as a JSON object.
func updateBody(t *testing.T, req *UpdateAgentRequest) (map[string]json.RawMessage, error) {
	t.Helper()
	var got map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-1", r.URL.Path)
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"agent-1"}`))
	}))
	defer server.Close()
	client, err := New(server.URL)
	require.NoError(t, err)
	_, err = client.Agents().Update(context.Background(), "agent-1", req)
	return got, err
}

func TestUpdateAgentRequest_SendsConfigAndNewFields(t *testing.T) {
	role := "readonly"
	tz := "Europe/Paris"
	got, err := updateBody(t, &UpdateAgentRequest{
		StateVersion:     7,
		Config:           &api.ScionConfig{Model: "claude-opus-4-6", MaxTurns: api.IntPtr(50)},
		GCPIdentity:      &GCPIdentityConfig{MetadataMode: "block"},
		AgentRole:        &role,
		ExplicitTimezone: &tz,
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"claude-opus-4-6","max_turns":50}`, string(got["config"]))
	assert.JSONEq(t, `{"metadata_mode":"block"}`, string(got["gcp_identity"]))
	assert.JSONEq(t, `"readonly"`, string(got["agentRole"]))
	assert.JSONEq(t, `"Europe/Paris"`, string(got["explicitTimezone"]))
	assert.JSONEq(t, `7`, string(got["stateVersion"]))
	assert.NotContains(t, got, "task", "Task is sent as config.task, never as a top-level key")
}

func TestUpdateAgentRequest_OmitsUnsetFields(t *testing.T) {
	got, err := updateBody(t, &UpdateAgentRequest{Name: "renamed", StateVersion: 3})
	require.NoError(t, err)
	for _, k := range []string{"config", "gcp_identity", "agentRole", "explicitTimezone", "task"} {
		assert.NotContains(t, got, k)
	}
}

func TestUpdateAgentRequest_TaskIsSentAsConfigTask(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		got, err := updateBody(t, &UpdateAgentRequest{Task: "fix the build"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"task":"fix the build"}`, string(got["config"]))
	})
	t.Run("with config", func(t *testing.T) {
		req := &UpdateAgentRequest{Task: "fix the build", Config: &api.ScionConfig{Model: "m"}}
		got, err := updateBody(t, req)
		require.NoError(t, err)
		assert.JSONEq(t, `{"model":"m","task":"fix the build"}`, string(got["config"]))
		assert.Empty(t, req.Config.Task, "the caller's Config is not modified")
	})
}

func TestUpdateAgentRequest_TaskAndConfigTaskIsAnError(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := New(server.URL)
	require.NoError(t, err)
	_, err = client.Agents().Update(context.Background(), "agent-1", &UpdateAgentRequest{
		Task:   "one",
		Config: &api.ScionConfig{Task: "two"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set Task or Config.Task, not both")
	assert.False(t, called, "nothing is sent")
}

func TestUpdateAgentRequest_ConfigLeavesOutUnsetPointerKeys(t *testing.T) {
	got, err := updateBody(t, &UpdateAgentRequest{Config: &api.ScionConfig{MaxTurns: api.IntPtr(5)}})
	require.NoError(t, err)
	// api.ScionConfig.Detached has no omitempty; the hub must not read a
	// null detached as an edit of it.
	assert.JSONEq(t, `{"max_turns":5}`, string(got["config"]))
}
