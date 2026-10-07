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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ownerGrantDenyText = "of the project owner role"

type ownerGrantFixture struct {
	srv  *Server
	s    store.Store
	user *store.User
}

func newOwnerGrantFixture(t *testing.T, name string) ownerGrantFixture {
	t.Helper()
	srv, s := testServer(t)
	return ownerGrantFixture{srv: srv, s: s, user: newHubMemberUser(t, s, "og-"+name)}
}

func ownerRolePermissions(t *testing.T, s store.Store) []string {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	require.NotEmpty(t, rd.Permissions)
	return rd.Permissions
}

// hubUAT returns a hub-boundary access token identity for the fixture user
// whose V1 ceiling is exactly ids.
func (f ownerGrantFixture) hubUAT(ids ...string) *ScopedUserIdentity {
	return NewScopedUserIdentityWithBoundary(authUser(f.user), TokenBoundary{Kind: BoundaryKindHub}, nil, "uat-"+f.user.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ids})
}

// fullCoverage is a ceiling covering project create and every owner
// permission; partialCoverage lacks one owner permission.
func (f ownerGrantFixture) fullCoverage(t *testing.T) []string {
	return append([]string{"project.create", "project.read"}, ownerRolePermissions(t, f.s)...)
}

func (f ownerGrantFixture) partialCoverage(t *testing.T) []string {
	ids := f.fullCoverage(t)
	owner := ownerRolePermissions(t, f.s)
	drop := owner[len(owner)-1]
	out := ids[:0:0]
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// assertNoOwnerRows asserts that no project with slug exists and the user
// holds no owner binding or owner audit record outside except.
func (f ownerGrantFixture) assertNoOwnerRows(t *testing.T, slug, except string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.s.GetProjectBySlug(ctx, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no project row")
	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.user.ID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID != except {
			t.Errorf("unexpected project binding %s on %s", b.ID, b.ScopeID)
		}
	}
	assert.Empty(t, f.ownerAudits(t, except), "no owner audit record")
}

// ownerAudits returns the project_member_add records naming the user,
// outside project except.
func (f ownerGrantFixture) ownerAudits(t *testing.T, except string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: "project_member_add"})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID != except && strings.Contains(r.AfterSummary, f.user.ID) {
			out = append(out, r)
		}
	}
	return out
}

