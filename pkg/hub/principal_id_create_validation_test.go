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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Principal-ID format checks on the create paths (ptone/scion#3478):
// POST /api/v1/admin/role-bindings and POST /api/v1/projects/{id}/members
// apply the same principal-address check as the members PUT, so a
// malformed user or agent ID gets the same 400 (code and message) there,
// once the caller's permissions are decided.

// pcvMalformedIDs are user/agent principal IDs that are neither an email
// nor a well-formed UUID.
var pcvMalformedIDs = []string{"not-a-uuid", "u1", "00000000-0000-0000-0000"}

// pcvError decodes an error response.
func pcvError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error
}

// pcvPutError returns the members PUT's 400 for the given principal, the
// reference the create paths must match.
func pcvPutError(t *testing.T, f *mmrFixture, principalType, principalID string) APIError {
	t.Helper()
	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, principalType, principalID, []string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, "PUT %s %q: %s", principalType, principalID, rec.Body.String())
	return pcvError(t, rec)
}

func pcvPostMember(t *testing.T, f *mmrFixture, principalType, principalID, roleDefID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/projects/"+f.projectID+"/members",
		map[string]interface{}{"principalType": principalType, "principalId": principalID, "roleDefinitionId": roleDefID})
}

func pcvSystemRole(t *testing.T, f *mmrFixture) *store.RoleDefinition {
	t.Helper()
	return createRoleViaAPI(t, f.srv, createRoleDefinitionRequest{
		Name:        "pcv-system-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"agent.read"},
	})
}

func pcvSeedUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	id := tid(name)
	u := &store.User{ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func TestCreateRoleBinding_MalformedPrincipalIDMatchesMembersPut(t *testing.T) {
	f := setupMMRFixture(t)
	systemRole := pcvSystemRole(t, f)

	cases := []struct {
		name          string
		principalType string
		roleID        string
		scopeType     string
		scopeID       string
	}{
		{"user/system role", "user", systemRole.ID, store.RoleScopeSystem, ""},
		{"user/custom project role", "user", f.withinCeiling.ID, store.RoleScopeProject, f.projectID},
		{"agent/custom project role", "agent", f.withinCeiling.ID, store.RoleScopeProject, f.projectID},
		{"user/built-in project role", "user", f.memberRD.ID, store.RoleScopeProject, f.projectID},
		{"agent/built-in project role", "agent", f.memberRD.ID, store.RoleScopeProject, f.projectID},
	}
	for _, tc := range cases {
		for _, bad := range pcvMalformedIDs {
			want := pcvPutError(t, f, tc.principalType, bad)

			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
				RoleDefinitionID: tc.roleID,
				PrincipalType:    tc.principalType,
				PrincipalID:      bad,
				ScopeType:        tc.scopeType,
				ScopeID:          tc.scopeID,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s %q: %s", tc.name, bad, rec.Body.String())
			got := pcvError(t, rec)
			assert.Equal(t, ErrCodeInvalidRequest, got.Code, "%s %q", tc.name, bad)
			assert.Equal(t, want.Code, got.Code, "%s %q: same code as members PUT", tc.name, bad)
			assert.Equal(t, want.Message, got.Message, "%s %q: same message as members PUT", tc.name, bad)
			if tc.scopeType == store.RoleScopeProject {
				assert.Empty(t, mmrBindingsFor(t, f.store, tc.principalType, bad, f.projectID), "%s %q", tc.name, bad)
			}
		}
	}
}

// A caller who may not create the binding gets 403 for a malformed principal
// ID too: role-binding create decides permissions before checking the
// principal ID, as members PUT does.
func TestCreateRoleBinding_BuiltInRoleWithoutRightsMalformedPrincipalIDForbidden(t *testing.T) {
	f := setupMMRFixture(t)

	for _, principalType := range []string{"user", "agent"} {
		for _, bad := range pcvMalformedIDs {
			rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
				RoleDefinitionID: f.memberRD.ID,
				PrincipalType:    principalType,
				PrincipalID:      bad,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          f.projectID,
			})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %q: %s", principalType, bad, rec.Body.String())
		}
	}
}

func TestCreateRoleBinding_CustomRoleWithoutRightsMalformedPrincipalIDForbidden(t *testing.T) {
	srv, st := testServer(t)

	// Holds role_binding.create, but not every permission of the role.
	caller := setupNonAdminUser(t, st, []string{"role_binding.create", "role_binding.read", "agent.read"})
	role := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name:        "pcv-unheld-" + tid(t.Name())[:8],
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"agent.read", "user.suspend"},
	})

	for _, bad := range pcvMalformedIDs {
		rec := doRequestAsIdentity(t, srv, caller, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: role.ID,
			PrincipalType:    "user",
			PrincipalID:      bad,
			ScopeType:        store.RoleScopeSystem,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%q: %s", bad, rec.Body.String())
	}
}

