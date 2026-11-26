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

// Decision prompts (design choices v4): the send path on outbound-message,
// the hub-minted type refusal on every client path, the decision answer and
// get endpoints, delivery of the decision-response, and the history views.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// decisionsFixture is the scheduled-send fixture (alice owns the project, bob
// is a member and owns the running agent, a topic whose default agent it is)
// with web.decisions on, plus an outsider with no project access.
type decisionsFixture struct {
	*scheduledSendFixture
	outsider *store.User
}

func newDecisionsFixture(t *testing.T) *decisionsFixture {
	t.Helper()
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	return newDecisionsFixtureOn(t, srv, s, alice, bob, project)
}

// newDecisionsFixtureOn builds the choices fixture on a server from
// setupDemoPolicyTest or setupDemoPolicyTestWithFault.
func newDecisionsFixtureOn(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, project *store.Project) *decisionsFixture {
	t.Helper()
	f := newScheduledSendFixtureOn(t, srv, s, alice, bob, project)
	setDecisionsExperiment(t, f.srv, true)
	ctx := context.Background()
	outsider := &store.User{
		ID: tid("choices-outsider"), Email: "outsider-choices@example.com", DisplayName: "Out Sider",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, f.store, outsider.ID)
	return &decisionsFixture{scheduledSendFixture: f, outsider: outsider}
}

func setDecisionsExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{experiments.Decisions: enabled})
	require.Equal(t, enabled, srv.experimentEnabled(experiments.Decisions))
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
func (f *decisionsFixture) sendAsAgent(t *testing.T, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return f.sendRawAsAgent(t, body)
}

// sendRawAsAgent posts a raw JSON body to the agent's outbound-message
// handler, for fields a typed request cannot carry (DecisionInput marshals
// its known fields only).
func (f *decisionsFixture) sendRawAsAgent(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentCtx(r.Context(), f.agent))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, r, f.agent.ID)
	return rr
}

type decisionSendResponse struct {
	MessageID string               `json:"message_id"`
	Decisions []DecisionSendResult `json:"decisions"`
}

// sendPromptToBob sends a decision prompt from the agent to bob's DM.
func (f *decisionsFixture) sendPromptToBob(t *testing.T, raw string, update bool) decisionSendResponse {
	t.Helper()
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email, Msg: "Two calls", Decisions: decisionInputs(t, raw), DecisionsUpdate: update,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp decisionSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

// topicPrompt stores a decision prompt in the fixture's topic directly.
func (f *decisionsFixture) topicPrompt(t *testing.T, raw string, update bool) (*store.Message, *store.DecisionPromptResult) {
	t.Helper()
	decs, err := messages.NormalizeDecisions("decisions", decisionInputs(t, raw))
	require.NoError(t, err)
	conv, err := f.store.GetConversationByExternalRef(context.Background(), "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	msg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
		Recipient: "user:" + f.alice.Email, RecipientID: f.alice.ID, ConversationID: conv.ID,
		AgentID: f.agent.ID, Channel: "web", ThreadID: f.topicID, CreatedAt: time.Now().UTC(),
		DispatchState: store.MessageDispatchDispatched,
	}
	res, err := f.store.CreateDecisionPrompt(context.Background(), &store.DecisionPromptInput{
		Message: msg, Intro: "Please decide", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name,
		Decisions: decs, Update: update,
	})
	require.NoError(t, err)
	return msg, res
}

func (f *decisionsFixture) answer(t *testing.T, u *store.User, rowID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, u, http.MethodPost, "/api/v1/decisions/"+rowID+"/answer", body)
}

// ---------------------------------------------------------------------------
// Send path
// ---------------------------------------------------------------------------

// Acceptance 1: with the experiment off a send with decisions fails naming
// it, and nothing is stored.
func TestDecisionsSend_ExperimentOffRefused(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiment(t, f.srv, false)
	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, twoDecisions)})
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeExperimentDisabled, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), "web.decisions")
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows.Items)
}

// Acceptance 2: a prompt with an agent key and a hub-numbered decision to a
// user DM returns the message id and key/version per decision, stores the
// prompt with its fallback and decisions, and its chat event carries them.
func TestDecisionsSend_UserDM(t *testing.T) {
	f := newDecisionsFixture(t)
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
	assert.Equal(t, messages.TypeDecisionPrompt, msg.Type)
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
			assert.Equal(t, "Two calls", evt.DecisionIntro)
			assert.Equal(t, "pending", evt.Decisions[0].Status)
		}
	}
	assert.True(t, found, "the prompt's chat event carries its decisions")

	// A second prompt without an id gets the next number.
	resp2 := f.sendPromptToBob(t, `[{"question":"Again?","choices":["y","n"]}]`, false)
	assert.Equal(t, "2", resp2.Decisions[0].Key)
}

// Acceptance 3 (send-side fields): agent recipients, --update without
// decisions, attachments and invalid decisions are refused field by field.
func TestDecisionsSend_Refusals(t *testing.T) {
	f := newDecisionsFixture(t)
	cases := []struct {
		name string
		req  OutboundMessageRequest
		code string
	}{
		{"agent recipient", OutboundMessageRequest{Recipient: "agent:other", Decisions: decisionInputs(t, twoDecisions)}, ErrCodeValidationError},
		{"broadcast recipient", OutboundMessageRequest{Recipient: "broadcast", Decisions: decisionInputs(t, twoDecisions)}, ErrCodeValidationError},
		{"update without decisions", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Msg: "x", DecisionsUpdate: true}, ErrCodeValidationError},
		{"attachments", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, twoDecisions), Attachments: []string{"/workspace/a"}}, ErrCodeValidationError},
		{"no choices", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, `[{"question":"q","choices":[]}]`)}, ErrCodeValidationError},
		{"never issued number", OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, `[{"id":"99","question":"q","choices":["a","b"]}]`)}, ErrCodeDecisionKeyNotIssue},
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
		rr := f.sendRawAsAgent(t, []byte(`{"recipient":"user:`+f.bob.Email+`","decisions":[{"question":"q","choices":["a","b"],"multiple":true}]}`))
		require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
		assert.Equal(t, ErrCodeValidationError, errorCode(t, rr))
		assert.Contains(t, rr.Body.String(), "decisions[0].multiple")
	})
	rows, err := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "no refused prompt is stored")
}

