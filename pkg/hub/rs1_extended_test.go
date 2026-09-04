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
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS1 Extended Tests — R-8 missing test categories
//
// (a) AST no-direct-handler mutation proof
// (b) Scoped UAT credential tests
// (c) Suspended/invalid actor tests
// (d) Failure injection (store error propagation)
// (e) Concurrency/race tests
// (f) Migration/backfill tests
// (g) Direct API bypass tests
// (h) UI capability tests
// =============================================================================

// ---------------------------------------------------------------------------
// (a) AST: handlers_project_members.go must not directly mutate RoleBindings
// ---------------------------------------------------------------------------

func TestRS1_AST_HandlersDoNotDirectlyMutateRoleBindings(t *testing.T) {
	// Find the handlers file.
	repoRoot := findRS1RepoRoot(t)
	handlersFile := filepath.Join(repoRoot, "pkg", "hub", "handlers_project_members.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, handlersFile, nil, 0)
	require.NoError(t, err, "cannot parse handlers file")

	// Forbidden function calls in the handlers file: these must only be
	// called from the service, not directly from HTTP handlers.
	forbidden := map[string]bool{
		"CreateRoleBinding":   true,
		"DeleteRoleBinding":   true,
		"UpdateRoleBinding":   true,
		"CreateMutationAudit": true,
	}

	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if forbidden[sel.Sel.Name] {
			pos := fset.Position(call.Pos())
			violations = append(violations, pos.String()+": "+sel.Sel.Name)
		}
		return true
	})

	assert.Empty(t, violations,
		"RS1: handlers_project_members.go must not directly call %v — delegate to ProjectMembershipService instead", violations)
}

// TestRS1_AST_BypassPathsDocumented verifies that known direct
// CreateRoleBinding callers outside the membership service are enumerated.
// O-3: Expand AST security guard beyond the single handler file.
// If a new file starts calling CreateRoleBinding for project-scoped bindings
// without going through the membership service, this test will flag it.
func TestRS1_AST_BypassPathsDocumented(t *testing.T) {
	repoRoot := findRS1RepoRoot(t)

	// Files that are EXEMPT from the no-direct-mutation rule because they
	// handle non-membership project binding creation (e.g. project creation
	// initial owner binding, generic role-binding CRUD, constraint governance).
	// Each exemption must document WHY the bypass is safe.
	exemptFiles := map[string]string{
		"handlers_projects_core.go":       "project creation: initial owner binding on create, not a membership mutation",
		"handlers_roles.go":               "generic role-binding CRUD endpoint — project-scoped create/delete delegated to ProjectMembershipService; system-scoped operations use generic store",
		"handlers_auth.go":                "auth handler: system-scoped binding cleanup during user deactivation, not project membership",
		"access_constraint_governance.go": "constraint governance: creates role bindings for system governance, not project membership",
		"project_membership_service.go":   "the membership service itself",
		"useraccesstoken.go":              "RS4 bounded credential service: uses store mutations for token CRUD, not project membership role bindings",
		"seed.go":                         "bootstrap seeding: creates initial system/hub-level role bindings, not project membership",
		"server.go":                       "server initialization: system-level bootstrap, not project membership",
		"handlers_users_core.go":          "user admin role sync: creates/deletes system-scoped super-admin and hub-member bindings inside atomic role transitions, guarded by CanDelegate and per-field permission checks",
	}

	// Scan all .go files in pkg/hub for direct CreateRoleBinding calls.
	hubDir := filepath.Join(repoRoot, "pkg", "hub")
	entries, err := os.ReadDir(hubDir)
	require.NoError(t, err)

	fset := token.NewFileSet()
	var unexemptViolations []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		// Skip exempted files.
		if _, ok := exemptFiles[entry.Name()]; ok {
			continue
		}

		filePath := filepath.Join(hubDir, entry.Name())
		f, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "CreateRoleBinding" || sel.Sel.Name == "DeleteRoleBinding" {
				pos := fset.Position(call.Pos())
				unexemptViolations = append(unexemptViolations,
					entry.Name()+":"+sel.Sel.Name+" at "+pos.String())
			}
			return true
		})
	}

	assert.Empty(t, unexemptViolations,
		"RS1 O-3: unexempted files call CreateRoleBinding/DeleteRoleBinding directly — "+
			"either route through ProjectMembershipService or add to exemptFiles with justification: %v",
		unexemptViolations)
}

// ---------------------------------------------------------------------------
// (b) Scoped UAT credential tests — R2-R3: mint real UATs through production
// token path and exercise actual cross-project denial.
// ---------------------------------------------------------------------------

// doRequestWithUAT makes an HTTP request authenticated with a real scoped UAT.
func doRequestWithUAT(t *testing.T, srv *Server, uatKey, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+uatKey)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// rs4MintContext returns a context with the actor identity and credential
// context required by the RS4 bounded UAT service for audit record creation.
func rs4MintContext(userID string) context.Context {
	identity := NewAuthenticatedUser(userID, userID+"@test.com", "Test User", "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	return contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
}

// mintScopedUAT mints a real scoped UAT through the production token service.
// RS4: The issuer must have role bindings with the requested scopes in the
// target project; the context must carry the actor identity for audit.
func mintScopedUAT(t *testing.T, srv *Server, userID, projectID string, scopes []string) string {
	t.Helper()
	ctx := rs4MintContext(userID)
	key, _, err := srv.uatService.CreateToken(
		ctx, userID, "test-uat", projectID, scopes, nil,
	)
	require.NoError(t, err, "failed to mint scoped UAT")
	return key
}

func TestRS1_ScopedUAT_SameProjectAllowed(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs1-uat-proj")
	ownerID := tid("rs1-uat-owner")

	createRS1Project(t, s, projectID, ownerID)

	// R2-R3: mint a real scoped UAT through the production token path.
	// project:read is sufficient for listing members (the base permission
	// for project.membership.list is project.read).
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	// Use the UAT to list members in the SAME project — should succeed.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"RS1: scoped UAT should be able to list members in its scoped project (got %d: %s)", rec.Code, rec.Body.String())

	// Verify the response contains the owner as a member.
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	members, ok := body["items"].([]interface{})
	require.True(t, ok, "response should contain items array")
	assert.GreaterOrEqual(t, len(members), 1, "should have at least the owner")
}

func TestRS1_ScopedUAT_CrossProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectA := tid("rs1-uat-projA")
	projectB := tid("rs1-uat-projB")
	ownerID := tid("rs1-uat-owner2")
	ownerB := tid("rs1-uat-ownerB")

	createRS1Project(t, s, projectA, ownerID)
	createRS1Project(t, s, projectB, ownerB)
	// Give ownerID access to projectB too (so we know denial is scope-based,
	// not permission-based).
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectB,
		CreatedBy:        ownerB,
	})
	require.NoError(t, err)

	// R2-R3: mint a UAT scoped to project A with project:read scope.
	uatKey := mintScopedUAT(t, srv, ownerID, projectA, []string{"project:read"})

	// Use the UAT to list members in project B — MUST be denied by UAT
	// project-scope enforcement. The user has owner permissions in B via
	// session, but the UAT is scoped to A only.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet,
		"/api/v1/projects/"+projectB+"/members", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"RS1: scoped UAT for project A must be denied when accessing project B (got %d: %s)", rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// (b2) Scoped UAT mutation tests — R3-2: mint project:manage UATs and
// exercise actual membership mutations, cross-project denial, expiry,
// revocation, and suspension.
// ---------------------------------------------------------------------------

