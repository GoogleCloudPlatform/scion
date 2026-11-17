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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func (f *choicesFixture) agentDecisionReq(t *testing.T, agent *store.Agent, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	r := httptest.NewRequest(method, path, &buf)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r = r.WithContext(agentCtx(r.Context(), agent))
	rr := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rr, r)
	return rr
}

// storeUserReply inserts a user message in threadID strictly after promptTime.
func (f *choicesFixture) storeUserReply(t *testing.T, u *store.User, threadID, convID, channel, text string) *store.Message {
	t.Helper()
	time.Sleep(2 * time.Millisecond)
	m := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      f.project.ID,
		Sender:         "user:" + u.Email,
		SenderID:       u.ID,
		Recipient:      "agent:" + f.agent.Slug,
		RecipientID:    f.agent.ID,
		Msg:            text,
		Type:           messages.TypeInstruction,
		AgentID:        f.agent.ID,
		Channel:        channel,
		ThreadID:       threadID,
		ConversationID: convID,
		DispatchState:  store.MessageDispatchDispatched,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(context.Background(), m))
	return m
}

func (f *choicesFixture) bobDMReply(t *testing.T, promptMsgID, text string) *store.Message {
	t.Helper()
	prompt, err := f.store.GetMessage(context.Background(), promptMsgID)
	require.NoError(t, err)
	return f.storeUserReply(t, f.bob, prompt.ThreadID, prompt.ConversationID, "web", text)
}

func TestAgentDecisions_ExperimentOffAndAuthz(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendPromptToBob(t, oneDecision, false)
	reply := f.bobDMReply(t, resp.MessageID, "D234: yes")

	// Experiment off -> 404 on all 3 routes.
	setChoicesExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions", nil).Code)
	assert.Equal(t, http.StatusNotFound, f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions/D234", nil).Code)
	assert.Equal(t, http.StatusNotFound, f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: reply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	}).Code)

	setChoicesExperiment(t, f.srv, true)

	// Unauthenticated -> 401 via RouteAgentToken guard.
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/agent/decisions", nil)
	unauth := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauth, unauthReq)
	assert.Equal(t, http.StatusUnauthorized, unauth.Code)

	// User caller (session / user identity) -> 403 agents_only on all 3 routes.
	for _, tc := range []struct {
		method string
		path   string
		body   interface{}
	}{
		{http.MethodGet, "/api/v1/agent/decisions", nil},
		{http.MethodGet, "/api/v1/agent/decisions/D234", nil},
		{http.MethodPost, "/api/v1/agent/decisions/interpreted", map[string]interface{}{
			"source_message_id": reply.ID,
			"answers":           []map[string]string{{"key": "D234", "choice_id": "1"}},
		}},
	} {
		rr := doRequestAsUser(t, f.srv, f.bob, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusForbidden, rr.Code, "%s %s body=%s", tc.method, tc.path, rr.Body.String())
		assert.Equal(t, ErrCodeAgentsOnly, errorCode(t, rr))
	}

	// Visitor identity -> 403 (plain 403 at guarded visitor gate; 403 agents_only at handler).
	vis := NewVisitorIdentity("vis-1", "https://issuer.example", "sub-1", "vis@example.com", "Visitor")
	visRR := requestAsIdentity(t, f.srv, vis, http.MethodGet, "/api/v1/agent/decisions", nil)
	assert.Equal(t, http.StatusForbidden, visRR.Code)
	visHandlerReq := httptest.NewRequest(http.MethodGet, "/api/v1/agent/decisions", nil)
	visHandlerReq = visHandlerReq.WithContext(contextWithIdentity(visHandlerReq.Context(), vis))
	visHandlerRR := httptest.NewRecorder()
	f.srv.handleAgentDecisionRoutes(visHandlerRR, visHandlerReq)
	assert.Equal(t, http.StatusForbidden, visHandlerRR.Code)
	assert.Equal(t, ErrCodeAgentsOnly, errorCode(t, visHandlerRR))

	// Method enforcement.
	assert.Equal(t, http.StatusMethodNotAllowed, f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions", nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, f.agentDecisionReq(t, f.agent, http.MethodDelete, "/api/v1/agent/decisions/D234", nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, f.agentDecisionReq(t, f.agent, http.MethodDelete, "/api/v1/agent/decisions/interpreted", nil).Code)
}

