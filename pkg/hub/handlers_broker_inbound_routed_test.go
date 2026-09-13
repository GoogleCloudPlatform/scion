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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// routedEnvelope is the parsed DeliveryText envelope for assertion.
// Reuses the existing extractEnvelopeJSON helper from def171.
type routedEnvelope struct {
	Type         string          `json:"type"`
	To           []string        `json:"to,omitempty"`
	From         string          `json:"from"`
	Msg          string          `json:"msg"`
	Conversation *routedConvInfo `json:"conversation,omitempty"`
}

type routedConvInfo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Surface string `json:"surface"`
}

func parseRoutedEnvelope(t *testing.T, deliveryText string) routedEnvelope {
	t.Helper()
	jsonStr := extractEnvelopeJSON(t, deliveryText)
	var env routedEnvelope
	require.NoError(t, json.Unmarshal([]byte(jsonStr), &env),
		"failed to unmarshal envelope JSON: %s", jsonStr)
	return env
}

func sortedStrings(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

// routedTestEnv holds the common test fixtures for routed inbound tests.
type routedTestEnv struct {
	srv          *Server
	store        store.Store
	dispatcher   *recordingDispatcher
	webChatStore WebChatStore
	user         *store.User
	project      *store.Project
	agent1       *store.Agent // "alpha" — running, project mode
	agent2       *store.Agent // "beta" — running, project mode
	agent3       *store.Agent // "gamma" — stopped
}

func setupRoutedTestEnv(t *testing.T) routedTestEnv {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Wire a recording dispatcher so dispatch tests exercise the full path.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable the envelope switch for DeliveryText assertions.
	enableWriteDenySwitch(t, srv)

	// Wire webChatStore for reply affinity assertions.
	wcsDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = wcsDB.Close() })
	wcs := NewWebChatStore(wcsDB, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Create a project owner (separate from the test sender).
	owner := &store.User{
		ID:          tid("owner-routed"),
		Email:       "owner-routed@example.com",
		DisplayName: "Routed Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create a separate sender user (non-owner).
	user := &store.User{
		ID:          tid("user-routed"),
		Email:       "routed@example.com",
		DisplayName: "Routed User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-routed"),
		Slug:      "routed-proj",
		Name:      "Routed Test Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	// Grant explicit agent.message send authority. Upstream removed
	// agent.message from the project-member role; tests must not rely on
	// that implicit grant. The negative tests (InactiveUser, UnknownSender)
	// still verify denial for users without this explicit binding.
	msgAuthzGrantAgentMessage(t, s, user.ID, project.ID)

	agent1 := &store.Agent{
		ID:           tid("agent-alpha"),
		Slug:         "alpha",
		Name:         "Alpha Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:           tid("agent-beta"),
		Slug:         "beta",
		Name:         "Beta Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	agent3 := &store.Agent{
		ID:           tid("agent-gamma"),
		Slug:         "gamma",
		Name:         "Gamma Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseStopped),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent3))

	return routedTestEnv{
		srv:          srv,
		store:        s,
		dispatcher:   dispatcher,
		webChatStore: wcs,
		user:         user,
		project:      project,
		agent1:       agent1,
		agent2:       agent2,
		agent3:       agent3,
	}
}

func (e routedTestEnv) doRoutedRequest(t *testing.T, req routedInboundRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound/routed", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithBrokerIdentity(httpReq.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	e.srv.mux.ServeHTTP(rec, httpReq)
	return rec
}

// --- Lifecycle tests (F-3): exercises dispatch → persist → SSE → affinity ---

func TestHandleBrokerInboundRouted_BasicDelivery(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + env.user.Email,
			Msg:       "hello",
			Type:      messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Delivered)
	assert.Equal(t, "alpha", resp.PrimaryAgent)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "delivered", resp.Results[0].Status)
	assert.Equal(t, "alpha", resp.Results[0].AgentSlug)
	assert.Equal(t, "message", resp.Results[0].Type)
	assert.NotEmpty(t, resp.Results[0].MessageID)

	// Verify dispatch.
	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "exactly one dispatch call expected")
	assert.Equal(t, env.agent1.ID, calls[0].Agent.ID)
	assert.Equal(t, "hello", calls[0].Message)
	assert.False(t, calls[0].Interrupt)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.Equal(t, messages.TypeInstruction, calls[0].StructuredMessage.Type)
	assert.Equal(t, "agent:alpha", calls[0].StructuredMessage.Recipient)

	// Verify persistence.
	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		AgentID: env.agent1.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(msgs.Items), 1, "at least one message must be persisted")
	persisted := msgs.Items[0]
	assert.Equal(t, "hello", persisted.Msg)
	assert.Equal(t, "user:"+env.user.Email, persisted.Sender)
	assert.Equal(t, "agent:alpha", persisted.Recipient)
	assert.Equal(t, env.agent1.ID, persisted.RecipientID)
	assert.Equal(t, env.user.ID, persisted.SenderID)
	assert.Equal(t, resp.Results[0].MessageID, persisted.ID)
	assert.Equal(t, store.MessageDispatchDispatched, persisted.DispatchState)

	// Verify reply affinity recorded.
	ch, err := env.webChatStore.GetLastChannel(context.Background(),
		env.user.ID, env.project.ID, env.agent1.ID)
	require.NoError(t, err)
	assert.Equal(t, "slack", ch, "reply affinity channel must be recorded")
}

