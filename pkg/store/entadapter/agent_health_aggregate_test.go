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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// healthAgentSeeder creates agents with a given phase, activity and broker
// and pins their updated time so reference order is deterministic.
type healthAgentSeeder struct {
	t         *testing.T
	s         *AgentStore
	projectID string
	base      time.Time
	n         int
}

func (h *healthAgentSeeder) add(phase, activity, broker string) *store.Agent {
	h.t.Helper()
	ctx := context.Background()
	h.n++
	a := makeAgent(h.projectID, fmt.Sprintf("h-%03d", h.n))
	a.Phase = phase
	a.Activity = activity
	a.RuntimeBrokerID = broker
	require.NoError(h.t, h.s.CreateAgent(ctx, a))
	uid := uuid.MustParse(a.ID)
	require.NoError(h.t, h.s.client.Agent.UpdateOneID(uid).
		SetUpdated(h.base.Add(time.Duration(h.n)*time.Second)).Exec(ctx))
	return a
}

func (h *healthAgentSeeder) softDelete(a *store.Agent) {
	h.t.Helper()
	require.NoError(h.t, h.s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
		SetDeletedAt(time.Now()).Exec(context.Background()))
}

func newHealthAgentSeeder(t *testing.T) *healthAgentSeeder {
	s, projectID := newTestAgentStore(t)
	return &healthAgentSeeder{t: t, s: s, projectID: projectID,
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
}

func refIDs(refs []store.AgentHealthRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

func TestAggregateAgentHealth_Empty(t *testing.T) {
	h := newHealthAgentSeeder(t)
	agg, err := h.s.AggregateAgentHealth(context.Background())
	require.NoError(t, err)
	assert.Zero(t, agg.Total)
	assert.Zero(t, agg.Errored)
	assert.Zero(t, agg.Considered)
	assert.Empty(t, agg.ByPhase)
	assert.Empty(t, agg.ByBroker)
	assert.Zero(t, agg.ErrorPhase.Count)
	assert.Nil(t, agg.ErrorPhase.Refs)
	assert.Nil(t, agg.Crashed.Refs)
	assert.Nil(t, agg.Offline.Refs)
}

func TestAggregateAgentHealth_CountsAndRefs(t *testing.T) {
	h := newHealthAgentSeeder(t)
	ctx := context.Background()

	running := h.add("running", "thinking", "b1")
	stalled := h.add("running", "stalled", "b1")     // a stall is not a problem
	errAndCrashed := h.add("error", "crashed", "b1") // counted once in Errored and Attention
	errored := h.add("error", "", "b2")
	crashed := h.add("running", "crashed", "b2")
	offline := h.add("running", "offline", "b2")
	suspendedCrashed := h.add("suspended", "crashed", "") // considered, so errored
	stoppedCrashed := h.add("stopped", "crashed", "b1")   // terminal crash on a stopped agent: excluded
	stoppedOffline := h.add("stopped", "offline", "b1")   // stale offline on a stopped agent: excluded
	h.add("stopped", "", "b2")
	gone := h.add("error", "crashed", "b1")
	h.softDelete(gone)

	agg, err := h.s.AggregateAgentHealth(ctx)
	require.NoError(t, err)

	assert.Equal(t, 10, agg.Total)
	assert.Equal(t, map[string]int{"running": 4, "error": 2, "suspended": 1, "stopped": 3}, agg.ByPhase)
	assert.Equal(t, 7, agg.Considered, "non-deleted, not stopped")
	assert.Equal(t, 4, agg.Errored, "error or crashed, each agent once, stopped excluded")
	assert.LessOrEqual(t, agg.Errored, agg.Considered)

	assert.Equal(t, 2, agg.ErrorPhase.Count)
	assert.Equal(t, []string{errored.ID, errAndCrashed.ID}, refIDs(agg.ErrorPhase.Refs), "most recently updated first")
	assert.Equal(t, 3, agg.Crashed.Count)
	assert.Equal(t, []string{suspendedCrashed.ID, crashed.ID, errAndCrashed.ID}, refIDs(agg.Crashed.Refs))
	assert.Equal(t, 1, agg.Offline.Count)
	assert.Equal(t, []string{offline.ID}, refIDs(agg.Offline.Refs))

	// Reference fields.
	r := agg.Offline.Refs[0]
	assert.Equal(t, offline.Name, r.Name)
	assert.Equal(t, h.projectID, r.ProjectID)
	assert.Equal(t, "b2", r.BrokerID)

	// Per broker: running and needing attention (stopped and stalled excluded,
	// error+crashed once). No broker key for an unplaced agent.
	assert.Equal(t, map[string]store.AgentBrokerCounts{
		"b1": {Running: 2, Attention: 1},
		"b2": {Running: 2, Attention: 3},
	}, agg.ByBroker)

	// No stalled output anywhere: the stalled agent is in no group.
	for _, g := range []store.AgentProblemGroup{agg.ErrorPhase, agg.Crashed, agg.Offline} {
		assert.NotContains(t, refIDs(g.Refs), stalled.ID)
		assert.NotContains(t, refIDs(g.Refs), running.ID)
		assert.NotContains(t, refIDs(g.Refs), stoppedCrashed.ID)
		assert.NotContains(t, refIDs(g.Refs), stoppedOffline.ID)
		assert.NotContains(t, refIDs(g.Refs), gone.ID)
	}
}

func TestAggregateAgentHealth_CountAboveCap(t *testing.T) {
	h := newHealthAgentSeeder(t)
	const n = store.AgentHealthRefCap + 5
	var last *store.Agent
	for i := 0; i < n; i++ {
		last = h.add("error", "", "b1")
	}
	agg, err := h.s.AggregateAgentHealth(context.Background())
	require.NoError(t, err)
	assert.Equal(t, n, agg.ErrorPhase.Count, "true count, not the capped list length")
	assert.Len(t, agg.ErrorPhase.Refs, store.AgentHealthRefCap)
	assert.Equal(t, last.ID, agg.ErrorPhase.Refs[0].ID)
	assert.Equal(t, n, agg.Errored)
	assert.Equal(t, n, agg.ByBroker["b1"].Attention)
}
