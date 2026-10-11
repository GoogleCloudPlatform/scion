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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createSourceProject creates a fully populated source project for clone tests,
// with settings annotations, labels, env vars, skill injections, a pre-start
// hook, and project-scoped harness configs and templates.
func createSourceProject(t *testing.T, srv *Server, s store.Store) *store.Project {
	t.Helper()
	ctx := context.Background()

	projectID := api.NewUUID()
	project := &store.Project{
		ID:                     projectID,
		Name:                   "Source Project",
		Slug:                   "source-project",
		GitRemote:              "https://github.com/test/repo.git",
		DefaultRuntimeBrokerID: "broker-123",
		OwnerID:                DevUserID,
		CreatedBy:              DevUserID,
		Annotations: map[string]string{
			"scion.io/default-model":          "claude-sonnet",
			"scion.io/default-max-turns":      "100",
			"scion.io/default-harness-config": "my-config",
		},
		Labels: map[string]string{
			"scion.dev/workspace-mode": "per-agent",
			"scion.dev/clone-url":      "https://github.com/test/repo.git",
			"scion.dev/default-branch": "main",
			"team":                     "backend",
		},
		SharedDirs: []api.SharedDir{
			{Name: "data"},
		},
		GitIdentity: &store.GitIdentityConfig{
			Name:  "Bot",
			Email: "bot@test.com",
		},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Add non-secret env vars
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "API_URL",
		Value:   "https://api.example.com",
		Scope:   store.ScopeProject,
		ScopeID: projectID,
	}))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:        api.NewUUID(),
		Key:       "MASKED_TOKEN",
		Value:     "token-value-123",
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Sensitive: true, // Sensitive but NOT a secret — should be copied
	}))

	// Add a secret-backed env var — should NOT be copied
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:      api.NewUUID(),
		Key:     "SECRET_KEY",
		Value:   "should-not-appear",
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Secret:  true,
	}))

	// Add skill injections
	require.NoError(t, s.SetSkillInjections(ctx, store.SkillInjectionScopeProject, projectID, []store.SkillInjection{
		{SkillURI: "skill://debugging", SortOrder: 1},
		{SkillURI: "skill://testing", SkillAs: "tdd", Optional: true, SortOrder: 2},
	}, DevUserID))

	// Add an older pre-start hook (will be archived when the second is created)
	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        api.NewUUID(),
		Scope:     store.PreStartHookScopeProject,
		ProjectID: projectID,
		Name:      "Old Setup",
		Slug:      "old-setup",
		Script:    "#!/bin/bash\necho old",
		Status:    store.ProjectPreStartHookStatusActive,
		CreatedBy: DevUserID,
	})
	require.NoError(t, err)

	// Add active pre-start hook — this archives the previous one
	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        api.NewUUID(),
		Scope:     store.PreStartHookScopeProject,
		ProjectID: projectID,
		Name:      "Setup",
		Slug:      "setup",
		Script:    "#!/bin/bash\necho setup",
		Status:    store.ProjectPreStartHookStatusActive,
		CreatedBy: DevUserID,
	})
	require.NoError(t, err)

	// Set up storage for project-scoped harness configs (with working Copy)
	stor := newCloneMockStorage("test-bucket")
	srv.SetStorage(stor)

	now := time.Now()

	// Add a project-scoped harness config
	hcStoragePath := "hubs/test-hub-id/harness-configs/project/" + projectID + "/my-config"
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:          api.NewUUID(),
		Name:        "My Config",
		Slug:        "my-config",
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeProject,
		ScopeID:     projectID,
		Status:      store.HarnessConfigStatusActive,
		StoragePath: hcStoragePath,
		Files: []store.TemplateFile{
			{Path: "config.yaml", Size: 100, Hash: "abc123"},
		},
		Created: now, Updated: now,
	}))
	// Seed mock storage with the file
	stor.seedObject(hcStoragePath+"/config.yaml", []byte("harness: claude"))

	// Add a project-scoped template
	tplStoragePath := "hubs/test-hub-id/templates/project/" + projectID + "/my-template"
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:          api.NewUUID(),
		Name:        "My Template",
		Slug:        "my-template",
		Harness:     "claude",
		Scope:       store.TemplateScopeProject,
		ScopeID:     projectID,
		Status:      store.TemplateStatusActive,
		StoragePath: tplStoragePath,
		Files: []store.TemplateFile{
			{Path: "template.yaml", Size: 50, Hash: "def456"},
		},
		Created: now, Updated: now,
	}))
	stor.seedObject(tplStoragePath+"/template.yaml", []byte("template: test"))

	return project
}

