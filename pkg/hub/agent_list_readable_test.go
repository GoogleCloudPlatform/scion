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
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRuleFixture is one project whose agents have three owners: the
// project owner, a project member, and a caller holding only agent.list on
// the project. The caller can read exactly the agents it owns.
type readRuleFixture struct {
	*sortedListFixture
	caller   *store.User
	readable []string // ids the caller can read, sorted
	all      []string // every agent id in the project, sorted
}

func readRuleSetup(t *testing.T, n int, callerOwns func(i int) bool) *readRuleFixture {
	t.Helper()
	f := &readRuleFixture{sortedListFixture: sortedListSetup(t)}
	ctx := context.Background()
	f.caller = &store.User{
		ID: tid("rr-caller"), Email: "rr-caller@test.com", DisplayName: "Caller",
		Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, f.caller))
	ensureHubMembership(ctx, f.store, f.caller.ID)
	grantProjectListOnly(t, f.store, f.caller.ID, f.project.ID, "rr-list-only")

	agents := f.createAgentsBulk(t, n, "rr", string(state.PhaseStopped), func(i int) string {
		switch {
		case callerOwns(i):
			return f.caller.ID
		case i%2 == 0:
			return f.member.ID
		default:
			return f.owner.ID
		}
	})
	for i, a := range agents {
		f.all = append(f.all, a.ID)
		if callerOwns(i) {
			f.readable = append(f.readable, a.ID)
		}
	}
	sort.Strings(f.all)
	sort.Strings(f.readable)
	return f
}

func (f *readRuleFixture) globalPath(query string) string {
	return "/api/v1/agents?projectId=" + f.project.ID + "&" + query
}

// walk follows nextCursor from the first page to the last, asserting that
// every page reports wantTotal as an exact total, and returns every id seen
// in order.
func walk(t *testing.T, srv *Server, user *store.User, path func(string) string, query string, wantTotal int) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 100, "walk did not terminate")
		q := query
		if cursor != "" {
			// fit applies to the first page only; it is not valid with
			// a cursor.
			v, err := url.ParseQuery(query)
			require.NoError(t, err)
			v.Del("fit")
			v.Set("cursor", cursor)
			q = v.Encode()
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, wantTotal, resp.TotalCount, "query %q page %d: totalCount", query, page)
		assert.False(t, resp.TotalCountApproximate, "query %q page %d: total below the cap is exact", query, page)
		for _, a := range resp.Agents {
			ids = append(ids, a.ID)
		}
		if resp.NextCursor == "" {
			return ids
		}
		cursor = resp.NextCursor
	}
}

var readRuleModes = []string{
	"limit=500",                      // legacy
	"sort=updated&limit=500",         // sorted, paged
	"sort=created&dir=asc&limit=500", // sorted, paged, other order
	"sort=updated&fit=500&limit=500", // sorted, fit (complete)
}

// TestAgentListReadRule_GlobalAndProjectReturnSameReadableSet is the parity
// test for ptone/scion#3346: for one user and one project with several
// agent owners, the global and project agent lists return the same set, in
// every mode, and that set is exactly the agents the user can read.
func TestAgentListReadRule_GlobalAndProjectReturnSameReadableSet(t *testing.T) {
	f := readRuleSetup(t, 12, func(i int) bool { return i%3 == 0 })
	require.Len(t, f.readable, 4)

	for _, mode := range readRuleModes {
		global := walk(t, f.srv, f.caller, f.globalPath, mode, len(f.readable))
		project := walk(t, f.srv, f.caller, f.listPath, mode, len(f.readable))
		assert.Equal(t, f.readable, sortedCopy(global), "%s: global list is the readable set", mode)
		assert.Equal(t, f.readable, sortedCopy(project), "%s: project list is the readable set", mode)
		assert.Equal(t, global, project, "%s: both endpoints return the same agents in the same order", mode)

		// A member who can read every agent sees all of them on both.
		assert.Equal(t, f.all, sortedCopy(walk(t, f.srv, f.member, f.globalPath, mode, len(f.all))), "%s: member global", mode)
		assert.Equal(t, f.all, sortedCopy(walk(t, f.srv, f.member, f.listPath, mode, len(f.all))), "%s: member project", mode)
	}
}

