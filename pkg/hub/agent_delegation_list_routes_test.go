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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtCatalogWorld extends the list world with skills and templates in
// every scope: project-scoped ones in P1 and P2 (where Alice may list and
// read) and P3 (where she is not a member), a global and a core skill, a
// global template, Alice's own user-scoped skill and template, and a
// legacy template whose deprecated project ID names P1 while its scope ID
// names P3.
type adtCatalogWorld struct {
	*adtListWorld
	skillP1, skillP2, skillP3, skillGlobal, skillCore, skillUser string
	tmplP1, tmplP2, tmplP3, tmplGlobal, tmplUser, tmplLegacy     string
}

func newADTCatalogWorld(t *testing.T, name string) *adtCatalogWorld {
	t.Helper()
	w := &adtCatalogWorld{adtListWorld: newADTListWorld(t, name)}
	w.skillP1 = w.addSkill(t, name+"-s-p1", store.SkillScopeProject, w.proj.ID)
	w.skillP2 = w.addSkill(t, name+"-s-p2", store.SkillScopeProject, w.other.ID)
	w.skillP3 = w.addSkill(t, name+"-s-p3", store.SkillScopeProject, w.p3.ID)
	w.skillGlobal = w.addSkill(t, name+"-s-global", store.SkillScopeGlobal, "")
	w.skillCore = w.addSkill(t, name+"-s-core", store.SkillScopeCore, "")
	w.skillUser = w.addSkill(t, name+"-s-user", store.SkillScopeUser, w.alice.ID)
	w.tmplP1 = w.addTemplate(t, name+"-t-p1", store.TemplateScopeProject, w.proj.ID, "")
	w.tmplP2 = w.addTemplate(t, name+"-t-p2", store.TemplateScopeProject, w.other.ID, "")
	w.tmplP3 = w.addTemplate(t, name+"-t-p3", store.TemplateScopeProject, w.p3.ID, "")
	w.tmplGlobal = w.addTemplate(t, name+"-t-global", store.TemplateScopeGlobal, "", "")
	w.tmplUser = w.addTemplate(t, name+"-t-user", store.TemplateScopeUser, w.alice.ID, "")
	w.tmplLegacy = w.addTemplate(t, name+"-t-legacy", store.TemplateScopeProject, w.p3.ID, w.proj.ID)
	return w
}

func (w *adtCatalogWorld) addSkill(t *testing.T, name, scope, scopeID string) string {
	t.Helper()
	now := time.Now()
	id := tid(name)
	require.NoError(t, w.store.CreateSkill(context.Background(), &store.Skill{
		ID: id, Name: name, Slug: name, Scope: scope, ScopeID: scopeID, Status: "active", OwnerID: w.alice.ID, Created: now, Updated: now,
	}))
	return id
}

func (w *adtCatalogWorld) addTemplate(t *testing.T, name, scope, scopeID, legacyProjectID string) string {
	t.Helper()
	now := time.Now()
	id := tid(name)
	require.NoError(t, w.store.CreateTemplate(context.Background(), &store.Template{
		ID: id, Name: name, Slug: name, Harness: "claude", Scope: scope, ScopeID: scopeID, ProjectID: legacyProjectID,
		Status: store.TemplateStatusActive, OwnerID: w.alice.ID, Created: now, Updated: now,
	}))
	return id
}

// adtItemList is a list response as a client sees it, for any of the
// agents, skills and templates lists.
type adtItemList struct {
	ids                   []string
	caps                  []*Capabilities
	NextCursor            string
	TotalCount            int
	TotalCountApproximate bool
	Cap                   *Capabilities
}

