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

func doBearerRequest(t *testing.T, srv *Server, method, path, bearer string) *httptest.ResponseRecorder {
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
	rrPostOff := doBearerRequest(t, srv, http.MethodPost, userMeSecretRequestsPath, aliceTok)
	assert.Equal(t, http.StatusMethodNotAllowed, rrPostOff.Code)
	rrGetOff := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, aliceTok)
	assert.Equal(t, http.StatusNotFound, rrGetOff.Code)

	// Enable hub.agent_secrets.
	enableHubExperiments(t, srv, st, map[string]bool{
		experiments.AgentSecrets:   true,
		experiments.InboxDashboard: true,
	})

	// 2. Caller authentication & credential matrix:
	// Unauthenticated -> 401.
	rrUnauth := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, "")
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
	rrUAT := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, uatRaw)
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
	rrAlice := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, aliceTok)
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
	rrAliceProbe := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?q=CAROL_DM_KEY", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceProbe.Code)
	var aliceProbe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceProbe.Body.Bytes(), &aliceProbe))
	assert.Empty(t, aliceProbe.Cards)
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 0}, *aliceProbe.FilteredCounts)
	assert.Equal(t, ActionInboxCounts{Answerable: 3, Open: 3}, aliceProbe.Counts)

	// Alice filters by q matching description (case-insensitive) and project facet.
	rrAliceQ := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?q=database+password", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceQ.Code)
	var aliceQ secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceQ.Body.Bytes(), &aliceQ))
	require.Len(t, aliceQ.Cards, 1)
	assert.Equal(t, card1.Message.ID, aliceQ.Cards[0].MessageID)
	assert.Equal(t, ActionInboxCounts{Answerable: 1, Open: 1}, *aliceQ.FilteredCounts)

	// sort=project puts "Alpha Project C" (Card 3) before "Secret Project" (Card 1).
	rrAliceSortProj := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?sort=project", aliceTok)
	require.Equal(t, http.StatusOK, rrAliceSortProj.Code)
	var aliceSortProj secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrAliceSortProj.Body.Bytes(), &aliceSortProj))
	require.Len(t, aliceSortProj.Cards, 2)
	assert.Equal(t, projC.ID, aliceSortProj.Cards[0].ProjectID)
	assert.Equal(t, f.project.ID, aliceSortProj.Cards[1].ProjectID)

	// Open tab pagination: limit=1 returns Card 3 + nextCursor; second page returns Card 1.
	rrPage1 := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1", aliceTok)
	require.Equal(t, http.StatusOK, rrPage1.Code)
	var page1 secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrPage1.Body.Bytes(), &page1))
	require.Len(t, page1.Cards, 1)
	assert.Equal(t, card3DMAlice.Message.ID, page1.Cards[0].MessageID)
	require.NotEmpty(t, page1.NextCursor)

	rrPage2 := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1&cursor="+page1.NextCursor, aliceTok)
	require.Equal(t, http.StatusOK, rrPage2.Code)
	var page2 secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrPage2.Body.Bytes(), &page2))
	require.Len(t, page2.Cards, 1)
	assert.Equal(t, card1.Message.ID, page2.Cards[0].MessageID)
	assert.Empty(t, page2.NextCursor)

	// Cursor bound to different query -> 400.
	rrBadCur := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=1&sort=project&cursor="+page1.NextCursor, aliceTok)
	assert.Equal(t, http.StatusBadRequest, rrBadCur.Code)

	// Carol (project-viewer on Project A: agent.read, NO agent.attach):
	// Sees Card 4 (DM to Carol, 1 row) + Card 1 (Project A thread, 2 rows) = 3 open, 0 answerable!
	carolTok := f.sessionToken(t, f.carol)
	rrCarol := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, carolTok)
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
	rrBob := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath, bobTok)
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
	rrOut := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?project="+f.project.ID, outsiderTok)
	require.Equal(t, http.StatusOK, rrOut.Code)
	var outResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrOut.Body.Bytes(), &outResp))
	assert.Equal(t, ActionInboxCounts{Answerable: 0, Open: 0}, outResp.Counts)
	assert.Empty(t, outResp.Cards)

	// 4. Resolved tab (AC-6): terminal non-replaced rows newest resolved_at first.
	// Dismiss Card 1 at base+10s, withdraw Card 3 at base+20s, and replace a key (must be excluded).
	_, err = st.DismissSecretRequests(ctx, &store.DismissSecretRequestsInput{
		MessageID: card1.Message.ID,
		UserID:    f.alice.ID,
		UserName:  f.alice.DisplayName,
		Reason:    "not needed",
		Now:       base.Add(10 * time.Second),
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

	rrRes := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?tab=resolved", aliceTok)
	require.Equal(t, http.StatusOK, rrRes.Code)
	var resResp secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrRes.Body.Bytes(), &resResp))
	// Only Card 3 (withdrawn at t+20s) and Card 1 (dismissed at t+10s); replaced v1 excluded.
	require.Len(t, resResp.Cards, 2)
	assert.Equal(t, card3DMAlice.Message.ID, resResp.Cards[0].MessageID)
	assert.Equal(t, card1.Message.ID, resResp.Cards[1].MessageID)
	assert.Equal(t, ActionInboxCounts{Answerable: 1, Open: 1}, resResp.Counts, "open counts reflect remaining REPL_KEY v2")

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
	rrTrunc := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
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
	rrCapped := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
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
	rrOff := doBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
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
	rrSum := doBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", aliceTok)
	require.Equal(t, http.StatusOK, rrSum.Code)
	var sumOn InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrSum.Body.Bytes(), &sumOn))

	rrProbe := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", aliceTok)
	require.Equal(t, http.StatusOK, rrProbe.Code)
	var probe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrProbe.Body.Bytes(), &probe))

	asCat := sumOn.Categories[InboxCategoryAgentSecrets]
	assert.Equal(t, InboxStateOK, asCat.State)
	assert.True(t, asCat.CountsTowardTotal)
	assert.Equal(t, 3, asCat.Count)
	assert.Equal(t, probe.Counts.Answerable, asCat.Count, "AC-7 count parity")
	assert.Equal(t, map[string]int{
		InboxBreakdownAgents:   1,
		InboxBreakdownProjects: 1,
	}, asCat.Breakdown)
	require.NotNil(t, asCat.NextExpiresAt)
	assert.Equal(t, firstRes.Requests[0].Row.ExpiresAt.UTC(), asCat.NextExpiresAt.UTC())
	assert.Equal(t, 0, sumOn.Categories[InboxCategoryDecisions].Count, "decisions count unchanged")

	// Carol (no ActionAttach): summary count == 0 == probe counts.answerable (while probe counts.open == 2).
	carolTok := f.sessionToken(t, f.carol)
	rrCarolSum := doBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", carolTok)
	require.Equal(t, http.StatusOK, rrCarolSum.Code)
	var carolSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(rrCarolSum.Body.Bytes(), &carolSum))

	rrCarolProbe := doBearerRequest(t, srv, http.MethodGet, userMeSecretRequestsPath+"?limit=0", carolTok)
	require.Equal(t, http.StatusOK, rrCarolProbe.Code)
	var carolProbe secretRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rrCarolProbe.Body.Bytes(), &carolProbe))

	assert.Equal(t, 0, carolSum.Categories[InboxCategoryAgentSecrets].Count)
	assert.Equal(t, 0, carolProbe.Counts.Answerable)
	assert.Equal(t, 2, carolProbe.Counts.Open)

	// UAT caller: agent_secrets is unavailable (§4.4, AC-6).
	uatRaw, _, err := srv.uatService.CreateToken(rs4MintContext(f.alice.ID), f.alice.ID, "inbox-uat", f.project.ID, []string{"inbox:read"}, nil)
	require.NoError(t, err)
	rrUAT := doBearerRequest(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", uatRaw)
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