// TestAgentListReadRule_PagingWithPageSmallerThanReadableSet pins paging
// under the rule: with a page size smaller than the readable set, every
// page carries the exact readable total, the walk returns each readable
// agent once, and no unreadable agent appears.
func TestAgentListReadRule_PagingWithPageSmallerThanReadableSet(t *testing.T) {
	f := readRuleSetup(t, 30, func(i int) bool { return i%4 == 1 })
	require.Len(t, f.readable, 8)

	for _, mode := range []string{"limit=3", "sort=updated&limit=3", "sort=created&dir=asc&limit=2", "sort=updated&fit=5&limit=3"} {
		for name, path := range map[string]func(string) string{"global": f.globalPath, "project": f.listPath} {
			ids := walk(t, f.srv, f.caller, path, mode, len(f.readable))
			assert.Len(t, ids, len(f.readable), "%s %s: each readable agent once", name, mode)
			assert.Equal(t, f.readable, sortedCopy(ids), "%s %s: the walk is the readable set", name, mode)
		}
	}
}

// TestAgentListReadRule_ApproximateFlagFollowsCandidateCount pins the
// above-cap behaviour: with more than authorizedListMaxCandidates agents
// in scope, two callers whose readable subsets differ both get
// totalCountApproximate=true and the same response shape and status, on
// both endpoints, so the flag follows the candidate count rather than
// what the caller can read.
func TestAgentListReadRule_ApproximateFlagFollowsCandidateCount(t *testing.T) {
	f := readRuleSetup(t, authorizedListMaxCandidates+1, func(i int) bool { return i < 3 })

	keys := func(body []byte) []string {
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &raw))
		var out []string
		for k := range raw {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, mode := range []string{"limit=2", "sort=updated&limit=2"} {
		paths := map[string]func(string) string{"global": f.globalPath}
		if mode == "limit=2" {
			// The project endpoint's sorted mode refuses a candidate set
			// above the cap outright (422), for every caller alike.
			paths["project"] = f.listPath
		}
		for name, path := range paths {
			var shapes [][]string
			for _, user := range []*store.User{f.caller, f.member} {
				rec := doRequestAsUser(t, f.srv, user, http.MethodGet, path(mode), nil)
				require.Equal(t, http.StatusOK, rec.Code, "%s %s %s: %s", name, mode, user.ID, rec.Body.String())
				resp := mustDecodeListAgentsResponse(t, rec.Body)
				assert.True(t, resp.TotalCountApproximate, "%s %s %s: flag set above the cap", name, mode, user.ID)
				shapes = append(shapes, keys(rec.Body.Bytes()))
			}
			assert.Equal(t, shapes[0], shapes[1], "%s %s: same response shape for both callers", name, mode)
		}
	}
}

// failingBindingsStore fails the role-binding loads whose 1-based call
// numbers are in failOn, counted from when it is armed, and passes every
// other call through.
type failingBindingsStore struct {
	store.Store
	mu     sync.Mutex
	armed  bool
	calls  int
	failOn map[int]bool
	failed int
}

