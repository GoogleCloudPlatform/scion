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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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

	// Explicit ids at 4m59s -> 409 dismiss_changed with reason too_recent and
	// details.availableAt, so the page can re-arm its unlock timer.
	earlyIDsRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, twoDismissPath, map[string]interface{}{
		"decisionIds": []string{twoRes.Decisions[0].RowID, twoRes.Decisions[1].RowID},
	})
	require.Equal(t, http.StatusConflict, earlyIDsRR.Code, earlyIDsRR.Body.String())
	idsCode, idsDetails := errorCodeAndDetails(t, earlyIDsRR)
	assert.Equal(t, ErrCodeDismissChanged, idsCode)
	idsReasons, ok := idsDetails["reasons"].(map[string]interface{})
	require.True(t, ok, "details: %+v", idsDetails)
	assert.Equal(t, store.AnswerSetReasonTooRecent, idsReasons[twoRes.Decisions[0].RowID])
	idsAvailStr, ok := idsDetails["availableAt"].(string)
	require.True(t, ok, "details: %+v", idsDetails)
	idsAvail, err := time.Parse(time.RFC3339Nano, idsAvailStr)
	require.NoError(t, err)
	assert.True(t, availAt.Equal(idsAvail), "want %v, got %v", availAt, idsAvail)
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
	assert.Equal(t, DismissDeliveryDelivered, dsmOut.DeliveryStatus)
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
	assert.Equal(t, DismissDeliveryNone, retryOut.DeliveryStatus, "nothing new dismissed: no delivery claim")
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
	assert.Equal(t, DismissDeliveryGone, softOut.DeliveryStatus)
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
	assert.Equal(t, DismissDeliveryGone, hardOut.DeliveryStatus)
	require.Len(t, hardOut.Dismissed, 1)
	assert.Equal(t, store.DecisionStatusDismissed, hardOut.Dismissed[0].Status)

	storedHard, err := f.store.GetMessage(ctx, hardOut.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeDecisionResponse, storedHard.Type)
	assert.Equal(t, store.MessageDispatchDispatched, storedHard.DispatchState, "hard-deleted asker dismissal must not be marked failed")
	assert.Nil(t, storedHard.DispatchFailureReason)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing delivered to hard-deleted asker")
}

// TestDecisionPromptDismiss_DeliveryStatus pins why nothing was delivered
// (ptone/scion#8213): a suspended asker gets its response stored failed and
// not dispatched (unreachable, also on replay); a dismiss that loses the race
// to another dismissal reports none.
func TestDecisionPromptDismiss_DeliveryStatus(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	suspMsg, suspRes := f.topicPrompt(t, `[{"id":"SUSP-1","question":"Suspended asker?","choices":["yes","no"]}]`, false)
	raceMsg, raceRes := f.topicPrompt(t, `[{"id":"RACE-1","question":"Raced?","choices":["yes","no"]}]`, false)
	dSusp, err := f.store.GetDecision(ctx, suspRes.Decisions[0].RowID)
	require.NoError(t, err)
	f.srv.decisionClock = func() time.Time { return dSusp.CreatedAt.Add(store.DecisionDismissMinAge + time.Minute) }

	// 1. Race: Alice dismisses first; Bob's dismiss of the same rows then finds
	//    nothing new to dismiss -> none, no delivery claim, nothing dispatched.
	aliceOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+raceMsg.ID+"/dismiss", nil))
	require.Len(t, aliceOut.Dismissed, 1)
	assert.Equal(t, DismissDeliveryDelivered, aliceOut.DeliveryStatus)
	beforeRace := len(f.dispatcher.getMessages())
	bobOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+raceMsg.ID+"/dismiss", map[string]interface{}{
		"decisionIds": []string{raceRes.Decisions[0].RowID},
	}))
	assert.Empty(t, bobOut.Dismissed)
	require.Len(t, bobOut.AlreadyDismissed, 1)
	assert.False(t, bobOut.Delivered)
	assert.Equal(t, DismissDeliveryNone, bobOut.DeliveryStatus)
	assert.Len(t, f.dispatcher.getMessages(), beforeRace)

	// 2. Suspended asker -> stored failed with the unreachable reason, not
	//    dispatched, deliveryStatus unreachable.
	f.agent.Phase = string(state.PhaseSuspended)
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))
	before := len(f.dispatcher.getMessages())
	suspOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+suspMsg.ID+"/dismiss", map[string]interface{}{
		"reason":         "Stale",
		"idempotencyKey": "susp-idem-1",
	}))
	require.Len(t, suspOut.Dismissed, 1)
	assert.False(t, suspOut.Delivered)
	assert.Equal(t, DismissDeliveryUnreachable, suspOut.DeliveryStatus)
	assert.Len(t, f.dispatcher.getMessages(), before, "nothing dispatched to a suspended asker")
	stored, err := f.store.GetMessage(ctx, suspOut.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, stored.DispatchState)
	require.NotNil(t, stored.DispatchFailureReason)
	assert.Equal(t, agentUnreachableReason(string(state.PhaseSuspended)), *stored.DispatchFailureReason)

	// 3. Replay returns the original status.
	replayOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/decision-prompts/"+suspMsg.ID+"/dismiss", map[string]interface{}{
		"reason":         "Stale",
		"idempotencyKey": "susp-idem-1",
	}))
	assert.Equal(t, suspOut.ResponseMessageID, replayOut.ResponseMessageID)
	assert.Equal(t, DismissDeliveryUnreachable, replayOut.DeliveryStatus)
	assert.False(t, replayOut.Delivered)
}

