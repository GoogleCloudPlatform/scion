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
	"encoding"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compactItemAllowlist is the exact JSON key set of a view=compact agent
// item when every field is populated.
var compactItemAllowlist = []string{
	"id", "slug", "name", "template", "projectId", "project", "labels",
	"phase", "activity", "containerStatus", "messageMode", "ancestry",
	"createdBy", "creatorName", "created", "updated", "lastActivityEvent",
	"_capabilities", "_messageability", "deletion",
}

// compactCaller is one identity class making requests through the real
// HTTP handler (or, for the unauthenticated case, the handler directly).
type compactCaller struct {
	name string
	do   func(t *testing.T, path string) *httptest.ResponseRecorder
}

// compactFixture holds one project with a mix of agents readable to
// different identity classes, a second project, and one caller per
// identity class.
type compactFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	other   *store.Project
	owner   *store.User
	member  *store.User
	agents  []*store.Agent // agents in project, in creation order
	callers []compactCaller
	// shortCircuit holds callers that reach the global endpoint's empty
	// short-circuit responses (no identity, None scope).
	shortCircuit []compactCaller
}

func (f *compactFixture) globalBase() string { return "/api/v1/agents" }
func (f *compactFixture) projectBase() string {
	return "/api/v1/projects/" + f.project.ID + "/agents"
}

func (f *compactFixture) caller(name string) compactCaller {
	for _, c := range append(append([]compactCaller{}, f.callers...), f.shortCircuit...) {
		if c.name == name {
			return c
		}
	}
	panic("no caller " + name)
}

func compactUser(t *testing.T, s store.Store, slug string) *store.User {
	t.Helper()
	u := &store.User{
		ID: tid("cv-" + slug), Email: "cv-" + slug + "@test.com", DisplayName: slug,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func compactSetup(t *testing.T) *compactFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &compactFixture{srv: srv, store: s}

	f.owner = compactUser(t, s, "owner")
	f.member = compactUser(t, s, "member")
	related := compactUser(t, s, "related")
	constrained := compactUser(t, s, "constrained")

	f.project = &store.Project{
		ID: tid("cv-project"), Name: "Compact View", Slug: "cv-project",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.createProjectMembersGroup(ctx, f.project)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.project.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, f.member.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	msgAuthzAddProjectMember(t, s, constrained.ID, f.project.ID, f.project.Slug, store.GroupMemberRoleMember)
	// related holds only agent.list on the project and owns two of its agents.
	grantProjectListOnly(t, s, related.ID, f.project.ID, "cv-list-only")

	f.other = &store.Project{
		ID: tid("cv-other"), Name: "Compact Other", Slug: "cv-other",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.other))
	srv.createProjectMembersGroup(ctx, f.other)
	createTestUserWithProjectRole(t, s, f.owner.ID, f.owner.Email, f.other.ID, store.ProjectRoleOwner)
	msgAuthzAddProjectMember(t, s, constrained.ID, f.other.ID, f.other.Slug, store.GroupMemberRoleMember)
	// A project-scoped access constraint removes agent.list in the other
	// project for constrained.
	pType := "user"
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "cv-block-other",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(constrained.ID),
		ScopeType:            "project",
		ScopeID:              f.other.ID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "compact view parity",
		CreatedBy:            f.owner.ID,
	})
	require.NoError(t, err)

	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	// updatedOrder makes the updated order differ from the created order.
	updatedOrder := []int{4, 0, 6, 2, 5, 1, 3}
	for i := 0; i < 7; i++ {
		owner := f.owner.ID
		if i == 1 || i == 4 {
			owner = related.ID
		}
		a := &store.Agent{
			ID: tid(fmt.Sprintf("cv-agent-%d", i)), Slug: fmt.Sprintf("cv-agent-%d", i), Name: fmt.Sprintf("Agent %d", i),
			Template:  []string{"", "claude"}[i%2],
			ProjectID: f.project.ID, Phase: []string{"running", "stopped"}[i%2],
			Activity:  []string{"", "idle", "executing"}[i%3],
			Labels:    map[string]string{"team": []string{"a", "b"}[i%2]},
			CreatedBy: owner, OwnerID: owner,
			AppliedConfig: &store.AgentAppliedConfig{
				CreatorName: fmt.Sprintf("Creator %d", i),
				Image:       "example.com/agent:latest",
				Model:       "model-x",
				Task:        strings.Repeat("task text ", 20),
				Env:         map[string]string{"SECRET_TOKEN": "s3cr3t"},
			},
		}
		if i >= 3 {
			a.Ancestry = []string{f.agentIDAt(0)}
		}
		if i == 5 {
			a.Ancestry = []string{f.agentIDAt(0), f.agentIDAt(3)}
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		lae := ""
		if i%3 == 0 {
			lae = base.Add(time.Duration(30+i) * time.Second).String()
		}
		setRawAgentTimes(t, s, a.ID,
			base.Add(time.Duration(i)*time.Second).String(),
			base.Add(time.Duration(10+updatedOrder[i])*time.Second).String(), lae)
		f.agents = append(f.agents, a)
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid(fmt.Sprintf("cv-other-%d", i)), Slug: fmt.Sprintf("cv-other-%d", i), Name: fmt.Sprintf("Other %d", i),
			ProjectID: f.other.ID, Phase: "stopped", CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
			AppliedConfig: &store.AgentAppliedConfig{CreatorName: "Other Creator"},
		}))
	}

	hubAdminID := tid("cv-hub-admin")
	createTestUserWithRole(t, s, hubAdminID, "cv-hub-admin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	hubAdmin, err := s.GetUser(ctx, hubAdminID)
	require.NoError(t, err)
	superAdminID := tid("cv-super-admin")
	createTestUserWithRole(t, s, superAdminID, "cv-super-admin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	superAdmin, err := s.GetUser(ctx, superAdminID)
	require.NoError(t, err)

	agentTok, err := srv.GetAgentTokenService().GenerateAgentToken(f.agents[0].ID, f.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	require.NoError(t, err)
	uatKey := mintScopedUAT(t, srv, f.owner.ID, f.project.ID, []string{"agent:manage"})

	asUser := func(u *store.User) func(t *testing.T, path string) *httptest.ResponseRecorder {
		return func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, srv, u, http.MethodGet, path, nil)
		}
	}
	f.callers = []compactCaller{
		{"owner", asUser(f.owner)},
		{"member", asUser(f.member)},
		{"hub-admin-non-member", asUser(hubAdmin)},
		{"super-admin", asUser(superAdmin)},
		{"agent-jwt", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, agentTok)
		}},
		{"scoped-uat", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestWithUAT(t, srv, uatKey, http.MethodGet, path, nil)
		}},
		{"project-constrained", asUser(constrained)},
		{"relationship-grant", asUser(related)},
	}

	none := noScopeUser(t, s)
	f.shortCircuit = []compactCaller{
		{"none-scope", asUser(none)},
		{"unauthenticated", func(t *testing.T, path string) *httptest.ResponseRecorder {
			u, err := url.Parse(path)
			require.NoError(t, err)
			require.Equal(t, "/api/v1/agents", u.Path, "the unauthenticated short-circuit is a global endpoint path")
			return listAgentsUnauthenticated(srv, u.RawQuery)
		}},
	}
	return f
}

