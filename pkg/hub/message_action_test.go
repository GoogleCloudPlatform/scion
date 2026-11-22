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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drainActionEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestPublishActionUpdated_SubjectsAndPayloads(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv := &Server{events: pub}

	// Project topic subject for "decision" and "secret_request".
	projDecCh, unsubProjDec := pub.Subscribe("project.proj-1.chat.decision.updated")
	defer unsubProjDec()
	projSecCh, unsubProjSec := pub.Subscribe("project.proj-1.chat.secret_request.updated")
	defer unsubProjSec()

	srv.publishActionUpdated("decision", "proj-1", "msg-1", "topic-1", map[string]string{"rowId": "r1"})
	srv.publishActionUpdated("secret_request", "proj-1", "msg-2", "topic-1", map[string]string{"rowId": "sr1"})

	decEvts := drainActionEvents(projDecCh)
	require.Len(t, decEvts, 1)
	var decPayload map[string]any
	require.NoError(t, json.Unmarshal(decEvts[0].Data, &decPayload))
	assert.Equal(t, "msg-1", decPayload["messageId"])
	assert.Equal(t, "topic-1", decPayload["threadId"])
	assert.Equal(t, map[string]any{"rowId": "r1"}, decPayload["decision"])

	secEvts := drainActionEvents(projSecCh)
	require.Len(t, secEvts, 1)
	var secPayload map[string]any
	require.NoError(t, json.Unmarshal(secEvts[0].Data, &secPayload))
	assert.Equal(t, "msg-2", secPayload["messageId"])
	assert.Equal(t, "topic-1", secPayload["threadId"])
	assert.Equal(t, map[string]any{"rowId": "sr1"}, secPayload["secret_request"])

	// DM subject reaches user participant only, never project subject.
	userCh, unsubUser := pub.Subscribe("user.u-42.chat.secret_request.updated")
	defer unsubUser()
	srv.publishActionUpdated("secret_request", "proj-1", "msg-3", "dm:agent:a-1:user:u-42", map[string]string{"rowId": "sr2"})

	userEvts := drainActionEvents(userCh)
	require.Len(t, userEvts, 1)
	var dmPayload map[string]any
	require.NoError(t, json.Unmarshal(userEvts[0].Data, &dmPayload))
	assert.Equal(t, "msg-3", dmPayload["messageId"])
	assert.Equal(t, "dm:agent:a-1:user:u-42", dmPayload["threadId"])
	assert.Equal(t, map[string]any{"rowId": "sr2"}, dmPayload["secret_request"])
	assert.Empty(t, drainActionEvents(projSecCh))
}

