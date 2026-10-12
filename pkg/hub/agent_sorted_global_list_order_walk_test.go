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
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderWalkAgent is one seeded agent with the attributes the expected-order
// computation needs, all chosen by the test rather than read back.
type orderWalkAgent struct {
	id, project, phase string
	labels             map[string]string
	created, updated   time.Time
	lastActivity       time.Time // zero means NULL
}

// seedOrderWalkTimes writes created, updated and last_activity_event for
// every agent in one transaction, as the verbose UTC text the store writes.
func seedOrderWalkTimes(t *testing.T, s store.Store, agents []orderWalkAgent) {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	tx, err := dbProvider.DB().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	for _, a := range agents {
		var lae any
		if !a.lastActivity.IsZero() {
			lae = a.lastActivity.String()
		}
		_, err := tx.Exec("UPDATE agents SET created = ?, updated = ?, last_activity_event = ? WHERE id = ?",
			a.created.String(), a.updated.String(), lae, a.id)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// expectedOrder filters agents with keep and returns their IDs in the
// agentsort reference order for (sortKey, dir).
func expectedOrder(agents []orderWalkAgent, sortKey, dir string, keep func(orderWalkAgent) bool) []string {
	var rows []agentsort.Row
	for _, a := range agents {
		if keep(a) {
			rows = append(rows, agentsort.KeyFor(sortKey, a.id, a.created, a.updated, a.lastActivity))
		}
	}
	agentsort.SortRows(dir, rows)
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// walkSortedPages follows nextCursor from the first page to the last and
// returns the concatenated IDs. Every page but the last must be full, and
// the last page must carry no cursor.
func walkSortedPages(t *testing.T, srv *Server, user *store.User, pathFor func(string) string, query string, limit int) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 2000, "walk did not terminate")
		q := fmt.Sprintf("%s&limit=%d", query, limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, pathFor(q), nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", q, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		for _, a := range resp.Agents {
			ids = append(ids, a.ID)
		}
		if resp.NextCursor == "" {
			return ids
		}
		require.Len(t, resp.Agents, limit, "%s: a page with a cursor must be full", q)
		cursor = resp.NextCursor
	}
}

// longOrderWalkEnv opts in to the 1200-agent order walk, which takes several
// minutes. The always-on walk is sized to keep the hub package inside its CI
// time budget.
const longOrderWalkEnv = "SCION_TEST_LONG_ORDER_WALK"

// TestListAgentsSorted_OrderWalkAcrossProjectsFiltersAndScopes runs the
// order walk over 520 agents, so page size 500 still pages twice over the
// unfiltered set in every sort and direction. Each filtered walk uses one
// page size that crosses pages; the combined filter also walks at page
// size 1.
func TestListAgentsSorted_OrderWalkAcrossProjectsFiltersAndScopes(t *testing.T) {
	runOrderWalkAcrossProjects(t, 520, false)
}

// TestListAgentsSorted_OrderWalkAcrossProjectsFiltersAndScopesLarge runs the
// walk over 1200 agents with every page size on every filter, and with
// scope=shared on its own. It runs only when SCION_TEST_LONG_ORDER_WALK is
// set.
func TestListAgentsSorted_OrderWalkAcrossProjectsFiltersAndScopesLarge(t *testing.T) {
	if os.Getenv(longOrderWalkEnv) == "" {
		t.Skip("skipping 1200-agent order walk; set " + longOrderWalkEnv + "=1 to run it")
	}
	if testing.Short() {
		t.Skip("skipping 1200-agent order walk in -short mode")
	}
	runOrderWalkAcrossProjects(t, 1200, true)
}

