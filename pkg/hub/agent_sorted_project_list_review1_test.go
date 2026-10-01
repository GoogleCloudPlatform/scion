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
	"net/http/httptest"
	"net/url"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file closes the remaining r1 review findings: B3 (limit clamp), B4
// (includeDeleted), B6 c/d/e/f (S6 test-plan gaps), B7 (S3/A3 test-plan
// gaps), N2 (S10 R<=fit), N6 (legacy byte-identity with sort-only params).
// The original B3 test (TestListProjectAgentsSorted_LimitClampedTo500) is
// superseded below by the r2 review's B-1 finding and its resolution,
// erratum E2 (lists-graph-errata.md): the 500 clamp alone does not keep
// every paged request inside the A15 decision ceiling at every candidate
// count n, which needed the P_eff = min(limit, floor((4000-n)/7)) page-size
// bound (effectivePagedPageSize, agent_sorted_project_list.go).

// --- B3/B-1: sorted mode must clamp limit to 500, and the paged page size
// must additionally stay inside A15 at every candidate count n -------------

// TestListProjectAgentsSorted_PagedPageSize_BoundedByN_DesignSizes is
// erratum E2's primary S6 test, superseding the original B3 test
// (TestListProjectAgentsSorted_LimitClampedTo500, which pinned
// limit=700/n=700 at 5+700+7*500=4,205 decisions -- itself over the A15
// ceiling, r2 review finding B-1): at limit=500, the paged branch's actual
// page size is P_eff = min(limit, floor((4000-n)/7)), not limit itself, so
// the per-request decision cost 5+n+7*P_eff never exceeds the A15 ceiling
// (sortedProjectDecisionCeiling, 4,005) at any of the design's own n values.
//
// r3 review N-1: the expected page size and decision count are hard-coded
// from erratum E2's own table here, not derived by calling
// effectivePagedPageSize (the function under test) -- the reviewer mutated
// "/7" to "/8" in that function and both this test and PagedRaced_E2 still
// passed, because a self-referential expected value cannot catch an
// over-strict P_eff (only an over-ceiling one, via the <=4,005 check). The
// literal table below is erratum E2's own worked example: at n<=500,
// P_eff==limit (500); at n=2,000, P_eff<=285.
func TestListProjectAgentsSorted_PagedPageSize_BoundedByN_DesignSizes(t *testing.T) {
	const limit = 500
	// n -> expected P_eff = min(500, floor((4000-n)/7)), hard-coded per r3
	// review N-1, not computed from effectivePagedPageSize.
	wantPEffBySize := map[int]int{
		500:  500,  // floor(3500/7)=500, equal to limit
		501:  499,  // floor(3499/7)=499
		700:  471,  // floor(3300/7)=471 (471*7=3297, 472*7=3304)
		1200: 400,  // floor(2800/7)=400
		2000: 285,  // floor(2000/7)=285 (285*7=1995, 286*7=2002)
	}
	for n, wantPEff := range wantPEffBySize {
		n, wantPEff := n, wantPEff
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			// Sanity-check the hard-coded table against the function under
			// test and against A15 itself, so a genuine future change to
			// either the formula or the ceiling constant is caught here
			// too, not just silently diverges from this literal table.
			require.Equal(t, wantPEff, effectivePagedPageSize(limit, n),
				"this test's hard-coded table must track effectivePagedPageSize's actual behavior")
			require.LessOrEqual(t, 5+n+7*wantPEff, sortedProjectDecisionCeiling,
				"the erratum's whole point: P_eff must keep the paged request inside A15")

			f := sortedListSetup(t)
			f.createAgentsBulk(t, n, "e2sz", string(state.PhaseStopped), nil) // nil ownerFor: every agent owned by f.owner, so R=n

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			// No fit: always paged, regardless of n (complete requires hasFit).
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&limit=%d", limit)), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)

			assert.Len(t, resp.Agents, wantPEff, "the page must hold exactly the erratum E2 table's P_eff, not min(limit, n)")
			assert.Equal(t, n, resp.TotalCount, "totalCount is the full readable candidate count, independent of page size")
			if wantPEff < n {
				assert.NotEmpty(t, resp.NextCursor, "fewer items than n were returned, so there must be a next page")
			} else {
				assert.Empty(t, resp.NextCursor, "P_eff consumed every candidate in one page")
			}

			want := 5 + n + 7*wantPEff
			assert.Len(t, emitter.records, want, "decision cost must reflect the erratum E2 table's P_eff, not the requested/clamped limit")
		})
	}
}

