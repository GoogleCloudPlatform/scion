// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uatCreateFixture is the bypassAgents world plus a member user who holds
// agent.create in the fixture project.
type uatCreateFixture struct {
	*bypassAgentsFixture
	creator *store.User
	path    string
}

func newUATCreateFixture(t *testing.T, name string) *uatCreateFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	creator := hubMemberUser(t, f.store, name+"-creator")
	grantFixtureRole(t, f, creator.ID, store.ProjectRoleMember)
	return &uatCreateFixture{bypassAgentsFixture: f, creator: creator, path: "/api/v1/projects/" + f.proj.ID + "/agents"}
}

// withDispatcher installs a dispatcher that mints through the server
// against a recording broker client.
func (f *uatCreateFixture) withDispatcher(t *testing.T) *mintBrokerClient {
	t.Helper()
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)
	return client
}

// uat returns a V1 UAT identity for the creator holding exactly selectors.
func (f *uatCreateFixture) uat(t *testing.T, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	c := uatCeilingFromSelectors(t, selectors...)
	return NewScopedUserIdentityWithCeiling(authUser(f.creator), f.proj.ID, selectors, "uat-"+f.creator.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs})
}

func (f *uatCreateFixture) create(t *testing.T, identity Identity, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	return requestAsIdentity(t, f.srv, identity, http.MethodPost, f.path, req)
}

// setProjectAnnotation writes one project annotation.
func (f *uatCreateFixture) setProjectAnnotation(t *testing.T, key, value string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[key] = value
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

// createdAgent returns the stored agent for slug and its single active edge.
func (f *uatCreateFixture) createdAgent(t *testing.T, rec *httptest.ResponseRecorder, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	return agent, edges[0]
}

// minimalSelectors is agent:create plus the seven read selectors.
func minimalSelectors(t *testing.T) []string {
	t.Helper()
	createP, _ := registryPermission("agent.create")
	return append([]string{createP.UATScope}, readonlyRoleUATSelectors(t)...)
}

// assertCreateWroteNothing asserts no agent row for slug, no edge delegated
// by delegatorID, no agent audit record and no project subscription.
func assertCreateWroteNothing(t *testing.T, s store.Store, projectID, slug, delegatorID string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.GetAgentBySlug(ctx, projectID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	edges, err := s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, delegatorID)
	require.NoError(t, err)
	assert.Empty(t, edges, "no delegation edge")
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent"})
	require.NoError(t, err)
	assert.Empty(t, audits, "no agent audit record")
	subs, err := s.GetNotificationSubscriptionsByProject(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, subs, "no subscription")
}

// assertCeilingDenial asserts a 403 carrying details.denied_by and returns
// the message.
func assertCeilingDenial(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, map[string]interface{}{"denied_by": string(DeniedByDelegationCeiling)}, apiErr.Details)
	return apiErr.Message
}

// A UAT holding agent:create and project:read, no role requested: the
// defaulted role fits nothing above none, so the create is denied and
// nothing is written.
func TestUATDefaultedRoleWithNoFittingRoleDenied(t *testing.T) {
	f := newUATCreateFixture(t, "uat-none-deny")
	rec := f.create(t, f.uat(t, "agent:create", "project:read"), CreateAgentRequest{Name: "uat-none-deny"})
	assert.Equal(t, reasonNoUsableRole, assertCeilingDenial(t, rec))
	assertCreateWroteNothing(t, f.store, f.proj.ID, "uat-none-deny", f.creator.ID)
}

