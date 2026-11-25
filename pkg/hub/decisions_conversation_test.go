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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeConversationDecisions(t *testing.T, rr *httptest.ResponseRecorder) ConversationOpenDecisionsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var out ConversationOpenDecisionsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "body: %s", rr.Body.String())
	return out
}

func conversationDecisionsPath(key string) string {
	return "/api/v1/chat/conversations/" + url.PathEscape(key) + "/decisions"
}

func TestConversationDecisions_ExperimentMethodAndQuery(t *testing.T) {
	f := newDecisionsFixture(t)
	path := conversationDecisionsPath(f.topicID)

	// web.decisions off -> GET 404, non-GET 405.
	setDecisionsExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, nil).Code)

	// web.decisions on -> GET 200, non-GET 405, unknown query param 400.
	setDecisionsExperiment(t, f.srv, true)
	resp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path, nil))
	assert.Equal(t, ProjectDecisionCounts{}, resp.Counts)
	assert.NotNil(t, resp.Prompts)
	assert.Empty(t, resp.Prompts)
	assert.False(t, resp.Truncated)

	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodPost, path, nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequestAsUser(t, f.srv, f.bob, http.MethodDelete, path, nil).Code)
	assert.Equal(t, http.StatusBadRequest, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, path+"?limit=10", nil).Code)
}

