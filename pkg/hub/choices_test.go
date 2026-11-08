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

// Choice prompts (design choices v4): the send path on outbound-message,
// the hub-minted type refusal on every client path, the decision answer and
// get endpoints, delivery of the choice-response, and the history views.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// choicesFixture is the scheduled-send fixture (alice owns the project, bob
// is a member and owns the running agent, a topic whose default agent it is)
// with web.chat_choices on, plus an outsider with no project access.
type choicesFixture struct {
	*scheduledSendFixture
	outsider *store.User
}

func newChoicesFixture(t *testing.T) *choicesFixture {
	t.Helper()
	f := newScheduledSendFixture(t)
	setChoicesExperiment(t, f.srv, true)
	ctx := context.Background()
	outsider := &store.User{
		ID: tid("choices-outsider"), Email: "outsider-choices@example.com", DisplayName: "Out Sider",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, f.store, outsider.ID)
	return &choicesFixture{scheduledSendFixture: f, outsider: outsider}
}

func setChoicesExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ChatScheduledSend: true, experiments.ChatChoices: enabled})
	require.Equal(t, enabled, srv.experimentEnabled(experiments.ChatChoices))
}

func decisionInputs(t *testing.T, raw string) []messages.DecisionInput {
	t.Helper()
	var in []messages.DecisionInput
	require.NoError(t, json.Unmarshal([]byte(raw), &in))
	return in
}

const twoDecisions = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"],"default":"yes"},{"question":"Which approach?","choices":[{"id":"a","label":"Extend"},{"id":"b","label":"New type"}]}]`

// sendAsAgent posts req to the agent's outbound-message handler.
func (f *choicesFixture) sendAsAgent(t *testing.T, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, r, f.agent.ID)
	return rr
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body.Error.Code
}

type choiceSendResponse struct {
	MessageID string                 `json:"message_id"`
	Decisions []ChoiceDecisionResult `json:"decisions"`
}

// sendPromptToBob sends a choice prompt from the agent to bob's DM.
func (f *choicesFixture) sendPromptToBob(t *testing.T, raw string, update bool) choiceSendResponse {
	t.Helper()
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email, Msg: "Two calls", Choices: decisionInputs(t, raw), ChoicesUpdate: update,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp choiceSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

// topicPrompt stores a choice prompt in the fixture's topic directly.
func (f *choicesFixture) topicPrompt(t *testing.T, raw string, update bool) (*store.Message, *store.ChoicePromptResult) {
	t.Helper()
	decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, raw))
	require.NoError(t, err)
	msg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
		AgentID: f.agent.ID, Channel: "web", ThreadID: f.topicID, CreatedAt: time.Now().UTC(),
		DispatchState: store.MessageDispatchDispatched,
	}
	res, err := f.store.CreateChoicePrompt(context.Background(), &store.ChoicePromptInput{
		Message: msg, Intro: "Please decide", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name,
		Decisions: decs, Update: update,
	})
	require.NoError(t, err)
	return msg, res
}

func (f *choicesFixture) answer(t *testing.T, u *store.User, rowID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, u, http.MethodPost, "/api/v1/decisions/"+rowID+"/answer", body)
}

// ---------------------------------------------------------------------------
// Send path
// ---------------------------------------------------------------------------

// Acceptance 1: with the experiment off a send with choices fails naming
// it, and nothing is stored.
func TestChoicesSend_ExperimentOffRefused(t *testing.T) {
	f := newChoicesFixture(t)
	setChoicesExperiment(t, f.srv, false)
	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, twoDecisions)})
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), "web.chat_choices")
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows.Items)
}

