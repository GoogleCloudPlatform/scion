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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	// issuerTok is the issuer's credential, for issuing more test identities.
	issuerTok string
	fx        TestIdentityTokenResponse
	project   string
	human     *store.User
	humanTok  string
	// fault is the server store's switch-gated wrapper (installStoreFault),
	// installed right after the server is built.
	fault *fpFaultStore
}

func (e fpEnv) stamp() store.FixtureProvenance {
	return store.FixtureProvenance{RootID: e.fx.Identity.ID, IssuerID: e.issuerID}
}

func newFPEnv(t *testing.T, name string) fpEnv {
	t.Helper()
	srv, s := newTestIdentityServer(t, true)
	fault, sw := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *fpFaultStore {
		return &fpFaultStore{Store: inner, fault: f}
	})
	fault.sw = sw
	srv.SetDispatcher(&deleteGuardDispatcher{})
	issuerID, issuerTok := tiIssuer(t, srv, s, name+"-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member", "purpose": "provenance"})
	project := tiCreateProject(t, srv, fx.AccessToken, name+"-fx-project")
	tiAddProvider(t, s, project)
	human := hubMemberUser(t, s, name+"-human")
	fpAllowMembersToCreateGroups(t, s)
	return fpEnv{srv: srv, store: s, issuerID: issuerID, issuerTok: issuerTok, fx: fx, project: project, human: human, humanTok: tiTokenFor(t, srv, human), fault: fault}
}

// fpFaultStore fails the failAt-th GetAgent of one agent ID after its
// switch is armed. Unarmed, or for any other ID or call, it delegates.
type fpFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	sw    *storeFaultSwitch

	mu      sync.Mutex
	agentID string
	failAt  int
	calls   int
	err     error
}

// failGetAgent arms the wrapper to fail the n-th GetAgent of agentID from
// now on with err (n counts from 1).
func (s *fpFaultStore) failGetAgent(agentID string, n int, err error) {
	s.mu.Lock()
	s.agentID, s.failAt, s.calls, s.err = agentID, n, 0, err
	s.mu.Unlock()
	s.sw.Arm()
}

func (s *fpFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() {
		s.mu.Lock()
		hit := false
		if id == s.agentID && s.failAt > 0 {
			s.calls++
			hit = s.calls == s.failAt
		}
		err := s.err
		s.mu.Unlock()
		if hit {
			return nil, err
		}
	}
	return s.Store.GetAgent(ctx, id)
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
	return fpAgentCreateRequestOn(t, srv, false, parentID, projectID, name)
}

// fpAgentCreateRequestOn posts a sub-agent create as parentID on the project
// route, or on /api/v1/agents when topLevel is set.
func fpAgentCreateRequestOn(t *testing.T, srv *Server, topLevel bool, parentID, projectID, name string) *httptest.ResponseRecorder {
	t.Helper()
	svc := srv.GetAgentTokenService()
	require.NotNil(t, svc)
	scopes := append([]AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}, ScopesForRole(AgentRoleFull)...)
	tok, err := svc.GenerateAgentToken(parentID, projectID, scopes, nil)
	require.NoError(t, err)
	if topLevel {
		return doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents",
			CreateAgentRequest{Name: name, ProjectID: projectID, AgentRole: string(AgentRoleFull)}, tok)
	}
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

// fpAssertEdgeDelegator asserts that agentID's active delegation edge was
// delegated by the given principal: the principal its stamp is resolved from.
func fpAssertEdgeDelegator(t *testing.T, s store.Store, agentID, kind, id string) {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, kind, edges[0].DelegatorType)
	assert.Equal(t, id, edges[0].DelegatorID)
}

// AC 3a-2 (as amended): a scheduled dispatch_agent whose revision principal
// is F, or F's agent, creates an agent stamped F. The stamp comes from the
// child's delegation edge, whose delegator is the revision principal. The
// child keeps no user material (empty ancestry, no owner), as before.
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
	assert.Empty(t, byUser.Ancestry, "a scheduled child keeps no ancestry")
	fpAssertEdgeDelegator(t, e.store, byUser.ID, store.DelegationPrincipalUser, fxID)

	parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-sched-parent")
	require.NoError(t, e.srv.dispatchAgentEventHandler()(ctx, withAgentRevision(t, e.srv, store.ScheduledEvent{
		ID: "evt-fp-agent", ProjectID: e.project, EventType: "dispatch_agent",
		Payload: `{"agentName":"fp-sched-agent","task":"t"}`, CreatedBy: parent,
	}, parent)))
	byAgent, err := e.store.GetAgentBySlug(ctx, e.project, "fp-sched-agent")
	require.NoError(t, err)
	assert.Equal(t, want, byAgent.Fixture)
	assert.Equal(t, parent, byAgent.CreatedBy)
	assert.Empty(t, byAgent.Ancestry, "a scheduled child keeps no ancestry")
	fpAssertEdgeDelegator(t, e.store, byAgent.ID, store.DelegationPrincipalAgent, parent)
}

