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
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#4223 (TS-3): the template API no longer writes the
// template config or image, a PUT replaces only metadata, and nothing writes
// Config.Telemetry from an uploaded scion-agent.yaml.

// templateWriteVariant is one of the four template write routes.
type templateWriteVariant struct {
	name string
	do   func(t *testing.T, body json.RawMessage) *httptest.ResponseRecorder
}

// templateWriteVariants returns the global and user create and PUT routes on
// one server. The PUT routes target a global template and one of alice's
// user templates.
func templateWriteVariants(t *testing.T) []templateWriteVariant {
	t.Helper()
	srv, s, alice, _ := setupUserTemplateTest(t)
	global := createAuthzTestTemplate(t, s, "retired-global", store.TemplateScopeGlobal, "", "")
	user := createUserTemplate(t, s, alice.ID, "retired-user")
	return []templateWriteVariant{
		{"global create", func(t *testing.T, body json.RawMessage) *httptest.ResponseRecorder {
			return doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)
		}},
		{"user create", func(t *testing.T, body json.RawMessage) *httptest.ResponseRecorder {
			return doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", body)
		}},
		{"global PUT", func(t *testing.T, body json.RawMessage) *httptest.ResponseRecorder {
			return doRequest(t, srv, http.MethodPut, "/api/v1/templates/"+global.ID, body)
		}},
		{"user PUT", func(t *testing.T, body json.RawMessage) *httptest.ResponseRecorder {
			return doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/users/me/templates/"+user.ID, body)
		}},
	}
}

// Acceptance 1: create and PUT with config return 400
// template_config_in_file; with image, 400 template_image_not_supported.
// A null config and an empty image are not values and pass.
func TestTemplateAPI_ConfigAndImageRefused(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"config object", `{"name":"refused-a","config":{"model":"opus"}}`, ErrCodeTemplateConfigInFile},
		{"empty config object", `{"name":"refused-b","config":{}}`, ErrCodeTemplateConfigInFile},
		{"config with env", `{"name":"refused-c","config":{"env":{"K":"v"}}}`, ErrCodeTemplateConfigInFile},
		{"image", `{"name":"refused-d","image":"evil:latest"}`, ErrCodeTemplateImageNotSupported},
		{"config and image", `{"name":"refused-e","config":{"model":"opus"},"image":"evil:latest"}`, ErrCodeTemplateConfigInFile},
	}
	for _, v := range templateWriteVariants(t) {
		for _, tc := range cases {
			t.Run(v.name+"/"+tc.name, func(t *testing.T) {
				rec := v.do(t, json.RawMessage(tc.body))
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				assert.Equal(t, tc.wantCode, errorCodeOf(t, rec))
			})
		}
		t.Run(v.name+"/null config and empty image pass", func(t *testing.T) {
			rec := v.do(t, json.RawMessage(`{"name":"passes-`+api.Slugify(v.name)+`","config":null,"image":""}`))
			assert.Less(t, rec.Code, 300, "body: %s", rec.Body.String())
		})
	}
}

// pinnedTemplateField is one field a template PUT must leave unchanged.
type pinnedTemplateField struct {
	name    string
	hostile string // the JSON member a PUT body sends to try to change it
	get     func(*store.Template) any
}

