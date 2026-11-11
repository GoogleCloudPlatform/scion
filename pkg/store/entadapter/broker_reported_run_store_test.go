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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrokerReportedRuns (ptone/scion#3661): a report is upserted per
// (agent, run, broker) with the latest seen_at, read back by
// AgentRunReportedSince, pruned once older than the cutoff, skipped (with
// an error, the others still recorded) for a missing agent, and removed
// with the agent.
func TestBrokerReportedRuns(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "reported-runs")
	require.NoError(t, s.CreateAgent(ctx, a))
	t0 := time.Now().Add(-time.Hour)

	reported := func(run string, since time.Time) bool {
		t.Helper()
		ok, err := s.AgentRunReportedSince(ctx, a.ID, run, since)
		require.NoError(t, err)
		return ok
	}

	require.NoError(t, s.RecordBrokerReportedRuns(ctx, "broker-c",
		[]store.BrokerReportedRun{{AgentID: a.ID, RunID: "run-r"}, {AgentID: a.ID, RunID: "run-r"}, {AgentID: a.ID, RunID: ""}},
		t0, time.Time{}))
	assert.True(t, reported("run-r", t0))
	assert.False(t, reported("run-r", t0.Add(time.Second)))
	assert.False(t, reported("run-other", time.Time{}))

	// Upsert moves seen_at forward; the same run from another broker is
	// its own row.
	t1 := t0.Add(10 * time.Minute)
	require.NoError(t, s.RecordBrokerReportedRuns(ctx, "broker-c",
		[]store.BrokerReportedRun{{AgentID: a.ID, RunID: "run-r"}}, t1, time.Time{}))
	require.NoError(t, s.RecordBrokerReportedRuns(ctx, "broker-d",
		[]store.BrokerReportedRun{{AgentID: a.ID, RunID: "run-d"}}, t0, time.Time{}))
	assert.True(t, reported("run-r", t1))
	n, err := s.client.BrokerReportedRun.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// A missing agent's run is skipped with an error; the others are
	// recorded.
	err = s.RecordBrokerReportedRuns(ctx, "broker-c",
		[]store.BrokerReportedRun{{AgentID: uuid.NewString(), RunID: "run-x"}, {AgentID: a.ID, RunID: "run-y"}}, t1, time.Time{})
	assert.Error(t, err)
	assert.True(t, reported("run-y", t1))

	// Prune: rows before the cutoff go, whichever broker wrote them.
	require.NoError(t, s.RecordBrokerReportedRuns(ctx, "broker-e", nil, t1, t0.Add(time.Minute)))
	assert.False(t, reported("run-d", time.Time{}), "pruned")
	assert.True(t, reported("run-r", t1))

	// Removed with the agent.
	require.NoError(t, s.client.Agent.DeleteOneID(uuid.MustParse(a.ID)).Exec(ctx))
	n, err = s.client.BrokerReportedRun.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