func (s *failingBindingsStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.mu.Lock()
	fail := false
	if s.armed {
		s.calls++
		fail = s.failOn[s.calls]
		if fail {
			s.failed++
		}
	}
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected role binding load fault")
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// TestAgentListReadRule_DecisionErrorDropsTheRow pins fail-closed row
// filtering: when the read decision for one row errors, that row is
// dropped from both the page and the total, the other readable rows are
// kept, and the request does not fail.
func TestAgentListReadRule_DecisionErrorDropsTheRow(t *testing.T) {
	f := readRuleSetup(t, 6, func(i int) bool { return i >= 3 })
	// The first row in legacy order (the newest) is decided first in both
	// of authorizedList's passes. Each pass runs one AuthorizeReadBatch
	// call with its own input memo, and a failed load is not memoized, so
	// the first row's decision is the first binding load of each pass:
	// call 1 (count pass) and call 3 (fill pass; call 2 is the second
	// row's successful load, which the rest of that pass reuses).
	failing := &failingBindingsStore{Store: f.store, failOn: map[int]bool{1: true, 3: true}}
	f.srv.store = failing
	f.srv.authzService.store = failing
	defer func() {
		f.srv.store = f.store
		f.srv.authzService.store = f.store
	}()

	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser(f.caller.ID, f.caller.Email, f.caller.DisplayName, f.caller.Role, "test"))
	identity := GetIdentityFromContext(ctx)
	filter := store.AgentFilter{ProjectID: f.project.ID}

	clean, err := f.srv.listAgentsLegacyPage(ctx, identity, filter, "", "rr-binding", 500)
	require.NoError(t, err)
	require.Len(t, clean.Items, 3)
	require.Equal(t, 3, clean.TotalCount)

	failing.armed = true
	result, err := f.srv.listAgentsLegacyPage(ctx, identity, filter, "", "rr-binding", 500)
	require.NoError(t, err, "a row decision error is a denial, not a request failure")
	require.Equal(t, 2, failing.failed)
	assert.Equal(t, 2, result.TotalCount, "the errored row is dropped from the total")
	require.Len(t, result.Items, 2, "the errored row is dropped from the page")
	assert.NotEqual(t, clean.Items[0].ID, result.Items[0].ID, "the first row's decision errored, so it is the one dropped")
	assert.Equal(t, clean.Items[1].ID, result.Items[0].ID)
	assert.Equal(t, clean.Items[2].ID, result.Items[1].ID)
}

// tokenWalk is walk for a user access token caller.
func tokenWalk(t *testing.T, srv *Server, key string, path func(string) string, query string) ([]string, []AgentWithCapabilities) {
	t.Helper()
	var ids []string
	var items []AgentWithCapabilities
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 100, "walk did not terminate")
		q := query
		if cursor != "" {
			v, err := url.ParseQuery(query)
			require.NoError(t, err)
			v.Del("fit")
			v.Set("cursor", cursor)
			q = v.Encode()
		}
		rec := doRequestWithUAT(t, srv, key, http.MethodGet, path(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", query, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			ids = append(ids, a.ID)
		}
		items = append(items, resp.Agents...)
		if resp.NextCursor == "" {
			return ids, items
		}
		cursor = resp.NextCursor
	}
}

// TestAgentListReadRule_ListScopedTokenSeesRowsItsHolderCanRead pins the
// token-scope part of the rule: a token holding agent:list but not
// agent:read lists, on both endpoints and in every mode, exactly the
// agents its holder can read. The holder's per-agent check still decides
// every row, so the agents of other owners stay out. Each row's
// capabilities are the token's real per-agent capabilities, so none
// carries read.
func TestAgentListReadRule_ListScopedTokenSeesRowsItsHolderCanRead(t *testing.T) {
	f := readRuleSetup(t, 9, func(i int) bool { return i%3 == 0 })
	keys := map[string]string{
		"project-bound": mintScopedUAT(t, f.srv, f.caller.ID, f.project.ID, []string{"agent:list"}),
	}
	hubKey, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.caller.ID), CreateTokenParams{
		UserID: f.caller.ID, Name: "rr-hub", Boundary: hubBoundary(), Scopes: []string{"agent:list"},
	})
	require.NoError(t, err)
	keys["hub-bound"] = hubKey

	for name, key := range keys {
		for _, mode := range readRuleModes {
			for ep, path := range map[string]func(string) string{"global": f.globalPath, "project": f.listPath} {
				ids, items := tokenWalk(t, f.srv, key, path, mode)
				assert.Equal(t, f.readable, sortedCopy(ids), "%s %s %s: the rows the holder can read", name, ep, mode)
				for _, a := range items {
					require.NotNil(t, a.Cap, "%s %s %s: row capabilities present", name, ep, mode)
					assert.NotContains(t, a.Cap.Actions, string(ActionRead), "%s %s %s: no read capability without agent:read", name, ep, mode)
				}
			}
		}
	}

	// The holder's full-access member token, for contrast, lists every
	// agent and carries read on each row.
	memberKey := mintScopedUAT(t, f.srv, f.member.ID, f.project.ID, []string{"agent:list", "agent:read"})
	ids, items := tokenWalk(t, f.srv, memberKey, f.listPath, "sort=updated&limit=500")
	assert.Equal(t, f.all, sortedCopy(ids))
	for _, a := range items {
		assert.Contains(t, a.Cap.Actions, string(ActionRead))
	}
}