// Acceptance 2: a prompt with an agent key and a hub-numbered decision to a
// user DM returns the message id and key/version per decision, stores the
// prompt with its fallback and decisions, and its chat event carries them.
func TestChoicesSend_UserDM(t *testing.T) {
	f := newChoicesFixture(t)
	events, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.dm")
	defer unsub()

	resp := f.sendPromptToBob(t, twoDecisions, false)
	require.NotEmpty(t, resp.MessageID)
	require.Len(t, resp.Decisions, 2)
	assert.Equal(t, "D234", resp.Decisions[0].Key)
	assert.Equal(t, 1, resp.Decisions[0].Version)
	assert.Equal(t, f.agent.ID+"/D234", resp.Decisions[0].Ref)
	assert.Nil(t, resp.Decisions[0].Replaced)
	assert.Equal(t, "1", resp.Decisions[1].Key, "the first hub number of this agent")

	ctx := context.Background()
	msg, err := f.store.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoicePrompt, msg.Type)
	assert.True(t, strings.HasPrefix(msg.Msg, "Two calls\n\n"))
	assert.Contains(t, msg.Msg, "**2 decisions needed**")
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	assert.Equal(t, dmKey, msg.ThreadID)

	rows, err := f.store.GetDecisionsByMessageIDs(ctx, []string{resp.MessageID})
	require.NoError(t, err)
	require.Len(t, rows[resp.MessageID], 2)

	var found bool
	for _, e := range collectEvents(events) {
		var evt UserMessageEvent
		require.NoError(t, json.Unmarshal(e.Data, &evt))
		if evt.ID == resp.MessageID {
			found = true
			require.Len(t, evt.Decisions, 2)
			assert.Equal(t, "Two calls", evt.ChoiceIntro)
			assert.Equal(t, "pending", evt.Decisions[0].Status)
		}
	}
	assert.True(t, found, "the prompt's chat event carries its decisions")

	// A second prompt without an id gets the next number.
	resp2 := f.sendPromptToBob(t, `[{"question":"Again?","choices":["y","n"]}]`, false)
	assert.Equal(t, "2", resp2.Decisions[0].Key)
}

// Acceptance 3 (send-side fields): agent recipients, --update without
// choices, attachments and invalid decisions are refused field by field.
func TestChoicesSend_Refusals(t *testing.T) {
	f := newChoicesFixture(t)
	cases := []struct {
		name string
		req  OutboundMessageRequest
		code string
	}{
		{"agent recipient", OutboundMessageRequest{Recipient: "agent:other", Choices: decisionInputs(t, twoDecisions)}, ErrCodeValidationError},
		{"broadcast recipient", OutboundMessageRequest{Recipient: "broadcast", Choices: decisionInputs(t, twoDecisions)}, ErrCodeValidationError},
		{"update without choices", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Msg: "x", ChoicesUpdate: true}, ErrCodeValidationError},
		{"attachments", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, twoDecisions), Attachments: []string{"/workspace/a"}}, ErrCodeValidationError},
		{"one choice", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, `[{"question":"q","choices":["only"]}]`)}, ErrCodeValidationError},
		{"reserved field", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, `[{"question":"q","choices":["a","b"],"multiple":true}]`)}, ErrCodeValidationError},
		{"never issued number", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, `[{"id":"99","question":"q","choices":["a","b"]}]`)}, ErrCodeDecisionKeyNotIssue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.sendAsAgent(t, tc.req)
			require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
			assert.Equal(t, tc.code, errorCode(t, rr))
		})
	}
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no refused prompt is stored")
}

// Acceptance 7 (send side): reusing a key without --update is refused as a
// whole naming each key, state and the hint; with --update it creates v2.
func TestChoicesSend_KeyExistsAndUpdate(t *testing.T) {
	f := newChoicesFixture(t)
	first := f.sendPromptToBob(t, twoDecisions, false)

	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, twoDecisions)})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeChoiceKeyExists, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), "D234 already exists (v1, pending)")
	assert.Contains(t, rr.Body.String(), "--update")

	upd := f.sendPromptToBob(t, `[{"id":"D234","question":"Ship it now?","choices":["yes","no"]}]`, true)
	require.Len(t, upd.Decisions, 1)
	assert.Equal(t, 2, upd.Decisions[0].Version)
	require.NotNil(t, upd.Decisions[0].Replaced)
	assert.Equal(t, 1, upd.Decisions[0].Replaced.Version)
	assert.Equal(t, store.DecisionStatusPending, upd.Decisions[0].Replaced.Status)

	old, err := f.store.GetDecision(context.Background(), first.Decisions[0].RowID)
	require.NoError(t, err)
	assert.Equal(t, store.DecisionStatusReplaced, old.Status)
}