func adtDecodeItems(t *testing.T, rec *httptest.ResponseRecorder, key string) adtItemList {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	var items []struct {
		ID  string        `json:"id"`
		Cap *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(raw[key], &items), rec.Body.String())
	out := adtItemList{}
	for _, it := range items {
		out.ids = append(out.ids, it.ID)
		out.caps = append(out.caps, it.Cap)
	}
	out.ids = sortedIDs(out.ids...)
	var meta struct {
		NextCursor            string        `json:"nextCursor"`
		TotalCount            int           `json:"totalCount"`
		TotalCountApproximate bool          `json:"totalCountApproximate"`
		Cap                   *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	out.NextCursor, out.TotalCount, out.TotalCountApproximate, out.Cap = meta.NextCursor, meta.TotalCount, meta.TotalCountApproximate, meta.Cap
	return out
}

// adtListRoute is one of the delegated list routes under test.
type adtListRoute struct {
	name, path, key string
}

func (w *adtCatalogWorld) routes() []adtListRoute {
	return []adtListRoute{
		{"project agents", "/api/v1/projects/" + w.proj.ID + "/agents", "agents"},
		{"skills", "/api/v1/skills", "skills"},
		{"templates", "/api/v1/templates", "templates"},
	}
}

func (f *adtFixture) listAt(t *testing.T, token, path string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return f.do(t, http.MethodGet, path, nil, adtBearer(token))
}

// adtDelegatedFor issues a grant from issuer to agent and exchanges it,
// returning the grant and the delegated credential.
func (f *adtFixture) adtDelegatedFor(t *testing.T, issuer *store.User, agent *store.Agent, boundary map[string]string, selectors ...string) (AgentDelegationGrantResponse, ExchangeAgentDelegationResponse) {
	t.Helper()
	rec := f.issue(t, f.session(t, issuer), agent.ID, map[string]interface{}{
		"boundary": boundary, "permissions": selectors, "name": "routes",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	rec = f.exchange(t, f.agentJWT(t, agent), agent.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cred ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cred))
	return grant, cred
}

func adtAssertReadOnlyCaps(t *testing.T, l adtItemList) {
	t.Helper()
	for i, c := range l.caps {
		if assert.NotNil(t, c, "item %s carries capabilities", l.ids[i]) {
			assert.Equal(t, []string{string(ActionRead)}, c.Actions, "a listed item carries only the read capability")
		}
	}
}

// TestDelegatedProjectAgentList_ScopeAndBinding: the project form of the
// agent list, for a hub grant with agent:list and agent:read, lists exactly
// the readable agents of that one project: P1 and P2 list their agents, P3
// (Alice is not a member) answers an empty 200. A project-bounded grant
// lists its own project only. The cursor is bound under the landed
// "project-agents:<projectID>" key; sort and ids are refused; and a path
// with a trailing slash is not admitted.
func TestDelegatedProjectAgentList_ScopeAndBinding(t *testing.T) {
	w := newADTListWorld(t, "adt-plist")
	ctx := context.Background()
	grant := w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read")
	cred := w.delegated(t, grant.ID)
	path := func(projectID string) string { return "/api/v1/projects/" + projectID + "/agents" }

	p1 := adtDecodeItems(t, w.listAt(t, cred.Token, path(w.proj.ID), nil), "agents")
	assert.Equal(t, w.p1Agents(), p1.ids)
	assert.Equal(t, len(w.p1Agents()), p1.TotalCount)
	adtAssertReadOnlyCaps(t, p1)
	if assert.NotNil(t, p1.Cap) {
		assert.Empty(t, p1.Cap.Actions, "the list carries no scope capabilities")
	}
	assert.NotContains(t, w.listAt(t, cred.Token, path(w.other.ID), nil).Body.String(), "env-probe-value", "the environment stays redacted")
	assert.Equal(t, w.p2Agents(), adtDecodeItems(t, w.listAt(t, cred.Token, path(w.other.ID), nil), "agents").ids)
	p3 := adtDecodeItems(t, w.listAt(t, cred.Token, path(w.p3.ID), nil), "agents")
	assert.Empty(t, p3.ids, "a project outside the issuer's scope lists nothing")
	assert.Zero(t, p3.TotalCount)

	bounded := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, "agent:list", "agent:read").ID)
	assert.Equal(t, w.p1Agents(), adtDecodeItems(t, w.listAt(t, bounded.Token, path(w.proj.ID), nil), "agents").ids)
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, bounded.Token, path(w.other.ID), nil), "agents").ids, "outside the grant boundary")

	// The landed binding key, bound to this credential.
	page := adtDecodeItems(t, w.listAt(t, cred.Token, path(w.proj.ID), url.Values{"limit": {"2"}}), "agents")
	require.NotEmpty(t, page.NextCursor)
	credRow, err := w.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
	require.NoError(t, err)
	grantRow, err := w.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	identity := newDelegatedAgentIdentity(credRow, grantRow)
	filter, ok := projectAgentListFilter(httptest.NewRecorder(), url.Values{"limit": {"2"}}, w.proj.ID)
	require.True(t, ok)
	_, err = w.srv.listCursorSealer.Open(page.NextCursor, scopedCursorBinding(sortSuffix("project-agents:"+w.proj.ID, "", ""), filter, identity))
	assert.NoError(t, err, "the cursor is bound under the project-agents:<projectID> key")
	_, err = w.srv.listCursorSealer.Open(page.NextCursor, scopedCursorBinding("project_agents:"+w.proj.ID, filter, identity))
	assert.Error(t, err, "and under no other key")

	// The refusal's record names its cause: the issuer's authority for P3, the grant boundary for P2 under a
	// P1-bounded grant.
	decisions := w.captureDecisions(t)
	for _, tc := range []struct {
		token, projectID, code string
	}{
		{cred.Token, w.p3.ID, agentDelegationCodeIssuerAuthority},
		{bounded.Token, w.other.ID, agentDelegationCodeOutsideBoundary},
	} {
		decisions.reset()
		assert.Empty(t, adtDecodeItems(t, w.listAt(t, tc.token, path(tc.projectID), nil), "agents").ids)
		primary := decisions.primaryDelegated()
		require.Len(t, primary, 1)
		assert.Equal(t, "deny", primary[0].Result)
		assert.Equal(t, "agent delegation: "+tc.code, primary[0].Reason)
	}
	// An unknown project answers the existing 404, before the list arm, as for every caller.
	rec := w.listAt(t, cred.Token, path(tid("adt-plist-no-such-project")), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// An unrestricted issuer: All ∩ {P} = {P}, so the admin lists P3's agents through the project form.
	admin := hubAdminUser(t, w.bypassAgentsFixture)
	adminAgent, _ := w.createdAgent(t, w.create(t, authUser(admin), CreateAgentRequest{Name: "adt-plist-admin-x"}), "adt-plist-admin-x")
	_, adminCred := w.adtDelegatedFor(t, admin, adminAgent, map[string]string{"kind": "hub"}, "agent:list", "agent:read")
	assert.Equal(t, []string{w.agentD.ID}, adtDecodeItems(t, w.listAt(t, adminCred.Token, path(w.p3.ID), nil), "agents").ids)

	adtAssertAPIError(t, w.listAt(t, cred.Token, path(w.proj.ID), url.Values{"sort": {"name"}}), http.StatusBadRequest, errCodeDelegatedListSortUnsupported)
	adtAssertAPIError(t, w.listAt(t, cred.Token, path(w.proj.ID), url.Values{"ids": {w.agentA.ID}}), http.StatusBadRequest, errCodeDelegatedListSortUnsupported)
	adtAssertAPIError(t, w.listAt(t, cred.Token, path(w.proj.ID), url.Values{"ids": {w.agentA.ID}, "sort": {"name"}}), http.StatusBadRequest, errCodeDelegatedListSortUnsupported)
	// The delegated refusal runs before the ids= format check; a user caller still gets that check unchanged.
	userRec := w.listAt(t, w.session(t, w.alice), path(w.proj.ID), url.Values{"ids": {w.agentA.ID}})
	assert.Equal(t, http.StatusBadRequest, userRec.Code, userRec.Body.String())
	assert.Contains(t, userRec.Body.String(), "ids requires sort")
	adtAssertAPIError(t, w.do(t, http.MethodGet, path(w.proj.ID)+"/", nil, adtBearer(cred.Token)), http.StatusForbidden, errCodeCredentialNotAdmitted)
}

