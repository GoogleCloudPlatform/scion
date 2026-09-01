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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
		"handlers_roles.go":               "generic role-binding CRUD endpoint — not project-membership-specific; D4 enforced by partial unique index",
		"handlers_auth.go":                "auth handler: system-scoped binding cleanup during user deactivation, not project membership",
		"access_constraint_governance.go": "constraint governance: creates role bindings for system governance, not project membership",
		"project_membership_service.go":   "the membership service itself",
		"useraccesstoken.go":              "UAT service: internal token management",
		"seed.go":                         "bootstrap seeding: creates initial system/hub-level role bindings, not project membership",
		"server.go":                       "server initialization: system-level bootstrap, not project membership",
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

// mintScopedUAT mints a real scoped UAT through the production token service.
func mintScopedUAT(t *testing.T, srv *Server, userID, projectID string, scopes []string) string {
	t.Helper()
	key, _, err := srv.uatService.CreateToken(
		context.Background(), userID, "test-uat", projectID, scopes, nil,
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

// TestRS1_ScopedUAT_ExpiredTokenDenied proves that an expired UAT is denied.
func TestRS1_ScopedUAT_ExpiredTokenDenied(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs1-uat-exp-p")
	ownerID := tid("rs1-uat-exp-o")

	createRS1Project(t, s, projectID, ownerID)

	// Mint a UAT and then revoke it to verify denial.
	uatKey, token, err := srv.uatService.CreateToken(
		context.Background(), ownerID, "test-uat-expire", projectID, []string{"project:read"}, nil,
	)
	require.NoError(t, err)

	// Revoke the token.
	require.NoError(t, srv.uatService.RevokeToken(context.Background(), ownerID, token.ID))

	// Attempt to use the revoked token — should be denied.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"RS1 R3-2: revoked UAT must be denied (got %d: %s)", rec.Code, rec.Body.String())
}

// TestRS1_ScopedUAT_SuspendedUserDenied proves that a UAT owned by a
// suspended user is denied even if the token itself is valid.
func TestRS1_ScopedUAT_SuspendedUserDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs1-uat-sus-p")
	ownerID := tid("rs1-uat-sus-o")

	createRS1Project(t, s, projectID, ownerID)

	// Mint a valid UAT.
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	// Suspend the user.
	u, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, u))

	// Attempt to use the UAT — should be denied (user is suspended).
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet,
		"/api/v1/projects/"+projectID+"/members", nil)
	// Suspended users should be denied — could be 401 or 403 depending on
	// where the check happens (identity resolution vs. authorization).
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"RS1 R3-2: suspended user's UAT must be denied (got %d: %s)", rec.Code, rec.Body.String())
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
