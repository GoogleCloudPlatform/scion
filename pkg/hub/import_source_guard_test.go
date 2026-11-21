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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedRefusedImportSources are import sources a hosted hub refuses before
// fetching when server.hub.allowed_import_sources is empty.
var hostedRefusedImportSources = []string{
	":local:/x",
	":local:/etc",
	":googlecloudstorage:b",
	":googlecloudstorage:bucket/templates",
	":gcs:bucket/templates",
	":gcs,endpoint=https://evil.example:bucket/templates",
	"gs://bucket/templates",
	":s3:bucket",
	"myremote:templates",
	"http://example.com/templates.tar.gz",
	"http://github.com/acme/repo/tree/main/templates",
	"http://169.254.169.254/latest/meta-data/",
	"https://169.254.169.254/latest/meta-data/x.tar.gz",
	"https://127.0.0.1/x.tar.gz",
	"https://10.0.0.5/x.tar.gz",
	"https://[::1]/x.tar.gz",
	"https://[fe80::1]/x.tar.gz",
	"https://example.com/templates.tar.gz",
	"https://github.com/acme/repo/tree/main/templates?token=secret",
	"https://github.com/acme/repo/tree/main/templates#frag",
	"ftp://example.com/x.tar.gz",
	"https:///x.tar.gz",
}

func TestCheckImportSource_HostedDefaultAndAllowlist(t *testing.T) {
	hosted := &Server{}
	for _, src := range hostedRefusedImportSources {
		_, err := hosted.checkImportSource(src)
		require.ErrorIsf(t, err, config.ErrImportSourceNotAllowed, "%q must be refused", src)
		assert.NotContainsf(t, err.Error(), src, "the refusal must not echo the source URL")
	}
	for _, src := range []string{
		"https://github.com/acme/repo/tree/main/templates",
		"https://GitHub.com/acme/repo/tree/main/templates",
		"https://github.com/acme/repo/archive/main.tar.gz",
	} {
		_, err := hosted.checkImportSource(src)
		assert.NoErrorf(t, err, "%q must be accepted", src)
	}

	allow, err := config.NormalizeAllowedImportSources([]string{
		"https://artifacts.example.com/scion",
		"gs://team-bucket/templates",
	})
	require.NoError(t, err)
	hosted.config.AllowedImportSources = allow

	got, err := hosted.checkImportSource("https://artifacts.example.com/scion/t.tar.gz")
	require.NoError(t, err)
	assert.Equal(t, "https://artifacts.example.com/scion/t.tar.gz", got)

	got, err = hosted.checkImportSource("gs://team-bucket/templates/dev")
	require.NoError(t, err)
	assert.Equal(t, ":gcs:team-bucket/templates/dev", got)

	_, err = hosted.checkImportSource("https://artifacts.example.com/other/t.tar.gz")
	require.ErrorIs(t, err, config.ErrImportSourceNotAllowed)
}

func TestCheckImportSource_WorkstationAcceptsEverySource(t *testing.T) {
	srv := &Server{workstation: true}
	for _, src := range hostedRefusedImportSources {
		got, err := srv.checkImportSource(src)
		assert.NoErrorf(t, err, "workstation mode must not refuse %q", src)
		assert.Equal(t, src, got)
	}

	hosted := &Server{}
	_, err := hosted.checkImportSource(":local:/x")
	assert.ErrorIs(t, err, config.ErrImportSourceNotAllowed)
}

