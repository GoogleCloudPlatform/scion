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
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the complete-branch decision budget on the sorted
// project list: the complete branch costs 5 + n*(1+perRow) decisions, so a
// fit request whose candidate count is above completeBranchMaxCandidates
// is served by the paged branch, keeping every branch within
// sortedProjectDecisionCeiling.

// TestCompleteBranchMaxCandidates_StaysWithinPageCeiling checks, for both
// per-row costs (the default and the higher per-row cost), that the
// complete branch at the boundary n stays within the decision ceiling and
// one more candidate would not. Both sides are asserted, so an over-strict
// threshold is caught too.
func TestCompleteBranchMaxCandidates_StaysWithinPageCeiling(t *testing.T) {
	cases := []struct {
		name   string
		perRow int
	}{
		{"default per-row cost", pageRowDecisions},
		{"higher per-row cost", scopedPageRowDecisions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := completeBranchMaxCandidates(tc.perRow)
			assert.LessOrEqual(t, 5+n*(1+tc.perRow), sortedProjectDecisionCeiling,
				"the complete branch at the boundary stays within the decision ceiling")
			assert.Greater(t, 5+(n+1)*(1+tc.perRow), sortedProjectDecisionCeiling,
				"one candidate past the boundary would exceed the decision ceiling")
		})
	}
	// The largest legal fit is 500, so at the default per-row cost the
	// complete branch is never narrowed by the threshold.
	assert.GreaterOrEqual(t, completeBranchMaxCandidates(pageRowDecisions), 500)
}

// The page size stays positive at the candidate cap for both row costs.
func TestEffectivePagedPageSize_PositiveAtCandidateCap(t *testing.T) {
	assert.GreaterOrEqual(t, effectivePagedPageSize(1, authorizedListMaxCandidates, scopedPageRowDecisions), 1)
	assert.GreaterOrEqual(t, effectivePagedPageSize(1, authorizedListMaxCandidates, pageRowDecisions), 1)
}

// TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_AtBoundary_StaysComplete
// checks that a caller with the higher per-row cost at exactly the boundary
// candidate count still gets the complete response, within the decision
// ceiling.
func TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_AtBoundary_StaysComplete(t *testing.T) {
	n := completeBranchMaxCandidates(scopedPageRowDecisions)

	f := sortedListSetup(t)
	f.createAgentsBulk(t, n, "cbhighcost-at", string(state.PhaseStopped), nil)
	key := mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:list"})

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)

	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete, "at the boundary the complete branch is still used")
	assert.Len(t, resp.Agents, n)
	assert.Equal(t, n, resp.TotalCount)
	assert.Empty(t, resp.NextCursor)

	want := 5 + n*9
	assert.Len(t, emitter.records, want)
	assert.LessOrEqual(t, len(emitter.records), sortedProjectDecisionCeiling)
}

// TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_OverBoundary_GoesPaged
// checks that a caller with the higher per-row cost, one candidate past the
// boundary, and at n=500, gets a paged response within the decision
// ceiling, and that the paged walk still returns every listed agent.
func TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_OverBoundary_GoesPaged(t *testing.T) {
	onePast := completeBranchMaxCandidates(scopedPageRowDecisions) + 1
	cases := []struct {
		name     string
		n        int
		wantPEff int
	}{
		// The paged branch serves floor((ceiling-5-n)/perRow) rows.
		{"one candidate past the boundary", onePast, (sortedProjectDecisionCeiling - 5 - onePast) / 8},
		{"n=500", 500, 437}, // floor((4000-500)/8) = 437
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Greater(t, tc.n, completeBranchMaxCandidates(scopedPageRowDecisions))

			f := sortedListSetup(t)
			f.createAgentsBulk(t, tc.n, fmt.Sprintf("cbhighcost-%d", tc.n), string(state.PhaseStopped), nil)
			key := mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:list"})

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500"), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)

			require.NotNil(t, resp.Complete)
			assert.False(t, *resp.Complete, "past the boundary the paged branch is used")
			assert.Len(t, resp.Agents, tc.wantPEff)
			assert.Equal(t, tc.n, resp.TotalCount, "totalCount is the whole listed count")
			require.NotEmpty(t, resp.NextCursor, "the rest of the set is reachable by cursor")

			want := 5 + tc.n + 8*tc.wantPEff
			assert.Len(t, emitter.records, want)
			assert.LessOrEqual(t, len(emitter.records), sortedProjectDecisionCeiling)

			// The continuation page (cursor, no fit) returns the rest.
			seen := make(map[string]bool, tc.n)
			for _, a := range resp.Agents {
				seen[a.ID] = true
			}
			rec2 := doRequestWithUAT(t, f.srv, key, http.MethodGet,
				f.listPath("sort=updated&limit=500&cursor="+resp.NextCursor), nil)
			require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
			resp2 := mustDecodeListAgentsResponse(t, rec2.Body)
			assert.Nil(t, resp2.Complete)
			assert.Empty(t, resp2.NextCursor)
			assert.Equal(t, tc.n, resp2.TotalCount)
			for _, a := range resp2.Agents {
				assert.False(t, seen[a.ID], "agent %s returned twice", a.ID)
				seen[a.ID] = true
			}
			assert.Len(t, seen, tc.n, "the two pages together hold every listed agent")
		})
	}
}

