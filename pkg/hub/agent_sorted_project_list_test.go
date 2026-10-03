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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sortedListFixture builds a project with an owner (full capabilities) and a
// plain member (read-only: the design's read-pass and race tests need a
// caller for whom some agents are unreadable), for the sorted-mode project
// list tests.
type sortedListFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	owner   *store.User
	member  *store.User // project member, no elevated role -- see grantMemberReadOnly
}

func sortedListSetup(t *testing.T) *sortedListFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &sortedListFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID: tid("sl-owner"), Email: "sl-owner@test.com", DisplayName: "Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.owner))
	ensureHubMembership(ctx, s, f.owner.ID)

	f.member = &store.User{
		ID: tid("sl-member"), Email: "sl-member@test.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, s, f.member.ID)

	f.project = &store.Project{
		ID: tid("sl-project"), Name: "Sorted List Project", Slug: "sl-project",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.seedProjectCreatorMembership(ctx, f.project)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.project.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, f.member.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)

	return f
}

func (f *sortedListFixture) listPath(query string) string {
	p := "/api/v1/projects/" + f.project.ID + "/agents"
	if query != "" {
		p += "?" + query
	}
	return p
}

// createAgent creates one agent, owned by the project owner unless
// ownerOverride is non-empty.
func (f *sortedListFixture) createAgent(t *testing.T, slug, phase string, labels map[string]string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("sl-agent-" + slug), Slug: slug, Name: slug,
		ProjectID: f.project.ID, Phase: phase,
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		Labels: labels,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAgentsBulk creates n agents in f.project inside one transaction
// (store.Store.WithTx), so a large fixture (hundreds to low thousands of
// rows) is fast regardless of the per-statement autocommit cost a loop of
// plain CreateAgent calls would otherwise pay. ownerFor, when non-nil,
// picks the OwnerID for agent index i (0-based); nil means every agent is
// owned by f.owner, matching createAgent's single-agent default.
func (f *sortedListFixture) createAgentsBulk(t *testing.T, n int, slugPrefix, phase string, ownerFor func(i int) string) []*store.Agent {
	t.Helper()
	agents := make([]*store.Agent, n)
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < n; i++ {
			owner := f.owner.ID
			if ownerFor != nil {
				owner = ownerFor(i)
			}
			slug := fmt.Sprintf("%s-%d", slugPrefix, i)
			a := &store.Agent{
				ID: tid("sl-bulk-" + slug), Slug: slug, Name: slug,
				ProjectID: f.project.ID, Phase: phase,
				CreatedBy: owner, OwnerID: owner,
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
			agents[i] = a
		}
		return nil
	})
	require.NoError(t, err)
	return agents
}

func mustDecodeListAgentsResponse(t *testing.T, rec interface{ Bytes() []byte }) ListAgentsResponse {
	t.Helper()
	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Bytes(), &resp))
	return resp
}

// --- cursor and parameter rejection ---------------------------------

func TestListProjectAgentsSorted_InvalidParams(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "a1", string(state.PhaseStopped), nil)

	cases := []struct {
		name  string
		query string
	}{
		{"invalid sort value", "sort=bogus"},
		{"invalid dir", "sort=updated&dir=sideways"},
		{"invalid dir with sort=created", "sort=created&dir=sideways"},
		{"fit with cursor", "sort=updated&fit=10&cursor=AAAA"},
		{"fit too large", "sort=updated&fit=501"},
		{"fit zero", "sort=updated&fit=0"},
		{"fit below limit", "sort=updated&fit=5&limit=10"},
		{"malformed cursor", "sort=updated&cursor=not-valid-base64!!"},
		{"legacy-shaped cursor in sorted mode", "sort=updated&cursor=MjAyNi0wMS0wMVQwMDowMDowMFosYWJj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(tc.query), nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "query=%q body=%s", tc.query, rec.Body.String())
		})
	}
}

