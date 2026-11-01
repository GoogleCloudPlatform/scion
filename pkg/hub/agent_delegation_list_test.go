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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtListWorld extends the delegation world with P3, a project where Alice
// is not a member, holding agent D (owned by Bob). P1 holds the base
// fixture's caller, sibling and child agents and agents A and C; P2 holds
// the stranger agent and agent B.
type adtListWorld struct {
	*adtFixture
	p3     *store.Project
	agentD *store.Agent
}

func newADTListWorld(t *testing.T, name string) *adtListWorld {
	t.Helper()
	ctx := context.Background()
	f := newADTFixture(t, name)
	w := &adtListWorld{adtFixture: f}
	w.p3 = &store.Project{ID: tid(name + "-p3"), Name: name + " P3", Slug: name + "-p3", OwnerID: f.bob.ID}
	require.NoError(t, f.store.CreateProject(ctx, w.p3))
	adtGrantRole(t, f.store, f.bob.ID, w.p3.ID, store.ProjectRoleMember)
	w.agentD = &store.Agent{
		ID: tid(name + "-agent-d"), Name: name + "-d", Slug: name + "-d", ProjectID: w.p3.ID,
		OwnerID: f.bob.ID, Ancestry: []string{f.bob.ID},
	}
	require.NoError(t, f.store.CreateAgent(ctx, w.agentD))
	return w
}

// p1Agents is the sorted IDs of every agent in P1.
func (w *adtListWorld) p1Agents() []string {
	return sortedIDs(w.caller.ID, w.sibling.ID, w.child.ID, w.agentA.ID, w.agentC.ID)
}

// p2Agents is the sorted IDs of every agent in P2.
func (w *adtListWorld) p2Agents() []string {
	return sortedIDs(w.stranger.ID, w.agentB.ID)
}

// aliceAgents is the sorted IDs of every agent Alice may list and read:
// those of P1 and P2.
func (w *adtListWorld) aliceAgents() []string {
	return sortedIDs(append(w.p1Agents(), w.p2Agents()...)...)
}

// adtListResponse is the hub agent list response as a client sees it.
type adtListResponse struct {
	Agents []struct {
		ID  string        `json:"id"`
		Cap *Capabilities `json:"_capabilities"`
	} `json:"agents"`
	NextCursor            string        `json:"nextCursor"`
	TotalCount            int           `json:"totalCount"`
	TotalCountApproximate bool          `json:"totalCountApproximate"`
	Cap                   *Capabilities `json:"_capabilities"`
}

func (f *adtFixture) listAgents(t *testing.T, token string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/agents"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return f.do(t, http.MethodGet, path, nil, adtBearer(token))
}

func adtDecodeList(t *testing.T, rec *httptest.ResponseRecorder) adtListResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out adtListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (l adtListResponse) ids() []string {
	out := make([]string, 0, len(l.Agents))
	for _, a := range l.Agents {
		out = append(out, a.ID)
	}
	sort.Strings(out)
	return out
}

// listFilterRecords returns the aggregated item-filter records captured.
func (c *adtDecisionCapture) listFilterRecords() []*store.DecisionAuditRecord {
	var out []*store.DecisionAuditRecord
	for _, r := range c.records() {
		if r.RecordKind == store.DecisionAuditRecordKindListFilter {
			out = append(out, r)
		}
	}
	return out
}

// primaryDelegated returns the captured agent delegation decision records
// that are not aggregated records.
func (c *adtDecisionCapture) primaryDelegated() []*store.DecisionAuditRecord {
	var out []*store.DecisionAuditRecord
	for _, r := range c.delegated() {
		if r.RecordKind == "" {
			out = append(out, r)
		}
	}
	return out
}

// TestAgentDelegatedAgentList_ExplicitScopeAcrossProjects: with a hub grant
// for agent:list and agent:read, agent A lists exactly the agents of the
// projects where Alice may list and read agents (P1 and P2), across both
// projects, and none of P3. Totals count only returned items, each item
// carries only the read capability, the environment stays redacted, and
// the list has no scope capabilities. A project-bounded grant lists only
// its project. The agent's own authority adds nothing.
func TestAgentDelegatedAgentList_ExplicitScopeAcrossProjects(t *testing.T) {
	w := newADTListWorld(t, "adt-list")
	hub := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)

	rec := w.listAgents(t, hub.Token, nil)
	list := adtDecodeList(t, rec)
	assert.Equal(t, w.aliceAgents(), list.ids())
	assert.Equal(t, len(w.aliceAgents()), list.TotalCount)
	assert.False(t, list.TotalCountApproximate)
	assert.Empty(t, list.NextCursor)
	for _, a := range list.Agents {
		require.NotNil(t, a.Cap, a.ID)
		assert.Equal(t, []string{string(ActionRead)}, a.Cap.Actions, "item %s carries only the read capability", a.ID)
	}
	require.NotNil(t, list.Cap)
	assert.Empty(t, list.Cap.Actions, "no scope capabilities")
	assert.NotContains(t, rec.Body.String(), "env-probe-value", "the environment stays redacted")
	assert.NotContains(t, rec.Body.String(), w.agentD.ID)

	project := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, "agent:list", "agent:read").ID)
	list = adtDecodeList(t, w.listAgents(t, project.Token, nil))
	assert.Equal(t, w.p1Agents(), list.ids(), "a project-bounded grant lists only its project")
	assert.Equal(t, len(w.p1Agents()), list.TotalCount)

	// A projectId filter outside the scope matches nothing, by ID or slug.
	list = adtDecodeList(t, w.listAgents(t, hub.Token, url.Values{"projectId": {w.p3.ID}}))
	assert.Empty(t, list.ids())
	list = adtDecodeList(t, w.listAgents(t, hub.Token, url.Values{"projectId": {w.p3.Slug}}))
	assert.Empty(t, list.ids())
	list = adtDecodeList(t, w.listAgents(t, hub.Token, url.Values{"projectId": {w.other.Slug}}))
	assert.Equal(t, w.p2Agents(), list.ids(), "a slug inside the scope resolves")
}

