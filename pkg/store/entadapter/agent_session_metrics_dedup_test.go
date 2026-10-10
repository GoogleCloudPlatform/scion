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

// A repeated report for the same agent and session ID (a retry, or a resend
// after the sender died before confirming) keeps one row: the first stored
// one, unchanged. The caller gets ErrAlreadyExists and the stored row's ID.
// The same session ID reported by another agent is a different session.
// Runs against SQLite by default and Postgres with -tags integration.
func TestCreateAgentSessionMetrics_RepeatedSessionKeepsOneRow(t *testing.T) {
	ctx := context.Background()
	s := NewAgentSessionMetricsStore(enttest.NewClient(t))

	started := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	first := &store.AgentSessionMetrics{
		AgentID: "agent-a", ProjectID: "project-1", SessionID: "session-1",
		StartedAt: started, Status: "completed", TurnCount: 3, TokensInput: 100,
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, first))
	require.NotEmpty(t, first.ID)

	repeat := &store.AgentSessionMetrics{
		AgentID: "agent-a", ProjectID: "project-1", SessionID: "session-1",
		StartedAt: started, Status: "error", TurnCount: 1, TokensInput: 7,
	}
	err := s.CreateAgentSessionMetrics(ctx, repeat)
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	assert.Equal(t, first.ID, repeat.ID, "the repeat must be told the stored row's ID")

	rows, err := s.ListAgentSessionMetricsByAgent(ctx, "agent-a")
	require.NoError(t, err)
	require.Len(t, rows, 1, "a repeated report must not add a row")
	assert.Equal(t, 3, rows[0].TurnCount, "the first stored report is kept unchanged")
	assert.Equal(t, int64(100), rows[0].TokensInput)
	assert.Equal(t, "completed", rows[0].Status)

	agg, err := s.AggregateByAgent(ctx, "agent-a")
	require.NoError(t, err)
	assert.Equal(t, 1, agg.Count)
	assert.Equal(t, int64(3), agg.SumTurnCount)

	other := &store.AgentSessionMetrics{
		AgentID: "agent-b", ProjectID: "project-1", SessionID: "session-1",
		StartedAt: started, TurnCount: 2,
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, other))
	rows, err = s.ListAgentSessionMetricsByProject(ctx, "project-1")
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}
