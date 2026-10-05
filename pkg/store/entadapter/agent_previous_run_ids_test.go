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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// previous_run_ids (ptone/scion#3097): SetAgentRunID appends the run it
// replaced, CompareAndSwapAgentRunID clears the list, and UpdateAgent never
// writes it. These run against Postgres too (make test-launch-store-postgres).

func TestPreviousRunIDs_SetAppendsAndCASClears(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "prev-runs")
	require.NoError(t, s.CreateAgent(ctx, a))

	_, err := s.SetAgentRunID(ctx, a.ID, "run-1")
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.PreviousRunIDs, "an empty run is never listed")

	_, err = s.SetAgentRunID(ctx, a.ID, "run-2")
	require.NoError(t, err)
	_, err = s.SetAgentRunID(ctx, a.ID, "run-3")
	require.NoError(t, err)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-3", got.RunID)
	assert.Equal(t, []string{"run-1", "run-2"}, got.PreviousRunIDs, "oldest first")

	// UpdateAgent from a struct carrying a different list leaves it alone.
	stale := *got
	stale.PreviousRunIDs = []string{"bogus"}
	stale.Message = "updated"
	require.NoError(t, s.UpdateAgent(ctx, &stale))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"run-1", "run-2"}, got.PreviousRunIDs, "UpdateAgent must not write previous_run_ids")

	// A failed swap (wrong expected run) clears nothing.
	swapped, err := s.CompareAndSwapAgentRunID(ctx, a.ID, "run-2", "run-1")
	require.NoError(t, err)
	require.False(t, swapped)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"run-1", "run-2"}, got.PreviousRunIDs)

	// A same-value swap settles the run: the list is cleared.
	swapped, err = s.CompareAndSwapAgentRunID(ctx, a.ID, "run-3", "run-3")
	require.NoError(t, err)
	require.True(t, swapped)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-3", got.RunID)
	assert.Empty(t, got.PreviousRunIDs)
}

// Reverting to the previous run (a start that was never sent) leaves that
// run current with no previous runs.
func TestPreviousRunIDs_RevertClears(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "prev-revert")
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-1")
	require.NoError(t, err)
	prev, err := s.SetAgentRunID(ctx, a.ID, "run-2")
	require.NoError(t, err)
	swapped, err := s.CompareAndSwapAgentRunID(ctx, a.ID, "run-2", prev)
	require.NoError(t, err)
	require.True(t, swapped)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", got.RunID)
	assert.Empty(t, got.PreviousRunIDs)
}

// The list never holds the current run or a duplicate: a run that comes
// back as current (a broker-reported run, then a new start) is dropped
// from it.
func TestPreviousRunIDs_Dedupe(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "prev-dedupe")
	require.NoError(t, s.CreateAgent(ctx, a))
	for _, r := range []string{"run-1", "run-2", "run-1", "run-2"} {
		_, err := s.SetAgentRunID(ctx, a.ID, r)
		require.NoError(t, err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, []string{"run-1"}, got.PreviousRunIDs)
}

// At the cap the oldest run is dropped.
func TestPreviousRunIDs_Cap(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "prev-cap")
	require.NoError(t, s.CreateAgent(ctx, a))
	n := store.MaxPreviousRunIDs + 2
	for i := 0; i <= n; i++ {
		_, err := s.SetAgentRunID(ctx, a.ID, fmt.Sprintf("run-%02d", i))
		require.NoError(t, err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("run-%02d", n), got.RunID)
	require.Len(t, got.PreviousRunIDs, store.MaxPreviousRunIDs)
	assert.Equal(t, fmt.Sprintf("run-%02d", n-store.MaxPreviousRunIDs), got.PreviousRunIDs[0], "the oldest runs were dropped")
	assert.Equal(t, fmt.Sprintf("run-%02d", n-1), got.PreviousRunIDs[store.MaxPreviousRunIDs-1])
}
