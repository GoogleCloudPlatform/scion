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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reimportTestSource = "https://github.com/acme/repo/tree/main/templates"

func TestResolveTemplateReimportSource(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		override string
		want     string
		wantCode string
	}{
		{name: "stored github folder", stored: reimportTestSource, want: reimportTestSource},
		{name: "override replaces stored", stored: reimportTestSource, override: "https://github.com/acme/other/tree/dev/t", want: "https://github.com/acme/other/tree/dev/t"},
		{name: "override without scheme is normalized", override: "github.com/acme/repo", want: "https://github.com/acme/repo/tree/main/.scion/templates"},
		{name: "git+https override", override: "git+https://github.com/acme/repo/tree/main/t", want: "https://github.com/acme/repo/tree/main/t"},
		{name: "no stored source", wantCode: "no_source_url"},
		{name: "whitespace stored source", stored: "   ", wantCode: "no_source_url"},
		{name: "builtin source", stored: "builtin://scion/1.0/template/default", wantCode: "unsupported_source"},
		{name: "http stored source", stored: "http://github.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "rclone stored source", stored: ":local:/etc", wantCode: "unsupported_source"},
		{name: "file stored source", stored: "file:///etc/passwd", wantCode: "unsupported_source"},
		{name: "host not allowed", stored: "https://example.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "github subdomain not allowed", stored: "https://other.github.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "lookalike host not allowed", stored: "https://github.com.example.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "loopback host", stored: "https://127.0.0.1/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "metadata host", stored: "https://169.254.169.254/acme/repo", wantCode: "unsupported_source"},
		{name: "non-default port", stored: "https://github.com:8443/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "archive url", stored: "https://github.com/acme/repo/archive/refs/heads/main.tar.gz", wantCode: "unsupported_source"},
		{name: "query string", stored: reimportTestSource + "?x=1", wantCode: "unsupported_source"},
		{name: "dot-dot path", stored: "https://github.com/acme/repo/tree/main/../../x", wantCode: "unsupported_source"},
		{name: "owner only", stored: "https://github.com/acme", wantCode: "unsupported_source"},
		{name: "http override", stored: reimportTestSource, override: "http://github.com/acme/repo", wantCode: "unsupported_source"},
		{name: "builtin override", stored: reimportTestSource, override: "builtin://scion/1.0/template/default", wantCode: "unsupported_source"},
		{name: "rclone override", stored: reimportTestSource, override: ":local:/etc", wantCode: "unsupported_source"},
		{name: "other host override", stored: reimportTestSource, override: "https://example.com/t.tgz", wantCode: "unsupported_source"},
		{name: "credentials in stored source", stored: "https://user:secret@github.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "credentials in override", override: "https://user:secret@github.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
		{name: "token only in override", override: "https://secret@github.com/acme/repo/tree/main/t", wantCode: "unsupported_source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, got, err := resolveTemplateReimportSource(tt.stored, tt.override)
			if tt.wantCode != "" {
				require.NotNil(t, err, "expected refusal, got %q", got)
				assert.Equal(t, tt.wantCode, err.code)
				assert.NotContains(t, err.message, "secret")
				assert.NotContains(t, err.message, "example.com")
				return
			}
			require.Nil(t, err)
			assert.NotEmpty(t, src.Owner)
			assert.Equal(t, tt.want, got)
		})
	}
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64 // tar header mode; 0 means 0644
}

// buildTarGz builds a gzip tarball from entries, in order.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		if tf == tar.TypeXGlobalHeader {
			comment := e.body
			if comment == "" {
				comment = "abc123"
			}
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Typeflag: tf, Name: e.name, PAXRecords: map[string]string{"comment": comment},
			}))
			continue
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Mode: mode, Typeflag: tf, Linkname: e.linkname}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if tf == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

// twoTemplateArchive is a GitHub-style archive with two templates under
// templates/: my-template and other-template.
func twoTemplateArchive(t *testing.T, readme string) []byte {
	return buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: defaultFakeReimportSHA},
		{name: "repo-main/", typeflag: tar.TypeDir},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: readme},
		{name: "repo-main/templates/other-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
	})
}

