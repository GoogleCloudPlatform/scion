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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compactParityField is one compact item field checked by the field parity
// harness: the compact JSON key and how to read the value the full item
// carries for the same agent. ok is false when the full item omits the
// value, in which case the compact item must omit its key too.
type compactParityField struct {
	compactKey string
	fullValue  func(t *testing.T, fullItem map[string]json.RawMessage) (raw json.RawMessage, ok bool)
}

// fullTopLevel reads a field the full item carries under the same key.
func fullTopLevel(key string) func(*testing.T, map[string]json.RawMessage) (json.RawMessage, bool) {
	return func(_ *testing.T, fullItem map[string]json.RawMessage) (json.RawMessage, bool) {
		raw, ok := fullItem[key]
		return raw, ok
	}
}

// compactParityFields is the field table of the field parity harness. To
// cover a new compact field, add one row here; the harness then checks it
// for every caller class, endpoint, query and page.
var compactParityFields = []compactParityField{
	{compactKey: "message", fullValue: fullTopLevel("message")},
	{compactKey: "deletion", fullValue: fullTopLevel("deletion")},
}

// compactParityAbsent is the value recorded in compactFieldTally.callerVals
// for a field the full item omits.
const compactParityAbsent = "<absent>"

// compactParityQuery is one first-page request of the field parity
// harness. Paged queries (those with a limit) are walked to the end by
// following nextCursor.
type compactParityQuery struct {
	endpoint string // "global", "project" or "other-project", for labels and tallies
	base     string
	query    string
}

// paged reports whether the query sets a page size, so its walk must reach
// more than one page wherever the caller reads rows.
func (q compactParityQuery) paged() bool {
	for _, p := range strings.Split(q.query, "&") {
		if strings.HasPrefix(p, "limit=") {
			return true
		}
	}
	return false
}

// compactParityWalkKey names one walk of the harness: a caller, an endpoint
// and a first-page query.
func compactParityWalkKey(caller string, q compactParityQuery) string {
	return caller + " " + q.endpoint + " " + q.query
}

// compactFieldTally counts, per field, what the harness compared, so a
// test can require that both present and absent values were exercised.
type compactFieldTally struct {
	rows     int
	present  map[string]int
	absent   map[string]int
	seenVals map[string]map[string]bool // field -> raw full value -> seen
	// cellRows is the number of rows compared per "caller endpoint" cell.
	cellRows map[string]int
	// cellVals is, per "caller endpoint" cell, the raw full values of every
	// field that were compared there.
	cellVals map[string]map[string]bool
	// walkPages and walkRows are the pages requested and rows compared per
	// walk, keyed by compactParityWalkKey.
	walkPages map[string]int
	walkRows  map[string]int
	// cellStatus is, per "caller endpoint" cell, the set of first-page
	// response statuses.
	cellStatus map[string]map[int]bool
	// cellObjects counts, per "caller endpoint" cell and field, the rows
	// whose full value is a JSON object (for deletion: a non-null view).
	cellObjects map[string]map[string]int
	// callerVals is, per caller, agent id and field, the raw full value
	// compared (compactParityAbsent when omitted). The harness asserts that
	// it is the same on every endpoint, query and page the caller reads the
	// agent on, so a test can compare one caller's values with another's.
	callerVals map[string]map[string]map[string]string
}