// The same UAT with role=none explicit: created as none with NoAuth; the
// launch request carries no LLM credentials and no project secret except
// GITHUB_TOKEN.
func TestUATExplicitRoleNoneAllowedNoAuth(t *testing.T) {
	f := newUATCreateFixture(t, "uat-none-ok")
	client := f.withDispatcher(t)
	disp := f.srv.GetDispatcher().(*HTTPAgentDispatcher)
	disp.SetSecretBackend(&mockGitTokenSecretBackend{
		secrets: []secret.SecretWithValue{
			{SecretMeta: secret.SecretMeta{Name: "CLAUDE_AUTH", SecretType: "file", Target: "~/.claude/.credentials.json"}, Value: "cred"},
			{SecretMeta: secret.SecretMeta{Name: "API_KEY", SecretType: "environment", Target: "API_KEY"}, Value: "sk-key"},
			{SecretMeta: secret.SecretMeta{Name: "PROJECT_SECRET", SecretType: "environment", Target: "PROJECT_SECRET", Scope: secret.ScopeProject}, Value: "p"},
		},
		getResponses: map[string]*secret.SecretWithValue{
			"GITHUB_TOKEN": {SecretMeta: secret.SecretMeta{Name: "GITHUB_TOKEN", SecretType: "environment", Target: "GITHUB_TOKEN", Scope: secret.ScopeProject}, Value: "ghp_x"},
		},
	})

	rec := f.create(t, f.uat(t, "agent:create", "project:read"),
		CreateAgentRequest{Name: "uat-none-ok", AgentRole: string(AgentRoleNone)})
	agent, edge := f.createdAgent(t, rec, "uat-none-ok")
	assert.Equal(t, string(AgentRoleNone), agent.AppliedConfig.AgentRole)
	assert.Equal(t, string(AgentRoleNone), edge.Role)
	assert.Equal(t, f.creator.ID, edge.DelegatorID)
	assertEdgeDelegatorIsSourcePrincipal(t, edge)
	assert.True(t, agent.AppliedConfig.NoAuth)

	req := client.lastCreateReq
	require.NotNil(t, req, "launch request sent")
	assert.True(t, req.NoAuth)
	assert.Empty(t, req.ResolvedSecrets, "no LLM credential or secret file")
	assert.Equal(t, "ghp_x", req.ResolvedEnv["GITHUB_TOKEN"])
	assert.NotContains(t, req.ResolvedEnv, "API_KEY")
	assert.NotContains(t, req.ResolvedEnv, "PROJECT_SECRET")
}

// agent:create plus the seven read selectors, project default full, no
// role requested: created as baseline; the edge role equals the stored role;
// not NoAuth.
func TestUATMinimalCeilingCapsToBaseline(t *testing.T) {
	f := newUATCreateFixture(t, "uat-baseline")
	f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleFull))
	rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-baseline"})
	agent, edge := f.createdAgent(t, rec, "uat-baseline")
	assert.Equal(t, string(AgentRoleBaseline), agent.AppliedConfig.AgentRole)
	assert.Equal(t, agent.AppliedConfig.AgentRole, edge.Role)
	assert.False(t, agent.AppliedConfig.NoAuth)
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.Equal(t, uatCeilingFromSelectors(t, minimalSelectors(t)...).PermissionIDs, edge.PermissionIDs)

	// Without the dev-auth mint override, the child's first token carries
	// exactly the baseline scopes: project:read and the four self operations.
	m := &mintFixture{srv: f.srv, store: f.store, projectID: f.proj.ID}
	f.srv.authzService.mintDevAuthOverride = false
	token, err := f.srv.issueAgentTokenForTest(context.Background(), agent)
	require.NoError(t, err)
	baseline := []AgentTokenScope{
		ScopeProjectRead, ScopeAgentStatusUpdate, ScopeAgentTokenRefresh, ScopeAgentNotify, ScopeAgentPortForward,
	}
	assert.ElementsMatch(t, baseline, m.tokenClaims(t, token).Scopes)

	// A refresh after the stored role is raised to full takes the full-role
	// candidates filtered by the frozen ceiling: baseline plus
	// project:agent:create, which the UAT's agent:create selector put in
	// the ceiling. Every refreshed scope is within the frozen ceiling: the
	// non-self scopes map into its IDs, the self operations are exempt.
	withinCeiling := append(append([]AgentTokenScope{}, baseline...), ScopeAgentCreate)
	agent.AppliedConfig.AgentRole = string(AgentRoleFull)
	require.NoError(t, f.store.UpdateAgent(context.Background(), agent))
	refreshed := m.tokenClaims(t, refreshedToken(t, m.refresh(t, agent, agent.Ancestry))).Scopes
	assert.ElementsMatch(t, withinCeiling, refreshed)
	assert.Subset(t, edge.PermissionIDs, agentScopeCoverage([]AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}))
	for _, scope := range refreshed {
		assert.True(t, ceilingAllowsScope(edge.EffectCeiling, scope), "scope %s is outside the frozen ceiling", scope)
	}
	for _, scope := range []AgentTokenScope{ScopeAgentLifecycle, ScopeProjectSecretRead, ScopeProjectTemplateWrite} {
		assert.NotContains(t, refreshed, scope)
	}

	// With the dev-auth mint override, the role is raised to full before the
	// ceiling filter, so the first mint equals the refreshed set.
	f.srv.authzService.mintDevAuthOverride = true
	agent.AppliedConfig.AgentRole = string(AgentRoleBaseline)
	require.NoError(t, f.store.UpdateAgent(context.Background(), agent))
	devToken, err := f.srv.issueAgentTokenForTest(context.Background(), agent)
	require.NoError(t, err)
	assert.ElementsMatch(t, withinCeiling, m.tokenClaims(t, devToken).Scopes)
}