// cloneMockStorage wraps mockStorage with a working Copy implementation.
type cloneMockStorage struct {
	mockStorage

	// raceBarrier, when set, makes Copy block the calling goroutine until a
	// fixed number of distinct concurrent requests have all reached their
	// first Copy call — see copyBarrier in clone_concurrency_test.go. Nil by
	// default, so every other test using cloneMockStorage is unaffected.
	raceBarrier *copyBarrier
}

func newCloneMockStorage(bucket string) *cloneMockStorage {
	return &cloneMockStorage{
		mockStorage: mockStorage{
			bucket:  bucket,
			objects: make(map[string]*storage.Object),
			content: make(map[string][]byte),
		},
	}
}

// writeProjectDirFile creates dir/keep.txt and returns its path.
func writeProjectDirFile(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(f, []byte("keep"), 0o644))
	return f
}

// asgManagerActor creates a user with NO built-in project role who still
// passes the endpoint's project.manage gate, through a custom project role
// carrying project.read and project.manage. With hubAdmin it also holds the
// system hub-admin role (system role_binding.*): the hub-override actor
// reachable over HTTP. Without it, the actor has neither a project role nor
// hub authority, so the PUT refuses it before governance. A plain hub admin
// is refused at the project.manage gate (see
// TestAssignableRoles_HubAdminWithNoProjectRole).
func asgManagerActor(t *testing.T, f *mmrFixture, suffix string, hubAdmin bool) (*store.User, *store.RoleDefinition) {
	t.Helper()
	ctx := context.Background()
	manager, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "asg-manager-" + tid(t.Name() + suffix)[:8], ScopeType: store.RoleScopeProject,
		Permissions: []string{"project.read", "project.manage"},
	})
	require.NoError(t, err)
	u := grpUser(t, f.store, t.Name()+"-"+suffix, "Manager "+suffix)
	grpBind(t, f.store, "user", u.ID, manager.ID, f.projectID)
	if hubAdmin {
		mmrSeedHubAdmin(t, f.store, u.ID)
	}
	return u, manager
}

// asgHubOverrideActor is asgManagerActor with the hub-admin seed.
func asgHubOverrideActor(t *testing.T, f *mmrFixture) (*store.User, *store.RoleDefinition) {
	t.Helper()
	return asgManagerActor(t, f, "huboverride", true)
}

// membersGroupFor returns the project's members group.
func membersGroupFor(t *testing.T, s store.Store, project *store.Project) *store.Group {
	t.Helper()
	g, err := s.GetGroupBySlug(context.Background(), projectMembersGroupSlug(project.Slug))
	require.NoError(t, err)
	require.True(t, isSystemProjectMembersGroup(g, project.ID), "must be the system members group")
	return g
}

// backfillFailingStore makes selected BackfillRoleBindings steps fail.
type backfillFailingStore struct {
	store.Store
	failListUsers  bool
	failListGroups bool
}

// grpBind writes a project-scope binding directly to the store (fixture
// setup only; the tests below exercise the read path).
func grpBind(t *testing.T, s store.Store, principalType, principalID, roleDefID, projectID string) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// grpUser creates a hub user with the given display name.
func grpUser(t *testing.T, s store.Store, name, displayName string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: displayName, Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, id)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

func legacyMembersPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/members"
}

type mmrFixture struct {
	srv            *Server
	store          store.Store
	owner          *store.User
	admin          *store.User
	member         *store.User
	projectID      string
	projectSlug    string
	otherProjectID string

	ownerRD, adminRD, memberRD *store.RoleDefinition
	withinCeiling              *store.RoleDefinition // member perms + agent.message: within the owner's ceiling
	beyondCeiling              *store.RoleDefinition // carries agent.attach, which owners do not hold
	roleBindingCustom          *store.RoleDefinition // carries role_binding.create, which owners do not hold
}

