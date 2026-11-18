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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeDismissResponse(t *testing.T, rr *httptest.ResponseRecorder) DismissDecisionPromptResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out DismissDecisionPromptResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func TestDecisionPromptDismiss_Authorization(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()

	f.agentA.OwnerID = f.owner
	f.agentA.Ancestry = []string{f.owner}
	f.agentA.MessageMode = store.MessageModeProject
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))

	createdAt := time.Now().UTC().Add(-10 * time.Minute)
	f.srv.decisionClock = func() time.Time { return time.Now().UTC().Add(10 * time.Minute) }

	seedTopicPrompt := func(key string) string {
		decs, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"Topic choice?","choices":["yes","no"]}]`))
		require.NoError(t, err)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
			Recipient: "chat", ThreadID: f.topicA, Channel: "web", CreatedAt: createdAt,
		}
		_, err = f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: msg, Intro: "Topic intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decs,
		})
		require.NoError(t, err)
		return msg.ID
	}

	dmKey := f.dm(t, f.owner, "agent", f.agentA.ID)
	seedDMPrompt := func(key string) string {
		decs, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"DM choice?","choices":["yes","no"]}]`))
		require.NoError(t, err)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
			Recipient: "user:" + f.owner, ThreadID: dmKey, Channel: "web", CreatedAt: createdAt,
		}
		_, err = f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: msg, Intro: "DM intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decs,
		})
		require.NoError(t, err)
		return msg.ID
	}

	topicMsgID := seedTopicPrompt("dsm-auth-topic-1")
	dmMsgID := seedDMPrompt("dsm-auth-dm-1")
	topicPath := "/api/v1/decision-prompts/" + topicMsgID + "/dismiss"
	dmPath := "/api/v1/decision-prompts/" + dmMsgID + "/dismiss"

	// 1. web.decisions off -> 404 across methods and callers.
	setDecisionsExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodPost, topicPath, map[string]string{}).Code)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodGet, topicPath, nil).Code)

	agentOffReq := httptest.NewRequest(http.MethodPost, topicPath, strings.NewReader(`{}`))
	agentOffReq.Header.Set("Content-Type", "application/json")
	agentOffRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentOffRec, agentOffReq.WithContext(agentCtx(agentOffReq.Context(), f.agentA)))
	assert.Equal(t, http.StatusNotFound, agentOffRec.Code)

	setDecisionsExperiment(t, f.srv, true)

	// 2. Unsupported methods -> 405; unknown sub-paths -> 404.
	assert.Equal(t, http.StatusMethodNotAllowed, f.session(t, f.member, http.MethodGet, topicPath, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, f.session(t, f.member, http.MethodDelete, topicPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodPost, topicPath+"/extra", map[string]string{}).Code)
	assert.Equal(t, http.StatusNotFound, f.session(t, f.member, http.MethodPost, "/api/v1/decision-prompts/"+topicMsgID+"/bogus", map[string]string{}).Code)

	// 3. Unauthenticated -> 401.
	unauthReq := httptest.NewRequest(http.MethodPost, topicPath, strings.NewReader(`{}`))
	unauthRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauthRec, unauthReq)
	assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	// 4. Agent caller -> 403 users_only.
	agentReq := httptest.NewRequest(http.MethodPost, topicPath, strings.NewReader(`{}`))
	agentReq.Header.Set("Content-Type", "application/json")
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq.WithContext(agentCtx(agentReq.Context(), f.agentA)))
	require.Equal(t, http.StatusForbidden, agentRec.Code, agentRec.Body.String())
	assert.Equal(t, ErrCodeUsersOnly, errorCode(t, agentRec))

	// 5. Visitor and federated user -> 403.
	visitor := NewVisitorIdentity(tid("dsm-visitor"), "https://issuer.test", "sub-v", "v@test.com", "Visitor")
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visitor, http.MethodPost, topicPath, map[string]string{}).Code)
	fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed", "fed@test.com", "Fed", "member", nil)
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, fed, http.MethodPost, topicPath, map[string]string{}).Code)

	// 6. Outsider (member of projB only, no access to projA) -> 404.
	outsider := api.NewUUID()
	createTestUserWithProjectRole(t, f.s, outsider, outsider+"@test.com", f.projB, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.s, outsider)
	assert.Equal(t, http.StatusNotFound, f.session(t, outsider, http.MethodPost, topicPath, map[string]string{}).Code)

	// 7. Read-only topic member (project.read without project.chat_post) -> 403 chat_post_denied.
	reader := f.chatReader(t, f.projA)
	readOnlyRec := f.session(t, reader, http.MethodPost, topicPath, map[string]string{})
	require.Equal(t, http.StatusForbidden, readOnlyRec.Code, readOnlyRec.Body.String())
	assert.Equal(t, ErrCodeChatPostDenied, errorCode(t, readOnlyRec))

	// 8. Non-participant admin on DM prompt -> 404 (admins cannot dismiss DM decisions, D5).
	assert.Equal(t, http.StatusNotFound, f.session(t, f.admin, http.MethodPost, dmPath, map[string]string{}).Code)

	// 9. Live asker refusing messages (MessageModeNone) -> 403 MESSAGE_DENIED.
	f.agentA.MessageMode = store.MessageModeNone
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))
	deniedRec := f.session(t, f.owner, http.MethodPost, topicPath, map[string]string{})
	require.Equal(t, http.StatusForbidden, deniedRec.Code, deniedRec.Body.String())
	assert.Equal(t, ErrCodeMessageDenied, errorCode(t, deniedRec))
	f.agentA.MessageMode = store.MessageModeProject
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))

	// 10. UAT scope and boundary checks.
	tokNoInboxWrite := f.mint(t, f.owner, projectBoundary(f.projA), "project:read", "inbox:read", "agent:read", "agent:message")
	assert.Equal(t, http.StatusForbidden, f.call(t, tokNoInboxWrite, http.MethodPost, topicPath, map[string]string{}).Code)

	tokWrongProj := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:write", "agent:read", "agent:message")
	assert.Equal(t, http.StatusNotFound, f.call(t, tokWrongProj, http.MethodPost, topicPath, map[string]string{}).Code)

	// Give f.owner owner role in projB so a projB-bounded UAT passes authorizeInboxToken,
	// then verify DM in projA is refused (404) as outside the UAT's project boundary.
	createTestUserWithProjectRole(t, f.s, f.owner, f.owner+"@test.com", f.projB, store.ProjectRoleOwner)
	tokOwnerWrongProj := f.mint(t, f.owner, projectBoundary(f.projB), "project:read", "inbox:write", "agent:read", "agent:message")
	assert.Equal(t, http.StatusNotFound, f.call(t, tokOwnerWrongProj, http.MethodPost, dmPath, map[string]string{}).Code)

	// Project-bounded UAT with inbox:write + project:read + agent:read + agent:message -> 200 on both topic and own DM!
	tokValidProj := f.mint(t, f.owner, projectBoundary(f.projA), "project:read", "inbox:write", "agent:read", "agent:message")
	uatTopicOut := decodeDismissResponse(t, f.call(t, tokValidProj, http.MethodPost, topicPath, map[string]string{"reason": "Token dismiss"}))
	assert.Len(t, uatTopicOut.Dismissed, 1)

	tokValidHub := f.mint(t, f.owner, hubBoundary(), "project:read", "inbox:write", "agent:read", "agent:message")
	uatDMOut := decodeDismissResponse(t, f.call(t, tokValidHub, http.MethodPost, dmPath, map[string]string{"reason": "Hub token DM dismiss"}))
	assert.Len(t, uatDMOut.Dismissed, 1)
}

