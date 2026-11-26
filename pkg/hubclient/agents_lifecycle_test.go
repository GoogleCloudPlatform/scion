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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleTestClient(t *testing.T, status int, body string, gotPath *string) Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.Method + " " + r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(t, err)
	return client
}

func TestAgentLifecycle_QueuedStopReturnsWarnings(t *testing.T) {
	var path string
	client := lifecycleTestClient(t, http.StatusAccepted,
		`{"id":"a1","name":"alpha","phase":"stopped","warnings":["Stop queued: broker offline"]}`, &path)

	resp, err := client.Agents().Stop(context.Background(), "a1")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.True(t, resp.Queued)
	assert.Equal(t, []string{"Stop queued: broker offline"}, resp.Warnings)
	require.NotNil(t, resp.Agent)
	assert.Equal(t, "a1", resp.Agent.ID)
	assert.True(t, strings.HasPrefix(path, "POST "), path)
	assert.True(t, strings.HasSuffix(path, "/a1/stop"), path)
}

func TestAgentLifecycle_OKResponseIsNotQueued(t *testing.T) {
	client := lifecycleTestClient(t, http.StatusOK, `{"id":"a1","name":"alpha","phase":"running"}`, nil)

	resp, err := client.Agents().Start(context.Background(), "a1")
	require.NoError(t, err)
	assert.False(t, resp.Queued)
	assert.Empty(t, resp.Warnings)
	require.NotNil(t, resp.Agent)
	assert.Equal(t, "running", resp.Agent.Phase)
}

func TestAgentLifecycle_EmptyBodyIsTolerated(t *testing.T) {
	client := lifecycleTestClient(t, http.StatusOK, ``, nil)

	resp, err := client.Agents().Restart(context.Background(), "a1")
	require.NoError(t, err)
	assert.False(t, resp.Queued)
	assert.Nil(t, resp.Agent)
}

func TestAgentLifecycle_ErrorStatusReturnsError(t *testing.T) {
	client := lifecycleTestClient(t, http.StatusConflict,
		`{"error":{"code":"conflict","message":"agent is not running"}}`, nil)

	resp, err := client.Agents().Suspend(context.Background(), "a1")
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not running")
}

// A start or restart that loses to a delete is answered 409
// delete_in_progress with no agent body (ptone/scion#3255): the client
// returns the API error, not a response.
func TestAgentLifecycle_DeleteInProgressReturnsError(t *testing.T) {
	body := `{"error":{"code":"delete_in_progress","message":"agent was deleted, or is being deleted, while it was starting",` +
		`"details":{"agentId":"a1","warnings":["agent was deleted while it was starting; its container was removed"]}}}`
	calls := map[string]func(Client) (*LifecycleResponse, error){
		"start":   func(c Client) (*LifecycleResponse, error) { return c.Agents().Start(context.Background(), "a1") },
		"restart": func(c Client) (*LifecycleResponse, error) { return c.Agents().Restart(context.Background(), "a1") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			resp, err := call(lifecycleTestClient(t, http.StatusConflict, body, nil))
			require.Error(t, err)
			assert.Nil(t, resp)
			var apiErr *apiclient.APIError
			require.True(t, errors.As(err, &apiErr), "%T", err)
			assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
			assert.Equal(t, "delete_in_progress", apiErr.Code)
			assert.Contains(t, err.Error(), "while it was starting")
		})
	}
}

func TestAgentLifecycle_SuspendWithOptionsAndTypedErrors(t *testing.T) {
	t.Run("force sends JSON body", func(t *testing.T) {
		var gotBody []byte
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.Method + " " + r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"a1","name":"lead","phase":"suspended","suspendPolicy":"never"}`))
		}))
		defer server.Close()

		client, err := New(server.URL)
		require.NoError(t, err)

		suspender, ok := client.Agents().(LifecycleOptionsSuspender)
		require.True(t, ok, "agentService must implement LifecycleOptionsSuspender")

		resp, err := suspender.SuspendWithOptions(context.Background(), "a1", LifecycleSuspendOptions{Force: true})
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotNil(t, resp.Agent)
		assert.Equal(t, "suspended", resp.Agent.Phase)
		assert.Equal(t, "never", resp.Agent.SuspendPolicy)
		assert.Equal(t, "POST /api/v1/agents/a1/suspend", gotPath)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(gotBody, &decoded))
		assert.Equal(t, true, decoded["force"])
	})

	t.Run("zero options sends no body", func(t *testing.T) {
		var gotLen int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			gotLen = len(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"a1","name":"worker","phase":"suspended"}`))
		}))
		defer server.Close()

		client, err := New(server.URL)
		require.NoError(t, err)

		suspender := client.Agents().(LifecycleOptionsSuspender)
		_, err = suspender.SuspendWithOptions(context.Background(), "a1", LifecycleSuspendOptions{})
		require.NoError(t, err)
		assert.Equal(t, 0, gotLen)
	})

	t.Run("409 agent_never_suspend matches IsAgentNeverSuspendError", func(t *testing.T) {
		client := lifecycleTestClient(t, http.StatusConflict,
			`{"error":{"code":"agent_never_suspend","message":"agent \"lead\" has suspend policy \"never\"; pass force=true to suspend it"}}`, nil)

		_, err := client.Agents().Suspend(context.Background(), "lead")
		require.Error(t, err)
		assert.True(t, apiclient.IsAgentNeverSuspendError(err))
		assert.False(t, apiclient.IsForceSuspendForbiddenError(err))
	})

	t.Run("403 force_suspend_forbidden matches IsForceSuspendForbiddenError", func(t *testing.T) {
		client := lifecycleTestClient(t, http.StatusForbidden,
			`{"error":{"code":"force_suspend_forbidden","message":"only human principals may force-suspend a never-suspend agent"}}`, nil)

		suspender := client.Agents().(LifecycleOptionsSuspender)
		_, err := suspender.SuspendWithOptions(context.Background(), "lead", LifecycleSuspendOptions{Force: true})
		require.Error(t, err)
		assert.True(t, apiclient.IsForceSuspendForbiddenError(err))
		assert.False(t, apiclient.IsAgentNeverSuspendError(err))
	})
}
