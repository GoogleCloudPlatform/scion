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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1916: enforce the user/project template read boundary. Mirrors
// skill_scope_authz_test.go (ptone/scion#1901) for the "template" resource
// type — see filterHubWideTemplateGrants (authz_template_scope.go) for the
// fix this suite regresses against.
// ============================================================================

// ----------------------------------------------------------------------
// User-scoped templates: readable only by the owning user and hub admins.
// ----------------------------------------------------------------------

func TestTemplateScope_UserScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-user-template", store.TemplateScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "owner should read their own user-scoped template; got: %s", rec.Body.String())
}

// TestTemplateScope_UserScoped_OtherHubMemberDenied is the ptone/scion#1916
// regression test: carol is a hub member (so the pre-fix hub-member-read-all
// grant applied to her) but is not alice's user-scoped template's owner. She
// must be denied — 404, not 403, so existence is not leaked.
func TestTemplateScope_UserScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-private-user-template", store.TemplateScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not the owner must not read another user's user-scoped template; got: %s", rec.Body.String())
}

func TestTemplateScope_UserScoped_SuperAdminAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-user-template-admin", store.TemplateScopeUser, alice.ID, alice.ID)
	admin := createScopeSuperAdmin(t, s, "tplscope-admin-user")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"super-admins must retain read access to user-scoped templates; got: %s", rec.Body.String())
}

func TestTemplateScope_UserScoped_DownloadDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-download-user-template", store.TemplateScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/download", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"download-URL issuance for another user's user-scoped template must be denied; got: %s", rec.Body.String())
}

func TestTemplateScope_UserScoped_ValidateDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-validate-user-template", store.TemplateScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/validate", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"validation of another user's user-scoped template must be denied; got: %s", rec.Body.String())
}

func TestTemplateScope_UserScoped_FilesDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-files-user-template", store.TemplateScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/files", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"file listing for another user's user-scoped template must be denied; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Project-scoped templates: readable only by project members (and admins).
// ----------------------------------------------------------------------

func TestTemplateScope_ProjectScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-project-template-carol", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not a project member must not read the project's template; got: %s", rec.Body.String())
}

func TestTemplateScope_ProjectScoped_ProjectMemberAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-project-template-member", store.TemplateScopeProject, project.ID, alice.ID)

	dave := createNamedTestUser(t, s, "tplscope-dave", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, dave, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a project member must still read the project's template after the scope fix; got: %s", rec.Body.String())
}

func TestTemplateScope_ProjectScoped_SuperAdminAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-project-template-admin", store.TemplateScopeProject, project.ID, alice.ID)
	admin := createScopeSuperAdmin(t, s, "tplscope-admin-project")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"super-admins must retain read access to project-scoped templates; got: %s", rec.Body.String())
}

func TestTemplateScope_ProjectScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-project-template-owner", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the project owner should read the project's own template; got: %s", rec.Body.String())
}

// TestTemplateScope_ProjectScoped_ListFiltersOtherHubMember is the
// ptone/scion#1916 regression test for the list surface: before the fix, a
// hub member's curated grant made hasCatalogWideListAccess return true for
// ANY scope/scopeId query, so an explicit ?scope=project&scopeId=<project>
// bypassed membership checking entirely (see filterHubWideTemplateGrants —
// the same fix that closes get/download/validate/files also removes the
// synthetic "hub" resource's wide-access eligibility for curated roles).
func TestTemplateScope_ProjectScoped_ListFiltersOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupTemplateScopeTest(t)
	createAuthzTestTemplate(t, s, "alice-project-list-template", store.TemplateScopeProject, project.ID, alice.ID)

	recAlice := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recAlice.Code)
	var aliceResp ListTemplatesResponse
	require.NoError(t, json.NewDecoder(recAlice.Body).Decode(&aliceResp))
	assert.NotEmpty(t, aliceResp.Templates, "the project owner should see the project's own templates")

	recCarol := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recCarol.Code)
	var carolResp ListTemplatesResponse
	require.NoError(t, json.NewDecoder(recCarol.Body).Decode(&carolResp))
	assert.Empty(t, carolResp.Templates, "a non-member hub member must not see the project's templates in a list")
}

// ----------------------------------------------------------------------
// Hub-scoped (global) templates: still readable by every hub member. This
// is the control that proves the fix narrows, rather than removes, the
// hub-member/hub-viewer grant.
// ----------------------------------------------------------------------