// TestListProjectAgentsSorted_PagedWalk_E2_AllReadableReturnedOnce is
// erratum E2's walk test: a limit=500 walk over n=2,000 must still return
// every readable agent exactly once, in order, even though P_eff (285 at
// n=2,000) is well under the requested limit -- both at R=n (every page
// item readable) and at R=400 (a strict readable subset, design 5.3's R<n
// case), per the erratum's own test list ("with R=n, and with R=400").
func TestListProjectAgentsSorted_PagedWalk_E2_AllReadableReturnedOnce(t *testing.T) {
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

// TestListProjectAgentsSorted_PagedRaced_E2_StaysUnderRacedCeiling is
// erratum E2's raced variant: at n=501, limit=500 (so P_eff=499, per
// effectivePagedPageSize), racing every single page item still costs
// exactly 4,498 decisions (5+n+8*P_eff: every raced item costs 8, not 7,
// because step 5a re-decides all 8 actions including read, not just the 7
// remaining ones) -- inside the design's stated raced exception to A15
// (4,505), even though the unraced variant above is already at 4,005.
//
// r3 review N-1: pEff (and therefore the expected decision count) is
// hard-coded here, not derived by calling effectivePagedPageSize -- see
// PagedPageSize_BoundedByN_DesignSizes's doc comment for why a
// self-referential expected value cannot catch an over-strict P_eff.
func TestListProjectAgentsSorted_PagedRaced_E2_StaysUnderRacedCeiling(t *testing.T) {
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
	assert.LessOrEqual(t, want, 4504, "the design's stated raced exception to A15")
}

// racingAllMembersStore mutates every candidate's Labels (via the real
// store, bypassing the read path) the first time ListAgentMembers is
// called, simulating every page item racing between the member read and the
// full-row read (design 5.3 step 5a) -- the n-items generalization of
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
			a, gerr := r.Store.GetAgent(ctx, m.ID)
			if gerr != nil {
				continue
			}
			a.Labels = map[string]string{"raced": "true"}
			_ = r.Store.UpdateAgent(ctx, a)
		}
	})
	return members, nil
}

// --- B4: includeDeleted=true must behave like legacy mode ------------------

func softDeleteAgentForTest(t *testing.T, s store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	a.DeletedAt = time.Now().UTC()
	require.NoError(t, s.UpdateAgent(ctx, a))
}

// TestListProjectAgentsSorted_IncludeDeleted_Complete reproduces and fixes
// the r1 review's finding: a complete response with includeDeleted=true
// used to silently drop the soft-deleted agent (classified as "missing
// between the two reads" by GetAgentsByIDs's hard-coded DeletedAtIsNil()),
// while CountAgents/ListAgentMembers/stats already honored IncludeDeleted --
// so the page, totalCount and stats disagreed with each other and with
// legacy mode.
func TestListProjectAgentsSorted_IncludeDeleted_Complete(t *testing.T) {
	f := sortedListSetup(t)
	live := f.createAgent(t, "live-complete", string(state.PhaseStopped), nil)
	deleted := f.createAgent(t, "deleted-complete", string(state.PhaseStopped), nil)
	softDeleteAgentForTest(t, f.store, deleted.ID)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&stats=1&includeDeleted=true"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)

	ids := map[string]bool{}
	for _, a := range resp.Agents {
		ids[a.ID] = true
	}
	assert.True(t, ids[live.ID], "the live agent must be present")
	assert.True(t, ids[deleted.ID], "includeDeleted=true must keep the soft-deleted agent, matching legacy semantics")
	assert.Equal(t, 2, resp.TotalCount, "totalCount must agree with the page")
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 2, resp.Stats.Total, "stats must agree with the page and totalCount")
}

