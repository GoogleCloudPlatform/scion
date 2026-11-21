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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedRefusedImportSources are import sources a hosted hub refuses before
// fetching.
var hostedRefusedImportSources = []string{
	":local:/x",
	":local:/etc",
	":googlecloudstorage:b",
	":googlecloudstorage:bucket/templates",
	":gcs:bucket/templates",
	":s3:bucket",
	"http://example.com/templates.tar.gz",
	"http://169.254.169.254/latest/meta-data/",
	"https://169.254.169.254/latest/meta-data/x.tar.gz",
	"https://127.0.0.1/x.tar.gz",
	"https://10.0.0.5/x.tar.gz",
	"https://[::1]/x.tar.gz",
	"https://[fe80::1]/x.tar.gz",
	"ftp://example.com/x.tar.gz",
	"https:///x.tar.gz",
}

func TestCheckHostedImportSource(t *testing.T) {
	for _, src := range hostedRefusedImportSources {
		err := checkHostedImportSource(src)
		var refused *importSourceRefusedError
		require.Truef(t, errors.As(err, &refused), "%q must be refused, got %v", src, err)
		assert.NotContainsf(t, err.Error(), src, "the refusal must not echo the source URL")
	}
	for _, src := range []string{
		"https://github.com/acme/repo/tree/main/templates",
		"https://GitHub.com/acme/repo/tree/main/templates",
		"https://example.com/templates.tar.gz",
		"HTTPS://example.com/templates.zip",
	} {
		assert.NoErrorf(t, checkHostedImportSource(src), "%q must be accepted", src)
	}
}

func TestCheckImportSource_WorkstationAcceptsEverySource(t *testing.T) {
	srv := &Server{workstation: true}
	for _, src := range hostedRefusedImportSources {
		assert.NoErrorf(t, srv.checkImportSource(src), "workstation mode must not refuse %q", src)
	}
	assert.Nil(t, srv.importArchiveClient(), "workstation archive downloads keep http.DefaultClient")

	hosted := &Server{}
	assert.Error(t, hosted.checkImportSource(":local:/x"))
	assert.NotNil(t, hosted.importArchiveClient(), "hosted archive downloads use the SSRF-safe client")
}

// requireInvalidRequest asserts a 400 invalid_request answer.
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
	assert.Containsf(t, resp.Error.Message, "hosted server", "%q", src)
}

// Every hub route that fetches a template or harness-config source refuses
// rclone, plain http and internal-address sources on a hosted hub, with
// invalid_request, and persists nothing.
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
		"http://example.com/templates.tar.gz",
		"https://169.254.169.254/latest/meta-data/x.tar.gz",
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
	for _, src := range []string{":local:/x", ":googlecloudstorage:b", "http://example.com/x.tar.gz", "https://169.254.169.254/x.tar.gz"} {
		_, err := srv.fetchRemoteForImport(context.Background(), store.TemplateScopeGlobal, "", src)
		var refused *importSourceRefusedError
		assert.Truef(t, errors.As(err, &refused), "%q: got %v", src, err)
	}
}

// A host name is checked when dialed: on a hosted hub an https archive whose
// host resolves to loopback is not downloaded.
func TestFetchRemoteForImport_HostedArchiveBlocksLoopbackHost(t *testing.T) {
	var hits int
	archive := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(archive.Close)
	port := archive.URL[strings.LastIndex(archive.URL, ":")+1:]

	srv, _ := testServer(t)
	_, err := srv.fetchRemoteForImport(context.Background(), store.TemplateScopeGlobal, "",
		"https://localhost:"+port+"/x.tar.gz")
	var refused *importSourceRefusedError
	require.Truef(t, errors.As(err, &refused), "got %v", err)
	assert.Equal(t, importFetchDisallowedMessage, err.Error())
	assert.NotContains(t, err.Error(), "localhost", "the source host must not be echoed")
	assert.Zero(t, hits, "the hub must not reach a loopback host")
}

func TestMapImportFetchError(t *testing.T) {
	for _, sentinel := range []error{ssrfclient.ErrBlockedAddress, ssrfclient.ErrRedirectBlocked} {
		wrapped := &url.Error{Op: "Get", URL: "https://10.0.0.5/x.tar.gz",
			Err: fmt.Errorf("failed to download archive: %w", sentinel)}
		err := mapImportFetchError(fmt.Errorf("failed to fetch archive: %w", wrapped))
		var refused *importSourceRefusedError
		require.Truef(t, errors.As(err, &refused), "%v: got %v", sentinel, err)
		assert.Equal(t, importFetchDisallowedMessage, err.Error())
	}
	other := errors.New("failed to download archive: HTTP 404")
	assert.Same(t, other, mapImportFetchError(other), "other errors pass through")
}

// A workstation hub keeps fetching plain http sources, including loopback
// ones.
func TestFetchRemoteForImport_WorkstationFetchesHTTPArchive(t *testing.T) {
	src := tarGzFilesServer(t, map[string]string{"config.yaml": "name: ws\nharness: claude\n"})
	srv, _ := testServer(t)
	srv.workstation = true
	cachePath, err := srv.fetchRemoteForImport(context.Background(), store.TemplateScopeGlobal, "", src.URL+"/x.tar.gz")
	require.NoError(t, err)
	assert.FileExists(t, cachePath+"/config.yaml")
}