// The same UAT with a readonly project default, and with a readonly project
// maximum: created as readonly; the cap does not raise it.
func TestUATMinimalCeilingKeepsReadonlyDefault(t *testing.T) {
	for _, key := range []string{projectSettingDefaultAgentRole, projectSettingMaxAgentRole} {
		t.Run(key, func(t *testing.T) {
			f := newUATCreateFixture(t, "uat-readonly")
			f.setProjectAnnotation(t, key, string(AgentRoleReadOnly))
			rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-readonly"})
			agent, edge := f.createdAgent(t, rec, "uat-readonly")
			assert.Equal(t, string(AgentRoleReadOnly), agent.AppliedConfig.AgentRole)
			assert.Equal(t, agent.AppliedConfig.AgentRole, edge.Role)
			assert.False(t, agent.AppliedConfig.NoAuth)
		})
	}
}

// The capped role replaces the effective role before the role→NoAuth
// mapping: a defaulted full capped to baseline, and a readonly default kept
// at readonly by the cap, are not NoAuth, and the stored, edge and launch
// values agree.
func TestRoleCapRunsBeforeNoAuthMapping(t *testing.T) {
	f := newUATCreateFixture(t, "uat-noauth-order")
	client := f.withDispatcher(t)

	rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-noauth-capped"})
	agent, edge := f.createdAgent(t, rec, "uat-noauth-capped")
	assert.Equal(t, string(AgentRoleBaseline), agent.AppliedConfig.AgentRole)
	assert.Equal(t, agent.AppliedConfig.AgentRole, edge.Role)
	assert.False(t, agent.AppliedConfig.NoAuth)
	require.NotNil(t, client.lastCreateReq)
	assert.False(t, client.lastCreateReq.NoAuth)

	f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleReadOnly))
	rec = f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-noauth-readonly"})
	agent, edge = f.createdAgent(t, rec, "uat-noauth-readonly")
	assert.Equal(t, string(AgentRoleReadOnly), agent.AppliedConfig.AgentRole)
	assert.Equal(t, agent.AppliedConfig.AgentRole, edge.Role)
	assert.False(t, agent.AppliedConfig.NoAuth, "a capped readonly child is not NoAuth")
	assert.False(t, client.lastCreateReq.NoAuth)
}

// An explicit role over the UAT ceiling: 403 at the delegation ceiling, no
// agent row, no edge.
func TestUATExplicitRoleOverCeilingForbidden(t *testing.T) {
	f := newUATCreateFixture(t, "uat-over")
	rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-over", AgentRole: string(AgentRoleFull)})
	assert.Contains(t, assertCeilingDenial(t, rec), `agent role "full"`)
	assertCreateWroteNothing(t, f.store, f.proj.ID, "uat-over", f.creator.ID)
}

// A UAT without gcp_service_account:assign, with the project default
// ladder selecting an assign-mode SA: 403 from evaluateSAAssignment with its
// existing body; nothing written.
func TestUATChildDefaultSAWithoutAssignDeniedAtCreate(t *testing.T) {
	f := newUATCreateFixture(t, "uat-default-sa")
	sa := bypassAgentsCreateSA(t, f.bypassAgentsFixture, f.proj.ID, true)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentityMode, store.GCPMetadataModeAssign)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentitySAID, sa.ID)

	rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-default-sa"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, saAssignGenericForbiddenMsg, apiErr.Message)
	assert.NotContains(t, apiErr.Details, "denied_by", "the SA gate's response, not the ceiling's")
	assertCreateWroteNothing(t, f.store, f.proj.ID, "uat-default-sa", f.creator.ID)
}