func TestDecisionPromptDismiss_MinimumAgeAndDelivery(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	twoMsg, twoRes := f.topicPrompt(t, twoDecisions, false)
	require.Len(t, twoRes.Decisions, 2)
	d0, err := f.store.GetDecision(ctx, twoRes.Decisions[0].RowID)
	require.NoError(t, err)
	createdAt := d0.CreatedAt
	availAt := createdAt.Add(store.DecisionDismissMinAge)
	twoDismissPath := "/api/v1/decision-prompts/" + twoMsg.ID + "/dismiss"

	// Validation: malformed JSON, unsupported surface, over-500-rune reason -> 400.
	rawBad := httptest.NewRequest(http.MethodPost, twoDismissPath, strings.NewReader(`{bad-json`))
	rawBad.Header.Set("Content-Type", "application/json")
	rawRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rawRec, rawBad.WithContext(contextWithIdentity(rawBad.Context(), authUser(f.bob))))
	assert.Equal(t, http.StatusBadRequest, rawRec.Code)

	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"surface": "slack",
	}).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"reason": strings.Repeat("☕", store.MaxDecisionDismissReasonRunes+1),
	}).Code)

	// Reserved sentinel $dismissed is rejected on single and set answer routes (400).
	oneMsg, oneRes := f.topicPrompt(t, `[{"id":"D-SINGLE","question":"Ship single?","choices":["yes","no"],"default":"yes"}]`, false)
	oneRowID := oneRes.Decisions[0].RowID
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+oneRowID+"/answer", map[string]interface{}{
		"choice_id": messages.ChoiceIDDismissed,
	}).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+twoMsg.ID+"/answers", map[string]interface{}{
		"answers": []map[string]interface{}{
			{"decision_row_id": twoRes.Decisions[0].RowID, "choice_id": messages.ChoiceIDDismissed},
			{"decision_row_id": twoRes.Decisions[1].RowID, "choice_id": "a"},
		},
	}).Code)

	// 1. At 4m59s -> 409 too_recent with details.availableAt, nothing stored or dispatched.
	beforeMsgs := len(f.dispatcher.getMessages())
	f.srv.decisionClock = func() time.Time { return createdAt.Add(4*time.Minute + 59*time.Second) }
	earlyRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"reason":  "Too early",
		"surface": store.DecisionSurfaceDecisionsPage,
	})
	require.Equal(t, http.StatusConflict, earlyRR.Code, earlyRR.Body.String())
	code, details := errorCodeAndDetails(t, earlyRR)
	assert.Equal(t, ErrCodeDecisionTooRecent, code)
	availStr, ok := details["availableAt"].(string)
	require.True(t, ok, "details: %+v", details)
	parsedAvail, err := time.Parse(time.RFC3339Nano, availStr)
	require.NoError(t, err)
	assert.True(t, availAt.Equal(parsedAvail), "want %v, got %v", availAt, parsedAvail)
	assert.Len(t, f.dispatcher.getMessages(), beforeMsgs)

	// Confirm both prompts appear on project and profile Waiting on you before dismissal.
	projBefore := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/decisions?tab=pending", nil))
	assert.Equal(t, 3, projBefore.Counts.WaitingOnYou)

	// Subscribe to SSE decision.updated and chat message events.
	updEvents, unsubUpd := f.srv.events.Subscribe("project." + f.project.ID + ".chat.decision.updated")
	defer unsubUpd()

	// 2. At 5m00s -> 200, dismisses both rows, publishes decision.updated, delivers one decision-response to live asker.
	f.srv.decisionClock = func() time.Time { return availAt }
	dsmRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"reason":  "  Stale: the release already shipped  ",
		"surface": store.DecisionSurfaceDecisionsPage,
	})
	dsmOut := decodeDismissResponse(t, dsmRR)
	assert.True(t, dsmOut.Delivered)
	require.NotEmpty(t, dsmOut.ResponseMessageID)
	require.Len(t, dsmOut.Dismissed, 2)
	assert.Empty(t, dsmOut.AlreadyDismissed)
	for i, v := range dsmOut.Dismissed {
		assert.Equal(t, store.DecisionStatusDismissed, v.Status, "row %d", i)
		assert.Nil(t, v.Decision, "dismissed row must have nil effective answer")
		require.NotNil(t, v.Dismissed)
		assert.Equal(t, f.bob.ID, v.Dismissed.ByUserID)
		assert.Equal(t, userDisplay(authUser(f.bob)), v.Dismissed.ByName)
		assert.Equal(t, "Stale: the release already shipped", v.Dismissed.Reason)
		assert.Equal(t, store.DecisionSurfaceDecisionsPage, v.Dismissed.Surface)
		assert.Equal(t, dsmOut.ResponseMessageID, v.Dismissed.ResponseMessageID)
	}

	// Verify 2 chat.decision.updated events published on the project subject.
	evts := collectEvents(updEvents)
	require.Len(t, evts, 2)

	// Verify live asker received one decision-response with design 4.5 line + metadata.
	sent := f.dispatcher.getMessages()[beforeMsgs:]
	require.Len(t, sent, 1)
	sm := sent[0].structured
	require.NotNil(t, sm)
	assert.Equal(t, messages.TypeDecisionResponse, sm.Type)
	assert.Equal(t, userDisplay(authUser(f.bob))+` dismissed D234 ("Ship the **feature** now?"), 1 ("Which approach?"). Reason: Stale: the release already shipped`, sm.Msg)
	assert.NotEmpty(t, sm.Metadata[messages.MetadataDecisionBatch])
	assert.Equal(t, "Stale: the release already shipped", sm.Metadata[messages.MetadataDecisionDismissReason])
	assert.Empty(t, sm.Metadata[messages.MetadataDecisionKey], "set dismissal omits scalar decision_key")

	var ansMeta []decisionDismissAnswerMeta
	require.NoError(t, json.Unmarshal([]byte(sm.Metadata[messages.MetadataDecisionAnswers]), &ansMeta))
	require.Len(t, ansMeta, 2)
	assert.Equal(t, decisionDismissAnswerMeta{
		Key: "D234", Version: 1, ChoiceID: messages.ChoiceIDDismissed,
		Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeDismiss,
	}, ansMeta[0])
	assert.Equal(t, decisionDismissAnswerMeta{
		Key: "1", Version: 1, ChoiceID: messages.ChoiceIDDismissed,
		Provenance: store.DecisionProvenanceDirect, Outcome: store.DecisionOutcomeDismiss,
	}, ansMeta[1])

	// Repeating the call returns alreadyDismissed and dispatches nothing new.
	retryOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"reason": "Ignored on retry",
	}))
	assert.Empty(t, retryOut.Dismissed)
	require.Len(t, retryOut.AlreadyDismissed, 2)
	assert.False(t, retryOut.Delivered)
	assert.Equal(t, dsmOut.ResponseMessageID, retryOut.ResponseMessageID)
	assert.Len(t, f.dispatcher.getMessages()[beforeMsgs:], 1)

	// Set answer on dismissed 2-row prompt -> 409 answer_set_changed with reason "dismissed".
	setAnsRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+twoMsg.ID+"/answers", map[string]interface{}{
		"answers": []map[string]interface{}{
			{"decision_row_id": twoRes.Decisions[0].RowID, "choice_id": "1"},
			{"decision_row_id": twoRes.Decisions[1].RowID, "choice_id": "a"},
		},
	})
	require.Equal(t, http.StatusConflict, setAnsRR.Code, setAnsRR.Body.String())
	setCode, setDetails := errorCodeAndDetails(t, setAnsRR)
	assert.Equal(t, ErrCodeAnswerSetChanged, setCode)
	reasonsMap, ok := setDetails["reasons"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, store.DecisionStatusDismissed, reasonsMap[twoRes.Decisions[0].RowID])
	assert.Equal(t, store.DecisionStatusDismissed, reasonsMap[twoRes.Decisions[1].RowID])

	// 3. Single-row prompt dismissed at +5m -> sets scalar metadata keys AND single answer gets 409 dismissed!
	dOne, err := f.store.GetDecision(ctx, oneRowID)
	require.NoError(t, err)
	f.srv.decisionClock = func() time.Time { return dOne.CreatedAt.Add(store.DecisionDismissMinAge) }
	beforeOne := len(f.dispatcher.getMessages())
	oneDsmOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+oneMsg.ID+"/dismiss", nil))
	assert.True(t, oneDsmOut.Delivered)
	require.Len(t, oneDsmOut.Dismissed, 1)

	sentOne := f.dispatcher.getMessages()[beforeOne:]
	require.Len(t, sentOne, 1)
	smOne := sentOne[0].structured
	require.NotNil(t, smOne)
	assert.Equal(t, userDisplay(authUser(f.bob))+` dismissed D-SINGLE ("Ship single?").`, smOne.Msg)
	assert.Equal(t, "D-SINGLE", smOne.Metadata[messages.MetadataDecisionKey])
	assert.Equal(t, "1", smOne.Metadata[messages.MetadataDecisionVersion])
	assert.Equal(t, messages.ChoiceIDDismissed, smOne.Metadata[messages.MetadataChoiceID])
	assert.Equal(t, store.DecisionProvenanceDirect, smOne.Metadata[messages.MetadataDecisionProvenance])
	_, hasReason := smOne.Metadata[messages.MetadataDecisionDismissReason]
	assert.False(t, hasReason, "empty reason omits decision_dismiss_reason")

	// Forced single answer -> 409 dismissed with details.decision.
	ansRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decisions/"+oneRowID+"/answer", map[string]interface{}{
		"choice_id": "1",
	})
	require.Equal(t, http.StatusConflict, ansRR.Code, ansRR.Body.String())
	ansCode, ansDetails := errorCodeAndDetails(t, ansRR)
	assert.Equal(t, ErrCodeDecisionDismissed, ansCode)
	require.NotNil(t, ansDetails["decision"])

	// Both prompts are now absent from project and profile Waiting on you!
	projAfter := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/decisions?tab=pending", nil))
	assert.Equal(t, ProjectDecisionCounts{}, projAfter.Counts)
	assert.Empty(t, projAfter.Prompts)

	profAfter := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/users/me/decisions?tab=pending", nil)
	require.Equal(t, http.StatusOK, profAfter.Code, profAfter.Body.String())
	var profBody ProfilePendingDecisionsResponse
	require.NoError(t, json.Unmarshal(profAfter.Body.Bytes(), &profBody))
	assert.Equal(t, ProjectDecisionCounts{}, profBody.Counts)
	assert.Empty(t, profBody.Prompts)
}

