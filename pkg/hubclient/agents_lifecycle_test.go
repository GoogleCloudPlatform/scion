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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