func newReimportAdmin(t *testing.T, s store.Store) *store.User {
	t.Helper()
	ctx := context.Background()
	admin := &store.User{ID: tid("user-reimport-admin"), Email: "reimport-admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)
	ensureAdminRoleBinding(t, s, admin.ID)
	return admin
}

func createReimportTemplate(t *testing.T, s store.Store, id, name, scope, scopeID, sourceURL string) *store.Template {
	t.Helper()
	return createReimportTemplateWithSlug(t, s, id, name, name, scope, scopeID, sourceURL)
}

func createReimportTemplateWithSlug(t *testing.T, s store.Store, id, name, slug, scope, scopeID, sourceURL string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:        tid(id),
		Name:      name,
		Slug:      slug,
		Harness:   "claude",
		Scope:     scope,
		ScopeID:   scopeID,
		ProjectID: scopeID,
		Status:    store.TemplateStatusActive,
		SourceURL: sourceURL,
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

func TestTemplateReimport_RefreshesOnlyTargetTemplate(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-ok", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	fetcher := &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ImportTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"my-template"}, resp.Templates)
	assert.Equal(t, 1, resp.Count)
	assert.Equal(t, []string{"https://github.com/acme/repo/archive/" + defaultFakeReimportSHA + ".tar.gz"}, fetcher.recordedTarballURLs())

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, got.ContentHash, "files should be refreshed")
	assert.Len(t, got.Files, 2)
	assert.Equal(t, store.TemplateStatusActive, got.Status)

	// The other template in the same source is not created by a reimport.
	_, err = s.GetTemplateBySlug(ctx, "other-template", store.TemplateScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestTemplateReimport_OverrideURLIsStored(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-override", "my-template", store.TemplateScopeGlobal, "", "https://github.com/acme/old/tree/main/templates/my-template")

	fetcher := &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

	override := "https://github.com/acme/repo/tree/main/templates"
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport",
		ReimportTemplateRequest{SourceURL: override})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"https://github.com/acme/repo/archive/" + defaultFakeReimportSHA + ".tar.gz"}, fetcher.recordedTarballURLs())

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, override+"/my-template", got.SourceURL)
}

func TestTemplateReimport_RefusesUnsupportedSources(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)

	// Refused sources must not be fetched at all.
	fetcher := &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

	tests := []struct {
		name     string
		stored   string
		override string
		wantCode string
	}{
		{name: "empty", wantCode: "no_source_url"},
		{name: "builtin", stored: "builtin://scion/1.0/template/default", wantCode: "unsupported_source"},
		{name: "http", stored: "http://github.com/acme/repo/tree/main/templates", wantCode: "unsupported_source"},
		{name: "rclone", stored: ":local:/etc", wantCode: "unsupported_source"},
		{name: "host not allowed", stored: "https://example.com/acme/repo/tree/main/templates", wantCode: "unsupported_source"},
		{name: "loopback", stored: "https://127.0.0.1/acme/repo/tree/main/templates", wantCode: "unsupported_source"},
		{name: "credentials in stored", stored: "https://user:secret@github.com/acme/repo/tree/main/templates", wantCode: "unsupported_source"},
		{name: "credentials in override", stored: reimportTestSource, override: "https://user:secret@github.com/acme/repo/tree/main/templates", wantCode: "unsupported_source"},
		{name: "host not allowed in override", stored: reimportTestSource, override: "https://example.com/t.tgz", wantCode: "unsupported_source"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := createReimportTemplate(t, s, "tmpl-refuse-"+tt.name, "refuse-"+string(rune('a'+i)), store.TemplateScopeGlobal, "", tt.stored)
			var body any
			if tt.override != "" {
				body = ReimportTemplateRequest{SourceURL: tt.override}
			}
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tt.wantCode)
			assert.NotContains(t, rec.Body.String(), "secret")

			got, err := s.GetTemplate(ctx, tmpl.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.stored, got.SourceURL, "stored source must be unchanged")
		})
	}
	assert.Empty(t, fetcher.recordedURLs(), "refused sources must not be fetched")
}

