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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project creation by user access token. Project create, project register
// and the new project of a clone check project.create as a collection-level
// create at the hub scope: a token needs the project:create selector on a
// hub boundary, and its holder needs live project.create authority. The
// creator is bound to the project-owner role, so a token must also cover
// every permission of that role (projectOwnerGrantDenial). A clone also
// needs project.read on the source and, for a token, the project:clone
// selector.

// pctOwnerPerms are the permissions of the narrowed project-owner role in
// these tests; each maps to a selector, so a token can cover the role.
var pctOwnerPerms = []string{"project.read", "agent.read"}

// pctFixture is a server whose project-owner role is narrowed to
// pctOwnerPerms, and a holder: a hub member (which holds project.create)
// with the project-admin role on its home project.
type pctFixture struct {
	srv    *Server
	s      store.Store
	holder string
	home   string
}

func newPCTFixture(t *testing.T, name string) pctFixture {
	t.Helper()
	srv, s := testServer(t)
	ownerRD, err := s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(context.Background(), ownerRD.ID, pctOwnerPerms))
	holder := projectOpsUser(t, s, name+"-holder", false)
	home := projectOpsProject(t, s, name+"-home")
	projectOpsGrant(t, s, holder, home, store.ProjectRoleAdmin)
	return pctFixture{srv: srv, s: s, holder: holder, home: home}
}

// pctMintErr mints a token through the token service and returns the mint
// error.
func pctMintErr(srv *Server, userID string, boundary TokenBoundary, scopes ...string) error {
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "pct-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	return err
}

// pctRequireMintRefused asserts that a mint was refused, with the uniform
// refusal or a selector violation.
func pctRequireMintRefused(t *testing.T, err error, label string) {
	t.Helper()
	require.Error(t, err, label)
	assert.True(t, errors.Is(err, ErrUATProjectForbidden) || errors.Is(err, ErrUATScopeViolation), "%s: %v", label, err)
}

// pctCreateDenialReason resolves the token key to its identity and returns
// the reason projectCreateDecision refuses it with.
func pctCreateDenialReason(t *testing.T, srv *Server, key string) string {
	t.Helper()
	ctx := context.Background()
	identity, err := srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	decision := srv.projectCreateDecision(ctx, identity)
	require.False(t, decision.Allowed, "project.create admission for the token")
	return decision.Reason
}

// pctSession sends a request as userID over an interactive session through
// the full server handler.
func pctSession(t *testing.T, srv *Server, s store.Store, userID, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return doRequestAsUser(t, srv, u, method, path, body)
}

// pctProjectBySlug returns the project with slug, or nil.
func pctProjectBySlug(t *testing.T, s store.Store, slug string) *store.Project {
	t.Helper()
	p, err := s.GetProjectBySlug(context.Background(), slug)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return nil
	}
	return p
}

// pctAssertCreated asserts that a project with slug exists, was created by
// userID, and binds userID to the project-owner role.
func pctAssertCreated(t *testing.T, s store.Store, slug, userID string) {
	t.Helper()
	p := pctProjectBySlug(t, s, slug)
	require.NotNil(t, p, "project %s created", slug)
	assert.Equal(t, userID, p.CreatedBy)
	roles := pctProjectRoles(t, s, userID, p.ID)
	assert.Equal(t, []string{store.ProjectRoleOwner}, roles, "creator holds the owner role")
}

// pctProjectRoles returns the names of userID's role bindings on projectID.
func pctProjectRoles(t *testing.T, s store.Store, userID, projectID string) []string {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var names []string
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeProject || b.ScopeID != projectID {
			continue
		}
		rd, err := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, err)
		names = append(names, rd.Name)
	}
	return names
}

// pctAssertRefused asserts a 403 and that no project with slug exists.
func pctAssertRefused(t *testing.T, s store.Store, rec *httptest.ResponseRecorder, slug, label string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	assert.Nil(t, pctProjectBySlug(t, s, slug), "%s: nothing written", label)
}

