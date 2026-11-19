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
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSweepTestServer builds a minimal Server backed by an in-memory SQLite
// store, sufficient for exercising the recurring sweep handlers directly
// (bypassing the scheduler).
func newSweepTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	srv := &Server{
		store:             cs,
		agentLifecycleLog: slog.Default(),
	}
	return srv, cs
}

func TestBrokerMessageSweepHandler_FailsPendingMessagesWithMissingRecipient(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// A pending message addressed to an agent ID that does not exist
	// (simulating a deleted/purged recipient) must be failed early by the
	// sweep, without waiting for stuckMessageExpireTTL.
	orphan := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: uuid.NewString(), Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, orphan))
	assert.Equal(t, store.MessageDispatchPending, orphan.DispatchState)

	srv.brokerMessageSweepHandler()(ctx)

	got, err := cs.GetMessage(ctx, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, missingRecipientFailureReason, *got.DispatchFailureReason)
}

// TestBrokerMessageSweepHandler_ExpiresStuckPendingWithSharedReason pins the
// sweep's TTL-expiry reason string to store.MessageExpiredStuckPendingReason
// — the same constant cmd/boot_non_agent_dispatch_state_backfill.go matches
// on to find and repair rows the sweep mislabeled (nc-promote-busy round 3
// M9). Before this constant existed, the two sites carried independent
// literals that could silently drift; this test fails if sweep.go is ever
// edited back to a hand-written string instead of the shared constant.
func TestBrokerMessageSweepHandler_ExpiresStuckPendingWithSharedReason(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	stuck := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:a", Msg: "hi",
		CreatedAt: time.Now().Add(-25 * time.Hour), // past stuckMessageExpireTTL
	}
	require.NoError(t, cs.CreateMessage(ctx, stuck))
	assert.Equal(t, store.MessageDispatchPending, stuck.DispatchState)

	srv.brokerMessageSweepHandler()(ctx)

	got, err := cs.GetMessage(ctx, stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, store.MessageExpiredStuckPendingReason, *got.DispatchFailureReason)
}

// TestBrokerMessageSweepHandler_LogsExpiredMessageFields checks that the
// TTL-expiry line names the expired row by ID, recipient and pending
// duration, and never carries the message body (ptone/scion#5211).
func TestBrokerMessageSweepHandler_LogsExpiredMessageFields(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	var logs bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	const body = "expired-body-sentinel-5211"
	stuck := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:ttl-target", Msg: body,
		CreatedAt: time.Now().Add(-25 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, stuck))

	srv.brokerMessageSweepHandler()(ctx)

	assert.NotContains(t, logs.String(), body, "no log line carries the message body")

	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec))
		if rec["msg"] == "sweep: expired stuck pending messages" {
			lines = append(lines, rec)
		}
	}
	require.Len(t, lines, 1, "one expiry line per sweep")
	rec := lines[0]
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, float64(1), rec["expired"])
	assert.Equal(t, []any{stuck.ID}, rec["message_ids"])
	assert.Equal(t, []any{"agent:ttl-target"}, rec["recipients"])
	assert.Equal(t, false, rec["message_ids_truncated"])
	durations, ok := rec["pending_durations"].([]any)
	require.True(t, ok)
	require.Len(t, durations, 1)
	d, err := time.ParseDuration(durations[0].(string))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, d, 25*time.Hour)
	assert.Less(t, d, 26*time.Hour)
}

// expiryFailingStore fails the TTL expiry and delegates everything else.
type expiryFailingStore struct {
	store.Store
}

func (expiryFailingStore) ExpireAndListStuckPendingMessages(context.Context, time.Time, string) ([]store.ExpiredPendingMessage, error) {
	return nil, errors.New("expiry failed")
}

// TestBrokerMessageSweepHandler_ExpiryErrorStillFailsMissingRecipient checks
// that a failed TTL expiry does not stop the sweep: pending messages whose
// recipient is gone are still failed (ptone/scion#5211).
func TestBrokerMessageSweepHandler_ExpiryErrorStillFailsMissingRecipient(t *testing.T) {
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	var logs bytes.Buffer
	srv := &Server{
		store:             expiryFailingStore{Store: cs},
		agentLifecycleLog: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	orphan := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: uuid.NewString(), Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, orphan))

	srv.brokerMessageSweepHandler()(ctx)

	assert.Contains(t, logs.String(), "sweep: expire stuck pending messages failed", "the expiry error is logged")
	got, err := cs.GetMessage(ctx, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState, "the missing-recipient step still runs after a failed expiry")
}

