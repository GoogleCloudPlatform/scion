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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slugsOutsideSlugFormat are client-supplied slugs that differ from their
// api.Slugify form.
var slugsOutsideSlugFormat = map[string]string{
	"uppercase":       "ProjAlpha",
	"underscore":      "proj_alpha",
	"dot":             "proj.alpha",
	"mixed":           "Proj_Alpha.v2",
	"space":           "proj alpha",
	"leading hyphen":  "-proj-alpha",
	"trailing hyphen": "proj-alpha-",
	"double hyphen":   "proj--alpha",
	"accented letter": "proj-alphé",
	"over max length": strings.Repeat("a", api.MaxSlugLength+1),
}

// slugsInSlugFormat are client-supplied slugs already equal to their
// api.Slugify form.
var slugsInSlugFormat = []string{
	"a",
	"proj-alpha",
	"proj-alpha-2",
	"2024-roadmap",
	strings.Repeat("a", api.MaxSlugLength),
}

// assertSlugFormatError checks the 400 names the slug field and the expected
// format, and does not repeat the submitted value.
func assertSlugFormatError(t *testing.T, rec *httptest.ResponseRecorder, submitted string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), submitted, "error must not repeat the submitted slug")
	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
	assert.Equal(t, projectSlugFormatMessage, resp.Error.Message)
	assert.Equal(t, "slug", resp.Error.Details["field"])
	assert.Equal(t, projectSlugFormat, resp.Error.Details["format"])
}

func assertNoProjectWithSlugOrSlugified(t *testing.T, s store.Store, slug string) {
	t.Helper()
	assertNoProjectWithSlug(t, s, slug)
	if slugified := api.Slugify(slug); slugified != "" {
		assertNoProjectWithSlug(t, s, slugified)
	}
}

func createSlugFormatSourceProject(t *testing.T, s store.Store, slug string) *store.Project {
	t.Helper()
	project := &store.Project{
		ID: api.NewUUID(), Name: "Source " + slug, Slug: slug, OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(context.Background(), project))
	return project
}

func TestIsProjectSlugFormat(t *testing.T) {
	for name, slug := range slugsOutsideSlugFormat {
		assert.False(t, isProjectSlugFormat(slug), "%s: %q", name, slug)
	}
	assert.False(t, isProjectSlugFormat(""), "empty slug")
	for _, slug := range slugsInSlugFormat {
		assert.True(t, isProjectSlugFormat(slug), "%q", slug)
	}
}

func TestCreateProject_SlugMustMatchSlugFormat(t *testing.T) {
	for name, slug := range slugsOutsideSlugFormat {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{
				"name": "Proj Alpha", "slug": slug,
			})
			assertSlugFormatError(t, rec, slug)
			assertNoProjectWithSlugOrSlugified(t, s, slug)
		})
	}
}

func TestCreateProject_SlugInSlugFormatStoredAsSubmitted(t *testing.T) {
	for _, slug := range slugsInSlugFormat {
		t.Run(slug, func(t *testing.T) {
			srv, s := testServer(t)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{
				"name": "Proj Alpha", "slug": slug,
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var created store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
			assert.Equal(t, slug, created.Slug)
			stored, err := s.GetProject(context.Background(), created.ID)
			require.NoError(t, err)
			assert.Equal(t, slug, stored.Slug)
		})
	}
}

func TestProjectClone_SlugMustMatchSlugFormat(t *testing.T) {
	for name, slug := range slugsOutsideSlugFormat {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			src := createSlugFormatSourceProject(t, s, "source")
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "Copy", "slug": slug})
			assertSlugFormatError(t, rec, slug)
			assertNoProjectWithSlugOrSlugified(t, s, slug)
		})
	}
}

func TestProjectClone_SlugInSlugFormatStoredAsSubmitted(t *testing.T) {
	for _, slug := range slugsInSlugFormat {
		t.Run(slug, func(t *testing.T) {
			srv, s := testServer(t)
			src := createSlugFormatSourceProject(t, s, "source")
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]interface{}{"name": "Copy", "slug": slug})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
			assert.Equal(t, slug, clone.Slug)
		})
	}
}

// A clone without an explicit slug derives it from the name with
// api.Slugify, also when the source project's stored slug is outside the slug
// format.
func TestProjectClone_DerivedSlugInSlugFormat(t *testing.T) {
	srv, s := testServer(t)
	src := createSlugFormatSourceProject(t, s, "Legacy_Source.v1")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]interface{}{"name": "Legacy_Source.v1 Copy"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	assert.Equal(t, "legacy-source-v1-copy", clone.Slug)
	assert.True(t, isProjectSlugFormat(clone.Slug))
}

func TestUpdateProject_SlugMustMatchSlugFormat(t *testing.T) {
	for name, slug := range slugsOutsideSlugFormat {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			project := createSlugFormatSourceProject(t, s, "proj-current")
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID,
				map[string]interface{}{"slug": slug})
			assertSlugFormatError(t, rec, slug)
			stored, err := s.GetProject(context.Background(), project.ID)
			require.NoError(t, err)
			assert.Equal(t, "proj-current", stored.Slug)
			assertNoProjectWithSlugOrSlugified(t, s, slug)
		})
	}
}

func TestUpdateProject_SlugInSlugFormatStoredAsSubmitted(t *testing.T) {
	for _, slug := range slugsInSlugFormat {
		t.Run(slug, func(t *testing.T) {
			srv, s := testServer(t)
			project := createSlugFormatSourceProject(t, s, "proj-current")
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID,
				map[string]interface{}{"slug": slug})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			stored, err := s.GetProject(context.Background(), project.ID)
			require.NoError(t, err)
			assert.Equal(t, slug, stored.Slug)
		})
	}
}

// Records stored before the slug format was required keep loading and stay
// updatable without a slug change.
func TestProject_StoredSlugOutsideSlugFormatStillLoads(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	const legacySlug = "Legacy_Project.v1"
	project := createSlugFormatSourceProject(t, s, legacySlug)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, legacySlug, got.Slug)

	bySlug, err := s.GetProjectBySlug(ctx, legacySlug)
	require.NoError(t, err)
	assert.Equal(t, project.ID, bySlug.ID)

	// A name-only update and an update repeating the stored slug both leave
	// the slug unchanged.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID,
		map[string]interface{}{"name": "Legacy Renamed"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+project.ID,
		map[string]interface{}{"name": "Legacy Renamed Again", "slug": legacySlug})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	stored, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, legacySlug, stored.Slug)
	assert.Equal(t, "Legacy Renamed Again", stored.Name)
}
