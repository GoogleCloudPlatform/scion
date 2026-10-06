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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
