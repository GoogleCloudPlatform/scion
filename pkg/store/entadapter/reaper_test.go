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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// ReapStaleBrokerAffinity
// ---------------------------------------------------------------------------

func TestReapStaleBrokerAffinity_ClearsStaleOnly(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	// Broker with stale heartbeat + affinity → should be reaped.
	stale := newBroker()
	stale.Status = store.BrokerStatusOnline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, stale))
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, stale.ID, "hub-old", "sess-old"))
	// Backdate heartbeat to make it stale.
	_, err := ps.client.RuntimeBroker.UpdateOneID(uuid.MustParse(stale.ID)).
		SetLastHeartbeat(time.Now().Add(-10 * time.Minute)).Save(ctx)
	require.NoError(t, err)

	// Broker with fresh heartbeat + affinity → should NOT be reaped.
	fresh := newBroker()
	fresh.Status = store.BrokerStatusOnline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, fresh))
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, fresh.ID, "hub-alive", "sess-alive"))

	// Broker with no affinity (NULL connected_hub_id) → should NOT be reaped.
	noAffinity := newBroker()
	noAffinity.Status = store.BrokerStatusOffline
	require.NoError(t, ps.CreateRuntimeBroker(ctx, noAffinity))

	cleared, err := ps.ReapStaleBrokerAffinity(ctx, time.Now().Add(-3*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, cleared)

	// Verify stale broker's affinity was cleared.
	got, err := ps.GetRuntimeBroker(ctx, stale.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ConnectedHubID)
	assert.Nil(t, got.ConnectedSessionID)
	assert.Nil(t, got.ConnectedAt)

	// Verify fresh broker's affinity is intact.
	got, err = ps.GetRuntimeBroker(ctx, fresh.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ConnectedHubID)
	assert.Equal(t, "hub-alive", *got.ConnectedHubID)

	// Verify no-affinity broker is untouched.
	got, err = ps.GetRuntimeBroker(ctx, noAffinity.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ConnectedHubID)
}

func TestReapStaleBrokerAffinity_NothingToReap(t *testing.T) {
	ps := newTestProjectStore(t)
	ctx := context.Background()

	b := newBroker()
	require.NoError(t, ps.CreateRuntimeBroker(ctx, b))
	require.NoError(t, ps.ClaimRuntimeBrokerConnection(ctx, b.ID, "hub-1", "sess-1"))

	cleared, err := ps.ReapStaleBrokerAffinity(ctx, time.Now().Add(-10*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, cleared)
}

// ---------------------------------------------------------------------------
// ReapStuckDispatch
// ---------------------------------------------------------------------------

func TestReapStuckDispatch_RedrivesBelowMax(t *testing.T) {
	client := enttest.NewClient(t)
	ds := NewBrokerDispatchStore(client)
	ctx := context.Background()
	brokerID := uuid.NewString()

	d := newDispatch(brokerID, "start")
	require.NoError(t, ds.InsertBrokerDispatch(ctx, d))
	claimed, err := ds.ClaimBrokerDispatch(ctx, d.ID, "hub-1")
	require.NoError(t, err)
	require.True(t, claimed)

	// Backdate updated_at to make it stuck.
	_, err = client.BrokerDispatch.UpdateOneID(uuid.MustParse(d.ID)).
		SetUpdatedAt(time.Now().Add(-10 * time.Minute)).Save(ctx)
	require.NoError(t, err)

	requeued, failed, err := ds.ReapStuckDispatch(ctx, time.Now().Add(-5*time.Minute), 3)
	require.NoError(t, err)
	assert.Equal(t, 1, requeued)
	assert.Equal(t, 0, failed)

	got, err := ds.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStatePending, got.State)
	assert.Equal(t, "", got.ClaimedBy)
	assert.Equal(t, 1, got.Attempts)
}

func TestReapStuckDispatch_FailsAtMaxAttempts(t *testing.T) {
	client := enttest.NewClient(t)
	ds := NewBrokerDispatchStore(client)
	ctx := context.Background()
	brokerID := uuid.NewString()

	d := newDispatch(brokerID, "stop")
	d.State = store.DispatchStateInProgress
	require.NoError(t, ds.InsertBrokerDispatch(ctx, d))

	// Set attempts to maxAttempts.
	_, err := client.BrokerDispatch.UpdateOneID(uuid.MustParse(d.ID)).
		SetAttempts(3).
		SetUpdatedAt(time.Now().Add(-10 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	requeued, failed, err := ds.ReapStuckDispatch(ctx, time.Now().Add(-5*time.Minute), 3)
	require.NoError(t, err)
	assert.Equal(t, 0, requeued)
	assert.Equal(t, 1, failed)

	got, err := ds.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, got.State)
	assert.Contains(t, got.Error, "max attempts exceeded")
}

func TestReapStuckDispatch_LeavesFreshAndTerminal(t *testing.T) {
	client := enttest.NewClient(t)
	ds := NewBrokerDispatchStore(client)
	ctx := context.Background()
	brokerID := uuid.NewString()

	// Fresh in_progress dispatch (updated recently) → should NOT be reaped.
	fresh := newDispatch(brokerID, "start")
	require.NoError(t, ds.InsertBrokerDispatch(ctx, fresh))
	claimed, err := ds.ClaimBrokerDispatch(ctx, fresh.ID, "hub-1")
	require.NoError(t, err)
	require.True(t, claimed)

	// Done dispatch → should NOT be reaped.
	done := newDispatch(brokerID, "stop")
	require.NoError(t, ds.InsertBrokerDispatch(ctx, done))
	_, err = ds.ClaimBrokerDispatch(ctx, done.ID, "hub-1")
	require.NoError(t, err)
	require.NoError(t, ds.CompleteBrokerDispatch(ctx, done.ID, `{"ok":true}`))

	// Pending dispatch → should NOT be reaped (only in_progress is targeted).
	pending := newDispatch(brokerID, "restart")
	require.NoError(t, ds.InsertBrokerDispatch(ctx, pending))

	requeued, failed, err := ds.ReapStuckDispatch(ctx, time.Now().Add(-5*time.Minute), 3)
	require.NoError(t, err)
	assert.Equal(t, 0, requeued)
	assert.Equal(t, 0, failed)

	// Verify states are unchanged.
	got, _ := ds.GetBrokerDispatch(ctx, fresh.ID)
	assert.Equal(t, store.DispatchStateInProgress, got.State)

	got, _ = ds.GetBrokerDispatch(ctx, done.ID)
	assert.Equal(t, store.DispatchStateDone, got.State)

	got, _ = ds.GetBrokerDispatch(ctx, pending.ID)
	assert.Equal(t, store.DispatchStatePending, got.State)
}

func TestReapStuckDispatch_PastDeadline(t *testing.T) {
	client := enttest.NewClient(t)
	ds := NewBrokerDispatchStore(client)
	ctx := context.Background()
	brokerID := uuid.NewString()

	d := newDispatch(brokerID, "start")
	pastDeadline := time.Now().Add(-1 * time.Minute)
	d.DeadlineAt = &pastDeadline
	d.State = store.DispatchStateInProgress
	require.NoError(t, ds.InsertBrokerDispatch(ctx, d))

	// updated_at is recent (within threshold), but deadline_at is past.
	requeued, failed, err := ds.ReapStuckDispatch(ctx, time.Now().Add(-10*time.Minute), 3)
	require.NoError(t, err)
	assert.Equal(t, 1, requeued)
	assert.Equal(t, 0, failed)

	got, err := ds.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStatePending, got.State)
}
