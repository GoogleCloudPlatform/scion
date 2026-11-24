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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inboxChatTestHelper struct {
	t   *testing.T
	srv *Server
	s   store.Store
	wcs WebChatStore
}

func (h *inboxChatTestHelper) decodeSummary(rec *httptest.ResponseRecorder) InboxSummaryResponse {
	h.t.Helper()
	require.Equal(h.t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out InboxSummaryResponse
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

func (h *inboxChatTestHelper) decodeChatPreviews(cat InboxCategory) []InboxChatPreview {
	h.t.Helper()
	out := make([]InboxChatPreview, 0, len(cat.Previews))
	for _, raw := range cat.Previews {
		b, err := json.Marshal(raw)
		require.NoError(h.t, err)
		var p InboxChatPreview
		require.NoError(h.t, json.Unmarshal(b, &p))
		out = append(out, p)
	}
	return out
}

func (h *inboxChatTestHelper) decodeUnreadCount(rec *httptest.ResponseRecorder) chatUnreadCountResponse {
	h.t.Helper()
	require.Equal(h.t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out chatUnreadCountResponse
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

func (h *inboxChatTestHelper) seedThread(proj *store.Project, name, senderID, body string, at time.Time, mentionUserIDs []string) (topicID, msgID string) {
	h.t.Helper()
	ctx := context.Background()
	topicID = api.NewUUID()
	require.NoError(h.t, h.wcs.CreateTopic(ctx, WebChatTopic{
		ID:             topicID,
		ProjectID:      proj.ID,
		Name:           name,
		CreatedBy:      senderID,
		CreatedAt:      at,
		LastActivityAt: at,
	}))
	msgID = api.NewUUID()
	require.NoError(h.t, h.s.CreateMessage(ctx, &store.Message{
		ID:        msgID,
		ProjectID: proj.ID,
		ThreadID:  topicID,
		Sender:    "user:" + senderID,
		SenderID:  senderID,
		Recipient: "project:" + proj.ID,
		Msg:       body,
		Type:      messages.TypeInstruction,
		CreatedAt: at,
	}))
	require.NoError(h.t, h.wcs.TouchTopicActivity(ctx, topicID, msgID))
	if dbProvider, ok := h.s.(interface{ DB() *sql.DB }); ok {
		_, err := dbProvider.DB().ExecContext(ctx,
			`UPDATE webchat_topic SET last_activity_at = ? WHERE id = ?`,
			at.UTC().Format(time.RFC3339Nano), topicID)
		require.NoError(h.t, err)
	}
	if len(mentionUserIDs) > 0 {
		require.NoError(h.t, h.wcs.RecordMentions(ctx, topicID, msgID, mentionUserIDs))
	}
	return topicID, msgID
}

func (h *inboxChatTestHelper) seedUserDM(projectID, meID, peerID, body string, at time.Time) (dmKey, msgID string) {
	h.t.Helper()
	ctx := context.Background()
	var err error
	dmKey, err = messages.DMConversationKey("user", meID, "user", peerID)
	require.NoError(h.t, err)
	convID := seedConversation(h.t, h.s, "native", dmKey, "direct")
	msgID = api.NewUUID()
	require.NoError(h.t, h.wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   meID,
		PeerID:          peerID,
		PeerKind:        "user",
		LastMessageID:   msgID,
		LastActivityAt:  at,
	}))
	require.NoError(h.t, h.s.CreateMessage(ctx, &store.Message{
		ID:             msgID,
		ProjectID:      projectID,
		ThreadID:       dmKey,
		ConversationID: convID,
		Channel:        "web",
		Sender:         "user:" + peerID,
		SenderID:       peerID,
		Recipient:      "user:" + meID,
		RecipientID:    meID,
		Msg:            body,
		Type:           messages.TypeInstruction,
		CreatedAt:      at,
	}))
	return dmKey, msgID
}

func (h *inboxChatTestHelper) seedAgentDM(meID string, agent *store.Agent, body string, at time.Time) (dmKey, msgID string) {
	h.t.Helper()
	ctx := context.Background()
	var err error
	dmKey, err = messages.DMConversationKey("user", meID, "agent", agent.ID)
	require.NoError(h.t, err)
	convID := seedConversation(h.t, h.s, "native", dmKey, "direct")
	msgID = api.NewUUID()
	require.NoError(h.t, h.wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   meID,
		PeerID:          agent.ID,
		PeerKind:        "agent",
		LastMessageID:   msgID,
		LastActivityAt:  at,
	}))
	require.NoError(h.t, h.s.CreateMessage(ctx, &store.Message{
		ID:             msgID,
		ProjectID:      agent.ProjectID,
		ThreadID:       dmKey,
		ConversationID: convID,
		Channel:        "web",
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      "user:" + meID,
		RecipientID:    meID,
		Msg:            body,
		Type:           messages.TypeInstruction,
		CreatedAt:      at,
	}))
	return dmKey, msgID
}