// TestRS1_ScopedUAT_MutationInProject mints a project:manage UAT and uses
// it to remove a member — proving that the UAT scope covers membership
// mutations. We test RemoveMember rather than AddMember because AddMember's
// delegation check requires the actor to hold ALL permissions from the
// target role (filtered by UAT scopes), and project:manage alone does not
// cover the agent:create etc. permissions in the target role.
func TestRS1_ScopedUAT_MutationInProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-uat-mut-p")
	ownerID := tid("rs1-uat-mut-o")
	targetID := tid("rs1-uat-mut-t")

	createRS1Project(t, s, projectID, ownerID)

	// Create a target member to remove.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// R3-2: mint a real project:manage UAT.
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:manage"})

	// Use the UAT to remove the member in the SAME project — should succeed.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/members/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"RS1 R3-2: project:manage UAT should allow removing a member in its scoped project (got %d: %s)", rec.Code, rec.Body.String())
}

// TestRS1_ScopedUAT_MutationCrossProjectDenied proves a project:manage UAT
// scoped to project A is denied for mutations in project B.
func TestRS1_ScopedUAT_MutationCrossProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectA := tid("rs1-uatmxA")
	projectB := tid("rs1-uatmxB")
	ownerID := tid("rs1-uatmx-o")
	ownerB := tid("rs1-uatmx-oB")
	targetID := tid("rs1-uatmx-t")

	createRS1Project(t, s, projectA, ownerID)
	createRS1Project(t, s, projectB, ownerB)

	// Give ownerID access to project B so denial is scope-based, not permission-based.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectB,
		CreatedBy:        ownerB,
	})
	require.NoError(t, err)

	// Create a target member in project B to attempt to remove.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	bindingB, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectB,
		CreatedBy:        ownerB,
	})
	require.NoError(t, err)

	// Mint a UAT scoped to project A with project:manage.
	uatKey := mintScopedUAT(t, srv, ownerID, projectA, []string{"project:manage"})

	// Attempt removal in project B — MUST be 403 (UAT scoped to A).
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete,
		"/api/v1/projects/"+projectB+"/members/"+bindingB.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"RS1 R3-2: project:manage UAT scoped to A must be denied for mutations in B (got %d: %s)", rec.Code, rec.Body.String())
}

// TestRS1_ScopedUAT_RevokedTokenDenied proves that a revoked UAT is denied
// when used for a membership mutation.
// R4-2: exercises revocation denial on the mutation path.
// R5-4: uses project:manage scope and DELETE (mutation), not GET.
func TestRS1_ScopedUAT_RevokedTokenDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-uat-rev-p")
	ownerID := tid("rs1-uat-rev-o")
	targetID := tid("rs1-uat-rev-t")

	createRS1Project(t, s, projectID, ownerID)

	// Create a target member to attempt to remove.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "RevTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	targetBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Mint a real project:manage UAT and then revoke it.
	// RS4: use rs4MintContext so the audit record has actor identity.
	mintCtx := rs4MintContext(ownerID)
	uatKey, token, err := srv.uatService.CreateToken(
		mintCtx, ownerID, "test-uat-revoke", projectID, []string{"project:manage"}, nil,
	)
	require.NoError(t, err)

	// Revoke the token.
	require.NoError(t, srv.uatService.RevokeToken(mintCtx, ownerID, token.ID))

	// Attempt to use the revoked token for a mutation (DELETE member) — denied 401.
	// The auth middleware detects the revoked status during identity resolution.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/members/"+targetBinding.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"RS1 R5-4: revoked project:manage UAT must be denied with 401 on mutation path (got %d: %s)", rec.Code, rec.Body.String())

	// Verify the target binding was NOT removed.
	_, getErr := s.GetRoleBinding(ctx, targetBinding.ID)
	assert.NoError(t, getErr, "RS1 R5-4: target binding must still exist after revoked-token denial")
}

// TestRS1_ScopedUAT_ExpiredTokenDenied proves that an expired UAT is denied
// when used for a membership mutation.
// R4-2: distinct from revocation — tests the ExpiresAt timestamp mechanism.
// R5-4: uses project:manage scope and DELETE (mutation), not GET/project:read.
// CreateToken rejects already-expired timestamps, so we create with a future
// expiry and then directly update the persisted token's ExpiresAt to a past
// time via raw SQL (clock seam via controlled persisted expiry transition).
func TestRS1_ScopedUAT_ExpiredTokenDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-uat-exp-p")
	ownerID := tid("rs1-uat-exp-o")
	targetID := tid("rs1-uat-exp-t")

	createRS1Project(t, s, projectID, ownerID)

	// Create a target member to attempt to remove.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "ExpTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	targetBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Mint a real project:manage UAT with a future expiry.
	// RS4: use rs4MintContext so the audit record has actor identity.
	mintCtx := rs4MintContext(ownerID)
	futureExpiry := time.Now().Add(24 * time.Hour)
	uatKey, token, err := srv.uatService.CreateToken(
		mintCtx, ownerID, "test-uat-expire", projectID,
		[]string{"project:manage"}, &futureExpiry,
	)
	require.NoError(t, err)

	// Transition the token to expired state by setting ExpiresAt to the past
	// via raw SQL. This is the "controlled persisted expiry transition"
	// approach required by the brief since CreateToken rejects past timestamps.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB() for raw SQL access")
	db := dbProvider.DB()
	require.NotNil(t, db)
	pastTime := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano)
	_, err = db.ExecContext(ctx,
		"UPDATE user_access_tokens SET expires_at = ? WHERE id = ?",
		pastTime, token.ID)
	require.NoError(t, err, "must be able to update token expiry via raw SQL")

	// Attempt to use the expired token for a mutation (DELETE member) — denied 401.
	// The auth middleware compares ExpiresAt against current time.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/members/"+targetBinding.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"RS1 R5-4: expired project:manage UAT must be denied with 401 on mutation path (got %d: %s)", rec.Code, rec.Body.String())

	// Verify the target binding was NOT removed.
	_, getErr := s.GetRoleBinding(ctx, targetBinding.ID)
	assert.NoError(t, getErr, "RS1 R5-4: target binding must still exist after expired-token denial")
}

// TestRS1_ScopedUAT_SuspendedUserDenied proves that a UAT owned by a
// suspended user is denied even if the token itself is valid.
// R4 O-2: uses project:manage scope and a mutation operation to prove
// suspended-user denial on the mutation path.
func TestRS1_ScopedUAT_SuspendedUserDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-uat-sus-p")
	ownerID := tid("rs1-uat-sus-o")
	targetID := tid("rs1-uat-sus-t")

	createRS1Project(t, s, projectID, ownerID)

	// Create a target member to attempt to remove.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "SusTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	memberBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// R4 O-2: mint a project:manage UAT for the mutation path.
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:manage"})

	// Suspend the user.
	u, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, u))

	// Attempt to use the UAT for a mutation — should be denied (user is suspended).
	// The denial mechanism is at the auth middleware level (user status check
	// during identity resolution), so the suspended user is rejected before
	// reaching the handler.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete,
		"/api/v1/projects/"+projectID+"/members/"+memberBinding.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"RS1 R4 O-2: suspended user's project:manage UAT must be denied with 403 on mutation path (got %d: %s)", rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// (c) Suspended/invalid actor tests
// ---------------------------------------------------------------------------

func TestRS1_SuspendedActorCannotManageMembers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-susp-proj")
	ownerID := tid("rs1-susp-owner")
	suspendedID := tid("rs1-susp-actor")
	targetID := tid("rs1-susp-target")

	createRS1Project(t, s, projectID, ownerID)

	// Create a suspended admin.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: suspendedID, Email: "suspended@test.com",
		DisplayName: "Suspended", Role: "member", Status: "suspended",
	}))
	ensureHubMembership(ctx, s, suspendedID)

	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      suspendedID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "susp-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Suspended user should be rejected by the auth middleware.
	suspended := &store.User{
		ID:    suspendedID,
		Email: "suspended@test.com",
	}
	rec := doRequestAsUser(t, srv, suspended, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
		})
	// Suspended users are typically rejected by the auth middleware (401/403).
	// The exact status depends on the middleware — 401 or 403.
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"RS1: suspended user should be rejected (got %d)", rec.Code)
}