// runCompactFieldParity is a reusable parity harness for compact item
// fields. For every caller, every query and every page of its cursor walk,
// it requests the full view and view=compact with the same filters and
// paging and asserts:
//   - the same status, and byte-identical bodies when the status is not 200;
//   - the same agent ids in the same order: no row in compact that is absent
//     from full, and none missing;
//   - the same nextCursor, before the walk follows it;
//   - for every field in fields and every row, the compact value equals the
//     value the full item carries for the same agent, byte for byte as JSON,
//     and the compact key is absent exactly when the full value is absent.
//     Whatever the full view emits for a caller class (including an omitted
//     or redacted value) is therefore what compact must emit.
//
// To extend it, add rows to compactParityFields, callers to the caller list
// passed in, or queries to compactFieldParityQueries.
func runCompactFieldParity(t *testing.T, f *compactFixture, callers []compactCaller, queries []compactParityQuery, fields []compactParityField) compactFieldTally {
	t.Helper()
	tally := compactFieldTally{
		present: map[string]int{}, absent: map[string]int{},
		seenVals: map[string]map[string]bool{}, cellRows: map[string]int{},
		cellVals: map[string]map[string]bool{}, walkPages: map[string]int{}, walkRows: map[string]int{},
		cellStatus: map[string]map[int]bool{}, cellObjects: map[string]map[string]int{},
		callerVals: map[string]map[string]map[string]string{},
	}
	for _, fd := range fields {
		tally.seenVals[fd.compactKey] = map[string]bool{}
	}
	for _, c := range callers {
		for _, q := range queries {
			cell := c.name + " " + q.endpoint
			walk := compactParityWalkKey(c.name, q)
			if _, ok := tally.cellRows[cell]; !ok {
				tally.cellRows[cell] = 0
				tally.cellVals[cell] = map[string]bool{}
				tally.cellStatus[cell] = map[int]bool{}
				tally.cellObjects[cell] = map[string]int{}
			}
			if _, ok := tally.callerVals[c.name]; !ok {
				tally.callerVals[c.name] = map[string]map[string]string{}
			}
			query := q.query
			for page := 0; ; page++ {
				require.Less(t, page, 30, "%s %q: walk did not end", cell, q.query)
				tally.walkPages[walk]++
				label := fmt.Sprintf("%s %q page %d", cell, q.query, page)
				full := c.do(t, withQuery(q.base, query))
				comp := c.do(t, withQuery(q.base, query, "view=compact"))
				require.Equal(t, full.Code, comp.Code, "%s: status", label)
				if page == 0 {
					tally.cellStatus[cell][full.Code] = true
				}
				if full.Code != http.StatusOK {
					assert.Equal(t, string(rawBodyWithoutServerTime(full)), string(rawBodyWithoutServerTime(comp)), "%s: non-200 body", label)
					break
				}
				fullItems := decodeItems(t, decodeObject(t, full.Body.Bytes())["agents"])
				compItems := decodeItems(t, decodeObject(t, comp.Body.Bytes())["agents"])
				fullIDs, compIDs := itemIDs(t, fullItems), itemIDs(t, compItems)
				assert.Empty(t, missingFrom(fullIDs, compIDs), "%s: compact rows absent from full", label)
				assert.Empty(t, missingFrom(compIDs, fullIDs), "%s: full rows missing from compact", label)
				require.Equal(t, fullIDs, compIDs, "%s: agent ids and order", label)
				for i := range fullItems {
					tally.rows++
					tally.cellRows[cell]++
					tally.walkRows[walk]++
					for _, fd := range fields {
						fv, fok := fd.fullValue(t, fullItems[i])
						cv, cok := compItems[i][fd.compactKey]
						assert.Equal(t, fok, cok, "%s: agent %s: %q presence", label, fullIDs[i], fd.compactKey)
						assert.Equal(t, string(fv), string(cv), "%s: agent %s: %q bytes", label, fullIDs[i], fd.compactKey)
						rec := compactParityAbsent
						if fok {
							rec = string(fv)
							tally.present[fd.compactKey]++
							tally.seenVals[fd.compactKey][string(fv)] = true
							tally.cellVals[cell][string(fv)] = true
							if len(fv) > 0 && fv[0] == '{' {
								tally.cellObjects[cell][fd.compactKey]++
							}
						} else {
							tally.absent[fd.compactKey]++
						}
						agentVals := tally.callerVals[c.name][fullIDs[i]]
						if agentVals == nil {
							agentVals = map[string]string{}
							tally.callerVals[c.name][fullIDs[i]] = agentVals
						}
						if prev, seen := agentVals[fd.compactKey]; seen {
							assert.Equal(t, prev, rec, "%s: agent %s: %q differs from this caller's earlier reads", label, fullIDs[i], fd.compactKey)
						}
						agentVals[fd.compactKey] = rec
					}
				}
				next := mustDecodeListAgentsResponse(t, full.Body).NextCursor
				compNext := mustDecodeListAgentsResponse(t, comp.Body).NextCursor
				require.Equal(t, next, compNext, "%s: nextCursor", label)
				if next == "" {
					break
				}
				query = strings.TrimPrefix(withCursor(dropParam(q.query, "fit"), next), "&")
			}
		}
	}
	return tally
}

func itemIDs(t *testing.T, items []map[string]json.RawMessage) []string {
	t.Helper()
	ids := make([]string, len(items))
	for i, it := range items {
		require.NoError(t, json.Unmarshal(it["id"], &ids[i]))
	}
	return ids
}

