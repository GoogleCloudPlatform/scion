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
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventfeed"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func watchTestUser(t *testing.T, s store.Store, projectID, projectRole string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	email := id + "@test.com"
	if projectID != "" {
		createTestUserWithProjectRole(t, s, id, email, projectID, projectRole)
	} else {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID:          id,
			Email:       email,
			DisplayName: "User",
			Role:        store.UserRoleMember,
			Status:      "active",
		}))
	}
	ensureHubMembership(ctx, s, id)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

func watchTestDirectConv(t *testing.T, s store.Store, kindA, idA, kindB, idB string) *store.Conversation {
	t.Helper()
	extRef, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             uuid.NewString(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(context.Background(), conv))
	return conv
}

func watchTestGroupConv(t *testing.T, s store.Store, projectID string) *store.Conversation {
	t.Helper()
	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             uuid.NewString(),
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "watch-group-" + uuid.NewString()[:8],
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	if projectID != "" {
		conv.ProjectID = &projectID
	}
	require.NoError(t, s.CreateConversation(context.Background(), conv))
	return conv
}

func callBearer(t *testing.T, srv *Server, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// Criterion 12: Authz parity between GET /api/v1/conversations/{id}/messages
// and GET /api/v1/conversations/{id}/watch across DM key, cross-project gate,
// group project read, UAT scopes, and refused credentials (delegated & federation).
func TestConversationWatch_AuthzParity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projA := uuid.NewString()
	projB := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projA, Name: "Proj A", Slug: "proj-a-" + projA[:8]}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projB, Name: "Proj B", Slug: "proj-b-" + projB[:8]}))

	memberA := watchTestUser(t, s, projA, store.ProjectRoleMember)
	outsider := watchTestUser(t, s, "", "")

	agentA := &store.Agent{ID: uuid.NewString(), Slug: "agent-a", Name: "Agent A", ProjectID: projA, Phase: string(state.PhaseRunning)}
	agentB := &store.Agent{ID: uuid.NewString(), Slug: "agent-b", Name: "Agent B", ProjectID: projB, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))

	dmUserAgent := watchTestDirectConv(t, s, "user", memberA.ID, "agent", agentA.ID)
	dmCrossProj := watchTestDirectConv(t, s, "agent", agentA.ID, "agent", agentB.ID)
	groupA := watchTestGroupConv(t, s, projA)

	assertParity := func(name string, wantStatus int, call func(path string) *httptest.ResponseRecorder, convID string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			msgRec := call("/api/v1/conversations/" + convID + "/messages")
			watchRec := call("/api/v1/conversations/" + convID + "/watch?wait=0")
			assert.Equal(t, wantStatus, msgRec.Code, "/messages body: %s", msgRec.Body.String())
			assert.Equal(t, msgRec.Code, watchRec.Code, "/watch body: %s", watchRec.Body.String())
		})
	}

	// 1. DM canonical key (session user)
	assertParity("dm participant user allowed", http.StatusOK, func(p string) *httptest.ResponseRecorder {
		return doRequestAsUser(t, srv, memberA, http.MethodGet, p, nil)
	}, dmUserAgent.ID)
	assertParity("dm outsider user denied", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		return doRequestAsUser(t, srv, outsider, http.MethodGet, p, nil)
	}, dmUserAgent.ID)

	// 2. Group project read (session user)
	assertParity("group project member allowed", http.StatusOK, func(p string) *httptest.ResponseRecorder {
		return doRequestAsUser(t, srv, memberA, http.MethodGet, p, nil)
	}, groupA.ID)
	assertParity("group outsider denied", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		return doRequestAsUser(t, srv, outsider, http.MethodGet, p, nil)
	}, groupA.ID)

	// 3. Cross-project agent DM read gate (hub cross-project disabled by default)
	assertParity("cross-project agent DM denied", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req = req.WithContext(agentContextWithScopes(agentA.ID, projA, []AgentTokenScope{ScopeProjectRead}))
		rec := httptest.NewRecorder()
		srv.handleConversationRoutes(rec, req)
		return rec
	}, dmCrossProj.ID)

	// 4. UAT scopes: DM needs inbox:read + agent:read inside peer agent's project;
	// group needs project:read.
	tokDMOK := mintHubConfigToken(t, srv, memberA.ID, projectBoundary(projA), "inbox:read", "agent:read")
	tokDMNoAgent := mintHubConfigToken(t, srv, memberA.ID, projectBoundary(projA), "inbox:read")
	tokGroupOK := mintHubConfigToken(t, srv, memberA.ID, projectBoundary(projA), "project:read")

	assertParity("UAT DM inbox:read+agent:read allowed", http.StatusOK, func(p string) *httptest.ResponseRecorder {
		return callBearer(t, srv, tokDMOK, p)
	}, dmUserAgent.ID)
	assertParity("UAT DM inbox:read without agent:read denied", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		return callBearer(t, srv, tokDMNoAgent, p)
	}, dmUserAgent.ID)
	assertParity("UAT DM project:read only denied", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		return callBearer(t, srv, tokGroupOK, p)
	}, dmUserAgent.ID)
	assertParity("UAT group project:read allowed", http.StatusOK, func(p string) *httptest.ResponseRecorder {
		return callBearer(t, srv, tokGroupOK, p)
	}, groupA.ID)

	// 5. Refused credentials: delegated agent token & federation token.
	assertParity("delegated agent credential refused", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		reqCtx := agentContextWithScopes(agentA.ID, projA, []AgentTokenScope{ScopeProjectRead})
		reqCtx = contextWithCredentialContext(reqCtx, CredentialContext{Kind: CredentialKind(AuthTypeAgentDelegation), Type: "agent"})
		req = req.WithContext(reqCtx)
		rec := httptest.NewRecorder()
		srv.handleConversationRoutes(rec, req)
		return rec
	}, dmUserAgent.ID)

	assertParity("federation credential refused", http.StatusForbidden, func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		reqCtx := contextWithIdentity(req.Context(), NewAuthenticatedUser(memberA.ID, memberA.Email, memberA.DisplayName, memberA.Role, "federation"))
		reqCtx = contextWithCredentialContext(reqCtx, CredentialContext{Kind: CredentialKindFederation, Type: "user"})
		req = req.WithContext(reqCtx)
		rec := httptest.NewRecorder()
		srv.handleConversationRoutes(rec, req)
		return rec
	}, dmUserAgent.ID)
}

