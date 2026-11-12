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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// threeDecisions is a set prompt (design choices v5): D234 recommends yes,
// the hub-numbered second decision has no recommended choice, ship-now
// recommends no.
const threeDecisions = `[{"id":"D234","question":"Ship the feature now?","choices":["yes","no"],"default":"yes"},{"question":"Which approach?","choices":[{"id":"a","label":"Extend"},{"id":"b","label":"New type"}]},{"id":"ship-now","question":"Deploy today?","choices":["yes","no"],"default":"no"}]`

// submitSet posts a set submit for prompt msgID as u.
func (f *choicesFixture) submitSet(t *testing.T, u *store.User, msgID, idem string, pairs ...string) *httptest.ResponseRecorder {
	t.Helper()
	answers := []map[string]interface{}{}
	for i := 0; i+1 < len(pairs); i += 2 {
		answers = append(answers, map[string]interface{}{"decision_row_id": pairs[i], "choice_id": pairs[i+1]})
	}
	body := map[string]interface{}{"answers": answers}
	if idem != "" {
		body["idempotency_key"] = idem
	}
	return doRequestAsUser(t, f.srv, u, http.MethodPost, "/api/v1/choice-prompts/"+msgID+"/answers", body)
}

// sendSetToBob sends the three-decision prompt to bob and returns the
// response with its message id and rows.
func (f *choicesFixture) sendSetToBob(t *testing.T) choiceSendResponse {
	t.Helper()
	resp := f.sendPromptToBob(t, threeDecisions, false)
	require.Len(t, resp.Decisions, 3)
	return resp
}

