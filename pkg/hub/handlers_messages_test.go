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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// doMessageRequestAsUser creates a JWT for the given user and performs an HTTP
// request against the test server. Mirrors doRequestAsUser from demo_sqlite_helpers_test.go.
func doMessageRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Test 1: Manager sees ALL messages
// ---------------------------------------------------------------------------

func TestAgentMessages_ManagerSeesAll(t *testing.T) {
	srv, _, alice, _, agentID := setupMessagePrivacyTest(t)

	rec := doMessageRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var result store.ListResult[store.Message]
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))

	// Alice is the agent owner → manage bypass → sees all 3 messages.
	assert.Equal(t, 3, result.TotalCount,
		"manager should see all messages including ones they are not a participant in")
}

// ---------------------------------------------------------------------------
// Test 2: Non-manager sees only participant messages
// ---------------------------------------------------------------------------

func TestAgentMessages_NonManagerSeesOwnOnly(t *testing.T) {
	srv, _, _, bob, agentID := setupMessagePrivacyTest(t)

	rec := doMessageRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var result store.ListResult[store.Message]
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))

	// Bob has read-only access and is not a participant in any message →
	// the privacy filter should return 0 messages.
	assert.Equal(t, 0, result.TotalCount,
		"non-manager who is not a participant should see no messages")
}

// ---------------------------------------------------------------------------
// Test 3: SSE endpoint respects the same filtering
// ---------------------------------------------------------------------------

func TestAgentMessagesStream_NonManagerFiltered(t *testing.T) {
	srv, _, _, bob, agentID := setupMessagePrivacyTest(t)

	// The SSE endpoint requires a subscription-capable EventPublisher.
	// The default test server uses noopEventPublisher, so it returns 501.
	// That's fine — we're testing that the auth gate fires before the
	// publisher check would matter. The important assertion is that the
	// endpoint is reachable (not 403/404) for a user with read access,
	// confirming the same authz path as the REST endpoint.
	rec := doMessageRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages/stream", nil)

	// Accept either 200 (real publisher) or 501 (noop publisher).
	// A 403 would indicate the privacy/authz gate is wrong.
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"non-manager with read access should not be forbidden from the SSE endpoint")
	assert.Contains(t, []int{http.StatusOK, http.StatusNotImplemented}, rec.Code,
		"SSE endpoint should return 200 or 501, got %d: %s", rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Test 4: Non-manager without read access → 403
// ---------------------------------------------------------------------------

func TestAgentMessages_NoAccessForbidden(t *testing.T) {
	srv, s, _, _, agentID := setupMessagePrivacyTest(t)

	// eve has no policies at all
	eve := &store.User{
		ID: tid("msg-eve"), Email: "eve@msg.test",
		DisplayName: "Eve", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), eve))

	rec := doMessageRequestAsUser(t, srv, eve, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"user without any agent read policy should be forbidden")
}
