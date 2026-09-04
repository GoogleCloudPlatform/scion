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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// postOutboundWithRef sends an outbound message with a conversation_ref.
func postOutboundWithRef(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:       "user:" + recipientEmail,
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// postOutboundWithRefAndConv sends a request with BOTH conversation_ref and
// conversation_id, which must be rejected.
func postOutboundWithRefAndConv(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg, convRef, convID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:       "user:" + recipientEmail,
		Msg:             msg,
		ConversationRef: convRef,
		ConversationID:  convID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// ---------------------------------------------------------------------------
// DEF-142 P3 step 1: mutual exclusion — both ref and id is a 400.
// ---------------------------------------------------------------------------

func TestDEF142_P3_MutualExclusion_BothRefAndID(t *testing.T) {
	srv, _, project, agent, user := def138Setup(t)

	rr := postOutboundWithRefAndConv(t, srv, project.ID, agent.ID, user.Email,
		"should-be-rejected", "#some-thread", "00000000-0000-0000-0000-000000000001")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"setting both conversation_ref and conversation_id must be rejected")
	assert.Contains(t, rr.Body.String(), "mutually exclusive")
}

// ---------------------------------------------------------------------------
// DEF-142 P3 step 3: ConversationRef resolves a conv:<uuid> reference
// through the handler. The resolved ID flows through the DEF-138 auth
// block (step 4), and asserted=true (step 5).
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_ConvUUID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation the agent's project owns.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d142-conv-uuid",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d142-conv-uuid",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Agent must be a participant for Resolve's post-resolution auth check
	// on the conv:<uuid> path (which checks participant membership for
	// group conversations via project containment — actually group is exempt
	// from participant check, but conv:<uuid> still requires project match).

	// Send with conversation_ref = conv:<uuid>
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello via conv ref", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"conv:<uuid> reference should resolve and authorize successfully")
}

// ---------------------------------------------------------------------------
// DEF-142 P3: ConversationRef with a #thread reference resolves and
// authorizes through the existing DEF-138 block.
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_ThreadRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation with a display name.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d142-thread-ref",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d142-thread-ref",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Send with conversation_ref = #d142-thread-ref
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello via thread ref", "#d142-thread-ref")
	require.Equal(t, http.StatusOK, rr.Code,
		"#thread reference should resolve and authorize successfully")
}

// ---------------------------------------------------------------------------
// DEF-142 P3: ConversationRef with a not-found reference → 400.
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_NotFound(t *testing.T) {
	srv, _, project, agent, user := def138Setup(t)

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello nowhere", "#nonexistent-thread-xyz")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"#nonexistent reference should return 400")
	// AC-3: collapsed response — no reason-specific text.
	assert.Contains(t, rr.Body.String(), "could not be resolved")
}

// ---------------------------------------------------------------------------
// DEF-142 P3: ConversationRef with an ambiguous thread → 400 with
// candidates listed.
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_Ambiguous(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// DEF-140 fork path: two group conversations, same display name,
	// different surfaces.
	for _, surface := range []string{"native", "discord"} {
		conv := &store.Conversation{
			Kind:        "group",
			Surface:     surface,
			ExternalRef: "thread:" + project.ID + ":d142-ambig-" + surface,
			ProjectID:   &project.ID,
			DriftState:  "active",
			DisplayName: "d142-ambig-thread",
		}
		_, err := s.UpsertConversationByExternalRef(ctx, conv)
		require.NoError(t, err)
	}

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello ambig", "#d142-ambig-thread")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"ambiguous #thread must be rejected")
	assert.Contains(t, rr.Body.String(), "ambiguous")
}

// ---------------------------------------------------------------------------
// DEF-142 P3: ConversationRef with invalid format → 400.
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_InvalidFormat(t *testing.T) {
	srv, _, project, agent, user := def138Setup(t)

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello bad ref", "not-a-valid-ref")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "resolution failed")
}

// ---------------------------------------------------------------------------
// DEF-142 P3 step 5 + DEF-141 integration: ConversationRef sets
// asserted=true → explicit_routes increments, not derived_routes.
// Uses broker setup for counter observation.
// ---------------------------------------------------------------------------