// TestAgentDelegatedAgentList_HubAdminAllProjectsAndConstraint: a hub-admin
// issuer with a hub grant lists agents across every project, except a
// project an access constraint removes from the admin's agent.list scope.
func TestAgentDelegatedAgentList_HubAdminAllProjectsAndConstraint(t *testing.T) {
	w := newADTListWorld(t, "adt-list-admin")
	ctx := context.Background()
	admin := hubAdminUser(t, w.bypassAgentsFixture)
	adminAgent, _ := w.createdAgent(t, w.create(t, authUser(admin), CreateAgentRequest{Name: "adt-list-admin-x"}), "adt-list-admin-x")

	rec := w.issue(t, w.session(t, admin), adminAgent.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:list", "agent:read"}, "name": "admin-list",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	rec = w.exchange(t, w.agentJWT(t, adminAgent), adminAgent.ID, grant.ID, map[string]interface{}{"audience": w.srv.agentDelegationAudience()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cred ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cred))

	list := adtDecodeList(t, w.listAgents(t, cred.Token, nil))
	for _, id := range []string{w.agentA.ID, w.agentB.ID, w.agentC.ID, w.agentD.ID, adminAgent.ID} {
		assert.Contains(t, list.ids(), id, "an unrestricted issuer lists agents of every project")
	}
	assert.Equal(t, len(list.Agents), list.TotalCount)

	pType := "user"
	_, err := w.store.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "adt-list-block-p3",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(admin.ID),
		ScopeType:            "project",
		ScopeID:              w.p3.ID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test constraint exclusion",
		CreatedBy:            admin.ID,
	})
	require.NoError(t, err)
	list = adtDecodeList(t, w.listAgents(t, cred.Token, nil))
	assert.NotContains(t, list.ids(), w.agentD.ID, "a project the constraint excludes does not appear")
	assert.Contains(t, list.ids(), w.agentB.ID)
	assert.Equal(t, len(list.Agents), list.TotalCount)
}

// TestAgentDelegatedAgentList_MixedItemsAndCounts: Alice may list agents
// in P3 (a custom role with agent.list only) but not read them. P3's agent
// is decided by the item filter and denied: it is not returned, not
// counted, and its ID appears nowhere in the response or the cursor. The
// aggregated record counts it and names it.
func TestAgentDelegatedAgentList_MixedItemsAndCounts(t *testing.T) {
	w := newADTListWorld(t, "adt-list-mixed")
	rd := createCustomRoleDef(t, w.store, "adt-list-only", []string{"agent.list"})
	createCustomBinding(t, w.store, rd.ID, w.alice.ID, w.p3.ID)
	decisions := w.captureDecisions(t)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)

	decisions.reset()
	rec := w.listAgents(t, cred.Token, url.Values{"limit": {"2"}})
	list := adtDecodeList(t, rec)
	allowed := w.aliceAgents()
	assert.Len(t, list.Agents, 2)
	assert.Equal(t, len(allowed), list.TotalCount, "the total counts only items the filter allows")
	require.NotEmpty(t, list.NextCursor)
	assert.NotContains(t, rec.Body.String(), w.agentD.ID)
	adtAssertCursorHidesIDs(t, list.NextCursor, append(allowed, w.agentD.ID)...)

	recs := decisions.listFilterRecords()
	require.Len(t, recs, 1, "one aggregated record per list request")
	lf := recs[0].ListFilter
	require.NotNil(t, lf)
	assert.Equal(t, "agent.read", lf.ItemPermission)
	assert.Equal(t, len(allowed), lf.Allowed)
	assert.Equal(t, 1, lf.Denied+lf.DroppedByAdmission)
	assert.Equal(t, []string{w.agentD.ID}, lf.DeniedIDs)
	assert.False(t, lf.Truncated)
	assert.False(t, lf.Incomplete)
	assert.Empty(t, recs[0].DeniedBy)

	// Following the cursors walks exactly the allowed items.
	walked, _ := w.walkAgents(t, cred.Token, 2)
	assert.Equal(t, allowed, sortedIDs(walked...), "no duplicates or gaps, and no denied item")

	// The issuer keeps agent.list on P2 but loses agent.read there (an access constraint that allows only
	// agent.list): P2 stays in the list scope, and the item filter drops each P2 agent.
	pType := "user"
	_, err := w.store.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:                 "adt-list-mixed-no-read-p2",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(w.alice.ID),
		ScopeType:            "project",
		ScopeID:              w.other.ID,
		MaximumPermissions:   []string{"agent.list"},
		Purpose:              "the issuer may list but not read agents in P2",
		CreatedBy:            w.alice.ID,
	})
	require.NoError(t, err)
	decisions.reset()
	list = adtDecodeList(t, w.listAgents(t, cred.Token, nil))
	assert.Equal(t, w.p1Agents(), list.ids(), "P2's agents drop out")
	assert.Equal(t, len(w.p1Agents()), list.TotalCount)
	recs = decisions.listFilterRecords()
	require.Len(t, recs, 1)
	assert.Equal(t, len(w.p1Agents()), recs[0].ListFilter.Allowed)
	assert.Equal(t, 1+len(w.p2Agents()), recs[0].ListFilter.Denied+recs[0].ListFilter.DroppedByAdmission,
		"agent D and every P2 agent are decided and refused by the item filter")
	for _, id := range w.p2Agents() {
		assert.Contains(t, recs[0].ListFilter.DeniedIDs, id)
	}
	walked, _ = w.walkAgents(t, cred.Token, 2)
	assert.Equal(t, w.p1Agents(), sortedIDs(walked...), "the walk has no gap and no refused agent")
}