// A child created by a UAT without gcp_service_account:assign, then given an
// assign-mode SA by a session user who holds assign: the PATCH succeeds and
// the child's next token omits the SA's GCP token scope.
func TestUATChildPatchedSAOmitsGCPScope(t *testing.T) {
	f := newUATCreateFixture(t, "uat-patch-sa")
	rec := f.create(t, f.uat(t, minimalSelectors(t)...), CreateAgentRequest{Name: "uat-patch-sa"})
	agent, _ := f.createdAgent(t, rec, "uat-patch-sa")

	sa := bypassAgentsCreateSA(t, f.bypassAgentsFixture, f.proj.ID, true)
	f.srv.createProjectMembersGroup(context.Background(), f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{
			"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": sa.ID,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agent, err := f.store.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	gcpScope := GCPTokenScopeForSA(sa.ID)
	require.Contains(t, f.srv.authzService.mintCandidateScopes(agent), gcpScope, "the SA is a mint candidate")

	tok, err := f.srv.issueAgentTokenForTest(context.Background(), agent)
	require.NoError(t, err)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	assert.NotContains(t, claims.Scopes, gcpScope)
	assert.NotEmpty(t, claims.Scopes)
}

// A child created by the same UAT with passthrough on a cloudrun-sandbox
// broker with a host SA: translated to assign mode, and the token omits the
// host SA's GCP token scope.
func TestUATChildHostPassthroughSAOmitsGCPScope(t *testing.T) {
	hostSAEmail := "broker-host@ceiling-sandbox.iam.gserviceaccount.com"
	owner := ptUser(tid("uat-pt-owner"), "uat-pt-owner@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "ceiling-sandbox")
	// The UAT holds no broker selector; an auto-provide broker admits it.
	broker.AutoProvide = true
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))
	require.NoError(t, srv.createProjectOwnerRoleBinding(context.Background(), project.ID, owner.ID))

	c := uatCeilingFromSelectors(t, minimalSelectors(t)...)
	uat := NewScopedUserIdentityWithCeiling(authUser(owner), project.ID, minimalSelectors(t), "uat-pt",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs})
	rec := requestAsIdentity(t, srv, uat, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name: "uat-pt", GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent, err := s.GetAgentBySlug(context.Background(), project.ID, "uat-pt")
	require.NoError(t, err)
	gcpID := agent.AppliedConfig.GCPIdentity
	require.NotNil(t, gcpID)
	assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode)
	assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
	gcpScope := GCPTokenScopeForSA(gcpID.ServiceAccountID)
	require.Contains(t, srv.authzService.mintCandidateScopes(agent), gcpScope)

	tok, err := srv.issueAgentTokenForTest(context.Background(), agent)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	assert.NotContains(t, claims.Scopes, gcpScope)
}

// A session-created child: role, scopes and walk as for a child with no
// frozen ceiling.
func TestSessionChildGetsPrincipalCeiling(t *testing.T) {
	f := newUATCreateFixture(t, "session-child")
	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "session-child"})
	agent, edge := f.createdAgent(t, rec, "session-child")
	assert.Equal(t, string(AgentRoleFull), agent.AppliedConfig.AgentRole)
	assert.False(t, agent.AppliedConfig.NoAuth)
	assert.Equal(t, store.EffectCeilingPrincipal, edge.Kind)
	assert.Nil(t, edge.PermissionIDs)
	assert.Equal(t, store.SourceCredentialSession, edge.SourceCredentialKind)
	assert.Equal(t, f.creator.ID, edge.DelegatorID)

	tok, err := f.srv.issueAgentTokenForTest(context.Background(), agent)
	require.NoError(t, err)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	assert.ElementsMatch(t, ScopesForRole(AgentRoleFull), claims.Scopes, "the role's scopes, unfiltered")

	var cause DenyCause
	allowed, reason, err := f.srv.authzService.walkDelegationChainWithCause(context.Background(),
		Resource{Type: "agent", ParentType: "project", ParentID: f.proj.ID}, ActionCreate, "agent.create",
		agent.ID, true, store.RoleScopeProject, f.proj.ID, nil, &cause)
	require.NoError(t, err)
	assert.True(t, allowed, reason)
	assert.Empty(t, cause)
}

