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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupTestSecretBackend is a minimal secret.SecretBackend fake whose List
// method returns caller-supplied metadata per scope, for testing
// AppliedConfigEnvCleanupExecutor's reachableSecretNames matching without
// pulling in a full secret store.
type cleanupTestSecretBackend struct {
	byScope map[string][]secret.SecretMeta // key: scope+"/"+scopeID

	// listCalls counts List invocations per scope key, for tests asserting
	// the cleanup's per-scope caching (each scope should be listed at most
	// once per sweep, not once per agent). nil is fine: a nil map is never
	// written to by tests that don't care about this.
	listCalls map[string]int
}

// rawAppliedConfigView decodes just enough of an agent response body to
// check whether AppliedConfig.Env and AppliedConfig.InlineConfig.Env are
// present on the wire, without depending on the full response DTO shape.
type rawAppliedConfigView struct {
	AppliedConfig *struct {
		Env          map[string]string `json:"env"`
		InlineConfig *struct {
			Env map[string]string `json:"env"`
		} `json:"inlineConfig"`
	} `json:"appliedConfig"`
}

func assertEnvHidden(t *testing.T, view rawAppliedConfigView, context string) {
	t.Helper()
	if view.AppliedConfig == nil {
		return // appliedConfig itself absent is the strongest form of hidden
	}
	assert.Nil(t, view.AppliedConfig.Env, "%s: appliedConfig.env must be absent", context)
	if view.AppliedConfig.InlineConfig != nil {
		assert.Nil(t, view.AppliedConfig.InlineConfig.Env, "%s: appliedConfig.inlineConfig.env must be absent", context)
	}
}

func assertEnvVisibleMinusGitHubToken(t *testing.T, view rawAppliedConfigView, context string) {
	t.Helper()
	require.NotNil(t, view.AppliedConfig, "%s: appliedConfig must be present", context)
	require.NotNil(t, view.AppliedConfig.Env, "%s: appliedConfig.env must be present", context)
	assert.Equal(t, "plain-value", view.AppliedConfig.Env["PLAIN_VAR"], "%s: appliedConfig.env.PLAIN_VAR", context)
	_, hasToken := view.AppliedConfig.Env["GITHUB_TOKEN"]
	assert.False(t, hasToken, "%s: appliedConfig.env.GITHUB_TOKEN must never be present", context)

	require.NotNil(t, view.AppliedConfig.InlineConfig, "%s: appliedConfig.inlineConfig must be present", context)
	require.NotNil(t, view.AppliedConfig.InlineConfig.Env, "%s: appliedConfig.inlineConfig.env must be present", context)
	assert.Equal(t, "inline-plain-value", view.AppliedConfig.InlineConfig.Env["INLINE_PLAIN_VAR"], "%s: appliedConfig.inlineConfig.env.INLINE_PLAIN_VAR", context)
	_, hasInlineToken := view.AppliedConfig.InlineConfig.Env["GITHUB_TOKEN"]
	assert.False(t, hasInlineToken, "%s: appliedConfig.inlineConfig.env.GITHUB_TOKEN must never be present", context)
}

// patchAgentConfig issues a PATCH /api/v1/agents/{id} with a raw (map-typed)
// config body, so that an explicit empty/zero value in rawConfig actually
// reaches the wire as a present JSON key -- marshaling a *api.ScionConfig
// directly would silently omit it (every ScionConfig field is `omitempty`),
// which is exactly the ambiguity recordExplicitEdits' "present keys only"
// rule exists to resolve on the read side.
func patchAgentConfig(t *testing.T, srv *Server, agentID string, rawConfig map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]interface{}{
		"config": rawConfig,
	})
}

// configureUntouchedBody loads the golden fixture shared with
// agent-configure-build-config.test.ts's "R2-2" vitest case
// (web/src/components/pages/agent-configure-build-config.test.ts): the exact
// JSON body the real, fixed buildConfig emits for a fully untouched form
// loaded from a live config with model "golden-model" and nothing else set.
// Loading the SAME file in both places means a future buildConfig change
// that stops matching it breaks the vitest case directly, instead of
// leaving this Go test to silently test a body nobody's buildConfig
// actually produces anymore (ptone/scion#2493 R2-2).
func configureUntouchedBody(t *testing.T) map[string]interface{} {
	t.Helper()
	return loadTestdataJSONBody(t, "configure-untouched-body.json")
}

// configureRowEditBody loads the golden fixture shared with
// agent-configure-build-config.test.ts's row-edit vitest case: the exact body
// the real buildConfig emits when the user adds one custom env row (FOO) on
// an agent whose AppliedConfig.Env has an unrelated template key
// (TEMPLATE_KEY) and whose auto-expose control is untouched, so no
// SCION_AUTO_EXPOSE_* key is sent. Loading the SAME file in both places means
// a future buildConfig change that stops matching it breaks the vitest case
// directly.
func configureRowEditBody(t *testing.T) map[string]interface{} {
	t.Helper()
	return loadTestdataJSONBody(t, "configure-row-edit-body.json")
}

// runMigrationViaHandler POSTs body to the migration run endpoint, expects
// 200, and waits until the migration is no longer running.
func runMigrationViaHandler(t *testing.T, srv *Server, s store.Store, key, body string) *store.MaintenanceOperation {
	t.Helper()
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/maintenance/migrations/"+key+"/run", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceMigrations(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got *store.MaintenanceOperation
	require.Eventually(t, func() bool {
		var err error
		got, err = s.GetMaintenanceOperation(context.Background(), key)
		return err == nil && got.Status != store.MaintenanceStatusRunning
	}, 10*time.Second, 20*time.Millisecond)
	return got
}

func (b *cleanupTestSecretBackend) key(scope, scopeID string) string { return scope + "/" + scopeID }

func (b *cleanupTestSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return false, nil, nil
}
func (b *cleanupTestSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	return nil
}
func (b *cleanupTestSecretBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	if b.listCalls != nil {
		b.listCalls[b.key(filter.Scope, filter.ScopeID)]++
	}
	return b.byScope[b.key(filter.Scope, filter.ScopeID)], nil
}
func (b *cleanupTestSecretBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) UpdateMeta(ctx context.Context, input *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}
func (b *cleanupTestSecretBackend) HubID() string { return "test-hub" }
func (b *cleanupTestSecretBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	results := make(map[string]secret.FetchResult, len(metas))
	for _, meta := range metas {
		results[meta.ID] = secret.FetchResult{Err: store.ErrNotFound}
	}
	return results, nil
}

func loadTestdataJSONBody(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &body))
	return body
}
