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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
// DELIVERY_UNCONFIRMED notice (naming the target agent, telling the sender that
// delivery may have succeeded and to check before resending, never claiming
// "delivery failed", and never redelivering to the target) and re-publishes
// delivery_unconfirmed status for human senders (ptone/scion#7405).
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
	require.Len(t, notices, 1, "agent sender must receive exactly one DELIVERY_UNCONFIRMED notice")
	require.NotNil(t, notices[0].StructuredMessage)
	assert.Equal(t, "DELIVERY_UNCONFIRMED", notices[0].StructuredMessage.Status)
	assert.Equal(t, messages.SystemCategoryDeliveryUnconfirmed, notices[0].StructuredMessage.Metadata["system_category"])
	assert.Contains(t, notices[0].Message, target.Slug, "notice must name the target agent")
	assert.NotContains(t, notices[0].Message, `agent "`+sender.Slug+`"`, "notice must not mistake sender for target")
	assert.Contains(t, notices[0].Message, "unconfirmed")
	assert.Contains(t, notices[0].Message, "may have been delivered; check before resending")
	assert.NotContains(t, notices[0].Message, "Message delivery failed", "must never claim definite failure for an expired pending row")
	assert.Empty(t, notices[0].MessageID, "notice itself must not carry a reportable hub message ID")

	select {
	case evt := <-sub:
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, userMsg.ID, payload.ID)
		assert.Equal(t, store.MessageDispatchFailed, payload.DispatchState)
		assert.Equal(t, store.MessageExpiredStuckPendingReason, payload.DispatchFailureReason)
		assert.Equal(t, dispatchFailureCodeDeliveryUnconfirmed, payload.DispatchFailureCode)
	case <-time.After(2 * time.Second):
		t.Fatal("expected user message delivery_unconfirmed SSE event on sweep expiry")
	}

	// A second sweep run must be a no-op (rows are already failed) and must not
	// send duplicate DELIVERY_UNCONFIRMED notices.
	srv.brokerMessageSweepHandler()(ctx)
	var afterSecondSweep int
	for _, c := range dispatcher.getCalls() {
		if c.Agent.ID == sender.ID {
			afterSecondSweep++
		}
	}
	assert.Equal(t, 1, afterSecondSweep, "subsequent sweep runs must not re-notify sender")
}

type backdatePendingCreateStore struct {
	store.Store
	fault    *storeFaultSwitch
	age      time.Duration
	backdate atomic.Bool
	failMark atomic.Bool
}

func (w *backdatePendingCreateStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	if w.fault.Active() && w.backdate.Load() && msg != nil {
		msg.CreatedAt = time.Now().Add(-w.age)
	}
	return w.Store.CreateMessage(ctx, msg)
}

func (w *backdatePendingCreateStore) MarkMessageDispatched(ctx context.Context, id string) (bool, error) {
	if w.fault.Active() && w.failMark.Load() {
		return false, context.DeadlineExceeded
	}
	return w.Store.MarkMessageDispatched(ctx, id)
}