func TestAgentDecisions_ListAndShow(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()

	// Create a second agent in the project to verify agent isolation.
	otherAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "other-bot", Name: "other-bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, Phase: "running",
		Created: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))

	// Post D234 v1 + hub-keyed 1, interpret D234=1, Bob overrides D234=2, then repost D234 v2.
	p1 := f.sendPromptToBob(t, twoDecisions, false)
	reply := f.bobDMReply(t, p1.MessageID, "D234: yes")
	recRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: reply.ID,
		IdempotencyKey:  "ik-list-1",
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, recRR.Code, recRR.Body.String())

	// Bob overrides D234 v1 to "2" ("no") via user set-submit on p1 (1=a + D234=2).
	subRR := f.submitSet(t, f.bob, p1.MessageID, "", p1.Decisions[0].RowID, "2", p1.Decisions[1].RowID, "a")
	require.Equal(t, http.StatusOK, subRR.Code, subRR.Body.String())

	// Repost D234 v2 (--update) + D900 pending.
	f.sendPromptToBob(t, `[{"id":"D234","question":"Ship v2?","choices":["yes","no"]},{"id":"D900","question":"Keep logs?","choices":["yes","no"]}]`, true)

	// List default (pending): D234 v2 and D900.
	listRR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions", nil)
	require.Equal(t, http.StatusOK, listRR.Code, listRR.Body.String())
	var pendingOut AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(listRR.Body.Bytes(), &pendingOut))
	require.Len(t, pendingOut.Items, 2)
	for _, it := range pendingOut.Items {
		assert.Equal(t, store.DecisionStatusPending, it.Status)
		assert.NotEmpty(t, it.ConversationID)
		assert.NotEmpty(t, it.PromptMessageID)
	}

	// List decided: hub-keyed "1" (decided).
	decRR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?state=decided", nil)
	require.Equal(t, http.StatusOK, decRR.Code)
	var decOut AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(decRR.Body.Bytes(), &decOut))
	require.Len(t, decOut.Items, 1)
	assert.Equal(t, "1", decOut.Items[0].Key)
	require.NotNil(t, decOut.Items[0].Answer)
	assert.Equal(t, "Extend", decOut.Items[0].Answer.Label)

	// List all with pagination (limit=2 -> 4 total items across 2 pages: D234 v1 replaced, 1 decided, D234 v2 pending, D900 pending).
	all1RR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?state=all&limit=2", nil)
	require.Equal(t, http.StatusOK, all1RR.Code)
	var all1 AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(all1RR.Body.Bytes(), &all1))
	require.Len(t, all1.Items, 2)
	require.NotEmpty(t, all1.NextCursor)

	all2RR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?state=all&limit=2&cursor="+all1.NextCursor, nil)
	require.Equal(t, http.StatusOK, all2RR.Code)
	var all2 AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(all2RR.Body.Bytes(), &all2))
	require.Len(t, all2.Items, 2)
	assert.Empty(t, all2.NextCursor)

	// Show D234 (case-insensitive "d234"): v1 has BOTH answers (interpreted + direct override), v2 is pending.
	showRR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions/d234", nil)
	require.Equal(t, http.StatusOK, showRR.Code, showRR.Body.String())
	var showOut AgentDecisionShowResponse
	require.NoError(t, json.Unmarshal(showRR.Body.Bytes(), &showOut))
	assert.Equal(t, "D234", showOut.Key)
	require.Len(t, showOut.Versions, 2)
	assert.Equal(t, 1, showOut.Versions[0].Version)
	assert.Equal(t, store.DecisionStatusReplaced, showOut.Versions[0].Status)
	require.NotNil(t, showOut.Versions[0].ReplacedBy)
	assert.Equal(t, 2, showOut.Versions[0].ReplacedBy.Version)
	require.Len(t, showOut.Versions[0].Answers, 2, "show must expose both interpreted and direct answers")
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, showOut.Versions[0].Answers[0].Provenance)
	assert.Equal(t, "yes", showOut.Versions[0].Answers[0].Label)
	assert.Equal(t, store.DecisionProvenanceDirect, showOut.Versions[0].Answers[1].Provenance)
	assert.Equal(t, store.DecisionOutcomeOverride, showOut.Versions[0].Answers[1].Outcome)
	assert.Equal(t, "no", showOut.Versions[0].Answers[1].Label)
	assert.Equal(t, 2, showOut.Versions[1].Version)
	assert.Equal(t, store.DecisionStatusPending, showOut.Versions[1].Status)
	assert.Empty(t, showOut.Versions[1].Answers)

	// Other agent sees empty list and 404 decision_not_found on D234.
	otherListRR := f.agentDecisionReq(t, otherAgent, http.MethodGet, "/api/v1/agent/decisions?state=all", nil)
	require.Equal(t, http.StatusOK, otherListRR.Code)
	var otherList AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(otherListRR.Body.Bytes(), &otherList))
	assert.Empty(t, otherList.Items)

	otherShowRR := f.agentDecisionReq(t, otherAgent, http.MethodGet, "/api/v1/agent/decisions/D234", nil)
	assert.Equal(t, http.StatusNotFound, otherShowRR.Code)
	assert.Equal(t, ErrCodeDecisionNotFound, errorCode(t, otherShowRR))

	// Invalid list query parameters -> 400.
	for _, q := range []string{"state=bogus", "limit=0", "limit=201", "limit=x", "cursor=not-a-cursor"} {
		rr := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?"+q, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
	}
}

