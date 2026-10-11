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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for creation-time test-identity provenance (ptone/scion#4240,
// Phase 3a). Every identity here is a synthetic fixture in an in-memory
// store.

// fpEnv is a test-identity server with one issued fixture, the fixture's
// own project (served by a broker) and a human member.
type fpEnv struct {
	srv      *Server
	store    store.Store
	issuerID string
	fx       TestIdentityTokenResponse
	project  string
	human    *store.User
	humanTok string
}

func (e fpEnv) stamp() store.FixtureProvenance {
	return store.FixtureProvenance{RootID: e.fx.Identity.ID, IssuerID: e.issuerID}
}

func newFPEnv(t *testing.T, name string) fpEnv {
	t.Helper()
	srv, s := newTestIdentityServer(t, true)
	srv.SetDispatcher(&deleteGuardDispatcher{})
	issuerID, issuerTok := tiIssuer(t, srv, s, name+"-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member", "purpose": "provenance"})
	project := tiCreateProject(t, srv, fx.AccessToken, name+"-fx-project")
	tiAddProvider(t, s, project)
	human := hubMemberUser(t, s, name+"-human")
	fpAllowMembersToCreateGroups(t, s)
	return fpEnv{srv: srv, store: s, issuerID: issuerID, fx: fx, project: project, human: human, humanTok: tiTokenFor(t, srv, human)}
}

// fpAllowMembersToCreateGroups adds group.create to the seeded hub-member
// role, as an operator may. The default hub-member role cannot create
// groups, and a test identity holds no system grant beyond hub-member.
func fpAllowMembersToCreateGroups(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, append(append([]string(nil), rd.Permissions...), "group.create")))
}

// fpCreateAgent creates an agent with a user token and returns its ID.
func fpCreateAgent(t *testing.T, srv *Server, token, projectID, name string) string {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/projects/"+projectID+"/agents",
		CreateAgentRequest{Name: name, AgentRole: string(AgentRoleFull)})
	return fpCreatedAgentID(t, rec)
}

// fpCreateSubAgent creates an agent with an agent token for parentID.
func fpCreateSubAgent(t *testing.T, srv *Server, parentID, projectID, name string) string {
	t.Helper()
	rec := fpAgentCreateRequest(t, srv, parentID, projectID, name)
	return fpCreatedAgentID(t, rec)
}

func fpAgentCreateRequest(t *testing.T, srv *Server, parentID, projectID, name string) *httptest.ResponseRecorder {
	t.Helper()
	svc := srv.GetAgentTokenService()
	require.NotNil(t, svc)
	scopes := append([]AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}, ScopesForRole(AgentRoleFull)...)
	tok, err := svc.GenerateAgentToken(parentID, projectID, scopes, nil)
	require.NoError(t, err)
	return doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/agents",
		CreateAgentRequest{Name: name, AgentRole: string(AgentRoleFull)}, tok)
}

func fpCreatedAgentID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create agent: %d %s", rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	require.NotNil(t, resp.Agent, rec.Body.String())
	return resp.Agent.ID
}

func fpAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func fpProject(t *testing.T, s store.Store, id string) *store.Project {
	t.Helper()
	p, err := s.GetProject(context.Background(), id)
	require.NoError(t, err)
	return p
}

// fpCreateGroup creates a group with token and returns its ID.
func fpCreateGroup(t *testing.T, srv *Server, token string, body any) string {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/groups", body)
	require.Equal(t, http.StatusCreated, rec.Code, "create group: %s", rec.Body.String())
	var g store.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g), rec.Body.String())
	require.NotEmpty(t, g.ID)
	return g.ID
}

// fpCreateSchedule creates a dispatch_agent schedule with token and
// returns its ID.
func fpCreateSchedule(t *testing.T, srv *Server, token, projectID string, body any) string {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", body)
	require.Equal(t, http.StatusCreated, rec.Code, "create schedule: %s", rec.Body.String())
	var sc store.Schedule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sc), rec.Body.String())
	require.NotEmpty(t, sc.ID)
	return sc.ID
}