// A UAT whose ceiling version is not one this binary interprets: the
// source ceiling is ceiling_unrecorded, and the create is 403 with nothing
// written. Through the handler the UAT scope gate denies first, because the
// frozen ceiling allows no permission at an unknown version.
func TestUATUnknownCeilingVersionDeniesCreate(t *testing.T) {
	f := newUATCreateFixture(t, "uat-v99")
	uat := NewScopedUserIdentityWithCeiling(authUser(f.creator), f.proj.ID, minimalSelectors(t), "uat-v99",
		permissions.FrozenPermissionCeiling{Version: 99, PermissionIDs: uatCeilingFromSelectors(t, minimalSelectors(t)...).PermissionIDs})
	_, _, err := f.srv.authzService.sourceEffectCeiling(context.Background(), uat)
	cause, structural := ceilingDenyCauseForError(err)
	require.True(t, structural)
	assert.Equal(t, DenyCauseCeilingUnrecorded, cause)

	rec := f.create(t, uat, CreateAgentRequest{Name: "uat-v99"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertCreateWroteNothing(t, f.store, f.proj.ID, "uat-v99", f.creator.ID)
}

// A UAT with an unspecified ceiling version and backfill-normalized IDs
// creates a child; the edge is bounded with the same version and IDs.
func TestLegacyBackfilledUATCreatesBoundedChild(t *testing.T) {
	f := newUATCreateFixture(t, "uat-legacy")
	selectors := minimalSelectors(t)
	uat := NewScopedUserIdentity(authUser(f.creator), f.proj.ID, selectors)
	require.Equal(t, permissions.CeilingVersionUnspecified, uat.Ceiling().Version)

	rec := f.create(t, uat, CreateAgentRequest{Name: "uat-legacy"})
	_, edge := f.createdAgent(t, rec, "uat-legacy")
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.Equal(t, permissions.CeilingVersionUnspecified, edge.Version)
	assert.Equal(t, permissions.NormalizeLegacyUATScopes(selectors), edge.PermissionIDs)
	assert.Equal(t, store.SourceCredentialUAT, edge.SourceCredentialKind)
}

// fakeIdentity is an identity type the hub does not accept as an authority
// source.
type fakeIdentity struct {
	id, typ string
}

func (f fakeIdentity) ID() string   { return f.id }
func (f fakeIdentity) Type() string { return f.typ }

// An identity reporting Type()=="dev" that is not the concrete *DevUser with
// DevUserID is not an authority source.
func TestDevTypeNotRecognizedDeniesCreate(t *testing.T) {
	f := newUATCreateFixture(t, "dev-type")
	for name, id := range map[string]Identity{
		"dev type string":        fakeIdentity{id: DevUserID, typ: "dev"},
		"dev user with other id": &DevUser{id: f.creator.ID},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, "dev", id.Type())
			_, _, err := f.srv.authzService.sourceEffectCeiling(context.Background(), id)
			assert.ErrorIs(t, err, errSourceNotAllowed)
			cause, structural := ceilingDenyCauseForError(err)
			assert.True(t, structural)
			assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)
		})
	}

	// Through the handler: the dev-typed DevUser passes the earlier gates as
	// a user and is denied at the ceiling.
	rec := f.create(t, &DevUser{id: f.creator.ID}, CreateAgentRequest{Name: "dev-type"})
	assert.Equal(t, ceilingSourceDenialMessage(DenyCauseCeilingSourceNotAllowed), assertCeilingDenial(t, rec))
	assertCreateWroteNothing(t, f.store, f.proj.ID, "dev-type", f.creator.ID)
}

