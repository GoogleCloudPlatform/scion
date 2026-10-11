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

const testSSHHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOq1Ofh2a4Rp9yUdu8sUgQhj3eQ8k2jzYhZzF9h4Xk3N"

// TestSetAgentSSHHostKey_CurrentRunOnly: the reported host key is bound
// to the reporting agent's current run. It is written only when runID is
// the stored run_id, and read back with its run.
func TestSetAgentSSHHostKey_CurrentRunOnly(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ssh-host-key")
	require.NoError(t, s.CreateAgent(ctx, a))

	ok, err := s.SetAgentSSHHostKey(ctx, a.ID, "run-1", testSSHHostKey)
	require.NoError(t, err)
	assert.False(t, ok, "no run yet: nothing is written")

	_, err = s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	ok, err = s.SetAgentSSHHostKey(ctx, a.ID, "run-0", testSSHHostKey)
	require.NoError(t, err)
	assert.False(t, ok, "an older run is refused")
	ok, err = s.SetAgentSSHHostKey(ctx, a.ID, "", testSSHHostKey)
	require.NoError(t, err)
	assert.False(t, ok, "an empty run is refused")

	ok, err = s.SetAgentSSHHostKey(ctx, a.ID, "run-1", testSSHHostKey)
	require.NoError(t, err)
	assert.True(t, ok)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, testSSHHostKey, got.SSHHostKey)
	assert.Equal(t, "run-1", got.SSHHostKeyRunID)

	// A new run: the stored key stays bound to run-1, and run-1 can no
	// longer write.
	_, err = s.SetAgentRunID(ctx, a.ID, "run-2", nil)
	require.NoError(t, err)
	ok, err = s.SetAgentSSHHostKey(ctx, a.ID, "run-1", testSSHHostKey)
	require.NoError(t, err)
	assert.False(t, ok)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", got.SSHHostKeyRunID)
	assert.Equal(t, "run-2", got.RunID)

	ok, err = s.SetAgentSSHHostKey(ctx, uuid.NewString(), "run-2", testSSHHostKey)
	require.NoError(t, err)
	assert.False(t, ok, "missing agent")
}

// TestSetAgentSSHHostKey_RefusedWhileDeleteHoldsRow: no write while a
// delete holds the row or after a soft delete.
func TestSetAgentSSHHostKey_RefusedWhileDeleteHoldsRow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	live := now.Add(time.Hour)
	for _, tc := range []struct {
		name      string
		state     string
		deletedAt *time.Time
	}{
		{"deleting, live lease", store.DeletionStateDeleting, nil},
		{"finalizing", store.DeletionStateFinalizing, nil},
		{"soft-deleted", "", &now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "ssh-host-key-delete")
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
			require.NoError(t, err)
			set := store.DeletionFields{DeletedAt: tc.deletedAt}
			if tc.state != "" {
				st := tc.state
				set.State = &st
				set.LeaseAt = &live
			}
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, set)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			ok, err := s.SetAgentSSHHostKey(ctx, a.ID, "run-1", testSSHHostKey)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}
