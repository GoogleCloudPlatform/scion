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
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revisionTestServer is a SQLite-backed server whose OperationalSettings
// does not self-apply, so the test decides which snapshot is applied when.
func revisionTestServer(t *testing.T) (*Server, store.Store, *OperationalSettings) {
	t.Helper()
	srv, st, ops := newSQLiteOpsServer(t, nil, nil)
	ops.server = nil
	return srv, st, ops
}

func writeLifecycleAndMaintenance(t *testing.T, ops *OperationalSettings, autoSuspend bool, msg string) Layer1Snapshot {
	t.Helper()
	ctx := context.Background()
	_, err := ops.Update(ctx, "lifecycle",
		json.RawMessage(fmt.Sprintf(`{"auto_suspend_stalled":%t}`, autoSuspend)), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "maintenance",
		json.RawMessage(fmt.Sprintf(`{"admin_mode":true,"maintenance_message":%q}`, msg)), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	return ops.Snapshot()
}

func appliedAutoSuspend(srv *Server) bool {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.config.AutoSuspendStalled
}

// Two snapshots applied out of order: the newer values remain, for the
// config and the maintenance state (ptone/scion#4457).
func TestApplySnapshot_OutOfOrderKeepsNewerValues(t *testing.T) {
	srv, _, ops := revisionTestServer(t)

	older := writeLifecycleAndMaintenance(t, ops, true, "older")
	newer := writeLifecycleAndMaintenance(t, ops, false, "newer")
	require.Greater(t, older.Revision, int64(0))
	require.Greater(t, newer.Revision, older.Revision)

	ApplySnapshot(srv, newer)
	ApplyMaintenanceFromSnapshot(srv, newer)
	res := ApplySnapshot(srv, older)
	ApplyMaintenanceFromSnapshot(srv, older)

	assert.Equal(t, true, res["skipped_stale"], "the older snapshot is reported as skipped")
	assert.False(t, appliedAutoSuspend(srv), "the newer auto_suspend_stalled stays applied")
	_, msg := srv.maintenance.State()
	assert.Equal(t, "newer", msg, "the newer maintenance message stays applied")

	// In order, both apply.
	srv2, _, ops2 := revisionTestServer(t)
	a := writeLifecycleAndMaintenance(t, ops2, true, "first")
	b := writeLifecycleAndMaintenance(t, ops2, false, "second")
	ApplySnapshot(srv2, a)
	assert.True(t, appliedAutoSuspend(srv2))
	ApplySnapshot(srv2, b)
	ApplyMaintenanceFromSnapshot(srv2, b)
	assert.False(t, appliedAutoSuspend(srv2))
	_, msg = srv2.maintenance.State()
	assert.Equal(t, "second", msg)
}

// Concurrent applies of snapshots in random order leave the values of the
// newest one (ptone/scion#4457).
func TestApplySnapshot_ConcurrentAppliesKeepNewest(t *testing.T) {
	srv, _, _ := revisionTestServer(t)
	const n = 20
	snaps := make([]Layer1Snapshot, n)
	for i := range snaps {
		snaps[i] = Layer1Snapshot{
			FromOperationalSettings: true,
			Revision:                int64(i + 1),
			AutoSuspendStalled:      i%2 == 0,
			HasMaintenanceRow:       true,
			AdminMode:               true,
			MaintenanceMessage:      fmt.Sprintf("m-%d", i+1),
		}
	}
	order := rand.Perm(n)
	var wg sync.WaitGroup
	for _, i := range order {
		wg.Add(1)
		go func(s Layer1Snapshot) {
			defer wg.Done()
			ApplySnapshot(srv, s)
			ApplyMaintenanceFromSnapshot(srv, s)
		}(snaps[i])
	}
	wg.Wait()

	newest := snaps[n-1]
	assert.Equal(t, newest.AutoSuspendStalled, appliedAutoSuspend(srv))
	_, msg := srv.maintenance.State()
	assert.Equal(t, newest.MaintenanceMessage, msg)
}

// An equal revision applies again (a write's self-apply and its own event).
// A DB-backed snapshot with revision 0 (built before the store had a
// revision) is older than any revisioned one and is skipped. A file
// snapshot carries no revision and always applies (ptone/scion#4457).
func TestApplySnapshot_EqualZeroAndFileRevisions(t *testing.T) {
	srv, _, _ := revisionTestServer(t)

	db := func(rev int64, auto bool) Layer1Snapshot {
		return Layer1Snapshot{FromOperationalSettings: true, Revision: rev, AutoSuspendStalled: auto}
	}

	// Before any revisioned apply, revision 0 applies (a database with no
	// operational write yet).
	ApplySnapshot(srv, db(0, true))
	assert.True(t, appliedAutoSuspend(srv))

	ApplySnapshot(srv, db(5, false))
	assert.False(t, appliedAutoSuspend(srv))

	res := ApplySnapshot(srv, db(5, true))
	assert.Nil(t, res["skipped_stale"])
	assert.True(t, appliedAutoSuspend(srv), "an equal revision applies again")

	res = ApplySnapshot(srv, db(0, false))
	assert.Equal(t, true, res["skipped_stale"])
	assert.True(t, appliedAutoSuspend(srv), "a revision-0 DB snapshot does not overwrite revision 5")

	res = ApplySnapshot(srv, db(4, false))
	assert.Equal(t, true, res["skipped_stale"])
	assert.True(t, appliedAutoSuspend(srv))

	// A file snapshot is not ordered and does not move the revision.
	res = ApplySnapshot(srv, Layer1Snapshot{AutoSuspendStalled: false})
	assert.Nil(t, res["skipped_stale"])
	assert.False(t, appliedAutoSuspend(srv))
	srv.settingsApply.mu.Lock()
	last := srv.settingsApply.lastRevision
	srv.settingsApply.mu.Unlock()
	assert.Equal(t, int64(5), last)

	// Maintenance follows the same rule.
	ApplyMaintenanceFromSnapshot(srv, Layer1Snapshot{FromOperationalSettings: true, Revision: 6, HasMaintenanceRow: true, AdminMode: true, MaintenanceMessage: "six"})
	ApplyMaintenanceFromSnapshot(srv, Layer1Snapshot{FromOperationalSettings: true, Revision: 0, HasMaintenanceRow: true, MaintenanceMessage: "zero"})
	ApplyMaintenanceFromSnapshot(srv, Layer1Snapshot{FromOperationalSettings: true, Revision: 5, HasMaintenanceRow: true, MaintenanceMessage: "five"})
	on, msg := srv.maintenance.State()
	assert.True(t, on)
	assert.Equal(t, "six", msg)
}

// The snapshot carries the store revision of its values, and
// SettingsRevision reports the same value (ptone/scion#4457).
func TestOperationalSettings_SnapshotCarriesRevision(t *testing.T) {
	_, st, ops := revisionTestServer(t)
	ctx := context.Background()

	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)

	row, err := st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	snap := ops.Snapshot()
	assert.Equal(t, row.Revision, snap.Revision)
	assert.Equal(t, row.Revision, ops.SettingsRevision())

	require.NoError(t, ops.DeleteSection(ctx, "quotas"))
	row, err = st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, row.Revision, ops.Snapshot().Revision, "a delete moves the revision too")

	// A fresh service reads the same revision with the values.
	fresh := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	_, err = fresh.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, row.Revision, fresh.Snapshot().Revision)
	assert.True(t, fresh.Snapshot().AutoSuspendStalled)
}

