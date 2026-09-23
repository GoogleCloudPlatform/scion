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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// This file covers Phase 4 (F2b, design doc §3.3, §8 Phase 4, §9 AC-11/AC-12):
// every agent actually dispatched into a group conversation becomes a
// participant (a listing index — §3.2 already made project membership the
// read authority), and a participant-insert failure never fails the send.

// hasParticipant reports whether p contains an active agent participant row.
func hasParticipant(t *testing.T, s store.Store, conversationID, agentID string) bool {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), conversationID)
	require.NoError(t, err)
	for _, p := range parts {
		if p.PrincipalKind == "agent" && p.PrincipalID == agentID && p.LeftAt == nil {
			return true
		}
	}
	return false
}

// TestPhase4_ChatV2_DefaultAgentDispatch_BecomesParticipant covers AC-11's
// default-agent half: a web post with no @mention, dispatched to the
// thread's default agent, records that agent as a participant.
func TestPhase4_ChatV2_DefaultAgentDispatch_BecomesParticipant(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID: tid("phase4-default-agent"), ProjectID: proj.ID, Name: "Helper Bot", Slug: "phase4-helper-bot",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := tid("phase4-topic-default")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "phase4-default-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: agent.ID,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	conv, err := s.GetConversationByExternalRef(ctx, "native", "thread:"+proj.ID+":"+topicID)
	require.NoError(t, err)
	require.False(t, hasParticipant(t, s, conv.ID, agent.ID), "sanity: not a participant before sending")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "please help"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, hasParticipant(t, s, conv.ID, agent.ID),
		"AC-11: the dispatched default agent must become a participant")
}

// TestPhase4_ChatV2_MentionFanOut_BothBecomeParticipants covers AC-11's
// mention half: a web post @-mentioning two agents (primary + fan-out
// secondary) records both as participants.
func TestPhase4_ChatV2_MentionFanOut_BothBecomeParticipants(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()

	primary := &store.Agent{
		ID: tid("phase4-mention-primary"), ProjectID: proj.ID, Name: "Primary", Slug: "phase4-primary",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, primary))
	secondary := &store.Agent{
		ID: tid("phase4-mention-secondary"), ProjectID: proj.ID, Name: "Secondary", Slug: "phase4-secondary",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, secondary))

	topicID := tid("phase4-topic-mentions")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "phase4-mentions-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(),
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	conv, err := s.GetConversationByExternalRef(ctx, "native", "thread:"+proj.ID+":"+topicID)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@phase4-primary @phase4-secondary please look"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, hasParticipant(t, s, conv.ID, primary.ID),
		"AC-11: the primary mention recipient must become a participant")
	require.True(t, hasParticipant(t, s, conv.ID, secondary.ID),
		"AC-11: the fan-out mention recipient must become a participant")
}

// TestPhase4_BrokerInboundRouted_BothRecipients_BecomeParticipants covers
// AC-11's broker-inbound-routed half, reusing the routed test harness that
// already proves alpha (default) + beta (mention) both dispatch.
//
// Surface + ExternalRef (Phase 11's explicit-key path) are required to land
// on a real *group* conversation shared by both recipients: with neither
// set, dispatchRoutedRecipient's Phase 5 fallback derives a per-recipient
// *direct* (DM) conversation keyed on (sender, that one agent) — see
// TestHandleBrokerInboundRouted_ConversationPersisted — which would give
// alpha and beta two different conversations and is deliberately excluded
// from F2b (a mention target is not a participant of a direct conversation,
// invariant D-1).
func TestPhase4_BrokerInboundRouted_BothRecipients_BecomeParticipants(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Surface:      "slack",
		ExternalRef:  "phase4-channel-ref",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello @beta",
			Type:    messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Delivered)
	require.Len(t, resp.Results, 2)

	ctx := context.Background()
	msgs, err := env.store.ListMessages(ctx, store.MessageFilter{AgentID: env.agent1.ID}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "alpha's dispatched message must be persisted")
	conversationID := msgs.Items[0].ConversationID
	require.NotEmpty(t, conversationID)

	conv, err := env.store.GetConversation(ctx, conversationID)
	require.NoError(t, err)
	require.Equal(t, "group", conv.Kind, "sanity: Surface+ExternalRef must resolve a group conversation")

	require.True(t, hasParticipant(t, env.store, conversationID, env.agent1.ID),
		"AC-11: the routed default agent must become a participant")
	require.True(t, hasParticipant(t, env.store, conversationID, env.agent2.ID),
		"AC-11: the routed mention recipient must become a participant")
}

// TestPhase4_AgentMessage_MentionCoRecipient_BecomesParticipant covers
// AC-11's handleAgentMessage half: a mention co-recipient dispatched via
// req.Mentions on a caller-supplied group conversation_id becomes a
// participant (the primary recipient is already covered by the existing
// TestAgentMessage_GroupConv_AutoRegistersParticipant).
func TestPhase4_AgentMessage_MentionCoRecipient_BecomesParticipant(t *testing.T) {
	srv, s, projectID, targetAgent, userID := def49Setup(t)
	ctx := context.Background()
	srv.SetDispatcher(&recordingDispatcher{})

	mentioned := &store.Agent{
		ID:              tid("phase4-agentmsg-mentioned"),
		Name:            "phase4-agentmsg-mentioned",
		Slug:            "phase4-agentmsg-mentioned",
		ProjectID:       projectID,
		RuntimeBrokerID: targetAgent.RuntimeBrokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, mentioned))

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "group:" + projectID + ":phase4-mention-copart",
		ProjectID:   &projectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:dev",
				SenderID:       userID,
				Recipient:      "agent:" + targetAgent.Slug,
				Msg:            "hello @" + mentioned.Slug,
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
			Mentions: []string{mentioned.Slug},
		})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.True(t, hasParticipant(t, s, created.ID, mentioned.ID),
		"AC-11: a dispatched mention co-recipient must become a participant")
}

