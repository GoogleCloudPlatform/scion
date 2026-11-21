// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type interagentCountingStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	calls map[string]int
}

func (s *interagentCountingStore) inc(method string) {
	if !s.fault.Active() {
		return
	}
	s.mu.Lock()
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	s.calls[method]++
	s.mu.Unlock()
}

func (s *interagentCountingStore) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *interagentCountingStore) reset() {
	s.mu.Lock()
	s.calls = make(map[string]int)
	s.mu.Unlock()
}

func (s *interagentCountingStore) ListParticipants(ctx context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	s.inc("ListParticipants")
	return s.Store.ListParticipants(ctx, conversationID)
}

func (s *interagentCountingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.inc("GetAgent")
	return s.Store.GetAgent(ctx, id)
}

func (s *interagentCountingStore) ListMessages(ctx context.Context, f store.MessageFilter, o store.ListOptions) (*store.ListResult[store.Message], error) {
	s.inc("ListMessages")
	return s.Store.ListMessages(ctx, f, o)
}

func (s *interagentCountingStore) GetMessage(ctx context.Context, id string) (*store.Message, error) {
	s.inc("GetMessage")
	return s.Store.GetMessage(ctx, id)
}

func TestInteragent_CrossProjectCheckOncePerConversation(t *testing.T) {
	srv, s, counter, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *interagentCountingStore {
		return &interagentCountingStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	projA := &store.Project{
		ID:        tid("ia-perf-proj-a"),
		Name:      "ia-perf-proj-a",
		Slug:      "ia-perf-proj-a",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, projA))

	projB := &store.Project{
		ID:   tid("ia-perf-proj-b"),
		Name: "ia-perf-proj-b",
		Slug: "ia-perf-proj-b",
	}
	require.NoError(t, s.CreateProject(ctx, projB))

	makeAgent := func(slug, projectID string) *store.Agent {
		a := &store.Agent{
			ID:          tid("ia-perf-" + slug),
			Name:        slug,
			Slug:        slug,
			ProjectID:   projectID,
			Phase:       "running",
			MessageMode: store.MessageModeHub,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}

	dmAgent := makeAgent("dm-agent", projA.ID)
	peers := []*store.Agent{
		makeAgent("peer-1", projA.ID),
		makeAgent("peer-2", projA.ID),
		makeAgent("peer-3", projA.ID),
	}

	convs := make([]string, len(peers))
	for i, peer := range peers {
		convKey, err := messages.DMConversationKey("agent", dmAgent.ID, "agent", peer.ID)
		require.NoError(t, err)
		conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
			Kind:        "direct",
			Surface:     "native",
			ExternalRef: convKey,
			DriftState:  "active",
		})
		require.NoError(t, err)
		for _, agID := range []string{dmAgent.ID, peer.ID} {
			require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
				ConversationID: conv.ID,
				PrincipalKind:  "agent",
				PrincipalID:    agID,
				Role:           "member",
			}))
		}
		convs[i] = conv.ID
	}

	baseTime := time.Now().Add(-10 * time.Minute)
	senderProj := projA.ID
	for i := 0; i < 200; i++ {
		peer := peers[i%len(peers)]
		convID := convs[i%len(convs)]
		recipientProj := peer.ProjectID
		msg := &store.Message{
			ID:                 tid(fmt.Sprintf("ia-perf-msg-%03d", i)),
			ProjectID:          projA.ID,
			Sender:             "agent:" + dmAgent.Slug,
			SenderID:           dmAgent.ID,
			Recipient:          "agent:" + peer.Slug,
			RecipientID:        peer.ID,
			Msg:                fmt.Sprintf("message-%03d", i),
			Type:               "instruction",
			AgentID:            dmAgent.ID,
			ConversationID:     convID,
			SenderProjectID:    &senderProj,
			RecipientProjectID: &recipientProj,
			CreatedAt:          baseTime.Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, s.CreateMessage(ctx, msg))
	}

	enableCPM(t, srv, s)

	dmKey, err := messages.DMConversationKey("user", DevUserID, "agent", dmAgent.ID)
	require.NoError(t, err)
	endpoint := "/api/v1/chat/conversations/" + url.PathEscape(dmKey) + "/interagent?limit=200"

	fault.Arm()
	counter.reset()

	rr := doRequest(t, srv, http.MethodGet, endpoint, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var resp interagentResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 200)

	assert.LessOrEqual(t, counter.count("ListParticipants"), 3,
		"expected at most 1 ListParticipants call per distinct conversation (3), got %d",
		counter.count("ListParticipants"))
	// 1 GetAgent for dmAgent + 2 per distinct 2-agent conversation (6) = 7.
	assert.LessOrEqual(t, counter.count("GetAgent"), 1+len(convs)*2,
		"expected GetAgent calls bounded by distinct conversations, got %d",
		counter.count("GetAgent"))

	// Also verify a cross-project non-participant conversation strips bodies
	// across repeated messages while still checking participation only once.
	foreignPeer := makeAgent("foreign-peer", projB.ID)
	foreignKey, err := messages.DMConversationKey("agent", dmAgent.ID, "agent", foreignPeer.ID)
	require.NoError(t, err)
	foreignConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: foreignKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	for _, agID := range []string{dmAgent.ID, foreignPeer.ID} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: foreignConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    agID,
			Role:           "member",
		}))
	}
	foreignProj := projB.ID
	for i := 0; i < 10; i++ {
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID:                 tid(fmt.Sprintf("ia-perf-foreign-%02d", i)),
			ProjectID:          projB.ID,
			Sender:             "agent:" + dmAgent.Slug,
			SenderID:           dmAgent.ID,
			Recipient:          "agent:" + foreignPeer.Slug,
			RecipientID:        foreignPeer.ID,
			Msg:                "secret cross-project body",
			Type:               "instruction",
			AgentID:            dmAgent.ID,
			ConversationID:     foreignConv.ID,
			SenderProjectID:    &senderProj,
			RecipientProjectID: &foreignProj,
			CreatedAt:          baseTime.Add(time.Duration(300+i) * time.Second),
		}))
	}

	counter.reset()
	rr = doRequest(t, srv, http.MethodGet, endpoint, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 200)

	// 3 same-project conversations (1 ListParticipants each) + 1 cross-project
	// conversation (2 ListParticipants inside AuthorizeCrossProjectContentAccess) = 5.
	assert.LessOrEqual(t, counter.count("ListParticipants"), 5,
		"expected cross-project conversation decision to be memoized across all 10 messages")
	strippedCount := 0
	for _, m := range resp.Messages {
		if strings.HasSuffix(m.Recipient, "foreign-peer") {
			strippedCount++
			assert.Empty(t, m.Msg, "cross-project body must remain stripped")
		}
	}
	assert.Equal(t, 10, strippedCount)
}

