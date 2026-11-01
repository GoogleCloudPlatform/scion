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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// TestBeginLaunchBindsDispatchedRun pins the launch_run_id binding (WP1
// slice 3 addendum v12 section 1, slice 3b design v5 section 3b): a launch
// binds the run the dispatcher minted only while the row still holds it.
func TestBeginLaunchBindsDispatchedRun(t *testing.T) {
	ctx := context.Background()
	launchRun := func(t *testing.T, s *AgentStore, id string) (string, string) {
		t.Helper()
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		return got.LaunchID, got.LaunchRunID
	}
	mint := func(t *testing.T, s *AgentStore, id string) string {
		t.Helper()
		run := uuid.NewString()
		_, err := s.SetAgentRunID(ctx, id, run, nil)
		require.NoError(t, err)
		return run
	}

	t.Run("the minted run still on the row is bound", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createLaunchableAgent(t, ctx, s, projectID, "bind-match")
		run := mint(t, s, a.ID)
		launchID, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: run})
		require.NoError(t, err)
		gotLaunch, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, launchID, gotLaunch)
		assert.Equal(t, run, gotRun)
	})

	t.Run("a run minted concurrently binds unknown", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createLaunchableAgent(t, ctx, s, projectID, "bind-race")
		minted := mint(t, s, a.ID)
		mint(t, s, a.ID) // another dispatch minted after beginRun
		_, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: minted})
		require.NoError(t, err)
		_, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, "", gotRun)
	})

	t.Run("no run binds unknown", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createLaunchableAgent(t, ctx, s, projectID, "bind-none")
		mint(t, s, a.ID)
		_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Minute)
		require.NoError(t, err)
		_, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, "", gotRun)
	})

	t.Run("the next launch replaces it; the finalizeEnv second pass binds the same run", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createLaunchableAgent(t, ctx, s, projectID, "bind-replace")
		run := mint(t, s, a.ID)
		first, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: run})
		require.NoError(t, err)
		// finalizeEnv's second BeginLaunch reuses the request, so the
		// same minted run binds to the new launch.
		second, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: run})
		require.NoError(t, err)
		require.NotEqual(t, first, second)
		gotLaunch, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, second, gotLaunch)
		assert.Equal(t, run, gotRun)
		// A later launch from an older caller replaces the binding.
		third, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Minute)
		require.NoError(t, err)
		gotLaunch, gotRun = launchRun(t, s, a.ID)
		assert.Equal(t, third, gotLaunch)
		assert.Equal(t, "", gotRun)
	})

	t.Run("start launches bind through the same path", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "bind-start")
		a.Phase = "starting"
		require.NoError(t, s.CreateAgent(ctx, a))
		run := mint(t, s, a.ID)
		_, err := s.BeginStartLaunch(ctx, a.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{RunID: run})
		require.NoError(t, err)
		_, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, run, gotRun)
	})

	t.Run("UpdateAgent never writes it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createLaunchableAgent(t, ctx, s, projectID, "bind-update")
		run := mint(t, s, a.ID)
		_, err := s.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: run})
		require.NoError(t, err)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		got.LaunchRunID = "caller-value"
		got.Labels = map[string]string{"k": "changed"}
		require.NoError(t, s.UpdateAgent(ctx, got))
		_, gotRun := launchRun(t, s, a.ID)
		assert.Equal(t, run, gotRun)
	})
}

// effectiveEcho is a valid echo for run on broker.
func effectiveEcho(run, broker string) store.AgentEffectiveConfig {
	return store.AgentEffectiveConfig{
		RunID: run, Template: "claude", TemplateHash: "sha256:aa", Harness: "claude", Model: "opus",
		BrokerID: broker, BrokerBuild: "v1", Source: store.EffectiveConfigSourceSync,
	}
}

// recordingObserver records the writes it observes.
type recordingObserver struct {
	mu     sync.Mutex
	writes []store.AgentWrite
}

func (o *recordingObserver) ObserveAgentWrite(_ context.Context, _ store.LifecycleTx, w store.AgentWrite) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.writes = append(o.writes, w)
	return nil
}

func (o *recordingObserver) ops() []store.AgentWriteOp {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]store.AgentWriteOp, len(o.writes))
	for i, w := range o.writes {
		out[i] = w.Op
	}
	return out
}