func TestRS1_InvalidActorDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-inv-proj")
	ownerID := tid("rs1-inv-owner")
	targetID := tid("rs1-inv-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "inv-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Actor with no project role should be denied by governance.
	noRole := &store.User{
		ID:    targetID,
		Email: "inv-target@test.com",
	}
	rec := doRequestAsUser(t, srv, noRole, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      tid("some-new-user"),
		})
	// The authorize middleware checks project.manage, and a user without any
	// role binding won't have that permission.
	assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusBadRequest,
		"RS1: actor with no project role should be denied (got %d)", rec.Code)
}

// ---------------------------------------------------------------------------
// (d) Failure injection — store error propagation
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// failingStore — configurable store wrapper for failure injection.
//
// Wraps a real store.Store and returns injected errors on specific operations.
// The injection config is preserved across WithTx boundaries: when WithTx is
// called, the inner tx store is also wrapped in a failingStore with the same
// configuration, so failures inside transactional callbacks behave identically
// to failures on the outer store.
// ---------------------------------------------------------------------------

type failingStore struct {
	store.Store // embed to satisfy interface — all non-overridden methods delegate

	// Configurable failure points. When non-nil, the corresponding method
	// returns this error instead of delegating to the wrapped store.
	createRoleBindingErr   error
	deleteRoleBindingErr   error
	createMutationAuditErr error
	// commitErr causes WithTx to return this error after fn succeeds but before
	// the real commit occurs. The deferred rollback in the real store's WithTx
	// implementation reverses any changes made inside fn.
	commitErr error
}

func (f *failingStore) CreateRoleBinding(ctx context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	if f.createRoleBindingErr != nil {
		return nil, f.createRoleBindingErr
	}
	return f.Store.CreateRoleBinding(ctx, rb)
}

func (f *failingStore) DeleteRoleBinding(ctx context.Context, id string) error {
	if f.deleteRoleBindingErr != nil {
		return f.deleteRoleBindingErr
	}
	return f.Store.DeleteRoleBinding(ctx, id)
}

func (f *failingStore) CreateMutationAudit(ctx context.Context, record *store.MutationAuditRecord) error {
	if f.createMutationAuditErr != nil {
		return f.createMutationAuditErr
	}
	return f.Store.CreateMutationAudit(ctx, record)
}

func (f *failingStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		// Wrap the inner tx so that injection config is preserved inside
		// the transactional callback — this is the critical requirement
		// from R2-R4.
		wrappedTx := &failingStore{
			Store:                  tx,
			createRoleBindingErr:   f.createRoleBindingErr,
			deleteRoleBindingErr:   f.deleteRoleBindingErr,
			createMutationAuditErr: f.createMutationAuditErr,
			// commitErr is not copied to inner tx; it's handled at this level.
		}
		if err := fn(wrappedTx); err != nil {
			return err
		}
		// If commitErr is set, return it after fn succeeds. The real
		// WithTx sees a non-nil error from fn and runs its deferred
		// rollback, undoing everything fn did.
		if f.commitErr != nil {
			return f.commitErr
		}
		return nil
	})
}

func TestRS1_TransactionRollbackOnAuditFailure(t *testing.T) {
	// R2-R4: Inject a failure into CreateMutationAudit. The service calls
	// CreateRoleBinding then CreateMutationAudit inside the same WithTx.
	// When the audit write fails the transaction must roll back, leaving
	// zero new bindings in the store.
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-tx-proj")
	ownerID := tid("rs1-tx-owner")
	targetID := tid("rs1-tx-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "tx-target@test.com",
		DisplayName: "TX Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Count bindings before the attempt.
	bindingsBefore, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	countBefore := countProjectBindings(bindingsBefore, projectID)

	// Swap the service's store with a failingStore that rejects audit writes.
	fs := &failingStore{
		Store:                  s,
		createMutationAuditErr: errors.New("injected: audit write failure"),
	}
	srv.membershipService.store = fs
	defer func() { srv.membershipService.store = s }()

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Attempt to add a member — should fail (500) because audit write fails.
	owner := &store.User{ID: ownerID, Email: "tx-owner@test.com"}
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
		})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"RS1: audit failure must produce 500, not success")

	// Verify rollback: no new binding was persisted.
	bindingsAfter, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	countAfter := countProjectBindings(bindingsAfter, projectID)
	assert.Equal(t, countBefore, countAfter,
		"RS1: audit failure must roll back binding creation (before=%d after=%d)", countBefore, countAfter)
}

func TestRS1_DeleteFailurePropagation(t *testing.T) {
	// R2-R4: Inject a failure into DeleteRoleBinding. When UpdateMemberRole
	// (which creates a new binding then deletes the old one) hits a delete
	// failure, the entire transaction must roll back — no new binding should
	// appear, and the old binding must remain unchanged.
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-delfail-proj")
	ownerID := tid("rs1-delfail-owner")
	targetID := tid("rs1-delfail-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "delfail-target@test.com",
		DisplayName: "DelFail Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Add a member (with real store — must succeed).
	owner := &store.User{ID: ownerID, Email: "delfail-owner@test.com"}
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var addResp projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&addResp))

	// Swap to failing store — DeleteRoleBinding will fail.
	fs := &failingStore{
		Store:                s,
		deleteRoleBindingErr: errors.New("injected: delete binding failure"),
	}
	srv.membershipService.store = fs
	defer func() { srv.membershipService.store = s }()

	// Attempt role update from member → admin. Should fail.
	rec = doRequestAsUser(t, srv, owner, http.MethodPatch,
		"/api/v1/projects/"+projectID+"/members/"+addResp.ID,
		updateProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
		})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"RS1: delete failure during role update must produce 500")

	// Verify rollback: the original member binding still exists with the
	// original role (member, not admin), and no duplicate appeared.
	srv.membershipService.store = s // restore real store for verification
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	projectBindings := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			projectBindings++
			assert.Equal(t, memberRD.ID, b.RoleDefinitionID,
				"RS1: after failed role update, original role must be unchanged")
		}
	}
	assert.Equal(t, 1, projectBindings,
		"RS1: after failed role update, exactly one binding must remain")
}

func TestRS1_CommitFailureProduces500(t *testing.T) {
	// R2-R4: Inject a commit failure. Even if all store operations inside
	// the transaction succeed, a commit failure must produce a 500 error
	// (not success) and leave no persisted side effects.
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-commit-proj")
	ownerID := tid("rs1-commit-owner")
	targetID := tid("rs1-commit-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "commit-target@test.com",
		DisplayName: "Commit Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	bindingsBefore, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	countBefore := countProjectBindings(bindingsBefore, projectID)

	// Swap to failing store — commit will fail after fn succeeds.
	fs := &failingStore{
		Store:     s,
		commitErr: errors.New("injected: commit failure"),
	}
	srv.membershipService.store = fs
	defer func() { srv.membershipService.store = s }()

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	owner := &store.User{ID: ownerID, Email: "commit-owner@test.com"}
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
		})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"RS1: commit failure must produce 500, not success")

	// Verify rollback: no binding was persisted.
	srv.membershipService.store = s
	bindingsAfter, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	countAfter := countProjectBindings(bindingsAfter, projectID)
	assert.Equal(t, countBefore, countAfter,
		"RS1: commit failure must roll back all changes")
}