// walkAgents follows the delegated hub agent list's cursors with page size
// limit and returns every listed ID in order and the number of pages. Every
// page reports the same total.
func (f *adtFixture) walkAgents(t *testing.T, token string, limit int) ([]string, int) {
	t.Helper()
	var ids []string
	cursor, total := "", -1
	for pages := 1; pages <= 50; pages++ {
		q := url.Values{"limit": {fmt.Sprint(limit)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		page := adtDecodeList(t, f.listAgents(t, token, q))
		if total >= 0 {
			assert.Equal(t, total, page.TotalCount, "the total is the same on every page")
		}
		total = page.TotalCount
		for _, a := range page.Agents {
			ids = append(ids, a.ID)
		}
		if page.NextCursor == "" {
			return ids, pages
		}
		cursor = page.NextCursor
	}
	t.Fatal("the list did not end within 50 pages")
	return nil, 0
}

// adtAssertCursorHidesIDs asserts that neither cursor nor any base64url
// decoding of it contains one of ids.
func adtAssertCursorHidesIDs(t *testing.T, cursor string, ids ...string) {
	t.Helper()
	views := []string{cursor}
	body := cursor
	if i := strings.LastIndex(cursor, "."); i >= 0 {
		body = cursor[i+1:]
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		if raw, err := enc.DecodeString(body); err == nil {
			views = append(views, string(raw))
		}
		if raw, err := enc.DecodeString(cursor); err == nil {
			views = append(views, string(raw))
		}
	}
	for _, v := range views {
		for _, id := range ids {
			assert.NotContains(t, v, id, "the cursor reveals no item ID")
		}
	}
}

// TestAgentDelegatedAgentList_ListWithoutReadAndReadWithoutList: a grant
// with agent:list but not agent:read lists nothing (every item is denied by
// the item filter); a grant with agent:read but not agent:list is refused
// by the list arm and lists nothing.
func TestAgentDelegatedAgentList_ListWithoutReadAndReadWithoutList(t *testing.T) {
	w := newADTListWorld(t, "adt-list-noread")
	decisions := w.captureDecisions(t)

	listOnly := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list").ID)
	decisions.reset()
	list := adtDecodeList(t, w.listAgents(t, listOnly.Token, nil))
	assert.Empty(t, list.Agents)
	assert.Zero(t, list.TotalCount)
	assert.Empty(t, list.NextCursor)
	recs := decisions.listFilterRecords()
	require.Len(t, recs, 1)
	assert.Zero(t, recs[0].ListFilter.Allowed)
	assert.Equal(t, len(w.aliceAgents()), recs[0].ListFilter.Denied)
	primary := decisions.primaryDelegated()
	require.Len(t, primary, 1)
	assert.Equal(t, "allow", primary[0].Result, "the list arm allows; the items are denied")

	readOnly := w.delegated(t, w.hubGrant(t).ID)
	decisions.reset()
	list = adtDecodeList(t, w.listAgents(t, readOnly.Token, nil))
	assert.Empty(t, list.Agents)
	assert.Zero(t, list.TotalCount)
	primary = decisions.primaryDelegated()
	require.Len(t, primary, 1)
	assert.Equal(t, "deny", primary[0].Result)
	assert.Equal(t, string(DeniedByAgentDelegation), primary[0].DeniedBy)
	assert.Equal(t, "agent delegation: "+agentDelegationCodeOutsideCeiling, primary[0].Reason)
	recs = decisions.listFilterRecords()
	require.Len(t, recs, 1, "the arm ran, so the request still writes its aggregated record")
	assert.Zero(t, recs[0].ListFilter.Allowed+recs[0].ListFilter.Denied+recs[0].ListFilter.DroppedByAdmission)
}

// TestAgentDelegatedAgentList_OneRecordEachAndNoReason: with allow sampling
// at 0, a delegated list writes exactly one primary decision record (the
// list arm) and one aggregated item-filter record, and nothing per item;
// the response carries no decision reason.
func TestAgentDelegatedAgentList_OneRecordEachAndNoReason(t *testing.T) {
	w := newADTListWorld(t, "adt-list-records")
	decisions := w.captureDecisions(t)
	w.srv.authzService.DecisionAuditSampleRate = 0
	t.Cleanup(func() { w.srv.authzService.DecisionAuditSampleRate = 1.0 })
	grant := w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read")
	cred := w.delegated(t, grant.ID)

	decisions.reset()
	rec := w.listAgents(t, cred.Token, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "agent delegation")

	all := decisions.records()
	require.Len(t, all, 2, "one primary and one aggregated record, nothing per item")
	primary := decisions.primaryDelegated()
	require.Len(t, primary, 1)
	assert.Equal(t, "allow", primary[0].Result)
	assert.Equal(t, "agent.list", primary[0].PermissionID)
	assert.Equal(t, "agent_delegation:"+grant.ID, primary[0].MatchedGrant)
	assert.Equal(t, w.agentA.ID, primary[0].PrincipalID)
	agg := decisions.listFilterRecords()
	require.Len(t, agg, 1)
	assert.Equal(t, "allow", agg[0].Result)
	assert.Empty(t, agg[0].DeniedBy)
	assert.Equal(t, len(w.aliceAgents()), agg[0].ListFilter.Allowed)
	assert.Equal(t, w.agentA.ID, agg[0].PrincipalID)
}

// TestAgentDelegatedAgentList_SortAndIDsRejected: a delegated hub agent list
// serves the default order only. sort= and ids= each answer 400 with one
// stable code, before the list runs, and write no records.
func TestAgentDelegatedAgentList_SortAndIDsRejected(t *testing.T) {
	w := newADTListWorld(t, "adt-list-sort")
	decisions := w.captureDecisions(t)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)

	for _, q := range []url.Values{
		{"sort": {"created"}},
		{"sort": {"updated"}, "dir": {"asc"}},
		{"ids": {w.agentB.ID}},
		{"sort": {"created"}, "ids": {w.agentB.ID}},
	} {
		decisions.reset()
		adtAssertAPIError(t, w.listAgents(t, cred.Token, q), http.StatusBadRequest, errCodeDelegatedListSortUnsupported)
		assert.Empty(t, decisions.records(), "no list ran for %v", q)
	}
}

// TestAgentDelegatedAgentList_MineAndSharedListNothing: the delegated caller
// is not a user, so scope=mine, scope=shared and mine=true list nothing.
func TestAgentDelegatedAgentList_MineAndSharedListNothing(t *testing.T) {
	w := newADTListWorld(t, "adt-list-mine")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	for _, q := range []url.Values{{"scope": {"mine"}}, {"scope": {"shared"}}, {"mine": {"true"}}} {
		list := adtDecodeList(t, w.listAgents(t, cred.Token, q))
		assert.Empty(t, list.Agents, "%v", q)
		assert.Zero(t, list.TotalCount, "%v", q)
	}
}

// adtMoreAgents creates n more agents in projectID owned by ownerID. The
// store stamps Created itself, so they are newer than every agent created
// before the call, in creation order.
func (f *adtFixture) adtMoreAgents(t *testing.T, name, projectID, ownerID string, n int) []*store.Agent {
	t.Helper()
	out := make([]*store.Agent, 0, n)
	for i := 0; i < n; i++ {
		a := &store.Agent{
			ID: tid(fmt.Sprintf("%s-%d", name, i)), Name: fmt.Sprintf("%s-%d", name, i), Slug: fmt.Sprintf("%s-%d", name, i),
			ProjectID: projectID, OwnerID: ownerID, Ancestry: []string{ownerID},
		}
		require.NoError(t, f.store.CreateAgent(context.Background(), a))
		out = append(out, a)
	}
	return out
}

// TestAgentDelegatedAgentList_SealedCursorPagingAndReplay: a delegated list
// pages through exactly the allowed items with sealed cursors. A returned
// cursor opens exactly once to a plaintext inner cursor (no double
// sealing); a hand-built plaintext cursor, and a cursor presented by
// another credential, grant or boundary, answer 400 invalid_cursor.
func TestAgentDelegatedAgentList_SealedCursorPagingAndReplay(t *testing.T) {
	w := newADTListWorld(t, "adt-list-page")
	ctx := context.Background()
	w.adtMoreAgents(t, "adt-list-page-more", w.proj.ID, w.carol.ID, 4)
	grant := w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read")
	cred := w.delegated(t, grant.ID)

	full := adtDecodeList(t, w.listAgents(t, cred.Token, nil))
	require.Equal(t, len(w.aliceAgents())+4, full.TotalCount)
	walked, pages := w.walkAgents(t, cred.Token, 3)
	assert.Greater(t, pages, 1)
	assert.Equal(t, full.ids(), sortedIDs(walked...), "pages hold exactly the allowed items, no duplicates or gaps")

	// The first page's cursor opens once, under the binding of this
	// credential and filter, to a plaintext inner cursor; the inner cursor
	// is not itself a sealed cursor.
	first := adtDecodeList(t, w.listAgents(t, cred.Token, url.Values{"limit": {"3"}}))
	require.NotEmpty(t, first.NextCursor)
	credRow, err := w.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
	require.NoError(t, err)
	grantRow, err := w.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	identity := newDelegatedAgentIdentity(credRow, grantRow)
	binding := scopedCursorBinding("agents", store.AgentFilter{AuthorizedProjectIDs: sortedIDs(w.proj.ID, w.other.ID)}, identity)
	inner, err := w.srv.listCursorSealer.Open(first.NextCursor, binding)
	require.NoError(t, err, "the cursor opens under its own binding")
	require.NoError(t, validateAuthorizedListCursor(inner, binding), "it opens to a plaintext inner cursor")
	_, err = w.srv.listCursorSealer.Open(inner, binding)
	assert.ErrorIs(t, err, errInvalidCursor, "the inner cursor is not sealed again")

	// A hand-built plaintext cursor is refused.
	plain := authorizedListCursor(time.Now(), w.agentA.ID, binding)
	adtAssertAPIError(t, w.listAgents(t, cred.Token, url.Values{"cursor": {plain}}), http.StatusBadRequest, ErrCodeInvalidCursor)

	// Another grant's credential, with the same scope, cannot replay it.
	other := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	adtAssertAPIError(t, w.listAgents(t, other.Token, url.Values{"limit": {"3"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)

	// Nor can a project-bounded grant's credential.
	project := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": w.proj.ID}, "agent:list", "agent:read").ID)
	adtAssertAPIError(t, w.listAgents(t, project.Token, url.Values{"limit": {"3"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)

	// A credential exchanged again from the same grant is a new credential.
	again := w.delegated(t, grant.ID)
	adtAssertAPIError(t, w.listAgents(t, again.Token, url.Values{"limit": {"3"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)
}

// TestAgentDelegatedAgentList_NilSealerFailsClosed: without a cursor sealer
// a page that needs a next cursor fails closed (500, no cursor); a page
// that needs none still works, and a presented cursor is invalid.
func TestAgentDelegatedAgentList_NilSealerFailsClosed(t *testing.T) {
	w := newADTListWorld(t, "adt-list-nosealer")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	first := adtDecodeList(t, w.listAgents(t, cred.Token, url.Values{"limit": {"1"}}))
	require.NotEmpty(t, first.NextCursor)

	saved := w.srv.listCursorSealer
	w.srv.listCursorSealer = nil
	t.Cleanup(func() { w.srv.listCursorSealer = saved })

	rec := w.listAgents(t, cred.Token, url.Values{"limit": {"1"}})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "nextCursor")
	list := adtDecodeList(t, w.listAgents(t, cred.Token, nil))
	assert.Equal(t, len(w.aliceAgents()), list.TotalCount, "a page with no next cursor needs no sealer")
	adtAssertAPIError(t, w.listAgents(t, cred.Token, url.Values{"limit": {"1"}, "cursor": {first.NextCursor}}), http.StatusBadRequest, ErrCodeInvalidCursor)
}

// TestAgentDelegatedAgentList_TruncatedPageCarriesCursor: newest first, the
// list holds more agents than the candidate cap in P3 (where Alice may list
// but not read agents), then an older readable P1 agent and the fixture
// agents. Page 1 is empty, carries a next cursor and an approximate total;
// following the cursor reaches the readable agents. The aggregated record of
// page 1 counts the denied candidates and names at most 50.
func TestAgentDelegatedAgentList_TruncatedPageCarriesCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("creates more agents than the list candidate cap")
	}
	w := newADTListWorld(t, "adt-list-trunc")
	ctx := context.Background()
	rd := createCustomRoleDef(t, w.store, "adt-trunc-list-only", []string{"agent.list"})
	createCustomBinding(t, w.store, rd.ID, w.alice.ID, w.p3.ID)
	// The list covers P1 and P3 only.
	require.NoError(t, w.store.DeleteRoleBinding(ctx, w.aliceP2Binding))
	// Creation order is list order, oldest first: the fixture agents and agent D, then the member agent, then the
	// bulk P3 agents.
	member := w.adtMoreAgents(t, "adt-list-trunc-member", w.proj.ID, w.carol.ID, 1)[0]
	w.adtMoreAgents(t, "adt-list-trunc-p3", w.p3.ID, w.bob.ID, authorizedListMaxCandidates+100)
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	decisions := w.captureDecisions(t)

	page := adtDecodeList(t, w.listAgents(t, cred.Token, url.Values{"limit": {"3"}}))
	assert.Empty(t, page.Agents, "the candidate budget ran out on denied items")
	assert.True(t, page.TotalCountApproximate, "the count pass stopped at the candidate cap")
	require.NotEmpty(t, page.NextCursor, "an empty page still carries a cursor")
	adtAssertCursorHidesIDs(t, page.NextCursor, member.ID)
	// After the remaining bulk agents: the member agent, agent D (refused), then agents C and A (B is in P2).
	page = adtDecodeList(t, w.listAgents(t, cred.Token, url.Values{"limit": {"3"}, "cursor": {page.NextCursor}}))
	assert.Equal(t, sortedIDs(member.ID, w.agentC.ID, w.agentA.ID), page.ids(), "following the cursor reaches the older readable agents")

	recs := decisions.listFilterRecords()
	require.Len(t, recs, 2, "one aggregated record per request")
	// Page 1's count pass starts at the bulk P3 agents and stops at the cap: nothing is allowed.
	assert.Zero(t, recs[0].ListFilter.Allowed, "the count pass sees only refused bulk agents")
	assert.True(t, recs[0].ListFilter.Truncated, "more denied items than the record names")
	assert.Len(t, recs[0].ListFilter.DeniedIDs, store.DecisionAuditListFilterMaxDeniedIDs)
}

// adtCtxCapture is a decision-audit emitter that also records whether the
// context it was given was already cancelled.
type adtCtxCapture struct {
	adtDecisionCapture
	ctxErrs []error
}

func (c *adtCtxCapture) EmitDecisionAudit(ctx context.Context, r *store.DecisionAuditRecord) {
	c.mu.Lock()
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	c.mu.Unlock()
	c.adtDecisionCapture.EmitDecisionAudit(ctx, r)
}

// TestListFilterFlush_CancelledRequest: a request cancelled before its list
// finished writes its one aggregated record, marked incomplete; a request
// cancelled after its list finished (the client went away after the
// response) is not marked incomplete. Either way the record is emitted on a
// context that is not cancelled.
func TestListFilterFlush_CancelledRequest(t *testing.T) {
	for _, complete := range []bool{false, true} {
		capture := &adtCtxCapture{}
		a := &AuthzService{DecisionAuditSampleRate: 1.0}
		a.SetDecisionAuditEmitter(capture)
		s := &Server{authzService: a}
		c := newListFilterCollector("agent.read")
		require.NoError(t, c.setDelegatedListAdmission("agent", delegatedListScope{Mode: delegatedListExplicit, ProjectIDs: []string{"p1"}}))
		c.start()
		if complete {
			c.markComplete()
		}
		ctx, cancel := context.WithCancel(contextWithListFilterCollector(contextWithIdentity(context.Background(), adtTestIdentity("agent-x", "project-x")), c))
		cancel()
		s.flushListFilter(ctx, c)
		recs := capture.records()
		require.Len(t, recs, 1, "complete=%v", complete)
		assert.Equal(t, !complete, recs[0].ListFilter.Incomplete, "complete=%v", complete)
		require.Len(t, capture.ctxErrs, 1)
		assert.NoError(t, capture.ctxErrs[0], "the record is emitted on a context that is not cancelled")
	}
}

// TestAgentDelegationDecide_ItemPermissionOnTheListRouteDenies: Decide itself
// refuses the list route's item-filter permission. A request admitted for
// the hub agent list that asks Decide for agent.read on an agent is denied
// credential_not_admitted, while the same state admitted for the agent read
// route is allowed.
func TestAgentDelegationDecide_ItemPermissionOnTheListRouteDenies(t *testing.T) {
	w := newADTListWorld(t, "adt-list-decide")
	ctx := context.Background()
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	row, err := w.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
	require.NoError(t, err)
	grant, err := w.store.GetAgentDelegationGrant(ctx, row.GrantID)
	require.NoError(t, err)
	issuer, err := w.store.GetUser(ctx, grant.IssuerUserID)
	require.NoError(t, err)
	agent, err := w.store.GetAgent(ctx, grant.AgentID)
	require.NoError(t, err)
	ac, err := w.store.GetAgentCredentialByID(ctx, row.ExchangeAgentCredentialID)
	require.NoError(t, err)
	st := &delegatedRequestState{
		identity: newDelegatedAgentIdentity(row, grant), credential: row, grant: grant,
		issuer: issuer, agent: agent, exchangeCred: ac, issuerPC: issuerPrincipal(issuer), memo: &ProjectAdmissionCache{},
	}
	listEntry, ok := agentDelegationAdmissionFor(http.MethodGet, agentsCollectionRouteID)
	require.True(t, ok)
	readEntry, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	req := AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: agentResource(w.agentB), Action: ActionRead, Permission: "agent.read"}

	listCtx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), listEntry))
	d := w.srv.authzService.Decide(listCtx, req)
	assert.False(t, d.Allowed)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, d.AgentDelegation.AgentDelegationCode)
	assert.Equal(t, DeniedByAgentDelegation, d.DeniedBy)

	// Control: the same state and request admitted for the agent read route is allowed.
	readCtx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), readEntry))
	d = w.srv.authzService.Decide(readCtx, req)
	assert.True(t, d.Allowed, d.Reason)
}

