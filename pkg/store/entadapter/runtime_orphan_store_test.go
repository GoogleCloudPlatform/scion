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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeOrphans (ptone/scion#7930): an orphan is created once and then
// upserted on its identity, keeping first_seen_at; orphans and rows on
// targets outside the complete inventory are left alone; a row missing from
// removeAfter complete inventories in a row is removed; list filters and
// counts group by broker, kind and reason.
func TestRuntimeOrphans(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	o := func(target, slug, reason string) store.RuntimeOrphan {
		return store.RuntimeOrphan{Kind: store.RuntimeOrphanKindInstance, RuntimeTarget: target,
			ProjectID: "p1", Slug: slug, AgentID: "a-" + slug, RunID: "r1", Reason: reason, ObservedPhase: "running"}
	}
	record := func(broker string, targets []string, at time.Time, os ...store.RuntimeOrphan) []store.RuntimeOrphan {
		t.Helper()
		created, err := s.RecordRuntimeOrphans(ctx, broker, targets, os, at, 3)
		require.NoError(t, err)
		return created
	}
	list := func(f store.RuntimeOrphanFilter) []store.RuntimeOrphan {
		t.Helper()
		got, err := s.ListRuntimeOrphans(ctx, f)
		require.NoError(t, err)
		return got
	}

	created := record("b1", []string{"cr"}, t0, o("cr", "gone", "no_agent"), o("other", "skipped", "no_agent"))
	require.Len(t, created, 1)
	assert.Equal(t, "gone", created[0].Slug)
	assert.Equal(t, "none", created[0].ActionState)
	record("b2", []string{"docker"}, t0, o("docker", "x", "unknown_project"))

	// Seen again: updated in place, not created; first_seen_at kept.
	t1 := t0.Add(time.Minute)
	upd := o("cr", "gone", "superseded")
	upd.RunID = "r2"
	assert.Empty(t, record("b1", []string{"cr"}, t1, upd))
	got := list(store.RuntimeOrphanFilter{BrokerID: "b1"})
	require.Len(t, got, 1)
	assert.Equal(t, t0, got[0].FirstSeenAt.UTC())
	assert.Equal(t, t1, got[0].LastSeenAt.UTC())
	assert.Equal(t, "superseded", got[0].Reason)
	assert.Equal(t, "r2", got[0].RunID)

	// Filters and counts.
	assert.Len(t, list(store.RuntimeOrphanFilter{}), 2)
	assert.Len(t, list(store.RuntimeOrphanFilter{ProjectID: "p1", Kind: store.RuntimeOrphanKindInstance}), 2)
	assert.Empty(t, list(store.RuntimeOrphanFilter{Kind: store.RuntimeOrphanKindNFSTree}))
	counts, err := s.CountRuntimeOrphans(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []store.RuntimeOrphanCount{
		{BrokerID: "b1", Kind: "instance", Reason: "superseded", Count: 1},
		{BrokerID: "b2", Kind: "instance", Reason: "unknown_project", Count: 1},
	}, counts)

	// An inventory that does not cover the row's target leaves it alone;
	// three complete inventories without it remove it, and a sighting in
	// between resets the count.
	record("b1", []string{"elsewhere"}, t1)
	record("b1", []string{"cr"}, t1)
	record("b1", []string{"cr"}, t1)
	assert.Equal(t, 2, list(store.RuntimeOrphanFilter{BrokerID: "b1"})[0].MissedInventories)
	record("b1", []string{"cr"}, t1, upd)
	for range 2 {
		record("b1", []string{"cr"}, t1)
	}
	assert.Len(t, list(store.RuntimeOrphanFilter{BrokerID: "b1"}), 1)
	record("b1", []string{"cr"}, t1)
	assert.Empty(t, list(store.RuntimeOrphanFilter{BrokerID: "b1"}))
	assert.Len(t, list(store.RuntimeOrphanFilter{BrokerID: "b2"}), 1, "other broker untouched")
}
