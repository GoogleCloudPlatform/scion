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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2894: outputJSONResult writes the body and, on failure,
// returns an error that exits 1 but is marked as already reported.
func TestOutputJSONResult(t *testing.T) {
	var err error
	stdout, stderr := captureStdIO(t, func() {
		err = outputJSONResult(map[string]string{"status": "partial"}, true, "failed to do some things")
	})
	require.Error(t, err)
	assert.True(t, isReportedInJSON(err))
	assert.True(t, isReportedInJSON(fmt.Errorf("wrapped: %w", err)), "detected through wrapping")
	assert.Equal(t, 1, exitCodeFor(err))
	assert.Empty(t, stderr)
	assert.JSONEq(t, `{"status":"partial"}`, stdout)

	stdout, _ = captureStdIO(t, func() {
		err = outputJSONResult(map[string]string{"status": "success"}, false, "unused")
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"success"}`, stdout)

	assert.False(t, isReportedInJSON(fmt.Errorf("plain")))
}

type jsonResultBody struct {
	Status  string                   `json:"status"`
	Results []map[string]interface{} `json:"results"`
}

// ptone/scion#2894: when every agent fails, JSON mode also exits non-zero.
// The body is unchanged: it still says "partial".
func TestDeleteAgentsViaHub_JSONAllFailedExitsNonZero(t *testing.T) {
	env := setupAsyncDelete(t, "bad-agent", getReply{body: replyRuntime})
	setJSONOutput(t)
	var err error
	stdout, stderr := captureStdIO(t, func() {
		err = deleteAgentsViaHub(env.hubCtx, []string{"bad-agent"})
	})
	require.Error(t, err)
	assert.True(t, isReportedInJSON(err))
	assert.Empty(t, stderr)
	var out jsonResultBody
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	assert.Equal(t, "partial", out.Status)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "error", out.Results[0]["status"])
	assert.True(t, env.dirExists())
}

// stopAllEnv wraps the async delete stub with a project agent list holding
// one running agent, for stopAllAgentsViaHub.
func stopAllEnv(t *testing.T, agentName string, replies ...getReply) *asyncDeleteEnv {
	t.Helper()
	env := setupAsyncDelete(t, agentName, replies...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+env.hub.projectID+"/agents" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []map[string]interface{}{{"id": "uuid-1", "name": agentName, "phase": "running"}},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
			return
		}
		env.hub.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	env.hubCtx.Client = client
	env.hubCtx.Endpoint = srv.URL
	return env
}

// jsonBodyOf returns stdout from the first '{'. stop --all --rm prints its
// confirmation list on stdout before the JSON body even in JSON mode; that
// predates ptone/scion#2894 and is left as is here.
func jsonBodyOf(stdout string) string {
	if i := strings.Index(stdout, "{"); i >= 0 {
		return stdout[i:]
	}
	return stdout
}

// ptone/scion#2894: scion stop --all --rm --format json exits non-zero when
// an agent fails, with the unchanged JSON body on stdout and nothing else.
func TestStopAllAgentsViaHub_JSONPartialExitsNonZero(t *testing.T) {
	env := stopAllEnv(t, "all-agent", getReply{body: replyInDoubt})
	setStopRm(t)
	setJSONOutput(t)
	var err error
	stdout, stderr := captureStdIO(t, func() { err = stopAllAgentsViaHub(env.hubCtx) })
	stdout = jsonBodyOf(stdout)
	require.Error(t, err)
	assert.True(t, isReportedInJSON(err))
	assert.Empty(t, stderr)
	var out jsonResultBody
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	assert.Equal(t, "partial", out.Status)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "error", out.Results[0]["status"])
	assert.Contains(t, out.Results[0]["error"], "(in_doubt)")
}

// ptone/scion#2894: a fully successful JSON stop still exits 0.
func TestStopAllAgentsViaHub_JSONSuccessExitsZero(t *testing.T) {
	env := stopAllEnv(t, "all-agent", getReply{body: replyDeleting})
	setStopRm(t)
	setJSONOutput(t)
	var err error
	stdout, _ := captureStdIO(t, func() { err = stopAllAgentsViaHub(env.hubCtx) })
	stdout = jsonBodyOf(stdout)
	require.NoError(t, err, "a pending removal is not a failure")
	var out jsonResultBody
	require.NoError(t, json.Unmarshal([]byte(stdout), &out), stdout)
	assert.Equal(t, "success", out.Status)
}
