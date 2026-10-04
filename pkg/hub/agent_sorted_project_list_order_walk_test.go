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
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the order-parity gate: an HTTP-level page walk whose
// concatenated pages must equal the agentsort reference order over the
// authorized, filtered set, at a scale closer to a realistic project
// (1,200 agents) than the small fixtures used elsewhere in this package.
//
// Scope note (deliberate reduction): the full matrix would be page sizes
// {1, 7, 25, 500} at n=1,200. Sorted mode's per-request cost is 5+n+7P -- every
// page re-evaluates the read pass over all 1,200 candidates, so a
// fine-grained walk (e.g. limit=1, 1,200 requests) at that N costs well over
// a million decisions and would make this single test the slowest thing in
// the suite by a wide margin. This test instead: (a) walks
// n=1,200 at limit=500 (3 pages, the page size real drains actually use)
// for both directions, and (b) walks the remaining {1,7,25} page sizes
// at a smaller n=100, which exercises the same
// off-page-boundary and cursor-continuation logic -- the actual risk this
// gate protects against -- at a cost the suite can afford. limit=500 is
// covered at n=100 by the existing small-fixture tests elsewhere in this
// package.

// referenceOrderIDs returns the authorized, filtered set's IDs in the
// sorted-mode total order for (sort, dir), independent of any HTTP
// pagination: a direct, single, unpaged store.Store.ListAgentMembers call.
func referenceOrderIDs(t *testing.T, s store.Store, projectID, sortKey, dir string, max int) []string {
	t.Helper()
	members, err := s.ListAgentMembers(context.Background(), store.AgentFilter{ProjectID: projectID}, sortKey, dir, max)
	require.NoError(t, err)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	return ids
}

// walkAllPagesIDs drives the real HTTP endpoint page by page (sort=updated)
// and returns the concatenated agent IDs, as f.owner.
func walkAllPagesIDs(t *testing.T, f *sortedListFixture, dir string, limit int) []string {
	t.Helper()
	return walkAllPagesIDsAs(t, f, f.owner, dir, limit)
}

// walkAllPagesIDsAs is walkAllPagesIDs for a caller other than f.owner: the
// R<n walk needs a caller with a strict readable subset, via
// grantProjectListOnly.
func walkAllPagesIDsAs(t *testing.T, f *sortedListFixture, user *store.User, dir string, limit int) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Lessf(t, pages, 5000, "walk did not terminate within a sane number of pages")
		q := fmt.Sprintf("sort=updated&dir=%s&limit=%d", dir, limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			ids = append(ids, a.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	return ids
}

func TestListProjectAgentsSorted_OrderParity_1200Agents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large order-parity walk in -short mode")
	}
	f := sortedListSetup(t)
	const n = 1200
	f.createAgentsBulk(t, n, "order1200", string(state.PhaseStopped), nil)

	for _, dir := range []string{"desc", "asc"} {
		t.Run(dir, func(t *testing.T) {
			want := referenceOrderIDs(t, f.store, f.project.ID, "updated", dir, n+1)
			require.Len(t, want, n)
			got := walkAllPagesIDs(t, f, dir, 500)
			require.Equal(t, want, got, "the HTTP page walk at n=1200 must concatenate to the agentsort reference order")
		})
	}
}

func TestListProjectAgentsSorted_OrderParity_PageSizeSweep(t *testing.T) {
	f := sortedListSetup(t)
	const n = 100
	f.createAgentsBulk(t, n, "orderpage", string(state.PhaseStopped), nil)

	want := referenceOrderIDs(t, f.store, f.project.ID, "updated", "desc", n+1)
	require.Len(t, want, n)

	for _, limit := range []int{1, 7, 25} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			got := walkAllPagesIDs(t, f, "desc", limit)
			require.Equal(t, want, got, "the HTTP page walk at limit=%d must concatenate to the agentsort reference order", limit)
		})
	}
}