func TestTemplateScope_GlobalScoped_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, _ := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "global-catalog-template", store.TemplateScopeGlobal, "", alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/templates/"+tpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"every hub member must still read hub-scoped (global) templates; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Agent tokens: an agent reads with its own project's scope grants, not
// beyond it. This must not regress: the scope fix only narrows the curated
// hub-member/hub-viewer grant, and must not accidentally widen or narrow an
// agent's own project-scoped grant.
// ----------------------------------------------------------------------

func TestTemplateScope_Agent_OutsideProjectDeniedOnUserScopedTemplate(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	owner := createNamedTestUser(t, s, "tplscope-agent-owner", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	tpl := createAuthzTestTemplate(t, s, "agent-outside-user-template", store.TemplateScopeUser, owner.ID, owner.ID)

	otherProject := &store.Project{ID: tid("tplscope-agent-project"), Name: "Agent Project", Slug: "tplscope-agent-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	agent := &store.Agent{
		ID: tid("tplscope-agent"), Slug: tid("tplscope-agent"), Name: "Scope Agent",
		ProjectID: otherProject.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: otherProject.ID,
	}}

	decision := authz.CheckAccess(ctx, identity, templateResource(tpl), ActionRead)
	assert.False(t, decision.Allowed,
		"an agent outside the owner's scope must not read the owner's user-scoped template")
}

func TestTemplateScope_Agent_SameProjectAllowedOnProjectScopedTemplate(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	owner := createNamedTestUser(t, s, "tplscope-agent-sp-owner", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	project := &store.Project{ID: tid("tplscope-agent-sp-project"), Name: "Agent SP Project", Slug: "tplscope-agent-sp-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	tpl := createAuthzTestTemplate(t, s, "agent-same-project-template", store.TemplateScopeProject, project.ID, owner.ID)

	agent := &store.Agent{
		ID: tid("tplscope-agent-sp"), Slug: tid("tplscope-agent-sp"), Name: "Scope Agent SP",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}

	decision := authz.CheckAccess(ctx, identity, templateResource(tpl), ActionRead)
	assert.True(t, decision.Allowed,
		"an agent in the same project as a project-scoped template must still be able to read it")
}

// ----------------------------------------------------------------------
// Resolve-at-agent-create (ptone/scion#1916): resolveTemplate's by-ID arm
// looks up a template across every scope with no authorization at all —
// that part is deliberately unchanged (the CLI resolves names to IDs, so
// the lookup itself must stay scope-agnostic). The fix is
// authorizeResolvedTemplate (handlers_agent_create_helpers.go), the new
// gate createAgentInProject runs on the resolved candidate before applying
// its config to the new agent. These tests exercise that gate directly,
// the same way TestTemplateScope_Agent_* exercise CheckAccess directly,
// to avoid the unrelated runtime-broker-dispatch permission machinery a
// full HTTP round trip through POST /api/v1/agents would also need.
// ----------------------------------------------------------------------

func TestTemplateScope_ResolveAtCreate_CrossProjectTemplateDenied(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-project-template", store.TemplateScopeProject, project.ID, alice.ID)

	otherProject := &store.Project{ID: tid("tplresolve-other-project"), Name: "Other Project", Slug: "tplresolve-other-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	bob := createNamedTestUser(t, s, "tplresolve-bob", store.UserRoleMember)
	ensureHubMembership(ctx, s, bob.ID)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, otherProject.ID, store.ProjectRoleMember)
	bobIdentity := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "cli")

	// resolveTemplate's by-ID arm resolves across every scope, exactly as it
	// would from bob's agent-create request referencing alice's project's
	// template by UUID.
	resolved, err := srv.resolveTemplate(ctx, tpl.ID, otherProject.ID, bob.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved, "resolveTemplate's by-ID lookup is scope-agnostic by design")

	allowed := srv.authorizeResolvedTemplate(ctx, bobIdentity, resolved)
	assert.False(t, allowed,
		"a project member from another project must not have another project's private template applied to their new agent")
}

// TestTemplateScope_ResolveAtCreate_CrossProjectTemplateDenied_HTTPCreate is
// the ptone/scion#1936 gemini-review regression for the HTTP create path
// (handlers_agents_core.go): denying a resolved template must read as 404
// and must not create the agent, whether with the private template silently
// dropped or with a nil-pointer panic.
func TestTemplateScope_ResolveAtCreate_CrossProjectTemplateDenied_HTTPCreate(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-http-create-template", store.TemplateScopeProject, project.ID, alice.ID)

	otherProject := &store.Project{ID: tid("tplresolve-http-other-project"), Name: "Other Project", Slug: "tplresolve-http-other-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	broker := &store.RuntimeBroker{ID: tid("tplresolve-http-broker"), Name: "broker", Slug: "tplresolve-http-broker", Status: store.BrokerStatusOnline, AutoProvide: true}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: otherProject.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))
	otherProject.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, otherProject))
	srv.SetDispatcher(&createAgentDispatcher{createPhase: string(state.PhaseRunning)})

	bob := createNamedTestUser(t, s, "tplresolve-http-bob", store.UserRoleMember)
	ensureHubMembership(ctx, s, bob.ID)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, otherProject.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "cross-project-template-agent",
		ProjectID: otherProject.ID,
		Template:  tpl.ID,
	})
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a denied cross-project template resolved at create must read as not-found; got: %s", rec.Body.String())

	_, err := s.GetAgentBySlug(ctx, otherProject.ID, "cross-project-template-agent")
	assert.ErrorIs(t, err, store.ErrNotFound, "an agent must not be created when its requested template is denied")
}

