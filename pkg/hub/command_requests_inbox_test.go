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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const userMeCommandRequestsPath = "/api/v1/users/me/command-requests"

type commandRequestsInboxResponse struct {
	Counts            ActionInboxCounts                       `json:"counts"`
	FilteredCounts    *ActionInboxCounts                      `json:"filteredCounts,omitempty"`
	Cards             []messages.CommandRequestDetailResponse `json:"cards"`
	Facets            ActionInboxFacets                       `json:"facets"`
	TruncatedProjects bool                                    `json:"truncatedProjects"`
	Capped            bool                                    `json:"capped"`
	NextCursor        string                                  `json:"nextCursor,omitempty"`
}

func crInboxSetExperiments(t *testing.T, srv *Server, st store.Store, overrides map[string]bool) {
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

func crInboxAttachWebChat(t *testing.T, srv *Server) {
	t.Helper()
	if srv.webChatStore != nil {
		return
	}
	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
}

func crInboxCreateTopic(t *testing.T, srv *Server, projID, name, createdBy string) string {
	t.Helper()
	crInboxAttachWebChat(t, srv)
	topicID := api.NewUUID()
	require.NoError(t, srv.webChatStore.CreateTopic(context.Background(), WebChatTopic{
		ID:        topicID,
		ProjectID: projID,
		Name:      name,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	}))
	return topicID
}

func crInboxDeleteMessage(t *testing.T, st store.Store, msgID string) {
	t.Helper()
	dbProvider, ok := st.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	res, err := dbProvider.DB().Exec(`DELETE FROM messages WHERE id = ?`, msgID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected to delete 1 message row")
}

func crInboxGrantProjectOwner(t *testing.T, st store.Store, userID, projID string) {
	t.Helper()
	ctx := context.Background()
	grantProjectAccessOnly(t, st, userID, projID)
	rd, err := st.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projID,
	})
	require.NoError(t, err)
}

func crInboxSessionToken(t *testing.T, srv *Server, u *store.User) string {
	t.Helper()
	tok, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

func crInboxBearerReq(t *testing.T, srv *Server, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func crInboxSeedDM(
	t *testing.T,
	st store.Store,
	projID, agentID, agentName, approverID, key, script string,
	update bool,
	now time.Time,
) *store.CommandRequestPromptResult {
	t.Helper()
	ctx := context.Background()
	dmKey, err := messages.DMConversationKey("agent", agentID, "user", approverID)
	require.NoError(t, err)
	req, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:         key,
		Description: "Run " + key,
		Script:      script,
	})
	require.NoError(t, err)
	res, err := st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:          api.NewUUID(),
			ProjectID:   projID,
			Sender:      "agent:" + agentName,
			SenderID:    agentID,
			Recipient:   "user:" + approverID,
			RecipientID: approverID,
			ThreadID:    dmKey,
			Msg:         "placeholder",
			Type:        messages.TypeCommandRequest,
		},
		Intro:        "Please run " + key,
		ProjectName:  "Project",
		ApproverHint: approverID,
		AgentID:      agentID,
		AgentName:    agentName,
		ApproverID:   approverID,
		Request:      req,
		Update:       update,
		Now:          now,
	})
	require.NoError(t, err)
	return res
}

func crInboxSeedThread(
	t *testing.T,
	st store.Store,
	projID, agentID, agentName, approverID, chatKey, key, script string,
	update bool,
	now time.Time,
) *store.CommandRequestPromptResult {
	t.Helper()
	ctx := context.Background()
	req, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:         key,
		Description: "Thread run " + key,
		Script:      script,
	})
	require.NoError(t, err)
	res, err := st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:          api.NewUUID(),
			ProjectID:   projID,
			Sender:      "agent:" + agentName,
			SenderID:    agentID,
			Recipient:   chatKey,
			RecipientID: chatKey,
			ThreadID:    chatKey,
			Msg:         "placeholder",
			Type:        messages.TypeCommandRequest,
		},
		Intro:        "Thread prompt for " + key,
		ProjectName:  "Project",
		ApproverHint: approverID,
		AgentID:      agentID,
		AgentName:    agentName,
		ApproverID:   approverID,
		Request:      req,
		Update:       update,
		Now:          now,
	})
	require.NoError(t, err)
	return res
}

