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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listCountingStore counts ListHubSettings calls and passes the optional
// revision-returning delete through.
type listCountingStore struct {
	store.HubSettingStore
	lists atomic.Int64
}

func (c *listCountingStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	c.lists.Add(1)
	return c.HubSettingStore.ListHubSettings(ctx)
}

func (c *listCountingStore) DeleteHubSettingRevision(ctx context.Context, section string) (int64, error) {
	rd, ok := c.HubSettingStore.(store.HubSettingRevisionDeleter)
	if !ok {
		return 0, fmt.Errorf("wrapped store has no DeleteHubSettingRevision")
	}
	return rd.DeleteHubSettingRevision(ctx, section)
}

// cacheOrderOps is an OperationalSettings over a SQLite store, wrapped so a
// test can run work between a write and the writer's cache step and count
// the re-reads. It does not self-apply.
func cacheOrderOps(t *testing.T) (store.Store, *OperationalSettings, *replayListStore, *listCountingStore) {
	t.Helper()
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	counter := &listCountingStore{HubSettingStore: st}
	rs := &replayListStore{HubSettingStore: counter}
	return st, NewOperationalSettings(rs, emptyKoanf(), emptyKoanf()), rs, counter
}

func cachedSection(ops *OperationalSettings, section string) (sectionState, bool) {
	ops.mu.RLock()
	defer ops.mu.RUnlock()
	entry, ok := ops.cache[section]
	return entry, ok
}

// A write below the cached high-water mark whose section the cache knows
// only at an older revision is cached directly, without re-reading the
// store (ptone/scion#5762).
func TestOperationalSettings_UpdateBelowHighWaterCachedDirectly(t *testing.T) {
	st, ops, rs, counter := cacheOrderOps(t)
	ctx := context.Background()

	rs.afterUpsert = func() {
		_, err := ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "b@example.com", -1, "managed")
		require.NoError(t, err)
	}
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)

	assert.Equal(t, int64(0), counter.lists.Load(), "no re-read of the store")
	lifecycle, ok := cachedSection(ops, "lifecycle")
	require.True(t, ok, "the older write is cached directly")
	assert.Equal(t, int64(1), lifecycle.settingsRev)
	assert.Equal(t, int64(2), ops.SettingsRevision(), "the high-water mark is the newer write's")
	snap := ops.Snapshot()
	assert.True(t, snap.AutoSuspendStalled)
	require.NotNil(t, snap.EnforceBrokerQuotas)
	assert.False(t, *snap.EnforceBrokerQuotas)

	row, err := st.GetHubSetting(ctx, "lifecycle")
	require.NoError(t, err)
	assert.Equal(t, row.Revision, lifecycle.Revision)
}

// The interleaving from ptone/scion#5762: this replica writes a section
// whose row has a high per-section revision; another replica then deletes
// the row and creates it again (per-section revision 1, higher settings
// revision), and a Refresh here caches the new row. Comparing per-section
// revisions would let the older write (6 > 1) overwrite the newer row; the
// per-entry settings revision keeps the newer row.
func TestOperationalSettings_DeleteRecreateKeepsNewerRow(t *testing.T) {
	st, ops, rs, _ := cacheOrderOps(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := ops.Update(ctx, "lifecycle", json.RawMessage(fmt.Sprintf(`{"auto_suspend_stalled":%t}`, i%2 == 0)), "a@example.com", -1, "managed")
		require.NoError(t, err)
	}
	rs.afterUpsert = func() {
		require.NoError(t, st.DeleteHubSetting(ctx, "lifecycle"))
		_, err := st.UpsertHubSetting(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":false}`), "other@example.com", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
	}
	newRev, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)
	require.Equal(t, int64(6), newRev, "the writer's row was at per-section revision 6")

	entry, ok := cachedSection(ops, "lifecycle")
	require.True(t, ok)
	assert.Equal(t, int64(1), entry.Revision, "the re-created row stays cached")
	assert.JSONEq(t, `{"auto_suspend_stalled":false}`, string(entry.Value))
	assert.False(t, ops.Snapshot().AutoSuspendStalled)
	stored, err := st.GetHubSetting(ctx, "lifecycle")
	require.NoError(t, err)
	assert.Equal(t, stored.Revision, entry.Revision)
}

