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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Read-only access to the hub-level environment variable list for the
// built-in hub-admin role (hub.env_vars.read). Writes, single-variable reads
// and every secret surface keep their existing checks.

const hubEnvSecretValue = "hub-env-secret-plaintext"

// newHubAdminRoleUser creates a user whose only elevated authority is a
// system-scope binding to the seeded hub-admin role.
func newHubAdminRoleUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	userID := tid(name)
	createTestUserWithRole(t, s, userID, name+"@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return u
}

// seedHubEnvFixtures creates, as the dev super-admin, one plain and one
// sensitive hub-level variable plus one hub-level environment-type secret.
func seedHubEnvFixtures(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_PLAIN",
		SetEnvVarRequest{Value: "plain-value", Description: "plain", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_SENSITIVE",
		SetEnvVarRequest{Value: "sensitive-value", Sensitive: true, Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_ENV_SECRET",
		SetEnvVarRequest{Value: hubEnvSecretValue, Secret: true, Scope: "hub",
			Description: "secret-backed description"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func listHubEnvKeys(t *testing.T, body []byte) (ListEnvVarsResponse, []string) {
	t.Helper()
	var resp ListEnvVarsResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	keys := make([]string, 0, len(resp.EnvVars))
	for _, ev := range resp.EnvVars {
		keys = append(keys, ev.Key)
	}
	return resp, keys
}

func TestBuiltInRoles_HubEnvVarsReadGrantedToHubAdminOnly(t *testing.T) {
	const perm = "hub.env_vars.read"
	assert.Contains(t, hubAdminPermissionIDs(), perm)
	assert.Contains(t, allPermissionIDs(), perm, "super-admin holds every registry permission")
	assert.NotContains(t, hubMemberPermissionIDs(), perm)
	assert.NotContains(t, hubViewerPermissionIDs(), perm)
}

func TestEnvVar_HubScope_HubAdminCanList(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)
	hubAdmin := newHubAdminRoleUser(t, s, "hub-env-read-admin")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/env?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp, keys := listHubEnvKeys(t, rec.Body.Bytes())
	assert.Equal(t, "test-hub-id", resp.ScopeID)
	assert.Equal(t, store.ScopeHub, resp.Scope)
	assert.ElementsMatch(t, []string{"HUB_PLAIN", "HUB_SENSITIVE"}, keys)
	for _, ev := range resp.EnvVars {
		switch ev.Key {
		case "HUB_PLAIN":
			assert.Equal(t, "plain-value", ev.Value)
		case "HUB_SENSITIVE":
			assert.Equal(t, "********", ev.Value, "sensitive values stay masked")
		}
	}
}

// The read-only list must not carry secret metadata: environment-type
// secrets merged into the full list stay visible only to callers that pass
// the secret access check.
func TestEnvVar_HubScope_HubAdminListExcludesSecretMetadata(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)
	hubAdmin := newHubAdminRoleUser(t, s, "hub-env-read-nosecret")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/env?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, keys := listHubEnvKeys(t, rec.Body.Bytes())
	assert.NotContains(t, keys, "HUB_ENV_SECRET")
	body := rec.Body.String()
	assert.NotContains(t, body, "secret-backed description")
	assert.NotContains(t, body, hubEnvSecretValue)
}

func TestEnvVar_HubScope_HubAdminWritesForbidden(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)
	hubAdmin := newHubAdminRoleUser(t, s, "hub-env-read-writer")

	// Create a new variable.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPut, "/api/v1/env/HUB_NEW",
		SetEnvVarRequest{Value: "x", Scope: "hub"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "create: %s", rec.Body.String())

	// Edit an existing variable.
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPut, "/api/v1/env/HUB_PLAIN",
		SetEnvVarRequest{Value: "changed", Scope: "hub"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "edit: %s", rec.Body.String())

	// Delete an existing variable.
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodDelete, "/api/v1/env/HUB_PLAIN?scope=hub", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "delete: %s", rec.Body.String())

	// Single-variable reads are outside the list permission.
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/env/HUB_PLAIN?scope=hub", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "get by key: %s", rec.Body.String())

	// Nothing changed.
	ev, err := s.GetEnvVar(context.Background(), "HUB_PLAIN", store.ScopeHub, "test-hub-id")
	require.NoError(t, err)
	assert.Equal(t, "plain-value", ev.Value)
	_, err = s.GetEnvVar(context.Background(), "HUB_NEW", store.ScopeHub, "test-hub-id")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSecret_HubScope_HubAdminStillForbidden(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)
	hubAdmin := newHubAdminRoleUser(t, s, "hub-env-read-secrets")

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/HUB_FILE_SECRET",
		SetSecretRequest{Value: "aHViLXNlY3JldA==", Scope: "hub"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	paths := []string{
		"/api/v1/secrets?scope=hub",
		"/api/v1/secrets/HUB_FILE_SECRET?scope=hub",
		"/api/v1/secrets/HUB_ENV_SECRET?scope=hub",
	}
	for _, p := range paths {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", p, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "HUB_FILE_SECRET", p)
		assert.NotContains(t, rec.Body.String(), hubEnvSecretValue, p)
	}

	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPut, "/api/v1/secrets/HUB_FILE_SECRET",
		SetSecretRequest{Value: "Y2hhbmdlZA==", Scope: "hub"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "secret write: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodDelete, "/api/v1/secrets/HUB_FILE_SECRET?scope=hub", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "secret delete: %s", rec.Body.String())
}

// Super-admin behaviour is unchanged: the full list, including merged
// environment-type secrets, plus write access.
func TestEnvVar_HubScope_SuperAdminUnchanged(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)
	superAdmin := newSuperAdminUser(t, s, "hub-env-read-super")

	rec := doRequestAsUser(t, srv, superAdmin, http.MethodGet, "/api/v1/env?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp, keys := listHubEnvKeys(t, rec.Body.Bytes())
	assert.ElementsMatch(t, []string{"HUB_PLAIN", "HUB_SENSITIVE", "HUB_ENV_SECRET"}, keys)
	i := slices.IndexFunc(resp.EnvVars, func(ev store.EnvVar) bool { return ev.Key == "HUB_ENV_SECRET" })
	require.GreaterOrEqual(t, i, 0)
	assert.True(t, resp.EnvVars[i].Secret)
	assert.Equal(t, "********", resp.EnvVars[i].Value)

	rec = doRequestAsUser(t, srv, superAdmin, http.MethodPut, "/api/v1/env/HUB_PLAIN",
		SetEnvVarRequest{Value: "changed", Scope: "hub"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, superAdmin, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Plain users and hub members (who hold hub.settings.read but not
// hub.env_vars.read) stay refused.
func TestEnvVar_HubScope_NonAdminsListForbidden(t *testing.T) {
	srv, s := testServer(t)
	seedHubEnvFixtures(t, srv, s)

	users := map[string]*store.User{
		"plain":      newPlainUser(t, s, "hub-env-read-plain"),
		"hub-member": newHubMemberUser(t, s, "hub-env-read-member"),
	}
	for name, u := range users {
		rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/env?scope=hub", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
	}
}
