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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertNoProjectWithSlug(t *testing.T, s store.Store, slug string) {
	t.Helper()
	_, err := s.GetProjectBySlug(context.Background(), slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no project may hold slug %q", slug)
}

func TestCreateProject_ExplicitReservedSlugRefused(t *testing.T) {
	for _, slug := range []string{"global", "GLOBAL", "Global"} {
		t.Run(slug, func(t *testing.T) {
			srv, s := testServer(t)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{
				"name": "my-app", "slug": slug,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "reserved for the global project")
			assertNoProjectWithSlug(t, s, "global")
			assertNoProjectWithSlug(t, s, slug)
		})
	}
}

func TestCreateProject_NameGlobalGetsSerialSlug(t *testing.T) {
	srv, s := testServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{"name": "Global"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var created store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	assert.Equal(t, "global-1", created.Slug)
	assertNoProjectWithSlug(t, s, "global")
}

// A git project named "global" never takes the global slug, so it gets no
// global-project exemption at a later register.
func TestProjectRegister_GitProjectNamedGlobalGetsSerialSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-gitslug-broker")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
		"name": "global", "gitRemote": "github.com/test/global",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "global-1", resp.Project.Slug)
	assertNoProjectWithSlug(t, s, "global")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", map[string]interface{}{
		"name": "global", "gitRemote": "github.com/test/global", "brokerId": broker.ID, "path": brokerGlobalDir,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	_, err := s.GetProjectProvider(ctx, resp.Project.ID, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateProject_ReservedSlugRefused(t *testing.T) {
	for _, slug := range []string{"global", "GLOBAL"} {
		t.Run(slug, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			project := &store.Project{
				ID: api.NewUUID(), Name: "my-app", Slug: "my-app", OwnerID: DevUserID, CreatedBy: DevUserID,
			}
			require.NoError(t, s.CreateProject(ctx, project))

			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID, map[string]interface{}{"slug": slug})
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "reserved for the global project")
			stored, err := s.GetProject(ctx, project.ID)
			require.NoError(t, err)
			assert.Equal(t, "my-app", stored.Slug)
		})
	}
}

func TestProjectClone_ReservedSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{
		ID: api.NewUUID(), Name: "Original", Slug: "original", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]interface{}{"name": "Copy", "slug": "global"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assertNoProjectWithSlug(t, s, "global")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/clone",
		map[string]interface{}{"name": "Global"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	assert.Equal(t, "global-1", clone.Slug)
}

// The real global project keeps working: the CLI global flow (register
// without a git remote) creates it with the global slug and its provider
// path, a re-register finds it, and dispatch marks it for the broker.
func TestProjectRegister_GlobalProjectStillRegistersAndDispatches(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := newLocalPathTestBroker(t, s, "gd-realglobal-broker")

	// The hub runs InitProject on a linked path; keep it off real dirs.
	globalDir := t.TempDir() + "/home/brokeruser/.scion"

	resp, code, body := registerWithBroker(t, srv, "global", broker.ID, globalDir)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, "global", resp.Project.Slug)

	again, code, body := registerWithBroker(t, srv, "global", broker.ID, globalDir)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, resp.Project.ID, again.Project.ID)

	// The broker's real global dir has the <home>/.scion shape; the global
	// project may store it.
	require.NoError(t, validateProviderLocalPath(resp.Project.Name, resp.Project.Slug, brokerGlobalDir))

	d := NewHTTPAgentDispatcherWithClient(s, nil, false, nil)
	info, err := d.resolveDispatchProjectInfo(ctx, &store.Agent{ProjectID: resp.Project.ID, RuntimeBrokerID: broker.ID})
	require.NoError(t, err)
	assert.Equal(t, globalDir, info.projectPath)
	assert.Equal(t, "global", info.projectSlug)
}