// A section this replica never cached, deleted by another replica after
// this replica's write and seen absent by a Refresh: the write is not
// cached, so the deleted section is not resurrected (ptone/scion#5762).
func TestOperationalSettings_LaterRemoteDeleteOfNeverCachedSection(t *testing.T) {
	st, ops, rs, _ := cacheOrderOps(t)
	ctx := context.Background()

	rs.afterUpsert = func() {
		require.NoError(t, st.DeleteHubSetting(ctx, "lifecycle"))
		_, err := ops.Refresh(ctx)
		require.NoError(t, err)
	}
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)

	_, ok := cachedSection(ops, "lifecycle")
	assert.False(t, ok, "the later delete wins")
	assert.False(t, ops.Snapshot().AutoSuspendStalled)
}

// A later local delete leaves a tombstone, so the older write's cache step
// does not resurrect the section; a section that was never cached and
// never deleted is cached directly (ptone/scion#5762).
func TestOperationalSettings_LocalDeleteTombstoneVersusNeverCached(t *testing.T) {
	_, ops, rs, counter := cacheOrderOps(t)
	ctx := context.Background()

	rs.afterUpsert = func() {
		require.NoError(t, ops.DeleteSection(ctx, "lifecycle"))
	}
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)
	_, ok := cachedSection(ops, "lifecycle")
	assert.False(t, ok, "the later local delete wins")
	assert.False(t, ops.Snapshot().AutoSuspendStalled)

	// A never-cached section with no tombstone is written directly.
	before := counter.lists.Load()
	_, err = ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "a@example.com", -1, "managed")
	require.NoError(t, err)
	_, ok = cachedSection(ops, "quotas")
	assert.True(t, ok)
	assert.Equal(t, before, counter.lists.Load(), "no re-read for a never-cached section")

	// An accepted Refresh clears the tombstones.
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ops.mu.RLock()
	tombstones := len(ops.tombstones)
	ops.mu.RUnlock()
	assert.Zero(t, tombstones)
}

// Snapshots at an equal revision are ordered by the cache sequence; a
// snapshot with Seq 0 (not built by OperationalSettings.Snapshot) keeps
// the revision-only order (ptone/scion#5762).
func TestApplySnapshot_SeqOrdersEqualRevisions(t *testing.T) {
	srv, _, _ := revisionTestServer(t)
	db := func(rev int64, seq uint64, auto bool) Layer1Snapshot {
		return Layer1Snapshot{FromOperationalSettings: true, Revision: rev, Seq: seq, AutoSuspendStalled: auto}
	}

	ApplySnapshot(srv, db(5, 3, false))
	res := ApplySnapshot(srv, db(5, 2, true))
	assert.Equal(t, true, res["skipped_stale"], "a lower sequence at the same revision is older")
	assert.False(t, appliedAutoSuspend(srv))

	res = ApplySnapshot(srv, db(5, 3, true))
	assert.Nil(t, res["skipped_stale"], "an equal revision and sequence applies again")
	assert.True(t, appliedAutoSuspend(srv))

	res = ApplySnapshot(srv, db(5, 0, false))
	assert.Nil(t, res["skipped_stale"], "Seq 0 is ordered by revision alone")
	assert.False(t, appliedAutoSuspend(srv))

	res = ApplySnapshot(srv, db(6, 1, true))
	assert.Nil(t, res["skipped_stale"], "a higher revision wins whatever its sequence")
	assert.True(t, appliedAutoSuspend(srv))
	res = ApplySnapshot(srv, db(5, 9, false))
	assert.Equal(t, true, res["skipped_stale"])
	assert.True(t, appliedAutoSuspend(srv))
}

// A snapshot built before a write that was cached below the high-water
// mark (so the revision did not move) is not applied over the writer's
// self-apply (ptone/scion#5762).
func TestOperationalSettings_DirectWriteBelowHighWaterNotUndoneByOlderSnapshot(t *testing.T) {
	srv, _, ops, rs := raceOpsServer(t)
	ctx := context.Background()

	var older Layer1Snapshot
	rs.afterUpsert = func() {
		_, err := ops.Update(ctx, "quotas", json.RawMessage(`{"enforce_broker_quotas":false}`), "b@example.com", -1, "managed")
		require.NoError(t, err)
		older = ops.Snapshot()
	}
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "a@example.com", -1, "managed")
	require.NoError(t, err)
	require.True(t, appliedAutoSuspend(srv), "the self-apply applied the write")
	require.False(t, older.AutoSuspendStalled, "the older snapshot lacks the write")
	require.Equal(t, older.Revision, ops.Snapshot().Revision, "the direct write left the revision unchanged")

	res := ApplySnapshot(srv, older)
	assert.Equal(t, true, res["skipped_stale"])
	assert.True(t, appliedAutoSuspend(srv), "the write stays applied")
}