// Criterion 13: Experiment off returns 404; wait is clamped to 55s; a wait
// longer than Server.WriteTimeout (5s with a 6s wait) completes with 200 OK.
func TestConversationWatch_ExperimentWaitClampAndWriteDeadline(t *testing.T) {
	// Wait clamping unit checks.
	d, err := parseWatchWait("120s")
	require.NoError(t, err)
	assert.Equal(t, 55*time.Second, d)

	d, err = parseWatchWait("999")
	require.NoError(t, err)
	assert.Equal(t, 55*time.Second, d)

	d, err = parseWatchWait("")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, d)

	_, err = parseWatchWait("-1s")
	assert.Error(t, err)

	srv, s := testServer(t)
	ctx := context.Background()
	projID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projID, Name: "P", Slug: "p-" + projID[:8]}))
	user := watchTestUser(t, s, projID, store.ProjectRoleMember)
	conv := watchTestGroupConv(t, s, projID)

	// Experiment off -> 404.
	offReg, regErr := experiments.NewRegistry([]experiments.Experiment{
		testExperiment(experiments.ConversationWatch, false, experiments.LayerServer),
	}, nil)
	require.NoError(t, regErr)
	srv.setExperimentRegistry(offReg)
	recOff := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/conversations/"+conv.ID+"/watch?wait=0", nil)
	assert.Equal(t, http.StatusNotFound, recOff.Code)

	// Restore default registry (hub.conversation_watch default true).
	srv.setExperimentRegistry(nil)

	// Real HTTP server with WriteTimeout=5s handling a 6s watch wait -> 200 OK.
	tok := mintHubConfigToken(t, srv, user.ID, projectBoundary(projID), "project:read")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpSrv := &http.Server{
		Handler:      srv.Handler(),
		WriteTimeout: 5 * time.Second,
	}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/api/v1/conversations/"+conv.ID+"/watch?wait=6s", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	var body conversationWatchResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.True(t, body.TimedOut)
	assert.Empty(t, body.Events)
	assert.NotEmpty(t, body.NextCursor)
}

