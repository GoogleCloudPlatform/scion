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
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Restore of deleted built-ins (ptone/scion#3544, Phase 2; design section 4
// and section 10 "Restore (Phase 2)").

// testRestoreServer is a dev-auth test server with storage, after one
// hosted bundled-resource bootstrap (every built-in seeded and in the
// ledger).
func testRestoreServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	require.NoError(t, srv.BootstrapBundledResources(context.Background(), BootstrapOptions{}))
	return srv, s
}

func deleteHarnessConfigViaAPI(t *testing.T, srv *Server, s store.Store, slug string) {
	t.Helper()
	hc, err := s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
	_, err = s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeGlobal, "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func deleteDefaultTemplateViaAPI(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	tpl, err := s.GetTemplateBySlug(context.Background(), "default", string(store.TemplateScopeGlobal), "")
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tpl.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
	_, err = s.GetTemplateBySlug(context.Background(), "default", string(store.TemplateScopeGlobal), "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func decodeRestoreResponse(t *testing.T, rec *httptest.ResponseRecorder) RestoreBuiltinsResponse {
	t.Helper()
	var resp RestoreBuiltinsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// newRestoreViewer creates a hub-viewer: it may read the catalog but holds
// no global harness_config.create or template.create.
func newRestoreViewer(t *testing.T, s store.Store) *store.User {
	t.Helper()
	ctx := context.Background()
	viewer := &store.User{
		ID:          tid("user-restore-viewer"),
		Email:       "restore-viewer@test.com",
		DisplayName: "Restore Viewer",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)
	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)
	return viewer
}

func TestRestoreBuiltin_HarnessConfig_DeletedIsRestored(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	deleteHarnessConfigViaAPI(t, srv, s, "claude")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeRestoreResponse(t, rec)
	assert.Equal(t, []string{"claude"}, resp.Restored)
	assert.Empty(t, resp.AlreadyPresent)

	hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err, "claude must be back after restore")
	assert.True(t, IsBuiltinManaged(hc.SourceURL), "restored SourceURL %q must be built-in-managed", hc.SourceURL)
	assert.Equal(t, store.HarnessConfigStatusActive, hc.Status)
	assert.Equal(t, "claude", hc.Harness)

	ledger, err := srv.loadBuiltinSeedLedger(ctx)
	require.NoError(t, err)
	assert.True(t, ledger.Seen(storage.ResourceKindHarnessConfig, "claude"),
		"restore never removes a name from the ledger")
}

