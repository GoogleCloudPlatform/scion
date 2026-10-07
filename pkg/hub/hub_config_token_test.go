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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubConfigTokenUser creates an active user holding the named system role
// and hub membership, and returns the user ID.
func hubConfigTokenUser(t *testing.T, s store.Store, name, systemRole string) string {
	t.Helper()
	id := tid(name)
	userRole := "member"
	if systemRole == store.SystemRoleSuperAdmin {
		userRole = "admin"
	}
	createTestUserWithRole(t, s, id, id+"@test.com", userRole, systemRole)
	ensureHubMembership(context.Background(), s, id)
	return id
}

// mintHubConfigToken mints a real token for userID through
// UserAccessTokenService over a session.
func mintHubConfigToken(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "hct-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

// requireTokenRefusedKeys asserts a refusal of refused settings keys: 403,
// the session-only details with reason GOV_PENDING, and details.keys equal
// to want.
func requireTokenRefusedKeys(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	assert.Equal(t, string(authzop.ReasonGovernancePending), resp.Error.Details["reason"])
	assert.Equal(t, sessionRequiredCredential, resp.Error.Details["credential"])
	var got []string
	if raw, ok := resp.Error.Details["keys"].([]interface{}); ok {
		for _, k := range raw {
			got = append(got, k.(string))
		}
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
}

// TestServerConfigUpdate_AuthorityKeysRefuseTokens requires a token write
// to server-config to carry configuration keys only: every key that
// confers authority, decides the hub's origin or selects what agents run
// with, and every key outside the Layer-1 sections, is refused for a token
// that holds hub_config:read and hub_config:update, whatever its value. A
// session is not subject to the rule, and a section that contains a
// refused key cannot be reset by a token.
func TestServerConfigUpdate_AuthorityKeysRefuseTokens(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")

	refused := []struct {
		name string
		body string
		keys []string
	}{
		{"admin emails", `{"server":{"hub":{"admin_emails":["x@example.com"]}}}`, []string{"server.hub.admin_emails"}},
		{"admin emails cleared", `{"server":{"hub":{"admin_emails":[]}}}`, []string{"server.hub.admin_emails"}},
		{"user access mode", `{"server":{"auth":{"user_access_mode":"open"}}}`, []string{"server.auth.user_access_mode"}},
		{"default user role", `{"server":{"auth":{"default_user_role":"admin"}}}`, []string{"server.auth.default_user_role"}},
		{"authorized domains", `{"server":{"auth":{"authorized_domains":["example.com"]}}}`, []string{"server.auth.authorized_domains"}},
		{"federation issuers", `{"federation":{"trusted_issuers":[]}}`, []string{"federation.trusted_issuers"}},
		{"federation switch off", `{"federation":{"enabled":false}}`, []string{"federation.enabled"}},
		{"github app", `{"server":{"github_app":{"app_id":1}}}`, []string{"server.github_app.app_id"}},
		{"default agent role", `{"default_agent_role":"member"}`, []string{"default_agent_role"}},
		{"default max agent role", `{"default_max_agent_role":"admin"}`, []string{"default_max_agent_role"}},
		{"default gcp identity", `{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":"sa"}`, []string{"default_gcp_identity_mode", "default_gcp_identity_service_account_id"}},
		{"default template", `{"default_template":"t"}`, []string{"default_template"}},
		{"default harness config", `{"default_harness_config":"h"}`, []string{"default_harness_config"}},
		{"default runtime broker", `{"default_runtime_broker":"b"}`, []string{"default_runtime_broker"}},
		{"public url", `{"server":{"hub":{"public_url":"https://hub.example.com"}}}`, []string{"server.hub.public_url"}},
		{"image registry", `{"image_registry":"registry.example.com"}`, []string{"image_registry"}},
		{"runtimes", `{"runtimes":{}}`, []string{"runtimes"}},
		{"profiles", `{"profiles":null}`, []string{"profiles"}},
		{"harness configs", `{"harness_configs":{}}`, []string{"harness_configs"}},
		{"agent secrets policy", `{"agent_secrets":{"user_scope_only":false}}`, []string{"agent_secrets.user_scope_only"}},
		{"auto expose ports", `{"auto_expose_ports":{"enabled":true}}`, []string{"auto_expose_ports.enabled"}},
		{"bootstrap key", `{"server":{"auth":{"dev_mode":true}}}`, []string{"server.auth.dev_mode"}},
		{"mixed with configuration", `{"server":{"hub":{"auto_suspend_stalled":true,"admin_emails":["x@example.com"]}}}`, []string{"server.hub.admin_emails"}},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
				rec := doRequestWithToken(t, srv, key, method, "/api/v1/admin/server-config", json.RawMessage(tc.body))
				requireTokenRefusedKeys(t, rec, tc.keys...)
			}
		})
	}

	// The refused mixed body wrote nothing: the lifecycle section has no row.
	_, err := s.GetHubSetting(context.Background(), "lifecycle")
	require.ErrorIs(t, err, store.ErrNotFound, "a refused write must not persist any key")

	t.Run("configuration keys", func(t *testing.T) {
		body := json.RawMessage(`{"server":{"hub":{"auto_suspend_stalled":true,"hub_name":"hct"}},"default_max_turns":7,"expected_revisions":{}}`)
		rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config", body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("session is not subject to the token rule", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(`{"server":{"hub":{"admin_emails":["x@example.com"]}}}`))
		require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("section reset", func(t *testing.T) {
		for section, class := range serverConfigTokenSections {
			if class == settingsTokenConfiguration {
				continue
			}
			rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/admin/server-config/sections/"+section, nil)
			requireTokenRefusedKeys(t, rec, section)
		}
		rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/admin/server-config/sections/lifecycle", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("file-backed hub", func(t *testing.T) {
		fileSrv, fileStore := testServer(t)
		fileAdmin := hubConfigTokenUser(t, fileStore, "hct-file-super", store.SystemRoleSuperAdmin)
		fileKey := mintHubConfigToken(t, fileSrv, fileAdmin, hubBoundary(), "hub_config:read", "hub_config:update")
		rec := doRequestWithToken(t, fileSrv, fileKey, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(`{"server":{"hub":{"admin_emails":["x@example.com"]}}}`))
		requireTokenRefusedKeys(t, rec, "server.hub.admin_emails")

		// server.hub.agent_endpoint decides the origin agents reach the hub
		// on: a token may not write it, including an explicit clear.
		for _, body := range []string{
			`{"server":{"hub":{"agent_endpoint":"https://hub-internal.example.com"}}}`,
			`{"server":{"hub":{"agent_endpoint":""}}}`,
		} {
			for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
				rec := doRequestWithToken(t, fileSrv, fileKey, method, "/api/v1/admin/server-config", json.RawMessage(body))
				requireTokenRefusedKeys(t, rec, "server.hub.agent_endpoint")
			}
		}
	})
}

