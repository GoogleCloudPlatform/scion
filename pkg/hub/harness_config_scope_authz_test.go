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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1916: enforce the user/project harness-config read boundary.
// Mirrors template_scope_authz_test.go and skill_scope_authz_test.go
// (ptone/scion#1901) for the "harness_config" resource type — see
// filterHubWideHarnessConfigGrants (authz_harness_config_scope.go) for the
// fix this suite regresses against.
// ============================================================================

// setupHarnessConfigScopeTest creates a test server with alice (hub member,
// project owner) and carol (hub member, neither owner nor project member —
// the principal the pre-fix bug affected).
func setupHarnessConfigScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID: tid("hcscope-alice"), Email: "hcscope-alice@test.com", DisplayName: "Alice",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	carol = createNamedTestUser(t, s, "hcscope-carol", store.UserRoleMember)
	ensureHubMembership(ctx, s, carol.ID)

	project = &store.Project{
		ID: tid("hcscope-project"), Name: "Harness Config Project", Slug: "hcscope-project",
		OwnerID: alice.ID, CreatedBy: alice.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	return srv, s, alice, carol, project
}

// createAuthzTestHarnessConfig inserts a scope-parameterized harness config
// directly into the store, mirroring createAuthzTestTemplate.
func createAuthzTestHarnessConfig(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Harness:     "claude",
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      store.HarnessConfigStatusActive,
		StoragePath: fmt.Sprintf("harness-configs/%s/%s", scope, api.Slugify(name)),
		Config:      &store.HarnessConfigData{Image: "example/claude:latest"},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// ----------------------------------------------------------------------
// User-scoped harness configs: readable only by the owning user and hub
// admins.
// ----------------------------------------------------------------------

func TestHarnessConfigScope_UserScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-user-hc", store.HarnessConfigScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "owner should read their own user-scoped harness config; got: %s", rec.Body.String())
}

// TestHarnessConfigScope_UserScoped_OtherHubMemberDenied is the
// ptone/scion#1916 regression test: carol is a hub member (so the pre-fix
// hub-member-read-all grant applied to her) but is not alice's user-scoped
// harness config's owner. She must be denied — 404, not 403.
func TestHarnessConfigScope_UserScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-private-user-hc", store.HarnessConfigScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not the owner must not read another user's user-scoped harness config; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_UserScoped_SuperAdminAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-user-hc-admin", store.HarnessConfigScopeUser, alice.ID, alice.ID)
	admin := createScopeSuperAdmin(t, s, "hcscope-admin-user")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"super-admins must retain read access to user-scoped harness configs; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_UserScoped_DownloadDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-download-user-hc", store.HarnessConfigScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/download", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"download-URL issuance for another user's user-scoped harness config must be denied; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_UserScoped_ValidateDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-validate-user-hc", store.HarnessConfigScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/validate", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"validation of another user's user-scoped harness config must be denied; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_UserScoped_FilesDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-files-user-hc", store.HarnessConfigScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/files", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"file listing for another user's user-scoped harness config must be denied; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Project-scoped harness configs: readable only by project members (and
// admins).
// ----------------------------------------------------------------------

func TestHarnessConfigScope_ProjectScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, project := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-project-hc-carol", store.HarnessConfigScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not a project member must not read the project's harness config; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_ProjectScoped_ProjectMemberAllowed(t *testing.T) {
	srv, s, alice, _, project := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-project-hc-member", store.HarnessConfigScopeProject, project.ID, alice.ID)

	dave := createNamedTestUser(t, s, "hcscope-dave", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, dave, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a project member must still read the project's harness config after the scope fix; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_ProjectScoped_SuperAdminAllowed(t *testing.T) {
	srv, s, alice, _, project := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-project-hc-admin", store.HarnessConfigScopeProject, project.ID, alice.ID)
	admin := createScopeSuperAdmin(t, s, "hcscope-admin-project")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"super-admins must retain read access to project-scoped harness configs; got: %s", rec.Body.String())
}

func TestHarnessConfigScope_ProjectScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-project-hc-owner", store.HarnessConfigScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the project owner should read the project's own harness config; got: %s", rec.Body.String())
}