func setupMMRFixture(t *testing.T) *mmrFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid(t.Name() + "-owner")
	projectID := tid(t.Name() + "-project")
	createRS1Project(t, s, projectID, ownerID)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	_, otherProjectID := func() (string, string) {
		oid := tid(t.Name() + "-other-owner")
		pid := tid(t.Name() + "-other-project")
		createRS1Project(t, s, pid, oid)
		return oid, pid
	}()

	adminID := tid(t.Name() + "-admin")
	createRS1UserWithRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	admin, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	memberID := tid(t.Name() + "-member")
	createRS1UserWithRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	within, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-within-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: append(append([]string{}, memberRD.Permissions...), "agent.message"),
	})
	require.NoError(t, err)
	beyond, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-beyond-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "agent.list", "agent.read", "agent.attach"},
	})
	require.NoError(t, err)
	rbCustom, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "mmr-rolebinding-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "role_binding.create", "role_binding.delete"},
	})
	require.NoError(t, err)

	return &mmrFixture{
		srv: srv, store: s,
		owner: owner, admin: admin, member: member,
		projectID: projectID, projectSlug: fmt.Sprintf("rs1-test-%s", projectID[:8]),
		otherProjectID:    otherProjectID,
		ownerRD:           ownerRD,
		adminRD:           adminRD,
		memberRD:          memberRD,
		withinCeiling:     within,
		beyondCeiling:     beyond,
		roleBindingCustom: rbCustom,
	}
}

func mmrPrincipalPath(projectID, principalType, principalID string) string {
	return fmt.Sprintf("/api/v1/projects/%s/members/principals/%s/%s", projectID, principalType, url.PathEscape(principalID))
}

func putMemberRoles(t *testing.T, srv *Server, actor *store.User, projectID, principalType, principalID string, roleIDs []string, expected *[]string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{"roleDefinitionIds": roleIDs}
	if expected != nil {
		body["expectedRoleDefinitionIds"] = *expected
	}
	return doRequestAsUser(t, srv, actor, http.MethodPut, mmrPrincipalPath(projectID, principalType, principalID), body)
}

func deleteMemberRoles(t *testing.T, srv *Server, actor *store.User, projectID, principalType, principalID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, srv, actor, http.MethodDelete, mmrPrincipalPath(projectID, principalType, principalID), nil)
}

// mmrBindingsFor returns the principal's active project-scope bindings.
func mmrBindingsFor(t *testing.T, s store.Store, principalType, principalID, projectID string) []*store.RoleBinding {
	t.Helper()
	bindings, err := s.ListRoleBindingsForPrincipal(context.Background(), principalType, principalID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			out = append(out, b)
		}
	}
	return out
}

func mmrAuditRows(t *testing.T, s store.Store, projectID string) []*store.MutationAuditRecord {
	t.Helper()
	rows, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetType: "project_membership",
		TargetID:   projectID,
		Limit:      1000,
	})
	require.NoError(t, err)
	return rows
}

func mmrSeedHubAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdmin.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// mmrServiceCtx builds a context carrying an interactive identity for a
// DIRECT ProjectMembershipService.SetMemberRoles call. The hub-override
// tests below call the service directly rather than through PUT, because the
// HTTP entry gate on this endpoint is project.manage and hub-admin does NOT
// hold project.manage (seed.go hubAdminPermissionIDs) —
// exactly like the existing AddMember/RemoveMember hub-override logic, whose
// own tests (rs5_global_admin_governance_test.go, rs5_r2_hardening_test.go)
// also call the service directly rather than through the project.manage-
// gated /members HTTP endpoints. In production, /api/v1/admin/role-bindings
// has no project.manage gate, so it is one way to reach the hub override —
// but not the only way: an actor with no built-in project role who passes
// this endpoint's own project.manage gate via a custom role carrying
// project.manage, and who also holds system role_binding.*, reaches the hub
// override over this endpoint too (review r2 R2-5).
func mmrServiceCtx(userID, email string) context.Context {
	identity := NewAuthenticatedUser(userID, email, "Test User", "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
}

func mmrServiceIdentity(userID, email string) UserIdentity {
	return NewAuthenticatedUser(userID, email, "Test User", "member", string(ClientTypeAPI))
}

// mmrAuthoritySwapStore wraps store.Store so a test can simulate a
// concurrent request landing between SetMemberRoles' pre-transaction phase
// (Phase P: reads, CanDelegate) and its locked re-read (Phase T, inside
// WithTx). It is swapped in for ProjectMembershipService.store for the
// duration of one SetMemberRoles call; its WithTx override runs swap()
// exactly once, immediately before delegating to the real WithTx — which is
// exactly the seam between Phase P (already complete by the time
// SetMemberRoles calls svc.store.WithTx) and Phase T (whose first statement
// is the lock). swap mutates the underlying store directly, outside of any
// transaction, modelling an already-committed concurrent write.
type mmrAuthoritySwapStore struct {
	store.Store
	swap    func()
	didSwap bool
}

// mmrDeleteSystemHubAdminBindings deletes userID's system-scope hub-admin
// binding(s) directly on s, modelling a concurrent revocation of the hub
// override that commits before SetMemberRoles takes its lock.
func mmrDeleteSystemHubAdminBindings(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	deleted := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == hubAdmin.ID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			deleted++
		}
	}
	require.Equal(t, 1, deleted, "expected exactly one system hub-admin binding to revoke")
}