// agentIDAt is the id the fixture gives to project agent i.
func (f *compactFixture) agentIDAt(i int) string { return tid(fmt.Sprintf("cv-agent-%d", i)) }

// withQuery joins a base path and query parts, skipping empty parts.
func withQuery(base string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// dropParam removes every name=... part from an & separated query.
func dropParam(query, name string) string {
	var kept []string
	for _, p := range strings.Split(query, "&") {
		if p != "" && !strings.HasPrefix(p, name+"=") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "&")
}

// auditKeys reduces decision audit records to the fields that identify a
// decision and its outcome, in emission order.
func auditKeys(records []*store.DecisionAuditRecord) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = strings.Join([]string{r.PrincipalKind, r.PrincipalID, r.ResourceType, r.ResourceID, r.Permission, r.Result, r.Reason}, "|")
	}
	return out
}

// viewPair is one request made in both views, with what each cost.
type viewPair struct {
	full, compact           *httptest.ResponseRecorder
	fullAudit, compactAudit []*store.DecisionAuditRecord
}

// requestBothViews issues the same request without view and with
// view=compact, recording the decision audit records of each.
func (f *compactFixture) requestBothViews(t *testing.T, c compactCaller, base, query string) viewPair {
	t.Helper()
	var p viewPair
	em := &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(em)
	p.full = c.do(t, withQuery(base, query))
	p.fullAudit = em.records
	em = &recordingDecisionAuditEmitter{}
	f.srv.authzService.SetDecisionAuditEmitter(em)
	p.compact = c.do(t, withQuery(base, query, "view=compact"))
	p.compactAudit = em.records
	f.srv.authzService.SetDecisionAuditEmitter(nil)
	return p
}

func decodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), string(body))
	return m
}

func decodeItems(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &items))
	return items
}

func rawKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isEmptyJSON reports whether raw is a value omitempty would drop.
func isEmptyJSON(raw json.RawMessage) bool {
	switch string(raw) {
	case `""`, `null`, `{}`, `[]`, `false`, `0`:
		return true
	}
	return false
}

// assertNoKeyAtAnyDepth fails if key appears as an object key anywhere in v.
func assertNoKeyAtAnyDepth(t *testing.T, label string, body []byte, key string) {
	t.Helper()
	var v interface{}
	require.NoError(t, json.Unmarshal(body, &v))
	var walk func(path string, v interface{})
	walk = func(path string, v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, child := range x {
				if k == key {
					t.Errorf("%s: key %q found at %s", label, key, path)
				}
				walk(path+"."+k, child)
			}
		case []interface{}:
			for i, child := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("$", v)
}

// creatorNameOf returns the full item's appliedConfig.creatorName, raw, or
// nil if absent.
func creatorNameOf(t *testing.T, fullItem map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	ac, ok := fullItem["appliedConfig"]
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(ac, &m))
	return m["creatorName"]
}

// assertCompactParity checks one full/compact response pair:
//   - same status; non-200 bodies identical;
//   - same top-level key set, and every top-level value other than agents
//     and serverTime byte-identical (nextCursor, totalCount, complete,
//     sort, dir, stats, _capabilities);
//   - the same agents in the same order;
//   - every compact item key is in the allowlist and exists in the full
//     item for the same agent with a byte-identical value (creatorName
//     against appliedConfig.creatorName), so compact is a strict subset;
//   - every allowlisted value the full item carries non-empty is present
//     in the compact item, and _capabilities/_messageability/deletion are
//     byte-identical including absence;
//   - no appliedConfig key at any depth of the compact body;
//   - identical decision audit records, in order.
//
// It returns the number of agent items compared.
func assertCompactParity(t *testing.T, label string, p viewPair) int {
	t.Helper()
	require.Equal(t, p.full.Code, p.compact.Code, "%s: status", label)
	assert.Equal(t, auditKeys(p.fullAudit), auditKeys(p.compactAudit), "%s: decision audit records", label)
	assert.Len(t, p.compactAudit, len(p.fullAudit), "%s: decision and audit count", label)

	fullBody, compBody := rawBodyWithoutServerTime(p.full), rawBodyWithoutServerTime(p.compact)
	if p.full.Code != http.StatusOK {
		assert.Equal(t, string(fullBody), string(compBody), "%s: non-200 body", label)
		return 0
	}
	assertNoKeyAtAnyDepth(t, label+": compact body", compBody, "appliedConfig")

	fm, cm := decodeObject(t, fullBody), decodeObject(t, compBody)
	require.Equal(t, rawKeys(fm), rawKeys(cm), "%s: top-level keys", label)
	for k := range fm {
		if k == "agents" || k == "serverTime" {
			continue
		}
		assert.Equal(t, string(fm[k]), string(cm[k]), "%s: top-level %q bytes", label, k)
	}

	fullItems, compItems := decodeItems(t, fm["agents"]), decodeItems(t, cm["agents"])
	require.Len(t, compItems, len(fullItems), "%s: item count", label)
	allow := map[string]bool{}
	for _, k := range compactItemAllowlist {
		allow[k] = true
	}
	for i := range fullItems {
		fi, ci := fullItems[i], compItems[i]
		require.Equal(t, string(fi["id"]), string(ci["id"]), "%s: item %d id/order", label, i)
		for k, cv := range ci {
			assert.True(t, allow[k], "%s: item %d: compact key %q not in allowlist", label, i, k)
			if k == "creatorName" {
				assert.Equal(t, string(creatorNameOf(t, fi)), string(cv), "%s: item %d creatorName", label, i)
				continue
			}
			fv, ok := fi[k]
			if assert.True(t, ok, "%s: item %d: compact key %q missing from full item", label, i, k) {
				assert.Equal(t, string(fv), string(cv), "%s: item %d key %q", label, i, k)
			}
		}
		for _, k := range compactItemAllowlist {
			if k == "creatorName" {
				if cn := creatorNameOf(t, fi); cn != nil && !isEmptyJSON(cn) {
					assert.Equal(t, string(cn), string(ci[k]), "%s: item %d creatorName present", label, i)
				}
				continue
			}
			if fv, ok := fi[k]; ok && !isEmptyJSON(fv) {
				assert.Contains(t, ci, k, "%s: item %d: full value of %q dropped from compact", label, i, k)
			}
		}
		for _, k := range []string{"_capabilities", "_messageability", "deletion"} {
			assert.Equal(t, string(fi[k]), string(ci[k]), "%s: item %d %s (including absence)", label, i, k)
		}
	}
	return len(fullItems)
}

