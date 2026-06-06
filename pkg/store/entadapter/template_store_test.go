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

package entadapter

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestTemplateStore(t *testing.T) *TemplateStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewTemplateStore(client)
}

// =============================================================================
// Template tests
// =============================================================================

func TestCreateAndGetTemplate(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	tmpl := &store.Template{
		ID:          uuid.New().String(),
		Name:        "claude",
		Slug:        "claude",
		Harness:     "claude",
		Image:       "img:latest",
		Scope:       store.TemplateScopeGlobal,
		Visibility:  "public",
		ContentHash: "abc123",
		Config: &store.TemplateConfig{
			Harness: "claude",
			Image:   "img:latest",
			Env:     map[string]string{"FOO": "bar"},
		},
		Files: []store.TemplateFile{{Path: "home/.bashrc", Size: 10, Hash: "h"}},
	}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))
	assert.Equal(t, store.TemplateStatusActive, tmpl.Status, "empty status defaults to active")
	assert.False(t, tmpl.Created.IsZero())

	got, err := ts.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "claude", got.Name)
	assert.Equal(t, "abc123", got.ContentHash)
	require.NotNil(t, got.Config)
	assert.Equal(t, "bar", got.Config.Env["FOO"])
	require.Len(t, got.Files, 1)
	assert.Equal(t, "home/.bashrc", got.Files[0].Path)
}

func TestGetTemplateNotFound(t *testing.T) {
	ts := newTestTemplateStore(t)
	_, err := ts.GetTemplate(context.Background(), uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateTemplateDuplicateID(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: id, Name: "a", Slug: "a", Harness: "claude", Scope: "global"}))
	err := ts.CreateTemplate(ctx, &store.Template{ID: id, Name: "b", Slug: "b", Harness: "claude", Scope: "global"})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestGetTemplateBySlug(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	tmpl := &store.Template{
		ID: uuid.New().String(), Name: "custom", Slug: "custom", Harness: "gemini",
		Scope: store.TemplateScopeProject, ScopeID: projectID,
	}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))

	got, err := ts.GetTemplateBySlug(ctx, "custom", store.TemplateScopeProject, projectID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, got.ID)

	_, err = ts.GetTemplateBySlug(ctx, "custom", store.TemplateScopeProject, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestGetTemplateBySlugLegacyProjectID verifies the backwards-compat path where
// a project-scoped template was stored with project_id rather than scope_id.
func TestGetTemplateBySlugLegacyProjectID(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	tmpl := &store.Template{
		ID: uuid.New().String(), Name: "legacy", Slug: "legacy", Harness: "claude",
		Scope: store.TemplateScopeProject, ProjectID: projectID, // scope_id intentionally empty
	}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))

	got, err := ts.GetTemplateBySlug(ctx, "legacy", store.TemplateScopeProject, projectID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, got.ID)
}

func TestUpdateTemplate(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	tmpl := &store.Template{ID: uuid.New().String(), Name: "old", Slug: "old", Harness: "claude", Scope: "global", Status: store.TemplateStatusActive}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))

	tmpl.Name = "new"
	tmpl.Status = store.TemplateStatusArchived
	tmpl.Config = &store.TemplateConfig{Model: "opus"}
	require.NoError(t, ts.UpdateTemplate(ctx, tmpl))

	got, err := ts.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
	assert.Equal(t, store.TemplateStatusArchived, got.Status)
	require.NotNil(t, got.Config)
	assert.Equal(t, "opus", got.Config.Model)
}

func TestUpdateTemplateNotFound(t *testing.T) {
	ts := newTestTemplateStore(t)
	tmpl := &store.Template{ID: uuid.New().String(), Name: "ghost", Slug: "ghost", Harness: "claude", Scope: "global", Status: "active"}
	err := ts.UpdateTemplate(context.Background(), tmpl)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteTemplate(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	tmpl := &store.Template{ID: uuid.New().String(), Name: "del", Slug: "del", Harness: "claude", Scope: "global"}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))
	require.NoError(t, ts.DeleteTemplate(ctx, tmpl.ID))
	_, err := ts.GetTemplate(ctx, tmpl.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, ts.DeleteTemplate(ctx, tmpl.ID), store.ErrNotFound)
}

func TestDeleteTemplatesByScope(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	for i, n := range []string{"a", "b", "c"} {
		require.NoError(t, ts.CreateTemplate(ctx, &store.Template{
			ID: uuid.New().String(), Name: n, Slug: n, Harness: "claude",
			Scope: store.TemplateScopeProject, ScopeID: scopeID, Status: "active",
		}))
		_ = i
	}
	// Different scope survives.
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{
		ID: uuid.New().String(), Name: "other", Slug: "other", Harness: "claude",
		Scope: store.TemplateScopeProject, ScopeID: uuid.New().String(), Status: "active",
	}))

	n, err := ts.DeleteTemplatesByScope(ctx, store.TemplateScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestListTemplatesPagination(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, ts.CreateTemplate(ctx, &store.Template{
			ID: uuid.New().String(), Name: uuid.NewString(), Slug: uuid.NewString(), Harness: "claude", Scope: "global", Status: "active",
		}))
	}

	all, err := ts.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, all.Items, 5)
	assert.Equal(t, 5, all.TotalCount)

	page, err := ts.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Equal(t, 5, page.TotalCount, "TotalCount independent of limit")
}