func fpScheduleBody(name string) CreateScheduleRequest {
	return CreateScheduleRequest{Name: name, CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: name + "-agent", Task: "t"}
}

// AC 3a-1: an agent created by fixture F (session JWT) is stamped
// (F, F.issued_by); a sub-agent it creates (agent JWT) and a sub-sub-agent
// carry the same stamp. A real user's agent has no stamp.
func TestFixtureProvenance_AgentChainStamped(t *testing.T) {
	e := newFPEnv(t, "fp-chain")
	want := e.stamp()

	top := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-top")
	sub := fpCreateSubAgent(t, e.srv, top, e.project, "fp-sub")
	subsub := fpCreateSubAgent(t, e.srv, sub, e.project, "fp-subsub")

	topRow, subRow, subsubRow := fpAgent(t, e.store, top), fpAgent(t, e.store, sub), fpAgent(t, e.store, subsub)
	assert.Equal(t, want, topRow.Fixture, "fixture's agent")
	assert.Equal(t, want, subRow.Fixture, "sub-agent")
	assert.Equal(t, want, subsubRow.Fixture, "sub-sub-agent")
	assert.Equal(t, []string{e.fx.Identity.ID}, topRow.Ancestry)
	assert.Equal(t, []string{e.fx.Identity.ID, top}, subRow.Ancestry)
	assert.Equal(t, []string{e.fx.Identity.ID, top, sub}, subsubRow.Ancestry)

	// A real user's agent, in the real user's own project.
	humanProject := tiCreateProject(t, e.srv, e.humanTok, "fp-chain-human-project")
	tiAddProvider(t, e.store, humanProject)
	humanAgent := fpCreateAgent(t, e.srv, e.humanTok, humanProject, "fp-human-agent")
	humanSub := fpCreateSubAgent(t, e.srv, humanAgent, humanProject, "fp-human-sub")
	assert.True(t, fpAgent(t, e.store, humanAgent).Fixture.IsZero(), "a real user's agent has no stamp")
	assert.True(t, fpAgent(t, e.store, humanSub).Fixture.IsZero(), "a real user's sub-agent has no stamp")
	assert.True(t, fpProject(t, e.store, humanProject).Fixture.IsZero(), "a real user's project has no stamp")
}

// AC 3a-2: a scheduled dispatch_agent whose revision principal is F, or F's
// agent, creates an agent stamped F with a non-empty OwnerID and Ancestry.
func TestFixtureProvenance_ScheduledDispatchStamped(t *testing.T) {
	e := newFPEnv(t, "fp-sched")
	ctx := context.Background()
	want := e.stamp()
	fxID := e.fx.Identity.ID

	require.NoError(t, e.srv.dispatchAgentEventHandler()(ctx, withSessionRevision(store.ScheduledEvent{
		ID: "evt-fp-user", ProjectID: e.project, EventType: "dispatch_agent",
		Payload: `{"agentName":"fp-sched-user","task":"t"}`, CreatedBy: fxID,
	}, fxID)))
	byUser, err := e.store.GetAgentBySlug(ctx, e.project, "fp-sched-user")
	require.NoError(t, err)
	assert.Equal(t, want, byUser.Fixture)
	assert.Equal(t, fxID, byUser.CreatedBy)
	assert.Equal(t, fxID, byUser.OwnerID, "a scheduled child has an owner")
	assert.Equal(t, []string{fxID}, byUser.Ancestry, "a scheduled child has an ancestry")

	parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-sched-parent")
	require.NoError(t, e.srv.dispatchAgentEventHandler()(ctx, withAgentRevision(t, e.srv, store.ScheduledEvent{
		ID: "evt-fp-agent", ProjectID: e.project, EventType: "dispatch_agent",
		Payload: `{"agentName":"fp-sched-agent","task":"t"}`, CreatedBy: parent,
	}, parent)))
	byAgent, err := e.store.GetAgentBySlug(ctx, e.project, "fp-sched-agent")
	require.NoError(t, err)
	assert.Equal(t, want, byAgent.Fixture)
	assert.Equal(t, parent, byAgent.CreatedBy)
	assert.Equal(t, parent, byAgent.OwnerID)
	assert.Equal(t, []string{fxID, parent}, byAgent.Ancestry)
}

