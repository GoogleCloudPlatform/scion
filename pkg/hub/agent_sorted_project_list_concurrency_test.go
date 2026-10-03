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
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the sorted-mode concurrency contract. Every
// project sorted-mode page request re-reads the full (bounded) member
// snapshot and positions after the cursor by comparison
// (pkg/store/agentsort), not by a stored offset, so each concurrent-change
// case falls out of that construction rather than needing bespoke handling
// per case. These tests exercise the three directions reachable through the
// real store's forward-only clock (CreateAgent/UpdateAgent(Status) always
// stamp time.Now(), so a real agent's sort key never regresses):
//
//   - delete before the next page fetch: the row disappears, nothing shifts;
//   - insert before the next page fetch: for a desc sort a new row always
//     sorts newest-first, i.e. before the cursor, so it is invisible to a
//     later page and appears only on a fresh page-0 fetch;
//   - a key bump (heartbeat) that moves an unfetched row from "after the
//     cursor" (not yet shown) to "before it" (newer than everything already
//     shown): the walk skips it, which is the documented contract for a
//     row that moves from after the cursor to before it.
//
// The reverse crossing (a row that moves from before the cursor to after
// it appears twice) needs a row's key to REGRESS, which
// this store's real write paths (every UpdateAgent/UpdateAgentStatus call
// stamps time.Now(), monotonically non-decreasing) cannot produce for a real
// agent; the same comparison code path handles both directions
// symmetrically (pkg/store/agentsort.Less has no direction-specific special
// case), so the gap is covered directly instead, below, via a store
// decorator that forces a key regression.
func TestListProjectAgentsSorted_Concurrency_DeleteBetweenPages(t *testing.T) {
	f := sortedListSetup(t)
	const n = 6
	const limit = 3
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		a := f.createAgent(t, fmt.Sprintf("del-%d", i), string(state.PhaseStopped), nil)
		ids[i] = a.ID
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, page0.Agents, limit)
	require.NotEmpty(t, page0.NextCursor)

	// Delete one of the not-yet-fetched agents (created earliest, so it
	// sorts last under dir=desc and is in the second page).
	require.NoError(t, f.store.DeleteAgent(context.Background(), ids[0]))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(page0.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page1 := mustDecodeListAgentsResponse(t, rec.Body)

	seen := map[string]bool{}
	for _, a := range page0.Agents {
		seen[a.ID] = true
	}
	for _, a := range page1.Agents {
		assert.False(t, seen[a.ID], "an agent must not appear on two pages of the same walk")
		seen[a.ID] = true
	}
	assert.False(t, seen[ids[0]], "the deleted agent must not appear on either page")
	assert.Len(t, seen, n-1, "every surviving agent must appear exactly once across the two pages")
}

