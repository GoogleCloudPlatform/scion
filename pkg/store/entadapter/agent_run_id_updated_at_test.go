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

// runIDUpdatedAt reads the agent's run_id_updated_at (nil when unset).
func runIDUpdatedAt(t *testing.T, s *AgentStore, agentID string) *time.Time {
	t.Helper()
	got, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	return got.RunIDUpdatedAt
}

// TestRunIDUpdatedAt_MovesOnlyWhenRunIDChanges (ptone/scion#3661): every
// run-ID write that changes run_id sets run_id_updated_at in the same
// write; a write that leaves run_id as it was (a settle, a swap that
// misses) and every other write leave it alone.
func TestRunIDUpdatedAt_MovesOnlyWhenRunIDChanges(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "run-id-updated-at")
	require.NoError(t, s.CreateAgent(ctx, a))
	assert.Nil(t, runIDUpdatedAt(t, s, a.ID), "a new row has no run_id_updated_at")

	// moved asserts the timestamp moved forward from prev, and returns it.
	moved := func(what string, prev *time.Time) time.Time {
		t.Helper()
		got := runIDUpdatedAt(t, s, a.ID)
		require.NotNil(t, got, what)
		if prev != nil {
			assert.True(t, got.After(*prev), "%s: run_id_updated_at must move forward", what)
		}
		return *got
	}
	unchanged := func(what string, want time.Time) {
		t.Helper()
		got := runIDUpdatedAt(t, s, a.ID)
		require.NotNil(t, got, what)
		assert.True(t, got.Equal(want), "%s: run_id_updated_at must not move", what)
	}
	tick := func() { time.Sleep(2 * time.Millisecond) }

	// Dispatch: SetAgentRunID to a new run.
	_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	t1 := moved("dispatch", nil)

	// SetAgentRunID to the same run changes nothing.
	tick()
	_, err = s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	unchanged("same-run set", t1)

	// Settle (same-value CAS, the launch report and adoption of the minted
	// run) changes nothing.
	tick()
	ok, err := s.CompareAndSwapAgentRunID(ctx, a.ID, "run-1", "run-1")
	require.NoError(t, err)
	require.True(t, ok)
	unchanged("settle", t1)

	// Restart: a new run.
	tick()
	_, err = s.SetAgentRunID(ctx, a.ID, "run-2", nil)
	require.NoError(t, err)
	t2 := moved("restart", &t1)

	// Broker adoption on a dispatch (CAS to another run).
	tick()
	ok, err = s.CompareAndSwapAgentRunID(ctx, a.ID, "run-2", "run-2b")
	require.NoError(t, err)
	require.True(t, ok)
	t3 := moved("broker adoption", &t2)

	// A missed CAS writes nothing.
	tick()
	ok, err = s.CompareAndSwapAgentRunID(ctx, a.ID, "not-current", "run-x")
	require.NoError(t, err)
	require.False(t, ok)
	unchanged("missed CAS", t3)

	// Revert.
	tick()
	ok, err = s.RevertAgentRunID(ctx, a.ID, "run-2b", "run-2")
	require.NoError(t, err)
	require.True(t, ok)
	t4 := moved("revert", &t3)

	// Heartbeat adoption.
	tick()
	ok, err = s.AdoptAgentRunID(ctx, a.ID, "run-2", "run-1", time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	t5 := moved("heartbeat adoption", &t4)

	// Unrelated writes leave it alone.
	tick()
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got.Message = "unrelated"
	require.NoError(t, s.UpdateAgent(ctx, got))
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Heartbeat: true}))
	unchanged("unrelated writes", t5)
}

