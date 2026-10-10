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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The TestHubInstanceStore_ suite runs on SQLite by default and on Postgres
// in the T1 Postgres CI job (make test-launch-store-postgres runs the full
// entadapter suite), so both dialects are covered by the same tests.

func newHubInstanceTestStore(t *testing.T) (*HubInstanceStore, *ent.Client) {
	t.Helper()
	client := enttest.NewClient(t)
	return NewHubInstanceStore(client), client
}

// setHubInstanceTimes rewrites a row's timestamps directly, so a test can
// place a row in the past without sleeping.
func setHubInstanceTimes(t *testing.T, client *ent.Client, id string, lastSeen time.Time, stoppedAt *time.Time) {
	t.Helper()
	u := client.HubInstance.UpdateOneID(id).SetLastSeen(lastSeen)
	if stoppedAt != nil {
		u.SetStoppedAt(*stoppedAt)
	} else {
		u.ClearStoppedAt()
	}
	require.NoError(t, u.Exec(context.Background()))
}

// hubInstanceListAll is a window that lists every row a test writes.
const hubInstanceListAll = 10 * 365 * 24 * time.Hour

func getHubInstance(t *testing.T, s *HubInstanceStore, id string) store.HubInstance {
	t.Helper()
	rows, _, err := s.ListHubInstances(context.Background(), hubInstanceListAll)
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("hub instance %q not listed", id)
	return store.HubInstance{}
}

func TestHubInstanceStore_UpsertInsertsRow(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Minute)

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID:      "hub-a-1",
		Label:   "hub-a",
		Version: "v1.2.3",
		Status:  "degraded",
		Checks:  map[string]string{"database": "healthy", "colocated_broker": "unhealthy"},
	}))

	got := getHubInstance(t, s, "hub-a-1")
	assert.Equal(t, "hub-a", got.Label)
	assert.Equal(t, "v1.2.3", got.Version)
	assert.Equal(t, "degraded", got.Status)
	assert.Equal(t, map[string]string{"database": "healthy", "colocated_broker": "unhealthy"}, got.Checks)
	assert.Nil(t, got.StoppedAt)
	assert.Nil(t, got.Stats, "stats stays empty until a writer fills it")
	assert.True(t, got.StartedAt.Equal(got.LastSeen), "first insert sets started_at = last_seen = store clock")
	assert.True(t, got.LastSeen.After(before), "last_seen %v is the store clock", got.LastSeen)
	assert.Equal(t, time.UTC, got.LastSeen.Location())
}

func TestHubInstanceStore_UpsertKeepsStartedAtAndReplacesFields(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Label: "hub-a", Version: "v1", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
	}))
	first := getHubInstance(t, s, "hub-a-1")

	// Move the row into the past and mark it stopped, then upsert again.
	past := first.LastSeen.Add(-10 * time.Minute)
	setHubInstanceTimes(t, client, "hub-a-1", past, &past)

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Label: "hub-a2", Version: "v2", Status: "unhealthy",
		Checks: map[string]string{"database": "unhealthy"},
		Stats:  json.RawMessage(`{"db":{"max_open":4}}`),
	}))
	got := getHubInstance(t, s, "hub-a-1")
	assert.True(t, got.StartedAt.Equal(first.StartedAt), "started_at keeps its first-insert value: got %v want %v", got.StartedAt, first.StartedAt)
	assert.True(t, got.LastSeen.After(past), "last_seen is rewritten")
	assert.Nil(t, got.StoppedAt, "an upsert clears stopped_at")
	assert.Equal(t, "hub-a2", got.Label)
	assert.Equal(t, "v2", got.Version)
	assert.Equal(t, "unhealthy", got.Status)
	assert.Equal(t, map[string]string{"database": "unhealthy"}, got.Checks)
	assert.JSONEq(t, `{"db":{"max_open":4}}`, string(got.Stats))
}

