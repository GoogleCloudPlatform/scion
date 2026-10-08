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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListAgents_SortedOrderMatchesAgentsortReference is the store-layer
// order-parity check for ListAgents' real-SQL sorted-mode branch: walking
// every page via the v2 keyset
// must concatenate to the agentsort reference order, for both sort keys and
// both directions, including the sub-second trailing-zero and NULL
// last_activity_event cases from the golden fixture.
func TestListAgents_SortedOrderMatchesAgentsortReference(t *testing.T) {
	s, projectID := newTestAgentStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type seeded struct {
		id, slug string
	}
	var all []seeded
	seed := func(slug string, created, updated, lastActivity time.Time) {
		a := createAgentWithTimestamps(t, s, projectID, slug, created, updated, lastActivity)
		all = append(all, seeded{id: a.ID, slug: slug})
	}

	seed("a", base, base.Add(1*time.Hour), time.Time{})
	seed("b", base.Add(1*time.Minute), base.Add(1*time.Hour), time.Time{})
	seed("c", base.Add(2*time.Minute), base.Add(2*time.Hour), base.Add(3*time.Hour))
	seed("d", base.Add(3*time.Minute), base.Add(5*time.Second+100*time.Millisecond), time.Time{})
	seed("e", base.Add(4*time.Minute), base.Add(5*time.Second+120*time.Millisecond), time.Time{})
	require.Len(t, all, 5)

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			// 5 is exactly the row count: a page that ends exactly at the
			// last row must carry no cursor.
			for _, pageSize := range []int{1, 2, 5, 500} {
				var walked []store.Agent
				binding := fmt.Sprintf("test-binding|%s|%s", sortKey, dir)
				opts := store.ListOptions{
					Limit: pageSize, SortBy: sortKey, SortDir: dir,
					CursorBinding: binding, SkipTotalCount: true,
				}
				pages := 0
				lastCursor := "unset"
				for i := 0; i < 10; i++ { // generous upper bound on page count
					result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, opts)
					require.NoError(t, err)
					pages++
					require.NotEmpty(t, result.Items, "sort=%s dir=%s pageSize=%d: no cursor may lead to an empty page", sortKey, dir, pageSize)
					walked = append(walked, result.Items...)
					lastCursor = result.NextCursor
					if result.NextCursor == "" {
						break
					}
					decoded, err := store.DecodeAgentCursor(result.NextCursor, sortKey, dir, binding)
					require.NoError(t, err)
					opts.SortCursor = &decoded
				}
				require.Len(t, walked, 5, "sort=%s dir=%s pageSize=%d: walk must cover every row exactly once", sortKey, dir, pageSize)
				assert.Empty(t, lastCursor, "sort=%s dir=%s pageSize=%d: the last page must carry no cursor", sortKey, dir, pageSize)
				assert.Equal(t, (5+pageSize-1)/pageSize, pages, "sort=%s dir=%s pageSize=%d: page count", sortKey, dir, pageSize)
				assertAgentsMatchRowOrder(t, sortKey, dir, walked)
			}
		}
	}

	// Pin the concrete expected order for sort=updated desc against the
	// golden fixture's slugs (same fixture shape as
	// TestListAgentMembers_OrderMatchesAgentsortReference).
	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{
		Limit: 500, SortBy: agentsort.Updated, SortDir: agentsort.Desc, SkipTotalCount: true,
	})
	require.NoError(t, err)
	slugByID := make(map[string]string, len(all))
	for _, a := range all {
		slugByID[a.id] = a.slug
	}
	slugs := make([]string, len(result.Items))
	for i, a := range result.Items {
		slugs[i] = slugByID[a.ID]
	}
	assert.Equal(t, []string{"c", "b", "a", "e", "d"}, slugs, "sort=updated desc order")
}

