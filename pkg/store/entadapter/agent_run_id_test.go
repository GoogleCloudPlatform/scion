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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
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

	prev, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	assert.Equal(t, "", prev, "the previous value of a new row is empty")
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

	prev, err = s.SetAgentRunID(ctx, a.ID, "run-2", nil)
	require.NoError(t, err)
	assert.Equal(t, "run-1", prev, "SetAgentRunID returns the value it replaced")
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)

	_, err = s.SetAgentRunID(ctx, uuid.NewString(), "x", nil)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestCompareAndSwapAgentRunID: the swap applies only while the row still
// holds the expected run ID, so a late correction cannot overwrite a newer
// run, and it never bumps state_version.
func TestCompareAndSwapAgentRunID(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "run-id-cas-agent")
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "minted", nil)
	require.NoError(t, err)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	ok, err := s.CompareAndSwapAgentRunID(ctx, a.ID, "minted", "actual")
	require.NoError(t, err)
	assert.True(t, ok)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "actual", got.RunID)
	assert.Equal(t, before.StateVersion, got.StateVersion, "CAS must not bump state_version")

	// A newer dispatch recorded its own run; a stale swap is a no-op.
	_, err = s.SetAgentRunID(ctx, a.ID, "newer", nil)
	require.NoError(t, err)
	ok, err = s.CompareAndSwapAgentRunID(ctx, a.ID, "minted", "stale")
	require.NoError(t, err)
	assert.False(t, ok)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "newer", got.RunID)

	ok, err = s.CompareAndSwapAgentRunID(ctx, uuid.NewString(), "", "x")
	require.NoError(t, err)
	assert.False(t, ok, "a missing agent swaps nothing")
}

// SetAgentRunID's retry: a writer that changes run_id between the read and
// the swap makes the swap miss, and the loop re-reads. One interference
// still succeeds and returns the value actually replaced (the interferer's);
// interference on every attempt gives up with an error and leaves the
// interferer's value in place.
func TestSetAgentRunID_RetriesOnConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "run-id-retry-agent")
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-0", nil)
	require.NoError(t, err)

	interfere := func(times int) *int {
		calls := 0
		s.afterRunIDRead = func(id string) {
			calls++
			if calls <= times {
				_, err := s.client.Agent.UpdateOneID(uuid.MustParse(id)).
					SetRunID(fmt.Sprintf("other-%d", calls)).Save(ctx)
				require.NoError(t, err)
			}
		}
		return &calls
	}

	t.Run("one concurrent write", func(t *testing.T) {
		calls := interfere(1)
		prev, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
		require.NoError(t, err)
		assert.Equal(t, "other-1", prev, "the previous value is the one the swap replaced")
		assert.Equal(t, 2, *calls)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "run-1", got.RunID)
	})

	t.Run("a concurrent write on every attempt", func(t *testing.T) {
		calls := interfere(setAgentRunIDAttempts)
		_, err := s.SetAgentRunID(ctx, a.ID, "run-2", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "kept changing")
		assert.Equal(t, setAgentRunIDAttempts, *calls)
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("other-%d", setAgentRunIDAttempts), got.RunID, "a failed set writes nothing")
	})
}

// A delete that holds the row refuses SetAgentRunID with
// store.ErrDeleteInProgress and writes nothing, so a delete's claim and a
// start's run-ID write are ordered (ptone/scion#2550 P1 round 3). The rule
// is the start gate's: finalizing, or deleting under a live lease, or
// soft-deleted. A failed delete, or a deleting row whose lease has passed,
// does not block.
func TestSetAgentRunID_RefusedWhileDeleteHoldsRow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	live, expired := now.Add(time.Minute), now.Add(-time.Minute)
	for _, tc := range []struct {
		name      string
		state     string
		leaseAt   *time.Time
		deletedAt *time.Time
		refused   bool
	}{
		{"no delete", "", nil, nil, false},
		{"deleting, live lease", store.DeletionStateDeleting, &live, nil, true},
		{"deleting, lease expired", store.DeletionStateDeleting, &expired, nil, false},
		{"deleting, lease nil", store.DeletionStateDeleting, nil, nil, false},
		{"finalizing, live lease", store.DeletionStateFinalizing, &live, nil, true},
		{"finalizing, lease expired", store.DeletionStateFinalizing, &expired, nil, true},
		{"failed", store.DeletionStateFailed, nil, nil, false},
		{"soft-deleted", "", nil, &now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := makeAgent(projectID, "run-id-delete-agent")
			require.NoError(t, s.CreateAgent(ctx, a))
			_, err := s.SetAgentRunID(ctx, a.ID, "run-0", nil)
			require.NoError(t, err)
			set := store.DeletionFields{DeletedAt: tc.deletedAt}
			if tc.state != "" {
				st := tc.state
				set.State = &st
			}
			if tc.leaseAt != nil {
				set.LeaseAt = tc.leaseAt
			}
			n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, set)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			prev, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
			got, gerr := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
			require.NoError(t, gerr)
			if tc.refused {
				require.ErrorIs(t, err, store.ErrDeleteInProgress)
				assert.Equal(t, "run-0", got.RunID, "a refused set writes nothing")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "run-0", prev)
				assert.Equal(t, "run-1", got.RunID)
			}
		})
	}
}