// TestListProjectAgentsSorted_OrderParity_PhaseAndLabelFilter covers the
// filters (phase, label) dimension: the walk must still concatenate
// to the reference order when restricted to a phase and a label.
func TestListProjectAgentsSorted_OrderParity_PhaseAndLabelFilter(t *testing.T) {
	f := sortedListSetup(t)
	const n = 60
	for i := 0; i < n; i++ {
		phase := string(state.PhaseStopped)
		labels := map[string]string{"team": "b"}
		if i%2 == 0 {
			phase = string(state.PhaseRunning)
			labels = map[string]string{"team": "a"}
		}
		f.createAgent(t, fmt.Sprintf("filt-%d", i), phase, labels)
	}

	wantFiltered, err := f.store.ListAgentMembers(context.Background(),
		store.AgentFilter{ProjectID: f.project.ID, Phase: "running", Labels: map[string]string{"team": "a"}},
		"updated", "desc", n+1)
	require.NoError(t, err)
	wantIDs := make([]string, len(wantFiltered))
	for i, m := range wantFiltered {
		wantIDs[i] = m.ID
	}
	require.NotEmpty(t, wantIDs)

	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 1000)
		q := "sort=updated&dir=desc&limit=7&phase=running&label=team=a"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			got = append(got, a.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	require.Equal(t, wantIDs, got, "a phase+label-filtered walk must concatenate to the equally-filtered reference order")
}