// TestDelegatedSkillList_ScopeAndItemFilter: a hub grant with skill:list
// and skill:read lists exactly the project-scoped skills of P1 and P2;
// never P3's, the hub catalog's or Alice's own user-scoped skill. A
// project-bounded grant lists its project only. scope and scopeId narrow
// within the scope; anything outside answers an empty 200. Without
// skill:read every item is refused; without skill:list the arm refuses.
func TestDelegatedSkillList_ScopeAndItemFilter(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-slist")
	decisions := w.captureDecisions(t)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "skill:list", "skill:read").ID)

	all := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", nil), "skills")
	assert.Equal(t, sortedIDs(w.skillP1, w.skillP2), all.ids)
	assert.Equal(t, 2, all.TotalCount)
	adtAssertReadOnlyCaps(t, all)

	q := func(k, v string) url.Values { return url.Values{k: {v}} }
	assert.Equal(t, []string{w.skillP1}, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", q("scopeId", w.proj.ID)), "skills").ids)
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", q("scopeId", w.p3.ID)), "skills").ids)
	assert.Equal(t, sortedIDs(w.skillP1, w.skillP2), adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", q("scope", "project")), "skills").ids)
	for _, scope := range []string{"global", "core", "user"} {
		l := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", q("scope", scope)), "skills")
		assert.Empty(t, l.ids, "scope=%s lists nothing", scope)
		assert.Zero(t, l.TotalCount)
	}

	bounded := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, "skill:list", "skill:read").ID)
	assert.Equal(t, []string{w.skillP1}, adtDecodeItems(t, w.listAt(t, bounded.Token, "/api/v1/skills", nil), "skills").ids)

	listOnly := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "skill:list").ID)
	decisions.reset()
	l := adtDecodeItems(t, w.listAt(t, listOnly.Token, "/api/v1/skills", nil), "skills")
	assert.Empty(t, l.ids)
	assert.Zero(t, l.TotalCount)
	agg := decisions.listFilterRecords()
	require.Len(t, agg, 1)
	assert.Positive(t, agg[0].ListFilter.Denied, "without skill:read every item is refused")
	assert.Zero(t, agg[0].ListFilter.Allowed)

	readOnly := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "skill:read").ID)
	decisions.reset()
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, readOnly.Token, "/api/v1/skills", nil), "skills").ids)
	primary := decisions.primaryDelegated()
	require.Len(t, primary, 1)
	assert.Equal(t, "deny", primary[0].Result, "without skill:list the list arm refuses")
	assert.Equal(t, "skill.list", primary[0].PermissionID)

	// No project.list scope is resolved or recorded for a delegated skill list.
	decisions.reset()
	adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", nil), "skills")
	for _, r := range decisions.records() {
		assert.NotEqual(t, "project.list", r.PermissionID, "a delegated skill list writes no project.list record")
	}
}

