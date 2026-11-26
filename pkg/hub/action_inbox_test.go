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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const userMeSecretRequestsPath = "/api/v1/users/me/secret-requests"

type secretRequestsInboxResponse struct {
	Counts            ActionInboxCounts           `json:"counts"`
	FilteredCounts    *ActionInboxCounts          `json:"filteredCounts,omitempty"`
	Cards             []SecretRequestCardResponse `json:"cards"`
	Facets            ActionInboxFacets           `json:"facets"`
	TruncatedProjects bool                        `json:"truncatedProjects"`
	Capped            bool                        `json:"capped"`
	NextCursor        string                      `json:"nextCursor,omitempty"`
}

func enableHubExperiments(t *testing.T, srv *Server, st store.Store, overrides map[string]bool) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(map[string]any{"overrides": overrides})
	require.NoError(t, err)
	_, err = st.UpsertHubSetting(ctx, "experiments", raw, "test", -1, "managed")
	require.NoError(t, err)
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

func seedThreadSecretRequest(
	t *testing.T,
	st store.Store,
	projID, agentID, agentName, chatKey string,
	now time.Time,
	reqs []messages.SecretRequest,
	update bool,
) *store.SecretRequestPromptResult {
	t.Helper()
	ctx := context.Background()
	msg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   projID,
		Sender:      "agent:" + agentName,
		SenderID:    agentID,
		Recipient:   chatKey,
		RecipientID: chatKey,
		ThreadID:    chatKey,
		Msg:         "placeholder",
		Type:        messages.TypeSecretRequest,
	}
	res, err := st.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message:   msg,
		Intro:     "Need credentials for deploy",
		AgentID:   agentID,
		AgentName: agentName,
		Requests:  reqs,
		Update:    update,
		Now:       now,
	})
	require.NoError(t, err)
	return res
}

func seedDMSecretRequest(
	t *testing.T,
	st store.Store,
	projID, agentID, agentName, userID string,
	now time.Time,
	reqs []messages.SecretRequest,
) *store.SecretRequestPromptResult {
	t.Helper()
	ctx := context.Background()
	dmKey, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)
	msg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   projID,
		Sender:      "agent:" + agentName,
		SenderID:    agentID,
		Recipient:   "user:" + userID,
		RecipientID: userID,
		ThreadID:    dmKey,
		Msg:         "placeholder",
		Type:        messages.TypeSecretRequest,
	}
	res, err := st.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message:   msg,
		Intro:     "Direct secret request",
		AgentID:   agentID,
		AgentName: agentName,
		Requests:  reqs,
		Now:       now,
	})
	require.NoError(t, err)
	return res
}

func attachTestWebChatStore(t *testing.T, srv *Server) {
	t.Helper()
	if srv.webChatStore != nil {
		return
	}
	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
}

func createUserProjectTopic(t *testing.T, srv *Server, projID, name, createdBy string) string {
	t.Helper()
	attachTestWebChatStore(t, srv)
	ctx := context.Background()
	wcs := srv.webChatStore
	require.NotNil(t, wcs)
	topicID := api.NewUUID()
	err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projID,
		Name:      name,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	return topicID
}