// recordingActionDispatcher captures DispatchAgentMessage calls for C2 seam tests.
type recordingActionDispatcher struct {
	AgentDispatcher
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (d *recordingActionDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, sm *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := *sm
	d.msgs = append(d.msgs, &cp)
	return nil
}

// Seam test C2: dispatchActionResponse can be called more than once per row
// (e.g. approved, then execution outcome), and delivery is optional (no asker
// or nil Structured is a safe no-op without panicking).
func TestDispatchActionResponse_RepeatableAndOptional(t *testing.T) {
	ctx := context.Background()
	disp := &recordingActionDispatcher{}
	srv := &Server{
		dispatcher: disp,
		messageLog: slog.Default(),
	}

	// Optional delivery: nil Asker, nil Message, and nil Structured are safe no-ops.
	held, err := srv.dispatchActionResponse(ctx, actionResponse{
		Asker:      nil,
		Message:    &store.Message{ID: "m-0"},
		Text:       "approved",
		Structured: &messages.StructuredMessage{Msg: "approved"},
	})
	require.NoError(t, err)
	assert.False(t, held)
	assert.Empty(t, disp.msgs)

	held, err = srv.dispatchActionResponse(ctx, actionResponse{
		Asker:      &store.Agent{ID: "a-1", Slug: "worker-1"},
		Message:    &store.Message{ID: "m-0"},
		Text:       "approved",
		Structured: nil,
	})
	require.NoError(t, err)
	assert.False(t, held)
	assert.Empty(t, disp.msgs)

	// Repeatable delivery: two responses for the same action row (e.g. approved,
	// then completed) both dispatch cleanly.
	asker := &store.Agent{ID: "a-1", Slug: "worker-1"}
	for _, step := range []struct {
		msgID string
		text  string
		stage string
	}{
		{"m-1", "[exec-response] approved", "approved"},
		{"m-2", "[exec-response] completed exit=0", "completed"},
	} {
		held, err := srv.dispatchActionResponse(ctx, actionResponse{
			Asker:      asker,
			Message:    &store.Message{ID: step.msgID},
			Text:       step.text,
			Structured: &messages.StructuredMessage{Msg: step.text},
			PopulateMeta: func(meta map[string]string) {
				meta["exec_stage"] = step.stage
			},
			LogPrefix: "exec_action",
			Label:     "exec-response",
		})
		require.NoError(t, err)
		assert.False(t, held)
	}
	require.Len(t, disp.msgs, 2)
	assert.Equal(t, "approved", disp.msgs[0].Metadata["exec_stage"])
	assert.Equal(t, "completed", disp.msgs[1].Metadata["exec_stage"])
}

func TestLogMessageActionEvent_StructuredAttrsAndAllowList(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := &LogAuditLogger{
		prefix: "[Audit]",
		log:    slog.New(handler),
	}
	logger.RegisterMessageActionDetailKeys("secret_request", "secret_type", "secret_version")

	ts := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	require.NoError(t, logger.LogMessageActionEvent(context.Background(), &MessageActionEvent{
		Kind:          "secret_request",
		EventType:     MessageActionEventResolved,
		RowID:         "row-1",
		MessageID:     "msg-1",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
		AgentName:     "worker-1",
		Key:           "API_TOKEN",
		Version:       2,
		ReplacesRowID: "row-0",
		ActorKind:     MessageActionActorUser,
		ActorID:       "user-1",
		Surface:       "native",
		Details: map[string]string{
			"secret_type":    "environment",
			"secret_version": "3",
			"secret_value":   "MUST_BE_DROPPED",
		},
		Timestamp: ts,
	}))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, "message action event", rec["msg"])
	assert.Equal(t, "secret_request", rec["kind"])
	assert.Equal(t, "resolved", rec["event_type"])
	assert.Equal(t, "row-1", rec["row_id"])
	assert.Equal(t, "msg-1", rec["message_id"])
	assert.Equal(t, "proj-1", rec["project_id"])
	assert.Equal(t, "agent-1", rec["agent_id"])
	assert.Equal(t, "worker-1", rec["agent_name"])
	assert.Equal(t, "API_TOKEN", rec["key"])
	assert.Equal(t, float64(2), rec["version"])
	assert.Equal(t, "row-0", rec["replaces_row_id"])
	assert.Equal(t, "user", rec["actor_kind"])
	assert.Equal(t, "user-1", rec["actor_id"])
	assert.Equal(t, "native", rec["surface"])
	assert.Equal(t, "environment", rec["detail_secret_type"])
	assert.Equal(t, "3", rec["detail_secret_version"])
	_, leaked := rec["detail_secret_value"]
	assert.False(t, leaked, "disallowed detail key must be dropped by per-kind allow-list")
	assert.NotContains(t, buf.String(), "MUST_BE_DROPPED")

	// Refused event logs at WARN level.
	buf.Reset()
	srv := &Server{auditLogger: logger}
	srv.emitMessageActionEvent(context.Background(), &MessageActionEvent{
		Kind:      "secret_request",
		EventType: MessageActionEventRefused,
		RowID:     "row-1",
		ActorKind: MessageActionActorUser,
		ActorID:   "user-2",
		Reason:    "forbidden",
	})
	var refusedRec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &refusedRec))
	assert.Equal(t, "WARN", refusedRec["level"])
	assert.Equal(t, "refused", refusedRec["event_type"])
	assert.Equal(t, "forbidden", refusedRec["reason"])

	// Nil safety.
	require.NoError(t, logger.LogMessageActionEvent(context.Background(), nil))
	var nilLogger *LogAuditLogger
	require.NoError(t, nilLogger.LogMessageActionEvent(context.Background(), &MessageActionEvent{}))
	srv.emitMessageActionEvent(context.Background(), nil)
}
