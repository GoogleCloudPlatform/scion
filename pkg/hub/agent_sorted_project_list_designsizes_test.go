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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file runs the decision-count and candidate-ceiling gates at the
// design's own test-plan sizes (n in 25, 100, 500, 501, 1200; real
// 2000/2001-row ceiling rows). Fixtures use store.Store.WithTx (one
// transaction for the whole batch)
// rather than one CreateAgent call per row: a 1200-row bulk insert this way
// takes well under a second in this sandbox, which is what makes these
// sizes practical to run as unit tests at all — the smaller sizes used
// elsewhere in this package were a overcautious reaction to this repo's
// cold-build compile time, not to real per-row insert cost.

// TestListProjectAgentsSorted_DecisionCounts_DesignSizes is the non-waivable
// decision-count hard gate at the design's own n values, all-readable (R=n).
// n <= 500 can be a complete fit response (fit's valid range is 1..500,
// design 4.1); n > 500 cannot, so those two sizes exercise the paged formula
// instead, with limit=25 to match the design's own illustrative P (section
// 6.4: "n + 180" is 7P with P=25).
func TestListProjectAgentsSorted_DecisionCounts_DesignSizes(t *testing.T) {
	sizes := []int{25, 100, 500, 501, 1200}
	for _, n := range sizes {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			f := sortedListSetup(t)
			f.createAgentsBulk(t, n, "sz", string(state.PhaseStopped), nil) // nil ownerFor: every agent owned by f.owner, so R=n

			emitter := &recordingDecisionAuditEmitter{}
			f.srv.authzService.SetDecisionAuditEmitter(emitter)

			var query string
			var want int
			if n <= 500 {
				// fit must be >= limit (design 4.1); use limit=n too so a
				// small fit (e.g. 25) isn't rejected against the default
				// limit of 500.
				query = fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)
				want = 5 + 8*n // design 6.4: "5 + 8n", equal to today's 205/805/4005 at 25/100/500 when every candidate is readable
			} else {
				const limit = 25
				// fit=500 (the max allowed) is still < n here, so the
				// response is paged regardless -- matching the real
				// client's first request, which always sends fit (4.3).
				query = fmt.Sprintf("sort=updated&fit=500&limit=%d", limit)
				want = 5 + n + 7*limit // design 6.4/5.3: "5 + n + 7P"
			}

			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(query), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			resp := mustDecodeListAgentsResponse(t, rec.Body)
			if n <= 500 {
				require.NotNil(t, resp.Complete)
				assert.True(t, *resp.Complete)
				assert.Len(t, resp.Agents, n)
			} else {
				require.NotNil(t, resp.Complete)
				assert.False(t, *resp.Complete)
			}
			assert.Len(t, emitter.records, want, "n=%d decision count", n)
		})
	}
}

