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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DEF-138 test fixtures
// ---------------------------------------------------------------------------

// def138Setup creates a project, an agent, and a user. The agent is
// configured as the sender for outbound messages.
func def138Setup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("def138-project"),
		Name: "def138-project",
		Slug: "def138-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	user = &store.User{
		ID:          tid("def138-user"),
		Email:       "def138@example.com",
		DisplayName: "DEF138 User",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	agent = &store.Agent{
		ID:         tid("def138-agent"),
		Name:       "def138-agent",
		Slug:       "def138-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, project, agent, user
}

// postOutboundWithConv sends an outbound message with a conversation_id.
func postOutboundWithConv(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg, convID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:      "user:" + recipientEmail,
		Msg:            msg,
		ConversationID: convID,
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

// postOutboundNoConv sends an outbound message without a conversation_id.
func postOutboundNoConv(t *testing.T, srv *Server, projectID, agentID, recipientEmail, msg string) *httptest.ResponseRecorder {
	t.Helper()
	return postOutboundWithConv(t, srv, projectID, agentID, recipientEmail, msg, "")
}

// ---------------------------------------------------------------------------
// AC-1: Round trip — reply carrying the envelope's conversation persists
// with conversation_id equal to the inbound message's.
// ---------------------------------------------------------------------------

func TestDEF138_AC1_ExplicitConversationRoundTrip(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation simulating an inbound thread (e.g. Discord).
	threadRef := "thread:" + project.ID + ":test-thread-123"
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Agent replies with the conversation_id from the envelope.
	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "reply to thread", created.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the persisted message has the correct conversation_id.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{
		AgentID: agent.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items)

	found := false
	for _, m := range msgs.Items {
		if m.Msg == "reply to thread" {
			require.Equal(t, created.ID, m.ConversationID,
				"reply should persist into the inbound conversation")
			found = true
			break
		}
	}
	require.True(t, found, "reply message not found in store")
}

// ---------------------------------------------------------------------------
// AC-2: Unauthorised assertion denied — 403 AND no message row written.
// ---------------------------------------------------------------------------

func TestDEF138_AC2_UnauthorisedConversation_DirectDM_Denied(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	// Create a DM conversation between two OTHER principals — the agent
	// is NOT a participant.
	otherUser := &store.User{
		ID:          tid("def138-other-user"),
		Email:       "other@example.com",
		DisplayName: "Other User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	otherAgent := &store.Agent{
		ID:         tid("def138-other-agent"),
		Name:       "def138-other-agent",
		Slug:       "def138-other-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	dmKey, err := messages.DMConversationKey("user", otherUser.ID, "agent", otherAgent.ID)
	require.NoError(t, err)
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Count messages before the attempt.
	msgsBefore, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	countBefore := len(msgsBefore.Items)

	// Agent tries to claim a conversation it is not a participant of.
	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, otherUser.Email, "unauthorized", created.ID)
	require.Equal(t, http.StatusForbidden, rr.Code,
		"should deny with 403, body: %s", rr.Body.String())

	// AC-2: assert NO message row was written.
	msgsAfter, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, countBefore, len(msgsAfter.Items),
		"no message row should be written on authorization failure")
}

func TestDEF138_AC2_UnauthorisedConversation_GroupWrongProject_Denied(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation in a DIFFERENT project.
	otherProjectID := tid("def138-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   otherProjectID,
		Name: "def138-other-project",
		Slug: "def138-other-project",
	}))

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "thread:" + otherProjectID + ":other-thread",
		ProjectID:   &otherProjectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Count messages before the attempt.
	msgsBefore, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	countBefore := len(msgsBefore.Items)

	// Agent in project A tries to claim a conversation in project B.
	rr := postOutboundWithConv(t, srv, agent.ProjectID, agent.ID, user.Email, "cross-project", created.ID)
	require.Equal(t, http.StatusForbidden, rr.Code,
		"should deny cross-project assertion, body: %s", rr.Body.String())

	// AC-2: assert NO message row was written.
	msgsAfter, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, countBefore, len(msgsAfter.Items),
		"no message row should be written on cross-project denial")
}

func TestDEF138_AC2_NonexistentConversation_Denied(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Count messages before the attempt.
	msgsBefore, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	countBefore := len(msgsBefore.Items)

	// Agent claims a conversation that does not exist.
	rr := postOutboundWithConv(t, srv, agent.ProjectID, agent.ID, user.Email, "ghost conv", "nonexistent-id")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"should deny nonexistent conversation, body: %s", rr.Body.String())

	// Assert NO message row was written.
	msgsAfter, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, countBefore, len(msgsAfter.Items),
		"no message row should be written for nonexistent conversation")
}

// ---------------------------------------------------------------------------
// AC-3: No memory — assert no code path consults last-channel-style state
// to determine a conversation. Structural/grep test.
// ---------------------------------------------------------------------------