// TestDelegatedTemplateList_ScopeAndItemFilter: a hub grant with
// template:list and template:read lists exactly the project-scoped
// templates of P1 and P2. The legacy row whose deprecated project ID names
// P1 but whose scope ID names P3 is not listed (the item's project is its
// scope ID). scope=global and scope=user answer an empty 200, never a 401;
// a projectId or scopeId outside the scope, or two that disagree, answer
// an empty 200. Without template:list the arm refuses.
func TestDelegatedTemplateList_ScopeAndItemFilter(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-tlist")
	decisions := w.captureDecisions(t)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "template:list", "template:read").ID)

	all := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", nil), "templates")
	assert.Equal(t, sortedIDs(w.tmplP1, w.tmplP2), all.ids)
	assert.Equal(t, 2, all.TotalCount)
	adtAssertReadOnlyCaps(t, all)
	assert.NotContains(t, all.ids, w.tmplLegacy)

	q := func(kv ...string) url.Values {
		v := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	for _, scope := range []string{"global", "user"} {
		decisions.reset()
		l := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", q("scope", scope)), "templates")
		assert.Empty(t, l.ids, "scope=%s lists nothing", scope)
		assert.Zero(t, l.TotalCount)
		assert.Len(t, decisions.primaryDelegated(), 1, "scope=%s still writes the arm's one record", scope)
	}
	assert.Equal(t, []string{w.tmplP1}, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", q("projectId", w.proj.ID)), "templates").ids)
	assert.Equal(t, []string{w.tmplP2}, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", q("scopeId", w.other.ID)), "templates").ids)
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", q("projectId", w.p3.ID)), "templates").ids)
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", q("projectId", w.proj.ID, "scopeId", w.other.ID)), "templates").ids)

	bounded := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.other.ID}, "template:list", "template:read").ID)
	assert.Equal(t, []string{w.tmplP2}, adtDecodeItems(t, w.listAt(t, bounded.Token, "/api/v1/templates", nil), "templates").ids)

	// Under a P1-bounded grant the legacy row (project ID P1, scope ID P3) is fetched through the project ID
	// term and dropped by admission, and the aggregated record counts it.
	boundedP1 := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, "template:list", "template:read").ID)
	decisions.reset()
	assert.Equal(t, []string{w.tmplP1}, adtDecodeItems(t, w.listAt(t, boundedP1.Token, "/api/v1/templates", nil), "templates").ids)
	agg := decisions.listFilterRecords()
	require.Len(t, agg, 1)
	assert.GreaterOrEqual(t, agg[0].ListFilter.DroppedByAdmission, 1, "the legacy row is dropped by admission and counted")

	readOnly := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "template:read").ID)
	decisions.reset()
	assert.Empty(t, adtDecodeItems(t, w.listAt(t, readOnly.Token, "/api/v1/templates", nil), "templates").ids)
	primary := decisions.primaryDelegated()
	require.Len(t, primary, 1)
	assert.Equal(t, "deny", primary[0].Result)
	assert.Equal(t, "template.list", primary[0].PermissionID)
}

