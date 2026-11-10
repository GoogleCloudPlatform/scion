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
	mu           sync.Mutex
	requests     []string
	bodies       []string
	forbidMerged bool // answer the merged list with 403
	plainLists   int  // approved-only list requests
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
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pending-secrets/pend-1/reveal":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "pend-1", "key": "TOOL_AUTH", "type": "file",
				"target": "~/.tool/creds.json", "value": "dGVzdC12YWx1ZS1yZXZlYWw=", "encoding": "base64"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets" && r.URL.Query().Get("includePending") == "true":
			if h.forbidMerged {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"code": "forbidden", "message": "no"}})
				return
			}
			approved := map[string]interface{}{"id": "sec-1", "key": "TOOL_AUTH", "type": "file", "scope": "user", "version": 2,
				"allowProgeny": true, "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"secrets": []interface{}{approved}, "scope": "user", "scopeId": "u1",
				"items": []interface{}{
					map[string]interface{}{"state": "approved", "key": "TOOL_AUTH", "secret": approved},
					map[string]interface{}{"state": "pending", "key": "TOOL_AUTH", "pending": map[string]interface{}{
						"id": "pend-1", "key": "TOOL_AUTH", "type": "file", "target": "~/.tool/creds.json", "scope": "user",
						"allowProgeny": true, "valueLen": 12, "agentId": "agent-1", "agentSlug": "worker", "projectId": "proj-1",
						"projectName": "Alpha", "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z",
						"expiresAt": "2026-01-08T00:00:00Z", "replacesExisting": true, "state": "pending",
						"overridesProjectSecrets": []interface{}{map[string]string{"projectId": "proj-2", "projectName": "Beta"}},
					}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets":
			h.plainLists++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"secrets": []interface{}{}, "scope": "user"})
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
	origJSON, origNoProgeny, origShowJSON := pendingSecretListJSON, pendingSecretApproveNoProgeny, pendingSecretShowJSON
	t.Cleanup(func() {
		orig.restore()
		outputFormat = origFormat
		pendingSecretListJSON, pendingSecretApproveNoProgeny, pendingSecretShowJSON = origJSON, origNoProgeny, origShowJSON
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
	pendingSecretShowJSON = false
	secretOutputJSON = false
	secretScope = ""
	return hub
}

func TestHubSecretPendingCmd_Subcommands(t *testing.T) {
	names := map[string]bool{}
	for _, sub := range hubSecretPendingCmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"list", "show", "approve", "reject", "reveal"} {
		assert.True(t, names[want], "pending has %q", want)
	}
	assert.Equal(t, hubSecretCmd, hubSecretPendingCmd.Parent())
}

func TestHubSecretPendingCmd_NotAvailableInAgentMode(t *testing.T) {
	for _, path := range []string{"hub.secret.pending", "hub.secret.pending.list", "hub.secret.pending.approve",
		"hub.secret.pending.reject", "hub.secret.pending.show", "hub.secret.pending.reveal"} {
		assert.False(t, agentAllowed[path], "%s is for the owner only, not agents", path)
		assert.True(t, windowsAllowed[path], "%s calls the Hub API only, like its siblings", path)
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

func TestRunSecretPendingShow(t *testing.T) {
	hub := setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretPendingShow(hubSecretPendingShowCmd, []string{"pend-1"}))
	})
	assert.Contains(t, out, "pend-1  TOOL_AUTH")
	assert.Contains(t, out, "Length:     12 bytes")
	assert.Contains(t, out, "agent worker (agent-1) in project Alpha (proj-1)")
	assert.Contains(t, out, "Overrides:  project secret TOOL_AUTH in Beta")
	assert.Contains(t, out, "Replaces:")
	assert.NotContains(t, out, "test-value")
	assert.Contains(t, hub.requests, "GET /api/v1/secrets")

	pendingSecretShowJSON = true
	out = captureStdout(t, func() {
		require.NoError(t, runSecretPendingShow(hubSecretPendingShowCmd, []string{"pend-1"}))
	})
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, "pend-1", got["id"])
	assert.Len(t, got["overridesProjectSecrets"], 1)

	err := runSecretPendingShow(hubSecretPendingShowCmd, []string{"missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRunSecretPendingReveal(t *testing.T) {
	hub := setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretPendingReveal(hubSecretPendingRevealCmd, []string{"pend-1"}))
	})
	assert.Equal(t, "test-value-reveal", out, "the decoded value, with no newline added")
	assert.Contains(t, hub.requests, "POST /api/v1/pending-secrets/pend-1/reveal")

	err := runSecretPendingReveal(hubSecretPendingRevealCmd, []string{"missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to reveal pending secret")
}

func TestRunSecretList_UserScopeShowsState(t *testing.T) {
	setupPendingSecretTest(t)
	out := captureStdout(t, func() {
		require.NoError(t, runSecretList(hubSecretListCmd, nil))
	})
	assert.Contains(t, out, "STATE")
	lines := strings.Split(out, "\n")
	var rows []string
	for _, l := range lines {
		if strings.HasPrefix(l, "TOOL_AUTH") {
			rows = append(rows, strings.Join(strings.Fields(l)[:2], " "))
		}
	}
	assert.Equal(t, []string{"TOOL_AUTH approved", "TOOL_AUTH pending"}, rows)
	assert.Contains(t, out, "scion hub secret pending list")

	secretOutputJSON = true
	out = captureStdout(t, func() {
		require.NoError(t, runSecretList(hubSecretListCmd, nil))
	})
	var got struct {
		Secrets []map[string]interface{} `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Secrets, 2)
	assert.Equal(t, "approved", got.Secrets[0]["state"])
	assert.Equal(t, "pending", got.Secrets[1]["state"])
	assert.Equal(t, "pend-1", got.Secrets[1]["pendingId"])
}

func TestRunSecretList_UserScopeFallsBackWhenMergedListRefused(t *testing.T) {
	hub := setupPendingSecretTest(t)
	hub.forbidMerged = true
	out := captureStdout(t, func() {
		require.NoError(t, runSecretList(hubSecretListCmd, nil))
	})
	assert.Contains(t, out, "No secrets found (scope: user)")
	assert.Equal(t, 1, hub.plainLists)
}

func TestPrintPendingSecretHint(t *testing.T) {
	hub := setupPendingSecretTest(t)
	t.Setenv("SCION_CLI_MODE", "human")
	t.Setenv("SCION_HOST_UID", "")
	client, ctx, cancel, err := pendingSecretClient()
	require.NoError(t, err)
	defer cancel()

	var buf strings.Builder
	printPendingSecretHint(ctx, &buf, client)
	assert.Equal(t, "1 secret awaits approval (scion hub secret pending list)\n", buf.String())
	assert.Contains(t, hub.requests, "GET /api/v1/pending-secrets")

	// Inside an agent nothing is looked up or printed.
	t.Setenv("SCION_CLI_MODE", "agent")
	before := len(hub.requests)
	buf.Reset()
	printPendingSecretHint(ctx, &buf, client)
	assert.Empty(t, buf.String())
	assert.Len(t, hub.requests, before)
}