// TestListAgents_SortedTiesAndNullLastActivity walks every page of both
// sort keys and directions over a fixture built around ties: exact created
// ties (broken by id DESC), and sort=updated position-key ties between rows
// whose key comes from COALESCE(last_activity_event, updated) through
// different branches (NULL last_activity_event vs a set one equal to another
// row's updated), plus a created tie inside that key tie. The walk must equal
// the agentsort reference order exactly. It uses only typed ent writes at
// microsecond precision, so it runs unchanged on SQLite and on Postgres
// timestamptz under -tags integration (ptone/scion#2590).
func TestListAgents_SortedTiesAndNullLastActivity(t *testing.T) {
	s, projectID := newTestAgentStore(t)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	k := base.Add(10*time.Hour + 250*time.Microsecond) // shared updated position key

	type fixture struct {
		slug                           string
		created, updated, lastActivity time.Time
	}
	fixtures := []fixture{
		// Three-way exact created tie, all with NULL last_activity_event.
		{"tie-c1", base, base.Add(1 * time.Hour), time.Time{}},
		{"tie-c2", base, base.Add(2 * time.Hour), time.Time{}},
		{"tie-c3", base, base.Add(3 * time.Hour), time.Time{}},
		// sort=updated key k reached three ways: NULL last_activity_event
		// with updated == k; last_activity_event == k with an earlier
		// updated; and last_activity_event == k with a later updated. The
		// last two share created, so the key tie falls through to id.
		{"key-null", base.Add(1 * time.Minute), k, time.Time{}},
		{"key-lae-a", base.Add(2 * time.Minute), base.Add(5 * time.Hour), k},
		{"key-lae-b", base.Add(2 * time.Minute), base.Add(20 * time.Hour), k},
		// A NULL row and a set row on either side of k.
		{"before-k", base.Add(3 * time.Minute), base.Add(9 * time.Hour), time.Time{}},
		{"after-k", base.Add(4 * time.Minute), base.Add(1 * time.Hour), k.Add(time.Microsecond)},
	}
	created := make([]*store.Agent, 0, len(fixtures))
	for _, f := range fixtures {
		created = append(created, createAgentWithTimestamps(t, s, projectID, f.slug, f.created, f.updated, f.lastActivity))
	}
	n := len(created)

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			// Reference order from the fixture's own values, not from what
			// the store read back, so a precision or NULL round-trip bug
			// cannot hide in both sides.
			rows := make([]agentsort.Row, n)
			for i, f := range fixtures {
				rows[i] = agentsort.KeyFor(sortKey, created[i].ID, f.created, f.updated, f.lastActivity)
			}
			agentsort.SortRows(dir, rows)
			want := make([]string, n)
			for i, r := range rows {
				want[i] = r.ID
			}

			for _, pageSize := range []int{1, 2, 3, n, 500} {
				binding := fmt.Sprintf("ties|%s|%s", sortKey, dir)
				opts := store.ListOptions{
					Limit: pageSize, SortBy: sortKey, SortDir: dir,
					CursorBinding: binding, SkipTotalCount: true,
				}
				var got []string
				for i := 0; i <= n; i++ {
					result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, opts)
					require.NoError(t, err)
					for _, a := range result.Items {
						got = append(got, a.ID)
					}
					if result.NextCursor == "" {
						break
					}
					decoded, err := store.DecodeAgentCursor(result.NextCursor, sortKey, dir, binding)
					require.NoError(t, err)
					opts.SortCursor = &decoded
				}
				assert.Equal(t, want, got, "sort=%s dir=%s pageSize=%d", sortKey, dir, pageSize)
			}
		}
	}
}