// compactParityModes is every list mode, as a first-page query. Paged
// modes are walked to the end by following nextCursor.
var compactParityModes = []string{
	"",                      // legacy, default limit
	"limit=3",               // legacy, paged walk
	"phase=stopped&limit=2", // legacy, filtered paged walk
	"sort=created&dir=desc&limit=3",
	"sort=created&dir=asc&limit=3",
	"sort=updated&dir=desc&limit=3",
	"sort=updated&dir=asc&limit=3",
	"sort=updated&fit=500",                 // fit, complete
	"sort=created&dir=asc&fit=500&stats=1", // fit, complete, stats
	"sort=updated&limit=2&fit=2",           // fit, paged, then walked
	"sort=updated&limit=2&fit=2&stats=1",   // fit, paged, stats
	"sort=updated&stats=1&limit=3&phase=running",
	"sort=updated&limit=2&label=team=a",
}

// walkParity walks one mode to its last page in both views, asserting
// parity on every page, and returns the number of items compared and the
// status of the first page.
func (f *compactFixture) walkParity(t *testing.T, c compactCaller, endpoint, base, mode string) (int, int) {
	t.Helper()
	compared, firstStatus := 0, 0
	query := mode
	for page := 0; page < 20; page++ {
		label := fmt.Sprintf("%s %s %q page %d", c.name, endpoint, mode, page)
		p := f.requestBothViews(t, c, base, query)
		if page == 0 {
			firstStatus = p.full.Code
		}
		compared += assertCompactParity(t, label, p)
		if p.full.Code != http.StatusOK {
			return compared, firstStatus
		}
		next := mustDecodeListAgentsResponse(t, p.full.Body).NextCursor
		if next == "" {
			return compared, firstStatus
		}
		// Continuation pages carry the cursor and drop fit, which is not
		// valid together with a cursor.
		query = withCursor(dropParam(mode, "fit"), next)
		query = strings.TrimPrefix(query, "&")
	}
	t.Fatalf("%s %s %q: walk did not end", c.name, endpoint, mode)
	return compared, firstStatus
}

// wantParityItems is the exact number of items the parity test compares,
// summed over every mode and page, per identity class and endpoint. A zero
// is an identity class that sees no agents on that endpoint (a 403 on the
// project endpoint, or an empty global list); an unexpected change in any
// class's population fails the parity test instead of leaving it with
// nothing to compare. The cells in positiveParityItems are checked for a
// non-zero total instead of an exact one.
var wantParityItems = map[string]int{
	"owner global":                 114,
	"owner project":                81,
	"member global":                81,
	"member project":               81,
	"hub-admin-non-member global":  0,
	"hub-admin-non-member project": 0,
	"super-admin global":           114,
	"super-admin project":          81,
	"agent-jwt global":             0,
	"agent-jwt project":            81,
	"scoped-uat global":            81,
	"scoped-uat project":           81,
	"project-constrained global":   81,
	"project-constrained project":  81,
	"relationship-grant project":   23,
}

var positiveParityItems = map[string]bool{
	"relationship-grant global": true,
}