func TestListTemplatesFilterByHarness(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "c", Slug: "c", Harness: "claude", Scope: "global", Status: "active"}))
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "g", Slug: "g", Harness: "gemini", Scope: "global", Status: "active"}))

	res, err := ts.ListTemplates(ctx, store.TemplateFilter{Harness: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "gemini", res.Items[0].Harness)
}

// TestListTemplatesProjectScopeIncludesGlobal verifies the projectId-without-scope
// filter returns global plus the project's own templates, but not other projects'.
func TestListTemplatesProjectScopeIncludesGlobal(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "global1", Slug: "global1", Harness: "claude", Scope: store.TemplateScopeGlobal, Status: "active"}))
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "proj1", Slug: "proj1", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: projectID, Status: "active"}))
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "otherproj", Slug: "otherproj", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: uuid.New().String(), Status: "active"}))

	res, err := ts.ListTemplates(ctx, store.TemplateFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalCount, "should see global + own project, not other project")
}

func TestListTemplatesSearch(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "Production Web", Slug: "prod-web", Harness: "claude", Scope: "global", Status: "active"}))
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "Staging", Slug: "staging", Harness: "claude", Scope: "global", Status: "active", Description: "production-like"}))
	require.NoError(t, ts.CreateTemplate(ctx, &store.Template{ID: uuid.New().String(), Name: "Dev", Slug: "dev", Harness: "claude", Scope: "global", Status: "active"}))

	res, err := ts.ListTemplates(ctx, store.TemplateFilter{Search: "produc"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalCount, "case-insensitive search matches name and description")
}

// =============================================================================
// HarnessConfig tests
// =============================================================================

func TestCreateAndGetHarnessConfig(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:          uuid.New().String(),
		Name:        "claude-web",
		Slug:        "claude-web",
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		ContentHash: "h1",
		Config:      &store.HarnessConfigData{Harness: "claude", Model: "opus", Env: map[string]string{"A": "B"}},
	}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))
	assert.Equal(t, store.HarnessConfigStatusActive, hc.Status)
	assert.False(t, hc.Created.IsZero())

	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "claude-web", got.Name)
	require.NotNil(t, got.Config)
	assert.Equal(t, "opus", got.Config.Model)
	assert.Equal(t, "B", got.Config.Env["A"])
}

func TestGetHarnessConfigBySlug(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	hc := &store.HarnessConfig{ID: uuid.New().String(), Name: "x", Slug: "x", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: scopeID}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))

	got, err := ts.GetHarnessConfigBySlug(ctx, "x", store.HarnessConfigScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, hc.ID, got.ID)

	_, err = ts.GetHarnessConfigBySlug(ctx, "x", store.HarnessConfigScopeProject, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateHarnessConfig(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{ID: uuid.New().String(), Name: "old", Slug: "old", Harness: "claude", Scope: "global", Status: "active"}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))

	hc.Name = "new"
	hc.Config = &store.HarnessConfigData{Model: "haiku"}
	require.NoError(t, ts.UpdateHarnessConfig(ctx, hc))

	got, err := ts.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
	require.NotNil(t, got.Config)
	assert.Equal(t, "haiku", got.Config.Model)
}

func TestUpdateHarnessConfigNotFound(t *testing.T) {
	ts := newTestTemplateStore(t)
	hc := &store.HarnessConfig{ID: uuid.New().String(), Name: "ghost", Slug: "ghost", Harness: "claude", Scope: "global", Status: "active"}
	err := ts.UpdateHarnessConfig(context.Background(), hc)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteHarnessConfig(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{ID: uuid.New().String(), Name: "del", Slug: "del", Harness: "claude", Scope: "global"}
	require.NoError(t, ts.CreateHarnessConfig(ctx, hc))
	require.NoError(t, ts.DeleteHarnessConfig(ctx, hc.ID))
	_, err := ts.GetHarnessConfig(ctx, hc.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteHarnessConfigsByScope(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	for _, n := range []string{"a", "b"} {
		require.NoError(t, ts.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: uuid.New().String(), Name: n, Slug: n, Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: scopeID, Status: "active"}))
	}
	n, err := ts.DeleteHarnessConfigsByScope(ctx, store.HarnessConfigScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestListHarnessConfigsPaginationAndFilter(t *testing.T) {
	ts := newTestTemplateStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		require.NoError(t, ts.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: uuid.New().String(), Name: uuid.NewString(), Slug: uuid.NewString(), Harness: "claude", Scope: "global", Status: "active"}))
	}
	require.NoError(t, ts.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: uuid.New().String(), Name: "g", Slug: "g", Harness: "gemini", Scope: "global", Status: "active"}))

	all, err := ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, all.TotalCount)

	page, err := ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Equal(t, 5, page.TotalCount)

	gemini, err := ts.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Harness: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, gemini.Items, 1)
	assert.Equal(t, "gemini", gemini.Items[0].Harness)
}
