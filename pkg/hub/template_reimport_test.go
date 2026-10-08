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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
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
			require.NotNil(t, src)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseTemplateGitHubSource_ArchiveURL(t *testing.T) {
	src, err := parseTemplateGitHubSource("https://github.com/acme/repo/tree/dev/templates/my-template")
	require.NoError(t, err)
	assert.Equal(t, "templates/my-template", src.Path)
	assert.Equal(t, "https://github.com/acme/repo/archive/refs/heads/dev.tar.gz", src.archiveURL())

	src, err = parseTemplateGitHubSource("https://github.com/acme/repo")
	require.NoError(t, err)
	assert.Equal(t, "", src.Path)
	assert.Equal(t, "https://github.com/acme/repo/archive/refs/heads/main.tar.gz", src.archiveURL())
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
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
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Typeflag: tf, Name: e.name, PAXRecords: map[string]string{"comment": "abc123"},
			}))
			continue
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tf, Linkname: e.linkname}
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
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		{name: "repo-main/", typeflag: tar.TypeDir},
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
		{name: "repo-main/templates/my-template/README.md", body: readme},
		{name: "repo-main/templates/other-template/scion-agent.yaml", body: "schema_version: \"1\"\nharness: claude\n"},
	})
}

// fakeTemplateSourceFetcher serves a fixed body (or error) and records the
// URLs it was asked for.
type fakeTemplateSourceFetcher struct {
	body []byte
	err  error
	urls []string
}

func (f *fakeTemplateSourceFetcher) FetchBytes(_ context.Context, rawURL string) (*remotefetch.Result, error) {
	f.urls = append(f.urls, rawURL)
	if f.err != nil {
		return nil, f.err
	}
	return &remotefetch.Result{Body: f.body}, nil
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

	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ImportTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"my-template"}, resp.Templates)
	assert.Equal(t, 1, resp.Count)
	assert.Equal(t, []string{"https://github.com/acme/repo/archive/refs/heads/main.tar.gz"}, fetcher.urls)

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

	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

	override := "https://github.com/acme/repo/tree/main/templates"
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport",
		ReimportTemplateRequest{SourceURL: override})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"https://github.com/acme/repo/archive/refs/heads/main.tar.gz"}, fetcher.urls)

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, override+"/my-template", got.SourceURL)
}

func TestTemplateReimport_RefusesUnsupportedSources(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	admin := newReimportAdmin(t, s)

	// Refused sources must not be fetched at all.
	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

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
	assert.Empty(t, fetcher.urls, "refused sources must not be fetched")
}

func TestTemplateReimport_DownloadFailuresAreReported(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-fail", "my-template", store.TemplateScopeGlobal, "", reimportTestSource)

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"oversize download":  {&remotefetch.Error{Reason: remotefetch.ReasonTooLarge, Detail: "body over 1 bytes"}, "size limit"},
		"too many redirects": {&remotefetch.Error{Reason: remotefetch.ReasonRedirects, Detail: "more than 3 redirects"}, "could not download"},
		"internal address":   {&remotefetch.Error{Reason: remotefetch.ReasonDeniedAddress, Detail: "codeload.github.com resolves to denied address 10.0.0.5"}, "could not download"},
		"host off the list":  {&remotefetch.Error{Reason: remotefetch.ReasonHostNotAllowed, Detail: `host "other.test" is not allowed`}, "could not download"},
		"not found":          {&remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}, "may not be public"},
	} {
		t.Run(name, func(t *testing.T) {
			srv.templateSourceFetcher = &fakeTemplateSourceFetcher{err: tc.err}
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "reimport_failed")
			assert.Contains(t, rec.Body.String(), tc.want)
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

	big := strings.Repeat("x", int(defaultTemplateSourceLimits.MaxExtract)+1)
	srv.templateSourceFetcher = &fakeTemplateSourceFetcher{body: buildTarGz(t, []tarEntry{
		{name: "repo-main/templates/my-template/scion-agent.yaml", body: "harness: claude\n"},
		{name: "repo-main/templates/my-template/big.bin", body: big},
	})}
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "size limit")

	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Files, "a refused source must not change the template")
}

// TestTemplateSourceFetcher_DefaultIsGuarded checks the fetcher reimport uses
// when none is injected: it refuses http, hosts off the GitHub allow-list
// (including internal and metadata addresses given as IP literals) and URLs
// with credentials, before making any connection.
func TestTemplateSourceFetcher_DefaultIsGuarded(t *testing.T) {
	srv := &Server{}
	f := srv.getTemplateSourceFetcher()
	require.IsType(t, &remotefetch.Fetcher{}, f)

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
			// Each URL is refused by the per-hop URL checks, before any
			// name resolution or connection.
			_, err := f.FetchBytes(context.Background(), tc.url)
			var fe *remotefetch.Error
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tc.want, fe.Reason)
		})
	}
}