// TestAgentCompactView_ParityWithFullViewForEveryIdentityClassAndMode is the
// compact projection's main guarantee: for every identity class, on both
// endpoints, in every mode and on every page of a cursor walk, view=compact
// returns the same agents in the same order with byte-identical totals,
// cursors, complete, stats, capabilities, messageability and scope
// capabilities, costs exactly the same decisions and audit records, and
// each compact item is a strict subset of its full item.
func TestAgentCompactView_ParityWithFullViewForEveryIdentityClassAndMode(t *testing.T) {
	f := compactSetup(t)

	// The full view must actually carry appliedConfig here, or the
	// "no appliedConfig" assertions would be vacuous.
	rec := f.caller("owner").do(t, f.projectBase())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"appliedConfig"`)
	require.Contains(t, rec.Body.String(), `"creatorName"`)

	wantProjectStatus := map[string]int{"hub-admin-non-member": http.StatusForbidden}
	require.Len(t, wantParityItems, 2*len(f.callers)-len(positiveParityItems), "one expected item total per identity class and endpoint")
	for cell := range positiveParityItems {
		_, dup := wantParityItems[cell]
		require.False(t, dup, "%s: both exact and positive-only", cell)
	}
	for _, c := range f.callers {
		for _, ep := range []struct{ name, base string }{{"global", f.globalBase()}, {"project", f.projectBase()}} {
			total := 0
			for _, mode := range compactParityModes {
				n, status := f.walkParity(t, c, ep.name, ep.base, mode)
				total += n
				if ep.name == "project" {
					want, ok := wantProjectStatus[c.name]
					if !ok {
						want = http.StatusOK
					}
					assert.Equal(t, want, status, "%s project %q: first page status", c.name, mode)
				}
			}
			if positiveParityItems[c.name+" "+ep.name] {
				assert.Positive(t, total, "%s %s: items compared across all modes and pages", c.name, ep.name)
				continue
			}
			want, ok := wantParityItems[c.name+" "+ep.name]
			require.True(t, ok, "%s %s: no expected item total", c.name, ep.name)
			assert.Equal(t, want, total, "%s %s: items compared across all modes and pages", c.name, ep.name)
		}
	}
	for _, c := range f.shortCircuit {
		for _, mode := range compactParityModes {
			n, status := f.walkParity(t, c, "global", f.globalBase(), mode)
			assert.Equal(t, http.StatusOK, status, "%s %q", c.name, mode)
			assert.Zero(t, n, "%s %q: short-circuit returns no agents", c.name, mode)
		}
	}
}

// deletionParityModes is the subset of list modes the deletion parity test
// walks: legacy, a sorted cursor walk, and a fit answer.
var deletionParityModes = []string{
	"",
	"sort=updated&dir=desc&limit=3",
	"sort=updated&fit=500",
}

// TestAgentCompactView_DeletionViewParityForEveryIdentityClass seeds a live
// deleting view on one agent and a failed view on another, both readable to
// every identity class that reads any agent of the project, and leaves every
// other agent with no delete. For every identity class that reads the
// project's agents on an endpoint (the fixture's classes, plus a project
// admin), it checks that each compact item's deletion bytes equal the full
// item's for the same agent and caller: the deleting view, the failed view,
// and the explicit null both views emit when no delete is active or failed.
// Every other cell, a runtime broker identity on both endpoints among them,
// is checked to read nothing: both views answer the cell's exact status, a
// 403 or a 200 with an empty list.
func TestAgentCompactView_DeletionViewParityForEveryIdentityClass(t *testing.T) {
	f := compactSetup(t)
	deletingID, failedID := f.agentIDAt(1), f.agentIDAt(4)
	seedAgentDeletion(t, f.store, deletingID, deleteSeed{state: store.DeletionStateDeleting, leaseIn: time.Hour})
	seedAgentDeletion(t, f.store, failedID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})
	errMsg := "broker unreachable"
	n, err := f.store.UpdateAgentDeletion(context.Background(), failedID, store.DeletionPredicate{}, store.DeletionFields{Error: &errMsg})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	adminID := tid("cv-project-admin")
	createTestUserWithProjectRole(t, f.store, adminID, "cv-project-admin@test.com", f.project.ID, store.ProjectRoleAdmin)
	admin, err := f.store.GetUser(context.Background(), adminID)
	require.NoError(t, err)
	brokerIdent := NewBrokerIdentity(tid("cv-broker"))
	callers := append(append([]compactCaller{}, f.callers...),
		compactCaller{"project-admin", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, admin, http.MethodGet, path, nil)
		}},
		compactCaller{"runtime-broker", func(t *testing.T, path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			ctx := contextWithBrokerIdentity(req.Context(), brokerIdent)
			req = req.WithContext(contextWithIdentity(ctx, brokerIdent))
			rec := httptest.NewRecorder()
			f.srv.mux.ServeHTTP(rec, req)
			return rec
		}},
	)

	// Identity classes that read no agent of the project on an endpoint,
	// each with the status both views answer: a 403, or a 200 with an
	// empty list.
	blind := map[string]int{
		"hub-admin-non-member global":  http.StatusOK,
		"hub-admin-non-member project": http.StatusForbidden,
		"agent-jwt global":             http.StatusOK,
		"runtime-broker global":        http.StatusOK,
		"runtime-broker project":       http.StatusForbidden,
	}
	for _, c := range callers {
		for _, ep := range []struct{ name, base string }{{"global", f.globalBase()}, {"project", f.projectBase()}} {
			cell := c.name + " " + ep.name
			for _, mode := range deletionParityModes {
				f.walkParity(t, c, ep.name, ep.base, mode)
			}
			p := f.requestBothViews(t, c, ep.base, "sort=updated&fit=500")
			if wantStatus, ok := blind[cell]; ok {
				assert.Equal(t, wantStatus, p.full.Code, "%s: full view status: %s", cell, p.full.Body.String())
				assert.Equal(t, wantStatus, p.compact.Code, "%s: compact view status: %s", cell, p.compact.Body.String())
				if wantStatus == http.StatusOK {
					assert.Empty(t, mustDecodeListAgentsResponse(t, p.full.Body).Agents, "%s: full view reads no agents", cell)
					assert.Empty(t, mustDecodeListAgentsResponse(t, p.compact.Body).Agents, "%s: compact view reads no agents", cell)
				}
				continue
			}
			require.Equal(t, http.StatusOK, p.full.Code, "%s: %s", cell, p.full.Body.String())
			require.Equal(t, http.StatusOK, p.compact.Code, "%s: %s", cell, p.compact.Body.String())
			fullItems := decodeItems(t, decodeObject(t, p.full.Body.Bytes())["agents"])
			compItems := decodeItems(t, decodeObject(t, p.compact.Body.Bytes())["agents"])
			require.Len(t, compItems, len(fullItems), cell)
			seen := map[string]bool{}
			for i := range fullItems {
				var id string
				require.NoError(t, json.Unmarshal(compItems[i]["id"], &id))
				fd, fok := fullItems[i]["deletion"]
				cd, cok := compItems[i]["deletion"]
				require.True(t, fok, "%s: full item %s carries deletion", cell, id)
				require.True(t, cok, "%s: compact item %s carries deletion", cell, id)
				assert.Equal(t, string(fd), string(cd), "%s: item %s deletion bytes", cell, id)
				var d *store.DeletionInfo
				require.NoError(t, json.Unmarshal(cd, &d))
				switch id {
				case deletingID:
					seen[id] = true
					if assert.NotNil(t, d, "%s: deleting view", cell) {
						assert.Equal(t, store.DeletionStateDeleting, d.State, cell)
						assert.NotNil(t, d.LeaseExpiresAt, "%s: deleting view lease", cell)
					}
				case failedID:
					seen[id] = true
					if assert.NotNil(t, d, "%s: failed view", cell) {
						assert.Equal(t, store.DeletionStateFailed, d.State, cell)
						assert.Equal(t, store.DeletionCodeRuntimeError, d.Code, cell)
						assert.Equal(t, errMsg, d.Error, cell)
					}
				default:
					assert.Equal(t, "null", string(cd), "%s: item %s has no delete", cell, id)
				}
			}
			assert.True(t, seen[deletingID], "%s: reads the deleting agent", cell)
			assert.True(t, seen[failedID], "%s: reads the failed agent", cell)
		}
	}
}

// TestAgentCompactView_ReadFilterAndConstraintAreAppliedBeforeProjection
// pins that the identity classes in the parity test really see different
// sets, so parity is not trivially comparing identical populations.
func TestAgentCompactView_ReadFilterAndConstraintAreAppliedBeforeProjection(t *testing.T) {
	f := compactSetup(t)
	ids := func(c compactCaller, path string) []string {
		rec := c.do(t, path)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp listAgentsCompactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		var out []string
		for _, a := range resp.Agents {
			out = append(out, a.ID)
		}
		sort.Strings(out)
		return out
	}
	related := ids(f.caller("relationship-grant"), withQuery(f.projectBase(), "view=compact", "sort=updated&fit=500"))
	assert.ElementsMatch(t, []string{f.agentIDAt(1), f.agentIDAt(4)}, related, "relationship grant reads only its own agents")

	constrained := ids(f.caller("project-constrained"), withQuery(f.globalBase(), "view=compact"))
	ownerAll := ids(f.caller("owner"), withQuery(f.globalBase(), "view=compact"))
	assert.Len(t, ownerAll, 10, "owner sees both projects")
	assert.Len(t, constrained, 7, "the constraint removes the other project's agents")
	for i := 0; i < 3; i++ {
		assert.NotContains(t, constrained, tid(fmt.Sprintf("cv-other-%d", i)))
	}
}

// TestAgentCompactView_KeySetIsAllowlist fixes the compact item JSON key
// set: with every field populated, the global endpoint's item keys equal
// the allowlist exactly, the project endpoint's equal it minus
// _messageability (the full view sets messageability on the global
// endpoint only), and the struct's own JSON names equal the allowlist.
func TestAgentCompactView_KeySetIsAllowlist(t *testing.T) {
	f := compactSetup(t)
	ctx := context.Background()
	full := &store.Agent{
		ID: tid("cv-allkeys"), Slug: "cv-allkeys", Name: "All Keys", Template: "claude",
		ProjectID: f.project.ID, Labels: map[string]string{"k": "v"},
		Phase: "running", Activity: "executing", ContainerStatus: "Up 5 minutes",
		MessageMode: "project", Ancestry: []string{f.agentIDAt(0)},
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{CreatorName: "All Keys Creator", Env: map[string]string{"A": "b"}},
	}
	require.NoError(t, f.store.CreateAgent(ctx, full))
	setRawAgentTimes(t, f.store, full.ID, "2026-03-04 05:07:00 +0000 UTC", "2026-03-04 05:08:00 +0000 UTC", "2026-03-04 05:09:00 +0000 UTC")
	seedAgentDeletion(t, f.store, full.ID, deleteSeed{state: store.DeletionStateDeleting, leaseIn: time.Hour})

	var tagNames []string
	rt := reflect.TypeOf(AgentCompactItem{})
	for i := 0; i < rt.NumField(); i++ {
		tagNames = append(tagNames, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	assert.ElementsMatch(t, compactItemAllowlist, tagNames, "AgentCompactItem JSON names")

	find := func(body []byte) map[string]json.RawMessage {
		for _, item := range decodeItems(t, decodeObject(t, body)["agents"]) {
			if string(item["id"]) == `"`+full.ID+`"` {
				return item
			}
		}
		t.Fatalf("agent %s not in response", full.ID)
		return nil
	}
	owner := f.caller("owner")

	rec := owner.do(t, withQuery(f.globalBase(), "view=compact"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, sortedKeysOf(compactItemAllowlist), rawKeys(find(rec.Body.Bytes())), "global compact item keys")

	rec = owner.do(t, withQuery(f.projectBase(), "view=compact", "sort=updated&fit=500"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var want []string
	for _, k := range compactItemAllowlist {
		if k != "_messageability" {
			want = append(want, k)
		}
	}
	assert.Equal(t, sortedKeysOf(want), rawKeys(find(rec.Body.Bytes())), "project compact item keys")
}

func sortedKeysOf(keys []string) []string {
	out := append([]string{}, keys...)
	sort.Strings(out)
	return out
}

// TestAgentCompactView_ZeroLastActivityEventEmittedLikeFullView pins that
// an unset lastActivityEvent is emitted as the zero time in compact, as in
// the full view, and that omitempty fields are omitted when empty while
// messageMode, ids, times and deletion (an explicit null, as in the full
// view) are always present.
func TestAgentCompactView_ZeroLastActivityEventEmittedLikeFullView(t *testing.T) {
	item := toCompact(AgentWithCapabilities{Agent: store.Agent{ID: "a", Slug: "s", Name: "n", ProjectID: "p"}})
	b, err := json.Marshal(item)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"a","slug":"s","name":"n","projectId":"p","messageMode":"",`+
		`"created":"0001-01-01T00:00:00Z","updated":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z",`+
		`"deletion":null}`, string(b))

	f := compactSetup(t)
	rec := f.caller("owner").do(t, withQuery(f.projectBase(), "view=compact", "sort=created&dir=asc&limit=2"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	items := decodeItems(t, decodeObject(t, rec.Body.Bytes())["agents"])
	require.Len(t, items, 2)
	assert.Equal(t, `"0001-01-01T00:00:00Z"`, string(items[1]["lastActivityEvent"]), "agent 1 has no last activity event")
}

// TestAgentCompactView_CreatorNameEqualsFullAppliedConfigCreatorName pins
// creatorName against the full view's appliedConfig.creatorName for every
// agent, on both endpoints.
func TestAgentCompactView_CreatorNameEqualsFullAppliedConfigCreatorName(t *testing.T) {
	f := compactSetup(t)
	owner := f.caller("owner")
	for _, base := range []string{f.globalBase(), f.projectBase()} {
		full := owner.do(t, base)
		comp := owner.do(t, withQuery(base, "view=compact"))
		require.Equal(t, http.StatusOK, full.Code)
		require.Equal(t, http.StatusOK, comp.Code)
		var fr ListAgentsResponse
		var cr listAgentsCompactResponse
		require.NoError(t, json.Unmarshal(full.Body.Bytes(), &fr))
		require.NoError(t, json.Unmarshal(comp.Body.Bytes(), &cr))
		require.Len(t, cr.Agents, len(fr.Agents))
		checked := 0
		for i := range fr.Agents {
			require.Equal(t, fr.Agents[i].ID, cr.Agents[i].ID)
			require.NotNil(t, fr.Agents[i].AppliedConfig)
			assert.Equal(t, fr.Agents[i].AppliedConfig.CreatorName, cr.Agents[i].CreatorName, base)
			assert.NotEmpty(t, cr.Agents[i].CreatorName)
			checked++
		}
		assert.Positive(t, checked)
	}
}

// cursorOf returns the nextCursor of a 200 response.
func cursorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
	require.NotEmpty(t, cur)
	return cur
}