func TestTemplateReimport_DownloadFailuresAreReported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-fail", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	for name, tc := range map[string]struct {
		err        error
		wantStatus int
		wantCode   string
		wantSub    string
	}{
		"oversize download":  {&remotefetch.Error{Reason: remotefetch.ReasonTooLarge, Detail: "body over 1 bytes"}, http.StatusBadRequest, "reimport_failed", "size or file count limit"},
		"too many redirects": {&remotefetch.Error{Reason: remotefetch.ReasonRedirects, Detail: "more than 3 redirects"}, http.StatusBadRequest, "reimport_failed", "could not download"},
		"internal address":   {&remotefetch.Error{Reason: remotefetch.ReasonDeniedAddress, Detail: "codeload.github.com resolves to denied address 10.0.0.5"}, http.StatusBadRequest, "reimport_failed", "could not download"},
		"host off the list":  {&remotefetch.Error{Reason: remotefetch.ReasonHostNotAllowed, Detail: `host "other.test" is not allowed`}, http.StatusBadRequest, "reimport_failed", "could not download"},
		"not found":          {&remotefetch.Error{Reason: remotefetch.ReasonStatus, Status: http.StatusNotFound, Detail: "status 404"}, http.StatusBadRequest, "reimport_failed", "not found"},
		"auth rejected":      {&remotefetch.Error{Reason: remotefetch.ReasonStatus, Status: http.StatusUnauthorized, Detail: "status 401"}, http.StatusBadRequest, "reimport_failed", "rejected"},
		"forbidden SSO":      {&remotefetch.Error{Reason: remotefetch.ReasonStatus, Status: http.StatusForbidden, Header: http.Header{"X-Github-Sso": {"required"}}, Detail: "status 403"}, http.StatusBadRequest, "reimport_failed", "SAML SSO"},
		"rate limited 429":   {&remotefetch.Error{Reason: remotefetch.ReasonStatus, Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"45"}}, Detail: "status 429"}, http.StatusTooManyRequests, "rate_limited", "rate limit"},
	} {
		t.Run(name, func(t *testing.T) {
			srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{err: tc.err})
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantCode)
			assert.Contains(t, rec.Body.String(), tc.wantSub)
			if tc.wantStatus == http.StatusTooManyRequests {
				assert.Equal(t, "45", rec.Header().Get("Retry-After"))
			}
			// Fetch details (hosts, addresses) stay in the server log.
			assert.NotContains(t, rec.Body.String(), "10.0.0.5")
			assert.NotContains(t, rec.Body.String(), "other.test")
		})
	}
}

func TestTemplateReimport_OversizeExtractRefused(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-bigextract", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	big := strings.Repeat("x", int(githubfetch.ImportHostedLimits().MaxFileBytes)+1)
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: buildTarGz(t, []tarEntry{
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "harness: claude\n"},
		{name: "repo-main/templates/my-template/big.bin", body: big},
	})})
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "size or file count limit")

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Files, "a refused source must not change the template")
}

// TestTemplateGitHubClient_DefaultIsGuarded checks the server's network-mode
// wiring (hosted -> NetworkVetted, workstation -> NetworkSystem) and that the
// hosted Doer refuses http, non-allowlisted hosts, userinfo, and denylisted
// loopback addresses before making any connection.
func TestTemplateGitHubClient_DefaultIsGuarded(t *testing.T) {
	hostedSrv := &Server{workstation: false}
	workstationSrv := &Server{workstation: true}
	assert.Equal(t, remotefetch.NetworkVetted, hostedSrv.templateReimportNetworkMode(), "hosted Hub must select NetworkVetted")
	assert.Equal(t, remotefetch.NetworkSystem, workstationSrv.templateReimportNetworkMode(), "workstation Hub must select NetworkSystem")

	doer := hostedSrv.newTemplateGitHubDoer(githubfetch.DefaultEndpoint(), githubfetch.ImportHostedLimits())
	_ = hostedSrv.getTemplateGitHubClient(githubfetch.ImportHostedLimits())

	for name, tc := range map[string]struct {
		url  string
		want remotefetch.Reason
	}{
		"http":             {"http://github.com/acme/repo/archive/refs/heads/main.tar.gz", remotefetch.ReasonScheme},
		"other host":       {"https://example.com/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"github subdomain": {"https://other.github.com/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"loopback":         {"https://127.0.0.1/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"localhost":        {"https://localhost/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"private":          {"https://10.0.0.5/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"metadata":         {"https://169.254.169.254/latest/meta-data/", remotefetch.ReasonHostNotAllowed},
		"ipv6 loopback":    {"https://[::1]/archive.tar.gz", remotefetch.ReasonHostNotAllowed},
		"userinfo":         {"https://u:p@github.com/acme/repo/archive/refs/heads/main.tar.gz", remotefetch.ReasonUserinfo},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := doer.Open(context.Background(), githubfetch.Request{URL: tc.url})
			var fe *remotefetch.Error
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tc.want, fe.Reason)
		})
	}

	t.Run("network mode wiring refuses loopback only on hosted", func(t *testing.T) {
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
			t.Setenv(k, "")
		}
		loopbackEp := githubfetch.Endpoint{
			APIBase:  "https://127.0.0.1",
			RawBase:  "https://127.0.0.1",
			WebHost:  "127.0.0.1",
			Codeload: "127.0.0.1",
		}
		lim := githubfetch.ImportHostedLimits()
		lim.RequestTimeout = 2 * time.Second

		hostedDoer := hostedSrv.newTemplateGitHubDoer(loopbackEp, lim)
		_, err := hostedDoer.Open(context.Background(), githubfetch.Request{URL: "https://127.0.0.1/repos/acme/repo"})
		var fe *remotefetch.Error
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, remotefetch.ReasonDeniedAddress, fe.Reason, "hosted Doer must run in NetworkVetted and refuse loopback with ReasonDeniedAddress")

		workstationDoer := workstationSrv.newTemplateGitHubDoer(loopbackEp, lim)
		resp, err := workstationDoer.Open(context.Background(), githubfetch.Request{URL: "https://127.0.0.1/repos/acme/repo"})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		var wfe *remotefetch.Error
		if errors.As(err, &wfe) {
			assert.NotEqual(t, remotefetch.ReasonDeniedAddress, wfe.Reason, "workstation Doer must run in NetworkSystem and not refuse loopback with ReasonDeniedAddress")
		}
	})
}