// AC 3a-2 (negative): a real user's scheduled dispatch has no stamp; its
// child still gets an owner and an ancestry.
func TestFixtureProvenance_ScheduledDispatchRealUserUnstamped(t *testing.T) {
	f := bypassAgentsSetup(t)
	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "fp-sched-human"))
	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "fp-sched-human")
	require.NoError(t, err)
	assert.True(t, got.Fixture.IsZero())
	assert.Equal(t, f.owner.ID, got.OwnerID)
	assert.Equal(t, []string{f.owner.ID}, got.Ancestry)
}

// AC 3a-3: a project, schedule and group created by F are stamped, and so
// are the project's implicit groups. A project created by a real user, with
// F later added as an owner, stays unstamped, while an agent F creates in
// it is stamped.
func TestFixtureProvenance_ProjectScheduleGroupStamped(t *testing.T) {
	e := newFPEnv(t, "fp-psg")
	ctx := context.Background()
	want := e.stamp()

	proj := fpProject(t, e.store, e.project)
	assert.Equal(t, want, proj.Fixture, "fixture's project")
	for _, slug := range []string{"project:" + proj.Slug + ":agents", projectMembersGroupSlug(proj.Slug)} {
		g, err := e.store.GetGroupBySlug(ctx, slug)
		require.NoError(t, err, slug)
		assert.Equal(t, want, g.Fixture, "implicit group %s", slug)
	}

	schedID := fpCreateSchedule(t, e.srv, e.fx.AccessToken, e.project, fpScheduleBody("fp-psg-sched"))
	sc, err := e.store.GetSchedule(ctx, schedID)
	require.NoError(t, err)
	assert.Equal(t, want, sc.Fixture, "fixture's schedule")

	groupID := fpCreateGroup(t, e.srv, e.fx.AccessToken, CreateGroupRequest{Name: "fp-psg-group"})
	g, err := e.store.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, want, g.Fixture, "fixture's group")

	// A real user's project, schedule and group are unstamped.
	humanProject := tiCreateProject(t, e.srv, e.humanTok, "fp-psg-human-project")
	tiAddProvider(t, e.store, humanProject)
	humanSched := fpCreateSchedule(t, e.srv, e.humanTok, humanProject, fpScheduleBody("fp-psg-human-sched"))
	hs, err := e.store.GetSchedule(ctx, humanSched)
	require.NoError(t, err)
	assert.True(t, hs.Fixture.IsZero(), "a real user's schedule has no stamp")
	humanGroup := fpCreateGroup(t, e.srv, e.humanTok, CreateGroupRequest{Name: "fp-psg-human-group"})
	hg, err := e.store.GetGroup(ctx, humanGroup)
	require.NoError(t, err)
	assert.True(t, hg.Fixture.IsZero(), "a real user's group has no stamp")

	// F added to the real user's project as an owner: the project stays
	// unstamped; what F creates in it is stamped.
	grantProjectRole(t, e.store, e.fx.Identity.ID, humanProject, store.ProjectRoleOwner)
	fxAgentInHuman := fpCreateAgent(t, e.srv, e.fx.AccessToken, humanProject, "fp-psg-fx-in-human")
	assert.Equal(t, want, fpAgent(t, e.store, fxAgentInHuman).Fixture)
	assert.True(t, fpProject(t, e.store, humanProject).Fixture.IsZero(), "a real user's project stays unstamped")
}

