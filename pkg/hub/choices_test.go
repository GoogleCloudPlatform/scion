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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
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
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	return newChoicesFixtureOn(t, srv, s, alice, bob, project)
}

// newChoicesFixtureOn builds the choices fixture on a server from
// setupDemoPolicyTest or setupDemoPolicyTestWithFault.
func newChoicesFixtureOn(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, project *store.Project) *choicesFixture {
	t.Helper()
	f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
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

// oneDecision is a one-decision prompt (the first decision of twoDecisions):
// it is answered one click at a time on the per-decision endpoint. A
// prompt with two or more decisions is a set (design choices v5) and is
// answered with the set endpoint.
const oneDecision = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"],"default":"yes"}]`

const twoDecisions = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"],"default":"yes"},{"question":"Which approach?","choices":[{"id":"a","label":"Extend"},{"id":"b","label":"New type"}]}]`

// sendAsAgent posts req to the agent's outbound-message handler.
func (f *choicesFixture) sendAsAgent(t *testing.T, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return f.sendRawAsAgent(t, body)
}

// sendRawAsAgent posts a raw JSON body to the agent's outbound-message
// handler, for fields a typed request cannot carry (DecisionInput marshals
// its known fields only).
func (f *choicesFixture) sendRawAsAgent(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, r, f.agent.ID)
	return rr
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
	conv, err := f.store.GetConversationByExternalRef(context.Background(), "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	msg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
		Recipient: "user:" + f.alice.Email, RecipientID: f.alice.ID, ConversationID: conv.ID,
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
	assert.Contains(t, msg.Msg, "**2 decisions needed. Answer all of them together.**")
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
		{"never issued number", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Choices: decisionInputs(t, `[{"id":"99","question":"q","choices":["a","b"]}]`)}, ErrCodeDecisionKeyNotIssue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.sendAsAgent(t, tc.req)
			require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
			assert.Equal(t, tc.code, errorCode(t, rr))
		})
	}
	t.Run("reserved field", func(t *testing.T) {
		// Raw JSON: a typed request drops the reserved field.
		rr := f.sendRawAsAgent(t, []byte(`{"recipient":"user:`+f.bob.Email+`","choices":[{"question":"q","choices":["a","b"],"multiple":true}]}`))
		require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
		assert.Equal(t, ErrCodeValidationError, errorCode(t, rr))
	})
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
	_, res := f.topicPrompt(t, oneDecision, false)
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
	_, res := f.topicPrompt(t, oneDecision, false)
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
	resp := f.sendPromptToBob(t, oneDecision, false)
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

	// An unknown choice id is refused (on a second one-decision prompt).
	second := f.sendPromptToBob(t, `[{"id":"D235","question":"Second?","choices":["yes","no"]}]`, false)
	rr = f.answer(t, f.bob, second.Decisions[0].RowID, map[string]interface{}{"choice_id": "zzz"})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// Acceptance 8: replaying an idempotency key returns 200 and stores and
// delivers nothing new.
func TestDecisionAnswer_IdempotentReplay(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
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
	_, res := f.topicPrompt(t, oneDecision, false)
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
	_, v1 := f.topicPrompt(t, oneDecision, false)
	_, v2 := f.topicPrompt(t, `[{"id":"D234","question":"Ship it?","choices":["yes","no"]}]`, true)
	rr := f.answer(t, f.bob, v1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionReplaced, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), v2.Decisions[0].RowID)
}

// The answer publishes choice.updated on the project subject for a thread.
func TestDecisionAnswer_PublishesChoiceUpdated(t *testing.T) {
	f := newChoicesFixture(t)
	events, unsub := f.srv.events.Subscribe("project." + f.project.ID + ".chat.choice.updated")
	defer unsub()
	_, res := f.topicPrompt(t, oneDecision, false)
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
	msg, _ := f.topicPrompt(t, twoDecisions, false)
	single, res := f.topicPrompt(t, `[{"id":"D300","question":"Ship the **feature** now?","choices":["yes","no"],"default":"yes"}]`, false)
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
	assert.Equal(t, single.ID, cr.PromptMessageID)

	setChoicesExperiment(t, f.srv, false)
	h = history()
	assert.Nil(t, h.MessageDecisions)
	assert.Nil(t, h.MessageChoiceIntros)
	assert.Nil(t, h.MessageChoiceResponses)
}

// ---------------------------------------------------------------------------
// Review round 1
// ---------------------------------------------------------------------------