func TestTemplateReimport_NotFoundAndMethod(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := newReimportAdmin(t, s)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tid("missing")+"/reimport", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	tmpl := createReimportTemplate(t, s, "tmpl-reimport-method", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

func TestTemplateReimport_GlobalRequiresCreatePermission(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	member := &store.User{ID: tid("user-reimport-member"), Email: "reimport-member@test.com", DisplayName: "Member", Role: store.UserRoleMember}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	tmpl := createReimportTemplate(t, s, "tmpl-reimport-global-authz", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	fetcher := &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, fetcher.recordedURLs())
}

func TestTemplateReimport_ProjectUsesTemplateCreateAuthorization(t *testing.T) {
	srv, s, project, _ := setupWorkspaceProject(t, "template-reimport-authz")
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-template-reimport-authz"),
		Email:       "template-reimport-authz@test.com",
		DisplayName: "Template Reimport Authz",
		Role:        store.UserRoleMember,
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	tmpl := createReimportTemplate(t, s, "tmpl-reimport-project-authz", "my-template", store.TemplateScopeProject, project.ID, reimportTestSource)
	path := "/api/v1/templates/" + tmpl.ID + "/reimport"

	srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")})

	// No read access: the template reads as not found.
	rec := doRequestAsUser(t, srv, user, http.MethodPost, path, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// Read access alone does not allow a reimport.
	grantUserActionOnResource(t, s, user.ID, "template", project.ID, ActionRead)
	rec = doRequestAsUser(t, srv, user, http.MethodPost, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// template.create on the project allows it.
	grantUserActionOnResource(t, s, user.ID, "template", project.ID, ActionCreate)
	rec = doRequestAsUser(t, srv, user, http.MethodPost, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// countTemplates returns the number of templates in a scope.
func countTemplates(t *testing.T, s store.Store, scope, scopeID string) int {
	t.Helper()
	res, err := s.ListTemplates(context.Background(), store.TemplateFilter{Scope: scope, ScopeID: scopeID}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return len(res.Items)
}

// TestTemplateReimport_NameDiffersFromSlug: a template whose name and slug
// differ is refreshed in place, a bystander whose slug equals the target's
// name is left untouched, and no new template is created.
func TestTemplateReimport_NameDiffersFromSlug(t *testing.T) {
	for _, withBystander := range []bool{true, false} {
		name := "without bystander"
		if withBystander {
			name = "with bystander"
		}
		t.Run(name, func(t *testing.T) {
			srv, s, _ := testTemplateBootstrapServer(t)
			ctx := context.Background()
			admin := newReimportAdmin(t, s)

			target := createReimportTemplateWithSlug(t, s, "tmpl-slug-target", "my-template", "renamed-slug",
				store.TemplateScopeGlobal, "", reimportTestSource)
			var bystander *store.Template
			if withBystander {
				bystander = createReimportTemplate(t, s, "tmpl-slug-bystander", "my-template-other", store.TemplateScopeGlobal, "", "")
				bystander.Slug = "my-template"
				require.NoError(t, s.UpdateTemplate(ctx, bystander))
			}
			before := countTemplates(t, s, store.TemplateScopeGlobal, "")

			srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")})
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+target.ID+"/reimport", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetTemplate(ctx, target.ID)
			require.NoError(t, err)
			assert.Len(t, got.Files, 2, "target must be refreshed")
			assert.NotEmpty(t, got.ContentHash)
			assert.Equal(t, "renamed-slug", got.Slug)
			assert.Equal(t, "my-template", got.Name)
			assert.Equal(t, reimportTestSource+"/my-template", got.SourceURL)

			if withBystander {
				other, err := s.GetTemplate(ctx, bystander.ID)
				require.NoError(t, err)
				assert.Empty(t, other.Files, "bystander must be untouched")
				assert.Empty(t, other.ContentHash)
				assert.Empty(t, other.SourceURL)
			}
			assert.Equal(t, before, countTemplates(t, s, store.TemplateScopeGlobal, ""), "no template may be created")
		})
	}
}

// TestTemplateReimport_FolderMatchedBySlug: a folder named after the
// template's slug (not its name) refreshes the template.
func TestTemplateReimport_FolderMatchedBySlug(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	target := createReimportTemplateWithSlug(t, s, "tmpl-slug-folder", "My Template", "my-template",
		store.TemplateScopeGlobal, "", reimportTestSource)

	srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")})
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+target.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := s.GetTemplate(ctx, target.ID)
	require.NoError(t, err)
	assert.Len(t, got.Files, 2)
	assert.Equal(t, 1, countTemplates(t, s, store.TemplateScopeGlobal, ""))
}

// TestTemplateReimport_NoMatchingFolder: a source without a folder for the
// template is refused and nothing is written.
func TestTemplateReimport_NoMatchingFolder(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	target := createReimportTemplate(t, s, "tmpl-no-folder", "absent-template", store.TemplateScopeGlobal, "", reimportTestSource)

	srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")})
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+target.ID+"/reimport", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "does not contain this template")
	assert.Equal(t, 1, countTemplates(t, s, store.TemplateScopeGlobal, ""))
	got, err := s.GetTemplate(ctx, target.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Files)
}

func TestTargetTemplatePersistence_NeverCreates(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	tmpl := createReimportTemplate(t, s, "tmpl-pinned", "pinned", store.TemplateScopeGlobal, "", reimportTestSource)

	p := srv.targetTemplateStore(tmpl.ID).pers
	rec, err := p.GetBySlug(ctx, "some-other-slug", store.TemplateScopeGlobal, "")
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, rec.ID, "lookup is by ID, not slug")

	_, err = p.GetBySlug(ctx, "pinned", store.TemplateScopeProject, "p1")
	assert.Error(t, err, "a scope mismatch is refused")

	assert.Error(t, p.Create(ctx, &ResourceRecord{}, nil))

	missing := srv.targetTemplateStore(tid("missing")).pers
	_, err = missing.GetBySlug(ctx, "pinned", store.TemplateScopeGlobal, "")
	assert.Error(t, err, "a missing target is an error, not a create")
}

func TestTemplateReimport_UserScopeAuthorization(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	owner := &store.User{ID: tid("user-reimport-owner"), Email: "reimport-owner@test.com", DisplayName: "Owner", Role: store.UserRoleMember}
	other := &store.User{ID: tid("user-reimport-other"), Email: "reimport-other@test.com", DisplayName: "Other", Role: store.UserRoleMember}
	for _, u := range []*store.User{owner, other} {
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
	}
	admin := newReimportAdmin(t, s)

	tmpl := createReimportTemplate(t, s, "tmpl-reimport-user", "my-template", store.TemplateScopeUser, owner.ID, reimportTestSource)
	tmpl.OwnerID = owner.ID
	require.NoError(t, s.UpdateTemplate(ctx, tmpl))
	path := "/api/v1/templates/" + tmpl.ID + "/reimport"

	fetcher := &fakeReimportGitHub{body: twoTemplateArchive(t, "v2")}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

	// Another user cannot read the template: it reads as not found.
	rec := doRequestAsUser(t, srv, other, http.MethodPost, path, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// A caller who can read it but is not the owner is refused.
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, fetcher.recordedURLs(), "refused callers must not trigger a fetch")

	// The owner may refresh it.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, fetcher.recordedTarballURLs(), 1)
	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Len(t, got.Files, 2)
}

func TestSelectTemplateSourceDir(t *testing.T) {
	tmpl := &store.Template{Name: "My Template", Slug: "my-template"}
	dir := func(name string) resourceDir { return resourceDir{name: name, path: "/x/" + name} }

	got, err := selectTemplateSourceDir([]resourceDir{dir("other"), dir("My Template")}, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "My Template", got.name, "folder named after the template's name")

	got, err = selectTemplateSourceDir([]resourceDir{dir("other"), dir("my-template")}, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "my-template", got.name, "folder named after the template's slug")

	got, err = selectTemplateSourceDir([]resourceDir{dir("My_Template!")}, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "My_Template!", got.name, "folder whose name slugifies to the slug")

	_, err = selectTemplateSourceDir([]resourceDir{dir("other")}, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain this template")

	_, err = selectTemplateSourceDir([]resourceDir{dir("my-template"), dir("other"), dir("My Template")}, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than one folder matching this template")
}

// TestTemplateReimport_SeveralMatchingFolders: a source with two folders
// that both match the template is refused and nothing is written.
func TestTemplateReimport_SeveralMatchingFolders(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	target := createReimportTemplateWithSlug(t, s, "tmpl-several", "My Template", "my-template",
		store.TemplateScopeGlobal, "", reimportTestSource)

	srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: buildTarGz(t, []tarEntry{
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "harness: claude\n"},
		{name: "repo-main/templates/My Template/scion-agent.yaml", body: "harness: claude\n"},
	})})
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+target.ID+"/reimport", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "more than one folder matching this template")

	got, err := s.GetTemplate(ctx, target.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Files, "nothing is written")
	assert.Equal(t, 1, countTemplates(t, s, store.TemplateScopeGlobal, ""))
}

// TestTemplateReimport_LeavesHarnessConfigsUnchanged: a source template folder
// that bundles harness-configs/ is refreshed without creating or updating any
// harness-config record, in every template scope.
func TestTemplateReimport_LeavesHarnessConfigsUnchanged(t *testing.T) {
	for _, scope := range []string{store.TemplateScopeGlobal, store.TemplateScopeUser, store.TemplateScopeProject} {
		t.Run(scope, func(t *testing.T) {
			srv, s, project, _ := setupWorkspaceProject(t, "reimport-hc-"+scope)
			ctx := context.Background()
			admin := newReimportAdmin(t, s)

			scopeID := ""
			hcScope, hcScopeID := store.HarnessConfigScopeGlobal, ""
			switch scope {
			case store.TemplateScopeUser:
				scopeID = admin.ID
			case store.TemplateScopeProject:
				scopeID = project.ID
				hcScope, hcScopeID = store.HarnessConfigScopeProject, project.ID
			}
			tmpl := createReimportTemplate(t, s, "tmpl-hc-"+scope, "my-template", scope, scopeID, reimportTestSource)
			if scope == store.TemplateScopeUser {
				tmpl.OwnerID = admin.ID
				require.NoError(t, s.UpdateTemplate(ctx, tmpl))
			}

			// An existing harness-config whose slug matches a bundled one.
			existing := &store.HarnessConfig{
				ID:          tid("hc-existing-" + scope),
				Name:        "probe-hc",
				Slug:        "probe-hc",
				Harness:     "claude",
				Scope:       hcScope,
				ScopeID:     hcScopeID,
				Status:      store.HarnessConfigStatusActive,
				ContentHash: "sha256:original",
			}
			require.NoError(t, s.CreateHarnessConfig(ctx, existing))
			countHC := func() int {
				res, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 1000})
				require.NoError(t, err)
				return len(res.Items)
			}
			before := countHC()

			srv.templateGitHubClient = newFakeReimportGitHubClient(t, &fakeReimportGitHub{body: buildTarGz(t, []tarEntry{
				{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
				{name: "repo-main/templates/my-template/harness-configs/probe-hc/config.yaml", body: "harness: claude\nimage: example/probe:latest\n"},
				{name: "repo-main/templates/my-template/harness-configs/new-hc/config.yaml", body: "harness: claude\nimage: example/new:latest\n"},
			})})
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetTemplate(ctx, tmpl.ID)
			require.NoError(t, err)
			assert.NotEmpty(t, got.Files, "the template itself is refreshed")

			assert.Equal(t, before, countHC(), "no harness-config may be created")
			after, err := s.GetHarnessConfig(ctx, existing.ID)
			require.NoError(t, err)
			assert.Equal(t, "sha256:original", after.ContentHash, "existing harness-config must be unchanged")
			assert.Empty(t, after.Files)
		})
	}
}

// TestTargetTemplatePersistence_HashMatchWritesNothing: the unchanged-content
// path of the refresh persistence writes nothing, neither the template nor any
// harness-config bundled in the source folder.
func TestTargetTemplatePersistence_HashMatchWritesNothing(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	tmpl := createReimportTemplate(t, s, "tmpl-hashmatch", "hashmatch", store.TemplateScopeGlobal, "", reimportTestSource)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"),
		[]byte("schema_version: \"1\"\nharness_config: probe-hc\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "harness-configs", "probe-hc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "harness-configs", "probe-hc", "config.yaml"),
		[]byte("harness: claude\n"), 0o644))

	p := srv.targetTemplateStore(tmpl.ID).pers
	rec, err := p.GetBySlug(ctx, "hashmatch", store.TemplateScopeGlobal, "")
	require.NoError(t, err)
	cd := collectTestDir(t, dir)
	changed, err := p.OnHashMatch(ctx, rec, cd)
	require.NoError(t, err)
	assert.False(t, changed)
	p.PostFinalize(ctx, rec, cd)

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Empty(t, got.DefaultHarnessConfig, "template must not be updated")
	res, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Empty(t, res.Items, "no harness-config may be created")
}