// TestListFilterFlush_PanicRecordsIncompleteOnce: the deferred flush routeGuard
// installs writes exactly one record, marked incomplete, when the handler
// panics after the list arm ran, and the panic still propagates.
func TestListFilterFlush_PanicRecordsIncompleteOnce(t *testing.T) {
	capture := &adtDecisionCapture{}
	a := &AuthzService{DecisionAuditSampleRate: 1.0}
	a.SetDecisionAuditEmitter(capture)
	s := &Server{authzService: a}
	id := adtTestIdentity("agent-x", "project-x")
	c := newListFilterCollector("agent.read")
	require.NoError(t, c.setDelegatedListAdmission("agent", delegatedListScope{Mode: delegatedListExplicit, ProjectIDs: []string{"p1"}}))
	c.start()
	ctx := contextWithListFilterCollector(contextWithIdentity(context.Background(), id), c)

	assert.Panics(t, func() {
		defer s.flushListFilter(ctx, c)
		panic("handler fault")
	})
	s.flushListFilter(ctx, c)
	recs := capture.records()
	require.Len(t, recs, 1, "exactly one aggregated record")
	assert.Equal(t, store.DecisionAuditRecordKindListFilter, recs[0].RecordKind)
	assert.True(t, recs[0].ListFilter.Incomplete)
	assert.Equal(t, "agent-x", recs[0].PrincipalID)

	// A request that never reached the list arm writes nothing.
	idle := newListFilterCollector("agent.read")
	s.flushListFilter(contextWithListFilterCollector(contextWithIdentity(context.Background(), id), idle), idle)
	assert.Len(t, capture.records(), 1)
}

