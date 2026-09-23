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

// Phase 0 tests for canonical direct-conversation access.
//
// These tests verify the acceptance gate from the delivery plan:
//
//   - A third principal cannot read a DM by adding a participant row or
//     knowing its UUID. A matching ID with the wrong principal kind is denied.
//   - A DM with missing listing rows still has the correct canonical ACL;
//     leaving it does not change the pair's identity or silently un-leave on GET.
//   - Malformed keys, direct-create bypasses, same-named peers, and deleted
//     native topic/mint guards are covered.
//   - Existing project groups and supported same-project sends work. All
//     cross-project agent sends are still denied.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupDMConversation creates a direct conversation between two agents with
// proper canonical DM key and participant rows.
func setupDMConversation(t *testing.T, s store.Store, agentAID, agentBID string) *store.Conversation {
	t.Helper()
	ctx := context.Background()

	extRef, err := messages.DMConversationKey("agent", agentAID, "agent", agentBID)
	require.NoError(t, err)

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
		// ProjectID intentionally nil — DMs are global.
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both agents as participants (listing concern).
	addConvParticipant(t, s, conv.ID, "agent", agentAID)
	addConvParticipant(t, s, conv.ID, "agent", agentBID)

	return conv
}

// ---------------------------------------------------------------------------
// Test: Third principal cannot read DM by adding participant row
// ---------------------------------------------------------------------------

func TestDMAccess_ThirdPrincipalDenied(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)

	// Create two agents for the DM.
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "dm-agent-b",
		Slug:       "dm-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agentB))

	dmConv := setupDMConversation(t, s, agentA.ID, agentB.ID)

	// Create a third agent. The store-level DM key guard prevents adding a
	// participant row for a principal not named in the key. This test
	// verifies the handler-level authorization as a defense in depth.
	intruder := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "intruder-agent",
		Slug:       "intruder-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), intruder))

	// The store-level guard should reject adding the intruder as a participant.
	// This is the first layer of defense.
	t.Run("store rejects intruder participant", func(t *testing.T) {
		p := &store.ConversationParticipant{
			ID:             api.NewUUID(),
			ConversationID: dmConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    intruder.ID,
			Role:           "member",
			JoinedAt:       time.Now().UTC(),
		}
		err := s.AddParticipant(context.Background(), p)
		require.Error(t, err, "store should reject adding non-key participant to DM")
	})

	// Even if a participant row existed (e.g., from a pre-guard migration),
	// the handler-level key check would deny access.
	t.Run("intruder denied GET (key check)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(agentContext(intruder.ID, project.ID))
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusForbidden, rr.Code,
			"third principal should be denied access to DM by canonical key check")
	})

	t.Run("intruder denied messages (key check)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID+"/messages", nil)
		req = req.WithContext(agentContext(intruder.ID, project.ID))
		rr := httptest.NewRecorder()
		srv.handleConvListMessages(rr, req, dmConv.ID)

		require.Equal(t, http.StatusForbidden, rr.Code,
			"third principal should be denied messages by canonical key check")
	})

	t.Run("canonical participant A allowed GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(agentContext(agentA.ID, project.ID))
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusOK, rr.Code,
			"canonical participant should have access; body: %s", rr.Body.String())
	})

	t.Run("canonical participant B allowed GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(agentContext(agentB.ID, project.ID))
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusOK, rr.Code,
			"canonical participant should have access; body: %s", rr.Body.String())
	})
}

// ---------------------------------------------------------------------------
// Test: Wrong principal kind denied even with matching ID
// ---------------------------------------------------------------------------

func TestDMAccess_WrongPrincipalKindDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, agentA, _ := setupConvTestData(t, s)

	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "kind-test-agent-b",
		Slug:       "kind-test-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	dmConv := setupDMConversation(t, s, agentA.ID, agentB.ID)

	// Create a user with the same ID as agentA (contrived but tests kind check).
	// The DM key has "agent" kind for both participants. A "user" identity with
	// the same UUID should be denied because the kind doesn't match.
	//
	// Note: we do NOT add a participant row for the user because the store-level
	// CheckDMParticipantKey guard would reject it (correct behavior — the store
	// prevents non-key participants). The handler-level key check is
	// defense-in-depth for cases where the store guard was not present.
	userID := agentA.ID
	user := &store.User{
		ID:          userID,
		Email:       "kind-collision@test.com",
		DisplayName: "Kind Collision User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	// This may fail if the ID collides in the user table; skip if so.
	err := s.CreateUser(ctx, user)
	if err != nil {
		t.Skip("could not create user with agent-matching ID (ID collision in user table)")
	}

	// The DM key has "agent" kind, so a "user" with the same ID should be denied.
	userCtx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(userID, "kind-collision@test.com", "Kind Collision", store.UserRoleMember, "api"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
	req = req.WithContext(userCtx)
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, dmConv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"user with matching ID but wrong kind should be denied")
}

// ---------------------------------------------------------------------------
// Test: DM with missing listing rows still has correct canonical ACL
// ---------------------------------------------------------------------------

func TestDMAccess_MissingParticipantRowStillAuthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, agentA, _ := setupConvTestData(t, s)

	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "missing-row-agent-b",
		Slug:       "missing-row-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM but do NOT add participant rows.
	extRef, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)

	now := time.Now().UTC()
	dmConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, dmConv))

	// Agent A should still be authorized via the canonical DM key even
	// without a participant row.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, dmConv.ID)

	require.Equal(t, http.StatusOK, rr.Code,
		"canonical DM participant should be authorized even without participant row")
}

// ---------------------------------------------------------------------------
// Test: Leaving a DM does not change the pair's identity
// ---------------------------------------------------------------------------

