//go:build !no_sqlite

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

package hub

// Tests for the p2a-r2 review round-2 fixes (design agent-reincarnate §3.7,
// Amendment A25.2): F1 (deferred ⇒ persisted, on every R3 path), F2 (chat v2
// secondary mention fan-out gated), F3 (deferred mentions get a reachable
// DM conversation), F4 (group[] aggregate counts deferred truthfully). O-a,
// O-b and O-c are covered in reincarnation_gate_r1_test.go (scheduler
// ordering), handlers_agent_reincarnate_test.go (preamble golden test) and
// pkg/messaging/envelope_compat_test.go respectively. All tests use the
// real SQLite store.
//
// The reviewer's own repro tests (reviews/p2a-r2-repro_test.go.txt) are
// folded in below as assertion-based tests rather than t.Logf probes.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// F1: deferred ⇒ persisted, on every R3 path
// ---------------------------------------------------------------------------

// TestHandleGroupMessage_F1_PersistFailureOnMigratingRecipientIsFailedNotDeferred
// is the group[] half of F1: a migrating recipient whose row fails to
// persist must be reported "failed", never "deferred" — "deferred" promises
// the message is saved for catch-up, which would be a lie here.
func TestHandleGroupMessage_F1_PersistFailureOnMigratingRecipientIsFailedNotDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("f1-group-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("f1-group-project"), Slug: "f1-group-project", Name: "f1-group-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	anchor := &store.Agent{ID: tid("f1-group-anchor"), Slug: "f1-group-anchor", Name: "f1-group-anchor", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, anchor))
	target := &store.Agent{
		ID: tid("f1-group-target"), Slug: "f1-group-target", Name: "f1-group-target",
		ProjectID: project.ID, Phase: string(state.PhaseProvisioning), RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStateProvisioning)
	// group[] syntax requires a multi-recipient set (matching
	// TestDEF19_GroupRecipient_FullHandlerPath's two-recipient form); the
	// peer's own result is not asserted, since the failing store fails its
	// persist too — this test only cares about the migrating recipient's.
	peer := &store.Agent{ID: tid("f1-group-peer"), Slug: "f1-group-peer", Name: "f1-group-peer", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, peer))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)
	// Swap in a failing store AFTER setup, so CreateProject/CreateAgent above
	// succeeded on the real store, but the group[] handler's own
	// CreateMessage call fails.
	srv.store = &createMessageFailStore{Store: s}

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "user:test", SenderID: tid("f1-group-sender"),
			Recipient: "group[agent:f1-group-target,agent:f1-group-peer]",
			Msg:       "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+anchor.ID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp GroupMessageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Results, 2)
	var targetResult *GroupMessageRecipientResult
	for i := range resp.Results {
		if resp.Results[i].Recipient == "agent:f1-group-target" {
			targetResult = &resp.Results[i]
		}
	}
	require.NotNil(t, targetResult)
	assert.Equal(t, "failed", targetResult.Status,
		"an unpersisted migrating recipient must be reported failed, not deferred")
	assert.Contains(t, targetResult.Error, "reincarnating")
	for _, d := range dispatcher.getMessages() {
		assert.NotEqual(t, "f1-group-target", d.agentSlug, "the migrating recipient must never be dispatched to")
	}
}

// TestHandleBrokerInbound_F1_PersistFailureOnMigratingRecipientIs5xx is the
// legacy broker-inbound half of F1: dispatch was already skipped for a
// migrating recipient, so a persist failure means the message is neither
// saved nor dispatched — this must be a hard error, not the pre-existing
// "non-fatal" 200 (which assumed dispatch had already succeeded).
func TestHandleBrokerInbound_F1_PersistFailureOnMigratingRecipientIs5xx(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("f1-inbound-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("f1-inbound-project"), Slug: "f1-inbound-project", Name: "f1-inbound-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	target := &store.Agent{
		ID: tid("f1-inbound-target"), Slug: "f1-inbound-target", Name: "f1-inbound-target",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStatePending)

	senderEmail := "f1-inbound-sender@test.com"
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("f1-inbound-sender"), Email: senderEmail, DisplayName: "Sender",
		Role: "admin", Status: store.UserStatusActive,
	}))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)
	srv.store = &createMessageFailStore{Store: s}

	reqBody := inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + target.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Sender: "user:" + senderEmail, Recipient: "agent:" + target.Slug,
			Msg: "hello from discord", Type: messages.TypeInstruction,
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(broker.ID)))
	rec := httptest.NewRecorder()

	srv.handleBrokerInbound(rec, req)

	require.NotEqual(t, http.StatusOK, rec.Code,
		"an unpersisted, undispatched message to a migrating recipient must not be reported as delivered; body: %s", rec.Body.String())
	assert.GreaterOrEqual(t, rec.Code, 500)
	assert.Empty(t, dispatcher.getMessages())
}