// TestConversationDecisions_Authz pins chat.conversation.decisions.topic and
// chat.conversation.decisions.dm across session users, non-readers, DM
// participants vs other users vs admins, agents, visitors, UAT tokens with and
// without inbox:read, wrong-project UATs, and user-to-user DMs.
func TestConversationDecisions_Authz(t *testing.T) {
	f := newChatFixture(t)
	setDecisionsExperiment(t, f.srv, true)
	ctx := context.Background()

	f.agentA.OwnerID = f.member
	f.agentA.Ancestry = []string{f.member}
	f.agentA.MessageMode = store.MessageModeProject
	require.NoError(t, f.s.UpdateAgent(ctx, f.agentA))

	// Seed one topic prompt and one agent DM prompt for f.member.
	decsTopic, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"t1","question":"Topic choice?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	topicMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
		Recipient: "chat", ThreadID: f.topicA, Channel: "web", CreatedAt: time.Now().UTC().Add(-2 * time.Minute),
	}
	_, err = f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: topicMsg, Intro: "Topic intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decsTopic,
	})
	require.NoError(t, err)

	dmKey := f.dm(t, f.member, "agent", f.agentA.ID)
	decsDM, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"d1","question":"DM choice?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	dmMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
		Recipient: "user:" + f.member, ThreadID: dmKey, Channel: "web", CreatedAt: time.Now().UTC().Add(-time.Minute),
	}
	_, err = f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: dmMsg, Intro: "DM intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decsDM,
	})
	require.NoError(t, err)

	memberUser, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	adminUser, err := f.s.GetUser(ctx, f.admin)
	require.NoError(t, err)
	outsiderID := f.plainUser(t)
	outsiderUser, err := f.s.GetUser(ctx, outsiderID)
	require.NoError(t, err)

	topicPath := conversationDecisionsPath(f.topicA)
	dmPath := conversationDecisionsPath(dmKey)

	// Unauthenticated -> 401.
	unauthRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(unauthRec, httptest.NewRequest(http.MethodGet, topicPath, nil))
	assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

	// Agent caller -> 403.
	agentReq := httptest.NewRequest(http.MethodGet, topicPath, nil)
	agentReq = agentReq.WithContext(agentCtx(agentReq.Context(), f.agentA))
	agentRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusForbidden, agentRec.Code)

	// Visitor -> 403.
	visitor := NewVisitorIdentity(tid("conv-dec-visitor"), "https://issuer.test", "sub-v", "v@test.com", "Visitor")
	assert.Equal(t, http.StatusForbidden, requestAsIdentity(t, f.srv, visitor, http.MethodGet, topicPath, nil).Code)

	// Topic: reader -> 200, non-reader -> 404, missing topic -> 404.
	topicResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, topicPath, nil))
	assert.Equal(t, f.projA, topicResp.ProjectID)
	assert.NotEmpty(t, topicResp.ProjectSlug)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, topicResp.Counts)
	require.Len(t, topicResp.Prompts, 1)
	assert.Equal(t, topicMsg.ID, topicResp.Prompts[0].PromptMessageID)
	assert.Len(t, topicResp.Prompts[0].OpenRowIDs, 1)

	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, outsiderUser, http.MethodGet, topicPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, conversationDecisionsPath(uuid.NewString()), nil).Code)

	// DM: participant -> 200; other user -> 404; admin non-participant -> 404.
	dmResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, dmPath, nil))
	assert.Equal(t, f.projA, dmResp.ProjectID)
	assert.NotEmpty(t, dmResp.ProjectSlug)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, dmResp.Counts)
	require.Len(t, dmResp.Prompts, 1)
	assert.Equal(t, dmMsg.ID, dmResp.Prompts[0].PromptMessageID)
	assert.Len(t, dmResp.Prompts[0].OpenRowIDs, 1)

	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, outsiderUser, http.MethodGet, dmPath, nil).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, f.srv, adminUser, http.MethodGet, dmPath, nil).Code)

	// User-to-user DM -> empty 200.
	userDMKey, err := messages.DMConversationKey("user", f.member, "user", f.admin)
	require.NoError(t, err)
	userDMResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, memberUser, http.MethodGet, conversationDecisionsPath(userDMKey), nil))
	assert.Equal(t, ProjectDecisionCounts{}, userDMResp.Counts)
	assert.NotNil(t, userDMResp.Prompts)
	assert.Empty(t, userDMResp.Prompts)
	assert.False(t, userDMResp.Truncated)

	// UAT without inbox:read -> 403 on both topic and DM.
	tokNoInbox := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, tokNoInbox, http.MethodGet, topicPath, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, tokNoInbox, http.MethodGet, dmPath, nil).Code)

	// UAT with inbox:read + project:read + agent:read on projA -> 200 on both topic and DM.
	tokFull := f.mint(t, f.member, projectBoundary(f.projA), "project:read", "inbox:read", "agent:read")
	uatTopic := decodeConversationDecisions(t, f.call(t, tokFull, http.MethodGet, topicPath, nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, uatTopic.Counts)
	require.Len(t, uatTopic.Prompts, 1)
	assert.Equal(t, "Decisions are answered from a web session", uatTopic.Prompts[0].DisabledReason)

	uatDM := decodeConversationDecisions(t, f.call(t, tokFull, http.MethodGet, dmPath, nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, uatDM.Counts)

	// UAT bounded to wrong project (projB) -> 404 on topic.
	tokWrongProj := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:read", "agent:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, tokWrongProj, http.MethodGet, topicPath, nil).Code)

	// Pin the DM token-boundary & agent:read guard (authorizeChatTokenConversation on DM path):
	// seed an agentA (projA) DM for f.admin so f.admin is a legitimate participant.
	adminDMKey := f.dm(t, f.admin, "agent", f.agentA.ID)
	decsAdminDM, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"ad1","question":"Admin DM choice?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	adminDMMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.projA, Sender: "agent:" + f.agentA.Slug, SenderID: f.agentA.ID,
		Recipient: "user:" + f.admin, ThreadID: adminDMKey, Channel: "web", CreatedAt: time.Now().UTC(),
	}
	_, err = f.s.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: adminDMMsg, Intro: "Admin DM intro", AskerAgentID: f.agentA.ID, AskerAgentName: f.agentA.Slug, Decisions: decsAdminDM,
	})
	require.NoError(t, err)
	adminDMPath := conversationDecisionsPath(adminDMKey)

	// Control: session admin reads own DM -> 200 with peer agent's project.
	adminDMResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, adminUser, http.MethodGet, adminDMPath, nil))
	assert.Equal(t, f.projA, adminDMResp.ProjectID)
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, adminDMResp.Counts)

	// (a) UAT bounded to another project (projB) -> 404 even though f.admin is a DM participant.
	assert.Equal(t, http.StatusNotFound, f.call(t, tokWrongProj, http.MethodGet, adminDMPath, nil).Code)

	// (b) Hub-bounded UAT with inbox:read but without agent:read -> 404.
	tokHubNoAgentRead := f.mint(t, f.admin, TokenBoundary{Kind: BoundaryKindHub}, "inbox:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, tokHubNoAgentRead, http.MethodGet, adminDMPath, nil).Code)

	// Control: hub-bounded UAT with inbox:read + agent:read -> 200.
	tokHubWithAgentRead := f.mint(t, f.admin, TokenBoundary{Kind: BoundaryKindHub}, "inbox:read", "agent:read")
	hubDMResp := decodeConversationDecisions(t, f.call(t, tokHubWithAgentRead, http.MethodGet, adminDMPath, nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 1}, hubDMResp.Counts)
}

