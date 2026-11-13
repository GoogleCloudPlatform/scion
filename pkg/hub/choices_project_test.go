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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setProjectDecisionsExperiments(t *testing.T, srv *Server, chatChoices, projectDecisions bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.ChatScheduledSend: true,
		experiments.ChatChoices:       chatChoices,
		experiments.ProjectDecisions:  projectDecisions,
	})
	require.Equal(t, chatChoices, srv.experimentEnabled(experiments.ChatChoices))
	require.Equal(t, projectDecisions, srv.experimentEnabled(experiments.ProjectDecisions))
}

func decodePendingDecisions(t *testing.T, rr *httptest.ResponseRecorder) ProjectPendingDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out ProjectPendingDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func decodeMadeDecisions(t *testing.T, rr *httptest.ResponseRecorder) ProjectMadeDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out ProjectMadeDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

type testRetentionProvider interface {
	DB() *sql.DB
	ChatRetention() store.ChatRetentionStore
}

func purgeMessages(t *testing.T, s store.Store, projectID string, msgIDs ...string) {
	t.Helper()
	ctx := context.Background()
	provider, ok := s.(testRetentionProvider)
	require.True(t, ok, "store %T must expose ChatRetention() and DB()", s)
	require.NoError(t, NewWebChatStore(provider.DB(), "sqlite").Init())
	projOf := make(map[string]string, len(msgIDs))
	for _, id := range msgIDs {
		projOf[id] = projectID
	}
	_, err := provider.ChatRetention().PurgeChatMessages(ctx, msgIDs, projOf, store.ChatRetentionCursor{})
	require.NoError(t, err)
	for _, id := range msgIDs {
		_, gerr := s.GetMessage(ctx, id)
		require.ErrorIs(t, gerr, store.ErrNotFound, "message %s should be purged", id)
	}
}

func TestProjectDecisions_ExperimentMatrixAndMethods(t *testing.T) {
	f := newChoicesFixture(t)
	path := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Both off -> GET 404, non-GET 405, nested suffix 404.
	setProjectDecisionsExperiments(t, f.srv, false, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path+"/extra", nil).Code)

	// Only web.chat_choices on -> GET 404.
	setProjectDecisionsExperiments(t, f.srv, true, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil).Code)

	// Only web.project_decisions on -> GET 404.
	setProjectDecisionsExperiments(t, f.srv, false, true)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil).Code)

	// Both on -> GET 200, unsupported methods 405, nested suffix 404.
	setProjectDecisionsExperiments(t, f.srv, true, true)
	resp := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil))
	assert.Equal(t, ProjectDecisionCounts{}, resp.Counts)
	assert.NotNil(t, resp.Prompts)
	assert.Empty(t, resp.Prompts)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, path, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path+"/extra", nil).Code)
}

// TestProjectDecisions_TokenAndCallerAuthz pins chat.project_decisions.list
// across session credentials, real minted UATs (project and hub boundaries,
// with and without project:read and inbox:read), wrong-project UATs,
// unauthenticated callers, agents, visitors, federated users, and non-members.
func TestProjectDecisions_TokenAndCallerAuthz(t *testing.T) {
	f := newChatFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	// Set f.member as owner/ancestor of f.agentA so f.member's pending prompts
	// pass authorizeAgentMessage and classify as WaitingOnYou.
	f.agentA.OwnerID = f.member
	f.agentA.Ancestry = []string{f.member}
	f.agentA.MessageMode = store.MessageModeProject
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))

	// Seed one topic decision in projA and one DM decision with f.member.
	decsTopic, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"topic-k","question":"Topic choice?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	topicMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
		Recipient: "chat", ThreadID: f.topicA, Channel: "web", CreatedAt: time.Now().UTC().Add(-2 * time.Minute),
	}
	_, err = f.s.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: topicMsg, Intro: "Topic intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decsTopic,
	})
	require.NoError(t, err)

	dmKey := f.dm(t, f.member, "agent", f.agentA.ID)
	decsDM, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"dm-k","question":"DM choice?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	dmMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
		Recipient: "user:" + f.member, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC().Add(-time.Minute),
	}
	_, err = f.s.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: dmMsg, Intro: "DM intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decsDM,
	})
	require.NoError(t, err)

	pathA := "/api/v1/projects/" + f.projA + "/decisions"
	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)

	// 1. Unauthenticated -> 401.
	unauthReq := httptest.NewRequest(http.MethodGet, pathA, nil)
	unauthRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauthRec, unauthReq)
	assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	// 2. Agent caller -> 403.
	agentReq := httptest.NewRequest(http.MethodGet, pathA, nil)
	agentReq = agentReq.WithContext(agentCtx(agentReq.Context(), f.agentA))
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code)

	// 3. Visitor -> 403.
	visitor := NewVisitorIdentity(tid("proj-dec-visitor"), "https://issuer.test", "sub-v", "v@test.com", "Visitor")
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visitor, http.MethodGet, pathA, nil).Code)

	// 4. Federated user -> 403.
	fed := NewFederatedUserIdentity("https://issuer.test", "sub-fed", memberUser.Email, "Fed Member", "member", nil)
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, fed, http.MethodGet, pathA, nil).Code)

	// 5. Non-member plain user -> 403; nonexistent project -> 404.
	outsiderID := f.plainUser(t)
	outsiderUser, err := f.s.GetUser(ctx, outsiderID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, f.srv, outsiderUser, http.MethodGet, pathA, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, "/api/v1/projects/"+uuid.NewString()+"/decisions", nil).Code)

	// 6. Session user (member) -> 200, sees BOTH topic and own DM (canAnswer=true).
	sessResp := decodePendingDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, pathA, nil))
	assert.Equal(t, 2, sessResp.Counts.WaitingOnYou)
	require.Len(t, sessResp.Prompts, 2)
	assert.True(t, sessResp.Prompts[0].CanAnswer)

	// 7. Project-scoped UAT with project:read only (no inbox:read) -> 200, sees topic ONLY, NOT DM!
	tokProjReadOnly := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
	uatProjResp := decodePendingDecisions(t, f.call(t, tokProjReadOnly, http.MethodGet, pathA, nil))
	assert.Equal(t, 1, uatProjResp.Counts.WaitingOnYou, "without inbox:read, UAT sees topic only")
	require.Len(t, uatProjResp.Prompts, 1)
	assert.Equal(t, f.topicA, uatProjResp.Prompts[0].ChatKey)
	assert.False(t, uatProjResp.Prompts[0].CanAnswer, "answer routes are session-only")

	// 8. Project-scoped UAT with project:read + inbox:read + agent:read -> 200, sees BOTH topic and own DM!
	tokProjWithInbox := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:read", "agent:read")
	uatProjDMResp := decodePendingDecisions(t, f.call(t, tokProjWithInbox, http.MethodGet, pathA, nil))
	assert.Equal(t, 2, uatProjDMResp.Counts.WaitingOnYou)
	require.Len(t, uatProjDMResp.Prompts, 2)

	// 9. Hub-scoped UAT: without inbox:read sees topic only; with inbox:read + agent:read sees both.
	tokHubReadOnly := f.mint(t, f.member, hubBoundary(), "project:read")
	uatHubResp := decodePendingDecisions(t, f.call(t, tokHubReadOnly, http.MethodGet, pathA, nil))
	assert.Equal(t, 1, uatHubResp.Counts.WaitingOnYou)
	require.Len(t, uatHubResp.Prompts, 1)

	tokHubWithInbox := f.mint(t, f.member, hubBoundary(), "project:read", "inbox:read", "agent:read")
	uatHubDMResp := decodePendingDecisions(t, f.call(t, tokHubWithInbox, http.MethodGet, pathA, nil))
	assert.Equal(t, 2, uatHubDMResp.Counts.WaitingOnYou)
	require.Len(t, uatHubDMResp.Prompts, 2)

	// 10. Wrong-project UAT (bounded to projB) -> 403.
	tokWrongProj := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:read", "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, tokWrongProj, http.MethodGet, pathA, nil).Code)

	// 11. UAT missing project:read selector -> 403.
	tokNoProjRead := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, tokNoProjRead, http.MethodGet, pathA, nil).Code)
}

