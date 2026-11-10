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

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	hubstore "github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	pollTestAgentID  = "agent-uuid-1"
	pollTestAnchorID = "anchor-msg"
)

// pollTestBase is the anchor's creation time in the fixtures.
var pollTestBase = time.Date(2026, 10, 18, 12, 0, 30, 250_000_000, time.UTC)

// dmRow builds a conversation message created offset after pollTestBase.
func dmRow(id, senderID, msgType, threadID, text string, offset time.Duration) hubstore.Message {
	return hubstore.Message{
		ID:        id,
		Sender:    "agent:helper",
		SenderID:  senderID,
		Msg:       text,
		Type:      msgType,
		ThreadID:  threadID,
		CreatedAt: pollTestBase.Add(offset),
	}
}

// anchorRow is the caller's own message, the anchor.
func anchorRow() hubstore.Message {
	m := dmRow(pollTestAnchorID, "caller-uuid", messages.TypeInstruction, "", "the request", 0)
	m.Sender = "user:caller@example.com"
	return m
}

// TestSelectReply_AnchorByID covers reply selection against the anchor: the
// page is newest first in the hub's keyset order (created, id), and only
// messages above the anchor on the page are newer than it.
func TestSelectReply_AnchorByID(t *testing.T) {
	const dm = "dm:agent:agent-uuid-1:user:caller-uuid"
	tests := []struct {
		name       string
		page       []hubstore.Message
		wantID     string
		wantAnchor bool
	}{
		{
			name: "agent messages with earlier timestamps are never returned",
			page: []hubstore.Message{
				anchorRow(),
				dmRow("earlier-1", pollTestAgentID, messages.TypeInstruction, dm, "earlier answer", -time.Second),
				dmRow("earlier-2", pollTestAgentID, "", dm, "much earlier answer", -time.Minute),
			},
			wantAnchor: true,
		},
		{
			name: "the newer agent reply is returned, not an earlier one",
			page: []hubstore.Message{
				dmRow("reply", pollTestAgentID, messages.TypeInstruction, dm, "the answer", 2*time.Second),
				anchorRow(),
				dmRow("earlier-1", pollTestAgentID, messages.TypeInstruction, dm, "earlier answer", -time.Second),
			},
			wantID:     "reply",
			wantAnchor: true,
		},
		{
			name: "the oldest newer candidate is returned, not the newest",
			page: []hubstore.Message{
				dmRow("second", pollTestAgentID, messages.TypeInstruction, dm, "second", 3*time.Second),
				dmRow("first", pollTestAgentID, messages.TypeInstruction, dm, "first", 2*time.Second),
				anchorRow(),
			},
			wantID:     "first",
			wantAnchor: true,
		},
		{
			// Equal timestamps: the hub orders (created, id) descending, so
			// a row with the anchor's time and a greater ID is above the
			// anchor (newer) and one with a smaller ID is below it (older).
			name: "equal timestamp: only the row above the anchor in keyset order",
			page: []hubstore.Message{
				dmRow("same-time-above", pollTestAgentID, messages.TypeInstruction, dm, "above", 0),
				anchorRow(),
				dmRow("same-time-below", pollTestAgentID, messages.TypeInstruction, dm, "below", 0),
			},
			wantID:     "same-time-above",
			wantAnchor: true,
		},
		{
			name: "equal timestamp: a row below the anchor in keyset order is never returned",
			page: []hubstore.Message{
				anchorRow(),
				dmRow("same-time-below", pollTestAgentID, messages.TypeInstruction, dm, "below", 0),
			},
			wantAnchor: true,
		},
		{
			name: "messages from others above the anchor are not replies",
			page: []hubstore.Message{
				dmRow("other", "someone-else", messages.TypeInstruction, dm, "not the agent", time.Second),
				anchorRow(),
			},
			wantAnchor: true,
		},
		{
			name: "without the anchor on the page nothing is taken",
			page: []hubstore.Message{
				dmRow("reply", pollTestAgentID, messages.TypeInstruction, dm, "the answer", 2*time.Second),
				dmRow("earlier-1", pollTestAgentID, messages.TypeInstruction, dm, "earlier answer", -time.Second),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, anchorSeen := selectReply(tc.page, pollTestAnchorID, pollTestAgentID)
			if anchorSeen != tc.wantAnchor {
				t.Fatalf("anchor seen = %v, want %v", anchorSeen, tc.wantAnchor)
			}
			gotID := ""
			if got != nil {
				gotID = got.ID
			}
			if gotID != tc.wantID {
				t.Fatalf("selected %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

// fakeReplyReader serves one caller's direct conversations and messages and
// records the requests. The embedded interface is nil; only the methods
// defined here are used.
type fakeReplyReader struct {
	hubclient.ConversationService

	mu            sync.Mutex
	conversations []hubclient.ConversationDetail
	byID          map[string]hubstore.Message
	page          []hubstore.Message
	listOpts      []hubclient.ListConversationsOptions
	getMessage    []string
	messageOpts   []hubclient.ConversationMessagesOptions
	// listErr, when set, is the answer to every list.
	listErr error
}

func (f *fakeReplyReader) List(_ context.Context, opts *hubclient.ListConversationsOptions) (*hubclient.ConversationListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listOpts = append(f.listOpts, *opts)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &hubclient.ConversationListResult{Conversations: f.conversations}, nil
}

func (f *fakeReplyReader) GetMessage(_ context.Context, conversationID, messageID string) (*hubstore.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getMessage = append(f.getMessage, conversationID+"/"+messageID)
	m, ok := f.byID[messageID]
	if !ok {
		return nil, errors.New("not found")
	}
	return &m, nil
}

func (f *fakeReplyReader) ListMessages(_ context.Context, _ string, opts *hubclient.ConversationMessagesOptions) (*hubstore.ListResult[hubstore.Message], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messageOpts = append(f.messageOpts, *opts)
	return &hubstore.ListResult[hubstore.Message]{Items: f.page}, nil
}

// TestReplyPoller_AppendsReplyForPollOnlyWait runs one poll against a fake
// conversation: the poller finds the conversation by its dmPeer, reads the
// anchor once, reads one page after the anchor's time, and appends the
// reply under a poll: dedup key. The poll-only wait skips an earlier push
// event and returns the polled reply.
func TestReplyPoller_AppendsReplyForPollOnlyWait(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	ctx := context.Background()
	const taskID = "poll-task-1"
	const dm = "dm:agent:agent-uuid-1:user:caller-uuid"

	reply := dmRow("reply-msg", pollTestAgentID, messages.TypeInstruction, dm, "the polled answer", 2*time.Second)
	reader := &fakeReplyReader{
		conversations: []hubclient.ConversationDetail{
			{Conversation: hubstore.Conversation{ID: "conv-other", Kind: "direct"},
				DMPeer: &hubclient.ConversationPeer{Kind: "agent", ID: "another-agent"}},
			{Conversation: hubstore.Conversation{ID: "conv-user", Kind: "direct"},
				DMPeer: &hubclient.ConversationPeer{Kind: "user", ID: pollTestAgentID}},
			{Conversation: hubstore.Conversation{ID: "conv-1", Kind: "direct"},
				DMPeer: &hubclient.ConversationPeer{Kind: "agent", ID: pollTestAgentID}},
		},
		byID: map[string]hubstore.Message{pollTestAnchorID: anchorRow()},
		page: []hubstore.Message{reply, anchorRow()},
	}

	// A push event (agent-wide status) lands before the reply.
	pushStatus, _ := json.Marshal(TaskStatusUpdate{TaskID: taskID, Status: TaskStatus{State: TaskStateCompleted}})
	if _, err := store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID: taskID, Kind: "status", Payload: pushStatus, Final: true, DedupKey: "push-status",
	}); err != nil {
		t.Fatalf("append push event: %v", err)
	}

	p := newReplyPoller(b, reader, taskID, "helper", pollTestAgentID, pollTestAnchorID, "", nil)
	p.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	p.start(ctx)

	ev, err := b.waitForTaskEvent(ctx, taskID, 5*time.Second, waitOptions{acceptEvent: pollOnlyEvent})
	p.stop()
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}
	// The reply is appended as an artifact event and a message event, both
	// under the poll: key of the hub message; the wait returns the first.
	if !strings.HasPrefix(ev.DedupKey, "poll:reply-msg:") {
		t.Fatalf("returned event dedup key %q, want the prefix %q", ev.DedupKey, "poll:reply-msg:")
	}
	if got := pollEventText(t, ev); got != "the polled answer" {
		t.Fatalf("returned event text %q, want %q", got, "the polled answer")
	}
	events, err := store.ReadTaskEvents(ctx, taskID, 0, 10)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var keys []string
	for _, e := range events {
		keys = append(keys, e.DedupKey)
	}
	if len(keys) != 3 || keys[0] != "push-status" || !strings.HasPrefix(keys[1], "poll:reply-msg:artifact:") || keys[2] != "poll:reply-msg:message" {
		t.Fatalf("event log dedup keys %v, want the push event, then the poll:reply-msg artifact and message events", keys)
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.listOpts) != 1 || reader.listOpts[0].Kind != "direct" || reader.listOpts[0].Limit != 0 {
		t.Fatalf("conversation list requests %+v, want one kind=direct list with no limit", reader.listOpts)
	}
	if len(reader.getMessage) != 1 || reader.getMessage[0] != "conv-1/"+pollTestAnchorID {
		t.Fatalf("anchor reads %v, want one read of conv-1/%s", reader.getMessage, pollTestAnchorID)
	}
	wantAfter := pollTestBase.Add(-time.Second).Format(time.RFC3339)
	if len(reader.messageOpts) != 1 || reader.messageOpts[0].After != wantAfter || reader.messageOpts[0].Limit != replyPollPageSize {
		t.Fatalf("message reads %+v, want one read with after=%s limit=%d", reader.messageOpts, wantAfter, replyPollPageSize)
	}
}

// TestWaitForTaskEvent_PollOnlySkipsPushEvents checks the poll-only wait:
// push content and a push final status are skipped, a poll: event is
// returned.
func TestWaitForTaskEvent_PollOnlySkipsPushEvents(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	ctx := context.Background()
	const taskID = "poll-only-wait"

	b.processAndAppendEvent(ctx, taskID, "helper", &messages.StructuredMessage{
		Sender: "agent:helper", Type: messages.TypeInstruction, Msg: "pushed content", Timestamp: "t1",
	}, "")
	b.processAndAppendEvent(ctx, taskID, "helper", &messages.StructuredMessage{
		Sender: "agent:helper", Type: messages.TypeStateChange, Status: "COMPLETED", Timestamp: "t2",
	}, "")
	b.processAndAppendEvent(ctx, taskID, "helper", &messages.StructuredMessage{
		Sender: "agent:helper", Type: messages.TypeInstruction, Msg: "polled content", Timestamp: "t3",
	}, replyPollDedupPrefix+"m-1")

	ev, err := b.waitForTaskEvent(ctx, taskID, 2*time.Second, waitOptions{acceptEvent: pollOnlyEvent})
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}
	if !strings.HasPrefix(ev.DedupKey, "poll:m-1:") {
		t.Fatalf("returned event dedup key %q, want the prefix %q", ev.DedupKey, "poll:m-1:")
	}
	if got := pollEventText(t, ev); got != "polled content" {
		t.Fatalf("returned event text %q, want %q", got, "polled content")
	}

	// Without the predicate the first pushed event is returned (today's
	// behaviour for calls without a poller).
	ev, err = b.waitForTaskEvent(ctx, taskID, 2*time.Second, waitOptions{})
	if err != nil {
		t.Fatalf("waitForTaskEvent without predicate: %v", err)
	}
	if strings.HasPrefix(ev.DedupKey, replyPollDedupPrefix) {
		t.Fatalf("wait without predicate returned the polled event %q, want the first pushed event", ev.DedupKey)
	}
	if got := pollEventText(t, ev); got != "pushed content" {
		t.Fatalf("wait without predicate returned text %q, want %q", got, "pushed content")
	}
}

// pollEventText returns the text of a content event: an artifact event's
// artifact parts or a message event's status message parts.
func pollEventText(t *testing.T, ev *state.TaskEvent) string {
	t.Helper()
	var parts []Part
	switch ev.Kind {
	case "artifact":
		var au TaskArtifactUpdate
		if err := json.Unmarshal(ev.Payload, &au); err != nil {
			t.Fatalf("decode artifact event: %v", err)
		}
		parts = au.Artifact.Parts
	case "message":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			t.Fatalf("decode message event: %v", err)
		}
		if su.Status.Message != nil {
			parts = su.Status.Message.Parts
		}
	default:
		t.Fatalf("event kind %q is not a content event", ev.Kind)
	}
	var text []string
	for _, p := range parts {
		text = append(text, p.Text)
	}
	return strings.Join(text, "\n")
}

// TestReplyWaitKey_SameCallerOnly: the same-caller check refuses a second
// waiting call only for the same principal and agent. Two users calling the
// same agent, and a user and a call without a caller, wait side by side.
func TestReplyWaitKey_SameCallerOnly(t *testing.T) {
	const admin = "admin-user-id"
	alice := &CallerIdentity{UserID: "alice-id"}
	bob := &CallerIdentity{UserID: "bob-id"}

	b := &Bridge{}
	if !b.acquireReplyWait(replyWaitKey(alice, admin, pollTestAgentID)) {
		t.Fatalf("first call by alice was refused")
	}
	if b.acquireReplyWait(replyWaitKey(&CallerIdentity{UserID: "alice-id"}, admin, pollTestAgentID)) {
		t.Fatalf("second concurrent call by alice to the same agent was admitted")
	}
	if !b.acquireReplyWait(replyWaitKey(bob, admin, pollTestAgentID)) {
		t.Fatalf("bob's call was refused while alice's call to the same agent waits")
	}
	if !b.acquireReplyWait(replyWaitKey(nil, admin, pollTestAgentID)) {
		t.Fatalf("a call without a caller was refused while user calls to the same agent wait")
	}
	if b.acquireReplyWait(replyWaitKey(nil, admin, pollTestAgentID)) {
		t.Fatalf("a second concurrent call without a caller to the same agent was admitted")
	}
	if !b.acquireReplyWait(replyWaitKey(alice, admin, "agent-uuid-2")) {
		t.Fatalf("alice's call to another agent was refused")
	}
	b.releaseReplyWait(replyWaitKey(alice, admin, pollTestAgentID))
	if !b.acquireReplyWait(replyWaitKey(alice, admin, pollTestAgentID)) {
		t.Fatalf("alice's call was refused after her earlier call ended")
	}
}
