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

package entadapter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDispatch(brokerID, op string) *store.BrokerDispatch {
	return &store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: brokerID,
		Op:       op,
	}
}

func TestBrokerDispatch_InsertListPending_OnlyPending(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()
	brokerA := uuid.NewString()
	brokerB := uuid.NewString()

	d1 := newDispatch(brokerA, "start")
	d2 := newDispatch(brokerA, "stop")
	dOther := newDispatch(brokerB, "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d1))
	require.NoError(t, s.InsertBrokerDispatch(ctx, d2))
	require.NoError(t, s.InsertBrokerDispatch(ctx, dOther))
	assert.Equal(t, store.DispatchStatePending, d1.State)

	// Claim d1 -> in_progress; it should drop out of the pending drain.
	claimed, err := s.ClaimBrokerDispatch(ctx, d1.ID, "hub-1")
	require.NoError(t, err)
	assert.True(t, claimed)

	pending, err := s.ListPendingDispatch(ctx, brokerA)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, d2.ID, pending[0].ID, "drain returns only pending rows for the broker")
}

func TestBrokerDispatch_ClaimOnceThenFalse(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()

	d := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))

	claimed, err := s.ClaimBrokerDispatch(ctx, d.ID, "hub-1")
	require.NoError(t, err)
	assert.True(t, claimed)

	again, err := s.ClaimBrokerDispatch(ctx, d.ID, "hub-2")
	require.NoError(t, err)
	assert.False(t, again, "a second claim of a non-pending row must lose")
}

func TestBrokerDispatch_ConcurrentClaimSingleWinner(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()

	d := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			won, err := s.ClaimBrokerDispatch(ctx, d.ID, "hub")
			if err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins, "exactly one concurrent claim must win (exactly-once execution)")
}

func TestBrokerDispatch_CompleteAndFail(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()

	d := newDispatch(uuid.NewString(), "check_prompt")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))
	_, err := s.ClaimBrokerDispatch(ctx, d.ID, "hub-1")
	require.NoError(t, err)

	require.NoError(t, s.CompleteBrokerDispatch(ctx, d.ID, `{"ok":true}`))
	got, err := client.BrokerDispatch.Get(ctx, uuid.MustParse(d.ID))
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateDone, got.State)
	assert.Equal(t, `{"ok":true}`, got.Result)

	d2 := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d2))
	_, err = s.ClaimBrokerDispatch(ctx, d2.ID, "hub-1")
	require.NoError(t, err)
	require.NoError(t, s.FailBrokerDispatch(ctx, d2.ID, "boom"))
	got2, err := client.BrokerDispatch.Get(ctx, uuid.MustParse(d2.ID))
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, got2.State)
	assert.Equal(t, "boom", got2.Error)
	assert.Equal(t, 1, got2.Attempts, "failure bumps the attempt counter")
}

func TestMarkMessageDispatched_Dedupe(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	msg := &store.Message{
		ID:        uuid.NewString(),
		ProjectID: uuid.NewString(),
		Sender:    "user:alice",
		Recipient: "agent:bob",
		Msg:       "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, msg))
	assert.Equal(t, store.MessageDispatchPending, msg.DispatchState)

	ok, err := cs.MarkMessageDispatched(ctx, msg.ID)
	require.NoError(t, err)
	assert.True(t, ok)

	again, err := cs.MarkMessageDispatched(ctx, msg.ID)
	require.NoError(t, err)
	assert.False(t, again, "second dispatch CAS must dedupe")

	got, err := cs.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, got.DispatchState)
	require.NotNil(t, got.DispatchedAt)
}