// A delete that claims between SetAgentRunID's read and its swap makes the
// swap miss on the delete predicate; the retry re-reads and refuses.
func TestSetAgentRunID_ClaimBetweenReadAndSwapRefuses(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "run-id-claim-race-agent")
	require.NoError(t, s.CreateAgent(ctx, a))
	_, err := s.SetAgentRunID(ctx, a.ID, "run-0", nil)
	require.NoError(t, err)
	lease := time.Now().Add(time.Minute)
	deleting := store.DeletionStateDeleting
	s.afterRunIDRead = func(id string) {
		s.afterRunIDRead = nil
		_, err := s.UpdateAgentDeletion(ctx, id, store.DeletionPredicate{}, store.DeletionFields{State: &deleting, LeaseAt: &lease, BumpClaim: true})
		require.NoError(t, err)
	}
	_, err = s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.ErrorIs(t, err, store.ErrDeleteInProgress)
	got, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
	require.NoError(t, err)
	assert.Equal(t, "run-0", got.RunID)
}

// runIDWritable (the SQL form, in SetAgentRunID's swap) and
// store.DeletionHoldsRow (the Go form, in SetAgentRunID's pre-read and the
// hub's start gate) agree over every deletion state x lease x deleted_at
// combination (ptone/scion#2550 P1 round 4, DN-2).
func TestRunIDWritable_MatchesGoPredicate(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	now := time.Now()
	live, expired := now.Add(time.Minute), now.Add(-time.Minute)
	states := []*string{nil}
	for _, st := range []string{"", store.DeletionStateDeleting, store.DeletionStateFinalizing, store.DeletionStateFailed} {
		st := st
		states = append(states, &st)
	}
	leases := map[string]*time.Time{"nil": nil, "live": &live, "expired": &expired}
	deleted := map[string]*time.Time{"live row": nil, "soft-deleted": &now}
	i := 0
	for _, st := range states {
		for leaseName, lease := range leases {
			for delName, del := range deleted {
				stName := "NULL"
				if st != nil {
					stName = fmt.Sprintf("%q", *st)
				}
				name := fmt.Sprintf("state %s, lease %s, %s", stName, leaseName, delName)
				i++
				a := makeAgent(projectID, fmt.Sprintf("run-id-writable-%d", i))
				require.NoError(t, s.CreateAgent(ctx, a), name)
				n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{},
					store.DeletionFields{State: st, LeaseAt: lease, DeletedAt: del})
				require.NoError(t, err, name)
				require.Equal(t, 1, n, name)

				row, err := s.client.Agent.Get(ctx, uuid.MustParse(a.ID))
				require.NoError(t, err, name)
				goWritable := row.DeletedAt == nil && !store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now)
				sqlWritable, err := s.client.Agent.Query().
					Where(agent.IDEQ(row.ID), runIDWritable(now)).Exist(ctx)
				require.NoError(t, err, name)
				assert.Equal(t, goWritable, sqlWritable, name)
			}
		}
	}
}

// TestSetAgentRunIDWithCredentialIsOneTransaction: with a credential,
// SetAgentRunID records the run ID and the credential (bound to that run)
// together, or neither. A credential insert that fails leaves run_id and
// previous_run_ids as they were.
func TestSetAgentRunIDWithCredentialIsOneTransaction(t *testing.T) {
	tests := []struct {
		name           string
		duplicateJTI   bool // a credential with the same JTI hash already exists
		missingAgent   bool
		wantErr        bool
		wantRunID      string
		wantPrevious   []string
		wantCredStored bool
	}{
		{name: "both recorded", wantRunID: "run-2", wantPrevious: []string{"run-1"}, wantCredStored: true},
		{name: "credential insert fails, run id unchanged", duplicateJTI: true, wantErr: true, wantRunID: "run-1"},
		{name: "no agent row, nothing recorded", missingAgent: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			creds := NewAgentCredentialStore(s.client)
			a := makeAgent(projectID, "run-id-cred-agent")
			agentID := a.ID
			if tt.missingAgent {
				agentID = uuid.NewString()
			} else {
				require.NoError(t, s.CreateAgent(ctx, a))
				_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
				require.NoError(t, err)
				_, err = s.CompareAndSwapAgentRunID(ctx, a.ID, "run-1", "run-1")
				require.NoError(t, err)
			}
			jtiHash := "jti-hash-" + tt.name
			if tt.duplicateJTI {
				require.NoError(t, creds.CreateAgentCredential(ctx, &store.AgentCredential{
					AgentID: agentID, ProjectID: projectID, TokenJTIHash: jtiHash,
					IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
				}))
			}
			cred := &store.AgentCredential{
				AgentID: agentID, ProjectID: projectID, TokenJTIHash: jtiHash,
				IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
			}

			_, err := s.SetAgentRunID(ctx, agentID, "run-2", cred)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, cred.ID, "a credential that was not recorded has no ID")
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, cred.ID)
			}

			if !tt.missingAgent {
				got, err := s.GetAgent(ctx, agentID)
				require.NoError(t, err)
				assert.Equal(t, tt.wantRunID, got.RunID)
				assert.Equal(t, tt.wantPrevious, got.PreviousRunIDs)
			}
			stored, err := creds.GetAgentCredentialByJTIHash(ctx, jtiHash)
			switch {
			case tt.wantCredStored:
				require.NoError(t, err)
				assert.Equal(t, "run-2", stored.RunID, "the credential is bound to the run recorded with it")
			case tt.duplicateJTI:
				require.NoError(t, err)
				assert.Empty(t, stored.RunID, "the pre-existing credential is untouched")
			default:
				assert.ErrorIs(t, err, store.ErrNotFound)
			}
		})
	}
}