// runOrderWalkAcrossProjects walks the global endpoint over n agents spread
// across three projects, with two phases, a label present with a value,
// present with an empty value, or absent, created times with four-way exact
// ties and fractional seconds (including whole seconds and fractions whose
// stored text drops trailing zeros), and last_activity_event NULL for a
// third of the rows. Every walk must concatenate to the agentsort reference
// order over the test's own filtered set, with no row skipped or repeated.
//
// The list endpoint's cost grows with the rows it returns, so the
// attributes are skewed to keep each filtered set small next to the
// unfiltered one: the caller's own project and the projectId target each
// hold a tenth of the agents, phase=running a ninth, and each label
// variant an eleventh. full adds the page sizes 500, 7 and 25 on every
// filter and walks scope=shared on its own, which returns most rows.
func runOrderWalkAcrossProjects(t *testing.T, n int, full bool) {
	t.Helper()
	f := globalSortedSetup(t)
	ctx := context.Background()

	// The caller owns walk-p1 through a project-owner binding, so scope=mine
	// is walk-p1 and scope=shared is the other two (f.project, which holds
	// no agents, is also shared).
	var projects []string
	for i, slug := range []string{"walk-p1", "walk-p2", "walk-p3"} {
		p := &store.Project{
			ID: tid("sg-" + slug), Name: slug, Slug: slug,
			OwnerID: f.admin.ID, CreatedBy: f.admin.ID, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, f.store.CreateProject(ctx, p))
		role := store.ProjectRoleAdmin
		if i == 0 {
			role = store.ProjectRoleOwner
		}
		createTestUserWithProjectRole(t, f.store, f.admin.ID, f.admin.Email, p.ID, role)
		projects = append(projects, p.ID)
	}

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	agents := make([]orderWalkAgent, n)
	require.NoError(t, f.store.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < n; i++ {
			project := projects[2]
			switch i % 10 {
			case 0:
				project = projects[0]
			case 1:
				project = projects[1]
			}
			phase := string(state.PhaseStopped)
			if i%9 == 0 {
				phase = string(state.PhaseRunning)
			}
			a := orderWalkAgent{
				id:      tid(fmt.Sprintf("sg-walk-%d", i)),
				project: project,
				phase:   phase,
				created: base.Add(time.Duration(i/4) * 700 * time.Millisecond),
				updated: base.Add(time.Duration((i*7)%300) * 250 * time.Millisecond),
			}
			switch i % 11 {
			case 0:
				a.labels = map[string]string{"team": "a"}
			case 1:
				a.labels = map[string]string{"team": ""}
			}
			if i%3 != 0 {
				a.lastActivity = base.Add(time.Duration((i*13)%500) * 125 * time.Millisecond)
			}
			agents[i] = a
			slug := fmt.Sprintf("walk-%d", i)
			if err := tx.CreateAgent(ctx, &store.Agent{
				ID: a.id, Slug: slug, Name: slug, ProjectID: a.project, Phase: a.phase, Labels: a.labels,
				CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
			}); err != nil {
				return err
			}
		}
		return nil
	}))
	seedOrderWalkTimes(t, f.store, agents)

	hasLabel := func(a orderWalkAgent, v string) bool {
		got, ok := a.labels["team"]
		return ok && got == v
	}
	running := func(a orderWalkAgent) bool { return a.phase == string(state.PhaseRunning) }
	type walkFilter struct {
		name, query string
		keep        func(orderWalkAgent) bool
	}
	filters := []walkFilter{
		{"phase", "phase=running", running},
		{"label", "label=team=a", func(a orderWalkAgent) bool { return hasLabel(a, "a") }},
		{"empty label value", "label=team=", func(a orderWalkAgent) bool { return hasLabel(a, "") }},
		{"project", "projectId=" + projects[1], func(a orderWalkAgent) bool { return a.project == projects[1] }},
		{"scope mine", "scope=mine", func(a orderWalkAgent) bool { return a.project == projects[0] }},
		{"scope shared and phase", "scope=shared&phase=running", func(a orderWalkAgent) bool {
			return a.project != projects[0] && running(a)
		}},
	}
	if full {
		filters = append(filters, walkFilter{"scope shared", "scope=shared", func(a orderWalkAgent) bool { return a.project != projects[0] }})
	}
	const combined = "project, label and phase"
	filters = append(filters, walkFilter{combined, "projectId=" + projects[2] + "&label=team=a&phase=stopped", func(a orderWalkAgent) bool {
		return a.project == projects[2] && hasLabel(a, "a") && a.phase == string(state.PhaseStopped)
	}})

	walk := func(fl walkFilter, sortKey, dir string, limits []int) {
		t.Helper()
		want := expectedOrder(agents, sortKey, dir, fl.keep)
		require.NotEmpty(t, want, fl.name)
		query := fmt.Sprintf("sort=%s&dir=%s", sortKey, dir)
		if fl.query != "" {
			query += "&" + fl.query
		}
		for _, limit := range limits {
			if limit < 500 {
				require.Greater(t, len(want), limit, "filter=%s: page size %d must cross pages", fl.name, limit)
			}
			got := walkSortedPages(t, f.srv, f.admin, f.listPath, query, limit)
			assert.Equal(t, want, got, "filter=%s sort=%s dir=%s limit=%d", fl.name, sortKey, dir, limit)
		}
	}

	var combos [][2]string
	for _, sortKey := range []string{agentsort.Updated, agentsort.Created} {
		for _, dir := range []string{agentsort.Desc, agentsort.Asc} {
			combos = append(combos, [2]string{sortKey, dir})
		}
	}
	// The unfiltered set is larger than 500, so page size 500 pages twice.
	all := walkFilter{"none", "", func(orderWalkAgent) bool { return true }}
	require.Greater(t, n, 500)
	for _, c := range combos {
		walk(all, c[0], c[1], []int{500})
	}
	for fi, fl := range filters {
		for ci, c := range combos {
			var limits []int
			if full {
				limits = []int{500, 7, 25}
			} else {
				// Rotate the page size so each filter walks both 7 and 25
				// across its sort and direction combinations.
				limits = []int{[]int{7, 25}[(fi+ci)%2]}
			}
			if fl.name == combined {
				limits = append(limits, 1)
			}
			walk(fl, c[0], c[1], limits)
		}
	}
}