func TestRestoreBuiltin_Template_DeletedIsRestored(t *testing.T) {
	srv, s := testRestoreServer(t)
	deleteDefaultTemplateViaAPI(t, srv, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/restore",
		map[string]interface{}{"names": []string{"default"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeRestoreResponse(t, rec)
	assert.Equal(t, []string{"default"}, resp.Restored)
	assert.Empty(t, resp.AlreadyPresent)

	tpl, err := s.GetTemplateBySlug(context.Background(), "default", string(store.TemplateScopeGlobal), "")
	require.NoError(t, err, "default template must be back after restore")
	assert.True(t, IsBuiltinManaged(tpl.SourceURL), "restored SourceURL %q must be built-in-managed", tpl.SourceURL)
}

func TestRestoreBuiltin_PresentIsAlreadyPresent(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	before, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeRestoreResponse(t, rec)
	assert.Empty(t, resp.Restored)
	assert.Equal(t, []string{"claude"}, resp.AlreadyPresent)

	after, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID, "restore must not replace an existing row")
	assert.Equal(t, before.ContentHash, after.ContentHash)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/templates/restore",
		map[string]interface{}{"names": []string{"default"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"default"}, decodeRestoreResponse(t, rec).AlreadyPresent)
}

func TestRestoreBuiltin_NonBuiltinIs400(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	deleteHarnessConfigViaAPI(t, srv, s, "claude")

	for _, tc := range []struct {
		path string
		body map[string]interface{}
	}{
		{"/api/v1/harness-configs/restore", map[string]interface{}{"names": []string{"my-custom"}}},
		{"/api/v1/templates/restore", map[string]interface{}{"names": []string{"my-template"}}},
		// A built-in harness config name is not a built-in template.
		{"/api/v1/templates/restore", map[string]interface{}{"names": []string{"claude"}}},
		// One bad name rejects the whole request before any mutation.
		{"/api/v1/harness-configs/restore", map[string]interface{}{"names": []string{"claude", "my-custom"}}},
	} {
		rec := doRequest(t, srv, http.MethodPost, tc.path, tc.body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s %v: %s", tc.path, tc.body, rec.Body.String())
	}

	_, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "a rejected request must not restore its valid names")
	_, err = s.GetHarnessConfigBySlug(ctx, "my-custom", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetTemplateBySlug(ctx, "claude", string(store.TemplateScopeGlobal), "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	created, err := srv.RestoreBuiltin(ctx, storage.ResourceKindHarnessConfig, "my-custom")
	assert.False(t, created)
	assert.True(t, errors.Is(err, ErrNotBuiltin), "RestoreBuiltin must return ErrNotBuiltin, got %v", err)
}

func TestRestoreBuiltin_WithoutGlobalCreateIs403(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	deleteDefaultTemplateViaAPI(t, srv, s)
	viewer := newRestoreViewer(t, s)

	rec := doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/templates/restore",
		map[string]interface{}{"all": true})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	_, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied restore must not create the row")
	_, err = s.GetTemplateBySlug(ctx, "default", string(store.TemplateScopeGlobal), "")
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied restore must not create the row")
}

func TestRestoreBuiltin_All(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	deleteHarnessConfigViaAPI(t, srv, s, "codex")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"all": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeRestoreResponse(t, rec)
	assert.Equal(t, []string{"claude", "codex"}, resp.Restored)

	all := builtinNames(storage.ResourceKindHarnessConfig)
	got := append(append([]string{}, resp.Restored...), resp.AlreadyPresent...)
	sort.Strings(got)
	assert.Equal(t, all, got, "all:true must cover every built-in harness config")
	for _, name := range all {
		_, err := s.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, "")
		assert.NoError(t, err, "%s must exist after restore --all", name)
	}

	deleteDefaultTemplateViaAPI(t, srv, s)
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/templates/restore",
		map[string]interface{}{"all": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"default"}, decodeRestoreResponse(t, rec).Restored)
}

func TestRestoreBuiltin_BootstrapAfterRestoreKeepsRow(t *testing.T) {
	srv, s := testRestoreServer(t)
	ctx := context.Background()
	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	deleteDefaultTemplateViaAPI(t, srv, s)

	// Deleted built-ins stay deleted across a restart (Phase 1)...
	require.NoError(t, srv.BootstrapBundledResources(ctx, BootstrapOptions{}))
	_, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	require.ErrorIs(t, err, store.ErrNotFound)

	// ...until restored.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/templates/restore",
		map[string]interface{}{"names": []string{"default"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	restored, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	restoredTpl, err := s.GetTemplateBySlug(ctx, "default", string(store.TemplateScopeGlobal), "")
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		require.NoError(t, srv.BootstrapBundledResources(ctx, BootstrapOptions{}))
		hc, err := s.GetHarnessConfigBySlug(ctx, "claude", store.HarnessConfigScopeGlobal, "")
		require.NoError(t, err, "bootstrap %d must keep the restored harness config", i)
		assert.Equal(t, restored.ID, hc.ID)
		assert.Equal(t, store.HarnessConfigStatusActive, hc.Status)
		tpl, err := s.GetTemplateBySlug(ctx, "default", string(store.TemplateScopeGlobal), "")
		require.NoError(t, err, "bootstrap %d must keep the restored template", i)
		assert.Equal(t, restoredTpl.ID, tpl.ID)
	}
}

func TestRestoreBuiltin_MarksLedgerWhenUnseen(t *testing.T) {
	// A hub whose ledger lacks the name (for example a bootstrap that never
	// created it) records it on restore, so the next bootstrap's state
	// table sees it as seeded.
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	ctx := context.Background()

	created, err := srv.RestoreBuiltin(ctx, storage.ResourceKindHarnessConfig, "codex")
	require.NoError(t, err)
	assert.True(t, created)
	_, err = s.GetHarnessConfigBySlug(ctx, "codex", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)

	ledger, err := srv.loadBuiltinSeedLedger(ctx)
	require.NoError(t, err)
	assert.True(t, ledger.Seen(storage.ResourceKindHarnessConfig, "codex"))
	row, err := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	require.NoError(t, err)
	assert.Equal(t, builtinSeedLedgerRestoredBy, row.UpdatedBy)

	created, err = srv.RestoreBuiltin(ctx, storage.ResourceKindHarnessConfig, "codex")
	require.NoError(t, err)
	assert.False(t, created, "second restore of a present row is a no-op")
}

func TestRestoreBuiltin_RequestValidation(t *testing.T) {
	srv, _ := testRestoreServer(t)
	for _, path := range []string{"/api/v1/harness-configs/restore", "/api/v1/templates/restore"} {
		rec := doRequest(t, srv, http.MethodPost, path, map[string]interface{}{})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s empty body: %s", path, rec.Body.String())

		rec = doRequest(t, srv, http.MethodPost, path, map[string]interface{}{"names": []string{"  "}})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s blank name: %s", path, rec.Body.String())

		rec = doRequest(t, srv, http.MethodPost, path,
			map[string]interface{}{"names": []string{"default"}, "all": true})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s names+all: %s", path, rec.Body.String())

		rec = doRequest(t, srv, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s GET: %s", path, rec.Body.String())
	}
}

// countingLockStore wraps a store and records LockBundledResources
// acquisitions. With busy set, the lock is reported as held elsewhere.
type countingLockStore struct {
	store.Store
	mu       sync.Mutex
	acquired int
	attempts int
	busy     bool
}

func (c *countingLockStore) TryAdvisoryLock(_ context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == store.LockBundledResources {
		c.attempts++
		if c.busy {
			return false, func() error { return nil }, nil
		}
		c.acquired++
	}
	return true, func() error { return nil }, nil
}

func (c *countingLockStore) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

// TestRestoreBuiltin_LockTakenOncePerRequest pins the round 1 F3 decision: a
// restore request takes LockBundledResources once for its whole name list,
// so a concurrently booting replica's bootstrap skip window is one restore,
// not one per name.
func TestRestoreBuiltin_LockTakenOncePerRequest(t *testing.T) {
	srv, s := testRestoreServer(t)
	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	deleteHarnessConfigViaAPI(t, srv, s, "codex")
	locker := &countingLockStore{Store: s}
	srv.store = locker

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"all": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"claude", "codex"}, decodeRestoreResponse(t, rec).Restored)
	assert.Equal(t, 1, locker.acquired, "all:true must take the bundled-resources lock once")

	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	deleteHarnessConfigViaAPI(t, srv, s, "codex")
	locker.acquired = 0
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude", "codex"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"claude", "codex"}, decodeRestoreResponse(t, rec).Restored)
	assert.Equal(t, 1, locker.acquired, "a names list must take the bundled-resources lock once")
}

// TestRestoreBuiltin_LockBusyIs503 covers the lock-held path: after the
// bounded wait the request answers 503 with Retry-After and creates nothing.
func TestRestoreBuiltin_LockBusyIs503(t *testing.T) {
	srv, s := testRestoreServer(t)
	deleteHarnessConfigViaAPI(t, srv, s, "claude")
	locker := &countingLockStore{Store: s, busy: true}
	srv.store = locker
	prev := builtinRestoreLockWait
	builtinRestoreLockWait = 300 * time.Millisecond
	t.Cleanup(func() { builtinRestoreLockWait = prev })

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/restore",
		map[string]interface{}{"names": []string{"claude"}})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.GreaterOrEqual(t, locker.attempts, 2, "restore must retry the lock before giving up")

	srv.store = s
	_, err := s.GetHarnessConfigBySlug(context.Background(), "claude", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "a busy restore must not create the row")
}