var pinnedTemplateFields = []pinnedTemplateField{
	{"ID", `"id":"00000000-0000-0000-0000-00000000beef"`, func(t *store.Template) any { return t.ID }},
	{"Harness", `"harness":"evil-harness"`, func(t *store.Template) any { return t.Harness }},
	{"DefaultHarnessConfig", `"defaultHarnessConfig":"evil-hc"`, func(t *store.Template) any { return t.DefaultHarnessConfig }},
	{"AgentConfig", `"agentConfig":{"model":"evil-model"}`, func(t *store.Template) any { return t.AgentConfig }},
	{"Files", `"files":[{"path":"evil.md","size":1,"hash":"sha256:evil"}]`, func(t *store.Template) any { return t.Files }},
	{"Files cleared", `"files":[]`, func(t *store.Template) any { return t.Files }},
	{"ContentHash", `"contentHash":"sha256:evil"`, func(t *store.Template) any { return t.ContentHash }},
	{"Layout", `"layout":"evil-layout"`, func(t *store.Template) any { return t.Layout }},
	{"Status", `"status":"archived"`, func(t *store.Template) any { return t.Status }},
	{"StoragePath", `"storagePath":"evil/path"`, func(t *store.Template) any { return t.StoragePath }},
	{"StorageBucket", `"storageBucket":"evil-bucket"`, func(t *store.Template) any { return t.StorageBucket }},
	{"StorageURI", `"storageUri":"gs://evil-bucket/evil/path"`, func(t *store.Template) any { return t.StorageURI }},
	{"Scope", `"scope":"project"`, func(t *store.Template) any { return t.Scope }},
	{"ScopeID", `"scopeId":"evil-scope"`, func(t *store.Template) any { return t.ScopeID }},
	{"ProjectID", `"projectId":"evil-project"`, func(t *store.Template) any { return t.ProjectID }},
	{"OwnerID", `"ownerId":"evil-owner"`, func(t *store.Template) any { return t.OwnerID }},
	{"BaseTemplate", `"baseTemplate":"evil-base"`, func(t *store.Template) any { return t.BaseTemplate }},
	{"SourceURL", `"sourceUrl":"https://evil.example.com/src"`, func(t *store.Template) any { return t.SourceURL }},
	{"CreatedBy", `"createdBy":"evil-creator"`, func(t *store.Template) any { return t.CreatedBy }},
	{"Created", `"created":"2001-01-01T00:00:00Z"`, func(t *store.Template) any { return t.Created.UTC().Truncate(time.Second) }},
	{"Config kept by a null config", `"config":null`, func(t *store.Template) any { return t.Config }},
	{"Image kept by an empty image", `"image":""`, func(t *store.Template) any { return t.Image }},
}