// TestListProjectAgentsSorted_CreatedSortWalkWithTiesBothDirections walks
// the project endpoint with sort=created in both directions over rows with
// exact created ties (so the id tie-break decides), fractional seconds and
// a phase filter, at several page sizes.
func TestListProjectAgentsSorted_CreatedSortWalkWithTiesBothDirections(t *testing.T) {
	f := sortedListSetup(t)
	const n = 40
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	agents := make([]orderWalkAgent, n)
	for i := 0; i < n; i++ {
		phase := []string{string(state.PhaseRunning), string(state.PhaseStopped)}[i%2]
		a := f.createAgent(t, fmt.Sprintf("cwalk-%d", i), phase, nil)
		agents[i] = orderWalkAgent{
			id: a.ID, project: f.project.ID, phase: phase,
			created: base.Add(time.Duration(i/3) * 333 * time.Millisecond),
			updated: base.Add(time.Duration(n-i) * time.Second),
		}
	}
	seedOrderWalkTimes(t, f.store, agents)

	for _, fl := range []struct {
		name, query string
		keep        func(orderWalkAgent) bool
	}{
		{"none", "", func(orderWalkAgent) bool { return true }},
		{"phase", "phase=stopped", func(a orderWalkAgent) bool { return a.phase == string(state.PhaseStopped) }},
	} {
		for _, dir := range []string{agentsort.Desc, agentsort.Asc} {
			want := expectedOrder(agents, agentsort.Created, dir, fl.keep)
			query := "sort=created&dir=" + dir
			if fl.query != "" {
				query += "&" + fl.query
			}
			for _, limit := range []int{1, 3, 7, 500} {
				got := walkSortedPages(t, f.srv, f.owner, f.listPath, query, limit)
				assert.Equal(t, want, got, "filter=%s dir=%s limit=%d", fl.name, dir, limit)
			}
		}
	}
}