// TestAgentCompactView_CursorBindingPerIdentityAndEndpointMatchesFullView
// repeats the cursor binding checks per identity class: a cursor minted in
// either view by one identity on one endpoint, replayed by every identity
// on every endpoint, gets the same status and body in both views; it is
// accepted by the identity and endpoint that minted it, refused with 400
// everywhere else, except by a caller whose global scope is None, whose
// empty short-circuit list never reads a cursor, and a project non-member,
// refused 403 at the project gate.
func TestAgentCompactView_CursorBindingPerIdentityAndEndpointMatchesFullView(t *testing.T) {
	f := compactSetup(t)
	// Callers that resolve to no global agent.list in this fixture.
	noneScopeGlobal := []string{"hub-admin-non-member", "agent-jwt"}
	for _, name := range noneScopeGlobal {
		rec := f.caller(name).do(t, withQuery(f.globalBase(), "limit=1"))
		require.Equal(t, http.StatusOK, rec.Code)
		resp := mustDecodeListAgentsResponse(t, rec.Body)
		require.Empty(t, resp.Agents, "%s: expected the None-scope global list", name)
		require.Empty(t, resp.NextCursor, name)
	}
	endpoints := []struct{ name, base string }{{"global", f.globalBase()}, {"project", f.projectBase()}}
	for _, mode := range []string{"limit=1", "sort=updated&limit=1"} {
		for _, minter := range f.callers {
			for _, mintEP := range endpoints {
				for _, mintView := range []string{"", "view=compact"} {
					rec := minter.do(t, withQuery(mintEP.base, mode, mintView))
					if rec.Code != http.StatusOK {
						continue
					}
					cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
					if cur == "" {
						continue
					}
					for _, replayer := range f.callers {
						for _, replayEP := range endpoints {
							label := fmt.Sprintf("%q minted by %s on %s (%q), replayed by %s on %s",
								mode, minter.name, mintEP.name, mintView, replayer.name, replayEP.name)
							q := withCursor(mode, cur)
							full := replayer.do(t, withQuery(replayEP.base, q))
							comp := replayer.do(t, withQuery(replayEP.base, q, "view=compact"))
							require.Equal(t, full.Code, comp.Code, label)
							if full.Code != http.StatusOK {
								assert.Equal(t, string(rawBodyWithoutServerTime(full)), string(rawBodyWithoutServerTime(comp)), label)
							}
							switch {
							case replayer.name == minter.name && replayEP.name == mintEP.name:
								assert.Equal(t, http.StatusOK, full.Code, "%s: own replay: %s", label, full.Body.String())
							case full.Code == http.StatusOK:
								// A caller whose global list scope is None gets
								// the empty short-circuit list, which never
								// reads the cursor: the replay discloses
								// nothing and continues nothing.
								resp := mustDecodeListAgentsResponse(t, full.Body)
								assert.Equal(t, "global", replayEP.name, "%s: foreign replay accepted", label)
								assert.Contains(t, noneScopeGlobal, replayer.name, "%s: foreign replay accepted", label)
								assert.Empty(t, resp.Agents, "%s: short-circuit lists nothing", label)
								assert.Empty(t, resp.NextCursor, "%s: short-circuit continues nothing", label)
								assert.Zero(t, resp.TotalCount, label)
								assert.Equal(t, string(rawBodyWithoutServerTime(full)), string(rawBodyWithoutServerTime(comp)), label)
							case replayer.name == "hub-admin-non-member" && replayEP.name == "project":
								// Refused at the project agent.list gate before
								// the cursor is read.
								assert.Equal(t, http.StatusForbidden, full.Code, label)
							default:
								assert.Equal(t, http.StatusBadRequest, full.Code, "%s: foreign replay must be refused", label)
							}
						}
					}
				}
			}
		}
	}
}

