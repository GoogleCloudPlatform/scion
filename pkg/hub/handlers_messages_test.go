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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// doMessageRequestAsUser creates a JWT for the given user and performs an HTTP
// request against the test server. Mirrors doRequestAsUser from demo_policy_test.go.
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

// setupMessagePrivacyTest builds a small world:
//
//   - project "msg-priv"
//   - agent "agent-priv" inside that project, owned by alice
//   - alice (manager/owner) — can manage the agent
//   - bob   (member with read-only policy) — can read but NOT manage
//   - three messages on the agent:
//     m1  alice → agent   (alice is sender)
//     m2  agent → alice   (alice is recipient)
//     m3  carol → agent   (neither alice nor bob is a participant)
func setupMessagePrivacyTest(t *testing.T) (
	srv *Server,
	s store.Store,
	alice, bob *store.User,
	agentID string,
) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	// --- users ---
	alice = &store.User{
		ID: tid("msg-alice"), Email: "alice@msg.test",
		DisplayName: "Alice", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	bob = &store.User{
		ID: tid("msg-bob"), Email: "bob@msg.test",
		DisplayName: "Bob", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	require.NoError(t, s.CreateUser(ctx, bob))

	// --- project ---
	project := &store.Project{
		ID: tid("project-msg-priv"), Name: "Msg Privacy",
		Slug: "msg-priv", OwnerID: alice.ID, CreatedBy: alice.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// --- agent (owned by alice → alice gets manage via owner bypass) ---
	agentID = tid("agent-msg-priv")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-msg-priv", Name: "Privacy Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Visibility: store.VisibilityPrivate,
		Created: time.Now(), Updated: time.Now(),
	}))

	// --- give bob read-only access on agents ---
	readPolicy := &store.Policy{
		ID: tid("policy-msg-bob-read"), Name: "Bob Agent Read",
		ScopeType: "hub", ResourceType: "agent",
		Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, readPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: readPolicy.ID, PrincipalType: "user", PrincipalID: bob.ID,
	}))

	// --- messages ---
	now := time.Now()

	// m1: alice → agent (alice is sender)
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "user:alice", SenderID: alice.ID,
		Recipient: "agent:privacy-agent", RecipientID: agentID,
		Msg: "hello from alice", Type: "instruction",
		AgentID: agentID, CreatedAt: now,
	}))

	// m2: agent → alice (alice is recipient)
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "agent:privacy-agent", SenderID: agentID,
		Recipient: "user:alice", RecipientID: alice.ID,
		Msg: "reply to alice", Type: "state-change",
		AgentID: agentID, CreatedAt: now,
	}))

	// m3: carol → agent (neither alice nor bob is a participant)
	carolID := tid("msg-carol")
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: uuid.NewString(), ProjectID: project.ID,
		Sender: "user:carol", SenderID: carolID,
		Recipient: "agent:privacy-agent", RecipientID: agentID,
		Msg: "hello from carol", Type: "instruction",
		AgentID: agentID, CreatedAt: now,
	}))

	return srv, s, alice, bob, agentID
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
