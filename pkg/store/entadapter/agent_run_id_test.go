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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetAgentRunID covers the run_id column (ptone/scion#2550): a new row
// has "", SetAgentRunID persists without bumping state_version, and a
// whole-row UpdateAgent from a struct read before (or carrying a different
// RunID) neither conflicts nor clobbers it.
func TestSetAgentRunID(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "run-id-agent")
	require.NoError(t, s.CreateAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.RunID, "a new row has no run ID")
	stale := *got
	v0 := got.StateVersion

	require.NoError(t, s.SetAgentRunID(ctx, a.ID, "run-1"))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", got.RunID)
	assert.Equal(t, v0, got.StateVersion, "SetAgentRunID must not bump state_version")

	// A CAS write from the struct read before SetAgentRunID still succeeds
	// (no version conflict) and does not write its stale RunID back.
	stale.Message = "updated"
	stale.RunID = "bogus"
	require.NoError(t, s.UpdateAgent(ctx, &stale))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", got.RunID, "UpdateAgent must not write run_id")
	assert.Equal(t, "updated", got.Message)

	require.NoError(t, s.SetAgentRunID(ctx, a.ID, "run-2"))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)

	assert.ErrorIs(t, s.SetAgentRunID(ctx, uuid.NewString(), "x"), store.ErrNotFound)
}
