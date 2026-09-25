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

// setupTemplateScopeTest builds on setupTemplateAuthzTest, adding carol: a
// hub member who is neither the resource owner nor a member of alice's
// project. Carol is the principal the pre-fix bug affected — unlike bob (not
// a hub member at all), carol's denial can only come from the scope boundary
// itself, not from missing hub membership.
func setupTemplateScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s, alice, _, project = setupTemplateAuthzTest(t)
	carol = createNamedTestUser(t, s, "tplscope-carol", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, carol.ID)
	return srv, s, alice, carol, project
}

// createScopeSuperAdmin creates a user with an explicit super-admin role
// binding. Unlike hub-admin, super-admin's permission set (allPermissionIDs,
// seed.go) is not curated per resource type — it is the only built-in role
// guaranteed to include template.read/template.list and
// harness_config.read/harness_config.list today, so it is the elevated-role
// control for this suite (hub-admin's curated permission list, unlike
// skill.read, was never extended to templates or harness configs — a
// separate, pre-existing product decision, not something this fix changes).
func createScopeSuperAdmin(t *testing.T, s store.Store, namePrefix string) *store.User {
	t.Helper()
	id := tid(namePrefix)
	email := namePrefix + "@test.com"
	// createTestUserWithRole (authz_candelegate_test.go) creates the user
	// record itself and binds the role; it does not return the user.
	createTestUserWithRole(t, s, id, email, store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	return &store.User{ID: id, Email: email, DisplayName: email, Role: store.UserRoleAdmin, Status: "active"}
}

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
	resolved, err := srv.resolveTemplate(ctx, tpl.ID, otherProject.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved, "resolveTemplate's by-ID lookup is scope-agnostic by design")

	allowed := srv.authorizeResolvedTemplate(ctx, bobIdentity, resolved)
	assert.False(t, allowed,
		"a project member from another project must not have another project's private template applied to their new agent")
}

func TestTemplateScope_ResolveAtCreate_OwnProjectTemplateAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-own-template", store.TemplateScopeProject, project.ID, alice.ID)

	dave := createNamedTestUser(t, s, "tplresolve-dave", store.UserRoleMember)
	ensureHubMembership(ctx, s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)
	daveIdentity := NewAuthenticatedUser(dave.ID, dave.Email, dave.DisplayName, dave.Role, "cli")

	resolved, err := srv.resolveTemplate(ctx, tpl.ID, project.ID)
	require.NoError(t, err)
	require.NotNil(t, resolved)

	allowed := srv.authorizeResolvedTemplate(ctx, daveIdentity, resolved)
	assert.True(t, allowed,
		"a fellow project member creating an agent with the project's own template must still be allowed")
}

func TestTemplateScope_ResolveAtCreate_BrokerIdentityExempt(t *testing.T) {
	srv, s, alice, _, project := setupTemplateScopeTest(t)
	ctx := context.Background()
	tpl := createAuthzTestTemplate(t, s, "alice-resolve-broker-template", store.TemplateScopeProject, project.ID, alice.ID)

	brokerCtx := contextWithBrokerIdentity(ctx, NewBrokerIdentity("broker-1"))
	allowed := srv.authorizeResolvedTemplate(brokerCtx, NewAuthenticatedUser("irrelevant", "", "", "", "cli"), tpl)
	assert.True(t, allowed, "a runtime broker hydrating a template during agent creation must remain exempt")
}