// The HTTP subtests below check the end-to-end result for a hub-boundary
// UAT without owner coverage: 403 and no rows. That 403 comes from the
// project.create authorization, which such a token does not pass, so these
// subtests do not isolate the owner-coverage check. TestOwnerGrantCoverageCheck
// tests the check itself, and TestOwnerGrantCheckPrecedesOwnerWrite pins its
// call sites.
func TestOwnerBindingRequiresCanDelegate(t *testing.T) {
	t.Run("create denied without owner coverage", func(t *testing.T) {
		f := newOwnerGrantFixture(t, "deny")
		setUserProjectQuotaCeiling(t, f.s, 5)
		rec := requestAsIdentity(t, f.srv, f.hubUAT(f.partialCoverage(t)...), http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "og deny"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		f.assertNoOwnerRows(t, api.Slugify("og deny"), "")
		assert.Zero(t, countProjectQuotaReservations(t, f.s, f.user.ID), "no quota consumed")
	})

	t.Run("session create unchanged", func(t *testing.T) {
		f := newOwnerGrantFixture(t, "session")
		rec := requestAsIdentity(t, f.srv, authUser(f.user), http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "og session"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		recs := f.ownerAudits(t, "")
		require.Len(t, recs, 1)
		assert.Equal(t, f.user.ID, recs[0].ActorPrincipalID)
	})

	t.Run("register denied without owner coverage", func(t *testing.T) {
		f := newOwnerGrantFixture(t, "register")
		rec := requestAsIdentity(t, f.srv, f.hubUAT(f.partialCoverage(t)...), http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "og register"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		f.assertNoOwnerRows(t, api.Slugify("og register"), "")
	})

	t.Run("clone denied without owner coverage", func(t *testing.T) {
		f := newOwnerGrantFixture(t, "clone")
		ctx := context.Background()
		src := &store.Project{ID: tid("og-clone-src"), Name: "og clone src", Slug: "og-clone-src", CreatedBy: f.user.ID, OwnerID: f.user.ID}
		require.NoError(t, f.s.CreateProject(ctx, src))
		require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, src.ID, f.user.ID))
		rec := requestAsIdentity(t, f.srv, f.hubUAT(f.partialCoverage(t)...), http.MethodPost, "/api/v1/projects/"+src.ID+"/clone", CloneProjectRequest{Name: "og clone"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		f.assertNoOwnerRows(t, api.Slugify("og clone"), src.ID)
	})
}

// The owner-grant check itself: a ceiling credential must keep every owner
// permission; every other identity is unchanged.
func TestOwnerGrantCoverageCheck(t *testing.T) {
	f := newOwnerGrantFixture(t, "check")
	check := func(identity Identity) (bool, int, string) {
		rec := httptest.NewRecorder()
		ok := f.srv.authorizeProjectOwnerGrant(rec, contextWithIdentity(context.Background(), identity))
		return ok, rec.Code, rec.Body.String()
	}

	ok, code, body := check(f.hubUAT(f.partialCoverage(t)...))
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, ownerGrantDenyText)

	ok, _, body = check(f.hubUAT(f.fullCoverage(t)...))
	assert.True(t, ok, body)

	projectUAT := NewScopedUserIdentityWithCeiling(authUser(f.user), tid("og-check-proj"), nil, "uat-p-"+f.user.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"project.read"}})
	ok, code, _ = check(projectUAT)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, code)

	ok, _, _ = check(authUser(f.user))
	assert.True(t, ok, "session unchanged")
	ok, _, _ = check(dcAgentIdentity(tid("og-check-agent"), tid("og-check-proj"), AgentRoleFull))
	assert.True(t, ok, "non-user identities are gated by project create authorization, not here")
}

// No credential that passes project.create authorization over HTTP lacks
// owner coverage: a hub-boundary UAT is refused project.create ("token not
// scoped for this project"), and a session or agent identity is not subject
// to the check. So the call sites are pinned structurally. In each of
// createProject, handleProjectRegister and handleProjectClone, a statement
// "if !s.authorizeProjectOwnerGrant(w, ctx) { return }" must come before every
// createProjectWithOwner call, in a block that encloses that call.
func TestOwnerGrantCheckPrecedesOwnerWrite(t *testing.T) {
	_, files := parseHubProduction(t)
	for _, fn := range []string{"Server.createProject", "Server.handleProjectRegister", "Server.handleProjectClone"} {
		decl := findHubFuncDecl(files, fn)
		require.NotNil(t, decl, fn)
		guards, writes := ownerGrantGuardsAndWrites(decl)
		require.NotEmpty(t, writes, "%s calls createProjectWithOwner", fn)
		for _, w := range writes {
			assert.True(t, ownerGrantGuarded(guards, w), "%s: createProjectWithOwner at offset %d is not preceded by an enclosing authorizeProjectOwnerGrant guard", fn, w)
		}
	}
}

// The structural matcher rejects a guard that cannot return and a guard in
// a block that does not enclose the write.
func TestOwnerGrantGuardMatcher(t *testing.T) {
	cases := map[string]bool{
		"if !s.authorizeProjectOwnerGrant(w, ctx) { return }\n_ = s.createProjectWithOwner(ctx, p, id)":          true,
		"if false && !s.authorizeProjectOwnerGrant(w, ctx) { return }\n_ = s.createProjectWithOwner(ctx, p, id)": false,
		"if !s.authorizeProjectOwnerGrant(w, ctx) { log() }\n_ = s.createProjectWithOwner(ctx, p, id)":           false,
		"_ = s.authorizeProjectOwnerGrant(w, ctx)\n_ = s.createProjectWithOwner(ctx, p, id)":                     false,
		"if c { if !s.authorizeProjectOwnerGrant(w, ctx) { return } }\n_ = s.createProjectWithOwner(ctx, p, id)": false,
		"_ = s.createProjectWithOwner(ctx, p, id)\nif !s.authorizeProjectOwnerGrant(w, ctx) { return }":          false,
		"if !s.authorizeProjectOwnerGrant(w, ctx) { return }\nif c { _ = s.createProjectWithOwner(ctx, p, id) }": true,
	}
	for body, want := range cases {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nfunc (s *S) h() {\n"+body+"\n}\n", 0)
		require.NoError(t, err, body)
		decl := f.Decls[0].(*ast.FuncDecl)
		guards, writes := ownerGrantGuardsAndWrites(decl)
		require.Len(t, writes, 1, body)
		assert.Equal(t, want, ownerGrantGuarded(guards, writes[0]), body)
	}
}