// replayListStore returns a recorded ListHubSettings result while replay is
// set, standing in for a read that was taken before later writes.
type replayListStore struct {
	store.HubSettingStore
	mu     sync.Mutex
	replay []store.HubSetting
	// afterUpsert and afterDelete, when set, run once after a successful
	// upsert or delete and before it returns, standing in for work that
	// lands between a write and the writer's cache update.
	afterUpsert func()
	afterDelete func()
	// duringList, when set, runs once inside ListHubSettings, after the
	// read and before it returns, standing in for work that lands while a
	// read is in flight.
	duringList func()
}

// DeleteHubSettingRevision passes the optional revision-returning delete
// through to the wrapped store (an embedded interface does not promote it).
func (r *replayListStore) DeleteHubSettingRevision(ctx context.Context, section string) (int64, error) {
	rd, ok := r.HubSettingStore.(store.HubSettingRevisionDeleter)
	if !ok {
		return 0, fmt.Errorf("wrapped store has no DeleteHubSettingRevision")
	}
	rev, err := rd.DeleteHubSettingRevision(ctx, section)
	if err == nil && r.afterDelete != nil {
		f := r.afterDelete
		r.afterDelete = nil
		f()
	}
	return rev, err
}

func (r *replayListStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	r.mu.Lock()
	replay := r.replay
	during := r.duringList
	r.duringList = nil
	r.mu.Unlock()
	rows, err := replay, error(nil)
	if replay == nil {
		rows, err = r.HubSettingStore.ListHubSettings(ctx)
	}
	if during != nil {
		during()
	}
	return rows, err
}

