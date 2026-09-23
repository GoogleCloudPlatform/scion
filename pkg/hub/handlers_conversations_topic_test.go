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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// topicEventSpy embeds noopEventPublisher and records PublishChatTopicEvent
// calls, so tests can assert an SSE "created" event fired without wiring a
// full ChannelEventPublisher and subscribing to it.
type topicEventSpy struct {
	noopEventPublisher
	mu     sync.Mutex
	topics []struct {
		projectID string
		action    string
		topic     WebChatTopic
	}
}

func (p *topicEventSpy) PublishChatTopicEvent(_ context.Context, projectID string, action string, topic WebChatTopic) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, struct {
		projectID string
		action    string
		topic     WebChatTopic
	}{projectID, action, topic})
}

func (p *topicEventSpy) events() []struct {
	projectID string
	action    string
	topic     WebChatTopic
} {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]struct {
		projectID string
		action    string
		topic     WebChatTopic
	}, len(p.topics))
	copy(out, p.topics)
	return out
}

// setupGroupConvTopicTest builds a server whose webChatStore shares the
// store's underlying SQLite DB. Sharing is load-bearing: CreateTopic's
// dual-write lands in the same "conversations" table that
// store.GetConversation reads from (see TestDEF96_PromoteDM_HistoryVisibleOnFirstRead
// for the same pattern). Without sharing, the handler's read-back after
// CreateTopic would always 500.
func setupGroupConvTopicTest(t *testing.T) (*Server, store.Store, WebChatStore, *topicEventSpy) {
	t.Helper()
	srv, s := testServer(t)

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store does not expose DB()")
	rawDB := dbProvider.DB()
	require.NotNil(t, rawDB, "store DB() returned nil")

	wcs := NewWebChatStore(rawDB, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	spy := &topicEventSpy{}
	srv.events = spy

	return srv, s, wcs, spy
}

// TestCreateConversation_GroupBecomesTopic is the Phase 1 vertical-slice
// gate for the chat-thread-bridge fix: a kind=group conversation created via
// POST /api/v1/conversations must mint a linked webchat_topic (not an
// orphan, empty-ref conversation), publish a chat.topic "created" SSE event,
// and appear in the web thread list.
func TestCreateConversation_GroupBecomesTopic(t *testing.T) {
	srv, s, wcs, spy := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{
		DisplayName: "pstack2",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "group", result.Kind)
	require.Equal(t, "native", result.Surface)
	require.NotEmpty(t, result.ID)

	// AC-1 (design §9): externalRef = "thread:<projectId>:<topicId>".
	refProjectID, topicID, err := messaging.ParseThreadConversationExternalRef(result.ExternalRef)
	require.NoError(t, err, "externalRef should be a well-formed thread key, got %q", result.ExternalRef)
	require.Equal(t, project.ID, refProjectID)
	require.NotEmpty(t, topicID)

	// AC-2: exactly one webchat_topic row links to the new conversation.
	topic, err := wcs.GetTopic(ctx, topicID)
	require.NoError(t, err)
	require.NotNil(t, topic, "expected a topic row for %s", topicID)
	require.Equal(t, result.ID, topic.ConversationID, "topic.conversation_id must equal the new conversation's id")
	require.Equal(t, "pstack2", topic.Name)
	require.Equal(t, project.ID, topic.ProjectID)

	// AC-3: a chat.topic "created" SSE event was published for the project
	// with the topic payload.
	events := spy.events()
	require.Len(t, events, 1, "expected exactly one PublishChatTopicEvent call")
	require.Equal(t, project.ID, events[0].projectID)
	require.Equal(t, "created", events[0].action)
	require.Equal(t, topicID, events[0].topic.ID)
	require.Equal(t, result.ID, events[0].topic.ConversationID)

	// AC-4: GET /api/v1/chat/spaces/{projectId}/threads as a project member
	// includes the thread. The dev-auth identity used by doRequest has
	// platform-admin project access, matching the convention used by
	// TestChatV2_CreateThread_AndList for the same endpoint.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+project.ID+"/threads", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var listResp chatTopicListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))

	var found *chatTopicEntry
	for i := range listResp.Threads {
		if listResp.Threads[i].ID == topicID {
			found = &listResp.Threads[i]
			break
		}
	}
	require.NotNil(t, found, "expected topic %s to appear in handleListThreads", topicID)
	require.Equal(t, "pstack2", found.Name)

	// Sanity: the conversation itself is readable from the store directly,
	// confirming the read-back path (§3.4) used the right ConversationID.
	conv, err := s.GetConversation(ctx, result.ID)
	require.NoError(t, err)
	require.Equal(t, result.ExternalRef, conv.ExternalRef)
	require.NotNil(t, conv.ProjectID)
	require.Equal(t, project.ID, *conv.ProjectID)
}

// TestCreateConversation_GroupMessageReachesThreadHistory is the AC-5/AC-6
// gate design §8 Phase 1 calls out explicitly: linkage alone
// (TestCreateConversation_GroupBecomesTopic covers AC-1..AC-4) doesn't prove
// that messages sent into the new conversation actually reach the web
// thread — which is the literal user-visible symptom in #1753.
//
// AC-5: `scion message conv:<id> "hi"` (conv-ref → outbound-message handler)
// persists with ThreadID == topicID and Channel == "web" (the same fields
// the DEF-160 group-routing branch has always set for `thread:`-prefixed
// external refs — untouched by this PR, exercised here for the first time
// against a CreateTopic-minted conversation), and the message is visible via
// thread history with the read-switch both OFF and ON.
// AC-6: a web post to the same thread persists with the same conversation_id.
func TestCreateConversation_GroupMessageReachesThreadHistory(t *testing.T) {
	srv, s, _, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create the group via the conversation API — the exact path this issue
	// fixes — rather than seeding a topic directly.
	createBody := createConversationRequest{
		DisplayName: "history-check",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	createBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())

	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))
	_, topicID, err := messaging.ParseThreadConversationExternalRef(created.ExternalRef)
	require.NoError(t, err)

	// AC-5: send via conv:<id> — the same path `scion message conv:<id> "..."`
	// takes (cmd/message.go sets ConversationRef and posts to
	// /api/v1/agents/{id}/outbound-message; postConvRefNoRecipient mirrors
	// that exactly, see handlers_outbound_def158_test.go).
	sendRR := postConvRefNoRecipient(t, srv, project.ID, agent.ID, "hi from cli", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, sendRR.Code, "body: %s", sendRR.Body.String())

	var sendResp map[string]interface{}
	require.NoError(t, json.Unmarshal(sendRR.Body.Bytes(), &sendResp))
	msgID, _ := sendResp["message_id"].(string)
	require.NotEmpty(t, msgID, "expected message_id in outbound-message response")

	stored, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	require.Equal(t, topicID, stored.ThreadID,
		"ThreadID must equal the topic ID: group routing sets it from the thread: external_ref (handlers_agent_messaging.go DEF-160 branch)")
	require.Equal(t, "web", stored.Channel,
		"Channel must be 'web' so live SSE fan-out and the read-switch-OFF history filter both see it")
	require.Equal(t, created.ID, stored.ConversationID)

	// AC-5: the message is returned by thread history — read-switch OFF...
	offRec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, offRec.Code, "body: %s", offRec.Body.String())
	var offHist chatHistoryResponse
	require.NoError(t, json.Unmarshal(offRec.Body.Bytes(), &offHist))
	require.True(t, chatHistoryContains(offHist.Messages, "hi from cli"),
		"read-switch OFF: message missing from thread history")

	// ...and ON.
	enableReadSwitch(t, srv)
	onRec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/conversations/"+topicID+"/messages", nil)
	require.Equal(t, http.StatusOK, onRec.Code, "body: %s", onRec.Body.String())
	var onHist chatHistoryResponse
	require.NoError(t, json.Unmarshal(onRec.Body.Bytes(), &onHist))
	require.True(t, chatHistoryContains(onHist.Messages, "hi from cli"),
		"read-switch ON: message missing from thread history")

	// AC-6: a web post to the same thread persists with the same conversation_id.
	webRec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "hi from web"})
	require.Equal(t, http.StatusCreated, webRec.Code, "body: %s", webRec.Body.String())
	var webResp chatMessageResponse
	require.NoError(t, json.Unmarshal(webRec.Body.Bytes(), &webResp))
	webStored, err := s.GetMessage(ctx, webResp.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, webStored.ConversationID,
		"AC-6: a web post to the same thread must persist with the same conversation_id as the CLI-created group")
}

