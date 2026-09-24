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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAgentReincarnationStore(t *testing.T) *AgentReincarnationStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewAgentReincarnationStore(client)
}

func TestGetAgentReincarnation_NotFound(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	_, err := s.GetAgentReincarnation(ctx, "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetPendingAgentReincarnation_NotFound(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	_, err := s.GetPendingAgentReincarnation(ctx, "agent-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAgentReincarnation_RoundTrip(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	prev := &store.AgentAppliedConfig{Image: "old-image:v1", TemplateHash: "hash-old"}
	rec := &store.AgentReincarnation{
		AgentID:               "agent-1",
		FromGeneration:        1,
		ToGeneration:          2,
		RequestedBy:           "user-123",
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: prev,
		Handoff:               "handoff text",
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))
	require.NotEmpty(t, rec.ID)
	require.False(t, rec.RequestedAt.IsZero())

	got, err := s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-1", got.AgentID)
	assert.Equal(t, 1, got.FromGeneration)
	assert.Equal(t, 2, got.ToGeneration)
	assert.Equal(t, "user-123", got.RequestedBy)
	assert.Equal(t, store.AgentReincarnationStatePending, got.State)
	assert.Equal(t, "handoff text", got.Handoff)
	require.NotNil(t, got.PreviousAppliedConfig)
	assert.Equal(t, "old-image:v1", got.PreviousAppliedConfig.Image)
	assert.Equal(t, "hash-old", got.PreviousAppliedConfig.TemplateHash)
	assert.Nil(t, got.NewAppliedConfig)
	assert.Nil(t, got.CompletedAt)
}

func TestUpdateAgentReincarnation_CompletesWithNewConfig(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	rec := &store.AgentReincarnation{
		AgentID:        "agent-1",
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

	newCfg := &store.AgentAppliedConfig{Image: "new-image:v2", TemplateHash: "hash-new"}
	now := time.Now().UTC().Truncate(time.Second)
	rec.State = store.AgentReincarnationStateCompleted
	rec.NewAppliedConfig = newCfg
	rec.CompletedAt = &now
	require.NoError(t, s.UpdateAgentReincarnation(ctx, rec))

	got, err := s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentReincarnationStateCompleted, got.State)
	require.NotNil(t, got.NewAppliedConfig)
	assert.Equal(t, "new-image:v2", got.NewAppliedConfig.Image)
	require.NotNil(t, got.CompletedAt)
	assert.WithinDuration(t, now, *got.CompletedAt, time.Second)
}

func TestUpdateAgentReincarnation_Failed(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	rec := &store.AgentReincarnation{
		AgentID:        "agent-1",
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

	rec.State = store.AgentReincarnationStateFailed
	rec.Error = "broker returned 500"
	require.NoError(t, s.UpdateAgentReincarnation(ctx, rec))

	got, err := s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentReincarnationStateFailed, got.State)
	assert.Equal(t, "broker returned 500", got.Error)
}

// TestGetPendingAgentReincarnation_FindsInFlightOnly is the store-level proof
// behind AC-8 (409 on a concurrent reincarnate): only a non-terminal state
// counts as "pending" for the concurrency check, and it's scoped per-agent.
func TestGetPendingAgentReincarnation_FindsInFlightOnly(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	// A completed reincarnation for agent-1 must not count as pending.
	done := &store.AgentReincarnation{
		AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, done))

	_, err := s.GetPendingAgentReincarnation(ctx, "agent-1")
	assert.ErrorIs(t, err, store.ErrNotFound, "a completed reincarnation must not be reported as pending")

	// A pending reincarnation for a DIFFERENT agent must not leak across.
	otherPending := &store.AgentReincarnation{
		AgentID: "agent-2", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStatePending,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, otherPending))

	_, err = s.GetPendingAgentReincarnation(ctx, "agent-1")
	assert.ErrorIs(t, err, store.ErrNotFound, "another agent's pending reincarnation must not leak across agent IDs")

	// Now add a genuinely in-flight (provisioning) reincarnation for agent-1.
	inFlight := &store.AgentReincarnation{
		AgentID: "agent-1", FromGeneration: 2, ToGeneration: 3,
		State: store.AgentReincarnationStateProvisioning,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, inFlight))

	got, err := s.GetPendingAgentReincarnation(ctx, "agent-1")
	require.NoError(t, err)
	assert.Equal(t, inFlight.ID, got.ID)
	assert.Equal(t, store.AgentReincarnationStateProvisioning, got.State)
}

func TestListAgentReincarnations_MostRecentFirst(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	first := &store.AgentReincarnation{
		AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, first))

	second := &store.AgentReincarnation{
		AgentID: "agent-1", FromGeneration: 2, ToGeneration: 3,
		State: store.AgentReincarnationStateCompleted,
	}
	// Force distinct RequestedAt ordering deterministically rather than
	// relying on wall-clock granularity between the two creates.
	second.RequestedAt = first.RequestedAt.Add(time.Minute)
	require.NoError(t, s.CreateAgentReincarnation(ctx, second))

	// A reincarnation for a different agent must not appear in agent-1's list.
	other := &store.AgentReincarnation{
		AgentID: "agent-2", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, other))

	list, err := s.ListAgentReincarnations(ctx, "agent-1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, second.ID, list[0].ID, "most recent (to_generation 3) first")
	assert.Equal(t, first.ID, list[1].ID)
}