// TestListFilterCollector_AdmissionRules: without a list admission every
// item drops; the admission can be set only once; an item without a
// project parent, or outside the scope, drops.
func TestListFilterCollector_AdmissionRules(t *testing.T) {
	id := adtTestIdentity("agent-x", "project-x")
	s := &Server{authzService: &AuthzService{}}

	c := newListFilterCollector("agent.read")
	ctx := contextWithListFilterCollector(context.Background(), c)
	allowed, err := s.delegatedListItemRead(ctx, id, []Resource{{Type: "agent", ID: "a1", ParentType: "project", ParentID: "p1"}})
	require.NoError(t, err)
	assert.Equal(t, []bool{false}, allowed)
	assert.Equal(t, 1, c.dropped, "no list admission: the item drops")

	require.NoError(t, c.setDelegatedListAdmission("agent", delegatedListScope{Mode: delegatedListExplicit, ProjectIDs: []string{"p1"}}))
	assert.ErrorIs(t, c.setDelegatedListAdmission("agent", delegatedListScope{Mode: delegatedListAllProjects}), errDelegatedListAdmissionSet)

	sc := delegatedListScope{Mode: delegatedListExplicit, ProjectIDs: []string{"p1"}}
	assert.True(t, sc.admitsProject("p1"))
	assert.False(t, sc.admitsProject("p2"))
	assert.False(t, sc.admitsProject(""))
	all := delegatedListScope{Mode: delegatedListAllProjects, Excluded: []string{"p2"}}
	assert.True(t, all.admitsProject("p1"))
	assert.False(t, all.admitsProject("p2"))
	assert.False(t, delegatedListScope{}.admitsProject("p1"))

	_, ok := projectOfListItem(Resource{Type: "agent", ID: "a2"})
	assert.False(t, ok, "an item without a project parent is never listed")
	_, ok = projectOfListItem(Resource{Type: "agent", ID: "a2", ParentType: "system"})
	assert.False(t, ok)
}

