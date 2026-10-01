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
	"net/url"
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

// --- B3: sorted mode must clamp limit to 500 -------------------------------

// TestListProjectAgentsSorted_LimitClampedTo500 reproduces the r1 review's
// exact finding: 700 agents with limit=700 used to return 700 items and
// cost 5,605 decisions (over the A15 bound). Clamping limit to 500 (design
// 4.1: "limit 1..500, Unchanged") caps both.
func TestListProjectAgentsSorted_LimitClampedTo500(t *testing.T) {
	f := sortedListSetup(t)
	const n = 700
	f.createAgentsBulk(t, n, "clamp", string(state.PhaseStopped), nil)

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(fmt.Sprintf("sort=updated&limit=%d", n)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	assert.Len(t, resp.Agents, 500, "limit must be clamped to 500 even when the request asks for more")
	assert.Equal(t, n, resp.TotalCount)
	assert.NotEmpty(t, resp.NextCursor)

	want := 5 + n + 7*500
	assert.Len(t, emitter.records, want, "decision cost must reflect the clamped page size (500), not the requested limit (700)")
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

// --- N6: legacy mode ignores fit/stats/dir, byte-identical apart from serverTime ---

// TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical is N6 (EM
// ruling, architect-confirmed r8 erratum): without "sort", fit/stats/dir are
// silently ignored and the response is byte-identical to the same request
// without them, apart from serverTime.
func TestListProjectAgentsLegacy_IgnoresFitStatsDir_ByteIdentical(t *testing.T) {
	f := sortedListSetup(t)
	f.createAgent(t, "n6-a", string(state.PhaseRunning), nil)
	f.createAgent(t, "n6-b", string(state.PhaseStopped), nil)

	plain := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath(""), nil)
	require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())

	withExtras := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.listPath("fit=500&stats=1&dir=asc"), nil)
	require.Equal(t, http.StatusOK, withExtras.Code, withExtras.Body.String())

	var plainBody, extrasBody map[string]interface{}
	require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &plainBody))
	require.NoError(t, json.Unmarshal(withExtras.Body.Bytes(), &extrasBody))
	delete(plainBody, "serverTime")
	delete(extrasBody, "serverTime")
	assert.Equal(t, plainBody, extrasBody, "fit/stats/dir without sort must be silently ignored: byte-identical apart from serverTime")
}