// TestUserMeCommandRequests_AuthorizationAndScope covers AC-2, AC-3, and AC-7.
func TestUserMeCommandRequests_AuthorizationAndScope(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleMember, Status: "active"}
	bob := &store.User{ID: api.NewUUID(), Email: "bob@example.com", DisplayName: "Bob", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	require.NoError(t, st.CreateUser(ctx, bob))

	projA := &store.Project{ID: api.NewUUID(), Slug: "cr-a-" + api.NewUUID()[:8], Name: "Alpha"}
	projB := &store.Project{ID: api.NewUUID(), Slug: "cr-b-" + api.NewUUID()[:8], Name: "Beta"}
	projC := &store.Project{ID: api.NewUUID(), Slug: "cr-c-" + api.NewUUID()[:8], Name: "Gamma"}
	for _, p := range []*store.Project{projA, projB, projC} {
		require.NoError(t, st.CreateProject(ctx, p))
		crInboxGrantProjectOwner(t, st, alice.ID, p.ID)
	}
	crInboxGrantProjectOwner(t, st, bob.ID, projA.ID)

	agentA := &store.Agent{ID: api.NewUUID(), Slug: "agent-a", Name: "Agent A", ProjectID: projA.ID, Phase: string(state.PhaseRunning)}
	agentB := &store.Agent{ID: api.NewUUID(), Slug: "agent-b", Name: "Agent B", ProjectID: projB.ID, Phase: string(state.PhaseRunning)}
	agentC := &store.Agent{ID: api.NewUUID(), Slug: "agent-c", Name: "Agent C", ProjectID: projC.ID, Phase: string(state.PhaseRunning)}
	for _, a := range []*store.Agent{agentA, agentB, agentC} {
		require.NoError(t, st.CreateAgent(ctx, a))
	}

	aliceTok := crInboxSessionToken(t, srv, alice)
	bobTok := crInboxSessionToken(t, srv, bob)

	// 1. Gate off: GET -> 404, non-GET -> 405 before gate (AC-7).
	rr := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, aliceTok, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = crInboxBearerReq(t, srv, http.MethodPost, userMeCommandRequestsPath, aliceTok, "")
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// Enable hub.command_requests.
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.CommandRequests: true,
		experiments.InboxDashboard:  true,
	})

	// Non-GET still gets 405 with gate on.
	rr = crInboxBearerReq(t, srv, http.MethodPost, userMeCommandRequestsPath, aliceTok, "")
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// 2. Non-session callers get 403 (AC-7): UAT, agent token, visitor, federated.
	uatPlain, _, err := srv.uatService.CreateToken(rs4MintContext(alice.ID), alice.ID, "cr-uat", projA.ID, []string{"inbox:read", "project:read"}, nil)
	require.NoError(t, err)
	rr = crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, uatPlain, "")
	assert.Equal(t, http.StatusForbidden, rr.Code, "UAT must be refused with 403")

	agentJWT, err := srv.GenerateAgentToken(agentA.ID, projA.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	rr = crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, agentJWT, "")
	assert.Equal(t, http.StatusForbidden, rr.Code, "agent token must be refused with 403")

	visitorIdent := NewVisitorIdentity(api.NewUUID(), "https://issuer.cmdreq.test", "cmdreq-visitor", "visitor@cmdreq.test", "Visitor")
	visReq := httptest.NewRequest(http.MethodGet, userMeCommandRequestsPath, nil)
	visReq = visReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(visReq.Context(), visitorIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
	))
	visRR := httptest.NewRecorder()
	srv.handleUserMeActionInbox(&commandRequestActionInboxKind{srv: srv})(visRR, visReq)
	assert.Equal(t, http.StatusForbidden, visRR.Code, "visitor must be refused with 403")

	fedIdent := NewFederatedUserIdentity("https://issuer.cmdreq.test", "cmdreq-fed", alice.Email, "Fed Alice", store.UserRoleMember, nil)
	fedReq := httptest.NewRequest(http.MethodGet, userMeCommandRequestsPath, nil)
	fedReq = fedReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(fedReq.Context(), fedIdent),
		CredentialContext{Kind: CredentialKindFederation, ID: fedIdent.ID()},
	))
	fedRR := httptest.NewRecorder()
	srv.handleUserMeActionInbox(&commandRequestActionInboxKind{srv: srv})(fedRR, fedReq)
	assert.Equal(t, http.StatusForbidden, fedRR.Code, "federated caller must be refused with 403")

	// 3. AC-2 Scope:
	// - Alice has requests in projA (thread shared with Bob), projB, and projC.
	// - Set actionInboxMaxProjectsForTest = 1 to prove viewer scope is unaffected by the 200-project cap.
	srv.actionInboxMaxProjectsForTest = 1
	defer func() { srv.actionInboxMaxProjectsForTest = 0 }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	topicA := crInboxCreateTopic(t, srv, projA.ID, "shared-topic", alice.ID)
	reqThreadA := crInboxSeedThread(t, st, projA.ID, agentA.ID, agentA.Name, alice.ID, topicA, "thread-a", "echo a", false, now)
	reqDMB := crInboxSeedDM(t, st, projB.ID, agentB.ID, agentB.Name, alice.ID, "dm-b", "echo b", false, now.Add(time.Second))
	reqDMC := crInboxSeedDM(t, st, projC.ID, agentC.ID, agentC.Name, alice.ID, "dm-c", "echo c", false, now.Add(2*time.Second))

	// Bob shares projA & topicA, but is NOT the approver -> sees 0 cards.
	rr = crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, bobTok, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var bobResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &bobResp))
	assert.Equal(t, 0, bobResp.Counts.Open)
	assert.Equal(t, 0, bobResp.Counts.Answerable)
	assert.Empty(t, bobResp.Cards)

	// Alice sees all 3 cards across 3 projects even with maxProjects=1, and truncatedProjects=false, capped=false.
	rr = crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, aliceTok, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var aliceResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &aliceResp))
	assert.Equal(t, 3, aliceResp.Counts.Open)
	assert.Equal(t, 3, aliceResp.Counts.Answerable)
	assert.False(t, aliceResp.TruncatedProjects, "viewer scope must not truncate projects")
	assert.False(t, aliceResp.Capped)
	assert.Len(t, aliceResp.Cards, 3)

	// 4. AC-3 Read parity:
	// (a) Approver WITHOUT agent.read on the asking agent (DM in a project where Alice has no project membership/role):
	//     both GET /api/v1/command-requests/{id} and /api/v1/users/me/command-requests list it.
	projNoRead := &store.Project{ID: api.NewUUID(), Slug: "cr-noread-" + api.NewUUID()[:8], Name: "NoRead"}
	require.NoError(t, st.CreateProject(ctx, projNoRead))
	agentNoRead := &store.Agent{ID: api.NewUUID(), Slug: "agent-noread", Name: "Agent NoRead", ProjectID: projNoRead.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, agentNoRead))
	reqNoRead := crInboxSeedDM(t, st, projNoRead.ID, agentNoRead.ID, agentNoRead.Name, alice.ID, "dm-noread", "echo noread", false, now.Add(3*time.Second))

	getRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/command-requests/"+reqNoRead.Request.ID, aliceTok, "")
	require.Equal(t, http.StatusOK, getRR.Code, "approver without agent.read must be able to GET DM request")

	// (b) Approver who has left the thread (topic in a project Alice is NOT a member of):
	//     GET returns 403 and list does NOT include it.
	topicLeft := crInboxCreateTopic(t, srv, projNoRead.ID, "left-topic", bob.ID)
	reqLeft := crInboxSeedThread(t, st, projNoRead.ID, agentNoRead.ID, agentNoRead.Name, alice.ID, topicLeft, "thread-left", "echo left", false, now.Add(4*time.Second))

	getLeftRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/command-requests/"+reqLeft.Request.ID, aliceTok, "")
	assert.Equal(t, http.StatusForbidden, getLeftRR.Code, "approver who left thread must get 403 on GET")

	// (c) Row with empty chat_key whose prompt message was deleted -> approver fallback:
	//     GET returns 200 and list includes it with promptAvailable=false.
	reqFallbackIn, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:    "fallback-key",
		Script: "echo fallback",
	})
	require.NoError(t, err)
	fallbackMsgID := api.NewUUID()
	reqFallback, err := st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:          fallbackMsgID,
			ProjectID:   projA.ID,
			Sender:      "agent:" + agentA.Name,
			SenderID:    agentA.ID,
			Recipient:   "agent:" + agentA.Name, // non-user: prefix so chat_key stays empty
			RecipientID: "",
			ThreadID:    "",
			Msg:         "placeholder",
			Type:        messages.TypeCommandRequest,
		},
		AgentID:    agentA.ID,
		AgentName:  agentA.Name,
		ApproverID: alice.ID,
		Request:    reqFallbackIn,
		Now:        now.Add(5 * time.Second),
	})
	require.NoError(t, err)
	require.Empty(t, reqFallback.Request.ChatKey)
	// Delete the prompt message so promptMsg == nil.
	crInboxDeleteMessage(t, st, fallbackMsgID)

	getFallbackRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/command-requests/"+reqFallback.Request.ID, aliceTok, "")
	require.Equal(t, http.StatusOK, getFallbackRR.Code, "approver fallback must allow GET when chat_key is empty and prompt deleted")
	var fbDetail messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(getFallbackRR.Body.Bytes(), &fbDetail))
	assert.False(t, fbDetail.PromptAvailable)

	// Now check the list endpoint: must contain {reqThreadA, reqDMB, reqDMC, reqNoRead, reqFallback} = 5 rows, NOT reqLeft.
	rr = crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath, aliceTok, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var parityResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &parityResp))
	assert.Equal(t, 5, parityResp.Counts.Open)
	assert.Equal(t, 5, parityResp.Counts.Answerable)

	listedIDs := make(map[string]messages.CommandRequestDetailResponse, len(parityResp.Cards))
	for _, c := range parityResp.Cards {
		listedIDs[c.Request.RowID] = c
	}
	assert.Contains(t, listedIDs, reqThreadA.Request.ID)
	assert.Contains(t, listedIDs, reqDMB.Request.ID)
	assert.Contains(t, listedIDs, reqDMC.Request.ID)
	assert.Contains(t, listedIDs, reqNoRead.Request.ID)
	assert.Contains(t, listedIDs, reqFallback.Request.ID)
	assert.NotContains(t, listedIDs, reqLeft.Request.ID)
	assert.False(t, listedIDs[reqFallback.Request.ID].PromptAvailable)
	assert.True(t, listedIDs[reqThreadA.Request.ID].PromptAvailable)
}