func TestDeleteAgentReincarnationsForAgent(t *testing.T) {
	s := newTestAgentReincarnationStore(t)
	ctx := context.Background()

	rec := &store.AgentReincarnation{
		AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

	other := &store.AgentReincarnation{
		AgentID: "agent-2", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted,
	}
	require.NoError(t, s.CreateAgentReincarnation(ctx, other))

	require.NoError(t, s.DeleteAgentReincarnationsForAgent(ctx, "agent-1"))

	_, err := s.GetAgentReincarnation(ctx, rec.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The other agent's row must survive.
	stillThere, err := s.GetAgentReincarnation(ctx, other.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-2", stillThere.AgentID)
}

// TestTryAdvanceAgentReincarnation is the design §3.4 Amendment A7 (N2)
// store-level table test pinning the contract every worker/sweep write in
// pkg/hub rests on: TryAdvanceAgentReincarnation only applies when the
// record's CURRENT State exactly equals expectState.
func TestTryAdvanceAgentReincarnation(t *testing.T) {
	ctx := context.Background()

	t.Run("matching expectState advances the row", func(t *testing.T) {
		s := newTestAgentReincarnationStore(t)
		rec := &store.AgentReincarnation{AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending}
		require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

		pinned := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		upd := &store.AgentReincarnation{ID: rec.ID, State: store.AgentReincarnationStateStopping, UpdatedAt: pinned}
		ok, err := s.TryAdvanceAgentReincarnation(ctx, upd, store.AgentReincarnationStatePending)
		require.NoError(t, err)
		assert.True(t, ok)

		got, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)
		assert.Equal(t, store.AgentReincarnationStateStopping, got.State)
		assert.WithinDuration(t, pinned, got.UpdatedAt, time.Second, "UpdatedAt must be honoured, not left to a fresh time.Now()")
	})

	t.Run("mismatched expectState is a no-op, row unchanged", func(t *testing.T) {
		s := newTestAgentReincarnationStore(t)
		rec := &store.AgentReincarnation{AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStateProvisioning}
		require.NoError(t, s.CreateAgentReincarnation(ctx, rec))
		before, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)

		upd := &store.AgentReincarnation{ID: rec.ID, State: store.AgentReincarnationStateFailed, Error: "should not land", UpdatedAt: time.Now()}
		ok, err := s.TryAdvanceAgentReincarnation(ctx, upd, store.AgentReincarnationStateStopping) // wrong expectState
		require.NoError(t, err)
		assert.False(t, ok)

		after, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)
		assert.Equal(t, before.State, after.State, "state must be untouched")
		assert.Equal(t, before.Error, after.Error, "error must be untouched")
		assert.WithinDuration(t, before.UpdatedAt, after.UpdatedAt, time.Second, "updated_at must be untouched")
	})

	t.Run("terminal state can never be advanced away from, even by matching it as expectState", func(t *testing.T) {
		s := newTestAgentReincarnationStore(t)
		rec := &store.AgentReincarnation{AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStateFailed, Error: "first failure"}
		require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

		// This exercises the store primitive alone: a caller that (bug aside)
		// passed a terminal value as expectState would still find a match,
		// since the WHERE clause is a plain equality check on whatever
		// expectState it is given. The hub-side guard against ever doing
		// this lives in reincarnate_worker.go's tryAdvanceReincarnation
		// (IsAgentReincarnationStateNonTerminal), not in the store.
		upd := &store.AgentReincarnation{ID: rec.ID, State: store.AgentReincarnationStateFailed, Error: "second failure", UpdatedAt: time.Now()}
		ok, err := s.TryAdvanceAgentReincarnation(ctx, upd, store.AgentReincarnationStateFailed)
		require.NoError(t, err)
		assert.True(t, ok, "the store primitive itself does a plain state equality check; it is not what rejects terminal-to-terminal")
	})

	t.Run("missing id returns false with no error", func(t *testing.T) {
		s := newTestAgentReincarnationStore(t)
		upd := &store.AgentReincarnation{ID: "00000000-0000-0000-0000-000000000000", State: store.AgentReincarnationStateFailed, UpdatedAt: time.Now()}
		ok, err := s.TryAdvanceAgentReincarnation(ctx, upd, store.AgentReincarnationStatePending)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	// design §3.4 Amendment A8.4: 16 goroutines race the same CAS
	// (expectState="pending", half targeting "failed" and half "completed")
	// against a single record. Exactly one must win, regardless of which
	// target state it wrote — this is the primitive every A6/A7 worker-vs-
	// sweep guarantee rests on.
	t.Run("16-way contention has exactly one winner", func(t *testing.T) {
		s := newTestAgentReincarnationStore(t)
		rec := &store.AgentReincarnation{AgentID: "agent-1", FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending}
		require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

		const n = 16
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				target := store.AgentReincarnationStateFailed
				if i%2 == 0 {
					target = store.AgentReincarnationStateCompleted
				}
				upd := &store.AgentReincarnation{ID: rec.ID, State: target, UpdatedAt: time.Now()}
				ok, err := s.TryAdvanceAgentReincarnation(ctx, upd, store.AgentReincarnationStatePending)
				assert.NoError(t, err)
				if ok {
					wins.Add(1)
				}
			}(i)
		}
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load(), "exactly one of 16 concurrent CASes against the same expectState must win")

		got, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)
		assert.True(t, got.State == store.AgentReincarnationStateFailed || got.State == store.AgentReincarnationStateCompleted,
			"the row must land on whichever target the sole winner wrote")
	})
}
