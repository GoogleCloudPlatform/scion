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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// harness config named like a global one: the resource fails with the
// refusal and no project row is written; a workstation hub imports it.
func TestHarnessConfigImport_GlobalSlug(t *testing.T) {
	for _, workstation := range []bool{false, true} {
		name := "hosted refuses"
		if workstation {
			name = "workstation accepts"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testInstallSourceServer(t)
			srv.workstation = workstation
			_, projectID := seedProjectOwner(t, s, "hc-import-global-slug")
			seedHarnessConfigRow(t, s, "shared-name", store.HarnessConfigScopeGlobal, "")

			src := tarGzFilesServer(t, map[string]string{
				"config.yaml": "name: shared-name\nharness: claude\n",
			})
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
				Kind: "harness-config", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
				SourceURL: src.URL + "/configs/shared-name.tar.gz",
			})
			if workstation {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.True(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
				return
			}
			assert.Contains(t, rec.Body.String(), `harness config slug \"shared-name\" is reserved`, rec.Body.String())
			assert.False(t, projectHarnessConfigExists(t, s, "shared-name", projectID))
		})
	}
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

	projects, err := s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, p := range projects.Items {
		assert.NotEqual(t, "Cloned Shadow", p.Name, "a refused clone must be rolled back")
	}
}