// TestListProjectAgentsSorted_CreatedSort pins sort=created end to end on
// the project endpoint: paging walks the agentsort "created" total order,
// and a cursor minted under sort=created binds to it.
func TestListProjectAgentsSorted_CreatedSort(t *testing.T) {
	f := sortedListSetup(t)
	var created []*store.Agent
	for i := 0; i < 5; i++ {
		created = append(created, f.createAgent(t, fmt.Sprintf("created-%d", i), string(state.PhaseStopped), nil))
	}

	// Ground truth from the agentsort reference over each agent's own
	// Created/ID, not an assumption about CreateAgent's real-clock timing
	// (two calls can land in the same clock tick).
	rows := make([]agentsort.Row, len(created))
	for i, a := range created {
		rows[i] = agentsort.KeyFor(agentsort.Created, a.ID, a.Created, a.Updated, a.LastActivityEvent)
	}
	agentsort.SortRows(agentsort.Desc, rows)
	want := make([]string, len(rows))
	for i, row := range rows {
		want[i] = row.ID
	}

	var walked []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := "sort=created&dir=desc&limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, "created", resp.Sort)
		assert.Equal(t, "desc", resp.Dir)
		for _, a := range resp.Agents {
			walked = append(walked, a.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	assert.Equal(t, want, walked)
}

// TestListProjectAgentsSorted_CursorWrongSortOrDirRejected pins that a
// cursor minted for one sort/dir is rejected when replayed against another
// (design 4.4).
func TestListProjectAgentsSorted_CursorWrongSortOrDirRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("a%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor, "expected a next cursor with limit=1 and 3 agents")

	// Same cursor, different dir: must be rejected.
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=asc&limit=1&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_CursorCrossPrincipalRejected pins that a
// cursor minted for one identity cannot be replayed by another (the
// binding includes the identity, design 4.4).
func TestListProjectAgentsSorted_CursorCrossPrincipalRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("a%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListProjectAgentsSorted_CursorPhaseReplayRejected pins the phase-replay
// rejection (design 4.4: "a phase=running cursor replayed under
// phase=stopped returns 400").
func TestListProjectAgentsSorted_CursorPhaseReplayRejected(t *testing.T) {
	f := sortedListSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("run-%d", i), string(state.PhaseRunning), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=1&phase=running"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor)

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet,
		f.listPath("sort=updated&dir=desc&limit=1&phase=stopped&cursor="+url.QueryEscape(resp.NextCursor)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// --- candidate ceiling (hard gate) -----------------------------------

// countingAgentStore wraps a real store.Store and lets tests fake
// CountAgents/ListAgentMembers results, or count calls, without paying for
// thousands of real row inserts in the test SQLite backend.
type countingAgentStore struct {
	store.Store
	mu                sync.Mutex
	countAgentsCalls  int
	membersCalls      int
	getByIDsCalls     int
	listAgentsCalls   int
	listAgentsIDs     [][]string // filter.IDs seen by each ListAgents call, in call order
	fakeCandidateSize int        // if > 0, CountAgents and ListAgentMembers report this size
	maxSeen           int        // last "max" ListAgentMembers was called with
}

func (c *countingAgentStore) CountAgents(ctx context.Context, filter store.AgentFilter) (int, error) {
	c.mu.Lock()
	c.countAgentsCalls++
	c.mu.Unlock()
	if c.fakeCandidateSize > 0 {
		return c.fakeCandidateSize, nil
	}
	return c.Store.CountAgents(ctx, filter)
}

func (c *countingAgentStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	c.mu.Lock()
	c.membersCalls++
	c.maxSeen = max
	c.mu.Unlock()
	if c.fakeCandidateSize > 0 {
		n := c.fakeCandidateSize
		if n > max {
			n = max
		}
		out := make([]store.AgentMember, n)
		for i := range out {
			out[i] = store.AgentMember{ID: fmt.Sprintf("fake-%d", i), ProjectID: filter.ProjectID}
		}
		return out, nil
	}
	return c.Store.ListAgentMembers(ctx, filter, sort, dir, max)
}

func (c *countingAgentStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	c.mu.Lock()
	c.getByIDsCalls++
	c.mu.Unlock()
	return c.Store.GetAgentsByIDs(ctx, ids)
}

// ListAgents is overridden so tests can observe loadFullRowsForPage's actual
// full-row read: how many times it runs per request, and exactly which IDs
// it asks for (the old getByIDsCalls assertion in
// TestListProjectAgentsSorted_CandidateCeiling was vacuous after the
// full-row read moved from GetAgentsByIDs to ListAgents to honor
// includeDeleted, so it passed regardless of what the handler actually did).
func (c *countingAgentStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	c.mu.Lock()
	c.listAgentsCalls++
	c.listAgentsIDs = append(c.listAgentsIDs, append([]string(nil), filter.IDs...))
	c.mu.Unlock()
	return c.Store.ListAgents(ctx, filter, opts)
}

// TestListProjectAgentsSorted_CandidateCeiling is the candidate-ceiling hard
// gate: a candidate pool above authorizedListMaxCandidates gets the 422
// refusal, with exactly
// one decision (the agent.list gate) and zero read-pass/capability
// decisions; ListAgentMembers is not called with results scanned into the
// read pass. The candidate pool is faked (via countingAgentStore) here,
// specifically to assert the call counts (countAgentsCalls/membersCalls/
// listAgentsCalls) a real 2001-row pool can't observe as directly;
// TestListProjectAgentsSorted_CandidateCeiling_RealRows (designsizes_test.go)
// covers the same gate with a real, materialized 2001-row pool.
func TestListProjectAgentsSorted_CandidateCeiling(t *testing.T) {
	f := sortedListSetup(t)
	counting := &countingAgentStore{Store: f.store, fakeCandidateSize: authorizedListMaxCandidates + 1}
	f.srv.store = counting

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
	assert.Equal(t, "too_many_candidates", body.Error.Details["reason"])

	assert.Len(t, emitter.records, 1, "exactly the agent.list gate decision, zero read-pass/capability decisions")
	assert.Equal(t, 1, counting.countAgentsCalls)
	assert.Equal(t, 0, counting.membersCalls, "ListAgentMembers must not be called once CountAgents already exceeds the ceiling")
	assert.Equal(t, 0, counting.listAgentsCalls, "the full-row read (loadFullRowsForPage -> ListAgents) must not run once the ceiling is breached; there is no page to read rows for")
}

// TestListProjectAgentsSorted_CandidateCeiling_Race is the candidate-ceiling
// gate's race sub-case: CountAgents reports under the ceiling, but the
// ListAgentMembers max=2001 read returns more than the ceiling (the pool
// grew in between).
// The response is still the 422, with the same single-decision cost.
func TestListProjectAgentsSorted_CandidateCeiling_Race(t *testing.T) {
	f := sortedListSetup(t)
	counting := &countingAgentStore{Store: f.store}
	f.srv.store = counting

	// CountAgents: real (0, since no agents exist), so it passes the
	// pre-check; then fake ListAgentMembers to report a grown pool. We
	// achieve this by giving CountAgents a fixed "just under" answer and
	// ListAgentMembers a fixed "over" answer independently.
	counting.fakeCandidateSize = 0 // use real CountAgents (0 agents) so step 0 passes
	// Override ListAgentMembers behavior via a second wrapper layer that
	// always returns an over-ceiling slice regardless of what CountAgents saw.
	raceStore := &raceMembersStore{countingAgentStore: counting, memberCount: authorizedListMaxCandidates + 1}
	f.srv.store = raceStore

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Len(t, emitter.records, 1, "the race must still cost only the agent.list gate decision")
}

// raceMembersStore always answers ListAgentMembers with memberCount rows
// (capped at the caller's max), independent of CountAgents' answer,
// simulating candidate growth between the two reads (design 5.3 step 1).
type raceMembersStore struct {
	*countingAgentStore
	memberCount int
}

func (r *raceMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	r.mu.Lock()
	r.membersCalls++
	r.mu.Unlock()
	n := r.memberCount
	if n > max {
		n = max
	}
	out := make([]store.AgentMember, n)
	for i := range out {
		out[i] = store.AgentMember{ID: fmt.Sprintf("race-%d", i), ProjectID: filter.ProjectID}
	}
	return out, nil
}

// TestListProjectAgentsSorted_FullRowRead_ExactlyOncePerRequest_IDsAreThePage
// proves: a real, successful request must trigger exactly one
// full-row read (loadFullRowsForPage -> ListAgents) per request, and the IDs
// that read asks for must be exactly the page's IDs -- not the whole
// candidate set, and not called once per item. This replaces an earlier,
// vacuous getByIDsCalls==0 assertion with one that actually
// guards the "no full-row read outside the page" property.
func TestListProjectAgentsSorted_FullRowRead_ExactlyOncePerRequest_IDsAreThePage(t *testing.T) {
	t.Run("paged", func(t *testing.T) {
		f := sortedListSetup(t)
		const n = 8
		const limit = 3
		for i := 0; i < n; i++ {
			f.createAgent(t, fmt.Sprintf("frr-paged-%d", i), string(state.PhaseStopped), nil)
		}
		counting := &countingAgentStore{Store: f.store}
		f.srv.store = counting

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", limit, limit)), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.Len(t, resp.Agents, limit)

		require.Equal(t, 1, counting.listAgentsCalls, "exactly one full-row read per request")
		require.Len(t, counting.listAgentsIDs, 1)
		gotIDs := counting.listAgentsIDs[0]
		assert.Len(t, gotIDs, limit, "the full-row read must ask for exactly the page's IDs, not the whole n-candidate set")
		wantIDs := make([]string, len(resp.Agents))
		for i, a := range resp.Agents {
			wantIDs[i] = a.ID
		}
		assert.ElementsMatch(t, wantIDs, gotIDs, "the full-row read's IDs must be exactly the page, not a superset or subset")
	})

	t.Run("complete", func(t *testing.T) {
		f := sortedListSetup(t)
		const n = 5
		for i := 0; i < n; i++ {
			f.createAgent(t, fmt.Sprintf("frr-complete-%d", i), string(state.PhaseStopped), nil)
		}
		counting := &countingAgentStore{Store: f.store}
		f.srv.store = counting

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.Len(t, resp.Agents, n)

		require.Equal(t, 1, counting.listAgentsCalls, "exactly one full-row read per request, even when the response is complete")
		require.Len(t, counting.listAgentsIDs, 1)
		assert.Len(t, counting.listAgentsIDs[0], n)
	})
}

// TestListProjectAgentsSorted_UnderCeiling_ReturnsExactTotals asserts the
// non-ceiling side of the candidate-ceiling gate: at or below the ceiling,
// the request succeeds with exact totals (a small N stand-in for "2,000
// agents", which is exercised
// above via the fake-size path; here we prove the real code path with real
// rows at a modest N).
func TestListProjectAgentsSorted_UnderCeiling_ReturnsExactTotals(t *testing.T) {
	f := sortedListSetup(t)
	const n = 12
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("under-%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Equal(t, n, resp.TotalCount)
	assert.Len(t, resp.Agents, n)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
	assert.Equal(t, "updated", resp.Sort)
	assert.Equal(t, "desc", resp.Dir)
}

// --- fit / completeness ---------------------------------------------

func TestListProjectAgentsSorted_Fit_CompleteWhenAtOrBelow(t *testing.T) {
	f := sortedListSetup(t)
	const n = 5
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("fit-%d", i), string(state.PhaseRunning), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d&phase=stopped", n, n)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete, "n <= fit must be complete even though every agent is 'running' and the request asked for phase=stopped")
	assert.Len(t, resp.Agents, n, "a complete response is unphased")
	assert.Empty(t, resp.NextCursor)
}

func TestListProjectAgentsSorted_Fit_IncompleteAboveFit(t *testing.T) {
	f := sortedListSetup(t)
	const n = 6
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("over-%d", i), string(state.PhaseStopped), nil)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n-1, n-1)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete)
	assert.NotEmpty(t, resp.NextCursor)
}

// --- stats ------------------------------------------------------------

func TestListProjectAgentsSorted_Stats(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "run-1", string(state.PhaseRunning), nil)
	f.createAgent(t, "run-2", string(state.PhaseRunning), nil)
	f.createAgent(t, "stop-1", string(state.PhaseStopped), nil)

	// stats must ignore the request's own phase filter: a
	// phase=stopped request still reports the true running count.
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&stats=1&phase=stopped"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Stats)
	assert.Equal(t, 3, resp.Stats.Total)
	assert.Equal(t, 2, resp.Stats.Running)
	require.NotNil(t, resp.Stats.Agents)
	assert.Len(t, *resp.Stats.Agents, 3)

	// This response happens to be complete (n=3 <= fit=500), so per design
	// 4.3/5.3 the page itself is the whole unphased set, not narrowed to
	// phase=stopped -- phase only narrows a *paged* response. That is
	// asserted separately in TestListProjectAgentsSorted_PagedAppliesPhase.
	assert.Len(t, resp.Agents, 3)
}

// TestListProjectAgentsSorted_PagedAppliesPhase confirms the complement:
// once the response is paged (not complete), the phase filter narrows the
// page, unlike a complete response (design 4.3: "Phase on a fit request is
// applied only to a paged response").
func TestListProjectAgentsSorted_PagedAppliesPhase(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "pf-run-1", string(state.PhaseRunning), nil)
	f.createAgent(t, "pf-run-2", string(state.PhaseRunning), nil)
	f.createAgent(t, "pf-stop-1", string(state.PhaseStopped), nil)

	// fit=1 forces a paged response (n=3 > fit=1).
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=1&limit=1&phase=stopped"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.False(t, *resp.Complete)
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "stopped", resp.Agents[0].Phase)
	assert.Equal(t, 1, resp.TotalCount, "totalCount is the phase-filtered readable count in paged mode")
}

// TestListProjectAgentsSorted_StatsOnlyValidWithSort pins that "stats=1"
// without "sort" is not silently accepted (design 4.1: "Only valid with
// sort"). The legacy endpoint has no stats concept, so this just checks the
// legacy response has no stats block (stats is unrecognized/ignored there,
// which is byte-identical to today per the legacy-mode contract).
func TestListProjectAgentsSorted_StatsIgnoredInLegacyMode(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "legacy-1", string(state.PhaseRunning), nil)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Nil(t, resp.Stats)
	assert.Empty(t, resp.Sort)
}

