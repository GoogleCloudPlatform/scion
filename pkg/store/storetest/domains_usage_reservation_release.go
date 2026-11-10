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

package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseFixture is one agent with an active broker reservation.
type releaseFixture struct {
	s      store.Store
	def    string
	broker string
	agent  *store.Agent
	res    *store.UsageReservation
}

// newReleaseFixture creates a project and an agent in phase with container
// status containerStatus and run ID runID, and an active broker reservation
// for it.
func newReleaseFixture(t *testing.T, ctx context.Context, s store.Store, phase, containerStatus, runID string) *releaseFixture {
	t.Helper()
	def := seedReservationLimit(t, ctx, s)
	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "rel-" + projectID[:8], Slug: "rel-" + projectID[:8]}))
	agentID := uuid.NewString()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Slug:            "rel-" + agentID[:8],
		Name:            "rel",
		Template:        "default",
		ProjectID:       projectID,
		Phase:           phase,
		ContainerStatus: containerStatus,
	}))
	if runID != "" {
		_, err := s.SetAgentRunID(ctx, agentID, runID, nil)
		require.NoError(t, err)
	}
	broker := uuid.NewString()
	res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, broker, agentID))
	require.NoError(t, err)
	f := &releaseFixture{s: s, def: def, broker: broker, res: res}
	f.reload(t, ctx)
	return f
}

func (f *releaseFixture) reload(t *testing.T, ctx context.Context) {
	t.Helper()
	a, err := f.s.GetAgent(ctx, f.res.ResourceID)
	require.NoError(t, err)
	f.agent = a
}