func doActionInboxBearerRequest(t *testing.T, srv *Server, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestUserMeSecretRequests_AuthorizationAndScope(t *testing.T) {
	ctx := context.Background()
	f := newSecretRequestsFixture(t, false)
	st := f.store
	srv := f.srv

	// 1. Gate off: POST returns 405 before gate; GET returns 404 (AC-13).
	aliceTok := f.sessionToken(t, f.alice)
	rrPostOff := doActionInboxBearerRequest(t, srv, http.MethodPost, userMeSecretRequestsPath, aliceTok)
	assert.Equal(t, http.StatusMethodNotAllowed, rrPostOff.Code)
	rrGetOff := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, aliceTok)
	assert.Equal(t, http.StatusNotFound, rrGetOff.Code)

	// Enable hub.agent_secrets.
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.AgentSecrets:   true,
		experiments.InboxDashboard: true,
	})

	// 2. Caller authentication & credential matrix:
	// Unauthenticated -> 401.
	rrUnauth := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, "")
	assert.Equal(t, http.StatusUnauthorized, rrUnauth.Code)

	// Agent identity -> 403.
	agentReq := httptest.NewRequest(http.MethodGet, userMeSecretRequestsPath, nil)
	agentReq = agentReq.WithContext(agentCtx(agentReq.Context(), f.agent))
	rrAgent := httptest.NewRecorder()
	srv.handleUserMeActionInbox(&secretRequestActionInboxKind{srv: srv})(rrAgent, agentReq)
	assert.Equal(t, http.StatusForbidden, rrAgent.Code)

	// Visitor -> 403.
	visitor := &store.User{
		ID:          api.NewUUID(),
		Email:       "visitor@example.com",
		DisplayName: "Visitor",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, visitor))
	visIdentity := NewVisitorIdentity(visitor.ID, "https://issuer.example.com", "sub-visitor", visitor.Email, visitor.DisplayName)
	visReq := httptest.NewRequest(http.MethodGet, userMeSecretRequestsPath, nil)
	visReq = visReq.WithContext(contextWithIdentity(visReq.Context(), visIdentity))
	visRR := httptest.NewRecorder()
	srv.handleUserMeActionInbox(&secretRequestActionInboxKind{srv: srv})(visRR, visReq)
	assert.Equal(t, http.StatusForbidden, visRR.Code)

	// User Access Token (UAT) -> 403 (AC-6 session-only).
	uatRaw, _, err := srv.uatService.CreateToken(rs4MintContext(f.alice.ID), f.alice.ID, "sr-uat", f.project.ID, []string{"inbox:read", "project:read", "agent:read"}, nil)
	require.NoError(t, err)
	rrUAT := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, uatRaw)
	assert.Equal(t, http.StatusForbidden, rrUAT.Code)

	// 3. Seed projects, topics, agents, and secret requests:
	// - Project A (f.project): Alice is project-owner (agent.read + agent.attach),
	//   Carol is project-viewer (agent.read, NO agent.attach), Bob is non-member.
	// - Project B: Bob is project-owner; Alice and Carol are non-members.
	// - Project C ("Alpha Project C"): Alice is project-owner; Bob and Carol are non-members.
	projB := &store.Project{
		ID:   api.NewUUID(),
		Slug: "sr-proj-b-" + api.NewUUID()[:8],
		Name: "Beta Project B",
	}
	require.NoError(t, st.CreateProject(ctx, projB))
	ownerRD, err := st.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projB.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.bob.ID,
	})
	require.NoError(t, err)
	agentB := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "beta-agent",
		Name:      "beta-agent",
		ProjectID: projB.ID,
		OwnerID:   f.bob.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, agentB))

	projC := &store.Project{
		ID:   api.NewUUID(),
		Slug: "sr-proj-c-" + api.NewUUID()[:8],
		Name: "Alpha Project C",
	}
	require.NoError(t, st.CreateProject(ctx, projC))
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projC.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.alice.ID,
	})
	require.NoError(t, err)
	agentC := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "alpha-agent",
		Name:      "alpha-agent",
		ProjectID: projC.ID,
		OwnerID:   f.alice.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, agentC))

	topicA := createUserProjectTopic(t, srv, f.project.ID, "general-a", f.alice.ID)
	topicB := createUserProjectTopic(t, srv, projB.ID, "general-b", f.bob.ID)

	base := time.Now().UTC().Truncate(time.Microsecond)

	// Card 1: Thread in Project A (2 keys) -> visible & answerable by Alice; readable-only by Carol; invisible to Bob.
	card1 := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, base, []messages.SecretRequest{
		{Key: "API_TOKEN", KeyNorm: "API_TOKEN", Description: "Upstream API token", SecretType: "environment"},
		{Key: "DB_PASS", KeyNorm: "DB_PASS", Description: "Database password", SecretType: "environment"},
	}, false)

	// Card 2: Thread in Project B -> visible to Bob; invisible to Alice (non-member) and Carol.
	_ = seedThreadSecretRequest(t, st, projB.ID, agentB.ID, agentB.Name, topicB, base.Add(time.Second), []messages.SecretRequest{
		{Key: "BOB_ONLY_KEY", KeyNorm: "BOB_ONLY_KEY", Description: "Secret in project B", SecretType: "environment"},
	}, false)

	// Card 3: DM from agentC (in Project C, "Alpha Project C") to Alice -> visible & answerable by Alice; invisible to Bob and Carol.
	card3DMAlice := seedDMSecretRequest(t, st, projC.ID, agentC.ID, agentC.Name, f.alice.ID, base.Add(2*time.Second), []messages.SecretRequest{
		{Key: "ALICE_DM_KEY", KeyNorm: "ALICE_DM_KEY", Description: "DM secret for Alice", SecretType: "environment"},
	})

	// Card 4: DM from f.agent (in Project A) to Carol -> visible to Carol (readable, not answerable since Carol lacks ActionAttach); invisible to Alice even though Alice owns Project A!
	card4DMCarol := seedDMSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, f.carol.ID, base.Add(3*time.Second), []messages.SecretRequest{
		{Key: "CAROL_DM_KEY", KeyNorm: "CAROL_DM_KEY", Description: "Private DM for Carol", SecretType: "environment"},
	})

	// Card 5: DM from agentB (in Project B, where Alice lacks agent.read) to Alice -> invisible to Alice AND invisible to Bob!
	_ = seedDMSecretRequest(t, st, projB.ID, agentB.ID, agentB.Name, f.alice.ID, base.Add(4*time.Second), []messages.SecretRequest{
		{Key: "UNREADABLE_DM_KEY", KeyNorm: "UNREADABLE_DM_KEY", Description: "DM from unreadable agent", SecretType: "environment"},
	})

	// Alice: sees Card 3 (DM to Alice, 1 row) + Card 1 (Project A thread, 2 rows) = 3 open, 3 answerable.
	// Does NOT see Card 2 (Project B thread), Card 4 (DM to Carol), or Card 5 (DM from unreadable agentB).
	rrAlice := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, aliceTok)
	require.Equal(t, http.StatusOK, rrAlice.Code, rrAlice.Body.String())
	var aliceResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAlice.Body.Bytes(), &aliceResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 3, Open: 3}, aliceResp.Counts)
	require.NotNil(t, aliceResp.FilteredCounts)
	assert.Equal(t, ActionInboxCounts{Answerable: 3, Open: 3}, *aliceResp.FilteredCounts)
	require.Len(t, aliceResp.Cards, 2)
	// Default sort=recent: Card 3 (t+2s) before Card 1 (t+0s).
	assert.Equal(t, card3DMAlice.Message.ID, aliceResp.Cards[0].MessageID)
	assert.Equal(t, card1.Message.ID, aliceResp.Cards[1].MessageID)
	assert.True(t, aliceResp.Cards[0].CanRespond)
	assert.True(t, aliceResp.Cards[1].CanRespond)

	// Alice cannot leak Carol's DM or Project B's thread via q or project filter.
	rrAliceProbe := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?q=CAROL_DM_KEY", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceProbe.Code)
	var aliceProbe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceProbe.Body.Bytes(), &aliceProbe))
	assert.Empty(t, aliceProbe.Cards)
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 0}, *aliceProbe.FilteredCounts)
	assert.Equal(t, ActionInboxCounts{Answerable: 3, Open: 3}, aliceProbe.Counts)

	// Alice filters by q matching description (case-insensitive) and project facet.
	rrAliceQ := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?q=database+password", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceQ.Code)
	var aliceQ secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceQ.Body.Bytes(), &aliceQ))
	require.Len(t, aliceQ.Cards, 1)
	assert.Equal(t, card1.Message.ID, aliceQ.Cards[0].MessageID)
	assert.Equal(t, ActionInboxCounts{Answerable: 1, Open: 1}, *aliceQ.FilteredCounts)

	// sort=project puts "Alpha Project C" (Card 3) before "Secret Project" (Card 1).
	rrAliceSortProj := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?sort=project", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceSortProj.Code)
	var aliceSortProj secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceSortProj.Body.Bytes(), &aliceSortProj))
	require.Len(t, aliceSortProj.Cards, 2)
	assert.Equal(t, projC.ID, aliceSortProj.Cards[0].ProjectID)
	assert.Equal(t, f.project.ID, aliceSortProj.Cards[1].ProjectID)

	// Open tab pagination: limit=1 returns Card 3 + nextCursor; second page returns Card 1.
	rrPage1 := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1", aliceTok)
	require.Equal(t, http.StatusOK, rrPage1.Code)
	var page1 secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrPage1.Body.Bytes(), &page1))
	require.Len(t, page1.Cards, 1)
	assert.Equal(t, card3DMAlice.Message.ID, page1.Cards[0].MessageID)
	require.NotEmpty(t, page1.NextCursor)

	rrPage2 := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1&cursor="+page1.NextCursor, aliceTok)
	require.Equal(t, http.StatusOK, rrPage2.Code)
	var page2 secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrPage2.Body.Bytes(), &page2))
	require.Len(t, page2.Cards, 1)
	assert.Equal(t, card1.Message.ID, page2.Cards[0].MessageID)
	assert.Empty(t, page2.NextCursor)

	// Cursor bound to different query -> 400.
	rrBadCur := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1&sort=project&cursor="+page1.NextCursor, aliceTok)
	assert.Equal(t, http.StatusBadRequest, rrBadCur.Code)

	// Carol (project-viewer on Project A: agent.read, NO agent.attach):
	// Sees Card 4 (DM to Carol, 1 row) + Card 1 (Project A thread, 2 rows) = 3 open, 0 answerable!
	carolTok := f.sessionToken(t, f.carol)
	rrCarol := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, carolTok)
	require.Equal(t, http.StatusOK, rrCarol.Code)
	var carolResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrCarol.Body.Bytes(), &carolResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 3}, carolResp.Counts)
	require.Len(t, carolResp.Cards, 2)
	assert.Equal(t, card4DMCarol.Message.ID, carolResp.Cards[0].MessageID)
	assert.False(t, carolResp.Cards[0].CanRespond)
	assert.Contains(t, carolResp.Cards[0].Reason, "agent.attach")
	assert.False(t, carolResp.Cards[1].CanRespond)
	assert.Contains(t, carolResp.Cards[1].Reason, "eligible responder")

	// Bob (project-owner of Project B, non-member of Project A):
	// Sees ONLY Card 2 (Project B thread). Must NOT see Alice's DM (even though agentB is in Project B!) or Project A.
	bobTok := f.sessionToken(t, f.bob)
	rrBob := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, bobTok)
	require.Equal(t, http.StatusOK, rrBob.Code)
	var bobResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrBob.Body.Bytes(), &bobResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 1, Open: 1}, bobResp.Counts)
	require.Len(t, bobResp.Cards, 1)
	assert.Equal(t, "BOB_ONLY_KEY", bobResp.Cards[0].Rows[0].Key)

	// Outsider (not a member of any project, no DMs) sees nothing.
	outsider := &store.User{
		ID:          api.NewUUID(),
		Email:       "outsider@example.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, outsider))
	outsiderTok := f.sessionToken(t, outsider)
	rrOut := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?project="+f.project.ID, outsiderTok)
	require.Equal(t, http.StatusOK, rrOut.Code)
	var outResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrOut.Body.Bytes(), &outResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 0}, outResp.Counts)
	assert.Empty(t, outResp.Cards)

	// 4. Resolved tab (AC-6): terminal non-replaced rows newest resolved_at first,
	// with per-row authorization enforced across cards, facets, and filter probes.
	// Resolve Card 1 (t+10s), Card 3 (t+20s), Card 4 (Carol's DM in Project A, t+12s),
	// Card 2 (Project B thread, t+14s), and Card 5 (DM from unreadable agentB to Alice, t+16s).
	_, err = st.DismissSecretRequests(ctx, &store.DismissSecretRequestsInput{
		MessageID: card1.Message.ID,
		UserID:    f.alice.ID,
		UserName:  f.alice.DisplayName,
		Reason:    "not needed",
		Now:       base.Add(10 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: f.agent.ID,
		Keys:    []string{"CAROL_DM_KEY"},
		Reason:  "carol dm withdrawn",
		Now:     base.Add(12 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: agentB.ID,
		Keys:    []string{"BOB_ONLY_KEY"},
		Reason:  "bob thread withdrawn",
		Now:     base.Add(14 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: agentB.ID,
		Keys:    []string{"UNREADABLE_DM_KEY"},
		Reason:  "unreadable dm withdrawn",
		Now:     base.Add(16 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: agentC.ID,
		Keys:    []string{"ALICE_DM_KEY"},
		Reason:  "agent cancelled",
		Now:     base.Add(20 * time.Second),
	})
	require.NoError(t, err)
	// Create + replace a key in Project A; replaced v1 must NOT appear in Resolved tab.
	_ = seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, base.Add(21*time.Second), []messages.SecretRequest{
		{Key: "REPL_KEY", KeyNorm: "REPL_KEY", SecretType: "environment"},
	}, false)
	_ = seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, base.Add(22*time.Second), []messages.SecretRequest{
		{Key: "REPL_KEY", KeyNorm: "REPL_KEY", SecretType: "environment"},
	}, true)

	// Alice's Resolved tab: ONLY Card 3 (t+20s) and Card 1 (t+10s).
	// Must exclude Card 4 (Carol's DM in Project A), Card 2 (Project B), and Card 5 (DM from unreadable agentB).
	rrRes := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?tab=resolved", aliceTok)
	require.Equal(t, http.StatusOK, rrRes.Code)
	var resResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrRes.Body.Bytes(), &resResp))
	require.Len(t, resResp.Cards, 2)
	assert.Equal(t, card3DMAlice.Message.ID, resResp.Cards[0].MessageID)
	assert.Equal(t, card1.Message.ID, resResp.Cards[1].MessageID)
	assert.Equal(t, ActionInboxCounts{Answerable: 1, Open: 1}, resResp.Counts, "open counts reflect remaining REPL_KEY v2")
	for _, pf := range resResp.Facets.Projects {
		assert.NotEqual(t, projB.ID, pf.ID, "resolved facets must not leak unreadable project B")
	}
	for _, af := range resResp.Facets.Agents {
		assert.NotEqual(t, agentB.ID, af.ID, "resolved facets must not leak unreadable agent B")
	}

	// Alice cannot leak Card 4, Card 2, or Card 5 on Resolved via project, agent, or q filters.
	for _, probePath := range []string{
		userMeSecretRequestsPath + "?tab=resolved&q=CAROL_DM_KEY",
		userMeSecretRequestsPath + "?tab=resolved&q=UNREADABLE_DM_KEY",
		userMeSecretRequestsPath + "?tab=resolved&q=BOB_ONLY_KEY",
		userMeSecretRequestsPath + "?tab=resolved&project=" + projB.ID,
		userMeSecretRequestsPath + "?tab=resolved&agent=" + agentB.ID,
	} {
		rrProbe := doActionInboxBearerRequest(t, srv, http.MethodGet, probePath, aliceTok)
		require.Equal(t, http.StatusOK, rrProbe.Code)
		var probeResp secretRequestsInboxResponse
		require.NoError(t, json.Unmarshal(rrProbe.Body.Bytes(), &probeResp))
		assert.Empty(t, probeResp.Cards, "probe %s must return no cards", probePath)
		assert.Empty(t, probeResp.Facets.Agents, "probe %s must return no agent facets", probePath)
		assert.Empty(t, probeResp.Facets.Projects, "probe %s must return no project facets", probePath)
	}
	// Filtering Alice's Resolved tab by Project A returns only Card 1 (not Carol's DM Card 4).
	rrAliceProjA := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?tab=resolved&project="+f.project.ID, aliceTok)
	require.Equal(t, http.StatusOK, rrAliceProjA.Code)
	var aliceProjAResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceProjA.Body.Bytes(), &aliceProjAResp))
	require.Len(t, aliceProjAResp.Cards, 1)
	assert.Equal(t, card1.Message.ID, aliceProjAResp.Cards[0].MessageID)

	// Bob's Resolved tab: includes Card 2 (Project B thread) and excludes Card 4 (Carol's DM) and Card 5 (Alice's DM from agentB).
	rrBobRes := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?tab=resolved", bobTok)
	require.Equal(t, http.StatusOK, rrBobRes.Code)
	var bobResResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrBobRes.Body.Bytes(), &bobResResp))
	require.Len(t, bobResResp.Cards, 1)
	assert.Equal(t, "BOB_ONLY_KEY", bobResResp.Cards[0].Rows[0].Key)
	for _, probePath := range []string{
		userMeSecretRequestsPath + "?tab=resolved&q=CAROL_DM_KEY",
		userMeSecretRequestsPath + "?tab=resolved&q=UNREADABLE_DM_KEY",
		userMeSecretRequestsPath + "?tab=resolved&project=" + f.project.ID,
	} {
		rrProbe := doActionInboxBearerRequest(t, srv, http.MethodGet, probePath, bobTok)
		require.Equal(t, http.StatusOK, rrProbe.Code)
		var probeResp secretRequestsInboxResponse
		require.NoError(t, json.Unmarshal(rrProbe.Body.Bytes(), &probeResp))
		assert.Empty(t, probeResp.Cards, "bob probe %s must return no cards", probePath)
	}

	// Carol's Resolved tab: includes Card 4 (Carol's DM, t+12s) and Card 1 (Project A thread, t+10s).
	rrCarolRes := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?tab=resolved", carolTok)
	require.Equal(t, http.StatusOK, rrCarolRes.Code)
	var carolResResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrCarolRes.Body.Bytes(), &carolResResp))
	require.Len(t, carolResResp.Cards, 2)
	assert.Equal(t, card4DMCarol.Message.ID, carolResResp.Cards[0].MessageID)
	assert.Equal(t, card1.Message.ID, carolResResp.Cards[1].MessageID)

	// 5. Caps: truncatedProjects and capped.
	// Add Alice as member of Project B so she has 2 member projects, then set maxProjects=1.
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projB.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.alice.ID,
	})
	require.NoError(t, err)

	srv.actionInboxMaxProjectsForTest = 1
	rrTrunc := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
	require.Equal(t, http.StatusOK, rrTrunc.Code)
	var truncResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrTrunc.Body.Bytes(), &truncResp))
	assert.True(t, truncResp.TruncatedProjects)
	srv.actionInboxMaxProjectsForTest = 0

	// Seed 2 more open rows in Project A (total 3 open rows in Project A + 1 in Project B) and set scanCap=2.
	_ = seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, base.Add(25*time.Second), []messages.SecretRequest{
		{Key: "CAP_KEY_1", KeyNorm: "CAP_KEY_1", SecretType: "environment"},
		{Key: "CAP_KEY_2", KeyNorm: "CAP_KEY_2", SecretType: "environment"},
	}, false)
	srv.actionInboxScanCapForTest = 2
	rrCapped := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
	require.Equal(t, http.StatusOK, rrCapped.Code)
	var cappedResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrCapped.Body.Bytes(), &cappedResp))
	assert.True(t, cappedResp.Capped)
	srv.actionInboxScanCapForTest = 0
}