// Single-choice confirmation decision (ptone/scion#8786): 1 choice without
// allow_other is accepted, stored, and answered with one click.
func TestDecisionsSend_SingleChoice(t *testing.T) {
	f := newDecisionsFixture(t)
	resp := f.sendPromptToBob(t, `[{"id":"confirm-deploy","question":"Approve production deploy?","choices":[{"id":"approve","label":"Approved"}]}]`, false)
	require.Len(t, resp.Decisions, 1)
	rowID := resp.Decisions[0].RowID

	getRR := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+rowID, nil)
	require.Equal(t, http.StatusOK, getRR.Code, "body: %s", getRR.Body.String())
	var got struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &got))
	assert.Equal(t, "pending", got.Decision.Status)
	require.Len(t, got.Decision.Choices, 1)
	assert.Equal(t, "approve", got.Decision.Choices[0].ID)
	assert.Equal(t, "Approved", got.Decision.Choices[0].Label)
	assert.Nil(t, got.Decision.Other)

	ansRR := f.answer(t, f.bob, rowID, map[string]interface{}{"choice_id": "approve"})
	require.Equal(t, http.StatusOK, ansRR.Code, "body: %s", ansRR.Body.String())
	var ansResp struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(ansRR.Body.Bytes(), &ansResp))
	assert.Equal(t, "decided", ansResp.Decision.Status)
	require.NotNil(t, ansResp.Decision.Decision)
	assert.Equal(t, "approve", ansResp.Decision.Decision.ChoiceID)
	assert.Equal(t, "Approved", ansResp.Decision.Decision.Label)
}

// Acceptance 7 (send side): reusing a key without --update is refused as a
// whole naming each key, state and the hint; with --update it creates v2.
func TestDecisionsSend_KeyExistsAndUpdate(t *testing.T) {
	f := newDecisionsFixture(t)
	first := f.sendPromptToBob(t, twoDecisions, false)

	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, twoDecisions)})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionKeyExists, errorCode(t, rr))
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

// Decision prompts count against the outbound message rate limit.
func TestDecisionsSend_CountsAgainstRateLimit(t *testing.T) {
	f := newDecisionsFixture(t)
	f.srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{chatSenderHuman: 100, chatSenderAgent: 1}, nil)
	f.sendPromptToBob(t, `[{"question":"First?","choices":["y","n"]}]`, false)
	rr := f.sendAsAgent(t, OutboundMessageRequest{Recipient: "user:" + f.bob.Email, Decisions: decisionInputs(t, `[{"question":"Second?","choices":["y","n"]}]`)})
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Hub-minted types on every client path (acceptance 9)
// ---------------------------------------------------------------------------