// TestUserMeInboxSummary_ChatCountsFiltersOrderingAndPrivacy verifies AC3 & AC5:
// - chat.count == chat.breakdown.unreadConversations == /chat/unread-count conversations
// - breakdown.dms and breakdown.mentions match fixture counts
// - muted threads, read threads, non-member projects, and deleted-agent DMs are excluded
// - chat=all|dms|mentions filters previews while keeping exact breakdown counts
// - mention wins over thread; newest message first; previews cap respected; no message bodies leaked
func TestUserMeInboxSummary_ChatCountsFiltersOrderingAndPrivacy(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	me := &store.User{
		ID:          api.NewUUID(),
		Email:       "me@example.com",
		DisplayName: "Me User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	peer := &store.User{
		ID:          api.NewUUID(),
		Email:       "alice@example.com",
		DisplayName: "Alice Example",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, me))
	require.NoError(t, s.CreateUser(ctx, peer))
	bindProjectMember(t, s, proj.ID, me.ID)

	// A second project the caller is NOT a member of (must not count).
	nonMemberProj := &store.Project{
		ID:      api.NewUUID(),
		Name:    "Other Project",
		Slug:    "other-project",
		Created: time.Now().UTC(),
		Updated: time.Now().UTC(),
	}
	require.NoError(t, s.CreateProject(ctx, nonMemberProj))

	liveAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Deploy Bot",
		Slug:      "deploy-bot",
		ProjectID: proj.ID,
		Phase:     "running",
		Created:   time.Now().UTC(),
		Updated:   time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(ctx, liveAgent))

	deletedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Ghost Bot",
		Slug:      "ghost-bot",
		ProjectID: proj.ID,
		Phase:     "stopped",
		Created:   time.Now().UTC(),
		Updated:   time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(ctx, deletedAgent))
	require.NoError(t, s.DeleteAgent(ctx, deletedAgent.ID))

	h := &inboxChatTestHelper{t: t, srv: srv, s: s, wcs: wcs}
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)

	// 1. Unread plain thread (oldest: base + 1m).
	plainTopic, _ := h.seedThread(proj, "release-notes", peer.ID, "SECRET_BODY_PLAIN_THREAD", base.Add(1*time.Minute), nil)
	// 2. Unread mention thread (base + 2m) — kind must be "mention" (wins over "thread").
	mentionTopic, _ := h.seedThread(proj, "incident-room", peer.ID, "SECRET_BODY_MENTION_THREAD", base.Add(2*time.Minute), []string{me.ID})
	// 3. Unread user DM (base + 3m).
	userDMKey, _ := h.seedUserDM(proj.ID, me.ID, peer.ID, "SECRET_BODY_USER_DM", base.Add(3*time.Minute))
	// 4. Unread agent DM (newest: base + 4m).
	agentDMKey, _ := h.seedAgentDM(me.ID, liveAgent, "SECRET_BODY_AGENT_DM", base.Add(4*time.Minute))

	// Excluded items:
	// - Muted unread thread with mention.
	mutedTopic, _ := h.seedThread(proj, "muted-mentions", peer.ID, "SECRET_BODY_MUTED", base.Add(5*time.Minute), []string{me.ID})
	require.NoError(t, wcs.SetMuted(ctx, me.ID, mutedTopic, true))
	// - Read thread with mention.
	readTopic, readMsg := h.seedThread(proj, "already-read", peer.ID, "SECRET_BODY_READ", base.Add(6*time.Minute), []string{me.ID})
	require.NoError(t, wcs.SetReadState(ctx, me.ID, readTopic, readMsg))
	// - Unread thread in non-member project.
	h.seedThread(nonMemberProj, "secret-other-proj", peer.ID, "SECRET_BODY_NON_MEMBER", base.Add(7*time.Minute), []string{me.ID})
	// - Unread DM with soft-deleted agent.
	h.seedAgentDM(me.ID, deletedAgent, "SECRET_BODY_DELETED_AGENT", base.Add(8*time.Minute))

	// Compare against GET /api/v1/chat/unread-count.
	unreadRec := doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/chat/unread-count", nil)
	unreadResp := h.decodeUnreadCount(unreadRec)
	require.Equal(t, chatUnreadCountResponse{Conversations: 4, Threads: 2, DMs: 2}, unreadResp)

	// Default chat=all with previews=10.
	rec := doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=10", nil)
	resp := h.decodeSummary(rec)
	assert.Equal(t, 4, resp.Total)

	chatCat := resp.Categories[InboxCategoryChat]
	assert.Equal(t, InboxStateOK, chatCat.State)
	assert.Equal(t, unreadResp.Conversations, chatCat.Count)
	assert.False(t, chatCat.Capped)
	assert.True(t, chatCat.CountsTowardTotal)
	assert.Equal(t, "/chat", chatCat.Href)
	assert.Equal(t, map[string]int{
		"dms":                 2,
		"mentions":            1,
		"unreadConversations": 4,
	}, chatCat.Breakdown)

	// No message bodies anywhere in the response.
	assert.NotContains(t, rec.Body.String(), "SECRET_BODY_")

	// Verify newest-first ordering and preview shapes.
	previews := h.decodeChatPreviews(chatCat)
	require.Len(t, previews, 4)

	assert.Equal(t, InboxChatKindDM, previews[0].Kind)
	assert.Equal(t, agentDMKey, previews[0].ConversationKey)
	assert.Equal(t, "Deploy Bot", previews[0].Label)
	assert.Equal(t, proj.ID, previews[0].ProjectID)
	assert.Equal(t, proj.Name, previews[0].ProjectName)
	assert.Equal(t, "/chat/dm/"+url.PathEscape(agentDMKey), previews[0].Href)

	assert.Equal(t, InboxChatKindDM, previews[1].Kind)
	assert.Equal(t, userDMKey, previews[1].ConversationKey)
	assert.Equal(t, "Alice Example", previews[1].Label)
	assert.Empty(t, previews[1].ProjectID)
	assert.Empty(t, previews[1].ProjectName)
	assert.Equal(t, "/chat/dm/"+url.PathEscape(userDMKey), previews[1].Href)

	assert.Equal(t, InboxChatKindMention, previews[2].Kind)
	assert.Equal(t, mentionTopic, previews[2].ConversationKey)
	assert.Equal(t, "incident-room", previews[2].Label)
	assert.Equal(t, proj.ID, previews[2].ProjectID)
	assert.Equal(t, proj.Name, previews[2].ProjectName)
	assert.Equal(t, "/chat/"+url.PathEscape(proj.Slug)+"/"+url.PathEscape(mentionTopic), previews[2].Href)

	assert.Equal(t, InboxChatKindThread, previews[3].Kind)
	assert.Equal(t, plainTopic, previews[3].ConversationKey)
	assert.Equal(t, "release-notes", previews[3].Label)
	assert.Equal(t, proj.ID, previews[3].ProjectID)
	assert.Equal(t, proj.Name, previews[3].ProjectName)
	assert.Equal(t, "/chat/"+url.PathEscape(proj.Slug)+"/"+url.PathEscape(plainTopic), previews[3].Href)

	// previews=2 truncates to the 2 newest while keeping exact counts.
	capResp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=2", nil))
	capPreviews := h.decodeChatPreviews(capResp.Categories[InboxCategoryChat])
	require.Len(t, capPreviews, 2)
	assert.Equal(t, agentDMKey, capPreviews[0].ConversationKey)
	assert.Equal(t, userDMKey, capPreviews[1].ConversationKey)
	assert.Equal(t, 4, capResp.Categories[InboxCategoryChat].Count)

	// previews=0 returns empty previews with exact counts and breakdown.
	zeroResp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=0", nil))
	assert.Empty(t, zeroResp.Categories[InboxCategoryChat].Previews)
	assert.Equal(t, 4, zeroResp.Categories[InboxCategoryChat].Count)
	assert.Equal(t, map[string]int{"dms": 2, "mentions": 1, "unreadConversations": 4}, zeroResp.Categories[InboxCategoryChat].Breakdown)

	// chat=dms returns only DM previews; breakdown still has all three counts.
	dmsResp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?chat=dms&previews=10", nil))
	dmsPreviews := h.decodeChatPreviews(dmsResp.Categories[InboxCategoryChat])
	require.Len(t, dmsPreviews, 2)
	for _, p := range dmsPreviews {
		assert.Equal(t, InboxChatKindDM, p.Kind)
	}
	assert.Equal(t, map[string]int{"dms": 2, "mentions": 1, "unreadConversations": 4}, dmsResp.Categories[InboxCategoryChat].Breakdown)

	// chat=mentions returns only mention previews; breakdown still has all three counts.
	mentionsResp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?chat=mentions&previews=10", nil))
	mentionPreviews := h.decodeChatPreviews(mentionsResp.Categories[InboxCategoryChat])
	require.Len(t, mentionPreviews, 1)
	assert.Equal(t, InboxChatKindMention, mentionPreviews[0].Kind)
	assert.Equal(t, mentionTopic, mentionPreviews[0].ConversationKey)
	assert.Equal(t, map[string]int{"dms": 2, "mentions": 1, "unreadConversations": 4}, mentionsResp.Categories[InboxCategoryChat].Breakdown)
}