func TestHandleBrokerInboundRouted_MentionRouting(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// Message with @beta mention → should route to alpha (default) + beta.
	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
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
	assert.True(t, resp.Delivered)
	assert.Equal(t, "alpha", resp.PrimaryAgent)
	require.Len(t, resp.Results, 2)

	// Primary: alpha (message type).
	assert.Equal(t, "alpha", resp.Results[0].AgentSlug)
	assert.Equal(t, "message", resp.Results[0].Type)
	assert.Equal(t, "delivered", resp.Results[0].Status)

	// Secondary: beta (mention type).
	assert.Equal(t, "beta", resp.Results[1].AgentSlug)
	assert.Equal(t, "mention", resp.Results[1].Type)
	assert.Equal(t, "delivered", resp.Results[1].Status)

	// Verify dispatch calls.
	calls := env.dispatcher.getCalls()
	require.Equal(t, 2, len(calls))
	assert.Equal(t, env.agent1.ID, calls[0].Agent.ID, "first dispatch to alpha")
	assert.Equal(t, env.agent2.ID, calls[1].Agent.ID, "second dispatch to beta")

	// Primary is TypeInstruction, secondary is TypeMention.
	assert.Equal(t, messages.TypeInstruction, calls[0].StructuredMessage.Type)
	assert.Equal(t, messages.TypeMention, calls[1].StructuredMessage.Type)

	// Secondary must have co-addressees metadata indicating the primary.
	require.NotNil(t, calls[1].StructuredMessage.Metadata)
	assert.Equal(t, "agent:alpha", calls[1].StructuredMessage.Metadata["mention_source"],
		"secondary must have mention_source pointing to primary")

	// Verify per-recipient: response ID == persisted ID, conversation ID, DeliveryText.
	expectedTypes := []string{"message", "mention"}
	for i, agentID := range []string{env.agent1.ID, env.agent2.ID} {
		msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
			AgentID: agentID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(msgs.Items), 1,
			"at least one message must be persisted for agent %s", agentID)
		persisted := msgs.Items[0]

		// Response message ID must match persisted row ID.
		assert.Equal(t, resp.Results[i].MessageID, persisted.ID,
			"response message ID must match persisted row for result %d", i)
		assert.NotEmpty(t, persisted.ConversationID,
			"persisted message must have conversation_id for agent %s", agentID)

		// Parse rendered DeliveryText envelope.
		require.NotEmpty(t, calls[i].StructuredMessage.DeliveryText,
			"DeliveryText must be rendered for dispatch %d", i)
		envelope := parseRoutedEnvelope(t, calls[i].StructuredMessage.DeliveryText)

		// Rendered type must match message/mention.
		assert.Equal(t, expectedTypes[i], envelope.Type,
			"rendered envelope type must be %q for dispatch %d", expectedTypes[i], i)

		// Rendered conversation.id must match persisted ConversationID.
		require.NotNil(t, envelope.Conversation,
			"rendered envelope must have conversation for dispatch %d", i)
		assert.Equal(t, persisted.ConversationID, envelope.Conversation.ID,
			"rendered conversation.id must match persisted ConversationID for dispatch %d", i)

		// Rendered "to" must contain both alpha and beta.
		assert.Equal(t,
			sortedStrings([]string{"agent:alpha", "agent:beta"}),
			sortedStrings(envelope.To),
			"rendered to list must contain both agents for dispatch %d", i)
	}

	// Verify reply affinity recorded for both successful recipients.
	ctx := context.Background()
	for _, agentID := range []string{env.agent1.ID, env.agent2.ID} {
		ch, err := env.webChatStore.GetLastChannel(ctx,
			env.user.ID, env.project.ID, agentID)
		require.NoError(t, err)
		assert.Equal(t, "slack", ch,
			"reply affinity channel must be recorded for agent %s", agentID)
	}

	// Verify TouchThread watermark via store read API.
	threads, err := env.webChatStore.GetThreads(ctx,
		env.user.ID, env.project.ID, 10)
	require.NoError(t, err)
	threadAgentIDs := make(map[string]string)
	for _, th := range threads {
		threadAgentIDs[th.AgentID] = th.LastMessageID
	}
	assert.Contains(t, threadAgentIDs, env.agent1.ID,
		"alpha must have a thread watermark")
	assert.Contains(t, threadAgentIDs, env.agent2.ID,
		"beta must have a thread watermark")
	// Watermark message IDs must match the persisted message IDs.
	assert.Equal(t, resp.Results[0].MessageID, threadAgentIDs[env.agent1.ID],
		"alpha thread watermark must reference alpha's message ID")
	assert.Equal(t, resp.Results[1].MessageID, threadAgentIDs[env.agent2.ID],
		"beta thread watermark must reference beta's message ID")
}