// Criterion 14 & 17: Any CreateMessage path wakes a waiting poll via
// feedNotifier + feedWaker without double-publishing user.<id>.message, and
// after_message returns replies already committed before watch started.
func TestConversationWatch_CreateMessageWakesPoll(t *testing.T) {
	srv, s := testServer(t)
	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	ctx := context.Background()
	projID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projID, Name: "P", Slug: "p-" + projID[:8]}))
	user := watchTestUser(t, s, projID, store.ProjectRoleMember)
	agent := &store.Agent{ID: uuid.NewString(), Slug: "a1", Name: "Agent 1", ProjectID: projID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agent))
	dm := watchTestDirectConv(t, s, "user", user.ID, "agent", agent.ID)

	// Send initial user message (seq 1).
	sent := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		Msg:            "do X",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, s.CreateMessage(ctx, sent))

	// Subscribe to user.<id>.message to prove feedNotifier does NOT double-publish.
	userMsgCh, unsubUser := ep.Subscribe("user." + user.ID + ".message")
	defer unsubUser()

	// Start waiting watch poll in background.
	resCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := doRequestAsUser(t, srv, user, http.MethodGet,
			"/api/v1/conversations/"+dm.ID+"/watch?after_message="+sent.ID+"&wait=5s", nil)
		resCh <- rec
	}()

	time.Sleep(50 * time.Millisecond)

	// Persist reply via store.CreateMessage directly (covers all writer paths,
	// including scheduled/held messages).
	reply := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		Msg:            "done X",
		Type:           "instruction",
		DispatchState:  store.MessageDispatchDispatched,
	}
	start := time.Now()
	require.NoError(t, s.CreateMessage(ctx, reply))

	select {
	case rec := <-resCh:
		assert.Less(t, time.Since(start), time.Second, "wakeup should deliver in < 1s")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body conversationWatchResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Events, 1)
		assert.Equal(t, "msg:"+reply.ID, body.Events[0].EventID)
		assert.Equal(t, "done X", body.Events[0].Data.Msg)
		assert.False(t, body.TimedOut)
	case <-time.After(3 * time.Second):
		t.Fatal("watch poll did not wake up after CreateMessage")
	}

	// Verify feedNotifier did NOT publish to user.<id>.message.
	select {
	case evt := <-userMsgCh:
		t.Fatalf("unexpected double-publish on %s", evt.Subject)
	default:
	}

	// Criterion 17: after_message returns reply already committed before watch started.
	recImmediate := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/conversations/"+dm.ID+"/watch?after_message="+sent.ID+"&wait=0", nil)
	require.Equal(t, http.StatusOK, recImmediate.Code)
	var immBody conversationWatchResponse
	require.NoError(t, json.Unmarshal(recImmediate.Body.Bytes(), &immBody))
	require.Len(t, immBody.Events, 1)
	assert.Equal(t, "msg:"+reply.ID, immBody.Events[0].EventID)

	// 400 invalid_cursor and 410 cursor_expired.
	recBad := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/conversations/"+dm.ID+"/watch?cursor=not-valid&wait=0", nil)
	assert.Equal(t, http.StatusBadRequest, recBad.Code)

	futureCursor := eventfeed.EncodeCursor(dm.ID, 999)
	recExp := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/conversations/"+dm.ID+"/watch?cursor="+string(futureCursor)+"&wait=0", nil)
	assert.Equal(t, http.StatusGone, recExp.Code)
}