// Choice prompts count against the outbound message rate limit.
func TestChoicesSend_CountsAgainstRateLimit(t *testing.T) {
	f := newChoicesFixture(t)
	f.srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{chatSenderHuman: 100, chatSenderAgent: 1}, nil)
	f.sendPromptToBob(t, `[{"question":"First?","choices":["y","n"]}]`, false)
	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, `[{"question":"Second?","choices":["y","n"]}]`)})
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Hub-minted types on every client path (acceptance 9)
// ---------------------------------------------------------------------------

func TestHubMintedTypes_RefusedOnClientPaths(t *testing.T) {
	for _, typ := range []string{messages.TypeChoicePrompt, messages.TypeChoiceResponse} {
		t.Run(typ, func(t *testing.T) {
			f := newChoicesFixture(t)

			t.Run("outbound-message", func(t *testing.T) {
				rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Msg: "x", Type: typ})
				require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				assert.Equal(t, ErrCodeHubMintedType, errorCode(t, rr))
			})

			t.Run("agent message", func(t *testing.T) {
				body, _ := json.Marshal(MessageRequest{StructuredMessage: &messages.StructuredMessage{
					Sender: "user:" + f.bob.Email, Recipient: "agent:" + f.agent.Slug, Msg: "x", Type: typ,
				}})
				r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/message", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r = r.WithContext(contextWithIdentity(r.Context(), userIdentityOf(f.bob)))
				rr := httptest.NewRecorder()
				f.srv.handleAgentMessage(rr, r, f.agent.ID)
				require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				assert.Equal(t, ErrCodeHubMintedType, errorCode(t, rr))
			})

			t.Run("broadcast", func(t *testing.T) {
				body, _ := json.Marshal(map[string]interface{}{"structured_message": &messages.StructuredMessage{
					Sender: "user:" + f.alice.Email, Msg: "x", Type: typ,
				}})
				r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project.ID+"/broadcast", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r = r.WithContext(contextWithIdentity(r.Context(), userIdentityOf(f.alice)))
				rr := httptest.NewRecorder()
				f.srv.handleProjectBroadcast(rr, r, f.project.ID)
				require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				assert.Equal(t, ErrCodeHubMintedType, errorCode(t, rr))
			})

			t.Run("broker inbound", func(t *testing.T) {
				body, _ := json.Marshal(inboundMessageRequest{
					Topic: "scion.project." + f.project.ID + ".agent." + f.agent.Slug + ".messages",
					Message: &messages.StructuredMessage{
						Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
						Channel: "discord", Sender: "user:" + f.bob.Email, Recipient: "agent:" + f.agent.Slug,
						Msg: "x", Type: typ,
					},
				})
				r := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r = r.WithContext(contextWithBrokerIdentity(r.Context(), NewBrokerIdentity("test-broker")))
				rr := httptest.NewRecorder()
				f.srv.mux.ServeHTTP(rr, r)
				require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
				assert.Equal(t, ErrCodeHubMintedType, errorCode(t, rr))
			})

			assert.Empty(t, f.dispatcher.getMessages(), "nothing reached the agent")
		})
	}
}

// ---------------------------------------------------------------------------
// Answer and get endpoints
// ---------------------------------------------------------------------------

// Acceptance 1: both decision routes are 404 while the experiment is off.
func TestDecisionRoutes_ExperimentOff404(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	row := res.Decisions[0].RowID
	setChoicesExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+row, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "yes"}).Code)
}

func TestDecisionGet_ReadAccess(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	row := res.Decisions[0].RowID

	rr := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+row, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var got struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "D234", got.Decision.Key)
	assert.Equal(t, store.DecisionStatusPending, got.Decision.Status)
	assert.Equal(t, "1", got.Decision.DefaultChoiceID)

	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.outsider, http.MethodGet, "/api/v1/decisions/"+row, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+tid("nope"), nil).Code)
}