func TestHubInstanceStore_TouchUpdatesLastSeenOnly(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Label: "hub-a", Version: "v1", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
	}))
	first := getHubInstance(t, s, "hub-a-1")
	past := first.LastSeen.Add(-time.Minute)
	setHubInstanceTimes(t, client, "hub-a-1", past, nil)

	found, err := s.TouchHubInstance(ctx, "hub-a-1")
	require.NoError(t, err)
	assert.True(t, found)

	got := getHubInstance(t, s, "hub-a-1")
	assert.True(t, got.LastSeen.After(past), "touch sets last_seen to the store clock")
	assert.True(t, got.StartedAt.Equal(first.StartedAt))
	assert.Equal(t, first.Label, got.Label)
	assert.Equal(t, first.Version, got.Version)
	assert.Equal(t, first.Status, got.Status)
	assert.Equal(t, first.Checks, got.Checks)
}

func TestHubInstanceStore_TouchMissingRowReturnsNotFound(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)

	found, err := s.TouchHubInstance(context.Background(), "hub-missing")
	require.NoError(t, err)
	assert.False(t, found, "touch on a missing row reports found=false so the caller upserts")
}

func TestHubInstanceStore_ListAppliesWindow(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"hub-live", "hub-old", "hub-stopped-recent", "hub-stopped-old"} {
		require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: id, Label: id, Status: "healthy"}))
	}
	_, now, err := s.ListHubInstances(ctx, hubInstanceListAll)
	require.NoError(t, err)

	recent := now.Add(-30 * time.Minute)
	old := now.Add(-2 * time.Hour)
	setHubInstanceTimes(t, client, "hub-old", old, nil)
	setHubInstanceTimes(t, client, "hub-stopped-recent", old, &recent)
	setHubInstanceTimes(t, client, "hub-stopped-old", old, &old)

	// The cut is taken from the store clock read inside the call, so the
	// caller passes only the window.
	rows, listNow, err := s.ListHubInstances(ctx, time.Hour)
	require.NoError(t, err)
	assert.False(t, listNow.IsZero())
	assert.WithinDuration(t, time.Now(), listNow, time.Minute, "store clock is close to the wall clock")

	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	// The cut is on stopped_at when set, otherwise last_seen; rows come
	// back in ID order.
	assert.Equal(t, []string{"hub-live", "hub-stopped-recent"}, ids)
}

func TestHubInstanceStore_RejectsInvalidID(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"", "has space", "tab\tid", strings.Repeat("a", hubInstanceMaxIDBytes+1)} {
		err := s.UpsertHubInstance(ctx, store.HubInstance{ID: id})
		assert.True(t, errors.Is(err, store.ErrInvalidInput), "upsert id %q: %v", id, err)
		_, err = s.TouchHubInstance(ctx, id)
		assert.True(t, errors.Is(err, store.ErrInvalidInput), "touch id %q: %v", id, err)
	}
	assert.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: strings.Repeat("a", hubInstanceMaxIDBytes)}))
}

func TestHubInstanceStore_MarkStoppedSetsStoppedAtAndLastSeen(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Label: "hub-a", Version: "v1", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
	}))
	first := getHubInstance(t, s, "hub-a-1")
	past := first.LastSeen.Add(-time.Minute)
	setHubInstanceTimes(t, client, "hub-a-1", past, nil)

	require.NoError(t, s.MarkHubInstanceStopped(ctx, "hub-a-1"))

	got := getHubInstance(t, s, "hub-a-1")
	require.NotNil(t, got.StoppedAt, "a clean stop sets stopped_at")
	assert.True(t, got.StoppedAt.Equal(got.LastSeen), "stopped_at = last_seen = one store clock reading: %v vs %v", got.StoppedAt, got.LastSeen)
	assert.True(t, got.LastSeen.After(past), "last_seen is the store clock at the stop")
	assert.Equal(t, time.UTC, got.StoppedAt.Location())
	assert.True(t, got.StartedAt.Equal(first.StartedAt))
	assert.Equal(t, first.Label, got.Label)
	assert.Equal(t, first.Status, got.Status)
	assert.Equal(t, first.Checks, got.Checks)

	// A later upsert (a restarted writer with the same ID) clears the stop.
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: "hub-a-1", Label: "hub-a", Status: "healthy"}))
	assert.Nil(t, getHubInstance(t, s, "hub-a-1").StoppedAt)
}

