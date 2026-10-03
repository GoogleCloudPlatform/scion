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

// This file covers includeDeleted semantics and the step 5a re-decision
// protocol's exact-decision-count and short-page-continuation behavior for
// rows that change, disappear, or move between the member read and the
// full-row read.

// --- includeDeleted=true must behave like legacy mode ------------------

func softDeleteAgentForTest(t *testing.T, s store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	a.DeletedAt = time.Now().UTC()
	require.NoError(t, s.UpdateAgent(ctx, a))
}

// TestListProjectAgentsSorted_IncludeDeleted_Complete reproduces and fixes a
// bug where a complete response with includeDeleted=true used to silently
// drop the soft-deleted agent (classified as "missing between the two
// reads" by GetAgentsByIDs's hard-coded DeletedAtIsNil()), while
// CountAgents/ListAgentMembers/stats already honored IncludeDeleted -- so
// the page, totalCount and stats disagreed with each other and with legacy
// mode.
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

// --- the OwnerID-change race --------------------------------------

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
		a, gerr := o.GetAgent(ctx, o.agentID)
		if gerr != nil {
			return
		}
		a.OwnerID = o.newOwnerID
		_ = o.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_Race_OwnerChange_BecomesUnreadable proves: a
// candidate owned by the caller (readable only via the owner relationship
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

// --- exact decision counts for missing-row / project-drop ---------

// TestListProjectAgentsSorted_Race_MissingRow_ExactDecisionCount extends the
// existing missing-row race test with the exact decision count required
// (a dropped row costs no additional decision): 1 (gate) + 1 (step-3
// read) + 0 (step 5a drop) + 4 (scope caps) = 6.
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
// project-mismatch analogue of the above: 1 (gate) + 1 (step-3 read) + 0
// (step 5a drop, ProjectID check) + 4 (scope caps) = 6.
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

// --- missing-row drop in paged mode must leave a valid short page ---

// TestListProjectAgentsSorted_Race_MissingRow_PagedShortPageContinues
// proves: a row dropped as "missing" inside a page must produce a short page
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

// --- nil-vs-empty Labels/Ancestry costs zero re-decisions, end to end ---

// labelsNilToEmptyAfterMembersStore rewrites an agent's Labels from nil to a
// non-nil empty map (via the real store) after the first ListAgentMembers
// call, simulating the narrow and full decoders disagreeing on "no labels"
// representation within one request.
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
		a, gerr := l.GetAgent(ctx, l.agentID)
		if gerr != nil {
			return
		}
		a.Labels = map[string]string{}
		_ = l.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_NilVsEmptyLabels_EndToEndZeroRedecisions
// proves: the nil/empty-Labels normalization must cost zero re-decisions
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
// the first ListAgentMembers call: closes a gap where the original
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
		a, gerr := f.GetAgent(ctx, f.agentID)
		if gerr != nil {
			return
		}
		f.mutate(a)
		_ = f.UpdateAgent(ctx, a)
	})
	return members, nil
}

// TestListProjectAgentsSorted_NilVsEmpty_TableDriven_EndToEndZeroRedecisions
// is the nil/empty normalization end-to-end proof, extended
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
