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

// TestGetAgentRunState pins the narrow run read against GetAgent: the same
// run and deletion time for a live and a soft-deleted row, and the same
// ErrNotFound for a missing row and a malformed id. It runs on the enttest
// backend (SQLite by default, PostgreSQL under -tags integration).
func TestGetAgentRunState(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	live := makeAgent(projectID, "run-state-live")
	require.NoError(t, s.CreateAgent(ctx, live))
	_, err := s.SetAgentRunID(ctx, live.ID, "run-live", nil)
	require.NoError(t, err)

	deleted := makeAgent(projectID, "run-state-deleted")
	require.NoError(t, s.CreateAgent(ctx, deleted))
	_, err = s.SetAgentRunID(ctx, deleted.ID, "run-deleted", nil)
	require.NoError(t, err)
	row, err := s.GetAgent(ctx, deleted.ID)
	require.NoError(t, err)
	row.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, row))

	norun := makeAgent(projectID, "run-state-norun")
	require.NoError(t, s.CreateAgent(ctx, norun))

	for name, id := range map[string]string{"live": live.ID, "deleted": deleted.ID, "no run": norun.ID} {
		t.Run(name, func(t *testing.T) {
			full, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			got, err := s.GetAgentRunState(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, full.RunID, got.RunID)
			assert.True(t, full.DeletedAt.Equal(got.DeletedAt), "deleted_at: GetAgent %v, GetAgentRunState %v", full.DeletedAt, got.DeletedAt)
		})
	}

	got, err := s.GetAgentRunState(ctx, live.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentRunState{RunID: "run-live"}, got, "a live row has no deletion time")
	got, err = s.GetAgentRunState(ctx, deleted.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-deleted", got.RunID)
	assert.False(t, got.DeletedAt.IsZero(), "a soft-deleted row is returned with its deletion time")

	for name, id := range map[string]string{"missing": uuid.NewString(), "malformed": "not-a-uuid"} {
		t.Run(name, func(t *testing.T) {
			_, fullErr := s.GetAgent(ctx, id)
			require.ErrorIs(t, fullErr, store.ErrNotFound)
			_, err := s.GetAgentRunState(ctx, id)
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}