// seedPinnedTemplate stores a template with a distinctive value in every
// pinned field.
func seedPinnedTemplate(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.Template {
	t.Helper()
	slug := api.Slugify(name)
	tmpl := &store.Template{
		ID:                   api.NewUUID(),
		Name:                 name,
		Slug:                 slug,
		DisplayName:          "Original Display",
		Description:          "original description",
		Harness:              "claude",
		DefaultHarnessConfig: "claude-web",
		Image:                "legacy-image:v1",
		Config:               &store.TemplateConfig{Model: "legacy-model", Env: map[string]string{"LEGACY": "1"}},
		AgentConfig:          &api.ScionConfig{Model: "snapshot-model"},
		ContentHash:          "sha256:original",
		Scope:                scope,
		ScopeID:              scopeID,
		ProjectID:            "original-project",
		StorageURI:           "gs://original-bucket/templates/" + slug + "/",
		StorageBucket:        "original-bucket",
		StoragePath:          "templates/" + slug,
		Layout:               store.TemplateLayoutBlobs,
		Files:                []store.TemplateFile{{Path: "scion-agent.yaml", Size: 7, Hash: "sha256:file"}},
		BaseTemplate:         "original-base",
		Status:               store.TemplateStatusActive,
		OwnerID:              ownerID,
		CreatedBy:            "original-creator",
		SourceURL:            "https://example.com/original-source",
		Created:              time.Now().Add(-time.Hour),
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	stored, err := s.GetTemplate(context.Background(), tmpl.ID)
	require.NoError(t, err)
	return stored
}

// Acceptance 2: a metadata PUT applies name, slug, display name and
// description and leaves every pinned field unchanged, in the stored row and
// in the response, even when the body sends a different value. Global and
// user variants. Mutant (a), the user PUT no longer pinning BaseTemplate,
// fails the user/BaseTemplate subtest.
func TestTemplatePut_MetadataOnlyPinsEveryField(t *testing.T) {
	srv, s, alice, _ := setupUserTemplateTest(t)

	variants := []struct {
		name   string
		seed   func(t *testing.T, name string) *store.Template
		put    func(t *testing.T, id string, body json.RawMessage) *httptest.ResponseRecorder
		caller string
	}{
		{
			name: "global",
			seed: func(t *testing.T, name string) *store.Template {
				return seedPinnedTemplate(t, s, name, store.TemplateScopeGlobal, "", "original-owner")
			},
			put: func(t *testing.T, id string, body json.RawMessage) *httptest.ResponseRecorder {
				return doRequest(t, srv, http.MethodPut, "/api/v1/templates/"+id, body)
			},
			caller: DevUserID,
		},
		{
			name: "user",
			seed: func(t *testing.T, name string) *store.Template {
				return seedPinnedTemplate(t, s, name, store.TemplateScopeUser, alice.ID, alice.ID)
			},
			put: func(t *testing.T, id string, body json.RawMessage) *httptest.ResponseRecorder {
				return doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/users/me/templates/"+id, body)
			},
			caller: alice.ID,
		},
	}

	for _, v := range variants {
		for i, f := range pinnedTemplateFields {
			t.Run(v.name+"/"+f.name, func(t *testing.T) {
				pre := v.seed(t, v.name+"-pinned-"+string(rune('a'+i)))
				body := `{"name":"renamed-` + pre.Slug + `","slug":"Renamed Slug ` + pre.Slug + `","displayName":"New Display","description":"new description",` + f.hostile + `}`
				rec := v.put(t, pre.ID, json.RawMessage(body))
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

				var resp store.Template
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				stored, err := s.GetTemplate(context.Background(), pre.ID)
				require.NoError(t, err)

				for _, got := range []struct {
					where string
					tmpl  *store.Template
				}{{"stored row", stored}, {"response", &resp}} {
					if !reflect.DeepEqual(f.get(got.tmpl), f.get(pre)) {
						t.Errorf("%s: %s = %#v, want the stored %#v", got.where, f.name, f.get(got.tmpl), f.get(pre))
					}
					assert.Equal(t, "renamed-"+pre.Slug, got.tmpl.Name, "%s: Name is metadata", got.where)
					assert.Equal(t, "New Display", got.tmpl.DisplayName, "%s: DisplayName is metadata", got.where)
					assert.Equal(t, "new description", got.tmpl.Description, "%s: Description is metadata", got.where)
					assert.NotEqual(t, pre.Slug, got.tmpl.Slug, "%s: Slug is metadata", got.where)
					assert.Equal(t, v.caller, got.tmpl.UpdatedBy, "%s: UpdatedBy is the caller", got.where)
				}
			})
		}
	}
}

// Acceptance 3: no upload path writes Config.Telemetry from scion-agent.yaml,
// and the same push leaves identical rows on local storage and on a GCS-style
// backend.
func TestTemplateUpload_NoConfigTelemetryAndBackendParity(t *testing.T) {
	const withTelemetry = "default_harness_config: claude-web\nmodel: opus\ntelemetry:\n  enabled: false\n  cloud:\n    endpoint: otel.example.com:4317\n"
	const otherTelemetry = "default_harness_config: claude-web\ntelemetry:\n  enabled: true\n"

	push := func(t *testing.T, srv *Server, s store.Store) *store.Template {
		t.Helper()
		tmpl := seedCommittedTemplate(t, srv, "parity-tpl", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": "default_harness_config: claude-web\n", "a.md": "a"})
		check := func(step string) {
			t.Helper()
			if got := getTemplate(t, s, tmpl.ID); got.Config != nil {
				t.Errorf("after %s: Config = %+v, want nil (no Config.Telemetry write)", step, got.Config)
			}
		}

		writeTemplateFileJSON(t, srv, tmpl.ID, "scion-agent.yaml", withTelemetry)
		check("json write")
		mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "application/octet-stream", []byte(otherTelemetry)), http.StatusOK)
		check("raw write")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, templateMultipartRequest(t, tmpl.ID, map[string][]byte{"scion-agent.yaml": []byte(withTelemetry)}))
		mustStatus(t, w, http.StatusOK)
		check("multipart upload")

		final := map[string]string{"scion-agent.yaml": otherTelemetry, "a.md": "a", "b.md": "b"}
		stageObjects(t, srv, getTemplate(t, s, tmpl.ID), final)
		mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(final)), http.StatusOK)
		check("finalize")
		return getTemplate(t, s, tmpl.ID)
	}

	local := func() *store.Template {
		srv, s := newCommitTestServer(t, newCommitTestStorage(t))
		return push(t, srv, s)
	}()
	gcs := func() *store.Template {
		mock := newMockStorage("gcs-bucket")
		mock.provider = storage.ProviderGCS
		srv, s := newCommitTestServer(t, mock)
		return push(t, srv, s)
	}()

	for _, f := range []struct {
		name string
		get  func(*store.Template) any
	}{
		{"Config", func(t *store.Template) any { return t.Config }},
		{"Image", func(t *store.Template) any { return t.Image }},
		{"AgentConfig", func(t *store.Template) any { return t.AgentConfig }},
		{"Harness", func(t *store.Template) any { return t.Harness }},
		{"DefaultHarnessConfig", func(t *store.Template) any { return t.DefaultHarnessConfig }},
		{"ContentHash", func(t *store.Template) any { return t.ContentHash }},
		{"Files", func(t *store.Template) any { return t.Files }},
		{"Layout", func(t *store.Template) any { return t.Layout }},
		{"Status", func(t *store.Template) any { return t.Status }},
	} {
		if !reflect.DeepEqual(f.get(local), f.get(gcs)) {
			t.Errorf("%s differs: local %#v, gcs %#v", f.name, f.get(local), f.get(gcs))
		}
	}
	require.NotNil(t, local.AgentConfig, "precondition: the push committed a snapshot")
	assert.Equal(t, "claude-web", local.AgentConfig.DefaultHarnessConfig)
}