// TestTemplateReimport_OverrideNotStoredOnFailure: a valid sourceUrl override
// is stored only after a successful refresh. When the download fails, or the
// source has no folder for the template, the stored source URL, files and
// content hash are unchanged.
func TestTemplateReimport_OverrideNotStoredOnFailure(t *testing.T) {
	const stored = "https://github.com/acme/old/tree/main/templates/my-template"
	const override = "https://github.com/acme/repo/tree/main/templates"

	for name, fetcher := range map[string]*fakeReimportGitHub{
		"download fails": {err: &remotefetch.Error{Reason: remotefetch.ReasonStatus, Status: http.StatusNotFound, Detail: "status 404"}},
		"no matching folder": {body: buildTarGz(t, []tarEntry{
			{name: "repo-main/templates/unrelated/scion-agent.yaml", body: "harness: claude\n"},
		})},
	} {
		t.Run(name, func(t *testing.T) {
			srv, s, _ := testTemplateBootstrapServer(t)
			ctx := context.Background()
			admin := newReimportAdmin(t, s)
			tmpl := createReimportTemplate(t, s, "tmpl-override-fail", "my-template", store.TemplateScopeGlobal, "", stored)
			tmpl.Files = []store.TemplateFile{{Path: "scion-agent.yaml", Size: 5, Hash: "sha256:old"}}
			tmpl.ContentHash = "sha256:oldhash"
			require.NoError(t, setTemplateContentForTest(ctx, s, tmpl))

			srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport",
				ReimportTemplateRequest{SourceURL: override})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "reimport_failed")
			assert.NotEmpty(t, fetcher.recordedURLs(), "the override passed validation and was fetched")

			got, err := s.GetTemplate(ctx, tmpl.ID)
			require.NoError(t, err)
			assert.Equal(t, stored, got.SourceURL, "override must not be stored after a failed refresh")
			assert.Equal(t, "sha256:oldhash", got.ContentHash)
			require.Len(t, got.Files, 1)
			assert.Equal(t, "sha256:old", got.Files[0].Hash)
		})
	}
}