// missingFrom returns the ids in want that are not in have, sorted.
func missingFrom(have, want []string) []string {
	in := map[string]bool{}
	for _, id := range have {
		in[id] = true
	}
	var out []string
	for _, id := range want {
		if !in[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// setRawAgentMessage sets an agent's stored detail message directly, without
// touching any other column (updated time and version stay as seeded).
func setRawAgentMessage(t *testing.T, s store.Store, id, message string) {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	res, err := dbProvider.DB().ExecContext(context.Background(), "UPDATE agents SET message = ? WHERE id = ?", message, id)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "agent %s", id)
}

// compactMessageSeed is the detail message given to each project agent of
// the compact fixture, by index; an empty string leaves it unset.
var compactMessageSeed = []string{
	"Waiting for review on the requested change",
	"",
	"Build failed: go vet reported 2 issues in pkg/hub",
	"Rebasing onto main after a conflict in agent_compact_view.go \"quoted\" <tag> & más",
	"",
	"Running the targeted test suite",
	"Stopped by the user",
}

// setRawAgentLabels replaces an agent's stored labels directly, without
// touching any other column.
func setRawAgentLabels(t *testing.T, s store.Store, id string, labels map[string]string) {
	t.Helper()
	raw, err := json.Marshal(labels)
	require.NoError(t, err)
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	res, err := dbProvider.DB().ExecContext(context.Background(), "UPDATE agents SET labels = ? WHERE id = ?", string(raw), id)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "agent %s", id)
}

// compactDeletionSeed is a delete marker the field parity tests give to one
// fixture agent, with the error text set on it afterwards ("" for none).
type compactDeletionSeed struct {
	agentID string
	seed    deleteSeed
	errText string
	// detail is true when the admin view carries a code (and the error
	// text, when set): the failed, in_doubt and abandoned views.
	detail bool
}

// compactDeletionSeeds covers every view shape: a live deleting row, a
// failed row with code and error, a live finalizing row (deleting with
// stage finalizing), an in_doubt row with an error (no expiresAt), an
// abandoned row (a deleting row whose lease lapsed, with an error) and a
// failed conflict in the other project. Fixture agents 0 and 2 and the
// other project's agent 0, whose tokens are callers, carry no marker, and
// neither do the rest, so null views are compared too.
func compactDeletionSeeds(f *compactFixture) []compactDeletionSeed {
	return []compactDeletionSeed{
		{agentID: f.agentIDAt(1), seed: seedLiveDeleting},
		{agentID: f.agentIDAt(3), seed: deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, errText: "broker said: container c-3 exited 137", detail: true},
		{agentID: f.agentIDAt(4), seed: deleteSeed{state: store.DeletionStateFinalizing, leaseIn: time.Hour}},
		{agentID: f.agentIDAt(5), seed: deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt}, errText: "teardown on broker unconfirmed", detail: true},
		{agentID: f.agentIDAt(6), seed: deleteSeed{state: store.DeletionStateDeleting, leaseIn: -time.Minute}, errText: "lease lapsed during revoke", detail: true},
		{agentID: tid("cv-other-1"), seed: deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict}, errText: "ambiguous container on broker", detail: true},
	}
}

func seedCompactDeletions(t *testing.T, f *compactFixture) []compactDeletionSeed {
	t.Helper()
	seeds := compactDeletionSeeds(f)
	for _, d := range seeds {
		seedDeletionWithError(t, f.store, d.agentID, d.seed, d.errText)
	}
	return seeds
}

// assertDeletionRedactionPerCaller checks the deletion visibility rule
// (ptone/scion#3122) on the values the harness compared: adminCaller (an
// unscoped local platform admin, who reads every agent) sees code and claim
// on every seeded failed view and the error text where one was set; every
// other caller sees, for every agent it reads, exactly the admin's view
// without code, error and claim, with the same presence (null or object),
// state, stage, soft flag and timestamps. It returns, per caller, the number
// of non-null views checked.
func assertDeletionRedactionPerCaller(t *testing.T, tally compactFieldTally, adminCaller string, seeds []compactDeletionSeed) map[string]int {
	t.Helper()
	adminRows := tally.callerVals[adminCaller]
	require.NotEmpty(t, adminRows, "%s reads agents", adminCaller)
	adminObj := func(id string) map[string]json.RawMessage {
		raw, ok := adminRows[id]["deletion"]
		require.True(t, ok, "%s reads agent %s", adminCaller, id)
		require.NotEqual(t, compactParityAbsent, raw, "the full item always carries deletion")
		return rawDeletionObject(t, json.RawMessage(raw))
	}
	for _, d := range seeds {
		obj := adminObj(d.agentID)
		require.NotNil(t, obj, "%s: agent %s has a deletion view", adminCaller, d.agentID)
		assert.Contains(t, obj, "claim", "%s: agent %s claim", adminCaller, d.agentID)
		if d.detail {
			assert.Contains(t, obj, "code", "%s: agent %s code", adminCaller, d.agentID)
		}
		if d.errText != "" {
			raw, err := json.Marshal(d.errText)
			require.NoError(t, err)
			assert.Equal(t, string(raw), string(obj["error"]), "%s: agent %s error", adminCaller, d.agentID)
		}
	}
	views := map[string]int{}
	for caller, rows := range tally.callerVals {
		if caller == adminCaller {
			continue
		}
		for id, vals := range rows {
			raw, ok := vals["deletion"]
			require.True(t, ok, "%s: agent %s: deletion compared", caller, id)
			require.NotEqual(t, compactParityAbsent, raw, "%s: agent %s: the full item always carries deletion", caller, id)
			got := rawDeletionObject(t, json.RawMessage(raw))
			assertGenericDeletionOf(t, adminObj(id), got, caller+" agent "+id)
			if got != nil {
				views[caller]++
			}
		}
	}
	return views
}