// TestUserMeCommandRequests_CountParityAndSummary covers AC-1 and AC-7 (summary).
func TestUserMeCommandRequests_CountParityAndSummary(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	aliceTok := crInboxSessionToken(t, srv, alice)

	// 1. Inbox on, hub.command_requests off -> agent_executions is disabled.
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard:  true,
		experiments.CommandRequests: false,
	})
	sumOffRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary", aliceTok, "")
	require.Equal(t, http.StatusOK, sumOffRR.Code, sumOffRR.Body.String())
	var sumOff InboxSummaryResponse
	require.NoError(t, json.Unmarshal(sumOffRR.Body.Bytes(), &sumOff))
	aeOff, ok := sumOff.Categories[InboxCategoryAgentExecutions]
	require.True(t, ok, "agent_executions key must exist in summary")
	assert.Equal(t, InboxStateDisabled, aeOff.State)
	assert.False(t, aeOff.CountsTowardTotal)
	assert.Equal(t, agentExecutionsInboxHref, aeOff.Href)

	// 2. Enable hub.command_requests. Seed across 3 projects (DM + thread) + expired + withdrawn + replaced + markdown_block.
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.InboxDashboard:  true,
		experiments.CommandRequests: true,
	})
	var projs []*store.Project
	var agents []*store.Agent
	for i := 0; i < 3; i++ {
		p := &store.Project{ID: api.NewUUID(), Slug: fmt.Sprintf("p-%d-%s", i, api.NewUUID()[:6]), Name: fmt.Sprintf("Project %d", i)}
		require.NoError(t, st.CreateProject(ctx, p))
		crInboxGrantProjectOwner(t, st, alice.ID, p.ID)
		a := &store.Agent{ID: api.NewUUID(), Slug: fmt.Sprintf("a-%d", i), Name: fmt.Sprintf("Agent %d", i), ProjectID: p.ID, Phase: string(state.PhaseRunning)}
		require.NoError(t, st.CreateAgent(ctx, a))
		projs = append(projs, p)
		agents = append(agents, a)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	topic0 := crInboxCreateTopic(t, srv, projs[0].ID, "exec-topic", alice.ID)
	// 3 active pending rows across 3 projects (1 thread + 2 DMs):
	r0 := crInboxSeedThread(t, st, projs[0].ID, agents[0].ID, agents[0].Name, alice.ID, topic0, "k0", "echo 0", false, now)
	_ = crInboxSeedDM(t, st, projs[1].ID, agents[1].ID, agents[1].Name, alice.ID, "k1", "echo 1", false, now.Add(time.Second))
	// Replace v1 -> v2 on proj 2 (only v2 counts):
	_ = crInboxSeedDM(t, st, projs[2].ID, agents[2].ID, agents[2].Name, alice.ID, "k2", "echo 2v1", false, now.Add(2*time.Second))
	_ = crInboxSeedDM(t, st, projs[2].ID, agents[2].ID, agents[2].Name, alice.ID, "k2", "echo 2v2", true, now.Add(3*time.Second))

	// Expired pending row (must not count):
	expReq, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{Key: "k-exp", Script: "echo exp"})
	require.NoError(t, err)
	dm0, err := messages.DMConversationKey("agent", agents[0].ID, "user", alice.ID)
	require.NoError(t, err)
	_, err = st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID: api.NewUUID(), ProjectID: projs[0].ID, Sender: "agent:" + agents[0].Name, SenderID: agents[0].ID,
			Recipient: "user:" + alice.ID, RecipientID: alice.ID, ThreadID: dm0, Msg: "x", Type: messages.TypeCommandRequest,
		},
		AgentID: agents[0].ID, AgentName: agents[0].Name, ApproverID: alice.ID, Request: expReq,
		Now: now.Add(-2 * time.Hour), TTL: time.Minute,
	})
	require.NoError(t, err)

	// Withdrawn row (must not count):
	_ = crInboxSeedDM(t, st, projs[0].ID, agents[0].ID, agents[0].Name, alice.ID, "k-wd", "echo wd", false, now)
	_, err = st.WithdrawCommandRequest(ctx, &store.WithdrawCommandRequestInput{AgentID: agents[0].ID, Key: "k-wd", Now: now.Add(4 * time.Second)})
	require.NoError(t, err)

	// Summary vs page ?limit=0 parity (AC-1).
	sumRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary", aliceTok, "")
	require.Equal(t, http.StatusOK, sumRR.Code, sumRR.Body.String())
	var sumOn InboxSummaryResponse
	require.NoError(t, json.Unmarshal(sumRR.Body.Bytes(), &sumOn))

	pageRR := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath+"?limit=0", aliceTok, "")
	require.Equal(t, http.StatusOK, pageRR.Code, pageRR.Body.String())
	var pageResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(pageRR.Body.Bytes(), &pageResp))

	aeCat := sumOn.Categories[InboxCategoryAgentExecutions]
	assert.Equal(t, InboxStateOK, aeCat.State)
	assert.Equal(t, 3, aeCat.Count)
	assert.Equal(t, pageResp.Counts.Answerable, aeCat.Count, "AC-1: summary count must equal page counts.answerable")
	assert.True(t, aeCat.CountsTowardTotal)
	assert.Equal(t, 3, aeCat.Breakdown[InboxBreakdownAgents])
	assert.Equal(t, 3, aeCat.Breakdown[InboxBreakdownProjects])
	require.NotNil(t, aeCat.NextExpiresAt)
	assert.True(t, aeCat.NextExpiresAt.Equal(r0.Request.ExpiresAt.UTC()))

	// UAT summary -> unavailable (AC-7).
	uatPlain, _, err := srv.uatService.CreateToken(rs4MintContext(alice.ID), alice.ID, "sum-uat", projs[0].ID, []string{"inbox:read"}, nil)
	require.NoError(t, err)
	uatSumRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/users/me/inbox/summary", uatPlain, "")
	require.Equal(t, http.StatusOK, uatSumRR.Code, uatSumRR.Body.String())
	var uatSum InboxSummaryResponse
	require.NoError(t, json.Unmarshal(uatSumRR.Body.Bytes(), &uatSum))
	uatAE := uatSum.Categories[InboxCategoryAgentExecutions]
	assert.Equal(t, InboxStateUnavailable, uatAE.State)
	assert.Equal(t, 0, uatAE.Count)
	assert.False(t, uatAE.CountsTowardTotal)
}