func TestInteragent_MergedResultSortedAndCapped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID:        tid("ia-merge-proj"),
		Name:      "ia-merge-proj",
		Slug:      "ia-merge-proj",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	dmAgent := &store.Agent{
		ID:        tid("ia-merge-agent"),
		Name:      "merge-agent",
		Slug:      "merge-agent",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, dmAgent))

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// Seed 6 modern messages (SenderID set -> matched by Query 1) at even minutes
	// and 6 legacy sender-only messages (SenderID empty -> matched ONLY by Query 2)
	// at odd minutes, plus a same-timestamp pair to exercise ID tie-breaking.
	for i := 0; i < 6; i++ {
		evenTS := base.Add(time.Duration(2*i) * time.Minute)
		oddTS := base.Add(time.Duration(2*i+1) * time.Minute)

		modern := &store.Message{
			ID:          tid(fmt.Sprintf("ia-merge-modern-%d", i)),
			ProjectID:   proj.ID,
			Sender:      "agent:" + dmAgent.Slug,
			SenderID:    dmAgent.ID,
			Recipient:   "agent:peer",
			RecipientID: tid("ia-merge-peer"),
			Msg:         fmt.Sprintf("modern-%d", i),
			Type:        "instruction",
			CreatedAt:   evenTS,
		}
		require.NoError(t, s.CreateMessage(ctx, modern))

		legacy := &store.Message{
			ID:        tid(fmt.Sprintf("ia-merge-legacy-%d", i)),
			ProjectID: proj.ID,
			Sender:    "agent:" + dmAgent.Slug,
			SenderID:  "", // legacy: no SenderID
			Recipient: "agent:peer",
			Msg:       fmt.Sprintf("legacy-%d", i),
			Type:      "instruction",
			CreatedAt: oddTS,
		}
		require.NoError(t, s.CreateMessage(ctx, legacy))
	}

	// Add two tie-breaker messages at the newest timestamp: one modern, one legacy.
	tieTS := base.Add(30 * time.Minute)
	tieLowID := "00000000-0000-4000-8000-000000000001"
	tieHighID := "00000000-0000-4000-8000-000000000002"
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID:        tieLowID,
		ProjectID: proj.ID,
		Sender:    "agent:" + dmAgent.Slug,
		SenderID:  dmAgent.ID,
		Recipient: "agent:peer",
		Msg:       "tie-low",
		Type:      "instruction",
		CreatedAt: tieTS,
	}))
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID:        tieHighID,
		ProjectID: proj.ID,
		Sender:    "agent:" + dmAgent.Slug,
		SenderID:  "", // legacy
		Recipient: "agent:peer",
		Msg:       "tie-high",
		Type:      "instruction",
		CreatedAt: tieTS,
	}))

	dmKey, err := messages.DMConversationKey("user", DevUserID, "agent", dmAgent.ID)
	require.NoError(t, err)
	endpoint := "/api/v1/chat/conversations/" + url.PathEscape(dmKey) + "/interagent?limit=8"

	rr := doRequest(t, srv, http.MethodGet, endpoint, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var resp interagentResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 8, "merged result must be capped to limit=8")

	// First two must be the tie-breaker pair in ID DESC order (tieHighID then tieLowID).
	assert.Equal(t, tieHighID, resp.Messages[0].ID)
	assert.Equal(t, tieLowID, resp.Messages[1].ID)

	// Remaining rows must be strictly non-increasing by (CreatedAt DESC, ID DESC).
	for i := 1; i < len(resp.Messages); i++ {
		prev := resp.Messages[i-1]
		curr := resp.Messages[i]
		cmp := prev.CreatedAt.Compare(curr.CreatedAt)
		if cmp == 0 {
			assert.Greater(t, prev.ID, curr.ID, "equal timestamps must sort by ID DESC at index %d", i)
		} else {
			assert.Greater(t, cmp, 0, "messages must sort by CreatedAt DESC at index %d: %v vs %v", i, prev.CreatedAt, curr.CreatedAt)
		}
	}
}