// TestAgentListReadRule_ProjectReadTokenListsItsProjectOnly pins the
// project:read part of the rule on the global list: a token bound to one
// project with project:read lists that project's agents its holder (a
// project member) can read, and nothing from another project the holder
// can also read.
func TestAgentListReadRule_ProjectReadTokenListsItsProjectOnly(t *testing.T) {
	f := readRuleSetup(t, 6, func(i int) bool { return i%2 == 0 })
	ctx := context.Background()
	other := &store.Project{
		ID: tid("rr-other"), Name: "Other", Slug: "rr-other",
		OwnerID: f.member.ID, CreatedBy: f.member.ID,
	}
	require.NoError(t, f.store.CreateProject(ctx, other))
	createTestUserWithProjectRole(t, f.store, f.member.ID, f.member.Email, other.ID, store.ProjectRoleOwner)
	otherAgent := &store.Agent{
		ID: tid("rr-other-agent"), Slug: "rr-other-agent", Name: "rr-other-agent",
		ProjectID: other.ID, Phase: string(state.PhaseStopped), CreatedBy: f.member.ID, OwnerID: f.member.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, otherAgent))

	key := mintScopedUAT(t, f.srv, f.member.ID, f.project.ID, []string{"project:read"})
	all := func(q string) string { return "/api/v1/agents?" + q }
	for _, mode := range readRuleModes {
		ids, items := tokenWalk(t, f.srv, key, all, mode)
		assert.Equal(t, f.all, sortedCopy(ids), "%s: the bound project's readable agents only", mode)
		assert.NotContains(t, ids, otherAgent.ID, "%s: another project's agent stays out", mode)
		for _, a := range items {
			assert.NotContains(t, a.Cap.Actions, string(ActionRead), "%s: no read capability without agent:read", mode)
		}
	}
}