// TestBrokerMessageSweepHandler_ExpiresAfter10mAndLeavesYoungerPending verifies
// that stuckMessageExpireTTL is 10m (ptone/scion#7405): a pending agent message
// older than 10m transitions to failed instead of degrading health for 24h,
// while one younger than 10m (within legitimate auto-wake / dispatch budgets)
// remains pending.
func TestBrokerMessageSweepHandler_ExpiresAfter10mAndLeavesYoungerPending(t *testing.T) {
	assert.Equal(t, 10*time.Minute, stuckMessageExpireTTL)

	srv, s, project, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	expiredMsg := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     project.ID,
		Sender:        "agent:" + sender.Slug,
		SenderID:      sender.ID,
		Recipient:     "agent:" + target.Slug,
		RecipientID:   target.ID,
		AgentID:       sender.ID,
		Msg:           "stuck-past-10m",
		DispatchState: store.MessageDispatchPending,
		CreatedAt:     time.Now().Add(-11 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, expiredMsg))

	youngerMsg := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     project.ID,
		Sender:        "agent:" + sender.Slug,
		SenderID:      sender.ID,
		Recipient:     "agent:" + target.Slug,
		RecipientID:   target.ID,
		AgentID:       sender.ID,
		Msg:           "still-within-10m-budget",
		DispatchState: store.MessageDispatchPending,
		CreatedAt:     time.Now().Add(-7 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, youngerMsg))

	srv.brokerMessageSweepHandler()(ctx)

	gotExpired, err := s.GetMessage(ctx, expiredMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotExpired.DispatchState)
	require.NotNil(t, gotExpired.DispatchFailureReason)
	assert.Equal(t, store.MessageExpiredStuckPendingReason, *gotExpired.DispatchFailureReason)

	gotYounger, err := s.GetMessage(ctx, youngerMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotYounger.DispatchState,
		"pending message younger than 10m TTL must not be expired prematurely")
}

// TestBrokerMessageSweepHandler_ExpiredPendingNotifiesAgentAndUserSenders
// verifies that expiring stuck pending messages notifies agent senders via a
// DELIVERY_FAILED notice (naming the target agent, never redelivering to the
// target) and re-publishes failed status for human senders (ptone/scion#7405).
func TestBrokerMessageSweepHandler_ExpiredPendingNotifiesAgentAndUserSenders(t *testing.T) {
	srv, s, project, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.SetEventPublisher(events)

	bus := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = bus.Close() }()
	srv.SetMessageBrokerProxy(NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return dispatcher }, slog.Default()))

	sub, unsubscribe := events.Subscribe("agent." + target.ID + ".message")
	defer unsubscribe()

	agentDM := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     project.ID,
		Sender:        "agent:" + sender.Slug,
		SenderID:      sender.ID,
		Recipient:     "agent:" + target.Slug,
		RecipientID:   target.ID,
		AgentID:       sender.ID, // ExecuteAgentDM sets AgentID to SenderAgent.ID
		Msg:           "interrupted-agent-dm",
		Type:          "instruction",
		DispatchState: store.MessageDispatchPending,
		CreatedAt:     time.Now().Add(-11 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, agentDM))

	userMsg := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     project.ID,
		Sender:        "user:alice",
		SenderID:      tid("sweep-user-alice"),
		Recipient:     "agent:" + target.Slug,
		RecipientID:   target.ID,
		AgentID:       target.ID,
		Msg:           "interrupted-user-msg",
		Type:          "instruction",
		DispatchState: store.MessageDispatchPending,
		CreatedAt:     time.Now().Add(-11 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, userMsg))

	srv.brokerMessageSweepHandler()(ctx)

	gotDM, err := s.GetMessage(ctx, agentDM.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotDM.DispatchState)

	gotUser, err := s.GetMessage(ctx, userMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotUser.DispatchState)

	var notices []dispatchCall
	for _, c := range dispatcher.getCalls() {
		assert.NotEqual(t, target.ID, c.Agent.ID, "sweep expiry must never redeliver to target agent")
		if c.Agent.ID == sender.ID {
			notices = append(notices, c)
		}
	}
	require.Len(t, notices, 1, "agent sender must receive exactly one DELIVERY_FAILED notice")
	require.NotNil(t, notices[0].StructuredMessage)
	assert.Equal(t, "DELIVERY_FAILED", notices[0].StructuredMessage.Status)
	assert.Contains(t, notices[0].Message, target.Slug, "notice must name the target agent")
	assert.NotContains(t, notices[0].Message, `agent "`+sender.Slug+`"`, "notice must not mistake sender for target")
	assert.Contains(t, notices[0].Message, store.MessageExpiredStuckPendingReason)
	assert.Empty(t, notices[0].MessageID, "notice itself must not carry a reportable hub message ID")

	select {
	case evt := <-sub:
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, userMsg.ID, payload.ID)
		assert.Equal(t, store.MessageDispatchFailed, payload.DispatchState)
		assert.Equal(t, store.MessageExpiredStuckPendingReason, payload.DispatchFailureReason)
	case <-time.After(2 * time.Second):
		t.Fatal("expected user message failed SSE event on sweep expiry")
	}

	// A second sweep run must be a no-op (rows are already failed) and must not
	// send duplicate DELIVERY_FAILED notices.
	srv.brokerMessageSweepHandler()(ctx)
	var afterSecondSweep int
	for _, c := range dispatcher.getCalls() {
		if c.Agent.ID == sender.ID {
			afterSecondSweep++
		}
	}
	assert.Equal(t, 1, afterSecondSweep, "subsequent sweep runs must not re-notify sender")
}