func TestInteragent_ListModePreviewTruncated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projA := &store.Project{
		ID:        tid("ia-trunc-proj-a"),
		Name:      "ia-trunc-proj-a",
		Slug:      "ia-trunc-proj-a",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, projA))
	projB := &store.Project{
		ID:   tid("ia-trunc-proj-b"),
		Name: "ia-trunc-proj-b",
		Slug: "ia-trunc-proj-b",
	}
	require.NoError(t, s.CreateProject(ctx, projB))

	dmAgent := &store.Agent{
		ID:          tid("ia-trunc-dm-agent"),
		Name:        "dm-agent",
		Slug:        "dm-agent",
		ProjectID:   projA.ID,
		Phase:       "running",
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, dmAgent))
	peerAgent := &store.Agent{
		ID:          tid("ia-trunc-peer-agent"),
		Name:        "peer-agent",
		Slug:        "peer-agent",
		ProjectID:   projA.ID,
		Phase:       "running",
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, peerAgent))
	foreignAgent := &store.Agent{
		ID:          tid("ia-trunc-foreign-agent"),
		Name:        "foreign-agent",
		Slug:        "foreign-agent",
		ProjectID:   projB.ID,
		Phase:       "running",
		MessageMode: store.MessageModeHub,
	}
	require.NoError(t, s.CreateAgent(ctx, foreignAgent))

	// Build multi-byte UTF-8 bodies: 350 runes (> 300), 300 runes (exact boundary), and short.
	longBody := strings.Repeat("你😀a", 116) + "尾🚀" // 348 + 2 = 350 runes
	require.Equal(t, 350, utf8.RuneCountInString(longBody))
	exactBody := strings.Repeat("あ", 300)
	require.Equal(t, 300, utf8.RuneCountInString(exactBody))
	shortBody := "short preview"

	senderProj := projA.ID
	recipientProj := projA.ID
	foreignProj := projB.ID
	base := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)

	longMsg := &store.Message{
		ID:                 tid("ia-trunc-long"),
		ProjectID:          projA.ID,
		Sender:             "agent:" + dmAgent.Slug,
		SenderID:           dmAgent.ID,
		Recipient:          "agent:" + peerAgent.Slug,
		RecipientID:        peerAgent.ID,
		Msg:                longBody,
		Type:               "instruction",
		AgentID:            dmAgent.ID,
		ThreadID:           "thread-unused",
		Channel:            "web",
		DispatchState:      "dispatched",
		SenderProjectID:    &senderProj,
		RecipientProjectID: &recipientProj,
		CreatedAt:          base.Add(3 * time.Minute),
	}
	exactMsg := &store.Message{
		ID:                 tid("ia-trunc-exact"),
		ProjectID:          projA.ID,
		Sender:             "agent:" + dmAgent.Slug,
		SenderID:           dmAgent.ID,
		Recipient:          "agent:" + peerAgent.Slug,
		RecipientID:        peerAgent.ID,
		Msg:                exactBody,
		Type:               "instruction",
		AgentID:            dmAgent.ID,
		SenderProjectID:    &senderProj,
		RecipientProjectID: &recipientProj,
		CreatedAt:          base.Add(2 * time.Minute),
	}
	shortMsg := &store.Message{
		ID:        tid("ia-trunc-short"),
		ProjectID: projA.ID,
		Sender:    "agent:" + dmAgent.Slug,
		SenderID:  dmAgent.ID,
		Recipient: "agent:" + peerAgent.Slug,
		Msg:       shortBody,
		Type:      "instruction",
		CreatedAt: base.Add(1 * time.Minute),
	}
	// Cross-project row with no ConversationID -> body stripped; must stay "" and msgTruncated=false.
	strippedLongMsg := &store.Message{
		ID:                 tid("ia-trunc-stripped"),
		ProjectID:          projB.ID,
		Sender:             "agent:" + dmAgent.Slug,
		SenderID:           dmAgent.ID,
		Recipient:          "agent:" + foreignAgent.Slug,
		RecipientID:        foreignAgent.ID,
		Msg:                longBody,
		Type:               "instruction",
		SenderProjectID:    &senderProj,
		RecipientProjectID: &foreignProj,
		CreatedAt:          base,
	}
	for _, m := range []*store.Message{longMsg, exactMsg, shortMsg, strippedLongMsg} {
		require.NoError(t, s.CreateMessage(ctx, m))
	}

	enableCPM(t, srv, s)

	dmKey, err := messages.DMConversationKey("user", DevUserID, "agent", dmAgent.ID)
	require.NoError(t, err)
	endpoint := "/api/v1/chat/conversations/" + url.PathEscape(dmKey) + "/interagent"

	rr := doRequest(t, srv, http.MethodGet, endpoint, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var raw struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	require.Len(t, raw.Messages, 4)

	allowedKeys := map[string]bool{
		"id":                 true,
		"sender":             true,
		"recipient":          true,
		"msg":                true,
		"msgTruncated":       true,
		"createdAt":          true,
		"senderProjectId":    true,
		"recipientProjectId": true,
	}
	byID := make(map[string]map[string]any, len(raw.Messages))
	for _, item := range raw.Messages {
		id, _ := item["id"].(string)
		byID[id] = item
		for k := range item {
			assert.True(t, allowedKeys[k], "unexpected wire field %q on interagent list entry", k)
		}
	}

	gotLong := byID[longMsg.ID]
	require.NotNil(t, gotLong)
	gotLongMsg, _ := gotLong["msg"].(string)
	assert.True(t, utf8.ValidString(gotLongMsg), "truncated msg must be valid UTF-8")
	assert.Equal(t, truncatePreview(longBody, 300), gotLongMsg)
	assert.Equal(t, true, gotLong["msgTruncated"])

	gotExact := byID[exactMsg.ID]
	require.NotNil(t, gotExact)
	assert.Equal(t, exactBody, gotExact["msg"])
	_, hasTruncExact := gotExact["msgTruncated"]
	assert.False(t, hasTruncExact, "300-rune message must not set msgTruncated")

	gotShort := byID[shortMsg.ID]
	require.NotNil(t, gotShort)
	assert.Equal(t, shortBody, gotShort["msg"])
	_, hasTruncShort := gotShort["msgTruncated"]
	assert.False(t, hasTruncShort, "short message must omit msgTruncated")

	gotStripped := byID[strippedLongMsg.ID]
	require.NotNil(t, gotStripped)
	assert.Equal(t, "", gotStripped["msg"])
	_, hasTruncStripped := gotStripped["msgTruncated"]
	assert.False(t, hasTruncStripped, "stripped cross-project body must have msgTruncated=false")
}