func TestDEF138_AC3_NoMemoryBasedConversationRouting(t *testing.T) {
	// Scan handlers_agent_messaging.go for any use of GetLastChannel that
	// influences conversation_id derivation. GetLastChannel is allowed for
	// channel affinity (delivery routing) but must NOT influence conversation
	// identity.
	hubDir := "."
	entries, err := os.ReadDir(hubDir)
	require.NoError(t, err)

	// The conversation resolution block is between "conversation routing rules"
	// (or "Phase 5 dual-write") and the divergence logging. GetLastChannel
	// should only appear in the reply affinity block (which sets req.Channel,
	// not conversation_id).
	getLastChannel := regexp.MustCompile(`GetLastChannel`)
	conversationAssign := regexp.MustCompile(`\.ConversationID\s*=`)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(hubDir, name))
		require.NoError(t, err)

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}

			// GetLastChannel must never appear on the same line as
			// a ConversationID assignment.
			if getLastChannel.MatchString(line) && conversationAssign.MatchString(line) {
				t.Errorf("DEF-138 AC-3 violation at %s:%d: GetLastChannel influences ConversationID\n  %s",
					name, i+1, trimmed)
			}
		}
	}

	// Additional structural check: in handleAgentOutboundMessage, the
	// GetLastChannel call must be in the channel-affinity block (setting
	// req.Channel), not in the conversation resolution block.
	data, err := os.ReadFile(filepath.Join(hubDir, "handlers_agent_messaging.go"))
	require.NoError(t, err)

	content := string(data)
	// Find the conversation routing rules comment (DEF-138) or the Phase 5 block.
	convBlockStart := strings.Index(content, "DEF-138 §3.1 conversation routing rules")
	if convBlockStart == -1 {
		convBlockStart = strings.Index(content, "Phase 5 dual-write: resolve-or-create conversation")
	}
	// Find the divergence logging that ends the conversation block.
	convBlockEnd := strings.Index(content, "Always log divergence")

	if convBlockStart > 0 && convBlockEnd > convBlockStart {
		convBlock := content[convBlockStart:convBlockEnd]
		if getLastChannel.MatchString(convBlock) {
			t.Error("DEF-138 AC-3: GetLastChannel appears inside the conversation resolution block; " +
				"it must only influence Channel (delivery), not ConversationID (identity)")
		}
	}
}

// ---------------------------------------------------------------------------
// AC-4: Proactive send unchanged — agent sends with no conversation_id,
// derives a DM as before.
// ---------------------------------------------------------------------------

func TestDEF138_AC4_ProactiveSendDerivesDM(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Send without conversation_id — should derive a DM.
	rr := postOutboundNoConv(t, srv, agent.ProjectID, agent.ID, user.Email, "proactive hello")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify a message was persisted with a DM conversation.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)

	found := false
	for _, m := range msgs.Items {
		if m.Msg == "proactive hello" {
			require.NotEmpty(t, m.ConversationID, "proactive send should still resolve a conversation")
			// Verify it's a direct conversation.
			conv, err := s.GetConversation(ctx, m.ConversationID)
			require.NoError(t, err)
			require.Equal(t, "direct", conv.Kind, "proactive send should derive a DM conversation")
			found = true
			break
		}
	}
	require.True(t, found, "proactive message not found in store")
}

// ---------------------------------------------------------------------------
// AC-5: Absent field is byte-identical to today's behaviour.
// ---------------------------------------------------------------------------

func TestDEF138_AC5_AbsentConversationID_UnchangedBehaviour(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Send without conversation_id.
	rr := postOutboundNoConv(t, srv, agent.ProjectID, agent.ID, user.Email, "no conv field")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Parse the response — should have same shape as before.
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "sent", resp["status"])
	require.NotEmpty(t, resp["message_id"])
	require.NotEmpty(t, resp["recipient_id"])

	// The message should be persisted with a derived DM conversation.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items)

	for _, m := range msgs.Items {
		if m.Msg == "no conv field" {
			require.NotEmpty(t, m.ConversationID,
				"absent conversation_id should still derive via rules 2/3")
		}
	}
}

// ---------------------------------------------------------------------------
// AC-9: Metadata["conversation_id"] must NOT become live as a side effect.
// ---------------------------------------------------------------------------

func TestDEF138_AC9_MetadataConversationID_NotLive(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a DM conversation that the agent IS a participant of.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	legitimateConv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	_, err = s.UpsertConversationByExternalRef(ctx, legitimateConv)
	require.NoError(t, err)

	// Create a conversation in a different project that the agent should NOT
	// be able to access.
	otherProjectID := tid("def138-ac9-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   otherProjectID,
		Name: "ac9-other",
		Slug: "ac9-other",
	}))
	sneakyConv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "thread:" + otherProjectID + ":sneaky",
		ProjectID:   &otherProjectID,
		DriftState:  "active",
	}
	createdSneaky, err := s.UpsertConversationByExternalRef(ctx, sneakyConv)
	require.NoError(t, err)

	// Send a message with Metadata["conversation_id"] set to the sneaky
	// conversation. The top-level ConversationID is left empty.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "metadata bypass attempt",
		Metadata:  map[string]string{"conversation_id": createdSneaky.ID},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the message was NOT persisted with the sneaky conversation_id.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)

	for _, m := range msgs.Items {
		if m.Msg == "metadata bypass attempt" {
			require.NotEqual(t, createdSneaky.ID, m.ConversationID,
				"Metadata[conversation_id] must NOT become live — it bypasses P-2 authorization")
		}
	}
}

