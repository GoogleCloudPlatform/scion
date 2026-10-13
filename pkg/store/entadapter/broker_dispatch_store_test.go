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
	require.NoError(t, s.FailBrokerDispatch(ctx, d2.ID, "boom", ""))
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

// TestBackfillNonAgentDispatchState seeds every recipient shape the pre-fix
// bug (and the sweep that later "healed" it) could have left behind —
// "user:", "thread:", and "conv:" recipients, pending or TTL-expired-failed
// — plus a differently-reasoned failure and a genuine agent-recipient
// pending row, and asserts only the non-agent bug shapes are repaired.
func TestBackfillNonAgentDispatchState(t *testing.T) {
	const expiredReason = "expired: stuck in pending state beyond TTL"

	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	seed := func(recipient, msg string, age time.Duration) *store.Message {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "agent:a", Recipient: recipient, Msg: msg,
			CreatedAt: time.Now().Add(-age),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		return m
	}

	// Bug shapes: still pending, or swept to failed with the exact TTL
	// reason, across every non-agent recipient prefix the writer bug could
	// produce (DM, group thread, conv-ref group).
	userPending := seed("user:alice", "reply 1", 2*time.Hour)
	threadPending := seed("thread:space-42", "reply 2", 2*time.Hour)
	convExpiredFailed := seed("conv:"+uuid.NewString(), "reply 3", 30*time.Hour)
	require.NoError(t, cs.MarkMessageFailed(ctx, convExpiredFailed.ID, expiredReason))

	// Negative control 1: failed for a genuine, unrelated reason — only the
	// exact TTL-expiry string is eligible.
	userOtherFailed := seed("user:carol", "reply 4", 30*time.Hour)
	require.NoError(t, cs.MarkMessageFailed(ctx, userOtherFailed.ID, "some unrelated delivery failure"))

	// Negative control 2: an agent-recipient row, genuinely pending. Only a
	// message addressed to an agent is ever legitimately pending.
	agentPending := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "instruction",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, agentPending))

	repaired, err := cs.BackfillNonAgentDispatchState(ctx, expiredReason)
	require.NoError(t, err)
	assert.Equal(t, 3, repaired, "user:, thread:, and conv: bug shapes are all repaired")

	for _, m := range []*store.Message{userPending, threadPending, convExpiredFailed} {
		got, err := cs.GetMessage(ctx, m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, got.DispatchState, "recipient %q", m.Recipient)
		assert.Nil(t, got.DispatchFailureReason, "recipient %q", m.Recipient)
		require.NotNil(t, got.DispatchedAt, "recipient %q", m.Recipient)
		assert.WithinDuration(t, m.CreatedAt, *got.DispatchedAt, time.Second,
			"dispatched_at is backdated to the row's own created time")
	}

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
	repairedAgain, err := cs.BackfillNonAgentDispatchState(ctx, expiredReason)
	require.NoError(t, err)
	assert.Equal(t, 0, repairedAgain, "a second pass finds nothing left to repair")
}