// TestListProjectAgentsSorted_CompleteBudget_NormalAt500_Unchanged checks
// that a caller with the default per-row cost at n=500 with fit=500 still
// gets the complete response, at exactly the decision ceiling.
func TestListProjectAgentsSorted_CompleteBudget_NormalAt500_Unchanged(t *testing.T) {
	const n = 500
	f := sortedListSetup(t)
	f.createAgentsBulk(t, n, "cbnormal", string(state.PhaseStopped), nil)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)

	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
	assert.Len(t, resp.Agents, n)
	assert.Equal(t, n, resp.TotalCount)
	assert.Empty(t, resp.NextCursor)

	const want = 5 + n*8 // 4,005
	assert.Len(t, emitter.records, want)
	assert.LessOrEqual(t, len(emitter.records), sortedProjectDecisionCeiling)
}

// TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_FallbackAppliesPhase
// checks that a fit request served through the paged branch because of
// the complete-branch budget applies the request's phase filter, as any
// paged response does: only the matching agents are returned, totalCount
// is the matching count, and the decision cost covers only those rows.
func TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_FallbackAppliesPhase(t *testing.T) {
	n := completeBranchMaxCandidates(scopedPageRowDecisions) + 1
	const running = 10

	f := sortedListSetup(t)
	f.createAgentsBulk(t, running, "cbphase-run", string(state.PhaseRunning), nil)
	f.createAgentsBulk(t, n-running, "cbphase-stop", string(state.PhaseStopped), nil)
	key := mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:list"})

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500&phase=running"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)

	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete, "past the boundary the paged branch is used")
	assert.Len(t, resp.Agents, running)
	for _, a := range resp.Agents {
		assert.Equal(t, string(state.PhaseRunning), a.Phase, "%s: the phase filter applies to the paged response", a.ID)
	}
	assert.Equal(t, running, resp.TotalCount, "totalCount is the phase-filtered count")
	assert.Empty(t, resp.NextCursor)

	want := 5 + n + 8*running
	assert.Len(t, emitter.records, want)
	assert.LessOrEqual(t, len(emitter.records), sortedProjectDecisionCeiling)
}

// TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_RacedWorstCase_WithinRaceAllowance
// checks, at the higher per-row cost, that a request whose every row
// changes between the member read and the full-row read stays within the
// race allowance (the decision ceiling plus one decision per row, for at
// most maxSortedLimit rows) at the worst case of each branch. A raced row
// costs its plain read, the 7 remaining actions and one list read, which is
// 1+perRow decisions; read is not re-decided.
//
// Complete branch: the cost with every row raced is 5 + n*(2+perRow), which
// grows with n, so the worst case is the largest n the complete branch
// admits, completeBranchMaxCandidates(perRow).
//
// Paged branch: past that n, the cost with every row raced is
// 5 + n + (1+perRow)*pEff(n), with pEff(n) = floor((ceiling-5-n)/perRow).
// Each step up in n adds 1, and pEff drops by one every perRow steps,
// removing 1+perRow, so the cost rises while pEff holds and falls when it
// drops. The worst case is therefore the largest n that keeps pEff at its
// value for the first paged n: n = ceiling-5 - perRow*pEff(first paged n).
// The test also checks both choices against every n from 1 up to the
// candidate ceiling. Wherever the complete branch applies, it takes the
// larger of the two branch costs.
func TestListProjectAgentsSorted_CompleteBudget_HigherRowCost_RacedWorstCase_WithinRaceAllowance(t *testing.T) {
	perRow := scopedPageRowDecisions
	raceAllowance := sortedProjectDecisionCeiling + maxSortedLimit
	completeMax := completeBranchMaxCandidates(perRow)

	pagedCost := func(n int) int {
		rows := effectivePagedPageSize(maxSortedLimit, n, perRow)
		if rows > n {
			rows = n
		}
		return 5 + n + (1+perRow)*rows
	}
	completeCost := func(n int) int {
		return 5 + n*(2+perRow)
	}
	firstPagedRows := effectivePagedPageSize(maxSortedLimit, completeMax+1, perRow)
	pagedWorst := sortedProjectDecisionCeiling - 5 - perRow*firstPagedRows
	require.Greater(t, pagedWorst, completeMax)
	worst := max(completeCost(completeMax), pagedCost(pagedWorst))
	for n := 1; n <= authorizedListMaxCandidates; n++ {
		cost := pagedCost(n)
		if n <= completeMax {
			cost = max(cost, completeCost(n))
		} else {
			require.LessOrEqual(t, cost, pagedCost(pagedWorst), "the paged worst case is the largest paged raced cost")
		}
		require.LessOrEqual(t, cost, worst, "the two worst cases bound every raced cost")
	}
	require.LessOrEqual(t, worst, raceAllowance, "the raced worst case fits the race allowance")

	cases := []struct {
		name     string
		n        int
		complete bool
		rows     int
	}{
		{"complete branch", completeMax, true, completeMax},
		{"paged branch", pagedWorst, false, firstPagedRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sortedListSetup(t)
			f.createAgentsBulk(t, tc.n, "cbraced", string(state.PhaseStopped), nil)
			key := mintScopedUAT(t, f.srv, f.owner.ID, f.project.ID, []string{"agent:list"})
			f.srv.store = &racingAllMembersStore{Store: f.store}

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500"), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)

			require.NotNil(t, resp.Complete)
			assert.Equal(t, tc.complete, *resp.Complete)
			require.Len(t, resp.Agents, tc.rows, "every raced row stays listed")
			for _, a := range resp.Agents {
				assert.Equal(t, "true", a.Labels["raced"], "%s: the row changed after the member read", a.ID)
				require.NotNil(t, a.Cap)
				assert.NotContains(t, a.Cap.Actions, string(ActionRead), "%s: no read capability at the higher per-row cost", a.ID)
			}

			want := 5 + tc.n + (1+perRow)*tc.rows
			assert.Len(t, emitter.records, want, "a raced row costs 1+perRow decisions")
			assert.LessOrEqual(t, len(emitter.records), raceAllowance, "the raced request stays within the race allowance")
		})
	}
}