// chatHistoryContains reports whether msgs contains a message with the given text.
func chatHistoryContains(msgs []store.Message, text string) bool {
	for _, m := range msgs {
		if m.Msg == text {
			return true
		}
	}
	return false
}

// TestCreateConversation_GroupServiceUnavailable verifies that when no
// webChatStore is wired (Init() failed at boot, or a build predating this
// fix), group creation fails loudly with 503 instead of silently minting
// the old orphan (empty-ref, topic-less) conversation.
func TestCreateConversation_GroupServiceUnavailable(t *testing.T) {
	srv, s := testServer(t) // no SetWebChatStore call: wcs is nil.
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{
		DisplayName: "no-store",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code, "body: %s", rr.Body.String())
}

// TestCreateConversation_InvalidName is a Phase 2 edge case (design §8):
// a name that fails validateThreadName's rules is rejected with 400 before
// any topic/conversation row is written.
func TestCreateConversation_InvalidName(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{
		DisplayName: "bad/name",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())

	topics, err := wcs.ListTopics(context.Background(), project.ID)
	require.NoError(t, err)
	require.Empty(t, topics, "an invalid name must not write a topic row")
}

// TestCreateConversation_NameConflict is a Phase 2 edge case (design §8,
// AC-7): a duplicate name (case-insensitive) in the same project is
// rejected with 409 NAME_CONFLICT, and the failed attempt leaves no orphan
// topic or conversation row behind (CreateTopic's insert is atomic).
func TestCreateConversation_NameConflict(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:        api.NewUUID(),
		ProjectID: project.ID,
		Name:      "Design Review",
		CreatedBy: agent.ID,
		CreatedAt: time.Now().UTC(),
	}))

	// Baseline: setupConvTestData already seeds one group conversation
	// directly via the store, plus the one from CreateTopic above — take the
	// count as it stands, rather than assuming a specific fixture count, so
	// this only asserts the failed create below adds nothing.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	var convCountBefore int
	require.NoError(t, dbProvider.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE project_id = ? AND kind = 'group'`, project.ID).Scan(&convCountBefore))

	body := createConversationRequest{
		DisplayName: "design review", // case-insensitive collision
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	require.Equal(t, "NAME_CONFLICT", errResp.Error.Code)

	// AC-7: no orphan conversation or topic row is left behind.
	topics, err := wcs.ListTopics(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, topics, 1, "the failed create must not have minted a second topic")

	var convCountAfter int
	require.NoError(t, dbProvider.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE project_id = ? AND kind = 'group'`, project.ID).Scan(&convCountAfter))
	require.Equal(t, convCountBefore, convCountAfter, "the failed create must not have minted a second (orphan) conversation")
}

