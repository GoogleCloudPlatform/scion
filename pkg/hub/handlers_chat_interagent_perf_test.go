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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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