// TestDelegatedCatalogLists_HubCatalogNeverListed: for an unrestricted
// issuer (hub admin, AllProjects mode), the skill and template lists hold
// project-scoped items of every project the issuer may read, and never a
// hub-catalog (global or core) or user-scoped item. catalogListReadBatch
// denies every row for a delegated identity.
func TestDelegatedCatalogLists_HubCatalogNeverListed(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-catlist")
	admin := hubAdminUser(t, w.bypassAgentsFixture)
	adminAgent, _ := w.createdAgent(t, w.create(t, authUser(admin), CreateAgentRequest{Name: "adt-catlist-x"}), "adt-catlist-x")
	_, cred := w.adtDelegatedFor(t, admin, adminAgent, map[string]string{"kind": "hub"}, "skill:list", "skill:read", "template:list", "template:read")

	skills := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/skills", nil), "skills")
	for _, id := range []string{w.skillP1, w.skillP2, w.skillP3} {
		assert.Contains(t, skills.ids, id, "an unrestricted issuer lists project skills of every project")
	}
	for _, id := range []string{w.skillGlobal, w.skillCore, w.skillUser} {
		assert.NotContains(t, skills.ids, id, "hub-catalog and user-scoped skills are never listed")
	}
	assert.Equal(t, len(skills.ids), skills.TotalCount)

	tmpls := adtDecodeItems(t, w.listAt(t, cred.Token, "/api/v1/templates", nil), "templates")
	for _, id := range []string{w.tmplP1, w.tmplP2, w.tmplP3, w.tmplLegacy} {
		assert.Contains(t, tmpls.ids, id, "an unrestricted issuer lists project templates of every project")
	}
	for _, id := range []string{w.tmplGlobal, w.tmplUser} {
		assert.NotContains(t, tmpls.ids, id, "hub-catalog and user-scoped templates are never listed")
	}
	assert.Equal(t, len(tmpls.ids), tmpls.TotalCount)

	d := adtTestIdentity("agent-x", "project-x")
	allowed, err := w.srv.catalogListReadBatch(d)(context.Background(), d, []Resource{
		{Type: "template", ID: "t-1", ScopeKind: store.TemplateScopeGlobal},
		{Type: "template", ID: "t-2", ScopeKind: store.TemplateScopeProject, ParentType: "project", ParentID: "p-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, false}, allowed, "the catalog batch denies every row for a delegated identity")
}

// TestDelegatedCatalogLists_HubMemberIssuerAndExcludedProject: an issuer scope of None cannot be reached on the skill
// and template lists for a hub-member issuer. The system-scoped hub-member role carries skill.list and template.list
// (seed.go hubMemberPermissionIDs, bound to the hub-members group), and list scopes come from role bindings alone, so
// the issuer's scope for them is every project. A P2-bounded grant whose issuer then loses P2 is therefore allowed by
// the list arm (scope {P2}). The issuer still holds skill.read and template.read everywhere through the same role;
// what refuses P2's items is project admission, because the issuer no longer holds P2. The result is an empty 200
// with one allow record, and an aggregated record that allows nothing and refuses at least one item. For an
// unrestricted issuer, an access constraint that excludes P2 removes P2's skill and template (AllProjects mode).
func TestDelegatedCatalogLists_HubMemberIssuerAndExcludedProject(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-catnone")
	ctx := context.Background()
	decisions := w.captureDecisions(t)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.other.ID},
		"skill:list", "skill:read", "template:list", "template:read").ID)
	require.NoError(t, w.store.DeleteRoleBinding(ctx, w.aliceP2Binding))
	for path, key := range map[string]string{"/api/v1/skills": "skills", "/api/v1/templates": "templates"} {
		decisions.reset()
		l := adtDecodeItems(t, w.listAt(t, cred.Token, path, nil), key)
		assert.Empty(t, l.ids, path)
		assert.Zero(t, l.TotalCount, path)
		primary := decisions.primaryDelegated()
		require.Len(t, primary, 1, path)
		assert.Equal(t, "allow", primary[0].Result, "%s: the hub-member role keeps the issuer's list scope on P2", path)
		agg := decisions.listFilterRecords()
		require.Len(t, agg, 1, path)
		assert.Zero(t, agg[0].ListFilter.Allowed, path)
		assert.Positive(t, agg[0].ListFilter.Denied+agg[0].ListFilter.DroppedByAdmission, "%s: P2's items are refused per item", path)
	}

	admin := hubAdminUser(t, w.bypassAgentsFixture)
	adminAgent, _ := w.createdAgent(t, w.create(t, authUser(admin), CreateAgentRequest{Name: "adt-catnone-x"}), "adt-catnone-x")
	_, adminCred := w.adtDelegatedFor(t, admin, adminAgent, map[string]string{"kind": "hub"}, "skill:list", "skill:read", "template:list", "template:read")
	pType := "user"
	_, err := w.store.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "adt-catnone-block-p2",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(admin.ID),
		ScopeType:            "project",
		ScopeID:              w.other.ID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test constraint exclusion",
		CreatedBy:            admin.ID,
	})
	require.NoError(t, err)
	skills := adtDecodeItems(t, w.listAt(t, adminCred.Token, "/api/v1/skills", nil), "skills")
	assert.Contains(t, skills.ids, w.skillP1)
	assert.NotContains(t, skills.ids, w.skillP2, "a project the constraint excludes does not appear")
	assert.Equal(t, len(skills.ids), skills.TotalCount)
	tmpls := adtDecodeItems(t, w.listAt(t, adminCred.Token, "/api/v1/templates", nil), "templates")
	assert.Contains(t, tmpls.ids, w.tmplP1)
	assert.NotContains(t, tmpls.ids, w.tmplP2, "a project the constraint excludes does not appear")
	assert.Equal(t, len(tmpls.ids), tmpls.TotalCount)
}