func TestListPendingMessages_ByBrokerAgent(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()
	brokerA := uuid.NewString()
	brokerB := uuid.NewString()

	// A project and two agents, one per broker.
	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8], OwnerID: uuid.NewString()}
	require.NoError(t, cs.CreateProject(ctx, proj))
	projUID := uuid.MustParse(proj.ID)
	agentA := mustCreateAgent(t, client, projUID, brokerA)
	agentB := mustCreateAgent(t, client, projUID, brokerB)

	// Pending message to agentA (on brokerA), and one to agentB (on brokerB).
	msgA := &store.Message{ID: uuid.NewString(), ProjectID: proj.ID, Sender: "user:x", Recipient: "agent:a", Msg: "for A", AgentID: agentA}
	msgB := &store.Message{ID: uuid.NewString(), ProjectID: proj.ID, Sender: "user:x", Recipient: "agent:b", Msg: "for B", AgentID: agentB}
	require.NoError(t, cs.CreateMessage(ctx, msgA))
	require.NoError(t, cs.CreateMessage(ctx, msgB))

	pending, err := cs.ListPendingMessages(ctx, brokerA)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, msgA.ID, pending[0].ID, "only the message for an agent on brokerA")

	// Once dispatched, it drops out of the pending set.
	_, err = cs.MarkMessageDispatched(ctx, msgA.ID)
	require.NoError(t, err)
	pending, err = cs.ListPendingMessages(ctx, brokerA)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestCountStuckPendingMessages(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// A message created 10 minutes ago (stuck).
	oldMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, cs.CreateMessage(ctx, oldMsg))
	assert.Equal(t, store.MessageDispatchPending, oldMsg.DispatchState)

	// A message created just now (not stuck).
	newMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "new",
	}
	require.NoError(t, cs.CreateMessage(ctx, newMsg))

	cutoff := time.Now().Add(-5 * time.Minute)
	count, err := cs.CountStuckPendingMessages(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the old message is stuck")

	// Dispatch the old message — it should no longer be stuck.
	_, err = cs.MarkMessageDispatched(ctx, oldMsg.ID)
	require.NoError(t, err)
	count, err = cs.CountStuckPendingMessages(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "dispatched message is not stuck")
}

func TestExpireStuckPendingMessages(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// A message created 25 hours ago (past TTL).
	expiredMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		CreatedAt: time.Now().Add(-25 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, expiredMsg))

	// A message created 10 minutes ago (within TTL, but past stuck threshold).
	recentMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "recent",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, cs.CreateMessage(ctx, recentMsg))

	// A message created just now (fresh).
	freshMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:c", Msg: "fresh",
	}
	require.NoError(t, cs.CreateMessage(ctx, freshMsg))

	ttlCutoff := time.Now().Add(-24 * time.Hour)
	reason := "expired: stuck in pending state beyond TTL"
	expired, err := cs.ExpireStuckPendingMessages(ctx, ttlCutoff, reason)
	require.NoError(t, err)
	assert.Equal(t, 1, expired, "only the 25h-old message should be expired")

	got, err := cs.GetMessage(ctx, expiredMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)

	gotRecent, err := cs.GetMessage(ctx, recentMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotRecent.DispatchState, "recent message still pending")

	gotFresh, err := cs.GetMessage(ctx, freshMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotFresh.DispatchState, "fresh message still pending")

	// Running again should expire 0.
	expired, err = cs.ExpireStuckPendingMessages(ctx, ttlCutoff, reason)
	require.NoError(t, err)
	assert.Equal(t, 0, expired, "already-expired message not counted again")
}