func (r *replayListStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage,
	updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	got, err := r.HubSettingStore.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
	if err == nil && r.afterUpsert != nil {
		f := r.afterUpsert
		r.afterUpsert = nil
		f()
	}
	return got, err
}

// A Refresh whose read is older than the cached values is discarded: the
// cache keeps the newer values and revision, and nothing is reported
// changed (ptone/scion#4457).
//
// The newer write lands while the read is in flight (duringList), which is
// how a stale read arises. A stale read with no change of the cache while
// it was in flight is instead the signature of a store reset and is
// resynchronised to (ptone/scion#5667,
// TestOperationalSettings_RefreshResyncsAfterStoreReset).
func TestOperationalSettings_RefreshDiscardsStaleRead(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	rs := &replayListStore{HubSettingStore: st}
	ops := NewOperationalSettings(rs, emptyKoanf(), emptyKoanf())

	_, err = ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	stale, err := st.ListHubSettings(ctx)
	require.NoError(t, err)

	rs.mu.Lock()
	rs.replay = stale
	rs.duringList = func() {
		_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":false}`), "admin@example.com", -1, "managed")
		require.NoError(t, err)
	}
	rs.mu.Unlock()
	changed, err := ops.Refresh(ctx)
	require.NoError(t, err)
	newRev := ops.SettingsRevision()
	row, err := st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	require.Equal(t, row.Revision, newRev, "the write during the read raised the cached revision")
	assert.Empty(t, changed)
	assert.Equal(t, newRev, ops.SettingsRevision())
	assert.False(t, ops.Snapshot().AutoSuspendStalled, "the stale read did not replace the newer value")

	// A current read is taken as usual.
	rs.mu.Lock()
	rs.replay = nil
	rs.mu.Unlock()
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, newRev, ops.SettingsRevision())
	assert.False(t, ops.Snapshot().AutoSuspendStalled)
}

// When a Refresh after this write already cached a newer revision, Update
// does not overwrite the cache with its own, older row; it re-reads, and
// the newer value stays (ptone/scion#4457).
func TestOperationalSettings_UpdateBelowCachedRevisionKeepsNewerValue(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	rs := &replayListStore{HubSettingStore: st}
	ops := NewOperationalSettings(rs, emptyKoanf(), emptyKoanf())

	// Between this write and its cache update, another replica writes the
	// same section and this replica's poll refreshes.
	rs.afterUpsert = func() {
		_, err := st.UpsertHubSetting(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":false}`), "other@example.com", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
	}
	_, err = ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)

	row, err := st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, int64(2), row.Revision)
	assert.Equal(t, int64(2), ops.SettingsRevision())
	assert.False(t, ops.Snapshot().AutoSuspendStalled, "the newer value from the refresh stays cached")
	stored, err := st.GetHubSetting(ctx, "lifecycle")
	require.NoError(t, err)
	ops.mu.RLock()
	cached := ops.cache["lifecycle"]
	ops.mu.RUnlock()
	assert.Equal(t, stored.Revision, cached.Revision)
}

// The revision row is not a settings section: a full settings GET and a
// snapshot never show it (ptone/scion#4457).
func TestServerConfigDB_RevisionRowNotShown(t *testing.T) {
	srv, st, ops := newSQLiteOpsServer(t, nil, nil)
	ctx := context.Background()
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err, "the revision row exists")
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), store.HubSettingsRevisionSection)

	ops.mu.RLock()
	_, cached := ops.cache[store.HubSettingsRevisionSection]
	ops.mu.RUnlock()
	assert.False(t, cached, "the revision row is not cached as a section")

	snapJSON, err := json.Marshal(ops.Snapshot())
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(snapJSON), store.HubSettingsRevisionSection))
}

func appliedEnforceBrokerQuotas(srv *Server) *bool {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.config.EnforceBrokerQuotas
}

// raceOpsServer is a SQLite-backed server that self-applies, with its
// OperationalSettings store wrapped so a test can run work between a write
// and the writer's cache update.
func raceOpsServer(t *testing.T) (*Server, store.Store, *OperationalSettings, *replayListStore) {
	t.Helper()
	srv, st, ops := newSQLiteOpsServer(t, nil, nil)
	rs := &replayListStore{HubSettingStore: ops.store}
	ops.store = rs
	return srv, st, ops, rs
}