func TestInboxSummary_AgentSecretsParityAndGates(t *testing.T) {
	f := newSecretRequestsFixture(t, false)
	st := f.store
	srv := f.srv

	// 1. Inbox on, hub.agent_secrets off -> agent_secrets is disabled (AC-13).
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   false,
	})

	aliceTok := f.sessionToken(t, f.alice)
	rrOff := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rrOff.Code)
	var sumOff InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrOff.Body.Bytes(), &sumOff))
	asOff, exists := sumOff.Categories[InboxCategoryAgentSecrets]
	require.True(t, exists)
	assert.Equal(t, InboxStateDisabled, asOff.State)
	assert.False(t, asOff.CountsTowardTotal)
	assert.Equal(t, 0, asOff.Count)
	assert.Equal(t, agentSecretsInboxHref, asOff.Href)

	// 2. Enable hub.agent_secrets as well.
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   true,
	})

	topicA := createUserProjectTopic(t, srv, f.project.ID, "parity-topic", f.alice.ID)
	base := time.Now().UTC().Truncate(time.Microsecond)
	firstRes := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, base, []messages.SecretRequest{
		{Key: "PARITY_KEY_1", KeyNorm: "PARITY_KEY_1", SecretType: "environment"},
		{Key: "PARITY_KEY_2", KeyNorm: "PARITY_KEY_2", SecretType: "environment"},
	}, false)
	_ = seedDMSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, f.alice.ID, base.Add(time.Minute), []messages.SecretRequest{
		{Key: "PARITY_DM_KEY", KeyNorm: "PARITY_DM_KEY", SecretType: "environment"},
	})

	// AC-7: agent_secrets.count == GET /users/me/secret-requests?limit=0 -> counts.answerable,
	// and decisions.count is unchanged.
	rrSum := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rrSum.Code)
	var sumOn InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrSum.Body.Bytes(), &sumOn))

	rrProbe := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
	require.Equal(t, http.StatusOK, rrProbe.Code)
	var probe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrProbe.Body.Bytes(), &probe))

	asCat := sumOn.Categories[InboxCategoryAgentSecrets]
	assert.Equal(t, InboxStateOK, asCat.State)
	assert.True(t, asCat.CountsTowardTotal)
	assert.Equal(t, 3, asCat.Count)
	assert.Equal(t, probe.Counts.Answerable, asCat.Count, "AC-7 count parity")
	assert.Equal(t, map[string]int{
		InboxBreakdownAgents:          1,
		InboxBreakdownProjects:        1,
		InboxBreakdownSecretsProjects: 1,
	}, asCat.Breakdown)
	require.NotNil(t, asCat.NextExpiresAt)
	assert.Equal(t, firstRes.Requests[0].Row.ExpiresAt.UTC(), asCat.NextExpiresAt.UTC())
	assert.Equal(t, 0, sumOn.Categories[InboxCategoryDecisions].Count, "decisions count unchanged")

	// Carol (no ActionAttach): summary count == 0 == probe counts.answerable (while probe counts.open == 2).
	carolTok := f.sessionToken(t, f.carol)
	rrCarolSum := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", carolTok)
	require.Equal(t, http.StatusOK, rrCarolSum.Code)
	var carolSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrCarolSum.Body.Bytes(), &carolSum))

	rrCarolProbe := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", carolTok)
	require.Equal(t, http.StatusOK, rrCarolProbe.Code)
	var carolProbe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrCarolProbe.Body.Bytes(), &carolProbe))

	assert.Equal(t, 0, carolSum.Categories[InboxCategoryAgentSecrets].Count)
	assert.Equal(t, 0, carolProbe.Counts.Answerable)
	assert.Equal(t, 2, carolProbe.Counts.Open)

	// UAT caller: agent_secrets is unavailable (§4.4, AC-6).
	uatRaw, _, err := srv.uatService.CreateToken(rs4MintContext(f.alice.ID), f.alice.ID, "inbox-uat", f.project.ID, []string{"inbox:read"}, nil)
	require.NoError(t, err)
	rrUAT := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", uatRaw)
	require.Equal(t, http.StatusOK, rrUAT.Code)
	var uatSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrUAT.Body.Bytes(), &uatSum))
	uatCat := uatSum.Categories[InboxCategoryAgentSecrets]
	assert.Equal(t, InboxStateUnavailable, uatCat.State)
	assert.Equal(t, 0, uatCat.Count)
	assert.False(t, uatCat.CountsTowardTotal)
}