// compactFieldParityQueries is every first-page request the field parity
// harness makes: the global endpoint, the fixture project's endpoint and the
// other project's endpoint, each in legacy and sorted modes, with paged
// walks, fit and filters, plus project id filters on the global endpoint
// for both projects. Every paged query uses limit=1, so a caller that reads
// two or more rows on a walk needs at least two pages; the lane=x label is
// set on agents of both projects (including two of the relationship-grant
// caller's agents) so the paged label walk reads two or more rows wherever
// a caller reads that endpoint.
func compactFieldParityQueries(f *compactFixture) []compactParityQuery {
	var qs []compactParityQuery
	for _, ep := range []struct{ name, base string }{
		{"global", f.globalBase()},
		{"project", f.projectBase()},
		{"other-project", "/api/v1/projects/" + f.other.ID + "/agents"},
	} {
		for _, q := range []string{
			"",
			"limit=1",
			"phase=running",
			"sort=updated&dir=desc&limit=1",
			"sort=created&dir=asc&fit=500",
			"label=team=a",
			"sort=updated&limit=1&label=lane=x",
		} {
			qs = append(qs, compactParityQuery{ep.name, ep.base, q})
		}
	}
	qs = append(qs,
		compactParityQuery{"global", f.globalBase(), "projectId=" + f.other.ID},
		compactParityQuery{"global", f.globalBase(), "sort=updated&limit=1&projectId=" + f.project.ID},
	)
	return qs
}

// compactParityCell is the pinned outcome of one "caller endpoint" cell of
// the field parity harness: the first-page status of every walk, and
// whether the cell compares at least one row (reads) or none.
type compactParityCell struct {
	status int
	reads  bool
}

// compactParityCells pins every caller and endpoint cell of
// TestAgentCompactView_FieldParityPerCallerClass, so any change in what a
// caller can read, or in how it is rejected, fails the harness.
var compactParityCells = map[string]compactParityCell{
	"owner global":        {http.StatusOK, true},
	"owner project":       {http.StatusOK, true},
	"owner other-project": {http.StatusOK, true},

	"member global":        {http.StatusOK, true},
	"member project":       {http.StatusOK, true},
	"member other-project": {http.StatusForbidden, false},

	"hub-admin-non-member global":        {http.StatusOK, false},
	"hub-admin-non-member project":       {http.StatusForbidden, false},
	"hub-admin-non-member other-project": {http.StatusForbidden, false},

	"super-admin global":        {http.StatusOK, true},
	"super-admin project":       {http.StatusOK, true},
	"super-admin other-project": {http.StatusOK, true},

	"agent-jwt global":        {http.StatusOK, false},
	"agent-jwt project":       {http.StatusOK, true},
	"agent-jwt other-project": {http.StatusNotFound, false},

	"scoped-uat global":        {http.StatusOK, true},
	"scoped-uat project":       {http.StatusOK, true},
	"scoped-uat other-project": {http.StatusForbidden, false},

	"project-constrained global":        {http.StatusOK, true},
	"project-constrained project":       {http.StatusOK, true},
	"project-constrained other-project": {http.StatusForbidden, false},

	"relationship-grant global":        {http.StatusOK, true},
	"relationship-grant project":       {http.StatusOK, true},
	"relationship-grant other-project": {http.StatusForbidden, false},

	"other-project-member global":        {http.StatusOK, true},
	"other-project-member project":       {http.StatusForbidden, false},
	"other-project-member other-project": {http.StatusOK, true},

	"other-project-agent-jwt global":        {http.StatusOK, false},
	"other-project-agent-jwt project":       {http.StatusNotFound, false},
	"other-project-agent-jwt other-project": {http.StatusOK, true},

	"agent-binding global":        {http.StatusOK, true},
	"agent-binding project":       {http.StatusOK, true},
	"agent-binding other-project": {http.StatusNotFound, false},

	// The ceiling carries agent:list only: the global list returns rows,
	// and both project endpoints return an empty list.
	"hub-uat global":        {http.StatusOK, true},
	"hub-uat project":       {http.StatusOK, false},
	"hub-uat other-project": {http.StatusOK, false},

	"agent-no-project-read global":        {http.StatusForbidden, false},
	"agent-no-project-read project":       {http.StatusForbidden, false},
	"agent-no-project-read other-project": {http.StatusForbidden, false},
}