// TestBackfillNonAgentDispatchState_PagesAcrossMultipleBatches shrinks the
// page size and seeds more rows than one page holds, proving the
// self-draining pagination (each page's repaired rows drop out of the
// eligible predicate, so the next call naturally fetches the remainder)
// terminates and repairs every eligible row, not just the first page.
func TestBackfillNonAgentDispatchState_PagesAcrossMultipleBatches(t *testing.T) {
	origPageSize := backfillPageSize
	backfillPageSize = 3
	t.Cleanup(func() { backfillPageSize = origPageSize })

	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	const rowCount = 7 // more than 2x backfillPageSize(3), forcing 3 pages
	ids := make([]string, rowCount)
	for i := 0; i < rowCount; i++ {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "agent:a", Recipient: "user:bulk", Msg: "reply",
			CreatedAt: time.Now().Add(-2 * time.Hour),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		ids[i] = m.ID
	}

	repaired, err := cs.BackfillNonAgentDispatchState(ctx, "expired: stuck in pending state beyond TTL")
	require.NoError(t, err)
	assert.Equal(t, rowCount, repaired, "every row across every page is repaired")

	for _, id := range ids {
		got, err := cs.GetMessage(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, got.DispatchState)
	}
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

func TestBrokerDispatch_FailRecordsResult(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()

	d := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))
	_, err := s.ClaimBrokerDispatch(ctx, d.ID, "hub-1")
	require.NoError(t, err)
	const envelope = `{"brokerError":{"status":429,"body":"{}"}}`
	require.NoError(t, s.FailBrokerDispatch(ctx, d.ID, "boom", envelope))
	got, err := client.BrokerDispatch.Get(ctx, uuid.MustParse(d.ID))
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, got.State)
	assert.Equal(t, "boom", got.Error)
	assert.Equal(t, envelope, got.Result, "result is written with the failed state")

	// The CAS still rejects a row that is not in_progress.
	err = s.FailBrokerDispatch(ctx, d.ID, "again", `{"other":true}`)
	assert.ErrorIs(t, err, store.ErrNotFound)
	got, err = client.BrokerDispatch.Get(ctx, uuid.MustParse(d.ID))
	require.NoError(t, err)
	assert.Equal(t, "boom", got.Error)
	assert.Equal(t, envelope, got.Result)

	// An empty result leaves the column empty.
	d2 := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d2))
	_, err = s.ClaimBrokerDispatch(ctx, d2.ID, "hub-1")
	require.NoError(t, err)
	require.NoError(t, s.FailBrokerDispatch(ctx, d2.ID, "boom", ""))
	got2, err := client.BrokerDispatch.Get(ctx, uuid.MustParse(d2.ID))
	require.NoError(t, err)
	assert.Empty(t, got2.Result)
}

// insertDispatchAt inserts a dispatch and forces its state and updated_at, so
// a test can place a row exactly on either side of a cutoff.
func insertDispatchAt(t *testing.T, client *ent.Client, s *BrokerDispatchStore, state string, updatedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	d := newDispatch(uuid.NewString(), "start")
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))
	require.NoError(t, client.BrokerDispatch.UpdateOneID(uuid.MustParse(d.ID)).
		SetState(state).
		SetUpdatedAt(updatedAt).
		Exec(ctx))
}

// TestCountBrokerDispatchHealth_Boundaries pins both cutoffs: stuck is
// strictly before stuckBefore, failed is at or after failedSince. Times are
// whole seconds so the boundary holds at SQLite and Postgres precision.
func TestCountBrokerDispatchHealth_Boundaries(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	stuckBefore := now.Add(-270 * time.Second)
	failedSince := now.Add(-time.Hour)

	// in_progress: one second older than the cutoff counts, exactly at the
	// cutoff and newer do not.
	insertDispatchAt(t, client, s, store.DispatchStateInProgress, stuckBefore.Add(-time.Second))
	insertDispatchAt(t, client, s, store.DispatchStateInProgress, stuckBefore)
	insertDispatchAt(t, client, s, store.DispatchStateInProgress, now)

	// failed: exactly at the window start and newer count, one second older
	// does not.
	insertDispatchAt(t, client, s, store.DispatchStateFailed, failedSince)
	insertDispatchAt(t, client, s, store.DispatchStateFailed, now)
	insertDispatchAt(t, client, s, store.DispatchStateFailed, failedSince.Add(-time.Second))

	// Other states never count, however old or recent.
	insertDispatchAt(t, client, s, store.DispatchStatePending, stuckBefore.Add(-time.Hour))
	insertDispatchAt(t, client, s, store.DispatchStateDone, stuckBefore.Add(-time.Hour))
	insertDispatchAt(t, client, s, store.DispatchStateDone, now)

	stuck, failed, err := s.CountBrokerDispatchHealth(ctx, stuckBefore, failedSince)
	require.NoError(t, err)
	assert.Equal(t, 1, stuck, "only the in_progress row strictly before stuckBefore is stuck")
	assert.Equal(t, 2, failed, "failed rows at or after failedSince are counted")
}

func TestCountBrokerDispatchHealth_Empty(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewBrokerDispatchStore(client)

	now := time.Now().UTC()
	stuck, failed, err := s.CountBrokerDispatchHealth(context.Background(), now, now.Add(-time.Hour))
	require.NoError(t, err)
	assert.Zero(t, stuck)
	assert.Zero(t, failed)
}