// fpForgedStampFields returns body (marshalled through JSON) with every
// spelling of the stamp fields a client might try added, all naming forged.
func fpForgedStampFields(t *testing.T, body any, forged string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	m := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"fixtureRootId", "fixtureIssuerId", "fixture_root_id", "fixture_issuer_id", "FixtureRootID", "FixtureIssuerID"} {
		m[k] = forged
	}
	m["fixture"] = map[string]string{"rootId": forged, "issuerId": forged, "RootID": forged, "IssuerID": forged}
	m["Fixture"] = map[string]string{"RootID": forged, "IssuerID": forged}
	return m
}

// AC 3a-4: no create or PATCH request can set or change either column. A
// real user sending forged stamp fields gets no stamp (the fields are
// ignored or the request refused); a fixture sending them gets its own
// stamp; PATCHes with them leave stamps unchanged.
func TestFixtureProvenance_RequestsCannotSetStamp(t *testing.T) {
	e := newFPEnv(t, "fp-req")
	ctx := context.Background()
	forged := tid("fp-forged-root")
	want := e.stamp()

	accepted := func(rec *httptest.ResponseRecorder) bool { return rec.Code >= 200 && rec.Code < 300 }
	refused := func(rec *httptest.ResponseRecorder) bool { return rec.Code >= 400 && rec.Code < 500 }

	t.Run("create as a real user", func(t *testing.T) {
		rec := doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, "/api/v1/projects",
			fpForgedStampFields(t, CreateProjectRequest{Name: "fp-req-human-project"}, forged))
		if !accepted(rec) {
			require.True(t, refused(rec), "%d %s", rec.Code, rec.Body.String())
			return
		}
		var p store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		assert.True(t, fpProject(t, e.store, p.ID).Fixture.IsZero(), "project")
		tiAddProvider(t, e.store, p.ID)

		rec = doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, "/api/v1/projects/"+p.ID+"/agents",
			fpForgedStampFields(t, CreateAgentRequest{Name: "fp-req-human-agent"}, forged))
		if accepted(rec) {
			assert.True(t, fpAgent(t, e.store, fpCreatedAgentID(t, rec)).Fixture.IsZero(), "agent")
		} else {
			assert.True(t, refused(rec), "%d %s", rec.Code, rec.Body.String())
		}

		rec = doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, "/api/v1/projects/"+p.ID+"/schedules",
			fpForgedStampFields(t, fpScheduleBody("fp-req-human-sched"), forged))
		if accepted(rec) {
			var sc store.Schedule
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sc))
			got, err := e.store.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			assert.True(t, got.Fixture.IsZero(), "schedule")
		} else {
			assert.True(t, refused(rec), "%d %s", rec.Code, rec.Body.String())
		}

		rec = doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, "/api/v1/groups",
			fpForgedStampFields(t, CreateGroupRequest{Name: "fp-req-human-group"}, forged))
		if accepted(rec) {
			var g store.Group
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
			got, err := e.store.GetGroup(ctx, g.ID)
			require.NoError(t, err)
			assert.True(t, got.Fixture.IsZero(), "group")
		} else {
			assert.True(t, refused(rec), "%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("create as the fixture", func(t *testing.T) {
		rec := doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodPost, "/api/v1/projects/"+e.project+"/agents",
			fpForgedStampFields(t, CreateAgentRequest{Name: "fp-req-fx-agent"}, forged))
		if accepted(rec) {
			assert.Equal(t, want, fpAgent(t, e.store, fpCreatedAgentID(t, rec)).Fixture, "the server's stamp wins")
		} else {
			assert.True(t, refused(rec), "%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("patch", func(t *testing.T) {
		agentID := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-req-patch-agent")
		schedID := fpCreateSchedule(t, e.srv, e.fx.AccessToken, e.project, fpScheduleBody("fp-req-patch-sched"))
		groupID := fpCreateGroup(t, e.srv, e.fx.AccessToken, CreateGroupRequest{Name: "fp-req-patch-group"})
		humanProject := tiCreateProject(t, e.srv, e.humanTok, "fp-req-patch-human-project")

		for _, r := range []struct {
			token, path string
			body        any
		}{
			{e.fx.AccessToken, "/api/v1/agents/" + agentID, map[string]any{"taskSummary": "patched"}},
			{e.fx.AccessToken, "/api/v1/projects/" + e.project, map[string]any{"name": "fp-req-patched"}},
			{e.fx.AccessToken, "/api/v1/projects/" + e.project + "/schedules/" + schedID, map[string]any{"name": "fp-req-patched-sched"}},
			{e.fx.AccessToken, "/api/v1/groups/" + groupID, map[string]any{"name": "fp-req-patched-group"}},
			{e.humanTok, "/api/v1/projects/" + humanProject, map[string]any{"name": "fp-req-patched-human"}},
		} {
			rec := doRequestWithToken(t, e.srv, r.token, http.MethodPatch, r.path, fpForgedStampFields(t, r.body, forged))
			assert.True(t, accepted(rec) || refused(rec), "PATCH %s: %d %s", r.path, rec.Code, rec.Body.String())
		}
		assert.Equal(t, want, fpAgent(t, e.store, agentID).Fixture, "agent")
		assert.Equal(t, want, fpProject(t, e.store, e.project).Fixture, "project")
		sc, err := e.store.GetSchedule(ctx, schedID)
		require.NoError(t, err)
		assert.Equal(t, want, sc.Fixture, "schedule")
		g, err := e.store.GetGroup(ctx, groupID)
		require.NoError(t, err)
		assert.Equal(t, want, g.Fixture, "group")
		assert.True(t, fpProject(t, e.store, humanProject).Fixture.IsZero(), "a real user's project cannot gain a stamp")
	})
}