// pageIDs returns the ids, nextCursor and totalCount of a 200 list page.
func pageIDs(t *testing.T, rec *httptest.ResponseRecorder) ([]string, string, int) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := mustDecodeListAgentsResponse(t, rec.Body)
	return agentIDs(resp), resp.NextCursor, resp.TotalCount
}

// TestAgentCompactView_CursorFromEitherViewReplaysToSameNextPageInBothViews
// pins that view is outside the cursor binding: a cursor issued under
// view=compact and replayed under view=full, and the reverse, returns
// exactly the same next-page membership, order, nextCursor and totalCount
// as replaying it under the issuing view, on both endpoints, in sorted and
// legacy mode.
func TestAgentCompactView_CursorFromEitherViewReplaysToSameNextPageInBothViews(t *testing.T) {
	f := compactSetup(t)
	views := []string{"view=full", "view=compact"}
	for _, c := range []compactCaller{f.caller("owner"), f.caller("member"), f.caller("agent-jwt")} {
		for _, base := range []string{f.globalBase(), f.projectBase()} {
			for _, mode := range []string{"limit=2", "sort=updated&limit=2", "sort=created&dir=asc&limit=2", "sort=updated&limit=2&stats=1"} {
				for _, issuing := range views {
					rec := c.do(t, withQuery(base, mode, issuing))
					if rec.Code != http.StatusOK {
						continue
					}
					cur := mustDecodeListAgentsResponse(t, rec.Body).NextCursor
					if cur == "" {
						continue
					}
					label := fmt.Sprintf("%s %s %q issued under %s", c.name, base, mode, issuing)
					wantIDs, wantNext, wantTotal := pageIDs(t, c.do(t, withQuery(base, withCursor(mode, cur), issuing)))
					require.NotEmpty(t, wantIDs, label)
					for _, replay := range views {
						ids, next, total := pageIDs(t, c.do(t, withQuery(base, withCursor(mode, cur), replay)))
						assert.Equal(t, wantIDs, ids, "%s, replayed under %s: membership and order", label, replay)
						assert.Equal(t, wantNext, next, "%s, replayed under %s: nextCursor", label, replay)
						assert.Equal(t, wantTotal, total, "%s, replayed under %s: totalCount", label, replay)
					}
				}
			}
		}
	}
}

// TestAgentCompactView_CursorRejectsFieldEditsWhenReplayedUnderOtherView
// repeats the cursor integrity cases with the view flipped between minting
// and replay: an edit to sort, dir, scope (global endpoint), project,
// labels, phase, broker, includeDeleted or identity is still a 400 under the other view, with the
// same body as under the issuing view, and an unedited replay under the
// other view is accepted.
func TestAgentCompactView_CursorRejectsFieldEditsWhenReplayedUnderOtherView(t *testing.T) {
	f := compactSetup(t)
	owner, member := f.caller("owner"), f.caller("member")
	otherProjectBase := "/api/v1/projects/" + f.other.ID + "/agents"

	type edit struct {
		name     string
		query    string        // replay query (cursor appended)
		base     string        // replay base; "" means the minting base
		replayer compactCaller // zero means the minter
	}
	edits := func(base, mint string, sorted bool) []edit {
		var out []edit
		if sorted {
			out = append(out,
				edit{name: "sort", query: strings.Replace(mint, "sort=updated", "sort=created", 1)},
				edit{name: "dir", query: mint + "&dir=asc"},
			)
		} else {
			out = append(out, edit{name: "sort", query: mint + "&sort=updated"})
		}
		if base == f.globalBase() {
			// scope is a global endpoint parameter only; the project
			// endpoint has no scope to edit.
			out = append(out, edit{name: "scope", query: mint + "&scope=mine"})
		}
		out = append(out,
			edit{name: "labels", query: mint + "&label=team=a"},
			edit{name: "phase", query: mint + "&phase=running"},
			edit{name: "broker", query: mint + "&runtimeBrokerId=" + tid("cv-broker")},
			edit{name: "includeDeleted", query: mint + "&includeDeleted=true"},
			edit{name: "identity", query: mint, replayer: member},
		)
		if base == f.globalBase() {
			out = append(out, edit{name: "project", query: mint + "&projectId=" + f.project.ID})
		} else {
			out = append(out, edit{name: "project", query: mint, base: otherProjectBase})
		}
		return out
	}

	flips := [][2]string{{"", "view=compact"}, {"view=compact", ""}, {"view=compact", "view=full"}}
	for _, base := range []string{f.globalBase(), f.projectBase()} {
		for _, m := range []struct {
			mint   string
			sorted bool
		}{{"sort=updated&limit=1", true}, {"limit=1", false}} {
			for _, flip := range flips {
				issuing, replayView := flip[0], flip[1]
				cur := cursorOf(t, owner.do(t, withQuery(base, m.mint, issuing)))

				own := owner.do(t, withQuery(base, withCursor(m.mint, cur), replayView))
				assert.Equal(t, http.StatusOK, own.Code, "%s %q %v: unedited replay under other view: %s", base, m.mint, flip, own.Body.String())

				for _, e := range edits(base, m.mint, m.sorted) {
					replayer := owner
					if e.replayer.do != nil {
						replayer = e.replayer
					}
					rb := base
					if e.base != "" {
						rb = e.base
					}
					label := fmt.Sprintf("%s %q minted under %q, %s edit replayed under %q", base, m.mint, issuing, e.name, replayView)
					q := withCursor(e.query, cur)
					flipped := replayer.do(t, withQuery(rb, q, replayView))
					same := replayer.do(t, withQuery(rb, q, issuing))
					assert.Equal(t, http.StatusBadRequest, flipped.Code, "%s: %s", label, flipped.Body.String())
					assert.Equal(t, same.Code, flipped.Code, label)
					assert.Equal(t, string(rawBodyWithoutServerTime(same)), string(rawBodyWithoutServerTime(flipped)), label)
				}
			}
		}
	}
}