func TestAgentDecisions_InterpretedSingleAndMultiPromptPartialSet(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()

	// Single pair: batch_id omitted, 1 choice-response message, 0 agent deliveries.
	pSolo := f.sendPromptToBob(t, `[{"id":"SOLO","question":"Deploy solo?","choices":["yes","no"],"default":"yes"}]`, false)
	replySolo := f.bobDMReply(t, pSolo.MessageID, "SOLO: yes")

	updates, unsubUpdates := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.choice.updated")
	defer unsubUpdates()
	chat, unsubChat := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.dm")
	defer unsubChat()

	beforeDeliveries := len(f.dispatcher.getMessages())
	rrSolo := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: replySolo.ID,
		IdempotencyKey:  "ik-solo",
		Answers:         []InterpretedAnswerInput{{Key: "SOLO", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrSolo.Code, rrSolo.Body.String())
	var outSolo RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rrSolo.Body.Bytes(), &outSolo))
	assert.Empty(t, outSolo.BatchID, "single pair omits batch_id")
	assert.NotEmpty(t, outSolo.ResponseMessageID)
	require.Len(t, outSolo.Decisions, 1)
	require.NotNil(t, outSolo.Decisions[0].Decision)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, outSolo.Decisions[0].Decision.Provenance)
	assert.True(t, outSolo.Decisions[0].Decision.MatchedDefault)
	assert.Equal(t, f.bob.ID, outSolo.Decisions[0].Decision.AnsweredByUserID)
	assert.Equal(t, f.bob.DisplayName, outSolo.Decisions[0].Decision.AnsweredByName)
	assert.Equal(t, f.bob.Email, outSolo.Decisions[0].Decision.AnsweredByEmail)
	assert.Equal(t, f.agent.ID, outSolo.Decisions[0].Decision.InterpretedByAgentID)
	assert.Equal(t, replySolo.ID, outSolo.Decisions[0].Decision.SourceMessageID)
	assert.Equal(t, beforeDeliveries, len(f.dispatcher.getMessages()), "interpreted answers are never delivered back to the agent")

	soloRow, err := f.store.GetDecision(ctx, pSolo.Decisions[0].RowID)
	require.NoError(t, err)
	require.Len(t, soloRow.Answers, 1)
	assert.Equal(t, f.bob.ID, soloRow.Answers[0].AnsweredByUserID)
	assert.Equal(t, f.bob.DisplayName, soloRow.Answers[0].AnsweredByName)
	assert.Equal(t, f.bob.Email, soloRow.Answers[0].AnsweredByEmail)
	assert.Equal(t, f.agent.ID, soloRow.Answers[0].InterpretedByAgentID)
	assert.Equal(t, replySolo.ID, soloRow.Answers[0].SourceMessageID)

	soloMsg, err := f.store.GetMessage(ctx, outSolo.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, "agent:"+f.agent.Slug, soloMsg.Sender)
	assert.Equal(t, store.MessageDispatchDispatched, soloMsg.DispatchState)
	assert.Contains(t, soloMsg.Msg, "recorded")
	assert.Contains(t, soloMsg.Msg, "answer yes to SOLO, interpreted from their reply")
	assert.Len(t, collectEvents(updates), 1)
	_ = collectEvents(chat)

	// Multi-prompt + partial set + reverse request order:
	// Prompt A has 3 decisions (D234, hub-keyed 1, ship-now); Prompt B has 1 decision (EXTRA).
	// Pass EXTRA=2 (later prompt), 1=a (earlier prompt pos 1), D234=1 (earlier prompt pos 0)
	// in reverse order, leaving ship-now pending. Live SSE, stored msg, HTTP response,
	// history hydration, and idempotency replay must all use canonical [D234, 1, EXTRA] order.
	pSet := f.sendSetToBob(t)
	time.Sleep(2 * time.Millisecond)
	pExtra := f.sendPromptToBob(t, `[{"id":"EXTRA","question":"Extra flag?","choices":["on","off"]}]`, false)
	replyBatch := f.bobDMReply(t, pExtra.MessageID, "D234: 1, 1: a, EXTRA: 2")

	beforeDeliveries = len(f.dispatcher.getMessages())
	rrBatch := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: replyBatch.ID,
		IdempotencyKey:  "ik-multi",
		Answers: []InterpretedAnswerInput{
			{Key: "EXTRA", ChoiceID: "2"},
			{Key: pSet.Decisions[1].Key, ChoiceID: "a"},
			{Key: "D234", ChoiceID: "1"},
		},
	})
	require.Equal(t, http.StatusOK, rrBatch.Code, rrBatch.Body.String())
	var outBatch RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rrBatch.Body.Bytes(), &outBatch))
	assert.NotEmpty(t, outBatch.BatchID)
	require.Len(t, outBatch.Decisions, 3)
	assert.Equal(t, []string{"D234", pSet.Decisions[1].Key, "EXTRA"}, []string{
		outBatch.Decisions[0].Key, outBatch.Decisions[1].Key, outBatch.Decisions[2].Key,
	})
	assert.Equal(t, beforeDeliveries, len(f.dispatcher.getMessages()), "multi-pair interpreted answers are never delivered back to the agent")
	assert.Len(t, collectEvents(updates), 3, "one choice.updated SSE event per recorded row")

	// ship-now (3rd row of pSet) remains pending!
	shipNow, err := f.store.GetDecision(ctx, pSet.Decisions[2].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, shipNow.Status)

	batchMsg, err := f.store.GetMessage(ctx, outBatch.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, batchMsg.DispatchState)
	assert.Contains(t, batchMsg.Msg, "answers to 3 decisions: D234 yes · "+pSet.Decisions[1].Key+" Extend · EXTRA off, interpreted from their reply")

	// Verify SSE ChoiceResponseSetView omits envelope PromptMessageID (multi-prompt)
	// and orders answers canonically with per-answer PromptMessageID!
	var sseSet *ChoiceResponseSetView
	for _, e := range collectEvents(chat) {
		var evt UserMessageEvent
		require.NoError(t, json.Unmarshal(e.Data, &evt))
		if evt.ID == outBatch.ResponseMessageID {
			sseSet = evt.ChoiceResponseSet
		}
	}
	require.NotNil(t, sseSet)
	assert.Empty(t, sseSet.PromptMessageID, "multi-prompt batch omits top-level promptMessageId")
	require.Len(t, sseSet.Answers, 3)
	assert.Equal(t, []string{"D234", pSet.Decisions[1].Key, "EXTRA"}, []string{
		sseSet.Answers[0].Key, sseSet.Answers[1].Key, sseSet.Answers[2].Key,
	})
	assert.Equal(t, pSet.MessageID, sseSet.Answers[0].PromptMessageID)
	assert.Equal(t, pSet.MessageID, sseSet.Answers[1].PromptMessageID)
	assert.Equal(t, pExtra.MessageID, sseSet.Answers[2].PromptMessageID)
	for i, a := range sseSet.Answers {
		assert.Equal(t, f.bob.ID, a.AnsweredByUserID, "sseSet[%d]", i)
		assert.Equal(t, f.bob.DisplayName, a.AnsweredByName, "sseSet[%d]", i)
		assert.Equal(t, f.agent.ID, a.InterpretedByAgentID, "sseSet[%d]", i)
		assert.Equal(t, replyBatch.ID, a.SourceMessageID, "sseSet[%d]", i)
	}

	// Verify chat history hydration also omits envelope PromptMessageID and matches canonical order.
	histResp := &chatHistoryResponse{Messages: []store.Message{*batchMsg}}
	f.srv.addChoiceHistoryViews(ctx, histResp)
	histSet, ok := histResp.MessageChoiceResponseSets[outBatch.ResponseMessageID]
	require.True(t, ok)
	assert.Empty(t, histSet.PromptMessageID)
	require.Len(t, histSet.Answers, 3)
	assert.Equal(t, []string{"D234", pSet.Decisions[1].Key, "EXTRA"}, []string{
		histSet.Answers[0].Key, histSet.Answers[1].Key, histSet.Answers[2].Key,
	})
	assert.Equal(t, pSet.MessageID, histSet.Answers[0].PromptMessageID)
	assert.Equal(t, pExtra.MessageID, histSet.Answers[2].PromptMessageID)

	// Verify idempotency replay returns 200 replayed:true in the same canonical order regardless of request pair order.
	rrBatchReplay := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: replyBatch.ID,
		IdempotencyKey:  "ik-multi",
		Answers: []InterpretedAnswerInput{
			{Key: pSet.Decisions[1].Key, ChoiceID: "a"},
			{Key: "EXTRA", ChoiceID: "2"},
			{Key: "D234", ChoiceID: "1"},
		},
	})
	require.Equal(t, http.StatusOK, rrBatchReplay.Code, rrBatchReplay.Body.String())
	var outBatchReplay RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rrBatchReplay.Body.Bytes(), &outBatchReplay))
	assert.True(t, outBatchReplay.Replayed)
	assert.Equal(t, outBatch.ResponseMessageID, outBatchReplay.ResponseMessageID)
	require.Len(t, outBatchReplay.Decisions, 3)
	assert.Equal(t, []string{"D234", pSet.Decisions[1].Key, "EXTRA"}, []string{
		outBatchReplay.Decisions[0].Key, outBatchReplay.Decisions[1].Key, outBatchReplay.Decisions[2].Key,
	})
}