func TestListProjectAgentsSorted_IncludeDeleted_Paged(t *testing.T) {
	f := sortedListSetup(t)
	live := f.createAgent(t, "live-paged", string(state.PhaseStopped), nil)
	deleted := f.createAgent(t, "deleted-paged", string(state.PhaseStopped), nil)
	softDeleteAgentForTest(t, f.store, deleted.ID)

	// fit=1 < n=2 forces paged mode.
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=1&limit=1&includeDeleted=true"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete)
	assert.Equal(t, 2, resp.TotalCount, "totalCount must count the soft-deleted agent too, matching legacy semantics")

	seen := map[string]bool{}
	for _, a := range resp.Agents {
		seen[a.ID] = true
	}
	require.NotEmpty(t, resp.NextCursor)
	rec2 := doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&limit=1&includeDeleted=true&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	resp2 := mustDecodeListAgentsResponse(t, rec2.Body)
	for _, a := range resp2.Agents {
		seen[a.ID] = true
	}
	assert.True(t, seen[live.ID])
	assert.True(t, seen[deleted.ID], "includeDeleted=true must keep the soft-deleted agent reachable across pages too")
}

// --- B6 (c): the OwnerID-change race --------------------------------------

// ownerChangingAfterMembersStore mutates an agent's OwnerID (via the real
// store) the first time ListAgentMembers is called, simulating a write
// landing between the member read and the full-row read.
type ownerChangingAfterMembersStore struct {
	store.Store
	once       sync.Once
	agentID    string
	newOwnerID string
}