// pctUnmappedOwnerPerms returns the seeded project-owner permissions that
// have no selector.
func pctUnmappedOwnerPerms() []string {
	var unmapped []string
	for _, p := range projectOwnerPermissionIDs() {
		if bearerMatrixSelector(p) == "" {
			unmapped = append(unmapped, p)
		}
	}
	return unmapped
}

// TestProjectCreateToken_RequiresCreateSelectorAndHubBoundary pins that
// POST /api/v1/projects admits a token only with project:create on a hub
// boundary, held by a holder with live project.create authority, and only
// when the token covers the project-owner role the creator is bound to.
// Sessions need project.create, as for every create.
func TestProjectCreateToken_RequiresCreateSelectorAndHubBoundary(t *testing.T) {
	f := newPCTFixture(t, "pct-create")
	create := func(key, name string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, f.srv, key, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	}

	// project:create is hub-only and needs project.create authority.
	pctRequireMintRefused(t, pctMintErr(f.srv, f.holder, projectBoundary(f.home), "project:create"), "project:create on a project boundary")
	plain := newPlainUser(t, f.s, "pct-create-plain")
	pctRequireMintRefused(t, pctMintErr(f.srv, plain.ID, hubBoundary(), "project:create"), "project:create for a user without project.create")

	t.Run("admitted with project:create on a hub boundary", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:read", "agent:read")
		rec := create(key, "pct create ok")
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		pctAssertCreated(t, f.s, api.Slugify("pct create ok"), f.holder)
	})

	t.Run("refused without project:create", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:read", "agent:read", "project:update")
		pctAssertRefused(t, f.s, create(key, "pct create nosel"), api.Slugify("pct create nosel"), "hub token without project:create")
	})

	t.Run("refused on a project boundary", func(t *testing.T) {
		// A minted project token cannot carry project:create, so this
		// first request is a selector control.
		key := projectOpsMint(t, f.srv, f.holder, projectBoundary(f.home), "project:read", "agent:read")
		pctAssertRefused(t, f.s, create(key, "pct create ptok"), api.Slugify("pct create ptok"), "project token")
		// A stored project-boundary row carrying project:create is refused
		// at request time by its boundary: project creation is a hub-level
		// request.
		stored := uatpInsertLegacyToken(t, f.s, f.holder, f.home, []string{"project:create", "project:read", "agent:read"})
		pctAssertRefused(t, f.s, create(stored, "pct create stored"), api.Slugify("pct create stored"), "stored project-boundary row")
		assert.Equal(t, bearerReasonHubLevelResource, pctCreateDenialReason(t, f.srv, stored))
	})

	t.Run("refused when the token does not cover the owner role", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:read")
		rec := create(key, "pct create partial")
		pctAssertRefused(t, f.s, rec, api.Slugify("pct create partial"), "token without agent:read")
		assert.Contains(t, rec.Body.String(), ownerGrantDenyText)
	})

	t.Run("sessions need project.create", func(t *testing.T) {
		rec := pctSession(t, f.srv, f.s, f.holder, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "pct create session"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		pctAssertCreated(t, f.s, api.Slugify("pct create session"), f.holder)
		rec = pctSession(t, f.srv, f.s, plain.ID, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "pct create plain"})
		pctAssertRefused(t, f.s, rec, api.Slugify("pct create plain"), "session without project.create")
	})
}