func TestTemplateScope_ResolveAtCreate_OwnProjectTemplateAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-own-template", store.TemplateScopeProject, project.ID, alice.ID)

	dave := createNamedTestUser(t, s, "tplresolve-dave", store.UserRoleMember)
	ensureHubMembership(ctx, s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)
	daveIdentity := NewAuthenticatedUser(dave.ID, dave.Email, dave.DisplayName, dave.Role, "cli")

	resolved, err := srv.resolveTemplate(ctx, tpl.ID, project.ID, dave.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved)

	allowed := srv.authorizeResolvedTemplate(ctx, daveIdentity, resolved)
	assert.True(t, allowed,
		"a fellow project member creating an agent with the project's own template must still be allowed")
}

// TestTemplateScope_ResolveAtCreate_NilIdentityDenied is the ptone/scion#1936
// gemini-review regression: a nil identity must fail closed, not fail open,
// for a non-global resolved template. Neither of authorizeResolvedTemplate's
// two callers can currently reach it with a nil identity (createAgentInProject
// runs behind authorizeAgentCreate, which rejects a nil identity outright, and
// the scheduler's creatorIdentity is resolved before dispatch ever calls this),
// so this pins the function's own contract directly rather than a live bypass.
func TestTemplateScope_ResolveAtCreate_NilIdentityDenied(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-nil-identity-template", store.TemplateScopeProject, project.ID, alice.ID)

	allowed := srv.authorizeResolvedTemplate(ctx, nil, tpl)
	assert.False(t, allowed, "a nil identity must not be treated as authorized to read a non-global resolved template")
}

// TestTemplateScope_ResolveAtCreate_BrokerIdentityExempt is the positive
// case: a broker registered as a provider for the template's own project may
// hydrate it. Broker admission is scoped by brokerMayReadCatalogResource
// (authorize.go), not an unconditional exemption — see the companion
// "NonProviderDenied" test below.
func TestTemplateScope_ResolveAtCreate_BrokerIdentityExempt(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-broker-template", store.TemplateScopeProject, project.ID, alice.ID)

	broker := &store.RuntimeBroker{ID: tid("tplresolve-provider-broker"), Name: "provider-broker", Slug: "tplresolve-provider-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))

	brokerCtx := contextWithBrokerIdentity(ctx, NewBrokerIdentity(broker.ID))
	allowed := srv.authorizeResolvedTemplate(brokerCtx, NewAuthenticatedUser("irrelevant", "", "", "", "cli"), tpl)
	assert.True(t, allowed, "a runtime broker registered as a provider for the template's project must be able to hydrate it")
}

// TestTemplateScope_ResolveAtCreate_BrokerIdentityNonProviderDenied is the
// ptone/scion#1916 follow-up regression: a broker that has never been
// registered as a provider for the template's project must not hydrate it
// merely by being an authenticated broker.
func TestTemplateScope_ResolveAtCreate_BrokerIdentityNonProviderDenied(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-nonprovider-broker-template", store.TemplateScopeProject, project.ID, alice.ID)

	broker := &store.RuntimeBroker{ID: tid("tplresolve-stranger-broker"), Name: "stranger-broker", Slug: "tplresolve-stranger-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	// Deliberately no AddProjectProvider call: this broker serves no project.

	brokerCtx := contextWithBrokerIdentity(ctx, NewBrokerIdentity(broker.ID))
	allowed := srv.authorizeResolvedTemplate(brokerCtx, NewAuthenticatedUser("irrelevant", "", "", "", "cli"), tpl)
	assert.False(t, allowed, "a broker that is not a registered provider for the template's project must not hydrate it")
}

// ----------------------------------------------------------------------
// Clone (C5): reading the clone source is a read like any other. A forbidden
// source must 404, exactly like a nonexistent one, not 403 (which would
// confirm the source exists).
// ----------------------------------------------------------------------

func TestTemplateScope_Clone_ForbiddenSourceIsNotFound(t *testing.T) {
	srv, s, alice, carol, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-clone-source-private", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{
		Name: "carols-clone-attempt",
	})
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"cloning a template carol may not read must 404, not 403 or 201; got: %s", rec.Body.String())
}