// --- decision counts (hard gate) ---------------------------------------

// TestListProjectAgentsSorted_DecisionCounts_Complete pins the section 6.4
// formula for a complete fit response: 5 + n + 7R (gate + one read decision
// per candidate + 7 remaining-action decisions per readable item), which is
// <= today's 5 + 8n and equal when R == n. n is kept small here as a quick
// unit-style check of the formula's shape;
// TestListProjectAgentsSorted_DecisionCounts_DesignSizes (designsizes_test.go)
// re-asserts the same formula at the design's own sizes (25-1200).
func TestListProjectAgentsSorted_DecisionCounts_Complete(t *testing.T) {
	f := sortedListSetup(t)
	const n = 6
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("dc-%d", i), string(state.PhaseStopped), nil)
	}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", n, n)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	require.True(t, *resp.Complete)
	require.Len(t, resp.Agents, n, "owner can read every candidate, so R == n")

	// 1 (agent.list gate) + n (read pass) + 7n (remaining actions, all
	// readable) + 4 (scope caps) = 5 + 8n.
	want := 5 + 8*n
	assert.Len(t, emitter.records, want, "decision count must equal 5 + 8n when every candidate is readable")
}

// TestListProjectAgentsSorted_DecisionCounts_Paged pins the *paged* cost
// bound, 5 + n + 7P, which does not depend on R at all (design 5.3,
// "completeness does not depend on R"). The R < n sub-cases themselves
// (n=1200/R=400 paged=1380, n=500/R=200 complete=1905) are in
// designsizes_test.go, using grantProjectListOnly plus per-agent ownership
// -- a minimal project-scoped role granting only agent.list, combined with
// the owner relationship grant, reaches R < n for one caller in one project
// with no access constraint needed.
func TestListProjectAgentsSorted_DecisionCounts_Paged(t *testing.T) {
	f := sortedListSetup(t)
	const n = 8
	const limit = 3
	for i := 0; i < n; i++ {
		f.createAgent(t, fmt.Sprintf("pg-%d", i), string(state.PhaseStopped), nil)
	}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&fit=%d&limit=%d", limit, limit)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	require.False(t, *resp.Complete)
	require.Len(t, resp.Agents, limit)

	// 1 (gate) + n (read pass over every candidate) + 7*limit (remaining
	// actions for the page only) + 4 (scope caps) = 5 + n + 7P.
	want := 5 + n + 7*limit
	assert.Len(t, emitter.records, want)
}