// Identity kinds that are not authority sources: sourceEffectCeiling
// returns errSourceNotAllowed (403 ceiling_source_not_allowed), and the
// create handler writes nothing.
func TestUnknownIdentityKindDeniesCreate(t *testing.T) {
	f := newUATCreateFixture(t, "unknown-kind")
	var nilUser *AuthenticatedUser
	cases := map[string]Identity{
		"broker":            NewBrokerIdentity(f.broker.ID),
		"hub_delivery":      fakeIdentity{id: "delivery", typ: "hub_delivery"},
		"federated_user":    NewFederatedUserIdentity("https://issuer", "sub", "u@x", "U", "member", nil),
		"federated_agent":   NewFederatedAgentIdentity("https://issuer", "a", f.proj.ID, "a", "root", nil, nil),
		"federated_service": NewFederatedServiceIdentity("https://issuer", "svc", "svc@x", nil),
		"future type":       fakeIdentity{id: "x", typ: "future_kind"},
		"empty type":        fakeIdentity{id: "x"},
		"nil":               nil,
		"stored agent":      &storedAgentIdentity{agent: f.caller},
		"peer agent":        &peerAgentIdentity{agent: f.caller},
		"explain agent":     &explainAgentIdentity{id: f.caller.ID, projectID: f.proj.ID},
		"wrapper nil claim": &agentIdentityWrapper{},
		"typed-nil pointer": nilUser,
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.srv.authzService.sourceEffectCeiling(context.Background(), id)
			require.ErrorIs(t, err, errSourceNotAllowed)
			cause, structural := ceilingDenyCauseForError(err)
			assert.True(t, structural)
			assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)

			if id == nil || name == "typed-nil pointer" || name == "wrapper nil claim" {
				return // authentication never places these on a request
			}
			slug := "unknown-" + tid(name)[:8]
			rec := f.create(t, id, CreateAgentRequest{Name: slug})
			assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
			assert.Less(t, rec.Code, 500, rec.Body.String())
			_, err = f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
			assert.ErrorIs(t, err, store.ErrNotFound, "nothing written")
		})
	}
	agents, err := f.store.ListAgents(context.Background(), store.AgentFilter{ProjectID: f.proj.ID}, store.ListOptions{})
	require.NoError(t, err)
	for _, a := range agents.Items {
		assert.Empty(t, activeEdgesFor(t, f.store, a.ID), "no edge written for %s", a.Slug)
	}
}

// Every delegation-ceiling DenyCause reaches the client as a 403 carrying
// details.denied_by="delegation_ceiling", at create and at mint.
// ceiling_unrecorded is reached at mint: at create the UAT scope gate
// denies an unknown ceiling version first. ceiling_resource_missing is
// reserved for the service-account parent-ceiling evaluator and has no
// emitter in this package.
func TestAllCeilingDenyCausesCarryDeniedBy(t *testing.T) {
	f := newUATCreateFixture(t, "denied-by")
	cases := []struct {
		cause    DenyCause
		identity Identity
		req      CreateAgentRequest
	}{
		{DenyCauseCeilingEffectExceeded, f.uat(t, "agent:create", "project:read"), CreateAgentRequest{Name: "db-exceeded"}},
		{DenyCauseCeilingSourceNotAllowed, &DevUser{id: f.creator.ID}, CreateAgentRequest{Name: "db-source"}},
	}
	for _, tc := range cases {
		t.Run("create "+string(tc.cause), func(t *testing.T) {
			assertCeilingDenial(t, f.create(t, tc.identity, tc.req))
		})
	}

	// Orphaned (and the other structural chain causes) at a mint site.
	for _, cause := range []DenyCause{DenyCauseCeilingOrphaned, DenyCauseCeilingSourceNotAllowed, DenyCauseCeilingUnrecorded} {
		t.Run("mint "+string(cause), func(t *testing.T) {
			rec := httptest.NewRecorder()
			require.True(t, writeAgentTokenIssueError(rec, &agentTokenIssueError{Site: mintSiteStart, Cause: cause}))
			assertCeilingDenial(t, rec)
		})
	}

	// Decide sets Decision.DeniedBy for a ceiling deny: see
	// TestWalkDeniesPermissionOutsideHopCeiling and assertUnrecordedDeny.
}
