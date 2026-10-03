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
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mintGlobalCursor requests page 0 of the global endpoint with query and
// returns its nextCursor, failing the test if there is none.
func (f *globalSortedFixture) mintGlobalCursor(t *testing.T, user *store.User, query string) string {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, f.listPath(query), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	require.NotEmpty(t, resp.NextCursor, "query %q must produce a next cursor", query)
	return resp.NextCursor
}

func withCursor(query, cursor string) string {
	return query + "&cursor=" + url.QueryEscape(cursor)
}

// addSecondHubAdmin creates another hub-admin with the same project-admin
// binding as f.admin, so the two callers resolve an identical filter and
// differ only by identity.
func (f *globalSortedFixture) addSecondHubAdmin(t *testing.T) *store.User {
	t.Helper()
	id := tid("sg-admin-2")
	createTestUserWithRole(t, f.store, id, "sg-admin-2@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	createTestUserWithProjectRole(t, f.store, id, "sg-admin-2@test.com", f.project.ID, store.ProjectRoleAdmin)
	u, err := f.store.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// --- cursor binding: every replay is a 400 --------------------------------

func TestListAgentsSorted_CursorPhaseReplayRejected(t *testing.T) {
	f := globalSortedSetup(t)
	for i := 0; i < 3; i++ {
		f.createAgent(t, fmt.Sprintf("run-%d", i), "running")
		f.createAgent(t, fmt.Sprintf("stop-%d", i), "stopped")
	}
	cur := f.mintGlobalCursor(t, f.admin, "sort=updated&limit=1&phase=running")

	// The same cursor under its own filter is accepted, so the 400 below is
	// the phase binding and not a malformed cursor.
	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor("sort=updated&limit=1&phase=running", cur)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor("sort=updated&limit=1&phase=stopped", cur)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestListAgentsSorted_CursorLabelReplayRejected(t *testing.T) {
	f := globalSortedSetup(t)
	for i := 0; i < 3; i++ {
		for _, team := range []string{"a", "b"} {
			a := &store.Agent{
				ID: tid(fmt.Sprintf("sg-label-%s-%d", team, i)), Slug: fmt.Sprintf("label-%s-%d", team, i),
				Name: fmt.Sprintf("label-%s-%d", team, i), ProjectID: f.project.ID, Phase: "stopped",
				CreatedBy: f.admin.ID, OwnerID: f.admin.ID, Labels: map[string]string{"team": team},
			}
			require.NoError(t, f.store.CreateAgent(context.Background(), a))
		}
	}
	cur := f.mintGlobalCursor(t, f.admin, "sort=updated&limit=1&label=team=a")

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor("sort=updated&limit=1&label=team=a", cur)), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor("sort=updated&limit=1&label=team=b", cur)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestListAgentsSorted_CursorWrongSortOrDirRejected(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 3, "sd", "stopped")
	cur := f.mintGlobalCursor(t, f.admin, "sort=updated&dir=desc&limit=1")

	for _, q := range []string{
		"sort=created&dir=desc&limit=1", // wrong sort
		"sort=updated&dir=asc&limit=1",  // wrong dir
	} {
		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor(q, cur)), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "query=%q body=%s", q, rec.Body.String())
	}
}

func TestListAgentsSorted_CursorCrossPrincipalRejected(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 3, "xp", "stopped")
	other := f.addSecondHubAdmin(t)

	// Both callers see the same rows, so only identity separates them.
	q := "sort=updated&limit=1"
	cur := f.mintGlobalCursor(t, f.admin, q)
	otherCur := f.mintGlobalCursor(t, other, q)
	require.NotEqual(t, cur, otherCur, "the binding must include the identity")

	rec := doRequestAsUser(t, f.srv, other, http.MethodGet, f.listPath(withCursor(q, cur)), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestListAgentsSorted_CursorFromProjectEndpointRejected(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 3, "ep", "stopped")

	q := "sort=updated&limit=1"
	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/agents?"+q, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	projectCur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
	require.NotEmpty(t, projectCur)

	for _, gq := range []string{q, q + "&projectId=" + f.project.ID} {
		rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor(gq, projectCur)), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "query=%q body=%s", gq, rec.Body.String())
	}
}

// globalCallSpyStore counts every store read the global sorted path can make.
type globalCallSpyStore struct {
	store.Store
	mu    sync.Mutex
	calls []string
}

func (g *globalCallSpyStore) record(name string) {
	g.mu.Lock()
	g.calls = append(g.calls, name)
	g.mu.Unlock()
}

func (g *globalCallSpyStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	g.record("ListAgents")
	return g.Store.ListAgents(ctx, filter, opts)
}

func (g *globalCallSpyStore) CountAgents(ctx context.Context, filter store.AgentFilter) (int, error) {
	g.record("CountAgents")
	return g.Store.CountAgents(ctx, filter)
}