// TestListDelegated_AlwaysUsesThePerItemScan: listDelegated never takes
// listAuthorizedOrAll's direct path. Only the per-item scan fetches with
// SkipTotalCount set, so every fetch must carry it.
func TestListDelegated_AlwaysUsesThePerItemScan(t *testing.T) {
	sealer, err := newListCursorSealer(make([]byte, 32))
	require.NoError(t, err)
	s := &Server{authzService: &AuthzService{}, listCursorSealer: sealer}
	id := adtTestIdentity("agent-x", "project-x")
	fetches := 0
	_, err = listDelegated(context.Background(), s, id, "", 10, "binding",
		func(_ context.Context, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
			fetches++
			assert.True(t, opts.SkipTotalCount, "a delegated list fetch is a per-item scan batch")
			return &store.ListResult[store.Agent]{Items: []store.Agent{{ID: "a1", ProjectID: "p1"}}}, nil
		},
		agentResource,
		func(a *store.Agent) string { return authorizedListCursor(a.Created, a.ID, "binding") })
	require.NoError(t, err)
	assert.Positive(t, fetches)
}

// TestAgentDelegationPermissionChecks_ItemFilterOnlyOnItemPath: the list
// route's item-filter permission passes the permission checks only on the
// item-filter path; asked for directly on the route, it is refused like
// any permission the route does not admit.
func TestAgentDelegationPermissionChecks_ItemFilterOnlyOnItemPath(t *testing.T) {
	entry, ok := agentDelegationAdmissionFor(http.MethodGet, agentsCollectionRouteID)
	require.True(t, ok)
	require.Equal(t, "agent.read", entry.ItemFilter)
	spec, ok := catalogOperationByID(entry.Operation)
	require.True(t, ok)

	id := adtTestIdentity("agent-x", "project-x")
	id.ceiling.PermissionIDs = []string{"agent.list", "agent.read"}
	st := &delegatedRequestState{identity: id, grant: &store.AgentDelegationGrant{CeilingPermissionIDs: []string{"agent.list", "agent.read"}}}
	a := &AuthzService{}
	a.agentDelegation.policy = func() (opsettings.AgentDelegationPolicySettings, error) {
		return opsettings.AgentDelegationPolicySettings{}, nil
	}

	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, a.agentDelegationPermissionChecks(id, st, spec, entry, "agent.read", false),
		"a direct request for the item permission on the list route is refused")
	assert.Empty(t, a.agentDelegationPermissionChecks(id, st, spec, entry, "agent.read", true), "the item filter accepts it")
	assert.Empty(t, a.agentDelegationPermissionChecks(id, st, spec, entry, "agent.list", false), "the list arm accepts the primary permission")
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, a.agentDelegationPermissionChecks(id, st, spec, entry, "agent.list", true),
		"the item filter accepts only its own permission")
	read, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, a.agentDelegationPermissionChecks(id, st, spec, read, "agent.read", true),
		"a route without an item filter has no item path")
}