// failGetAgentStore fails GetAgent for one agent ID.
type failGetAgentStore struct {
	store.Store
	agentID string
	err     error
}

func (s *failGetAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.agentID {
		return nil, s.err
	}
	return s.Store.GetAgent(ctx, id)
}

// AC 3a-5: a parent-agent lookup failure during sub-agent create fails the
// create. Before 3a, resolveAgentCreateAttribution's predecessor went ahead
// with an empty ancestry when the calling agent's row could not be read (the
// regression covered here): a missing row now answers 409 and a read error
// 500, before anything is written.
func TestFixtureProvenance_ParentLookupFailureFailsAttribution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{"parent row missing", store.ErrNotFound, http.StatusConflict},
		{"parent read error", errors.New("injected read failure"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFPEnv(t, "fp-parent")
			parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-parent")
			e.srv.store = &failGetAgentStore{Store: e.store, agentID: parent, err: tc.err}
			defer func() { e.srv.store = e.store }()

			ctx := contextWithIdentity(context.Background(), agentIdentityFor(parent, e.project, ScopeAgentCreate))
			attr, err := e.srv.resolveAgentCreateAttribution(ctx)
			require.ErrorIs(t, err, errAgentCreatorUnreadable)
			assert.Empty(t, attr.Ancestry, "no attribution is returned")
			rec := httptest.NewRecorder()
			writeAgentCreateAttributionError(ctx, rec, err)
			assert.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
		})
	}
}

// AC 3a-5 (route): a sub-agent create by an agent whose row is gone is
// refused and writes no row, edge or audit.
func TestFixtureProvenance_DeletedParentCreateWritesNothing(t *testing.T) {
	e := newFPEnv(t, "fp-gone")
	ctx := context.Background()
	parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-gone-parent")
	edges, err := e.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, parent)
	require.NoError(t, err)
	require.NoError(t, e.store.DeleteAgent(ctx, parent))

	rec := fpAgentCreateRequest(t, e.srv, parent, e.project, "fp-gone-child")
	assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusConflict, "%d %s", rec.Code, rec.Body.String())
	_, err = e.store.GetAgentBySlug(ctx, e.project, "fp-gone-child")
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	after, err := e.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, parent)
	require.NoError(t, err)
	assert.Len(t, after, len(edges), "no delegation edge")
}