func TestDismissReplayDeliveryStatus(t *testing.T) {
	unreachable := agentUnreachableReason("stopped")
	require.True(t, strings.HasPrefix(unreachable, agentUnreachableReasonPrefix), "prefix must track agentUnreachableReason")
	other := "broker timeout"
	cases := []struct {
		name      string
		askerGone bool
		m         *store.Message
		want      string
	}{
		{"gone", true, nil, DismissDeliveryGone},
		{"lookup failed", false, nil, DismissDeliveryFailed},
		{"dispatched", false, &store.Message{DispatchState: store.MessageDispatchDispatched}, DismissDeliveryDelivered},
		{"delivered", false, &store.Message{DispatchState: store.MessageDispatchDelivered}, DismissDeliveryDelivered},
		{"held", false, &store.Message{DispatchState: store.MessageDispatchAwaitingSession}, DismissDeliveryHeld},
		{"unreachable", false, &store.Message{DispatchState: store.MessageDispatchFailed, DispatchFailureReason: &unreachable}, DismissDeliveryUnreachable},
		{"failed", false, &store.Message{DispatchState: store.MessageDispatchFailed, DispatchFailureReason: &other}, DismissDeliveryFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, dismissReplayDeliveryStatus(tc.askerGone, tc.m))
		})
	}
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