// TestCountStuckPendingMessages_ExcludesUserRecipients is a regression test
// for nc-promote-busy round 2 (R1): only a message addressed to an agent is
// ever actually dispatched through the broker/runtime, so only that row can
// be genuinely "stuck". A "user:" recipient row landing in dispatch_state
// "pending" is always a writer bug (e.g. the deliverToUser omission fixed by
// nc-promote-busy), not a stalled dispatch, and must not be counted here —
// counting it would let the sweep "heal" the bug into a silent data loss
// instead of surfacing it.
func TestCountStuckPendingMessages_ExcludesUserRecipients(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// An old pending row addressed to a user — must never be counted stuck.
	userMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:alice", Msg: "old",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, cs.CreateMessage(ctx, userMsg))

	// An old pending row addressed to an agent — still counted stuck.
	agentMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "old",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	require.NoError(t, cs.CreateMessage(ctx, agentMsg))

	cutoff := time.Now().Add(-5 * time.Minute)
	count, err := cs.CountStuckPendingMessages(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the agent-recipient row is counted stuck")
}

// TestExpireStuckPendingMessages_SkipsUserRecipients is the ExpireStuck
// counterpart of TestCountStuckPendingMessages_ExcludesUserRecipients: a
// user-recipient pending row past the TTL must survive untouched (not be
// flipped to failed, which would put it on the PurgeFailedMessages clock).
func TestExpireStuckPendingMessages_SkipsUserRecipients(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	userMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:alice", Msg: "old",
		CreatedAt: time.Now().Add(-25 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, userMsg))

	agentMsg := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "old",
		CreatedAt: time.Now().Add(-25 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, agentMsg))

	ttlCutoff := time.Now().Add(-24 * time.Hour)
	reason := "expired: stuck in pending state beyond TTL"
	expired, err := cs.ExpireStuckPendingMessages(ctx, ttlCutoff, reason)
	require.NoError(t, err)
	assert.Equal(t, 1, expired, "only the agent-recipient row is expired")

	gotUser, err := cs.GetMessage(ctx, userMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotUser.DispatchState,
		"user-recipient row must survive untouched, not be flipped to failed")

	gotAgent, err := cs.GetMessage(ctx, agentMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotAgent.DispatchState,
		"agent-recipient row is still expired")
}

// TestBackfillUserRecipientDispatchState is the regression test for
// nc-promote-busy round 2 (R3): it seeds one row of each shape the pre-fix
// bug (and the sweep that later "healed" it) could have left behind, plus a
// genuine agent-recipient dispatch failure that must never be touched, and
// asserts only the two nc-promote-busy shapes change.
func TestBackfillUserRecipientDispatchState(t *testing.T) {
	const expiredReason = "expired: stuck in pending state beyond TTL"

	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// Shape 1: a user-recipient row still exactly as the bug left it —
	// dispatch_state defaults to "pending" because nothing stamped it.
	userPending := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:alice", Msg: "reply 1",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, userPending))

	// Shape 2: the same bug, but the stuck-message sweep has since flipped
	// it to "failed" with the exact TTL-expiry reason.
	userExpiredFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:bob", Msg: "reply 2",
		CreatedAt: time.Now().Add(-30 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, userExpiredFailed))
	require.NoError(t, cs.MarkMessageFailed(ctx, userExpiredFailed.ID, expiredReason))

	// Negative control 1: a user-recipient row that failed for a genuine,
	// unrelated reason. Must stay failed with its own reason untouched —
	// only the exact TTL-expiry string is eligible.
	userOtherFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:carol", Msg: "reply 3",
		CreatedAt: time.Now().Add(-30 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, userOtherFailed))
	require.NoError(t, cs.MarkMessageFailed(ctx, userOtherFailed.ID, "some unrelated delivery failure"))

	// Negative control 2: an agent-recipient row, genuinely pending. Only a
	// "user:" recipient is ever eligible for this backfill.
	agentPending := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "instruction",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, agentPending))

	repaired, err := cs.BackfillUserRecipientDispatchState(ctx, expiredReason)
	require.NoError(t, err)
	assert.Equal(t, 2, repaired, "only the two nc-promote-busy shapes are repaired")

	gotPending, err := cs.GetMessage(ctx, userPending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, gotPending.DispatchState)
	assert.Nil(t, gotPending.DispatchFailureReason)
	require.NotNil(t, gotPending.DispatchedAt)
	assert.WithinDuration(t, userPending.CreatedAt, *gotPending.DispatchedAt, time.Second,
		"dispatched_at is backdated to the row's own created time")

	gotExpiredFailed, err := cs.GetMessage(ctx, userExpiredFailed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, gotExpiredFailed.DispatchState)
	assert.Nil(t, gotExpiredFailed.DispatchFailureReason)
	require.NotNil(t, gotExpiredFailed.DispatchedAt)

	gotOtherFailed, err := cs.GetMessage(ctx, userOtherFailed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotOtherFailed.DispatchState,
		"a genuine, differently-reasoned failure must never be repaired")
	require.NotNil(t, gotOtherFailed.DispatchFailureReason)
	assert.Equal(t, "some unrelated delivery failure", *gotOtherFailed.DispatchFailureReason)

	gotAgentPending, err := cs.GetMessage(ctx, agentPending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotAgentPending.DispatchState,
		"an agent-recipient row is never touched by this backfill")

	// Idempotent: running again repairs nothing further.
	repairedAgain, err := cs.BackfillUserRecipientDispatchState(ctx, expiredReason)
	require.NoError(t, err)
	assert.Equal(t, 0, repairedAgain, "a second pass finds nothing left to repair")
}