// ---------------------------------------------------------------------------
// F2: chat v2 secondary (mentioned) agent fan-out is gated
// ---------------------------------------------------------------------------

// TestSendAgentRouted_F2_MigratingSecondaryDeferred is the reviewer's
// TestRev2a2Repro_ChatV2SecondaryMigratingAgent, folded in as an
// assertion-based test: a mentioned (secondary) agent that is mid-`scion
// reincarnate` must not be dispatched to, and its row must be deferred.
func TestSendAgentRouted_F2_MigratingSecondaryDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	broker := &store.RuntimeBroker{ID: tid("f2-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("f2-p"), Slug: "f2-p", Name: "f2-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("f2-primary"), Slug: "f2-primary", Name: "f2-primary", ProjectID: project.ID, Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	second := &store.Agent{ID: tid("f2-second"), Slug: "f2-second", Name: "f2-second", ProjectID: project.ID, Phase: string(state.PhaseProvisioning), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, second))
	setReincarnationState(t, s, second, store.ReincarnationStateProvisioning)

	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "f2@test.com", "Owner", "member", "cli")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "f2@test.com", DisplayName: "Owner"}))
	ensureHubMembership(ctx, s, userID)
	srv.createProjectMembersGroup(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, userID))
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), owner))
	rr := httptest.NewRecorder()
	mentionResults := []messages.MentionResult{{Slug: "f2-second", Status: "delivered"}}
	srv.sendAgentRouted(rr, req, "topic:"+project.ID, project.ID, owner,
		"hello @f2-second", "Owner", []*store.Agent{primary, second}, []string{"f2-second"}, mentionResults, nil, time.Now(), "", nil)

	for _, d := range dispatcher.getMessages() {
		assert.NotEqual(t, "f2-second", d.agentSlug, "a migrating secondary must not be dispatched to")
	}
	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: second.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, store.MessageDispatchDeferred, rows.Items[0].DispatchState)
	assert.Equal(t, "deferred", mentionResults[0].Status,
		"the mentionResults slice passed in must be updated in place to reflect the deferral")
}

// ---------------------------------------------------------------------------
// F3: a deferred mention gets a reachable DM conversation
// ---------------------------------------------------------------------------

// TestProcessMentions_F3_DeferredMentionGetsDMConversation is the reviewer's
// TestRev2a2Repro_DeferredMentionHasNoConversation, folded in as an
// assertion-based test: a deferred mention row must carry a conversation_id
// (the sender <-> mentioned-agent DM), or the preamble's "saved to your
// conversations" claim is false for it.
func TestProcessMentions_F3_DeferredMentionGetsDMConversation(t *testing.T) {
	srv, s, primary, mentioned, mctx := rev2MentionSetup(t)
	orig := messages.NewInstruction("user:tester", "agent:"+primary.Slug, "hey @"+mentioned.Slug)
	orig.SenderID = tid("f3-user")

	res := srv.processMentions(mctx, []string{mentioned.Slug}, primary, orig, "", "")
	require.Len(t, res, 1)
	assert.Equal(t, "deferred", res[0].Status)

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: mentioned.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.NotEmpty(t, rows.Items[0].ConversationID,
		"a deferred mention must have a conversation_id so catch-up can reach it")
}