func TestInteragent_MessageIDFullBody(t *testing.T) {
	srv, s, counter, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *interagentCountingStore {
		return &interagentCountingStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	proj := &store.Project{
		ID:        tid("ia-single-proj"),
		Name:      "ia-single-proj",
		Slug:      "ia-single-proj",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	dmAgent := &store.Agent{
		ID:        tid("ia-single-dm-agent"),
		Name:      "dm-agent",
		Slug:      "dm-agent",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	otherAgentA := &store.Agent{
		ID:        tid("ia-single-other-a"),
		Name:      "other-a",
		Slug:      "other-a",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	otherAgentB := &store.Agent{
		ID:        tid("ia-single-other-b"),
		Name:      "other-b",
		Slug:      "other-b",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	for _, a := range []*store.Agent{dmAgent, otherAgentA, otherAgentB} {
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	fullMarkdown := "# Full Report\n\n" + strings.Repeat("Detailed finding paragraph with unicode 🎯. ", 20)
	require.Greater(t, utf8.RuneCountInString(fullMarkdown), 300)

	validMsg := &store.Message{
		ID:          tid("ia-single-valid"),
		ProjectID:   proj.ID,
		Sender:      "agent:" + dmAgent.Slug,
		SenderID:    dmAgent.ID,
		Recipient:   "agent:" + otherAgentA.Slug,
		RecipientID: otherAgentA.ID,
		Msg:         fullMarkdown,
		Type:        "instruction",
		CreatedAt:   time.Now().Add(-3 * time.Minute),
	}
	legacyMsg := &store.Message{
		ID:        tid("ia-single-legacy"),
		ProjectID: proj.ID,
		Sender:    "agent:" + dmAgent.Slug,
		SenderID:  "", // legacy pre-SenderID row
		Recipient: "agent:" + otherAgentA.Slug,
		Msg:       fullMarkdown,
		Type:      "instruction",
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	unrelatedAgentMsg := &store.Message{
		ID:          tid("ia-single-unrelated"),
		ProjectID:   proj.ID,
		Sender:      "agent:" + otherAgentA.Slug,
		SenderID:    otherAgentA.ID,
		Recipient:   "agent:" + otherAgentB.Slug,
		RecipientID: otherAgentB.ID,
		Msg:         "other agents talking",
		Type:        "instruction",
		CreatedAt:   time.Now().Add(-time.Minute),
	}
	humanToAgentMsg := &store.Message{
		ID:          tid("ia-single-human"),
		ProjectID:   proj.ID,
		Sender:      "user:dev@example.com",
		SenderID:    DevUserID,
		Recipient:   "agent:" + dmAgent.Slug,
		RecipientID: dmAgent.ID,
		Msg:         "human DM message",
		Type:        "instruction",
		CreatedAt:   time.Now(),
	}
	for _, m := range []*store.Message{validMsg, legacyMsg, unrelatedAgentMsg, humanToAgentMsg} {
		require.NoError(t, s.CreateMessage(ctx, m))
	}

	dmKey, err := messages.DMConversationKey("user", DevUserID, "agent", dmAgent.ID)
	require.NoError(t, err)
	baseEndpoint := "/api/v1/chat/conversations/" + url.PathEscape(dmKey) + "/interagent"

	fault.Arm()
	counter.reset()

	// 1. Valid modern message returns full untruncated body with msgTruncated=false and 0 ListMessages calls.
	rr := doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+url.QueryEscape(validMsg.ID), nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var singleRaw struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &singleRaw))
	require.Len(t, singleRaw.Messages, 1)
	assert.Equal(t, validMsg.ID, singleRaw.Messages[0]["id"])
	assert.Equal(t, fullMarkdown, singleRaw.Messages[0]["msg"])
	_, hasTrunc := singleRaw.Messages[0]["msgTruncated"]
	assert.False(t, hasTrunc, "full-body response must have msgTruncated=false")
	assert.Equal(t, 1, counter.count("GetMessage"))
	assert.Equal(t, 0, counter.count("ListMessages"))

	// 2. Legacy sender-only message involving the DM agent also resolves full body.
	rr = doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+url.QueryEscape(legacyMsg.ID), nil)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &singleRaw))
	require.Len(t, singleRaw.Messages, 1)
	assert.Equal(t, fullMarkdown, singleRaw.Messages[0]["msg"])

	// 3. Message not involving the DM agent returns 404.
	rr = doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+url.QueryEscape(unrelatedAgentMsg.ID), nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, "unrelated agent message must 404")

	// 4. Human<->agent message returns 404.
	rr = doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+url.QueryEscape(humanToAgentMsg.ID), nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, "human-agent message must 404")

	// 5. Unknown UUID and malformed ID return 404.
	rr = doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+tid("ia-single-missing"), nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, "unknown message ID must 404")
	rr = doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId=not-a-uuid", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, "invalid UUID must 404")

	// 6. Caller without ActionAttach receives 403.
	readerID := tid("ia-single-reader")
	createTestUserWithProjectRole(t, s, readerID, "ia-reader@test.com", proj.ID, store.ProjectRoleMember)
	readerDMKey, err := messages.DMConversationKey("user", readerID, "agent", dmAgent.ID)
	require.NoError(t, err)
	readerIdentity := NewAuthenticatedUser(readerID, "ia-reader@test.com", "Reader", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/conversations/placeholder/interagent?messageId="+url.QueryEscape(validMsg.ID), nil)
	req = req.WithContext(contextWithIdentity(req.Context(), readerIdentity))
	rec := httptest.NewRecorder()
	srv.handleConversationInteragent(rec, req, readerDMKey)
	assert.Equal(t, http.StatusForbidden, rec.Code, "caller without ActionAttach must get 403")
}