// TestAgentCompactView_SortedInvalidViewIs400AtSameCostAsOtherSorted400s
// pins the sorted-mode validation of view: an invalid value is a 400
// "invalid view" in the existing invalid-parameter envelope, made before
// any agent read, at exactly the decision cost of another sorted 400
// (an invalid fit) for the same caller, with the same body for an owner,
// a non-member and an agent JWT wherever the request reaches validation.
func TestAgentCompactView_SortedInvalidViewIs400AtSameCostAsOtherSorted400s(t *testing.T) {
	f := compactSetup(t)
	nonMember := compactUser(t, f.store, "non-member")
	callers := []compactCaller{
		f.caller("owner"),
		{"non-member", func(t *testing.T, path string) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, nonMember, http.MethodGet, path, nil)
		}},
		f.caller("agent-jwt"),
	}
	const wantBody = `{"error":{"code":"invalid_request","message":"invalid view"}}` + "\n"

	realStore := f.store
	for _, base := range []string{f.globalBase(), f.projectBase()} {
		for _, bad := range []string{"bogus", "Compact", "FULL", "full,compact", "compact "} {
			for _, c := range callers {
				label := fmt.Sprintf("%s %s view=%q", c.name, base, bad)
				measure := func(query string) (*httptest.ResponseRecorder, int, []string) {
					spy := &globalCallSpyStore{Store: realStore}
					f.srv.store = spy
					em := &recordingDecisionAuditEmitter{}
					f.srv.authzService.SetDecisionAuditEmitter(em)
					rec := c.do(t, withQuery(base, query))
					f.srv.authzService.SetDecisionAuditEmitter(nil)
					f.srv.store = realStore
					return rec, len(em.records), spy.calls
				}
				viewRec, viewCost, viewCalls := measure("sort=updated&view=" + url.QueryEscape(bad))
				fitRec, fitCost, _ := measure("sort=updated&fit=0")

				assert.Equal(t, fitRec.Code, viewRec.Code, "%s: same status as another sorted 400", label)
				assert.Equal(t, fitCost, viewCost, "%s: same decision count as another sorted 400", label)
				assert.Empty(t, viewCalls, "%s: no agent read before the 400", label)
				if fitRec.Code == http.StatusBadRequest {
					assert.Equal(t, wantBody, viewRec.Body.String(), label)
				} else {
					// The project endpoint refuses a non-member at its
					// agent.list gate before any parameter is parsed, for
					// every sorted parameter alike.
					assert.Equal(t, fitRec.Body.String(), viewRec.Body.String(), label)
				}
			}
		}
	}

	// Valid values are accepted in sorted mode.
	for _, base := range []string{f.globalBase(), f.projectBase()} {
		for _, v := range []string{"view=full", "view=compact", "view="} {
			rec := f.caller("owner").do(t, withQuery(base, "sort=updated", v))
			assert.Equal(t, http.StatusOK, rec.Code, "%s %s: %s", base, v, rec.Body.String())
		}
	}
}

// TestAgentCompactView_SortedViewEmptyIsByteIdenticalToViewAbsent pins that
// an empty view value in sorted mode is the same as no view: same status
// and the same bytes apart from serverTime.
func TestAgentCompactView_SortedViewEmptyIsByteIdenticalToViewAbsent(t *testing.T) {
	f := compactSetup(t)
	for _, c := range []compactCaller{f.caller("owner"), f.caller("agent-jwt")} {
		for _, base := range []string{f.globalBase(), f.projectBase()} {
			for _, mode := range []string{"sort=updated&limit=2", "sort=created&dir=asc&fit=500&stats=1"} {
				a := c.do(t, base+"?"+mode)
				b := c.do(t, base+"?"+mode+"&view=")
				require.Equal(t, http.StatusOK, a.Code, a.Body.String())
				require.Equal(t, a.Code, b.Code, b.Body.String())
				assert.Equal(t, string(rawBodyWithoutServerTime(a)), string(rawBodyWithoutServerTime(b)), "%s %s %q", c.name, base, mode)
			}
		}
	}
}

// TestAgentCompactView_NilAgentsMarshalsLikeFullView pins that a response
// with a nil agents slice writes the same agents value (null) in both views.
func TestAgentCompactView_NilAgentsMarshalsLikeFullView(t *testing.T) {
	for _, agents := range [][]AgentWithCapabilities{nil, {}} {
		resp := ListAgentsResponse{Agents: agents, TotalCount: 0}
		full := httptest.NewRecorder()
		writeAgentList(full, agentListViewFull, resp)
		compact := httptest.NewRecorder()
		writeAgentList(compact, agentListViewCompact, resp)
		require.Equal(t, http.StatusOK, full.Code)
		require.Equal(t, http.StatusOK, compact.Code)
		fullAgents := decodeObject(t, full.Body.Bytes())["agents"]
		compactAgents := decodeObject(t, compact.Body.Bytes())["agents"]
		if agents == nil {
			assert.Equal(t, "null", string(fullAgents))
		} else {
			assert.Equal(t, "[]", string(fullAgents))
		}
		assert.Equal(t, string(fullAgents), string(compactAgents), "nil slice: %v", agents == nil)
	}
}

// TestAgentCompactView_SortedViewFullIsByteIdenticalToViewAbsent pins that
// view=full in sorted mode is exactly today's sorted response.
func TestAgentCompactView_SortedViewFullIsByteIdenticalToViewAbsent(t *testing.T) {
	f := compactSetup(t)
	for _, c := range []compactCaller{f.caller("owner"), f.caller("agent-jwt")} {
		for _, base := range []string{f.globalBase(), f.projectBase()} {
			for _, mode := range []string{"sort=updated&limit=2", "sort=created&dir=asc&fit=500&stats=1"} {
				a := c.do(t, withQuery(base, mode))
				b := c.do(t, withQuery(base, mode, "view=full"))
				require.Equal(t, a.Code, b.Code)
				assert.Equal(t, string(rawBodyWithoutServerTime(a)), string(rawBodyWithoutServerTime(b)), "%s %s %q", c.name, base, mode)
			}
		}
	}
}