// TestListProjectAgentsSorted_PagedWalk_NonOwnerPartialRead_IndependentReference
// closes a gap in the existing order-parity walks: they all use the owner
// identity (R==n), a single phase, no other project in the store, and
// reference order built from the same ListAgentMembers call the handler
// itself uses -- "the test checks paging against the same sort it uses as
// reference, not against an independent oracle." This walk instead uses:
//   - a non-owner member identity (grantProjectListOnly) whose readable set
//     is a strict subset (R < n) of the candidate pool;
//   - two other projects present in the same store, with their own agents,
//     to prove the walk cannot leak a row across projects;
//   - mixed phases and a label with an empty value among the candidates;
//   - a reference order built independently of ListAgentMembers: the real,
//     store-persisted rows are fetched via GetAgentsByIDs (which applies no
//     ordering at all, unlike ListAgentMembers), and sorted with
//     agentsort.SortRows/Less directly, by this test, not by the store.
func TestListProjectAgentsSorted_PagedWalk_NonOwnerPartialRead_IndependentReference(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()

	// Noise: two other projects, each with their own agents, so a
	// cross-project leak (or a bug in the project-scoping of the sort/walk)
	// would show up as an unexpected ID rather than passing by accident
	// because this store only ever had one project in it.
	for _, suffix := range []string{"other-a", "other-b"} {
		otherProject := &store.Project{
			ID: tid("sl-pr-" + suffix), Name: "Other " + suffix, Slug: "sl-pr-" + suffix,
			OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, f.store.CreateProject(ctx, otherProject))
		f.srv.seedProjectCreatorMembership(ctx, otherProject)
		for i := 0; i < 5; i++ {
			a := &store.Agent{
				ID: tid(fmt.Sprintf("sl-pr-%s-agent-%d", suffix, i)), Slug: fmt.Sprintf("pr-%s-%d", suffix, i), Name: fmt.Sprintf("pr-%s-%d", suffix, i),
				ProjectID: otherProject.ID, Phase: string(state.PhaseRunning),
				CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
			}
			require.NoError(t, f.store.CreateAgent(ctx, a))
		}
	}

	// A non-owner member identity with a strict readable subset: a
	// project-scoped role carrying only agent.list, plus ownership of some
	// agents (the same grantProjectListOnly + per-agent-ownership technique
	// designsizes_test.go uses for R<n).
	caller := &store.User{
		ID: tid("sl-pr-caller"), Email: "sl-pr-caller@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, caller))
	ensureHubMembership(ctx, f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-pr-list-only")

	const n = 40
	const r = 15 // the first r agents are caller-owned (readable); the rest are owner-owned (unreadable to caller)
	phases := []string{string(state.PhaseRunning), string(state.PhaseStopped)}
	var created []*store.Agent
	err := f.store.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < n; i++ {
			owner := f.owner.ID
			if i < r {
				owner = caller.ID
			}
			labels := map[string]string{"team": "a"}
			if i%3 == 0 {
				labels = map[string]string{"team": ""} // a label with an empty value
			}
			a := &store.Agent{
				ID: tid(fmt.Sprintf("sl-pr-agent-%d", i)), Slug: fmt.Sprintf("pr-agent-%d", i), Name: fmt.Sprintf("pr-agent-%d", i),
				ProjectID: f.project.ID, Phase: phases[i%2], // mixed phases
				CreatedBy: owner, OwnerID: owner, Labels: labels,
			}
			if err := tx.CreateAgent(ctx, a); err != nil {
				return err
			}
			created = append(created, a)
		}
		return nil
	})
	require.NoError(t, err)

	readableIDs := make([]string, 0, r)
	for i := 0; i < r; i++ {
		readableIDs = append(readableIDs, created[i].ID)
	}

	// Independent reference: fetch the real persisted rows via
	// GetAgentsByIDs (no ordering applied at all -- unlike ListAgentMembers,
	// which is what the handler under test itself calls), then sort them
	// with agentsort.SortRows/Less directly, in this test, over the
	// caller-readable subset only.
	fullRows, err := f.store.GetAgentsByIDs(ctx, readableIDs)
	require.NoError(t, err)
	require.Len(t, fullRows, r)
	rows := make([]agentsort.Row, 0, r)
	for _, id := range readableIDs {
		full := fullRows[id]
		rows = append(rows, agentsort.KeyFor(agentsort.Updated, full.ID, full.Created, full.Updated, full.LastActivityEvent))
	}
	agentsort.SortRows(agentsort.Desc, rows)
	want := make([]string, len(rows))
	for i, row := range rows {
		want[i] = row.ID
	}

	readableSet := make(map[string]bool, r)
	for _, id := range readableIDs {
		readableSet[id] = true
	}

	t.Run("no_label_filter", func(t *testing.T) {
		got := walkAllPagesIDsAs(t, f, caller, "desc", 7)
		assert.Equal(t, want, got, "a non-owner member's walk over a multi-project store with a strict readable subset must match an independently agentsort-sorted reference")
		for _, id := range got {
			assert.True(t, readableSet[id], "the walk must never return an agent from another project or one the caller cannot read: %s", id)
		}
	})

	// The fixture above creates team="" (empty-value label) rows, but the
	// walk never actually filtered on that label, so the k="" filter edge
	// case went unexercised on the sorted path. This sub-test
	// reuses the exact same fixture (not a fresh one) and adds label=team=,
	// checked against an independently filtered reference: the same
	// readable/GetAgentsByIDs/agentsort.SortRows oracle as above, additionally
	// restricted in this test to the rows whose persisted Labels["team"]=="".
	t.Run("empty_value_label_filter", func(t *testing.T) {
		var wantFiltered []string
		for _, row := range rows {
			// Distinguish "key present with an empty value" from "key
			// absent" (a bare map index would treat both as "" and
			// silently pass a missing-key row too; it makes no
			// difference for this fixture, since every row sets "team"
			// explicitly, but the predicate should still say what it means).
			v, ok := fullRows[row.ID].Labels["team"]
			if ok && v == "" {
				wantFiltered = append(wantFiltered, row.ID)
			}
		}
		require.NotEmpty(t, wantFiltered, "the fixture must actually contain readable team=\"\" rows, or this sub-test proves nothing")

		// limit=2: the 5 readable team="" rows no longer fit on a single
		// page (limit=7 did, so a cursor carrying an empty-value label
		// filter was never exercised across pages).
		var got []string
		pages := 0
		cursor := ""
		for ; ; pages++ {
			require.Less(t, pages, 1000)
			q := "sort=updated&dir=desc&limit=2&label=team="
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath(q), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)
			for _, a := range resp.Agents {
				got = append(got, a.ID)
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		require.Greater(t, pages, 0, "limit=2 over 5 rows must take more than one page, or the cursor round-trip for an empty-value label filter is not actually exercised")
		assert.Equal(t, wantFiltered, got, "a label=team= (empty-value) walk must match the same independent reference, additionally filtered to team=\"\" in this test")
		for _, id := range got {
			assert.True(t, readableSet[id], "the filtered walk must never return an agent from another project or one the caller cannot read: %s", id)
		}
	})
}