// TestProjectDecisions_DMPrivacyAcrossTabsCountsAndFacets verifies that private
// DM decisions never leak to another project member, the project owner/admin,
// or a Hub super-admin in tab=pending, tab=made, counts, or filter facets.
func TestProjectDecisions_DMPrivacyAcrossTabsCountsAndFacets(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	// Create a second agent exclusively used in Bob's private DM so we can also
	// prove that agents/answerers facets do not leak DM-only actors.
	dmOnlyAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "secret-dm-bot", Name: "Secret DM Bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, dmOnlyAgent))

	// Create a Hub super-admin user who also has read access to the project.
	superID := tid("choices-superadmin")
	createTestUserWithRole(t, f.store, superID, "super-choices@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.store, superID)
	superUser, err := f.store.GetUser(ctx, superID)
	require.NoError(t, err)

	// 1. Topic decision in #general asked by f.agent.
	_, topicRes := f.topicPrompt(t, `[{"id":"pub-1","question":"Public release?","choices":["yes","no"]}]`, false)
	require.Len(t, topicRes.Decisions, 1)

	// 2. Private DM decision to Bob asked by dmOnlyAgent.
	dmBobKey, err := messages.DMConversationKey("agent", dmOnlyAgent.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decsDM, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"priv-bob","question":"Secret Bob DM?","choices":["approve","reject"]}]`))
	require.NoError(t, err)
	dmMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + dmOnlyAgent.Slug, SenderID: dmOnlyAgent.ID,
		Recipient: "user:" + f.bob.ID, ThreadID: dmBobKey, Channel: "web", CreatedAt: time.Now().UTC(),
	}
	dmRes, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: dmMsg, Intro: "Private DM", AskerAgentID: dmOnlyAgent.ID, AskerAgentName: dmOnlyAgent.Name, Decisions: decsDM,
	})
	require.NoError(t, err)

	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Check tab=pending for Alice (project owner) and Super Admin: ONLY pub-1, zero trace of priv-bob or dmOnlyAgent.
	for label, viewer := range map[string]*store.User{"project-owner-alice": f.alice, "hub-super-admin": superUser} {
		got := decodePendingDecisions(t, doRequestAsUser(t, f.srv, viewer, http.MethodGet, basePath+"?tab=pending", nil))
		assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1, WaitingOnOthers: 0}, got.Counts, label)
		require.Len(t, got.Prompts, 1, label)
		assert.Equal(t, "pub-1", got.Prompts[0].Decisions[0].Key, label)
		for _, ag := range got.Agents {
			assert.NotEqual(t, dmOnlyAgent.ID, ag.ID, "%s must not see DM-only agent in facets", label)
		}
	}

	// Bob sees BOTH pub-1 and priv-bob!
	bobPending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2, WaitingOnOthers: 0}, bobPending.Counts)
	require.Len(t, bobPending.Prompts, 2)
	assert.Len(t, bobPending.Agents, 2)

	// Now Bob answers his private DM decision via decisions_page.
	ansRR := f.answer(t, f.bob, dmRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	})
	require.Equal(t, http.StatusOK, ansRR.Code, "body: %s", ansRR.Body.String())

	// Check tab=made for Alice and Super Admin: ZERO entries, ZERO answerers facet leak, ZERO DM agent facet leak!
	for label, viewer := range map[string]*store.User{"project-owner-alice": f.alice, "hub-super-admin": superUser} {
		made := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, viewer, http.MethodGet, basePath+"?tab=made", nil))
		assert.Empty(t, made.Entries, "%s must not see Bob's DM answer in tab=made", label)
		assert.Empty(t, made.Answerers, "%s must not see Bob in answerers facet from a private DM answer", label)
		for _, ag := range made.Agents {
			assert.NotEqual(t, dmOnlyAgent.ID, ag.ID, label)
		}
	}

	// Bob sees his DM answer in tab=made with conversationLabel "DM with Secret DM Bot".
	bobMade := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made", nil))
	require.Len(t, bobMade.Entries, 1)
	assert.Equal(t, "priv-bob", bobMade.Entries[0].Decision.Key)
	assert.Equal(t, "DM with Secret DM Bot", bobMade.Entries[0].ConversationLabel)
	assert.True(t, bobMade.Entries[0].ConversationAvailable)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, bobMade.Entries[0].Surface)
	require.Len(t, bobMade.Answerers, 1)
	assert.Equal(t, f.bob.ID, bobMade.Answerers[0].ID)
}

func TestProjectDecisions_WaitingClassificationPaginationAndValidation(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Create a second agent with MessageModeNone ("silent-bot").
	silentBot := &store.Agent{
		ID: api.NewUUID(), Slug: "silent-bot", Name: "Silent Bot",
		ProjectID: f.project.ID, Phase: string(state.PhaseRunning), MessageMode: store.MessageModeNone,
	}
	require.NoError(t, f.store.CreateAgent(ctx, silentBot))

	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)

	// Prompt 1 (silentBot, pending -> message_denied -> WaitingOnOthers).
	dSilent, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"d-silent","question":"Silent question?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	mSilent := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + silentBot.Slug, SenderID: silentBot.ID,
		Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web",
		CreatedAt: time.Now().UTC().Add(-3 * time.Minute),
	}
	_, err = f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: mSilent, Intro: "Silent", AskerAgentID: silentBot.ID, AskerAgentName: silentBot.Name, Decisions: dSilent,
	})
	require.NoError(t, err)

	// Prompt 2 (f.agent, 2-decision set: row 0 interpreted for Alice -> other_user_confirm;
	// row 1 interpreted for Bob -> WaitingOnYou for Bob!).
	mMixed, resMixed := f.topicPrompt(t, twoDecisions, false)
	srcAlice := api.NewUUID()
	require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
		ID: srcAlice, ProjectID: f.project.ID, Sender: "user:" + f.alice.ID, SenderID: f.alice.ID,
		Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Type: messages.TypeChat, Channel: "web", Msg: "D234: yes", CreatedAt: time.Now().UTC(),
	}))
	_, err = f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agent.ID, SourceMessageID: srcAlice,
		AnsweredByUserID: f.alice.ID, AnsweredByName: f.alice.DisplayName, AnsweredByEmail: f.alice.Email,
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: resMixed.Decisions[0].RowID, ChoiceID: "1"}},
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "recorded alice" },
	})
	require.NoError(t, err)

	srcBob := api.NewUUID()
	require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
		ID: srcBob, ProjectID: f.project.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
		Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Type: messages.TypeChat, Channel: "web", Msg: "2: a", CreatedAt: time.Now().UTC(),
	}))
	_, err = f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agent.ID, SourceMessageID: srcBob,
		AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName, AnsweredByEmail: f.bob.Email,
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: resMixed.Decisions[1].RowID, ChoiceID: "a"}},
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "recorded bob" },
	})
	require.NoError(t, err)

	// Prompt 3 (f.agent, fresh pending single prompt -> WaitingOnYou for Bob).
	mFresh, _ := f.topicPrompt(t, `[{"id":"d-fresh","question":"Fresh question?","choices":["go","hold"]}]`, false)

	// As Bob (full member):
	// - d-fresh -> WaitingOnYou (1)
	// - mMixed row 1 (Bob's interpreted) -> WaitingOnYou (1); mMixed row 0 (Alice's interpreted) -> WaitingOnOthers (1)
	// - d-silent -> WaitingOnOthers (1, message_denied)
	// Total for Bob: WaitingOnYou = 2, WaitingOnOthers = 2!
	// Ordering of prompts: WaitingOnYou prompts (mFresh, mMixed) come BEFORE WaitingOnOthers-only prompt (mSilent)!
	limitZero := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending&limit=0", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2, WaitingOnOthers: 2}, limitZero.Counts)
	assert.NotNil(t, limitZero.Prompts)
	assert.Empty(t, limitZero.Prompts)
	assert.Empty(t, limitZero.NextCursor)
	rawZero := doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending&limit=0", nil).Body.String()
	assert.Contains(t, rawZero, `"prompts":[]`)

	// Paginate tab=pending 1 prompt per page across all 3 prompts!
	var seenPromptIDs []string
	cursor := ""
	for page := 0; page < 5; page++ {
		u := basePath + "?tab=pending&limit=1"
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		got := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
		assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 2, WaitingOnOthers: 2}, got.Counts)
		for _, p := range got.Prompts {
			seenPromptIDs = append(seenPromptIDs, p.PromptMessageID)
			if p.PromptMessageID == mMixed.ID {
				assert.True(t, p.WaitingOnYou)
				assert.True(t, p.CanAnswer)
				assert.Equal(t, ChoiceAnswerModeSet, p.AnswerMode)
				assert.Len(t, p.Decisions, 2, "all sibling rows of a set prompt are loaded")
				assert.Equal(t, WaitingReasonOtherUserConfirm, p.WaitingReasons[resMixed.Decisions[0].RowID])
			}
			if p.PromptMessageID == mSilent.ID {
				assert.False(t, p.WaitingOnYou)
				assert.False(t, p.CanAnswer)
				assert.Equal(t, WaitingReasonMessageDenied, p.WaitingReasons[p.Decisions[0].RowID])
			}
		}
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}
	assert.Equal(t, []string{mFresh.ID, mMixed.ID, mSilent.ID}, seenPromptIDs, "WaitingOnYou prompts precede WaitingOnOthers prompts")

	// Demote Bob to read-only chat reader: every open row in the topic becomes WaitingOnOthers (read_only / other_user_confirm)!
	f.makeChatReader(t, f.bob)
	readerView := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 0, WaitingOnOthers: 4}, readerView.Counts)
	for _, p := range readerView.Prompts {
		assert.False(t, p.WaitingOnYou)
		assert.False(t, p.CanAnswer)
	}

	// Query validation -> 400 validation_error.
	badQueries := []string{
		"?tab=invalid",
		"?limit=-1",
		"?limit=201",
		"?limit=abc",
		"?tab=pending&cursor=not-valid-base64!!!",
		"?tab=made&cursor=not-valid-base64!!!",
		"?tab=made&agent=not-a-uuid",
		"?tab=made&provenance=bogus",
		"?tab=made&from=not-a-date",
		"?tab=made&to=not-a-date",
		"?tab=made&from=2026-11-14&to=2026-11-13",
	}
	for _, bq := range badQueries {
		rr := doRequestAsUser(t, f.srv, f.alice, http.MethodGet, basePath+bq, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "query %s: %s", bq, rr.Body.String())
	}
}

func TestProjectDecisions_PurgedPromptsAndAnswerSurface(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Subscribe to SSE choice.updated on both project and Bob's user subject.
	projUpdated, unsubProj := f.srv.events.Subscribe("project." + f.project.ID + ".chat.choice.updated")
	defer unsubProj()
	bobUpdated, unsubBob := f.srv.events.Subscribe("user." + f.bob.ID + ".chat.choice.updated")
	defer unsubBob()

	// 1. Single prompt in Bob's DM + 3-decision set prompt in Bob's DM + single prompt in topic (for --update after purge) + soft-deleted topic prompt.
	dmSingle := f.sendPromptToBob(t, `[{"id":"dm-purge-1","question":"Purged single?","choices":["yes","no"],"default":"yes"}]`, false)
	dmSet := f.sendSetToBob(t)
	topicToReplaceMsg, topicToReplaceRes := f.topicPrompt(t, `[{"id":"rep-after-purge","question":"Replace me?","choices":["v1a","v1b"]}]`, false)
	topicSoftDelMsg, _ := f.topicPrompt(t, `[{"id":"soft-del","question":"Soft deleted prompt?","choices":["ok","skip"]}]`, false)

	// Purge dmSingle, dmSet, and topicToReplaceMsg from messages via real chat retention!
	purgeMessages(t, f.store, f.project.ID, dmSingle.MessageID, dmSet.MessageID, topicToReplaceMsg.ID)

	// Soft-delete topicSoftDelMsg in WebChatStore.
	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()
	require.NoError(t, wcs.SetMessageDeleted(ctx, topicSoftDelMsg.ID, time.Now().UTC()))

	// Also insert a legacy decision row whose chat_key is NULL (simulating a pre-3A already-purged prompt):
	// create a prompt, clear chat_key in SQL, and purge its message.
	legacyMsg, legacyRes := f.topicPrompt(t, `[{"id":"legacy-null-key","question":"Legacy null key?","choices":["y","n"]}]`, false)
	purgeMessages(t, f.store, f.project.ID, legacyMsg.ID)
	provider, ok := f.store.(testRetentionProvider)
	require.True(t, ok)
	_, err := provider.DB().ExecContext(ctx, `UPDATE decisions SET chat_key = NULL WHERE id = ?`, legacyRes.Decisions[0].RowID)
	require.NoError(t, err)

	// Verify legacy null-chat_key purged row is hidden on GET /decisions/{rowId}, POST /answer, and tab=pending!
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+legacyRes.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.answer(t, f.bob, legacyRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)

	// Verify tab=pending for Bob lists dmSingle, dmSet, topicToReplace, and topicSoftDel — ALL with promptAvailable=false, and hides legacy-null-key!
	pending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	promptAvailByMsg := make(map[string]bool)
	for _, p := range pending.Prompts {
		promptAvailByMsg[p.PromptMessageID] = p.PromptAvailable
		for _, d := range p.Decisions {
			assert.NotEqual(t, "legacy-null-key", d.Key, "null-chat_key purged row must stay hidden")
		}
	}
	assert.Contains(t, promptAvailByMsg, dmSingle.MessageID)
	assert.False(t, promptAvailByMsg[dmSingle.MessageID], "purged DM single prompt has promptAvailable=false")
	assert.Contains(t, promptAvailByMsg, dmSet.MessageID)
	assert.False(t, promptAvailByMsg[dmSet.MessageID], "purged DM set prompt has promptAvailable=false")
	assert.Contains(t, promptAvailByMsg, topicSoftDelMsg.ID)
	assert.False(t, promptAvailByMsg[topicSoftDelMsg.ID], "soft-deleted prompt has promptAvailable=false")

	// Verify GET /api/v1/decisions/{rowId} on purged DM prompt returns 200 for Bob and 404 for Alice!
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+dmSingle.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/decisions/"+dmSingle.Decisions[0].RowID, nil).Code)

	// Validate surface parameter on single and set answer endpoints: "slack" and "invalid" -> 400!
	for _, badSurface := range []string{"slack", "google_chat", "other", "invalid"} {
		rr1 := f.answer(t, f.bob, dmSingle.Decisions[0].RowID, map[string]interface{}{
			"choice_id": "1",
			"surface":   badSurface,
		})
		assert.Equal(t, http.StatusBadRequest, rr1.Code, "single surface=%s", badSurface)

		rr2 := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/choice-prompts/"+dmSet.MessageID+"/answers", map[string]interface{}{
			"surface": badSurface,
			"answers": []map[string]interface{}{
				{"decision_row_id": dmSet.Decisions[0].RowID, "choice_id": "1"},
				{"decision_row_id": dmSet.Decisions[1].RowID, "choice_id": "a"},
				{"decision_row_id": dmSet.Decisions[2].RowID, "choice_id": "1"},
			},
		})
		assert.Equal(t, http.StatusBadRequest, rr2.Code, "set surface=%s", badSurface)
	}

	// Calling single-answer route on a purged set row still returns 409 batch_required without nil deref!
	batchReqRR := f.answer(t, f.bob, dmSet.Decisions[0].RowID, map[string]interface{}{"choice_id": "1", "surface": "decisions_page"})
	assert.Equal(t, http.StatusConflict, batchReqRR.Code)
	assert.Equal(t, ErrCodeBatchRequired, errorCode(t, batchReqRR))

	// Drain any earlier SSE events, then answer purged single DM prompt with surface="decisions_page"!
	_ = collectEvents(bobUpdated)
	singleAnsRR := f.answer(t, f.bob, dmSingle.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	})
	require.Equal(t, http.StatusOK, singleAnsRR.Code, "body: %s", singleAnsRR.Body.String())
	var singleAns DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(singleAnsRR.Body.Bytes(), &singleAns))
	require.NotNil(t, singleAns.Decision.Decision)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, singleAns.Decision.Decision.Surface)
	assert.NotEmpty(t, collectEvents(bobUpdated), "SSE choice.updated published for purged DM single prompt")

	// Answer purged 3-decision DM set prompt with surface="decisions_page"!
	_ = collectEvents(bobUpdated)
	setAnsRR := doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/choice-prompts/"+dmSet.MessageID+"/answers", map[string]interface{}{
		"surface": "decisions_page",
		"answers": []map[string]interface{}{
			{"decision_row_id": dmSet.Decisions[0].RowID, "choice_id": "1"},
			{"decision_row_id": dmSet.Decisions[1].RowID, "choice_id": "a"},
			{"decision_row_id": dmSet.Decisions[2].RowID, "choice_id": "2"},
		},
	})
	require.Equal(t, http.StatusOK, setAnsRR.Code, "body: %s", setAnsRR.Body.String())
	setAns := decodeSetResponse(t, setAnsRR)
	require.Len(t, setAns.Decisions, 3)
	for _, dv := range setAns.Decisions {
		require.NotNil(t, dv.Decision)
		assert.Equal(t, store.DecisionSurfaceDecisionsPage, dv.Decision.Surface)
	}
	assert.Len(t, collectEvents(bobUpdated), 3, "SSE choice.updated published per row of purged DM set prompt")

	// Replace purged topic prompt's decision via --update through handleAgentOutboundMessage
	// and verify project SSE choice.updated still fires on the old topic's subject!
	_ = collectEvents(projUpdated)
	repRes := f.sendPromptToBob(t, `[{"id":"rep-after-purge","question":"Replace me v2?","choices":["v2a","v2b"]}]`, true)
	require.NotNil(t, repRes.Decisions[0].Replaced)
	replacedEvent(t, collectEvents(projUpdated), topicToReplaceRes.Decisions[0].RowID, repRes.Decisions[0].RowID)
}

func TestProjectDecisions_MadeLogFiltersPaginationAndDeletedPrincipals(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()

	// Create a temporary user Charlie and a temporary agent tempBot that we will delete after recording decisions!
	charlie := &store.User{
		ID: tid("choices-charlie"), Email: "charlie-choices@example.com", DisplayName: "Charlie Temp",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateUser(ctx, charlie))
	ensureHubMembership(ctx, f.store, charlie.ID)
	addProjectMemberWithRole(t, f.store, f.project, charlie.ID, store.GroupMemberRoleAdmin)

	tempBot := &store.Agent{
		ID: api.NewUUID(), Slug: "ephemeral-bot", Name: "Ephemeral Bot",
		ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(ctx, tempBot))

	// Entry 1 (oldest): Charlie answers a topic prompt from f.agent (surface="native"), then Charlie is deleted!
	_, resCharlie := f.topicPrompt(t, `[{"id":"d-charlie","question":"Backfill before cutover?","choices":["yes","no"],"default":"yes"}]`, false)
	rrCharlie := f.answer(t, charlie, resCharlie.Decisions[0].RowID, map[string]interface{}{"choice_id": "1", "surface": "native"})
	require.Equal(t, http.StatusOK, rrCharlie.Code)
	require.NoError(t, f.store.DeleteUser(ctx, charlie.ID))

	time.Sleep(5 * time.Millisecond)

	// Entry 2 & Entry 3: f.agent records interpreted "yes" for Alice on D9 (from sourceMsg), then Alice overrides to "no" on decisions_page!
	_, resD9 := f.topicPrompt(t, `[{"id":"D9","question":"Cut the RC from main?","choices":["yes","no"]}]`, false)
	srcMsgID := api.NewUUID()
	require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
		ID: srcMsgID, ProjectID: f.project.ID, Sender: "user:" + f.alice.ID, SenderID: f.alice.ID,
		Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Type: messages.TypeChat, Channel: "slack", Msg: "D9: yes", CreatedAt: time.Now().UTC(),
	}))
	_, err = f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agent.ID, SourceMessageID: srcMsgID,
		AnsweredByUserID: f.alice.ID, AnsweredByName: f.alice.DisplayName, AnsweredByEmail: f.alice.Email,
		Surface: store.DecisionSurfaceSlack,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: resD9.Decisions[0].RowID, ChoiceID: "1"}},
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "interpreted D9 yes" },
	})
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)

	rrOverride := f.answer(t, f.alice, resD9.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "2",
		"surface":   "decisions_page",
	})
	require.Equal(t, http.StatusOK, rrOverride.Code, "body: %s", rrOverride.Body.String())

	time.Sleep(5 * time.Millisecond)

	// Entry 4 & Entry 5: Interpreted answer for Bob, then Bob confirms!
	_, resConf := f.topicPrompt(t, `[{"id":"d-conf","question":"Confirm rollout?","choices":["yes","no"]}]`, false)
	srcConfID := api.NewUUID()
	require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
		ID: srcConfID, ProjectID: f.project.ID, Sender: "user:" + f.bob.ID, SenderID: f.bob.ID,
		Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Type: messages.TypeChat, Channel: "web", Msg: "d-conf: yes", CreatedAt: time.Now().UTC(),
	}))
	_, err = f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
		InterpretedByAgentID: f.agent.ID, SourceMessageID: srcConfID,
		AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName, AnsweredByEmail: f.bob.Email,
		Surface: store.DecisionSurfaceNative,
		Answers: []store.InterpretedAnswerPair{{DecisionRowID: resConf.Decisions[0].RowID, ChoiceID: "1"}},
		ResponseMessage: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "interpreted d-conf yes" },
	})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, resConf.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)

	time.Sleep(5 * time.Millisecond)

	// Deleted-agent DM test: tempBot sends a DM decision to Bob, then tempBot is SOFT-DELETED and HARD-PURGED!
	dmTempKey, err := messages.DMConversationKey("agent", tempBot.ID, "user", f.bob.ID)
	require.NoError(t, err)
	decsTemp, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"dm-gone-bot","question":"Keep archive?","choices":["keep","drop"]}]`))
	require.NoError(t, err)
	dmTempMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + tempBot.Slug, SenderID: tempBot.ID,
		Recipient: "user:" + f.bob.ID, ThreadID: dmTempKey, Channel: "web", CreatedAt: time.Now().UTC(),
	}
	resTemp, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: dmTempMsg, Intro: "From tempBot", AskerAgentID: tempBot.ID, AskerAgentName: tempBot.Name, Decisions: decsTemp,
	})
	require.NoError(t, err)

	require.NoError(t, f.store.DeleteAgent(ctx, tempBot.ID))
	_, gerr := f.store.GetAgent(ctx, tempBot.ID)
	require.ErrorIs(t, gerr, store.ErrNotFound, "tempBot hard-deleted from agents table")

	// Bob STILL sees dm-gone-bot on tab=pending, can GET /decisions/{rowId}, and can answer it (undeliverable semantics)!
	bobPending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	require.Len(t, bobPending.Prompts, 1)
	assert.Equal(t, "dm-gone-bot", bobPending.Prompts[0].Decisions[0].Key)
	assert.Equal(t, "Ephemeral Bot", bobPending.Prompts[0].AskerAgentName)
	assert.Equal(t, "DM with Ephemeral Bot", bobPending.Prompts[0].ConversationLabel)
	assert.False(t, bobPending.Prompts[0].ConversationAvailable, "hard-deleted DM peer agent marks conversationAvailable=false")
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+resTemp.Decisions[0].RowID, nil).Code)

	rrGoneDM := f.answer(t, f.bob, resTemp.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	})
	require.Equal(t, http.StatusOK, rrGoneDM.Code, "body: %s", rrGoneDM.Body.String())

	// Now tab=made for Bob has 6 entries! Paginate 2 per page across 3 pages and verify order + fields.
	var allEntries []ProjectDecisionLogEntryView
	cursor := ""
	pages := 0
	for page := 0; page < 10; page++ {
		u := basePath + "?tab=made&limit=2"
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		got := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
		pages++
		allEntries = append(allEntries, got.Entries...)
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}
	assert.Equal(t, 3, pages, "6 entries at limit=2 paginate across 3 pages")
	require.Len(t, allEntries, 6)

	// Entry 0 (newest): dm-gone-bot answered by Bob (hard-deleted agent preserved!)
	assert.Equal(t, "dm-gone-bot", allEntries[0].Decision.Key)
	assert.Equal(t, "Ephemeral Bot", allEntries[0].Decision.AskerAgentName)
	assert.False(t, allEntries[0].ConversationAvailable)

	// Entry 1 & 2: d-conf confirmed + interpreted
	assert.Equal(t, "d-conf", allEntries[1].Decision.Key)
	assert.Equal(t, store.DecisionOutcomeConfirm, allEntries[1].Outcome)
	assert.Equal(t, "d-conf", allEntries[2].Decision.Key)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, allEntries[2].Provenance)

	// Entry 3 & 4: D9 override ("no", previousChoiceLabel="yes", surface="decisions_page") + D9 interpreted ("yes", surface="slack", sourceMessageAvailable=true)
	assert.Equal(t, "D9", allEntries[3].Decision.Key)
	assert.Equal(t, store.DecisionOutcomeOverride, allEntries[3].Outcome)
	assert.Equal(t, "no", allEntries[3].Label)
	assert.Equal(t, "yes", allEntries[3].PreviousChoiceLabel)
	assert.Equal(t, store.DecisionSurfaceDecisionsPage, allEntries[3].Surface)

	assert.Equal(t, "D9", allEntries[4].Decision.Key)
	assert.Equal(t, store.DecisionProvenanceAgentInterpreted, allEntries[4].Provenance)
	assert.Equal(t, store.DecisionSurfaceSlack, allEntries[4].Surface)
	assert.True(t, allEntries[4].SourceMessageAvailable)
	assert.NotEmpty(t, allEntries[4].InterpretedByAgentName)

	// Entry 5 (oldest): Charlie (deleted user -> AnsweredByUserID empty, AnsweredByName preserved!)
	assert.Equal(t, "d-charlie", allEntries[5].Decision.Key)
	assert.Empty(t, allEntries[5].AnsweredByUserID, "deleted user's ID is cleared by ON DELETE SET NULL")
	assert.Equal(t, "Charlie Temp", allEntries[5].AnsweredByName)
	assert.True(t, allEntries[5].MatchedDefault)

	// Soft-delete D9's source reply -> sourceMessageAvailable becomes false; hard-purge srcConfID -> also false!
	require.NoError(t, wcs.SetMessageDeleted(ctx, srcMsgID, time.Now().UTC()))
	purgeMessages(t, f.store, f.project.ID, srcConfID)

	interpOnly := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made&provenance=interpreted", nil))
	require.Len(t, interpOnly.Entries, 2)
	assert.False(t, interpOnly.Entries[0].SourceMessageAvailable, "hard-purged source reply marks sourceMessageAvailable=false")
	assert.False(t, interpOnly.Entries[1].SourceMessageAvailable, "soft-deleted source reply marks sourceMessageAvailable=false")

	// Test every provenance filter + combinable agent, user, and YYYY-MM-DD date filters!
	directOnly := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made&provenance=direct", nil))
	assert.Len(t, directOnly.Entries, 2, "d-charlie and dm-gone-bot")

	confirmedOnly := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made&provenance=confirmed", nil))
	require.Len(t, confirmedOnly.Entries, 1)
	assert.Equal(t, "d-conf", confirmedOnly.Entries[0].Decision.Key)

	overriddenOnly := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made&provenance=overridden", nil))
	require.Len(t, overriddenOnly.Entries, 1)
	assert.Equal(t, "D9", overriddenOnly.Entries[0].Decision.Key)

	today := time.Now().UTC().Format("2006-01-02")
	combined := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet,
		basePath+"?tab=made&agent="+f.agent.ID+"&user="+f.alice.ID+"&provenance=overridden&from="+today+"&to="+today, nil))
	require.Len(t, combined.Entries, 1)
	assert.Equal(t, "D9", combined.Entries[0].Decision.Key)
}