func TestConversationDecisions_ParityAndStates(t *testing.T) {
	f := newDecisionsFixture(t)
	setDecisionsExperiment(t, f.srv, true)
	ctx := context.Background()

	// Make agent accept messages from project members so both alice and bob can answer.
	f.agent.MessageMode = store.MessageModeProject
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	type seededPrompt struct {
		MessageID string
		RowIDs    []string
	}
	baseTime := time.Now().UTC().Add(-10 * time.Minute)
	seedPrompt := func(offset time.Duration, raw string, update bool) seededPrompt {
		t.Helper()
		decs, derr := messages.NormalizeDecisions("decisions", decisionInputs(t, raw))
		require.NoError(t, derr)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: f.topicID, Channel: "web",
			CreatedAt: baseTime.Add(offset),
		}
		res, cerr := f.store.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: msg, Intro: "intro", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Slug,
			Decisions: decs, Update: update,
		})
		require.NoError(t, cerr)
		rowIDs := make([]string, len(res.Decisions))
		for i, d := range res.Decisions {
			rowIDs[i] = d.RowID
		}
		return seededPrompt{MessageID: msg.ID, RowIDs: rowIDs}
	}

	// 1. Single prompt (oldest), later purged from messages table -> promptAvailable=false.
	p1Resp := seedPrompt(1*time.Minute, `[{"id":"p1","question":"Purged question?","choices":["yes","no"]}]`, false)
	purgeMessages(t, f.store, f.project.ID, p1Resp.MessageID)

	respMsg := func() *store.Message {
		return &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID,
			Sender: "user:" + f.bob.Email, SenderID: f.bob.ID,
			Recipient: "chat", ThreadID: f.topicID, Channel: "web",
			Type: messages.TypeDecisionResponse, Msg: "answered", CreatedAt: time.Now().UTC(),
		}
	}

	// 2. 3-row set prompt, with 1 row already decided -> openCount=2, totalCount=3.
	p2Resp := seedPrompt(2*time.Minute, `[
		{"id":"s1","question":"First set Q?","choices":["a","b"]},
		{"id":"s2","question":"Second set Q?","choices":["a","b"]},
		{"id":"s3","question":"Third set Q?","choices":["a","b"]}
	]`, false)
	_, _, err := f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: p2Resp.RowIDs[0], ChoiceID: "1",
		AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName,
		Provenance: store.DecisionProvenanceDirect, Surface: "native", IdempotencyKey: api.NewUUID(),
		ResponseMessage: respMsg(),
	})
	require.NoError(t, err)

	// 3. Interpreted answer recorded for bob -> waitingOnYou + awaitingConfirmation for bob,
	//    waitingOnOthers + awaitingConfirmation + disabledReason for alice.
	p3Resp := seedPrompt(3*time.Minute, `[{"id":"interp1","question":"Confirm interpreted?","choices":["yes","no"]}]`, false)
	srcMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: f.project.ID,
		Sender: "user:" + f.bob.Email, SenderID: f.bob.ID,
		Recipient: "chat", ThreadID: f.topicID, Channel: "web",
		Msg: "yes please", CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, f.store.CreateMessage(ctx, srcMsg))
	_, _, err = f.store.RecordAnswer(ctx, &store.RecordAnswerInput{
		DecisionRowID: p3Resp.RowIDs[0], ChoiceID: "1",
		AnsweredByUserID: f.bob.ID, AnsweredByName: f.bob.DisplayName,
		Provenance: store.DecisionProvenanceAgentInterpreted, InterpretedByAgentID: f.agent.ID,
		SourceMessageID: srcMsg.ID, Surface: "native", IdempotencyKey: api.NewUUID(),
		ResponseMessage: respMsg(),
	})
	require.NoError(t, err)

	// 4. Replaced prompt (--update): old version must NOT appear; new version appears.
	_ = seedPrompt(4*time.Minute, `[{"id":"upd","question":"Old v1?","choices":["yes","no"]}]`, false)
	p4NewResp := seedPrompt(5*time.Minute, `[{"id":"upd","question":"New v2?","choices":["yes","no"]}]`, true)

	// 5. Withdrawn prompt -> must NOT appear.
	_ = seedPrompt(6*time.Minute, `[{"id":"wd1","question":"Withdrawn?","choices":["yes","no"]}]`, false)
	_, err = f.store.WithdrawDecisions(ctx, &store.WithdrawDecisionsInput{
		AskerAgentID: f.agent.ID, Keys: []string{"wd1"}, Reason: "no longer needed",
	})
	require.NoError(t, err)

	// 6. Dismissed prompt -> must NOT appear.
	p6Resp := seedPrompt(7*time.Minute, `[{"id":"dsm1","question":"Dismissed?","choices":["yes","no"]}]`, false)
	_, err = f.store.DismissDecisions(ctx, &store.DismissDecisionsInput{
		MessageID: p6Resp.MessageID, RowIDs: []string{p6Resp.RowIDs[0]},
		User:    store.DecisionAnswerer{UserID: f.bob.ID, Name: f.bob.DisplayName, Email: f.bob.Email},
		Surface: "native", IdempotencyKey: api.NewUUID(),
		ResponseMessage: respMsg(),
		Now:             func() time.Time { return time.Now().UTC().Add(10 * time.Minute) },
	})
	require.NoError(t, err)

	// Check Bob's view: all 4 open prompts wait on Bob, oldest first.
	bobResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, conversationDecisionsPath(f.topicID), nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 4, WaitingOnOthers: 0}, bobResp.Counts)
	require.Len(t, bobResp.Prompts, 4)

	assert.Equal(t, p1Resp.MessageID, bobResp.Prompts[0].PromptMessageID)
	assert.False(t, bobResp.Prompts[0].PromptAvailable, "purged prompt must have promptAvailable=false")
	assert.Equal(t, "single", bobResp.Prompts[0].AnswerMode)
	assert.Equal(t, 1, bobResp.Prompts[0].OpenCount)
	assert.Equal(t, 1, bobResp.Prompts[0].TotalCount)

	assert.Equal(t, p2Resp.MessageID, bobResp.Prompts[1].PromptMessageID)
	assert.True(t, bobResp.Prompts[1].PromptAvailable)
	assert.Equal(t, "set", bobResp.Prompts[1].AnswerMode)
	assert.Equal(t, 2, bobResp.Prompts[1].OpenCount)
	assert.Equal(t, 3, bobResp.Prompts[1].TotalCount)
	assert.Equal(t, "Second set Q?", bobResp.Prompts[1].Question, "should pick first open row's question")

	assert.Equal(t, p3Resp.MessageID, bobResp.Prompts[2].PromptMessageID)
	assert.True(t, bobResp.Prompts[2].WaitingOnYou)
	assert.True(t, bobResp.Prompts[2].AwaitingConfirmation)

	assert.Equal(t, p4NewResp.MessageID, bobResp.Prompts[3].PromptMessageID)
	assert.Equal(t, "New v2?", bobResp.Prompts[3].Question)

	// Check Alice's view: 3 waiting on Alice (p1, p2, p4New), 1 waiting on others (p3, Bob to confirm).
	aliceResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, conversationDecisionsPath(f.topicID), nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 3, WaitingOnOthers: 1}, aliceResp.Counts)
	require.Len(t, aliceResp.Prompts, 4)
	assert.Equal(t, p3Resp.MessageID, aliceResp.Prompts[3].PromptMessageID)
	assert.False(t, aliceResp.Prompts[3].WaitingOnYou)
	assert.True(t, aliceResp.Prompts[3].AwaitingConfirmation)
	assert.Equal(t, "Waiting for the recorded answerer to confirm", aliceResp.Prompts[3].DisabledReason)

	// Parity with GET /api/v1/projects/{id}/decisions?tab=pending&conversation=K for Alice.
	projPath := "/api/v1/projects/" + f.project.ID + "/decisions?tab=pending&conversation=" + url.QueryEscape(f.topicID)
	projResp := decodePendingDecisions(t, doRequestAsUser(t, f.srv, f.alice, http.MethodGet, projPath, nil))
	require.Len(t, projResp.Prompts, len(aliceResp.Prompts))
	projByMsg := make(map[string]ProjectDecisionPromptView, len(projResp.Prompts))
	for _, p := range projResp.Prompts {
		projByMsg[p.PromptMessageID] = p
	}
	for _, item := range aliceResp.Prompts {
		pp, ok := projByMsg[item.PromptMessageID]
		require.True(t, ok, "missing prompt %s in project endpoint", item.PromptMessageID)
		assert.Equal(t, pp.WaitingOnYou, item.WaitingOnYou)
		assert.Equal(t, pp.PromptAvailable, item.PromptAvailable)
		assert.Equal(t, pp.DisabledReason, item.DisabledReason)
	}

	// Demote Bob to read-only chat reader: all 4 prompts become waitingOnOthers with disabledReason.
	f.makeChatReader(t, f.bob)
	roResp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, conversationDecisionsPath(f.topicID), nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 0, WaitingOnOthers: 4}, roResp.Counts)
	require.Len(t, roResp.Prompts, 4)
	for _, item := range roResp.Prompts {
		assert.False(t, item.WaitingOnYou)
		assert.NotEmpty(t, item.DisabledReason)
	}
}