// TestUserMeCommandRequests_TabsPaginationAndExclusions covers AC-4.
func TestUserMeCommandRequests_TabsPaginationAndExclusions(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.CommandRequests: true,
		experiments.InboxDashboard:  true,
	})

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	proj := &store.Project{ID: api.NewUUID(), Slug: "pg-" + api.NewUUID()[:6], Name: "PageProj"}
	require.NoError(t, st.CreateProject(ctx, proj))
	grantProjectAccessOnly(t, st, alice.ID, proj.ID)
	agent := &store.Agent{ID: api.NewUUID(), Slug: "pg-agent", Name: "PageAgent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, agent))
	execAgent := &store.Agent{ID: api.NewUUID(), Slug: "exec-agent", Name: "ExecAgent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, execAgent))
	aliceTok := crInboxSessionToken(t, srv, alice)

	base := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Hour)

	// 120 resolved rows across approved (in-progress queued run), dismissed, withdrawn, and expired.
	wantResolved := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		key := fmt.Sprintf("item-%03d", i)
		createdAt := base.Add(time.Duration(i) * time.Second)
		resolvedAt := base.Add(30*time.Minute + time.Duration(i)*time.Second)

		switch i % 4 {
		case 0: // approved with queued run
			pr := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, key, "echo "+key, false, createdAt)
			appRes, err := st.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
				RequestID:     pr.Request.ID,
				CommandSHA256: pr.Request.CommandSHA256,
				UserID:        alice.ID,
				ExecAgentID:   execAgent.ID,
				ExecProjectID: proj.ID,
				Surface:       msgaction.CommandRequestSurfaceNative,
				Now:           resolvedAt,
			})
			require.NoError(t, err)
			wantResolved = append(wantResolved, appRes.Request.ID)
		case 1: // dismissed
			pr := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, key, "echo "+key, false, createdAt)
			disRes, err := st.DismissCommandRequest(ctx, &store.DismissCommandRequestInput{
				RequestID: pr.Request.ID,
				UserID:    alice.ID,
				Surface:   msgaction.CommandRequestSurfaceStandalonePage,
				Now:       resolvedAt,
			})
			require.NoError(t, err)
			wantResolved = append(wantResolved, disRes.Request.ID)
		case 2: // withdrawn
			_ = crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, key, "echo "+key, false, createdAt)
			wdRes, err := st.WithdrawCommandRequest(ctx, &store.WithdrawCommandRequestInput{
				AgentID: agent.ID,
				Key:     key,
				Now:     resolvedAt,
			})
			require.NoError(t, err)
			wantResolved = append(wantResolved, wdRes.Request.ID)
		case 3: // expired
			req, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{Key: key, Script: "echo " + key})
			require.NoError(t, err)
			dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", alice.ID)
			require.NoError(t, err)
			_, err = st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
				Message: &store.Message{
					ID: api.NewUUID(), ProjectID: proj.ID, Sender: "agent:" + agent.Name, SenderID: agent.ID,
					Recipient: "user:" + alice.ID, RecipientID: alice.ID, ThreadID: dmKey, Msg: "x", Type: messages.TypeCommandRequest,
				},
				AgentID: agent.ID, AgentName: agent.Name, ApproverID: alice.ID, Request: req,
				Now: createdAt, TTL: time.Second,
			})
			require.NoError(t, err)
			expRows, err := st.ExpireCommandRequests(ctx, resolvedAt, 10)
			require.NoError(t, err)
			require.Len(t, expRows, 1)
			wantResolved = append(wantResolved, expRows[0].ID)
		}
	}

	// Also seed a replaced row (v1 replaced by v2, where v2 is still open) and a markdown_block row.
	now := time.Now().UTC().Truncate(time.Microsecond)
	repV1 := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, "rep-open", "echo v1", false, now)
	repV2 := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, "rep-open", "echo v2", true, now.Add(time.Second))
	mdRes, err := st.CreateAndApproveMarkdownCommandRequest(ctx, &store.CreateMarkdownCommandRequestInput{
		ProjectID: proj.ID, AgentID: agent.ID, AgentName: agent.Name, MessageID: api.NewUUID(),
		Script: "echo md", UserID: alice.ID, ExecAgentID: execAgent.ID, ExecProjectID: proj.ID, Now: now.Add(2 * time.Second),
	})
	require.NoError(t, err)

	// Open tab: only repV2.
	openRR := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath+"?tab=open", aliceTok, "")
	require.Equal(t, http.StatusOK, openRR.Code, openRR.Body.String())
	var openResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(openRR.Body.Bytes(), &openResp))
	require.Len(t, openResp.Cards, 1)
	assert.Equal(t, repV2.Request.ID, openResp.Cards[0].Request.RowID)

	// Resolved tab: page through all 120 rows with limit=25; verify no duplicates, no gaps, newest-first order,
	// and neither repV1 nor mdRes appears.
	var gotIDs []string
	seen := make(map[string]bool)
	cursor := ""
	pages := 0
	for {
		path := userMeCommandRequestsPath + "?tab=resolved&limit=25"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rr := crInboxBearerReq(t, srv, http.MethodGet, path, aliceTok, "")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var page commandRequestsInboxResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
		pages++
		for _, c := range page.Cards {
			assert.NotEqual(t, repV1.Request.ID, c.Request.RowID, "replaced row must not appear on resolved tab")
			assert.NotEqual(t, mdRes.Request.ID, c.Request.RowID, "markdown_block row must not appear on resolved tab")
			assert.False(t, seen[c.Request.RowID], "duplicate row %s across resolved pages", c.Request.RowID)
			seen[c.Request.RowID] = true
			gotIDs = append(gotIDs, c.Request.RowID)
			if c.Request.Status == store.CommandRequestStatusApproved {
				require.NotNil(t, c.Run, "approved resolved card must include live run state")
				assert.Equal(t, store.CommandRunStatusQueued, c.Run.Status)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		require.Less(t, pages, 10, "pagination loop guard")
	}
	assert.Equal(t, 120, len(gotIDs))
	// wantResolved was appended oldest-resolved to newest-resolved; response must be reverse order.
	for i := 0; i < 120; i++ {
		assert.Equal(t, wantResolved[119-i], gotIDs[i], "mismatch at resolved index %d", i)
	}
}

// TestUserMeCommandRequests_CardShapeParityAndPromptAvailable covers AC-5 and AC-16 (hub half).
func TestUserMeCommandRequests_CardShapeParityAndPromptAvailable(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.CommandRequests: true,
		experiments.ExecContainers:  true,
		experiments.AgentSecrets:    true,
		experiments.InboxDashboard:  true,
	})

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleAdmin, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	proj := &store.Project{ID: api.NewUUID(), Slug: "parity-" + api.NewUUID()[:6], Name: "Parity Project"}
	require.NoError(t, st.CreateProject(ctx, proj))
	crInboxGrantProjectOwner(t, st, alice.ID, proj.ID)
	agent := &store.Agent{ID: api.NewUUID(), Slug: "parity-agent", Name: "Parity Agent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, agent))
	execAgent := &store.Agent{ID: api.NewUUID(), Slug: "parity-exec", Name: "Parity Exec", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, execAgent))
	aliceTok := crInboxSessionToken(t, srv, alice)

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Seed v1 approved + finished with unknown status, then v2 pending (so v2 has previousVersionUnknown!).
	v1 := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, "deploy-key", "echo v1", false, now)
	appV1, err := st.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID: v1.Request.ID, CommandSHA256: v1.Request.CommandSHA256, UserID: alice.ID,
		ExecAgentID: execAgent.ID, ExecProjectID: proj.ID, Surface: msgaction.CommandRequestSurfaceNative, Now: now.Add(time.Second),
	})
	require.NoError(t, err)
	won, err := st.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID: appV1.Run.ID, Status: store.CommandRunStatusUnknown, OutputStatus: "captured",
		Output: []byte("partial output"), FinishedAt: now.Add(2 * time.Second),
	})
	require.NoError(t, err)
	require.True(t, won)

	// v2 pending (open) and v3 resolved (v3 also has v2 as prior, and we also test v2 after approval).
	v2 := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, "deploy-key", "echo v2", false, now.Add(3*time.Second))

	// Compare Open card (v2) with GET /api/v1/command-requests/{v2.ID}: must match field-for-field!
	openRR := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath+"?tab=open", aliceTok, "")
	require.Equal(t, http.StatusOK, openRR.Code, openRR.Body.String())
	var openResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(openRR.Body.Bytes(), &openResp))
	require.Len(t, openResp.Cards, 1)

	getV2RR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/command-requests/"+v2.Request.ID, aliceTok, "")
	require.Equal(t, http.StatusOK, getV2RR.Code, getV2RR.Body.String())
	var singleV2 messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(getV2RR.Body.Bytes(), &singleV2))

	assert.True(t, openResp.Cards[0].PromptAvailable)
	assert.NotNil(t, openResp.Cards[0].PreviousVersionUnknown, "pending v2 card must carry previousVersionUnknown")
	assert.NotNil(t, openResp.Cards[0].ExecContainer, "pending card must carry exec_container")
	assert.Equal(t, singleV2, openResp.Cards[0], "AC-5: pending card must equal single-GET response")

	// Compare Resolved card (v1) with GET /api/v1/command-requests/{v1.ID}:
	// must match except exec_container and previousVersionUnknown are omitted on resolved list cards.
	resRR := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath+"?tab=resolved", aliceTok, "")
	require.Equal(t, http.StatusOK, resRR.Code, resRR.Body.String())
	var resResp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(resRR.Body.Bytes(), &resResp))
	require.Len(t, resResp.Cards, 1)

	getV1RR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/command-requests/"+v1.Request.ID, aliceTok, "")
	require.Equal(t, http.StatusOK, getV1RR.Code, getV1RR.Body.String())
	var singleV1 messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(getV1RR.Body.Bytes(), &singleV1))

	assert.Nil(t, resResp.Cards[0].ExecContainer, "AC-5: resolved list card omits exec_container")
	assert.Nil(t, resResp.Cards[0].PreviousVersionUnknown, "AC-5: resolved list card omits previousVersionUnknown")
	assert.True(t, resResp.Cards[0].OutputAvailable, "resolved card with captured output must set outputAvailable=true")
	assert.True(t, resResp.Cards[0].PromptAvailable)

	singleV1Normalized := singleV1
	singleV1Normalized.ExecContainer = nil
	singleV1Normalized.PreviousVersionUnknown = nil
	assert.Equal(t, singleV1Normalized, resResp.Cards[0], "AC-5: resolved card must equal single-GET minus exec_container & previousVersionUnknown")

	// AC-16 hub half: verify promptAvailable on Agent Secrets cards too (both list and single GET).
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", alice.ID)
	require.NoError(t, err)
	srMsgID := api.NewUUID()
	_, err = st.CreateSecretRequestPrompt(ctx, &store.SecretRequestPromptInput{
		Message: &store.Message{
			ID: srMsgID, ProjectID: proj.ID, Sender: "agent:" + agent.Name, SenderID: agent.ID,
			Recipient: "user:" + alice.ID, RecipientID: alice.ID, ThreadID: dmKey, Msg: "placeholder", Type: messages.TypeSecretRequest,
		},
		Intro:     "Need secret",
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Requests:  []messages.SecretRequest{{Key: "API_TOKEN", KeyNorm: "API_TOKEN", SecretType: "environment"}},
		Now:       now,
	})
	require.NoError(t, err)

	srListRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/users/me/secret-requests?tab=open", aliceTok, "")
	require.Equal(t, http.StatusOK, srListRR.Code, srListRR.Body.String())
	var srList struct {
		Cards []SecretRequestCardResponse `json:"cards"`
	}
	require.NoError(t, json.Unmarshal(srListRR.Body.Bytes(), &srList))
	require.Len(t, srList.Cards, 1)
	assert.True(t, srList.Cards[0].PromptAvailable)

	// Delete the secret request prompt message and confirm promptAvailable becomes false on both list and single GET.
	crInboxDeleteMessage(t, st, srMsgID)

	srListAfterRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/users/me/secret-requests?tab=open", aliceTok, "")
	require.Equal(t, http.StatusOK, srListAfterRR.Code, srListAfterRR.Body.String())
	var srListAfter struct {
		Cards []SecretRequestCardResponse `json:"cards"`
	}
	require.NoError(t, json.Unmarshal(srListAfterRR.Body.Bytes(), &srListAfter))
	require.Len(t, srListAfter.Cards, 1)
	assert.False(t, srListAfter.Cards[0].PromptAvailable)

	srSingleRR := crInboxBearerReq(t, srv, http.MethodGet, "/api/v1/secret-requests/"+srMsgID, aliceTok, "")
	require.Equal(t, http.StatusOK, srSingleRR.Code, srSingleRR.Body.String())
	var srSingle SecretRequestCardResponse
	require.NoError(t, json.Unmarshal(srSingleRR.Body.Bytes(), &srSingle))
	assert.False(t, srSingle.PromptAvailable)
}