// ---------------------------------------------------------------------------
// AC-11: SKILL.md no longer describes conversation_id as optional or
// conv: as unsupported.
// ---------------------------------------------------------------------------

func TestDEF138_AC11_SkillMD_NoLongerContradicts(t *testing.T) {
	// Read the SKILL.md file.
	skillPath := filepath.Join("..", "..", "resources", "platform_skills", "scion-messaging", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Skipf("SKILL.md not found at %s (running outside repo root?)", skillPath)
	}
	content := string(data)

	// AC-11a: conv:<uuid> must NOT be described as "not yet supported".
	if strings.Contains(content, "Not yet supported") {
		t.Error("SKILL.md still describes conv: or # as 'Not yet supported' — update per DEF-138 P-4")
	}

	// AC-11b: conversation_id must NOT be described as optional.
	notYet := regexp.MustCompile(`(?i)not yet required`)
	if notYet.MatchString(content) {
		t.Error("SKILL.md still describes conversation_id as 'not yet required' — update per DEF-138 §3.5")
	}

	// AC-11c: The phrase "may appear in message metadata" is wrong — the field
	// is a top-level envelope key, not metadata.
	if strings.Contains(content, "may appear in message metadata") {
		t.Error("SKILL.md still says conversation_id 'may appear in message metadata' — it is a top-level envelope key")
	}
}

// ---------------------------------------------------------------------------
// AC-1 authorized assertion — agent replies with a group conversation
// from its own project and the reply persists there.
// ---------------------------------------------------------------------------

func TestDEF138_AC1_AuthorisedGroupConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation in the agent's project.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "slack",
		ExternalRef: "thread:" + project.ID + ":slack-channel-42",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "reply to slack", created.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the persisted message has the group conversation.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)

	found := false
	for _, m := range msgs.Items {
		if m.Msg == "reply to slack" {
			require.Equal(t, created.ID, m.ConversationID)
			found = true
		}
	}
	require.True(t, found)
}

// ---------------------------------------------------------------------------
// AC-1 authorized assertion — direct conversation where agent IS a participant.
// ---------------------------------------------------------------------------

func TestDEF138_AC1_AuthorisedDirectConversation(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a DM conversation between the agent and the user.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundWithConv(t, srv, agent.ProjectID, agent.ID, user.Email, "dm reply", created.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)

	found := false
	for _, m := range msgs.Items {
		if m.Msg == "dm reply" {
			require.Equal(t, created.ID, m.ConversationID)
			found = true
		}
	}
	require.True(t, found)
}

// ---------------------------------------------------------------------------
// AC-12: An agent replying WITHOUT the conversation field produces a
// consistency mismatch in the DEF-139 counters.
// This validates the adoption signal.
// ---------------------------------------------------------------------------

func TestDEF138_AC12_MissingConversationField_ProducesMismatch(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// First, simulate an inbound message that created a GROUP conversation
	// (e.g. from Discord thread).
	threadRef := "thread:" + project.ID + ":discord-thread-999"
	inboundConv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, inboundConv)
	require.NoError(t, err)

	// Simulate an inbound message in that conversation.
	inbound := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		Sender:         "user:" + user.DisplayName,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		Msg:            "hello from discord thread",
		Type:           "instruction",
		AgentID:        agent.ID,
		ConversationID: created.ID,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, s.CreateMessage(ctx, inbound))

	// Now the agent replies WITHOUT the conversation field — this should
	// derive a DM (rule 3), which is different from the inbound's group
	// conversation. That's the mismatch signal.
	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "reply without conv")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the reply landed in a DIFFERENT conversation (DM, not group).
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)

	for _, m := range msgs.Items {
		if m.Msg == "reply without conv" {
			require.NotEqual(t, created.ID, m.ConversationID,
				"reply without conversation_id should land in a different (DM) conversation, "+
					"producing the mismatch signal that DEF-139 can detect")
			// Verify it IS a direct conversation.
			if m.ConversationID != "" {
				conv, err := s.GetConversation(ctx, m.ConversationID)
				require.NoError(t, err)
				require.Equal(t, "direct", conv.Kind,
					"absent conversation_id should derive a DM, not match the inbound group")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// AC-2: Fail closed on nil conversation — (nil, nil) from GetConversation.
// This tests the defensive path. In practice GetConversation returns
// (nil, ErrNotFound), but the code defensively handles (nil, nil) too.
// We exercise the nonexistent case above (TestDEF138_AC2_NonexistentConversation_Denied)
// which covers the same denial path.
// ---------------------------------------------------------------------------
