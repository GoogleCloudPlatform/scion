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
	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8], Visibility: store.VisibilityPrivate, OwnerID: uuid.NewString()}
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
		Visibility: store.VisibilityPrivate, OwnerID: uuid.NewString(),
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
		Visibility: store.VisibilityPrivate, OwnerID: uuid.NewString(),
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