// TestAgentCompactView_FieldParityPerCallerClass runs the field parity
// harness on the global endpoint and on both projects' endpoints for these
// caller classes: the compact fixture's (project owner, project member, hub
// admin who is not a member, super admin, an agent JWT scoped to its own
// project, a project-boundary user access token, a project-constrained user
// and a user with a relationship grant only), plus a member of the other
// project only, an agent JWT scoped to the other project, an agent JWT
// whose agent principal holds a project role binding granting agent.list, a
// hub-boundary user access token whose ceiling carries agent:list, and an
// agent JWT without project:read (rejected on every endpoint, in both
// views).
//
// Not run here: the unauthenticated and None-scope short-circuits, which
// return no agents and are covered by
// TestAgentCompactView_ParityWithFullViewForEveryIdentityClassAndMode, and
// the scope= and mine=true query classifications.
//
// Several agents in both projects carry distinct non-empty detail messages
// and some carry none, so a compact value taken from any source other than
// the same full item, or a row set or cursor that differs from full, fails
// the harness.
func TestAgentCompactView_FieldParityPerCallerClass(t *testing.T) {
	f := compactSetup(t)
	for i, msg := range compactMessageSeed {
		if msg != "" {
			setRawAgentMessage(t, f.store, f.agentIDAt(i), msg)
		}
	}
	otherMessages := []string{
		"Other project: deploying the staging build",
		"Other project: waiting on credentials",
		"",
	}
	for i, msg := range otherMessages {
		if msg != "" {
			setRawAgentMessage(t, f.store, tid(fmt.Sprintf("cv-other-%d", i)), msg)
		}
	}
	for _, i := range []int{0, 1, 3, 4, 6} {
		setRawAgentLabels(t, f.store, f.agentIDAt(i), map[string]string{"team": []string{"a", "b"}[i%2], "lane": "x"})
	}
	for _, i := range []int{0, 1} {
		setRawAgentLabels(t, f.store, tid(fmt.Sprintf("cv-other-%d", i)), map[string]string{"lane": "x"})
	}
	seeds := seedCompactDeletions(t, f)

	otherUser := compactUser(t, f.store, "other-only")
	msgAuthzAddProjectMember(t, f.store, otherUser.ID, f.other.ID, f.other.Slug, store.GroupMemberRoleMember)
	otherTok, err := f.srv.GetAgentTokenService().GenerateAgentToken(tid("cv-other-0"), f.other.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	// The agent principal of fixture agent 2 holds a project role binding
	// that grants agent.list, so its token reads rows on the global list.
	listRole := createTestRoleDefinition(t, f.store, "cv-agent-list", store.RoleScopeProject, []string{"agent.list"})
	_, err = f.store.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: listRole.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      f.agentIDAt(2),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	bindingTok, err := f.srv.GetAgentTokenService().GenerateAgentToken(f.agentIDAt(2), f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)

	hubKey, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.owner.ID), CreateTokenParams{
		UserID: f.owner.ID, Name: "cv-hub-uat", Boundary: hubBoundary(), Scopes: []string{"agent:list"},
	})
	require.NoError(t, err)

	noReadTok, err := f.srv.GetAgentTokenService().GenerateAgentToken(f.agentIDAt(0), f.project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	asAgent := func(tok string) func(t *testing.T, path string) *httptest.ResponseRecorder {
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithAgentToken(t, f.srv, http.MethodGet, path, nil, tok)
		}
	}
	callers := append(append([]compactCaller{}, f.callers...),
		compactCaller{"other-project-member", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, otherUser, http.MethodGet, path, nil)
		}},
		compactCaller{"other-project-agent-jwt", asAgent(otherTok)},
		compactCaller{"agent-binding", asAgent(bindingTok)},
		compactCaller{"hub-uat", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithUAT(t, f.srv, hubKey, http.MethodGet, path, nil)
		}},
		compactCaller{"agent-no-project-read", asAgent(noReadTok)},
	)
	queries := compactFieldParityQueries(f)
	tally := runCompactFieldParity(t, f, callers, queries, compactParityFields)

	// Non-vacuity: present and absent values were both compared, and every
	// seeded message (including the other project's) was compared.
	assert.Positive(t, tally.present["message"], "message present on some rows")
	assert.Positive(t, tally.absent["message"], "message absent on some rows")
	rawMessage := func(msg string) string {
		raw, err := json.Marshal(msg)
		require.NoError(t, err)
		return string(raw)
	}
	for _, msg := range append(append([]string{}, compactMessageSeed...), otherMessages...) {
		if msg != "" {
			assert.True(t, tally.seenVals["message"][rawMessage(msg)], "seeded message %q compared", msg)
		}
	}
	// The agent token paths and the hub-boundary token read rows: an agent
	// token on its own (other) project, an agent token with an agent.list
	// binding on the global list, and the hub-boundary token on the global
	// list.
	for _, cell := range []string{"other-project-agent-jwt other-project", "agent-binding global", "hub-uat global"} {
		assert.Positive(t, tally.cellRows[cell], "%s: rows compared", cell)
	}
	// The other project's messages were compared on the agent token path.
	for _, msg := range otherMessages {
		if msg != "" {
			assert.True(t, tally.cellVals["other-project-agent-jwt other-project"][rawMessage(msg)], "other project message %q compared for its agent token", msg)
		}
	}

	// Deletion: compact equals full for every caller class (the harness
	// compared the deletion field on every row, null views included), and
	// only the super admin sees code, error and claim. Every other class
	// that reads rows, the agent token paths and both user access tokens
	// among them, gets the generic view of the same rows.
	assert.Positive(t, tally.present["deletion"], "deletion compared")
	assert.Zero(t, tally.absent["deletion"], "the full item always carries deletion")
	views := assertDeletionRedactionPerCaller(t, tally, "super-admin", seeds)
	for _, caller := range []string{"owner", "member", "agent-jwt", "scoped-uat", "project-constrained", "other-project-agent-jwt", "agent-binding", "hub-uat"} {
		assert.Positive(t, views[caller], "%s: non-null deletion views checked", caller)
	}
	for _, cell := range []string{"owner global", "owner project", "owner other-project", "agent-jwt project", "scoped-uat project", "other-project-agent-jwt other-project"} {
		assert.Positive(t, tally.cellObjects[cell]["deletion"], "%s: non-null deletion views compared", cell)
	}

	// The full cell map: every caller and endpoint cell is pinned with its
	// status and whether it reads rows.
	for cell, want := range compactParityCells {
		assert.Equal(t, map[int]bool{want.status: true}, tally.cellStatus[cell], "%s: first-page statuses", cell)
		if want.reads {
			assert.Positive(t, tally.cellRows[cell], "%s: rows compared", cell)
		} else {
			assert.Zero(t, tally.cellRows[cell], "%s: no rows compared", cell)
		}
	}
	for cell := range tally.cellRows {
		_, ok := compactParityCells[cell]
		assert.True(t, ok, "%s: cell not pinned", cell)
	}
	assert.Len(t, compactParityCells, len(callers)*3, "every caller has a global, project and other-project cell")

	// Paging: every limit query in every reading cell walked two or more
	// pages, except the walks pinned here, whose project filter names a
	// project the caller cannot read on the global list.
	unreadableFilter := map[string]bool{}
	for _, caller := range []string{"other-project-member"} {
		unreadableFilter[caller+" global sort=updated&limit=1&projectId="+f.project.ID] = true
	}
	pagedWalks := 0
	for _, c := range callers {
		for _, q := range queries {
			if !q.paged() || !compactParityCells[c.name+" "+q.endpoint].reads {
				continue
			}
			walk := compactParityWalkKey(c.name, q)
			if unreadableFilter[walk] {
				assert.Zero(t, tally.walkRows[walk], "%s: rows compared", walk)
				continue
			}
			pagedWalks++
			assert.GreaterOrEqual(t, tally.walkPages[walk], 2, "%s: pages walked (%d rows)", walk, tally.walkRows[walk])
			// With limit=1, two or more compared rows means rows were compared on
			// at least two pages, not only requested.
			assert.GreaterOrEqual(t, tally.walkRows[walk], 2, "%s: rows compared across pages", walk)
		}
	}
	assert.Positive(t, pagedWalks, "paged walks checked")
}