// TestDelegatedListRoutes_CursorBoundToCredentialGrantAgentAndBoundary:
// on each route a walk with a small limit returns exactly the full list,
// and a cursor issued to one credential is refused (400 invalid_cursor) for
// another grant with the same scope, another agent of the same issuer, a
// project-bounded grant, and a credential exchanged again from the same
// grant.
func TestDelegatedListRoutes_CursorBoundToCredentialGrantAgentAndBoundary(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-rcursor")
	for i := 0; i < 3; i++ {
		w.addSkill(t, fmt.Sprintf("adt-rcursor-s-more-%d", i), store.SkillScopeProject, w.proj.ID)
		w.addTemplate(t, fmt.Sprintf("adt-rcursor-t-more-%d", i), store.TemplateScopeProject, w.proj.ID, "")
	}
	selectors := []string{"agent:list", "agent:read", "skill:list", "skill:read", "template:list", "template:read"}
	hub := map[string]string{"kind": "hub"}
	grant := w.adtIssueGrant(t, hub, selectors...)
	cred := w.delegated(t, grant.ID)
	otherGrant := w.delegated(t, w.adtIssueGrant(t, hub, selectors...).ID)
	secondAgent, _ := w.createdAgent(t, w.create(t, authUser(w.alice), CreateAgentRequest{Name: "adt-rcursor-a2"}), "adt-rcursor-a2")
	_, otherAgent := w.adtDelegatedFor(t, w.alice, secondAgent, hub, selectors...)
	bounded := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, selectors...).ID)
	again := w.delegated(t, grant.ID)

	for _, rt := range w.routes() {
		t.Run(rt.name, func(t *testing.T) {
			full := adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, nil), rt.key)
			var walked []string
			cursor := ""
			for pages := 1; ; pages++ {
				require.LessOrEqual(t, pages, 50)
				q := url.Values{"limit": {"2"}}
				if cursor != "" {
					q.Set("cursor", cursor)
				}
				page := adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, q), rt.key)
				assert.Equal(t, full.TotalCount, page.TotalCount, "the total is the same on every page")
				walked = append(walked, page.ids...)
				if page.NextCursor == "" {
					assert.Greater(t, pages, 1)
					break
				}
				adtAssertCursorHidesIDs(t, page.NextCursor, page.ids...)
				cursor = page.NextCursor
			}
			assert.Equal(t, full.ids, sortedIDs(walked...), "pages hold exactly the allowed items")

			first := adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, url.Values{"limit": {"2"}}), rt.key)
			require.NotEmpty(t, first.NextCursor)
			// A hand-built plaintext cursor is refused.
			plain := authorizedListCursor(time.Now(), "adt-rcursor-plain", "binding")
			adtAssertAPIError(t, w.listAt(t, cred.Token, rt.path, url.Values{"cursor": {plain}}), http.StatusBadRequest, ErrCodeInvalidCursor)
			for name, other := range map[string]string{"another grant": otherGrant.Token, "another agent": otherAgent.Token, "a project-bounded grant": bounded.Token, "a re-exchanged credential": again.Token} {
				t.Run(name, func(t *testing.T) {
					adtAssertAPIError(t, w.listAt(t, other, rt.path, url.Values{"limit": {"2"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)
				})
			}
		})
	}
}