func TestInteragent_MessageIDCrossProjectStripped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectA := &store.Project{
		ID:        tid("ia-mid-proj-a"),
		Name:      "ia-mid-proj-a",
		Slug:      "ia-mid-proj-a",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))
	projectB := &store.Project{
		ID:   tid("ia-mid-proj-b"),
		Name: "ia-mid-proj-b",
		Slug: "ia-mid-proj-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	newAgent := func(name, projectID string) *store.Agent {
		a := &store.Agent{
			ID:          tid(name),
			Name:        name,
			Slug:        name,
			ProjectID:   projectID,
			Phase:       "running",
			MessageMode: store.MessageModeHub,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	agentA := newAgent("ia-mid-agent-a", projectA.ID)
	agentA2 := newAgent("ia-mid-agent-a2", projectA.ID)
	agentB := newAgent("ia-mid-agent-b", projectB.ID)
	agentB2 := newAgent("ia-mid-agent-b2", projectB.ID)

	// Cross-project direct conversation WITHOUT DevUserID as participant.
	crossDMKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	crossConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: crossDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	for _, id := range []string{agentA.ID, agentB.ID} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: crossConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    id,
			Role:           "member",
		}))
	}

	// Cross-project group conversation WITH DevUserID as participant.
	partConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "test-group:" + tid("ia-mid-part-group"),
		DriftState:  "active",
	})
	require.NoError(t, err)
	for _, id := range []string{agentA.ID, agentB2.ID} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: partConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    id,
			Role:           "member",
		}))
	}
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: partConv.ID,
		PrincipalKind:  "user",
		PrincipalID:    DevUserID,
		Role:           "member",
	}))

	// Same-project direct conversation.
	sameDMKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentA2.ID)
	require.NoError(t, err)
	sameConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: sameDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	for _, id := range []string{agentA.ID, agentA2.ID} {
		require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
			ConversationID: sameConv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    id,
			Role:           "member",
		}))
	}

	longCrossSecret := strings.Repeat("cross-secret-", 30) // > 300 runes
	longParticipantVisible := strings.Repeat("cross-visible-", 30)
	longSameVisible := strings.Repeat("same-visible-", 30)

	projAID := projectA.ID
	projBID := projectB.ID

	nonParticipantMsg := &store.Message{
		ID:                 tid("ia-mid-nonpart"),
		ProjectID:          projectB.ID,
		Sender:             "agent:" + agentA.Slug,
		SenderID:           agentA.ID,
		Recipient:          "agent:" + agentB.Slug,
		RecipientID:        agentB.ID,
		Msg:                longCrossSecret,
		Type:               "instruction",
		AgentID:            agentA.ID,
		ConversationID:     crossConv.ID,
		SenderProjectID:    &projAID,
		RecipientProjectID: &projBID,
		CreatedAt:          time.Now().Add(-3 * time.Minute),
	}
	noConvCrossMsg := &store.Message{
		ID:                 tid("ia-mid-noconv"),
		ProjectID:          projectB.ID,
		Sender:             "agent:" + agentA.Slug,
		SenderID:           agentA.ID,
		Recipient:          "agent:" + agentB.Slug,
		RecipientID:        agentB.ID,
		Msg:                longCrossSecret,
		Type:               "instruction",
		AgentID:            agentA.ID,
		ConversationID:     "",
		SenderProjectID:    &projAID,
		RecipientProjectID: &projBID,
		CreatedAt:          time.Now().Add(-2 * time.Minute),
	}
	participantMsg := &store.Message{
		ID:                 tid("ia-mid-part"),
		ProjectID:          projectB.ID,
		Sender:             "agent:" + agentA.Slug,
		SenderID:           agentA.ID,
		Recipient:          "agent:" + agentB2.Slug,
		RecipientID:        agentB2.ID,
		Msg:                longParticipantVisible,
		Type:               "instruction",
		AgentID:            agentA.ID,
		ConversationID:     partConv.ID,
		SenderProjectID:    &projAID,
		RecipientProjectID: &projBID,
		CreatedAt:          time.Now().Add(-time.Minute),
	}
	sameProjMsg := &store.Message{
		ID:                 tid("ia-mid-same"),
		ProjectID:          projectA.ID,
		Sender:             "agent:" + agentA.Slug,
		SenderID:           agentA.ID,
		Recipient:          "agent:" + agentA2.Slug,
		RecipientID:        agentA2.ID,
		Msg:                longSameVisible,
		Type:               "instruction",
		AgentID:            agentA.ID,
		ConversationID:     sameConv.ID,
		SenderProjectID:    &projAID,
		RecipientProjectID: &projAID,
		CreatedAt:          time.Now(),
	}
	for _, m := range []*store.Message{nonParticipantMsg, noConvCrossMsg, participantMsg, sameProjMsg} {
		require.NoError(t, s.CreateMessage(ctx, m))
	}

	enableCPM(t, srv, s)

	userDMKey, err := messages.DMConversationKey("user", DevUserID, "agent", agentA.ID)
	require.NoError(t, err)
	baseEndpoint := "/api/v1/chat/conversations/" + url.PathEscape(userDMKey) + "/interagent"

	cases := []struct {
		name    string
		id      string
		wantMsg string
	}{
		{"non-participant cross-project stripped", nonParticipantMsg.ID, ""},
		{"no-conversation cross-project stripped", noConvCrossMsg.ID, ""},
		{"participant cross-project full body", participantMsg.ID, longParticipantVisible},
		{"same-project full body", sameProjMsg.ID, longSameVisible},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(t, srv, http.MethodGet, baseEndpoint+"?messageId="+url.QueryEscape(tc.id), nil)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
			var resp interagentResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			require.Len(t, resp.Messages, 1)
			assert.Equal(t, tc.id, resp.Messages[0].ID)
			assert.Equal(t, tc.wantMsg, resp.Messages[0].Msg)
		})
	}
}