// TestTemplateScope_Clone_ForbiddenAndMissingSourceUseIdenticalMessage proves
// the genuinely-missing and denied-read cases are indistinguishable by
// message text, not merely by status code.
func TestTemplateScope_Clone_ForbiddenAndMissingSourceUseIdenticalMessage(t *testing.T) {
	srv, s, alice, carol, project := setupTemplateScopeTest(t)
	tpl := createAuthzTestTemplate(t, s, "alice-clone-source-private-2", store.TemplateScopeProject, project.ID, alice.ID)

	forbiddenRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/clone", CloneTemplateRequest{Name: "carols-clone-attempt-3"})
	require.Equal(t, http.StatusNotFound, forbiddenRec.Code, "got: %s", forbiddenRec.Body.String())
	var forbidden ErrorResponse
	require.NoError(t, json.Unmarshal(forbiddenRec.Body.Bytes(), &forbidden))

	missingRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/templates/"+tid("tpl-clone-does-not-exist")+"/clone", CloneTemplateRequest{Name: "carols-clone-attempt-4"})
	require.Equal(t, http.StatusNotFound, missingRec.Code, "got: %s", missingRec.Body.String())
	var missing ErrorResponse
	require.NoError(t, json.Unmarshal(missingRec.Body.Bytes(), &missing))

	assert.Equal(t, "Template not found", forbidden.Error.Message)
	assert.Equal(t, forbidden.Error.Message, missing.Error.Message,
		"a forbidden clone source and a nonexistent one must read identically")
}

// ----------------------------------------------------------------------
// ptone/scion#5776: Hub template slug resolution must follow the documented
// order project -> user -> global (matching the CLI's findTemplateOnHub and
// .design/hosted/hosted-templates.md §3.2).
// ----------------------------------------------------------------------

func createHydratableScopeTemplate(t *testing.T, s store.Store, name, scope, scopeID, ownerID, hash string) *store.Template {
	t.Helper()
	tpl := &store.Template{
		ID:          tid("tplres-" + scope + "-" + scopeID + "-" + name),
		Name:        name,
		Slug:        name,
		Harness:     "claude",
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      store.TemplateStatusActive,
		ContentHash: hash,
		Files:       []store.TemplateFile{{Path: "scion-agent.yaml", Hash: hash, Size: 16}},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tpl))
	return tpl
}

