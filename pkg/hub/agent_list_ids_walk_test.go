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

package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frozenWalk pages a sorted agent list the way the web client does for a
// paged view (ptone/scion#3744): page 0 with stats=1, whose population
// freezes the walk order, then every later page by ids= over that order.
// mutate runs after each page (with the page index just fetched), so a
// test can change sort keys mid-walk. It returns every id received, in
// order, and checks that no page returns an id it did not ask for.
func frozenWalk(t *testing.T, srv *Server, user *store.User, path func(string) string, dir string, pageSize int, mutate func(page int)) []string {
	return frozenWalkInjecting(t, srv, user, path, dir, pageSize, nil, mutate)
}

// frozenWalkInjecting is frozenWalk, additionally naming the inject ids
// (unreadable or nonexistent agents) in every ids page, so the per-row read
// pass, not the client's choice of ids, is what keeps them out. The page
// size is raised by len(inject) so the request stays within its cap.
func frozenWalkInjecting(t *testing.T, srv *Server, user *store.User, path func(string) string, dir string, pageSize int, inject []string, mutate func(page int)) []string {
	t.Helper()
	base := "sort=updated&dir=" + dir + "&limit=" + strconv.Itoa(pageSize)

	rec := doRequestAsUser(t, srv, user, http.MethodGet, path(base+"&stats=1"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page0 := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotNil(t, page0.Stats)
	require.NotNil(t, page0.Stats.Agents, "the population freezes the walk order")
	order := make([]string, 0, len(*page0.Stats.Agents))
	for _, pair := range *page0.Stats.Agents {
		order = append(order, pair[0])
	}
	var got []string
	for i, a := range page0.Agents {
		require.Less(t, i, len(order))
		assert.Equal(t, order[i], a.ID, "page 0 is the head of the frozen order")
		got = append(got, a.ID)
	}
	if mutate != nil {
		mutate(0)
	}

	for page := 1; page*pageSize < len(order); page++ {
		end := (page + 1) * pageSize
		if end > len(order) {
			end = len(order)
		}
		want := order[page*pageSize : end]
		named := append(append([]string{}, want...), inject...)
		q := "sort=updated&dir=" + dir + "&limit=" + strconv.Itoa(pageSize+len(inject)) +
			"&ids=" + url.QueryEscape(strings.Join(named, ","))
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Empty(t, resp.NextCursor, "an ids page is answered in one page")
		assert.Equal(t, len(resp.Agents), resp.TotalCount, "an ids page counts only its readable matches")
		asked := make(map[string]bool, len(want))
		for _, id := range want {
			asked[id] = true
		}
		for _, a := range resp.Agents {
			require.True(t, asked[a.ID], "page %d returned %s, which it did not ask for", page, a.ID)
			got = append(got, a.ID)
		}
		if mutate != nil {
			mutate(page)
		}
	}
	return got
}

// bumpSortKey moves an agent's updated sort key to now, as a heartbeat or
// an activity event does.
func bumpSortKey(t *testing.T, s store.Store, id string, n int) {
	t.Helper()
	activity := "executing"
	if n%2 == 1 {
		activity = "idle"
	}
	require.NoError(t, s.UpdateAgentStatus(context.Background(), id, store.AgentStatusUpdate{Activity: activity}))
}

// TestAgentListIDsWalk_FrozenOrderSurvivesSortKeyChanges is the gate test
// for ptone/scion#3744. A caller who can read only some agents, mixed
// through the sort order, walks the project and the global lists with
// page sizes 3 to 5 in both directions while sort keys change mid-walk
// (agents not yet shown, agents already shown, readable and unreadable).
// Every readable agent is returned exactly once and no unreadable agent
// ever appears, on every page.
func TestAgentListIDsWalk_FrozenOrderSurvivesSortKeyChanges(t *testing.T) {
	f := readRuleSetup(t, 24, func(i int) bool { return i%3 != 1 })
	require.Len(t, f.readable, 16)
	readable := make(map[string]bool, len(f.readable))
	for _, id := range f.readable {
		readable[id] = true
	}
	var unreadable []string
	for _, id := range f.all {
		if !readable[id] {
			unreadable = append(unreadable, id)
		}
	}
	require.NotEmpty(t, unreadable)

	paths := map[string]func(string) string{"project": f.listPath, "global": f.globalPath}
	bumps := 0
	for name, path := range paths {
		for _, dir := range []string{"desc", "asc"} {
			for _, pageSize := range []int{3, 4, 5} {
				t.Run(fmt.Sprintf("%s/%s/limit=%d", name, dir, pageSize), func(t *testing.T) {
					// The reference walk order, to pick agents ahead of and
					// behind the walk position.
					order := frozenWalk(t, f.srv, f.caller, path, dir, pageSize, nil)
					require.ElementsMatch(t, f.readable, order)

					// Every ids page also names an unreadable and a nonexistent
					// agent: only the per-row read pass keeps them out.
					inject := []string{unreadable[0], uuid.NewString()}
					got := frozenWalkInjecting(t, f.srv, f.caller, path, dir, pageSize, inject, func(page int) {
						if page > 1 {
							return
						}
						// One agent already shown, two not yet shown, and an
						// unreadable one: each moves to the newest key.
						shown := order[page*pageSize]
						ahead := order[len(order)-1-page]
						ahead2 := order[len(order)/2+page]
						for _, id := range []string{shown, ahead, ahead2, unreadable[page%len(unreadable)]} {
							bumps++
							bumpSortKey(t, f.store, id, bumps)
						}
					})
					// The bumps really reordered the list (the test is not
					// vacuous): a fresh walk now starts from a different order.
					after := frozenWalk(t, f.srv, f.caller, path, dir, pageSize, nil)
					assert.NotEqual(t, order, after, "sort keys changed mid-walk")

					assert.Len(t, got, len(f.readable), "each readable agent exactly once")
					assert.ElementsMatch(t, f.readable, got, "the walk is exactly the readable set")
					for _, id := range got {
						assert.True(t, readable[id], "unreadable agent %s returned", id)
					}
				})
			}
		}
	}
}

// TestAgentListIDs_NamesUnreadableOrNonexistent pins that ids= only
// narrows: naming unreadable or nonexistent ids returns only the readable
// matches, and a request naming only unreadable ids looks exactly like one
// naming only ids that do not exist.
func TestAgentListIDs_NamesUnreadableOrNonexistent(t *testing.T) {
	f := readRuleSetup(t, 12, func(i int) bool { return i%2 == 0 })
	readable := make(map[string]bool)
	for _, id := range f.readable {
		readable[id] = true
	}
	var unreadable []string
	for _, id := range f.all {
		if !readable[id] {
			unreadable = append(unreadable, id)
		}
	}
	nonexistent := []string{uuid.NewString(), uuid.NewString()}

	for name, path := range map[string]func(string) string{"project": f.listPath, "global": f.globalPath} {
		get := func(ids []string, extra string) ListAgentsResponse {
			q := "sort=updated&limit=5&ids=" + url.QueryEscape(strings.Join(ids, ",")) + extra
			rec := doRequestAsUser(t, f.srv, f.caller, http.MethodGet, path(q), nil)
			require.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
			return mustDecodeListAgentsResponse(t, rec.Body)
		}

		mixed := get([]string{f.readable[0], unreadable[0], nonexistent[0], f.readable[1], unreadable[1]}, "&stats=1")
		var ids []string
		for _, a := range mixed.Agents {
			ids = append(ids, a.ID)
		}
		assert.ElementsMatch(t, []string{f.readable[0], f.readable[1]}, ids, "%s: only the readable matches", name)
		assert.Equal(t, 2, mixed.TotalCount, "%s: totalCount covers only readable matches", name)
		require.NotNil(t, mixed.Stats)
		assert.Equal(t, 2, mixed.Stats.Total, "%s: stats cover only readable matches", name)
		if mixed.Stats.Agents != nil {
			assert.Len(t, *mixed.Stats.Agents, 2, "%s: stats population covers only readable matches", name)
		}

		onlyUnreadable := get(unreadable[:2], "")
		onlyMissing := get(nonexistent, "")
		for label, r := range map[string]ListAgentsResponse{"unreadable": onlyUnreadable, "nonexistent": onlyMissing} {
			assert.Empty(t, r.Agents, "%s %s", name, label)
			assert.Equal(t, 0, r.TotalCount, "%s %s", name, label)
			assert.Empty(t, r.NextCursor, "%s %s", name, label)
			assert.False(t, r.TotalCountApproximate, "%s %s", name, label)
		}
		assert.Equal(t, onlyUnreadable.Capabilities, onlyMissing.Capabilities, "%s: same scope capabilities", name)

		// ids= combines with the other filters: a phase that no named
		// agent has returns nothing.
		none := get(f.readable[:3], "&phase=running")
		assert.Empty(t, none.Agents, "%s: phase still applies with ids", name)
	}

	// On the global endpoint ids= ANDs with id=: only ids named by both
	// come back, and no overlap matches nothing.
	both := func(idParams []string, ids []string) []string {
		q := "sort=updated&limit=5&ids=" + url.QueryEscape(strings.Join(ids, ","))
		for _, id := range idParams {
			q += "&id=" + url.QueryEscape(id)
		}
		rec := doRequestAsUser(t, f.srv, f.caller, http.MethodGet, f.globalPath(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out []string
		for _, a := range mustDecodeListAgentsResponse(t, rec.Body).Agents {
			out = append(out, a.ID)
		}
		return out
	}
	assert.ElementsMatch(t, []string{f.readable[1]},
		both([]string{f.readable[0], f.readable[1]}, []string{f.readable[1], f.readable[2]}),
		"id= and ids= intersect")
	assert.Empty(t, both([]string{f.readable[0]}, []string{f.readable[1]}), "no overlap matches nothing")
}

// TestAgentListIDs_AgentJWT pins ids= on the project agent-JWT path: ids of
// agents in the token's own project come back, an id of an agent in
// another project never does.
func TestAgentListIDs_AgentJWT(t *testing.T) {
	f := sortedListSetup(t)
	ctx := context.Background()
	self := f.createAgent(t, "ids-jwt-self", "running", nil)
	sibling := f.createAgent(t, "ids-jwt-sibling", "stopped", nil)

	other := &store.Project{
		ID: tid("ids-jwt-other"), Name: "Other", Slug: "ids-jwt-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateProject(ctx, other))
	foreign := &store.Agent{
		ID: tid("ids-jwt-foreign"), Slug: "ids-jwt-foreign", Name: "ids-jwt-foreign",
		ProjectID: other.ID, Phase: "stopped", CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, foreign))

	tok := f.agentJWTFor(t, self.ID)
	q := "sort=updated&limit=5&ids=" + url.QueryEscape(strings.Join([]string{sibling.ID, foreign.ID, self.ID}, ","))
	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(q), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	var ids []string
	for _, a := range resp.Agents {
		ids = append(ids, a.ID)
	}
	assert.ElementsMatch(t, []string{sibling.ID, self.ID}, ids, "own-project ids only")
	assert.Equal(t, 2, resp.TotalCount)

	rec = doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath("sort=updated&limit=5&ids="+foreign.ID), nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, mustDecodeListAgentsResponse(t, rec.Body).Agents, "a foreign-project id never comes back")
}

// TestAgentListIDs_Validation pins the 400s: each message is fixed and
// names no id, whatever the ids are, and ids is refused outside sorted
// mode and together with cursor or fit.
func TestAgentListIDs_Validation(t *testing.T) {
	f := readRuleSetup(t, 6, func(i int) bool { return i%2 == 0 })
	nonexistent := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}

	cases := []struct {
		name, query, msg string
	}{
		{"too many (readable)", "sort=updated&limit=3&ids=" + strings.Join(f.all[:4], ","), "too many ids"},
		{"too many (nonexistent)", "sort=updated&limit=3&ids=" + strings.Join(nonexistent, ","), "too many ids"},
		{"too many above the hard cap", "sort=updated&limit=500&ids=" + strings.Repeat(nonexistent[0]+",", maxAgentListIDs) + nonexistent[1], "too many ids"},
		{"malformed", "sort=updated&limit=5&ids=" + f.readable[0] + ",not-a-uuid", "invalid ids"},
		{"uppercase", "sort=updated&limit=5&ids=" + strings.ToUpper(f.readable[0]), "invalid ids"},
		{"with cursor", "sort=updated&limit=5&cursor=abc&ids=" + f.readable[0], "ids is not valid together with cursor"},
		{"with fit", "sort=updated&limit=5&fit=10&ids=" + f.readable[0], "ids is not valid together with fit"},
		{"legacy mode", "limit=5&ids=" + f.readable[0], "ids requires sort"},
	}
	for name, path := range map[string]func(string) string{"project": f.listPath, "global": f.globalPath} {
		for _, tc := range cases {
			rec := doRequestAsUser(t, f.srv, f.caller, http.MethodGet, path(tc.query), nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s %s: %s", name, tc.name, rec.Body.String())
			body := rec.Body.String()
			assert.Contains(t, body, tc.msg, "%s %s", name, tc.name)
			for _, id := range append(append([]string{}, f.all...), nonexistent...) {
				assert.NotContains(t, body, id, "%s %s: the 400 names no id", name, tc.name)
			}
		}
	}

	// Duplicates collapse; an empty ids= is no filter at all.
	rec := doRequestAsUser(t, f.srv, f.caller, http.MethodGet,
		f.listPath("sort=updated&limit=2&ids="+f.readable[0]+","+f.readable[0]), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, mustDecodeListAgentsResponse(t, rec.Body).Agents, 1)
	rec = doRequestAsUser(t, f.srv, f.caller, http.MethodGet, f.listPath("sort=updated&limit=50&ids="), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, mustDecodeListAgentsResponse(t, rec.Body).Agents, len(f.readable))
}

// TestNarrowFilterByIDs pins the helper's edge cases: a nil filter is a
// no-op, no ids leave the filter alone, and an existing set is intersected.
func TestNarrowFilterByIDs(t *testing.T) {
	assert.NotPanics(t, func() { narrowFilterByIDs(nil, []string{"a"}) })

	f := store.AgentFilter{}
	narrowFilterByIDs(&f, nil)
	assert.Nil(t, f.IDs)
	narrowFilterByIDs(&f, []string{"a", "b"})
	assert.Equal(t, []string{"a", "b"}, f.IDs)
	narrowFilterByIDs(&f, []string{"b", "c"})
	assert.Equal(t, []string{"b"}, f.IDs)
	narrowFilterByIDs(&f, []string{"c"})
	assert.NotNil(t, f.IDs)
	assert.Empty(t, f.IDs, "no overlap matches nothing")
}