func TestDecisionPromptDismiss_GoneAsker(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	// Create two prompts before deleting the asking agent: one for soft-delete, one for hard-delete.
	softMsg, softRes := f.topicPrompt(t, `[{"id":"GONE-1","question":"Soft deleted asker?","choices":["yes","no"]}]`, false)
	hardMsg, hardRes := f.topicPrompt(t, `[{"id":"GONE-2","question":"Hard deleted asker?","choices":["yes","no"]}]`, false)
	dSoft, err := f.store.GetDecision(ctx, softRes.Decisions[0].RowID)
	require.NoError(t, err)
	dHard, err := f.store.GetDecision(ctx, hardRes.Decisions[0].RowID)
	require.NoError(t, err)
	f.srv.decisionClock = func() time.Time {
		return dHard.CreatedAt.Add(store.DecisionDismissMinAge)
	}
	_ = dSoft

	before := len(f.dispatcher.getMessages())

	// 1. Soft-deleted asking agent -> 200 delivered=false, stored, NOT marked failed, NOT dispatched.
	f.agent.DeletedAt = time.Now().UTC()
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	softOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+softMsg.ID+"/dismiss", map[string]interface{}{
		"reason": "Agent was soft-deleted",
	}))
	assert.False(t, softOut.Delivered)
	require.Len(t, softOut.Dismissed, 1)
	assert.Equal(t, store.DecisionStatusDismissed, softOut.Dismissed[0].Status)

	storedSoft, err := f.store.GetMessage(ctx, softOut.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, storedSoft.Type)
	assert.Equal(t, store.MessageDispatchDispatched, storedSoft.DispatchState, "gone asker dismissal must not be marked failed")
	assert.Nil(t, storedSoft.DispatchFailureReason)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing delivered to soft-deleted asker")

	// 2. Hard-deleted asking agent -> 200 delivered=false, stored, NOT marked failed, NOT dispatched.
	require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))

	hardOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+hardMsg.ID+"/dismiss", map[string]interface{}{
		"reason": "Agent was deleted",
	}))
	assert.False(t, hardOut.Delivered)
	require.Len(t, hardOut.Dismissed, 1)
	assert.Equal(t, store.DecisionStatusDismissed, hardOut.Dismissed[0].Status)

	storedHard, err := f.store.GetMessage(ctx, hardOut.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, storedHard.Type)
	assert.Equal(t, store.MessageDispatchDispatched, storedHard.DispatchState, "hard-deleted asker dismissal must not be marked failed")
	assert.Nil(t, storedHard.DispatchFailureReason)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing delivered to hard-deleted asker")
}