func TestTemplateResolutionOrder_HTTPCreate_ProjectUserGlobal(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()

	// Create a second project that has no project-scoped "shared-tpl" so we can
	// exercise user vs global precedence alongside project vs user vs global.
	projectNoTpl := &store.Project{ID: tid("tplres-http-no-proj-tpl"), Name: "No Project Template", Slug: "tplres-http-no-proj-tpl"}
	require.NoError(t, s.CreateProject(ctx, projectNoTpl))
	broker := &store.RuntimeBroker{ID: tid("tplres-http-broker"), Name: "broker", Slug: "tplres-http-broker", Status: store.BrokerStatusOnline, AutoProvide: true}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	for _, p := range []*store.Project{project, projectNoTpl} {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: p.ID, BrokerID: broker.ID, BrokerName: broker.Name, LocalPath: t.TempDir(), Status: store.BrokerStatusOnline}))
		p.DefaultRuntimeBrokerID = broker.ID
		require.NoError(t, s.UpdateProject(ctx, p))
	}
	srv.SetDispatcher(&createAgentDispatcher{createPhase: string(state.PhaseRunning)})

	// Alice is a member of both projects; bob is a member of projectNoTpl only.
	createTestUserWithProjectRole(t, s, alice.ID, alice.Email, projectNoTpl.ID, store.ProjectRoleMember)
	bob := createNamedTestUser(t, s, "tplres-http-bob", store.UserRoleMember)
	ensureHubMembership(ctx, s, bob.ID)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, projectNoTpl.ID, store.ProjectRoleMember)

	projTpl := createHydratableScopeTemplate(t, s, "shared-tpl", store.TemplateScopeProject, project.ID, alice.ID, "sha256:proj")
	userTpl := createHydratableScopeTemplate(t, s, "shared-tpl", store.TemplateScopeUser, alice.ID, alice.ID, "sha256:user")
	globalTpl := createHydratableScopeTemplate(t, s, "shared-tpl", store.TemplateScopeGlobal, "", alice.ID, "sha256:global")

	// 1. All three scopes present: project beats user and global.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "tplres-agent-proj",
		ProjectID: project.ID,
		Template:  "shared-tpl",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "project-scoped create failed: %s", rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, projTpl.ID, resp.Agent.AppliedConfig.TemplateID, "project template must take precedence over user and global")

	// 2. No project template, user + global present: alice gets her user template.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "tplres-agent-user",
		ProjectID: projectNoTpl.ID,
		Template:  "shared-tpl",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "user-scoped create failed: %s", rec.Body.String())
	resp = CreateAgentResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, userTpl.ID, resp.Agent.AppliedConfig.TemplateID, "alice's user template must take precedence over global")

	// 3. Another user (bob) has no user template: falls through to global, never sees alice's user template.
	rec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "tplres-agent-global",
		ProjectID: projectNoTpl.ID,
		Template:  "shared-tpl",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "global-scoped create failed: %s", rec.Body.String())
	resp = CreateAgentResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, globalTpl.ID, resp.Agent.AppliedConfig.TemplateID, "bob must fall through to global template and not see alice's user template")
}

func TestTemplateResolutionOrder_ResolveTemplateForIdentity(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()

	otherProject := &store.Project{ID: tid("tplres-unit-other-proj"), Name: "Other Project", Slug: "tplres-unit-other-proj"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	createTestUserWithProjectRole(t, s, alice.ID, alice.Email, otherProject.ID, store.ProjectRoleMember)

	bob := createNamedTestUser(t, s, "tplres-unit-bob", store.UserRoleMember)
	ensureHubMembership(ctx, s, bob.ID)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, otherProject.ID, store.ProjectRoleMember)

	aliceIdent := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, "cli")
	bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "cli")
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("tplres-unit-agent")},
		ProjectID: otherProject.ID,
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}
	// Project-bounded UAT for alice: cannot read user-scoped templates (which
	// require a hub boundary), so slug resolution must skip user scope and
	// fall through to global (matching CLI scopeNotVisible).
	aliceProjectUAT := NewScopedUserIdentity(aliceIdent, otherProject.ID, []string{"agent:create", "template:read"})

	projTpl := createAuthzTestTemplate(t, s, "shared-tpl", store.TemplateScopeProject, project.ID, alice.ID)
	userTpl := createAuthzTestTemplate(t, s, "shared-tpl", store.TemplateScopeUser, alice.ID, alice.ID)
	globalTpl := createAuthzTestTemplate(t, s, "shared-tpl", store.TemplateScopeGlobal, "", alice.ID)

	t.Run("project beats user and global", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, aliceIdent, "shared-tpl", project.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, projTpl.ID, got.ID)
	})

	t.Run("user beats global when no project template exists", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, aliceIdent, "shared-tpl", otherProject.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, userTpl.ID, got.ID)
	})

	t.Run("other user falls through to global", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, bobIdent, "shared-tpl", otherProject.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, globalTpl.ID, got.ID)
	})

	t.Run("agent identity skips user scope and falls through to global", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, agentIdent, "shared-tpl", otherProject.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, globalTpl.ID, got.ID)
	})

	t.Run("project-bounded UAT skips unreadable user scope and falls through to global", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, aliceProjectUAT, "shared-tpl", otherProject.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, globalTpl.ID, got.ID)
	})

	t.Run("explicit ID does not fall back to another scope when unreadable", func(t *testing.T) {
		got, err := srv.resolveTemplateForIdentity(ctx, bobIdent, userTpl.ID, otherProject.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, userTpl.ID, got.ID)
		assert.False(t, srv.authorizeResolvedTemplate(ctx, bobIdent, got),
			"caller authorization gate must reject another user's template ID")
	})

	t.Run("empty project and user scopes resolve global only", func(t *testing.T) {
		got, err := srv.resolveTemplate(ctx, "shared-tpl", "", "")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, globalTpl.ID, got.ID)
	})
}
