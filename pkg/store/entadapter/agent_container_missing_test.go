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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentStore_MarkAgentContainerMissing(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	old := time.Now().Add(-time.Hour)
	cutoff := time.Now().Add(-5 * time.Minute)

	create := func(slug string, mutate func(a *store.Agent)) *store.Agent {
		a := makeAgent(projectID, slug)
		a.RuntimeBrokerID = "broker-1"
		a.Activity = "blocked"
		a.LastSeen = old
		a.ContainerStatus = "Running"
		if mutate != nil {
			mutate(a)
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}

	t.Run("marks running agent", func(t *testing.T) {
		a := create("target", nil)
		got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "error", got.Phase)
		assert.Equal(t, "", got.Activity)
		assert.Equal(t, "container_missing", got.ExitReason)
		assert.Equal(t, "missing", got.ContainerStatus)
		assert.Equal(t, "gone", got.Message)

		stored, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", stored.Phase)
		assert.Equal(t, "container_missing", stored.ExitReason)
	})

	// setExit stores an exit reason, code and message directly, as a
	// broker status report would have before the container went away.
	setExit := func(t *testing.T, id, reason string, code int, message string) {
		t.Helper()
		require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(id)).
			SetExitReason(reason).SetExitCode(code).SetMessage(message).Exec(ctx))
	}

	for _, reason := range []string{"preempted", "evicted"} {
		t.Run("keeps a "+reason+" exit reason", func(t *testing.T) {
			a := create("kept-"+reason, nil)
			setExit(t, a.ID, reason, 137, "node "+reason+" the pod")
			got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, "error", got.Phase)
			assert.Equal(t, "", got.Activity)
			assert.Equal(t, "missing", got.ContainerStatus)
			assert.Equal(t, reason, got.ExitReason)
			assert.Equal(t, "node "+reason+" the pod", got.Message)
			require.NotNil(t, got.ExitCode)
			assert.Equal(t, 137, *got.ExitCode)
		})
	}

	t.Run("replaces another exit reason", func(t *testing.T) {
		a := create("replaced-reason", nil)
		setExit(t, a.ID, "crashed", 1, "earlier crash")
		got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "container_missing", got.ExitReason)
		assert.Equal(t, "gone", got.Message)
		assert.Nil(t, got.ExitCode)
	})

	// Each guard is checked with no stored exit reason (the second UPDATE)
	// and with a stored preempted reason (the first UPDATE), so both
	// conditional writes are guard-tested on their own.
	guards := []struct {
		name   string
		broker string
		mutate func(a *store.Agent)
		after  func(t *testing.T, a *store.Agent)
	}{
		{name: "other broker", broker: "broker-2"},
		{name: "not running", broker: "broker-1", mutate: func(a *store.Agent) { a.Phase = "provisioning" }},
		{name: "seen after cutoff", broker: "broker-1", mutate: func(a *store.Agent) { a.LastSeen = time.Now() }},
		{name: "reincarnating", broker: "broker-1", after: func(t *testing.T, a *store.Agent) {
			a.ReincarnationState = store.ReincarnationStatePending
			require.NoError(t, s.UpdateAgent(ctx, a))
		}},
		{name: "soft deleted", broker: "broker-1", after: func(t *testing.T, a *store.Agent) {
			a.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(ctx, a))
		}},
	}
	for _, stored := range []string{"", "preempted"} {
		for i, tc := range guards {
			name := tc.name
			if stored != "" {
				name += " with a stored " + stored + " reason"
			}
			t.Run(name, func(t *testing.T) {
				a := create(fmt.Sprintf("guard-%s-%d", stored, i), tc.mutate)
				if tc.after != nil {
					tc.after(t, a)
				}
				if stored != "" {
					setExit(t, a.ID, stored, 137, "kept message")
				}
				before, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
				require.NoError(t, err)

				got, err := s.MarkAgentContainerMissing(ctx, a.ID, tc.broker, cutoff, "gone")
				require.NoError(t, err)
				assert.Nil(t, got)

				row, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
				require.NoError(t, err)
				assert.Equal(t, before.Phase, row.Phase)
				assert.NotEqual(t, "error", row.Phase)
				assert.Equal(t, before.ContainerStatus, row.ContainerStatus)
				assert.Equal(t, stored, row.ExitReason)
				assert.Equal(t, before.Message, row.Message)
				assert.Equal(t, before.ExitCode, row.ExitCode)
			})
		}
	}

	t.Run("unknown agent", func(t *testing.T) {
		got, err := s.MarkAgentContainerMissing(ctx, "00000000-0000-0000-0000-00000000dead", "broker-1", cutoff, "gone")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// TestAgentStore_MarkAgentContainerMissing_GuardsInUpdate pins that the row
// exclusions are part of the conditional UPDATE itself rather than a read
// before it, so a row soft-deleted (or otherwise changed) between a read and
// the write can never be overwritten.
func TestAgentStore_MarkAgentContainerMissing_GuardsInUpdate(t *testing.T) {
	ctx := context.Background()
	base, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "cas")
	a.RuntimeBrokerID = "broker-1"
	a.LastSeen = time.Now().Add(-time.Hour)
	require.NoError(t, base.CreateAgent(ctx, a))

	var (
		mu    sync.Mutex
		stmts []string
	)
	drv := dialect.DebugWithContext(base.client.Driver(), func(_ context.Context, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		stmts = append(stmts, fmt.Sprint(v...))
	})
	s := NewAgentStore(ent.NewClient(ent.Driver(drv)))

	got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", time.Now().Add(-5*time.Minute), "gone")
	require.NoError(t, err)
	require.NotNil(t, got)

	mu.Lock()
	defer mu.Unlock()
	// Two conditional UPDATEs run (kept exit reason, then the rest); no read
	// precedes either, and each carries every guard.
	var updates []string
	for _, q := range stmts {
		if strings.Contains(q, "UPDATE") && strings.Contains(q, "agents") {
			updates = append(updates, q)
			continue
		}
		if len(updates) < 2 {
			assert.NotContains(t, q, "SELECT", "no read may precede the conditional updates: %s", q)
		}
	}
	require.Len(t, updates, 2, "expected two UPDATE statements, got %v", stmts)
	for _, update := range updates {
		where := update[strings.Index(update, "WHERE"):]
		for _, cond := range []string{"deleted_at", "runtime_broker_id", "phase", "reincarnation_state", "last_seen", "exit_reason"} {
			assert.Contains(t, where, cond, "guard %s must be in the UPDATE predicate", cond)
		}
		assert.Regexp(t, `deleted_at[`+"`"+`"]? IS NULL`, update, "soft-delete guard must be deleted_at IS NULL")
	}
}

func TestAgentStore_ClearAgentRuntimeTarget(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("clears the target and candidate and bumps state_version", func(t *testing.T) {
		a := makeAgent(projectID, "with-target")
		a.AppliedConfig = &store.AgentAppliedConfig{
			Image:                  "example/image:1",
			Profile:                "remote",
			Env:                    map[string]string{"A": "1"},
			RuntimeTarget:          "kubernetes|context=c|namespace=n",
			RuntimeTargetCandidate: "kubernetes|context=c|namespace=m",
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)

		cleared, newVersion, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, cleared)
		assert.Equal(t, before.StateVersion+1, newVersion)

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Empty(t, got.AppliedConfig.RuntimeTarget)
		assert.Empty(t, got.AppliedConfig.RuntimeTargetCandidate)
		assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
		assert.Equal(t, "remote", got.AppliedConfig.Profile)
		assert.Equal(t, map[string]string{"A": "1"}, got.AppliedConfig.Env)
		assert.Equal(t, newVersion, got.StateVersion)

		// A full update by a holder of the pre-clear read must not write the
		// old target back: it gets a version conflict.
		err = s.UpdateAgent(ctx, before)
		require.ErrorIs(t, err, store.ErrVersionConflict)
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.AppliedConfig.RuntimeTarget, "the stale write did not restore the target")

		// A holder of the post-clear version still writes normally.
		require.NoError(t, s.UpdateAgent(ctx, got))
	})

	t.Run("candidate only is cleared", func(t *testing.T) {
		a := makeAgent(projectID, "candidate-only")
		a.AppliedConfig = &store.AgentAppliedConfig{RuntimeTargetCandidate: "docker"}
		require.NoError(t, s.CreateAgent(ctx, a))
		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		assert.True(t, cleared)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.AppliedConfig.RuntimeTargetCandidate)
	})

	t.Run("no target is a no-op", func(t *testing.T) {
		a := makeAgent(projectID, "no-target")
		a.AppliedConfig = &store.AgentAppliedConfig{Image: "example/image:1"}
		require.NoError(t, s.CreateAgent(ctx, a))
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, cleared)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
		assert.Equal(t, before.StateVersion, got.StateVersion, "a no-op does not bump state_version")
	})

	t.Run("no applied config is a no-op", func(t *testing.T) {
		a := makeAgent(projectID, "no-config")
		a.AppliedConfig = nil
		require.NoError(t, s.CreateAgent(ctx, a))
		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, cleared)
	})

	t.Run("unknown agent is a no-op", func(t *testing.T) {
		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, "00000000-0000-0000-0000-00000000abcd")
		require.NoError(t, err)
		assert.False(t, cleared)
	})

	t.Run("soft-deleted agent is a no-op", func(t *testing.T) {
		a := makeAgent(projectID, "soft-deleted")
		a.AppliedConfig = &store.AgentAppliedConfig{RuntimeTarget: "docker"}
		require.NoError(t, s.CreateAgent(ctx, a))
		uid := uuid.MustParse(a.ID)
		_, err := s.client.Agent.UpdateOneID(uid).SetDeletedAt(time.Now()).Save(ctx)
		require.NoError(t, err)
		before, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)

		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		assert.False(t, cleared)
		after, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)
		assert.Equal(t, before.AppliedConfig, after.AppliedConfig, "applied_config unchanged")
		assert.Equal(t, before.StateVersion, after.StateVersion)
	})
}

