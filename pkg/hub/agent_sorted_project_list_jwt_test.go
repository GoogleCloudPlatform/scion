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
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentJWTFor mints a project-scoped agent token for agentID, usable against
// the sortedListFixture's project.
func (f *sortedListFixture) agentJWTFor(t *testing.T, agentID string) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	tok, err := svc.GenerateAgentToken(agentID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	return tok
}

// TestListProjectAgentsSortedAgentJWT_BasicSortedRead pins that a sorted
// request from an agent JWT succeeds, returns every sibling agent (the
// agent-JWT path has no read filter), and echoes sort/dir.
func TestListProjectAgentsSortedAgentJWT_BasicSortedRead(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "self", string(state.PhaseRunning), nil)
	f.createAgent(t, "sibling-1", string(state.PhaseStopped), nil)
	f.createAgent(t, "sibling-2", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)

	for _, sortKey := range []string{"updated", "created"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort="+sortKey+"&fit=500"), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, sortKey, resp.Sort)
		assert.Equal(t, "desc", resp.Dir)
		require.NotNil(t, resp.Complete)
		assert.True(t, *resp.Complete)
		assert.Len(t, resp.Agents, 3, "agent-JWT path has no read filter: every member is readable")
	}
}

// TestListProjectAgentsSortedAgentJWT_CursorBinding pins that agent-JWT
// sorted paging covers every agent exactly once. Cross-principal replay is
// covered by TestListProjectAgentsSortedAgentJWT_CursorCrossPrincipalRejected.
func TestListProjectAgentsSortedAgentJWT_CursorBinding(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "self2", string(state.PhaseRunning), nil)
	for i := 0; i < 4; i++ {
		f.createAgent(t, fmt.Sprintf("jwt-sib-%d", i), string(state.PhaseStopped), nil)
	}
	tok := f.agentJWTFor(t, self.ID)

	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 10; i++ {
		q := "sort=updated&dir=desc&limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			assert.False(t, seen[a.ID], "agent %s seen twice", a.ID)
			seen[a.ID] = true
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	assert.Len(t, seen, 5)
}

// TestListProjectAgentsSortedAgentJWT_DecisionCounts_Paged is the agent-JWT
// paged-path decision-count gate: limit=500 at n=501 costs exactly 4,004
// decisions (4 + 8P, no gate, no effective-page-size reduction on this
// path), and so do limit=501 and limit=100000 via the limit clamp.
func TestListProjectAgentsSortedAgentJWT_DecisionCounts_Paged(t *testing.T) {
	f, counting, fault := sortedListSetupWithFault(t, newCountingAgentStore)
	self := f.createAgent(t, "self3", string(state.PhaseRunning), nil)
	f.createAgentsBulk(t, 500, "pagedjwt", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)

	// limit above the 500 maximum is clamped to 500, not honoured: the clamp
	// is this path's page-size bound, so 501 and 100000 must cost exactly
	// what 500 costs. The page itself must be 500 rows with a next cursor
	// for the 501st, and the full-row read must ask for exactly those 500
	// ids: an unclamped 501-row page would reach the end (no cursor) and
	// ask for 501 ids, even though the store's own read cap would still
	// hand back only 500 rows.
	fault.Arm()
	for _, limit := range []int{500, 501, 100000} {
		counting.mu.Lock()
		counting.listAgentsIDs = nil
		counting.mu.Unlock()
		emitter := &recordingDecisionAuditEmitter{}
		f.srv.authzService.SetDecisionAuditEmitter(emitter)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&dir=desc&limit=%d", limit)), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, "limit=%d: %s", limit, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Len(t, resp.Agents, 500, "limit=%d", limit)
		assert.Equal(t, 501, resp.TotalCount, "limit=%d", limit)
		assert.NotEmpty(t, resp.NextCursor, "limit=%d: the page holds 500 of 501 rows, so it must carry a next cursor", limit)

		counting.mu.Lock()
		var fullRowReads []int
		for _, ids := range counting.listAgentsIDs {
			if len(ids) > 0 {
				fullRowReads = append(fullRowReads, len(ids))
			}
		}
		counting.mu.Unlock()
		assert.Contains(t, fullRowReads, 500, "limit=%d: the full-row read must ask for exactly the 500-row page", limit)
		for _, n := range fullRowReads {
			assert.LessOrEqual(t, n, 500, "limit=%d: no id-set read may exceed the 500-row page", limit)
		}

		assert.Len(t, emitter.records, 4004, "limit=%d: 4 + 8*500", limit)
	}
}

// TestListProjectAgentsSortedAgentJWT_DecisionCounts_Complete pins the
// complete-branch decision count for the agent-JWT path: 4 fixed
// scope-capability decisions plus 8 per candidate, equal to today's legacy
// cost since there is no read filter on this path.
func TestListProjectAgentsSortedAgentJWT_DecisionCounts_Complete(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "self4", string(state.PhaseRunning), nil)
	f.createAgentsBulk(t, 24, "completejwt", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500&limit=500"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, resp.Complete)
	assert.True(t, *resp.Complete)
	assert.Len(t, resp.Agents, 25)

	assert.Len(t, emitter.records, 4+8*25)
}