func TestCreateRoleBinding_WellFormedAndEmailPrincipalsAccepted(t *testing.T) {
	f := setupMMRFixture(t)
	role := pcvSystemRole(t, f)

	byID := pcvSeedUser(t, f.store, t.Name()+"-by-id")
	rb := createBindingViaAPI(t, f.srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID, PrincipalType: "user", PrincipalID: byID.ID, ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, byID.ID, rb.PrincipalID)

	// A non-canonical spelling is stored under the canonical ID, as on PUT.
	upper := pcvSeedUser(t, f.store, t.Name()+"-upper")
	rb = createBindingViaAPI(t, f.srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID, PrincipalType: "user", PrincipalID: strings.ToUpper(upper.ID), ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, upper.ID, rb.PrincipalID)

	byEmail := pcvSeedUser(t, f.store, t.Name()+"-by-email")
	rb = createBindingViaAPI(t, f.srv, createRoleBindingRequest{
		RoleDefinitionID: role.ID, PrincipalType: "user", PrincipalID: byEmail.Email, ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, byEmail.ID, rb.PrincipalID)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: role.ID, PrincipalType: "user", PrincipalID: "nobody-pcv@test.com", ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "user not found with email: nobody-pcv@test.com", pcvError(t, rec).Message)
}

func TestAddProjectMember_MalformedPrincipalIDMatchesMembersPut(t *testing.T) {
	f := setupMMRFixture(t)

	for _, principalType := range []string{"user", "agent"} {
		for _, bad := range pcvMalformedIDs {
			want := pcvPutError(t, f, principalType, bad)

			rec := pcvPostMember(t, f, principalType, bad, f.memberRD.ID)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s %q: %s", principalType, bad, rec.Body.String())
			got := pcvError(t, rec)
			assert.Equal(t, ErrCodeInvalidRequest, got.Code, "%s %q", principalType, bad)
			assert.Equal(t, want.Code, got.Code, "%s %q: same code as members PUT", principalType, bad)
			assert.Equal(t, want.Message, got.Message, "%s %q: same message as members PUT", principalType, bad)
			assert.Empty(t, mmrBindingsFor(t, f.store, principalType, bad, f.projectID))
		}
	}
}

func TestAddProjectMember_WellFormedAndEmailPrincipalsAccepted(t *testing.T) {
	f := setupMMRFixture(t)

	decode := func(rec *httptest.ResponseRecorder) projectMemberInfo {
		t.Helper()
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var info projectMemberInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info), rec.Body.String())
		return info
	}

	byID := pcvSeedUser(t, f.store, t.Name()+"-by-id")
	assert.Equal(t, byID.ID, decode(pcvPostMember(t, f, "user", byID.ID, f.memberRD.ID)).PrincipalID)

	// A non-canonical spelling is stored under the canonical ID, as on PUT.
	upper := pcvSeedUser(t, f.store, t.Name()+"-upper")
	assert.Equal(t, upper.ID, decode(pcvPostMember(t, f, "user", strings.ToUpper(upper.ID), f.memberRD.ID)).PrincipalID)
	assert.Len(t, mmrBindingsFor(t, f.store, "user", upper.ID, f.projectID), 1)

	byEmail := pcvSeedUser(t, f.store, t.Name()+"-by-email")
	assert.Equal(t, byEmail.ID, decode(pcvPostMember(t, f, "user", byEmail.Email, f.memberRD.ID)).PrincipalID)

	rec := pcvPostMember(t, f, "user", "nobody-pcv@test.com", f.memberRD.ID)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "user not found with email: nobody-pcv@test.com", pcvError(t, rec).Message)
}

func TestAddProjectMember_AgentUpperCaseIDStoredCanonical(t *testing.T) {
	f := setupMMRFixture(t)

	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: agentID, Name: "pcv-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))

	rec := pcvPostMember(t, f, "agent", strings.ToUpper(agentID), f.memberRD.ID)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var info projectMemberInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info), rec.Body.String())
	assert.Equal(t, agentID, info.PrincipalID)
	assert.Len(t, mmrBindingsFor(t, f.store, "agent", agentID, f.projectID), 1)
}

// Role-binding create on a built-in project role stores an upper-case
// UUID spelling under its canonical lower-case ID.
func TestCreateRoleBinding_BuiltInRoleUpperCaseIDStoredCanonical(t *testing.T) {
	f := setupMMRFixture(t)

	user := pcvSeedUser(t, f.store, t.Name()+"-user")
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: agentID, Name: "pcv-rb-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))

	for _, tc := range []struct{ principalType, id string }{
		{"user", user.ID},
		{"agent", agentID},
	} {
		rb := createBindingViaAPI(t, f.srv, createRoleBindingRequest{
			RoleDefinitionID: f.memberRD.ID,
			PrincipalType:    tc.principalType,
			PrincipalID:      strings.ToUpper(tc.id),
			ScopeType:        store.RoleScopeProject,
			ScopeID:          f.projectID,
		})
		assert.Equal(t, tc.id, rb.PrincipalID, tc.principalType)
		assert.Len(t, mmrBindingsFor(t, f.store, tc.principalType, tc.id, f.projectID), 1, tc.principalType)
	}
}

// Members POST answers a malformed agent ID with the principal-address
// message, also when the requested role is the owner role.
func TestAddProjectMember_OwnerRoleMalformedAgentIDAddressMessage(t *testing.T) {
	f := setupMMRFixture(t)

	for _, bad := range pcvMalformedIDs {
		rec := pcvPostMember(t, f, "agent", bad, f.ownerRD.ID)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", bad, rec.Body.String())
		got := pcvError(t, rec)
		assert.Equal(t, ErrCodeInvalidRequest, got.Code, "%q", bad)
		assert.Equal(t, "agent principal must be addressed by agent ID: "+bad, got.Message, "%q", bad)
		assert.Empty(t, mmrBindingsFor(t, f.store, "agent", bad, f.projectID), "%q", bad)
	}
}