func TestAgentStore_SetAgentRuntimeTarget(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	create := func(t *testing.T, name string, cfg *store.AgentAppliedConfig) *store.Agent {
		t.Helper()
		a := makeAgent(projectID, name)
		a.AppliedConfig = cfg
		require.NoError(t, s.CreateAgent(ctx, a))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return got
	}

	t.Run("writes only the target keys and keeps state_version", func(t *testing.T) {
		a := create(t, "set-target", &store.AgentAppliedConfig{
			Image:         "example/image:1",
			Profile:       "remote",
			Env:           map[string]string{"A": "1"},
			RuntimeTarget: "docker",
		})
		// A status write that does not bump state_version lands after the
		// caller's read; the target write must keep it.
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", Activity: "working"}))

		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "docker", "kubernetes|context=c|namespace=n")
		require.NoError(t, err)
		assert.True(t, written)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "working", got.Activity)
		assert.Equal(t, a.StateVersion, got.StateVersion, "the target write does not bump state_version")
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTarget)
		assert.Equal(t, "kubernetes|context=c|namespace=n", got.AppliedConfig.RuntimeTargetCandidate)
		assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
		assert.Equal(t, "remote", got.AppliedConfig.Profile)
		assert.Equal(t, map[string]string{"A": "1"}, got.AppliedConfig.Env)

		// Empty values remove the keys.
		written, err = s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "kubernetes|context=c|namespace=n", "")
		require.NoError(t, err)
		assert.True(t, written)
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "kubernetes|context=c|namespace=n", got.AppliedConfig.RuntimeTarget)
		assert.Empty(t, got.AppliedConfig.RuntimeTargetCandidate)
		raw, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
		require.NoError(t, err)
		assert.NotContains(t, raw.AppliedConfig, "runtimeTargetCandidate")
	})

	t.Run("version mismatch writes nothing", func(t *testing.T) {
		a := create(t, "set-stale", &store.AgentAppliedConfig{RuntimeTarget: "docker"})
		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
		require.NoError(t, err)
		require.True(t, cleared)

		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "docker", "")
		require.NoError(t, err)
		assert.False(t, written)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Empty(t, got.AppliedConfig.RuntimeTarget, "a write based on a read before the clear is dropped")
	})

	t.Run("no applied config", func(t *testing.T) {
		a := create(t, "set-no-config", nil)
		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "", "docker")
		require.NoError(t, err)
		assert.True(t, written)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetCandidate)
	})

	t.Run("NULL applied config", func(t *testing.T) {
		a := create(t, "set-null-config", nil)
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).ClearAppliedConfig().Save(ctx)
		require.NoError(t, err)
		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "", "docker")
		require.NoError(t, err)
		assert.True(t, written)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetCandidate)
	})

	t.Run("JSON null applied config", func(t *testing.T) {
		a := create(t, "set-json-null-config", nil)
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetAppliedConfig("null").Save(ctx)
		require.NoError(t, err)
		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "", "docker")
		require.NoError(t, err)
		assert.True(t, written)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetCandidate)
	})

	t.Run("unknown agent", func(t *testing.T) {
		written, err := s.SetAgentRuntimeTarget(ctx, "00000000-0000-0000-0000-00000000abcd", 1, "docker", "")
		require.NoError(t, err)
		assert.False(t, written)
	})

	t.Run("soft-deleted agent", func(t *testing.T) {
		a := create(t, "set-soft-deleted", &store.AgentAppliedConfig{RuntimeTarget: "docker"})
		uid := uuid.MustParse(a.ID)
		_, err := s.client.Agent.UpdateOneID(uid).SetDeletedAt(time.Now()).Save(ctx)
		require.NoError(t, err)
		before, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)

		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, before.StateVersion, "", "kubernetes|context=c|namespace=n")
		require.NoError(t, err)
		assert.False(t, written)
		after, err := s.client.Agent.Get(ctx, uid)
		require.NoError(t, err)
		assert.Equal(t, before.AppliedConfig, after.AppliedConfig)
	})
}

