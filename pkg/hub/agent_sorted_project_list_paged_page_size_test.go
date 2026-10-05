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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the paged branch's page-size bound: the 500 clamp alone
// does not keep every paged request inside the per-request decision ceiling
// at every candidate count n, which needed the P_eff = min(limit,
// floor((4000-n)/7)) page-size bound (effectivePagedPageSize,
// agent_sorted_project_list.go).

// --- sorted mode must clamp limit to 500, and the paged page size must
// additionally stay inside the decision ceiling at every candidate count n --

// TestListProjectAgentsSorted_PagedPageSize_BoundedByN_DesignSizes is the
// primary decision-count test for the page-size bound: at limit=500, the
// paged branch's actual page size is P_eff = min(limit, floor((4000-n)/7)),
// not limit itself, so the per-request decision cost 5+n+7*P_eff never
// exceeds the decision ceiling (sortedProjectDecisionCeiling, 4,005) at any
// of the n values tested here.
//
// The expected page size and decision count are hard-coded from the
// page-size bound's own worked table here, not derived by calling
// effectivePagedPageSize (the function under test): a self-referential
// expected value cannot catch an over-strict P_eff (only an over-ceiling one,
// via the <=4,005 check). The literal table below is that worked example: at
// n<=500, P_eff==limit (500); at n=2,000, P_eff<=285.
func TestListProjectAgentsSorted_PagedPageSize_BoundedByN_DesignSizes(t *testing.T) {
	const limit = 500
	// n -> expected P_eff = min(500, floor((4000-n)/7)), hard-coded rather
	// than computed from effectivePagedPageSize.
	wantPEffBySize := map[int]int{
		500:  500, // floor(3500/7)=500, equal to limit
		501:  499, // floor(3499/7)=499
		700:  471, // floor(3300/7)=471 (471*7=3297, 472*7=3304)
		1200: 400, // floor(2800/7)=400
		2000: 285, // floor(2000/7)=285 (285*7=1995, 286*7=2002)
	}
	for n, wantPEff := range wantPEffBySize {
		n, wantPEff := n, wantPEff
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			// Sanity-check the hard-coded table against the function under
			// test and against the decision ceiling itself, so a genuine
			// future change to either the formula or the ceiling constant is
			// caught here too, not just silently diverges from this literal
			// table.
			require.Equal(t, wantPEff, effectivePagedPageSize(limit, n),
				"this test's hard-coded table must track effectivePagedPageSize's actual behavior")
			require.LessOrEqual(t, 5+n+7*wantPEff, sortedProjectDecisionCeiling,
				"the whole point of the page-size bound: P_eff must keep the paged request inside the decision ceiling")

			f := sortedListSetup(t)
			f.createAgentsBulk(t, n, "e2sz", string(state.PhaseStopped), nil) // nil ownerFor: every agent owned by f.owner, so R=n

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			// No fit: always paged, regardless of n (complete requires hasFit).
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&limit=%d", limit)), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)

			assert.Len(t, resp.Agents, wantPEff, "the page must hold exactly the worked table's P_eff, not min(limit, n)")
			assert.Equal(t, n, resp.TotalCount, "totalCount is the full readable candidate count, independent of page size")
			if wantPEff < n {
				assert.NotEmpty(t, resp.NextCursor, "fewer items than n were returned, so there must be a next page")
			} else {
				assert.Empty(t, resp.NextCursor, "P_eff consumed every candidate in one page")
			}

			want := 5 + n + 7*wantPEff
			assert.Len(t, emitter.records, want, "decision cost must reflect the worked table's P_eff, not the requested/clamped limit")
		})
	}
}

