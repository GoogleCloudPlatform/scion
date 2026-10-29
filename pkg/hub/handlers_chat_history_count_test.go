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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// listOptsRecordingStore records the options of every ListMessages call
// made while its switch is active (always, for a nil switch). It always
// delegates. Install it with installStoreFault and Arm it once setup is
// done, so only the calls under test are recorded.
type listOptsRecordingStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	opts  []store.ListOptions
}

func (s *listOptsRecordingStore) ListMessages(ctx context.Context, f store.MessageFilter, o store.ListOptions) (*store.ListResult[store.Message], error) {
	if s.fault.Active() {
		s.mu.Lock()
		s.opts = append(s.opts, o)
		s.mu.Unlock()
	}
	return s.Store.ListMessages(ctx, f, o)
}

// installListOptsRecorder installs a disarmed listOptsRecordingStore as
// srv.store.
func installListOptsRecorder(t *testing.T, srv *Server) (*listOptsRecordingStore, *storeFaultSwitch) {
	t.Helper()
	return installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *listOptsRecordingStore {
		return &listOptsRecordingStore{Store: inner, fault: fault}
	})
}

// assertListMessagesSkippedCount fails unless at least one ListMessages
// call was made since the last check and every one skipped the count. It
// then clears the record.
func assertListMessagesSkippedCount(t *testing.T, rs *listOptsRecordingStore, label string) {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.opts) == 0 {
		t.Fatalf("%s: no ListMessages call recorded", label)
	}
	for i, o := range rs.opts {
		if !o.SkipTotalCount {
			t.Errorf("%s: ListMessages call %d requested a total count", label, i)
		}
	}
	rs.opts = nil
}

// Every history page (the first page, a scroll-back page and an around
// jump) lists messages without a COUNT over the whole conversation. The
// response keeps its totalCount field, at 0 (ptone/scion#6571).
func TestChatV2_History_SkipsTotalCount(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	rs, record := installListOptsRecorder(t, srv)
	ctx := context.Background()

	topicID := tid("topic-history-skip-count")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "history-skip-count",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	seedHistoryMessages(t, s, proj.ID, topicID, 12)
	record.Arm()

	fetch := func(label, query string) chatHistoryResponse {
		t.Helper()
		path := "/api/v1/chat/conversations/" + topicID + "/messages?" + query
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET %s: expected 200, got %d: %s", label, path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"totalCount":0`) {
			t.Errorf("%s: response should carry totalCount 0, got %s", label, body)
		}
		var resp chatHistoryResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
		if len(resp.Messages) == 0 {
			t.Fatalf("%s: expected messages", label)
		}
		assertListMessagesSkippedCount(t, rs, label)
		return resp
	}

	first := fetch("first page", "limit=5")
	if first.NextCursor == "" {
		t.Fatal("first page: expected a nextCursor")
	}
	fetch("scroll-back page", "limit=5&cursor="+url.QueryEscape(first.NextCursor))

	anchorID := tid(topicID + "-msg-message-005")
	around := fetch("around jump", "limit=5&around="+url.QueryEscape(anchorID))
	if got, want := len(around.Messages), 5; got != want {
		t.Errorf("around jump: got %d messages, want %d", got, want)
	}
}

// The interagent listing returns no total, so neither of its queries
// counts.
func TestInteragent_SkipsTotalCount(t *testing.T) {
	srv, s := testServer(t)
	rs, record := installListOptsRecorder(t, srv)
	ctx := context.Background()

	proj := &store.Project{ID: tid("interagent-skip-count-proj"), Slug: "skip-count", Name: "Skip Count"}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        tid("interagent-skip-count-agent"),
		Slug:      "skip-count-agent",
		Name:      "Skip Count Agent",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msg := &store.Message{
		ID:            tid("interagent-skip-count-msg"),
		ProjectID:     proj.ID,
		Sender:        "agent:" + agent.Slug,
		SenderID:      agent.ID,
		Recipient:     "agent:skip-count-peer",
		RecipientID:   tid("interagent-skip-count-peer"),
		Msg:           "hello peer",
		Type:          "agent_message",
		DispatchState: "dispatched",
		CreatedAt:     time.Now().Add(-time.Minute),
	}
	if err := s.CreateMessage(ctx, msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	record.Arm()

	dmKey := fmt.Sprintf("dm:agent:%s:user:%s", agent.ID, DevUserID)
	path := fmt.Sprintf("/api/v1/chat/conversations/%s/interagent", url.PathEscape(dmKey))
	rec := doRequest(t, srv, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
	}
	var resp interagentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].ID != msg.ID {
		t.Fatalf("expected the one inter-agent message, got %+v", resp.Messages)
	}

	rs.mu.Lock()
	calls := len(rs.opts)
	rs.mu.Unlock()
	if calls != 2 {
		t.Errorf("expected 2 ListMessages calls (participant and legacy sender), got %d", calls)
	}
	assertListMessagesSkippedCount(t, rs, "interagent")
}

// The edit/delete guard scan pages through every later message without a
// COUNT on any page. 60 non-agent messages make it read two pages and
// find no agent reply.
func TestHasAgentReplyAfter_SkipsTotalCount(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{ID: tid("reply-guard-skip-count-proj"), Slug: "reply-guard-skip-count", Name: "Reply Guard Skip Count"}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	threadID := tid("reply-guard-skip-count-thread")
	seedHistoryMessages(t, s, proj.ID, threadID, 60)

	rs := &listOptsRecordingStore{Store: s}
	if hasAgentReplyAfter(ctx, rs, threadID, time.Now().Add(-time.Hour)) {
		t.Fatal("hasAgentReplyAfter = true, want false: no agent sent a message")
	}

	rs.mu.Lock()
	calls := len(rs.opts)
	rs.mu.Unlock()
	if calls != 2 {
		t.Errorf("expected 2 ListMessages pages, got %d", calls)
	}
	assertListMessagesSkippedCount(t, rs, "reply guard scan")
}