// TestAgentListReadRule_SingleAgentGetStillNeedsAgentRead pins that the
// list rule does not reach the single-agent read: a token holding
// agent:list but not agent:read lists an agent it cannot then fetch, on
// either path.
func TestAgentListReadRule_SingleAgentGetStillNeedsAgentRead(t *testing.T) {
	f := readRuleSetup(t, 3, func(i int) bool { return i == 0 })
	key := mintScopedUAT(t, f.srv, f.caller.ID, f.project.ID, []string{"agent:list"})
	ids, _ := tokenWalk(t, f.srv, key, f.listPath, "limit=500")
	require.Equal(t, f.readable, ids)

	for _, path := range []string{
		"/api/v1/agents/" + f.readable[0],
		"/api/v1/projects/" + f.project.ID + "/agents/" + f.readable[0],
	} {
		rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, path, nil)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, "%s: %s", path, rec.Body.String())
	}
	// A token with agent:read fetches the same agent (the member can read
	// every agent in the project).
	readKey := mintScopedUAT(t, f.srv, f.member.ID, f.project.ID, []string{"agent:read"})
	rec := doRequestWithUAT(t, f.srv, readKey, http.MethodGet, "/api/v1/agents/"+f.readable[0], nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestListRowReadCeiling covers the token-scope mapping directly: only an
// agent-list row read of an agent, on a ceiling with agent.list (or
// project.read on a token bound to the agent's project), gains agent.read.
func TestListRowReadCeiling(t *testing.T) {
	ceil := func(ids ...string) permissions.FrozenPermissionCeiling {
		return permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ids}
	}
	row := AuthzRequest{ListRow: true, Action: ActionRead, Resource: Resource{Type: "agent", ParentType: "project", ParentID: "p1"}}
	bound := &TokenBoundary{Kind: BoundaryKindProject, ProjectID: "p1"}
	otherBound := &TokenBoundary{Kind: BoundaryKindProject, ProjectID: "p2"}
	hub := &TokenBoundary{Kind: BoundaryKindHub}
	notRow := row
	notRow.ListRow = false
	project := row
	project.Resource = Resource{Type: "project", ID: "p1"}
	updateRow := row
	updateRow.Action = ActionUpdate
	noParent := row
	noParent.Resource = Resource{Type: "agent"}
	emptyBound := &TokenBoundary{Kind: BoundaryKindProject}

	for _, c := range []struct {
		name     string
		request  AuthzRequest
		perm     string
		boundary *TokenBoundary
		ceiling  permissions.FrozenPermissionCeiling
		want     bool
	}{
		{"agent.list", row, "agent.read", hub, ceil("agent.list"), true},
		{"project.read bound to the agent's project", row, "agent.read", bound, ceil("project.read"), true},
		{"project.read bound elsewhere", row, "agent.read", otherBound, ceil("project.read"), false},
		{"project.read on a hub token", row, "agent.read", hub, ceil("project.read"), false},
		{"neither scope", row, "agent.read", bound, ceil("agent.message"), false},
		{"not a list row", notRow, "agent.read", hub, ceil("agent.list"), false},
		{"other permission", row, "agent.update", hub, ceil("agent.list"), false},
		{"other resource", project, "agent.read", hub, ceil("agent.list"), false},
		{"unknown ceiling version", row, "agent.read", hub, permissions.FrozenPermissionCeiling{Version: 99, PermissionIDs: []string{"agent.list"}}, false},
		{"action not read", updateRow, "agent.read", hub, ceil("agent.list"), false},
		{"agent.list with no boundary", row, "agent.read", nil, ceil("agent.list"), true},
		{"project.read with no boundary", row, "agent.read", nil, ceil("project.read"), false},
		{"project.read, empty boundary project, no parent", noParent, "agent.read", emptyBound, ceil("project.read"), false},
		{"ceiling already holds agent.read", row, "agent.read", hub, ceil("agent.read"), true},
	} {
		got := listRowReadCeiling(c.request, c.perm, c.boundary, c.ceiling).Allows("agent.read")
		assert.Equal(t, c.want, got, c.name)
	}

	// A ceiling that already allows agent.read comes back unchanged.
	held := ceil("agent.read", "agent.list")
	assert.Equal(t, held, listRowReadCeiling(row, "agent.read", hub, held))

	// Widening never writes into the caller's PermissionIDs backing array,
	// even when it has spare capacity.
	backing := make([]string, 4)
	backing[0], backing[1] = "agent.list", "sentinel"
	input := ceil()
	input.PermissionIDs = backing[:1]
	widened := listRowReadCeiling(row, "agent.read", hub, input)
	assert.True(t, widened.Allows("agent.read"))
	assert.Equal(t, []string{"agent.list"}, input.PermissionIDs, "input slice unchanged")
	assert.Equal(t, "sentinel", backing[1], "input backing array unchanged")
}