func TestHubInstanceStore_MarkStoppedMissingRowIsNoop(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.MarkHubInstanceStopped(ctx, "hub-missing"))
	rows, _, err := s.ListHubInstances(ctx, hubInstanceListAll)
	require.NoError(t, err)
	assert.Empty(t, rows, "marking a missing row stopped does not create it")
}

func TestHubInstanceStore_PruneDeletesOnlyRowsPastRetention(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	ids := []string{
		"hub-live",               // written just now
		"hub-seen-23h",           // last write inside retention
		"hub-seen-25h",           // last write outside retention
		"hub-stopped-23h",        // stopped inside retention, last_seen older
		"hub-stopped-25h",        // stopped outside retention
		"hub-stopped-25h-seen-1", // stopped_at governs, not last_seen
	}
	for _, id := range ids {
		require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: id, Label: id, Status: "healthy"}))
	}
	_, now, err := s.ListHubInstances(ctx, hubInstanceListAll)
	require.NoError(t, err)

	h23 := now.Add(-23 * time.Hour)
	h25 := now.Add(-25 * time.Hour)
	h30 := now.Add(-30 * time.Hour)
	setHubInstanceTimes(t, client, "hub-seen-23h", h23, nil)
	setHubInstanceTimes(t, client, "hub-seen-25h", h25, nil)
	setHubInstanceTimes(t, client, "hub-stopped-23h", h30, &h23)
	setHubInstanceTimes(t, client, "hub-stopped-25h", h25, &h25)
	// A stopped row's stop time decides, even with a recent last_seen.
	setHubInstanceTimes(t, client, "hub-stopped-25h-seen-1", now.Add(-time.Hour), &h25)

	n, err := s.PruneHubInstances(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	rows, _, err := s.ListHubInstances(ctx, hubInstanceListAll)
	require.NoError(t, err)
	var left []string
	for _, r := range rows {
		left = append(left, r.ID)
	}
	assert.Equal(t, []string{"hub-live", "hub-seen-23h", "hub-stopped-23h"}, left)

	// Nothing left to prune.
	n, err = s.PruneHubInstances(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestHubInstanceStore_PruneRejectsNonPositiveRetention(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: "hub-a-1", Status: "healthy"}))

	for _, d := range []time.Duration{0, -time.Hour} {
		_, err := s.PruneHubInstances(ctx, d)
		assert.True(t, errors.Is(err, store.ErrInvalidInput), "retention %v: %v", d, err)
	}
	getHubInstance(t, s, "hub-a-1") // still there
}

func TestHubInstanceStore_MarkStoppedRejectsInvalidID(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	for _, id := range []string{"", "has space", strings.Repeat("a", hubInstanceMaxIDBytes+1)} {
		err := s.MarkHubInstanceStopped(context.Background(), id)
		assert.True(t, errors.Is(err, store.ErrInvalidInput), "id %q: %v", id, err)
	}
}

// TestHubInstanceStore_PrunedLiveRowIsRecreated covers the writer's repair
// path at the store level: after a prune removes a row, Touch reports
// found=false and the following Upsert re-creates it with a new
// started_at.
func TestHubInstanceStore_PrunedLiveRowIsRecreated(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: "hub-a-1", Label: "hub-a", Status: "healthy"}))
	first := getHubInstance(t, s, "hub-a-1")
	old := first.LastSeen.Add(-25 * time.Hour)
	setHubInstanceTimes(t, client, "hub-a-1", old, nil)
	_, err := client.HubInstance.UpdateOneID("hub-a-1").SetStartedAt(old).Save(ctx)
	require.NoError(t, err)

	n, err := s.PruneHubInstances(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	found, err := s.TouchHubInstance(ctx, "hub-a-1")
	require.NoError(t, err)
	assert.False(t, found)
	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: "hub-a-1", Label: "hub-a", Status: "healthy"}))
	got := getHubInstance(t, s, "hub-a-1")
	assert.True(t, got.StartedAt.After(old), "the re-created row has a new started_at")
	assert.Nil(t, got.StoppedAt)
}