// TestAgentCompactView_MessageCopiedAsIsForAnyValue checks that the
// compact item emits exactly the message bytes the full item emits: absent
// when empty, and the same escaped JSON string otherwise, including a
// message that is only white space.
func TestAgentCompactView_MessageCopiedAsIsForAnyValue(t *testing.T) {
	for _, msg := range []string{
		"",
		" ",
		"Waiting for review",
		"quotes \" backslash \\ <html> & unicode é 世界   newline\n tab\t",
		strings.Repeat("long detail ", 40),
	} {
		full := AgentWithCapabilities{Agent: store.Agent{ID: "a", Message: msg}}
		fb, err := json.Marshal(full)
		require.NoError(t, err)
		cb, err := json.Marshal(toCompact(full))
		require.NoError(t, err)
		fv, fok := decodeObject(t, fb)["message"]
		cv, cok := decodeObject(t, cb)["message"]
		assert.Equal(t, fok, cok, "%q: message presence", msg)
		assert.Equal(t, msg != "", cok, "%q: message emitted only when non-empty", msg)
		assert.Equal(t, string(fv), string(cv), "%q: message bytes", msg)
	}
}

// payloadMessage returns a realistic detail message for agent i: 30% empty,
// 50% between 40 and 120 characters, 20% between 200 and 400 characters.
func payloadMessage(i int) string {
	const words = "Waiting for review on the requested change while the targeted tests run against the hub package and the build checks the generated code for drift "
	clip := func(n int) string {
		s := strings.Repeat(words, n/len(words)+1)
		return strings.TrimSpace(s[:n])
	}
	switch b := i % 10; {
	case b < 3:
		return ""
	case b < 8:
		return clip(40 + (i*37)%81)
	default:
		return clip(200 + (i*53)%201)
	}
}