func TestDecisionPromptDismiss_SubsetsIdempotencyInterpretedMadeAndAgentEndpoints(t *testing.T) {
	f := newDecisionsFixture(t)
	ctx := context.Background()

	setMsg, setRes := f.topicPrompt(t, `[
		{"id":"H-SUB-1","question":"Ship Q1?","choices":["yes","no"]},
		{"id":"H-SUB-2","question":"Ship Q2?","choices":["yes","no"]},
		{"id":"H-SUB-3","question":"Ship Q3?","choices":["yes","no"]}
	]`, false)
	r0 := setRes.Decisions[0].RowID
	r1 := setRes.Decisions[1].RowID
	r2 := setRes.Decisions[2].RowID
	dismissPath := "/api/v1/decision-prompts/" + setMsg.ID + "/dismiss"

	d0, err := f.store.GetDecision(ctx, r0)
	require.NoError(t, err)
	t5m := d0.CreatedAt.Add(store.DecisionDismissMinAge)
	f.srv.decisionClock = func() time.Time { return t5m }

	// 1. Validation 400s: idempotencyKey > 128 bytes, >10 decisionIds, unknown id, duplicate id.
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"idempotencyKey": strings.Repeat("x", store.MaxDecisionIdempotencyKeyLen+1),
	}).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds": []string{"id0", "id1", "id2", "id3", "id4", "id5", "id6", "id7", "id8", "id9", "id10"},
	}).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds": []string{api.NewUUID()},
	}).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds": []string{r0, r0},
	}).Code)

	// 2. Agent records interpreted answer naming Alice on H-SUB-1.
	aliceReply := f.storeUserReply(t, f.alice, f.topicID, setMsg.ConversationID, "web", "yes to Q1")
	interpRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", map[string]interface{}{
		"source_message_id": aliceReply.ID,
		"answers": []map[string]string{
			{"key": "H-SUB-1", "choice_id": "1"},
		},
	})
	require.Equal(t, http.StatusOK, interpRR.Code, interpRR.Body.String())

	// Bob tries to dismiss [r0, r1] -> 409 dismiss_changed (r0 is not_recorded_answerer), nothing stored!
	beforeDispatch := len(f.dispatcher.getMessages())
	bobDismissRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds": []string{r0, r1},
		"reason":      "Bob tried",
	})
	require.Equal(t, http.StatusConflict, bobDismissRR.Code, bobDismissRR.Body.String())
	chgCode, chgDetails := errorCodeAndDetails(t, bobDismissRR)
	assert.Equal(t, ErrCodeDismissChanged, chgCode)
	reasonsMap, ok := chgDetails["reasons"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, store.AnswerSetReasonNotRecordedAnswerer, reasonsMap[r0])
	_, hasAvail := chgDetails["availableAt"]
	assert.False(t, hasAvail, "no too-recent row: no availableAt")
	assert.Len(t, f.dispatcher.getMessages(), beforeDispatch)

	// 3. Alice dismisses subset [r0, r1] with idempotencyKey and reason -> 200, agent line includes "(your reading: yes)".
	aliceOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds":    []string{r0, r1},
		"reason":         "  Misread + stale — quartz-token  ",
		"surface":        store.DecisionSurfaceDecisionsPage,
		"idempotencyKey": "hub-idem-key-1",
	}))
	assert.True(t, aliceOut.Delivered)
	assert.Equal(t, DismissDeliveryDelivered, aliceOut.DeliveryStatus)
	require.Len(t, aliceOut.Dismissed, 2)
	assert.Empty(t, aliceOut.AlreadyDismissed)

	sent := f.dispatcher.getMessages()[beforeDispatch:]
	require.Len(t, sent, 1)
	assert.Equal(t,
		userDisplay(authUser(f.alice))+` dismissed H-SUB-1 ("Ship Q1?") (your reading: yes), H-SUB-2 ("Ship Q2?"). Reason: Misread + stale — quartz-token`,
		sent[0].structured.Msg,
	)

	// 4. Idempotent replay returns identical 200 without re-delivering; altered reason or ids -> 422 idempotency_mismatch.
	replayOut := decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds":    []string{r0, r1},
		"reason":         "Misread + stale — quartz-token",
		"idempotencyKey": "hub-idem-key-1",
	}))
	assert.Equal(t, aliceOut.ResponseMessageID, replayOut.ResponseMessageID)
	assert.True(t, replayOut.Delivered)
	assert.Equal(t, DismissDeliveryDelivered, replayOut.DeliveryStatus, "replay returns the original status")
	require.Len(t, replayOut.Dismissed, 2)
	assert.Len(t, f.dispatcher.getMessages()[beforeDispatch:], 1, "idempotent replay must not re-deliver")

	mismatchRR := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, dismissPath, map[string]interface{}{
		"decisionIds":    []string{r0},
		"reason":         "Misread + stale — quartz-token",
		"idempotencyKey": "hub-idem-key-1",
	})
	require.Equal(t, http.StatusUnprocessableEntity, mismatchRR.Code, mismatchRR.Body.String())
	assert.Equal(t, ErrCodeIdempotencyMismatch, errorCode(t, mismatchRR))

	// 5. H-SUB-3 remains open in Waiting on you (count 1) and can be submitted as the remaining actionable set!
	projPending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/decisions?tab=pending", nil))
	assert.Equal(t, 1, projPending.Counts.WaitingOnYou)

	remSetRR := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+setMsg.ID+"/answers", map[string]interface{}{
		"answers": []map[string]interface{}{
			{"decision_row_id": r2, "choice_id": "2"},
		},
	})
	require.Equal(t, http.StatusOK, remSetRR.Code, remSetRR.Body.String())

	// 6. Agent interpreted answer on dismissed H-SUB-1 -> 409 dismissed.
	interpDismissedRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/interpreted", map[string]interface{}{
		"source_message_id": aliceReply.ID,
		"answers": []map[string]string{
			{"key": "H-SUB-1", "choice_id": "2"},
		},
	})
	require.Equal(t, http.StatusConflict, interpDismissedRR.Code, interpDismissedRR.Body.String())
	assert.Equal(t, ErrCodeDecisionDismissed, errorCode(t, interpDismissedRR))
	assert.Contains(t, interpDismissedRR.Body.String(), "H-SUB-1: dismissed by "+userDisplay(authUser(f.alice)))

	// 7. Withdraw on dismissed H-SUB-2 fails with per-key "dismissed"; withdraw wholeSet skips dismissed siblings!
	wdFailRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", map[string]interface{}{
		"keys": []string{"H-SUB-2"},
	})
	require.Equal(t, http.StatusConflict, wdFailRR.Code, wdFailRR.Body.String())
	assert.Equal(t, ErrCodeWithdrawFailed, errorCode(t, wdFailRR))
	assert.Contains(t, wdFailRR.Body.String(), "H-SUB-2: dismissed by "+userDisplay(authUser(f.alice)))

	wdMsg, wdRes := f.topicPrompt(t, `[
		{"id":"H-WD-1","question":"Dismissed sibling?","choices":["yes","no"]},
		{"id":"H-WD-2","question":"Pending sibling?","choices":["yes","no"]}
	]`, false)
	dWd, err := f.store.GetDecision(ctx, wdRes.Decisions[0].RowID)
	require.NoError(t, err)
	f.srv.decisionClock = func() time.Time { return dWd.CreatedAt.Add(store.DecisionDismissMinAge) }
	decodeDismissResponse(t, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/decision-prompts/"+wdMsg.ID+"/dismiss", map[string]interface{}{
		"decisionIds": []string{wdRes.Decisions[0].RowID},
	}))

	wdSetRR := f.agentDecisionReq(t, f.agent, http.MethodPost, "/api/v1/agent/decisions/withdraw", map[string]interface{}{
		"keys":     []string{"H-WD-2"},
		"wholeSet": true,
		"reason":   "Retire open rest",
	})
	require.Equal(t, http.StatusOK, wdSetRR.Code, wdSetRR.Body.String())
	var wdSetOut WithdrawDecisionsResponse
	require.NoError(t, json.Unmarshal(wdSetRR.Body.Bytes(), &wdSetOut))
	require.Len(t, wdSetOut.Withdrawn, 1)
	assert.Equal(t, "H-WD-2", wdSetOut.Withdrawn[0].Key)

	// 8. Reuse of dismissed key without --update gives hint "H-SUB-2 already exists (v1, dismissed by <Alice>)";
	// --update over dismissed key creates v2!
	reuseRR := f.sendAsAgent(t, OutboundMessageRequest{
		Recipient: "user:" + f.bob.Email,
		Msg:       "Re-asking without update",
		Decisions: decisionInputs(t, `[{"id":"H-SUB-2","question":"Again?","choices":["yes","no"]}]`),
	})
	require.Equal(t, http.StatusConflict, reuseRR.Code, reuseRR.Body.String())
	assert.Equal(t, ErrCodeDecisionKeyExists, errorCode(t, reuseRR))
	assert.Contains(t, reuseRR.Body.String(), "H-SUB-2 already exists (v1, dismissed by "+userDisplay(authUser(f.alice))+")")

	// 9. Project and profile Made endpoints: provenance=dismissed, dismissReason field, and q over dismiss_reason!
	projMadeDsm := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/decisions?tab=made&provenance=dismissed", nil))
	require.Len(t, projMadeDsm.Entries, 3) // H-WD-1, H-SUB-2, H-SUB-1
	for _, e := range projMadeDsm.Entries {
		assert.Equal(t, store.DecisionOutcomeDismiss, e.Outcome)
	}

	projMadeSearch := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/decisions?tab=made&q=quartz-token", nil))
	require.Len(t, projMadeSearch.Entries, 2)
	assert.Equal(t, "Misread + stale — quartz-token", projMadeSearch.Entries[0].DismissReason)

	// Unfiltered Made folds the interpreted answer into the dismissal entry on H-SUB-1 (design-8443 D2)!
	projMadeAll := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
		"/api/v1/projects/"+f.project.ID+"/decisions?tab=made&q=H-SUB-1", nil))
	require.Len(t, projMadeAll.Entries, 1)
	assert.Equal(t, store.DecisionOutcomeDismiss, projMadeAll.Entries[0].Outcome)
	assert.Equal(t, store.DecisionStatusDismissed, projMadeAll.Entries[0].Decision.Status)
	require.NotEmpty(t, projMadeAll.Entries[0].Decision.Choices)
	assert.False(t, projMadeAll.Entries[0].AwaitingConfirmation)
	require.NotNil(t, projMadeAll.Entries[0].InterpretedAnswer)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, projMadeAll.Entries[0].InterpretedAnswer.Provenance)

	profMadeRR := doRequestAsUser(t, f.srv, f.alice, http.MethodGet,
		"/api/v1/users/me/decisions?tab=made&provenance=dismissed&q=quartz-token", nil)
	require.Equal(t, http.StatusOK, profMadeRR.Code, profMadeRR.Body.String())
	assert.Contains(t, profMadeRR.Body.String(), "quartz-token")

	// 10. Agent list state=dismissed and show H-SUB-1.
	listRR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions?state=dismissed", nil)
	require.Equal(t, http.StatusOK, listRR.Code, listRR.Body.String())
	var agentList AgentDecisionListResponse
	require.NoError(t, json.Unmarshal(listRR.Body.Bytes(), &agentList))
	require.Len(t, agentList.Items, 3)
	for _, it := range agentList.Items {
		assert.Equal(t, store.DecisionStatusDismissed, it.Status)
		assert.Nil(t, it.Answer)
		require.NotNil(t, it.Dismissed)
	}

	showRR := f.agentDecisionReq(t, f.agent, http.MethodGet, "/api/v1/agent/decisions/H-SUB-1", nil)
	require.Equal(t, http.StatusOK, showRR.Code, showRR.Body.String())
	var agentShow AgentDecisionShowResponse
	require.NoError(t, json.Unmarshal(showRR.Body.Bytes(), &agentShow))
	require.Len(t, agentShow.Versions, 1)
	assert.Equal(t, store.DecisionStatusDismissed, agentShow.Versions[0].Status)
	require.NotNil(t, agentShow.Versions[0].Dismissed)
	assert.Equal(t, "Misread + stale — quartz-token", agentShow.Versions[0].Dismissed.Reason)
	require.Len(t, agentShow.Versions[0].Answers, 2)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, agentShow.Versions[0].Answers[0].Provenance)
	assert.Equal(t, store.DecisionOutcomeDismiss, agentShow.Versions[0].Answers[1].Outcome)
	assert.Equal(t, "Misread + stale — quartz-token", agentShow.Versions[0].Answers[1].DismissReason)
}