// TestListProjectAgentsSortedAgentJWT_CandidateCeiling asserts the exact
// decision count above the candidate ceiling for the agent-JWT path: there,
// exactly zero decisions are recorded, since there is no agent.list gate on
// this path at all.
func TestListProjectAgentsSortedAgentJWT_CandidateCeiling(t *testing.T) {
	f, counting, fault := sortedListSetupWithFault(t, newCountingAgentStore)
	self := f.createAgent(t, "self5", string(state.PhaseRunning), nil)
	counting.fakeCandidateSize = authorizedListMaxCandidates + 1
	fault.Arm()
	tok := f.agentJWTFor(t, self.ID)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500"), nil, tok)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, errCodeSortedViewUnavailable, decodeErrorCode(t, rec.Body.Bytes()))
	assert.Empty(t, emitter.records, "the agent-JWT path has no agent.list gate: the 422 must cost zero decisions")
	assert.Equal(t, 1, counting.countAgentsCalls)
	assert.Equal(t, 0, counting.membersCalls, "ListAgentMembers must not be called once CountAgents already exceeds the ceiling")
	assert.Equal(t, 0, counting.listAgentsCalls, "no full-row read once the ceiling is breached")
}

// TestListProjectAgentsSortedAgentJWT_CandidateCeiling_Race is the agent-JWT
// variant of the ceiling race: CountAgents reports under the ceiling, but
// the member read returns more than the ceiling (the pool grew in between).
// The response is still the 422, at zero decisions, with no full-row read.
func TestListProjectAgentsSortedAgentJWT_CandidateCeiling_Race(t *testing.T) {
	f, raceStore, fault := sortedListSetupWithFault(t, newRaceMembersStore(authorizedListMaxCandidates+1))
	counting := raceStore.countingAgentStore
	self := f.createAgent(t, "self5r", string(state.PhaseRunning), nil)
	fault.Arm()
	tok := f.agentJWTFor(t, self.ID)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500"), nil, tok)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, errCodeSortedViewUnavailable, decodeErrorCode(t, rec.Body.Bytes()))
	assert.Empty(t, emitter.records, "the race must still cost zero decisions on the agent-JWT path")
	assert.Equal(t, 1, counting.countAgentsCalls, "the pre-check COUNT ran and passed")
	assert.Equal(t, 1, counting.membersCalls, "the member read ran once and tripped the ceiling")
	assert.Equal(t, 0, counting.listAgentsCalls, "no full-row read once the member read breaches the ceiling")
}

// TestListProjectAgentsSortedAgentJWT_CursorCrossPrincipalRejected mints a
// cursor under sibling agent A's token and replays it under sibling agent
// B's token and under a project user: both are 400. The same cursor under
// A's token is accepted, so the 400s come from the identity in the binding.
func TestListProjectAgentsSortedAgentJWT_CursorCrossPrincipalRejected(t *testing.T) {
	f := sortedListSetup(t)
	a := f.createAgent(t, "xp-a", string(state.PhaseRunning), nil)
	b := f.createAgent(t, "xp-b", string(state.PhaseRunning), nil)
	f.createAgent(t, "xp-c", string(state.PhaseStopped), nil)
	tokA := f.agentJWTFor(t, a.ID)
	tokB := f.agentJWTFor(t, b.ID)

	q := "sort=updated&limit=1"
	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tokA)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
	require.NotEmpty(t, cur)
	replay := f.listPath(q + "&cursor=" + url.QueryEscape(cur))

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, replay, nil, tokA)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, replay, nil, tokB)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "agent B: %s", rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, replay, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "user: %s", rec.Body.String())
}

// TestListProjectAgentsSortedAgentJWT_CursorPhaseReplayRejected pins that an
// agent-JWT cursor minted under one phase filter is a 400 under another.
func TestListProjectAgentsSortedAgentJWT_CursorPhaseReplayRejected(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "phase-self", string(state.PhaseRunning), nil)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("phase-stop-%d", i), string(state.PhaseStopped), nil)
		f.createAgent(t, fmt.Sprintf("phase-run-%d", i), string(state.PhaseRunning), nil)
	}
	tok := f.agentJWTFor(t, self.ID)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&limit=1&phase=stopped"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
	require.NotEmpty(t, cur)

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&limit=1&phase=stopped&cursor="+url.QueryEscape(cur)), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&limit=1&phase=running&cursor="+url.QueryEscape(cur)), nil, tok)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// --- agent-JWT race drops ----------------------------------------------------