// TestBrokerMessageSweepHandler_ExpiredAC3AndAC4RowsSendUnconfirmedNotice
// verifies the post-#5058 (ptone/scion#5111) and #5104 (ptone/scion#7405)
// interaction for both ambiguous outcomes:
//   - AC-3 (broker accepted, all MarkMessageDispatched retries failed): the row
//     stays pending until the 10m sweep expires it, which sends one
//     DELIVERY_UNCONFIRMED notice ("check before resending", never
//     DELIVERY_FAILED) and never redelivers to the target.
//   - AC-4 (dispatch deadline exceeded mid-flight): ExecuteAgentDM returns
//     AgentDMAmbiguous synchronously (HTTP 202 "ambiguous") and immediately
//     finalizes the row as failed with dispatchOutcomeUnknownReason instead of
//     leaving it pending for the 10m sweep, so a subsequent sweep is a no-op
//     (no duplicate target dispatch, no late sweep notice, reason preserved).
func TestBrokerMessageSweepHandler_ExpiredAC3AndAC4RowsSendUnconfirmedNotice(t *testing.T) {
	for _, tc := range []struct {
		name              string
		setupCase         func(dispatcher *recordingDispatcher, fs *backdatePendingCreateStore)
		wantCalls         int
		wantPreSweepState string
		wantFinalReason   string
		wantSenderNotices int
	}{
		{
			name: "AC-3_broker_accepted_mark_dispatched_failed",
			setupCase: func(_ *recordingDispatcher, fs *backdatePendingCreateStore) {
				fs.backdate.Store(true)
				fs.failMark.Store(true)
			},
			wantCalls:         1,
			wantPreSweepState: store.MessageDispatchPending,
			wantFinalReason:   store.MessageExpiredStuckPendingReason,
			wantSenderNotices: 1,
		},
		{
			name: "AC-4_dispatch_deadline_ambiguous",
			setupCase: func(dispatcher *recordingDispatcher, fs *backdatePendingCreateStore) {
				fs.backdate.Store(true)
				dispatcher.returnErr = context.DeadlineExceeded
			},
			wantCalls:         1,
			wantPreSweepState: store.MessageDispatchFailed,
			wantFinalReason:   dispatchOutcomeUnknownReason,
			wantSenderNotices: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, faultStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *backdatePendingCreateStore {
				return &backdatePendingCreateStore{Store: inner, fault: f, age: 11 * time.Minute}
			})
			ctx := context.Background()

			owner := &store.User{
				ID: tid("r1-owner-" + tc.name), Email: "r1-" + tc.name + "@test.example",
				Role: store.UserRoleMember, Status: "active", Created: time.Now(),
			}
			require.NoError(t, s.CreateUser(ctx, owner))
			ensureHubMembership(ctx, s, owner.ID)
			proj := &store.Project{
				ID: tid("r1-proj-" + tc.name), Name: "r1-" + tc.name, Slug: "r1-" + tc.name,
				OwnerID: owner.ID, CreatedBy: owner.ID,
			}
			require.NoError(t, s.CreateProject(ctx, proj))
			ensureStandingRoot(t, s, proj.ID, owner.ID)
			brokerID := tid("r1-broker-" + tc.name)
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: brokerID, Name: "r1-broker", Slug: "r1-broker-" + tc.name, Status: store.BrokerStatusOnline,
			}))
			sender := &store.Agent{
				ID: tid("r1-sender-" + tc.name), Name: "r1-sender", Slug: "r1-sender-" + tc.name,
				ProjectID: proj.ID, Phase: "running", RuntimeBrokerID: brokerID,
				MessageMode: store.MessageModeProject, Ancestry: []string{owner.ID},
			}
			require.NoError(t, s.CreateAgent(ctx, sender))
			target := &store.Agent{
				ID: tid("r1-target-" + tc.name), Name: "r1-target", Slug: "r1-target-" + tc.name,
				ProjectID: proj.ID, Phase: "running", RuntimeBrokerID: brokerID,
				MessageMode: store.MessageModeProject, Ancestry: []string{owner.ID},
			}
			require.NoError(t, s.CreateAgent(ctx, target))

			dispatcher := &recordingDispatcher{}
			srv.SetDispatcher(dispatcher)
			events := NewChannelEventPublisher()
			defer events.Close()
			srv.SetEventPublisher(events)
			bus := eventbus.NewInProcessEventBus(slog.Default())
			defer func() { _ = bus.Close() }()
			srv.SetMessageBrokerProxy(NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return dispatcher }, slog.Default()))

			fault.Arm()
			tc.setupCase(dispatcher, faultStore)
			res, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "ac-ambiguous-"+tc.name))
			require.Nil(t, dmErr)
			require.NotNil(t, res)
			require.Equal(t, AgentDMAmbiguous, res.Outcome)
			require.NotEmpty(t, res.MessageID)

			preSweepMsg, err := s.GetMessage(ctx, res.MessageID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPreSweepState, preSweepMsg.DispatchState, "unexpected dispatch_state before sweep")

			faultStore.backdate.Store(false)
			faultStore.failMark.Store(false)
			dispatcher.returnErr = nil
			srv.brokerMessageSweepHandler()(ctx)

			postSweepMsg, err := s.GetMessage(ctx, res.MessageID)
			require.NoError(t, err)
			assert.Equal(t, store.MessageDispatchFailed, postSweepMsg.DispatchState)
			require.NotNil(t, postSweepMsg.DispatchFailureReason)
			assert.Equal(t, tc.wantFinalReason, *postSweepMsg.DispatchFailureReason)

			var targetCalls, senderNotices int
			for _, c := range dispatcher.getCalls() {
				switch c.Agent.ID {
				case target.ID:
					targetCalls++
				case sender.ID:
					senderNotices++
					require.NotNil(t, c.StructuredMessage)
					assert.Equal(t, "DELIVERY_UNCONFIRMED", c.StructuredMessage.Status)
					assert.Contains(t, c.Message, "may have been delivered; check before resending")
					assert.NotContains(t, c.Message, "Message delivery failed")
				}
			}
			assert.Equal(t, tc.wantCalls, targetCalls, "sweep must never redeliver to target")
			assert.Equal(t, tc.wantSenderNotices, senderNotices, "unexpected DELIVERY_UNCONFIRMED sender notice count")
		})
	}
}