// TestDelegatedListRoutes_NilSealerFailsClosed: on each route, without a
// cursor sealer a page that needs a next cursor fails closed (500, no
// cursor), a page that needs none still works, and a presented cursor is
// invalid.
func TestDelegatedListRoutes_NilSealerFailsClosed(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-rnosealer")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"},
		"agent:list", "agent:read", "skill:list", "skill:read", "template:list", "template:read").ID)
	for _, rt := range w.routes() {
		t.Run(rt.name, func(t *testing.T) {
			first := adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, url.Values{"limit": {"1"}}), rt.key)
			require.NotEmpty(t, first.NextCursor)
			full := adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, nil), rt.key)

			saved := w.srv.listCursorSealer
			w.srv.listCursorSealer = nil
			defer func() { w.srv.listCursorSealer = saved }()

			rec := w.listAt(t, cred.Token, rt.path, url.Values{"limit": {"1"}})
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "nextCursor")
			assert.Equal(t, full.TotalCount, adtDecodeItems(t, w.listAt(t, cred.Token, rt.path, nil), rt.key).TotalCount, "a page with no next cursor needs no sealer")
			adtAssertAPIError(t, w.listAt(t, cred.Token, rt.path, url.Values{"limit": {"1"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)
		})
	}
}

// TestDelegatedListRoutes_OneRecordEachAndNoReason: each new list route
// writes one primary record (the list arm's allow, for the route's list
// permission) and one aggregated item-filter record, with allow sampling
// at 0, and no response carries the decision's reason.
func TestDelegatedListRoutes_OneRecordEachAndNoReason(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-rrecords")
	decisions := w.captureDecisions(t)
	w.srv.authzService.DecisionAuditSampleRate = 0
	t.Cleanup(func() { w.srv.authzService.DecisionAuditSampleRate = 1.0 })
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"},
		"agent:list", "agent:read", "skill:list", "skill:read", "template:list", "template:read").ID)
	perms := map[string]string{"project agents": "agent.list", "skills": "skill.list", "templates": "template.list"}
	for _, rt := range w.routes() {
		t.Run(rt.name, func(t *testing.T) {
			decisions.reset()
			rec := w.listAt(t, cred.Token, rt.path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "agent delegation")
			primary := decisions.primaryDelegated()
			require.Len(t, primary, 1)
			assert.Equal(t, "allow", primary[0].Result)
			assert.Equal(t, perms[rt.name], primary[0].PermissionID)
			require.Len(t, decisions.listFilterRecords(), 1)
		})
	}
}

// TestDelegatedItemFilterKeys_RefusedOnSingleReads: skill.read and
// template.read are item-filter-only keys. With a grant holding them, a
// single skill or template read, a download and a skill resolve are refused
// at the route gate (no admission entry).
func TestDelegatedItemFilterKeys_RefusedOnSingleReads(t *testing.T) {
	w := newADTCatalogWorld(t, "adt-rsingle")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "skill:list", "skill:read", "template:list", "template:read").ID)
	for _, path := range []string{
		"/api/v1/skills/" + w.skillP1,
		"/api/v1/skills/" + w.skillP1 + "/download",
		"/api/v1/skills/" + w.skillP1 + "/versions",
		"/api/v1/templates/" + w.tmplP1,
		"/api/v1/templates/" + w.tmplP1 + "/download",
		"/api/v1/templates/" + w.tmplP1 + "/files",
		"/api/v1/templates/" + w.tmplP1 + "/validate",
		"/api/v1/skills/" + w.skillP1 + "/resolve",
		"/api/v1/skills/" + w.skillP1 + "/files/x",
		"/api/v1/users/me/templates",
	} {
		adtAssertAPIError(t, w.do(t, http.MethodGet, path, nil, adtBearer(cred.Token)), http.StatusForbidden, errCodeCredentialNotAdmitted)
	}
	adtAssertAPIError(t, w.do(t, http.MethodPost, "/api/v1/skills/resolve", map[string]interface{}{"ref": "x"}, adtBearer(cred.Token)), http.StatusForbidden, errCodeCredentialNotAdmitted)
	// The list routes themselves accept only their exact path.
	for _, path := range []string{"/api/v1/skills/", "/api/v1/templates/"} {
		adtAssertAPIError(t, w.do(t, http.MethodGet, path, nil, adtBearer(cred.Token)), http.StatusForbidden, errCodeCredentialNotAdmitted)
	}
}