func TestProjectDecisions_LimitZeroSkipsFacetsAndPreservesCounts(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	_, openRes := f.topicPrompt(t, `[{"id":"lz-open","question":"Open?","choices":["yes","no"]}]`, false)
	require.Len(t, openRes.Decisions, 1)
	_, madeRes := f.topicPrompt(t, `[{"id":"lz-made","question":"Answered?","choices":["yes","no"]}]`, false)
	require.Equal(t, http.StatusOK, f.answer(t, f.bob, madeRes.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	}).Code)

	// Non-zero limit returns facets; limit=0 skips facet queries and returns empty arrays with accurate counts.
	withLimit := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending&limit=10", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1, WaitingOnOthers: 0}, withLimit.Counts)
	assert.Len(t, withLimit.Prompts, 1)
	assert.Len(t, withLimit.Agents, 1)
	assert.Len(t, withLimit.Answerers, 1)

	pendingZero := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending&limit=0", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1, WaitingOnOthers: 0}, pendingZero.Counts)
	assert.Empty(t, pendingZero.Prompts)
	assert.Empty(t, pendingZero.Agents, "limit=0 skips facet query")
	assert.Empty(t, pendingZero.Answerers, "limit=0 skips facet query")
	assert.Empty(t, pendingZero.NextCursor)

	madeZero := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made&limit=0", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1, WaitingOnOthers: 0}, madeZero.Counts)
	assert.Empty(t, madeZero.Entries)
	assert.Empty(t, madeZero.Agents, "limit=0 skips facet query")
	assert.Empty(t, madeZero.Answerers, "limit=0 skips facet query")
	assert.Empty(t, madeZero.NextCursor)
}

