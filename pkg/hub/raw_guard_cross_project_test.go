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

// ---------------------------------------------------------------------------
// A rejected cross-project agent-sender raw request must create zero
// conversation rows.
//
// The cross-project raw rejection lives in two places: the HTTP-layer check
// in handlers_agent_messaging.go, and a second check in
// agent_dm_operation.go step 4b inside ExecuteAgentDM. The existing
// cross-project raw tests in agent_dm_raw_cross_project_test.go don't catch
// a regression that removes the HTTP-layer check, because foreignAttachSetup
// pre-creates the conversation, so "no change" is indistinguishable from "no
// creation". This file uses a fresh pair with no pre-seeded conversation so
// conversation creation is directly observable.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshCrossProjectPair creates two projects and one running agent in each,
// with cross-project messaging enabled, a dispatcher wired, and — unlike
// foreignAttachSetup — no conversation pre-created between them.
func freshCrossProjectPair(t *testing.T) (srv *Server, s store.Store, agentA, agentB *store.Agent, dispatcher *recordingDispatcher) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("fcp-owner"),
		Email:   "fcp-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	projectA := &store.Project{
		ID:        tid("fcp-project-a"),
		Name:      "fcp-project-a",
		Slug:      "fcp-project-a",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("fcp-project-b"),
		Name: "fcp-project-b",
		Slug: "fcp-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("fcp-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fcp-broker",
		Slug:   "fcp-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA = &store.Agent{
		ID:              tid("fcp-agent-a"),
		Name:            "fcp-agent-a",
		Slug:            "fcp-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB = &store.Agent{
		ID:              tid("fcp-agent-b"),
		Name:            "fcp-agent-b",
		Slug:            "fcp-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	enableCPM(t, srv, s)

	return srv, s, agentA, agentB, dispatcher
}

// TestHandleAgentMessage_CrossProjectRaw_ZeroConversationRows uses a fresh
// cross-project agent pair with no pre-seeded conversation, sends a raw
// agent-sender DM, and asserts that ListConversations is unchanged (0
// before, 0 after) — not just that messages/dispatch are zero.
func TestHandleAgentMessage_CrossProjectRaw_ZeroConversationRows(t *testing.T) {
	srv, s, agentA, agentB, dispatcher := freshCrossProjectPair(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	ctx := context.Background()

	convCountBefore := countStoreConversations(t, s, ctx)
	require.Equal(t, 0, convCountBefore, "precondition: no conversation exists before the send")
	subsBefore, err := s.GetNotificationSubscriptions(ctx, agentB.ID)
	require.NoError(t, err)

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + agentA.Slug,
		SenderID:    agentA.ID,
		Recipient:   "agent:" + agentB.Slug,
		RecipientID: agentB.ID,
		Msg:         "RAWPROBE-CROSS-FRESH",
		Raw:         true,
	}
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+agentB.ProjectID+"/agents/"+agentB.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentA.ID},
		ProjectID: agentA.ProjectID,
		Ancestry:  agentA.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agentB.ID)

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialCrossProjectRawUnsupported), errResp.Error.Details["reason"])

	// Use the shared helper (conversations, subscriptions, mention rows and
	// published events), not just dispatch/messages.
	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, agentB, convCountBefore, len(subsBefore))
}

// TestHandleAgentMessage_CrossProjectRaw_ZeroConversationRows_SecondCall
// sends the same rejected request twice to prove the guard is idempotent
// and never creates a conversation lazily on a retry either.
func TestHandleAgentMessage_CrossProjectRaw_ZeroConversationRows_SecondCall(t *testing.T) {
	srv, s, agentA, agentB, dispatcher := freshCrossProjectPair(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	ctx := context.Background()

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, agentB.ID)
	require.NoError(t, err)

	send := func() *httptest.ResponseRecorder {
		sm := &messages.StructuredMessage{
			Version:     messages.Version,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Type:        messages.TypeInstruction,
			Sender:      "agent:" + agentA.Slug,
			SenderID:    agentA.ID,
			Recipient:   "agent:" + agentB.Slug,
			RecipientID: agentB.ID,
			Msg:         "RAWPROBE-CROSS-FRESH-RETRY",
			Raw:         true,
		}
		reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+agentB.ProjectID+"/agents/"+agentB.ID+"/message",
			bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agentA.ID},
			ProjectID: agentA.ProjectID,
			Ancestry:  agentA.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, req, agentB.ID)
		return rr
	}

	rr1 := send()
	require.Equal(t, http.StatusUnprocessableEntity, rr1.Code)
	rr2 := send()
	require.Equal(t, http.StatusUnprocessableEntity, rr2.Code)

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, agentB, convCountBefore, len(subsBefore))
}

// nilAgentRecordStore returns (nil, nil) for GetAgent when queried with a
// specific agent ID — some store implementations signal "not found" this
// way instead of returning an error — delegating every other call, including
// GetAgent for any other ID, to the embedded store.
//
// handleAgentMessage calls GetAgent for the sender's own ID twice before the
// cross-project raw check under test here runs: once earlier, to resolve the
// sender's slug for structured_message.sender (pre-existing code, unrelated
// to this guard). Returning nil starting only on the second call targets the
// cross-project check specifically, without also tripping that earlier,
// unrelated call site.
type nilAgentRecordStore struct {
	store.Store
	nilForID  string
	callCount int
}

func (s *nilAgentRecordStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.nilForID {
		s.callCount++
		if s.callCount > 1 {
			return nil, nil
		}
	}
	return s.Store.GetAgent(ctx, id)
}

// TestHandleAgentMessage_RawGuard_CrossProjectCheck_NilSenderAgentRecord
// proves the cross-project raw check in handlers_agent_messaging.go does not
// panic when GetAgent returns a nil sender record with a nil error: it is
// treated the same as store.ErrNotFound (404) instead, before any side
// effect (GoogleCloudPlatform/scion#2125).
func TestHandleAgentMessage_RawGuard_CrossProjectCheck_NilSenderAgentRecord(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	srv.store = &nilAgentRecordStore{Store: s, nilForID: sender.ID}
	ctx := context.Background()

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, target.ID)
	require.NoError(t, err)

	sm := baseRawStructuredMessage(sender, target, "RAWGUARD-nil-sender-record")

	var rr *httptest.ResponseRecorder
	assert.NotPanics(t, func() {
		rr = sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})
	})
	require.NotNil(t, rr, "handler must return a response instead of panicking")
	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeNotFound, errResp.Error.Code)

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, target, convCountBefore, len(subsBefore))
}