func TestExtractTemplateSource(t *testing.T) {
	lim := templateSourceLimits{MaxUnpacked: 1 << 20, MaxExtract: 64, MaxFiles: 3}
	readFile := func(t *testing.T, p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		return string(b)
	}

	t.Run("extracts only the requested folder", func(t *testing.T) {
		dest := t.TempDir()
		archive := buildTarGz(t, []tarEntry{
			{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
			{name: "repo-main/README.md", body: "root"},
			{name: "repo-main/templates/a/scion-agent.yaml", body: "a"},
			{name: "repo-main/templatesX/b", body: "b"},
		})
		require.NoError(t, extractTemplateSource(archive, "templates", dest, lim))
		assert.Equal(t, "a", readFile(t, filepath.Join(dest, "a", "scion-agent.yaml")))
		assert.NoFileExists(t, filepath.Join(dest, "README.md"))
		assert.NoFileExists(t, filepath.Join(dest, "b"))
	})

	t.Run("skips links and names that leave the folder", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out")
		require.NoError(t, os.MkdirAll(dest, 0o755))
		archive := buildTarGz(t, []tarEntry{
			{name: "repo-main/t/ok", body: "ok"},
			{name: "repo-main/t/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			{name: "repo-main/t/hard", typeflag: tar.TypeLink, linkname: "repo-main/t/ok"},
			{name: "repo-main/t/../../../outside", body: "x"},
			{name: "../../outside2", body: "x"},
		})
		require.NoError(t, extractTemplateSource(archive, "t", dest, lim))
		assert.FileExists(t, filepath.Join(dest, "ok"))
		assert.NoFileExists(t, filepath.Join(dest, "link"))
		assert.NoFileExists(t, filepath.Join(dest, "hard"))
		assert.NoFileExists(t, filepath.Join(filepath.Dir(dest), "outside"))
		assert.NoFileExists(t, filepath.Join(filepath.Dir(dest), "outside2"))
	})

	t.Run("refuses oversize extract", func(t *testing.T) {
		archive := buildTarGz(t, []tarEntry{
			{name: "repo-main/t/a", body: strings.Repeat("a", 40)},
			{name: "repo-main/t/b", body: strings.Repeat("b", 40)},
		})
		err := extractTemplateSource(archive, "t", t.TempDir(), lim)
		assert.ErrorIs(t, err, errTemplateSourceTooLarge)
	})

	t.Run("directories count toward the entry limit", func(t *testing.T) {
		archive := buildTarGz(t, []tarEntry{
			{name: "repo-main/t/d1/", typeflag: tar.TypeDir},
			{name: "repo-main/t/d2/", typeflag: tar.TypeDir},
			{name: "repo-main/t/d3/", typeflag: tar.TypeDir},
			{name: "repo-main/t/d4/", typeflag: tar.TypeDir},
		})
		err := extractTemplateSource(archive, "t", t.TempDir(), lim)
		assert.ErrorIs(t, err, errTemplateSourceTooLarge)
	})

	t.Run("refuses too many files", func(t *testing.T) {
		archive := buildTarGz(t, []tarEntry{
			{name: "repo-main/t/1", body: "1"},
			{name: "repo-main/t/2", body: "2"},
			{name: "repo-main/t/3", body: "3"},
			{name: "repo-main/t/4", body: "4"},
		})
		err := extractTemplateSource(archive, "t", t.TempDir(), lim)
		assert.ErrorIs(t, err, errTemplateSourceTooLarge)
	})

	t.Run("refuses oversize unpacked archive outside the folder", func(t *testing.T) {
		archive := buildTarGz(t, []tarEntry{
			{name: "repo-main/huge", body: strings.Repeat("z", 2<<20)},
			{name: "repo-main/t/a", body: "a"},
		})
		err := extractTemplateSource(archive, "t", t.TempDir(), lim)
		assert.ErrorIs(t, err, errTemplateSourceTooLarge)
	})

	t.Run("missing folder", func(t *testing.T) {
		archive := buildTarGz(t, []tarEntry{{name: "repo-main/other/a", body: "a"}})
		err := extractTemplateSource(archive, "t", t.TempDir(), lim)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("not an archive", func(t *testing.T) {
		err := extractTemplateSource([]byte("<html>"), "t", t.TempDir(), lim)
		require.Error(t, err)
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

	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher
	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, fetcher.urls)
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

	srv.templateSourceFetcher = &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}

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

			srv.templateSourceFetcher = &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
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

	srv.templateSourceFetcher = &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
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

	srv.templateSourceFetcher = &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
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

	assert.Error(t, p.Create(ctx, &ResourceRecord{}, t.TempDir()))

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

	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

	// Another user cannot read the template: it reads as not found.
	rec := doRequestAsUser(t, srv, other, http.MethodPost, path, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// A caller who can read it but is not the owner is refused.
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, fetcher.urls, "refused callers must not trigger a fetch")

	// The owner may refresh it.
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, fetcher.urls, 1)
	got, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Len(t, got.Files, 2)
}

func TestExtractTarget(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	for name, tc := range map[string]struct {
		rel string
		ok  bool
	}{
		"plain file":          {"a/b.txt", true},
		"nested dot segments": {"a/./b/../c.txt", true},
		"empty":               {"", false},
		"dot":                 {".", false},
		"parent":              {"..", false},
		"leaves dest":         {"../x", false},
		"leaves dest deeper":  {"a/../../x", false},
		"absolute":            {"/etc/passwd", false},
		"nul byte":            {"a\x00b", false},
	} {
		t.Run(name, func(t *testing.T) {
			target, ok := extractTarget(dest, tc.rel)
			assert.Equal(t, tc.ok, ok)
			if ok {
				r, err := filepath.Rel(dest, target)
				require.NoError(t, err)
				assert.False(t, strings.HasPrefix(r, ".."), "target %q is outside dest", target)
			}
		})
	}
}