func TestProjectDecisions_DeletedTopicPreservesAuditLog(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	f.srv.mu.RLock()
	wcs := f.srv.webChatStore
	f.srv.mu.RUnlock()
	require.NotNil(t, wcs)

	// Create a second non-general topic (#incidents) so DeleteTopic is allowed.
	incTopicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:           incTopicID,
		ProjectID:    f.project.ID,
		Name:         "incidents",
		IsGeneral:    false,
		CreatedBy:    f.alice.ID,
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: f.agent.Slug,
	}))
	setTopicConversationID(t, f.db, f.store, incTopicID, f.project.ID)
	incConv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+incTopicID)
	require.NoError(t, err)
	incConvID := incConv.ID

	seedInIncidents := func(raw string) *store.ChoicePromptResult {
		decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, raw))
		require.NoError(t, err)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: incTopicID, ConversationID: incConvID, Channel: "web", CreatedAt: time.Now().UTC(),
		}
		res, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: msg, Intro: "Incidents", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name, Decisions: decs,
		})
		require.NoError(t, err)
		return res
	}

	inc1 := seedInIncidents(`[{"id":"inc-answered","question":"Mitigate?","choices":["rollback","patch"]}]`)
	inc2 := seedInIncidents(`[{"id":"inc-open","question":"Postmortem owner?","choices":["alice","bob"]}]`)

	require.Equal(t, http.StatusOK, f.answer(t, f.bob, inc1.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	}).Code)

	// Soft-delete #incidents.
	require.NoError(t, wcs.DeleteTopic(ctx, incTopicID))
	goneTopic, err := wcs.GetTopic(ctx, incTopicID)
	require.NoError(t, err)
	require.Nil(t, goneTopic, "GetTopic hides soft-deleted topic")
	inclTopic, err := wcs.GetTopicIncludingDeleted(ctx, incTopicID)
	require.NoError(t, err)
	require.NotNil(t, inclTopic)
	require.NotNil(t, inclTopic.DeletedAt)

	// 1. tab=made preserves the answered entry with conversationAvailable=false and label "#incidents".
	made := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=made", nil))
	require.Len(t, made.Entries, 1)
	assert.Equal(t, "inc-answered", made.Entries[0].Decision.Key)
	assert.Equal(t, "#incidents", made.Entries[0].ConversationLabel)
	assert.False(t, made.Entries[0].ConversationAvailable, "soft-deleted topic marks conversationAvailable=false")

	// 2. tab=pending classifies the remaining open prompt under waitingOnOthers (read_only, canAnswer=false, conversationAvailable=false).
	pending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 0, WaitingOnOthers: 1}, pending.Counts)
	require.Len(t, pending.Prompts, 1)
	assert.Equal(t, "inc-open", pending.Prompts[0].Decisions[0].Key)
	assert.Equal(t, "#incidents", pending.Prompts[0].ConversationLabel)
	assert.False(t, pending.Prompts[0].ConversationAvailable)
	assert.False(t, pending.Prompts[0].WaitingOnYou)
	assert.False(t, pending.Prompts[0].CanAnswer)
	assert.Equal(t, WaitingReasonReadOnly, pending.Prompts[0].WaitingReasons[inc2.Decisions[0].RowID])

	// 3. GET /api/v1/decisions/{rowId} succeeds for project reader (200) and refuses outsider (404).
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+inc2.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.outsider, http.MethodGet, "/api/v1/decisions/"+inc2.Decisions[0].RowID, nil).Code)

	// 4. POST /api/v1/decisions/{rowId}/answer is refused (404) because the topic is soft-deleted.
	assert.Equal(t, http.StatusNotFound, f.answer(t, f.bob, inc2.Decisions[0].RowID, map[string]interface{}{
		"choice_id": "1",
		"surface":   "decisions_page",
	}).Code)
}