// TestProjectCreateToken_SeededOwnerRoleNeedsUnmappedPermissions pins that
// with the seeded project-owner role no token can create a project: the
// role carries permissions with no selector, so a token holding
// project:create and every mintable owner selector is refused by the owner
// coverage check and nothing is written. The assertion on the unmapped set
// fails once every owner permission has a selector, so this pin is then
// rewritten to a positive one.
func TestProjectCreateToken_SeededOwnerRoleNeedsUnmappedPermissions(t *testing.T) {
	srv, s := testServer(t)
	holder := projectOpsUser(t, s, "pct-seeded-holder", false)
	home := projectOpsProject(t, s, "pct-seeded-home")
	projectOpsGrant(t, s, holder, home, store.ProjectRoleOwner)

	unmapped := pctUnmappedOwnerPerms()
	require.NotEmpty(t, unmapped, "the seeded project-owner role carries permissions with no selector")
	// The token carries project:create and every owner selector the holder
	// can select on a hub boundary.
	scopes := []string{"project:create"}
	for _, p := range projectOwnerPermissionIDs() {
		sel := bearerMatrixSelector(p)
		if sel == "" || !permissionEligibleForBoundary(p, BoundaryKindHub) {
			continue
		}
		if pctMintErr(srv, holder, hubBoundary(), sel) == nil {
			scopes = append(scopes, sel)
		}
	}
	key := projectOpsMint(t, srv, holder, hubBoundary(), scopes...)

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "pct seeded"})
	pctAssertRefused(t, s, rec, api.Slugify("pct seeded"), "create")
	assert.Contains(t, rec.Body.String(), ownerGrantDenyText)
	rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "pct seeded register"})
	pctAssertRefused(t, s, rec, api.Slugify("pct seeded register"), "register")
	assert.Contains(t, rec.Body.String(), ownerGrantDenyText)

	// The holder's session creates the project.
	rec = pctSession(t, srv, s, holder, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "pct seeded session"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestProjectCreateToken_IdempotentPathRequiresUpdate pins that a create
// naming an existing project by id needs project.update on that project: a
// token needs project:update and its holder live project.update on the
// project, beyond project:create; the existing project is returned as
// stored and the caller gains no role on it.
func TestProjectCreateToken_IdempotentPathRequiresUpdate(t *testing.T) {
	f := newPCTFixture(t, "pct-again")
	other := projectOpsProject(t, f.s, "pct-again-other")
	body := func(id string) CreateProjectRequest { return CreateProjectRequest{ID: id, Name: "pct idem"} }

	createOnly := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:read", "agent:read")
	rec := doRequestWithToken(t, f.srv, createOnly, http.MethodPost, "/api/v1/projects", body(f.home))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token without project:update: %s", rec.Body.String())

	withUpdate := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:update", "project:read", "agent:read")
	rec = doRequestWithToken(t, f.srv, withUpdate, http.MethodPost, "/api/v1/projects", body(f.home))
	require.Equal(t, http.StatusOK, rec.Code, "token with project:update: %s", rec.Body.String())
	var got store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, f.home, got.ID)
	assert.Equal(t, []string{store.ProjectRoleAdmin}, pctProjectRoles(t, f.s, f.holder, f.home), "no role added")

	// project.update on a project the holder has no role on is refused.
	rec = doRequestWithToken(t, f.srv, withUpdate, http.MethodPost, "/api/v1/projects", body(other))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token on a project without a role: %s", rec.Body.String())
	assert.Empty(t, pctProjectRoles(t, f.s, f.holder, other), "no role added")

	// project:update alone does not reach the idempotent path.
	updateOnly := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:update", "project:read")
	rec = doRequestWithToken(t, f.srv, updateOnly, http.MethodPost, "/api/v1/projects", body(f.home))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token without project:create: %s", rec.Body.String())

	// Sessions: the same project.update rule.
	rec = pctSession(t, f.srv, f.s, f.holder, http.MethodPost, "/api/v1/projects", body(f.home))
	assert.Equal(t, http.StatusOK, rec.Code, "session with project.update: %s", rec.Body.String())
	rec = pctSession(t, f.srv, f.s, f.holder, http.MethodPost, "/api/v1/projects", body(other))
	assert.Equal(t, http.StatusForbidden, rec.Code, "session without project.update: %s", rec.Body.String())
}