// TestSetAgentEffectiveConfig covers the single writer's fences, their
// precedence and the Row* fields, with and without an observer.
func TestSetAgentEffectiveConfig(t *testing.T) {
	ctx := context.Background()
	const broker = "broker-1"
	for _, observed := range []bool{true, false} {
		name := "observer off"
		if observed {
			name = "observed"
		}
		t.Run(name, func(t *testing.T) {
			setup := func(t *testing.T) (*CompositeStore, *recordingObserver, *store.Agent, string) {
				t.Helper()
				cs := newTestCompositeStore(t)
				projectID := uuid.NewString()
				require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "ec", Slug: "ec-" + projectID[:8]}))
				a := makeAgent(projectID, "ec-"+uuid.NewString()[:8])
				a.Phase, a.RuntimeBrokerID = "created", broker
				require.NoError(t, cs.CreateAgent(ctx, a))
				run := uuid.NewString()
				_, err := cs.SetAgentRunID(ctx, a.ID, run, nil)
				require.NoError(t, err)
				_, err = cs.BeginLaunchWithOptions(ctx, a.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{RunID: run})
				require.NoError(t, err)
				rec := &recordingObserver{}
				if observed {
					cs.SetAgentWriteObserver(rec)
				}
				got, err := cs.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				return cs, rec, got, run
			}

			t.Run("a matching echo is written and observed; state_version and updated stay", func(t *testing.T) {
				cs, rec, a, run := setup(t)
				res, err := cs.SetAgentEffectiveConfig(ctx, a.ID, run, effectiveEcho(run, broker))
				require.NoError(t, err)
				assert.Equal(t, store.EffectiveConfigResult{Written: true, RowRunID: run, RowLaunchID: a.LaunchID, RowLaunchRunID: run}, res)
				got, err := cs.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				require.NotNil(t, got.EffectiveConfig)
				want := effectiveEcho(run, broker)
				assert.True(t, want.SameEcho(*got.EffectiveConfig), "stored %+v", *got.EffectiveConfig)
				assert.False(t, got.EffectiveConfig.ReportedAt.IsZero(), "ReportedAt is the hub commit time")
				assert.Equal(t, a.StateVersion, got.StateVersion)
				assert.Equal(t, a.Updated, got.Updated)
				if observed {
					assert.Equal(t, []store.AgentWriteOp{store.AgentWriteEffectiveConfig}, rec.ops())
				}

				// An identical retry is Written with no write.
				stored := got.EffectiveConfig.ReportedAt
				res, err = cs.SetAgentEffectiveConfig(ctx, a.ID, run, effectiveEcho(run, broker))
				require.NoError(t, err)
				assert.True(t, res.Written)
				got, err = cs.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				assert.Equal(t, stored, got.EffectiveConfig.ReportedAt, "an identical retry writes nothing")
				if observed {
					assert.Len(t, rec.ops(), 1, "an identical retry is not observed")
				}

				// UpdateAgent never touches the column.
				got.EffectiveConfig = nil
				got.Labels = map[string]string{"k": "changed"}
				require.NoError(t, cs.UpdateAgent(ctx, got))
				got, err = cs.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				assert.NotNil(t, got.EffectiveConfig, "UpdateAgent keeps the stored echo")
			})

			stale := []struct {
				name   string
				mutate func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (runID string, eff store.AgentEffectiveConfig)
				reason string
			}{
				{"stale run", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					other := uuid.NewString()
					return other, effectiveEcho(other, broker)
				}, store.StaleEchoRunMismatch},
				{"echo of another run", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					return run, effectiveEcho(uuid.NewString(), broker)
				}, store.StaleEchoRunMismatch},
				{"empty run", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					return "", effectiveEcho("", broker)
				}, store.StaleEchoRunMismatch},
				{"stale broker beats stale run", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					other := uuid.NewString()
					return other, effectiveEcho(other, "broker-2")
				}, store.StaleEchoBrokerMismatch},
				{"no broker provenance", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					return run, effectiveEcho(run, "")
				}, store.StaleEchoBrokerMismatch},
				{"delete-held beats stale broker", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					deleting, lease := store.DeletionStateDeleting, time.Now().Add(time.Hour)
					_, err := cs.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &lease})
					require.NoError(t, err)
					return run, effectiveEcho(run, "broker-2")
				}, store.StaleEchoDeletedOrHeld},
				{"soft-deleted", func(t *testing.T, cs *CompositeStore, a *store.Agent, run string) (string, store.AgentEffectiveConfig) {
					now := time.Now()
					_, err := cs.FinalizeAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{DeletedAt: &now}, nil)
					require.NoError(t, err)
					return run, effectiveEcho(run, broker)
				}, store.StaleEchoDeletedOrHeld},
			}
			for _, c := range stale {
				t.Run(c.name, func(t *testing.T) {
					cs, rec, a, run := setup(t)
					runID, eff := c.mutate(t, cs, a, run)
					before := len(rec.ops())
					res, err := cs.SetAgentEffectiveConfig(ctx, a.ID, runID, eff)
					require.NoError(t, err)
					assert.Equal(t, store.EffectiveConfigResult{StaleReason: c.reason, RowRunID: run, RowLaunchID: a.LaunchID, RowLaunchRunID: run}, res)
					got, err := cs.client.Agent.Get(ctx, uuid.MustParse(a.ID))
					require.NoError(t, err)
					assert.Nil(t, got.EffectiveConfig, "a stale echo writes nothing")
					assert.Len(t, rec.ops(), before, "a stale echo is not observed")
				})
			}

			t.Run("a gone row", func(t *testing.T) {
				cs, rec, a, run := setup(t)
				cs.SetAgentWriteObserver(nil)
				require.NoError(t, cs.DeleteAgent(ctx, a.ID))
				if observed {
					cs.SetAgentWriteObserver(rec)
				}
				res, err := cs.SetAgentEffectiveConfig(ctx, a.ID, run, effectiveEcho(run, broker))
				require.NoError(t, err)
				assert.Equal(t, store.EffectiveConfigResult{StaleReason: store.StaleEchoGone}, res)
			})
		})
	}
}