func TestSecretRequest_DMUpdateEmitsInboxHint(t *testing.T) {
	ctx := context.Background()
	f := newSecretRequestsFixture(t, false)
	pub := NewChannelEventPublisher()
	defer pub.Close()
	f.srv.SetEventPublisher(pub)
	enableHubExperiments(t, f.srv, f.store, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   true,
	})

	ch, unsub := pub.Subscribe("user." + f.alice.ID + ".inbox")
	defer unsub()

	base := time.Now().UTC().Truncate(time.Microsecond)
	dmRes := seedDMSecretRequest(t, f.store, f.project.ID, f.agent.ID, f.agent.Name, f.alice.ID, base, []messages.SecretRequest{
		{Key: "HINT_DM_KEY", KeyNorm: "HINT_DM_KEY", SecretType: "environment"},
	})
	f.srv.publishSecretRequestUpdated(ctx, dmRes.Requests[0].Row, nil)

	select {
	case ev := <-ch:
		var hint InboxHint
		require.NoError(t, json.Unmarshal(ev.Data, &hint))
		assert.Equal(t, InboxCategoryAgentSecrets, hint.Category)
	case <-time.After(2 * time.Second):
		t.Fatal("expected agent_secrets inbox hint for DM secret request update")
	}

	// Thread updates must NOT emit per-user inbox hints.
	topicA := createUserProjectTopic(t, f.srv, f.project.ID, "hint-topic", f.alice.ID)
	threadRes := seedThreadSecretRequest(t, f.store, f.project.ID, f.agent.ID, f.agent.Name, topicA, base.Add(time.Second), []messages.SecretRequest{
		{Key: "HINT_THREAD_KEY", KeyNorm: "HINT_THREAD_KEY", SecretType: "environment"},
	}, false)
	f.srv.publishSecretRequestUpdated(ctx, threadRes.Requests[0].Row, nil)

	select {
	case ev := <-ch:
		t.Fatalf("unexpected user inbox hint for thread secret request: %s", string(ev.Data))
	case <-time.After(100 * time.Millisecond):
	}
	// Multi-key DM card dismiss emits exactly ONE inbox hint per message (not one per row).
	dmMulti := seedDMSecretRequest(t, f.store, f.project.ID, f.agent.ID, f.agent.Name, f.alice.ID, base.Add(2*time.Second), []messages.SecretRequest{
		{Key: "HINT_MULTI_1", KeyNorm: "HINT_MULTI_1", SecretType: "environment"},
		{Key: "HINT_MULTI_2", KeyNorm: "HINT_MULTI_2", SecretType: "environment"},
	})
	aliceTok := f.sessionToken(t, f.alice)
	dismissReq := httptest.NewRequest(http.MethodPost, "/api/v1/secret-requests/"+dmMulti.Message.ID+"/dismiss", strings.NewReader(`{"reason":"not needed"}`))
	dismissReq.Header.Set("Authorization", "Bearer "+aliceTok)
	dismissReq.Header.Set("Content-Type", "application/json")
	dismissRR := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(dismissRR, dismissReq)
	require.Equal(t, http.StatusOK, dismissRR.Code, dismissRR.Body.String())

	select {
	case ev := <-ch:
		var hint InboxHint
		require.NoError(t, json.Unmarshal(ev.Data, &hint))
		assert.Equal(t, InboxCategoryAgentSecrets, hint.Category)
	case <-time.After(2 * time.Second):
		t.Fatal("expected single agent_secrets inbox hint for 2-key DM dismiss")
	}
	select {
	case ev := <-ch:
		t.Fatalf("expected only one inbox hint for 2-key DM dismiss, got extra: %s", string(ev.Data))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUserMeSecretRequests_ResolvedPagination(t *testing.T) {
	ctx := context.Background()
	f := newSecretRequestsFixture(t, false)
	st := f.store
	srv := f.srv
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   true,
	})

	ownerRD, err := st.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	projAlpha := &store.Project{ID: api.NewUUID(), Slug: "alpha-page-" + api.NewUUID()[:8], Name: "Alpha Probe"}
	require.NoError(t, st.CreateProject(ctx, projAlpha))
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projAlpha.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.alice.ID,
	})
	require.NoError(t, err)
	agentAlpha := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "alpha-page-agent",
		Name:      "alpha-page-agent",
		ProjectID: projAlpha.ID,
		OwnerID:   f.alice.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, agentAlpha))

	topicMain := createUserProjectTopic(t, srv, f.project.ID, "pg-main", f.alice.ID)
	topicAlpha := createUserProjectTopic(t, srv, projAlpha.ID, "pg-alpha", f.alice.ID)
	aliceTok := f.sessionToken(t, f.alice)
	base := time.Now().UTC().Truncate(time.Microsecond)

	walkAllPages := func(baseQuery string) (rowIDs []string, pages []secretRequestsInboxResponse) {
		seen := make(map[string]bool)
		cursor := ""
		for i := 0; i < 20; i++ {
			url := userMeSecretRequestsPath + "?" + baseQuery
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			rr := doActionInboxBearerRequest(t, srv, http.MethodGet, url, aliceTok)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var resp secretRequestsInboxResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			pages = append(pages, resp)
			for _, c := range resp.Cards {
				for _, r := range c.Rows {
					assert.False(t, seen[r.RowID], "duplicate row %s across pages for %s", r.RowID, baseQuery)
					seen[r.RowID] = true
					rowIDs = append(rowIDs, r.RowID)
				}
			}
			if resp.NextCursor == "" {
				return rowIDs, pages
			}
			cursor = resp.NextCursor
		}
		t.Fatalf("pagination did not terminate for %s", baseQuery)
		return nil, nil
	}

	// Scenario 1: sort=recent with mixed per-row resolution times on one card.
	// Card M has M_KEY_1 (withdrawn at t+30s) and M_KEY_2 (withdrawn at t+5s).
	// Card N has N_KEY_1 (withdrawn at t+15s).
	cardM := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicMain, base, []messages.SecretRequest{
		{Key: "M_KEY_1", KeyNorm: "M_KEY_1", SecretType: "environment"},
		{Key: "M_KEY_2", KeyNorm: "M_KEY_2", SecretType: "environment"},
	}, false)
	cardN := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicMain, base.Add(time.Second), []messages.SecretRequest{
		{Key: "N_KEY_1", KeyNorm: "N_KEY_1", SecretType: "environment"},
	}, false)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: f.agent.ID, Keys: []string{"M_KEY_2"}, Reason: "early", Now: base.Add(5 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: f.agent.ID, Keys: []string{"N_KEY_1"}, Reason: "middle", Now: base.Add(15 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: f.agent.ID, Keys: []string{"M_KEY_1"}, Reason: "late", Now: base.Add(30 * time.Second),
	})
	require.NoError(t, err)

	// Scenario 2: sort=project with Secret Project (X, t+20s) and Alpha Probe (Y, t+10s).
	cardX := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicMain, base.Add(2*time.Second), []messages.SecretRequest{
		{Key: "X_KEY", KeyNorm: "X_KEY", SecretType: "environment"},
	}, false)
	cardY := seedThreadSecretRequest(t, st, projAlpha.ID, agentAlpha.ID, agentAlpha.Name, topicAlpha, base.Add(3*time.Second), []messages.SecretRequest{
		{Key: "Y_KEY", KeyNorm: "Y_KEY", SecretType: "environment"},
	}, false)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: agentAlpha.ID, Keys: []string{"Y_KEY"}, Reason: "alpha", Now: base.Add(10 * time.Second),
	})
	require.NoError(t, err)
	_, err = st.WithdrawSecretRequests(ctx, &store.WithdrawSecretRequestsInput{
		AgentID: f.agent.ID, Keys: []string{"X_KEY"}, Reason: "secret", Now: base.Add(20 * time.Second),
	})
	require.NoError(t, err)

	// Scenario 3: multi-row card straddling the scan batch boundary (batch size = 2, card has 3 rows).
	cardStraddle := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicMain, base.Add(4*time.Second), []messages.SecretRequest{
		{Key: "STRADDLE_1", KeyNorm: "STRADDLE_1", SecretType: "environment"},
		{Key: "STRADDLE_2", KeyNorm: "STRADDLE_2", SecretType: "environment"},
		{Key: "STRADDLE_3", KeyNorm: "STRADDLE_3", SecretType: "environment"},
	}, false)
	_, err = st.DismissSecretRequests(ctx, &store.DismissSecretRequestsInput{
		MessageID: cardStraddle.Message.ID,
		UserID:    f.alice.ID,
		UserName:  f.alice.DisplayName,
		Reason:    "straddle",
		Now:       base.Add(40 * time.Second),
	})
	require.NoError(t, err)

	expectedIDs := []string{
		cardStraddle.Requests[0].Row.ID, cardStraddle.Requests[1].Row.ID, cardStraddle.Requests[2].Row.ID,
		cardM.Requests[0].Row.ID, cardM.Requests[1].Row.ID,
		cardN.Requests[0].Row.ID,
		cardX.Requests[0].Row.ID,
		cardY.Requests[0].Row.ID,
	}

	// Unpaged baseline.
	unpagedIDs, _ := walkAllPages("tab=resolved&limit=50")
	assert.ElementsMatch(t, expectedIDs, unpagedIDs)

	// Walk every page with limit=1 under sort=recent and small scan batch size (2) to force batch straddling.
	srv.actionInboxResolvedBatchForTest = 2
	defer func() { srv.actionInboxResolvedBatchForTest = 0 }()

	recentIDs, recentPages := walkAllPages("tab=resolved&sort=recent&limit=1")
	assert.ElementsMatch(t, expectedIDs, recentIDs, "sort=recent pagination must visit every row with no skips or duplicates")
	// Page 1 must contain all 3 rows of cardStraddle despite batchSize=2.
	require.NotEmpty(t, recentPages)
	require.Len(t, recentPages[0].Cards, 1)
	assert.Equal(t, cardStraddle.Message.ID, recentPages[0].Cards[0].MessageID)
	assert.Len(t, recentPages[0].Cards[0].Rows, 3)

	// Walk every page with limit=1 under sort=project: neither X nor Y is skipped.
	projIDs, _ := walkAllPages("tab=resolved&sort=project&limit=1")
	assert.ElementsMatch(t, expectedIDs, projIDs, "sort=project pagination must visit every row with no skips or duplicates")

	// Within-page sort=project orders "Alpha Probe" (Y) before "Secret Project" when both land on the same page.
	_, projWidePages := walkAllPages("tab=resolved&sort=project&limit=50")
	require.Len(t, projWidePages, 1)
	require.GreaterOrEqual(t, len(projWidePages[0].Cards), 2)
	assert.Equal(t, projAlpha.ID, projWidePages[0].Cards[0].ProjectID)
}