type staleOwnerFixture struct {
	srv     *Server
	s       store.Store
	creator *store.User
	coOwner *store.User
	project *store.Project
}

func createStaleOwnerUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: id, Email: email, DisplayName: email,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	return u
}

// setupStaleOwnerFixture creates a project whose creator (OwnerID and
// CreatedBy) holds the project-owner binding, adds a co-owner, and then has
// the co-owner remove the creator through the members API with no
// ownership transfer. Project.OwnerID still names the creator afterwards.
func setupStaleOwnerFixture(t *testing.T) staleOwnerFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	creator := createStaleOwnerUser(t, s, tid("stale-owner-creator"), "stale-creator@test.com")
	coOwner := createStaleOwnerUser(t, s, tid("stale-owner-coowner"), "stale-coowner@test.com")

	project := &store.Project{
		ID:        tid("stale-owner-project"),
		Name:      "Stale Owner Project",
		Slug:      "stale-owner-project",
		GitRemote: "github.com/test/stale-owner",
		OwnerID:   creator.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, creator.ID))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, coOwner.ID))

	creatorBinding := projectOwnerBindingFor(t, s, creator.ID, project.ID)
	require.NotNil(t, creatorBinding, "creator must start with a project-owner binding")

	rec := doRequestAsUser(t, srv, coOwner, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+creatorBinding.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code,
		"co-owner removes creator: %s", rec.Body.String())
	require.Nil(t, projectOwnerBindingFor(t, s, creator.ID, project.ID),
		"creator's project-owner binding must be gone")

	stored, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, creator.ID, stored.OwnerID, "precondition: OwnerID still names the removed creator")

	return staleOwnerFixture{srv: srv, s: s, creator: creator, coOwner: coOwner, project: stored}
}