// TestListProjectAgentsSorted_PagedWalk_PageSizeBound_AllReadableReturnedOnce
// is the page-size bound's walk test: a limit=500 walk over n=2,000 must
// still return every readable agent exactly once, in order, even though
// P_eff (285 at n=2,000) is well under the requested limit -- both at R=n
// (every page item readable) and at R=400 (a strict readable subset, the
// R<n case).
func TestListProjectAgentsSorted_PagedWalk_PageSizeBound_AllReadableReturnedOnce(t *testing.T) {
	t.Run("R=n", func(t *testing.T) {
		f := sortedListSetup(t)
		const n = 2000
		f.createAgentsBulk(t, n, "e2walk-full", string(state.PhaseStopped), nil)

		want := referenceOrderIDs(t, f.store, f.project.ID, "updated", "desc", n+1)
		require.Len(t, want, n)

		got := walkAllPagesIDs(t, f, "desc", 500)
		assert.Equal(t, want, got, "a limit=500 walk at n=2,000 (R=n) must still concatenate to the full reference order despite P_eff<limit")
	})

	t.Run("R=400", func(t *testing.T) {
		f := sortedListSetup(t)
		caller := &store.User{
			ID: tid("sl-e2walk-caller"), Email: "sl-e2walk@test.com", DisplayName: "Caller",
			Role: store.UserRoleMember, Status: "active",
		}
		require.NoError(t, f.store.CreateUser(context.Background(), caller))
		ensureHubMembership(context.Background(), f.store, caller.ID)
		grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-e2walk-list-only")

		const n, r = 2000, 400
		agents := f.createAgentsBulk(t, n, "e2walk-partial", string(state.PhaseStopped), func(i int) string {
			if i < r {
				return caller.ID // readable to caller via the owner relationship grant
			}
			return f.owner.ID // unreadable to caller: no agent.read permission, no ownership
		})
		readable := make(map[string]bool, r)
		for i := 0; i < r; i++ {
			readable[agents[i].ID] = true
		}

		full := referenceOrderIDs(t, f.store, f.project.ID, "updated", "desc", n+1)
		require.Len(t, full, n)
		var want []string
		for _, id := range full {
			if readable[id] {
				want = append(want, id)
			}
		}
		require.Len(t, want, r)

		got := walkAllPagesIDsAs(t, f, caller, "desc", 500)
		assert.Equal(t, want, got, "a limit=500 walk at n=2,000, R=400 must return every readable agent exactly once, in reference order")
	})
}

// TestListProjectAgentsSorted_PagedRaced_PageSizeBound_StaysUnderRacedCeiling
// is the page-size bound's raced variant: at n=501, limit=500 (so
// P_eff=499, per effectivePagedPageSize), racing every single page item
// still costs exactly 4,498 decisions (5+n+8*P_eff: every raced item costs
// 8, not 7, because the race re-decision redoes all 8 actions including
// read, not just the 7 remaining ones) -- inside the raced exception to
// the decision ceiling: the ceiling plus the race allowance is 4,505
// (4,005 + 500), and the most any paged request can actually reach is
// 4,504, which is the bound asserted below. The unraced variant above is
// already at 4,005.
//
// pEff (and therefore the expected decision count) is hard-coded here, not
// derived by calling effectivePagedPageSize -- see
// PagedPageSize_BoundedByN_DesignSizes's doc comment for why a
// self-referential expected value cannot catch an over-strict P_eff.
func TestListProjectAgentsSorted_PagedRaced_PageSizeBound_StaysUnderRacedCeiling(t *testing.T) {
	f := sortedListSetup(t)
	const n = 501
	const limit = 500
	const wantPEff = 499 // floor((4000-501)/7) = floor(3499/7) = 499
	f.createAgentsBulk(t, n, "e2raced", string(state.PhaseStopped), nil)

	require.Equal(t, wantPEff, effectivePagedPageSize(limit, n),
		"this test's hard-coded pEff must track effectivePagedPageSize's actual behavior")
	require.Less(t, wantPEff, n, "a race on every page item is only interesting if the page doesn't already cover every candidate")

	raced := &racingAllMembersStore{Store: f.store}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&limit=%d", limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, wantPEff, "every page item must still be kept: the race only changes Labels, which no filter in this request cares about")

	const want = 5 + n + 8*wantPEff // 5 + 501 + 8*499 = 4,498
	assert.Len(t, emitter.records, want, "every page item raced costs 8 (full re-decision), not 7")
	assert.LessOrEqual(t, want, 4504, "the raced exception to the decision ceiling")
}

// racingAllMembersStore mutates every candidate's Labels (via the real
// store, bypassing the read path) the first time ListAgentMembers is
// called, simulating every page item racing between the member read and the
// full-row read -- the n-items generalization of
// mutatingAfterMembersStore, which only races one row.
type racingAllMembersStore struct {
	store.Store
	once sync.Once
}

func (r *racingAllMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := r.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil {
		return nil, err
	}
	r.once.Do(func() {
		for _, m := range members {
			a, gerr := r.GetAgent(ctx, m.ID)
			if gerr != nil {
				continue
			}
			a.Labels = map[string]string{"raced": "true"}
			_ = r.UpdateAgent(ctx, a)
		}
	})
	return members, nil
}