func TestHandleBrokerInboundRouted_LeadingMentionOverride(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// Leading @beta overrides default alpha.
	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "@beta hello",
			Type:    messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "beta", resp.PrimaryAgent, "leading mention must override default")
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "delivered", resp.Results[0].Status)

	// Verify dispatch to beta, not alpha.
	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	assert.Equal(t, env.agent2.ID, calls[0].Agent.ID)
}

func TestHandleBrokerInboundRouted_UnresolvedMentionDiagnostic(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "@unknown hello",
			Type:    messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "alpha", resp.PrimaryAgent)
	assert.Contains(t, resp.UnresolvedMentions, "unknown")

	// Verify dispatch to alpha (default fallback).
	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	assert.Equal(t, env.agent1.ID, calls[0].Agent.ID)
}

func TestHandleBrokerInboundRouted_MissingDefault_WithMention(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// No default, but @alpha is mentioned → alpha becomes primary.
	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID: env.project.ID,
		// No DefaultAgent
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "@alpha hello",
			Type:    messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "alpha", resp.PrimaryAgent)

	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	assert.Equal(t, env.agent1.ID, calls[0].Agent.ID)
}

func TestHandleBrokerInboundRouted_StoppedAgentPrimary(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// gamma is stopped → should fail with not_running.
	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "gamma",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusConflict, rec.Code)

	// No dispatch should have occurred.
	calls := env.dispatcher.getCalls()
	assert.Equal(t, 0, len(calls), "stopped agent must not be dispatched to")
}

