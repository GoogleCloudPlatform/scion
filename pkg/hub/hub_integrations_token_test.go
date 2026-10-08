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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubIntegrationSelectors are the hub integration and observability
// selectors, all hub-only.
var hubIntegrationSelectors = []string{
	"hub_scheduler:read", "hub_health:read", "hub_validate:execute",
	"hub_integrations:read", "hub_integrations:update", "hub_teams_manifest:read",
	"hub_diagnostics:read", "hub_metrics:read", "hub_github_app:read", "hub_github_app:update",
}

// requireSessionOnlyRefusal asserts a session-only refusal with reason.
func requireSessionOnlyRefusal(t *testing.T, rec *httptest.ResponseRecorder, reason authzop.SessionOnlyReason) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	r, credential := sessionOnlyDetailsOf(rec)
	assert.Equal(t, string(reason), r, rec.Body.String())
	assert.Equal(t, sessionRequiredCredential, credential, rec.Body.String())
}

// requireNoSessionOnlyRefusal asserts the response is not a session-only
// refusal.
func requireNoSessionOnlyRefusal(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	_, credential := sessionOnlyDetailsOf(rec)
	assert.NotEqual(t, sessionRequiredCredential, credential, "%d %s", rec.Code, rec.Body.String())
}

// requireTokenRefusedIntegrationKeys asserts a refusal of integration
// config keys: 403, reason CREDENTIAL_MANAGEMENT and details.keys equal to
// want.
func requireTokenRefusedIntegrationKeys(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	requireSessionOnlyRefusal(t, rec, authzop.ReasonCredentialManagement)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	var got []string
	if raw, ok := resp.Error.Details["keys"].([]interface{}); ok {
		for _, k := range raw {
			got = append(got, k.(string))
		}
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
}

// TestHubIntegrationSelectors_ProjectBoundaryRefused requires the hub
// integration and observability selectors to be hub-only: a super-admin
// who owns a project mints each on a hub token but not on a project token,
// and a project token reaches none of their routes.
func TestHubIntegrationSelectors_ProjectBoundaryRefused(t *testing.T) {
	srv, s := testServer(t)
	project := tid("hit-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: project, Name: "HIT", Slug: "hit-project"}))
	super := hubConfigTokenUser(t, s, "hit-pb-super", store.SystemRoleSuperAdmin)
	createTestUserWithProjectRole(t, s, super, super+"@test.com", project, store.ProjectRoleOwner)

	for _, sel := range hubIntegrationSelectors {
		_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(super), CreateTokenParams{
			UserID: super, Name: "hit-" + tid("pb"), Boundary: projectBoundary(project), Scopes: []string{sel},
		})
		require.Error(t, err, "a project token with %s must not mint", sel)
		mintHubConfigToken(t, srv, super, hubBoundary(), sel)
	}

	key := mintHubConfigToken(t, srv, super, projectBoundary(project), "project:read")
	for _, path := range []string{
		"/api/v1/admin/scheduler", "/api/v1/admin/gcp-quota", "/api/v1/admin/health/summary",
		"/api/v1/admin/validate-resources", "/api/v1/admin/integrations", "/api/v1/admin/integrations/available",
		"/api/v1/admin/integrations/teams/manifest", "/api/v1/admin/diagnostics/logs",
		"/api/v1/admin/messaging/divergence", "/api/v1/metrics/dashboard", "/api/v1/admin/metrics-dashboard",
		"/api/v1/github-app", "/api/v1/github-app/installations",
	} {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

// TestIntegrationInstall_SessionOnlyForTokens requires installing an
// integration and starting an integration update to be session-only
// (HOST_OPERATIONS): a super-admin's hub token with every integration
// selector is refused with the reason, and a session is not.
func TestIntegrationInstall_SessionOnlyForTokens(t *testing.T) {
	srv, s := testServer(t)
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	srv.pluginManager = mgr
	super := hubConfigTokenUser(t, s, "hit-inst-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_integrations:read", "hub_integrations:update")

	for _, path := range []string{
		"/api/v1/admin/integrations/telegram/install",
		"/api/v1/admin/integrations/telegram/update",
		"/api/v1/admin/integrations/telegram/update/latest",
	} {
		rec := doRequestWithToken(t, srv, key, http.MethodPost, path, nil)
		requireSessionOnlyRefusal(t, rec, authzop.ReasonHostOperations)

		rec = doRequest(t, srv, http.MethodPost, path, nil)
		requireNoSessionOnlyRefusal(t, rec)
	}

	// The same token reads update status and restarts: only install and
	// starting an update are session-only.
	rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/integrations/telegram/update/latest", nil)
	requireNoSessionOnlyRefusal(t, rec)
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/admin/integrations/telegram/restart", nil)
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestGitHubAppConfigUpdate_SessionOnlyForTokens requires the GitHub App
// configuration update to be session-only (CREDENTIAL_MANAGEMENT), while
// the same token manages installations on hub_github_app:update.
func TestGitHubAppConfigUpdate_SessionOnlyForTokens(t *testing.T) {
	srv, s := testServer(t)
	super := hubConfigTokenUser(t, s, "hit-gha-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_github_app:read", "hub_github_app:update")

	rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/github-app", map[string]interface{}{"app_id": 1})
	requireSessionOnlyRefusal(t, rec, authzop.ReasonCredentialManagement)

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/github-app", map[string]interface{}{})
	requireNoSessionOnlyRefusal(t, rec)
	require.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/github-app/installations",
		map[string]interface{}{"installation_id": 4242, "account_login": "hit-org"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/github-app/installations/4242", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/github-app/installations/4242", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// hub_github_app:read alone does not write installations.
	readKey := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_github_app:read")
	rec = doRequestWithToken(t, srv, readKey, http.MethodPost, "/api/v1/github-app/installations",
		map[string]interface{}{"installation_id": 4243, "account_login": "hit-org"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestMetricsDashboard_RequiresMetricsSelector requires hub.metrics.read
// on both metrics dashboard paths: a hub admin's token needs
// hub_metrics:read, and a hub member's session is refused.
func TestMetricsDashboard_RequiresMetricsSelector(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "hit-met-admin", store.SystemRoleHubAdmin)
	without := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_health:read")
	with := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_metrics:read")

	member := hubConfigTokenUser(t, s, "hit-met-member", store.SystemRoleHubMember)
	memberUser, err := s.GetUser(context.Background(), member)
	require.NoError(t, err)

	for _, path := range []string{"/api/v1/metrics/dashboard", "/api/v1/admin/metrics-dashboard"} {
		rec := doRequestWithToken(t, srv, without, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s without the selector: %s", path, rec.Body.String())

		rec = doRequestWithToken(t, srv, with, http.MethodGet, path, nil)
		require.NotEqual(t, http.StatusForbidden, rec.Code, "%s with the selector: %s", path, rec.Body.String())
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s with the selector: %s", path, rec.Body.String())

		rec = doRequestAsUser(t, srv, memberUser, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s as a hub member: %s", path, rec.Body.String())
	}
}

// TestIntegrationConfigUpdate_SecretsSessionOnlyForTokens requires a
// token's integration config update to carry configuration settings keys
// only: any entry in secrets, and every settings key outside the
// integration's configuration set (credential material, authentication,
// identity mapping, endpoints, addresses, host paths, unlisted keys), is
// refused with reason CREDENTIAL_MANAGEMENT before anything is written. A
// session is not subject to the rule.
func TestIntegrationConfigUpdate_SecretsSessionOnlyForTokens(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "telegram.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("agent_cache_ttl: 5m\n"), 0o600))
	srv, s := testServer(t)
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{"config_file": configFile}
	srv.pluginManager = mgr
	super := hubConfigTokenUser(t, s, "hit-cfg-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_integrations:read", "hub_integrations:update")
	path := "/api/v1/admin/integrations/telegram/config"

	refused := []struct {
		name string
		body map[string]interface{}
		keys []string
	}{
		{"secret", map[string]interface{}{"secrets": map[string]string{"bot_token": "x"}}, []string{"secrets"}},
		{"secret with empty value", map[string]interface{}{"secrets": map[string]string{"bot_token": ""}}, []string{"secrets"}},
		{"secret config key as a setting", map[string]interface{}{"settings": map[string]string{"bot_token": "x"}}, []string{"settings.bot_token"}},
		{"webhook url", map[string]interface{}{"settings": map[string]string{"webhook_url": "https://x.example.com"}}, []string{"settings.webhook_url"}},
		{"register url", map[string]interface{}{"settings": map[string]string{"register_url": "https://x.example.com"}}, []string{"settings.register_url"}},
		{"user mappings", map[string]interface{}{"settings": map[string]string{"user_mappings": "{}"}}, []string{"settings.user_mappings"}},
		{"host path", map[string]interface{}{"settings": map[string]string{"db_path": "/tmp/x.db"}}, []string{"settings.db_path"}},
		{"listen address", map[string]interface{}{"settings": map[string]string{"webhook_listen": ":1"}}, []string{"settings.webhook_listen"}},
		{"wiring key", map[string]interface{}{"settings": map[string]string{"hub_url": "https://x.example.com"}}, []string{"settings.hub_url"}},
		{"unlisted key", map[string]interface{}{"settings": map[string]string{"not_a_key": "1"}}, []string{"settings.not_a_key"}},
		{"mixed with configuration", map[string]interface{}{
			"settings": map[string]string{"agent_cache_ttl": "1m", "webhook_url": "https://x.example.com"},
			"secrets":  map[string]string{"webhook_secret": "x"},
		}, []string{"secrets", "settings.webhook_url"}},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			rec := doRequestWithToken(t, srv, key, http.MethodPut, path, tc.body)
			requireTokenRefusedIntegrationKeys(t, rec, tc.keys...)
		})
	}
	data, err := os.ReadFile(configFile)
	require.NoError(t, err)
	assert.Equal(t, "agent_cache_ttl: 5m\n", string(data), "a refused update must write nothing")

	t.Run("configuration keys", func(t *testing.T) {
		rec := doRequestWithToken(t, srv, key, http.MethodPut, path,
			map[string]interface{}{"settings": map[string]string{"agent_cache_ttl": "1m", "inbound_mode": "poll"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		data, err := os.ReadFile(configFile)
		require.NoError(t, err)
		assert.Contains(t, string(data), "1m")
	})

	t.Run("an integration with no configuration keys refuses every setting", func(t *testing.T) {
		mgr.plugins["discord"] = map[string]string{"config_file": filepath.Join(t.TempDir(), "discord.yaml")}
		rec := doRequestWithToken(t, srv, key, http.MethodPut, "/api/v1/admin/integrations/discord/config",
			map[string]interface{}{"settings": map[string]string{"guild_ids": "1"}})
		requireTokenRefusedIntegrationKeys(t, rec, "settings.guild_ids")
	})

	t.Run("session is not subject to the token rule", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, path,
			map[string]interface{}{"settings": map[string]string{"webhook_listen": ":9095"}})
		requireNoSessionOnlyRefusal(t, rec)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("update selector alone is refused at the route guard", func(t *testing.T) {
		updateOnly := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_integrations:update")
		rec := doRequestWithToken(t, srv, updateOnly, http.MethodPut, path,
			map[string]interface{}{"settings": map[string]string{"agent_cache_ttl": "2m"}})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		requireNoSessionOnlyRefusal(t, rec)
	})
}

// TestIntegrationTokenSettingsKeys_EveryKnownPluginClassified requires
// every known integration to have an entry in integrationTokenSettingsKeys
// and no entry to admit a secret-mapped key or a wiring or internal key. A
// missing credential kind is refused.
func TestIntegrationTokenSettingsKeys_EveryKnownPluginClassified(t *testing.T) {
	for _, p := range knownPluginCatalog {
		if _, ok := integrationTokenSettingsKeys[p.Name]; !ok {
			t.Errorf("known integration %q has no entry in integrationTokenSettingsKeys", p.Name)
		}
	}
	for name := range integrationTokenSettingsKeys {
		if lookupKnownPlugin(name) == nil {
			t.Errorf("integrationTokenSettingsKeys names unknown integration %q", name)
		}
	}
	notConfiguration := map[string]bool{
		"mode": true, "path": true, "address": true, "tls_cert_file": true, "tls_key_file": true,
		"tls_ca_file": true, "tls_skip_verify": true, "config_file": true, "hub_url": true,
		"hmac_key": true, "broker_id": true, "bot_id": true, "plugin_name": true,
		"project_slug_map": true, "database_url": true, "database_driver": true,
	}
	for name, keys := range integrationTokenSettingsKeys {
		for key := range keys {
			if notConfiguration[key] || allSecretConfigKeys(name)[key] {
				t.Errorf("integration %q admits %q for tokens", name, key)
			}
		}
	}
	for name, mappings := range config.PluginSecretKeyMap {
		for _, m := range mappings {
			got := tokenRefusedIntegrationConfigKeys(name, IntegrationConfigUpdateRequest{Settings: map[string]string{m.ConfigKey: ""}})
			assert.Equal(t, []string{"settings." + m.ConfigKey}, got, "%s %s", name, m.ConfigKey)
		}
	}
	assert.Equal(t, []string{"settings.agent_cache_ttl"},
		tokenRefusedIntegrationConfigKeys("not-an-integration", IntegrationConfigUpdateRequest{Settings: map[string]string{"agent_cache_ttl": "1m"}}))

	rec := httptest.NewRecorder()
	require.True(t, writeTokenRefusedIntegrationKeys(rec, context.Background(), []string{"secrets"}),
		"a request with no credential kind is refused")
	requireTokenRefusedIntegrationKeys(t, rec, "secrets")
}

// blockingLogQuerier serves a log tail that stays open until the request
// ends.
type blockingLogQuerier struct{ fakeLogQuerier }

func (b *blockingLogQuerier) Tail(ctx context.Context, _ LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	return make(chan CloudLogEntry), func() {}, nil
}

// TestDiagnosticsLogStream_EndsWhenTokenStopsValidating requires the
// diagnostics log stream opened by a token to end, with the same event for
// every cause, once the token is revoked, expires, belongs to a suspended
// user or no longer holds hub.diagnostics.read; a token that still
// validates keeps the stream open.
func TestDiagnosticsLogStream_EndsWhenTokenStopsValidating(t *testing.T) {
	prev := streamCredentialRecheckInterval
	streamCredentialRecheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { streamCredentialRecheckInterval = prev })

	srv, s := testServer(t)
	srv.logQueryService = &blockingLogQuerier{}
	ctx := context.Background()
	const path = "/api/v1/admin/diagnostics/logs/stream"

	newUser := func(name string) string {
		id := tid(name)
		createTestUserWithRole(t, s, id, id+"@test.com", "member", store.SystemRoleSuperAdmin)
		ensureHubMembership(ctx, s, id)
		return id
	}

	// openStream starts the stream and returns a channel closed when the
	// handler returns, the recorder, and a cancel for the request.
	openStream := func(key string) (chan struct{}, *httptest.ResponseRecorder, context.CancelFunc) {
		reqCtx, cancel := context.WithCancel(ctx)
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(reqCtx)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			srv.Handler().ServeHTTP(rec, req)
			close(done)
		}()
		return done, rec, cancel
	}
	stillOpen := func(t *testing.T, done chan struct{}) {
		t.Helper()
		select {
		case <-done:
			t.Fatal("stream ended while the token still validates")
		case <-time.After(10 * streamCredentialRecheckInterval):
		}
	}
	waitEnded := func(t *testing.T, done chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("stream did not end")
		}
	}

	causes := []struct {
		name   string
		expiry time.Duration
		change func(t *testing.T, userID, tokenID string)
	}{
		{"revoked", 0, func(t *testing.T, userID, tokenID string) {
			require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(userID), userID, tokenID))
		}},
		{"expired", time.Second, func(t *testing.T, userID, tokenID string) {}},
		{"suspended", 0, func(t *testing.T, userID, tokenID string) {
			setUserStatus(t, s, userID, store.UserStatusSuspended)
		}},
		{"denied", 0, func(t *testing.T, userID, tokenID string) {
			rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
			require.NoError(t, err)
			bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
			require.NoError(t, err)
			for _, b := range bindings {
				if b.RoleDefinitionID == rd.ID {
					require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
				}
			}
		}},
	}
	var events []string
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			userID := newUser("hit-diag-" + tc.name)
			params := CreateTokenParams{
				UserID: userID, Name: "hit-" + tid("diag"), Boundary: hubBoundary(), Scopes: []string{"hub_diagnostics:read"},
			}
			if tc.expiry > 0 {
				exp := time.Now().Add(tc.expiry)
				params.ExpiresAt = &exp
			}
			key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), params)
			require.NoError(t, err)

			done, rec, cancel := openStream(key)
			defer cancel()
			if tc.expiry == 0 {
				stillOpen(t, done)
			}
			tc.change(t, userID, token.ID)
			waitEnded(t, done)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), streamCredentialEndedEvent)
			body := rec.Body.String()
			events = append(events, body[len(body)-len(streamCredentialEndedEvent):])
		})
	}
	for _, e := range events {
		assert.Equal(t, streamCredentialEndedEvent, e, "every cause ends the stream with the same event")
	}

	t.Run("a valid token keeps the stream open", func(t *testing.T) {
		userID := newUser("hit-diag-valid")
		key := mintHubConfigToken(t, srv, userID, hubBoundary(), "hub_diagnostics:read")
		done, rec, cancel := openStream(key)
		stillOpen(t, done)
		cancel()
		waitEnded(t, done)
		assert.NotContains(t, rec.Body.String(), streamCredentialEndedEvent)
	})
}