// TestServerConfigTokenSections_EveryLayer1KeyClassified requires every
// Layer-1 settings section, and every key of a per-key section, to carry a
// token classification, and no classification to name a section or key
// that does not exist.
func TestServerConfigTokenSections_EveryLayer1KeyClassified(t *testing.T) {
	sections := map[string]bool{}
	perKey := map[string]bool{}
	for _, sec := range opsettings.Registry {
		sections[sec.Name] = true
		class, ok := serverConfigTokenSections[sec.Name]
		if !ok {
			t.Errorf("Layer-1 section %q has no token classification in serverConfigTokenSections", sec.Name)
			continue
		}
		if class != settingsTokenPerKey {
			continue
		}
		for _, k := range sec.KoanfPaths {
			perKey[k] = true
			if _, ok := serverConfigTokenKeys[k]; !ok {
				t.Errorf("key %q of per-key section %q has no token classification in serverConfigTokenKeys", k, sec.Name)
			}
		}
	}
	for name := range serverConfigTokenSections {
		if !sections[name] {
			t.Errorf("serverConfigTokenSections names unknown section %q", name)
		}
	}
	for k := range serverConfigTokenKeys {
		if !perKey[k] {
			t.Errorf("serverConfigTokenKeys names %q, which is not a key of a per-key section", k)
		}
	}
	// A key outside every Layer-1 section is refused.
	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass("server.auth.dev_mode"))
	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass("schema_version"))
	// File-only keys sit outside every Layer-1 section and are refused.
	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass("server.hub.agent_endpoint"))
}