func (g *globalCallSpyStore) CountAgentsByPhaseIDs(ctx context.Context, filter store.AgentFilter) ([]store.IDPhase, error) {
	g.record("CountAgentsByPhaseIDs")
	return g.Store.CountAgentsByPhaseIDs(ctx, filter)
}

func (g *globalCallSpyStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sort, dir string, max int) ([]store.AgentMember, error) {
	g.record("ListAgentMembers")
	return g.Store.ListAgentMembers(ctx, filter, sort, dir, max)
}

// TestListAgentsSorted_MalformedV2CursorRejectedBeforeAnyRead takes a real
// cursor, corrupts its key timestamp while keeping its prefix, sort, dir and
// binding valid, and checks the 400 happens before any agent read or
// authorization decision.
func TestListAgentsSorted_MalformedV2CursorRejectedBeforeAnyRead(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 3, "mal", "stopped")
	q := "sort=updated&limit=1&stats=1"
	cur := f.mintGlobalCursor(t, f.admin, q)

	raw, err := base64.URLEncoding.DecodeString(cur)
	require.NoError(t, err)
	parts := strings.SplitN(string(raw), ",", 7)
	require.Len(t, parts, 7)
	parts[3] = "not-a-time"
	malformed := base64.URLEncoding.EncodeToString([]byte(strings.Join(parts, ",")))

	spy := &globalCallSpyStore{Store: f.store}
	f.srv.store = spy
	emitter := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(emitter)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(withCursor(q, malformed)), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, spy.calls, "no agent read may run before the cursor is rejected")
	assert.Empty(t, emitter.records, "no decision may be made before the cursor is rejected")
}

// --- totals and order --------------------------------------------------------

// TestListAgentsSorted_PagedTotalCountAppliesPhase pins that a paged sorted
// response's totalCount is the exact count with the phase filter applied.
func TestListAgentsSorted_PagedTotalCountAppliesPhase(t *testing.T) {
	f := globalSortedSetup(t)
	f.createAgentsBulk(t, 3, "tc-run", "running")
	f.createAgentsBulk(t, 4, "tc-stop", "stopped")

	for _, tc := range []struct {
		query string
		total int
	}{
		{"sort=updated&limit=2&phase=running", 3},
		{"sort=created&limit=2&phase=stopped", 4},
		{"sort=updated&limit=2&fit=2&phase=running", 3}, // over fit: paged
		{"sort=updated&limit=2", 7},
	} {
		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(tc.query), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Len(t, resp.Agents, 2, tc.query)
		assert.NotEmpty(t, resp.NextCursor, tc.query)
		assert.Equal(t, tc.total, resp.TotalCount, tc.query)
	}
}

// setRawAgentTimes overwrites an agent's stored time columns with literal
// text, for building exact ties that CreateAgent's own clock cannot produce.
// An empty lastActivity stores NULL.
func setRawAgentTimes(t *testing.T, s store.Store, id, created, updated, lastActivity string) {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	var lae any
	if lastActivity != "" {
		lae = lastActivity
	}
	_, err := dbProvider.DB().ExecContext(context.Background(),
		"UPDATE agents SET created = ?, updated = ?, last_activity_event = ? WHERE id = ?",
		created, updated, lae, id)
	require.NoError(t, err)
}

// TestListAgentsSorted_OrderWithTiesAndLastActivity walks every page with
// limit=2 over rows that tie on created (so the id tie-break decides) and on
// the updated key K, including non-NULL last_activity_event values, and
// checks the walk matches the agentsort reference order with no row skipped
// or repeated.
func TestListAgentsSorted_OrderWithTiesAndLastActivity(t *testing.T) {
	f := globalSortedSetup(t)
	seeded := f.createAgentsBulk(t, 7, "tie", "stopped")

	const (
		t0 = "2026-01-01 00:00:00 +0000 UTC"
		t1 = "2026-01-01 00:00:01 +0000 UTC"
		t2 = "2026-01-01 00:00:02.5 +0000 UTC"
		t3 = "2026-01-01 00:00:03 +0000 UTC"
	)
	// created: four rows tie at t0, three at t1.
	// K = COALESCE(last_activity_event, updated):
	//   rows 0-2 have K = t2 (two via updated, one via last_activity_event),
	//   rows 3-4 have K = t3 via last_activity_event, rows 5-6 have K = t1.
	times := []struct{ created, updated, lae string }{
		{t0, t2, ""},
		{t0, t2, ""},
		{t0, t1, t2},
		{t0, t1, t3},
		{t1, t2, t3},
		{t1, t1, ""},
		{t1, t1, ""},
	}
	for i, a := range seeded {
		setRawAgentTimes(t, f.store, a.ID, times[i].created, times[i].updated, times[i].lae)
	}

	rows := map[string][]agentsort.Row{}
	for _, a := range seeded {
		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
			rows[sortKey] = append(rows[sortKey], agentsort.KeyFor(sortKey, got.ID, got.Created, got.Updated, got.LastActivityEvent))
		}
	}

	for _, sortKey := range []string{agentsort.Created, agentsort.Updated} {
		for _, dir := range []string{agentsort.Asc, agentsort.Desc} {
			ref := append([]agentsort.Row(nil), rows[sortKey]...)
			agentsort.SortRows(dir, ref)
			want := make([]string, len(ref))
			for i, row := range ref {
				want[i] = row.ID
			}

			var walked []string
			cursor := ""
			for i := 0; i < 10; i++ {
				q := fmt.Sprintf("sort=%s&dir=%s&limit=2", sortKey, dir)
				if cursor != "" {
					q = withCursor(q, cursor)
				}
				rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.listPath(q), nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				resp := mustDecodeListAgentsResponse(t, rec.Body)
				for _, a := range resp.Agents {
					walked = append(walked, a.ID)
				}
				if resp.NextCursor == "" {
					break
				}
				cursor = resp.NextCursor
			}
			assert.Equal(t, want, walked, "sort=%s dir=%s", sortKey, dir)
		}
	}
}