// findHubFuncDecl returns the function declaration named name ("Recv.Name"
// or "Name") in files.
func findHubFuncDecl(files []*ast.File, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil && declReceiverName(fn) == name {
				return fn
			}
		}
	}
	return nil
}

// ownerGrantGuard is an "if !….authorizeProjectOwnerGrant(…) { …; return }"
// statement: its end, and the end of the block that holds it.
type ownerGrantGuard struct {
	end, blockEnd token.Pos
}

// ownerGrantGuardsAndWrites returns the owner-grant guards in decl and the
// positions of its createProjectWithOwner calls.
func ownerGrantGuardsAndWrites(decl *ast.FuncDecl) ([]ownerGrantGuard, []token.Pos) {
	var guards []ownerGrantGuard
	var writes []token.Pos
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			for _, st := range n.List {
				if isOwnerGrantGuard(st) {
					guards = append(guards, ownerGrantGuard{end: st.End(), blockEnd: n.End()})
				}
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "createProjectWithOwner" {
				writes = append(writes, n.Pos())
			}
		}
		return true
	})
	return guards, writes
}

// isOwnerGrantGuard reports whether st is exactly
// "if !X.authorizeProjectOwnerGrant(…) { …; return }" with no init statement
// and no else branch.
func isOwnerGrantGuard(st ast.Stmt) bool {
	ifs, ok := st.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || len(ifs.Body.List) == 0 {
		return false
	}
	not, ok := ifs.Cond.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return false
	}
	call, ok := not.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "authorizeProjectOwnerGrant" {
		return false
	}
	_, ok = ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt)
	return ok
}

// ownerGrantGuarded reports whether a guard ends before write in a block
// that encloses write.
func ownerGrantGuarded(guards []ownerGrantGuard, write token.Pos) bool {
	for _, g := range guards {
		if g.end <= write && write < g.blockEnd {
			return true
		}
	}
	return false
}

// An injected failure of the owner binding's audit write rolls back the
// project row and the binding, and releases the quota reservation.
func TestOwnerBindingAuditFailureRollsBack(t *testing.T) {
	f := newOwnerGrantFixture(t, "audit")
	setUserProjectQuotaCeiling(t, f.s, 5)
	real := f.srv.store
	f.srv.store = &createTxFaultStore{Store: real, auditErrFor: "project_member_add"}

	rec := requestAsIdentity(t, f.srv, authUser(f.user), http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "og audit"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	f.assertNoOwnerRows(t, api.Slugify("og audit"), "")
	assert.Zero(t, countProjectQuotaReservations(t, f.s, f.user.ID), "quota released")

	rec = requestAsIdentity(t, f.srv, authUser(f.user), http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "og audit register"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	f.assertNoOwnerRows(t, api.Slugify("og audit register"), "")
	assert.Zero(t, countProjectQuotaReservations(t, f.s, f.user.ID), "quota released")

	// Control: without the fault the project, binding and audit commit
	// together.
	f.srv.store = real
	rec = requestAsIdentity(t, f.srv, authUser(f.user), http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "og audit"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, f.ownerAudits(t, ""), 1)
	assert.Equal(t, int64(1), countProjectQuotaReservations(t, f.s, f.user.ID))
}
