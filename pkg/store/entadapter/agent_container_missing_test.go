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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