// TestTemplateReimport_UnchangedSourceKeepsHash: refreshing twice from the same
// source, which includes an executable file, stores the executable mode and
// leaves the content hash unchanged on the second refresh without redownloading
// the tarball or rewriting storage.
func TestTemplateReimport_UnchangedSourceKeepsHash(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-modes", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	fetcher := &fakeReimportGitHub{body: buildTarGz(t, []tarEntry{
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/hooks/start.sh", body: "#!/bin/sh\necho hi\n", mode: 0o755},
	})}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
	path := "/api/v1/templates/" + tmpl.ID + "/reimport"

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	first, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	modes := map[string]string{}
	for _, f := range first.Files {
		modes[f.Path] = f.Mode
	}
	assert.Equal(t, "0755", modes["hooks/start.sh"])
	assert.Equal(t, "0644", modes["scion-agent.yaml"])
	assert.Len(t, fetcher.recordedTarballURLs(), 1)

	// Second reimport at the same commit/tree SHA must be a no-op (no second tarball download).
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ContentHash, second.ContentHash, "an unchanged source must keep the content hash")
	assert.Len(t, fetcher.recordedTarballURLs(), 1, "unchanged tree SHA must skip tarball download and storage write")
}

func TestTemplateReimport_RecordsAndAdvancesSourceCommit(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-commit", "my-template", store.TemplateScopeGlobal, "", reimportTestSource+"/my-template")

	const commit1 = "1111111111111111111111111111111111111111"
	const commit2 = "2222222222222222222222222222222222222222"

	fetcher := &fakeReimportGitHub{}
	fetcher.setCommitBody(commit1, buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: commit1},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: "v1"},
	}))
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
	reimportPath := "/api/v1/templates/" + tmpl.ID + "/reimport"

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, commit1, got.SourceCommit)
	firstHash := got.ContentHash
	assert.Len(t, fetcher.recordedTarballURLs(), 1)

	// Refresh again with identical files (same tree SHA) at a newer commit:
	// tarball download and storage write are skipped, hash stays, commit advances.
	fetcher.setCommitBody(commit2, buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: commit2},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: "v1"},
	}))
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, firstHash, got.ContentHash)
	assert.Equal(t, commit2, got.SourceCommit)
	assert.Len(t, fetcher.recordedTarballURLs(), 1, "unchanged tree SHA must skip tarball download and storage write")
}

