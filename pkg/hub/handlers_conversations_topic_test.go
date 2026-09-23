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

func TestIsTopicNameConflict(t *testing.T) {
	require.False(t, isTopicNameConflict(nil))
	require.True(t, isTopicNameConflict(errors.New("name conflict: pstack2")))

	// SQLite (mattn/go-sqlite3) reports column names, not the index name,
	// even for the named expression index idx_webchat_topic_project_name.
	require.True(t, isTopicNameConflict(errors.New(
		"webchat store: create topic: UNIQUE constraint failed: webchat_topic.project_id, webchat_topic.name")))

	// Postgres reports the constraint/index name verbatim.
	require.True(t, isTopicNameConflict(errors.New(
		`webchat store: create topic: pq: duplicate key value violates unique constraint "idx_webchat_topic_project_name"`)))

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
		`pq: duplicate key value violates unique constraint "conversation_surface_external_ref"`)))

	require.False(t, isTopicNameConflict(errors.New("some other failure")))
}