// A choice prompt into a group conversation is addressed to humans: it
// wakes no default agent and fans out no agent mention. The same posts
// without choices (the controls) do both.
func TestChoicesSend_GroupPostWakesNoAgent(t *testing.T) {
	f := newGroupWakeFixture(t)
	setChoicesExperiment(t, f.srv, true)
	f.setDefault(t, f.target.ID)

	f.post(t, "status update for the group", "")
	require.Len(t, dispatchesTo(f.dispatcher, f.target.ID), 1, "control: a plain post wakes the default agent")
	f.post(t, "@"+mentionBystanderSlug+" please look", "")
	require.Len(t, dispatchesTo(f.dispatcher, f.bystander.ID), 1, "control: a plain post fans out its mention")
	before := len(f.dispatcher.getCalls())

	for _, msg := range []string{"Please decide", "@" + mentionBystanderSlug + " please decide"} {
		resp := f.postRequest(t, OutboundMessageRequest{
			Msg: msg, ConversationRef: "conv:" + f.convID,
			Choices: decisionInputs(t, `[{"question":"Ship?","choices":["y","n"]}]`),
		})
		assert.Nil(t, resp.DefaultAgentResult, msg)
		assert.Empty(t, resp.MentionResults, msg)
		assert.Empty(t, resp.WokenAgents, msg)
	}
	assert.Len(t, f.dispatcher.getCalls(), before, "a choice prompt dispatches to no agent")
}

// A DM answer publishes choice.updated to the DM's user only, never on the
// project subject (a thread answer does, TestDecisionAnswer_PublishesChoiceUpdated).
func TestDecisionAnswer_DMChoiceUpdatedOnlyToUser(t *testing.T) {
	f := newChoicesFixture(t)
	projEvents, unsubProject := f.srv.events.Subscribe("project." + f.project.ID + ".chat.choice.updated")
	defer unsubProject()
	userEvents, unsubUser := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.choice.updated")
	defer unsubUser()

	dm := f.sendPromptToBob(t, `[{"id":"dm-upd","question":"DM?","choices":["y","n"]}]`, false)
	rr := f.answer(t, f.bob, dm.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	assert.Len(t, collectEvents(userEvents), 1, "the DM's user gets choice.updated")
	assert.Empty(t, collectEvents(projEvents), "a DM answer is never published on the project subject")
}

// The asker is gone: the answer still stands (200), its response row is
// stored as failed, and nothing is dispatched.
func TestDecisionAnswer_AskerGoneStoresFailedResponse(t *testing.T) {
	f := newChoicesFixture(t)
	ctx := context.Background()
	_, res := f.topicPrompt(t, oneDecision, false)
	require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))
	before := len(f.dispatcher.getMessages())

	rr := f.answer(t, f.bob, res.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var ans DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ans))
	assert.Equal(t, store.DecisionStatusDecided, ans.Decision.Status)

	m, err := f.store.GetMessage(ctx, ans.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoiceResponse, m.Type)
	assert.Equal(t, store.MessageDispatchFailed, m.DispatchState)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing is dispatched to a gone asker")
}

// A user access token, a federated user and a visitor get 403 on both
// decision routes; answers come from a web session only. The token's and
// the federated user's refusal is the session-only one the catalog
// records (reason INTERACTIVE_STATE, credential session_required); the
// bearer disposition matrix cannot reach it while web.chat_choices is
// off, so this test pins it. The refusal does not depend on the token's
// selectors, which is why a project:read token covers the matrix rows.
func TestDecisionRoutes_TokenAndVisitorForbidden(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID

	uat := NewScopedUserIdentity(authUser(f.bob), f.project.ID, []string{"project:read"})
	visitor := NewVisitorIdentity(tid("choices-visitor"), "https://issuer.choices.test", "choices-visitor", "visitor@choices.test", "Visitor")
	federated := NewFederatedUserIdentity("https://issuer.choices.test", "choices-fed", f.bob.Email, "Fed Bob", "member", nil)
	for name, id := range map[string]Identity{"user access token": uat, "federated user": federated, "visitor": visitor} {
		answer := requestAsIdentity(t, f.srv, id, http.MethodPost, "/api/v1/decisions/"+row+"/answer", map[string]interface{}{"choice_id": "1"})
		get := requestAsIdentity(t, f.srv, id, http.MethodGet, "/api/v1/decisions/"+row, nil)
		for route, rec := range map[string]*httptest.ResponseRecorder{"answer": answer, "get": get} {
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", name, route, rec.Body.String())
			_, details := errorCodeAndDetails(t, rec)
			if name != "visitor" {
				assert.Equal(t, string(authzop.ReasonInteractiveState), details["reason"], "%s %s", name, route)
				assert.Equal(t, "session_required", details["credential"], "%s %s", name, route)
			} else {
				assert.Nil(t, details["credential"], "%s %s: a visitor gets the plain 403", name, route)
			}
		}
	}
	d, err := f.store.GetDecision(context.Background(), row)
	require.NoError(t, err)
	assert.Empty(t, d.Answers)
	// Control: bob's session answers.
	assert.Equal(t, http.StatusOK, f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1"}).Code)
}

// An idempotency key replays only its own answer: the same key with
// another choice, or from another user, is 409 at the hub.
func TestDecisionAnswer_IdempotencyKeyOtherAnswer409(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	first := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1", "idempotency_key": "k1"})
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "2", "idempotency_key": "k1"})
	assert.Equal(t, http.StatusConflict, rr.Code, "another choice: %s", rr.Body.String())
	rr = f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "1", "idempotency_key": "k1"})
	assert.Equal(t, http.StatusConflict, rr.Code, "another user: %s", rr.Body.String())

	// Control: the true replay.
	rr = f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1", "idempotency_key": "k1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var a DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &a))
	assert.True(t, a.Replayed)
}