// Regression test: PromoteDM moving messages from a DM conversation into a new
// topic conversation must re-sequence conversation_seq and update the target
// conversation's last_message_seq so subsequent CreateMessage calls never
// collide on the (conversation_id, conversation_seq) unique index.
func TestPromoteDM_ReallocatesConversationSeqWithoutCollision(t *testing.T) {
	_, s := testServer(t)
	cs, ok := s.(*entadapter.CompositeStore)
	require.True(t, ok, "testServer store must be *entadapter.CompositeStore")
	wcs := NewWebChatStore(cs.DB(), "sqlite")
	require.NoError(t, wcs.Init())

	ctx := context.Background()
	projID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projID, Name: "P", Slug: "p-" + projID[:8]}))
	user := watchTestUser(t, s, projID, store.ProjectRoleMember)
	agent := &store.Agent{ID: uuid.NewString(), Slug: "a1", Name: "Agent 1", ProjectID: projID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agent))
	dm := watchTestDirectConv(t, s, "user", user.ID, "agent", agent.ID)

	m1 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		Channel:        "web",
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		Msg:            "first DM msg",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, s.CreateMessage(ctx, m1))
	m2 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		Channel:        "web",
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		Msg:            "second DM msg",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, s.CreateMessage(ctx, m2))

	now := time.Now().UTC()
	topic, err := wcs.PromoteDM(ctx, WebChatTopic{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "promoted-topic",
		CreatedBy:      user.ID,
		CreatedAt:      now,
		LastActivityAt: now,
	}, PromoteKeys{
		DMKey:                dm.ExternalRef,
		DirectConversationID: dm.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, topic.ConversationID)

	head, err := s.ConversationHeadSeq(ctx, topic.ConversationID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), head)

	// Next CreateMessage in the promoted topic must succeed with seq 3 (no unique collision).
	m3 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: topic.ConversationID,
		ThreadID:       topic.ID,
		Channel:        "web",
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		Msg:            "after promote",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, s.CreateMessage(ctx, m3))
	assert.Equal(t, int64(3), m3.ConversationSeq)

	page, err := s.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: topic.ConversationID,
		AfterSeq:       0,
		Filter:         store.ConversationFeedFilterAll,
	})
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)
	assert.Equal(t, []string{m1.ID, m2.ID, m3.ID}, []string{
		page.Messages[0].ID, page.Messages[1].ID, page.Messages[2].ID,
	})
}

// Regression test (O1): CreateMessage inside store.WithTx wakes a waiting poll
// after Commit, and does NOT wake if the transaction rolls back.
func TestConversationWatch_WithTxCreateMessageWakesPoll(t *testing.T) {
	srv, s := testServer(t)
	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	srv.SetEventPublisher(ep)

	ctx := context.Background()
	projID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projID, Name: "P", Slug: "p-" + projID[:8]}))
	user := watchTestUser(t, s, projID, store.ProjectRoleMember)
	conv := watchTestGroupConv(t, s, projID)

	// Subscribe directly to verify rollback does NOT publish.
	wakeCh, unsub := ep.Subscribe("conversation." + conv.ID + ".message")
	defer unsub()

	rollbackErr := assert.AnError
	err := s.WithTx(ctx, func(tx store.Store) error {
		if cErr := tx.CreateMessage(ctx, &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      projID,
			ConversationID: conv.ID,
			Sender:         "agent:a",
			SenderID:       "a1",
			Recipient:      "user:" + user.Email,
			RecipientID:    user.ID,
			Msg:            "rolled back",
			Type:           "chat",
			DispatchState:  store.MessageDispatchDispatched,
		}); cErr != nil {
			return cErr
		}
		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)
	select {
	case evt := <-wakeCh:
		t.Fatalf("rolled-back tx must not publish wakeup, got %s", evt.Subject)
	default:
	}

	// Start a waiting watch poll, then commit a message inside WithTx.
	resCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		resCh <- doRequestAsUser(t, srv, user, http.MethodGet,
			"/api/v1/conversations/"+conv.ID+"/watch?start=head&wait=5s", nil)
	}()
	time.Sleep(50 * time.Millisecond)

	txMsg := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: conv.ID,
		Sender:         "agent:a",
		SenderID:       "a1",
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		Msg:            "committed in tx",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	start := time.Now()
	require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
		return tx.CreateMessage(ctx, txMsg)
	}))

	select {
	case rec := <-resCh:
		assert.Less(t, time.Since(start), time.Second, "WithTx commit must wake poll promptly")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body conversationWatchResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Events, 1)
		assert.Equal(t, "msg:"+txMsg.ID, body.Events[0].EventID)
	case <-time.After(3 * time.Second):
		t.Fatal("watch poll did not wake after WithTx CreateMessage commit")
	}
}

// Regression test (O2): dev and federated_user identities normalize to "user".
func TestNormalizeFeedPrincipalKind(t *testing.T) {
	assert.Equal(t, "user", normalizeFeedPrincipalKind("user"))
	assert.Equal(t, "user", normalizeFeedPrincipalKind("dev"))
	assert.Equal(t, "user", normalizeFeedPrincipalKind("federated_user"))
	assert.Equal(t, "agent", normalizeFeedPrincipalKind("agent"))
}