func (o *ownerChangingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := o.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil {
		return nil, err
	}
	o.once.Do(func() {
		a, gerr := o.Store.GetAgent(ctx, o.agentID)
		if gerr != nil {
			return
		}
		a.OwnerID = o.newOwnerID
		_ = o.Store.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_Race_OwnerChange_BecomesUnreadable is B6(c):
// a candidate owned by the caller (readable only via the owner relationship
// grant, not via any role permission) whose OwnerID changes away from the
// caller between the two reads must be re-decided on the full row and
// dropped, in exactly 9 decisions for that item (1 step-3 read + 8 step-5a
// full re-decision + 0 step-6, since it was re-decided).
func TestListProjectAgentsSorted_Race_OwnerChange_BecomesUnreadable(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()

	caller := &store.User{
		ID: tid("sl-ownerrace-caller"), Email: "sl-ownerrace@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, caller))
	ensureHubMembership(ctx, f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-ownerrace-role")

	a := &store.Agent{
		ID: tid("sl-ownerrace-agent"), Slug: "ownerrace-agent", Name: "ownerrace-agent",
		ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
		CreatedBy: caller.ID, OwnerID: caller.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, a))

	raced := &ownerChangingAfterMembersStore{Store: f.store, agentID: a.ID, newOwnerID: f.owner.ID}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents, "an item whose owner changed away from the caller mid-request must be dropped")

	// 1 (gate) + 1 (step-3 read, still owned by caller at that snapshot) +
	// 8 (step-5a full re-decision, now unreadable) + 0 (step-6 skip) +
	// 4 (scope caps) = 14.
	assert.Len(t, emitter.records, 14)
}

// --- B6 (d): exact decision counts for missing-row / project-drop ---------

// TestListProjectAgentsSorted_Race_MissingRow_ExactDecisionCount extends the
// existing missing-row race test with the exact decision count the design
// requires ("no additional decision"): 1 (gate) + 1 (step-3 read) + 0 (step
// 5a drop) + 4 (scope caps) = 6.
func TestListProjectAgentsSorted_Race_MissingRow_ExactDecisionCount(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-missing-count", string(state.PhaseStopped), nil)

	raced := &deletingAfterMembersStore{Store: f.store, agentID: a.ID}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents)
	assert.Len(t, emitter.records, 6, "a missing row must cost exactly the step-3 read, no more")
}

// TestListProjectAgentsSorted_Race_ProjectMismatch_ExactDecisionCount is the
// NB-3 analogue of the above: 1 (gate) + 1 (step-3 read) + 0 (step 5a drop,
// ProjectID check) + 4 (scope caps) = 6.
func TestListProjectAgentsSorted_Race_ProjectMismatch_ExactDecisionCount(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-project-count", string(state.PhaseStopped), nil)

	raced := &reprojectingListAgentsStore{Store: f.store, agentID: a.ID, newProjectID: tid("sl-other-project-count")}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents)
	assert.Len(t, emitter.records, 6, "a project-mismatched row must cost exactly the step-3 read, no more")
}

// --- B6 (e): missing-row drop in paged mode must leave a valid short page ---

// TestListProjectAgentsSorted_Race_MissingRow_PagedShortPageContinues is
// B6(e): a row dropped as "missing" inside a page must produce a short page
// whose nextCursor still points past the dropped row's member position, so
// the next page picks up where the walk actually left off rather than
// skipping or re-serving anything.
func TestListProjectAgentsSorted_Race_MissingRow_PagedShortPageContinues(t *testing.T) {
	f := sortedListSetup(t)
	// Created oldest-to-newest, so under dir=desc the order is c2, c1, c0.
	c0 := f.createAgent(t, "short-c0", string(state.PhaseStopped), nil)
	c1 := f.createAgent(t, "short-c1", string(state.PhaseStopped), nil)
	c2 := f.createAgent(t, "short-c2", string(state.PhaseStopped), nil)

	// limit=2: page 0 is [c2, c1]. Drop c1 (the last item of page 0) at the
	// step-5a boundary.
	raced := &deletingAfterMembersStore{Store: f.store, agentID: c1.ID}
	f.srv.store = raced

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&limit=2"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, page0.Agents, 1, "the short page must contain only c2; c1 was dropped as missing")
	assert.Equal(t, c2.ID, page0.Agents[0].ID)
	require.NotEmpty(t, page0.NextCursor, "a short page must still carry a cursor past the dropped (examined) item")

	rec2 := doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&limit=2&cursor="+url.QueryEscape(page0.NextCursor)), nil)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	page1 := mustDecodeListAgentsResponse(t, rec2.Body)
	require.Len(t, page1.Agents, 1, "the walk must continue from c1's examined position, landing on c0 next")
	assert.Equal(t, c0.ID, page1.Agents[0].ID)
}

// --- B6 (f): nil-vs-empty Labels/Ancestry costs zero re-decisions, end to end ---

// labelsNilToEmptyAfterMembersStore rewrites an agent's Labels from nil to a
// non-nil empty map (via the real store) after the first ListAgentMembers
// call, simulating the narrow and full decoders disagreeing on "no labels"
// representation (r8 NB-2) within one request.
type labelsNilToEmptyAfterMembersStore struct {
	store.Store
	once    sync.Once
	agentID string
}

func (l *labelsNilToEmptyAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := l.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil {
		return nil, err
	}
	l.once.Do(func() {
		a, gerr := l.Store.GetAgent(ctx, l.agentID)
		if gerr != nil {
			return
		}
		a.Labels = map[string]string{}
		_ = l.Store.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_NilVsEmptyLabels_EndToEndZeroRedecisions is
// B6(f): the nil/empty-Labels normalization must cost zero re-decisions
// end to end through the real handler, not just at the resourceEqual unit
// level -- exactly 5+8n (13 at n=1), never 5+9n (14).
func TestListProjectAgentsSorted_NilVsEmptyLabels_EndToEndZeroRedecisions(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "nilempty-e2e", string(state.PhaseStopped), nil) // Labels left nil

	raced := &labelsNilToEmptyAfterMembersStore{Store: f.store, agentID: a.ID}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the item must be kept: nil vs empty Labels must not look like a project/filter mismatch either")

	assert.Len(t, emitter.records, 13, "nil-to-empty Labels must cost zero re-decisions: 5+8*1, not 5+9*1")
}

// fieldMutatingAfterMembersStore generalizes labelsNilToEmptyAfterMembersStore
// (and mutatingAfterMembersStore) to any single-field mutation applied after
// the first ListAgentMembers call: r2 review N-4 found the original
// end-to-end test exercised only Labels nil->empty, when
// normalizeResourceForCompare normalizes both Labels and Ancestry, in both
// directions.
type fieldMutatingAfterMembersStore struct {
	store.Store
	once    sync.Once
	agentID string
	mutate  func(a *store.Agent)
}

func (f *fieldMutatingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	members, err := f.Store.ListAgentMembers(ctx, filter, sortKey, dir, max)
	if err != nil {
		return nil, err
	}
	f.once.Do(func() {
		a, gerr := f.Store.GetAgent(ctx, f.agentID)
		if gerr != nil {
			return
		}
		f.mutate(a)
		_ = f.Store.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_NilVsEmpty_TableDriven_EndToEndZeroRedecisions
// is r2 review N-4: the nil/empty normalization end-to-end proof, extended
// to both fields normalizeResourceForCompare touches (Labels, Ancestry) and
// both directions (nil->empty and empty->nil), not just Labels nil->empty.
// Every case must cost exactly 5+8*1=13 decisions, never 5+9*1=14 -- a
// re-decision would mean the normalization missed this field or direction.
func TestListProjectAgentsSorted_NilVsEmpty_TableDriven_EndToEndZeroRedecisions(t *testing.T) {
	cases := []struct {
		name    string
		initial func(a *store.Agent)
		mutate  func(a *store.Agent)
	}{
		{"Labels_nil_to_empty", func(a *store.Agent) { a.Labels = nil }, func(a *store.Agent) { a.Labels = map[string]string{} }},
		{"Labels_empty_to_nil", func(a *store.Agent) { a.Labels = map[string]string{} }, func(a *store.Agent) { a.Labels = nil }},
		{"Ancestry_nil_to_empty", func(a *store.Agent) { a.Ancestry = nil }, func(a *store.Agent) { a.Ancestry = []string{} }},
		{"Ancestry_empty_to_nil", func(a *store.Agent) { a.Ancestry = []string{} }, func(a *store.Agent) { a.Ancestry = nil }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := sortedListSetup(t)
			ctx := context.Background()

			a := &store.Agent{
				ID: tid("sl-nilempty-" + tc.name), Slug: "nilempty-" + tc.name, Name: "nilempty-" + tc.name,
				ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
				CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
			}
			require.NoError(t, f.store.CreateAgent(ctx, a))
			// Pin the exact "before" shape via an explicit UpdateAgent round
			// trip, rather than trusting CreateAgent's own default
			// normalization of a nil/empty field -- this is what the first
			// (pre-race) ListAgentMembers call will see.
			tc.initial(a)
			require.NoError(t, f.store.UpdateAgent(ctx, a))

			raced := &fieldMutatingAfterMembersStore{Store: f.store, agentID: a.ID, mutate: tc.mutate}
			f.srv.store = raced

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)
			require.Len(t, resp.Agents, 1, "the item must be kept: %s must not look like a project/filter mismatch", tc.name)

			assert.Len(t, emitter.records, 13, "%s must cost zero re-decisions: 5+8*1=13, never 5+9*1=14", tc.name)
		})
	}
}

// --- B7: S3/A3 test-plan gaps ----------------------------------------------

// TestListProjectAgentsSorted_CursorLabelReplayRejected is B7: a cursor
// minted under one label filter is rejected when replayed under another
// (design 4.4: "as does a label replay"), mirroring the existing phase-replay
// test.
func TestListProjectAgentsSorted_CursorLabelReplayRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("labelreplay-%d", i), string(state.PhaseStopped), map[string]string{"team": "a"})
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1&label=team=a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&label=team=b&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Also: the same cursor with no label at all must be rejected too.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_CursorCrossProjectRejected is B7: a cursor
// minted on project A is rejected when replayed against project B, since
// the binding's endpoint string includes the project ID (design 4.4).
func TestListProjectAgentsSorted_CursorCrossProjectRejected(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("crossproj-%d", i), string(state.PhaseStopped), nil)
	}

	other := &store.Project{
		ID: tid("sl-crossproj-other"), Name: "Other", Slug: "sl-crossproj-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, other))
	f.srv.createProjectMembersGroup(ctx, other)
	createTestUserWithProjectRole(t, f.store, f.owner.ID, f.owner.Email, other.ID, store.ProjectRoleOwner)
	// At least one agent in the other project so the request is otherwise valid.
	otherAgent := &store.Agent{
		ID: tid("sl-crossproj-agent"), Slug: "crossproj-agent", Name: "crossproj-agent",
		ProjectID: other.ID, Phase: string(state.PhaseStopped), CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	otherPath := "/api/v1/projects/" + other.ID + "/agents?sort=updated&dir=desc&limit=1&cursor=" + url.QueryEscape(resp.NextCursor)
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, otherPath, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_TamperedCursorRejected is B7: a tampered v2
// cursor byte is rejected at the hub (HTTP) level, not just the store codec
// level.
func TestListProjectAgentsSorted_TamperedCursorRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("tamper-%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	tampered := "X" + resp.NextCursor[1:]
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&cursor="+url.QueryEscape(tampered)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_MalformedCursor_NoSQLBeforeRejection is B7:
// a malformed v2 cursor must be rejected before any COUNT, member read or
// decision -- the store and authz spies record nothing beyond (in this
// case, not even) the agent.list gate.
func TestListProjectAgentsSorted_MalformedCursor_NoSQLBeforeRejection(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "malformed-precheck", string(state.PhaseStopped), nil)

	counting := &countingAgentStore{Store: f.store}
	f.srv.store = counting

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&cursor=not-a-valid-v2-cursor"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	assert.Equal(t, 0, counting.countAgentsCalls, "no COUNT before the cursor is validated")
	assert.Equal(t, 0, counting.membersCalls, "no member read before the cursor is validated")
	assert.Len(t, emitter.records, 1, "only the agent.list gate decision runs before cursor validation")
}

// --- N2: S10, n = fit+1 with R <= fit (project user path, R3-B1) ----------

// TestListProjectAgentsSorted_Fit_IncompleteEvenWhenReadableAtOrBelowFit is
// N2: completeness is decided on the candidate count n, not the readable
// count R (design Q-G) -- even when every readable agent would fit, a
// candidate pool above fit must still page.
func TestListProjectAgentsSorted_Fit_IncompleteEvenWhenReadableAtOrBelowFit(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()

	caller := &store.User{
		ID: tid("sl-n2-caller"), Email: "sl-n2@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, caller))
	ensureHubMembership(ctx, f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-n2-role")

	const n, fit = 6, 5 // R = 5 (caller owns 5), n = 6 > fit
	f.createAgentsBulk(t, n, "n2", string(state.PhaseStopped), func(i int) string {
		if i < fit {
			return caller.ID
		}
		return f.owner.ID
	})

	rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", fit, fit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete, "n=6 > fit=5 must page even though R=5 <= fit")
	// The paged response still serves exactly the readable set here (R=5
	// fits in one page of limit=5), so no cursor is needed -- the design
	// point is complete=false despite R<=fit, not that a cursor must exist.
	assert.Equal(t, fit, resp.TotalCount)
	assert.Len(t, resp.Agents, fit)
}

// --- N6/E1: legacy mode ignores fit/stats/dir, byte-identical apart from serverTime ---

// serverTimeJSONRe matches the "serverTime":"..." field in a
// ListAgentsResponse's JSON encoding, so rawBodyWithoutServerTime can blank
// it out for an exact byte comparison of everything else (r2 review N-3:
// "compare ... raw bytes after stripping serverTime").
var serverTimeJSONRe = regexp.MustCompile(`"serverTime":"[^"]*"`)

// rawBodyWithoutServerTime returns rec's raw response body with the
// serverTime value blanked out, for an exact byte-for-byte comparison
// against another response (r2 review N-3).
func rawBodyWithoutServerTime(rec *httptest.ResponseRecorder) []byte {
	return serverTimeJSONRe.ReplaceAll(rec.Body.Bytes(), []byte(`"serverTime":""`))
}

// TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical is N6/E1 (EM
// ruling, architect-confirmed r8 erratum): without "sort", fit/stats/dir are
// silently ignored and the response is byte-identical to the same request
// without them, apart from serverTime. Extended per r2 review N-3, which
// found the original version under-scoped against errata E1's own test
// description: only the project endpoint, only valid values, no cursor in
// play, and a decoded-map compare rather than raw bytes. This version adds
// invalid values (dir=sideways, fit=0, and others), a cursor already in
// play (so the emitted nextCursor/binding is part of what must match), raw
// byte comparison, and the global endpoint (whose legacy path P1b leaves
// unchanged, so cheap to add).
func TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "n6-a", string(state.PhaseRunning), nil)
	f.createAgent(t, "n6-b", string(state.PhaseStopped), nil)
	// A third agent (r3 review nit-2): with only 2 agents, the
	// project_endpoint_cursor_present sub-case's page-2 responses (the ones
	// actually compared) were always the *last* page, so neither ever had a
	// nextCursor, even though its comment claimed the emitted
	// nextCursor/binding was part of the byte comparison. A 3rd agent makes
	// page 2 non-terminal, so it carries a real nextCursor too.
	f.createAgent(t, "n6-c", string(state.PhaseStopped), nil)

	// Valid values (the original test's case), plus the invalid shapes the
	// reviewer's probe used (dir=sideways, fit=0) plus two more unparsable
	// ones -- all of these would be 400s in sorted mode (design 4.1), and
	// must instead be silently ignored here, exactly like the valid case.
	extras := []string{
		"fit=500&stats=1&dir=asc",
		"dir=sideways",
		"fit=0",
		"fit=abc&stats=yes",
	}

	t.Run("project_endpoint_no_cursor", func(t *testing.T) {
		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(""), nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(extra), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"project endpoint: fit/stats/dir (%s) without sort must be silently ignored, byte-identical apart from serverTime", extra)
			})
		}
	})

	t.Run("project_endpoint_cursor_present", func(t *testing.T) {
		// limit=1 over 3 agents: page 1 carries a cursor into page 2, and
		// page 2 (the one actually compared below) is *not* the last page
		// either, so it carries its own nextCursor too (r3 review nit-2).
		page1 := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1"), nil)
		require.Equal(t, http.StatusOK, page1.Code, page1.Body.String())
		resp1 := mustDecodeListAgentsResponse(t, page1.Body)
		require.NotEmpty(t, resp1.NextCursor, "need a cursor in play for this sub-case")
		cursorQS := "cursor=" + url.QueryEscape(resp1.NextCursor)

		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1&"+cursorQS), nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseResp := mustDecodeListAgentsResponse(t, base.Body)
		require.NotEmpty(t, baseResp.NextCursor,
			"page 2 must itself emit a nextCursor, or the byte comparison below never actually exercises one (r3 review nit-2)")
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("limit=1&"+cursorQS+"&"+extra), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"project endpoint with a cursor already in play: fit/stats/dir (%s) must still be ignored -- including the emitted nextCursor/binding, which is part of this byte comparison", extra)
			})
		}
	})

	t.Run("global_endpoint", func(t *testing.T) {
		globalBase := "/api/v1/agents?projectId=" + f.project.ID
		base := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, globalBase, nil)
		require.Equal(t, http.StatusOK, base.Code, base.Body.String())
		baseBody := rawBodyWithoutServerTime(base)

		for _, extra := range extras {
			t.Run(extra, func(t *testing.T) {
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, globalBase+"&"+extra, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, string(baseBody), string(rawBodyWithoutServerTime(rec)),
					"global endpoint: fit/stats/dir (%s) must be ignored too -- P1b does not touch this endpoint's legacy path", extra)
			})
		}
	})
}