// Acceptance 8: a user without send rights gets 404, an agent token 403;
// in a DM only the addressee may answer.
func TestDecisionAnswer_Authorization(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	row := res.Decisions[0].RowID

	assert.Equal(t, http.StatusNotFound, f.answer(t, f.outsider, row, map[string]interface{}{"choice_id": "1"}).Code)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/decisions/"+row+"/answer", strings.NewReader(`{"choice_id":"1"}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.handleDecisionRoutes(rr, r)
	assert.Equal(t, http.StatusForbidden, rr.Code, "an agent never answers directly")

	dm := f.sendPromptToBob(t, `[{"id":"dm-key","question":"DM?","choices":["y","n"]}]`, false)
	dmRow := dm.Decisions[0].RowID
	assert.Equal(t, http.StatusNotFound, f.answer(t, f.alice, dmRow, map[string]interface{}{"choice_id": "1"}).Code,
		"a user outside the DM is not its addressee")
	rr = f.answer(t, f.bob, dmRow, map[string]interface{}{"choice_id": "1"})
	assert.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
}

// Acceptance 6 and 10: the answer copies the label from the stored row
// (a tampered label is ignored), records the snapshots, stores a
// choice-response from the clicker and delivers the hub line with the
// choice metadata to the asker.
func TestDecisionAnswer_RecordsAndDelivers(t *testing.T) {
	f := newChoicesFixture(t)
	resp := f.sendPromptToBob(t, twoDecisions, false)
	row := resp.Decisions[0].RowID
	before := len(f.dispatcher.getMessages())

	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1", "label": "HACKED", "choice_label": "HACKED"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var ans DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ans))
	require.NotNil(t, ans.Decision.Decision)
	assert.Equal(t, "yes", ans.Decision.Decision.Label)
	assert.Equal(t, store.DecisionStatusDecided, ans.Decision.Status)

	ctx := context.Background()
	d, err := f.store.GetDecision(ctx, row)
	require.NoError(t, err)
	a := d.EffectiveAnswer()
	require.NotNil(t, a)
	assert.Equal(t, "yes", a.ChoiceLabel)
	assert.Equal(t, "Ship the **feature** now?", a.QuestionText)
	assert.Equal(t, f.bob.ID, a.AnsweredByUserID)
	assert.Equal(t, f.bob.Email, a.AnsweredByEmail)
	assert.Equal(t, store.DecisionProvenanceDirect, a.Provenance)
	assert.Equal(t, store.DecisionSurfaceNative, a.Surface)
	assert.Equal(t, ans.ResponseMessageID, a.ResponseMessageID)

	m, err := f.store.GetMessage(ctx, a.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
	assert.Equal(t, f.bob.ID, m.SenderID)
	assert.NotContains(t, m.Msg, "HACKED")

	sent := f.dispatcher.getMessages()[before:]
	require.Len(t, sent, 1)
	sm := sent[0].structured
	require.NotNil(t, sm)
	assert.Equal(t, messages.TypeChoiceResponse, sm.Type)
	assert.True(t, strings.HasPrefix(sm.Msg, `[choice-response] D234 v1 ("Ship the **feature** now?") answer=yes (choice 1) by `+f.bob.Email), sm.Msg)
	assert.Equal(t, "D234", sm.Metadata[messages.MetadataChoiceKey])
	assert.Equal(t, "1", sm.Metadata[messages.MetadataChoiceVersion])
	assert.Equal(t, "1", sm.Metadata[messages.MetadataChoiceID])
	assert.Equal(t, store.DecisionProvenanceDirect, sm.Metadata[messages.MetadataChoiceProvenance])

	// Acceptance 8: an answered version is 409 with the effective answer.
	rr = f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "2"})
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, ErrCodeDecisionAnswered, errorCode(t, rr))

	// An unknown choice id is refused.
	rr = f.answer(t, f.bob, resp.Decisions[1].RowID, map[string]interface{}{"choice_id": "zzz"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// Acceptance 8: replaying an idempotency key returns 200 and stores and
// delivers nothing new.
func TestDecisionAnswer_IdempotentReplay(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	row := res.Decisions[0].RowID
	body := map[string]interface{}{"choice_id": "2", "idempotency_key": "click-1"}

	first := f.answer(t, f.bob, row, body)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	sent := len(f.dispatcher.getMessages())

	again := f.answer(t, f.bob, row, body)
	require.Equal(t, http.StatusOK, again.Code, "body: %s", again.Body.String())
	var a1, a2 DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &a1))
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &a2))
	assert.True(t, a2.Replayed)
	assert.Equal(t, a1.ResponseMessageID, a2.ResponseMessageID)
	assert.Equal(t, sent, len(f.dispatcher.getMessages()), "a replay delivers nothing")
}

