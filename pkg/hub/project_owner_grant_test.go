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
// permission; every other identity is unchanged. Create, register and clone
// all call authorizeProjectOwnerGrant before writing.
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