// projectBindingsFor returns the project-scoped role bindings held directly
// by the given user.
func projectBindingsFor(t *testing.T, s store.Store, projectID, userID string) []*store.RoleBinding {
	t.Helper()
	all, err := s.ListRoleBindingsForScope(context.Background(), store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range all {
		if b.PrincipalType == store.RoleBindingPrincipalUser && b.PrincipalID == userID {
			out = append(out, b)
		}
	}
	return out
}

// requireSingleOwnerBinding asserts that the project has exactly one
// project-owner binding and that it belongs to wantUserID.
func requireSingleOwnerBinding(t *testing.T, s store.Store, projectID, wantUserID string) {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	all, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var owners []*store.RoleBinding
	for _, b := range all {
		if b.RoleDefinitionID == ownerRD.ID {
			owners = append(owners, b)
		}
	}
	require.Len(t, owners, 1, "expected exactly one project-owner binding")
	assert.Equal(t, store.RoleBindingPrincipalUser, owners[0].PrincipalType)
	assert.Equal(t, wantUserID, owners[0].PrincipalID)
}

// transferAndRemoveCreator transfers ownership alice -> bob and then has bob
// remove all of alice's remaining bindings.
func transferAndRemoveCreator(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects/"+project.ID+"/transfer-ownership",
		map[string]string{"newOwnerId": bob.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	removeAllProjectBindings(t, srv, s, bob, project.ID, alice.ID)
}

// addProjectOwner gives user a project-owner binding through the members API.
func addProjectOwner(t *testing.T, srv *Server, s store.Store, actor, user *store.User, projectID string) {
	t.Helper()
	ownerRD, err := s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, actor, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		map[string]string{"roleDefinitionId": ownerRD.ID, "principalType": "user", "principalId": user.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
}

// usableOwnerCount returns the number of distinct principals on projectID
// that hold a usable owner binding. It goes through usableOwnerPrincipals,
// the same path projectHasUsableOwner (and so enforcement) uses.
func usableOwnerCount(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	ids, err := usableOwnerPrincipals(context.Background(), s, projectID, time.Now(), "", false)
	require.NoError(t, err)
	return len(ids)
}

// createTestProjectForPSH creates a test project for pre-start hook tests.
func createTestProjectForPSH(t *testing.T, s store.Store) *store.Project {
	t.Helper()
	project := &store.Project{
		ID:      tid("test-project-psh-" + t.Name()),
		Name:    "Test Project PSH",
		Slug:    "test-project-psh-" + strings.ToLower(t.Name()),
		OwnerID: "dev@localhost",
	}
	require.NoError(t, s.CreateProject(t.Context(), project))
	return project
}

// profileDefaultFixture is a bypassAgents fixture whose broker reports the
// stock two-profile set (local=docker, remote=kubernetes) with the given
// default profile, plus two verified project-scoped SAs: broad (the
// project-wide default) and k8s (the per-profile default for "remote").
type profileDefaultFixture struct {
	*bypassAgentsFixture
	broad *store.GCPServiceAccount
	k8s   *store.GCPServiceAccount
}

// newSettingsTestSA builds a project-scoped, verified SA belonging to project.
func newSettingsTestSA(t *testing.T, s store.Store, projectID, idName string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 tid(idName + t.Name()),
		Scope:              store.ScopeProject,
		ScopeID:            projectID,
		Email:              idName + "@proj.iam.gserviceaccount.com",
		ProjectID:          "gcp-proj",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: store.GCPVerificationVerified,
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(t.Context(), sa))
	return sa
}

func createTestProjectForSettings(t *testing.T, s store.Store) *store.Project {
	t.Helper()
	project := &store.Project{
		ID:   tid("test-project-settings-" + t.Name()),
		Name: "Test Project",
		Slug: "test-project-settings",
	}
	require.NoError(t, s.CreateProject(t.Context(), project))
	return project
}

func assertNoProjectWithSlug(t *testing.T, s store.Store, slug string) {
	t.Helper()
	_, err := s.GetProjectBySlug(context.Background(), slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no project may hold slug %q", slug)
}

// doDavRequest issues a WebDAV request with a raw body and optional headers.
// The JSON-encoding doRequest helper cannot express these verbs.
func doDavRequest(t *testing.T, srv *Server, method, urlPath string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// workspaceSecret is planted in a victim project's workspace. No response to a
// non-member may contain it.
const workspaceSecret = "PRIVATE-WORKSPACE-BYTES-do-not-serve-this"

// doMultipartRequest creates a multipart form request with file uploads.
// files is a map of field name (relative path) to file content.
func doMultipartRequest(t *testing.T, srv *Server, method, path string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for fieldName, content := range files {
		part, err := writer.CreateFormFile(fieldName, fieldName)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// createTestHubManagedProject creates a hub-managed project (no git remote) via the API
// and returns the project and its workspace path. Cleans up the workspace and any
// external project-config directory on test completion.
func createTestHubManagedProject(t *testing.T, srv *Server, name string) (*store.Project, string) {
	t.Helper()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	workspacePath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Clean up the external project-config directory created by initInRepoProject
		// (e.g. ~/.scion/project-configs/<slug>__<uuid>/).
		scionDir := filepath.Join(workspacePath, ".scion")
		if extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extAgentsDir != "" {
			// extAgentsDir is ~/.scion/project-configs/<slug>__<uuid>/.scion/agents
			// Go up past "agents" and ".scion" to remove the <slug>__<uuid> parent dir
			_ = os.RemoveAll(filepath.Dir(filepath.Dir(extAgentsDir)))
		}
		// Remove the project-configs directory named by the project record.
		_ = os.RemoveAll(filepath.Dir(filepath.Dir(resolveTestSharedDirPath(t, &project, "x"))))
		_ = os.RemoveAll(workspacePath)
	})

	return &project, workspacePath
}

// resolveTestSharedDirPath returns the shared dir path for a test hub-managed
// project, computed from the project record (slug and ID) alone:
// ~/.scion/project-configs/<slug>__<first 8 hex chars of ID>/shared-dirs/<dirName>.
func resolveTestSharedDirPath(t *testing.T, project *store.Project, dirName string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	shortID := strings.ReplaceAll(project.ID, "-", "")
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir,
		project.Slug+"__"+shortID, config.SharedDirsSubdir, dirName)
}

// createTestGitProject creates a git-backed project via the API.
func createTestGitProject(t *testing.T, srv *Server, name, remote string) *store.Project {
	t.Helper()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      name,
		GitRemote: remote,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return &project
}

// addSharedDirToProject adds a shared directory to a project via the API.
func addSharedDirToProject(t *testing.T, srv *Server, projectID, dirName string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", projectID), map[string]interface{}{
		"name": dirName,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// setNFSSharedDirStorageGlobalSettings writes a global settings.yaml with
// server.shared_dir_storage: nfs, under a fresh HOME, and returns the
// resolved host base (<mount_root>/<share id>) so the caller can
// pre-populate/inspect the export directly. Must be called BEFORE
// testServer(t), since HOME must be set before any settings are read.
func setNFSSharedDirStorageGlobalSettings(t *testing.T) (hostBase string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	hostBase = filepath.Join(tmpHome, "nfs-export")
	require.NoError(t, os.MkdirAll(hostBase, 0o2775))

	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: ` + tmpHome + `
      shares:
        - id: nfs-export
          pv_name: pv
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))
	return hostBase
}

// outsideSecret is the content the handlers must never read out of the tree
// outside the base, and never overwrite.
const outsideSecret = "TOP-SECRET-HUB-TOKEN-do-not-serve-this"

// outsideTree is the stand-in for everything the hub process can reach but the
// file browser has no business touching — ~/.scion, hub.db, authorized_keys.
type outsideTree struct {
	dir string
}

// newOutsideTree builds the tree and returns it. It deliberately lives in its
// own TempDir rather than a sibling of the base, so that a path that reaches
// it cannot have got there by any route except following a link.
func newOutsideTree(t *testing.T) *outsideTree {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(outsideSecret), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("delete me and the test fails"), 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "landing"), 0755))
	return &outsideTree{dir: dir}
}