// TestProjectDefaultsUpdate_EveryKeyClassifiedForTokens requires every
// project_defaults key to carry a token classification, admits a token
// write of the configuration keys, and refuses a token write that carries
// any unclassified key.
func TestProjectDefaultsUpdate_EveryKeyClassifiedForTokens(t *testing.T) {
	fields := map[string]bool{}
	typ := reflect.TypeOf(opsettings.ProjectDefaultsSettings{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		fields[name] = true
		if _, ok := projectDefaultsTokenKeys[name]; !ok {
			t.Errorf("project_defaults key %q has no token classification in projectDefaultsTokenKeys", name)
		}
	}
	for name := range projectDefaultsTokenKeys {
		if !fields[name] {
			t.Errorf("projectDefaultsTokenKeys names unknown key %q", name)
		}
	}

	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-pd-admin", store.SystemRoleHubAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_project_defaults:read", "hub_project_defaults:update")

	rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/project-defaults", json.RawMessage(`{"default_scratchpad":false}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/project-defaults", json.RawMessage(`{"default_scratchpad":true,"unclassified_key":1}`))
	requireTokenRefusedKeys(t, rec, "unclassified_key")
}

// TestHubConfigToken_ProjectBoundaryDenied requires the hub configuration
// selectors to be hub-only: a project owner who is also a hub admin cannot
// mint them on a project token, and a project token reaches no hub
// configuration route.
func TestHubConfigToken_ProjectBoundaryDenied(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-pb-admin", store.SystemRoleHubAdmin)
	project := tid("hct-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: project, Name: "HCT", Slug: "hct-project"}))
	createTestUserWithProjectRole(t, s, admin, admin+"@test.com", project, store.ProjectRoleOwner)

	// A super-admin who owns the project holds live authority for every
	// selector, so only the boundary refuses the project token.
	super := hubConfigTokenUser(t, s, "hct-pb-super", store.SystemRoleSuperAdmin)
	createTestUserWithProjectRole(t, s, super, super+"@test.com", project, store.ProjectRoleOwner)
	for _, sel := range []string{
		"hub_config:read", "hub_config:update", "hub_project_defaults:read", "hub_project_defaults:update",
		"hub_messaging:update", "hub_experiments:update",
		"hub_lifecycle_hooks:read", "hub_lifecycle_hooks:update", "hub_settings:update",
	} {
		for _, holder := range []string{admin, super} {
			_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(holder), CreateTokenParams{
				UserID: holder, Name: "hct-" + tid("pb"), Boundary: projectBoundary(project), Scopes: []string{sel},
			})
			require.Error(t, err, "a project token with %s must not mint", sel)
		}
		mintHubConfigToken(t, srv, super, hubBoundary(), sel)
	}

	key := mintHubConfigToken(t, srv, super, projectBoundary(project), "project:read")
	for _, path := range []string{"/api/v1/admin/server-config", "/api/v1/admin/project-defaults", "/api/v1/admin/lifecycle-hooks", "/api/v1/admin/messaging", "/api/v1/admin/experiments"} {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

// TestHubConfigToken_RequiresLiveHubAuthority requires live hub authority
// for a hub configuration token: a hub member cannot mint hub_config:read,
// and a hub admin's token stops reading server-config once the admin's
// binding is removed.
func TestHubConfigToken_RequiresLiveHubAuthority(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()

	member := hubConfigTokenUser(t, s, "hct-member", store.SystemRoleHubMember)
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(member), CreateTokenParams{
		UserID: member, Name: "hct-" + tid("m"), Boundary: hubBoundary(), Scopes: []string{"hub_config:read"},
	})
	require.Error(t, err, "a hub member must not mint hub_config:read")

	admin := hubConfigTokenUser(t, s, "hct-live-admin", store.SystemRoleHubAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read")
	rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/server-config", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, admin)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed)

	rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/server-config", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestHubConfigWrite_RequiresReadAndUpdateSelectors requires a token write
// behind a route guard on a read permission to carry both the read and the
// update selector: either one alone is refused.
func TestHubConfigWrite_RequiresReadAndUpdateSelectors(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-rw-admin", store.SystemRoleHubAdmin)

	cases := []struct {
		method, path, body string
		read, update       string
	}{
		{http.MethodPut, "/api/v1/admin/server-config", `{"server":{"hub":{"auto_suspend_stalled":true}}}`, "hub_config:read", "hub_config:update"},
		{http.MethodPut, "/api/v1/admin/project-defaults", `{"default_scratchpad":true}`, "hub_project_defaults:read", "hub_project_defaults:update"},
		{http.MethodPost, "/api/v1/admin/lifecycle-hooks", `{}`, "hub_lifecycle_hooks:read", "hub_lifecycle_hooks:update"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			for _, scopes := range [][]string{{tc.update}, {tc.read}} {
				key := mintHubConfigToken(t, srv, admin, hubBoundary(), scopes...)
				rec := doRequestWithToken(t, srv, key, tc.method, tc.path, json.RawMessage(tc.body))
				require.Equal(t, http.StatusForbidden, rec.Code, "%v: %s", scopes, rec.Body.String())
			}
			key := mintHubConfigToken(t, srv, admin, hubBoundary(), tc.read, tc.update)
			rec := doRequestWithToken(t, srv, key, tc.method, tc.path, json.RawMessage(tc.body))
			require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
			require.NotEqual(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		})
	}
}

// TestHubPreStartHookRead_TokenGetsRedactedScript requires hub pre-start
// hook reads to strip the script body for every token, including one that
// carries hub_lifecycle_hooks:read with live hub authority, while a session
// with that authority sees it.
func TestHubPreStartHookRead_TokenGetsRedactedScript(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-hook-super", store.SystemRoleSuperAdmin)
	hook, err := s.CreateHubPreStartHook(context.Background(), &store.ProjectPreStartHook{
		Scope: store.PreStartHookScopeHub, Name: "hct-hook", Slug: "hct-hook", Script: "#!/bin/sh\necho hct\n",
		CreatedBy: admin + "@test.com", UpdatedBy: admin + "@test.com",
	})
	require.NoError(t, err)

	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_lifecycle_hooks:read", "hub_lifecycle_hooks:update")
	for _, path := range []string{"/api/v1/pre-start-hooks/" + hook.ID, "/api/v1/pre-start-hooks"} {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "echo hct", path)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/pre-start-hooks/"+hook.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "echo hct")
}

// TestHubConfig_MissingCredentialKindRefusedAndRedacted requires the
// settings key rule and the hub hook script redaction to treat a request
// with no credential context like a token: a refused key is refused, and a
// hub pre-start hook script is redacted, even for a super-admin whose
// interactive session sees the script.
func TestHubConfig_MissingCredentialKindRefusedAndRedacted(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	id := hubConfigTokenUser(t, s, "hct-nocred", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(id, id+"@test.com", "Admin", "admin", "cli")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", strings.NewReader(`{"server":{"hub":{"admin_emails":["x@example.com"]}}}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rec := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rec, req, srv.GetOperationalSettings())
	requireTokenRefusedKeys(t, rec, "server.hub.admin_emails")

	hook, err := s.CreateHubPreStartHook(context.Background(), &store.ProjectPreStartHook{
		Scope: store.PreStartHookScopeHub, Name: "hct-nocred-hook", Slug: "hct-nocred-hook", Script: "#!/bin/sh\necho nocred\n",
		CreatedBy: id + "@test.com", UpdatedBy: id + "@test.com",
	})
	require.NoError(t, err)
	getHook := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, hubPSHPath+"/"+hook.ID, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.handleHubPreStartHookByID(rec, req)
		return rec
	}

	session := contextWithCredentialContext(contextWithIdentity(context.Background(), admin), CredentialContext{Kind: CredentialKindInteractive})
	rec = getHook(session)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "echo nocred", "an interactive session of the super-admin sees the script")

	rec = getHook(contextWithIdentity(context.Background(), admin))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "echo nocred", "no credential context gets the redacted script")
}

// TestHubConfigWrite_RepeatedMemberNamesRejected requires a hub
// configuration write whose body repeats an object member name, at any
// depth and under the decoder's case folding, to be rejected with 400 for
// every credential before anything is written, and a body with no repeated
// member to be processed.
func TestHubConfigWrite_RepeatedMemberNamesRejected(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-dup-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")

	repeated := []string{
		`{"server":{"hub":{"public_url":"https://zz.example.com"}},"server":{"hub":{"hub_name":"zz"}}}`,
		`{"server":{"hub":{"admin_emails":["zz@example.com"]}},"server":{}}`,
		`{"agent_secrets":{"user_scope_only":false},"agent_secrets":{}}`,
		`{"server":{"hub":{"hub_name":"a","hub_name":"b"}}}`,
		`{"server":{"hub":{"hub_name":"a"}},"Server":{"hub":{"public_url":"https://zz.example.com"}}}`,
		`{"profiles":{"p":{"runtime":"a","runtime":"b"}}}`,
	}
	for _, body := range repeated {
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
			rec := doRequestWithToken(t, srv, key, method, "/api/v1/admin/server-config", json.RawMessage(body))
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s %s: %s", method, body, rec.Body.String())
		}
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(body))
		require.Equal(t, http.StatusBadRequest, rec.Code, "session %s: %s", body, rec.Body.String())
	}
	for _, section := range []string{"endpoints", "access", "agent_secrets", "profiles"} {
		_, err := s.GetHubSetting(context.Background(), section)
		require.ErrorIs(t, err, store.ErrNotFound, "section %s must not be written", section)
	}

	rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(`{"server":{"hub":{"hub_name":"zz"}}}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	pdKey := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_project_defaults:read", "hub_project_defaults:update")
	rec = doRequestWithToken(t, srv, pdKey, http.MethodPut, "/api/v1/admin/project-defaults", json.RawMessage(`{"default_scratchpad":true,"default_scratchpad":false}`))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	fileSrv, fileStore := testServer(t)
	fileAdmin := hubConfigTokenUser(t, fileStore, "hct-dup-file-super", store.SystemRoleSuperAdmin)
	fileKey := mintHubConfigToken(t, fileSrv, fileAdmin, hubBoundary(), "hub_config:read", "hub_config:update")
	rec = doRequestWithToken(t, fileSrv, fileKey, http.MethodPut, "/api/v1/admin/server-config", json.RawMessage(repeated[0]))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestRepeatedJSONMember_FindsRepeatsInOneObjectOnly requires a repeat to
// be reported only for two members of the same object, at any depth, and a
// body that is not exactly one JSON value to be an error, never "no
// repeat".
func TestRepeatedJSONMember_FindsRepeatsInOneObjectOnly(t *testing.T) {
	cases := []struct {
		body    string
		want    string
		invalid bool
	}{
		{body: `{"a":1,"b":2}`},
		{body: "{\"a\":1}  \n"},
		{body: `{"a":{"x":1},"b":{"x":2}}`},
		{body: `[{"a":1},{"a":2}]`},
		{body: `{"a":[{"x":1},{"x":2}],"b":[1,2,{"y":{}}]}`},
		{body: `{"a":1,"a":2}`, want: "a"},
		{body: `{"a":1,"A":2}`, want: "A"},
		{body: `{"k":1,"\u212a":2}`, want: "\u212a"},
		{body: `{"a":{"b":{"c":1,"c":2}}}`, want: "c"},
		{body: `{"a":[{"x":1,"x":2}]}`, want: "x"},
		{body: `{"profiles":{"Foo":{},"foo":{}}}`, want: "foo"},
		{body: `not json`, invalid: true},
		{body: ``, invalid: true},
		{body: `{"a":1`, invalid: true},
		{body: `{"a":1}x`, invalid: true},
		{body: `{"a":1}]`, invalid: true},
		{body: `{"a":1} {}`, invalid: true},
		{body: `{"a":1} {"a":2}`, invalid: true},
		{body: `]`, invalid: true},
		{body: `}`, invalid: true},
		{body: `[1]]`, invalid: true},
		{body: `{}}`, invalid: true},
	}
	for _, tc := range cases {
		got, repeated, err := repeatedJSONMember([]byte(tc.body))
		if tc.invalid {
			assert.Error(t, err, "%q must be refused as not one JSON value", tc.body)
			assert.False(t, repeated, tc.body)
			continue
		}
		require.NoError(t, err, tc.body)
		if tc.want == "" {
			assert.False(t, repeated, "%s reported %q", tc.body, got)
			continue
		}
		assert.True(t, repeated, tc.body)
		assert.Equal(t, tc.want, got, tc.body)
	}
}

// TestHubConfigKeyClassifiers_UnparsedBodyIsRefused requires each settings
// key classifier to refuse a body it cannot parse instead of reporting no
// refused key.
func TestHubConfigKeyClassifiers_UnparsedBodyIsRefused(t *testing.T) {
	for _, body := range []string{`not json`, `{"server":{}}x`, `{"server":{}} {}`, `[]`, ``} {
		assert.Equal(t, []string{unparsedBodyKey}, tokenRefusedServerConfigKeys([]byte(body)), "server-config %q", body)
		assert.Equal(t, []string{unparsedBodyKey}, tokenRefusedProjectDefaultsKeys([]byte(body)), "project-defaults %q", body)
	}
}

// rawHubConfigRequest sends body verbatim with the given bearer key.
func rawHubConfigRequest(srv *Server, key, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHubConfigWrite_BodyMustBeOneJSONValue requires a hub configuration
// write whose body is followed by trailing data or a second JSON value,
// closes a container it never opened, or does not parse, to be rejected with 400 for a token and a session on the
// file-backed and the DB-backed hub, with nothing written.
func TestHubConfigWrite_BodyMustBeOneJSONValue(t *testing.T) {
	bodies := []string{
		`{"server":{"hub":{"public_url":"https://zz.example.com"}}} {}`,
		`{"server":{"hub":{"admin_emails":["zz@example.com"]}}}x`,
		`{"agent_secrets":{"user_scope_only":false}}]`,
		`{"server":{"hub":{"hub_name":"zz"}}} {"server":{"hub":{"admin_emails":["zz@example.com"]}}}`,
		`{"server":{"hub":{"admin_emails":["zz@example.com"]}}`,
		`]`,
		`[1]]`,
	}

	t.Run("file-backed hub", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		settingsPath := filepath.Join(home, ".scion", "settings.yaml")
		require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
		srv, s := testServer(t)
		admin := hubConfigTokenUser(t, s, "hct-trail-file", store.SystemRoleSuperAdmin)
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")
		for _, body := range bodies {
			for _, bearer := range []string{key, testDevToken} {
				rec := rawHubConfigRequest(srv, bearer, http.MethodPut, "/api/v1/admin/server-config", body)
				require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", body, rec.Body.String())
			}
			_, err := os.Stat(settingsPath)
			require.True(t, os.IsNotExist(err), "settings.yaml must not be written for %s", body)
		}
		// Control: the same hub writes settings.yaml for a valid body.
		rec := rawHubConfigRequest(srv, key, http.MethodPut, "/api/v1/admin/server-config", `{"server":{"hub":{"hub_name":"zz"}}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, err := os.Stat(settingsPath)
		require.NoError(t, err, "a valid body writes settings.yaml")
	})

	t.Run("DB-backed hub", func(t *testing.T) {
		srv, s := testServerWithOps(t, nil)
		admin := hubConfigTokenUser(t, s, "hct-trail-db", store.SystemRoleSuperAdmin)
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_config:read", "hub_config:update")
		for _, body := range bodies {
			for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
				rec := rawHubConfigRequest(srv, key, method, "/api/v1/admin/server-config", body)
				require.Equal(t, http.StatusBadRequest, rec.Code, "%s %s: %s", method, body, rec.Body.String())
			}
			rec := rawHubConfigRequest(srv, testDevToken, http.MethodPut, "/api/v1/admin/server-config", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "session %s: %s", body, rec.Body.String())
		}
		for _, section := range []string{"endpoints", "access", "agent_secrets"} {
			_, err := s.GetHubSetting(context.Background(), section)
			require.ErrorIs(t, err, store.ErrNotFound, "section %s must not be written", section)
		}

		pdKey := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_project_defaults:read", "hub_project_defaults:update")
		for _, body := range []string{`{"default_scratchpad":false} {}`, `{"default_scratchpad":false}x`, `{"default_scratchpad":`} {
			rec := rawHubConfigRequest(srv, pdKey, http.MethodPut, "/api/v1/admin/project-defaults", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", body, rec.Body.String())
		}
		_, err := s.GetHubSetting(context.Background(), "project_defaults")
		require.ErrorIs(t, err, store.ErrNotFound, "project defaults must not be written")
	})
}