// TestAdoptAgentRunID: the swap applies only while run_id still holds the
// expected run, clears previous_run_ids, and writes nothing while a delete
// holds the row or the row is soft-deleted.
func TestAdoptAgentRunID(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	live := now.Add(time.Minute)

	t.Run("swaps and settles", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "adopt-run")
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.SetAgentRunID(ctx, a.ID, "run-l", nil)
		require.NoError(t, err)
		_, err = s.SetAgentRunID(ctx, a.ID, "run-r", nil)
		require.NoError(t, err)

		ok, err := s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", time.Now())
		require.NoError(t, err)
		assert.True(t, ok)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-l", got.RunID)
		assert.Empty(t, got.PreviousRunIDs)
	})

	t.Run("concurrent change loses cleanly", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "adopt-run-lost")
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.SetAgentRunID(ctx, a.ID, "run-newer", nil)
		require.NoError(t, err)
		before := runIDUpdatedAt(t, s, a.ID)

		ok, err := s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", time.Now())
		require.NoError(t, err)
		assert.False(t, ok)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-newer", got.RunID)
		assert.True(t, got.RunIDUpdatedAt.Equal(*before))
	})

	t.Run("run_id changed and changed back after the read", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "adopt-run-aba")
		require.NoError(t, s.CreateAgent(ctx, a))
		_, err := s.SetAgentRunID(ctx, a.ID, "run-r", nil)
		require.NoError(t, err)
		cutoff := time.Now() // the caller's read: R, unchanged since before this
		time.Sleep(2 * time.Millisecond)
		_, err = s.SetAgentRunID(ctx, a.ID, "run-x", nil)
		require.NoError(t, err)
		ok, err := s.RevertAgentRunID(ctx, a.ID, "run-x", "run-r")
		require.NoError(t, err)
		require.True(t, ok)
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)

		ok, err = s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", cutoff)
		require.NoError(t, err)
		assert.False(t, ok, "run_id_updated_at is after the cutoff")
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-r", got.RunID)
		assert.Equal(t, before.PreviousRunIDs, got.PreviousRunIDs, "nothing written")
		assert.True(t, got.RunIDUpdatedAt.Equal(*before.RunIDUpdatedAt), "nothing written")

		// With a cutoff after the revert, the same swap applies.
		ok, err = s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", time.Now())
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("no run_id_updated_at", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "adopt-run-nil")
		require.NoError(t, s.CreateAgent(ctx, a))
		// A row whose run_id was set before the column existed: write
		// run_id alone.
		require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetRunID("run-r").Exec(ctx))
		ok, err := s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", time.Now())
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("missing agent", func(t *testing.T) {
		s, _ := newTestAgentStore(t)
		ok, err := s.AdoptAgentRunID(ctx, uuid.NewString(), "a", "b", time.Now())
		require.NoError(t, err)
		assert.False(t, ok)
	})

	for _, tc := range []struct {
		name      string
		state     string
		deletedAt *time.Time
	}{
		{"deleting, live lease", store.DeletionStateDeleting, nil},
		{"finalizing", store.DeletionStateFinalizing, nil},
		{"soft-deleted", "", &now},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "adopt-run-delete")
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetAgentRunID(ctx, a.ID, "run-r", nil)
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

			ok, err := s.AdoptAgentRunID(ctx, a.ID, "run-r", "run-l", time.Now())
			require.NoError(t, err)
			assert.False(t, ok)
			got, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
			require.NoError(t, err)
			assert.Equal(t, "run-r", got.RunID)
		})
	}
}

// TestHasAgentCredentialForRun: true only for a credential row of the
// same agent with the same run, revoked or not.
func TestHasAgentCredentialForRun(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	creds := NewAgentCredentialStore(s.client)
	agentID, otherID := uuid.NewString(), uuid.NewString()
	add := func(agent, run, jti string) *store.AgentCredential {
		c := &store.AgentCredential{
			AgentID: agent, ProjectID: projectID, TokenJTIHash: jti, RunID: run,
			IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}
		require.NoError(t, creds.CreateAgentCredential(ctx, c))
		return c
	}
	add(agentID, "run-a", "jti-1")
	revoked := add(agentID, "run-b", "jti-2")
	require.NoError(t, creds.RevokeAgentCredential(ctx, revoked.ID, "system", "test"))
	add(otherID, "run-c", "jti-3")
	add(agentID, "", "jti-4")

	for _, tc := range []struct {
		agent, run string
		want       bool
	}{
		{agentID, "run-a", true},
		{agentID, "run-b", true},
		{agentID, "run-c", false},
		{otherID, "run-a", false},
		{agentID, "", false},
		{"", "run-a", false},
	} {
		got, err := creds.HasAgentCredentialForRun(ctx, tc.agent, tc.run)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "agent %q run %q", tc.agent, tc.run)
	}
}