// assertNotLeaked fails if a response body carries content from outside.
func assertNotLeaked(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.NotContains(t, rec.Body.String(), outsideSecret,
		"response leaked content from outside the served directory")
}

// plantSymlinks lays the four link shapes of the matrix into base, alongside
// one genuinely inside file for the allowed cases to resolve to.
//
// The two dangling forms are kept separate on purpose. os.Root refuses every
// absolute link outright, whether or not its target exists, while a relative
// link to a missing name inside the base is an ordinary not-found — the two
// arrive at the handlers as different errors and must both be shown to be safe.
func plantSymlinks(t *testing.T, base string, outside *outsideTree) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(base, "real"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "real", "inside.txt"), []byte("inside content"), 0644))

	// Escaping leaf: a link whose target is a file outside the base.
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "secret.txt"), filepath.Join(base, "esc_leaf")))
	// Escaping intermediate directory: a link used as a path component, so
	// that everything addressed beneath it resolves outside the base.
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(base, "esc_dir")))
	// Inside link: resolves back into the base, and is allowed.
	require.NoError(t, os.Symlink("real/inside.txt", filepath.Join(base, "in_link")))
	require.NoError(t, os.Symlink("real", filepath.Join(base, "in_dir")))
	// Dangling, absolute and outside.
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "no-such-file"), filepath.Join(base, "dangling")))
	// Dangling, relative and inside.
	require.NoError(t, os.Symlink("no-such-file", filepath.Join(base, "dangling_rel")))
}