// TestListRowReadCeiling_ActionAndBoundaryProjectChecks fails if either
// the Action check or the boundary.ProjectID check is dropped from
// listRowReadCeiling: every other condition holds in each case, so only
// that one check keeps agent.read out.
func TestListRowReadCeiling_ActionAndBoundaryProjectChecks(t *testing.T) {
	ceiling := permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.list", "project.read"}}

	// Action: the permission is agent.read and the request is a list row
	// of an agent, but the action is not read.
	update := AuthzRequest{ListRow: true, Action: ActionUpdate, Resource: Resource{Type: "agent", ParentType: "project", ParentID: "p1"}}
	assert.False(t, listRowReadCeiling(update, "agent.read", &TokenBoundary{Kind: BoundaryKindHub}, ceiling).Allows("agent.read"),
		"a non-read action never gains agent.read")

	// boundary.ProjectID: a project boundary with no project and an agent
	// with no parent would match on ProjectID == ParentID alone.
	projectOnly := permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"project.read"}}
	noParent := AuthzRequest{ListRow: true, Action: ActionRead, Resource: Resource{Type: "agent"}}
	assert.False(t, listRowReadCeiling(noParent, "agent.read", &TokenBoundary{Kind: BoundaryKindProject}, projectOnly).Allows("agent.read"),
		"an empty boundary project never matches an empty parent")
}