func TestAgentDecisions_InterpretedValidationAndConflicts(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()

	dmPrompt := f.sendPromptToBob(t, twoDecisions, false)
	dmReply := f.bobDMReply(t, dmPrompt.MessageID, "D234: 1")
	topicPromptMsg, topicPromptRes := f.topicPrompt(t, `[{"id":"TOP1","question":"Topic decision?","choices":["yes","no"]}]`, false)
	_ = topicPromptRes
	topicReply := f.storeUserReply(t, f.alice, f.topicID, topicPromptMsg.ConversationID, "web", "TOP1: yes")

	// 1. Shape errors -> 400.
	assert.Equal(t, http.StatusBadRequest, f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
	}).Code)
	var eleven []InterpretedAnswerInput
	for i := 0; i < 11; i++ {
		eleven = append(eleven, InterpretedAnswerInput{Key: api.NewUUID()[:8], ChoiceID: "1"})
	}
	assert.Equal(t, http.StatusBadRequest, f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         eleven,
	}).Code)
	assert.Equal(t, http.StatusBadRequest, f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}, {Key: "d234", ChoiceID: "2"}},
	}).Code)

	// 2. Unknown key -> 404 decision_not_found naming the key.
	rrMissing := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}, {Key: "NO_SUCH_KEY", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusNotFound, rrMissing.Code)
	assert.Equal(t, ErrCodeDecisionNotFound, errorCode(t, rrMissing))
	assert.Contains(t, rrMissing.Body.String(), "NO_SUCH_KEY")

	// 3. Source wrong conversation (missing message ID, or topic reply used for DM decision) -> 400 source_wrong_conversation.
	rrNoMsg := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: api.NewUUID(),
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrNoMsg.Code)
	assert.Equal(t, ErrCodeSourceWrongConversation, errorCode(t, rrNoMsg))

	rrWrongConv := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: topicReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrWrongConv.Code)
	assert.Equal(t, ErrCodeSourceWrongConversation, errorCode(t, rrWrongConv))

	// Batch spanning decisions from two different conversations -> 400 source_wrong_conversation.
	rrSpanConv := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}, {Key: "TOP1", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrSpanConv.Code)
	assert.Equal(t, ErrCodeSourceWrongConversation, errorCode(t, rrSpanConv))

	// Soft-deleted (retracted) web-chat source message -> 400 source_wrong_conversation.
	deletedReply := f.bobDMReply(t, dmPrompt.MessageID, "D234: 1 (retracted)")
	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()
	require.NotNil(t, wcs)
	_, err := markChatMessageDeleted(ctx, wcs, deletedReply.ID, time.Now().UTC())
	require.NoError(t, err)
	rrDeletedSrc := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: deletedReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrDeletedSrc.Code, rrDeletedSrc.Body.String())
	assert.Equal(t, ErrCodeSourceWrongConversation, errorCode(t, rrDeletedSrc))
	d234AfterDel, err := f.store.GetDecision(ctx, dmPrompt.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d234AfterDel.Status)
	assert.Empty(t, d234AfterDel.Answers)

	// Cross-agent isolation on RecordInterpretedAnswers -> 404 decision_not_found.
	otherAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "other-rec-bot", Name: "other-rec-bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, Phase: "running",
		Created: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))
	rrCrossAgent := f.agentDecisionReq(t, otherAgent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		IdempotencyKey:  "ik-cross-agent",
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusNotFound, rrCrossAgent.Code, rrCrossAgent.Body.String())
	assert.Equal(t, ErrCodeDecisionNotFound, errorCode(t, rrCrossAgent))

	// 4. Source message authored by an agent (e.g. the prompt message itself) -> 400 source_not_user.
	rrAgentSrc := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmPrompt.MessageID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrAgentSrc.Code)
	assert.Equal(t, ErrCodeSourceNotUser, errorCode(t, rrAgentSrc))

	// 5. Source message created before a prompt -> 400 source_before_prompt (both 1st and 2nd prompt).
	promptStored, err := f.store.GetMessage(ctx, dmPrompt.MessageID)
	require.NoError(t, err)
	earlyMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID,
		Sender: "user:" + f.bob.Email, SenderID: f.bob.ID,
		Recipient: "agent:" + f.agent.Slug, RecipientID: f.agent.ID,
		Msg: "too early", Type: messages.TypeInstruction, AgentID: f.agent.ID,
		Channel: "web", ThreadID: promptStored.ThreadID, ConversationID: promptStored.ConversationID,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     promptStored.CreatedAt.Add(-time.Second),
	}
	require.NoError(t, f.store.CreateMessage(ctx, earlyMsg))
	rrBefore := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: earlyMsg.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrBefore.Code)
	assert.Equal(t, ErrCodeSourceBeforePrompt, errorCode(t, rrBefore))

	time.Sleep(3 * time.Millisecond)
	laterPrompt := f.sendPromptToBob(t, `[{"id":"QB1","question":"Asked after dmReply?","choices":["yes","no"]}]`, false)
	rrBeforeSecond := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}, {Key: "QB1", ChoiceID: "2"}},
	})
	assert.Equal(t, http.StatusBadRequest, rrBeforeSecond.Code, rrBeforeSecond.Body.String())
	assert.Equal(t, ErrCodeSourceBeforePrompt, errorCode(t, rrBeforeSecond))
	assert.Contains(t, rrBeforeSecond.Body.String(), "QB1")
	for _, rowID := range []string{dmPrompt.Decisions[0].RowID, laterPrompt.Decisions[0].RowID} {
		d, gerr := f.store.GetDecision(ctx, rowID)
		require.NoError(t, gerr)
		assert.Equal(t, store.DecisionStatusPending, d.Status)
		assert.Empty(t, d.Answers)
	}

	// 6. Source author not allowed:
	// (a) Alice forged into Bob's DM thread -> 403 source_author_not_allowed.
	aliceForgedInBobDM := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID,
		Sender: "user:" + f.alice.Email, SenderID: f.alice.ID,
		Recipient: "agent:" + f.agent.Slug, RecipientID: f.agent.ID,
		Msg: "D234: 1", Type: messages.TypeInstruction, AgentID: f.agent.ID,
		Channel: "web", ThreadID: promptStored.ThreadID, ConversationID: promptStored.ConversationID,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().UTC().Add(time.Millisecond),
	}
	require.NoError(t, f.store.CreateMessage(ctx, aliceForgedInBobDM))
	rrWrongAddressee := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: aliceForgedInBobDM.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusForbidden, rrWrongAddressee.Code)
	assert.Equal(t, ErrCodeSourceAuthorNotAllowed, errorCode(t, rrWrongAddressee))

	// (b) Outsider without topic send permission -> 403 source_author_not_allowed.
	outsiderTopicReply := f.storeUserReply(t, f.outsider, f.topicID, topicPromptMsg.ConversationID, "web", "TOP1: yes")
	rrOutsider := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: outsiderTopicReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "TOP1", ChoiceID: "1"}},
	})
	assert.Equal(t, http.StatusForbidden, rrOutsider.Code)
	assert.Equal(t, ErrCodeSourceAuthorNotAllowed, errorCode(t, rrOutsider))

	// 7. Invalid choice ID in a 2-pair batch -> 400 invalid_choice, and valid pair stays pending!
	rrBadChoice := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers: []InterpretedAnswerInput{
			{Key: "D234", ChoiceID: "1"},
			{Key: dmPrompt.Decisions[1].Key, ChoiceID: "nonexistent"},
		},
	})
	assert.Equal(t, http.StatusBadRequest, rrBadChoice.Code)
	assert.Equal(t, ErrCodeInvalidChoice, errorCode(t, rrBadChoice))
	d234Row, err := f.store.GetDecision(ctx, dmPrompt.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, d234Row.Status, "all-or-nothing: D234 must stay pending")

	// 8. Already answered -> 409 already_answered with DecisionView and human-readable message;
	// 2nd pair in a failing batch stays pending!
	rrOK := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrOK.Code)

	rrDup := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: dmReply.ID,
		Answers: []InterpretedAnswerInput{
			{Key: "D234", ChoiceID: "2"},
			{Key: dmPrompt.Decisions[1].Key, ChoiceID: "zzz"},
		},
	})
	assert.Equal(t, http.StatusConflict, rrDup.Code)
	dupCode, dupDetails := errorCodeAndDetails(t, rrDup)
	assert.Equal(t, ErrCodeDecisionAnswered, dupCode)
	assert.Contains(t, rrDup.Body.String(), "D234: already answered (interpreted by you from")
	dupErrs, _ := dupDetails["errors"].([]interface{})
	assert.Len(t, dupErrs, 2, "mixed failure reports both already_answered and invalid_choice")
	assert.NotNil(t, dupDetails["decision"], "409 already_answered includes DecisionView")
	secondRow, err := f.store.GetDecision(ctx, dmPrompt.Decisions[1].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusPending, secondRow.Status, "all-or-nothing: 2nd row must stay pending")

	// 9. Concurrent replacement during record -> 409 replaced with decision and replacedBy details.
	repV1 := f.sendPromptToBob(t, `[{"id":"REP1","question":"Replace v1?","choices":["yes","no"]}]`, false)
	f.sendPromptToBob(t, `[{"id":"REP1","question":"Replace v2?","choices":["yes","no"]}]`, true)
	repV1Row, err := f.store.GetDecision(ctx, repV1.Decisions[0].RowID)
	require.NoError(t, err)
	rrReplaced := httptest.NewRecorder()
	f.srv.writeInterpretedRecordError(rrReplaced, ctx, &store.InterpretedAnswersError{
		Failures: []store.InterpretedKeyFailure{{
			Key:      "REP1",
			Reason:   store.InterpretedReasonReplaced,
			Decision: repV1Row,
		}},
	})
	assert.Equal(t, http.StatusConflict, rrReplaced.Code, rrReplaced.Body.String())
	repCode, repDetails := errorCodeAndDetails(t, rrReplaced)
	assert.Equal(t, ErrCodeDecisionReplaced, repCode)
	assert.NotNil(t, repDetails["decision"])
	assert.NotNil(t, repDetails["replacedBy"])
}