// TestAgentsCollectionRoute_ResolvesOnlyTheExactPath: the hub agent list
// resolution the admission table matches accepts exactly /api/v1/agents,
// and only GET has an operation.
func TestAgentsCollectionRoute_ResolvesOnlyTheExactPath(t *testing.T) {
	route, ok := resolveAgentsCollectionRoute(http.MethodGet, "/api/v1/agents")
	require.True(t, ok)
	assert.Equal(t, agentsCollectionRouteID, route.RouteID)
	assert.Equal(t, opAgentList, route.OperationID)
	assert.Equal(t, hubAgentCollectionTarget, route.Target)

	route, ok = resolveAgentsCollectionRoute(http.MethodPost, "/api/v1/agents")
	require.True(t, ok)
	assert.Empty(t, route.OperationID, "only GET lists")
	for _, p := range []string{"/api/v1/agents/", "/api/v1/agents/a-1", "/api/v1/agentsx", "/api/v1/projects/p/agents"} {
		_, ok := resolveAgentsCollectionRoute(http.MethodGet, p)
		assert.False(t, ok, p)
	}
}

// TestAgentDelegatedAgentList_ProjectFormStillRefused: the project form of
// the agent list is not admitted for a delegated credential yet.
func TestAgentDelegatedAgentList_ProjectFormStillRefused(t *testing.T) {
	w := newADTListWorld(t, "adt-list-pform")
	cred := w.delegated(t, w.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:list", "agent:read").ID)
	rec := w.do(t, http.MethodGet, "/api/v1/projects/"+w.proj.ID+"/agents", nil, adtBearer(cred.Token))
	adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
}

// TestResolveListScopesForPrincipal_UserOnly: the issuer list scope is
// resolved only for a user principal.
func TestResolveListScopesForPrincipal_UserOnly(t *testing.T) {
	a := &AuthzService{}
	for _, pc := range []PrincipalContext{
		{},
		principalContextForIdentity(adtTestIdentity("agent-x", "project-x")),
	} {
		res, err := a.resolveListScopesForPrincipal(context.Background(), pc, "agent.list")
		assert.Error(t, err)
		assert.True(t, res.Scopes.IsNone())
	}
}

// TestDecisionLog_ListFilterRecordExcludedByKind: an aggregated list-filter
// record is never mapped to the authorization/decide event, even when every
// other field is in domain; it is counted under its own disposition.
func TestDecisionLog_ListFilterRecordExcludedByKind(t *testing.T) {
	rec := inDomainDecisionRecord()
	rec.RecordKind = store.DecisionAuditRecordKindListFilter
	rec.ListFilter = &store.DecisionAuditListFilter{ItemPermission: "agent.read", Allowed: 1}
	_, d := mapDecisionEnvelope(decisionCtx(), rec)
	assert.Equal(t, decisionAuditExcludedRecordKind, d)

	sink := auditevent.NewCaptureSink()
	counts := &decisionAuditCounts{}
	logger := &decisionAuditLogger{sink: sink, enabled: func() bool { return true }, counts: counts}
	logger.EmitDecisionAudit(decisionCtx(), rec)
	assert.Empty(t, sink.Records())
	assert.Equal(t, uint64(1), counts.get(decisionAuditExcludedRecordKind, true))
	assert.Equal(t, "excluded_record_kind", decisionAuditExcludedRecordKind.String())

	// The same record without a kind is in domain.
	rec.RecordKind, rec.ListFilter = "", nil
	_, d = mapDecisionEnvelope(decisionCtx(), rec)
	assert.Equal(t, decisionAuditEnqueued, d)
}