// Two local Updates of different sections, the second one's cache step
// landing before the first one's: both values end up in the cache, the
// snapshot and the applied config (ptone/scion#4457).
func TestOperationalSettings_ConcurrentUpdatesOfTwoSectionsBothApplied(t *testing.T) {
	srv, st, ops, rs := raceOpsServer(t)
	ctx := context.Background()

	rs.afterUpsert = func() {
		_, err := ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "b@example.com", -1, "managed")
		require.NoError(t, err)
	}
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)

	snap := ops.Snapshot()
	assert.True(t, snap.AutoSuspendStalled, "the first write is in the snapshot")
	require.NotNil(t, snap.EnforceBrokerQuotas)
	assert.False(t, *snap.EnforceBrokerQuotas, "the second write is in the snapshot")
	assert.True(t, appliedAutoSuspend(srv), "the first write is applied")
	q := appliedEnforceBrokerQuotas(srv)
	require.NotNil(t, q)
	assert.False(t, *q, "the second write is applied")

	row, err := st.GetHubSetting(ctx, "lifecycle")
	require.NoError(t, err)
	ops.mu.RLock()
	cached := ops.cache["lifecycle"]
	ops.mu.RUnlock()
	assert.Equal(t, row.Revision, cached.Revision, "the cache holds the stored lifecycle row")
	rev, err := st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, rev.Revision, ops.SettingsRevision())
}

// A Delete whose cache step lands after a concurrent local Update of
// another section: the section is gone from the cache, the snapshot and
// the applied config, and the Update is kept (ptone/scion#4457).
func TestOperationalSettings_DeleteRacingUpdateBothApplied(t *testing.T) {
	srv, st, ops, rs := raceOpsServer(t)
	ctx := context.Background()

	_, err := ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "a@example.com", -1, "managed")
	require.NoError(t, err)
	q := appliedEnforceBrokerQuotas(srv)
	require.NotNil(t, q)
	require.False(t, *q)

	rs.afterDelete = func() {
		_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "b@example.com", -1, "managed")
		require.NoError(t, err)
	}
	require.NoError(t, ops.DeleteSection(ctx, "quotas"))

	ops.mu.RLock()
	_, cached := ops.cache["quotas"]
	ops.mu.RUnlock()
	assert.False(t, cached, "the deleted section is not cached")
	snap := ops.Snapshot()
	assert.Nil(t, snap.EnforceBrokerQuotas, "the delete is in the snapshot")
	assert.True(t, snap.AutoSuspendStalled, "the concurrent update is in the snapshot")
	assert.Nil(t, appliedEnforceBrokerQuotas(srv), "the delete is applied")
	assert.True(t, appliedAutoSuspend(srv), "the concurrent update is applied")
	rev, err := st.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, rev.Revision, ops.SettingsRevision())
}

// countingListStore counts ListHubSettings calls on a store without the
// optional revision-returning delete.
type countingListStore struct {
	*fakeHubSettingStore
	mu    sync.Mutex
	lists int
}

func (c *countingListStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.fakeHubSettingStore.ListHubSettings(ctx)
}

func (c *countingListStore) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}

// A store without the optional revision-returning delete gives revision 0:
// DeleteSection still removes the section from the cache, even when a
// revision was cached before, and re-reads the store before the
// self-apply (ptone/scion#4457).
func TestOperationalSettings_DeleteWithoutRevisionTakesSafePath(t *testing.T) {
	fake := newFakeHubSettingStore()
	cs := &countingListStore{fakeHubSettingStore: fake}
	var _ store.HubSettingStore = cs
	_, isDeleter := interface{}(cs).(store.HubSettingRevisionDeleter)
	require.False(t, isDeleter, "the fake must not have the optional method")

	ops := NewOperationalSettings(cs, emptyKoanf(), emptyKoanf())
	fake.seedWithOrigin("quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "managed")
	fake.seedWithOrigin("lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	before := cs.listCount()
	require.NoError(t, ops.DeleteSection(context.Background(), "quotas"))
	assert.Greater(t, cs.listCount(), before, "the delete re-read the store")
	ops.mu.RLock()
	_, cached := ops.cache["quotas"]
	ops.mu.RUnlock()
	assert.False(t, cached)
	assert.Nil(t, ops.Snapshot().EnforceBrokerQuotas)
	assert.True(t, ops.Snapshot().AutoSuspendStalled)

	// With a revision cached earlier, a revision-0 delete still removes the
	// section from the cache.
	ops.mu.Lock()
	ops.revision = 5
	ops.mu.Unlock()
	require.NoError(t, ops.DeleteSection(context.Background(), "lifecycle"))
	ops.mu.RLock()
	_, cached = ops.cache["lifecycle"]
	ops.mu.RUnlock()
	assert.False(t, cached, "a revision-0 delete never skips its cache delete")
	assert.False(t, ops.Snapshot().AutoSuspendStalled)
}