func TestAgentDecisions_SurfaceMappingAndIdempotency(t *testing.T) {
	// Unit checks for every channel alias in A6.
	for ch, want := range map[string]string{
		"":            store.DecisionSurfaceNative,
		"web":         store.DecisionSurfaceNative,
		"native":      store.DecisionSurfaceNative,
		"gchat":       store.DecisionSurfaceGoogleChat,
		"google_chat": store.DecisionSurfaceGoogleChat,
		"chat-app":    store.DecisionSurfaceGoogleChat,
		"slack":       store.DecisionSurfaceSlack,
		"discord":     store.DecisionSurfaceDiscord,
		"telegram":    store.DecisionSurfaceTelegram,
		"teams":       store.DecisionSurfaceTeams,
		"irc":         store.DecisionSurfaceOther,
	} {
		assert.Equal(t, want, decisionSurfaceFromChannel(ch), "channel=%q", ch)
	}

	// HTTP test: gchat source message records google_chat surface, publishes project topic SSE,
	// and idempotency replays (including after --update repost) / rejects mismatches.
	f := newChoicesFixture(t)
	promptMsg, _ := f.topicPrompt(t, `[{"id":"GC1","question":"Approve from Google Chat?","choices":["yes","no"]},{"id":"GC2","question":"Second?","choices":["yes","no"]}]`, false)
	gchatReply := f.storeUserReply(t, f.alice, f.topicID, promptMsg.ConversationID, "gchat", "GC1: yes")
	otherReply := f.storeUserReply(t, f.alice, f.topicID, promptMsg.ConversationID, "slack", "GC2: no")

	projUpdates, unsubProjUpdates := f.srv.events.Subscribe("project." + f.project.ID + ".chat.choice.updated")
	defer unsubProjUpdates()

	rr1 := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: gchatReply.ID,
		IdempotencyKey:  "ik-gchat-1",
		Answers:         []InterpretedAnswerInput{{Key: "GC1", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rr1.Code, rr1.Body.String())
	var out1 RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rr1.Body.Bytes(), &out1))
	assert.False(t, out1.Replayed)
	assert.Equal(t, store.DecisionSurfaceGoogleChat, out1.Decisions[0].Decision.Surface)
	assert.Len(t, collectEvents(projUpdates), 1, "topic interpreted record publishes choice.updated on project subject")

	// Exact retry -> 200 with replayed=true and same response_message_id.
	rrReplay := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: gchatReply.ID,
		IdempotencyKey:  "ik-gchat-1",
		Answers:         []InterpretedAnswerInput{{Key: "gc1", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrReplay.Code, rrReplay.Body.String())
	var outReplay RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rrReplay.Body.Bytes(), &outReplay))
	assert.True(t, outReplay.Replayed)
	assert.Equal(t, out1.ResponseMessageID, outReplay.ResponseMessageID)

	// Repost GC1 with --update (v2) -> idempotent retry of ik-gchat-1 still replays v1 cleanly!
	_, _ = f.topicPrompt(t, `[{"id":"GC1","question":"Approve from Google Chat v2?","choices":["yes","no"]}]`, true)
	rrReplayAfterUpdate := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: gchatReply.ID,
		IdempotencyKey:  "ik-gchat-1",
		Answers:         []InterpretedAnswerInput{{Key: "GC1", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrReplayAfterUpdate.Code, rrReplayAfterUpdate.Body.String())
	var outAfterUpdate RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(rrReplayAfterUpdate.Body.Bytes(), &outAfterUpdate))
	assert.True(t, outAfterUpdate.Replayed)
	assert.Equal(t, out1.ResponseMessageID, outAfterUpdate.ResponseMessageID)
	require.Len(t, outAfterUpdate.Decisions, 1)
	assert.Equal(t, 1, outAfterUpdate.Decisions[0].Version)
	assert.NotNil(t, outAfterUpdate.Decisions[0].ReplacedBy)

	// Mismatched choice, key (existing or nonexistent), or source_message_id -> 422 idempotency_mismatch.
	for name, bad := range map[string]RecordInterpretedRequest{
		"different choice": {
			SourceMessageID: gchatReply.ID, IdempotencyKey: "ik-gchat-1",
			Answers: []InterpretedAnswerInput{{Key: "GC1", ChoiceID: "2"}},
		},
		"different existing key": {
			SourceMessageID: gchatReply.ID, IdempotencyKey: "ik-gchat-1",
			Answers: []InterpretedAnswerInput{{Key: "GC2", ChoiceID: "1"}},
		},
		"nonexistent key": {
			SourceMessageID: gchatReply.ID, IdempotencyKey: "ik-gchat-1",
			Answers: []InterpretedAnswerInput{{Key: "UNKNOWN_KEY", ChoiceID: "1"}},
		},
		"different source message": {
			SourceMessageID: otherReply.ID, IdempotencyKey: "ik-gchat-1",
			Answers: []InterpretedAnswerInput{{Key: "GC1", ChoiceID: "1"}},
		},
	} {
		rr := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", bad)
		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "%s: %s", name, rr.Body.String())
		assert.Equal(t, ErrCodeIdempotencyMismatch, errorCode(t, rr), name)
	}
}