func TestListProjectAgentsSorted_Concurrency_InsertInvisibleToLaterPage(t *testing.T) {
	f := sortedListSetup(t)
	const n = 4
	const limit = 2
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("ins-%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, page0.NextCursor)

	// A brand-new agent is always the newest thing in the project, so under
	// dir=desc it sorts before the cursor -- invisible to a later page.
	newAgent := f.createAgent(t, "ins-new", string(state.PhaseStopped), nil)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(page0.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page1 := mustDecodeListAgentsResponse(t, rec.Body)
	for _, a := range page1.Agents {
		assert.NotEqual(t, newAgent.ID, a.ID, "an agent created after the cursor was minted must not appear in a later page under dir=desc")
	}

	// It IS visible on a fresh page-0 fetch.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	freshPage0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, freshPage0.Agents)
	assert.Equal(t, newAgent.ID, freshPage0.Agents[0].ID, "the newest agent must lead a fresh page-0 fetch")
}

func TestListProjectAgentsSorted_Concurrency_KeyBumpMovesRowOutOfLaterPage(t *testing.T) {
	f := sortedListSetup(t)
	const n = 6
	const limit = 3
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		a := f.createAgent(t, fmt.Sprintf("bump-%d", i), string(state.PhaseStopped), nil)
		ids[i] = a.ID
	}
	// ids[0] was created first, so under dir=desc it is deepest in the walk
	// (destined for the second page).
	target := ids[0]

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, page0.NextCursor)
	for _, a := range page0.Agents {
		require.NotEqual(t, target, a.ID, "target must not already be on page 0")
	}

	// Heartbeat-style bump: moves target's key to "now", newer than
	// everything already shown on page 0, i.e. from "after the cursor"
	// (not yet visited) to "before it".
	require.NoError(t, f.store.UpdateAgentStatus(context.Background(), target, store.AgentStatusUpdate{Activity: "executing"}))

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(page0.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page1 := mustDecodeListAgentsResponse(t, rec.Body)
	for _, a := range page1.Agents {
		assert.NotEqual(t, target, a.ID,
			"a row bumped to be newer than everything already shown must be skipped by the walk, not shown a second time on the old cursor's continuation")
	}
}

// regressingAfterNCallsStore makes one agent's position key appear older
// than it actually is, starting from the Nth call to ListAgentMembers. Real
// write paths in this store cannot regress a key (every write stamps
// time.Now(), monotonically non-decreasing — see the package doc comment
// above), so this reverse crossing ("moves from before the cursor to after
// it", the "duplicate" case) is exercised at this
// synthetic boundary instead of through a real mutation.
type regressingAfterNCallsStore struct {
	store.Store
	agentID      string
	regressAfter int // ListAgentMembers call number (1-indexed) after which the row regresses
	olderThan    time.Time
	calls        int
}

func (r *regressingAfterNCallsStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := r.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil {
		return nil, err
	}
	r.calls++
	if r.calls > r.regressAfter {
		for i := range members {
			if members[i].ID == r.agentID {
				// Older than everything else in the fixture: K becomes the
				// oldest, LastActivityEvent cleared so K falls back to
				// Updated under the COALESCE rule.
				members[i].Updated = r.olderThan
				members[i].LastActivityEvent = time.Time{}
			}
		}
	}
	return members, nil
}

// TestListProjectAgentsSorted_Concurrency_KeyRegressionDuplicatesRow proves:
// a row already shown on an earlier page, whose key then regresses to sort
// after the cursor, is shown again on a later page of the same walk. This
// is the concurrency contract's stated behavior for that crossing direction
// (the row appears twice), not a bug — the test exists to prove the walk
// reaches that documented outcome (via re-reading and repositioning, not an
// offset) rather than silently deduplicating or skipping, and to pin it as
// a regression guard since positionAfterCursor's comparison is symmetric
// (pkg/store/agentsort.Less has no direction-specific branch): if the
// "skip" direction (tested above) works, this proves the same code path
// also produces the mirror-image "duplicate" outcome the contract specifies.
func TestListProjectAgentsSorted_Concurrency_KeyRegressionDuplicatesRow(t *testing.T) {
	f := sortedListSetup(t)
	const n = 6
	const limit = 3
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		a := f.createAgent(t, fmt.Sprintf("regress-%d", i), string(state.PhaseStopped), nil)
		ids[i] = a.ID
	}
	// ids[n-1] was created last, so it leads page 0 under dir=desc.
	target := ids[n-1]

	veryOld := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	raced := &regressingAfterNCallsStore{Store: f.store, agentID: target, regressAfter: 1, olderThan: veryOld}
	f.srv.store = raced

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, page0.NextCursor)
	var sawOnPage0 bool
	for _, a := range page0.Agents {
		if a.ID == target {
			sawOnPage0 = true
		}
	}
	require.True(t, sawOnPage0, "target must lead page 0 before regressing")

	// Walk the rest of the pages; the regressed row now sorts after
	// everything, so it surfaces again on the last page.
	cursor := page0.NextCursor
	var seenAgain bool
	for pages := 0; cursor != "" && pages < 10; pages++ {
		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
			f.listPath("sort=updated&dir=desc&limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(cursor)), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		page := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range page.Agents {
			if a.ID == target {
				seenAgain = true
			}
		}
		cursor = page.NextCursor
	}
	assert.True(t, seenAgain, "a row whose key regresses below the cursor must resurface later in the same walk (design 4.5, documented behavior)")
}