func TestHandleBrokerInboundRouted_PrimaryFailureStopsFanOut(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// Make dispatcher fail for all calls.
	env.dispatcher.returnErr = fmt.Errorf("simulated dispatch failure")

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello @beta",
			Type:    messages.TypeInstruction,
		},
	})

	// Primary dispatch failed → should return error, not 200.
	assert.NotEqual(t, http.StatusOK, rec.Code)

	// Only one dispatch attempt (the primary); secondary must be not_attempted.
	calls := env.dispatcher.getCalls()
	assert.Equal(t, 1, len(calls), "only primary dispatch should be attempted")

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	// The response should contain results with not_attempted for secondary.
	if errResp.Error.Details != nil {
		if results, ok := errResp.Error.Details["results"]; ok {
			resultsJSON, _ := json.Marshal(results)
			assert.Contains(t, string(resultsJSON), "not_attempted",
				"secondary agent must be not_attempted when primary fails")
		}
	}
}

func TestHandleBrokerInboundRouted_SecondaryFailureReported(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// Make the dispatcher succeed for alpha (first call) but fail for beta (second call).
	callCount := 0
	env.dispatcher.returnErr = nil // reset
	// We need a more targeted approach. Since recordingDispatcher uses a fixed
	// returnErr, let's stop beta so it fails at phase check instead.
	ctx := context.Background()
	env.agent2.Phase = string(state.PhaseStopped)
	require.NoError(t, env.store.UpdateAgent(ctx, env.agent2))

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello @beta",
			Type:    messages.TypeInstruction,
		},
	})
	_ = callCount

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Delivered, "overall delivery succeeds when primary succeeds")
	require.Len(t, resp.Results, 2)
	assert.Equal(t, "delivered", resp.Results[0].Status, "primary must succeed")
	assert.Equal(t, "not_running", resp.Results[1].Status, "stopped secondary reported")

	// Only one dispatch call (alpha); beta never reaches dispatch.
	calls := env.dispatcher.getCalls()
	assert.Equal(t, 1, len(calls))

	// Verify reply affinity: alpha (success) has affinity, beta (failed) does not.
	chAlpha, err := env.webChatStore.GetLastChannel(ctx,
		env.user.ID, env.project.ID, env.agent1.ID)
	require.NoError(t, err)
	assert.Equal(t, "slack", chAlpha, "successful recipient must have affinity")

	chBeta, err := env.webChatStore.GetLastChannel(ctx,
		env.user.ID, env.project.ID, env.agent2.ID)
	require.NoError(t, err)
	assert.Empty(t, chBeta, "failed recipient must NOT have affinity")
}

func TestHandleBrokerInboundRouted_InterruptStripping(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "! urgent message",
			Type:    messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify dispatch with stripped message and interrupt=true.
	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	assert.Equal(t, "urgent message", calls[0].Message)
	assert.True(t, calls[0].Interrupt, "interrupt must be set for ! prefix")
}

func TestHandleBrokerInboundRouted_IncomingMentionMetadataStripped(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
			Metadata: map[string]string{
				"mention_co_addressees": `["evil"]`,
				"group_id":              "injected",
				"mention_source":        "injected:source",
				"mention_position":      "injected:position",
				"safe_key":              "preserved",
			},
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify dispatched message has safe_key but not the stripped fields.
	calls := env.dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	require.NotNil(t, calls[0].StructuredMessage)
	meta := calls[0].StructuredMessage.Metadata
	assert.Equal(t, "preserved", meta["safe_key"])
	_, hasMentionCoAddr := meta["mention_co_addressees"]
	_, hasGroupID := meta["group_id"]
	_, hasMentionSource := meta["mention_source"]
	_, hasMentionPosition := meta["mention_position"]
	assert.False(t, hasMentionCoAddr, "mention_co_addressees must be stripped")
	assert.False(t, hasGroupID, "group_id must be stripped")
	assert.False(t, hasMentionSource, "mention_source must be stripped from primary")
	assert.False(t, hasMentionPosition, "mention_position must be stripped from primary")
}

