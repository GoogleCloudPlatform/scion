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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	old, oldFormat, oldConfirm, oldPath := stopRm, outputFormat, autoConfirm, projectPath
	stopRm, outputFormat, autoConfirm = rm, "", true
	// A confirmed stop --rm runs local cleanup; keep it inside temp dirs.
	t.Setenv("HOME", t.TempDir())
	projectPath = filepath.Join(t.TempDir(), ".scion")
	t.Cleanup(func() { stopRm, outputFormat, autoConfirm, projectPath = old, oldFormat, oldConfirm, oldPath })
}

const queuedStopBody = `{"id":"a1","name":"alpha","phase":"stopped","warnings":["Stop queued: broker offline; it will be applied when the broker reconnects."]}`

func TestStopViaHub_QueuedStopWithRmSkipsDelete(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusAccepted, queuedStopBody)
	agentDir := createAgentDir(t, projectPath, "alpha")

	var runErr error
	stderr := captureStderr(t, func() { runErr = stopAgentViaHub(hubCtx, "alpha") })
	require.NoError(t, runErr)
	assert.Zero(t, deletes.Load(), "a queued stop must not delete the agent")
	assert.DirExists(t, agentDir, "a queued stop keeps local files")
	assert.Contains(t, stderr, "Warning: Stop queued: broker offline")
	assert.Contains(t, stderr, stopQueuedNotRemovedMessage)
}

func TestStopViaHub_StopWithRmDeletesWhenApplied(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusOK, `{"id":"a1","name":"alpha","phase":"stopped"}`)
	agentDir := createAgentDir(t, projectPath, "alpha")

	var runErr error
	_ = captureStderr(t, func() { runErr = stopAgentViaHub(hubCtx, "alpha") })
	require.NoError(t, runErr)
	assert.EqualValues(t, 1, deletes.Load())
	assert.NoDirExists(t, agentDir, "a confirmed (204) removal cleans up local files (ptone/scion#2896)")
}

// ptone/scion#2896: stop --all --rm cleans up local files for a confirmed
// (204) removal, and reports it in JSON without warnings.
func TestStopAllViaHub_StopWithRmCleansUpLocalFiles(t *testing.T) {
	withStopRm(t, true)
	hubCtx, deletes := stopTestHub(t, http.StatusOK, `{"id":"a1","name":"alpha","phase":"stopped"}`)
	agentDir := createAgentDir(t, projectPath, "alpha")
	outputFormat = "json"

	var runErr error
	stdout := captureStdout(t, func() { runErr = stopAllAgentsViaHub(hubCtx) })
	require.NoError(t, runErr)
	assert.EqualValues(t, 1, deletes.Load())
	assert.NoDirExists(t, agentDir)
	assert.Contains(t, stdout, `"removed": true`)
	assert.NotContains(t, stdout, "local cleanup failed")
}

func TestStopRmCleanupWarning(t *testing.T) {
	w := stopRmCleanupWarning("alpha", errors.New("boom"))
	assert.Equal(t, "removed via Hub but local cleanup failed: boom; run 'scion --no-hub delete --preserve-branch alpha' to retry", w,
		"stop --rm keeps the branch, so the retry hint must too")
}

func TestNoHubDeleteCommand(t *testing.T) {
	assert.Equal(t, "scion --no-hub delete --preserve-branch alpha", noHubDeleteCommand("alpha", true))
	assert.Equal(t, "scion --no-hub delete alpha", noHubDeleteCommand("alpha", false))
}

// ptone/scion#2896: when the Hub confirms a stop --rm removal but local
// cleanup fails, the command still succeeds and reports the failure as a
// warning: on stderr in text mode, in "warnings" in JSON mode.
func TestStopViaHub_RmLocalCleanupFailureWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	const wantHint = "local cleanup failed"
	const wantRetry = "scion --no-hub delete --preserve-branch alpha"
	for _, tc := range []struct {
		name string
		all  bool
		json bool
	}{
		{"single/text", false, false},
		{"single/json", false, true},
		{"all/text", true, false},
		{"all/json", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStopRm(t, true)
			hubCtx, deletes := stopTestHub(t, http.StatusOK, `{"id":"a1","name":"alpha","phase":"stopped"}`)
			agentDir := createAgentDir(t, projectPath, "alpha")
			// A read-only agents dir makes removing the agent dir fail.
			agentsDir := filepath.Dir(agentDir)
			require.NoError(t, os.Chmod(agentsDir, 0o555))
			t.Cleanup(func() { _ = os.Chmod(agentsDir, 0o755) })
			if tc.json {
				outputFormat = "json"
			}

			var runErr error
			var stdout string
			stderr := captureStderr(t, func() {
				stdout = captureStdout(t, func() {
					if tc.all {
						runErr = stopAllAgentsViaHub(hubCtx)
					} else {
						runErr = stopAgentViaHub(hubCtx, "alpha")
					}
				})
			})
			require.NoError(t, runErr, "a local cleanup failure does not fail the command")
			assert.EqualValues(t, 1, deletes.Load())
			assert.DirExists(t, agentDir, "cleanup failed, so the files are still there")

			if !tc.json {
				prefix := "Warning: "
				if tc.all {
					prefix = "Agent 'alpha': warning: "
				}
				assert.Contains(t, stderr, prefix+"removed via Hub but "+wantHint)
				assert.Contains(t, stderr, wantRetry)
				return
			}
			var warnings []interface{}
			if tc.all {
				var out jsonResultBody
				require.NoError(t, json.Unmarshal([]byte(jsonBodyOf(stdout)), &out), stdout)
				assert.Equal(t, "success", out.Status)
				require.Len(t, out.Results, 1)
				assert.Equal(t, true, out.Results[0]["removed"])
				warnings, _ = out.Results[0]["warnings"].([]interface{})
			} else {
				var out map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
				assert.Equal(t, "success", out["status"])
				warnings, _ = out["warnings"].([]interface{})
			}
			require.Len(t, warnings, 1, stdout)
			assert.Contains(t, warnings[0], wantHint)
			assert.Contains(t, warnings[0], wantRetry)
		})
	}
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