// TestListProjectAgentsSortedAgentJWT_Race_ProjectMismatch pins that a full
// row whose ProjectID no longer matches is dropped on the agent-JWT path at
// no decision cost: 4 scope decisions plus 8 for the one kept row.
func TestListProjectAgentsSortedAgentJWT_Race_ProjectMismatch(t *testing.T) {
	f, raced, fault := sortedListSetupWithFault(t, newReprojectingListAgentsStore)
	self := f.createAgent(t, "rp-self", string(state.PhaseRunning), nil)
	moved := f.createAgent(t, "rp-moved", string(state.PhaseStopped), nil)
	tok := f.agentJWTFor(t, self.ID)

	raced.agentID, raced.newProjectID = moved.ID, tid("sl-other-project")
	fault.Arm()
	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the row that moved project must be dropped")
	assert.Equal(t, self.ID, resp.Agents[0].ID)
	assert.Equal(t, 1, resp.TotalCount)
	assert.Len(t, emitter.records, 4+8*1)
}

// TestListProjectAgentsSortedAgentJWT_Race_MissingRow pins that a page row
// deleted between the member read and the full-row read is dropped, leaving
// a short page that still carries a cursor, and that following the cursor
// continues after it with no row repeated.
func TestListProjectAgentsSortedAgentJWT_Race_MissingRow(t *testing.T) {
	f, raced, fault := sortedListSetupWithFault(t, newDeletingAfterMembersStore)
	self := f.createAgent(t, "rm-self", string(state.PhaseRunning), nil)
	sibs := make([]string, 4)
	for i := range sibs {
		sibs[i] = f.createAgent(t, fmt.Sprintf("rm-sib-%d", i), string(state.PhaseStopped), nil).ID
	}
	tok := f.agentJWTFor(t, self.ID)

	// updated desc: the newest sibling is first on page 0.
	newest := sibs[len(sibs)-1]
	raced.agentID = newest
	fault.Arm()
	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=2"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the deleted row must be dropped, leaving a short page")
	assert.Equal(t, sibs[len(sibs)-2], resp.Agents[0].ID)
	require.NotEmpty(t, resp.NextCursor, "a short page must still carry the cursor")
	assert.Len(t, emitter.records, 4+8*1)

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&dir=desc&limit=2&cursor="+url.QueryEscape(resp.NextCursor)), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	next := mustDecodeListAgentsResponse(t, rec.Body)
	got := []string{}
	for _, a := range next.Agents {
		got = append(got, a.ID)
	}
	assert.Equal(t, []string{sibs[1], sibs[0]}, got)
}

// TestListProjectAgentsSortedAgentJWT_Race_LabelChange_NoLongerMatchesFilter
// pins that a row whose labels change so it fails the label filter is
// dropped on the agent-JWT path at no decision cost.
func TestListProjectAgentsSortedAgentJWT_Race_LabelChange_NoLongerMatchesFilter(t *testing.T) {
	f, mutating, fault := sortedListSetupWithFault(t, newMutatingAfterMembersStore)
	self := f.createAgent(t, "rf-self", string(state.PhaseRunning), map[string]string{"team": "a"})
	raced := f.createAgent(t, "rf-raced", string(state.PhaseStopped), map[string]string{"team": "a"})
	tok := f.agentJWTFor(t, self.ID)

	mutating.agentID, mutating.newLabels = raced.ID, map[string]string{"team": "b"}
	fault.Arm()
	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500&label=team=a"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the row that no longer matches label=team=a must be dropped")
	assert.Equal(t, self.ID, resp.Agents[0].ID)
	assert.Equal(t, 1, resp.TotalCount)
	assert.Len(t, emitter.records, 4+8*1)
}

// TestListProjectAgentsSortedAgentJWT_Race_LabelChange_StillMatchesFilter
// pins that a row that changes between the two reads but still passes is
// kept with the full row's data and capabilities, for exactly 4 + 8P
// decisions. The capabilities are compared against a legacy agent-JWT
// listing of the same row after the change.
func TestListProjectAgentsSortedAgentJWT_Race_LabelChange_StillMatchesFilter(t *testing.T) {
	f, mutating, fault := sortedListSetupWithFault(t, newMutatingAfterMembersStore)
	self := f.createAgent(t, "rs-self", string(state.PhaseRunning), nil)
	raced := f.createAgent(t, "rs-raced", string(state.PhaseStopped), map[string]string{"team": "a", "extra": "1"})
	tok := f.agentJWTFor(t, self.ID)

	newLabels := map[string]string{"team": "a", "extra": "2"}
	mutating.agentID, mutating.newLabels = raced.ID, newLabels
	fault.Arm()
	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&fit=500&label=team=a"), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Len(t, resp.Agents, 1, "the changed row still matches label=team=a and must be kept")
	assert.Equal(t, raced.ID, resp.Agents[0].ID)
	assert.Equal(t, newLabels, resp.Agents[0].Labels, "the response must carry the full row, not the member snapshot")
	assert.Len(t, emitter.records, 4+8*1)

	legacyRec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("label=team=a"), nil, tok)
	require.Equal(t, http.StatusOK, legacyRec.Code, legacyRec.Body.String())
	legacy := mustDecodeListAgentsResponse(t, legacyRec.Body)
	require.Len(t, legacy.Agents, 1)
	require.NotNil(t, resp.Agents[0].Cap)
	assert.Equal(t, legacy.Agents[0].Cap, resp.Agents[0].Cap)
}