// TestAgentStore_ClearAgentRuntimeTarget_WriteMiss covers the conditional
// write missing because the agent changed between the read and the write:
// the clear re-reads and retries, and gives up with an error after a bounded
// number of attempts.
func TestAgentStore_ClearAgentRuntimeTarget_WriteMiss(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	create := func(t *testing.T, name string) *store.Agent {
		t.Helper()
		a := makeAgent(projectID, name)
		a.AppliedConfig = &store.AgentAppliedConfig{
			Image:         "example/image:1",
			RuntimeTarget: "kubernetes|context=c|namespace=n",
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	// changeConfig rewrites applied_config inside the attempt's transaction,
	// so the conditional write that follows matches no row.
	changeConfig := func(ctx context.Context, tx *ent.Tx, id uuid.UUID, n int) {
		cfg := fmt.Sprintf(`{"image":"example/image:%d","runtimeTarget":"kubernetes|context=c|namespace=n"}`, n+2)
		_, err := tx.Agent.UpdateOneID(id).SetAppliedConfig(cfg).Save(ctx)
		require.NoError(t, err)
	}
	// bumpVersion changes only state_version, as a concurrent status write
	// with the same applied config would.
	bumpVersion := func(ctx context.Context, tx *ent.Tx, id uuid.UUID, _ int) {
		_, err := tx.Agent.UpdateOneID(id).AddStateVersion(1).Save(ctx)
		require.NoError(t, err)
	}

	for name, change := range map[string]func(context.Context, *ent.Tx, uuid.UUID, int){
		"applied config changed": changeConfig,
		"state_version changed":  bumpVersion,
	} {
		t.Run(name+": one miss then retry clears the target", func(t *testing.T) {
			a := create(t, "miss-once-"+strings.ReplaceAll(name, " ", "-"))
			calls := 0
			clearRuntimeTargetHook = func(ctx context.Context, tx *ent.Tx, id uuid.UUID) {
				if calls == 0 {
					change(ctx, tx, id, calls)
				}
				calls++
			}
			t.Cleanup(func() { clearRuntimeTargetHook = nil })

			cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
			require.NoError(t, err)
			assert.True(t, cleared)
			assert.Equal(t, 2, calls, "the clear re-read and retried once")
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.NotNil(t, got.AppliedConfig)
			assert.Empty(t, got.AppliedConfig.RuntimeTarget)
			assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
		})

		t.Run(name+": persistent miss returns an error", func(t *testing.T) {
			a := create(t, "miss-always-"+strings.ReplaceAll(name, " ", "-"))
			calls := 0
			clearRuntimeTargetHook = func(ctx context.Context, tx *ent.Tx, id uuid.UUID) {
				change(ctx, tx, id, calls)
				calls++
			}
			t.Cleanup(func() { clearRuntimeTargetHook = nil })

			cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID)
			require.Error(t, err)
			assert.False(t, cleared)
			assert.Contains(t, err.Error(), "changed concurrently")
			assert.Equal(t, 5, calls, "retries are bounded")
		})
	}
}

// TestAgentStore_SetAgentRuntimeTarget_Guards pins each guard of the target
// write on its own: the read skips soft-deleted rows and returns early on a
// version mismatch (the write is never reached), and the conditional write
// misses when state_version, applied_config or deleted_at changed between the
// read and the write.
func TestAgentStore_SetAgentRuntimeTarget_Guards(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	const target = "kubernetes|context=c|namespace=n"

	create := func(t *testing.T, name string) *store.Agent {
		t.Helper()
		a := makeAgent(projectID, name)
		a.AppliedConfig = &store.AgentAppliedConfig{Image: "example/image:1", RuntimeTarget: target}
		require.NoError(t, s.CreateAgent(ctx, a))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return got
	}
	hookCalls := func(t *testing.T, change func(context.Context, *ent.Tx, uuid.UUID)) *int {
		t.Helper()
		calls := 0
		setRuntimeTargetHook = func(ctx context.Context, tx *ent.Tx, id uuid.UUID) {
			calls++
			if change != nil {
				change(ctx, tx, id)
			}
		}
		t.Cleanup(func() { setRuntimeTargetHook = nil })
		return &calls
	}

	t.Run("version mismatch returns before the write", func(t *testing.T) {
		a := create(t, "guard-early-version")
		calls := hookCalls(t, nil)
		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion+1, "", "docker")
		require.NoError(t, err)
		assert.False(t, written)
		assert.Equal(t, 0, *calls, "the read's version check returns before the write")
	})

	t.Run("soft-deleted row is not read", func(t *testing.T) {
		a := create(t, "guard-read-deleted")
		_, err := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetDeletedAt(time.Now()).Save(ctx)
		require.NoError(t, err)
		calls := hookCalls(t, nil)
		written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "", "docker")
		require.NoError(t, err)
		assert.False(t, written)
		assert.Equal(t, 0, *calls, "the read skips a soft-deleted row")
	})

	for _, tc := range []struct {
		name   string
		change func(context.Context, *ent.Tx, uuid.UUID) error
	}{
		{"state_version changed", func(ctx context.Context, tx *ent.Tx, id uuid.UUID) error {
			return tx.Agent.UpdateOneID(id).AddStateVersion(1).Exec(ctx)
		}},
		{"applied_config changed", func(ctx context.Context, tx *ent.Tx, id uuid.UUID) error {
			return tx.Agent.UpdateOneID(id).SetAppliedConfig(`{"image":"example/image:2","runtimeTarget":"` + target + `"}`).Exec(ctx)
		}},
		{"soft-deleted", func(ctx context.Context, tx *ent.Tx, id uuid.UUID) error {
			return tx.Agent.UpdateOneID(id).SetDeletedAt(time.Now()).Exec(ctx)
		}},
	} {
		t.Run("write misses when "+tc.name+" after the read", func(t *testing.T) {
			a := create(t, "guard-write-"+strings.ReplaceAll(tc.name, " ", "-"))
			uid := uuid.MustParse(a.ID)
			calls := hookCalls(t, func(ctx context.Context, tx *ent.Tx, id uuid.UUID) {
				require.NoError(t, tc.change(ctx, tx, id))
			})
			written, err := s.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "", "docker")
			require.NoError(t, err)
			assert.False(t, written)
			assert.Equal(t, 1, *calls)
			// The miss rolls the transaction back, so no target write (and
			// not the injected change either) is visible.
			after, err := s.client.Agent.Get(ctx, uid)
			require.NoError(t, err)
			assert.NotContains(t, after.AppliedConfig, "runtimeTargetCandidate")
		})
	}
}