// AC 3a-2 (negative): a real user's scheduled dispatch has no stamp.
func TestFixtureProvenance_ScheduledDispatchRealUserUnstamped(t *testing.T) {
	f := bypassAgentsSetup(t)
	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "fp-sched-human"))
	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "fp-sched-human")
	require.NoError(t, err)
	assert.True(t, got.Fixture.IsZero())
	fpAssertEdgeDelegator(t, f.store, got.ID, store.DelegationPrincipalUser, f.owner.ID)
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

// fpClientStampFields returns body (marshalled through JSON) with every
// spelling of the stamp fields a client might send, naming clientRoot and
// clientIssuer.
func fpClientStampFields(t *testing.T, body any, clientRoot, clientIssuer string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	m := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"fixtureRootId", "fixture_root_id", "FixtureRootID"} {
		m[k] = clientRoot
	}
	for _, k := range []string{"fixtureIssuerId", "fixture_issuer_id", "FixtureIssuerID"} {
		m[k] = clientIssuer
	}
	m["fixture"] = map[string]string{"rootId": clientRoot, "issuerId": clientIssuer, "RootID": clientRoot, "IssuerID": clientIssuer}
	m["Fixture"] = map[string]string{"RootID": clientRoot, "IssuerID": clientIssuer}
	return m
}

// fpRequireCreated requires a 2xx answer: the handlers ignore unknown
// JSON fields, so client-supplied stamp fields never change the outcome.
func fpRequireCreated(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.True(t, rec.Code >= 200 && rec.Code < 300, "%s: %d %s", what, rec.Code, rec.Body.String())
}

// fpCreateAllWithClientStamp creates a project (unless projectID is set), an
// agent, a schedule and a group as token, each request carrying the client
// stamp fields, and returns the stored stamps.
func fpCreateAllWithClientStamp(t *testing.T, e fpEnv, token, projectID, prefix, clientRoot, clientIssuer string) map[string]store.FixtureProvenance {
	t.Helper()
	ctx := context.Background()
	out := map[string]store.FixtureProvenance{}
	if projectID == "" {
		rec := doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/projects",
			fpClientStampFields(t, CreateProjectRequest{Name: prefix + "-project"}, clientRoot, clientIssuer))
		fpRequireCreated(t, "project", rec)
		var p store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		projectID = p.ID
		out["project"] = fpProject(t, e.store, p.ID).Fixture
		tiAddProvider(t, e.store, p.ID)
	}

	rec := doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/projects/"+projectID+"/agents",
		fpClientStampFields(t, CreateAgentRequest{Name: prefix + "-agent"}, clientRoot, clientIssuer))
	out["agent"] = fpAgent(t, e.store, fpCreatedAgentID(t, rec)).Fixture

	rec = doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		fpClientStampFields(t, fpScheduleBody(prefix+"-sched"), clientRoot, clientIssuer))
	fpRequireCreated(t, "schedule", rec)
	var sc store.Schedule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sc))
	gotSc, err := e.store.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	out["schedule"] = gotSc.Fixture

	rec = doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/groups",
		fpClientStampFields(t, CreateGroupRequest{Name: prefix + "-group"}, clientRoot, clientIssuer))
	fpRequireCreated(t, "group", rec)
	var g store.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	gotG, err := e.store.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	out["group"] = gotG.Fixture
	return out
}