// active reports whether the agent still holds an active reservation.
func (f *releaseFixture) active(t *testing.T, ctx context.Context) bool {
	t.Helper()
	_, err := f.s.GetActiveReservation(ctx, f.def, f.agent.ID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

// ReservationReleaseConformance checks the conditional releases
// ReleaseReservationIfAgent (each condition field) and
// ReleaseReservationIfScopeBrokerAbsent on whichever backend factory
// selects.
func ReservationReleaseConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("release_if_agent", func(t *testing.T) {
		t.Run("missing agent is not found", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
				released, err := s.ReleaseReservationIfAgent(ctx, def, id, store.AgentReleaseCond{})
				assert.ErrorIs(t, err, store.ErrNotFound, id)
				assert.False(t, released, id)
			}
		})

		t.Run("expected run ID", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", "", "run-1")

			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-0"})
			require.NoError(t, err)
			assert.False(t, released, "another run was minted since the read")
			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{})
			require.NoError(t, err)
			assert.False(t, released, "an empty expected run ID does not match a set run")
			assert.True(t, f.active(t, ctx))

			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1"})
			require.NoError(t, err)
			assert.True(t, released)
			assert.False(t, f.active(t, ctx))

			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1"})
			require.NoError(t, err)
			assert.False(t, released, "no active row is left to release")
		})

		t.Run("no start-kind claim: a start claim blocks", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", "", "run-1")
			_, err := s.ClaimAgentStart(ctx, f.agent.ID, "owner", store.StartClaimRestart, "", time.Minute)
			require.NoError(t, err)

			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", NoStartKindClaim: true})
			require.NoError(t, err)
			assert.False(t, released)
			assert.True(t, f.active(t, ctx))

			// Without the condition the same row releases.
			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1"})
			require.NoError(t, err)
			assert.True(t, released)
		})

		t.Run("no start-kind claim: a stop claim does not block", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", "", "run-1")
			at, err := s.SetRunIntent(ctx, f.agent.ID, store.RunIntentStopped)
			require.NoError(t, err)
			_, err = s.ClaimAgentStop(ctx, f.agent.ID, "owner", at, time.Minute)
			require.NoError(t, err)
			f.reload(t, ctx)
			require.Equal(t, store.StartClaimStop, f.agent.StartClaimKind)

			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", NoStartKindClaim: true})
			require.NoError(t, err)
			assert.True(t, released)
		})

		t.Run("queued stop intent", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", store.ContainerStatusStopQueued, "run-1")
			_, err := s.SetRunIntent(ctx, f.agent.ID, store.RunIntentStopped)
			require.NoError(t, err)
			f.reload(t, ctx)
			require.NotNil(t, f.agent.RunIntentAt)
			intentAt := *f.agent.RunIntentAt
			other := intentAt.Add(time.Second)

			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{
				ExpectedRunID: "run-1", QueuedStopIntent: true, QueuedStopRunIntentAt: &other,
			})
			require.NoError(t, err)
			assert.False(t, released, "a different run intent time is another stop")
			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{
				ExpectedRunID: "run-1", QueuedStopIntent: true,
			})
			require.NoError(t, err)
			assert.False(t, released, "an unset time matches only an unset run intent time")
			assert.True(t, f.active(t, ctx))

			released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{
				ExpectedRunID: "run-1", QueuedStopIntent: true, QueuedStopRunIntentAt: &intentAt,
			})
			require.NoError(t, err)
			assert.True(t, released)
		})

		t.Run("queued stop intent: a start supersedes it", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", store.ContainerStatusStopQueued, "run-1")
			_, err := s.SetRunIntent(ctx, f.agent.ID, store.RunIntentStopped)
			require.NoError(t, err)
			f.reload(t, ctx)
			intentAt := *f.agent.RunIntentAt
			_, err = s.ClaimAgentStart(ctx, f.agent.ID, "owner", store.StartClaimUser, "", time.Minute)
			require.NoError(t, err)

			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{
				ExpectedRunID: "run-1", QueuedStopIntent: true, QueuedStopRunIntentAt: &intentAt,
			})
			require.NoError(t, err)
			assert.False(t, released)
			assert.True(t, f.active(t, ctx))
		})

		t.Run("not counted on the stored row", func(t *testing.T) {
			s := factory(t)
			running := newReleaseFixture(t, ctx, s, "running", "", "run-1")
			released, err := s.ReleaseReservationIfAgent(ctx, running.def, running.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", NotCounted: true})
			require.NoError(t, err)
			assert.False(t, released, "a running agent holds capacity")

			queued := newReleaseFixture(t, ctx, s, "stopped", store.ContainerStatusStopQueued, "run-1")
			released, err = s.ReleaseReservationIfAgent(ctx, queued.def, queued.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", NotCounted: true})
			require.NoError(t, err)
			assert.False(t, released, "a queued stop holds capacity")

			for _, phase := range []string{"stopped", "suspended", "error"} {
				f := newReleaseFixture(t, ctx, s, phase, "", "run-1")
				released, err = s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", NotCounted: true})
				require.NoError(t, err)
				assert.True(t, released, phase)
			}
		})

		t.Run("reservation ID limits the release to that row", func(t *testing.T) {
			s := factory(t)
			f := newReleaseFixture(t, ctx, s, "stopped", "", "run-1")
			for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
				released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", ReservationID: id})
				require.NoError(t, err)
				assert.False(t, released, id)
			}
			assert.True(t, f.active(t, ctx))
			released, err := s.ReleaseReservationIfAgent(ctx, f.def, f.agent.ID, store.AgentReleaseCond{ExpectedRunID: "run-1", ReservationID: f.res.ID})
			require.NoError(t, err)
			assert.True(t, released)
		})
	})

	t.Run("release_if_scope_broker_absent", func(t *testing.T) {
		t.Run("a registered broker keeps its rows", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			broker := uuid.NewString()
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: broker, Name: "live-" + broker[:8], Slug: "live-" + broker[:8], Status: store.BrokerStatusOnline}))
			res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, broker, uuid.NewString()))
			require.NoError(t, err)

			released, err := s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, broker)
			require.NoError(t, err)
			assert.False(t, released)
			_, err = s.GetActiveReservation(ctx, def, res.ResourceID)
			assert.NoError(t, err)
		})

		t.Run("the row must be on the named scope", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			gone, other := uuid.NewString(), uuid.NewString()
			res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, gone, uuid.NewString()))
			require.NoError(t, err)

			released, err := s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, other)
			require.NoError(t, err)
			assert.False(t, released, "another broker's absence does not release this row")
			_, err = s.GetActiveReservation(ctx, def, res.ResourceID)
			assert.NoError(t, err)
		})

		t.Run("an absent broker's row is released, once, and only that row", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			gone := uuid.NewString()
			res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, gone, uuid.NewString()))
			require.NoError(t, err)
			other, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, gone, uuid.NewString()))
			require.NoError(t, err)

			released, err := s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, gone)
			require.NoError(t, err)
			assert.True(t, released)
			_, err = s.GetActiveReservation(ctx, def, res.ResourceID)
			assert.ErrorIs(t, err, store.ErrNotFound)
			_, err = s.GetActiveReservation(ctx, def, other.ResourceID)
			assert.NoError(t, err, "keyed by reservation ID")

			released, err = s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, gone)
			require.NoError(t, err)
			assert.False(t, released, "already released")
			for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
				released, err = s.ReleaseReservationIfScopeBrokerAbsent(ctx, id, gone)
				require.NoError(t, err)
				assert.False(t, released, id)
			}
		})
	})
}

// ReleaseIfScopeBrokerAbsentUnparseableScopeConformance checks that a row
// whose scope ID cannot be a broker ID is released with no error: no broker
// can have that ID (GetRuntimeBroker reports ErrNotFound for it), and the
// write never compares a non-UUID literal with the UUID column.
func ReleaseIfScopeBrokerAbsentUnparseableScopeConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)
	def := seedReservationLimit(t, ctx, s)
	res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, "not-a-uuid", uuid.NewString()))
	require.NoError(t, err)

	released, err := s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, "not-a-uuid")
	require.NoError(t, err)
	assert.True(t, released)
	_, err = s.GetActiveReservation(ctx, def, res.ResourceID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ReleaseIfScopeBrokerAbsentNonCanonicalScopeConformance checks that a
// non-canonical spelling (upper case) of a registered broker's ID keeps the
// row: the scope ID is parsed and compared as a UUID, not as raw text.
func ReleaseIfScopeBrokerAbsentNonCanonicalScopeConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)
	def := seedReservationLimit(t, ctx, s)
	broker := uuid.NewString()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: broker, Name: "live-" + broker[:8], Slug: "live-" + broker[:8], Status: store.BrokerStatusOnline}))
	upper := strings.ToUpper(broker)
	res, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, upper, uuid.NewString()))
	require.NoError(t, err)

	released, err := s.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, upper)
	require.NoError(t, err)
	assert.False(t, released)
	_, err = s.GetActiveReservation(ctx, def, res.ResourceID)
	assert.NoError(t, err)
}
