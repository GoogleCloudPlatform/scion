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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// envSecretTokenFixture is a test server with a secret backend, a hub admin
// who owns a project, and one seeded hub-level variable and secret.
type envSecretTokenFixture struct {
	srv     *Server
	s       store.Store
	admin   string
	project string
}

const (
	envSecretTokenVar    = "EST_HUB_VAR"
	envSecretTokenSecret = "EST_HUB_SECRET"
)

func newEnvSecretTokenFixture(t *testing.T, name string) *envSecretTokenFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	admin := hubConfigTokenUser(t, s, name+"-admin", store.SystemRoleSuperAdmin)
	project := tid(name + "-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: project, Name: name, Slug: project}))
	createTestUserWithProjectRole(t, s, admin, admin+"@test.com", project, store.ProjectRoleOwner)

	// Seed hub-level material through the session admin, so a read that is
	// wrongly admitted answers 200 rather than 404.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/"+envSecretTokenVar,
		SetEnvVarRequest{Value: "seed", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/secrets/"+envSecretTokenSecret,
		SetSecretRequest{Value: "c2VlZA==", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	return &envSecretTokenFixture{srv: srv, s: s, admin: admin, project: project}
}

// hubEnvSecretRequests lists every hub-level env and secret operation.
func hubEnvSecretRequests() []struct {
	name   string
	method string
	path   string
	body   interface{}
} {
	return []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"env list", http.MethodGet, "/api/v1/env?scope=hub", nil},
		{"env get", http.MethodGet, "/api/v1/env/" + envSecretTokenVar + "?scope=hub", nil},
		{"env set", http.MethodPut, "/api/v1/env/" + envSecretTokenVar, SetEnvVarRequest{Value: "changed", Scope: "hub"}},
		{"env set new", http.MethodPut, "/api/v1/env/EST_HUB_NEW", SetEnvVarRequest{Value: "new", Scope: "hub"}},
		{"env delete", http.MethodDelete, "/api/v1/env/" + envSecretTokenVar + "?scope=hub", nil},
		{"secret list", http.MethodGet, "/api/v1/secrets?scope=hub", nil},
		{"secret get", http.MethodGet, "/api/v1/secrets/" + envSecretTokenSecret + "?scope=hub", nil},
		{"secret set", http.MethodPut, "/api/v1/secrets/" + envSecretTokenSecret, SetSecretRequest{Value: "Y2hhbmdlZA==", Scope: "hub"}},
		{"secret patch", http.MethodPatch, "/api/v1/secrets/" + envSecretTokenSecret + "?scope=hub", map[string]string{"description": "changed"}},
		{"secret delete", http.MethodDelete, "/api/v1/secrets/" + envSecretTokenSecret + "?scope=hub", nil},
	}
}

// requireHubMaterialUnchanged checks, as the session admin, that the seeded
// hub-level variable and secret are still present with their seeded values.
func requireHubMaterialUnchanged(t *testing.T, f *envSecretTokenFixture) {
	t.Helper()
	requireHubMaterialUnchangedVia(t, func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return doRequest(t, f.srv, method, path, body)
	})
}

// requireHubMaterialUnchangedVia is requireHubMaterialUnchanged with the
// checking requests sent by do, an admin session of the caller's choice.
func requireHubMaterialUnchangedVia(t *testing.T, do func(method, path string, body interface{}) *httptest.ResponseRecorder) {
	t.Helper()
	rec := do(http.MethodGet, "/api/v1/env/"+envSecretTokenVar+"?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, "hub variable must survive refused requests: %s", rec.Body.String())
	var ev store.EnvVar
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&ev))
	require.Equal(t, "seed", ev.Value, "hub variable value must be unchanged")

	rec = do(http.MethodGet, "/api/v1/env/EST_HUB_NEW?scope=hub", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, "a refused request must not create a hub variable: %s", rec.Body.String())

	rec = do(http.MethodGet, "/api/v1/secrets/"+envSecretTokenSecret+"?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, "hub secret must survive refused requests: %s", rec.Body.String())
}

// TestHubEnvSecret_ProjectBoundaryTokenRefused: an admin's user access token
// with a project boundary and a narrow scope set is refused on every
// hub-level env and secret operation, and changes nothing.
func TestHubEnvSecret_ProjectBoundaryTokenRefused(t *testing.T) {
	f := newEnvSecretTokenFixture(t, "est-pb")
	key := mintHubConfigToken(t, f.srv, f.admin, projectBoundary(f.project), "project:read")

	for _, tc := range hubEnvSecretRequests() {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestWithToken(t, f.srv, key, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		})
	}
	requireHubMaterialUnchanged(t, f)
}

// TestHubEnvSecret_HubBoundaryTokenWithoutHubMaterialScopeRefused: a hub
// boundary alone does not admit a token whose scopes do not cover hub-level
// env and secret material.
func TestHubEnvSecret_HubBoundaryTokenWithoutHubMaterialScopeRefused(t *testing.T) {
	f := newEnvSecretTokenFixture(t, "est-hb")
	key := mintHubConfigToken(t, f.srv, f.admin, hubBoundary(), "hub_config:read")

	for _, tc := range hubEnvSecretRequests() {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestWithToken(t, f.srv, key, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		})
	}
	requireHubMaterialUnchanged(t, f)
}