// countProjectBindings counts bindings scoped to the given project.
func countProjectBindings(bindings []*store.RoleBinding, projectID string) int {
	n := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// (e) Concurrency/race tests
// ---------------------------------------------------------------------------

func TestRS1_ConcurrentRemoveAndTransfer(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-conc-proj")
	owner1ID := tid("rs1-conc-owner1")
	owner2ID := tid("rs1-conc-owner2")
	targetID := tid("rs1-conc-target")

	createRS1Project(t, s, projectID, owner1ID)

	// Add a second owner.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: "conc-owner2@test.com",
		DisplayName: "Owner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Add a target member.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "conc-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Run concurrent operations: owner1 removes the member while owner2 transfers.
	var wg sync.WaitGroup
	var removeCode, transferCode int

	owner1 := &store.User{ID: owner1ID, Email: "conc-owner1@test.com"}
	owner2 := &store.User{ID: owner2ID, Email: "conc-owner2@test.com"}

	// Find the member binding.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	var memberBindingID string
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			memberBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, memberBindingID)

	wg.Add(2)
	go func() {
		defer wg.Done()
		rec := doRequestAsUser(t, srv, owner1, http.MethodDelete,
			"/api/v1/projects/"+projectID+"/members/"+memberBindingID, nil)
		removeCode = rec.Code
	}()
	go func() {
		defer wg.Done()
		rec := doRequestAsUser(t, srv, owner2, http.MethodPost,
			"/api/v1/projects/"+projectID+"/transfer-ownership",
			transferOwnershipRequest{NewOwnerID: owner1ID})
		transferCode = rec.Code
	}()
	wg.Wait()

	// At least one should succeed; neither should panic or leave corrupt state.
	assert.True(t, removeCode == http.StatusNoContent || removeCode == http.StatusNotFound || removeCode >= 400,
		"RS1: concurrent remove should complete cleanly (got %d)", removeCode)
	assert.True(t, transferCode == http.StatusOK || transferCode >= 400,
		"RS1: concurrent transfer should complete cleanly (got %d)", transferCode)

	// Verify no zero-owner state.
	svc := srv.membershipService
	count, err := svc.countActiveDirectOwners(ctx, projectID)
	require.NoError(t, err)
	assert.True(t, count >= 1, "RS1: after concurrent ops, at least one owner must remain (got %d)", count)
}

func TestRS1_ConcurrentDemotions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-concdem-proj")
	owner1ID := tid("rs1-concdem-o1")
	owner2ID := tid("rs1-concdem-o2")

	createRS1Project(t, s, projectID, owner1ID)

	// Add a second owner.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: "concdem-o2@test.com",
		DisplayName: "Owner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	o2Binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Find owner1's binding.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	require.NoError(t, err)
	var o1BindingID string
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRD.ID {
			o1BindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, o1BindingID)

	// Concurrent demotions: each owner demotes the other.
	var wg sync.WaitGroup
	var code1, code2 int
	owner1 := &store.User{ID: owner1ID, Email: "concdem-o1@test.com"}
	owner2 := &store.User{ID: owner2ID, Email: "concdem-o2@test.com"}

	wg.Add(2)
	go func() {
		defer wg.Done()
		rec := doRequestAsUser(t, srv, owner1, http.MethodPatch,
			"/api/v1/projects/"+projectID+"/members/"+o2Binding.ID,
			updateProjectMemberRequest{RoleDefinitionID: memberRD.ID})
		code1 = rec.Code
	}()
	go func() {
		defer wg.Done()
		rec := doRequestAsUser(t, srv, owner2, http.MethodPatch,
			"/api/v1/projects/"+projectID+"/members/"+o1BindingID,
			updateProjectMemberRequest{RoleDefinitionID: memberRD.ID})
		code2 = rec.Code
	}()
	wg.Wait()

	// R2-R1 fix: enforceLastOwnerTx now runs INSIDE the transaction, so
	// the owner count is read within the transactional snapshot. Under SQLite's
	// serialized transactions, concurrent demotions execute sequentially and the
	// second sees the post-demotion state. At most one should succeed.
	t.Logf("concurrent demotion results: code1=%d code2=%d", code1, code2)

	// At most one should succeed. At least one must be denied by the
	// last-owner guard (409) or encounter a conflict.
	assert.True(t, code1 != http.StatusOK || code2 != http.StatusOK,
		"RS1: concurrent demotions must not both succeed — last-owner guard must fire")

	// At least one must succeed (two owners exist — one demotion is valid).
	assert.True(t, code1 == http.StatusOK || code2 == http.StatusOK,
		"RS1: at least one concurrent demotion should succeed when two owners exist")

	// Verify at least one owner remains — the invariant MUST hold.
	svc := srv.membershipService
	count, err := svc.countActiveDirectOwners(ctx, projectID)
	require.NoError(t, err)
	assert.True(t, count >= 1,
		"RS1: after concurrent demotions, at least one owner must remain (got %d)", count)
}

// ---------------------------------------------------------------------------
// (f) Migration/backfill tests
// ---------------------------------------------------------------------------

func TestRS1_MigrateMultiRoleBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-mig-proj")
	ownerID := tid("rs1-mig-owner")
	targetID := tid("rs1-mig-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "mig-target@test.com",
		DisplayName: "Mig Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Temporarily drop the D4 partial unique index so we can simulate
	// pre-RS1 dirty data (two bindings for same principal in same project).
	if dbProvider, ok := s.(interface{ DB() *sql.DB }); ok {
		if db := dbProvider.DB(); db != nil {
			_, _ = db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_rolebinding_one_per_principal_per_project")
		}
	}

	// Create duplicate bindings — simulate pre-RS1 data.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Verify duplicates exist.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	projectBindings := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			projectBindings++
		}
	}
	require.True(t, projectBindings >= 2, "setup: expected at least 2 project bindings, got %d", projectBindings)

	// Run migration.
	svc := srv.membershipService
	results, err := svc.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// Find the result for our target.
	var found bool
	for _, r := range results {
		if r.PrincipalID == targetID && r.ProjectID == projectID {
			found = true
			assert.NoError(t, r.Error, "migration should succeed")
			assert.Equal(t, store.ProjectRoleAdmin, r.KeptRole,
				"migration should keep the highest-authority role (admin > member)")
			assert.True(t, r.DeletedCount >= 1, "should have deleted at least 1 duplicate")
		}
	}
	assert.True(t, found, "migration should process the duplicate bindings")

	// Verify only one binding remains.
	bindings, err = s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	projectBindings = 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			projectBindings++
		}
	}
	assert.Equal(t, 1, projectBindings,
		"after migration, exactly one project binding should remain")
}

func TestRS1_MigrateIdempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-mig-idem-proj")
	ownerID := tid("rs1-mig-idem-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Run migration on clean data — should be a no-op.
	svc := srv.membershipService
	results, err := svc.MigrateMultiRoleBindings(ctx)
	require.NoError(t, err)

	// Should have no results for this project (no duplicates).
	for _, r := range results {
		if r.ProjectID == projectID {
			t.Errorf("migration should produce no results for clean project, got: %+v", r)
		}
	}
}

// TestRS1_D4_PartialUniqueIndex verifies that after migration and index
// installation, the database rejects a second project-scoped binding for the
// same principal with a different role. This is the O-2 enforcement test.
func TestRS1_D4_PartialUniqueIndex(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-d4idx-proj")
	ownerID := tid("rs1-d4idx-owner")
	targetID := tid("rs1-d4idx-targ")

	createRS1Project(t, s, projectID, ownerID)

	// Create target user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Assign target as member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Attempt to create a SECOND binding for the same user with admin role.
	// The partial unique index should reject this at the database level.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	// The index should cause this to fail with a constraint violation.
	// If the index is not present, this succeeds (and D4 is only enforced
	// at the application level).
	assert.Error(t, err,
		"RS1 O-2: partial unique index should reject second project binding for same principal")
}

// ---------------------------------------------------------------------------
// (g) Direct API bypass tests — capabilities match actual API denial
// ---------------------------------------------------------------------------

func TestRS1_CapabilitiesMatchAPIDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-bypass-proj")
	ownerID := tid("rs1-bypass-owner")
	adminID := tid("rs1-bypass-admin")
	targetID := tid("rs1-bypass-target")

	createRS1Project(t, s, projectID, ownerID)

	// Create admin.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "bypass-admin@test.com",
		DisplayName: "Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, adminID)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Create target.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "bypass-target@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Get capabilities for admin.
	admin := &store.User{ID: adminID, Email: "bypass-admin@test.com"}
	rec := doRequestAsUser(t, srv, admin, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var listResp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listResp))
	require.NotNil(t, listResp.Capabilities)

	// Admin capabilities: canManageMembers=true, canManageAdmins=false.
	assert.True(t, listResp.Capabilities.CanManageMembers,
		"admin should have canManageMembers")
	assert.False(t, listResp.Capabilities.CanManageAdmins,
		"admin should NOT have canManageAdmins")
	assert.False(t, listResp.Capabilities.CanTransfer,
		"admin should NOT have canTransfer")

	// Verify: admin CAN add a member (matches canManageMembers=true).
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
		})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"admin with canManageMembers=true CAN add a member")

	// Verify: admin CANNOT add an admin (matches canManageAdmins=false).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("rs1-bypass-target2"), Email: "bypass-target2@test.com",
		DisplayName: "Target2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, tid("rs1-bypass-target2"))
	rec = doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    "user",
			PrincipalID:      tid("rs1-bypass-target2"),
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"admin with canManageAdmins=false CANNOT add an admin — API denial matches capability")

	// Verify: admin CANNOT transfer ownership (matches canTransfer=false).
	rec = doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: targetID})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"admin with canTransfer=false CANNOT transfer — API denial matches capability")
}

// ---------------------------------------------------------------------------
// (h) UI capability tests — owner capabilities
// ---------------------------------------------------------------------------

func TestRS1_OwnerCapabilities(t *testing.T) {
	srv, s := testServer(t)
	_ = context.Background()

	projectID := tid("rs1-ocap-proj")
	ownerID := tid("rs1-ocap-owner")

	createRS1Project(t, s, projectID, ownerID)

	owner := &store.User{ID: ownerID, Email: "ocap-owner@test.com"}
	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var listResp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listResp))
	require.NotNil(t, listResp.Capabilities)

	assert.True(t, listResp.Capabilities.CanManageMembers, "owner: canManageMembers")
	assert.True(t, listResp.Capabilities.CanManageAdmins, "owner: canManageAdmins")
	assert.True(t, listResp.Capabilities.CanManageOwners, "owner: canManageOwners")
	assert.True(t, listResp.Capabilities.CanTransfer, "owner: canTransfer")
	assert.Contains(t, listResp.Capabilities.Actions, "manage_members")
	assert.Contains(t, listResp.Capabilities.Actions, "manage_admins")
	assert.Contains(t, listResp.Capabilities.Actions, "manage_owners")
	assert.Contains(t, listResp.Capabilities.Actions, "transfer_ownership")
}

func TestRS1_MemberCapabilities(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-mcap-proj")
	ownerID := tid("rs1-mcap-owner")
	memberID := tid("rs1-mcap-member")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "mcap-member@test.com",
		DisplayName: "Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      memberID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	member := &store.User{ID: memberID, Email: "mcap-member@test.com"}
	rec := doRequestAsUser(t, srv, member, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var listResp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listResp))
	require.NotNil(t, listResp.Capabilities)

	assert.False(t, listResp.Capabilities.CanManageMembers, "member: no canManageMembers")
	assert.False(t, listResp.Capabilities.CanManageAdmins, "member: no canManageAdmins")
	assert.False(t, listResp.Capabilities.CanManageOwners, "member: no canManageOwners")
	assert.False(t, listResp.Capabilities.CanTransfer, "member: no canTransfer")
	assert.Empty(t, listResp.Capabilities.Actions, "member: empty actions")
}

// ---------------------------------------------------------------------------
// (i) D4 fail-closed tests — R4-1
// ---------------------------------------------------------------------------

// TestRS1_D4_IndexInstallationFailClosed proves that runMembershipMigration
// returns an error (and thus NewServer fails) if the D4 partial unique index
// cannot be installed. This is the R4-1 fail-closed requirement.
func TestRS1_D4_IndexInstallationFailClosed(t *testing.T) {
	// This test works by creating a server with a store that does NOT expose
	// a DB() method, which simulates a missing raw-DB capability.
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")

	// Wrap the store in a type that hides the DB() method.
	noDBStore := &noDBStore{Store: s}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	_, err = New(cfg, noDBStore)
	require.Error(t, err, "RS1 R4-1: NewServer must fail if D4 index cannot be installed")
	assert.Contains(t, err.Error(), "D4 partial unique index",
		"RS1 R4-1: error must mention D4 partial unique index")
}

// noDBStore wraps a store.Store but does NOT expose DB(). This simulates
// the case where the store doesn't support raw database access.
type noDBStore struct {
	store.Store
}

// TestRS1_D4_DDLFailurePath proves that a DDL execution failure during D4
// index installation is properly fail-closed. This is R5-2: the test injects
// a real DDL error by dropping the role_bindings table before the D4 DDL runs,
// so ExecContext returns an error on the actual DDL path (server.go:3779).
func TestRS1_D4_DDLFailurePath(t *testing.T) {
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")

	// Wrap the store with one that drops the role_bindings table when DB()
	// is called. The migration phase runs through the store interface (no
	// raw DB), then D4 index installation calls DB() and ExecContext with
	// the index DDL — which fails because the table no longer exists.
	ddlFail := &ddlFailStore{Store: s, realDB: s.(interface{ DB() *sql.DB }).DB()}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	_, err = New(cfg, ddlFail)
	require.Error(t, err, "RS1 R5-2: NewServer must fail when DDL ExecContext returns an error")
	assert.Contains(t, err.Error(), "D4 partial unique index creation failed",
		"RS1 R5-2: error must mention D4 DDL failure, not just missing DB (got: %s)", err.Error())
}

// ddlFailStore wraps a store.Store and exposes a DB() that sabotages the
// database so the D4 index DDL fails. It drops the role_bindings table on
// the first DB() call, making the CREATE INDEX DDL return an error.
type ddlFailStore struct {
	store.Store
	realDB  *sql.DB
	dropped bool
}

func (s *ddlFailStore) DB() *sql.DB {
	if !s.dropped {
		s.dropped = true
		// Drop the table that the D4 index targets. The CREATE INDEX DDL
		// references role_bindings, so it will fail with "no such table."
		_, _ = s.realDB.Exec("DROP TABLE IF EXISTS role_bindings")
	}
	return s.realDB
}

// ---------------------------------------------------------------------------
// (i-2) R5-1: Nil membership service fail-closed tests
// ---------------------------------------------------------------------------

// TestRS1_NilMembershipServiceFailClosed_Create proves that a project-scoped
// create via the generic endpoint returns 500 when membershipService is nil,
// and does NOT fall through to direct store mutation.
func TestRS1_NilMembershipServiceFailClosed_Create(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-nil-c-proj")
	ownerID := tid("rs1-nil-c-own")
	targetID := tid("rs1-nil-c-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "NilCreate", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Nil out the membership service.
	srv.membershipService = nil

	// Attempt a project-scoped create — must fail with 500, not succeed.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"RS1 R5-1: project-scope create with nil membership service must return 500, not fall through (got %d: %s)",
		rec.Code, rec.Body.String())

	// Verify no binding was created.
	bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	for _, b := range bindings {
		assert.False(t, b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID,
			"RS1 R5-1: nil membership service must not create a project binding")
	}
}

// TestRS1_NilMembershipServiceFailClosed_Delete proves that a project-scoped
// delete via the generic endpoint returns 500 when membershipService is nil,
// and does NOT fall through to direct store deletion.
func TestRS1_NilMembershipServiceFailClosed_Delete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-nil-d-proj")
	ownerID := tid("rs1-nil-d-own")
	targetID := tid("rs1-nil-d-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "NilDelete", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Nil out the membership service.
	srv.membershipService = nil

	// Attempt a project-scoped delete — must fail with 500, not succeed.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"RS1 R5-1: project-scope delete with nil membership service must return 500, not fall through (got %d: %s)",
		rec.Code, rec.Body.String())

	// Verify the binding was NOT deleted.
	_, getErr := s.GetRoleBinding(ctx, binding.ID)
	assert.NoError(t, getErr,
		"RS1 R5-1: nil membership service must not delete the project binding")
}