// TestAgentListReadRule_ScopedTokenRacedRowStaysListed covers the race
// path of the sorted project list for a token holding agent:list but not
// agent:read: every page row changes between the member read and the
// full-row read, so its decisions are redone on the full row. The plain
// read fails (no agent:read) and the list read passes, so the row stays,
// with no read capability.
func TestAgentListReadRule_ScopedTokenRacedRowStaysListed(t *testing.T) {
	f := readRuleSetup(t, 6, func(i int) bool { return i%2 == 0 })
	key := mintScopedUAT(t, f.srv, f.caller.ID, f.project.ID, []string{"agent:list"})
	f.srv.store = &racingAllMembersStore{Store: f.store}

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath("sort=updated&limit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)

	ids := make([]string, 0, len(resp.Agents))
	for _, a := range resp.Agents {
		ids = append(ids, a.ID)
		assert.Equal(t, "true", a.Labels["raced"], "%s: the row changed after the member read", a.ID)
		require.NotNil(t, a.Cap)
		assert.NotContains(t, a.Cap.Actions, string(ActionRead), "%s: no read capability without agent:read", a.ID)
	}
	assert.Equal(t, f.readable, sortedCopy(ids), "every raced row the holder can read stays listed")
	assert.Equal(t, len(f.readable), resp.TotalCount)
}

// TestAgentListReadRule_ScopedTokenRacedRowLeavesReads covers the race
// path of the sorted project list for a token holding agent:list but not
// agent:read, when the race moves a row out of the holder's reads: the
// row gets a new owner between the member read and the full-row read.
// The list read on the full row fails, so the row is dropped before its
// remaining actions are decided, and the complete response's totalCount
// follows the dropped row.
func TestAgentListReadRule_ScopedTokenRacedRowLeavesReads(t *testing.T) {
	f := readRuleSetup(t, 6, func(i int) bool { return i%2 == 0 })
	key := mintScopedUAT(t, f.srv, f.caller.ID, f.project.ID, []string{"agent:list"})
	require.NotEmpty(t, f.readable)
	moved := f.readable[0]
	f.srv.store = &ownerChangingAfterMembersStore{Store: f.store, agentID: moved, newOwnerID: f.owner.ID}

	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath("sort=updated&fit=500"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.Empty(t, resp.NextCursor, "the response is complete")

	// The list read on the full row drops the moved row before the
	// remaining actions are decided, so the moved row has no decision
	// record for any action other than read.
	var movedRemaining []string
	for _, r := range emitter.records {
		if r.ResourceType == "agent" && r.ResourceID == moved && r.Permission != string(ActionRead) {
			movedRemaining = append(movedRemaining, r.Permission)
		}
	}
	assert.Empty(t, movedRemaining, "the dropped row is not charged for the remaining actions")

	ids := make([]string, 0, len(resp.Agents))
	for _, a := range resp.Agents {
		ids = append(ids, a.ID)
	}
	want := append([]string{}, f.readable[1:]...)
	assert.NotContains(t, ids, moved, "a row moved out of the holder's reads is dropped")
	assert.Equal(t, want, sortedCopy(ids), "every other readable row stays listed")
	assert.Equal(t, len(want), resp.TotalCount, "totalCount follows the dropped row")
}

// TestAgentListReadRule_ListRowReadReasonMarked pins the audit Reason of
// an agent-list row read: a read allowed only through the list-row rule
// carries listRowReadReasonMarker, and a read allowed by the ceiling
// itself does not. It also pins the denied read records: a denied read is
// never marked, and the plain read decision for a listed row the caller
// owns is still recorded, and denied, when the ceiling lacks agent.read.
func TestAgentListReadRule_ListRowReadReasonMarked(t *testing.T) {
	f := readRuleSetup(t, 4, func(i int) bool { return i%2 == 0 })

	type readCounts struct {
		marked, unmarked int
		deniedMarked     int
		deniedByID       map[string]int
	}
	readReasons := func(key, query string) readCounts {
		t.Helper()
		emitter := &recordingDecisionAuditEmitter{}
		f.srv.authzService.SetDecisionAuditEmitter(emitter)
		rec := doRequestWithUAT(t, f.srv, key, http.MethodGet, f.listPath(query), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		c := readCounts{deniedByID: map[string]int{}}
		for _, r := range emitter.records {
			if r.ResourceType != "agent" || r.Permission != string(ActionRead) {
				continue
			}
			isMarked := strings.Contains(r.Reason, listRowReadReasonMarker)
			switch {
			case r.Result != "allow":
				c.deniedByID[r.ResourceID]++
				if isMarked {
					c.deniedMarked++
				}
			case isMarked:
				c.marked++
			default:
				c.unmarked++
			}
		}
		return c
	}

	listKey := mintScopedUAT(t, f.srv, f.caller.ID, f.project.ID, []string{"agent:list"})
	c := readReasons(listKey, "limit=500")
	// The legacy list decides each row twice: once in the count pass and
	// once in the fill pass.
	assert.Equal(t, 2*len(f.readable), c.marked, "each listed row's read is marked")
	assert.Zero(t, c.unmarked)
	assert.Zero(t, c.deniedMarked, "a denied read is never marked")
	for _, id := range f.readable {
		assert.GreaterOrEqual(t, c.deniedByID[id], 1, "%s: the listed row's plain read is recorded as denied", id)
	}

	// The sorted list decides one list read per candidate, then one plain
	// read per page row: each listed row has exactly one denied read
	// record (its plain read), and each row that is not listed has
	// exactly one (its list read).
	c = readReasons(listKey, "sort=updated&limit=500")
	assert.Equal(t, len(f.readable), c.marked, "each listed row's list read is marked")
	assert.Zero(t, c.unmarked)
	assert.Zero(t, c.deniedMarked, "a denied read is never marked")
	for _, id := range f.all {
		assert.Equal(t, 1, c.deniedByID[id], "%s: one denied read record", id)
	}

	// The member can read every agent in the project; its token holds
	// agent:read, so the row reads (and the row capability reads) are
	// allowed without widening.
	readKey := mintScopedUAT(t, f.srv, f.member.ID, f.project.ID, []string{"agent:list", "agent:read"})
	c = readReasons(readKey, "limit=500")
	assert.Zero(t, c.marked, "a ceiling holding agent.read is not widened")
	assert.GreaterOrEqual(t, c.unmarked, len(f.all))
}

// TestGlobalAgentStatsCapCoversCandidateBound pins the assumption
// buildGlobalAgentStats relies on to always send stats.agents when the
// member read is not truncated: the readable count never exceeds
// authorizedListMaxCandidates, which must not exceed globalAgentStatsCap.
func TestGlobalAgentStatsCapCoversCandidateBound(t *testing.T) {
	assert.LessOrEqual(t, authorizedListMaxCandidates, globalAgentStatsCap)
}