// TestCreateConversation_ProjectRequired is a Phase 2 edge case (design §7
// Q2, AC-10): a user identity with no projectId in the body and no token
// project to fall back to (only agents have one) gets 400 before any write.
func TestCreateConversation_ProjectRequired(t *testing.T) {
	srv, s := testServer(t)
	wireSharedWebChatStore(t, srv, s)

	userCtx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(api.NewUUID(), "noproj@example.com", "No Project User", "user", "web"))

	body := createConversationRequest{
		DisplayName: "orphan-attempt",
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(userCtx)
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())

	// AC-10: no conversation or topic row was written for this attempt.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	var convCount, topicCount int
	require.NoError(t, dbProvider.DB().QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE display_name = ?`, "orphan-attempt").Scan(&convCount))
	require.Equal(t, 0, convCount, "no conversation row should exist")
	require.NoError(t, dbProvider.DB().QueryRow(
		`SELECT COUNT(*) FROM webchat_topic WHERE name = ?`, "orphan-attempt").Scan(&topicCount))
	require.Equal(t, 0, topicCount, "no topic row should exist")
}

// TestCreateConversation_AgentNoProjectUsesTokenProject verifies the other
// half of AC-10: an agent identity with no projectId in the body uses its
// token project instead of 400ing (unchanged from Phase 1's defaulting
// behavior, re-asserted here alongside the new project-required guard so
// the two don't regress into each other).
func TestCreateConversation_AgentNoProjectUsesTokenProject(t *testing.T) {
	srv, s, _, _ := setupGroupConvTopicTest(t)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{DisplayName: "agent-default-project"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.NotNil(t, result.ProjectID)
	require.Equal(t, project.ID, *result.ProjectID)
}

// TestCreateConversation_DevIdentitySkipsParticipant is a Phase 2 edge case
// (design §8): ConversationParticipant.principal_kind only allows
// "user"/"agent" (ent schema), so the dev pseudo-identity used by
// SCION_DEV_AUTH_TOKEN cannot be inserted as a participant. The create must
// still succeed — skipping the participant insert — rather than fail after
// the topic (and its linked conversation) already exist.
func TestCreateConversation_DevIdentitySkipsParticipant(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "dev-identity-project", Slug: "dev-identity-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	devCtx := contextWithIdentity(context.Background(), NewDevUser(DevUserConfig{}))

	body := createConversationRequest{
		DisplayName: "dev-created",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(devCtx)
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Participants, "dev identity is not a valid participant principal_kind; the insert must be skipped, not fail the create")

	// The topic itself was still created — only the participant insert was skipped.
	_, topicID, err := messaging.ParseThreadConversationExternalRef(result.ExternalRef)
	require.NoError(t, err)
	topic, err := wcs.GetTopic(ctx, topicID)
	require.NoError(t, err)
	require.NotNil(t, topic)
}

// failingAddParticipantStore wraps a real store.Store and makes
// AddParticipant always fail. Used to cheaply inject the O1 partial-failure
// scenario: the topic (and its linked conversation) commit successfully,
// but the subsequent participant insert fails.
type failingAddParticipantStore struct {
	store.Store
}

func (s *failingAddParticipantStore) AddParticipant(_ context.Context, _ *store.ConversationParticipant) error {
	return errors.New("injected AddParticipant failure")
}

// TestCreateConversation_AddParticipantFailureStillReturns201 covers O1
// (review round 1): if AddParticipant fails after the topic/conversation
// have already committed and been announced via SSE, the create must still
// return 201 with an empty participants array — not a 500 for a resource
// that in fact exists (a retry would then 409 NAME_CONFLICT with no way to
// recover the participant row).
func TestCreateConversation_AddParticipantFailureStillReturns201(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	srv.store = &failingAddParticipantStore{Store: s}

	body := createConversationRequest{
		DisplayName: "participant-fail",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Participants, "AddParticipant failed; participants must be empty, not fabricated")

	// Restore the real store and verify the topic/conversation genuinely exist.
	srv.store = s
	_, topicID, err := messaging.ParseThreadConversationExternalRef(result.ExternalRef)
	require.NoError(t, err)
	topic, err := wcs.GetTopic(context.Background(), topicID)
	require.NoError(t, err)
	require.NotNil(t, topic, "the topic must still exist despite the participant-insert failure")
}

func TestValidateThreadName(t *testing.T) {
	tooLongRunes := make([]rune, 101)
	for i := range tooLongRunes {
		tooLongRunes[i] = 'a'
	}
	tooLong := string(tooLongRunes)

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "trims whitespace", raw: "  hello world  ", want: "hello world"},
		{name: "empty after trim", raw: "   ", wantErr: true},
		{name: "invalid characters", raw: "bad/name", wantErr: true},
		{name: "too long", raw: tooLong, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateThreadName(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestPhase1_SetDefaultAgent_ConvergesAndDispatches is the Phase 1
// vertical-slice gate for the conv-default-mention fix (design doc §8): a
// default agent set through the conversation API on a topic-linked group
// must converge both storage columns in one transaction, publish a topic
// "updated" SSE event, appear on the topic API, and actually drive web-post
// routing — not just sit in a column nothing reads (design doc F1 / findings
// F1).
func TestPhase1_SetDefaultAgent_ConvergesAndDispatches(t *testing.T) {
	srv, s, wcs, spy := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create the group through the conversation API — the exact write path
	// this issue's F1 fix targets — so the conversation is topic-linked.
	createBody := createConversationRequest{
		DisplayName: "default-agent-e2e",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	createBytes, _ := json.Marshal(createBody)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())

	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))
	_, topicID, err := messaging.ParseThreadConversationExternalRef(created.ExternalRef)
	require.NoError(t, err)

	// The creating agent (`agent`) was granted project access above via
	// grantAgentProjectAccess, so it can call PUT default-agent under the
	// project-based gate (design §3.2, landed in Phase 3;
	// authorizeGroupConversationAccess replaced the participant-row check
	// this comment used to describe — review round 1 finding #8).
	defaultAgent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "default-bot",
		Slug:       "default-bot",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, defaultAgent))

	putBody := setDefaultAgentRequest{AgentID: defaultAgent.ID}
	putBytes, _ := json.Marshal(putBody)
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+created.ID+"/default-agent", bytes.NewReader(putBytes))
	putReq.Header.Set("Content-Type", "application/json")
	putReq = putReq.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	putRR := httptest.NewRecorder()
	srv.handleSetDefaultAgent(putRR, putReq, created.ID)
	require.Equal(t, http.StatusOK, putRR.Code, "body: %s", putRR.Body.String())

	// webchat_topic.default_agent == agent.Slug.
	topic, err := wcs.GetTopic(ctx, topicID)
	require.NoError(t, err)
	require.NotNil(t, topic)
	require.Equal(t, defaultAgent.Slug, topic.DefaultAgent)

	// conversations.default_agent_id == agent.ID.
	conv, err := s.GetConversation(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, conv.DefaultAgentID, "conversations.default_agent_id must be set")
	require.Equal(t, defaultAgent.ID, *conv.DefaultAgentID)

	// A topic "updated" SSE event was published (the create above already
	// published one "created" event, so look for the "updated" one).
	var sawUpdated bool
	for _, e := range spy.events() {
		if e.action == "updated" && e.topic.ID == topicID {
			sawUpdated = true
			require.Equal(t, defaultAgent.Slug, e.topic.DefaultAgent)
		}
	}
	require.True(t, sawUpdated, "expected a topic 'updated' SSE event after PUT default-agent")

	// GET of the topic API returns defaultAgent.
	getRR := doRequest(t, srv, http.MethodGet, "/api/v1/chat/topics/"+topicID, nil)
	require.Equal(t, http.StatusOK, getRR.Code, "body: %s", getRR.Body.String())
	var gotTopic WebChatTopic
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &gotTopic))
	require.Equal(t, defaultAgent.Slug, gotTopic.DefaultAgent)

	// A web post with no @mention is dispatched to the default agent
	// (agent-routed messages get type "instruction", not "chat").
	postRR := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "please help"})
	require.Equal(t, http.StatusCreated, postRR.Code, "body: %s", postRR.Body.String())
	var msgResp chatMessageResponse
	require.NoError(t, json.Unmarshal(postRR.Body.Bytes(), &msgResp))
	require.Equal(t, messages.TypeInstruction, msgResp.Type,
		"expected the no-mention post to be routed to the default agent")
}

// TestPhase2_CreateThread_DefaultAgent_SetsBothColumns is AC-4 (design doc
// §9): creating a web thread with a defaultAgent must set
// conversations.default_agent_id on the conversation CreateTopic mints
// alongside it, not just webchat_topic.default_agent.
func TestPhase2_CreateThread_DefaultAgent_SetsBothColumns(t *testing.T) {
	srv, s, _, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "phase2-create-thread", Slug: "phase2-create-thread"}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "phase2-agent",
		Slug:       "phase2-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	body := map[string]string{"name": "phase2-thread", "defaultAgent": agent.Slug}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/spaces/"+project.ID+"/threads", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var topic WebChatTopic
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &topic))
	require.Equal(t, agent.Slug, topic.DefaultAgent)

	// The response's topic doesn't carry ConversationID back (CreateTopic
	// takes WebChatTopic by value and auto-generates it internally), so look
	// the conversation up the same way the reverse-pointer design relies on:
	// by its derived thread: external_ref.
	extRef, err := messaging.ThreadConversationExternalRef(project.ID, topic.ID)
	require.NoError(t, err)
	conv, err := s.GetConversationByExternalRef(ctx, "native", extRef)
	require.NoError(t, err)
	require.NotNil(t, conv)
	require.NotNil(t, conv.DefaultAgentID, "AC-4: creating a web thread with a defaultAgent must also set the conversation's default_agent_id")
	require.Equal(t, agent.ID, *conv.DefaultAgentID)
}

// TestPhase2_TopicPatch_DefaultAgent_ConvergesBothColumns is AC-3 (design doc
// §9): setting or clearing the default via the topic PATCH must update
// conversations.default_agent_id, not just the topic projection.
func TestPhase2_TopicPatch_DefaultAgent_ConvergesBothColumns(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "phase2-patch", Slug: "phase2-patch"}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "phase2-patch-agent",
		Slug:       "phase2-patch-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := api.NewUUID()
	convID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "phase2-patch-thread",
		ConversationID: convID, CreatedBy: "dev", CreatedAt: time.Now().UTC(),
	}))

	// Set via PATCH.
	slug := agent.Slug
	setRec := doRequest(t, srv, http.MethodPatch, "/api/v1/chat/topics/"+topicID,
		map[string]*string{"defaultAgent": &slug})
	require.Equal(t, http.StatusOK, setRec.Code, "body: %s", setRec.Body.String())

	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.NotNil(t, conv.DefaultAgentID, "AC-3: setting via topic PATCH must set conversations.default_agent_id")
	require.Equal(t, agent.ID, *conv.DefaultAgentID)

	// Clear via PATCH.
	empty := ""
	clearRec := doRequest(t, srv, http.MethodPatch, "/api/v1/chat/topics/"+topicID,
		map[string]*string{"defaultAgent": &empty})
	require.Equal(t, http.StatusOK, clearRec.Code, "body: %s", clearRec.Body.String())

	conv, err = s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Nil(t, conv.DefaultAgentID, "AC-3: clearing via topic PATCH must clear conversations.default_agent_id")
}

// TestPhase2_ClearTopicDefaultAgent_ClearsBothColumns is AC-5 (design doc
// §9): deleting an agent must clear it from both the topic projection and
// the conversation owner column.
func TestPhase2_ClearTopicDefaultAgent_ClearsBothColumns(t *testing.T) {
	srv, s, wcs, _ := setupGroupConvTopicTest(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "phase2-agent-delete", Slug: "phase2-agent-delete"}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "phase2-delete-agent",
		Slug:       "phase2-delete-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := api.NewUUID()
	convID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "phase2-delete-thread",
		DefaultAgent:   agent.Slug,
		ConversationID: convID, CreatedBy: "dev", CreatedAt: time.Now().UTC(),
	}))

	// Simulate a topic whose default agent was already converged onto its
	// conversation (e.g. by a Phase-1/2 writer) before deletion.
	agentID := agent.ID
	require.NoError(t, wcs.UpdateTopic(ctx, topicID, TopicUpdate{DefaultAgentID: &agentID}))

	conv, err := s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.NotNil(t, conv.DefaultAgentID, "sanity: conversation default agent is set before delete")

	srv.ClearTopicDefaultAgent(ctx, agent.ID, agent.Slug, project.ID)

	topic, err := wcs.GetTopic(ctx, topicID)
	require.NoError(t, err)
	require.Empty(t, topic.DefaultAgent, "AC-5: topic default agent must be cleared")

	conv, err = s.GetConversation(ctx, convID)
	require.NoError(t, err)
	require.Nil(t, conv.DefaultAgentID, "AC-5: deleting an agent must also clear the conversation's default_agent_id")
}

func TestIsTopicNameConflict(t *testing.T) {
	require.False(t, isTopicNameConflict(nil))
	require.True(t, isTopicNameConflict(errors.New("name conflict: pstack2")))

	// SQLite (modernc.org/sqlite, the hub's actual driver) reports column
	// names, not the index name, even for the named expression index
	// idx_webchat_topic_project_name.
	require.True(t, isTopicNameConflict(errors.New(
		"webchat store: create topic: constraint failed: UNIQUE constraint failed: webchat_topic.project_id, webchat_topic.name (2067)")))

	// Postgres (pgx, the hub's actual Postgres driver) reports the
	// constraint/index name verbatim.
	require.True(t, isTopicNameConflict(errors.New(
		`webchat store: create topic: ERROR: duplicate key value violates unique constraint "idx_webchat_topic_project_name" (SQLSTATE 23505)`)))

	// Not a name conflict: a *different* webchat_topic unique index (the
	// one-#general-per-project guard) hitting the same "UNIQUE constraint
	// failed" shape.
	require.False(t, isTopicNameConflict(errors.New(
		"UNIQUE constraint failed: webchat_topic.project_id")))

	// Not a name conflict: the UNRELATED conversations(surface, external_ref)
	// partial unique index. CreateTopic/UpdateTopic/PromoteDM write to both
	// webchat_topic and conversations in the same transaction, so a race on
	// the external_ref index (DEF-156) can produce this same "unique
	// violation" shape — a bare "unique"/"duplicate key" substring match
	// would misreport it as NAME_CONFLICT.
	require.False(t, isTopicNameConflict(errors.New(
		"UNIQUE constraint failed: conversations.surface, conversations.external_ref")))
	require.False(t, isTopicNameConflict(errors.New(
		`ERROR: duplicate key value violates unique constraint "conversation_surface_external_ref" (SQLSTATE 23505)`)))

	require.False(t, isTopicNameConflict(errors.New("some other failure")))
}
