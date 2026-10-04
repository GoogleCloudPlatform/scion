// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopTestHub serves a single-agent project whose stop answers with
// stopStatus, and counts DELETE requests.
func stopTestHub(t *testing.T, stopStatus int, stopBody string) (*HubContext, *atomic.Int32) {
	t.Helper()
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(stopStatus)
			_, _ = w.Write([]byte(stopBody))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
			_, _ = w.Write([]byte(`{"agents":[{"id":"a1","name":"alpha","phase":"running"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: server.URL, ProjectID: "p1"}, &deletes
}

func withStopRm(t *testing.T, rm bool) {
	t.Helper()
	old, oldFormat, oldConfirm := stopRm, outputFormat, autoConfirm
	stopRm, outputFormat, autoConfirm = rm, "", true
	t.Cleanup(func() { stopRm, outputFormat, autoConfirm = old, oldFormat, oldConfirm })
}

const queuedStopBody = `{"id":"a1","name":"alpha","phase":"stopped","warnings":["Stop queued: broker offline; it will be applied when the broker reconnects."]}`

func TestStopViaHub_QueuedStopWithRmSkipsDelete(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusAccepted, queuedStopBody)

	var runErr error
	stderr := captureStderr(t, func() { runErr = stopAgentViaHub(hubCtx, "alpha") })
	require.NoError(t, runErr)
	assert.Zero(t, deletes.Load(), "a queued stop must not delete the agent")
	assert.Contains(t, stderr, "Warning: Stop queued: broker offline")
	assert.Contains(t, stderr, stopQueuedNotRemovedMessage)
}

func TestStopViaHub_StopWithRmDeletesWhenApplied(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusOK, `{"id":"a1","name":"alpha","phase":"stopped"}`)

	var runErr error
	_ = captureStderr(t, func() { runErr = stopAgentViaHub(hubCtx, "alpha") })
	require.NoError(t, runErr)
	assert.EqualValues(t, 1, deletes.Load())
}

func TestStopAllViaHub_QueuedStopWithRmSkipsDelete(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusAccepted, queuedStopBody)

	var runErr error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { runErr = stopAllAgentsViaHub(hubCtx) })
	})
	require.NoError(t, runErr)
	assert.Zero(t, deletes.Load(), "a queued stop must not delete the agent")
	assert.Contains(t, stderr, "Agent 'alpha': warning: Stop queued: broker offline")
	assert.Contains(t, stderr, "Agent 'alpha': "+stopQueuedNotRemovedMessage)
}