// Acceptance 8: two concurrent answers produce one decision; the loser
// gets 409.
func TestDecisionAnswer_ConcurrentOneWinner(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, twoDecisions, false)
	row := res.Decisions[0].RowID

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, u := range []*store.User{f.alice, f.bob} {
		wg.Add(1)
		go func(i int, u *store.User) {
			defer wg.Done()
			codes[i] = f.answer(t, u, row, map[string]interface{}{"choice_id": []string{"1", "2"}[i]}).Code
		}(i, u)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	assert.Equal(t, 1, ok, "codes %v", codes)
	assert.Equal(t, 1, conflict, "codes %v", codes)
	d, err := f.store.GetDecision(context.Background(), row)
	require.NoError(t, err)
	assert.Len(t, d.Answers, 1)
}

// Acceptance 7: answering a replaced version is 409 replaced with the newer
// version's view.
func TestDecisionAnswer_Replaced409(t *testing.T) {
	f := newChoicesFixture(t)
	_, v1 := f.topicPrompt(t, twoDecisions, false)
	_, v2 := f.topicPrompt(t, `[{"id":"D234","question":"Ship it?","choices":["yes","no"]}]`, true)
	rr := f.answer(t, f.bob, v1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionReplaced, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), v2.Decisions[0].RowID)
}

// The answer publishes choice.updated on the project subject for a thread.
func TestDecisionAnswer_PublishesChoiceUpdated(t *testing.T) {
	f := newChoicesFixture(t)
	events, unsub := f.srv.events.Subscribe("project." + f.project.ID + ".choice.updated")
	defer unsub()
	_, res := f.topicPrompt(t, twoDecisions, false)
	rr := f.answer(t, f.bob, res.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	evts := collectEvents(events)
	require.Len(t, evts, 1)
	var got struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(evts[0].Data, &got))
	assert.Equal(t, store.DecisionStatusDecided, got.Decision.Status)
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// History rows for choice prompts gain decisions and the intro, and
// choice-response rows gain choiceResponse; both are omitted while the
// experiment is off.
func TestChoicesHistory_ViewsGatedByExperiment(t *testing.T) {
	f := newChoicesFixture(t)
	msg, res := f.topicPrompt(t, twoDecisions, false)
	rr := f.answer(t, f.bob, res.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var ans DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ans))

	history := func() chatHistoryResponse {
		rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var h chatHistoryResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &h))
		return h
	}

	h := history()
	require.Len(t, h.MessageDecisions[msg.ID], 2)
	assert.Equal(t, "Please decide", h.MessageChoiceIntros[msg.ID])
	cr, ok := h.MessageChoiceResponses[ans.ResponseMessageID]
	require.True(t, ok)
	assert.Equal(t, "yes", cr.Label)
	assert.Equal(t, msg.ID, cr.PromptMessageID)

	setChoicesExperiment(t, f.srv, false)
	h = history()
	assert.Nil(t, h.MessageDecisions)
	assert.Nil(t, h.MessageChoiceIntros)
	assert.Nil(t, h.MessageChoiceResponses)
}