// assertAgentsMatchRowOrder is the store.Agent analogue of
// assertMembersMatchRowOrder in agent_sorted_list_test.go.
func assertAgentsMatchRowOrder(t *testing.T, sortKey, dir string, got []store.Agent) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		prev := agentsort.KeyFor(sortKey, got[i-1].ID, got[i-1].Created, got[i-1].Updated, got[i-1].LastActivityEvent)
		cur := agentsort.KeyFor(sortKey, got[i].ID, got[i].Created, got[i].Updated, got[i].LastActivityEvent)
		if agentsort.Less(dir, cur, prev) {
			t.Fatalf("sort=%s dir=%s: row %d (%s) sorts before row %d (%s), but ListAgents returned them in the opposite order",
				sortKey, dir, i, got[i].ID, i-1, got[i-1].ID)
		}
	}
}

// TestListAgents_SortedRespectsFilter confirms the sorted-mode branch applies
// the same AgentFilter predicates (phase, label) as the legacy branch, since
// both share agentFilterPredicates.
func TestListAgents_SortedRespectsFilter(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	running := makeAgent(projectID, "sg-running-1")
	running.Phase = "running"
	require.NoError(t, s.CreateAgent(ctx, running))

	stopped := makeAgent(projectID, "sg-stopped-1")
	stopped.Phase = "stopped"
	require.NoError(t, s.CreateAgent(ctx, stopped))

	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, Phase: "running"}, store.ListOptions{
		Limit: 10, SortBy: agentsort.Updated, SortDir: agentsort.Desc, SkipTotalCount: true,
	})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "running", result.Items[0].Phase)
}

// TestListAgents_SortedInvalidSortOrDirFailsClosed pins ListAgents' own
// fail-closed contract for sorted mode: an unrecognized sort or dir
// returns ErrInvalidInput, independent of any one caller's own validation.
func TestListAgents_SortedInvalidSortOrDirFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	_, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{
		Limit: 10, SortBy: "bogus", SortDir: agentsort.Desc,
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)

	_, err = s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{
		Limit: 10, SortBy: agentsort.Created, SortDir: "sideways",
	})
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

// TestListAgents_SortedRejectsLegacyCursor pins that sorted mode fails
// closed on a legacy opaque cursor, whether passed alone or together with a
// SortCursor, instead of silently serving a page that ignores it.
func TestListAgents_SortedRejectsLegacyCursor(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateAgent(ctx, makeAgent(projectID, fmt.Sprintf("legacy-cur-%d", i))))
	}
	filter := store.AgentFilter{ProjectID: projectID}

	legacy, err := s.ListAgents(ctx, filter, store.ListOptions{Limit: 1, CursorBinding: "b"})
	require.NoError(t, err)
	require.NotEmpty(t, legacy.NextCursor)

	page0, err := s.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1, SortBy: agentsort.Created, SortDir: agentsort.Desc, CursorBinding: "b",
	})
	require.NoError(t, err)
	require.NotEmpty(t, page0.NextCursor)
	sortCur, err := store.DecodeAgentCursor(page0.NextCursor, agentsort.Created, agentsort.Desc, "b")
	require.NoError(t, err)

	_, err = s.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1, SortBy: agentsort.Created, SortDir: agentsort.Desc,
		Cursor: legacy.NextCursor, CursorBinding: "b",
	})
	require.ErrorIs(t, err, store.ErrInvalidInput, "legacy cursor alone in sorted mode")

	_, err = s.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1, SortBy: agentsort.Created, SortDir: agentsort.Desc,
		Cursor: legacy.NextCursor, SortCursor: &sortCur, CursorBinding: "b",
	})
	require.ErrorIs(t, err, store.ErrInvalidInput, "legacy cursor together with SortCursor")
}