// TestPhase4_ChatV2_UnresolvedMention_NoParticipant covers the negative
// half of AC-11: a name that never resolves to an agent (so it is never in
// plan.Agents, never dispatched) must not create a participant row for
// anyone but the actually-dispatched agent.
func TestPhase4_ChatV2_UnresolvedMention_NoParticipant(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()

	real := &store.Agent{
		ID: tid("phase4-unresolved-real"), ProjectID: proj.ID, Name: "Real", Slug: "phase4-real",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, real))

	topicID := tid("phase4-topic-unresolved")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "phase4-unresolved-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(),
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	conv, err := s.GetConversationByExternalRef(ctx, "native", "thread:"+proj.ID+":"+topicID)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@phase4-real @totally-unknown-agent please look"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, hasParticipant(t, s, conv.ID, real.ID), "the actually-dispatched agent must be a participant")

	// Review round 2 finding #7: the original assertion here only compared
	// participant IDs against the literal slug "totally-unknown-agent",
	// which is never a valid PrincipalID (participants are always stored
	// by UUID) — so it could never fail. Assert the actual invariant
	// instead: the conversation has exactly one agent participant, real.ID.
	parts, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	var agentParticipants []store.ConversationParticipant
	for _, p := range parts {
		if p.PrincipalKind == "agent" {
			agentParticipants = append(agentParticipants, p)
		}
	}
	require.Len(t, agentParticipants, 1,
		"a mention that never resolved to an agent must not produce a participant row: %+v", agentParticipants)
	require.Equal(t, real.ID, agentParticipants[0].PrincipalID)
}

// TestPhase4_ChatV2_DeniedMention_NoParticipant is review round 1 finding
// #5's replacement for the vacuous unresolved-name test: a *resolvable*
// agent that is named but denied by authorizeAgentMessage (MessageMode
// "none") must not become a participant, even though it is a real agent in
// plan.Agents. This proves the fix (record only after conversation
// resolution's continue AND a successful dispatch) actually gates on
// authorization outcome, not merely on whether a slug resolves to an agent.
func TestPhase4_ChatV2_DeniedMention_NoParticipant(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()
	srv.createProjectMembersGroup(ctx, proj)

	sender := &store.User{
		ID: tid("phase4-denied-sender"), Email: "phase4-denied-sender@example.com",
		DisplayName: "Sender", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, sender))
	msgAuthzAddProjectMember(t, s, sender.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
	// Upstream removed agent.message from the project-member role (see
	// setupRoutedTestEnv); grant it explicitly so the *allowed* mention
	// isn't denied for the wrong reason.
	msgAuthzGrantAgentMessage(t, s, sender.ID, proj.ID)

	allowedAgent := &store.Agent{
		ID: tid("phase4-denied-allowed"), ProjectID: proj.ID, Name: "Allowed", Slug: "phase4-allowed",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, allowedAgent))

	deniedAgent := &store.Agent{
		ID: tid("phase4-denied-target"), ProjectID: proj.ID, Name: "Denied", Slug: "phase4-denied",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeNone,
	}
	require.NoError(t, s.CreateAgent(ctx, deniedAgent))

	topicID := tid("phase4-topic-denied")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "phase4-denied-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(),
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	conv, err := s.GetConversationByExternalRef(ctx, "native", "thread:"+proj.ID+":"+topicID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, sender, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@phase4-allowed @phase4-denied please look"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	require.True(t, hasParticipant(t, s, conv.ID, allowedAgent.ID),
		"the authorized mention recipient must become a participant")
	require.False(t, hasParticipant(t, s, conv.ID, deniedAgent.ID),
		"AC-11: a resolvable but authorization-denied mention must not become a participant")
}

// failingParticipantStore wraps a real store.Store and makes both
// AddParticipant and EnsureParticipant always fail, to exercise AC-12: a
// participant-row insert failure must never fail or alter the send response.
type failingParticipantStore struct {
	store.Store
}

func (s *failingParticipantStore) AddParticipant(_ context.Context, _ *store.ConversationParticipant) error {
	return errors.New("injected AddParticipant failure")
}

func (s *failingParticipantStore) EnsureParticipant(_ context.Context, _ *store.ConversationParticipant) error {
	return errors.New("injected EnsureParticipant failure")
}

// TestPhase4_ParticipantInsertFailure_DoesNotFailSend is AC-12: injecting a
// participant-row insert failure must not fail or alter the send response.
func TestPhase4_ParticipantInsertFailure_DoesNotFailSend(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID: tid("phase4-failing-agent"), ProjectID: proj.ID, Name: "Helper Bot", Slug: "phase4-failing-bot",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := tid("phase4-topic-failing")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "phase4-failing-thread",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: agent.ID,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	srv.store = &failingParticipantStore{Store: s}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "please help despite the failure"})
	require.Equal(t, http.StatusCreated, rec.Code,
		"AC-12: a participant-insert failure must not fail the send; body: %s", rec.Body.String())

	var resp chatMessageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "please help despite the failure", resp.Content)
}