// TestSetAgentEffectiveConfig_UpdatedRaceRetries: an unrelated write that
// moves updated between the read and the write (no fence fails) is
// retried, and the echo is written with the new updated kept; a row that
// changes under every attempt is a version conflict, not a stale echo.
func TestSetAgentEffectiveConfig_UpdatedRaceRetries(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*AgentStore, *store.Agent) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "ec-upd-"+uuid.NewString()[:6])
		a.RuntimeBrokerID = "broker-1"
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.SetAgentRunID(ctx, a.ID, "R1", nil)
		require.NoError(t, err)
		return s, a
	}
	bump := func(t *testing.T, s *AgentStore, id string) time.Time {
		uid, err := parseUUID(id)
		require.NoError(t, err)
		at := time.Now().Add(time.Duration(time.Now().UnixNano()%1000+1) * time.Millisecond).UTC().Truncate(time.Microsecond)
		require.NoError(t, s.client.Agent.UpdateOneID(uid).SetUpdated(at).Exec(ctx))
		return at
	}

	t.Run("one race is retried", func(t *testing.T) {
		s, a := setup(t)
		var bumped time.Time
		calls := 0
		effectiveConfigRaceHook = func() {
			calls++
			if calls == 1 {
				bumped = bump(t, s, a.ID)
			}
		}
		t.Cleanup(func() { effectiveConfigRaceHook = nil })
		res, err := s.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
		require.NoError(t, err)
		assert.True(t, res.Written)
		assert.Equal(t, 2, calls)
		uid, _ := parseUUID(a.ID)
		got, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)
		assert.NotNil(t, got.EffectiveConfig)
		assert.True(t, bumped.Equal(got.Updated), "the echo keeps the raced updated %v, got %v", bumped, got.Updated)
	})

	t.Run("a row changing under every attempt is a conflict", func(t *testing.T) {
		s, a := setup(t)
		effectiveConfigRaceHook = func() { bump(t, s, a.ID) }
		t.Cleanup(func() { effectiveConfigRaceHook = nil })
		_, err := s.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
		require.ErrorIs(t, err, store.ErrVersionConflict)
	})
}

// TestSetAgentEffectiveConfig_RaceReclassifies: with no observer the read
// is not locked, so a run swap between the read and the write leaves the
// conditional write matching no row; the writer re-reads and classifies
// from the re-read (review r4 O2), and a row gone at the re-read is gone.
func TestSetAgentEffectiveConfig_RaceReclassifies(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		race   func(t *testing.T, s *AgentStore, id string)
		reason string
		rowRun bool
	}{
		{"run swapped", func(t *testing.T, s *AgentStore, id string) {
			_, err := s.SetAgentRunID(ctx, id, "R-new", nil)
			require.NoError(t, err)
		}, store.StaleEchoRunMismatch, true},
		{"row deleted", func(t *testing.T, s *AgentStore, id string) {
			require.NoError(t, s.DeleteAgent(ctx, id))
		}, store.StaleEchoGone, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "ec-race")
			a.RuntimeBrokerID = "broker-1"
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetAgentRunID(ctx, a.ID, "R1", nil)
			require.NoError(t, err)
			raced := false
			effectiveConfigRaceHook = func() {
				if !raced {
					raced = true
					c.race(t, s, a.ID)
				}
			}
			t.Cleanup(func() { effectiveConfigRaceHook = nil })
			res, err := s.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
			require.NoError(t, err)
			assert.False(t, res.Written)
			assert.Equal(t, c.reason, res.StaleReason)
			if c.rowRun {
				assert.Equal(t, "R-new", res.RowRunID, "Row* come from the re-read")
			}
		})
	}
}