// TestCountAgentsByPhaseIDs_MatchesFilterAndPhases pins CountAgentsByPhaseIDs'
// narrow id+phase projection and that it applies the same filter predicates
// as the rest of the agent store.
func TestCountAgentsByPhaseIDs_MatchesFilterAndPhases(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	running := makeAgent(projectID, "idph-running")
	running.Phase = "running"
	require.NoError(t, s.CreateAgent(ctx, running))

	stopped := makeAgent(projectID, "idph-stopped")
	stopped.Phase = "stopped"
	stopped.Labels = map[string]string{"team": "b"}
	require.NoError(t, s.CreateAgent(ctx, stopped))

	all, err := s.CountAgentsByPhaseIDs(ctx, store.AgentFilter{ProjectID: projectID})
	require.NoError(t, err)
	require.Len(t, all, 2)
	byID := make(map[string]string, len(all))
	for _, ip := range all {
		byID[ip.ID] = ip.Phase
	}
	assert.Equal(t, "running", byID[running.ID])
	assert.Equal(t, "stopped", byID[stopped.ID])

	// A "stats" caller clears Phase but keeps other filters (label here).
	labeled, err := s.CountAgentsByPhaseIDs(ctx, store.AgentFilter{ProjectID: projectID, Labels: map[string]string{"team": "b"}})
	require.NoError(t, err)
	require.Len(t, labeled, 1)
	assert.Equal(t, stopped.ID, labeled[0].ID)
}

// setRawCreatedUpdatedText overwrites agent id's stored created/updated TEXT
// directly via the driver, bypassing ent's typed time.Time setters. This is
// the only way to reproduce the exact on-disk text a monotonic-clock-bearing
// time.Now() write produces: this SQLite driver's default time.Time
// parameter conversion renders Go's verbose Stringer form, which appends
// " m=+X.XXXXXXXXX" while the value still carries a monotonic reading -- a
// form Go's public time API cannot construct directly, since a monotonic
// reading can only come from an actual time.Now() call. SQLite only: callers
// must call enttest.SkipOnPostgres before building their fixtures.
func setRawCreatedUpdatedText(t *testing.T, s *AgentStore, id, createdText, updatedText string) {
	t.Helper()
	err := s.client.Driver().Exec(context.Background(),
		"UPDATE agents SET created = ?, updated = ? WHERE id = ?",
		[]any{createdText, updatedText, id}, nil)
	require.NoError(t, err)
}