// grantProjectListOnly binds userID to a project-scoped role carrying only
// "agent.list" — no "agent.read" — so the project's agent.list gate passes
// but no agent is readable except through a resource-level relationship
// grant (ownership), independent of any role permission (pkg/hub/
// authz_relationship_rules.go; confirmed unconditional-of-role-bindings by
// TestAuthz_OwnerBypass, pkg/hub/authz_test.go). This is how a single
// caller, one project, one role, reads exactly its owned subset of agents —
// the project endpoint's agent.read is otherwise all-or-nothing per
// (principal, project) via role bindings and has no other per-resource
// visibility narrowing (design 5.3's R < n note).
func grantProjectListOnly(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	rd := createTestRoleDefinition(t, s, roleName, store.RoleScopeProject, []string{"agent.list"})
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// TestListProjectAgentsSorted_DecisionCounts_PartialRead_Paged is the
// design's explicit decision-count sub-case: n=1200, R=400 (paged), exactly
// 1380 decisions (design 9: "5 + 8n" withdrawn in favor of "n = 1,200 with
// R = 400 (paged, 1,380, not complete)").
func TestListProjectAgentsSorted_DecisionCounts_PartialRead_Paged(t *testing.T) {
	f := sortedListSetup(t)

	caller := &store.User{
		ID: tid("sl-partial-caller-1200"), Email: "sl-partial-1200@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(context.Background(), caller))
	ensureHubMembership(context.Background(), f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-list-only-1200")

	const n, r = 1200, 400
	agents := f.createAgentsBulk(t, n, "pr1200", string(state.PhaseStopped), func(i int) string {
		if i < r {
			return caller.ID // first r agents are owned by caller -> readable via the owner grant
		}
		return f.owner.ID // the rest are owned by someone else -> unreadable to caller
	})

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	const limit = 25
	rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=500&limit=%d&stats=1", limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete, "n=1200 > fit's 500 max, so this can only ever be paged")
	assert.Equal(t, r, resp.TotalCount, "totalCount is the readable (phase-filtered) count, independent of page size")
	assertStatsWithinReadable(t, resp, agents[:r])

	// 5 (gate+scope caps) + n (step-3 read pass over every candidate) +
	// 7*limit (remaining-action pass for the page only) = 5 + 1200 + 175 = 1380.
	assert.Len(t, emitter.records, 1380)
}

// TestListProjectAgentsSorted_DecisionCounts_PartialRead_Complete is the
// design's other explicit decision-count sub-case: n=500, R=200 (complete),
// exactly 5 + 500 + 7*200 = 1905 decisions (design 9).
func TestListProjectAgentsSorted_DecisionCounts_PartialRead_Complete(t *testing.T) {
	f := sortedListSetup(t)

	caller := &store.User{
		ID: tid("sl-partial-caller-500"), Email: "sl-partial-500@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(context.Background(), caller))
	ensureHubMembership(context.Background(), f.store, caller.ID)
	grantProjectListOnly(t, f.store, caller.ID, f.project.ID, "sl-list-only-500")

	const n, r = 500, 200
	agents := f.createAgentsBulk(t, n, "pr500", string(state.PhaseStopped), func(i int) string {
		if i < r {
			return caller.ID
		}
		return f.owner.ID
	})

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, caller, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&stats=1", n)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete, "completeness is decided on the candidate count n, not R (design 5.3 step 2)")
	assert.Len(t, resp.Agents, r, "a complete response's page is the whole readable set")
	assert.Equal(t, r, resp.TotalCount)
	assertStatsWithinReadable(t, resp, agents[:r])

	// 5 (gate+scope caps) + n (step-3 read pass over every candidate) +
	// 7*r (remaining-action pass over every readable item) = 5 + 500 + 1400 = 1905.
	assert.Len(t, emitter.records, 1905)
}

// --- candidate ceiling: real rows at the ceiling (design hard gate) --------

// TestListProjectAgentsSorted_CandidateCeiling_RealRows uses a real
// 2001-row candidate pool (no store decorator): the ceiling trips on the
// genuine CountAgents/ListAgentMembers path, costing exactly the agent.list
// gate decision.
func TestListProjectAgentsSorted_CandidateCeiling_RealRows(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgentsBulk(t, authorizedListMaxCandidates+1, "ceil-real", string(state.PhaseStopped), nil)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, errCodeSortedViewUnavailable, body.Error.Code)
	assert.Len(t, emitter.records, 1, "exactly the agent.list gate decision")
}

// TestListProjectAgentsSorted_UnderCeiling_RealRowsAtCeiling is the
// candidate-ceiling gate's complement with a real, exactly-at-the-ceiling
// 2000-row pool: the request must succeed with exact totals, never refused.
func TestListProjectAgentsSorted_UnderCeiling_RealRowsAtCeiling(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgentsBulk(t, authorizedListMaxCandidates, "ceil-ok", string(state.PhaseStopped), nil)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&limit=1&stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Equal(t, authorizedListMaxCandidates, resp.TotalCount)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, authorizedListMaxCandidates, resp.Stats.Total)
}

// TestListProjectAgentsSorted_CandidateCeiling_LabelNarrowsBelowCeiling is
// the direct proof that the ceiling COUNT runs on the label-filtered
// candidate set, not the raw per-project row count. A project with 2,001
// agents, only 5 of which match the request's label filter, must succeed.
func TestListProjectAgentsSorted_CandidateCeiling_LabelNarrowsBelowCeiling(t *testing.T) {
	f := sortedListSetup(t)
	const total = authorizedListMaxCandidates + 1
	const keep = 5
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < total; i++ {
			labels := map[string]string{"team": "other"}
			if i < keep {
				labels = map[string]string{"team": "keep"}
			}
			a := &store.Agent{
				ID: tid(fmt.Sprintf("sl-labelnarrow-%d", i)), Slug: fmt.Sprintf("labelnarrow-%d", i), Name: fmt.Sprintf("labelnarrow-%d", i),
				ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
				CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Labels: labels,
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&label=team=keep"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Equal(t, keep, resp.TotalCount)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
}

// TestListProjectAgentsSorted_LegacyUnaffectedAbove2001 proves that a legacy
// (no sort) request on a project with more than the sorted-mode ceiling's
// worth of agents is unaffected -- it just truncates to 500 as it always
// has, with no 422.
func TestListProjectAgentsSorted_LegacyUnaffectedAbove2001(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgentsBulk(t, authorizedListMaxCandidates+1, "legacy-unaffected", string(state.PhaseStopped), nil)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, resp.Agents, 500)
	assert.NotEmpty(t, resp.NextCursor)
}

// assertStatsWithinReadable checks that a project user response's stats
// block counts and lists exactly the caller-readable agents: stats.total is
// len(readable) and every stats.agents id is one of them. stats=1 adds no
// decisions, so the exact decision counts in the callers still hold.
func assertStatsWithinReadable(t *testing.T, resp ListAgentsResponse, readable []*store.Agent) {
	t.Helper()
	ids := make(map[string]bool, len(readable))
	for _, a := range readable {
		ids[a.ID] = true
	}
	require.NotNil(t, resp.Stats)
	require.NotNil(t, resp.Stats.Agents)
	assert.Equal(t, len(readable), resp.Stats.Total, "stats.total must be the readable count")
	assert.Len(t, *resp.Stats.Agents, len(readable))
	for _, ip := range *resp.Stats.Agents {
		assert.True(t, ids[ip[0]], "stats.agents lists %s, which the caller cannot read", ip[0])
	}
}
