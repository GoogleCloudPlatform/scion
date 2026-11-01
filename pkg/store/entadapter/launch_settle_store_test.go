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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// settleSetup creates an agent on broker-1 at generation 1, mints Rm and
// begins a create launch bound to Rm, and returns the fence a Completed
// report for that launch carries (WP1 slice 3b design section 3a).
func settleSetup(t *testing.T, s *AgentStore, projectID, slug string) (*store.Agent, store.LaunchSettleFence) {
	t.Helper()
	ctx := context.Background()
	a := makeAgent(projectID, slug)
	a.Phase, a.RuntimeBrokerID, a.Generation = "created", "broker-1", 1
	require.NoError(t, s.CreateAgent(ctx, a))
	rm := "rm-" + uuid.NewString()
	_, err := s.SetAgentRunID(ctx, a.ID, rm, nil)
	require.NoError(t, err)
	launchID, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: rm})
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, rm, got.LaunchRunID, "slice 3 binds the minted run")
	require.NotNil(t, got.RunIDUpdatedAt)
	return got, store.LaunchSettleFence{
		LaunchID:       launchID,
		FromRunID:      rm,
		ToRunID:        "rb-" + uuid.NewString(),
		BrokerID:       "broker-1",
		Generation:     got.Generation,
		UnchangedSince: *got.RunIDUpdatedAt,
	}
}