// Acceptance 4: the validate report lists a row with a stored Config or
// Image under stored_config, naming the fields, and not a clean row.
func TestAdminValidateResources_ListsStoredTemplateConfig(t *testing.T) {
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	ctx := context.Background()

	both := &store.Template{
		ID: api.NewUUID(), Name: "stored-both", Slug: "stored-both", Scope: store.TemplateScopeGlobal,
		Status: store.TemplateStatusActive, Image: "legacy:1",
		Config: &store.TemplateConfig{Model: "opus", Env: map[string]string{"K": "v"}},
	}
	imageOnly := &store.Template{
		ID: api.NewUUID(), Name: "stored-image", Slug: "stored-image", Scope: store.TemplateScopeUser,
		ScopeID: DevUserID, OwnerID: DevUserID, Status: store.TemplateStatusActive, Image: "legacy:2",
	}
	clean := &store.Template{
		ID: api.NewUUID(), Name: "stored-clean", Slug: "stored-clean", Scope: store.TemplateScopeGlobal,
		Status: store.TemplateStatusActive, Config: &store.TemplateConfig{},
	}
	for _, tmpl := range []*store.Template{both, imageOnly, clean} {
		require.NoError(t, s.CreateTemplate(ctx, tmpl))
	}

	w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/admin/validate-resources", "", nil)
	mustStatus(t, w, http.StatusOK)
	var resp struct {
		TemplateConfigFindings []TemplateConfigFinding `json:"templateConfigFindings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())

	byName := map[string]TemplateConfigFinding{}
	for _, f := range resp.TemplateConfigFindings {
		if f.Kind == TemplateFindingStoredConfig {
			byName[f.Template] = f
		}
	}
	require.Len(t, byName, 2, "findings: %+v", resp.TemplateConfigFindings)
	b := byName["stored-both"]
	assert.Equal(t, both.ID, b.TemplateID)
	assert.Equal(t, store.TemplateScopeGlobal, b.Scope)
	assert.Equal(t, []string{"config.env", "config.model", "image"}, b.Keys)
	assert.Contains(t, b.Message, "stored-both")
	i := byName["stored-image"]
	assert.Equal(t, imageOnly.ID, i.TemplateID)
	assert.Equal(t, store.TemplateScopeUser, i.Scope)
	assert.Equal(t, DevUserID, i.ScopeID)
	assert.Equal(t, []string{"image"}, i.Keys)
}
