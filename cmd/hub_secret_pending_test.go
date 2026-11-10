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
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingMockHub serves the pending secret endpoints and records requests.
type pendingMockHub struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (h *pendingMockHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pending-secrets":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"pendingSecrets": []map[string]interface{}{{
				"id": "pend-1", "key": "TOOL_AUTH", "type": "file", "target": "~/.tool/creds.json", "scope": "user",
				"allowProgeny": true, "valueLen": 12, "agentId": "agent-1", "agentSlug": "worker", "projectId": "proj-1",
				"projectName": "Alpha", "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z",
				"expiresAt": "2026-01-08T00:00:00Z", "replacesExisting": true, "state": "pending",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pending-secrets/pend-1/approve":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "pend-1", "key": "TOOL_AUTH", "scope": "user", "state": "approved"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pending-secrets/pend-1/reject":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "pend-1", "key": "TOOL_AUTH", "scope": "user", "state": "rejected"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"code": "not_found", "message": "Pending secret not found"}})
		}
	}))
}

func setupPendingSecretTest(t *testing.T) *pendingMockHub {
	t.Helper()
	orig := saveSecretTestState()
	origFormat := outputFormat
	origJSON, origNoProgeny := pendingSecretListJSON, pendingSecretApproveNoProgeny
	t.Cleanup(func() {
		orig.restore()
		outputFormat = origFormat
		pendingSecretListJSON, pendingSecretApproveNoProgeny = origJSON, origNoProgeny
	})
	hub := &pendingMockHub{}
	server := hub.server(t)
	t.Cleanup(server.Close)
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	projectPath = setupSecretProject(t, tmpHome, server.URL)
	outputFormat = ""
	pendingSecretListJSON = false
	pendingSecretApproveNoProgeny = false
	return hub
}

func TestHubSecretPendingCmd_Subcommands(t *testing.T) {
	names := map[string]bool{}
	for _, sub := range hubSecretPendingCmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"list", "approve", "reject"} {
		assert.True(t, names[want], "pending has %q", want)
	}
	assert.Equal(t, hubSecretCmd, hubSecretPendingCmd.Parent())
}

func TestHubSecretPendingCmd_NotAvailableInAgentMode(t *testing.T) {
	for _, path := range []string{"hub.secret.pending", "hub.secret.pending.list", "hub.secret.pending.approve", "hub.secret.pending.reject"} {
		assert.False(t, agentAllowed[path], "%s is for the owner only, not agents", path)
	}
}

func TestRunSecretPendingList(t *testing.T) {
	hub := setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretPendingList(hubSecretPendingListCmd, nil))
	})
	assert.Contains(t, out, "pend-1  TOOL_AUTH")
	assert.Contains(t, out, "agent worker (agent-1) in project Alpha (proj-1)")
	assert.Contains(t, out, "and to agents they create")
	assert.Contains(t, out, "Replaces:")
	assert.Contains(t, hub.requests, "GET /api/v1/pending-secrets")

	pendingSecretListJSON = true
	out = captureStdout(t, func() {
		require.NoError(t, runSecretPendingList(hubSecretPendingListCmd, nil))
	})
	var got struct {
		PendingSecrets []map[string]interface{} `json:"pendingSecrets"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.PendingSecrets, 1)
	assert.Equal(t, "TOOL_AUTH", got.PendingSecrets[0]["key"])
}

func TestRunSecretPendingApprove(t *testing.T) {
	hub := setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretPendingApprove(hubSecretPendingApproveCmd, []string{"pend-1"}))
	})
	assert.Equal(t, "Approved secret TOOL_AUTH (scope: user)\n", out)
	assert.NotContains(t, hub.bodies[len(hub.bodies)-1], "allowProgeny")

	pendingSecretApproveNoProgeny = true
	_ = captureStdout(t, func() {
		require.NoError(t, runSecretPendingApprove(hubSecretPendingApproveCmd, []string{"pend-1"}))
	})
	assert.JSONEq(t, `{"allowProgeny":false}`, hub.bodies[len(hub.bodies)-1])

	err := runSecretPendingApprove(hubSecretPendingApproveCmd, []string{"missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to approve pending secret")
}

func TestRunSecretPendingReject(t *testing.T) {
	hub := setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretPendingReject(hubSecretPendingRejectCmd, []string{"pend-1"}))
	})
	assert.Equal(t, "Rejected secret TOOL_AUTH\n", out)
	found := false
	for _, r := range hub.requests {
		if strings.HasSuffix(r, "/pend-1/reject") {
			found = true
		}
	}
	assert.True(t, found)
}