func TestFailPendingMessagesWithMissingRecipient(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	projUID := uuid.MustParse(proj.ID)

	// A live agent — its pending message must be left alone.
	aliveID := mustCreateAgent(t, client, projUID, "broker-1")

	// A soft-deleted agent — its pending message must be failed early.
	deletedID := mustCreateAgent(t, client, projUID, "broker-1")
	_, err := client.Agent.UpdateOneID(uuid.MustParse(deletedID)).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)

	// A recipient_id that never existed (e.g. hard-deleted/purged) — also failed early.
	goneID := uuid.NewString()

	toAlive := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:alive", RecipientID: aliveID, Msg: "hi",
	}
	toDeleted := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:deleted", RecipientID: deletedID, Msg: "hi",
	}
	toGone := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: goneID, Msg: "hi",
	}
	// A pending message to a human recipient must never be treated as an
	// orphaned agent DM, even though its recipient_id resolves to nothing.
	toUser := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:alive", Recipient: "user:carol", RecipientID: "carol-user-id", Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, toAlive))
	require.NoError(t, cs.CreateMessage(ctx, toDeleted))
	require.NoError(t, cs.CreateMessage(ctx, toGone))
	require.NoError(t, cs.CreateMessage(ctx, toUser))

	reason := "recipient agent no longer exists"
	failed, err := cs.FailPendingMessagesWithMissingRecipient(ctx, reason)
	require.NoError(t, err)
	assert.Equal(t, 2, failed, "the soft-deleted and never-existed recipients are failed")

	gotAlive, err := cs.GetMessage(ctx, toAlive.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotAlive.DispatchState, "live recipient untouched")

	gotDeleted, err := cs.GetMessage(ctx, toDeleted.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotDeleted.DispatchState)
	require.NotNil(t, gotDeleted.DispatchFailureReason)
	assert.Equal(t, reason, *gotDeleted.DispatchFailureReason)

	gotGone, err := cs.GetMessage(ctx, toGone.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotGone.DispatchState)

	gotUser, err := cs.GetMessage(ctx, toUser.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotUser.DispatchState, "human recipient never touched")

	// Running again should fail 0 (already-failed rows are excluded by the
	// dispatch_state=pending guard).
	failed, err = cs.FailPendingMessagesWithMissingRecipient(ctx, reason)
	require.NoError(t, err)
	assert.Equal(t, 0, failed, "already-failed messages not counted again")
}

func mustCreateAgent(t *testing.T, client *ent.Client, projectID uuid.UUID, brokerID string) string {
	t.Helper()
	a, err := client.Agent.Create().
		SetSlug("agent-" + uuid.NewString()[:8]).
		SetName("agent").
		SetProjectID(projectID).
		SetRuntimeBrokerID(brokerID).
		Save(context.Background())
	require.NoError(t, err)
	return a.ID.String()
}