// TestUserMeInboxSummary_ChatTokenScoping verifies §4.2: a project-bounded UAT
// sees only threads, mentions, and agent DMs in its boundary project, and agrees
// with GET /api/v1/chat/unread-count for the same token.
func TestUserMeInboxSummary_ChatTokenScoping(t *testing.T) {
	f := newChatFixture(t)
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	projA, err := f.s.GetProject(ctx, f.projA)
	require.NoError(t, err)
	projB, err := f.s.GetProject(ctx, f.projB)
	require.NoError(t, err)
	bindProjectMember(t, f.s, f.projB, f.member)

	h := &inboxChatTestHelper{t: t, srv: f.srv, s: f.s, wcs: f.wcs}
	base := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Minute)

	otherUserID := mustCreateActiveUser(t, f.s, "inbox-chat-tok-peer")
	topicA, _ := h.seedThread(projA, "projA-mention", otherUserID, "body-a", base.Add(1*time.Minute), []string{f.member})
	h.seedThread(projB, "projB-mention", otherUserID, "body-b", base.Add(2*time.Minute), []string{f.member})
	agentDM_A, _ := h.seedAgentDM(f.member, f.agentA, "dm-a", base.Add(3*time.Minute))
	h.seedAgentDM(f.member, f.agentB, "dm-b", base.Add(4*time.Minute))
	h.seedUserDM(projA.ID, f.member, otherUserID, "dm-user", base.Add(5*time.Minute))

	// Hub-bounded UAT with inbox:read + project:read + agent:read sees everything.
	tokHub := f.mint(t, f.member, hubBoundary(), "inbox:read", "project:read", "agent:read")
	hubUnread := h.decodeUnreadCount(f.call(t, tokHub, http.MethodGet, "/api/v1/chat/unread-count", nil))
	hubSummary := h.decodeSummary(f.call(t, tokHub, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=10", nil))
	assert.Equal(t, InboxStateUnavailable, hubSummary.Categories[InboxCategoryApprovals].State)
	assert.Equal(t, InboxStateOK, hubSummary.Categories[InboxCategoryChat].State)
	assert.Equal(t, hubUnread.Conversations, hubSummary.Categories[InboxCategoryChat].Count)
	assert.Equal(t, 5, hubSummary.Categories[InboxCategoryChat].Count)
	assert.Equal(t, map[string]int{"dms": 3, "mentions": 2, "unreadConversations": 5}, hubSummary.Categories[InboxCategoryChat].Breakdown)
	assert.Equal(t, 5, hubSummary.Total)

	// Project-bounded UAT for projA sees only projA's thread/mention and projA's agent DM.
	tokProjA := f.mint(t, f.member, projectBoundary(f.projA), "inbox:read", "project:read", "agent:read")
	projUnread := h.decodeUnreadCount(f.call(t, tokProjA, http.MethodGet, "/api/v1/chat/unread-count", nil))
	projSummary := h.decodeSummary(f.call(t, tokProjA, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=10", nil))
	assert.Equal(t, projUnread.Conversations, projSummary.Categories[InboxCategoryChat].Count)
	assert.Equal(t, 2, projSummary.Categories[InboxCategoryChat].Count)
	assert.Equal(t, map[string]int{"dms": 1, "mentions": 1, "unreadConversations": 2}, projSummary.Categories[InboxCategoryChat].Breakdown)
	assert.Equal(t, 2, projSummary.Total)

	projPreviews := h.decodeChatPreviews(projSummary.Categories[InboxCategoryChat])
	require.Len(t, projPreviews, 2)
	keys := []string{projPreviews[0].ConversationKey, projPreviews[1].ConversationKey}
	assert.ElementsMatch(t, []string{topicA, agentDM_A}, keys)
}

type inboxChatFaultWebChatStore struct {
	WebChatStore
	mentionErr error
}

func (s *inboxChatFaultWebChatStore) UnreadMentionKeys(ctx context.Context, userID string, conversationKeys []string) (map[string]bool, error) {
	if s.mentionErr != nil {
		return nil, s.mentionErr
	}
	return s.WebChatStore.UnreadMentionKeys(ctx, userID, conversationKeys)
}

// TestUserMeInboxSummary_ChatDisabledAndErrorStates verifies AC4:
// - nativeChatEnabled == false -> state:"disabled", count:0, excluded from total
// - webChatStore == nil -> state:"disabled", count:0, excluded from total
// - UnreadMentionKeys error -> state:"error" with "internal" or "timeout", HTTP 200, other categories intact
func TestUserMeInboxSummary_ChatDisabledAndErrorStates(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	me := &store.User{
		ID:          api.NewUUID(),
		Email:       "err-user@example.com",
		DisplayName: "Err User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, me))
	bindProjectMember(t, s, proj.ID, me.ID)

	h := &inboxChatTestHelper{t: t, srv: srv, s: s, wcs: wcs}
	h.seedThread(proj, "t1", api.NewUUID(), "hello", time.Now().UTC(), []string{me.ID})

	// 1. Native chat disabled via config -> state:"disabled".
	disabled := false
	srv.config.NativeChatEnabled = &disabled
	resp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary", nil))
	assert.Equal(t, InboxStateDisabled, resp.Categories[InboxCategoryChat].State)
	assert.Equal(t, 0, resp.Categories[InboxCategoryChat].Count)
	assert.Empty(t, resp.Categories[InboxCategoryChat].Previews)
	assert.Equal(t, 0, resp.Total)
	srv.config.NativeChatEnabled = nil

	// 2. webChatStore == nil -> state:"disabled".
	srv.SetWebChatStore(nil)
	resp = h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary", nil))
	assert.Equal(t, InboxStateDisabled, resp.Categories[InboxCategoryChat].State)
	assert.Equal(t, 0, resp.Categories[InboxCategoryChat].Count)
	assert.Equal(t, 0, resp.Total)

	// 3. UnreadMentionKeys internal error -> state:"error", errorCode:"internal", HTTP 200.
	faultWCS := &inboxChatFaultWebChatStore{WebChatStore: wcs, mentionErr: errors.New("db hiccup")}
	srv.SetWebChatStore(faultWCS)
	resp = h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary", nil))
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryChat].State)
	assert.Equal(t, InboxErrorInternal, resp.Categories[InboxCategoryChat].ErrorCode)
	assert.Equal(t, 0, resp.Categories[InboxCategoryChat].Count)
	assert.Equal(t, InboxStateOK, resp.Categories[InboxCategoryApprovals].State)

	// 4. UnreadMentionKeys timeout -> state:"error", errorCode:"timeout", HTTP 200.
	faultWCS.mentionErr = context.DeadlineExceeded
	resp = h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary", nil))
	assert.Equal(t, InboxStateError, resp.Categories[InboxCategoryChat].State)
	assert.Equal(t, InboxErrorTimeout, resp.Categories[InboxCategoryChat].ErrorCode)
}

type inboxChatCountingWebChatStore struct {
	WebChatStore
	listTopicsCalls atomic.Int32
	readStatesCalls atomic.Int32
	listDMsCalls    atomic.Int32
	unreadMentions  atomic.Int32
}

func (s *inboxChatCountingWebChatStore) ListTopicsByProjects(ctx context.Context, projectIDs []string) ([]WebChatTopic, error) {
	s.listTopicsCalls.Add(1)
	return s.WebChatStore.ListTopicsByProjects(ctx, projectIDs)
}

func (s *inboxChatCountingWebChatStore) GetReadStates(ctx context.Context, userID string, keys []string) ([]WebChatReadState, error) {
	s.readStatesCalls.Add(1)
	return s.WebChatStore.GetReadStates(ctx, userID, keys)
}

func (s *inboxChatCountingWebChatStore) ListDMs(ctx context.Context, userID string) ([]WebChatDM, error) {
	s.listDMsCalls.Add(1)
	return s.WebChatStore.ListDMs(ctx, userID)
}

func (s *inboxChatCountingWebChatStore) UnreadMentionKeys(ctx context.Context, userID string, conversationKeys []string) (map[string]bool, error) {
	s.unreadMentions.Add(1)
	return s.WebChatStore.UnreadMentionKeys(ctx, userID, conversationKeys)
}

// TestUserMeInboxSummary_ChatStoreCallCount verifies AC6: regardless of how
// many unread threads and DMs exist across projects, the webchat store calls
// remain a small constant with exactly one batched UnreadMentionKeys call.
func TestUserMeInboxSummary_ChatStoreCallCount(t *testing.T) {
	srv, s, wcs, proj := setupSharedChatTest(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.InboxDashboard: true})
	ctx := context.Background()

	spy := &inboxChatCountingWebChatStore{WebChatStore: wcs}
	srv.SetWebChatStore(spy)

	me := &store.User{
		ID:          api.NewUUID(),
		Email:       "spy-chat@example.com",
		DisplayName: "Spy Chat",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, me))
	bindProjectMember(t, s, proj.ID, me.ID)

	h := &inboxChatTestHelper{t: t, srv: srv, s: s, wcs: wcs}
	now := time.Now().UTC().Truncate(time.Second)

	// Seed 5 unread threads across 2 projects + 4 unread DMs.
	proj2 := &store.Project{
		ID:      api.NewUUID(),
		Name:    "Second Project",
		Slug:    "second-project",
		Created: now,
		Updated: now,
	}
	require.NoError(t, s.CreateProject(ctx, proj2))
	bindProjectMember(t, s, proj2.ID, me.ID)

	for i := 0; i < 3; i++ {
		h.seedThread(proj, fmt.Sprintf("p1-t%d", i), api.NewUUID(), "msg", now.Add(time.Duration(i)*time.Second), []string{me.ID})
	}
	for i := 0; i < 2; i++ {
		h.seedThread(proj2, fmt.Sprintf("p2-t%d", i), api.NewUUID(), "msg", now.Add(time.Duration(10+i)*time.Second), nil)
	}
	for i := 0; i < 4; i++ {
		peer := &store.User{
			ID:          api.NewUUID(),
			Email:       fmt.Sprintf("peer%d@example.com", i),
			DisplayName: fmt.Sprintf("Peer %d", i),
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     now,
		}
		require.NoError(t, s.CreateUser(ctx, peer))
		h.seedUserDM(proj.ID, me.ID, peer.ID, "dm", now.Add(time.Duration(20+i)*time.Second))
	}

	spy.listTopicsCalls.Store(0)
	spy.readStatesCalls.Store(0)
	spy.listDMsCalls.Store(0)
	spy.unreadMentions.Store(0)

	resp := h.decodeSummary(doRequestAsUser(t, srv, me, http.MethodGet, "/api/v1/users/me/inbox/summary?previews=3", nil))
	assert.Equal(t, 9, resp.Categories[InboxCategoryChat].Count)
	assert.Equal(t, 3, resp.Categories[InboxCategoryChat].Breakdown["mentions"])
	assert.Equal(t, 4, resp.Categories[InboxCategoryChat].Breakdown["dms"])

	assert.Equal(t, int32(1), spy.listTopicsCalls.Load(), "topics must be read in one batched call")
	assert.Equal(t, int32(2), spy.readStatesCalls.Load(), "read states must be read in two batched calls (threads + DMs)")
	assert.Equal(t, int32(1), spy.listDMsCalls.Load(), "DMs must be read in one call")
	assert.Equal(t, int32(1), spy.unreadMentions.Load(), "UnreadMentionKeys must be called exactly once")
}