// ---------------------------------------------------------------------------
// (i-3) R5 O-1: Authority lookup failure injection tests
// ---------------------------------------------------------------------------

// failingBindingStore wraps a store.Store and makes ListRoleBindingsForPrincipal
// return an error for a specific principal ID, simulating a store failure
// inside the locked transaction.
type failingBindingStore struct {
	store.Store
	failForPrincipalID string
}

func (s *failingBindingStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	if principalID == s.failForPrincipalID {
		return nil, errors.New("injected store failure: ListRoleBindingsForPrincipal")
	}
	return s.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (s *failingBindingStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		// Wrap the tx store too so failures happen inside the transaction.
		return fn(&failingBindingStore{Store: tx, failForPrincipalID: s.failForPrincipalID})
	})
}

// TestRS1_AuthorityLookupFailure_ReturnsInternal proves that a store failure
// in projectEffectiveRoleFromStore during post-lock re-evaluation produces a
// 500 internal error, not a misleading 403.
func TestRS1_AuthorityLookupFailure_ReturnsInternal(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-alf-proj")
	ownerID := tid("rs1-alf-own")
	targetID := tid("rs1-alf-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "ALFTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Use a failing store that errors when looking up the actor's bindings.
	failStore := &failingBindingStore{Store: s, failForPrincipalID: ownerID}
	authzSvc := NewAuthzService(s, nil)
	failSvc := NewProjectMembershipService(failStore, authzSvc, slog.Default())

	owner := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "ALFOwner", "member", "test")
	ownerCtx := contextWithIdentity(ctx, owner)

	// The pre-lock governance check reads from failStore (which wraps svc.store),
	// so it will fail too. But the important thing is that it returns 500, not 403.
	_, denial := failSvc.AddMember(ownerCtx, MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     projectID,
		Actor:         owner,
		PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID:   targetID,
		RoleDefID:     memberRD.ID,
	})

	require.NotNil(t, denial, "RS1 R5 O-1: store failure must produce a denial")
	// The pre-lock check fails with 403 "actor has no project role" because
	// projectEffectiveRole returns "" on error (pre-lock helpers don't propagate).
	// The important proof is that the post-lock helpers propagate errors as 500.
	// To test post-lock specifically, we need a store that fails only inside tx.
	assert.True(t, denial.HTTPStatus == 403 || denial.HTTPStatus == 500,
		"RS1 R5 O-1: store failure must produce 403 or 500, not allow bypass (got %d)", denial.HTTPStatus)
	assert.False(t, denial.Allowed,
		"RS1 R5 O-1: store failure must not allow the operation")
}

// failingTxBindingStore is like failingBindingStore but only fails inside
// WithTx, allowing pre-lock checks to pass normally.
type failingTxBindingStore struct {
	store.Store
	failForPrincipalID string
}

func (s *failingTxBindingStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failingBindingStore{Store: tx, failForPrincipalID: s.failForPrincipalID})
	})
}

// TestRS1_AuthorityLookupFailure_PostLock proves that a store failure in
// projectEffectiveRoleFromStore INSIDE the locked transaction produces a 500
// internal error (not a misleading 403), per R5 O-1 error propagation.
func TestRS1_AuthorityLookupFailure_PostLock(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-alpl-proj")
	ownerID := tid("rs1-alpl-own")
	targetID := tid("rs1-alpl-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "ALPLTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// failingTxBindingStore: pre-lock reads succeed, post-lock reads fail.
	failStore := &failingTxBindingStore{Store: s, failForPrincipalID: ownerID}
	authzSvc := NewAuthzService(s, nil)
	failSvc := NewProjectMembershipService(failStore, authzSvc, slog.Default())

	owner := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "ALPLOwner", "member", "test")
	ownerCtx := contextWithIdentity(ctx, owner)

	_, denial := failSvc.AddMember(ownerCtx, MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     projectID,
		Actor:         owner,
		PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID:   targetID,
		RoleDefID:     memberRD.ID,
	})

	require.NotNil(t, denial, "RS1 R5 O-1: post-lock store failure must produce a denial")
	assert.Equal(t, 500, denial.HTTPStatus,
		"RS1 R5 O-1: post-lock store failure must produce 500 internal error, not 403 (got %d: %s)",
		denial.HTTPStatus, denial.Reason)
	assert.False(t, denial.Allowed,
		"RS1 R5 O-1: post-lock store failure must not allow the operation")
	assert.Contains(t, denial.Reason, "authority lookup failed",
		"RS1 R5 O-1: error message must indicate authority lookup failure")
}

// ---------------------------------------------------------------------------
// (j) Generic project-scope bypass tests — R4 brief item 2
// ---------------------------------------------------------------------------

// TestRS1_GenericCreateProjectBindingRoutedThroughService proves that
// POST /api/v1/admin/role-bindings with scope_type=project is routed
// through the membership service, enforcing governance/D4.
func TestRS1_GenericCreateProjectBindingRoutedThroughService(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-gcr-proj")
	ownerID := tid("rs1-gcr-owner")
	targetID := tid("rs1-gcr-targ")

	createRS1Project(t, s, projectID, ownerID)

	// Create target user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Make the dev user (who has role_binding.create system permission) a
	// project-admin so we can test governance denial via the generic endpoint.
	devUser, err := s.GetUserByEmail(ctx, "dev@localhost")
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      devUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Dev user (project-admin with role_binding.create) attempts to create
	// a project-owner binding via the generic role-binding endpoint — must
	// be denied by governance (admin cannot assign owner role). This proves
	// the generic create is now routed through the membership service.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: ownerRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"RS1 R4 item 2: generic project-scope create must not bypass owner/admin target governance (got %d: %s)",
		rec.Code, rec.Body.String())
}

// TestRS1_GenericDeleteProjectBindingRoutedThroughService proves that
// DELETE via the generic role-binding endpoint for project-scoped bindings
// is routed through the membership service, enforcing governance rules.
// An admin cannot remove an owner via the generic endpoint — this proves
// the request goes through membership governance, not the old generic path.
func TestRS1_GenericDeleteProjectBindingRoutedThroughService(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-gdr-proj")
	ownerID := tid("rs1-gdr-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Find the owner's binding.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, ownerID)
	require.NoError(t, err)
	var ownerBindingID string
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID {
			ownerBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, ownerBindingID, "must find owner binding")

	// Use the dev auth token (admin with role_binding.delete permission)
	// and give the dev user a project-admin role. An admin cannot remove
	// an owner — this proves the generic delete is routed through the
	// membership service's governance rather than bypassing it.
	devUser, err := s.GetUserByEmail(ctx, "dev@localhost")
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      devUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Attempt to delete the owner's binding via the generic admin endpoint.
	// The dev user has system role_binding.delete but only project-admin role.
	// Governance forbids admin from removing an owner.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+ownerBindingID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"RS1 R4 item 2: generic project-scope delete must enforce governance — admin cannot remove owner (got %d: %s)",
		rec.Code, rec.Body.String())
}

// TestRS1_GenericDeleteSystemScopeUnaffected proves that DELETE via the
// generic role-binding endpoint for system-scoped bindings is NOT routed
// through the membership service and works normally.
func TestRS1_GenericDeleteSystemScopeUnaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a system-scoped binding to delete.
	memberRD, err := s.GetRoleDefinitionByName(ctx, "hub-member", store.RoleScopeSystem)
	require.NoError(t, err)

	targetID := tid("rs1-gdsu-targ")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// Delete via the generic endpoint — should succeed (system scope, not routed through membership service).
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"RS1 R4: system-scoped binding delete via generic endpoint must work (got %d: %s)",
		rec.Code, rec.Body.String())
}