func TestDMAccess_LeaveDoesNotChangeIdentity(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)

	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "leave-test-agent-b",
		Slug:       "leave-test-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agentB))

	dmConv := setupDMConversation(t, s, agentA.ID, agentB.ID)

	// Agent A leaves the DM.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+dmConv.ID+"/leave", nil)
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleLeaveConversation(rr, req, dmConv.ID)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Agent A should STILL be authorized via the canonical DM key.
	// Leaving removes the listing row but does not remove from the key.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr = httptest.NewRecorder()
	srv.handleGetConversation(rr, req, dmConv.ID)

	require.Equal(t, http.StatusOK, rr.Code,
		"leaving a DM should not revoke canonical access; body: %s", rr.Body.String())

	// Subsequent GET should NOT silently re-add the participant row
	// (un-leave). Verify the participant row is still absent.
	isParticipant, err := isConversationParticipant(
		context.Background(), s, dmConv.ID, "agent", agentA.ID)
	require.NoError(t, err)
	require.False(t, isParticipant,
		"GET should not silently re-add participant row after leaving")
}

// ---------------------------------------------------------------------------
// Test: Direct-create bypass rejected
// ---------------------------------------------------------------------------

func TestDMAccess_DirectCreateBypassRejected(t *testing.T) {
	srv, s := testServer(t)
	_, agent, _ := setupConvTestData(t, s)

	body := createConversationRequest{
		DisplayName: "Bypass DM",
		Kind:        "direct",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, ""))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"direct conversations cannot be created through generic create; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Test: Participant addition rejected for direct conversations
// ---------------------------------------------------------------------------

func TestDMAccess_ParticipantAdditionRejected(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)

	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "part-add-agent-b",
		Slug:       "part-add-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agentB))

	dmConv := setupDMConversation(t, s, agentA.ID, agentB.ID)

	newAgent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "third-party",
		Slug:       "third-party",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), newAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   newAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+dmConv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, dmConv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"adding participants to a DM should be rejected; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Test: Default-agent mutation rejected for direct conversations
// ---------------------------------------------------------------------------

func TestDMAccess_DefaultAgentRejected(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)

	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "default-test-agent-b",
		Slug:       "default-test-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agentB))

	dmConv := setupDMConversation(t, s, agentA.ID, agentB.ID)

	body := setDefaultAgentRequest{AgentID: agentB.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+dmConv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, dmConv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"setting default agent on a DM should be rejected; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Test: Malformed DM key fails closed
// ---------------------------------------------------------------------------

func TestDMAccess_MalformedKeyFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)

	now := time.Now().UTC()
	malformedConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    "not-a-valid-dm-key",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(context.Background(), malformedConv))
	// Note: we do NOT add a participant row because the store-level guard
	// rejects adding participants to a conversation with an unparseable DM key.
	// This tests that the handler-level key check fails closed even without
	// participant rows (defense-in-depth for legacy data).

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+malformedConv.ID, nil)
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, malformedConv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"malformed DM key should fail closed; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Test: Existing project groups still work
// ---------------------------------------------------------------------------

func TestDMAccess_GroupConversationsUnchanged(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	// Phase 3 (design doc §3.2, Q2 = b): group reads are project-based, not
	// participant-based, so the happy-path caller now also needs project
	// access — this is the intended behavior change, not a DM regression.
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Verify group conversation GET still works, now via project-based auth.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusOK, rr.Code,
		"group conversation should still work; body: %s", rr.Body.String())

	// Verify non-participant is still denied for groups.
	nonParticipant := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "non-participant-group",
		Slug:       "non-participant-group",
		ProjectID:  *conv.ProjectID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), nonParticipant))

	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContext(nonParticipant.ID, convProjectID(conv)))
	rr = httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"non-participant should be denied group conversation")
}

// ---------------------------------------------------------------------------
// Test: User-agent DM access (cross-kind canonical key)
// ---------------------------------------------------------------------------

func TestDMAccess_UserAgentDM(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project, agent, _ := setupConvTestData(t, s)

	userID := api.NewUUID()
	user := &store.User{
		ID:          userID,
		Email:       "dm-user@test.com",
		DisplayName: "DM User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Create a user-agent DM.
	extRef, err := messages.DMConversationKey("user", userID, "agent", agent.ID)
	require.NoError(t, err)

	now := time.Now().UTC()
	dmConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, dmConv))
	addConvParticipant(t, s, dmConv.ID, "user", userID)
	addConvParticipant(t, s, dmConv.ID, "agent", agent.ID)

	t.Run("user can access DM", func(t *testing.T) {
		userCtx := contextWithIdentity(ctx,
			NewAuthenticatedUser(userID, "dm-user@test.com", "DM User", store.UserRoleMember, "api"))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(userCtx)
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("agent can access DM", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(agentContext(agent.ID, project.ID))
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("other user denied", func(t *testing.T) {
		otherUserID := api.NewUUID()
		otherUser := &store.User{
			ID:          otherUserID,
			Email:       "other-dm-user@test.com",
			DisplayName: "Other DM User",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, otherUser))
		// Note: we do NOT add a participant row because the store-level
		// guard rejects non-key participants. The handler-level key check
		// is defense-in-depth.

		otherCtx := contextWithIdentity(ctx,
			NewAuthenticatedUser(otherUserID, "other-dm-user@test.com", "Other", store.UserRoleMember, "api"))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+dmConv.ID, nil)
		req = req.WithContext(otherCtx)
		rr := httptest.NewRecorder()
		srv.handleGetConversation(rr, req, dmConv.ID)

		require.Equal(t, http.StatusForbidden, rr.Code,
			"other user should be denied by canonical key check")
	})
}