// TestAgentCompactView_LegacyIgnoresViewOtherThanCompact pins that without
// sort, view absent, view=full and any invalid view value produce
// byte-identical responses (apart from serverTime), including the emitted
// nextCursor, on both endpoints, with and without a cursor in play.
func TestAgentCompactView_LegacyIgnoresViewOtherThanCompact(t *testing.T) {
	f := compactSetup(t)
	owner := f.caller("owner")
	for _, base := range []string{f.globalBase(), f.projectBase()} {
		page1 := owner.do(t, withQuery(base, "limit=2"))
		cur := cursorOf(t, page1)
		for _, q := range []string{"", "limit=2", withCursor("limit=2", cur), "phase=stopped&label=team=b"} {
			want := owner.do(t, withQuery(base, q))
			require.Equal(t, http.StatusOK, want.Code, want.Body.String())
			for _, v := range []string{"view=full", "view=bogus", "view=", "view=FULL", "view=compact,full"} {
				got := owner.do(t, withQuery(base, q, v))
				assert.Equal(t, want.Code, got.Code)
				assert.Equal(t, string(rawBodyWithoutServerTime(want)), string(rawBodyWithoutServerTime(got)), "%s %q %s", base, q, v)
			}
		}
	}
}

// TestAgentCompactView_PayloadBytesRecord records, without asserting exact
// sizes, the response bytes of the full and compact views at 100 and 500
// agents on both endpoints. Compact must be smaller.
func TestAgentCompactView_PayloadBytesRecord(t *testing.T) {
	for _, n := range []int{100, 500} {
		f := globalSortedSetup(t)
		err := f.store.WithTx(context.Background(), func(tx store.Store) error {
			for i := 0; i < n; i++ {
				slug := fmt.Sprintf("payload-%d", i)
				a := &store.Agent{
					ID: tid("cv-payload-" + slug), Slug: slug, Name: slug, Template: "claude",
					ProjectID: f.project.ID, Phase: "running", Activity: "idle",
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
		for _, ep := range []struct{ name, base string }{
			{"global", f.listPath("")}, {"project", "/api/v1/projects/" + f.project.ID + "/agents"},
		} {
			full := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, withQuery(ep.base, "sort=updated&fit=500"), nil)
			comp := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, withQuery(ep.base, "sort=updated&fit=500", "view=compact"), nil)
			require.Equal(t, http.StatusOK, full.Code, full.Body.String())
			require.Equal(t, http.StatusOK, comp.Code, comp.Body.String())
			require.Len(t, mustDecodeListAgentsResponse(t, comp.Body).Agents, n)
			t.Logf("payload %s n=%d: full=%d bytes compact=%d bytes (%.1f%%)", ep.name, n, full.Body.Len(), comp.Body.Len(),
				100*float64(comp.Body.Len())/float64(full.Body.Len()))
			assert.Less(t, comp.Body.Len(), full.Body.Len(), "%s n=%d", ep.name, n)
		}
	}
}

// TestAgentCompactView_GlobalStatsBytesRecordAt2000And2001 records the
// global stats block's bytes at 2000 and 2001 agents. The omission of
// stats.agents above 2000 is asserted by
// TestListAgentsSorted_StatsOmitsAgentsAbove2000 and its inclusion at 2000
// by TestListAgentsSorted_StatsIncludesAgentsAtExactly2000; here the presence is
// re-checked only to label the record, in both views.
func TestAgentCompactView_GlobalStatsBytesRecordAt2000And2001(t *testing.T) {
	for _, n := range []int{2000, 2001} {
		f := globalSortedSetup(t)
		f.createAgentsBulk(t, n, "stats-bytes", "stopped")
		var statsBytes [2]int
		for i, v := range []string{"", "view=compact"} {
			rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, withQuery(f.listPath(""), "sort=updated&limit=1&stats=1", v), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			stats := decodeObject(t, rec.Body.Bytes())["stats"]
			statsBytes[i] = len(stats)
			_, hasAgents := decodeObject(t, stats)["agents"]
			assert.Equal(t, n <= globalAgentStatsCap, hasAgents, "n=%d: stats.agents presence", n)
		}
		assert.Equal(t, statsBytes[0], statsBytes[1], "n=%d: stats bytes identical across views", n)
		t.Logf("global stats n=%d: %d bytes", n, statsBytes[0])
	}
}

// TestAgentCompactView_ListAgentsResponseHasNoMarshalJSON guards the
// embedding in listAgentsCompactResponse: a MarshalJSON or MarshalText
// method on ListAgentsResponse (or anything it embeds) would be promoted
// onto the compact response and override its compact Agents field, so
// view=compact would silently emit full items.
func TestAgentCompactView_ListAgentsResponseHasNoMarshalJSON(t *testing.T) {
	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	for _, typ := range []reflect.Type{
		reflect.TypeOf(listAgentsCompactResponse{}),
		reflect.TypeOf(&listAgentsCompactResponse{}),
		reflect.TypeOf(ListAgentsResponse{}),
		reflect.TypeOf(&ListAgentsResponse{}),
	} {
		assert.False(t, typ.Implements(jsonMarshaler), "%s must not implement json.Marshaler", typ)
		assert.False(t, typ.Implements(textMarshaler), "%s must not implement encoding.TextMarshaler", typ)
	}

	resp := ListAgentsResponse{
		Agents: []AgentWithCapabilities{{Agent: store.Agent{
			ID: "a", Slug: "s", Name: "n", ProjectID: "p",
			AppliedConfig: &store.AgentAppliedConfig{CreatorName: "c"},
		}}},
		TotalCount: 1,
	}
	b, err := json.Marshal(listAgentsCompactResponse{Agents: []AgentCompactItem{toCompact(resp.Agents[0])}, ListAgentsResponse: resp})
	require.NoError(t, err)
	assertNoKeyAtAnyDepth(t, "compact response", b, "appliedConfig")
	items := decodeItems(t, decodeObject(t, b)["agents"])
	require.Len(t, items, 1)
	assert.Equal(t, `"c"`, string(items[0]["creatorName"]), "compact agents are serialized, not the embedded full agents")
	assert.Equal(t, "1", string(decodeObject(t, b)["totalCount"]))
}

// TestAgentCompactView_MessageabilityCopiedAsIsForAnyValue pins that the
// compact item emits exactly the _messageability bytes the full item emits,
// whatever value the list site stored, including types other than
// *AgentMessageability, a typed nil and no value.
func TestAgentCompactView_MessageabilityCopiedAsIsForAnyValue(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
	}{
		{"absent", nil},
		{"list messageability", &AgentMessageability{}},
		{"detail messageability", &AgentMessageabilityDetail{}},
		{"typed nil", (*AgentMessageability)(nil)},
		{"other value", map[string]bool{"x": true}},
	}
	for _, c := range cases {
		full := AgentWithCapabilities{Agent: store.Agent{ID: "a"}, Messageability: c.v}
		fb, err := json.Marshal(full)
		require.NoError(t, err, c.name)
		cb, err := json.Marshal(toCompact(full))
		require.NoError(t, err, c.name)
		fv, fok := decodeObject(t, fb)["_messageability"]
		cv, cok := decodeObject(t, cb)["_messageability"]
		assert.Equal(t, fok, cok, "%s: _messageability presence", c.name)
		assert.Equal(t, string(fv), string(cv), "%s: _messageability bytes", c.name)
		if c.name != "absent" {
			assert.True(t, cok, "%s: a set value must be emitted", c.name)
		}
	}
}
