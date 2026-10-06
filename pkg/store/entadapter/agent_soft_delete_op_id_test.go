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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SetAgentSoftDeleteOpID stamps and clears soft_delete_op_id, and
// UpdateAgent leaves the column as it is, whatever the struct carries.
func TestAgentSoftDeleteOpIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "soft-op-id")
	require.NoError(t, s.CreateAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.SoftDeleteOpID, "unset on create")

	require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, a.ID, "op-1"))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "op-1", got.SoftDeleteOpID)
	version := got.StateVersion

	require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, a.ID, ""))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.SoftDeleteOpID, "an empty value clears the column")
	assert.Equal(t, version, got.StateVersion, "the op ID write does not bump state_version")

	assert.ErrorIs(t, s.SetAgentSoftDeleteOpID(ctx, "00000000-0000-0000-0000-000000000000", "op-x"), store.ErrNotFound)
}

// UpdateAgent never writes soft_delete_op_id: a struct with the current
// state_version and an empty (or different) SoftDeleteOpID leaves the stored
// value in place.
func TestUpdateAgentLeavesSoftDeleteOpIDUnchanged(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "soft-op-id-keep")
	require.NoError(t, s.CreateAgent(ctx, a))
	row, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	row.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, row))
	require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, a.ID, "op-keep"))

	for _, carried := range []string{"", "op-other"} {
		row, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		row.SoftDeleteOpID = carried
		row.Message = "write with op ID " + carried
		require.NoError(t, s.UpdateAgent(ctx, row))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "op-keep", got.SoftDeleteOpID, "UpdateAgent carrying %q leaves the column unchanged", carried)
		assert.False(t, got.DeletedAt.IsZero())
	}
}