// --- validation before the short-circuits -----------------------------------

// noScopeUser creates a user with no hub membership and no project bindings,
// whose list scope resolves to None.
func noScopeUser(t *testing.T, s store.Store) *store.User {
	t.Helper()
	user := &store.User{
		ID: tid("sg-none-v"), Email: "sg-none-v@test.com",
		DisplayName: "No Scope User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), user))
	return user
}

// listAgentsUnauthenticated calls the global list handler with no identity
// in the request context, reaching its unauthenticated short-circuit.
func listAgentsUnauthenticated(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?"+query, nil)
	rec := httptest.NewRecorder()
	srv.listAgents(rec, req)
	return rec
}

// TestListAgentsSorted_ShortCircuitsValidateSortAndDir pins that an invalid
// sort or dir is a 400 for unauthenticated and None-scope callers too, the
// same as for a caller with scope.
func TestListAgentsSorted_ShortCircuitsValidateSortAndDir(t *testing.T) {
	srv, s := testServer(t)
	user := noScopeUser(t, s)

	for _, tc := range []struct {
		query string
		msg   string
	}{
		{"sort=bogus", "invalid sort"},
		{"sort=updated&dir=sideways", "invalid dir"},
		{"sort=bogus&dir=sideways", "invalid dir"},
	} {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?"+tc.query, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, "none-scope query=%q body=%s", tc.query, rec.Body.String())
		assert.Equal(t, tc.msg, decodeErrorMessage(t, rec.Body.Bytes()))

		rec = listAgentsUnauthenticated(srv, tc.query)
		require.Equal(t, http.StatusBadRequest, rec.Code, "unauthenticated query=%q body=%s", tc.query, rec.Body.String())
		assert.Equal(t, tc.msg, decodeErrorMessage(t, rec.Body.Bytes()))
	}
}

// TestListAgentsSorted_ShortCircuitsEchoValidatedValues pins the echo for a
// valid sorted request on both short-circuits, including the default dir.
func TestListAgentsSorted_ShortCircuitsEchoValidatedValues(t *testing.T) {
	srv, s := testServer(t)
	user := noScopeUser(t, s)

	for name, do := range map[string]func(q string) *httptest.ResponseRecorder{
		"none-scope": func(q string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?"+q, nil)
		},
		"unauthenticated": func(q string) *httptest.ResponseRecorder { return listAgentsUnauthenticated(srv, q) },
	} {
		rec := do("sort=created")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		assert.Equal(t, "created", resp.Sort, name)
		assert.Equal(t, "desc", resp.Dir, name)
		assert.Nil(t, resp.Complete, name)
		assert.Nil(t, resp.Stats, name)
		assert.Empty(t, resp.Agents, name)
	}
}

// TestListAgentsSorted_ShortCircuitsWithoutSortStayLegacy pins that, without
// sort, both short-circuits ignore dir/fit/stats (even invalid values) and
// return exactly the legacy empty list.
func TestListAgentsSorted_ShortCircuitsWithoutSortStayLegacy(t *testing.T) {
	srv, s := testServer(t)
	user := noScopeUser(t, s)

	base := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, base.Code, base.Body.String())
	assertResponsesEqualIgnoringServerTime(t, []byte(legacyEmptyListBody), base.Body.Bytes())

	for _, q := range []string{"dir=sideways&fit=0&stats=1", "dir=asc&fit=500"} {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?"+q, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assertResponsesEqualIgnoringServerTime(t, base.Body.Bytes(), rec.Body.Bytes())

		rec = listAgentsUnauthenticated(srv, q)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assertResponsesEqualIgnoringServerTime(t, base.Body.Bytes(), rec.Body.Bytes())
	}
}

// legacyEmptyListBody is the legacy short-circuit body, minus serverTime.
const legacyEmptyListBody = `{"agents":[],"totalCount":0}`