// TestLifecycleHookWrite_ExecutionIdentityRequiresSession requires a token
// that creates or updates a lifecycle hook to leave its execution identity
// as it is: setting, changing or clearing it is refused with the
// GOV_PENDING session-only reason, and other hook fields stay writable.
func TestLifecycleHookWrite_ExecutionIdentityRequiresSession(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	admin := hubConfigTokenUser(t, s, "hct-lh-admin", store.SystemRoleHubAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_lifecycle_hooks:read", "hub_lifecycle_hooks:update")

	create := validCreateRequest()
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/admin/lifecycle-hooks", create)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var plain store.LifecycleHook
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))

	withIdentity := validCreateRequest()
	withIdentity.Name = "with-identity"
	withIdentity.ExecutionIdentity = "hct-sa"
	rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/admin/lifecycle-hooks", withIdentity)
	requireTokenRefusedKeys(t, rec, "executionIdentity")

	update := func(h store.LifecycleHook, name, identity string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/lifecycle-hooks/"+h.ID, updateLifecycleHookRequest{
			Name: name, Selector: h.Selector, Trigger: h.Trigger, Action: h.Action,
			ExecutionIdentity: identity, Enabled: h.Enabled, StateVersion: h.StateVersion,
		})
	}
	rec = update(plain, "renamed", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))
	requireTokenRefusedKeys(t, update(plain, "renamed-again", "hct-sa"), "executionIdentity")

	seeded := &store.LifecycleHook{
		ID: uuid.New().String(), Name: "seeded", ScopeType: store.LifecycleHookScopeHub,
		Trigger: store.LifecycleHookTriggerRunning, Action: validWebhookAction(),
		ExecutionIdentity: "hct-seeded-sa", Enabled: true, CreatedBy: admin + "@test.com",
	}
	require.NoError(t, s.CreateLifecycleHook(context.Background(), seeded))
	stored, err := s.GetLifecycleHook(context.Background(), seeded.ID)
	require.NoError(t, err)
	requireTokenRefusedKeys(t, update(*stored, "seeded", ""), "executionIdentity")
	requireTokenRefusedKeys(t, update(*stored, "seeded", "hct-other-sa"), "executionIdentity")

	// A hook whose execution identity resolves and that the holder may act
	// as: a token update that keeps the identity and renames the hook is
	// admitted, and the identity stays as stored.
	sa := wiringSA(t, s, store.ScopeHub, "test-hub-id", "hct-keep@p.iam.gserviceaccount.com")
	enforceHookIdentity(srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))
	resolvable := &store.LifecycleHook{
		ID: uuid.New().String(), Name: "resolvable", ScopeType: store.LifecycleHookScopeHub,
		Trigger: store.LifecycleHookTriggerRunning, Action: validWebhookAction(),
		ExecutionIdentity: sa.ID, Enabled: true, CreatedBy: admin + "@test.com",
	}
	require.NoError(t, s.CreateLifecycleHook(context.Background(), resolvable))
	stored, err = s.GetLifecycleHook(context.Background(), resolvable.ID)
	require.NoError(t, err)
	rec = update(*stored, "resolvable-renamed", sa.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after, err := s.GetLifecycleHook(context.Background(), resolvable.ID)
	require.NoError(t, err)
	assert.Equal(t, "resolvable-renamed", after.Name)
	assert.Equal(t, sa.ID, after.ExecutionIdentity)
}
