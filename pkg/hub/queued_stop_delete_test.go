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

package hub

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteAfterStartReleaseStore runs afterRelease once, right after the
// start's claim is released: after the start, before the superseded
// queued-stop clear re-reads the row (ptone/scion#3696).
type deleteAfterStartReleaseStore struct {
	store.Store
	afterRelease func(ctx context.Context, agentID string)
	fired        bool
}

func (s *deleteAfterStartReleaseStore) ReleaseAgentStart(ctx context.Context, agentID, claimID, owner string) (bool, error) {
	held, err := s.Store.ReleaseAgentStart(ctx, agentID, claimID, owner)
	if !s.fired && s.afterRelease != nil {
		s.fired = true
		s.afterRelease(ctx, agentID)
	}
	return held, err
}

// setDeletion writes a delete marker on a row that has none.
func setDeletion(t *testing.T, s store.Store, agentID, state string, leaseAt time.Time) {
	t.Helper()
	n, err := s.UpdateAgentDeletion(context.Background(), agentID,
		store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNull: true},
		store.DeletionFields{State: &state, BumpClaim: true, LeaseAt: &leaseAt})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// A start that supersedes a queued stop clears the stop_queued status and
// notice afterwards, unless a delete has won the row by then: a live
// deleting claim or a finalizing row (even with its lease expired). A
// failed delete, or a deleting row whose lease lapsed, is a live row and is
// still cleared, as is a row with no delete at all.
func TestQueuedStop_SupersedingStartClearSkipsDeleteWonRow(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name    string
		delete  func(t *testing.T, f *reconcileFixture, agentID string)
		cleared bool
	}{
		{name: "no delete", cleared: true},
		{name: "live delete claim", delete: func(t *testing.T, f *reconcileFixture, agentID string) {
			plan, err := f.srv.claimAgentDeletion(context.Background(), agentID, agentDeleteParams{requestedBy: "test"})
			require.NoError(t, err)
			require.NotNil(t, plan, "the delete claims the row")
		}},
		{name: "finalizing with expired lease", delete: func(t *testing.T, f *reconcileFixture, agentID string) {
			setDeletion(t, f.s, agentID, store.DeletionStateFinalizing, past)
		}},
		{name: "deleting with lapsed lease", cleared: true, delete: func(t *testing.T, f *reconcileFixture, agentID string) {
			setDeletion(t, f.s, agentID, store.DeletionStateDeleting, past)
		}},
		{name: "failed delete", cleared: true, delete: func(t *testing.T, f *reconcileFixture, agentID string) {
			setDeletion(t, f.s, agentID, store.DeletionStateFailed, past)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, a := newClaimFixture(t)
			httpOnlyBroker(t, f)
			queueStop(t, f, a, "")

			var afterDelete *store.Agent
			ws := &deleteAfterStartReleaseStore{Store: f.s}
			if tc.delete != nil {
				ws.afterRelease = func(_ context.Context, agentID string) {
					tc.delete(t, f, agentID)
					afterDelete = getAgent(t, f.s, agentID)
				}
			}
			f.srv.store = ws

			code, body := lifecycle(t, f, a.ID, "start")
			require.Equal(t, 200, code, body)
			if tc.delete != nil {
				require.True(t, ws.fired, "the delete ran between the start's release and the clear")
				require.NotNil(t, afterDelete)
			}

			got := getAgent(t, f.s, a.ID)
			if tc.cleared {
				assert.Equal(t, "running", got.ContainerStatus, "the superseded queued stop is cleared")
				assert.Empty(t, got.Message, "the queued-stop notice is cleared")
				return
			}
			assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "a delete-won row is not repainted running")
			assert.Equal(t, offlineStopMessage, got.Message)
			// The delete's row is untouched: the clear wrote nothing.
			assert.Equal(t, afterDelete.Phase, got.Phase)
			assert.Equal(t, afterDelete.Activity, got.Activity)
			assert.Equal(t, afterDelete.RunIntent, got.RunIntent)
			assert.Equal(t, afterDelete.DeletionState, got.DeletionState)
			assert.Equal(t, afterDelete.DeletionClaim, got.DeletionClaim)
			assert.Equal(t, afterDelete.DeletionLeaseAt, got.DeletionLeaseAt)
			assert.Equal(t, afterDelete.StateVersion, got.StateVersion)
			assert.True(t, afterDelete.Updated.Equal(got.Updated), "no status write landed after the delete")
		})
	}
}