func TestProjectDecisions_LiveDMAgentAuthzAndSoftDeletedAsker(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	// Part A: Live DM agent authorization for active member with MessageModeNone vs revoked project member.
	dave := &store.User{
		ID: tid("choices-dave"), Email: "dave-choices@example.com", DisplayName: "Dave Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateUser(ctx, dave))
	ensureHubMembership(ctx, f.store, dave.ID)
	addProjectMemberWithRole(t, f.store, f.project, dave.ID, store.GroupMemberRoleMember)

	dmDaveKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", dave.ID)
	require.NoError(t, err)

	seedDaveDM := func(raw string) *store.ChoicePromptResult {
		decs, err := messages.NormalizeDecisions("choices", decisionInputs(t, raw))
		require.NoError(t, err)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "user:" + dave.ID, ThreadID: dmDaveKey, Channel: "web", CreatedAt: time.Now().UTC(),
		}
		res, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
			Message: msg, Intro: "DM to Dave", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Name, Decisions: decs,
		})
		require.NoError(t, err)
		return res
	}

	dmOpen := seedDaveDM(`[{"id":"dm-dave-open","question":"Open DM?","choices":["yes","no"]}]`)
	dmInterp1 := seedDaveDM(`[{"id":"dm-dave-interp-1","question":"Interp DM 1?","choices":["yes","no"]}]`)
	dmInterp2 := seedDaveDM(`[{"id":"dm-dave-interp-2","question":"Interp DM 2?","choices":["yes","no"]}]`)

	for _, res := range []*store.ChoicePromptResult{dmInterp1, dmInterp2} {
		srcID := api.NewUUID()
		require.NoError(t, f.store.CreateMessage(ctx, &store.Message{
			ID: srcID, ProjectID: f.project.ID, Sender: "user:" + dave.ID, SenderID: dave.ID,
			Recipient: "agent:" + f.agent.Slug, ThreadID: dmDaveKey, Type: messages.TypeChat, Channel: "web", Msg: "yes", CreatedAt: time.Now().UTC(),
		}))
		_, err := f.store.RecordInterpretedAnswers(ctx, &store.RecordInterpretedInput{
			InterpretedByAgentID: f.agent.ID, SourceMessageID: srcID,
			AnsweredByUserID: dave.ID, AnsweredByName: dave.DisplayName, AnsweredByEmail: dave.Email,
			Surface: store.DecisionSurfaceNative,
			Answers: []store.InterpretedAnswerPair{{DecisionRowID: res.Decisions[0].RowID, ChoiceID: "1"}},
			ResponseMessage: &store.Message{
				ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
				Recipient: "user:" + dave.ID, ThreadID: dmDaveKey, Channel: "web", CreatedAt: time.Now().UTC(),
			},
			FormatMessage: func(_ []store.InterpretedFormattedItem) string { return "interpreted yes" },
		})
		require.NoError(t, err)
	}

	// Switch live f.agent (owned by Bob, not Dave) to MessageModeNone:
	// Dave (still a project member with ActionRead on f.agent) gets 200 on GET,
	// 200 on confirm of his own interpreted answer, and 403 MESSAGE_DENIED on fresh answer!
	f.agent.MessageMode = store.MessageModeNone
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, dave, http.MethodGet, "/api/v1/decisions/"+dmOpen.Decisions[0].RowID, nil).Code)
	freshDenied := f.answer(t, dave, dmOpen.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
	assert.Equal(t, http.StatusForbidden, freshDenied.Code)
	assert.Equal(t, ErrCodeMessageDenied, errorCode(t, freshDenied))
	assert.Equal(t, http.StatusOK, f.answer(t, dave, dmInterp1.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code,
		"current project member can confirm own interpreted DM answer even when live agent has MessageModeNone")

	// Restore MessageModeProject on f.agent, then revoke Dave's project membership completely.
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	membersGroup, err := f.store.GetGroupBySlug(ctx, "project:"+f.project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, f.store.RemoveGroupMember(ctx, membersGroup.ID, store.GroupMemberTypeUser, dave.ID))
	_, err = f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, dave.ID)
	require.NoError(t, err)

	// Revoked user no longer holds ActionRead on the live agent -> GET, fresh answer, AND confirm all return 404!
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, dave, http.MethodGet, "/api/v1/decisions/"+dmOpen.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.answer(t, dave, dmOpen.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)
	assert.Equal(t, http.StatusNotFound, f.answer(t, dave, dmInterp2.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)

	// Part B: Soft-deleted restrictive asker parity in choiceAsker & classifyProjectOpenPrompts.
	softBot := &store.Agent{
		ID: api.NewUUID(), Slug: "soft-deleted-bot", Name: "Soft Deleted Bot",
		ProjectID: f.project.ID, OwnerID: f.alice.ID, CreatedBy: f.alice.ID, Ancestry: []string{f.alice.ID},
		Phase: string(state.PhaseRunning), MessageMode: store.MessageModeNone,
	}
	require.NoError(t, f.store.CreateAgent(ctx, softBot))

	conv, err := f.store.GetConversationByExternalRef(ctx, "native", "thread:"+f.project.ID+":"+f.topicID)
	require.NoError(t, err)
	decsSoft, err := messages.NormalizeDecisions("choices", decisionInputs(t, `[{"id":"soft-asker-1","question":"Proceed after soft-delete?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	softRes, err := f.store.CreateChoicePrompt(ctx, &store.ChoicePromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID, Sender: "agent:" + softBot.Slug, SenderID: softBot.ID,
			Recipient: "chat", ThreadID: f.topicID, ConversationID: conv.ID, Channel: "web", CreatedAt: time.Now().UTC(),
		},
		Intro: "Soft bot", AskerAgentID: softBot.ID, AskerAgentName: softBot.Name, Decisions: decsSoft,
	})
	require.NoError(t, err)
	softRowID := softRes.Decisions[0].RowID

	// While live with MessageModeNone, Bob (not owner) is blocked with message_denied (403).
	assert.Equal(t, http.StatusForbidden, f.answer(t, f.bob, softRowID, map[string]interface{}{"choice_id": "1"}).Code)

	// Soft-delete softBot (row remains in agents table with DeletedAt != zero).
	now := time.Now().UTC()
	n, err := f.store.UpdateAgentDeletion(ctx, softBot.ID, store.DeletionPredicate{}, store.DeletionFields{DeletedAt: &now})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	gotSoft, err := f.store.GetAgent(ctx, softBot.ID)
	require.NoError(t, err)
	require.False(t, gotSoft.DeletedAt.IsZero())

	// Now Bob sees soft-asker-1 as WaitingOnYou (canAnswer=true) AND POST /answer succeeds 200 with failed/gone dispatch!
	bobPending := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, basePath+"?tab=pending", nil))
	require.Len(t, bobPending.Prompts, 1)
	assert.Equal(t, softRowID, bobPending.Prompts[0].Decisions[0].RowID)
	assert.True(t, bobPending.Prompts[0].WaitingOnYou)
	assert.True(t, bobPending.Prompts[0].CanAnswer)

	ansSoftRR := f.answer(t, f.bob, softRowID, map[string]interface{}{"choice_id": "1", "surface": "decisions_page"})
	require.Equal(t, http.StatusOK, ansSoftRR.Code, "body: %s", ansSoftRR.Body.String())
	var ansSoft DecisionAnswerResponse
	require.NoError(t, json.Unmarshal(ansSoftRR.Body.Bytes(), &ansSoft))
	respMsg, err := f.store.GetMessage(ctx, ansSoft.ResponseMessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, respMsg.DispatchState, "soft-deleted asker records undeliverable response message")
}

func TestProjectDecisions_NullChatKeyFallbackToPromptThread(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()

	_, topicRes := f.topicPrompt(t, `[{"id":"null-topic-1","question":"Fallback topic?","choices":["yes","no"]}]`, false)
	dmSingle := f.sendPromptToBob(t, `[{"id":"null-dm-1","question":"Fallback DM single?","choices":["yes","no"]}]`, false)
	dmSet := f.sendSetToBob(t)

	// Simulate pre-backfill rows: clear chat_key on all created decisions while prompt messages remain in messages!
	provider, ok := f.store.(testRetentionProvider)
	require.True(t, ok)
	_, err := provider.DB().ExecContext(ctx, `UPDATE decisions SET chat_key = NULL`)
	require.NoError(t, err)

	// 1. GET /api/v1/decisions/{rowId}: Bob allowed on topic + own DM; outsider and non-participant Alice get 404.
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+topicRes.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.outsider, http.MethodGet, "/api/v1/decisions/"+topicRes.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, "/api/v1/decisions/"+dmSingle.Decisions[0].RowID, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, "/api/v1/decisions/"+dmSingle.Decisions[0].RowID, nil).Code)

	// 2. POST /api/v1/decisions/{rowId}/answer succeeds on both topic and DM fallback!
	assert.Equal(t, http.StatusOK, f.answer(t, f.bob, topicRes.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)
	assert.Equal(t, http.StatusOK, f.answer(t, f.bob, dmSingle.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"}).Code)

	// 3. POST /api/v1/choice-prompts/{messageId}/answers refuses non-participant Alice (404) and succeeds for Bob (200)!
	setBody := map[string]interface{}{
		"surface": "decisions_page",
		"answers": []map[string]interface{}{
			{"decision_row_id": dmSet.Decisions[0].RowID, "choice_id": "1"},
			{"decision_row_id": dmSet.Decisions[1].RowID, "choice_id": "a"},
			{"decision_row_id": dmSet.Decisions[2].RowID, "choice_id": "2"},
		},
	}
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/choice-prompts/"+dmSet.MessageID+"/answers", setBody).Code)
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, "/api/v1/choice-prompts/"+dmSet.MessageID+"/answers", setBody).Code)
}

func TestProjectDecisions_EqualTimestampCursorTieBreak(t *testing.T) {
	f := newChoicesFixture(t)
	setProjectDecisionsExperiments(t, f.srv, true, true)
	ctx := context.Background()
	basePath := "/api/v1/projects/" + f.project.ID + "/decisions"

	provider, ok := f.store.(testRetentionProvider)
	require.True(t, ok)

	// Seed 4 open prompts and 4 answered prompts.
	var openMsgIDs []string
	for i := 0; i < 4; i++ {
		msg, _ := f.topicPrompt(t, `[{"id":"tie-open-`+uuid.NewString()[:8]+`","question":"Open tie?","choices":["yes","no"]}]`, false)
		openMsgIDs = append(openMsgIDs, msg.ID)
	}
	var madeAnswerIDs []string
	for i := 0; i < 4; i++ {
		_, res := f.topicPrompt(t, `[{"id":"tie-made-`+uuid.NewString()[:8]+`","question":"Made tie?","choices":["yes","no"]}]`, false)
		rr := f.answer(t, f.bob, res.Decisions[0].RowID, map[string]interface{}{"choice_id": "1"})
		require.Equal(t, http.StatusOK, rr.Code)
		cur, err := f.store.GetDecision(ctx, res.Decisions[0].RowID)
		require.NoError(t, err)
		require.Len(t, cur.Answers, 1)
		madeAnswerIDs = append(madeAnswerIDs, cur.Answers[0].ID)
	}

	// Force identical timestamps across all 4 open prompts and across all 4 decision_answers!
	samePendingTS := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	sameMadeTS := time.Date(2026, 11, 13, 13, 0, 0, 0, time.UTC)
	_, err := provider.DB().ExecContext(ctx, `UPDATE decisions SET created_at = ?`, samePendingTS)
	require.NoError(t, err)
	_, err = provider.DB().ExecContext(ctx, `UPDATE decision_answers SET created_at = ?`, sameMadeTS)
	require.NoError(t, err)

	// 1. Paginate tab=pending at limit=1 across all 4 open prompts -> strictly descending promptMessageId, zero duplicates/skips.
	var gotPendingMsgIDs []string
	cur := ""
	for page := 0; page < 6; page++ {
		u := basePath + "?tab=pending&limit=1"
		if cur != "" {
			u += "&cursor=" + cur
		}
		got := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
		for _, p := range got.Prompts {
			gotPendingMsgIDs = append(gotPendingMsgIDs, p.PromptMessageID)
		}
		if got.NextCursor == "" {
			break
		}
		cur = got.NextCursor
	}
	require.Len(t, gotPendingMsgIDs, 4)
	for i := 1; i < len(gotPendingMsgIDs); i++ {
		assert.Greater(t, gotPendingMsgIDs[i-1], gotPendingMsgIDs[i], "tab=pending equal created_at tie-breaks by promptMessageId DESC")
	}
	assert.ElementsMatch(t, openMsgIDs, gotPendingMsgIDs)

	// 2. Paginate tab=made at limit=1 across all 4 decision_answers -> strictly descending answerId, zero duplicates/skips.
	var gotMadeAnsIDs []string
	cur = ""
	for page := 0; page < 6; page++ {
		u := basePath + "?tab=made&limit=1"
		if cur != "" {
			u += "&cursor=" + cur
		}
		got := decodeMadeDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, u, nil))
		for _, e := range got.Entries {
			gotMadeAnsIDs = append(gotMadeAnsIDs, e.AnswerID)
		}
		if got.NextCursor == "" {
			break
		}
		cur = got.NextCursor
	}
	require.Len(t, gotMadeAnsIDs, 4)
	for i := 1; i < len(gotMadeAnsIDs); i++ {
		assert.Greater(t, gotMadeAnsIDs[i-1], gotMadeAnsIDs[i], "tab=made equal created_at tie-breaks by answerId DESC")
	}
	assert.ElementsMatch(t, madeAnswerIDs, gotMadeAnsIDs)
}