func TestHubMintedTypes_RefusedOnClientPaths(t *testing.T) {
	for _, typ := range []string{messages.TypeDecisionPrompt, messages.TypeDecisionResponse} {
		t.Run(typ, func(t *testing.T) {
			f := newDecisionsFixture(t)

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
	f := newDecisionsFixture(t)
	_, res := f.topicPrompt(t, oneDecision, false)
	row := res.Decisions[0].RowID
	setDecisionsExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+row, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.answer(t, f.bob, row, map[string]interface{}{"choice_id": "yes"}).Code)
}

func TestDecisionGet_ReadAccess(t *testing.T) {
	f := newDecisionsFixture(t)
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
	f := newDecisionsFixture(t)
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
// decision-response from the clicker and delivers the hub line with the
// decision metadata to the asker.
func TestDecisionAnswer_RecordsAndDelivers(t *testing.T) {
	f := newDecisionsFixture(t)
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
	assert.Equal(t, messages.TypeDecisionResponse, m.Type)
	assert.Equal(t, f.bob.ID, m.SenderID)
	assert.NotContains(t, m.Msg, "HACKED")

	sent := f.dispatcher.getMessages()[before:]
	require.Len(t, sent, 1)
	sm := sent[0].structured
	require.NotNil(t, sm)
	assert.Equal(t, messages.TypeDecisionResponse, sm.Type)
	assert.True(t, strings.HasPrefix(sm.Msg, `[decision-response] D234 v1 ("Ship the **feature** now?") answer=yes (choice 1) by `+f.bob.Email), sm.Msg)
	assert.Equal(t, "D234", sm.Metadata[messages.MetadataDecisionKey])
	assert.Equal(t, "1", sm.Metadata[messages.MetadataDecisionVersion])
	assert.Equal(t, "1", sm.Metadata[messages.MetadataChoiceID])
	assert.Equal(t, store.DecisionProvenanceDirect, sm.Metadata[messages.MetadataDecisionProvenance])

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
	f := newDecisionsFixture(t)
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
	f := newDecisionsFixture(t)
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
	f := newDecisionsFixture(t)
	_, v1 := f.topicPrompt(t, oneDecision, false)
	_, v2 := f.topicPrompt(t, `[{"id":"D234","question":"Ship it?","choices":["yes","no"]}]`, true)
	rr := f.answer(t, f.bob, v1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	assert.Equal(t, ErrCodeDecisionReplaced, errorCode(t, rr))
	assert.Contains(t, rr.Body.String(), v2.Decisions[0].RowID)
}

// The answer publishes decision.updated on the project subject for a thread.
func TestDecisionAnswer_PublishesDecisionUpdated(t *testing.T) {
	f := newDecisionsFixture(t)
	events, unsub := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
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

// History rows for decision prompts gain decisions and the intro, and
// decision-response rows gain decisionResponse; both are omitted while the
// experiment is off.
func TestDecisionsHistory_ViewsGatedByExperiment(t *testing.T) {
	f := newDecisionsFixture(t)
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
	assert.Equal(t, "Please decide", h.MessageDecisionIntros[msg.ID])
	cr, ok := h.MessageDecisionResponses[ans.ResponseMessageID]
	require.True(t, ok)
	assert.Equal(t, "yes", cr.Label)
	assert.Equal(t, single.ID, cr.PromptMessageID)

	setDecisionsExperiment(t, f.srv, false)
	h = history()
	assert.Nil(t, h.MessageDecisions)
	assert.Nil(t, h.MessageDecisionIntros)
	assert.Nil(t, h.MessageDecisionResponses)
}

// ---------------------------------------------------------------------------
// Review round 1
// ---------------------------------------------------------------------------

// A decision prompt into a group conversation is addressed to humans: it
// wakes no default agent and fans out no agent mention. The same posts
// without decisions (the controls) do both.
func TestDecisionsSend_GroupPostWakesNoAgent(t *testing.T) {
	f := newGroupWakeFixture(t)
	setDecisionsExperiment(t, f.srv, true)
	f.setDefault(t, f.target.ID)

	f.post(t, "status update for the group", "")
	require.Len(t, dispatchesTo(f.dispatcher, f.target.ID), 1, "control: a plain post wakes the default agent")
	f.post(t, "@"+mentionBystanderSlug+" please look", "")
	require.Len(t, dispatchesTo(f.dispatcher, f.bystander.ID), 1, "control: a plain post fans out its mention")
	before := len(f.dispatcher.getCalls())

	for _, msg := range []string{"Please decide", "@" + mentionBystanderSlug + " please decide"} {
		resp := f.postRequest(t, OutboundMessageRequest{
			Msg: msg, ConversationRef: "conv:" + f.convID,
			Decisions: decisionInputs(t, `[{"question":"Ship?","choices":["y","n"]}]`),
		})
		assert.Nil(t, resp.DefaultAgentResult, msg)
		assert.Empty(t, resp.MentionResults, msg)
		assert.Empty(t, resp.WokenAgents, msg)
	}
	assert.Len(t, f.dispatcher.getCalls(), before, "a decision prompt dispatches to no agent")
}

// A DM answer publishes decision.updated to the DM's user only, never on the
// project subject (a thread answer does, TestDecisionAnswer_PublishesDecisionUpdated).
func TestDecisionAnswer_DMDecisionUpdatedOnlyToUser(t *testing.T) {
	f := newDecisionsFixture(t)
	projEvents, unsubProject := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
	defer unsubProject()
	userEvents, unsubUser := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.decision.updated")
	defer unsubUser()

	dm := f.sendPromptToBob(t, `[{"id":"dm-upd","question":"DM?","choices":["y","n"]}]`, false)
	rr := f.answer(t, f.bob, dm.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	assert.Len(t, collectEvents(userEvents), 1, "the DM's user gets decision.updated")
	assert.Empty(t, collectEvents(projEvents), "a DM answer is never published on the project subject")
}

// replacedEvent decodes the one chat.decision.updated event of evts and
// checks it is the replaced view of row, pointing at its new version.
func replacedEvent(t *testing.T, evts []Event, row string, newRow string) {
	t.Helper()
	require.Len(t, evts, 1, "one chat.decision.updated for the replaced version")
	var got struct {
		Decision  DecisionView `json:"decision"`
		MessageID string       `json:"messageId"`
	}
	require.NoError(t, json.Unmarshal(evts[0].Data, &got))
	assert.Equal(t, row, got.Decision.RowID)
	assert.Equal(t, store.DecisionStatusReplaced, got.Decision.Status)
	require.NotNil(t, got.Decision.ReplacedBy, "the view carries replacedBy")
	assert.Equal(t, newRow, got.Decision.ReplacedBy.RowID)
	assert.Equal(t, 2, got.Decision.ReplacedBy.Version)
	assert.Equal(t, got.Decision.MessageID, got.MessageID, "published for the old row's own prompt")
}

// An --update send publishes chat.decision.updated for the version it
// replaced, on the old prompt's own subjects: here the old version is in a
// project thread and the new one in a DM, so the replaced view goes out on
// the project subject (ptone/scion#7673 review R1).
func TestDecisionsSend_UpdatePublishesReplacedOnOldThread(t *testing.T) {
	f := newDecisionsFixture(t)
	_, v1 := f.topicPrompt(t, oneDecision, false)
	events, unsub := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
	defer unsub()

	v2 := f.sendPromptToBob(t, oneDecision, true)
	require.NotNil(t, v2.Decisions[0].Replaced, "the send replaced v1")
	replacedEvent(t, collectEvents(events), v1.Decisions[0].RowID, v2.Decisions[0].RowID)
}

// A replaced DM decision is published to the DM's user only, never on the
// project subject.
func TestDecisionsSend_UpdatePublishesReplacedDMOnlyToUser(t *testing.T) {
	f := newDecisionsFixture(t)
	raw := `[{"id":"dm-rep","question":"DM?","choices":["y","n"]}]`
	v1 := f.sendPromptToBob(t, raw, false)
	projEvents, unsubProject := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
	defer unsubProject()
	userEvents, unsubUser := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.decision.updated")
	defer unsubUser()

	v2 := f.sendPromptToBob(t, raw, true)
	replacedEvent(t, collectEvents(userEvents), v1.Decisions[0].RowID, v2.Decisions[0].RowID)
	assert.Empty(t, collectEvents(projEvents), "a DM replacement is never published on the project subject")
}

// A send without --update replaces nothing and publishes no decision.updated.
func TestDecisionsSend_NewPromptPublishesNoDecisionUpdated(t *testing.T) {
	f := newDecisionsFixture(t)
	userEvents, unsub := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.decision.updated")
	defer unsub()
	f.sendPromptToBob(t, `[{"id":"fresh","question":"New?","choices":["y","n"]}]`, false)
	assert.Empty(t, collectEvents(userEvents))
}

// The asker is gone: the answer still stands (200), its response row is
// stored as failed, and nothing is dispatched.
func TestDecisionAnswer_AskerGoneStoresFailedResponse(t *testing.T) {
	f := newDecisionsFixture(t)
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
	assert.Equal(t, messages.TypeDecisionResponse, m.Type)
	assert.Equal(t, store.MessageDispatchFailed, m.DispatchState)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing is dispatched to a gone asker")
}

// A user access token, a federated user and a visitor get 403 on both
// decision routes; answers come from a web session only. The token's and
// the federated user's refusal is the session-only one the catalog
// records (reason INTERACTIVE_STATE, credential session_required); the
// bearer disposition matrix cannot reach it while web.decisions is
// off, so this test pins it. The refusal does not depend on the token's
// selectors, which is why a project:read token covers the matrix rows.
func TestDecisionRoutes_TokenAndVisitorForbidden(t *testing.T) {
	f := newDecisionsFixture(t)
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
	f := newDecisionsFixture(t)
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

// Same idempotency key with $other replays 200 when other_text matches (after
// trim), and returns 409 already_answered when other_text differs.
func TestDecisionAnswer_IdempotencyKeyOtherTextMismatch409(t *testing.T) {
	f := newDecisionsFixture(t)
	_, res := f.topicPrompt(t, `[{"id":"region","question":"Which region?","choices":["us-central1"],"allow_other":"Another region"}]`, false)
	row := res.Decisions[0].RowID

	first := f.answer(t, f.bob, row, map[string]interface{}{
		"choice_id":       messages.ChoiceIDOther,
		"other_text":      "  europe-west4  ",
		"idempotency_key": "k-other",
	})
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	// Different other_text under the same idempotency key is 409 already_answered.
	diffText := f.answer(t, f.bob, row, map[string]interface{}{
		"choice_id":       messages.ChoiceIDOther,
		"other_text":      "asia-east1",
		"idempotency_key": "k-other",
	})
	assert.Equal(t, http.StatusConflict, diffText.Code, "different other_text: %s", diffText.Body.String())
	assert.Equal(t, ErrCodeDecisionAnswered, errorCode(t, diffText))

	// Same trimmed other_text under the same idempotency key replays 200.
	replay := f.answer(t, f.bob, row, map[string]interface{}{
		"choice_id":       messages.ChoiceIDOther,
		"other_text":      "europe-west4",
		"idempotency_key": "k-other",
	})
	require.Equal(t, http.StatusOK, replay.Code, "body: %s", replay.Body.String())
	var ans DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &ans))
	assert.True(t, ans.Replayed)
	require.NotNil(t, ans.Decision.Decision)
	assert.Equal(t, "europe-west4", ans.Decision.Decision.OtherText)
}

// A project member who may read but not post (a reader) can read a
// decision but not answer it.
func TestDecisionAnswer_ReaderForbidden(t *testing.T) {
	f := newDecisionsFixture(t)
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
func (f *decisionsFixture) recordInterpreted(t *testing.T, u *store.User, row, choiceID string) {
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
	f := newDecisionsFixture(t)
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
func TestDecisionsSend_ReplacedNamesWhoDecided(t *testing.T) {
	f := newDecisionsFixture(t)
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
func TestDecisionsSend_IntroGetsMentionRewrite(t *testing.T) {
	f := newDecisionsFixture(t)
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email, Msg: "Ask @agent:helper-bot first",
		Decisions: decisionInputs(t, `[{"question":"Ship?","choices":["y","n"]}]`),
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp decisionSendResponse
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
	f := newDecisionsFixtureOn(t, srv, st, alice, bob, project)
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
	f := newDecisionsFixture(t)
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

// A decision prompt may leave out its intro: the send is accepted and the
// stored body is the markdown fallback rendered from the decisions.
func TestDecisionsSend_WithoutIntro(t *testing.T) {
	f := newDecisionsFixture(t)
	rr := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email,
		Decisions: decisionInputs(t, `[{"id":"ship","question":"Ship it now?","choices":["yes","no"]}]`),
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp decisionSendResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID)
	require.Len(t, resp.Decisions, 1)

	m, err := f.store.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionPrompt, m.Type)
	assert.Contains(t, m.Msg, "Ship it now?", "the stored body is the rendered fallback")
	decs, err := f.store.GetDecisionsByMessageIDs(context.Background(), []string{resp.MessageID})
	require.NoError(t, err)
	require.Len(t, decs[resp.MessageID], 1)
	assert.Equal(t, 0, decs[resp.MessageID][0].IntroLength, "no intro")
}

// ptone/scion#7775: DecisionResponseView carries the answerer identity,
// outcome, interpreted provenance metadata, and overridden interpreted
// label identically on live SSE events and history reloads.
func TestDecisionResponseView_AttributionAndOutcomes(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()
	chat, unsub := f.srv.events.Subscribe("project." + f.project.ID + ".chat.message")
	defer unsub()

	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)

	recordTopicInterpreted := func(u *store.User, row, choiceID string) (srcID, respID string) {
		srcID = api.NewUUID()
		respID = api.NewUUID()
		_, _, err := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
			DecisionRowID: row, ChoiceID: choiceID,
			AnsweredByUserID: u.ID, AnsweredByName: u.DisplayName, AnsweredByEmail: u.Email,
			Provenance: store.DecisionProvenanceAgentInterpreted, InterpretedByAgentID: f.agent.ID,
			SourceMessageID: srcID, Surface: store.DecisionSurfaceNative,
			ResponseMessage: &store.Message{
				ID: respID, ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
				Recipient: "user:" + u.Email, RecipientID: u.ID, ConversationID: conv.ID,
				AgentID: f.agent.ID, Channel: "web", ThreadID: f.topicID, Msg: "interpreted", CreatedAt: time.Now().UTC(),
				DispatchState: store.MessageDispatchDispatched,
			},
		})
		require.NoError(t, err)
		return srcID, respID
	}

	// 1. Direct answer by Bob.
	directPrompt, directRes := f.topicPrompt(t, `[{"id":"D-direct","question":"Direct?","choices":["yes","no"]}]`, false)
	rr := f.answer(t, f.bob, directRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var directAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &directAns))

	// 2. Agent-interpreted answer for Alice.
	interpPrompt, interpRes := f.topicPrompt(t, `[{"id":"D-interp","question":"Interp?","choices":["yes","no"]}]`, false)
	srcID, interpRespID := recordTopicInterpreted(f.alice, interpRes.Decisions[0].RowID, "1")

	// 3. Confirmed answer by Alice.
	confirmPrompt, confirmRes := f.topicPrompt(t, `[{"id":"D-confirm","question":"Confirm?","choices":["yes","no"]}]`, false)
	recordTopicInterpreted(f.alice, confirmRes.Decisions[0].RowID, "1")
	rr = f.answer(t, f.alice, confirmRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var confirmAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &confirmAns))

	// 4. Overridden answer by Alice (interpreted as "yes", overridden to "no").
	overridePrompt, overrideRes := f.topicPrompt(t, `[{"id":"D-override","question":"Override?","choices":["yes","no"]}]`, false)
	recordTopicInterpreted(f.alice, overrideRes.Decisions[0].RowID, "1")
	rr = f.answer(t, f.alice, overrideRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "2"})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var overrideAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &overrideAns))

	// AnswerView on prompt DecisionView also populates PreviousChoiceLabel on
	// immediate answer, GET /api/v1/decisions/{rowId}, and chat history reload.
	require.NotNil(t, directAns.Decision.Decision)
	assert.Empty(t, directAns.Decision.Decision.PreviousChoiceLabel)
	require.NotNil(t, confirmAns.Decision.Decision)
	assert.Empty(t, confirmAns.Decision.Decision.PreviousChoiceLabel)
	require.NotNil(t, overrideAns.Decision.Decision)
	assert.Equal(t, "yes", overrideAns.Decision.Decision.PreviousChoiceLabel)

	getRec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+overrideRes.Decisions[0].RowID, nil)
	require.Equal(t, http.StatusOK, getRec.Code, "body: %s", getRec.Body.String())
	var getOut struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getOut))
	require.NotNil(t, getOut.Decision.Decision)
	assert.Equal(t, "yes", getOut.Decision.Decision.PreviousChoiceLabel)

	liveByID := map[string]DecisionResponseView{}
	for _, e := range collectEvents(chat) {
		var evt UserMessageEvent
		require.NoError(t, json.Unmarshal(e.Data, &evt))
		if evt.DecisionResponse != nil {
			liveByID[evt.ID] = *evt.DecisionResponse
		}
	}

	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var h chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &h))

	for source, views := range map[string]map[string]DecisionResponseView{"live": liveByID, "history": h.MessageDecisionResponses} {
		direct := views[directAns.ResponseMessageID]
		assert.Equal(t, f.bob.ID, direct.AnsweredByUserID, source)
		assert.Equal(t, f.bob.DisplayName, direct.AnsweredByName, source)
		assert.Equal(t, store.DecisionProvenanceDirect, direct.Provenance, source)
		assert.Equal(t, store.DecisionOutcomeAnswer, direct.Outcome, source)
		assert.Equal(t, directPrompt.ID, direct.PromptMessageID, source)
		assert.Empty(t, direct.PreviousChoiceLabel, source)
		assert.Empty(t, direct.InterpretedByAgentID, source)

		confirm := views[confirmAns.ResponseMessageID]
		assert.Equal(t, f.alice.ID, confirm.AnsweredByUserID, source)
		assert.Equal(t, f.alice.DisplayName, confirm.AnsweredByName, source)
		assert.Equal(t, store.DecisionProvenanceDirect, confirm.Provenance, source)
		assert.Equal(t, store.DecisionOutcomeConfirm, confirm.Outcome, source)
		assert.Equal(t, "yes", confirm.Label, source)
		assert.Equal(t, confirmPrompt.ID, confirm.PromptMessageID, source)
		assert.Empty(t, confirm.PreviousChoiceLabel, source)

		override := views[overrideAns.ResponseMessageID]
		assert.Equal(t, f.alice.ID, override.AnsweredByUserID, source)
		assert.Equal(t, f.alice.DisplayName, override.AnsweredByName, source)
		assert.Equal(t, store.DecisionProvenanceDirect, override.Provenance, source)
		assert.Equal(t, store.DecisionOutcomeOverride, override.Outcome, source)
		assert.Equal(t, "no", override.Label, source)
		assert.Equal(t, "yes", override.PreviousChoiceLabel, source)
		assert.Equal(t, overridePrompt.ID, override.PromptMessageID, source)
	}

	// History also enriches stored agent_interpreted decision-response rows.
	interp := h.MessageDecisionResponses[interpRespID]
	assert.Equal(t, f.alice.ID, interp.AnsweredByUserID)
	assert.Equal(t, f.alice.DisplayName, interp.AnsweredByName)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, interp.Provenance)
	assert.Equal(t, store.DecisionOutcomeAnswer, interp.Outcome)
	assert.Equal(t, f.agent.ID, interp.InterpretedByAgentID)
	assert.Equal(t, srcID, interp.SourceMessageID)
	assert.Equal(t, interpPrompt.ID, interp.PromptMessageID)
	assert.Empty(t, interp.PreviousChoiceLabel)

	// History prompt cards (messageDecisions) preserve PreviousChoiceLabel on
	// overridden decisions across reload.
	require.NotNil(t, h.MessageDecisions[directPrompt.ID][0].Decision)
	assert.Empty(t, h.MessageDecisions[directPrompt.ID][0].Decision.PreviousChoiceLabel)
	require.NotNil(t, h.MessageDecisions[interpPrompt.ID][0].Decision)
	assert.Empty(t, h.MessageDecisions[interpPrompt.ID][0].Decision.PreviousChoiceLabel)
	require.NotNil(t, h.MessageDecisions[confirmPrompt.ID][0].Decision)
	assert.Empty(t, h.MessageDecisions[confirmPrompt.ID][0].Decision.PreviousChoiceLabel)
	require.NotNil(t, h.MessageDecisions[overridePrompt.ID][0].Decision)
	assert.Equal(t, "yes", h.MessageDecisions[overridePrompt.ID][0].Decision.PreviousChoiceLabel)

	// Empty or whitespace-only AnsweredByName falls back to AnsweredByEmail.
	for _, blank := range []string{"", "   \t "} {
		emailAns := &store.DecisionAnswer{
			DecisionRowID: "row-email", AskerAgentID: f.agent.ID, Key: "D-email", Version: 1,
			QuestionText: "Email fallback?", ChoiceID: "1", ChoiceLabel: "yes",
			Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeAnswer,
			AnsweredByUserID: "u-no-name", AnsweredByName: blank, AnsweredByEmail: "noname@example.test",
		}
		assert.Equal(t, "noname@example.test", decisionResponseViewOf(emailAns, nil).AnsweredByName)
		assert.Equal(t, "noname@example.test", decisionResponseSetAnswerOf(emailAns, nil).AnsweredByName)
	}

	// Deleted answerer (AnsweredByUserID empty, snapshot AnsweredByName retained)
	// omits answeredByUserId on the wire while keeping answeredByName.
	deletedAns := &store.DecisionAnswer{
		DecisionRowID: "row-del", AskerAgentID: f.agent.ID, Key: "D-del", Version: 1,
		QuestionText: "Deleted user?", ChoiceID: "1", ChoiceLabel: "yes",
		Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeAnswer,
		AnsweredByUserID: "", AnsweredByName: "Old User", AnsweredByEmail: "old@example.test",
	}
	singleJSON, err := json.Marshal(decisionResponseViewOf(deletedAns, nil))
	require.NoError(t, err)
	assert.NotContains(t, string(singleJSON), `"answeredByUserId"`)
	assert.Contains(t, string(singleJSON), `"answeredByName":"Old User"`)

	setItemJSON, err := json.Marshal(decisionResponseSetAnswerOf(deletedAns, nil))
	require.NoError(t, err)
	assert.NotContains(t, string(setItemJSON), `"answeredByUserId"`)
	assert.Contains(t, string(setItemJSON), `"answeredByName":"Old User"`)
}

