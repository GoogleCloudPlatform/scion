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