// TestDelegatedListRoutes_ResolveOnlyTheExactPaths: the project agent,
// skill and template list resolutions accept only their exact paths, and
// only GET has an operation.
func TestDelegatedListRoutes_ResolveOnlyTheExactPaths(t *testing.T) {
	route, sub, ok := resolveProjectAgentsCollectionRoute(http.MethodGet, "/api/v1/projects/p-1/agents")
	require.True(t, ok)
	require.NotNil(t, sub)
	assert.Equal(t, ProjectAgentRouteCollection, sub.RouteID)
	assert.Equal(t, projectAgentsCollectionRouteID, route.RouteID)
	assert.Equal(t, opAgentList, route.OperationID)
	assert.Equal(t, projectAgentCollectionTarget("p-1"), route.Target)
	route, _, ok = resolveProjectAgentsCollectionRoute(http.MethodGet, "/api/v1/projects/p-1__my-project/agents")
	require.True(t, ok)
	assert.Equal(t, projectAgentCollectionTarget("p-1"), route.Target, "the {uuid}__{slug} form resolves to the ID")
	route, _, ok = resolveProjectAgentsCollectionRoute(http.MethodPost, "/api/v1/projects/p-1/agents")
	require.True(t, ok)
	assert.Empty(t, route.OperationID, "only GET has an operation")
	for _, p := range []string{"/api/v1/projects/p-1/agents/", "/api/v1/projects/p-1/agents/a-1", "/api/v1/projects/p-1/agents/stop-all", "/api/v1/projects/p-1"} {
		_, _, ok := resolveProjectAgentsCollectionRoute(http.MethodGet, p)
		assert.False(t, ok, p)
	}

	for _, tc := range []struct {
		meta, path string
		routeID    delegatedRouteID
		target     delegatedRouteTarget
	}{
		{"skills.list", "/api/v1/skills", skillsCollectionRouteID, hubSkillCollectionTarget},
		{"templates.list", "/api/v1/templates", templatesCollectionRouteID, hubTemplateCollectionTarget},
	} {
		route, _, ok := resolveDelegatedRoute(RouteMetadata{RouteID: tc.meta}, http.MethodGet, tc.path)
		require.True(t, ok, tc.path)
		assert.Equal(t, tc.routeID, route.RouteID)
		assert.Equal(t, tc.target, route.Target)
		assert.NotEmpty(t, route.OperationID)
		for _, p := range []string{tc.path + "/", tc.path + "/x"} {
			_, _, ok := resolveDelegatedRoute(RouteMetadata{RouteID: tc.meta}, http.MethodGet, p)
			assert.False(t, ok, p)
		}
		route, _, ok = resolveDelegatedRoute(RouteMetadata{RouteID: tc.meta}, http.MethodPost, tc.path)
		require.True(t, ok)
		assert.Empty(t, route.OperationID, "only GET has an operation")
	}
}

// TestAgentSkillAccessScope_NeverSetsAllProjects: the agent-principal skill
// scope (agentSkillAccessScope) never sets SkillAccessScope.AllProjects;
// only the delegated list handler does.
func TestAgentSkillAccessScope_NeverSetsAllProjects(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "skill_handlers.go", nil, 0)
	require.NoError(t, err)
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "agentSkillAccessScope" {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok {
				assert.NotEqual(t, "AllProjects", id.Name, "agentSkillAccessScope must not set AllProjects")
			}
			return true
		})
		return false
	})
	require.True(t, found, "agentSkillAccessScope is in skill_handlers.go")
}