// TestSettleLaunchedRunFence: SettleLaunchedRun swaps Rm -> Rb only while
// every fence condition holds; each condition, changed on its own, refuses
// the swap and writes nothing (WP1 slice 3b design sections 3a, 7).
func TestSettleLaunchedRunFence(t *testing.T) {
	ctx := context.Background()
	live := time.Now().Add(time.Minute)
	now := time.Now()

	t.Run("settles and clears previous runs", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, f := settleSetup(t, s, projectID, "settle-ok")
		ok, err := s.SettleLaunchedRun(ctx, a.ID, f)
		require.NoError(t, err)
		require.True(t, ok)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, f.ToRunID, got.RunID)
		assert.Empty(t, got.PreviousRunIDs)
		assert.True(t, got.RunIDUpdatedAt.After(f.UnchangedSince) || got.RunIDUpdatedAt.Equal(f.UnchangedSince))

		// A resend after the settle: the row is on Rb, not launch_run_id.
		ok, err = s.SettleLaunchedRun(ctx, a.ID, f)
		require.NoError(t, err)
		assert.False(t, ok, "a resend after a settle changes nothing")
	})

	t.Run("a start launch bound to its minted run settles", func(t *testing.T) {
		// Async start and restart launches bind launch_run_id too (slice 3
		// 22bb022), so the completed settle applies to them as to a create.
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "settle-start")
		a.Phase, a.RuntimeBrokerID, a.Generation = "starting", "broker-1", 1
		require.NoError(t, s.CreateAgent(ctx, a))
		rm := "rm-" + uuid.NewString()
		_, err := s.SetAgentRunID(ctx, a.ID, rm, nil)
		require.NoError(t, err)
		launchID, err := s.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{RunID: rm})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, rm, got.LaunchRunID)
		require.Equal(t, store.LaunchKindStart, got.LaunchKind)
		rb := "rb-" + uuid.NewString()
		ok, err := s.SettleLaunchedRun(ctx, a.ID, store.LaunchSettleFence{
			LaunchID: launchID, FromRunID: rm, ToRunID: rb, BrokerID: "broker-1",
			Generation: got.Generation, UnchangedSince: *got.RunIDUpdatedAt,
		})
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, rb, mustGetRunID(t, s, a.ID))
	})

	t.Run("same run is a same-value settle", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a, f := settleSetup(t, s, projectID, "settle-same")
		f.ToRunID = f.FromRunID
		ok, err := s.SettleLaunchedRun(ctx, a.ID, f)
		require.NoError(t, err)
		require.True(t, ok)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, f.FromRunID, got.RunID)
		assert.True(t, got.RunIDUpdatedAt.Equal(f.UnchangedSince), "a same-value swap leaves run_id_updated_at")
	})

	for _, tc := range []struct {
		name string
		// change mutates the row or the fence after settleSetup.
		change func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence)
	}{
		{"run: a newer run was minted", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			_, err := s.SetAgentRunID(ctx, a.ID, "r3-"+uuid.NewString(), nil)
			require.NoError(t, err)
		}},
		{"run_id_updated_at: the run changed after the report's read", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			f.UnchangedSince = f.UnchangedSince.Add(-time.Second)
		}},
		{"run_id_updated_at: unset", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).ClearRunIDUpdatedAt().Exec(ctx))
		}},
		{"launch: a newer launch began on the same run", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			_, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: f.FromRunID})
			require.NoError(t, err)
		}},
		{"launch_run_id: unknown binding", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetLaunchRunID("").Exec(ctx))
		}},
		{"launch_run_id: a stale binding from another run", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetLaunchRunID("rm-old").Exec(ctx))
		}},
		{"broker: reassigned", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRuntimeBrokerID("broker-2").Exec(ctx))
		}},
		{"broker: the report is from another broker", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			f.BrokerID = "broker-2"
		}},
		{"generation: bumped", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetGeneration(a.Generation+1).Exec(ctx))
		}},
		{"deleted: soft-deleted", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{DeletedAt: &now})
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}},
		{"delete-held: deleting with a live lease", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			st := store.DeletionStateDeleting
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: &st, LeaseAt: &live})
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}},
		{"delete-held: finalizing", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			st := store.DeletionStateFinalizing
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: &st, LeaseAt: &live})
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}},
		{"empty fence: no launch", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			f.LaunchID = ""
		}},
		{"empty fence: no broker", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRuntimeBrokerID("").Exec(ctx))
			f.BrokerID = ""
		}},
		{"empty fence: no from run", func(t *testing.T, s *AgentStore, a *store.Agent, f *store.LaunchSettleFence) {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRunID("").SetLaunchRunID("").Exec(ctx))
			f.FromRunID = ""
		}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, f := settleSetup(t, s, projectID, "settle-refused")
			tc.change(t, s, a, &f)
			before, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
			require.NoError(t, err)

			ok, err := s.SettleLaunchedRun(ctx, a.ID, f)
			require.NoError(t, err)
			assert.False(t, ok)
			after, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
			require.NoError(t, err)
			assert.Equal(t, before.RunID, after.RunID, "nothing written")
			assert.Equal(t, before.PreviousRunIds, after.PreviousRunIds, "nothing written")
			assert.Equal(t, before.RunIDUpdatedAt, after.RunIDUpdatedAt, "nothing written")
		})
	}

	t.Run("missing agent", func(t *testing.T) {
		s, _ := newTestAgentStore(t)
		ok, err := s.SettleLaunchedRun(ctx, uuid.NewString(), store.LaunchSettleFence{
			LaunchID: "l", FromRunID: "a", ToRunID: "b", BrokerID: "x", UnchangedSince: time.Now(),
		})
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

// TestRunAdoptionSourceFrom: the annotation defaults to heartbeat_adopt.
func TestRunAdoptionSourceFrom(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, store.AdoptionSourceHeartbeat, store.RunAdoptionSourceFrom(ctx))
	assert.Equal(t, store.AdoptionSourceCAS, store.RunAdoptionSourceFrom(store.WithRunAdoptionSource(ctx, store.AdoptionSourceCAS)))
	assert.Equal(t, store.AdoptionSourceHeartbeat, store.RunAdoptionSourceFrom(store.WithRunAdoptionSource(ctx, "")))
	assert.Equal(t, store.AdoptionSourceHeartbeat, store.RunAdoptionSourceFrom(store.WithRunAdoptionSource(ctx, "not-a-source")),
		"a value outside the closed set falls back to the default")
}

// TestLifecycleStore3b_SettleObservedAsCAS: both launch-report settle
// paths are observed run_adopt writes with adoption_source cas: the
// Completed path (SettleLaunchedRun) by its hints, the Applied path
// (AdoptAgentRunID) by the context annotation. The settle rebinds the open
// segment (no superseded, no new start), and the next event carries the
// provenance (WP1 slice 3b design sections 6, 7).
func TestLifecycleStore3b_SettleObservedAsCAS(t *testing.T) {
	for _, path := range []string{"completed", "applied"} {
		t.Run(path, func(t *testing.T) {
			f := newLifecycleFixture(t)
			ctx := context.Background()
			a := f.create("s3b-"+path, func(a *store.Agent) {
				a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1"
			})
			f.mint(a.ID, "RM")
			launchID, err := f.cs.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: "RM"})
			require.NoError(t, err)
			f.startWrite(a.ID)
			row, err := f.cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, "RM", row.LaunchRunID)

			var ok bool
			if path == "completed" {
				ok, err = f.cs.SettleLaunchedRun(ctx, a.ID, store.LaunchSettleFence{
					LaunchID: launchID, FromRunID: "RM", ToRunID: "RB", BrokerID: "broker-1",
					Generation: row.Generation, UnchangedSince: *row.RunIDUpdatedAt,
				})
			} else {
				ok, err = f.cs.AdoptAgentRunID(store.WithRunAdoptionSource(ctx, store.AdoptionSourceCAS), a.ID, "RM", "RB", *row.RunIDUpdatedAt)
			}
			require.NoError(t, err)
			require.True(t, ok)
			f.stopRecord(a.ID, "RB", "stopped")
			evs := f.events(a.ID)
			require.Equal(t, "created@1[/1] started@2[RM/1] stopped@3(stopped)[RB/1]", lcFull(evs))
			assert.Equal(t, "RM", evs[2].AdoptedFromLaunchID)
			assert.Equal(t, store.AdoptionSourceCAS, evs[2].AdoptionSource)
			assert.NotEmpty(t, evs[2].AdoptedAt)
		})
	}
}

func mustGetRunID(t *testing.T, s *AgentStore, id string) string {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.RunID
}