// TestHarnessConfigScope_ProjectScoped_ListFiltersOtherHubMember is the
// ptone/scion#1916 regression test for the list surface — see the identical
// template-side comment on TestTemplateScope_ProjectScoped_ListFiltersOtherHubMember.
func TestHarnessConfigScope_ProjectScoped_ListFiltersOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupHarnessConfigScopeTest(t)
	createAuthzTestHarnessConfig(t, s, "alice-project-list-hc", store.HarnessConfigScopeProject, project.ID, alice.ID)

	recAlice := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/harness-configs?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recAlice.Code)
	var aliceResp ListHarnessConfigsResponse
	require.NoError(t, json.NewDecoder(recAlice.Body).Decode(&aliceResp))
	assert.NotEmpty(t, aliceResp.HarnessConfigs, "the project owner should see the project's own harness configs")

	recCarol := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recCarol.Code)
	var carolResp ListHarnessConfigsResponse
	require.NoError(t, json.NewDecoder(recCarol.Body).Decode(&carolResp))
	assert.Empty(t, carolResp.HarnessConfigs, "a non-member hub member must not see the project's harness configs in a list")
}

// ----------------------------------------------------------------------
// Hub-scoped (global) harness configs: still readable by every hub member.
// ----------------------------------------------------------------------

func TestHarnessConfigScope_GlobalScoped_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, _ := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "global-catalog-hc", store.HarnessConfigScopeGlobal, "", alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/harness-configs/"+hc.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"every hub member must still read hub-scoped (global) harness configs; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Agent tokens: an agent reads with its own project's scope grants, not
// beyond it.
// ----------------------------------------------------------------------

func TestHarnessConfigScope_Agent_OutsideProjectDeniedOnUserScopedHarnessConfig(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	owner := createNamedTestUser(t, s, "hcscope-agent-owner", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	hc := createAuthzTestHarnessConfig(t, s, "agent-outside-user-hc", store.HarnessConfigScopeUser, owner.ID, owner.ID)

	otherProject := &store.Project{ID: tid("hcscope-agent-project"), Name: "Agent Project", Slug: "hcscope-agent-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	agent := &store.Agent{
		ID: tid("hcscope-agent"), Slug: tid("hcscope-agent"), Name: "Scope Agent",
		ProjectID: otherProject.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: otherProject.ID,
	}}

	decision := authz.CheckAccess(ctx, identity, harnessConfigResource(hc), ActionRead)
	assert.False(t, decision.Allowed,
		"an agent outside the owner's scope must not read the owner's user-scoped harness config")
}

func TestHarnessConfigScope_Agent_SameProjectAllowedOnProjectScopedHarnessConfig(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	owner := createNamedTestUser(t, s, "hcscope-agent-sp-owner", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	project := &store.Project{ID: tid("hcscope-agent-sp-project"), Name: "Agent SP Project", Slug: "hcscope-agent-sp-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	hc := createAuthzTestHarnessConfig(t, s, "agent-same-project-hc", store.HarnessConfigScopeProject, project.ID, owner.ID)

	agent := &store.Agent{
		ID: tid("hcscope-agent-sp"), Slug: tid("hcscope-agent-sp"), Name: "Scope Agent SP",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}

	decision := authz.CheckAccess(ctx, identity, harnessConfigResource(hc), ActionRead)
	assert.True(t, decision.Allowed,
		"an agent in the same project as a project-scoped harness config must still be able to read it")
}

// ----------------------------------------------------------------------
// Clone (C5): the shared harness-config route dispatcher
// (handleHarnessConfigByID) gates every action, including clone, on
// authorizeHarnessConfigRoute before the switch runs, so a forbidden clone
// source and a nonexistent one must already read identically. This test
// pins that down for the harness-config route the way
// TestTemplateScope_Clone_ForbiddenAndMissingSourceUseIdenticalMessage does
// for templates, where the two outcomes were not identical before the fix.
// ----------------------------------------------------------------------

func TestHarnessConfigScope_Clone_ForbiddenAndMissingSourceUseIdenticalMessage(t *testing.T) {
	srv, s, alice, carol, project := setupHarnessConfigScopeTest(t)
	hc := createAuthzTestHarnessConfig(t, s, "alice-clone-source-private", store.HarnessConfigScopeProject, project.ID, alice.ID)

	forbiddenRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/clone", CloneTemplateRequest{Name: "carols-hc-clone-attempt"})
	require.Equal(t, http.StatusNotFound, forbiddenRec.Code, "got: %s", forbiddenRec.Body.String())
	var forbidden ErrorResponse
	require.NoError(t, json.Unmarshal(forbiddenRec.Body.Bytes(), &forbidden))

	missingRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/harness-configs/"+tid("hc-clone-does-not-exist")+"/clone", CloneTemplateRequest{Name: "carols-hc-clone-attempt-2"})
	require.Equal(t, http.StatusNotFound, missingRec.Code, "got: %s", missingRec.Body.String())
	var missing ErrorResponse
	require.NoError(t, json.Unmarshal(missingRec.Body.Bytes(), &missing))

	assert.Equal(t, "HarnessConfig not found", forbidden.Error.Message)
	assert.Equal(t, forbidden.Error.Message, missing.Error.Message,
		"a forbidden clone source and a nonexistent one must read identically")
}