// TestHubEnvSecret_SessionAdminAllowed: the session admin keeps full
// hub-level env and secret access.
func TestHubEnvSecret_SessionAdminAllowed(t *testing.T) {
	f := newEnvSecretTokenFixture(t, "est-sa")
	want := map[string]int{
		"env list": http.StatusOK, "env get": http.StatusOK, "env set": http.StatusOK,
		"env set new": http.StatusOK, "env delete": http.StatusNoContent,
		"secret list": http.StatusOK, "secret get": http.StatusOK, "secret set": http.StatusOK,
		"secret patch": http.StatusOK, "secret delete": http.StatusNoContent,
	}
	for _, tc := range hubEnvSecretRequests() {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, f.srv, tc.method, tc.path, tc.body)
			require.Equal(t, want[tc.name], rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		})
	}
}

// TestHubEnvSecret_NonAdminSessionRefused: a session alone is not enough on
// hub-level env and secret operations; a non-admin user's session is refused
// and changes nothing.
func TestHubEnvSecret_NonAdminSessionRefused(t *testing.T) {
	f := newEnvSecretTokenFixture(t, "est-na")
	memberID := hubConfigTokenUser(t, f.s, "est-na-member", store.SystemRoleHubMember)
	member, err := f.s.GetUser(context.Background(), memberID)
	require.NoError(t, err)
	require.NotEqual(t, store.UserRoleAdmin, member.Role)

	for _, tc := range hubEnvSecretRequests() {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, f.srv, member, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		})
	}
	requireHubMaterialUnchanged(t, f)
}

// TestHubEnvSecret_TestAdminSessionRefused: a test identity with an admin
// role uses a session-class credential, and is still refused on every
// hub-level env and secret operation (test-admin policy); a human
// super-admin's session keeps access.
func TestHubEnvSecret_TestAdminSessionRefused(t *testing.T) {
	srv, s, _ := tsaServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	issuer := taUser(t, s, "est-ta-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "est-ta-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	human := taTokenFor(t, srv, issuer)
	asHuman := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return doRequestWithToken(t, srv, human, method, path, body)
	}

	rec := asHuman(http.MethodPut, "/api/v1/env/"+envSecretTokenVar, SetEnvVarRequest{Value: "seed", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = asHuman(http.MethodPut, "/api/v1/secrets/"+envSecretTokenSecret, SetSecretRequest{Value: "c2VlZA==", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for who, tok := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
		for _, tc := range hubEnvSecretRequests() {
			t.Run(who+" "+tc.name, func(t *testing.T) {
				rec := doRequestWithToken(t, srv, tok, tc.method, tc.path, tc.body)
				require.Equal(t, http.StatusForbidden, rec.Code, "%s %s %s: %s", who, tc.method, tc.path, rec.Body.String())
			})
		}
	}
	requireHubMaterialUnchangedVia(t, asHuman)
}

// TestUserEnvSecret_ProjectBoundaryTokenRefused: a project-boundary token
// with a narrow scope set does not reach the holder's user-level env and
// secret material; the holder's session still does.
func TestUserEnvSecret_ProjectBoundaryTokenRefused(t *testing.T) {
	f := newEnvSecretTokenFixture(t, "est-us")
	key := mintHubConfigToken(t, f.srv, f.admin, projectBoundary(f.project), "project:read")

	requests := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"env list", http.MethodGet, "/api/v1/env?scope=user", nil},
		{"env get", http.MethodGet, "/api/v1/env/EST_USER_VAR?scope=user", nil},
		{"env set", http.MethodPut, "/api/v1/env/EST_USER_VAR", SetEnvVarRequest{Value: "v", Scope: "user"}},
		{"env delete", http.MethodDelete, "/api/v1/env/EST_USER_VAR?scope=user", nil},
		{"secret list", http.MethodGet, "/api/v1/secrets?scope=user", nil},
		{"secret get", http.MethodGet, "/api/v1/secrets/EST_USER_SECRET?scope=user", nil},
		{"secret set", http.MethodPut, "/api/v1/secrets/EST_USER_SECRET", SetSecretRequest{Value: "dg==", Scope: "user"}},
		{"secret delete", http.MethodDelete, "/api/v1/secrets/EST_USER_SECRET?scope=user", nil},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestWithToken(t, f.srv, key, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
		})
	}

	user, err := f.s.GetUser(context.Background(), f.admin)
	require.NoError(t, err)
	rec := doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/env?scope=user", nil)
	require.Equal(t, http.StatusOK, rec.Code, "the holder's session keeps user-level access: %s", rec.Body.String())
	rec = doRequestAsUser(t, f.srv, user, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	require.Equal(t, http.StatusOK, rec.Code, "the holder's session keeps user-level access: %s", rec.Body.String())
}