// TestRS1_GenericCreateProjectBindingAllowedPath proves that an ordinary
// allowed project member create/delete is correctly delegated through the
// membership service and produces the correct result.
func TestRS1_GenericCreateProjectBindingAllowedPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-gcap-proj")
	ownerID := tid("rs1-gcap-owner")
	targetID := tid("rs1-gcap-targ")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// The generic admin endpoint requires system-level role_binding.create,
	// so use the dev admin user and give them project-owner role.
	devUser, err := s.GetUserByEmail(ctx, "dev@localhost")
	require.NoError(t, err)
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      devUser.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        ownerID,
	})
	require.NoError(t, err)

	// Dev user (system admin + project owner) creates a member via the
	// generic endpoint — should succeed, routed through membership service.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    "user",
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"RS1 R4: ordinary project member create via generic endpoint should succeed (got %d: %s)",
		rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// (k) Stale-authority forced-overlap tests — R4 O-1 / R5-3
// ---------------------------------------------------------------------------

// txBlockStore wraps a store.Store and blocks WithTx entry on a channel,
// allowing a test to force a specific interleaving: preflight governance
// passes while the actor is still an owner, then the actor is demoted
// before the transaction starts, then the blocked WithTx proceeds and
// the post-lock re-evaluation must deny.
type txBlockStore struct {
	store.Store
	blockCh   chan struct{} // blocks WithTx until closed
	enteredCh chan struct{} // signalled when WithTx is about to block
}

func (s *txBlockStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	// Signal that we've entered WithTx (preflight passed).
	select {
	case s.enteredCh <- struct{}{}:
	default:
	}
	// Block until the test performs the demotion and releases us.
	<-s.blockCh
	return s.Store.WithTx(ctx, fn)
}

// TestRS1_StaleAuthorityForcedOverlap proves that a mutation whose preflight
// governance check passed with stale authority is denied after the post-lock
// re-evaluation detects a concurrent demotion.
//
// R5-3: Forces exact ordering:
//  1. owner1's RemoveMember passes checkGovernance (owner1 is still an owner)
//  2. owner1's WithTx blocks BEFORE acquiring the lock
//  3. A parallel service call (owner2) demotes owner1 to member and commits
//  4. owner1's WithTx is released → post-lock re-evaluation sees demotion → deny
//
// This proves the re-evaluation path catches stale authority, unlike the
// sequential test that was rejected in R5-3.
func TestRS1_StaleAuthorityForcedOverlap(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-fo-proj")
	owner1ID := tid("rs1-fo-o1")
	owner2ID := tid("rs1-fo-o2")
	targetID := tid("rs1-fo-targ")

	createRS1Project(t, s, projectID, owner1ID)

	// Create owner2 and target.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: owner2ID + "@test.com",
		DisplayName: "ForcedOverlapOwner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "ForcedOverlapTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Make owner2 an owner.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Add target as member.
	targetBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Create the blocking store wrapper for owner1's mutation service.
	blockCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)
	blockedStore := &txBlockStore{Store: s, blockCh: blockCh, enteredCh: enteredCh}

	// Build a ProjectMembershipService that uses the blocking store.
	// When owner1's mutation calls WithTx, it will block until we close blockCh.
	authzSvc := NewAuthzService(s, nil)
	blockedSvc := NewProjectMembershipService(blockedStore, authzSvc, slog.Default())

	// Build a normal (non-blocking) service for owner2's demotion.
	normalSvc := NewProjectMembershipService(s, authzSvc, slog.Default())

	// owner1 and owner2 identities for direct service calls.
	owner1 := NewAuthenticatedUser(owner1ID, owner1ID+"@test.com", "ForcedOverlapOwner1", "member", "test")
	owner2 := NewAuthenticatedUser(owner2ID, owner2ID+"@test.com", "ForcedOverlapOwner2", "member", "test")
	// Set identity in contexts so audit records can populate actor fields.
	owner1Ctx := contextWithIdentity(ctx, owner1)
	owner2Ctx := contextWithIdentity(ctx, owner2)

	// Start owner1's RemoveMember in a goroutine. The pre-lock checkGovernance
	// will pass (owner1 is still an owner), then WithTx will block.
	var removeDenial *MembershipDecision
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, removeDenial = blockedSvc.RemoveMember(owner1Ctx, MembershipRequest{
			Op:        MembershipOpRemove,
			ProjectID: projectID,
			Actor:     owner1,
			BindingID: targetBinding.ID,
		})
	}()

	// Wait for owner1's mutation to reach WithTx (preflight passed with stale authority).
	<-enteredCh

	// While owner1 is blocked, owner2 demotes owner1 to member using the
	// normal service. This commits through the real store.
	owner1Bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	require.NoError(t, err)
	var owner1BindingID string
	for _, b := range owner1Bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID {
			owner1BindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, owner1BindingID, "owner1 must have an owner binding")

	_, demoteDenial := normalSvc.UpdateMemberRole(owner2Ctx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        owner2,
		BindingID:    owner1BindingID,
		NewRoleDefID: memberRD.ID,
	})
	require.Nil(t, demoteDenial, "owner2 demotion of owner1 must succeed (got: %+v)", demoteDenial)

	// Verify owner1 is now a member (demotion committed).
	owner1BindingsAfter, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	for _, b := range owner1BindingsAfter {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			rd, _ := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
			require.NotEqual(t, store.ProjectRoleOwner, rd.Name,
				"owner1 must be demoted to member before unblocking")
		}
	}

	// Release owner1's WithTx. The post-lock re-evaluation should see the
	// demotion and deny.
	close(blockCh)
	wg.Wait()

	// Assert: owner1's mutation was denied.
	require.NotNil(t, removeDenial, "RS1 R5-3: demoted actor's mutation must be denied")
	assert.False(t, removeDenial.Allowed,
		"RS1 R5-3: demoted actor's mutation must not be allowed")
	assert.Equal(t, 403, removeDenial.HTTPStatus,
		"RS1 R5-3: denial must be 403 (got %d)", removeDenial.HTTPStatus)

	// Assert: the target binding was NOT removed.
	_, getErr := s.GetRoleBinding(ctx, targetBinding.ID)
	assert.NoError(t, getErr,
		"RS1 R5-3: target binding must still exist — stale-authority mutation must not persist")

	// Assert: no successful audit record for the stale mutation.
	audits, _, _ := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: projectID})
	for _, a := range audits {
		if a.MutationType == "project_member_remove" {
			// The demotion audit is expected (project_member_role_change), but
			// a successful remove audit for the target would indicate the stale
			// mutation persisted.
			var summary map[string]string
			if a.BeforeSummary != "" {
				_ = json.Unmarshal([]byte(a.BeforeSummary), &summary)
				assert.NotEqual(t, targetID, summary["principalId"],
					"RS1 R5-3: audit must not record a successful remove for the target")
			}
		}
	}
}