func TestTemplateReimport_ChangedTreeAtNewCommitReimports(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-commit-changed", "my-template", store.TemplateScopeGlobal, "", reimportTestSource+"/my-template")

	const commit1 = "1111111111111111111111111111111111111111"
	const commit2 = "2222222222222222222222222222222222222222"

	fetcher := &fakeReimportGitHub{}
	fetcher.setCommitBody(commit1, buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: commit1},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: "v1"},
	}))
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
	reimportPath := "/api/v1/templates/" + tmpl.ID + "/reimport"

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	first, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, commit1, first.SourceCommit)
	assert.Len(t, fetcher.recordedTarballURLs(), 1)

	// Advance to commit2 with a modified file (different tree SHA):
	// tarball MUST be downloaded, ContentHash must change, and SourceCommit must advance.
	fetcher.setCommitBody(commit2, buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: commit2},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: "v2-updated-content"},
	}))
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, commit2, second.SourceCommit)
	assert.NotEqual(t, first.ContentHash, second.ContentHash, "changed tree must rewrite storage and update ContentHash")
	assert.Len(t, fetcher.recordedTarballURLs(), 2, "changed tree SHA must download the new commit tarball")
}

func TestTemplateReimport_TagsSHAsAndSlashedBranches(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	cases := []struct {
		name      string
		sourceURL string
		branches  []string
		tags      []string
	}{
		{
			name:      "tag ref",
			sourceURL: "https://github.com/acme/repo/tree/v1.2.3/templates/my-template",
			branches:  []string{"main"},
			tags:      []string{"v1.2.3"},
		},
		{
			name:      "40-hex commit SHA",
			sourceURL: "https://github.com/acme/repo/tree/" + sha + "/templates/my-template",
		},
		{
			name:      "slashed branch feat/x/y",
			sourceURL: "https://github.com/acme/repo/tree/feat/x/y/templates/my-template",
			branches:  []string{"feat/x", "feat/x/y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _ := testTemplateBootstrapServer(t)
			ctx := context.Background()
			admin := newReimportAdmin(t, s)
			tmpl := createReimportTemplate(t, s, "tmpl-ref-"+tc.name, "my-template", store.TemplateScopeGlobal, "", tc.sourceURL)

			fetcher := &fakeReimportGitHub{
				commitSHA: sha,
				branches:  tc.branches,
				tags:      tc.tags,
				body: buildTarGz(t, []tarEntry{
					{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: sha},
					{name: "repo-sha/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
				}),
			}
			srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetTemplate(ctx, tmpl.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.sourceURL, got.SourceURL, "original ref must round-trip in sourceURL")
			assert.Equal(t, sha, got.SourceCommit, "source_commit must record the resolved SHA")
		})
	}
}

func TestTemplateReimport_BarePathSourceURL(t *testing.T) {
	const sha = "fedcba9876543210fedcba9876543210fedcba98"
	const bareURL = "https://github.com/acme/repo/templates/my-template"

	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-bare-path", "my-template", store.TemplateScopeGlobal, "", bareURL)

	fetcher := &fakeReimportGitHub{
		commitSHA: sha,
		body: buildTarGz(t, []tarEntry{
			{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: sha},
			{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
			{name: "repo-main/templates/my-template/README.md", body: "bare-path-ok"},
		}),
	}
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, bareURL, got.SourceURL)
	assert.Equal(t, sha, got.SourceCommit)
	assert.Len(t, got.Files, 2)
}

func TestTemplateReimport_OverrideFolderAtSameCommitDownloads(t *testing.T) {
	const (
		commitC = "3333333333333333333333333333333333333333"
		folderA = "https://github.com/acme/repo/tree/main/templates-a/my-template"
		folderB = "https://github.com/acme/repo/tree/main/templates-b/my-template"
	)

	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-same-commit-folder-override", "my-template", store.TemplateScopeGlobal, "", folderA)

	fetcher := &fakeReimportGitHub{}
	fetcher.setCommitBody(commitC, buildTarGz(t, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, body: commitC},
		{name: "repo-main/templates-a/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates-a/my-template/README.md", body: "folder-a-content"},
		{name: "repo-main/templates-b/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates-b/my-template/README.md", body: "folder-b-content"},
	}))
	srv.templateGitHubClient = newFakeReimportGitHubClient(t, fetcher)
	reimportPath := "/api/v1/templates/" + tmpl.ID + "/reimport"

	// First import from folder A at commit C.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	first, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, folderA, first.SourceURL)
	assert.Equal(t, commitC, first.SourceCommit)
	assert.Len(t, fetcher.recordedTarballURLs(), 1)

	// Reimport with sourceUrl override pointing to folder B in the same repo at the same commit C:
	// must NOT take the no-op shortcut; must download tarball and store folder B's content and URL.
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, reimportPath, ReimportTemplateRequest{SourceURL: folderB})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	second, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Len(t, fetcher.recordedTarballURLs(), 2, "override to a different folder at the same commit must download the tarball")
	assert.Equal(t, folderB, second.SourceURL, "folder B's URL must be stored")
	assert.Equal(t, commitC, second.SourceCommit)
	assert.NotEqual(t, first.ContentHash, second.ContentHash, "folder B's content must replace folder A's content")
}