// requireInvalidRequest asserts a 400 invalid_request answer that names
// server.hub.allowed_import_sources and never echoes src.
func requireInvalidRequest(t *testing.T, rec *httptest.ResponseRecorder, src string) {
	t.Helper()
	require.Equalf(t, http.StatusBadRequest, rec.Code, "%q: %s", src, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equalf(t, ErrCodeInvalidRequest, resp.Error.Code, "%q", src)
	assert.Containsf(t, resp.Error.Message, config.AllowedImportSourcesKey, "%q", src)
	assert.NotContainsf(t, resp.Error.Message, src, "refusal must not echo %q", src)
}

// Every hub route that fetches a template or harness-config source refuses
// disallowed sources on a hosted hub, with 400 invalid_request (including
// before starting NDJSON streams), and persists nothing.
func TestHostedImport_RefusesHostSources(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	require.False(t, srv.workstation, "test servers are hosted")
	ctx := context.Background()

	project := &store.Project{ID: tid("import-guard-project"), Name: "Import Guard", Slug: "import-guard"}
	require.NoError(t, s.CreateProject(ctx, project))

	templatesBefore, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	hc := installClaudeViaHub(t, srv, s, ":local:/etc")
	require.Equal(t, ":local:/etc", hc.SourceURL)

	for _, src := range []string{
		":local:/x",
		":googlecloudstorage:b",
		"gs://b/templates",
		"http://example.com/templates.tar.gz",
		"https://169.254.169.254/latest/meta-data/x.tar.gz",
		"https://unlisted.example.com/templates.tar.gz",
	} {
		for _, kind := range []string{"template", "harness-config"} {
			for _, scope := range []string{"global", "project"} {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
					Kind: kind, Scope: scope, ScopeID: project.ID, SourceURL: src,
				})
				requireInvalidRequest(t, rec, src)

				rec = doRequest(t, srv, http.MethodPost, "/api/v1/resources/discover", DiscoverResourcesUnifiedRequest{
					Kind: kind, Scope: scope, ScopeID: project.ID, SourceURL: src,
				})
				requireInvalidRequest(t, rec, src)
			}
		}

		// Streaming callers get the same plain answer: the check runs before
		// the stream starts.
		rec := doStreamRequestWithToken(t, srv, testDevToken, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "template", Scope: "global", SourceURL: src,
		})
		requireInvalidRequest(t, rec, src)

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/import-templates",
			ImportTemplatesRequest{SourceURL: src})
		requireInvalidRequest(t, rec, src)
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/import-harness-configs",
			ImportHarnessConfigsRequest{SourceURL: src})
		requireInvalidRequest(t, rec, src)
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/discover-templates",
			DiscoverResourcesRequest{SourceURL: src})
		requireInvalidRequest(t, rec, src)
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/discover-harness-configs",
			DiscoverResourcesRequest{SourceURL: src})
		requireInvalidRequest(t, rec, src)

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
			map[string]interface{}{"sourceUrl": src})
		requireInvalidRequest(t, rec, src)
	}

	// A refused source recorded earlier is refused at reimport too.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport", map[string]interface{}{})
	requireInvalidRequest(t, rec, hc.SourceURL)

	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ContentHash, after.ContentHash, "a refused reimport must not change the files")
	templates, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, templatesBefore.TotalCount, templates.TotalCount, "a refused import must not create templates")
}

// fetchRemoteForImport refuses on its own, so a fetch path that skips the
// handler check still cannot reach a refused source.
func TestFetchRemoteForImport_HostedRefusesHostSources(t *testing.T) {
	srv, _ := testServer(t)
	for _, src := range []string{":local:/x", ":googlecloudstorage:b", "gs://b/x", "http://example.com/x.tar.gz", "https://169.254.169.254/x.tar.gz", "https://example.com/x.tar.gz"} {
		_, err := srv.fetchRemoteForImport(context.Background(), store.TemplateScopeGlobal, "", src)
		assert.ErrorIsf(t, err, config.ErrImportSourceNotAllowed, "%q: got %v", src, err)
	}
}

// A workstation hub keeps fetching plain http sources, including loopback
// ones.
func TestFetchRemoteForImport_WorkstationFetchesHTTPArchive(t *testing.T) {
	src := tarGzFilesServer(t, map[string]string{"config.yaml": "name: ws\nharness: claude\n"})
	require.True(t, strings.HasPrefix(src.URL, "http://127.0.0.1:") || strings.HasPrefix(src.URL, "http://[::1]:"), "src.URL=%q", src.URL)
	srv, _ := testServer(t)
	srv.workstation = true
	cachePath, err := srv.fetchRemoteForImport(context.Background(), store.TemplateScopeGlobal, "", src.URL+"/x.tar.gz")
	require.NoError(t, err)
	assert.FileExists(t, cachePath+"/config.yaml")
}

// TestImportAndDiscover_NoResourcesDoesNotEchoURL verifies that when a valid
// remote archive contains no templates/harness-configs, the error message does
// not echo the source URL.
func TestImportAndDiscover_NoResourcesDoesNotEchoURL(t *testing.T) {
	src := tarGzFilesServer(t, map[string]string{"readme.txt": "not a template"})
	routeImportTestServer(t, src)
	srv, s := testInstallSourceServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("no-echo-project"), Name: "No Echo", Slug: "no-echo"}
	require.NoError(t, s.CreateProject(ctx, project))

	sourceURL := src.URL + "/empty-sentinel-path/archive.tar.gz"
	_, err := srv.importFromRemote(ctx, project.ID, sourceURL, store.TemplateScopeProject, srv.templateImportKind(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no scion templates found at the source")
	assert.NotContains(t, err.Error(), "empty-sentinel-path")

	_, _, err = srv.discoverFromRemote(ctx, store.TemplateScopeProject, project.ID, sourceURL, srv.templateImportKind())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no scion templates found at the source")
	assert.NotContains(t, err.Error(), "empty-sentinel-path")
}