type countingCommandRequestStore struct {
	store.Store
	fault *storeFaultSwitch

	getCommandRunsByRequestIDs atomic.Int64
	getCommandRunByRequestID   atomic.Int64
	getCommandRequestsByIDs    atomic.Int64
	getCommandRequest          atomic.Int64
	hasActionRunOutputs        atomic.Int64
	hasActionRunOutput         atomic.Int64
	getMessagesByIDs           atomic.Int64
	getMessage                 atomic.Int64
	listAgentCommandRequests   atomic.Int64
	getUser                    atomic.Int64
}

func (w *countingCommandRequestStore) GetCommandRunsByRequestIDs(ctx context.Context, ids []string) (map[string]*store.CommandRun, error) {
	if w.fault.Active() {
		w.getCommandRunsByRequestIDs.Add(1)
	}
	return w.Store.GetCommandRunsByRequestIDs(ctx, ids)
}

func (w *countingCommandRequestStore) GetCommandRunByRequestID(ctx context.Context, id string) (*store.CommandRun, error) {
	if w.fault.Active() {
		w.getCommandRunByRequestID.Add(1)
	}
	return w.Store.GetCommandRunByRequestID(ctx, id)
}

func (w *countingCommandRequestStore) GetCommandRequestsByIDs(ctx context.Context, ids []string) (map[string]*store.CommandRequest, error) {
	if w.fault.Active() {
		w.getCommandRequestsByIDs.Add(1)
	}
	return w.Store.GetCommandRequestsByIDs(ctx, ids)
}