// ptone/scion#8249: POST /api/v1/agents/{id}/outbound-message explicitly
// rejects the removed legacy "choices" and "choices_update" request keys with
// 400 validation_error (even when empty, false, or null, and whether or not
// msg is present), directing callers to "decisions" and an updated CLI.
func TestDecisionsSend_RejectsLegacyChoicesFields(t *testing.T) {
	f := newDecisionsFixture(t)

	// Primary decisions field continues to succeed.
	rrPrimary := f.sendRawAsAgent(t, []byte(`{"recipient":"user:`+f.bob.Email+`","msg":"Primary","decisions":[{"id":"D-leg","question":"Ship v1?","choices":["yes","no"]}]}`))
	require.Equal(t, http.StatusOK, rrPrimary.Code, rrPrimary.Body.String())

	cases := []struct {
		name      string
		payload   string
		wantField string
	}{
		{
			name:      "choices populated with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Pick one","choices":[{"id":"D-old","question":"Ship?","choices":["yes","no"]}]}`,
			wantField: "choices",
		},
		{
			name:      "choices populated without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices":[{"id":"D-old","question":"Ship?","choices":["yes","no"]}]}`,
			wantField: "choices",
		},
		{
			name:      "choices empty array with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Pick one","choices":[]}`,
			wantField: "choices",
		},
		{
			name:      "choices empty array without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices":[]}`,
			wantField: "choices",
		},
		{
			name:      "choices null with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Pick one","choices":null}`,
			wantField: "choices",
		},
		{
			name:      "choices null without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices":null}`,
			wantField: "choices",
		},
		{
			name:      "choices_update true with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Update","choices_update":true}`,
			wantField: "choices_update",
		},
		{
			name:      "choices_update true without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices_update":true}`,
			wantField: "choices_update",
		},
		{
			name:      "choices_update false with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Update","choices_update":false}`,
			wantField: "choices_update",
		},
		{
			name:      "choices_update false without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices_update":false}`,
			wantField: "choices_update",
		},
		{
			name:      "choices_update null with msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Update","choices_update":null}`,
			wantField: "choices_update",
		},
		{
			name:      "choices_update null without msg",
			payload:   `{"recipient":"user:` + f.bob.Email + `","choices_update":null}`,
			wantField: "choices_update",
		},
		{
			name:      "both decisions and choices",
			payload:   `{"recipient":"user:` + f.bob.Email + `","decisions":[{"question":"A?","choices":["y","n"]}],"choices":[{"question":"B?","choices":["y","n"]}]}`,
			wantField: "choices",
		},
		{
			name:      "choices and choices_update together",
			payload:   `{"recipient":"user:` + f.bob.Email + `","msg":"Fallback","choices":[{"id":"D-leg","question":"Ship v2?","choices":["yes","no"]}],"choices_update":true}`,
			wantField: "choices",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, lerr := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 20})
			require.NoError(t, lerr)

			rr := f.sendRawAsAgent(t, []byte(tc.payload))
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Equal(t, ErrCodeValidationError, errorCode(t, rr))
			body := rr.Body.String()
			assert.Contains(t, body, "decisions")
			assert.Contains(t, body, "update your scion CLI")

			var env struct {
				Error struct {
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env))
			assert.Equal(t, tc.wantField, env.Error.Details["field"])

			after, lerr := f.store.ListMessages(context.Background(), store.MessageFilter{SenderID: f.agent.ID}, store.ListOptions{Limit: 20})
			require.NoError(t, lerr)
			assert.Equal(t, before.TotalCount, after.TotalCount, "rejected request must store nothing")
		})
	}
}

// ptone/scion#8224 (plan §5.2 & §12 item 3): clean break for legacy message
// types. Pre-upgrade "choice-prompt" and "choice-response" rows in messages
// are not hydrated as decision cards or response pills in chat history, and
// POST /api/v1/decision-prompts/{id}/answers answers 404 on a "choice-prompt"
// row, while per-decision POST /api/v1/decisions/{rowId}/answer and
// POST /api/v1/agent/decisions/interpreted continue to work on the underlying
// decision rows.
func TestDecisions_CleanBreakLegacyMessageTypes(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	legacySingleMsg, legacySingleRes := f.topicPrompt(t, `[{"id":"LEG-1","question":"Legacy single?","choices":["yes","no"],"default":"yes"}]`, false)
	legacySetMsg, legacySetRes := f.topicPrompt(t, `[{"id":"LEG-S1","question":"Legacy set 1?","choices":["yes","no"]},{"id":"LEG-S2","question":"Legacy set 2?","choices":["a","b"]}]`, false)
	alreadyAnsweredMsg, alreadyAnsweredRes := f.topicPrompt(t, `[{"id":"LEG-ANS","question":"Already answered?","choices":["yes","no"]}]`, false)
	ansRR := f.answer(t, f.bob, alreadyAnsweredRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, ansRR.Code, ansRR.Body.String())
	var priorAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(ansRR.Body.Bytes(), &priorAns))

	// Rewrite stored messages.type to pre-S2 wire values to simulate an upgraded DB.
	provider, ok := f.store.(testRetentionProvider)
	require.True(t, ok)
	for _, id := range []string{legacySingleMsg.ID, legacySetMsg.ID, alreadyAnsweredMsg.ID} {
		_, err := provider.DB().ExecContext(ctx, `UPDATE messages SET type = 'choice-prompt' WHERE id = ?`, id)
		require.NoError(t, err)
	}
	_, err := provider.DB().ExecContext(ctx, `UPDATE messages SET type = 'choice-response' WHERE id = ?`, priorAns.ResponseMessageID)
	require.NoError(t, err)

	// 1. Chat history returns the raw markdown/text bodies and omits legacy rows from all side maps.
	rec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var h chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &h))

	byID := map[string]store.Message{}
	for _, m := range h.Messages {
		byID[m.ID] = m
	}
	assert.Contains(t, byID[legacySingleMsg.ID].Msg, "Legacy single?")
	assert.Contains(t, byID[legacySetMsg.ID].Msg, "Legacy set 1?")
	assert.Contains(t, byID[priorAns.ResponseMessageID].Msg, "chose")
	assert.NotContains(t, h.MessageDecisions, legacySingleMsg.ID)
	assert.NotContains(t, h.MessageDecisions, legacySetMsg.ID)
	assert.NotContains(t, h.MessageDecisionIntros, legacySingleMsg.ID)
	assert.NotContains(t, h.MessageDecisionAnswerModes, legacySetMsg.ID)
	assert.NotContains(t, h.MessageDecisionResponses, priorAns.ResponseMessageID)

	// 2. Set submit on a "choice-prompt" message returns 404.
	setRR := f.submitSet(t, f.bob, legacySetMsg.ID, "", legacySetRes.Decisions[0].RowID, "1", legacySetRes.Decisions[1].RowID, "1")
	assert.Equal(t, http.StatusNotFound, setRR.Code, setRR.Body.String())

	// 3. Per-decision answer on the single-decision row still succeeds and mints a decision-response!
	before := len(f.dispatcher.getMessages())
	singleRR := f.answer(t, f.bob, legacySingleRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	require.Equal(t, http.StatusOK, singleRR.Code, singleRR.Body.String())
	var singleAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(singleRR.Body.Bytes(), &singleAns))
	newRespMsg, err := f.store.GetMessage(ctx, singleAns.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, newRespMsg.Type)
	sent := f.dispatcher.getMessages()[before:]
	require.Len(t, sent, 1)
	assert.True(t, strings.HasPrefix(sent[0].structured.Msg, "[decision-response] LEG-1 v1"), sent[0].structured.Msg)

	// 4. Agent interpreted recording still works on the legacy set's rows!
	userReply := f.storeUserReply(t, f.alice, f.topicID, legacySetMsg.ConversationID, "web", "LEG-S1: yes, LEG-S2: a")
	interpRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", RecordInterpretedRequest{
		SourceMessageID: userReply.ID,
		IdempotencyKey:  "ik-legacy-set",
		Answers: []InterpretedAnswerInput{
			{Key: "LEG-S1", ChoiceID: "1"},
			{Key: "LEG-S2", ChoiceID: "1"},
		},
	})
	require.Equal(t, http.StatusOK, interpRR.Code, interpRR.Body.String())
	var interpOut RecordInterpretedResponse
	require.NoError(t, json.Unmarshal(interpRR.Body.Bytes(), &interpOut))
	interpMsg, err := f.store.GetMessage(ctx, interpOut.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, interpMsg.Type)
}

func TestDecisionResponseAgentLine_OtherGolden(t *testing.T) {
	cases := []struct {
		name       string
		answer     *store.DecisionAnswer
		overridden *store.DecisionAnswer
		want       string
	}{
		{
			name: "design 4.5 single Other example",
			answer: &store.DecisionAnswer{
				Key:             "region",
				Version:         1,
				QuestionText:    "Which region for the canary?",
				ChoiceID:        messages.ChoiceIDOther,
				ChoiceLabel:     "Another region",
				OtherText:       "europe-west4 canary",
				AnsweredByEmail: "alice@example.com",
			},
			want: `[decision-response] region v1 ("Which region for the canary?") answer=Another region (choice $other) text="europe-west4 canary" by alice@example.com`,
		},
		{
			name: "quotes backslashes and non-ASCII Unicode",
			answer: &store.DecisionAnswer{
				Key:             "region",
				Version:         2,
				QuestionText:    "Which region?",
				ChoiceID:        messages.ChoiceIDOther,
				ChoiceLabel:     "Other",
				OtherText:       `use "eu-west4\a" — café 東京 🚀`,
				AnsweredByEmail: "alice@example.com",
			},
			want: `[decision-response] region v2 ("Which region?") answer=Other (choice $other) text="use \"eu-west4\\a\" — café 東京 🚀" by alice@example.com`,
		},
		{
			name: "override of an interpreted Other answer",
			answer: &store.DecisionAnswer{
				Key:             "region",
				Version:         1,
				QuestionText:    "Which region for the canary?",
				ChoiceID:        messages.ChoiceIDOther,
				ChoiceLabel:     "Another region",
				OtherText:       "europe-west4 canary",
				AnsweredByEmail: "alice@example.com",
				Outcome:         store.DecisionOutcomeOverride,
			},
			overridden: &store.DecisionAnswer{
				ChoiceID:    messages.ChoiceIDOther,
				ChoiceLabel: "Another region",
				OtherText:   "europe-west4",
			},
			want: `[decision-response] region v1 ("Which region for the canary?") answer=Another region (choice $other) text="europe-west4 canary" by alice@example.com, overriding interpreted answer Another region text="europe-west4"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decisionResponseAgentLine(tc.answer, tc.overridden))
		})
	}
}

