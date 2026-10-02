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

	guards := []struct {
		name   string
		broker string
		mutate func(a *store.Agent)
	}{
		{name: "other broker", broker: "broker-2"},
		{name: "not running", broker: "broker-1", mutate: func(a *store.Agent) { a.Phase = "provisioning" }},
		{name: "seen after cutoff", broker: "broker-1", mutate: func(a *store.Agent) { a.LastSeen = time.Now() }},
	}
	for i, tc := range guards {
		t.Run(tc.name, func(t *testing.T) {
			a := create("guard-"+string(rune('a'+i)), tc.mutate)
			got, err := s.MarkAgentContainerMissing(ctx, a.ID, tc.broker, cutoff, "gone")
			require.NoError(t, err)
			assert.Nil(t, got)
			stored, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.NotEqual(t, "error", stored.Phase)
			assert.Empty(t, stored.ExitReason)
		})
	}

	t.Run("reincarnating", func(t *testing.T) {
		a := create("reincarnating", nil)
		a.ReincarnationState = store.ReincarnationStatePending
		require.NoError(t, s.UpdateAgent(ctx, a))
		got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("soft deleted", func(t *testing.T) {
		a := create("deleted", nil)
		a.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(ctx, a))
		got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

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
	var update string
	for _, q := range stmts {
		if strings.Contains(q, "UPDATE") && strings.Contains(q, "agents") {
			update = q
			break
		}
		assert.NotContains(t, q, "SELECT", "no read may precede the conditional update: %s", q)
	}
	require.NotEmpty(t, update, "expected an UPDATE statement, got %v", stmts)
	for _, cond := range []string{"deleted_at", "runtime_broker_id", "phase", "reincarnation_state", "last_seen"} {
		where := update[strings.Index(update, "WHERE"):]
		assert.Contains(t, where, cond, "guard %s must be in the UPDATE predicate", cond)
	}
	assert.Regexp(t, `deleted_at[`+"`"+`"]? IS NULL`, update, "soft-delete guard must be deleted_at IS NULL")
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
			assert.Equal(t, clearRuntimeTargetAttempts, calls, "retries are bounded")
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

// TestAgentStore_ClearAgentRuntimeTarget_RowLock pins that the read inside
// the clear's transaction locks the row (SELECT ... FOR UPDATE) on Postgres
// and does not on SQLite, which has no row locks.
func TestAgentStore_ClearAgentRuntimeTarget_RowLock(t *testing.T) {
	const id = "00000000-0000-0000-0000-00000000abcd"
	for _, tc := range []struct {
		dialect string
		lock    bool
	}{
		{dialect.Postgres, true},
		{dialect.SQLite, false},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			drv := &captureDriver{dialectName: tc.dialect}
			s := NewAgentStore(ent.NewClient(ent.Driver(drv)))
			_, _, err := s.ClearAgentRuntimeTarget(context.Background(), id)
			require.Error(t, err, "the capture driver fails every statement")

			drv.mu.Lock()
			defer drv.mu.Unlock()
			var read string
			for _, q := range drv.stmts {
				if strings.Contains(q, "SELECT") && strings.Contains(q, "applied_config") {
					read = q
				}
			}
			require.NotEmpty(t, read, "expected the clear's read, got %v", drv.stmts)
			if tc.lock {
				assert.Contains(t, read, "FOR UPDATE")
			} else {
				assert.NotContains(t, read, "FOR UPDATE")
			}
		})
	}
}