func TestDEF142_P3_ConversationRef_SetsAsserted(t *testing.T) {
	srv, s, project, agent, user := def141BrokerSetup(t)
	ctx := context.Background()

	// Create a group conversation the agent's project owns.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d142-asserted-test",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d142-asserted-test",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Snapshot counters before.
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()
	derivedBefore := messaging.DivergenceMetrics.DerivedRoutes()

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello asserted via ref", "#d142-asserted-test")
	require.Equal(t, http.StatusOK, rr.Code)

	// Give async broker delivery time to complete.
	time.Sleep(200 * time.Millisecond)

	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	derivedAfter := messaging.DivergenceMetrics.DerivedRoutes()

	require.Greater(t, explicitAfter, explicitBefore,
		"DEF-142 P3: conversation_ref must set asserted=true so explicit_routes increments")
	require.Equal(t, derivedBefore, derivedAfter,
		"DEF-142 P3: conversation_ref must NOT increment derived_routes")
}

// ---------------------------------------------------------------------------
// ELEVATED CONSTRAINT (DEF-142 review): ResolveContext.ProjectID comes from
// the authenticated agent (agent.ProjectID), not from the request body.
//
// Disclosure concern: Resolve errors (including ambiguity) carry conversation
// UUIDs and surfaces. If rctx.ProjectID came from the body, those errors
// would be an enumeration oracle for conversations in any project the caller
// names. rctx.ProjectID is the containment boundary for what P1's ambiguity
// error can disclose.
// ---------------------------------------------------------------------------

func TestDEF142_G1_ResolveContext_ProjectFromAuth_NotBody(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a SECOND project with a group conversation that should be
	// invisible to the agent.
	foreignProject := &store.Project{
		ID:   tid("d142-foreign-project"),
		Name: "d142-foreign-project",
		Slug: "d142-foreign-project",
	}
	require.NoError(t, s.CreateProject(ctx, foreignProject))

	foreignConv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + foreignProject.ID + ":foreign-secret",
		ProjectID:   &foreignProject.ID,
		DriftState:  "active",
		DisplayName: "foreign-secret",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, foreignConv)
	require.NoError(t, err)

	// The agent belongs to def138-project, NOT to d142-foreign-project.
	// rctx.ProjectID comes from agent.ProjectID (the authenticated agent's
	// project), so #foreign-secret is invisible.
	rr := postOutboundWithRef(t, srv, agent.ProjectID, agent.ID, user.Email,
		"probing foreign project", "#foreign-secret")

	// The thread does not exist in the agent's project → collapsed error.
	// The response must NOT leak the foreign project's conversation details,
	// the reason, or distinguish this from not-found (AC-3).
	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "could not be resolved",
		"foreign project threads must get the same generic error")
	assert.NotContains(t, rr.Body.String(), foreignProject.ID,
		"foreign project ID must never appear in the error response")
	assert.NotContains(t, rr.Body.String(), "foreign-secret",
		"foreign conversation details must not appear in the error response")
}

// ---------------------------------------------------------------------------
// DEF-142 AC-3: "not-found" and "not-a-participant" produce BYTE-IDENTICAL
// response bodies. A caller must not be able to distinguish "does not exist"
// from "exists but I'm not allowed".
// ---------------------------------------------------------------------------