// effOfEvent renders an event's effective fields ("-" when omitted,
// "nometa" for a non-snapshot event).
func effOfEvent(e *tc.LifecycleEvent) string {
	m := e.Metadata
	if m == nil {
		return "nometa"
	}
	if m.EffectiveTemplate == "" && m.EffectiveTemplateHash == "" && m.EffectiveModel == "" &&
		m.EffectiveHarness == "" && m.HarnessConfigRevision == "" && m.EffectiveBrokerBuild == "" {
		return "-"
	}
	return m.EffectiveModel
}

// TestLifecycleStore3_EchoEmitsConfigured: through the real store, a
// matching echo emits configured with effective_* and launch_id = the run;
// an identical retry emits nothing.
func TestLifecycleStore3_EchoEmitsConfigured(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	a := f.create("s3-echo", func(a *store.Agent) { a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1" })
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	res, err := f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
	require.NoError(t, err)
	require.True(t, res.Written)
	_, err = f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
	require.NoError(t, err)
	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] configured@3[R1/1]", lcFull(evs))
	assert.Equal(t, "opus", effOfEvent(evs[2]))
	assert.Equal(t, "claude", evs[2].Metadata.EffectiveHarness)
}

// TestLifecycleStore3_ReincarnationEchoPair is review r2 R2 (the B3
// fixture pair): the reincarnation step's base-run configured (N+1, no
// launch_id) has no effective_*, although R1's echo is stored; R2's echo,
// while the reincarnation is in flight, closes R1 and gives configured
// tagged N+1 with launch_id R2 and R2's effective_*.
func TestLifecycleStore3_ReincarnationEchoPair(t *testing.T) {
	ctx := context.Background()
	f := newLifecycleFixture(t)
	a := f.create("s3-reinc", func(a *store.Agent) { a.Phase, a.Generation, a.RuntimeBrokerID = "created", 1, "broker-1" })
	f.mint(a.ID, "R1")
	f.startWrite(a.ID)
	_, err := f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R1", effectiveEcho("R1", "broker-1"))
	require.NoError(t, err)
	cur, err := f.cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	_, err = f.cs.ClaimAgentReincarnation(ctx, a.ID, cur.StateVersion, time.Now())
	require.NoError(t, err)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Phase = "stopping"; a.ReincarnationState = store.ReincarnationStatePending })
	f.update(lifecycleevents.WithConfigResolution(ctx), a.ID, func(a *store.Agent) {
		a.Phase = "starting"
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "next"}
	})
	f.mint(a.ID, "R2")
	r2 := effectiveEcho("R2", "broker-1")
	r2.Model = "opus-next"
	res, err := f.cs.SetAgentEffectiveConfig(ctx, a.ID, "R2", r2)
	require.NoError(t, err)
	require.True(t, res.Written)
	f.update(ctx, a.ID, func(a *store.Agent) { a.Generation = 2 })
	f.update(ctx, a.ID, func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateNone })
	f.heartbeat(a.ID, "running", "R2")

	evs := f.events(a.ID)
	require.Equal(t, "created@1[/1] started@2[R1/1] configured@3[R1/1] configured@4[/2] stopped@5(superseded)[R1/1] configured@6[R2/2] reincarnated@7[R2/2] started@8[R2/2]",
		lcFull(evs))
	assert.Equal(t, "opus", effOfEvent(evs[2]), "R1's echo configured")
	assert.Equal(t, "-", effOfEvent(evs[3]), "the base-run configured (N+1, no launch_id) has no effective_*")
	assert.Equal(t, "opus-next", effOfEvent(evs[5]), "R2's echo configured keeps R2's effective_*")
	assert.Equal(t, "opus-next", effOfEvent(evs[6]), "a later R2 snapshot carries R2's echo")
}