func (w *countingCommandRequestStore) GetCommandRequest(ctx context.Context, id string) (*store.CommandRequest, error) {
	if w.fault.Active() {
		w.getCommandRequest.Add(1)
	}
	return w.Store.GetCommandRequest(ctx, id)
}

func (w *countingCommandRequestStore) HasActionRunOutputs(ctx context.Context, ids []string) (map[string]bool, error) {
	if w.fault.Active() {
		w.hasActionRunOutputs.Add(1)
	}
	return w.Store.HasActionRunOutputs(ctx, ids)
}

func (w *countingCommandRequestStore) HasActionRunOutput(ctx context.Context, id string) (bool, error) {
	if w.fault.Active() {
		w.hasActionRunOutput.Add(1)
	}
	return w.Store.HasActionRunOutput(ctx, id)
}

func (w *countingCommandRequestStore) GetMessagesByIDs(ctx context.Context, ids []string) (map[string]*store.Message, error) {
	if w.fault.Active() {
		w.getMessagesByIDs.Add(1)
	}
	return w.Store.GetMessagesByIDs(ctx, ids)
}

func (w *countingCommandRequestStore) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	if w.fault.Active() {
		w.getMessage.Add(1)
	}
	return w.Store.GetMessage(ctx, id)
}

func (w *countingCommandRequestStore) ListAgentCommandRequests(ctx context.Context, opts *store.ListAgentCommandRequestsOptions) (*store.ListAgentCommandRequestsResult, error) {
	if w.fault.Active() {
		w.listAgentCommandRequests.Add(1)
	}
	return w.Store.ListAgentCommandRequests(ctx, opts)
}