func TestUserMeSecretRequests_AnswerableGuards(t *testing.T) {
	ctx := context.Background()
	f := newSecretRequestsFixture(t, false)
	st := f.store
	srv := f.srv
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   true,
	})

	// 1. User with agent.attach + agent.read + project.read, but WITHOUT project.chat_post:
	//    readable == true, CanRespond (agent.attach) == true, answering-recipient == false.
	//    Must give counts.open == 1, counts.answerable == 0, and inbox summary count == 0.
	noPostRole := &store.RoleDefinition{
		ID:          api.NewUUID(),
		Name:        "attach-without-chat-post-" + api.NewUUID()[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.read", "agent.attach"},
	}
	_, err := st.CreateRoleDefinition(ctx, noPostRole)
	require.NoError(t, err)

	dave := &store.User{
		ID:          api.NewUUID(),
		Email:       "dave@example.com",
		DisplayName: "Dave",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, dave))
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: noPostRole.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      dave.ID,
	})
	require.NoError(t, err)

	topicA := createUserProjectTopic(t, srv, f.project.ID, "guards-topic", f.alice.ID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	card := seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, now, []messages.SecretRequest{
		{Key: "GUARD_KEY", KeyNorm: "GUARD_KEY", SecretType: "environment"},
	}, false)

	daveTok := f.sessionToken(t, dave)
	rrDave := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, daveTok)
	require.Equal(t, http.StatusOK, rrDave.Code, rrDave.Body.String())
	var daveResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrDave.Body.Bytes(), &daveResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 1}, daveResp.Counts)
	require.Len(t, daveResp.Cards, 1)
	assert.False(t, daveResp.Cards[0].CanRespond)

	rrDaveSum := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", daveTok)
	require.Equal(t, http.StatusOK, rrDaveSum.Code)
	var daveSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrDaveSum.Body.Bytes(), &daveSum))
	assert.Equal(t, 0, daveSum.Categories[InboxCategoryAgentSecrets].Count)

	// 2. Claimed (fulfilling) row: open == 1, answerable == 0 in both list and summary.
	row := card.Requests[0].Row
	claim, err := st.ClaimSecretRequestsForFulfill(ctx, &store.ClaimSecretRequestsInput{
		MessageID: row.MessageID,
		RowIDs:    []string{row.ID},
		UserID:    f.alice.ID,
		UserName:  f.alice.DisplayName,
		Now:       now.Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, claim.Claimed, 1)

	aliceTok := f.sessionToken(t, f.alice)
	rrAlice := doActionInboxBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, aliceTok)
	require.Equal(t, http.StatusOK, rrAlice.Code)
	var aliceResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAlice.Body.Bytes(), &aliceResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 1}, aliceResp.Counts)

	rrAliceSum := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceSum.Code)
	var aliceSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrAliceSum.Body.Bytes(), &aliceSum))
	assert.Equal(t, 0, aliceSum.Categories[InboxCategoryAgentSecrets].Count)
}