// worstCaseMessage returns a 2000-character detail message for agent i,
// distinct per agent, the size of a long free-text status.
func worstCaseMessage(i int) string {
	prefix := fmt.Sprintf("agent %d asks: ", i)
	return prefix + strings.Repeat("x", 2000-len(prefix))
}

// recordMessagePayload seeds n agents whose detail messages come from
// message, then requests the full and compact global list (and the project
// list when withProject is set) as the admin with sort=updated&fit=500. It
// logs the full bytes, the compact bytes and the compact bytes without the
// message key, which is what the compact view was before it carried the
// message. Compact must stay smaller than full.
func recordMessagePayload(t *testing.T, kind string, n int, message func(int) string, withProject bool) {
	t.Helper()
	f := globalSortedSetup(t)
	err := f.store.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < n; i++ {
			slug := fmt.Sprintf("msg-payload-%d", i)
			a := &store.Agent{
				ID: tid("cv-msg-" + slug), Slug: slug, Name: slug, Template: "claude",
				ProjectID: f.project.ID, Phase: "running", Activity: "idle",
				Message:   message(i),
				Labels:    map[string]string{"team": "a", "tier": "dev"},
				CreatedBy: f.admin.ID, OwnerID: f.admin.ID,
				AppliedConfig: &store.AgentAppliedConfig{
					CreatorName:   "Payload Creator",
					Image:         "us-docker.pkg.dev/example/scion/claude:latest",
					HarnessConfig: "claude",
					Model:         "claude-model",
					Task:          strings.Repeat("Implement the requested change and report back. ", 8),
					Branch:        "scion/" + slug,
					Env:           map[string]string{"GIT_AUTHOR_NAME": "Scion Agent", "SCION_PROJECT": "payload"},
				},
			}
			if err := tx.CreateAgent(context.Background(), a); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	// messageBytes is what the message key adds to the compact body: a
	// comma, the key and the encoded value, for every agent that has one.
	messageBytes := 0
	var segments []string
	for i := 0; i < n; i++ {
		if msg := message(i); msg != "" {
			raw, err := json.Marshal(msg)
			require.NoError(t, err)
			seg := `,"message":` + string(raw)
			segments = append(segments, seg)
			messageBytes += len(seg)
		}
	}
	eps := []struct{ name, base string }{{"global", f.listPath("")}}
	if withProject {
		eps = append(eps, struct{ name, base string }{"project", "/api/v1/projects/" + f.project.ID + "/agents"})
	}
	for _, ep := range eps {
		full := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, withQuery(ep.base, "sort=updated&fit=500"), nil)
		comp := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, withQuery(ep.base, "sort=updated&fit=500", "view=compact"), nil)
		require.Equal(t, http.StatusOK, full.Code, full.Body.String())
		require.Equal(t, http.StatusOK, comp.Code, comp.Body.String())
		require.Len(t, mustDecodeListAgentsResponse(t, comp.Body).Agents, n)
		for _, seg := range segments {
			require.Contains(t, comp.Body.String(), seg, "%s n=%d: compact body carries each message", ep.name, n)
		}
		fullLen, compLen := full.Body.Len(), comp.Body.Len()
		before := compLen - messageBytes
		t.Logf("message payload %s %s n=%d: full=%d bytes compact before=%d after=%d bytes (+%d; %.1f%% / %.1f%% of full)",
			kind, ep.name, n, fullLen, before, compLen, compLen-before,
			100*float64(before)/float64(fullLen), 100*float64(compLen)/float64(fullLen))
		assert.Less(t, compLen, fullLen, "%s %s n=%d", kind, ep.name, n)
	}
}