func (w *countingCommandRequestStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if w.fault.Active() {
		w.getUser.Add(1)
	}
	return w.Store.GetUser(ctx, id)
}

// TestUserMeCommandRequests_NoPerCardNPlusOne covers AC-6 and AC-16 batching.
func TestUserMeCommandRequests_NoPerCardNPlusOne(t *testing.T) {
	ctx := context.Background()
	srv, st, counter, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *countingCommandRequestStore {
		return &countingCommandRequestStore{Store: inner, fault: f}
	})
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.CommandRequests: true,
		experiments.InboxDashboard:  true,
	})

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	proj := &store.Project{ID: api.NewUUID(), Slug: "n1-" + api.NewUUID()[:6], Name: "N1Proj"}
	require.NoError(t, st.CreateProject(ctx, proj))
	grantProjectAccessOnly(t, st, alice.ID, proj.ID)
	agent := &store.Agent{ID: api.NewUUID(), Slug: "n1-agent", Name: "N1Agent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, agent))
	execAgent := &store.Agent{ID: api.NewUUID(), Slug: "n1-exec", Name: "N1Exec", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, execAgent))
	aliceTok := crInboxSessionToken(t, srv, alice)

	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	exitZero := 0
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("batch-%02d", i)
		pr := crInboxSeedDM(t, st, proj.ID, agent.ID, agent.Name, alice.ID, key, "echo "+key, false, base.Add(time.Duration(i)*time.Second))
		appRes, err := st.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID: pr.Request.ID, CommandSHA256: pr.Request.CommandSHA256, UserID: alice.ID,
			ExecAgentID: execAgent.ID, ExecProjectID: proj.ID, Surface: msgaction.CommandRequestSurfaceNative,
			Now: base.Add(10*time.Minute + time.Duration(i)*time.Second),
		})
		require.NoError(t, err)
		won, err := st.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID: appRes.Run.ID, Status: store.CommandRunStatusSucceeded, ExitCode: &exitZero,
			OutputStatus: "captured", Output: []byte("ok"), FinishedAt: base.Add(20*time.Minute + time.Duration(i)*time.Second),
		})
		require.NoError(t, err)
		require.True(t, won)
	}

	// Arm counting only for the 50-card Resolved page request.
	fault.Arm()
	rr := crInboxBearerReq(t, srv, http.MethodGet, userMeCommandRequestsPath+"?tab=resolved&limit=50", aliceTok, "")

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp commandRequestsInboxResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Cards, 50)

	assert.Equal(t, int64(1), counter.getCommandRunsByRequestIDs.Load(), "must batch-load runs in 1 query")
	assert.Equal(t, int64(0), counter.getCommandRunByRequestID.Load(), "must not issue per-card GetCommandRunByRequestID")
	assert.LessOrEqual(t, counter.getCommandRequestsByIDs.Load(), int64(1), "must batch-load replacedBy in <= 1 query")
	assert.Equal(t, int64(0), counter.getCommandRequest.Load(), "must not issue per-card GetCommandRequest")
	assert.Equal(t, int64(1), counter.hasActionRunOutputs.Load(), "must batch-load output presence in 1 query")
	assert.Equal(t, int64(0), counter.hasActionRunOutput.Load(), "must not issue per-card HasActionRunOutput")
	assert.Equal(t, int64(1), counter.getMessagesByIDs.Load(), "AC-16: must batch-load prompt messages in 1 query")
	assert.Equal(t, int64(0), counter.getMessage.Load(), "must not issue per-card GetMessage")
	assert.Equal(t, int64(0), counter.listAgentCommandRequests.Load(), "must not check previousVersionUnknown on resolved cards")
	assert.LessOrEqual(t, counter.getUser.Load(), int64(2), "GetUser must be O(1) across 50 cards (1 auth middleware + 1 memoized approver)")
}