// A project member who may read but not post (a reader) can read a
// decision but not answer it.
func TestDecisionAnswer_ReaderForbidden(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	f.makeChatReader(t, f.bob)

	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1"})
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rr.Code, "body: %s", rr.Body.String())
	d, err := f.store.GetDecision(context.Background(), row)
	require.NoError(t, err)
	assert.Empty(t, d.Answers)
	// Control: reading is enough to get it.
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+row, nil).Code)
}

// recordInterpreted stores an agent_interpreted answer of u to row, as an
// agent recording an answer it read elsewhere would.
func (f *choicesFixture) recordInterpreted(t *testing.T, u *store.User, row, choiceID string) {
	t.Helper()
	_, _, err := f.store.RecordAnswer(context.Background(), &store.RecordAnswerInput{
		DecisionRowID: row, ChoiceID: choiceID,
		AnsweredByUserID: u.ID, AnsweredByName: u.DisplayName, AnsweredByEmail: u.Email,
		Provenance: store.DecisionProvenanceAgentInterpreted, InterpretedByAgentID: f.agent.ID,
		SourceMessageID: api.NewUUID(), Surface: store.DecisionSurfaceSlack,
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "user:" + u.Email, RecipientID: u.ID,
			AgentID: f.agent.ID, Channel: "web", ThreadID: f.topicID, Msg: "interpreted", CreatedAt: time.Now().UTC(),
			DispatchState: store.MessageDispatchDispatched,
		},
	})
	require.NoError(t, err)
}

// A fresh idempotency key skips none of the state checks: a third user's
// fresh key against an interpreted answer is 403, a fresh key on a
// replaced or directly answered version is 409. A true replay still
// answers 200.
func TestDecisionAnswer_IdempotencyKeyKeepsChecks(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	f.recordInterpreted(t, f.alice, row, "1")

	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "2", "idempotency_key": "bob-fresh"})
	require.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeNotRecordedAnswerer, errorCode(t, rr))

	// The recorded answerer confirms with a key; the true replay is 200.
	rr = f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "1", "idempotency_key": "alice-1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	rr = f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "1", "idempotency_key": "alice-1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var replay DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &replay))
	assert.True(t, replay.Replayed)

	// Directly answered now: a fresh key is 409 answered.
	rr = f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "2", "idempotency_key": "alice-2"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionAnswered, errorCode(t, rr))

	// Replaced: a fresh key is 409 replaced.
	_, otherRes := f.topicPrompt(t, `[{"id":"Q1","question":"Which?","choices":["a","b"]}]`, false)
	other := otherRes.Decisions[0].RowID
	f.topicPrompt(t, `[{"id":"Q1","question":"Which, again?","choices":["a","b"]}]`, true)
	rr = f.answer(t, f.bob, other, map[string]interface{}{"choice_id": "1", "idempotency_key": "bob-replaced"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionReplaced, errorCode(t, rr))

	d, err := f.store.GetDecision(context.Background(), row)
	require.NoError(t, err)
	assert.Len(t, d.Answers, 2, "the interpreted answer and alice's confirm only")
}