// AC 3a-5 (transaction): commitAgentCreate refuses a write whose delegator
// agent's row cannot be read, inside the create transaction, and writes no
// row, edge or audit.
func TestFixtureProvenance_CommitRefusesUnresolvedDelegator(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	owner := newActiveMember(t, s, "user-fp-owner", "fp-owner@test.com")
	ghost := tid("agent-fp-ghost")
	a := &store.Agent{ID: tid("agent-fp-ghost-child"), Slug: "fp-ghost-child", Name: "fp-ghost-child", ProjectID: project.ID,
		Phase: "created", OwnerID: ghost, CreatedBy: ghost, Ancestry: []string{owner.ID, ghost}}
	err := srv.commitAgentCreate(ctx, agentCreateWrite{
		Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
		Agent:      a,
		Slug:       a.Slug,
		Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalAgent, DelegatorID: ghost,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: project.ID, Role: string(AgentRoleNone), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
	require.ErrorIs(t, err, errFixtureRootUnresolved)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, getErr := s.GetAgent(ctx, a.ID)
	require.ErrorIs(t, getErr, store.ErrNotFound, "no agent row")
	edges, edgeErr := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, a.ID)
	require.NoError(t, edgeErr)
	assert.Empty(t, edges, "no delegation edge")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentDelegation, a.ID), "no create audit")
}

// resolveFixtureRoot stamps a test identity with its issuer; yields no
// stamp for a human, a missing user row, the dev user and a system caller;
// and fails closed for an unreadable agent or user row and an unknown
// principal kind.
func TestResolveFixtureRoot_Rules(t *testing.T) {
	_, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	issuer := hubMemberUser(t, s, "fp-rules-issuer")
	fx := tiStoreFixture(t, s, issuer.ID, time.Now().Add(30*time.Minute))
	human := hubMemberUser(t, s, "fp-rules-human")

	got, err := resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalUser, ID: fx.ID})
	require.NoError(t, err)
	assert.Equal(t, store.FixtureProvenance{RootID: fx.ID, IssuerID: issuer.ID}, got)

	for name, p := range map[string]fixturePrincipal{
		"human":        {Kind: fixturePrincipalUser, ID: human.ID},
		"missing user": {Kind: fixturePrincipalUser, ID: tid("fp-rules-nobody")},
		"dev user":     {Kind: fixturePrincipalUser, ID: DevUserID},
		"system":       {},
	} {
		got, err := resolveFixtureRoot(ctx, s, p)
		require.NoError(t, err, name)
		assert.True(t, got.IsZero(), name)
	}

	_, err = resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalAgent, ID: tid("fp-rules-no-agent")})
	assert.ErrorIs(t, err, errFixtureRootUnresolved)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalAgent})
	assert.ErrorIs(t, err, errFixtureRootUnresolved)
	_, err = resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: "broker", ID: "b"})
	assert.ErrorIs(t, err, errFixtureRootUnresolved)

	readErr := errors.New("injected read failure")
	_, err = resolveFixtureRoot(ctx, &failGetUserStore{Store: s, err: readErr}, fixturePrincipal{Kind: fixturePrincipalUser, ID: fx.ID})
	assert.ErrorIs(t, err, errFixtureRootUnresolved)
	assert.ErrorIs(t, err, readErr)
}

// failGetUserStore fails every GetUser.
type failGetUserStore struct {
	store.Store
	err error
}

func (s *failGetUserStore) GetUser(context.Context, string) (*store.User, error) { return nil, s.err }