func (m *cloneMockStorage) Copy(ctx context.Context, srcPath, dstPath string) (*storage.Object, error) {
	if m.raceBarrier != nil {
		// Must run before the mutex below: every racing goroutine needs to
		// reach the barrier on its own, so none of them can be holding m.mu
		// while waiting. Identified by the request's raceRequestID rather
		// than dstPath: see copyBarrier's doc comment in
		// clone_concurrency_test.go for why.
		if id, ok := raceRequestIDFromContext(ctx); ok {
			m.raceBarrier.arrive(id)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	srcObj, ok := m.objects[srcPath]
	if !ok {
		return nil, storage.ErrNotFound
	}
	dstObj := &storage.Object{
		Name: dstPath,
		Size: srcObj.Size,
	}
	m.objects[dstPath] = dstObj
	if data, ok := m.content[srcPath]; ok {
		m.content[dstPath] = data
	}
	return dstObj, nil
}

// Upload arrives at raceBarrier, like Copy: a template clone copies a
// legacy-layout source by reading each file and writing it as a blob
// (ptone/scion#4221), so its first storage write is an Upload, not a Copy.
// A request that already arrived through Copy (or an earlier Upload) does
// not arrive again (see copyBarrier).
func (m *cloneMockStorage) Upload(ctx context.Context, objectPath string, r io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	if m.raceBarrier != nil {
		if id, ok := raceRequestIDFromContext(ctx); ok {
			m.raceBarrier.arrive(id)
		}
	}
	return m.mockStorage.Upload(ctx, objectPath, r, opts)
}

// DeletePrefix actually removes matching keys, unlike the embedded
// mockStorage's no-op, so tests can observe whether a handler deleted a
// prefix it should not have (ptone/scion#1916 follow-up: clone failure
// cleanup must never remove a prefix the request did not itself create).
func (m *cloneMockStorage) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for path := range m.objects {
		if strings.HasPrefix(path, prefix) {
			delete(m.objects, path)
			delete(m.content, path)
		}
	}
	return nil
}

// seedObject inserts data into the mock storage for testing.
func (m *cloneMockStorage) seedObject(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.content[path] = data
	m.objects[path] = &storage.Object{
		Name: path,
		Size: int64(len(data)),
	}
}

func (m *cloneMockStorage) hasObject(objectPath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[objectPath]
	return ok
}

func (b *backfillFailingStore) ListUsers(ctx context.Context, f store.UserFilter, o store.ListOptions) (*store.ListResult[store.User], error) {
	if b.failListUsers {
		return nil, errors.New("injected ListUsers failure")
	}
	return b.Store.ListUsers(ctx, f, o)
}

func (b *backfillFailingStore) ListGroups(ctx context.Context, f store.GroupFilter, o store.ListOptions) (*store.ListResult[store.Group], error) {
	if b.failListGroups {
		return nil, errors.New("injected ListGroups failure")
	}
	return b.Store.ListGroups(ctx, f, o)
}

func (s *mmrAuthoritySwapStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.didSwap {
		s.didSwap = true
		s.swap()
	}
	return s.Store.WithTx(ctx, fn)
}

// setAnnotations writes project annotations straight to the store.
func (pf *profileDefaultFixture) setAnnotations(t *testing.T, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	proj, err := pf.store.GetProject(ctx, pf.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	for k, v := range kv {
		proj.Annotations[k] = v
	}
	require.NoError(t, pf.store.UpdateProject(ctx, proj))
}

func (pf *profileDefaultFixture) setProjectDefaultAssignBroad(t *testing.T) {
	t.Helper()
	pf.setAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: pf.broad.ID,
	})
}

func (pf *profileDefaultFixture) setProfileDefaults(t *testing.T, byProfile map[string]string) {
	t.Helper()
	b, err := json.Marshal(byProfile)
	require.NoError(t, err)
	pf.setAnnotations(t, map[string]string{projectSettingDefaultGCPIdentitySAIDByProfile: string(b)})
}

// snapshot records every path and file content under the tree.
func (o *outsideTree) snapshot(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(o.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(o.dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			got[rel+"/"] = ""
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		got[rel] = string(b)
		return nil
	})
	require.NoError(t, err)
	return got
}

// assertIntact fails if anything outside was created, modified or removed.
func (o *outsideTree) assertIntact(t *testing.T, before map[string]string) {
	t.Helper()
	assert.Equal(t, before, o.snapshot(t),
		"the tree outside the served directory changed; a handler followed a symlink out of it")
}

// projectOwnerBindingFor returns the user's project-owner binding on the
// project, or nil.
func projectOwnerBindingFor(t *testing.T, s store.Store, userID, projectID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for i := range bindings {
		b := bindings[i]
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRD.ID {
			return b
		}
	}
	return nil
}

// removeAllProjectBindings deletes every binding the user holds on the
// project through the members API, acting as remover.
func removeAllProjectBindings(t *testing.T, srv *Server, s store.Store, remover *store.User, projectID, userID string) {
	t.Helper()
	for _, b := range projectBindingsFor(t, s, projectID, userID) {
		rec := doRequestAsUser(t, srv, remover, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+b.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
	require.Empty(t, projectBindingsFor(t, s, projectID, userID))
}