// TestListProjectAgentsSorted_MemberProjectionEquality is the hub-side half
// of the non-waivable decision-count gate: for a candidate that does not
// race, memberResource(m) must equal agentResource(full), and the page's
// merged capabilities must deep-equal ComputeCapabilitiesBatch's output over
// the same resource.
// TestListProjectAgentsSorted_MemberProjectionEquality (the reflection-filled,
// real-round-trip version the non-waivable gate requires) lives in
// agent_sorted_project_list_reflection_test.go. The old
// hand-built-member version that lived here could not fail on a new
// agentResource input and has been replaced, not merely supplemented.

// TestMergeCapabilities_EquivalentToSingleBatchPass is the pure-logic half
// of the decision-count deep-equality gate: splitting
// ResourceActions["agent"] into a read-only pass and a remaining-actions
// pass and merging them with
// mergeCapabilities must produce the same set (order and membership) as
// deciding every action in one pass would, for every combination of
// allowed actions.
func TestMergeCapabilities_EquivalentToSingleBatchPass(t *testing.T) {
	order := ResourceActions["agent"]
	require.NotEmpty(t, order)

	// Enumerate every subset of "which actions are allowed" up to a bound,
	// by testing each single-action-allowed case plus the all- and
	// none-allowed cases, which exercises the order-preservation and
	// membership logic without 2^8 cases.
	allowedSets := [][]Action{
		{},
		order,
	}
	for _, a := range order {
		allowedSets = append(allowedSets, []Action{a})
	}

	for _, allowed := range allowedSets {
		allowedSet := map[Action]bool{}
		for _, a := range allowed {
			allowedSet[a] = true
		}
		readCap := &Capabilities{}
		restCap := &Capabilities{}
		var singlePass []string
		for _, a := range order {
			if !allowedSet[a] {
				continue
			}
			singlePass = append(singlePass, string(a))
			if a == ActionRead {
				readCap.Actions = append(readCap.Actions, string(a))
			} else {
				restCap.Actions = append(restCap.Actions, string(a))
			}
		}
		if singlePass == nil {
			singlePass = []string{}
		}
		merged := mergeCapabilities(order, readCap, restCap)
		assert.Equal(t, singlePass, merged.Actions, "allowed=%v", allowed)
	}
}