func TestDecisions_AllowOther_SingleEndToEndValidationAndAskerOnlyDelivery(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	// Create a bystander agent in the same project so we can prove @mentions in
	// other_text never fan out to other agents (acceptance 10).
	bystander := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "bystander-bot",
		Name:            "Bystander Bot",
		ProjectID:       f.project.ID,
		RuntimeBrokerID: f.agent.RuntimeBrokerID,
		OwnerID:         f.agent.OwnerID,
		Phase:           "running",
		Activity:        "idle",
		MessageMode:     f.agent.MessageMode,
	}
	require.NoError(t, f.store.CreateAgent(ctx, bystander))

	// 1. Single decision with 1 fixed choice + allow_other object succeeds.
	// (Multi-decision prompts with allow_other are now supported in P2 and
	// covered by TestDecisionSetAnswer_AllowOther_EndToEnd in decisions_batch_test.go.)
	promptMsg, promptRes := f.topicPrompt(t, `[{
		"id": "region",
		"question": "Which region for the canary?",
		"choices": ["us-central1"],
		"default": "us-central1",
		"allow_other": {
			"label": "Another region",
			"description": "Any GCP region code",
			"placeholder": "e.g. europe-west4"
		}
	}]`, false)
	row := promptRes.Decisions[0].RowID
	assert.Contains(t, promptMsg.Msg, "  - Another region: Any GCP region code — *your own answer, in words*\n")

	// GET /api/v1/decisions/{rowId} carries other.
	getRec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+row, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var getOut struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getOut))
	require.NotNil(t, getOut.Decision.Other)
	assert.Equal(t, messages.OtherOption{
		Label:       "Another region",
		Description: "Any GCP region code",
		Placeholder: "e.g. europe-west4",
	}, *getOut.Decision.Other)

	// 3. Validation failures on POST /api/v1/decisions/{rowId}/answer.
	_, noOtherRes := f.topicPrompt(t, `[{"id":"strict","question":"Strict?","choices":["yes","no"]}]`, false)
	rrNoOther := f.answer(t, f.bob, noOtherRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id":  messages.ChoiceIDOther,
		"other_text": "custom",
	})
	require.Equal(t, http.StatusBadRequest, rrNoOther.Code, rrNoOther.Body.String())

	for _, tc := range []struct {
		name     string
		body     map[string]interface{}
		wantCode string
	}{
		{"missing text", map[string]interface{}{"choice_id": messages.ChoiceIDOther}, messages.DecisionErrRequired},
		{"spaces text", map[string]interface{}{"choice_id": messages.ChoiceIDOther, "other_text": "   "}, messages.DecisionErrRequired},
		{"newline", map[string]interface{}{"choice_id": messages.ChoiceIDOther, "other_text": "eu\nwest"}, messages.DecisionErrInvalid},
		{"tab", map[string]interface{}{"choice_id": messages.ChoiceIDOther, "other_text": "eu\twest"}, messages.DecisionErrInvalid},
		{"bidi", map[string]interface{}{"choice_id": messages.ChoiceIDOther, "other_text": "eu\u202ewest"}, messages.DecisionErrInvalid},
		{"too long", map[string]interface{}{"choice_id": messages.ChoiceIDOther, "other_text": strings.Repeat("é", 201)}, messages.DecisionErrTooLong},
		{"other_text on fixed choice", map[string]interface{}{"choice_id": "1", "other_text": "extra"}, messages.DecisionErrNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.answer(t, f.bob, row, tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			errCode, det := errorCodeAndDetails(t, rr)
			assert.Equal(t, ErrCodeValidationError, errCode)
			assert.Equal(t, "other_text", det["field"])
			assert.Equal(t, tc.wantCode, det["code"])
		})
	}

	// 4. Valid Other answer with @mentions, quotes, backslashes, Markdown & HTML.
	before := len(f.dispatcher.getMessages())
	typed := `  eu-west4 "canary\1" *bold* <b>tag</b> @bystander-bot @everyone <!channel>  `
	trimmed := strings.TrimSpace(typed)
	rr := f.answer(t, f.bob, row, map[string]interface{}{
		"choice_id":  messages.ChoiceIDOther,
		"other_text": typed,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var ans DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ans))
	require.NotNil(t, ans.Decision.Decision)
	assert.Equal(t, messages.ChoiceIDOther, ans.Decision.Decision.ChoiceID)
	assert.Equal(t, "Another region", ans.Decision.Decision.Label)
	assert.Equal(t, trimmed, ans.Decision.Decision.OtherText)
	assert.False(t, ans.Decision.Decision.MatchedDefault)

	// Only the asking agent received a dispatch; @bystander-bot got nothing.
	sent := f.dispatcher.getMessages()[before:]
	require.Len(t, sent, 1)
	assert.Equal(t, f.agent.Slug, sent[0].agentSlug)
	sm := sent[0].structured
	require.NotNil(t, sm)
	wantLine := fmt.Sprintf(`[decision-response] region v1 ("Which region for the canary?") answer=Another region (choice $other) text=%s by %s`,
		strconv.Quote(trimmed), f.bob.Email)
	assert.Equal(t, wantLine, sm.Msg)
	assert.Equal(t, "region", sm.Metadata[messages.MetadataDecisionKey])
	assert.Equal(t, "1", sm.Metadata[messages.MetadataDecisionVersion])
	assert.Equal(t, messages.ChoiceIDOther, sm.Metadata[messages.MetadataChoiceID])
	assert.Equal(t, store.DecisionProvenanceDirect, sm.Metadata[messages.MetadataDecisionProvenance])
	assert.Equal(t, trimmed, sm.Metadata[messages.MetadataDecisionOtherText])

	// Stored response fallback message escapes Markdown/HTML and neutralizes mentions.
	storedResp, err := f.store.GetMessage(ctx, ans.ResponseMessageID)
	require.NoError(t, err)
	who := userDisplay(authUser(f.bob))
	wantStored := fmt.Sprintf(`%s answered "%s" (Another region) for region "Which region for the canary?"`,
		who, messages.EscapeMarkdownPlain(trimmed))
	assert.Equal(t, wantStored, storedResp.Msg)
	assert.Contains(t, storedResp.Msg, `\*bold\*`)
	assert.Contains(t, storedResp.Msg, `\<b\>tag\</b\>`)
	assert.Contains(t, storedResp.Msg, "@\u200beveryone")
	assert.Contains(t, storedResp.Msg, "\\<\u200b!channel\\>")

	// Chat history includes DecisionView.Other, AnswerView.OtherText, and DecisionResponseView.OtherText.
	histRec := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/chat/conversations/"+f.topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
	var hist chatHistoryResponse
	require.NoError(t, json.Unmarshal(histRec.Body.Bytes(), &hist))
	require.Len(t, hist.MessageDecisions[promptMsg.ID], 1)
	require.NotNil(t, hist.MessageDecisions[promptMsg.ID][0].Other)
	assert.Equal(t, "Another region", hist.MessageDecisions[promptMsg.ID][0].Other.Label)
	require.NotNil(t, hist.MessageDecisions[promptMsg.ID][0].Decision)
	assert.Equal(t, trimmed, hist.MessageDecisions[promptMsg.ID][0].Decision.OtherText)
	respView, ok := hist.MessageDecisionResponses[ans.ResponseMessageID]
	require.True(t, ok)
	assert.Equal(t, messages.ChoiceIDOther, respView.ChoiceID)
	assert.Equal(t, trimmed, respView.OtherText)

	// Held-message redrive enrichment also populates text=... and decision_other_text.
	redriveSM := &messages.StructuredMessage{
		Metadata: map[string]string{messages.MetadataDecisionOtherText: "forged"},
	}
	fixedReason, rerr := f.srv.enrichHeldDecisionResponse(ctx, storedResp, redriveSM)
	require.NoError(t, rerr)
	assert.Empty(t, fixedReason)
	assert.Equal(t, wantLine, redriveSM.Msg)
	assert.Equal(t, trimmed, redriveSM.Metadata[messages.MetadataDecisionOtherText])

	// 5. Withdrawn and replaced rows refuse $other with 409 withdrawn / 409 replaced.
	_, wdRes := f.topicPrompt(t, `[{"id":"wd-1","question":"Withdraw?","choices":["yes"],"allow_other":true}]`, false)
	wdCall := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", map[string]interface{}{
		"keys": []string{"wd-1"},
	})
	require.Equal(t, http.StatusOK, wdCall.Code, wdCall.Body.String())
	rrWd := f.answer(t, f.bob, wdRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id":  messages.ChoiceIDOther,
		"other_text": "too late",
	})
	assert.Equal(t, http.StatusConflict, rrWd.Code, rrWd.Body.String())
	assert.Equal(t, ErrCodeDecisionWithdrawn, errorCode(t, rrWd))

	_, repRes := f.topicPrompt(t, `[{"id":"rep-1","question":"Replace?","choices":["yes"],"allow_other":true}]`, false)
	f.topicPrompt(t, `[{"id":"rep-1","question":"Replace v2?","choices":["yes","no"]}]`, true)
	rrRep := f.answer(t, f.bob, repRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id":  messages.ChoiceIDOther,
		"other_text": "too late",
	})
	assert.Equal(t, http.StatusConflict, rrRep.Code, rrRep.Body.String())
	assert.Equal(t, ErrCodeDecisionReplaced, errorCode(t, rrRep))

	// Other "$"-prefixed sentinels (like $dismissed) stay refused on /answer,
	// and dismissed rows with allow_other refuse $other with 409 dismissed.
	dsmPrompt, dsmRes := f.topicPrompt(t, `[{"id":"dsm-1","question":"Dismiss?","choices":["yes"],"allow_other":true}]`, false)
	rrReserved := f.answer(t, f.bob, dsmRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id": messages.ChoiceIDDismissed,
	})
	assert.Equal(t, http.StatusBadRequest, rrReserved.Code, rrReserved.Body.String())

	dsmDec, err := f.store.GetDecision(ctx, dsmRes.Decisions[0].RowID)
	require.NoError(t, err)
	_, err = f.store.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID:      dsmPrompt.ID,
		Surface:        store.DecisionSurfaceDecisionsPage,
		IdempotencyKey: "dsm-idem-1",
		User: store.DecisionAnswerer{
			UserID: f.bob.ID,
			Name:   f.bob.DisplayName,
			Email:  f.bob.Email,
		},
		ResponseMessage: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.project.ID,
			ConversationID: dsmPrompt.ConversationID,
			Sender:         "user:" + f.bob.Email,
			SenderID:       f.bob.ID,
			Recipient:      "agent:" + f.agent.Slug,
			RecipientID:    f.agent.ID,
			DispatchState:  store.MessageDispatchDispatched,
		},
		FormatMessage: func([]*store.Decision) string { return "dismissed" },
		Now:           func() time.Time { return dsmDec.CreatedAt.Add(store.DecisionDismissMinAge) },
	})
	require.NoError(t, err)
	rrDsm := f.answer(t, f.bob, dsmRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id":       messages.ChoiceIDOther,
		"other_text":      "too late",
		"idempotency_key": "dsm-idem-1",
	})
	assert.Equal(t, http.StatusConflict, rrDsm.Code, rrDsm.Body.String())
	assert.Equal(t, ErrCodeDecisionDismissed, errorCode(t, rrDsm))

	// 6. Multibyte at-limit Other fields (100 Cyrillic runes label, 300 CJK runes
	// description, 100 CJK runes placeholder) save and round-trip with HTTP 200;
	// 101 Cyrillic runes returns HTTP 400 validation_error (never 500).
	mbLabel := strings.Repeat("д", messages.MaxChoiceLabelLen)
	mbDesc := strings.Repeat("東", messages.MaxChoiceDescriptionLen)
	mbPlaceholder := strings.Repeat("東", messages.MaxOtherPlaceholderLen)
	_, mbRes := f.topicPrompt(t, fmt.Sprintf(`[{
		"id": "mb-other",
		"question": "Multibyte Other?",
		"choices": ["yes"],
		"allow_other": {
			"label": %q,
			"description": %q,
			"placeholder": %q
		}
	}]`, mbLabel, mbDesc, mbPlaceholder), false)
	mbGet := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+mbRes.Decisions[0].RowID, nil)
	require.Equal(t, http.StatusOK, mbGet.Code, mbGet.Body.String())
	var mbOut struct {
		Decision DecisionView `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(mbGet.Body.Bytes(), &mbOut))
	require.NotNil(t, mbOut.Decision.Other)
	assert.Equal(t, mbLabel, mbOut.Decision.Other.Label)
	assert.Equal(t, mbDesc, mbOut.Decision.Other.Description)
	assert.Equal(t, mbPlaceholder, mbOut.Decision.Other.Placeholder)

	mbOver := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient:       "conv:" + f.topicID,
		ConversationRef: "conv:" + f.topicID,
		Msg:             "Over multibyte limit",
		Decisions: decisionInputs(t, fmt.Sprintf(`[{
			"id": "mb-over",
			"question": "Too long?",
			"choices": ["yes"],
			"allow_other": {"label": %q}
		}]`, strings.Repeat("д", messages.MaxChoiceLabelLen+1))),
	})
	require.Equal(t, http.StatusBadRequest, mbOver.Code, mbOver.Body.String())
	assert.Equal(t, ErrCodeValidationError, errorCode(t, mbOver))
}