// AC 3a-4: no create or PATCH request can set or change either column.
// Every request carries client-supplied stamp fields naming a second issued
// test identity: a real user's creates stay unstamped, the test identity's
// creates carry its own stamp, and PATCHes leave every stamp unchanged.
func TestFixtureProvenance_RequestsCannotSetStamp(t *testing.T) {
	e := newFPEnv(t, "fp-req")
	ctx := context.Background()
	other := tiIssue(t, e.srv, e.issuerTok, map[string]string{"role": "member", "purpose": "provenance-other"})
	clientRoot, clientIssuer := other.Identity.ID, e.issuerID
	want := e.stamp()

	t.Run("create as a real user", func(t *testing.T) {
		got := fpCreateAllWithClientStamp(t, e, e.humanTok, "", "fp-req-human", clientRoot, clientIssuer)
		require.Len(t, got, 4)
		for kind, stamp := range got {
			assert.True(t, stamp.IsZero(), "%s created by a real user must have no stamp, got %+v", kind, stamp)
		}
	})

	t.Run("create as the test identity", func(t *testing.T) {
		got := fpCreateAllWithClientStamp(t, e, e.fx.AccessToken, "", "fp-req-fx", clientRoot, clientIssuer)
		require.Len(t, got, 4)
		for kind, stamp := range got {
			assert.Equal(t, want, stamp, "%s: the server's stamp, not the client's", kind)
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
			rec := doRequestWithToken(t, e.srv, r.token, http.MethodPatch, r.path, fpClientStampFields(t, r.body, clientRoot, clientIssuer))
			fpRequireCreated(t, "PATCH "+r.path, rec)
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

// fpCreateGitProject creates a git-backed project as token and returns its ID.
func fpCreateGitProject(t *testing.T, e fpEnv, token, name string) string {
	t.Helper()
	rec := doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: name, GitRemote: "https://github.com/example/" + name + ".git"})
	fpRequireCreated(t, "create git project", rec)
	var p store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.NotEmpty(t, p.ID, rec.Body.String())
	return p.ID
}

// fpCloneProject clones sourceID as token and returns the clone's ID.
func fpCloneProject(t *testing.T, e fpEnv, token, sourceID, name string) string {
	t.Helper()
	rec := doRequestWithToken(t, e.srv, token, http.MethodPost, "/api/v1/projects/"+sourceID+"/clone", map[string]any{"name": name})
	fpRequireCreated(t, "clone", rec)
	var p store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.NotEmpty(t, p.ID, rec.Body.String())
	return p.ID
}

// AC 3a-3 (clone, register): a clone takes the cloning principal's stamp,
// never the source's: a real user's clone of a test identity's project is
// unstamped, and a test identity's clone of a real user's project carries
// the test identity's own stamp. A project created through register is
// stamped like a create.
func TestFixtureProvenance_CloneAndRegisterStamp(t *testing.T) {
	e := newFPEnv(t, "fp-clone")
	want := e.stamp()
	fxProject := fpCreateGitProject(t, e, e.fx.AccessToken, "fp-clone-fx-src")
	require.Equal(t, want, fpProject(t, e.store, fxProject).Fixture)
	humanProject := fpCreateGitProject(t, e, e.humanTok, "fp-clone-human-src")

	// Each principal needs read access to the source it clones.
	grantProjectRole(t, e.store, e.human.ID, fxProject, store.ProjectRoleMember)
	grantProjectRole(t, e.store, e.fx.Identity.ID, humanProject, store.ProjectRoleMember)

	humanClone := fpCloneProject(t, e, e.humanTok, fxProject, "fp-clone-by-human")
	assert.True(t, fpProject(t, e.store, humanClone).Fixture.IsZero(), "a real user's clone of a stamped project is unstamped")
	fxClone := fpCloneProject(t, e, e.fx.AccessToken, humanProject, "fp-clone-by-fx")
	assert.Equal(t, want, fpProject(t, e.store, fxClone).Fixture, "a test identity's clone carries its own stamp")

	rec := doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{Name: "fp-clone-registered", GitRemote: "https://github.com/example/fp-clone-registered.git"})
	fpRequireCreated(t, "register", rec)
	var reg struct {
		Project *store.Project `json:"project"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg), rec.Body.String())
	require.NotNil(t, reg.Project, rec.Body.String())
	assert.Equal(t, want, fpProject(t, e.store, reg.Project.ID).Fixture, "a registered project is stamped")
}

// AC 3a-5: a parent-agent lookup failure during sub-agent create fails the
// create with no row or edge, on both create routes. Before 3a the create
// went ahead with an empty ancestry when the calling agent's row could not
// be read (the regression covered here). The calling agent's row is read
// several times per request (standing, attribution, role ceiling), so the
// test fails each read in turn until it reaches the attribution read,
// identified by its own 409 message; the read-error case then fails that
// same read and expects 500.
func TestFixtureProvenance_ParentLookupFailureFailsCreate(t *testing.T) {
	for _, topLevel := range []bool{false, true} {
		route := "project route"
		if topLevel {
			route = "agents route"
		}
		t.Run(route, func(t *testing.T) {
			e := newFPEnv(t, "fp-parent")
			ctx := context.Background()
			parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-parent")
			edges, err := e.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, parent)
			require.NoError(t, err)

			attributionRead := 0
			for n := 1; n <= 8 && attributionRead == 0; n++ {
				child := fmt.Sprintf("fp-probe-%d", n)
				e.fault.failGetAgent(parent, n, store.ErrNotFound)
				rec := fpAgentCreateRequestOn(t, e.srv, topLevel, parent, e.project, child)
				if rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "the agent creating it no longer exists") {
					attributionRead = n
					fpAssertNoChild(t, e.store, e.project, child, parent, len(edges))
				}
			}
			require.NotZero(t, attributionRead, "no GetAgent of the calling agent reached the attribution read")

			e.fault.failGetAgent(parent, attributionRead, errors.New("injected read failure"))
			rec := fpAgentCreateRequestOn(t, e.srv, topLevel, parent, e.project, "fp-read-error")
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			fpAssertNoChild(t, e.store, e.project, "fp-read-error", parent, len(edges))
		})
	}
}

// fpAssertNoChild asserts that no agent named slug exists in projectID and
// that parentID delegated no new edge.
func fpAssertNoChild(t *testing.T, s store.Store, projectID, slug, parentID string, edgesBefore int) {
	t.Helper()
	ctx := context.Background()
	_, err := s.GetAgentBySlug(ctx, projectID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row for %s", slug)
	edges, err := s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, parentID)
	require.NoError(t, err)
	assert.Len(t, edges, edgesBefore, "no delegation edge for %s", slug)
}

// A sub-agent create by an agent whose row is gone is refused by the
// creating agent's standing check (403) before attribution runs, and writes
// nothing. This pins today's answer; the attribution branch itself is
// covered by TestFixtureProvenance_ParentLookupFailureFailsCreate.
func TestFixtureProvenance_DeletedParentCreateWritesNothing(t *testing.T) {
	e := newFPEnv(t, "fp-gone")
	ctx := context.Background()
	parent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fp-gone-parent")
	edges, err := e.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, parent)
	require.NoError(t, err)
	require.NoError(t, e.store.DeleteAgent(ctx, parent))

	rec := fpAgentCreateRequest(t, e.srv, parent, e.project, "fp-gone-child")
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	fpAssertNoChild(t, e.store, e.project, "fp-gone-child", parent, len(edges))
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
		"human":                        {Kind: fixturePrincipalUser, ID: human.ID},
		"missing user":                 {Kind: fixturePrincipalUser, ID: tid("fp-rules-nobody")},
		"missing user, ordinary email": {Kind: fixturePrincipalUser, ID: tid("fp-rules-nobody-2"), Email: "nobody@example.com"},
		"dev user":                     {Kind: fixturePrincipalUser, ID: DevUserID},
		"system":                       {},
	} {
		got, err := resolveFixtureRoot(ctx, s, p)
		require.NoError(t, err, name)
		assert.True(t, got.IsZero(), name)
	}

	// A user ID with no row whose email is in the reserved test identity
	// domain is a deleted test identity: refused, never left unstamped.
	_, err = resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalUser, ID: tid("fp-rules-gone-fx"),
		Email: "gone@" + store.TestFixtureEmailDomain})
	assert.ErrorIs(t, err, errFixtureRootUnresolved)
	assert.ErrorIs(t, err, store.ErrNotFound)

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

// B4 / DEC-2 b: a test identity whose user row is gone (deleted after it
// authenticated) cannot create an unstamped project, group or schedule. The
// identity is put in the context directly, standing in for a request that
// authenticated before the delete.
func TestFixtureProvenance_DeletedTestIdentityCannotCreate(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	gone := tid("fp-deleted-fx")
	email := "deleted-" + gone[:8] + "@" + store.TestFixtureEmailDomain
	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser(gone, email, "", store.UserRoleMember, string(ClientTypeAPI)))

	pid := tid("fp-deleted-fx-project")
	err := srv.createProjectWithOwner(ctx, &store.Project{ID: pid, Name: "fp-deleted", Slug: "fp-deleted", CreatedBy: gone, OwnerID: gone}, gone)
	require.ErrorIs(t, err, errFixtureRootUnresolved)
	rec := httptest.NewRecorder()
	require.True(t, writeFixtureRootError(rec, err))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "the identity creating it no longer exists")
	_, getErr := s.GetProject(context.Background(), pid)
	assert.ErrorIs(t, getErr, store.ErrNotFound, "no project row")

	principal := fixturePrincipalFromContext(ctx)
	assert.Equal(t, email, principal.Email)
	require.ErrorIs(t, stampGroup(ctx, s, &store.Group{ID: tid("fp-deleted-fx-group")}, principal), errFixtureRootUnresolved)
	require.ErrorIs(t, stampSchedule(ctx, s, &store.Schedule{ID: tid("fp-deleted-fx-sched")}, principal), errFixtureRootUnresolved)

	// An ordinary identity with no row (outside the reserved domain) still
	// creates, unstamped.
	plain := tid("fp-rowless-human")
	plainCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(plain, "rowless@example.com", "", store.UserRoleMember, string(ClientTypeAPI)))
	ppid := tid("fp-rowless-project")
	require.NoError(t, srv.createProjectWithOwner(plainCtx, &store.Project{ID: ppid, Name: "fp-rowless", Slug: "fp-rowless"}, ""))
	assert.True(t, fpProject(t, s, ppid).Fixture.IsZero())
}