// singleKeySpyStore wraps *entadapter.CompositeStore (preserving DB() and
// retention interfaces) to record every ListOpenPrompts call's arguments.
type singleKeySpyStore struct {
	*entadapter.CompositeStore
	mu    sync.Mutex
	calls []listOpenPromptsCall
}

type listOpenPromptsCall struct {
	projectIDs []string
	chatKeys   []string
}

func (s *singleKeySpyStore) ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*store.ProjectDecisionPrompt, error) {
	s.mu.Lock()
	s.calls = append(s.calls, listOpenPromptsCall{
		projectIDs: append([]string(nil), projectIDs...),
		chatKeys:   append([]string(nil), chatKeys...),
	})
	s.mu.Unlock()
	return s.CompositeStore.ListOpenPrompts(ctx, projectIDs, chatKeys)
}

func TestConversationDecisions_OrderCapAndSingleKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "conv-dec-spy.db")
	client, err := entc.OpenSQLite(dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	base := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = base.Close() })

	spy := &singleKeySpyStore{CompositeStore: base}
	srv, _ := testServerOnMigratedStore(t, spy)
	alice, bob, project := setupDemoPolicyOn(t, srv, spy)
	f := newDecisionsFixtureOn(t, srv, spy, alice, bob, project)
	setDecisionsExperiment(t, f.srv, true)
	ctx := context.Background()

	// Seed 105 open prompts in f.topicID across 3 agents (each agent has a 50-pending cap)
	// with strictly increasing timestamps, plus 1 prompt in another topic (which must not be queried).
	agents := []*store.Agent{f.agent}
	for a := 1; a <= 2; a++ {
		ag := &store.Agent{
			ID: api.NewUUID(), Slug: fmt.Sprintf("cap-agent-%d", a), Name: fmt.Sprintf("Cap Agent %d", a),
			ProjectID: f.project.ID, OwnerID: f.bob.ID, CreatedBy: f.bob.ID, Ancestry: []string{f.bob.ID},
			Phase: "running", MessageMode: store.MessageModeProject,
		}
		require.NoError(t, spy.CreateAgent(ctx, ag))
		agents = append(agents, ag)
	}
	otherTopic := WebChatTopic{
		ID: api.NewUUID(), ProjectID: f.project.ID, Name: "other-topic", CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, f.wcs.CreateTopic(ctx, otherTopic))

	baseTime := time.Now().UTC().Add(-200 * time.Minute)
	var firstMsgID, hundredthMsgID string
	for i := 0; i < 105; i++ {
		ag := agents[i/35]
		decs, derr := messages.NormalizeDecisions("decisions", decisionInputs(t,
			fmt.Sprintf(`[{"id":"cap-%03d","question":"Question %03d?","choices":["yes","no"]}]`, i, i)))
		require.NoError(t, derr)
		msg := &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID,
			Sender: "agent:" + ag.Slug, SenderID: ag.ID,
			Recipient: "chat", ThreadID: f.topicID, Channel: "web",
			CreatedAt: baseTime.Add(time.Duration(i) * time.Minute),
		}
		_, cerr := spy.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
			Message: msg, Intro: "intro", AskerAgentID: ag.ID, AskerAgentName: ag.Slug, Decisions: decs,
		})
		require.NoError(t, cerr)
		if i == 0 {
			firstMsgID = msg.ID
		}
		if i == 99 {
			hundredthMsgID = msg.ID
		}
	}

	// One prompt in otherTopic.
	otherDecs, err := messages.NormalizeDecisions("decisions", decisionInputs(t, `[{"id":"other","question":"Other topic?","choices":["yes","no"]}]`))
	require.NoError(t, err)
	_, err = spy.CreateDecisionPrompt(ctx, &store.DecisionPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: f.project.ID,
			Sender: "agent:" + f.agent.Slug, SenderID: f.agent.ID,
			Recipient: "chat", ThreadID: otherTopic.ID, Channel: "web",
			CreatedAt: baseTime,
		},
		Intro: "other", AskerAgentID: f.agent.ID, AskerAgentName: f.agent.Slug, Decisions: otherDecs,
	})
	require.NoError(t, err)

	// Reset spy calls and issue the conversation decisions request.
	spy.mu.Lock()
	spy.calls = nil
	spy.mu.Unlock()

	resp := decodeConversationDecisions(t, doRequestAsUser(t, f.srv, f.bob, http.MethodGet, conversationDecisionsPath(f.topicID), nil))
	assert.Equal(t, ProjectDecisionCounts{WaitingOnYou: 105, WaitingOnOthers: 0}, resp.Counts, "counts stay exact past the 100 cap")
	assert.True(t, resp.Truncated)
	require.Len(t, resp.Prompts, maxConversationOpenPrompts)
	assert.Equal(t, firstMsgID, resp.Prompts[0].PromptMessageID, "oldest prompt comes first")
	assert.Equal(t, hundredthMsgID, resp.Prompts[99].PromptMessageID, "100th oldest prompt is last in capped list")

	// Verify ListOpenPrompts received only the single chat key and project ID.
	spy.mu.Lock()
	calls := append([]listOpenPromptsCall(nil), spy.calls...)
	spy.mu.Unlock()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{f.project.ID}, calls[0].projectIDs)
	assert.Equal(t, []string{f.topicID}, calls[0].chatKeys)
}