func TestDecisionPromptDismiss_TestModeMinAge(t *testing.T) {
	// 1. ValidateTestModeConfig refuses any non-zero override when TestMode is off
	//    and refuses negative values in all modes.
	assert.ErrorIs(t, ValidateTestModeConfig(ServerConfig{
		TestDecisionDismissMinAge: 5 * time.Second,
	}), ErrTestDecisionDismissMinAgeRequiresTestMode)
	assert.ErrorIs(t, ValidateTestModeConfig(ServerConfig{
		TestMode:                  TestModeConfig{Level: TestModeOff},
		TestDecisionDismissMinAge: 5 * time.Second,
	}), ErrTestDecisionDismissMinAgeRequiresTestMode)
	assert.ErrorIs(t, ValidateTestModeConfig(ServerConfig{
		TestDecisionDismissMinAge: -time.Second,
	}), ErrTestDecisionDismissMinAgeInvalid)
	assert.NoError(t, ValidateTestModeConfig(ServerConfig{
		HubID:                     "hub-test",
		HubIDPinned:               true,
		EnableTestIdentities:      true,
		TestDecisionDismissMinAge: 5 * time.Second,
		TestMode: TestModeConfig{
			Level:       TestModeMember,
			HubID:       "hub-test",
			HubClass:    "test",
			MarkerHubID: "hub-test",
			MarkerClass: "test",
		},
	}))

	// 2. Runtime enforcement: outside test mode, even a directly-populated
	//    TestDecisionDismissMinAge is ignored and the 5-minute floor holds.
	f := newChatFixture(t)
	ctx := context.Background()
	setDecisionsExperiment(t, f.srv, true)
	f.agentA.OwnerID = f.owner
	f.agentA.Ancestry = []string{f.owner}
	f.agentA.MessageMode = store.MessageModeProject
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))

	seedPrompt := func(key string) (string, *store.Decision) {
		decs, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"`+key+`","question":"Choice?","choices":["yes","no"]}]`))
		require.NoError(t, err)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
			Recipient: "chat", ThreadID: f.topicA, Channel: "web",
		}
		res, err := f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: msg, Intro: "Pick", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decs,
		})
		require.NoError(t, err)
		d, err := f.s.GetDecision(ctx, res.Decisions[0].RowID)
		require.NoError(t, err)
		return msg.ID, d
	}

	msgDefault, dDefault := seedPrompt("D-DEF")
	f.srv.config.TestDecisionDismissMinAge = 3 * time.Second // TestMode is still off
	assert.Equal(t, store.DecisionDismissMinAge, f.srv.decisionDismissMinAge())

	f.srv.decisionClock = func() time.Time { return dDefault.CreatedAt.Add(10 * time.Second) }
	rr := f.session(t, f.owner, http.MethodPost, "/api/v1/decision-prompts/"+msgDefault+"/dismiss", map[string]any{})
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeDecisionTooRecent)
	defaultView := f.srv.decisionViewFor(ctx, dDefault)
	require.NotNil(t, defaultView.DismissAvailableAt)
	assert.True(t, dDefault.CreatedAt.Add(store.DecisionDismissMinAge).Equal(*defaultView.DismissAvailableAt))

	// 3. Inside test mode, TestDecisionDismissMinAge governs both dismiss
	//    enforcement and DecisionView.DismissAvailableAt.
	f.srv.testMode = newTestModeState(TestModeConfig{Level: TestModeMember})
	assert.Equal(t, 3*time.Second, f.srv.decisionDismissMinAge())

	msgTest, dTest := seedPrompt("D-TST")
	testView := f.srv.decisionViewFor(ctx, dTest)
	require.NotNil(t, testView.DismissAvailableAt)
	assert.True(t, dTest.CreatedAt.Add(3*time.Second).Equal(*testView.DismissAvailableAt))

	f.srv.decisionClock = func() time.Time { return dTest.CreatedAt.Add(2 * time.Second) }
	rrEarly := f.session(t, f.owner, http.MethodPost, "/api/v1/decision-prompts/"+msgTest+"/dismiss", map[string]any{})
	require.Equal(t, http.StatusConflict, rrEarly.Code, rrEarly.Body.String())
	assert.Contains(t, rrEarly.Body.String(), ErrCodeDecisionTooRecent)
	assert.Contains(t, rrEarly.Body.String(), dTest.CreatedAt.Add(3*time.Second).UTC().Format(time.RFC3339Nano))

	f.srv.decisionClock = func() time.Time { return dTest.CreatedAt.Add(3 * time.Second) }
	rrOk := f.session(t, f.owner, http.MethodPost, "/api/v1/decision-prompts/"+msgTest+"/dismiss", map[string]any{"reason": "short window"})
	out := decodeDismissResponse(t, rrOk)
	require.Len(t, out.Dismissed, 1)
	assert.Equal(t, store.DecisionStatusDismissed, out.Dismissed[0].Status)
}