// TestListAgents_SortedSurvivesMonotonicSuffixAndVariableFractions is the
// regression test for the read-side normalization fix in
// agentTimeColumnExpr/agentTimeArg: rows whose stored
// created/updated TEXT carries a monotonic-clock suffix (as every row
// written via time.Now() before any future write-side fix does), rows
// without one, and rows with different fractional-second digit counts must
// all page correctly -- no duplicate, no skipped row -- for both ASC and
// DESC, including exact ties on created (broken by id) between a
// suffix-bearing and a suffix-free row.
func TestListAgents_SortedSurvivesMonotonicSuffixAndVariableFractions(t *testing.T) {
	// Skip before creating fixtures: setRawCreatedUpdatedText (this test's
	// only caller of it) writes SQLite-only TEXT timestamps.
	enttest.SkipOnPostgres(t, "writes SQLite TEXT timestamps (with a monotonic suffix) that only the SQLite driver produces")
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	type row struct {
		id, slug, createdText, updatedText string
	}
	// a, b: a genuine tie on created (identical instant, one with a
	// monotonic suffix, one without -- exactly the self-boundary shape the
	// original bug hit), broken by id.
	// c: variable-length fraction (trailing-zero / short fraction).
	// d: no fractional seconds at all (time.Time.String() omits the "."
	// entirely when sub-second is exactly zero).
	// e: a large monotonic reading (multi-digit seconds component, e.g.
	// "m=+123.456") to rule out any accidental numeric (vs. textual)
	// handling of the suffix.
	rows := []row{
		{slug: "a", createdText: "2026-01-01 00:00:05.123456789 +0000 UTC m=+10.000000001"},
		{slug: "b", createdText: "2026-01-01 00:00:05.123456789 +0000 UTC"},
		{slug: "c", createdText: "2026-01-01 00:00:06.1 +0000 UTC m=+11.5"},
		{slug: "d", createdText: "2026-01-01 00:00:07 +0000 UTC"},
		{slug: "e", createdText: "2026-01-01 00:00:08.5 +0000 UTC m=+123.456"},
	}
	for i := range rows {
		rows[i].updatedText = rows[i].createdText // keep sort=updated's COALESCE path exercised identically
		a := makeAgent(projectID, "msuf-"+rows[i].slug)
		rows[i].id = a.ID
		require.NoError(t, s.CreateAgent(ctx, a))
		setRawCreatedUpdatedText(t, s, rows[i].id, rows[i].createdText, rows[i].updatedText)
	}

	// Ground truth: parse each row's own stored text (stripping any " m="
	// suffix, exactly as the production normalization does) to build the
	// agentsort reference order independent of the fix under test.
	parseNormalized := func(text string) time.Time {
		if idx := strings.Index(text, " m="); idx >= 0 {
			text = text[:idx]
		}
		tm, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", text)
		require.NoError(t, err, "parse %q", text)
		return tm
	}

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			refRows := make([]agentsort.Row, len(rows))
			for i, r := range rows {
				ts := parseNormalized(r.createdText)
				refRows[i] = agentsort.KeyFor(sortKey, r.id, ts, ts, time.Time{})
			}
			agentsort.SortRows(dir, refRows)
			want := make([]string, len(refRows))
			for i, rr := range refRows {
				want[i] = rr.ID
			}

			var walked []string
			binding := fmt.Sprintf("test|%s|%s", sortKey, dir)
			opts := store.ListOptions{Limit: 2, SortBy: sortKey, SortDir: dir, CursorBinding: binding, SkipTotalCount: true}
			for i := 0; i < 10; i++ {
				result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, opts)
				require.NoError(t, err)
				for _, a := range result.Items {
					walked = append(walked, a.ID)
				}
				if result.NextCursor == "" {
					break
				}
				decoded, err := store.DecodeAgentCursor(result.NextCursor, sortKey, dir, binding)
				require.NoError(t, err)
				opts.SortCursor = &decoded
			}
			assert.Equal(t, want, walked, "sort=%s dir=%s: no duplicate or skipped row across monotonic-suffix/mixed-fraction rows", sortKey, dir)
		}
	}
}

// A sorted cursor is positioned by instant, not by the zone its timestamps
// were written in: the same K and created expressed with a non-UTC offset
// (as time.Parse returns them for a cursor carrying "+05:45") must resume at
// exactly the same row as the UTC form.
func TestListAgents_SortedCursorIsZoneIndependent(t *testing.T) {
	s, projectID := newTestAgentStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		createAgentWithTimestamps(t, s, projectID, fmt.Sprintf("zone-%d", i), ts, ts, time.Time{})
	}
	kathmandu := time.FixedZone("+0545", 5*3600+45*60)

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			binding := fmt.Sprintf("zone-binding|%s|%s", sortKey, dir)
			opts := store.ListOptions{Limit: 2, SortBy: sortKey, SortDir: dir, CursorBinding: binding, SkipTotalCount: true}
			page0, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, opts)
			require.NoError(t, err)
			require.NotEmpty(t, page0.NextCursor)
			utcCur, err := store.DecodeAgentCursor(page0.NextCursor, sortKey, dir, binding)
			require.NoError(t, err)

			shifted := utcCur
			shifted.K = utcCur.K.In(kathmandu)
			shifted.Created = utcCur.Created.In(kathmandu)

			var pages [2][]string
			for i, cur := range []store.AgentCursor{utcCur, shifted} {
				o := opts
				c := cur
				o.SortCursor = &c
				res, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, o)
				require.NoError(t, err)
				for _, a := range res.Items {
					pages[i] = append(pages[i], a.ID)
				}
			}
			require.Len(t, pages[0], 2, "sort=%s dir=%s", sortKey, dir)
			assert.Equal(t, pages[0], pages[1], "sort=%s dir=%s: a +05:45 cursor must resume at the same row as its UTC form", sortKey, dir)
		}
	}
}