func TestDecisionPromptDismiss_MetadataStripsReservedKeys(t *testing.T) {
	raw := map[string]string{
		messages.MetadataDecisionKey:           "spoof-key",
		messages.MetadataDecisionVersion:       "999",
		messages.MetadataChoiceID:              "spoof-choice",
		messages.MetadataDecisionProvenance:    "spoof-prov",
		messages.MetadataDecisionBatch:         "spoof-batch",
		messages.MetadataDecisionAnswers:       "spoof-answers",
		messages.MetadataDecisionDismissReason: "spoof-reason",
		messages.MetadataSenderVisitor:         "true",
		"custom_unreserved":                    "kept",
	}
	answers := []*store.DecisionAnswer{
		{
			Key:          "D1",
			Version:      1,
			ChoiceID:     messages.ChoiceIDDismissed,
			Provenance:   store.DecisionProvenanceDirect,
			Outcome:      store.DecisionOutcomeDismiss,
			QuestionText: "Real?",
		},
		{
			Key:          "D2",
			Version:      1,
			ChoiceID:     messages.ChoiceIDDismissed,
			Provenance:   store.DecisionProvenanceDirect,
			Outcome:      store.DecisionOutcomeDismiss,
			QuestionText: "Real 2?",
		},
	}
	cleaned := decisionDismissMetadata(raw, "real-batch", answers)
	assert.Equal(t, "real-batch", cleaned[messages.MetadataDecisionBatch])
	assert.Empty(t, cleaned[messages.MetadataDecisionKey], "spoofed scalar key stripped on multi-row dismiss")
	assert.Empty(t, cleaned[messages.MetadataDecisionDismissReason], "spoofed dismiss reason stripped when reason is empty")
	assert.Empty(t, cleaned[messages.MetadataSenderVisitor], "reserved sender_visitor stripped")
	assert.Equal(t, "kept", cleaned["custom_unreserved"])
}