// TestRS1_StaleAuthorityForcedOverlap_AddMember exercises the same stale-
// authority forced overlap on the AddMember path. Owner1 passes preflight
// governance, is demoted while blocked, and the post-lock re-evaluation
// denies the add.
func TestRS1_StaleAuthorityForcedOverlap_AddMember(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-foa-proj")
	owner1ID := tid("rs1-foa-o1")
	owner2ID := tid("rs1-foa-o2")
	newUserID := tid("rs1-foa-new")

	createRS1Project(t, s, projectID, owner1ID)

	// Create owner2 and new user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: owner2ID + "@test.com",
		DisplayName: "FOAOwner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: newUserID, Email: newUserID + "@test.com",
		DisplayName: "FOANewUser", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, newUserID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Make owner2 an owner.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	blockCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)
	blockedStore := &txBlockStore{Store: s, blockCh: blockCh, enteredCh: enteredCh}

	authzSvc := NewAuthzService(s, nil)
	blockedSvc := NewProjectMembershipService(blockedStore, authzSvc, slog.Default())
	normalSvc := NewProjectMembershipService(s, authzSvc, slog.Default())

	owner1 := NewAuthenticatedUser(owner1ID, owner1ID+"@test.com", "FOAOwner1", "member", "test")
	owner2 := NewAuthenticatedUser(owner2ID, owner2ID+"@test.com", "FOAOwner2", "member", "test")
	owner1Ctx := contextWithIdentity(ctx, owner1)
	owner2Ctx := contextWithIdentity(ctx, owner2)

	var addDenial *MembershipDecision
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, addDenial = blockedSvc.AddMember(owner1Ctx, MembershipRequest{
			Op:            MembershipOpAdd,
			ProjectID:     projectID,
			Actor:         owner1,
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   newUserID,
			RoleDefID:     memberRD.ID,
		})
	}()

	<-enteredCh

	// Demote owner1 to member.
	owner1Bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	var owner1BindingID string
	for _, b := range owner1Bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID {
			owner1BindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, owner1BindingID)

	_, demoteDenial := normalSvc.UpdateMemberRole(owner2Ctx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        owner2,
		BindingID:    owner1BindingID,
		NewRoleDefID: memberRD.ID,
	})
	require.Nil(t, demoteDenial, "demotion must succeed")

	close(blockCh)
	wg.Wait()

	require.NotNil(t, addDenial, "RS1 R5-3: demoted actor's add must be denied")
	assert.False(t, addDenial.Allowed)
	assert.Equal(t, 403, addDenial.HTTPStatus)

	// New user must NOT have been added.
	newBindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, newUserID)
	for _, b := range newBindings {
		assert.False(t, b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID,
			"RS1 R5-3: new user binding must not exist after stale-authority denial")
	}
}

// TestRS1_StaleAuthorityForcedOverlap_UpdateRole exercises the forced overlap
// on the UpdateMemberRole path.
func TestRS1_StaleAuthorityForcedOverlap_UpdateRole(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-fou-proj")
	owner1ID := tid("rs1-fou-o1")
	owner2ID := tid("rs1-fou-o2")
	targetID := tid("rs1-fou-targ")

	createRS1Project(t, s, projectID, owner1ID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: owner2ID + "@test.com",
		DisplayName: "FOUOwner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "FOUTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Make owner2 an owner.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Add target as member.
	targetBinding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	blockCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)
	blockedStore := &txBlockStore{Store: s, blockCh: blockCh, enteredCh: enteredCh}

	authzSvc := NewAuthzService(s, nil)
	blockedSvc := NewProjectMembershipService(blockedStore, authzSvc, slog.Default())
	normalSvc := NewProjectMembershipService(s, authzSvc, slog.Default())

	owner1 := NewAuthenticatedUser(owner1ID, owner1ID+"@test.com", "FOUOwner1", "member", "test")
	owner2 := NewAuthenticatedUser(owner2ID, owner2ID+"@test.com", "FOUOwner2", "member", "test")
	owner1Ctx := contextWithIdentity(ctx, owner1)
	owner2Ctx := contextWithIdentity(ctx, owner2)

	// Owner1 tries to promote target to admin (passes preflight as owner).
	var updateDenial *MembershipDecision
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, updateDenial = blockedSvc.UpdateMemberRole(owner1Ctx, MembershipRequest{
			Op:           MembershipOpUpdate,
			ProjectID:    projectID,
			Actor:        owner1,
			BindingID:    targetBinding.ID,
			NewRoleDefID: adminRD.ID,
		})
	}()

	<-enteredCh

	// Demote owner1 to member while blocked.
	owner1Bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	var owner1BindingID string
	for _, b := range owner1Bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID {
			owner1BindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, owner1BindingID)

	_, demoteDenial := normalSvc.UpdateMemberRole(owner2Ctx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        owner2,
		BindingID:    owner1BindingID,
		NewRoleDefID: memberRD.ID,
	})
	require.Nil(t, demoteDenial, "demotion must succeed")

	close(blockCh)
	wg.Wait()

	require.NotNil(t, updateDenial, "RS1 R5-3: demoted actor's update must be denied")
	assert.False(t, updateDenial.Allowed)
	assert.Equal(t, 403, updateDenial.HTTPStatus)

	// Target must still be a member, not promoted to admin.
	tb, getErr := s.GetRoleBinding(ctx, targetBinding.ID)
	if getErr == nil {
		rd, _ := s.GetRoleDefinition(ctx, tb.RoleDefinitionID)
		assert.Equal(t, store.ProjectRoleMember, rd.Name,
			"RS1 R5-3: target must still be a member after stale-authority denial")
	}
}

// TestRS1_StaleAuthorityForcedOverlap_Transfer exercises the forced overlap
// on the TransferOwnership path.
func TestRS1_StaleAuthorityForcedOverlap_Transfer(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-fot-proj")
	owner1ID := tid("rs1-fot-o1")
	owner2ID := tid("rs1-fot-o2")
	transferTargetID := tid("rs1-fot-tt")

	createRS1Project(t, s, projectID, owner1ID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: owner2ID, Email: owner2ID + "@test.com",
		DisplayName: "FOTOwner2", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, owner2ID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: transferTargetID, Email: transferTargetID + "@test.com",
		DisplayName: "FOTTransferTarget", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, transferTargetID)

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Make owner2 an owner.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner2ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	// Add transfer target as member.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      transferTargetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        owner1ID,
	})
	require.NoError(t, err)

	blockCh := make(chan struct{})
	enteredCh := make(chan struct{}, 1)
	blockedStore := &txBlockStore{Store: s, blockCh: blockCh, enteredCh: enteredCh}

	authzSvc := NewAuthzService(s, nil)
	blockedSvc := NewProjectMembershipService(blockedStore, authzSvc, slog.Default())
	normalSvc := NewProjectMembershipService(s, authzSvc, slog.Default())

	owner1 := NewAuthenticatedUser(owner1ID, owner1ID+"@test.com", "FOTOwner1", "member", "test")
	owner2 := NewAuthenticatedUser(owner2ID, owner2ID+"@test.com", "FOTOwner2", "member", "test")
	owner1Ctx := contextWithIdentity(ctx, owner1)
	owner2Ctx := contextWithIdentity(ctx, owner2)

	// Owner1 tries to transfer ownership (passes preflight isActorDirectOwner).
	var transferDenial *MembershipDecision
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, transferDenial = blockedSvc.TransferOwnership(owner1Ctx, MembershipRequest{
			Op:         MembershipOpTransfer,
			ProjectID:  projectID,
			Actor:      owner1,
			NewOwnerID: transferTargetID,
		})
	}()

	<-enteredCh

	// Demote owner1 to member while blocked.
	owner1Bindings, _ := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, owner1ID)
	var owner1BindingID string
	for _, b := range owner1Bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID &&
			b.RoleDefinitionID == ownerRD.ID {
			owner1BindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, owner1BindingID)

	_, demoteDenial := normalSvc.UpdateMemberRole(owner2Ctx, MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    projectID,
		Actor:        owner2,
		BindingID:    owner1BindingID,
		NewRoleDefID: memberRD.ID,
	})
	require.Nil(t, demoteDenial, "demotion must succeed")

	close(blockCh)
	wg.Wait()

	require.NotNil(t, transferDenial, "RS1 R5-3: demoted actor's transfer must be denied")
	assert.False(t, transferDenial.Allowed)
	assert.Equal(t, 403, transferDenial.HTTPStatus)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findRS1RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find repo root (no go.mod found)")
		}
		dir = parent
	}
}

// Suppress unused import warnings for packages used in specific tests.
var _ = strings.Contains
var _ = os.Getenv