// TestUserMeCommandRequests_InboxHints covers AC-8 for every writer in §4.7.
func TestUserMeCommandRequests_InboxHints(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer(t)
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv.SetEventPublisher(pub)
	crInboxSetExperiments(t, srv, st, map[string]bool{
		experiments.CommandRequests: true,
		experiments.ExecContainers:  true,
		experiments.InboxDashboard:  true,
	})

	alice := &store.User{ID: api.NewUUID(), Email: "alice@example.com", DisplayName: "Alice", Role: store.UserRoleAdmin, Status: "active"}
	require.NoError(t, st.CreateUser(ctx, alice))
	proj := &store.Project{ID: api.NewUUID(), Slug: "hints-" + api.NewUUID()[:6], Name: "HintsProj"}
	require.NoError(t, st.CreateProject(ctx, proj))
	crInboxGrantProjectOwner(t, st, alice.ID, proj.ID)
	agent := &store.Agent{ID: api.NewUUID(), Slug: "hints-agent", Name: "HintsAgent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, agent))
	execAgent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "hints-exec",
		Name:      "HintsExec",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "shell",
		},
	}
	require.NoError(t, st.CreateAgent(ctx, execAgent))
	aliceTok := crInboxSessionToken(t, srv, alice)
	agentTok, err := srv.GenerateAgentToken(agent.ID, proj.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Register Alice's exec container so approve succeeds.
	_, err = st.PutUserExecContainer(ctx, alice.ID, execAgent.ID)
	require.NoError(t, err)

	// Stub run launcher so approve doesn't try to dial a real broker.
	srv.SetCommandRunLauncher(CommandRunLauncherFunc(func(_ *store.CommandRequest, _ *store.CommandRun) {}))

	ch, unsub := pub.Subscribe("user." + alice.ID + ".inbox")
	defer unsub()

	expectOneHint := func(label string) {
		t.Helper()
		select {
		case ev := <-ch:
			var hint InboxHint
			require.NoError(t, json.Unmarshal(ev.Data, &hint))
			assert.Equal(t, InboxCategoryAgentExecutions, hint.Category, label)
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: expected 1 agent_executions inbox hint", label)
		}
		select {
		case ev := <-ch:
			t.Fatalf("%s: expected only 1 inbox hint, got extra: %s", label, string(ev.Data))
		case <-time.After(80 * time.Millisecond):
		}
	}
	expectNoHint := func(label string) {
		t.Helper()
		select {
		case ev := <-ch:
			t.Fatalf("%s: expected 0 inbox hints, got: %s", label, string(ev.Data))
		case <-time.After(100 * time.Millisecond):
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	topicID := crInboxCreateTopic(t, srv, proj.ID, "hints-topic", alice.ID)

	// 1. Create (thread) -> 1 hint.
	crCreate := crInboxSeedThread(t, st, proj.ID, agent.ID, agent.Name, alice.ID, topicID, "h-create", "echo 1", false, now)
	srv.publishReplacedCommandRequest(ctx, crCreate)
	expectOneHint("create")

	// 2. Replace on --update -> 1 deduplicated hint for the same approver.
	crReplace := crInboxSeedThread(t, st, proj.ID, agent.ID, agent.Name, alice.ID, topicID, "h-create", "echo 2", true, now.Add(time.Second))
	srv.publishReplacedCommandRequest(ctx, crReplace)
	expectOneHint("replace --update")

	// 3. Approve -> 1 hint.
	appBody := fmt.Sprintf(`{"command_sha256":%q,"surface":"standalone_page"}`, crReplace.Request.CommandSHA256)
	appRR := crInboxBearerReq(t, srv, http.MethodPost, "/api/v1/command-requests/"+crReplace.Request.ID+"/approve", aliceTok, appBody)
	require.Equal(t, http.StatusAccepted, appRR.Code, appRR.Body.String())
	expectOneHint("approve")

	// 4. Dismiss -> 1 hint.
	crDis := crInboxSeedThread(t, st, proj.ID, agent.ID, agent.Name, alice.ID, topicID, "h-dismiss", "echo dis", false, now.Add(2*time.Second))
	disRR := crInboxBearerReq(t, srv, http.MethodPost, "/api/v1/command-requests/"+crDis.Request.ID+"/dismiss", aliceTok, `{"reason":"no"}`)
	require.Equal(t, http.StatusOK, disRR.Code, disRR.Body.String())
	expectOneHint("dismiss")

	// 5. Agent withdraw -> 1 hint.
	_ = crInboxSeedThread(t, st, proj.ID, agent.ID, agent.Name, alice.ID, topicID, "h-withdraw", "echo wd", false, now.Add(3*time.Second))
	wdRR := crInboxBearerReq(t, srv, http.MethodPost, "/api/v1/agent/command-requests/withdraw", agentTok, `{"key":"h-withdraw","reason":"done"}`)
	require.Equal(t, http.StatusOK, wdRR.Code, wdRR.Body.String())
	expectOneHint("agent withdraw")

	// 6. Agent hard-delete withdraw (2 pending rows for same approver) -> 1 deduplicated hint.
	delAgent := &store.Agent{ID: api.NewUUID(), Slug: "del-agent", Name: "DelAgent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, st.CreateAgent(ctx, delAgent))
	_ = crInboxSeedDM(t, st, proj.ID, delAgent.ID, delAgent.Name, alice.ID, "del-1", "echo d1", false, now)
	_ = crInboxSeedDM(t, st, proj.ID, delAgent.ID, delAgent.Name, alice.ID, "del-2", "echo d2", false, now.Add(time.Second))
	withdrawnRows, err := st.WithdrawAgentCommandRequestsOnDelete(ctx, delAgent.ID, now.Add(5*time.Second))
	require.NoError(t, err)
	require.Len(t, withdrawnRows, 2)
	srv.emitPostCommitDeletedAgentCommandRequests(ctx, withdrawnRows)
	expectOneHint("agent hard-delete withdraw")

	// 7. Sweeper expiry (2 expired rows for same approver) -> 1 deduplicated hint.
	for _, k := range []string{"exp-1", "exp-2"} {
		req, nerr := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{Key: k, Script: "echo " + k})
		require.NoError(t, nerr)
		_, cerr := st.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: proj.ID, Sender: "agent:" + agent.Name, SenderID: agent.ID,
				Recipient: topicID, RecipientID: topicID, ThreadID: topicID, Msg: "x", Type: messages.TypeCommandRequest,
			},
			AgentID: agent.ID, AgentName: agent.Name, ApproverID: alice.ID, Request: req,
			Now: now.Add(-2 * time.Hour), TTL: time.Minute,
		})
		require.NoError(t, cerr)
	}
	srv.sweepCommandRuns(ctx)
	expectOneHint("sweeper expiry")

	// 8. Run finished (via finishCommandRun) -> 1 hint.
	runRow, err := st.GetCommandRunByRequestID(ctx, crReplace.Request.ID)
	require.NoError(t, err)
	won, err := srv.finishCommandRun(ctx, crReplace.Request, runRow, nil, store.CommandRunStatusSucceeded, "", nil, "none")
	require.NoError(t, err)
	require.True(t, won)
	expectOneHint("run finished")

	// 9. Markdown from-message -> 0 hints!
	mdMsg := &store.Message{
		ID: api.NewUUID(), ProjectID: proj.ID, Sender: "agent:" + agent.Name, SenderID: agent.ID,
		Recipient: "user:" + alice.ID, RecipientID: alice.ID,
		ThreadID: fmt.Sprintf("dm:agent:%s:user:%s", agent.ID, alice.ID),
		Msg:      "Try this:\n```sh\necho hi-md\n```\n",
		Type:     messages.TypeInstruction,
	}
	require.NoError(t, st.CreateMessage(ctx, mdMsg))
	mdBody := fmt.Sprintf(`{"message_id":%q,"script":"echo hi-md","command_sha256":%q}`,
		mdMsg.ID, messages.CommandRequestCanonicalHash("echo hi-md", "", messages.DefaultCommandRequestTimeoutS))
	mdRR := crInboxBearerReq(t, srv, http.MethodPost, "/api/v1/command-requests/from-message", aliceTok, mdBody)
	require.Equal(t, http.StatusAccepted, mdRR.Code, mdRR.Body.String())
	expectNoHint("markdown from-message")
}
