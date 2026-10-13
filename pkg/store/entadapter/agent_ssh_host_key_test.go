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

	set := func(runID, key string) store.SSHHostKeyResult {
		t.Helper()
		res, err := s.SetAgentSSHHostKey(ctx, a.ID, runID, key)
		require.NoError(t, err)
		return res
	}
	assert.Equal(t, store.SSHHostKeyNotCurrentRun, set("run-1", testSSHHostKey), "no run yet")

	_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	assert.Equal(t, store.SSHHostKeyNotCurrentRun, set("run-0", testSSHHostKey), "an older run")
	assert.Equal(t, store.SSHHostKeyNotCurrentRun, set("", testSSHHostKey), "an empty run")

	assert.Equal(t, store.SSHHostKeyWritten, set("run-1", testSSHHostKey))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, testSSHHostKey, got.SSHHostKey)
	assert.Equal(t, "run-1", got.SSHHostKeyRunID)

	// A new run: the stored key stays bound to run-1, and run-1 can no
	// longer write.
	_, err = s.SetAgentRunID(ctx, a.ID, "run-2", nil)
	require.NoError(t, err)
	assert.Equal(t, store.SSHHostKeyNotCurrentRun, set("run-1", testSSHHostKey))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", got.SSHHostKeyRunID)
	assert.Equal(t, "run-2", got.RunID)

	res, err := s.SetAgentSSHHostKey(ctx, uuid.NewString(), "run-2", testSSHHostKey)
	require.NoError(t, err)
	assert.Equal(t, store.SSHHostKeyNotCurrentRun, res, "missing agent")
}

// TestSetAgentSSHHostKey_FirstKeyPerRunKept: the first host key reported
// for a run is kept; a repeat of the same key succeeds (unchanged); a
// different key for the same run is not recorded; a new run accepts a new
// key.
func TestSetAgentSSHHostKey_FirstKeyPerRunKept(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "ssh-host-key-first")
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	const other = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	set := func(runID, key string) store.SSHHostKeyResult {
		t.Helper()
		res, err := s.SetAgentSSHHostKey(ctx, a.ID, runID, key)
		require.NoError(t, err)
		return res
	}
	stored := func() (string, string) {
		t.Helper()
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return got.SSHHostKey, got.SSHHostKeyRunID
	}

	assert.Equal(t, store.SSHHostKeyWritten, set("run-1", testSSHHostKey))
	assert.Equal(t, store.SSHHostKeyUnchanged, set("run-1", testSSHHostKey), "a repeat of the same key succeeds")
	assert.Equal(t, store.SSHHostKeyAlreadySet, set("run-1", other), "a different key for the same run")
	key, run := stored()
	assert.Equal(t, testSSHHostKey, key, "the first key is kept")
	assert.Equal(t, "run-1", run)

	_, err = s.SetAgentRunID(ctx, a.ID, "run-2", nil)
	require.NoError(t, err)
	assert.Equal(t, store.SSHHostKeyWritten, set("run-2", other), "a new run accepts a new key")
	key, run = stored()
	assert.Equal(t, other, key)
	assert.Equal(t, "run-2", run)
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
			res, err := s.SetAgentSSHHostKey(ctx, a.ID, "run-1", testSSHHostKey)
			require.NoError(t, err)
			assert.Equal(t, store.SSHHostKeyNotCurrentRun, res)
		})
	}
}
