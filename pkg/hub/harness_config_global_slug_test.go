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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// seedHarnessConfigRow writes a harness config row directly to the store,
// bypassing the handlers (and so the global-slug check).
func seedHarnessConfigRow(t *testing.T, s store.Store, slug, scope, scopeID string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:      api.NewUUID(),
		Name:    slug,
		Slug:    slug,
		Harness: "claude",
		Scope:   scope,
		ScopeID: scopeID,
		Status:  store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// assertSlugReserved checks a 409 harness_config_slug_reserved answer.
func assertSlugReserved(t *testing.T, rec *httptest.ResponseRecorder, slug string) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeHarnessConfigSlugReserved, code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), slug)
}

// projectHarnessConfigExists reports whether a project-scope row with slug exists.
func projectHarnessConfigExists(t *testing.T, s store.Store, slug, projectID string) bool {
	t.Helper()
	_, err := s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeProject, projectID)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

// TestCreateHarnessConfig_GlobalSlug covers creating a project-scope harness
// config with the slug of a hub global one: refused on a hosted hub,
// accepted on a workstation hub. Other scopes and other slugs are unchanged.
func TestCreateHarnessConfig_GlobalSlug(t *testing.T) {
	projectID := tid("hc-global-slug-project")

	t.Run("hosted refuses project scope", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = false
		seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

		for _, req := range []CreateHarnessConfigRequest{
			{Name: "shared-name", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID},
			// The slug field and a name that normalizes to the slug.
			{Name: "other", Slug: "shared-name", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID},
			{Name: "Shared Name", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID},
		} {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", req)
			assertSlugReserved(t, rec, "shared-name")
		}
		assert.False(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
	})

	t.Run("hosted accepts other slugs and scopes", func(t *testing.T) {
		srv, _ := testServer(t)
		srv.workstation = false
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
			Name: "shared-name", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
			Name: "project-own", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		// A user-scope config is never resolved for other users' agents.
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
			Name: "shared-name", Harness: "claude", Scope: store.HarnessConfigScopeUser,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("workstation accepts project scope", func(t *testing.T) {
		srv, s := testServer(t)
		srv.workstation = true
		seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
			Name: "shared-name", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.True(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
	})
}

// TestCreateHarnessConfig_SettingsHarnessConfigSlug covers the global
// settings harness_configs: a hosted hub refuses a project-scope harness
// config named like one of its entries even when no hub global row exists.
func TestCreateHarnessConfig_SettingsHarnessConfigSlug(t *testing.T) {
	newServer := func(t *testing.T, workstation bool) *Server {
		t.Helper()
		srv, _ := testServer(t)
		srv.workstation = workstation
		hs := newFakeHubSettingStore()
		_, err := hs.UpsertHubSetting(context.Background(), "harness_configs",
			json.RawMessage(`{"ops-defined":{"harness":"claude","image":"example.com/img:1"}}`), "test", 0, "")
		require.NoError(t, err)
		ops := NewOperationalSettings(hs, emptyKoanf(), emptyKoanf())
		_, err = ops.Refresh(context.Background())
		require.NoError(t, err)
		require.Contains(t, ops.Snapshot().HarnessConfigs, "ops-defined")
		srv.operationalSettings.Store(ops)
		return srv
	}
	req := CreateHarnessConfigRequest{
		Name: "ops-defined", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: tid("hc-settings-slug-project"),
	}

	t.Run("hosted refuses", func(t *testing.T) {
		rec := doRequest(t, newServer(t, false), http.MethodPost, "/api/v1/harness-configs", req)
		assertSlugReserved(t, rec, "ops-defined")
	})
	t.Run("workstation accepts", func(t *testing.T) {
		rec := doRequest(t, newServer(t, true), http.MethodPost, "/api/v1/harness-configs", req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
}

// TestRenameHarnessConfig_GlobalSlug covers PUT and PATCH: a hosted hub
// refuses renaming a project-scope harness config to a global slug, and a
// metadata update that keeps the slug is not refused.
func TestRenameHarnessConfig_GlobalSlug(t *testing.T) {
	projectID := tid("hc-rename-global-slug-project")

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		t.Run(method+"/hosted refuses", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = false
			seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
			own := seedHarnessConfigRow(t, s, "project-own", store.HarnessConfigScopeProject, projectID)

			rec := doRequest(t, srv, method, "/api/v1/harness-configs/"+own.ID,
				map[string]string{"name": "project-own", "slug": "shared-name"})
			assertSlugReserved(t, rec, "shared-name")

			got, err := s.GetHarnessConfig(context.Background(), own.ID)
			require.NoError(t, err)
			assert.Equal(t, "project-own", got.Slug, "a refused rename must not change the slug")
		})

		t.Run(method+"/hosted keeps an existing slug", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = false
			seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
			// A row stored before the check (or created on a workstation hub).
			legacy := seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeProject, projectID)

			rec := doRequest(t, srv, method, "/api/v1/harness-configs/"+legacy.ID,
				map[string]string{"name": "shared-name", "slug": "shared-name", "description": "updated"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})

		t.Run(method+"/workstation accepts", func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = true
			seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
			own := seedHarnessConfigRow(t, s, "project-own", store.HarnessConfigScopeProject, projectID)

			rec := doRequest(t, srv, method, "/api/v1/harness-configs/"+own.ID,
				map[string]string{"name": "project-own", "slug": "shared-name"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// TestHandleHarnessConfigClone_GlobalSlug covers cloning into a project
// under a global slug.
func TestHandleHarnessConfigClone_GlobalSlug(t *testing.T) {
	projectID := tid("hc-clone-global-slug-project")
	for _, workstation := range []bool{false, true} {
		name := "hosted refuses"
		if workstation {
			name = "workstation accepts"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = workstation
			global := seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+global.ID+"/clone",
				map[string]string{"name": "shared-name", "scope": store.HarnessConfigScopeProject, "scopeId": projectID})
			if workstation {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				assert.True(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
				return
			}
			assertSlugReserved(t, rec, "shared-name")
			assert.False(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
		})
	}
}

// TestHarnessConfigImport_GlobalSlug covers a project-scope import of a
// harness config named like a global one: a workstation hub imports it from a
// remote source; a hosted hub refuses that loopback http:// source before
// fetching (checkImportSource), and refuses the name itself on a workspace
// import, writing no project row either way.
func TestHarnessConfigImport_GlobalSlug(t *testing.T) {
	t.Run("workstation accepts", func(t *testing.T) {
		srv, s := testInstallSourceServer(t)
		srv.workstation = true
		_, projectID := seedProjectOwner(t, s, "hc-import-global-slug")
		seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

		src := tarGzFilesServer(t, map[string]string{
			"config.yaml": "name: shared-name\nharness: claude\n",
		})
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "harness-config", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			SourceURL: src.URL + "/configs/shared-name.tar.gz",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
	})

	t.Run("hosted refuses remote source", func(t *testing.T) {
		srv, s := testInstallSourceServer(t)
		_, projectID := seedProjectOwner(t, s, "hc-import-global-slug-remote")
		seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

		src := tarGzFilesServer(t, map[string]string{
			"config.yaml": "name: shared-name\nharness: claude\n",
		})
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "harness-config", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			SourceURL: src.URL + "/configs/shared-name.tar.gz",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.False(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
	})

	t.Run("hosted refuses workspace import", func(t *testing.T) {
		srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-import-global-slug-ws")
		require.False(t, srv.workstation)
		ctx := context.Background()
		seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
		writeWorkspaceFiles(t, filepath.Join(wsRoot, ".scion", "harness-configs"), map[string]string{
			"shared-name/config.yaml": "name: shared-name\nharness: claude\n",
		})

		admin := &store.User{
			ID:          tid("user-admin-hc-import-global-slug-ws"),
			Email:       "hc-import-global-slug-ws@test.com",
			DisplayName: "Admin",
			Role:        store.UserRoleAdmin,
		}
		require.NoError(t, s.CreateUser(ctx, admin))
		ensureHubMembership(ctx, s, admin.ID)
		ensureAdminRoleBinding(t, s, admin.ID)

		rec := doRequestAsUser(t, srv, admin, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/import-harness-configs",
			ImportHarnessConfigsRequest{WorkspacePath: "/.scion/harness-configs"})
		assert.Contains(t, rec.Body.String(), `harness config slug \"shared-name\" is reserved`, rec.Body.String())
		assert.False(t, projectHarnessConfigExists(t, s, "shared-name", project.ID))
	})
}

// TestProjectClone_GlobalSlug covers a project clone whose source project
// holds a harness config under a global slug (stored before the check):
// a hosted hub refuses the clone and rolls it back.
func TestProjectClone_GlobalSlug(t *testing.T) {
	srv, s := testServer(t)
	srv.workstation = false
	src := createSourceProject(t, srv, s)
	seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
	seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeProject, src.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Cloned Shadow"})
	assertSlugReserved(t, rec, "shared-name")
	assert.Contains(t, rec.Body.String(), "source project", "the refusal must name the source project's config")

	projects, err := s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, p := range projects.Items {
		assert.NotEqual(t, "Cloned Shadow", p.Name, "a refused clone must be rolled back")
	}
}

// TestImportTemplate_BundledHarnessConfigGlobalSlug covers a template that
// bundles harness-configs/<global-slug>/ imported into a project on a
// hosted hub: that bundled config is skipped, the other bundled config and
// the template itself are imported.
func TestImportTemplate_BundledHarnessConfigGlobalSlug(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-bundle-global-slug")
	srv.workstation = false
	ctx := context.Background()
	seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

	tmplDir := filepath.Join(wsRoot, "templates", "bundle-tmpl")
	for rel, body := range map[string]string{
		"scion-agent.yaml":                        "harness: claude\n",
		"harness-configs/shared-name/config.yaml": "harness: claude\n",
		"harness-configs/own-bundled/config.yaml": "harness: claude\n",
	} {
		full := filepath.Join(tmplDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}

	imported, err := srv.importTemplatesFromWorkspace(ctx, project, "/templates")
	require.NoError(t, err)
	assert.Equal(t, []string{"bundle-tmpl"}, imported, "the template import must still succeed")

	assert.True(t, projectHarnessConfigExists(t, s, "own-bundled", project.ID))
	assert.False(t, projectHarnessConfigExists(t, s, "shared-name", project.ID),
		"a bundled config under a global slug must be skipped")
}

// errGlobalSlugLookupFault is returned by globalSlugLookupFaultStore while
// its switch is armed.
var errGlobalSlugLookupFault = errors.New("injected global harness config lookup fault")

// globalSlugLookupFaultStore fails the global-scope harness config lookup
// while armed.
type globalSlugLookupFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *globalSlugLookupFaultStore) GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*store.HarnessConfig, error) {
	if f.fault.Active() && scope == store.HarnessConfigScopeGlobal {
		return nil, errGlobalSlugLookupFault
	}
	return f.Store.GetHarnessConfigBySlug(ctx, slug, scope, scopeID)
}

// TestCreateHarnessConfig_GlobalSlugLookupFailsClosed covers a store
// failure of the global lookup: a hosted hub refuses the project-scope
// create instead of allowing it, and writes no row.
func TestCreateHarnessConfig_GlobalSlugLookupFailsClosed(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *globalSlugLookupFaultStore {
		return &globalSlugLookupFaultStore{Store: inner, fault: fault}
	})
	srv.workstation = false
	projectID := tid("hc-global-slug-fault-project")

	err := srv.checkHarnessConfigSlugNotGlobal(context.Background(), store.HarnessConfigScopeProject, "any-name")
	require.NoError(t, err, "unarmed lookup finds no global config")

	fault.Arm()
	err = srv.checkHarnessConfigSlugNotGlobal(context.Background(), store.HarnessConfigScopeProject, "any-name")
	assert.ErrorIs(t, err, errGlobalSlugLookupFault)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "any-name", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
	})
	assert.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, "body: %s", rec.Body.String())
	assert.False(t, projectHarnessConfigExists(t, s, "any-name", projectID))
}

// seedHarnessConfigRowWithHarness is seedHarnessConfigRow with a harness type.
func seedHarnessConfigRowWithHarness(t *testing.T, s store.Store, slug, harnessType, scope, scopeID string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:      api.NewUUID(),
		Name:    slug,
		Slug:    slug,
		Harness: harnessType,
		Scope:   scope,
		ScopeID: scopeID,
		Status:  store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// TestResolveHarnessConfigBySlug_ScopeOrder covers the resolution order: a
// hosted hub resolves a name to the global config even when the project
// holds a row of the same slug (stored before the write check, or before
// the global one was created); a workstation hub keeps project first. A
// project-only name resolves to the project row in both modes.
func TestResolveHarnessConfigBySlug_ScopeOrder(t *testing.T) {
	projectID := tid("hc-resolve-order-project")
	cases := []struct {
		name          string
		workstation   bool
		globalFirst   bool // create the global row before the project row
		wantGlobalHit bool
	}{
		{"hosted legacy project row", false, true, true},
		{"hosted global created after project row", false, false, true},
		{"workstation project first", true, true, false},
		{"workstation global created after project row", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = tc.workstation
			var global, project *store.HarnessConfig
			if tc.globalFirst {
				global = seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
				project = seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeProject, projectID)
			} else {
				project = seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeProject, projectID)
				global = seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")
			}
			own := seedHarnessConfigRow(t, s, "project-only", store.HarnessConfigScopeProject, projectID)

			got, err := srv.resolveHarnessConfigBySlug(context.Background(), projectID, "shared-name")
			require.NoError(t, err)
			require.NotNil(t, got)
			if tc.wantGlobalHit {
				assert.Equal(t, global.ID, got.ID, "hosted: the global config must win")
			} else {
				assert.Equal(t, project.ID, got.ID, "workstation: the project config must win")
			}

			got, err = srv.resolveHarnessConfigBySlug(context.Background(), projectID, "project-only")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, own.ID, got.ID)
		})
	}
}

// TestResolveHarnessConfigBySlug_HostedGlobalLookupFailsClosed covers a
// store failure of the global lookup: a hosted hub returns the error with
// no config instead of falling through to the project row; a workstation
// hub, which looks in the project scope first, still finds it.
func TestResolveHarnessConfigBySlug_HostedGlobalLookupFailsClosed(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		name := "hosted"
		if workstation {
			name = "workstation"
		}
		t.Run(name, func(t *testing.T) {
			srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *globalSlugLookupFaultStore {
				return &globalSlugLookupFaultStore{Store: inner, fault: fault}
			})
			srv.workstation = workstation
			projectID := tid("hc-resolve-fault-project")
			project := seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeProject, projectID)

			fault.Arm()
			got, err := srv.resolveHarnessConfigBySlug(context.Background(), projectID, "shared-name")
			if workstation {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, project.ID, got.ID)
				return
			}
			assert.ErrorIs(t, err, errGlobalSlugLookupFault)
			assert.Nil(t, got, "a hosted hub must not fall through to the project row")
		})
	}
}

