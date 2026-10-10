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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

func TestHubInstanceStore_TouchUpdatesLastSeenAndPoolGaugesOnly(t *testing.T) {
	s, client := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Label: "hub-a", Version: "v1", Status: "healthy",
		Checks: map[string]string{"database": "healthy"},
		Stats: json.RawMessage(`{"db":{"in_use":1,"idle":2,"max_open":10,"wait_count":3},` +
			`"integrations":[{"name":"chat","health":"healthy","connected":true,"version":"1.0"}]}`),
	}))
	first := getHubInstance(t, s, "hub-a-1")
	past := first.LastSeen.Add(-time.Minute)
	setHubInstanceTimes(t, client, "hub-a-1", past, nil)

	found, err := s.TouchHubInstance(ctx, "hub-a-1", &api.HubInstanceDBStats{InUse: 7, Idle: 1, MaxOpen: 10, WaitCount: 9})
	require.NoError(t, err)
	assert.True(t, found)

	got := getHubInstance(t, s, "hub-a-1")
	assert.True(t, got.LastSeen.After(past), "touch sets last_seen to the store clock")
	assert.True(t, got.StartedAt.Equal(first.StartedAt))
	assert.Equal(t, first.Label, got.Label)
	assert.Equal(t, first.Version, got.Version)
	assert.Equal(t, first.Status, got.Status)
	assert.Equal(t, first.Checks, got.Checks)
	assert.JSONEq(t, `{"db":{"in_use":7,"idle":1,"max_open":10,"wait_count":9},`+
		`"integrations":[{"name":"chat","health":"healthy","connected":true,"version":"1.0"}]}`,
		string(got.Stats), "touch replaces stats.db and keeps every other stats key")
}

func TestHubInstanceStore_TouchOnEmptyStatsWritesDB(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: "hub-a-1", Status: "healthy"}))
	found, err := s.TouchHubInstance(ctx, "hub-a-1", &api.HubInstanceDBStats{InUse: 2, MaxOpen: 5})
	require.NoError(t, err)
	assert.True(t, found)
	assert.JSONEq(t, `{"db":{"in_use":2,"idle":0,"max_open":5,"wait_count":0}}`, string(getHubInstance(t, s, "hub-a-1").Stats))
}

func TestHubInstanceStore_TouchWithNilDBRemovesDBOnly(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{
		ID: "hub-a-1", Status: "healthy",
		Stats: json.RawMessage(`{"db":{"in_use":1,"idle":0,"max_open":4,"wait_count":0},"integrations_truncated":true}`),
	}))
	found, err := s.TouchHubInstance(ctx, "hub-a-1", nil)
	require.NoError(t, err)
	assert.True(t, found)
	assert.JSONEq(t, `{"integrations_truncated":true}`, string(getHubInstance(t, s, "hub-a-1").Stats))
}

func TestHubInstanceStore_TouchMissingRowReturnsNotFound(t *testing.T) {
	s, _ := newHubInstanceTestStore(t)

	found, err := s.TouchHubInstance(context.Background(), "hub-missing", &api.HubInstanceDBStats{InUse: 1})
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
		_, err = s.TouchHubInstance(ctx, id, nil)
		assert.True(t, errors.Is(err, store.ErrInvalidInput), "touch id %q: %v", id, err)
	}
	assert.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: strings.Repeat("a", hubInstanceMaxIDBytes)}))
}