func TestAgentDecisions_ConfirmOverrideAndMixedBatchEndToEnd(t *testing.T) {
	f := newChoicesFixture(t)

	// 1. Single-decision confirm (not delivered) vs override (delivered) vs wrong user (409 not_recorded_answerer).
	topicMsg, topicRes := f.topicPrompt(t, `[{"id":"SINGLE-CONF","question":"Confirm me?","choices":["yes","no"]}]`, false)
	aliceReply := f.storeUserReply(t, f.alice, f.topicID, topicMsg.ConversationID, "web", "yes")

	rr := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: aliceReply.ID,
		Answers:         []InterpretedAnswerInput{{Key: "SINGLE-CONF", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Bob (not recorded answerer) gets 403 not_recorded_answerer.
	bobTry := f.answer(t, f.bob, topicRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	assert.Equal(t, http.StatusForbidden, bobTry.Code)
	assert.Equal(t, ErrCodeNotRecordedAnswerer, errorCode(t, bobTry))

	// Alice confirms -> 200, outcome=confirm, NOT delivered to agent!
	before := len(f.dispatcher.getMessages())
	aliceConf := f.answer(t, f.alice, topicRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, aliceConf.Code, aliceConf.Body.String())
	assert.Equal(t, before, len(f.dispatcher.getMessages()), "confirm is not delivered to the agent")

	// Single-decision override -> IS delivered to agent with overriding suffix!
	overMsg, overRes := f.topicPrompt(t, `[{"id":"SINGLE-OVER","question":"Override me?","choices":["yes","no"]}]`, false)
	aliceReply2 := f.storeUserReply(t, f.alice, f.topicID, overMsg.ConversationID, "web", "yes")
	rr = f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: aliceReply2.ID,
		Answers:         []InterpretedAnswerInput{{Key: "SINGLE-OVER", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rr.Code)

	before = len(f.dispatcher.getMessages())
	aliceOver := f.answer(t, f.alice, overRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "2"})
	require.Equal(t, http.StatusOK, aliceOver.Code, aliceOver.Body.String())
	require.Len(t, f.dispatcher.getMessages(), before+1, "override must be delivered to the agent")
	assert.Contains(t, f.dispatcher.getMessages()[before].structured.Msg, ", overriding interpreted answer yes")

	// 2. Mixed set batch submit (2 interpreted + 1 pending -> confirm + override + answer -> delivered!).
	setPrompt := f.sendSetToBob(t)
	bobReply := f.bobDMReply(t, setPrompt.MessageID, "D234: 1, "+setPrompt.Decisions[1].Key+": a")
	rr = f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: bobReply.ID,
		Answers: []InterpretedAnswerInput{
			{Key: "D234", ChoiceID: "1"},
			{Key: setPrompt.Decisions[1].Key, ChoiceID: "a"},
		},
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	before = len(f.dispatcher.getMessages())
	mixRR := f.submitSet(t, f.bob, setPrompt.MessageID, "",
		setPrompt.Decisions[0].RowID, "1", // confirm
		setPrompt.Decisions[1].RowID, "b", // override
		setPrompt.Decisions[2].RowID, "2", // answer on pending row
	)
	require.Equal(t, http.StatusOK, mixRR.Code, mixRR.Body.String())
	mixOut := decodeSetResponse(t, mixRR)
	assert.Equal(t, store.DecisionOutcomeConfirm, mixOut.Decisions[0].Decision.Outcome)
	assert.Equal(t, store.DecisionOutcomeOverride, mixOut.Decisions[1].Decision.Outcome)
	assert.Equal(t, store.DecisionOutcomeAnswer, mixOut.Decisions[2].Decision.Outcome)
	require.Len(t, f.dispatcher.getMessages(), before+1)
	deliveredText := f.dispatcher.getMessages()[before].structured.Msg
	assert.Contains(t, deliveredText, "(choice 1, recommended, confirmed)")
	assert.Contains(t, deliveredText, "(choice b, overriding interpreted answer Extend)")

	// 3. All-confirm set submit -> 200, NOT delivered to agent!
	allConfPrompt := f.sendPromptToBob(t, `[{"id":"AC1","question":"First?","choices":["yes","no"]},{"id":"AC2","question":"Second?","choices":["yes","no"]}]`, false)
	bobReply2 := f.bobDMReply(t, allConfPrompt.MessageID, "AC1: yes, AC2: no")
	rr = f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: bobReply2.ID,
		Answers: []InterpretedAnswerInput{
			{Key: "AC1", ChoiceID: "1"},
			{Key: "AC2", ChoiceID: "2"},
		},
	})
	require.Equal(t, http.StatusOK, rr.Code)

	before = len(f.dispatcher.getMessages())
	allConfRR := f.submitSet(t, f.bob, allConfPrompt.MessageID, "",
		allConfPrompt.Decisions[0].RowID, "1",
		allConfPrompt.Decisions[1].RowID, "2",
	)
	require.Equal(t, http.StatusOK, allConfRR.Code, allConfRR.Body.String())
	assert.Equal(t, before, len(f.dispatcher.getMessages()), "an all-confirm set submit is not delivered to the agent")
}

func agentListKeys(t *testing.T, f *choicesFixture, agent *store.Agent, params url.Values) []string {
	t.Helper()
	path := "/api/v1/agent/decisions"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	rr := f.agentDecisionReq(t, agent, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
	var out AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	keys := make([]string, len(out.Items))
	for i, it := range out.Items {
		keys[i] = it.Key
	}
	return keys
}

func TestAgentDecisions_SearchArmsWildcardsPagingAndIsolation(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()

	// Other agent's matching decision must never appear in f.agent's search.
	otherAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "other-search-agent", Name: "other-search-agent",
		ProjectID: f.project.ID, OwnerID: f.alice.ID, Phase: "running",
		Created: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))
	otherDecs, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"other-canary","question":"Deploy canary shard?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + otherAgent.Slug, SenderID: otherAgent.ID,
			Recipient: "chat", ThreadID: f.topicID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Other prompt", AskerAgentID: otherAgent.ID, AskerAgentName: otherAgent.Name, Decisions: otherDecs,
	})
	require.NoError(t, err)

	// 1. Topic prompt in #sched-topic by sched-agent: pending, matches question & key,
	//    must NOT match topic label ("sched-topic") or asker agent name ("sched-agent")
	//    or unchosen choice labels ("CanaryLane", "BlueGreenLane").
	_, pCanary := f.topicPrompt(t, `[{"id":"rollout-plan","question":"Roll out the CANARY shard now?","choices":[{"id":"c","label":"CanaryLane"},{"id":"b","label":"BlueGreenLane"}]}]`, false)
	_ = pCanary
	time.Sleep(2 * time.Millisecond)

	// 2. Direct-answered decision: matches effective direct answer label ("OvernightWindow").
	_, pDirect := f.topicPrompt(t, `[{"id":"maint-win","question":"Maintenance slot?","choices":[{"id":"a","label":"OvernightWindow"},{"id":"b","label":"MiddaySlot"}]}]`, false)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, pDirect.Decisions[0].RowID, map[string]interface{}{"choice_id": "a"}).Code)
	time.Sleep(2 * time.Millisecond)

	// 3. Unconfirmed agent-interpreted decision: matches interpreted label ("SoftFreeze").
	pInterp := f.sendPromptToBob(t, `[{"id":"freeze-mode","question":"Freeze policy?","choices":[{"id":"1","label":"SoftFreeze"},{"id":"2","label":"HardLock"}]}]`, false)
	replyInterp := f.bobDMReply(t, pInterp.MessageID, "1")
	rrInterp := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: replyInterp.ID,
		Answers:         []InterpretedAnswerInput{{Key: "freeze-mode", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrInterp.Code, rrInterp.Body.String())
	time.Sleep(2 * time.Millisecond)

	// 4. Overridden decision: interpreted as "AlphaDraft", then overridden directly to "FinalCut".
	//    Effective answer is "FinalCut"; "AlphaDraft" must NOT match!
	pOver := f.sendPromptToBob(t, `[{"id":"cut-mode","question":"Release cut?","choices":[{"id":"1","label":"AlphaDraft"},{"id":"2","label":"FinalCut"}]}]`, false)
	replyOver := f.bobDMReply(t, pOver.MessageID, "1")
	rrOver := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: replyOver.ID,
		Answers:         []InterpretedAnswerInput{{Key: "cut-mode", ChoiceID: "1"}},
	})
	require.Equal(t, http.StatusOK, rrOver.Code, rrOver.Body.String())
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, pOver.Decisions[0].RowID, map[string]interface{}{"choice_id": "2"}).Code)
	time.Sleep(2 * time.Millisecond)

	// 5. Wildcard literals in question and key, plus a lookalike without wildcards.
	f.topicPrompt(t, `[{"id":"rate_limit_v1","question":"Throttle at 50% of C:\\logs?","choices":["yes","no"]}]`, false)
	time.Sleep(2 * time.Millisecond)
	f.topicPrompt(t, `[{"id":"rateXlimitXv1","question":"Throttle at 500 of C:Xlogs?","choices":["yes","no"]}]`, false)
	time.Sleep(2 * time.Millisecond)

	// 6. Reposted key to exercise state=pending vs state=decided vs state=all + paging.
	//    v1 has "shared-tag" in question, gets reposted (becomes replaced), v2 also has "shared-tag".
	f.topicPrompt(t, `[{"id":"repost-k","question":"First shared-tag check?","choices":["yes","no"]}]`, false)
	time.Sleep(2 * time.Millisecond)
	f.topicPrompt(t, `[{"id":"repost-k","question":"Second shared-tag check?","choices":["yes","no"]}]`, true)
	time.Sleep(2 * time.Millisecond)
	_, pSharedDec := f.topicPrompt(t, `[{"id":"decided-tag","question":"Third shared-tag check?","choices":["yes","no"]}]`, false)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, pSharedDec.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)

	// Arms on state=all:
	assert.Equal(t, []string{"rollout-plan"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"canary shard"}}), "question arm, case-insensitive, isolated from otherAgent")
	assert.Equal(t, []string{"rollout-plan"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"ROLLOUT-P"}}), "key arm, case-insensitive")
	assert.Equal(t, []string{"maint-win"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"overnightwindow"}}), "direct effective answer label")
	assert.Equal(t, []string{"freeze-mode"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"softfreeze"}}), "unconfirmed interpreted effective answer label")
	assert.Equal(t, []string{"cut-mode"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"finalcut"}}), "overriding direct answer label matches")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"alphadraft"}}), "overridden interpreted label is not effective and must not match")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"canarylane"}}), "unchosen choice option label must not match")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"middaya"}}), "non-matching substring")

	// D12 exclusions: no conversation label arm and no asker agent name arm.
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"sched-topic"}}), "conversation label must not match")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"#sched"}}), "conversation #label must not match")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"sched-agent"}}), "asker agent name must not match")
	assert.Empty(t, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"other-canary"}}), "other agent's decision must not leak")

	// Wildcards %, _, \ match literally.
	assert.Equal(t, []string{"rate_limit_v1"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"50%"}}))
	assert.Equal(t, []string{"rate_limit_v1"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {"rate_limit"}}))
	assert.Equal(t, []string{"rate_limit_v1"}, agentListKeys(t, f, f.agent, url.Values{"state": {"all"}, "q": {`c:\logs`}}))

	// Combined with state filter:
	// "shared-tag" matches repost-k v2 (pending), decided-tag (decided), repost-k v1 (replaced).
	assert.Equal(t, []string{"repost-k"}, agentListKeys(t, f, f.agent, url.Values{"q": {"shared-tag"}}), "default state=pending")
	assert.Equal(t, []string{"decided-tag"}, agentListKeys(t, f, f.agent, url.Values{"state": {"decided"}, "q": {"shared-tag"}}))

	// Cursor paging with state=all and limit=1 across the 3 "shared-tag" rows (newest first).
	var pagedRefs []string
	cursor := ""
	for page := 0; page < 5; page++ {
		params := url.Values{"state": {"all"}, "q": {"shared-tag"}, "limit": {"1"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		rr := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?"+params.Encode(), nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var out AgentDecisionListResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		for _, it := range out.Items {
			pagedRefs = append(pagedRefs, fmt.Sprintf("%s@%d", it.Key, it.Version))
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	assert.Equal(t, []string{"decided-tag@1", "repost-k@2", "repost-k@1"}, pagedRefs)
}

func TestAgentDecisions_SearchValidationTimeoutAndExperimentGate(t *testing.T) {
	f := newChoicesFixture(t)
	f.topicPrompt(t, oneDecision, false)

	// Bounds validation: 2..256 runes after trim; empty/whitespace means no q.
	for name, qVal := range map[string]string{
		"one rune":            "a",
		"one rune after trim": "   a   ",
		"257 multibyte runes": strings.Repeat("é", 257),
	} {
		rr := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q="+url.QueryEscape(qVal), nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", name, rr.Body.String())
	}
	for name, qVal := range map[string]string{
		"two runes":           "sh",
		"256 multibyte runes": strings.Repeat("é", 256),
		"whitespace is no q":  "   ",
	} {
		rr := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q="+url.QueryEscape(qVal), nil)
		require.Equal(t, http.StatusOK, rr.Code, "%s: %s", name, rr.Body.String())
		if name == "whitespace is no q" {
			var out AgentDecisionListResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
			assert.Len(t, out.Items, 1, "whitespace-only q returns unfiltered list")
			var raw map[string]interface{}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
			_, hasQuery := raw["query"]
			assert.False(t, hasQuery, "whitespace-only q omits query marker")
		}
	}

	// Search capability marker: present as trimmed q when q is set, omitted when q is absent.
	rrTrimmedMarker := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q="+url.QueryEscape("  feature  "), nil)
	require.Equal(t, http.StatusOK, rrTrimmedMarker.Code, rrTrimmedMarker.Body.String())
	var rawWithQ map[string]interface{}
	require.NoError(t, json.Unmarshal(rrTrimmedMarker.Body.Bytes(), &rawWithQ))
	assert.Equal(t, "feature", rawWithQ["query"])

	rrAbsentMarker := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions", nil)
	require.Equal(t, http.StatusOK, rrAbsentMarker.Code, rrAbsentMarker.Body.String())
	var rawWithoutQ map[string]interface{}
	require.NoError(t, json.Unmarshal(rrAbsentMarker.Body.Bytes(), &rawWithoutQ))
	_, hasAbsentQuery := rawWithoutQ["query"]
	assert.False(t, hasAbsentQuery, "absent q omits query marker")

	// Invalid cursor with q still preserves store.ErrInvalidInput message text.
	rrBadCursor := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q=feature&cursor=not-a-cursor", nil)
	assert.Equal(t, http.StatusBadRequest, rrBadCursor.Code, rrBadCursor.Body.String())
	assert.Contains(t, rrBadCursor.Body.String(), "invalid cursor")

	// Experiment gating: web.decisions gates agent decisions?q=.
	setChoicesExperiment(t, f.srv, false)
	rrOff := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q=feature", nil)
	assert.Equal(t, http.StatusNotFound, rrOff.Code, "web.decisions=false returns 404")

	setChoicesExperiment(t, f.srv, true)
	assert.Equal(t, []string{"D234"}, agentListKeys(t, f, f.agent, url.Values{"q": {"feature"}}),
		"agent search works when web.decisions=true")

	// Timeout mapping: ChatSearchTimeout expiry with q returns 503 SEARCH_TIMEOUT; omitting q stays 200.
	f.srv.config.ChatSearchTimeout = time.Nanosecond
	rrTimeout := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q=feature", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rrTimeout.Code, rrTimeout.Body.String())
	assert.Equal(t, "SEARCH_TIMEOUT", errorCode(t, rrTimeout))

	rrNoQ := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions", nil)
	assert.Equal(t, http.StatusOK, rrNoQ.Code, rrNoQ.Body.String())
}

type agentDecisionSearchFaultStore struct {
	store.Store
	fault     *storeFaultSwitch
	interrupt bool
}

func (s *agentDecisionSearchFaultStore) ListAgentDecisions(ctx context.Context, agentID string, state store.DecisionListState, query string, cursor string, limit int) ([]*store.Decision, string, error) {
	if s.fault.Active() {
		if s.interrupt {
			<-ctx.Done()
			return nil, "", errors.New("interrupted (9)")
		}
		return nil, "", &pgconn.PgError{Code: pgQueryCanceled, Message: "canceling statement due to statement timeout"}
	}
	return s.Store.ListAgentDecisions(ctx, agentID, state, query, cursor, limit)
}

func TestAgentDecisions_SearchPostgresCancelAndInterruptMapToSearchTimeout(t *testing.T) {
	var wrapped *agentDecisionSearchFaultStore
	srv, s, alice, bob, project, wrapped, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *agentDecisionSearchFaultStore {
		return &agentDecisionSearchFaultStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, s, alice, bob, project)
	fault.Arm()

	// Postgres SQLSTATE 57014 -> 503 SEARCH_TIMEOUT.
	rrPg := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q=feature", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rrPg.Code, rrPg.Body.String())
	assert.Equal(t, "SEARCH_TIMEOUT", errorCode(t, rrPg))

	// SQLite interrupt after searchCtx deadline expires (non-context error) -> 503 SEARCH_TIMEOUT.
	wrapped.interrupt = true
	f.srv.config.ChatSearchTimeout = 20 * time.Millisecond
	rrInterrupt := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?q=feature", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rrInterrupt.Code, rrInterrupt.Body.String())
	assert.Equal(t, "SEARCH_TIMEOUT", errorCode(t, rrInterrupt))
}