// TestProcessMentions_F3_NonDeferredMentionStillHasNoConversation confirms
// F3's fix is scoped to the deferred case only (A25.2: "non-deferred mention
// rows are unchanged"), so a normal, non-migrating mention keeps Phase 9b's
// existing no-conversation behavior.
func TestProcessMentions_F3_NonDeferredMentionStillHasNoConversation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("f3n-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("f3n-p"), Slug: "f3n-p", Name: "f3n-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("f3n-primary"), Slug: "f3n-primary", Name: "f3n-primary", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{ID: tid("f3n-target"), Slug: "f3n-target", Name: "f3n-target", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, mentioned))
	srv.SetDispatcher(&brokerMockDispatcher{})
	admin := NewAuthenticatedUser(tid("f3n-admin"), "f3n-admin@test.com", "Admin", "admin", "cli")
	mctx := contextWithIdentity(ctx, admin)

	orig := messages.NewInstruction("user:tester", "agent:f3n-primary", "hey @f3n-target")
	orig.SenderID = tid("f3n-user")
	res := srv.processMentions(mctx, []string{"f3n-target"}, primary, orig, "", "")
	require.Len(t, res, 1)
	assert.Equal(t, "delivered", res[0].Status)

	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: mentioned.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Empty(t, rows.Items[0].ConversationID)
}

// TestProcessMentions_F1_PersistFailureOnDeferredMentionIsErrorNotDeferred
// is the reviewer's TestRev2a2Repro_DeferredMentionPersistFailureStillDeferred,
// folded in as an assertion-based test.
func TestProcessMentions_F1_PersistFailureOnDeferredMentionIsErrorNotDeferred(t *testing.T) {
	srv, s, primary, _, mctx := rev2MentionSetup(t)
	srv.store = &createMessageFailStore{Store: s}
	orig := messages.NewInstruction("user:tester", "agent:"+primary.Slug, "hey @target")
	orig.SenderID = tid("f1m-user")

	res := srv.processMentions(mctx, []string{"rvm-target"}, primary, orig, "", "")
	require.Len(t, res, 1)
	assert.Equal(t, "error", res[0].Status,
		"an unpersisted deferred mention must be reported as error, never deferred")
}

// rev2MentionSetup mirrors the reviewer's rev2a2MentionSetup helper: a
// primary agent, a migrating mentioned agent, and an admin identity in
// context (pierces per-mention authorization so these tests focus on the
// migration gate, not message-mode setup).
func rev2MentionSetup(t *testing.T) (*Server, store.Store, *store.Agent, *store.Agent, context.Context) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("rvm-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("rvm-p"), Slug: "rvm-p", Name: "rvm-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("rvm-primary"), Slug: "rvm-primary", Name: "rvm-primary", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{ID: tid("rvm-target"), Slug: "rvm-target", Name: "rvm-target", ProjectID: project.ID, Phase: string(state.PhaseStarting), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, mentioned))
	setReincarnationState(t, s, mentioned, store.ReincarnationStateStarting)
	srv.SetDispatcher(&brokerMockDispatcher{})
	admin := NewAuthenticatedUser(tid("rvm-admin"), "rvm-admin@test.com", "Admin", "admin", "cli")
	return srv, s, primary, mentioned, contextWithIdentity(ctx, admin)
}

// ---------------------------------------------------------------------------
// F4: group[] aggregate counts deferred truthfully
// ---------------------------------------------------------------------------

// TestHandleGroupMessage_F4_AggregateCountsDeferredSeparatelyFromFailed
// covers all four group[] recipient statuses in one request and asserts the
// aggregate Delivered/Failed/Deferred counts, plus the log field, are
// truthful — a deferred recipient must not inflate Failed.
func TestHandleGroupMessage_F4_AggregateCountsDeferredSeparatelyFromFailed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("f4-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("f4-project"), Slug: "f4-project", Name: "f4-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	anchor := &store.Agent{ID: tid("f4-anchor"), Slug: "f4-anchor", Name: "f4-anchor", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, anchor))

	running := &store.Agent{ID: tid("f4-running"), Slug: "f4-running", Name: "f4-running", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, running))
	migrating := &store.Agent{ID: tid("f4-migrating"), Slug: "f4-migrating", Name: "f4-migrating", ProjectID: project.ID, Phase: string(state.PhaseStarting), RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, migrating))
	setReincarnationState(t, s, migrating, store.ReincarnationStateStarting)

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "user:test", SenderID: tid("f4-sender"),
			Recipient: "group[agent:f4-running,agent:f4-migrating,agent:f4-missing]",
			Msg:       "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+anchor.ID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp GroupMessageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Results, 3)

	assert.Equal(t, 1, resp.Delivered, "only the running agent is delivered")
	assert.Equal(t, 1, resp.Failed, "only the not-found agent counts as failed")
	assert.Equal(t, 1, resp.Deferred, "the migrating agent counts as deferred, not failed")
}
