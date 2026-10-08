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
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getHealthSummary fetches the summary through the router and decodes it.
func getHealthSummary(t *testing.T, srv *Server) (HealthSummaryResponse, map[string]json.RawMessage) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	return resp, raw
}

// seedDispatchHealth writes one row on each side of every dispatch cutoff:
// a stuck and a fresh pending message, a stale and a fresh in_progress
// dispatch, and a recent and an old failed dispatch. Each section then
// counts exactly 1.
func seedDispatchHealth(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))
	for _, created := range []time.Time{now.Add(-2 * stuckMessageThreshold), now} {
		require.NoError(t, st.CreateMessage(ctx, &store.Message{
			ID: uuid.NewString(), ProjectID: proj.ID,
			Sender: "user:x", Recipient: "agent:a", Msg: "m",
			CreatedAt: created,
		}))
	}

	dbp, ok := st.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB() to backdate dispatch rows")
	insert := func(state string, updatedAt time.Time) {
		d := &store.BrokerDispatch{ID: uuid.NewString(), BrokerID: uuid.NewString(), Op: "start"}
		require.NoError(t, st.InsertBrokerDispatch(ctx, d))
		_, err := dbp.DB().ExecContext(ctx,
			`UPDATE broker_dispatch SET state = ?, updated_at = ? WHERE id = ?`,
			state, updatedAt, d.ID)
		require.NoError(t, err)
	}
	insert(store.DispatchStateInProgress, now.Add(-2*dispatchStuckAge))
	insert(store.DispatchStateInProgress, now)
	insert(store.DispatchStateFailed, now.Add(-healthDispatchFailedWindow/2))
	insert(store.DispatchStateFailed, now.Add(-2*healthDispatchFailedWindow))
	insert(store.DispatchStateDone, now.Add(-2*dispatchStuckAge))
}

// TestHandleHealthSummary_DispatchFromStore: the dispatch section reports
// the store's counts (design acceptance 6), matching direct store queries.
func TestHandleHealthSummary_DispatchFromStore(t *testing.T) {
	srv, st := testServer(t)
	seedDispatchHealth(t, st)

	resp, _ := getHealthSummary(t, srv)
	require.NotNil(t, resp.Dispatch)
	assert.Equal(t, HealthSummaryDispatch{
		StuckMessages:          1,
		StuckBrokerDispatch:    1,
		FailedBrokerDispatch1h: 1,
	}, *resp.Dispatch)

	// The same numbers as counting the tables directly.
	ctx := context.Background()
	now := time.Now().UTC()
	msgs, err := st.CountStuckPendingMessages(ctx, now.Add(-stuckMessageThreshold))
	require.NoError(t, err)
	stuck, failed, err := st.CountBrokerDispatchHealth(ctx, now.Add(-dispatchStuckAge), now.Add(-healthDispatchFailedWindow))
	require.NoError(t, err)
	assert.Equal(t, HealthSummaryDispatch{StuckMessages: msgs, StuckBrokerDispatch: stuck, FailedBrokerDispatch1h: failed}, *resp.Dispatch)
}

// TestHandleHealthSummary_DispatchSameOnTwoHubInstances: two hub instances
// on one store report identical dispatch numbers. The section is
// store-backed, with no per-replica (in-process) counters.
func TestHandleHealthSummary_DispatchSameOnTwoHubInstances(t *testing.T) {
	srvA, st := testServer(t)
	srvB, err := New(testServerConfig(), st)
	require.NoError(t, err)
	srvB.SetHubID("test-hub-id")
	// Registered after srvA's cleanup, so it runs first, before the shared
	// store is closed.
	t.Cleanup(func() { _ = srvB.Shutdown(context.Background()) })
	waitUserScopedDataSweep(t, srvB)
	require.NotEqual(t, srvA.InstanceID(), srvB.InstanceID())

	seedDispatchHealth(t, st)

	respA, _ := getHealthSummary(t, srvA)
	respB, _ := getHealthSummary(t, srvB)
	require.NotNil(t, respA.Dispatch)
	require.NotNil(t, respB.Dispatch)
	assert.Equal(t, *respA.Dispatch, *respB.Dispatch)
	assert.Equal(t, 1, respA.Dispatch.StuckBrokerDispatch)
}

// dispatchCountStore wraps a real store, records the cutoffs the summary
// passes to the dispatch counts, and can fail either count.
type dispatchCountStore struct {
	store.Store
	failMessages, failDispatch bool

	messagesBefore, stuckBefore, failedSince time.Time
}

func (d *dispatchCountStore) CountStuckPendingMessages(ctx context.Context, before time.Time) (int, error) {
	d.messagesBefore = before
	if d.failMessages {
		return 0, errors.New("count messages failed")
	}
	return d.Store.CountStuckPendingMessages(ctx, before)
}

func (d *dispatchCountStore) CountBrokerDispatchHealth(ctx context.Context, stuckBefore, failedSince time.Time) (int, int, error) {
	d.stuckBefore, d.failedSince = stuckBefore, failedSince
	if d.failDispatch {
		return 0, 0, errors.New("count dispatch failed")
	}
	return d.Store.CountBrokerDispatchHealth(ctx, stuckBefore, failedSince)
}

// TestHandleHealthSummary_DispatchThresholds: the summary reuses the sweep's
// and the reaper's stuck thresholds and a one-hour failure window.
func TestHandleHealthSummary_DispatchThresholds(t *testing.T) {
	srv, _ := testServer(t)
	wrapped := &dispatchCountStore{Store: srv.store}
	srv.store = wrapped

	before := time.Now().UTC()
	resp, _ := getHealthSummary(t, srv)
	after := time.Now().UTC()
	require.NotNil(t, resp.Dispatch)

	within := func(name string, got time.Time, age time.Duration) {
		t.Helper()
		assert.False(t, got.Before(before.Add(-age)) || got.After(after.Add(-age)),
			"%s cutoff %s should be now-%s", name, got, age)
	}
	within("stuck messages", wrapped.messagesBefore, stuckMessageThreshold)
	within("stuck broker dispatch", wrapped.stuckBefore, dispatchStuckAge)
	within("failed broker dispatch", wrapped.failedSince, time.Hour)
}

// TestHandleHealthSummary_DispatchNullOnStoreError: if either count fails,
// dispatch is null ("not reported", never zeros) and the status degrades.
func TestHandleHealthSummary_DispatchNullOnStoreError(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		failMessages, failDispatch bool
	}{
		{name: "messages count fails", failMessages: true},
		{name: "broker dispatch count fails", failDispatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			srv.store = &dispatchCountStore{Store: srv.store, failMessages: tc.failMessages, failDispatch: tc.failDispatch}

			resp, raw := getHealthSummary(t, srv)
			assert.Equal(t, "null", string(raw["dispatch"]))
			assert.Nil(t, resp.Dispatch)
			assert.Equal(t, HealthStatusDegraded, resp.Status)
			assert.NotNil(t, resp.Agents, "a dispatch failure must not drop other sections")
		})
	}
}