func TestHandleBrokerInboundRouted_AttachmentsDeepCopied(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:     messages.Version,
			Channel:     "slack",
			Sender:      "user:" + env.user.Email,
			Msg:         "hello @beta with attachments",
			Type:        messages.TypeInstruction,
			Attachments: []string{"file1.txt", "file2.txt"},
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	calls := env.dispatcher.getCalls()
	require.Equal(t, 2, len(calls), "two dispatches: alpha + beta")

	// Both recipients must have the attachments.
	for i, call := range calls {
		require.NotNil(t, call.StructuredMessage)
		assert.Equal(t, []string{"file1.txt", "file2.txt"}, call.StructuredMessage.Attachments,
			"call %d must have attachments", i)
	}
}

func TestHandleBrokerInboundRouted_ConversationPersisted(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "conversation test",
			Type:    messages.TypeInstruction,
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify persisted message has a conversation_id (Phase 5 DM resolution).
	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		AgentID: env.agent1.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(msgs.Items), 1)
	assert.NotEmpty(t, msgs.Items[0].ConversationID,
		"persisted message must have a conversation_id from Phase 5 DM resolution")
}

// --- Validation tests (these don't need dispatcher) ---

func TestHandleBrokerInboundRouted_MissingProjectID(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_MissingMessage(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID: env.project.ID,
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_NonUserSender(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "bot:something",
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_BroadcastRejected(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:     messages.Version,
			Channel:     "slack",
			Sender:      "user:" + env.user.Email,
			Msg:         "hello",
			Type:        messages.TypeInstruction,
			Broadcasted: true,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_DMThreadRejected(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:  messages.Version,
			Channel:  "slack",
			Sender:   "user:" + env.user.Email,
			Msg:      "hello",
			Type:     messages.TypeInstruction,
			ThreadID: "dm:agent:123:user:456",
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_ExternalRefWithoutSurface(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		ExternalRef:  "some-ref",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_ParentRefWithoutExternalRef(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		ParentRef:    "parent-ref",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleBrokerInboundRouted_NoRecipient(t *testing.T) {
	env := setupRoutedTestEnv(t)

	// No default, no mentions → 422
	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID: env.project.ID,
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello with no agent",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, "no_routing_recipient", errResp.Error.Code)
}

func TestHandleBrokerInboundRouted_InactiveUser(t *testing.T) {
	env := setupRoutedTestEnv(t)
	ctx := context.Background()

	// Create inactive user.
	inactiveUser := &store.User{
		ID:      tid("user-inactive-routed"),
		Email:   "inactive-routed@example.com",
		Role:    store.UserRoleMember,
		Status:  "suspended",
		Created: time.Now(),
	}
	require.NoError(t, env.store.CreateUser(ctx, inactiveUser))

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + inactiveUser.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleBrokerInboundRouted_UnknownSender(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:nobody@example.com",
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHandleBrokerInboundRouted_NoBrokerAuth(t *testing.T) {
	env := setupRoutedTestEnv(t)

	body, err := json.Marshal(routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
		},
	})
	require.NoError(t, err)

	// No broker identity in context.
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound/routed", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	env.srv.mux.ServeHTTP(rec, httpReq)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandleBrokerInboundRouted_MethodNotAllowed(t *testing.T) {
	env := setupRoutedTestEnv(t)

	httpReq := httptest.NewRequest(http.MethodGet, "/api/v1/broker/inbound/routed", nil)
	httpReq = httpReq.WithContext(contextWithBrokerIdentity(httpReq.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	env.srv.mux.ServeHTTP(rec, httpReq)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandleBrokerInboundRouted_EmptyBody(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "",
			Type:    messages.TypeInstruction,
		},
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