// TestListProjectAgentsSorted_NilVsEmptyLabelsNoRedecision proves:
// nil vs empty Labels/Ancestry must never trigger a step-5a re-decision.
func TestListProjectAgentsSorted_NilVsEmptyLabelsNoRedecision(t *testing.T) {
	a := &Resource{Type: "agent", ID: "x", Labels: nil, Ancestry: nil}
	b := &Resource{Type: "agent", ID: "x", Labels: map[string]string{}, Ancestry: []string{}}
	assert.True(t, resourceEqual(*a, *b), "nil and empty Labels/Ancestry must compare equal")
}

// --- Step 5a race behavior -------------------------------------------------

// mutatingAfterMembersStore mutates an agent's labels (via the real store,
// bypassing the read path) the first time ListAgentMembers is called,
// simulating a write landing between the member read and the full-row read
// (design 5.3 step 5a).
type mutatingAfterMembersStore struct {
	store.Store
	once      sync.Once
	agentID   string
	newLabels map[string]string
}

func (m *mutatingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := m.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil {
		return nil, err
	}
	m.once.Do(func() {
		a, gerr := m.GetAgent(ctx, m.agentID)
		if gerr != nil {
			return
		}
		a.Labels = m.newLabels
		_ = m.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_Race_LabelChange_StillMatchesFilter is the
// decision-count gate's race sub-case: a page item's labels change between
// the two reads but it still matches the request's label filter, so it is
// kept and re-decided (9 decisions total: 1 in step 3, 8 in step 5a, 0 in
// step 6).
func TestListProjectAgentsSorted_Race_LabelChange_StillMatchesFilter(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-match", string(state.PhaseStopped), map[string]string{"team": "a", "extra": "1"})

	raced := &mutatingAfterMembersStore{Store: f.store, agentID: a.ID, newLabels: map[string]string{"team": "a", "extra": "2"}}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&label=team=a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the raced item still matches label=team=a and must be kept")

	// n=1 candidate: 5 (gate+caps) + 1 (step3 read) + 8 (step5a re-decision) + 0 (step6 skip) = 14.
	assert.Len(t, emitter.records, 14)
}

// TestListProjectAgentsSorted_Race_LabelChange_NoLongerMatchesFilter proves:
// a page item whose labels change so it no longer matches the
// request's label filter is dropped, at no extra decision cost.
func TestListProjectAgentsSorted_Race_LabelChange_NoLongerMatchesFilter(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-drop", string(state.PhaseStopped), map[string]string{"team": "a"})

	raced := &mutatingAfterMembersStore{Store: f.store, agentID: a.ID, newLabels: map[string]string{"team": "b"}}
	f.srv.store = raced

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500&label=team=a"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents, "the raced item no longer matches label=team=a and must be dropped")

	// n=1 candidate: 5 (gate+caps) + 1 (step3 read) + 0 (filter-mismatch
	// drop, no additional decision) = 6.
	assert.Len(t, emitter.records, 6)
}

// TestListProjectAgentsSorted_Race_MissingRow is the "deleted between the
// two reads" sub-case: the row disappears with no additional decision cost.
func TestListProjectAgentsSorted_Race_MissingRow(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-missing", string(state.PhaseStopped), nil)

	raced := &deletingAfterMembersStore{Store: f.store, agentID: a.ID}
	f.srv.store = raced

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents, "a row deleted between the two reads must be dropped")
	assert.Equal(t, 0, resp.TotalCount)
}

type deletingAfterMembersStore struct {
	store.Store
	once    sync.Once
	agentID string
}

func (d *deletingAfterMembersStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	members, err := d.Store.ListAgentMembers(ctx, filter, sort, dir, max)
	if err != nil {
		return nil, err
	}
	d.once.Do(func() { _ = d.DeleteAgent(ctx, d.agentID) })
	return members, nil
}

// TestListProjectAgentsSorted_Race_ProjectMismatch proves: a full row
// whose ProjectID differs from the request project is dropped at no
// decision cost. UpdateAgent never mutates ProjectID in this codebase
// (buildAgentUpdate, entadapter/agent_store.go), so this race cannot be
// produced by writing through the normal store API; the test instead
// fabricates the mismatch at the full-row-load boundary itself (ListAgents,
// which listProjectAgentsSorted uses to honor IncludeDeleted), exercising
// the handler's explicit ProjectID check directly, regardless of whether
// today's write paths can reach it.
func TestListProjectAgentsSorted_Race_ProjectMismatch(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "race-project", string(state.PhaseStopped), nil)

	raced := &reprojectingListAgentsStore{Store: f.store, agentID: a.ID, newProjectID: tid("sl-other-project")}
	f.srv.store = raced

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Empty(t, resp.Agents, "a row that moved to another project between the two reads must be dropped")
}

type reprojectingListAgentsStore struct {
	store.Store
	agentID      string
	newProjectID string
}

func (r *reprojectingListAgentsStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	result, err := r.Store.ListAgents(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	for i := range result.Items {
		if result.Items[i].ID == r.agentID {
			result.Items[i].ProjectID = r.newProjectID
		}
	}
	return result, nil
}

// fullRowReadRecorder records the full-row reads loadFullRowsForPage could
// issue and answers them with no rows. Every other store method panics via
// the nil embedded Store, so any unexpected read fails the test too.
type fullRowReadRecorder struct {
	store.Store
	listAgentsCalls int
	getByIDsCalls   int
}

func (r *fullRowReadRecorder) ListAgents(_ context.Context, _ store.AgentFilter, _ store.ListOptions) (*store.ListResult[store.Agent], error) {
	r.listAgentsCalls++
	return &store.ListResult[store.Agent]{}, nil
}

func (r *fullRowReadRecorder) GetAgentsByIDs(_ context.Context, _ []string) (map[string]*store.Agent, error) {
	r.getByIDsCalls++
	return map[string]*store.Agent{}, nil
}

// TestLoadFullRowsForPage_OverBoundFailsBeforeRead: the store clamps a
// ListAgents read to its page cap, so more than maxSortedLimit ids could
// come back short without an error. loadFullRowsForPage must refuse such a
// request with an internal (non-validation) error before touching the
// store, while a request at the bound still reads.
func TestLoadFullRowsForPage_OverBoundFailsBeforeRead(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		}
		return out
	}

	rec := &fullRowReadRecorder{}
	srv := &Server{store: rec}
	rows, err := srv.loadFullRowsForPage(context.Background(), ids(maxSortedLimit+1), false)
	require.Error(t, err)
	assert.Nil(t, rows)
	assert.False(t, errors.Is(err, store.ErrInvalidInput), "an over-bound read is an internal error, not caller input")
	assert.False(t, errors.Is(err, store.ErrNotFound))
	assert.Equal(t, 0, rec.listAgentsCalls, "no full-row read may run once the bound is exceeded")
	assert.Equal(t, 0, rec.getByIDsCalls, "no full-row read may run once the bound is exceeded")

	w := httptest.NewRecorder()
	writeErrorFromErr(w, err, "")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	// The client gets only the generic internal error: no id count, no
	// bound value and no guard text.
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.Equal(t, "internal_error", body.Error.Code)
	assert.Equal(t, "Internal server error", body.Error.Message)
	for _, leak := range []string{
		fmt.Sprint(maxSortedLimit + 1),
		fmt.Sprint(maxSortedLimit),
		"501",
		"500",
		"bound",
	} {
		assert.NotContains(t, strings.ToLower(w.Body.String()), leak, "error body must not reveal %q", leak)
	}

	// At the bound the read runs, and a short (here empty) result is not an
	// error: it is the race-drop signal handled by the callers.
	rows, err = srv.loadFullRowsForPage(context.Background(), ids(maxSortedLimit), false)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Equal(t, 1, rec.listAgentsCalls)
}