func TestDEF142_AC3_NotFound_vs_NotParticipant_ByteIdentical(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a direct DM between two OTHER principals. The sending agent
	// (def138-agent) is NOT a participant.
	otherAgentID := tid("d142-ac3-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:         otherAgentID,
		Name:       "d142-ac3-other",
		Slug:       "d142-ac3-other",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}))
	otherUserID := tid("d142-ac3-other-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          otherUserID,
		Email:       "d142-other@example.com",
		DisplayName: "Other User",
	}))

	dmKey, err := messages.DMConversationKey("agent", otherAgentID, "user", otherUserID)
	require.NoError(t, err)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
		// ProjectID intentionally nil — DMs are global.
	})
	require.NoError(t, err)

	// Case 1: conv:<uuid> that does not exist.
	nonexistentUUID := "00000000-dead-beef-0000-000000000001"
	rr1 := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"probing nonexistent", "conv:"+nonexistentUUID)
	require.Equal(t, http.StatusBadRequest, rr1.Code)

	// Case 2: conv:<uuid> naming a DIRECT conversation the sender is not
	// party to. Resolve finds it, checkPostResolutionAuth returns
	// "not-a-participant", handler collapses to the same generic error.
	rr2 := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"probing someone elses DM", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusBadRequest, rr2.Code)

	// AC-3: the two bodies must be BYTE-IDENTICAL. "Both fail" does not
	// test the property — the bodies must carry no distinguishing text.
	body1 := rr1.Body.String()
	body2 := rr2.Body.String()
	require.Equal(t, body1, body2,
		"AC-3: not-found and not-a-participant responses must be byte-identical.\n"+
			"  not-found body:         %s\n"+
			"  not-a-participant body:  %s",
		body1, body2)

	// Sanity: the collapsed message is present, not an empty body.
	assert.Contains(t, body1, "could not be resolved")
}

// ---------------------------------------------------------------------------
// DEF-142 AC-3 allowlist: disclosableResolutionReason must be the single
// artefact every ResolutionError reason passes through. Unknown reasons
// — including any future reason — collapse by default.
// ---------------------------------------------------------------------------

func TestDEF142_AC3_DisclosableResolutionReason(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"ambiguous", true},
		{"no-shared-project", true},
		{"not-found", false},
		{"not-a-participant", false},
		{"boundary-violation", false},
		{"some-future-reason", false},
		{"", false},
	}
	for _, tt := range tests {
		name := tt.reason
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			got := disclosableResolutionReason(tt.reason)
			require.Equal(t, tt.want, got,
				"disclosableResolutionReason(%q) = %v, want %v",
				tt.reason, got, tt.want)
		})
	}
}

// ---------------------------------------------------------------------------
// DEF-142 AC-3 end-to-end: a known-collapsed reason ("not-found") produces
// the generic "could not be resolved" body through the actual handler.
// ---------------------------------------------------------------------------

func TestDEF142_AC3_KnownReason_CollapsedEndToEnd(t *testing.T) {
	srv, _, project, agent, user := def138Setup(t)

	// Post a request that produces a known-collapsed reason (not-found).
	nonexistentUUID := "00000000-dead-beef-0000-000000000099"
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"baseline", "conv:"+nonexistentUUID)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "could not be resolved",
		"collapsed body must contain the generic message")
	assert.NotContains(t, rr.Body.String(), "not found",
		"collapsed body must NOT contain the reason-specific text")
}

// ---------------------------------------------------------------------------
// DEF-142 AC-6: A reference that resolves to a NEWLY CREATED conversation
// (@agent resolve-or-create) must still pass through the DEF-138
// authorization block. Resolve exempts Created==true from its own
// post-resolution check, so the DEF-138 block is the only gate.
// ---------------------------------------------------------------------------

func TestDEF142_AC6_ResolveOrCreate_FlowsThroughDEF138Auth(t *testing.T) {
	srv, s, project, agent, user := def141BrokerSetup(t)
	ctx := context.Background()

	// Create a second agent in the same project that the sending agent can
	// DM via @slug.
	targetAgent := &store.Agent{
		ID:         tid("d142-ac6-target"),
		Name:       "d142-ac6-target",
		Slug:       "d142-ac6-target",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	// Snapshot counters. The DEF-138 block sets asserted=true, which the
	// broker routes through the explicit path.
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()

	// Send via @agent-slug — Resolve creates a new DM (Created==true),
	// skips its own post-resolution auth, promotes to req.ConversationID,
	// and the DEF-138 block authorizes + sets asserted=true.
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello via agent ref", "@"+targetAgent.Slug)
	require.Equal(t, http.StatusOK, rr.Code,
		"@agent-slug reference should resolve-or-create and authorize")

	// Give async broker delivery time to complete.
	time.Sleep(200 * time.Millisecond)

	// explicit_routes must increment — proving the message went through
	// the DEF-138 authorization block with asserted=true.
	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Greater(t, explicitAfter, explicitBefore,
		"AC-6: @agent resolve-or-create must flow through DEF-138 auth "+
			"(explicit_routes should increment)")
}