// TestProjectRegisterToken_EnforcesProjectCreate pins that project
// register checks project.create, the same admission as project create:
// a token needs project:create on a hub boundary. The route metadata names
// project.create, and project.register is a reserved registry row that no
// route checks. An existing project named by id needs project.update.
func TestProjectRegisterToken_EnforcesProjectCreate(t *testing.T) {
	meta := routeMetadataTable["/api/v1/projects/register"]
	assert.Equal(t, "project.create", meta.Permission)
	assert.Equal(t, "create", meta.Action)
	for _, p := range permissions.Registry {
		switch p.ID {
		case "project.register":
			assert.True(t, p.IsReserved(), "project.register is reserved")
			assert.Empty(t, p.UATScope)
		case "project.create":
			assert.Equal(t, "project:create", p.UATScope)
		}
	}

	// register, clone and list are hub-level collection actions: none
	// applies to an existing project target.
	for _, id := range []string{"project.register", "project.clone", "project.list", "project.create"} {
		applies, listed := permissions.AppliesToExistingProjectTarget(id)
		assert.True(t, listed, "%s has an applicability entry", id)
		assert.False(t, applies, "%s does not apply to an existing project target", id)
	}

	f := newPCTFixture(t, "pct-reg")
	register := func(key string, req RegisterProjectRequest) *httptest.ResponseRecorder {
		return doRequestWithToken(t, f.srv, key, http.MethodPost, "/api/v1/projects/register", req)
	}

	t.Run("admitted with project:create on a hub boundary", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:read", "agent:read")
		rec := register(key, RegisterProjectRequest{Name: "pct reg ok"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp RegisterProjectResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.True(t, resp.Created)
		pctAssertCreated(t, f.s, api.Slugify("pct reg ok"), f.holder)
	})

	t.Run("refused without project:create", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:read", "agent:read", "project:update")
		pctAssertRefused(t, f.s, register(key, RegisterProjectRequest{Name: "pct reg nosel"}), api.Slugify("pct reg nosel"), "hub token without project:create")
	})

	t.Run("refused on a project boundary", func(t *testing.T) {
		// A minted project token cannot carry project:create, so this
		// first request is a selector control.
		key := projectOpsMint(t, f.srv, f.holder, projectBoundary(f.home), "project:read", "agent:read")
		pctAssertRefused(t, f.s, register(key, RegisterProjectRequest{Name: "pct reg ptok"}), api.Slugify("pct reg ptok"), "project token")
		// A stored project-boundary row carrying project:create is refused
		// by its boundary: registration is a hub-level request.
		stored := uatpInsertLegacyToken(t, f.s, f.holder, f.home, []string{"project:create", "project:read", "agent:read"})
		pctAssertRefused(t, f.s, register(stored, RegisterProjectRequest{Name: "pct reg stored"}), api.Slugify("pct reg stored"), "stored project-boundary row")
		assert.Equal(t, bearerReasonHubLevelResource, pctCreateDenialReason(t, f.srv, stored))
	})

	t.Run("an existing project named by id needs project:update", func(t *testing.T) {
		createOnly := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:read", "agent:read")
		rec := register(createOnly, RegisterProjectRequest{ID: f.home, Name: "pct reg home"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		withUpdate := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:update", "project:read", "agent:read")
		rec = register(withUpdate, RegisterProjectRequest{ID: f.home, Name: "pct reg home"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp RegisterProjectResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Created)
		require.NotNil(t, resp.Project)
		assert.Equal(t, f.home, resp.Project.ID)
	})

	t.Run("sessions need project.create", func(t *testing.T) {
		rec := pctSession(t, f.srv, f.s, f.holder, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "pct reg session"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		pctAssertCreated(t, f.s, api.Slugify("pct reg session"), f.holder)
		plain := newPlainUser(t, f.s, "pct-reg-plain")
		rec = pctSession(t, f.srv, f.s, plain.ID, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "pct reg plain"})
		pctAssertRefused(t, f.s, rec, api.Slugify("pct reg plain"), "session without project.create")
	})
}

// TestProjectRegisterToken_EmbeddedBrokerFormIsSessionOnly pins that a
// user access token is refused the embedded-broker form of project
// register, on an existing project and on a new one, with or without
// broker:create beside project:create and project:update: 403 with the
// session-only reason, and no broker, provider link, project change or
// broker secret. The holder's session sends the same request and is
// admitted.
func TestProjectRegisterToken_EmbeddedBrokerFormIsSessionOnly(t *testing.T) {
	f := newPCTFixture(t, "pct-emb")
	ctx := context.Background()
	key := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:update", "project:read", "agent:read", "broker:create")
	broker := func(name string) *RegisterProjectBrokerInfo {
		return &RegisterProjectBrokerInfo{Name: name, Version: "1.0.0"}
	}
	assertNoBroker := func(name string) {
		t.Helper()
		_, err := f.s.GetRuntimeBrokerByName(ctx, name)
		assert.ErrorIs(t, err, store.ErrNotFound, "no broker %s", name)
	}
	assertSessionOnly := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		code, details := errorCodeAndDetails(t, rec)
		assert.Equal(t, ErrCodeForbidden, code, rec.Body.String())
		assert.Equal(t, string(authzop.ReasonCredentialManagement), details["reason"], rec.Body.String())
		assert.Equal(t, sessionRequiredCredential, details["credential"], rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "secretKey")
	}
	before, err := f.s.GetProject(ctx, f.home)
	require.NoError(t, err)

	// The refusal keys on the embedded broker in the body, not on any
	// broker selector: tokens with and without broker:create are refused.
	createOnly := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create")
	createUpdate := projectOpsMint(t, f.srv, f.holder, hubBoundary(), "project:create", "project:update")
	n := 0
	for _, tc := range []struct {
		name     string
		key      string
		existing bool
	}{
		{"all selectors, existing project", key, true},
		{"all selectors, new project", key, false},
		{"project:create and project:update, existing project", createUpdate, true},
		{"project:create only, new project", createOnly, false},
	} {
		n++
		brokerName := fmt.Sprintf("pct-emb-broker-t%d", n)
		req := RegisterProjectRequest{Name: fmt.Sprintf("pct emb new %d", n), Broker: broker(brokerName)}
		if tc.existing {
			req.ID = f.home
		}
		rec := doRequestWithToken(t, f.srv, tc.key, http.MethodPost, "/api/v1/projects/register", req)
		assertSessionOnly(rec)
		assertNoBroker(brokerName)
		assert.Nil(t, pctProjectBySlug(t, f.s, api.Slugify(req.Name)), "%s: no project", tc.name)
		providers, err := f.s.GetProjectProviders(ctx, f.home)
		require.NoError(t, err)
		assert.Empty(t, providers, "%s: no provider link", tc.name)
		after, err := f.s.GetProject(ctx, f.home)
		require.NoError(t, err)
		assert.Equal(t, before.DefaultRuntimeBrokerID, after.DefaultRuntimeBrokerID, tc.name)
		assert.Equal(t, before.Updated, after.Updated, "%s: project not written", tc.name)
	}

	// The holder's session sends the existing-project request and is
	// admitted: the broker is created and linked.
	rec := pctSession(t, f.srv, f.s, f.holder, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{ID: f.home, Name: "pct emb home", Broker: broker("pct-emb-broker-3")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	b, err := f.s.GetRuntimeBrokerByName(ctx, "pct-emb-broker-3")
	require.NoError(t, err)
	assert.Equal(t, f.holder, b.CreatedBy)
	providers, err := f.s.GetProjectProviders(ctx, f.home)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	assert.Equal(t, b.ID, providers[0].BrokerID)
}

// TestBuiltInRoles_SuperAdminRevision pins the super-admin revision and its
// latest change: revision 8 drops project.register, a reserved row.
func TestBuiltInRoles_SuperAdminRevision(t *testing.T) {
	admin := builtInRoleByName(t, store.SystemRoleSuperAdmin)
	assert.Equal(t, 8, admin.Revision)
	assert.NotContains(t, admin.Permissions, "project.register")
	assert.Contains(t, admin.Permissions, "project.create")
}

// TestProjectCloneToken_RequiresSourceReadCloneAndCreate pins that a clone
// by token needs, each independently: project.read on the source
// (project:read plus live access to the source project), the project:clone
// selector, and project.create through the hub collection admission
// (project:create on a hub boundary). Only a super-admin can mint
// project:clone. A session needs project.read on the source and
// project.create, and no project.clone binding.
func TestProjectCloneToken_RequiresSourceReadCloneAndCreate(t *testing.T) {
	f := newPCTFixture(t, "pct-clone")
	src := projectOpsProject(t, f.s, "pct-clone-src")
	super := projectOpsUser(t, f.s, "pct-clone-super", true)
	projectOpsGrant(t, f.s, super, src, store.ProjectRoleMember)
	clonePath := "/api/v1/projects/" + src + "/clone"
	clone := func(key, name string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, f.srv, key, http.MethodPost, clonePath, CloneProjectRequest{Name: name})
	}

	// A hub member cannot mint project:clone on either boundary.
	projectOpsGrant(t, f.s, f.holder, src, store.ProjectRoleMember)
	pctRequireMintRefused(t, pctMintErr(f.srv, f.holder, hubBoundary(), "project:clone"), "project:clone for a hub member")
	pctRequireMintRefused(t, pctMintErr(f.srv, f.holder, projectBoundary(src), "project:clone"), "project:clone on a project boundary")

	t.Run("admitted with all three selectors", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, super, hubBoundary(), "project:read", "project:clone", "project:create", "agent:read")
		rec := clone(key, "pct clone ok")
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		pctAssertCreated(t, f.s, api.Slugify("pct clone ok"), super)
	})

	t.Run("each selector is required", func(t *testing.T) {
		// The checks run in order: source read, the clone selector, then
		// project creation. Each case is refused by the check for its
		// missing selector, named in the response.
		for _, tc := range []struct {
			missing string
			scopes  []string
			refusal string
		}{
			{"project:read", []string{"project:clone", "project:create", "agent:read"}, "read source project"},
			{"project:clone", []string{"project:read", "project:create", "agent:read"}, "clone projects"},
			{"project:create", []string{"project:read", "project:clone", "agent:read"}, "create projects"},
		} {
			key := projectOpsMint(t, f.srv, super, hubBoundary(), tc.scopes...)
			name := "pct clone no " + tc.missing
			rec := clone(key, name)
			pctAssertRefused(t, f.s, rec, api.Slugify(name), "token without "+tc.missing)
			assert.Contains(t, rec.Body.String(), tc.refusal, "token without %s", tc.missing)
		}
	})

	t.Run("source read needs live access to the source project", func(t *testing.T) {
		// Only a super-admin can mint project:clone, and a super-admin
		// reads every project, so this case uses a token identity with a
		// stored ceiling: the holder is a hub member with a role on src and
		// none on other.
		other := projectOpsProject(t, f.s, "pct-clone-other")
		u, err := f.s.GetUser(context.Background(), f.holder)
		require.NoError(t, err)
		token := NewScopedUserIdentityWithBoundary(authUser(u), hubBoundary(), nil, "pct-clone-cred",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"project.read", "project.clone", "project.create", "agent.read"}})
		rec := requestAsIdentity(t, f.srv, token, http.MethodPost, "/api/v1/projects/"+other+"/clone", CloneProjectRequest{Name: "pct clone other"})
		pctAssertRefused(t, f.s, rec, api.Slugify("pct clone other"), "token without a role on the source")
		assert.Contains(t, rec.Body.String(), "read source project")
		rec = requestAsIdentity(t, f.srv, token, http.MethodPost, clonePath, CloneProjectRequest{Name: "pct clone member"})
		require.Equal(t, http.StatusCreated, rec.Code, "the same token on a source it can read: %s", rec.Body.String())
	})

	t.Run("refused on a project boundary", func(t *testing.T) {
		// A minted project token cannot carry project:create, so this
		// first request is a selector control.
		key := projectOpsMint(t, f.srv, super, projectBoundary(src), "project:read", "agent:read")
		pctAssertRefused(t, f.s, clone(key, "pct clone ptok"), api.Slugify("pct clone ptok"), "project token")
		// A stored project-boundary row on the source carrying all three
		// selectors reads the source and carries project:clone, and is
		// refused by its boundary at the project creation check, a
		// hub-level request.
		stored := uatpInsertLegacyToken(t, f.s, super, src, []string{"project:read", "project:clone", "project:create", "agent:read"})
		rec := clone(stored, "pct clone stored")
		pctAssertRefused(t, f.s, rec, api.Slugify("pct clone stored"), "stored project-boundary row")
		assert.Contains(t, rec.Body.String(), "create projects")
		assert.Equal(t, bearerReasonHubLevelResource, pctCreateDenialReason(t, f.srv, stored))
	})

	t.Run("refused when the token does not cover the owner role", func(t *testing.T) {
		key := projectOpsMint(t, f.srv, super, hubBoundary(), "project:read", "project:clone", "project:create")
		rec := clone(key, "pct clone partial")
		pctAssertRefused(t, f.s, rec, api.Slugify("pct clone partial"), "token without agent:read")
		assert.Contains(t, rec.Body.String(), ownerGrantDenyText)
	})

	t.Run("sessions need source read and project.create only", func(t *testing.T) {
		rec := pctSession(t, f.srv, f.s, f.holder, http.MethodPost, clonePath, CloneProjectRequest{Name: "pct clone session"})
		require.Equal(t, http.StatusCreated, rec.Code, "hub member session with a source role: %s", rec.Body.String())
		pctAssertCreated(t, f.s, api.Slugify("pct clone session"), f.holder)
		stranger := projectOpsUser(t, f.s, "pct-clone-stranger", false)
		rec = pctSession(t, f.srv, f.s, stranger, http.MethodPost, clonePath, CloneProjectRequest{Name: "pct clone stranger"})
		pctAssertRefused(t, f.s, rec, api.Slugify("pct clone stranger"), "session without source read")
		plain := newPlainUser(t, f.s, "pct-clone-plain")
		projectOpsGrant(t, f.s, plain.ID, src, store.ProjectRoleMember)
		rec = pctSession(t, f.srv, f.s, plain.ID, http.MethodPost, clonePath, CloneProjectRequest{Name: "pct clone plain"})
		pctAssertRefused(t, f.s, rec, api.Slugify("pct clone plain"), "session without project.create")
	})
}

// TestProjectCloneTokenDenial pins the clone selector check: a token passes
// only when its ceiling allows project.clone on a boundary eligible for it;
// every other identity passes.
func TestProjectCloneTokenDenial(t *testing.T) {
	user := authUser(&store.User{ID: tid("pctd-user"), Email: "pctd@test.com"})
	token := func(boundary TokenBoundary, ids ...string) *ScopedUserIdentity {
		return NewScopedUserIdentityWithBoundary(user, boundary, nil, "pctd-cred",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ids})
	}
	assert.Empty(t, projectCloneTokenDenial(token(hubBoundary(), "project.clone")))
	assert.Empty(t, projectCloneTokenDenial(token(projectBoundary(tid("pctd-p")), "project.clone")))
	assert.NotEmpty(t, projectCloneTokenDenial(token(hubBoundary(), "project.read", "project.create")))
	assert.NotEmpty(t, projectCloneTokenDenial(token(hubBoundary())))
	assert.NotEmpty(t, projectCloneTokenDenial(token(TokenBoundary{}, "project.clone")))
	assert.Empty(t, projectCloneTokenDenial(user))
}
