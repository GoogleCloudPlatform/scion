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
// malformed user or agent ID gets the same 400 (code and message) there.

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
	role := pcvSystemRole(t, f)

	for _, principalType := range []string{"user", "agent"} {
		for _, bad := range pcvMalformedIDs {
			want := pcvPutError(t, f, principalType, bad)

			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
				RoleDefinitionID: role.ID,
				PrincipalType:    principalType,
				PrincipalID:      bad,
				ScopeType:        store.RoleScopeSystem,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s %q: %s", principalType, bad, rec.Body.String())
			got := pcvError(t, rec)
			assert.Equal(t, ErrCodeInvalidRequest, got.Code, "%s %q", principalType, bad)
			assert.Equal(t, want.Code, got.Code, "%s %q: same code as members PUT", principalType, bad)
			assert.Equal(t, want.Message, got.Message, "%s %q: same message as members PUT", principalType, bad)
		}
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
