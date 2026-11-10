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
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// TestCorrelateToTask_PluginAcceptanceTable covers the plugin-mode push
// acceptance table, one case per row and condition (D4b). Alice's task has
// caller C = Alice; the bridge admin's topic user is A ("test-user" in the
// lifecycle test bridge). The poller mark is off in every case.
func TestCorrelateToTask_PluginAcceptanceTable(t *testing.T) {
	const (
		project = "proj1"
		agent   = "helper"
		alice   = "alice-id"
		bob     = "bob-id"
		admin   = "test-user"
	)
	userTopic := func(user string) string { return projectkeys.UserTopic(project, user) }
	aliceCaller := &CallerIdentity{UserID: alice, Email: "alice@example.com", TokenType: "jwt"}
	agentMsg := func(meta map[string]string) *messages.StructuredMessage {
		return &messages.StructuredMessage{Sender: "agent:" + agent, Type: messages.TypeInstruction, Msg: "content", Metadata: meta}
	}

	tests := []struct {
		name     string
		register func(b *Bridge)
		topic    string
		msg      *messages.StructuredMessage
		wantTask string // "" means dropped
		wantWhy  string
	}{
		{
			name:     "1 user topic of another user: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    userTopic(bob),
			msg:      agentMsg(nil),
			wantWhy:  pushDropTopicUser,
		},
		{
			name:     "2 user topic of the caller: accepted",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    userTopic(alice),
			msg:      agentMsg(nil),
			wantTask: "task-alice",
		},
		{
			name:     "3 admin class, admin topic: accepted",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-admin", agentKey(project, agent), nil) },
			topic:    userTopic(admin),
			msg:      agentMsg(nil),
			wantTask: "task-admin",
		},
		{
			name:     "4 admin class, another user's topic: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-admin", agentKey(project, agent), nil) },
			topic:    userTopic(bob),
			msg:      agentMsg(nil),
			wantWhy:  pushDropTopicUser,
		},
		{
			name:     "5 agent topic: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    projectkeys.AgentTopic(project, agent),
			msg:      agentMsg(nil),
			wantWhy:  pushDropTopicKind,
		},
		{
			name:     "5 project broadcast topic: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-admin", agentKey(project, agent), nil) },
			topic:    "scion.project." + project + ".broadcast",
			msg:      agentMsg(nil),
			wantWhy:  pushDropTopicKind,
		},
		{
			name:     "6 unparseable topic: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    "scion.project." + project + ".user." + alice,
			msg:      agentMsg(nil),
			wantWhy:  pushDropTopicKind,
		},
		{
			name:     "7 empty sender: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    userTopic(alice),
			msg:      &messages.StructuredMessage{Type: messages.TypeInstruction, Msg: "content"},
			wantWhy:  pushDropSender,
		},
		{
			name:     "7 user sender: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    userTopic(alice),
			msg:      &messages.StructuredMessage{Sender: "user:x", Type: messages.TypeInstruction, Msg: "content"},
			wantWhy:  pushDropSender,
		},
		{
			name:     "8 entry project differs from the topic's: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey("proj-other", agent), aliceCaller) },
			topic:    userTopic(alice),
			msg:      agentMsg(map[string]string{"a2aTaskId": "task-alice"}),
			wantWhy:  pushDropProject,
		},
		{
			name:     "9 entry agent differs from the sender: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, "other-agent"), aliceCaller) },
			topic:    userTopic(alice),
			msg:      agentMsg(map[string]string{"a2aTaskId": "task-alice"}),
			wantWhy:  pushDropAgent,
		},
		{
			name:     "10 the caller's task ID on another user's topic: dropped",
			register: func(b *Bridge) { b.registerActiveTaskFor("task-alice", agentKey(project, agent), aliceCaller) },
			topic:    userTopic(bob),
			msg:      agentMsg(map[string]string{"a2aTaskId": "task-alice"}),
			wantWhy:  pushDropTopicUser,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newLifecycleTestBridge(t)
			tc.register(b)
			got, err := b.correlateToTask(context.Background(), project, extractAgentIDFromSender(tc.msg.Sender), tc.topic, tc.msg)
			if tc.wantTask != "" {
				if err != nil || got != tc.wantTask {
					t.Fatalf("correlated to %q, %v; want %q", got, err, tc.wantTask)
				}
				return
			}
			if err == nil {
				t.Fatalf("correlated to %q, want the message dropped (%s)", got, tc.wantWhy)
			}
			if why := pushDropReasonOf(err); why != tc.wantWhy {
				t.Fatalf("drop reason %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

// TestHandleBrokerMessage_DropsPushWhilePollerLive: a pushed message for a
// task whose call has a live reply poller is dropped (no event, no state
// update); with the mark cleared the same message is appended (R12).
func TestHandleBrokerMessage_DropsPushWhilePollerLive(t *testing.T) {
	logs := &syncBuffer{}
	b, store := newDebugLoggedBridge(t, logs)
	ctx := context.Background()
	const taskID = "poller-owned-task"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	b.setTaskPollerLive(taskID, true)

	topic := "scion.project.proj1.user.test-user.messages"
	question := &messages.StructuredMessage{
		Version: 1, Sender: "agent:agent-a", Type: messages.TypeInputNeeded, Msg: "a pushed question",
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metadata: map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(ctx, topic, question); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	if events, _ := store.ReadTaskEvents(ctx, taskID, 0, 100); len(events) != 0 {
		t.Fatalf("%d events appended for a pushed message while the poller is live, want 0", len(events))
	}
	if task, _ := store.GetTask(ctx, taskID); task == nil || task.State != TaskStateWorking {
		t.Fatalf("task state changed by a dropped push: %+v", task)
	}
	if !strings.Contains(logs.String(), "reason="+pushDropPollerOwns) {
		t.Fatalf("no %s drop line; logs:\n%s", pushDropPollerOwns, logs.String())
	}
	if strings.Contains(logs.String(), "a pushed question") || strings.Contains(logs.String(), topic) {
		t.Fatalf("a drop line carries message text or the topic; logs:\n%s", logs.String())
	}

	b.setTaskPollerLive(taskID, false)
	if err := b.HandleBrokerMessage(ctx, topic, question); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	if events, _ := store.ReadTaskEvents(ctx, taskID, 0, 100); len(events) == 0 {
		t.Fatal("no events appended once the mark is cleared")
	}
	// With no poller the message reaches the accepted-message line, which
	// carries metadata only (D9).
	if !strings.Contains(logs.String(), "handling broker message") || !strings.Contains(logs.String(), "text_len=") {
		t.Fatalf("no metadata line for the accepted message; logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "a pushed question") || strings.Contains(logs.String(), topic) {
		t.Fatalf("a log line carries message text or the topic; logs:\n%s", logs.String())
	}
}

// TestProcessAndAppendEvent_PollPrefixReserved: the push path never yields
// a poll: dedup key; a message whose msgId starts with poll: is appended
// under its content hash (R17).
func TestProcessAndAppendEvent_PollPrefixReserved(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	ctx := context.Background()
	const taskID = "poll-prefix-task"
	b.processAndAppendEvent(ctx, taskID, "helper", &messages.StructuredMessage{
		Sender: "agent:helper", Type: messages.TypeInstruction, Msg: "pushed content",
		Timestamp: "2026-10-18T12:00:00Z", Metadata: map[string]string{"msgId": "poll:x"},
	}, "")
	events, err := store.ReadTaskEvents(ctx, taskID, 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("events %v, %v; want the pushed message appended", events, err)
	}
	for _, ev := range events {
		if strings.HasPrefix(ev.DedupKey, replyPollDedupPrefix) {
			t.Fatalf("push path appended an event with dedup key %q", ev.DedupKey)
		}
		if pollOnlyEvent(ev) {
			t.Fatalf("the poll-only wait would accept push event %q", ev.DedupKey)
		}
	}

	// A plain msgId is still the dedup key.
	b.processAndAppendEvent(ctx, "msgid-task", "helper", &messages.StructuredMessage{
		Sender: "agent:helper", Type: messages.TypeStateChange, Status: "WORKING",
		Timestamp: "2026-10-18T12:00:01Z", Metadata: map[string]string{"msgId": "stable-id"},
	}, "")
	events, _ = store.ReadTaskEvents(ctx, "msgid-task", 0, 100)
	if len(events) != 1 || events[0].DedupKey != "stable-id" {
		t.Fatalf("events %+v, want one event with dedup key stable-id", events)
	}
}

// TestRegisterActiveTaskFor_KeepsEarlierCaller: a re-registration (a
// continuation) keeps the caller recorded first, and the admin class is
// recorded for a nil caller and for an agent caller.
func TestRegisterActiveTaskFor_KeepsEarlierCaller(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	aKey := agentKey("proj1", "helper")

	b.registerActiveTaskFor("task-1", aKey, &CallerIdentity{UserID: "alice-id"})
	b.registerActiveTaskFor("task-1", aKey, &CallerIdentity{UserID: "bob-id"})
	b.registerActiveTaskFor("task-1", aKey, nil)
	b.tasksMu.RLock()
	entry := b.activeTasks["task-1"]
	n := len(b.agentTasks[aKey])
	b.tasksMu.RUnlock()
	if !entry.userCaller || entry.callerUserID != "alice-id" {
		t.Fatalf("entry %+v after re-registration, want the first caller alice-id", entry)
	}
	if n != 1 {
		t.Fatalf("agent task list has %d entries, want 1", n)
	}

	b.registerActiveTaskFor("task-agent", aKey, &CallerIdentity{AgentID: "agent-caller", UserID: "ignored"})
	b.registerActiveTask("task-legacy", aKey)
	b.tasksMu.RLock()
	agentEntry, legacyEntry := b.activeTasks["task-agent"], b.activeTasks["task-legacy"]
	b.tasksMu.RUnlock()
	if agentEntry.userCaller || legacyEntry.userCaller || agentEntry.callerUserID != "" || legacyEntry.callerUserID != "" {
		t.Fatalf("agent caller entry %+v and legacy entry %+v, want both in the admin class", agentEntry, legacyEntry)
	}
}

// newDebugLoggedBridge is newLifecycleTestBridge with a debug-level log
// sink.
func newDebugLoggedBridge(t *testing.T, logs *syncBuffer) (*Bridge, state.Store) {
	t.Helper()
	store, err := state.NewSQLite(filepath.Join(t.TempDir(), "debug-logged.db"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &Config{
		Hub:      HubConfig{User: "test-user"},
		Timeouts: TimeoutConfig{SendMessage: 5 * time.Second},
	}
	b := New(store, nil, nil, cfg, nil, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(b.Shutdown)
	return b, store
}