// The send response names who decided a replaced version (design 4.2:
// "replaced D1 v1 (decided by <who>: <label>)").
func TestChoicesSend_ReplacedNamesWhoDecided(t *testing.T) {
	f := newChoicesFixture(t)
	first := f.sendPromptToBob(t, `[{"id":"D7","question":"Ship?","choices":["yes","no"]}]`, false)
	rr := f.answer(t, f.bob, first.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	upd := f.sendPromptToBob(t, `[{"id":"D7","question":"Ship now?","choices":["yes","no"]}]`, true)
	require.NotNil(t, upd.Decisions[0].Replaced)
	r := upd.Decisions[0].Replaced
	assert.Equal(t, store.DecisionStatusDecided, r.Status)
	assert.Equal(t, "yes", r.Answer)
	want := f.bob.DisplayName
	if want == "" {
		want = f.bob.Email
	}
	assert.Equal(t, want, r.AnsweredBy)
}

// The intro is the body after the hub's agent-mention rewrite, like every
// stored body: "@agent:<slug>" is stored as "@<slug>".
func TestChoicesSend_IntroGetsMentionRewrite(t *testing.T) {
	f := newChoicesFixture(t)
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email, Msg: "Ask @agent:helper-bot first",
		Choices: decisionInputs(t, `[{"question":"Ship?","choices":["y","n"]}]`),
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp choiceSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	msg, err := f.store.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	assert.Contains(t, msg.Msg, "helper-bot")
	assert.NotContains(t, msg.Msg, "agent:helper-bot", "the intro was stored before the rewrite")
}

// interpretedFirstStore records an agent_interpreted answer just before
// the next RecordAnswer once armed, as one landing between the answer
// endpoint's state check and its write would. It is installed with
// setupDemoPolicyTestWithFault, before the fixture's audited setup.
type interpretedFirstStore struct {
	store.Store
	fault  *storeFaultSwitch
	before func()
}

func (s *interpretedFirstStore) RecordAnswer(ctx context.Context, in *store.RecordAnswerInput) (*store.DecisionAnswer, bool, error) {
	if b := s.before; b != nil && s.fault.Active() {
		s.before = nil
		b()
	}
	return s.Store.RecordAnswer(ctx, in)
}

// An interpreted answer by the same user recorded after the endpoint read
// the decision makes the store refuse the direct answer's outcome; the
// user gets 409 with the current decision, not a validation error.
func TestDecisionAnswer_InterpretedAfterStateCheck409(t *testing.T) {
	srv, st, alice, bob, project, first, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *interpretedFirstStore {
		return &interpretedFirstStore{Store: inner, fault: f}
	})
	f := newChoicesFixtureOn(t, srv, st, alice, bob, project)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID

	first.before = func() { f.recordInterpreted(t, f.alice, row, "1") }
	fault.Arm()

	rr := f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	code, details := errorCodeAndDetails(t, rr)
	assert.Equal(t, ErrCodeDecisionAnswered, code)
	view, ok := details["decision"].(map[string]interface{})
	require.True(t, ok, "the 409 carries the current decision: %s", rr.Body.String())
	assert.Equal(t, store.DecisionStatusDecided, view["status"])
	eff, ok := view["decision"].(map[string]interface{})
	require.True(t, ok, "the current decision has the interpreted answer")
	assert.Equal(t, "1", eff["choiceId"])

	d, err := f.store.GetDecision(context.Background(), row)
	require.NoError(t, err)
	require.Len(t, d.Answers, 1, "only the interpreted answer is stored")
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, d.Answers[0].Provenance)

	// Control: the same answer now, as a confirm, is recorded.
	rr = f.answer(t, f.alice, row, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
}

// An answer is a message from the user to the asking agent: when the
// agent takes no messages from the user, the answer is refused with
// the chat send's MESSAGE_DENIED before anything is recorded.
func TestDecisionAnswer_AskerRefusesMessages(t *testing.T) {
	f := newChoicesFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	ctx := context.Background()

	agent, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	mode := agent.MessageMode
	agent.MessageMode = store.MessageModeNone
	require.NoError(t, f.store.UpdateAgent(ctx, agent))

	rr := f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeMessageDenied, errorCode(t, rr))
	d, err := f.store.GetDecision(ctx, row)
	require.NoError(t, err)
	assert.Empty(t, d.Answers, "a refused answer stores nothing")
	assert.Equal(t, store.DecisionStatusPending, d.Status)

	// Control: with the agent taking messages again, the same answer is recorded.
	agent, err = f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	agent.MessageMode = mode
	require.NoError(t, f.store.UpdateAgent(ctx, agent))
	rr = f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
}

// A choice prompt may leave out its intro: the send is accepted and the
// stored body is the markdown fallback rendered from the decisions.
func TestChoicesSend_WithoutIntro(t *testing.T) {
	f := newChoicesFixture(t)
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email,
		Choices:   decisionInputs(t, `[{"id":"ship","question":"Ship it now?","choices":["yes","no"]}]`),
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp choiceSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID)
	require.Len(t, resp.Decisions, 1)

	m, err := f.store.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeChoicePrompt, m.Type)
	assert.Contains(t, m.Msg, "Ship it now?", "the stored body is the rendered fallback")
	decs, err := f.store.GetDecisionsByMessageIDs(context.Background(), []string{resp.MessageID})
	require.NoError(t, err)
	require.Len(t, decs[resp.MessageID], 1)
	assert.Equal(t, 0, decs[resp.MessageID][0].IntroLength, "no intro")
}