// TestAgentCompactView_MessagePayloadBytesRecord records, without asserting
// exact sizes, the full and compact response bytes with realistic detail
// messages at 25, 100 and 500 agents on the global endpoint and at 100 on
// the project endpoint, and the worst case of a 2000-character message on
// every agent at 25, 100 and 500 agents on the global endpoint. Compact must
// stay smaller than full.
func TestAgentCompactView_MessagePayloadBytesRecord(t *testing.T) {
	for _, n := range []int{25, 100, 500} {
		recordMessagePayload(t, "realistic", n, payloadMessage, n == 100)
	}
	for _, n := range []int{25, 100, 500} {
		recordMessagePayload(t, "worst-case", n, worstCaseMessage, false)
	}
}

// TestAgentCompactView_DeletionDetailAdminClasses runs the field parity
// harness with the admin caller classes of the deletion visibility rule
// (ptone/scion#3122): the unscoped local super admin, who sees code, error
// and claim; a super admin's project-scoped user access token and a
// federated user with the admin role, who get the generic view (the
// federated user reads no rows here; see the end of the test); and a
// member and an agent token for reference. The scoped admin is also a
// project owner, so its token reads the project's rows. Every non-admin
// row must be the super admin's view without the detail fields.
func TestAgentCompactView_DeletionDetailAdminClasses(t *testing.T) {
	f := compactSetup(t)
	seeds := seedCompactDeletions(t, f)
	ctx := context.Background()

	scopedAdminID := tid("cv-scoped-admin")
	createTestUserWithRole(t, f.store, scopedAdminID, "cv-scoped-admin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.store, scopedAdminID)
	createTestUserWithProjectRole(t, f.store, scopedAdminID, "cv-scoped-admin@test.com", f.project.ID, store.ProjectRoleOwner)
	scopedAdminKey := mintScopedUAT(t, f.srv, scopedAdminID, f.project.ID, []string{"agent:manage"})

	federatedAdmin := NewFederatedUserIdentity("https://issuer.example", "cv-fed-admin", "cv-fed-admin@example.com", "Fed Admin", "admin", nil)

	callers := []compactCaller{
		f.caller("super-admin"),
		f.caller("member"),
		f.caller("agent-jwt"),
		{"scoped-admin-uat", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithUAT(t, f.srv, scopedAdminKey, http.MethodGet, path, nil)
		}},
		{"federated-admin", func(t *testing.T, path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(contextWithIdentity(req.Context(), federatedAdmin))
			rec := httptest.NewRecorder()
			f.srv.mux.ServeHTTP(rec, req)
			return rec
		}},
	}
	other := "/api/v1/projects/" + f.other.ID + "/agents"
	queries := []compactParityQuery{
		{"global", f.globalBase(), ""},
		{"global", f.globalBase(), "sort=created&dir=asc&fit=500"},
		{"project", f.projectBase(), ""},
		{"project", f.projectBase(), "sort=updated&dir=desc&limit=1"},
		{"other-project", other, ""},
	}
	tally := runCompactFieldParity(t, f, callers, queries, compactParityFields)

	views := assertDeletionRedactionPerCaller(t, tally, "super-admin", seeds)
	for _, caller := range []string{"member", "agent-jwt", "scoped-admin-uat"} {
		assert.Positive(t, views[caller], "%s: non-null deletion views checked", caller)
	}
	assert.Positive(t, tally.cellObjects["scoped-admin-uat project"]["deletion"], "the scoped admin token reads deletion views on its project")
	// The federated admin cannot read agent rows on these endpoints at all:
	// a federated principal's ID is issuer:subject, not a local user UUID,
	// so it holds no project membership or role binding and authorization
	// resolves no grant for it (the same fail-closed path pinned for skills
	// by TestListSkills_FederatedUserFailsClosedNotServerError). No fixture
	// can grant it membership, so this harness pins that it reads nothing
	// and never errors, in both views alike (the harness compares status
	// and body). That federated users get the generic deletion view is
	// pinned by TestCallerSeesDeletionDetail_OnlyUnscopedLocalAdmins and
	// TestEnrichAgent_DeletionDetailFailsClosed, which call the predicate
	// and both enrichment functions with a federated admin identity.
	for _, ep := range []string{"global", "project", "other-project"} {
		cell := "federated-admin " + ep
		require.NotEmpty(t, tally.cellStatus[cell], "%s: requested", cell)
		for status := range tally.cellStatus[cell] {
			assert.Less(t, status, 500, "%s: first-page status %d", cell, status)
		}
		assert.Zero(t, tally.cellRows[cell], "%s: a federated principal reads no agent rows", cell)
	}
	assert.Zero(t, views["federated-admin"], "federated-admin: no deletion views read")
}