// TestHarnessConfigParallelLookups_ScopeOrder covers the harness type
// resolved for a config reference (with and without the per-request memo):
// it follows dispatch's hosted/workstation order.
func TestHarnessConfigParallelLookups_ScopeOrder(t *testing.T) {
	projectID := tid("hc-parallel-order-project")
	for _, workstation := range []bool{false, true} {
		name := "hosted"
		if workstation {
			name = "workstation"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = workstation
			seedHarnessConfigRowWithHarness(t, s, "shared-name", "claude", store.HarnessConfigScopeProject, projectID)
			seedHarnessConfigRowWithHarness(t, s, "shared-name", "codex", store.HarnessConfigScopeGlobal, "")

			want := "codex"
			if workstation {
				want = "claude"
			}
			assert.Equal(t, want, srv.resolveHarnessTypeFromConfigRef(context.Background(), projectID, "shared-name"))
			memo := newHarnessTypeMemo()
			assert.Equal(t, want, srv.resolveHarnessTypeFromConfigRefMemo(context.Background(), projectID, "shared-name", memo))
		})
	}
}

// TestCreateHarnessConfig_DeletedBuiltinSlug covers a built-in harness
// config name whose global row is absent (deleted by the operator; the
// seed ledger keeps it deleted): a hosted hub still refuses it for a
// project-scope config, a workstation hub accepts it.
func TestCreateHarnessConfig_DeletedBuiltinSlug(t *testing.T) {
	builtins := resources.BuiltinHarnessConfigNames()
	require.NotEmpty(t, builtins)
	builtin := api.Slugify(builtins[0])
	projectID := tid("hc-deleted-builtin-project")

	for _, workstation := range []bool{false, true} {
		name := "hosted refuses"
		if workstation {
			name = "workstation accepts"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.workstation = workstation
			_, err := s.GetHarnessConfigBySlug(context.Background(), builtin, store.HarnessConfigScopeGlobal, "")
			require.ErrorIs(t, err, store.ErrNotFound, "precondition: no global row for the built-in")

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
				Name: builtin, Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			})
			if workstation {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				return
			}
			assertSlugReserved(t, rec, builtin)
			assert.False(t, projectHarnessConfigExists(t, s, builtin, projectID))
		})
	}
}