// captureDriver is a dialect.Driver that records every statement and fails
// it, so a test can see the SQL a store method builds for a dialect it cannot
// execute.
type captureDriver struct {
	dialectName string
	mu          sync.Mutex
	stmts       []string
}

var errCaptured = errors.New("statement captured, not executed")

func (d *captureDriver) record(query string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, query)
	return errCaptured
}

func (d *captureDriver) Exec(_ context.Context, query string, _, _ any) error {
	return d.record(query)
}

func (d *captureDriver) Query(_ context.Context, query string, _, _ any) error {
	return d.record(query)
}

func (d *captureDriver) Tx(context.Context) (dialect.Tx, error) { return dialect.NopTx(d), nil }
func (d *captureDriver) Close() error                           { return nil }
func (d *captureDriver) Dialect() string                        { return d.dialectName }

// TestAgentStore_RuntimeTarget_RowLock pins that the read inside the
// transaction of the target clear and the target write locks the row
// (SELECT ... FOR UPDATE) on Postgres and does not on SQLite, which has no
// row locks.
func TestAgentStore_RuntimeTarget_RowLock(t *testing.T) {
	const id = "00000000-0000-0000-0000-00000000abcd"
	calls := map[string]func(s *AgentStore) error{
		"clear": func(s *AgentStore) error {
			_, _, err := s.ClearAgentRuntimeTarget(context.Background(), id)
			return err
		},
		"set": func(s *AgentStore) error {
			_, err := s.SetAgentRuntimeTarget(context.Background(), id, 1, "docker", "")
			return err
		},
	}
	for _, tc := range []struct {
		dialect string
		lock    bool
	}{
		{dialect.Postgres, true},
		{dialect.SQLite, false},
	} {
		for name, call := range calls {
			t.Run(name+"/"+tc.dialect, func(t *testing.T) {
				drv := &captureDriver{dialectName: tc.dialect}
				s := NewAgentStore(ent.NewClient(ent.Driver(drv)))
				require.Error(t, call(s), "the capture driver fails every statement")

				drv.mu.Lock()
				defer drv.mu.Unlock()
				var read string
				for _, q := range drv.stmts {
					if strings.Contains(q, "SELECT") && strings.Contains(q, "applied_config") {
						read = q
					}
				}
				require.NotEmpty(t, read, "expected the read, got %v", drv.stmts)
				if tc.lock {
					assert.Contains(t, read, "FOR UPDATE")
				} else {
					assert.NotContains(t, read, "FOR UPDATE")
				}
			})
		}
	}
}