func decodeSetResponse(t *testing.T, rr *httptest.ResponseRecorder) ChoiceSetAnswerResponse {
	t.Helper()
	var out ChoiceSetAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

// Criteria 19, 22 and 23: one submit records every row with one batch id,
// stores and delivers one choice-response listing every answer in position
// order with labels from the stored rows, marks the recommended choices,
// and carries the set metadata (not v4's per-decision keys).
func TestChoiceSetAnswer_RecordsAndDeliversOneResponse(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	before := len(f.dispatcher.getMessages())

	rr := f.submitSet(t, f.bob, resp.MessageID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)
	require.NotEmpty(t, out.BatchID)
	require.NotEmpty(t, out.ResponseMessageID)
	require.Len(t, out.Decisions, 3)
	for _, v := range out.Decisions {
		assert.Equal(t, store.DecisionStatusDecided, v.Status)
		require.NotNil(t, v.Decision)
		assert.Equal(t, out.BatchID, v.Decision.BatchID)
		assert.Equal(t, out.ResponseMessageID, v.Decision.ResponseMessageID)
	}
	assert.True(t, out.Decisions[0].Decision.MatchedDefault, "D234 answered yes, its recommended choice")
	assert.False(t, out.Decisions[1].Decision.MatchedDefault, "no recommended choice")
	assert.False(t, out.Decisions[2].Decision.MatchedDefault, "ship-now answered yes against its recommended no")

	ctx := context.Background()
	m, err := f.store.GetMessage(ctx, out.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
	assert.Contains(t, m.Msg, "answered 3 decisions: D234 yes · 1 Extend · ship-now yes")

	sent := f.dispatcher.getMessages()[before:]
	require.Len(t, sent, 1, "one delivery for the whole set")
	sm := sent[0].structured
	require.NotNil(t, sm)
	assert.Equal(t, messages.TypeChoiceResponse, sm.Type)
	lines := strings.Split(sm.Msg, "\n")
	require.Len(t, lines, 4, sm.Msg)
	assert.Equal(t, "[choice-response] 3 decisions answered by "+f.bob.Email+" (prompt "+resp.MessageID+")", lines[0])
	assert.Equal(t, `D234 v1 ("Ship the feature now?") answer=yes (choice 1, recommended)`, lines[1])
	assert.Equal(t, `1 v1 ("Which approach?") answer=Extend (choice a)`, lines[2])
	assert.Equal(t, `ship-now v1 ("Deploy today?") answer=yes (choice 1)`, lines[3])
	assert.Equal(t, out.BatchID, sm.Metadata[messages.MetadataChoiceBatch])
	assert.Equal(t, "D234@1=1,1@1=a,ship-now@1=1", sm.Metadata[messages.MetadataChoiceAnswers])
	assert.Empty(t, sm.Metadata[messages.MetadataChoiceKey], "v4's per-decision keys are not set on a set response")
}

// Criterion 22: labels come from the stored rows even when the client sends
// its own.
func TestChoiceSetAnswer_LabelsFromStoredRows(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	body := map[string]interface{}{"answers": []map[string]interface{}{
		{"decision_row_id": r[0].RowID, "choice_id": "1", "label": "HACKED"},
		{"decision_row_id": r[1].RowID, "choice_id": "a", "choice_label": "HACKED"},
		{"decision_row_id": r[2].RowID, "choice_id": "2"},
	}}
	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/choice-prompts/"+resp.MessageID+"/answers", body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)
	assert.Equal(t, "yes", out.Decisions[0].Decision.Label)
	assert.Equal(t, "Extend", out.Decisions[1].Decision.Label)
	assert.True(t, out.Decisions[2].Decision.MatchedDefault, "ship-now answered no, its recommended choice")
}

// Criterion 19: a set that differs from the actionable set is 409 with fresh
// views and a reason per row; nothing is stored and nothing is sent.
func TestChoiceSetAnswer_ChangedSet409(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	before := len(f.dispatcher.getMessages())

	rr := f.submitSet(t, f.bob, resp.MessageID, "", r[0].RowID, "1", r[1].RowID, "a")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	code, details := errorCodeAndDetails(t, rr)
	assert.Equal(t, ErrCodeAnswerSetChanged, code)
	reasons, ok := details["reasons"].(map[string]interface{})
	require.True(t, ok, rr.Body.String())
	assert.Equal(t, map[string]interface{}{r[2].RowID: store.AnswerSetReasonMissing}, reasons)
	views, ok := details["decisions"].([]interface{})
	require.True(t, ok)
	assert.Len(t, views, 3, "every fresh view of the prompt")

	ctx := context.Background()
	for _, d := range r {
		got, err := f.store.GetDecision(ctx, d.RowID)
		require.NoError(t, err)
		assert.Empty(t, got.Answers, "nothing stored")
	}
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing sent")
}

// Criterion 24: of two set submits one wins and the other gets 409; after an
// --update repost of one key the old prompt's remaining decisions submit as
// a smaller set.
func TestChoiceSetAnswer_SecondSubmitAndSmallerSet(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	rr := f.submitSet(t, f.bob, resp.MessageID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	rr = f.submitSet(t, f.bob, resp.MessageID, "", r[0].RowID, "2", r[1].RowID, "b", r[2].RowID, "2")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	code, details := errorCodeAndDetails(t, rr)
	assert.Equal(t, ErrCodeAnswerSetChanged, code)
	reasons := details["reasons"].(map[string]interface{})
	for _, d := range r {
		assert.Equal(t, store.AnswerSetReasonDecided, reasons[d.RowID])
	}

	g := newChoicesFixture(t)
	set := g.sendSetToBob(t)
	s := set.Decisions
	g.sendPromptToBob(t, `[{"id":"ship-now","question":"Deploy today, really?","choices":["yes","no"]}]`, true)
	rr = g.submitSet(t, g.bob, set.MessageID, "", s[0].RowID, "1", s[1].RowID, "b")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)
	require.Len(t, out.Decisions, 3)
	assert.Equal(t, store.DecisionStatusReplaced, out.Decisions[2].Status)
}

// Criterion 20: shape errors are 400; a replay with the same body returns the
// recorded set, and the same key with another body is 422.
func TestChoiceSetAnswer_ShapeAndIdempotency(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	other := f.sendPromptToBob(t, `[{"id":"E1","question":"Other?","choices":["yes","no"]},{"id":"E2","question":"Other two?","choices":["yes","no"]}]`, false)

	for name, pairs := range map[string][]string{
		"empty":          nil,
		"duplicate row":  {r[0].RowID, "1", r[0].RowID, "2", r[1].RowID, "a", r[2].RowID, "1"},
		"row not in M":   {r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1", other.Decisions[0].RowID, "1"},
		"unknown choice": {r[0].RowID, "1", r[1].RowID, "z", r[2].RowID, "1"},
	} {
		rr := f.submitSet(t, f.bob, resp.MessageID, "", pairs...)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", name, rr.Body.String())
	}

	pairs := []string{r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "2"}
	first := f.submitSet(t, f.bob, resp.MessageID, "k1", pairs...)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	// The 200's wire keys are camelCase (design §6, Q9), like the single
	// endpoint's; the struct round-trip below does not depend on the tags.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &raw))
	assert.Contains(t, raw, "responseMessageId")
	assert.Contains(t, raw, "batchId")
	assert.NotContains(t, raw, "response_message_id")
	assert.NotContains(t, raw, "batch_id")
	again := f.submitSet(t, f.bob, resp.MessageID, "k1", pairs...)
	require.Equal(t, http.StatusOK, again.Code, "body: %s", again.Body.String())
	a, b := decodeSetResponse(t, first), decodeSetResponse(t, again)
	assert.True(t, b.Replayed)
	assert.Equal(t, a.BatchID, b.BatchID)
	assert.Equal(t, a.ResponseMessageID, b.ResponseMessageID)

	rr := f.submitSet(t, f.bob, resp.MessageID, "k1", r[0].RowID, "2", r[1].RowID, "a", r[2].RowID, "2")
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeIdempotencyMismatch, errorCode(t, rr))
}

// Criterion 20: an outsider gets 404; an agent, a visitor and a token get
// 403 (the token and a federated user the session-only refusal); with the
// flag off the endpoint is 404.
func TestChoiceSetAnswer_Authorization(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, threeDecisions, false)
	msg := res.Decisions[0]
	d, err := f.store.GetDecision(context.Background(), msg.RowID)
	require.NoError(t, err)
	promptID := d.MessageID
	path := "/api/v1/choice-prompts/" + promptID + "/answers"
	body := map[string]interface{}{"answers": []map[string]interface{}{{"decision_row_id": msg.RowID, "choice_id": "1"}}}

	rr := doRequestAsUser(t, f.srv, f.outsider, http.MethodPost, path, body)
	assert.Equal(t, http.StatusNotFound, rr.Code, "outsider: %s", rr.Body.String())

	uat := NewScopedUserIdentity(authUser(f.bob), f.project.ID, []string{"project:read"})
	federated := NewFederatedUserIdentity("https://issuer.choices.test", "choices-fed", f.bob.Email, "Fed Bob", "member", nil)
	visitor := NewVisitorIdentity(tid("choices-visitor"), "https://issuer.choices.test", "choices-visitor", "visitor@choices.test", "Visitor")
	for name, id := range map[string]Identity{"user access token": uat, "federated user": federated, "visitor": visitor} {
		rec := requestAsIdentity(t, f.srv, id, http.MethodPost, path, body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		_, details := errorCodeAndDetails(t, rec)
		if name != "visitor" {
			assert.Equal(t, string(authzop.ReasonInteractiveState), details["reason"], name)
			assert.Equal(t, "session_required", details["credential"], name)
		}
	}
	agentReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"answers":[{"decision_row_id":"`+msg.RowID+`","choice_id":"1"}]}`))
	agentReq.Header.Set("Content-Type", "application/json")
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq.WithContext(agentCtx(agentReq.Context(), f.agent)))
	assert.Equal(t, http.StatusForbidden, agentRec.Code, "agent: %s", agentRec.Body.String())

	setChoicesExperiment(t, f.srv, false)
	rr = doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, body)
	assert.Equal(t, http.StatusNotFound, rr.Code, "flag off: %s", rr.Body.String())
}

// Criterion 21: the per-decision endpoint refuses any row of a set with 409
// batch_required, naming the prompt and the rows the caller may answer.
func TestDecisionAnswer_BatchRequired(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendPromptToBob(t, twoDecisions, false)
	rr := f.answer(t, f.bob, resp.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	code, details := errorCodeAndDetails(t, rr)
	assert.Equal(t, ErrCodeBatchRequired, code)
	assert.Equal(t, resp.MessageID, details["promptMessageId"])
	actionable, ok := details["actionable"].([]interface{})
	require.True(t, ok, rr.Body.String())
	assert.Len(t, actionable, 2)
	d, err := f.store.GetDecision(context.Background(), resp.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Empty(t, d.Answers)
}

// Criterion 22: a set whose every row is a confirm of the user's own
// interpreted answers is logged and not delivered.
func TestChoiceSetAnswer_AllConfirmNotDelivered(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	// Each row's own first choice: D234 has 1 and 2, the second decision a and b.
	f.recordInterpreted(t, f.alice, res.Decisions[0].RowID, "1")
	f.recordInterpreted(t, f.alice, res.Decisions[1].RowID, "a")
	row, err := f.store.GetDecision(context.Background(), res.Decisions[0].RowID)
	require.NoError(t, err)
	before := len(f.dispatcher.getMessages())

	rr := f.submitSet(t, f.alice, row.MessageID, "", res.Decisions[0].RowID, "1", res.Decisions[1].RowID, "a")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)
	for _, v := range out.Decisions {
		require.NotNil(t, v.Decision)
		assert.Equal(t, store.DecisionOutcomeConfirm, v.Decision.Outcome)
	}
	assert.Len(t, f.dispatcher.getMessages(), before, "an all-confirm set is not delivered")
	m, err := f.store.GetMessage(context.Background(), out.ResponseMessageID)
	require.NoError(t, err, "it is still logged")
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
}

// Criterion 18 and design v5 5.2: history marks set and single prompts, and
// a set's response row carries choiceResponseSet; with the flag off both
// are omitted.
func TestChoicesHistory_AnswerModeAndResponseSet(t *testing.T) {
	f := newChoicesFixture(t)
	setMsg, setRes := f.topicPrompt(t, threeDecisions, false)
	singleMsg, _ := f.topicPrompt(t, `[{"id":"solo","question":"Alone?","choices":["yes","no"]}]`, false)
	rows := setRes.Decisions
	rr := f.submitSet(t, f.bob, setMsg.ID, "", rows[0].RowID, "1", rows[1].RowID, "a", rows[2].RowID, "2")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)

	history := func() chatHistoryResponse {
		rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var h chatHistoryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &h))
		return h
	}
	h := history()
	assert.Equal(t, ChoiceAnswerModeSet, h.MessageChoiceAnswerModes[setMsg.ID])
	assert.Equal(t, ChoiceAnswerModeSingle, h.MessageChoiceAnswerModes[singleMsg.ID])
	crs, ok := h.MessageChoiceResponseSets[out.ResponseMessageID]
	require.True(t, ok)
	assert.Equal(t, setMsg.ID, crs.PromptMessageID)
	assert.Equal(t, out.BatchID, crs.BatchID)
	require.Len(t, crs.Answers, 3)
	assert.Equal(t, "D234", crs.Answers[0].Key, "position order")
	assert.True(t, crs.Answers[2].MatchedDefault, "ship-now answered no, its recommended choice")
	assert.Equal(t, store.DecisionOutcomeAnswer, crs.Answers[1].Outcome)
	_, single := h.MessageChoiceResponses[out.ResponseMessageID]
	assert.False(t, single, "a set response is not a one-click response")

	setChoicesExperiment(t, f.srv, false)
	h = history()
	assert.Nil(t, h.MessageChoiceAnswerModes)
	assert.Nil(t, h.MessageChoiceResponseSets)
}

// The idempotency key is scoped to the user and the prompt: in a group
// topic, bob reusing alice's key gets the state result (409, the rows are
// decided), not 422.
func TestChoiceSetAnswer_IdempotencyKeyScopedToUser(t *testing.T) {
	f := newChoicesFixture(t)
	msg, res := f.topicPrompt(t, threeDecisions, false)
	r := res.Decisions
	rr := f.submitSet(t, f.alice, msg.ID, "k", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	rr = f.submitSet(t, f.bob, msg.ID, "k", r[0].RowID, "2", r[1].RowID, "b", r[2].RowID, "2")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeAnswerSetChanged, errorCode(t, rr))
}

// Criterion 24: concurrent set submits of one prompt have exactly one
// winner: one 200, the rest 409 answer_set_changed, one batch, one response
// message and one delivery.
func TestChoiceSetAnswer_ConcurrentOneWinner(t *testing.T) {
	f := newChoicesFixture(t)
	msg, res := f.topicPrompt(t, threeDecisions, false)
	r := res.Decisions
	before := len(f.dispatcher.getMessages())

	const n = 4
	var wg sync.WaitGroup
	codes := make([]int, n)
	errCodes := make([]string, n)
	// A start barrier: every goroutine waits until all are spawned, so the
	// submits really overlap.
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			u := []*store.User{f.alice, f.bob}[i%2]
			choice := []string{"1", "2"}[i%2]
			second := []string{"a", "b"}[i%2]
			rec := f.submitSet(t, u, msg.ID, "", r[0].RowID, choice, r[1].RowID, second, r[2].RowID, choice)
			codes[i] = rec.Code
			if rec.Code != http.StatusOK {
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				errCodes[i] = body.Error.Code
			}
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, c := range codes {
		if c == http.StatusOK {
			ok++
			continue
		}
		assert.Equal(t, http.StatusConflict, c, "codes %v", codes)
		assert.Equal(t, ErrCodeAnswerSetChanged, errCodes[i], "codes %v", codes)
	}
	assert.Equal(t, 1, ok, "codes %v", codes)

	ctx := context.Background()
	batches := map[string]bool{}
	responses := map[string]bool{}
	for _, d := range r {
		got, err := f.store.GetDecision(ctx, d.RowID)
		require.NoError(t, err)
		require.Len(t, got.Answers, 1, "one answer per row")
		batches[got.Answers[0].BatchID] = true
		responses[got.Answers[0].ResponseMessageID] = true
	}
	assert.Len(t, batches, 1, "one batch")
	assert.Len(t, responses, 1, "one response message")
	assert.Len(t, f.dispatcher.getMessages(), before+1, "one delivery")
}

// Design v5 5.2: choice.updated is published once per answered row, in
// position order, and the response row's chat event carries
// choiceResponseSet.
func TestChoiceSetAnswer_PublishesUpdatesAndResponseSet(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendSetToBob(t)
	r := resp.Decisions
	updates, unsubUpdates := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.choice.updated")
	defer unsubUpdates()
	chat, unsubChat := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.dm")
	defer unsubChat()

	rr := f.submitSet(t, f.bob, resp.MessageID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "2")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)

	evts := collectEvents(updates)
	require.Len(t, evts, 3, "one choice.updated per row")
	for i, e := range evts {
		var got struct {
			Decision DecisionView `json:"decision"`
		}
		require.NoError(t, json.Unmarshal(e.Data, &got))
		assert.Equal(t, r[i].RowID, got.Decision.RowID, "position order")
		assert.Equal(t, store.DecisionStatusDecided, got.Decision.Status)
	}

	var set *ChoiceResponseSetView
	for _, e := range collectEvents(chat) {
		var evt UserMessageEvent
		require.NoError(t, json.Unmarshal(e.Data, &evt))
		if evt.ID == out.ResponseMessageID {
			set = evt.ChoiceResponseSet
		}
	}
	require.NotNil(t, set, "the response row's chat event carries choiceResponseSet")
	assert.Equal(t, out.BatchID, set.BatchID)
	assert.Equal(t, resp.MessageID, set.PromptMessageID)
	require.Len(t, set.Answers, 3)
	assert.Equal(t, "D234", set.Answers[0].Key)
}

// The remaining set outcomes over HTTP: a mixed set's delivery lines, the
// replaced and not_recorded_answerer reasons, MESSAGE_DENIED, and a chat
// reader without send rights.
func TestChoiceSetAnswer_Outcomes(t *testing.T) {
	ctx := context.Background()
	t.Run("mixed set delivery lines", func(t *testing.T) {
		f := newChoicesFixture(t)
		msg, res := f.topicPrompt(t, threeDecisions, false)
		r := res.Decisions
		f.recordInterpreted(t, f.alice, r[0].RowID, "1")
		f.recordInterpreted(t, f.alice, r[1].RowID, "a")
		before := len(f.dispatcher.getMessages())
		rr := f.submitSet(t, f.alice, msg.ID, "", r[0].RowID, "1", r[1].RowID, "b", r[2].RowID, "2")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
		sent := f.dispatcher.getMessages()[before:]
		require.Len(t, sent, 1)
		text := sent[0].structured.Msg
		assert.Contains(t, text, `D234 v1 ("Ship the feature now?") answer=yes (choice 1, recommended, confirmed)`)
		assert.Contains(t, text, `answer=New type (choice b, overriding interpreted answer Extend)`)
		assert.Contains(t, text, `ship-now v1 ("Deploy today?") answer=no (choice 2, recommended)`)
	})
	t.Run("replaced row", func(t *testing.T) {
		f := newChoicesFixture(t)
		msg, res := f.topicPrompt(t, threeDecisions, false)
		r := res.Decisions
		f.topicPrompt(t, `[{"id":"ship-now","question":"Deploy today, really?","choices":["yes","no"]}]`, true)
		before := len(f.dispatcher.getMessages())
		rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
		require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
		_, details := errorCodeAndDetails(t, rr)
		assert.Equal(t, store.AnswerSetReasonReplaced, details["reasons"].(map[string]interface{})[r[2].RowID])
		for _, d := range r {
			got, err := f.store.GetDecision(ctx, d.RowID)
			require.NoError(t, err)
			assert.Empty(t, got.Answers, "nothing stored")
		}
		assert.Len(t, f.dispatcher.getMessages(), before, "nothing sent")
	})
	t.Run("another user's interpreted row", func(t *testing.T) {
		f := newChoicesFixture(t)
		msg, res := f.topicPrompt(t, threeDecisions, false)
		r := res.Decisions
		f.recordInterpreted(t, f.alice, r[0].RowID, "1")
		before := len(f.dispatcher.getMessages())
		rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
		require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
		_, details := errorCodeAndDetails(t, rr)
		assert.Equal(t, store.AnswerSetReasonNotRecordedAnswerer, details["reasons"].(map[string]interface{})[r[0].RowID])
		for i, d := range r {
			got, err := f.store.GetDecision(ctx, d.RowID)
			require.NoError(t, err)
			want := 0
			if i == 0 {
				want = 1 // alice's interpreted answer only
			}
			assert.Len(t, got.Answers, want, "nothing stored")
		}
		assert.Len(t, f.dispatcher.getMessages(), before, "nothing sent")
	})
	t.Run("asker takes no messages", func(t *testing.T) {
		f := newChoicesFixture(t)
		msg, res := f.topicPrompt(t, threeDecisions, false)
		r := res.Decisions
		agent, err := f.store.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		agent.MessageMode = store.MessageModeNone
		require.NoError(t, f.store.UpdateAgent(ctx, agent))
		rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
		require.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
		assert.Equal(t, ErrCodeMessageDenied, errorCode(t, rr))
		d, err := f.store.GetDecision(ctx, r[0].RowID)
		require.NoError(t, err)
		assert.Empty(t, d.Answers, "nothing stored")
	})
	t.Run("chat reader", func(t *testing.T) {
		f := newChoicesFixture(t)
		msg, res := f.topicPrompt(t, threeDecisions, false)
		r := res.Decisions
		f.makeChatReader(t, f.bob)
		rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
		assert.Equal(t, http.StatusForbidden, rr.Code, "a reader may read the thread but not post in it: %s", rr.Body.String())
		assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, rr))
		d, err := f.store.GetDecision(ctx, r[0].RowID)
		require.NoError(t, err)
		assert.Empty(t, d.Answers)
	})
}

// askerFaultStore fails GetAgent for one agent id while its switch is
// armed, and counts those armed lookups; every other call delegates.
type askerFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
	hits    atomic.Int32
}

var errAskerLookupFault = errors.New("injected asker lookup fault")

func (s *askerFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() && id == s.agentID {
		s.hits.Add(1)
		return nil, errAskerLookupFault
	}
	return s.Store.GetAgent(ctx, id)
}

// newAskerFaultFixture is a choices fixture whose store fails the asking
// agent's lookup once armed (installed before the audited setup).
func newAskerFaultFixture(t *testing.T) (*choicesFixture, *askerFaultStore, *storeFaultSwitch) {
	t.Helper()
	srv, s, alice, bob, project, wrapped, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *askerFaultStore {
		return &askerFaultStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, s, alice, bob, project)
	wrapped.agentID = f.agent.ID
	return f, wrapped, fault
}

// A deleted asker (design choices v4: answers are still recorded, and
// dispatch follows the undeliverable semantics): the set is recorded
// (200), its one response row is stored failed, and nothing is sent.
func TestChoiceSetAnswer_AskerGoneRecordsWithoutDelivery(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()
	msg, res := f.topicPrompt(t, threeDecisions, false)
	r := res.Decisions
	require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))
	before := len(f.dispatcher.getMessages())

	rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	out := decodeSetResponse(t, rr)
	for _, d := range r {
		got, err := f.store.GetDecision(ctx, d.RowID)
		require.NoError(t, err)
		require.Len(t, got.Answers, 1, "the answer is recorded")
		assert.Equal(t, out.BatchID, got.Answers[0].BatchID)
	}
	m, err := f.store.GetMessage(ctx, out.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
	assert.Equal(t, store.MessageDispatchFailed, m.DispatchState)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing is sent to a gone asker")
	// Positive control for assertNoChoiceResponse: the same thread filter
	// finds this one response row.
	rows, err := f.store.ListMessages(ctx, store.MessageFilter{ThreadID: f.topicID, Type: messages.TypeChoiceResponse}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, rows.Items, 1, "control: the thread filter sees the stored response row")
}

// A store fault on the asker lookup that is not "not found" ends the
// submit with an error response before anything is recorded (500 for this
// untyped fault): no answer, no response row, nothing sent, and no
// messaging check skipped.
func TestChoiceSetAnswer_AskerLookupFaultRecordsNothing(t *testing.T) {
	f, wrapped, fault := newAskerFaultFixture(t)
	ctx := context.Background()
	msg, res := f.topicPrompt(t, threeDecisions, false)
	r := res.Decisions
	before := len(f.dispatcher.getMessages())

	fault.Arm()
	rr := f.submitSet(t, f.bob, msg.ID, "", r[0].RowID, "1", r[1].RowID, "a", r[2].RowID, "1")
	require.Equal(t, http.StatusInternalServerError, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeInternalError, errorCode(t, rr))
	assert.Equal(t, int32(1), wrapped.hits.Load(), "the asker lookup was reached once")
	for _, d := range r {
		got, err := f.store.GetDecision(ctx, d.RowID)
		require.NoError(t, err)
		assert.Empty(t, got.Answers, "nothing stored")
	}
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing sent")
	f.assertNoChoiceResponse(t)
}

// The per-decision endpoint (one-decision prompt) gets the same handling:
// a non-not-found asker lookup fault is an error response (500 for this
// untyped fault) with nothing recorded or sent. The gone-asker case is
// TestDecisionAnswer_AskerGoneStoresFailedResponse.
func TestDecisionAnswer_AskerLookupFaultRecordsNothing(t *testing.T) {
	f, wrapped, fault := newAskerFaultFixture(t)
	ctx := context.Background()
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	before := len(f.dispatcher.getMessages())

	fault.Arm()
	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusInternalServerError, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeInternalError, errorCode(t, rr))
	assert.Equal(t, int32(1), wrapped.hits.Load(), "the asker lookup was reached once")
	got, err := f.store.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Empty(t, got.Answers, "nothing stored")
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing sent")
	f.assertNoChoiceResponse(t)
}

// assertNoChoiceResponse checks that the fixture's topic thread holds no
// choice-response message.
func (f *choicesFixture) assertNoChoiceResponse(t *testing.T) {
	t.Helper()
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{ThreadID: f.topicID, Type: messages.TypeChoiceResponse}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no choice-response row is stored")
}