func TestInboxSummary_SecretsProjectsUnion(t *testing.T) {
	ctx := context.Background()
	f := newSecretRequestsFixture(t, false)
	st := f.store
	srv := f.srv
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard: true,
		experiments.AgentSecrets:   true,
	})

	ownerRD, err := st.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)

	// Project A (f.project): overlapping — has BOTH an answerable agent_secrets row AND a pending_secrets row.
	// Project B: disjoint — has ONLY an answerable agent_secrets row.
	// Project C: disjoint — has ONLY a pending_secrets row.
	projB := &store.Project{ID: api.NewUUID(), Slug: "union-proj-b-" + api.NewUUID()[:8], Name: "Union Proj B"}
	require.NoError(t, st.CreateProject(ctx, projB))
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projB.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.alice.ID,
	})
	require.NoError(t, err)
	agentB := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "union-agent-b",
		Name:      "union-agent-b",
		ProjectID: projB.ID,
		OwnerID:   f.alice.ID,
		Phase:     "running",
	}
	require.NoError(t, st.CreateAgent(ctx, agentB))

	projC := &store.Project{ID: api.NewUUID(), Slug: "union-proj-c-" + api.NewUUID()[:8], Name: "Union Proj C"}
	require.NoError(t, st.CreateProject(ctx, projC))

	topicA := createUserProjectTopic(t, srv, f.project.ID, "union-topic-a", f.alice.ID)
	topicB := createUserProjectTopic(t, srv, projB.ID, "union-topic-b", f.alice.ID)

	now := time.Now().UTC().Truncate(time.Microsecond)
	_ = seedThreadSecretRequest(t, st, f.project.ID, f.agent.ID, f.agent.Name, topicA, now, []messages.SecretRequest{
		{Key: "UNION_SR_A", KeyNorm: "UNION_SR_A", SecretType: "environment"},
	}, false)
	_ = seedThreadSecretRequest(t, st, projB.ID, agentB.ID, agentB.Name, topicB, now.Add(time.Second), []messages.SecretRequest{
		{Key: "UNION_SR_B", KeyNorm: "UNION_SR_B", SecretType: "environment"},
	}, false)

	_, err = st.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		ID:          api.NewUUID(),
		Key:         "UNION_PS_A",
		SecretType:  store.SecretTypeEnvironment,
		Scope:       store.ScopeUser,
		ScopeID:     f.alice.ID,
		ValueRef:    "ref-a",
		ValueLen:    8,
		AgentID:     f.agent.ID,
		AgentSlug:   f.agent.Slug,
		ProjectID:   f.project.ID,
		ProjectName: f.project.Name,
		Created:     now,
		Updated:     now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}, now)
	require.NoError(t, err)
	_, err = st.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		ID:          api.NewUUID(),
		Key:         "UNION_PS_C",
		SecretType:  store.SecretTypeEnvironment,
		Scope:       store.ScopeUser,
		ScopeID:     f.alice.ID,
		ValueRef:    "ref-c",
		ValueLen:    8,
		AgentID:     agentB.ID,
		AgentSlug:   agentB.Slug,
		ProjectID:   projC.ID,
		ProjectName: projC.Name,
		Created:     now,
		Updated:     now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}, now)
	require.NoError(t, err)

	aliceTok := f.sessionToken(t, f.alice)
	rr := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rr.Code)
	var sum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &sum))

	asCat := sum.Categories[InboxCategoryAgentSecrets]
	psCat := sum.Categories[InboxCategoryPendingSecrets]
	assert.Equal(t, InboxStateOK, asCat.State)
	assert.Equal(t, InboxStateOK, psCat.State)
	assert.Equal(t, 2, asCat.Count)
	assert.Equal(t, 2, psCat.Count)
	assert.Equal(t, 2, asCat.Breakdown[InboxBreakdownProjects], "agent_secrets own projects = {A, B}")
	assert.Equal(t, 2, psCat.Breakdown[InboxBreakdownProjects], "pending_secrets own projects = {A, C}")
	assert.Equal(t, 3, asCat.Breakdown[InboxBreakdownSecretsProjects], "distinct union across both = {A, B, C}")
	// secretsProjects does not inflate total.
	assert.Equal(t, 4, sum.Total)

	// When pending_secrets is not ok (error), union falls back to agent_secrets' own projects (2).
	srv.inboxSourcesForTest = []inboxSourceEntry{
		{
			key: InboxCategoryPendingSecrets,
			run: func(_ context.Context, _ inboxViewer, _ int, _ string) InboxCategory {
				return InboxCategory{State: InboxStateError, ErrorCode: InboxErrorInternal, Previews: []any{}}
			},
		},
		{key: InboxCategoryAgentSecrets, run: srv.inboxAgentSecretsCategory},
	}
	defer func() { srv.inboxSourcesForTest = nil }()

	rrErr := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rrErr.Code)
	var sumErr InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrErr.Body.Bytes(), &sumErr))
	assert.Equal(t, 2, sumErr.Categories[InboxCategoryAgentSecrets].Breakdown[InboxBreakdownSecretsProjects])

	// When agent_secrets is unavailable (UAT), secretsProjects is omitted.
	srv.inboxSourcesForTest = nil
	uatRaw, _, err := srv.uatService.CreateToken(rs4MintContext(f.alice.ID), f.alice.ID, "union-uat", f.project.ID, []string{"inbox:read"}, nil)
	require.NoError(t, err)
	rrUAT := doActionInboxBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", uatRaw)
	require.Equal(t, http.StatusOK, rrUAT.Code)
	var sumUAT InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrUAT.Body.Bytes(), &sumUAT))
	assert.Equal(t, InboxStateUnavailable, sumUAT.Categories[InboxCategoryAgentSecrets].State)
	_, hasKey := sumUAT.Categories[InboxCategoryAgentSecrets].Breakdown[InboxBreakdownSecretsProjects]
	assert.False(t, hasKey)
}

func TestActionInboxGenericity_NoSecretRequestBranches(t *testing.T) {
	data, err := os.ReadFile("action_inbox.go")
	require.NoError(t, err)
	src := string(data)
	for _, forbidden := range []string{"secret_request", "SecretRequest", "agent_secrets", "AgentSecrets"} {
		assert.False(t, strings.Contains(src, forbidden),
			"AC-12: pkg/hub/action_inbox.go must remain kind-neutral and not reference %q", forbidden)
	}
}
