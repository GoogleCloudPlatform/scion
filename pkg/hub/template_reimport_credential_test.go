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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reimportCredSource is a folder in a repository that, for these tests, needs
// a credential: its GH_ convention names are GH_ACME_CORP__PRIVATE_TEMPLATES
// and GH_ACME_CORP.
const (
	reimportCredSource     = "https://github.com/acme-corp/private.templates/tree/main/templates"
	reimportCredAPITarball = "https://api.github.com/repos/acme-corp/private.templates/tarball/refs/heads/main"
	reimportCredArchive    = "https://github.com/acme-corp/private.templates/archive/refs/heads/main.tar.gz"
)

// useReimportCredBackend installs a credential backend on srv that serves
// values from credScope/scopeID only, captures srv's resource log, and
// returns both.
func useReimportCredBackend(srv *Server, credScope, scopeID string, values map[string]string) (*importCredSecretBackend, *bytes.Buffer) {
	backend := &importCredSecretBackend{scope: credScope, projectID: scopeID, values: values}
	srv.SetSecretBackend(backend)
	var logBuf bytes.Buffer
	srv.resourceLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return backend, &logBuf
}

// TestTemplateReimport_ProjectScopeSendsProjectCredential: reimporting a
// project template from a source that needs a credential sends the
// project's credential, selected in the same order as template import, to
// the API tarball endpoint. The value never reaches the log or the response.
func TestTemplateReimport_ProjectScopeSendsProjectCredential(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		want    string
		wantLog string
	}{
		{
			name:    "repo key",
			values:  map[string]string{"GH_ACME_CORP__PRIVATE_TEMPLATES": "fake-repo-cred", "GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-repo-cred",
			wantLog: "GH_ACME_CORP__PRIVATE_TEMPLATES",
		},
		{
			name:    "owner key",
			values:  map[string]string{"GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-owner-cred",
			wantLog: "GH_ACME_CORP",
		},
		{
			name:    "GITHUB_TOKEN",
			values:  map[string]string{"GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-default-cred",
			wantLog: "GITHUB_TOKEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s, project, _ := setupWorkspaceProject(t, "reimport-cred-project")
			admin := newReimportAdmin(t, s)
			tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-project", "my-template", store.TemplateScopeProject, project.ID, reimportCredSource)
			_, logBuf := useReimportCredBackend(srv, secret.ScopeProject, project.ID, tt.values)
			fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
			srv.templateSourceFetcher = fetcher

			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			assert.Equal(t, []string{reimportCredAPITarball}, fetcher.urls)
			assert.Equal(t, []string{"Bearer " + tt.want}, fetcher.auths)
			assert.Contains(t, logBuf.String(), "secret="+tt.wantLog)
			assertNoCredentialInLog(t, logBuf.String(), tt.values)
			assertNoCredentialInLog(t, rec.Body.String(), tt.values)

			got, err := s.GetTemplate(context.Background(), tmpl.ID)
			require.NoError(t, err)
			assert.Len(t, got.Files, 2, "the template is refreshed from the credentialed fetch")
		})
	}
}

// TestTemplateReimport_HubScopeSendsHubCredential: a global template is
// reimported with the hub's own credential.
func TestTemplateReimport_HubScopeSendsHubCredential(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-hub", "my-template", store.TemplateScopeGlobal, "", reimportCredSource)
	values := map[string]string{"GH_ACME_CORP": "fake-hub-cred"}
	_, logBuf := useReimportCredBackend(srv, secret.ScopeHub, srv.HubID(), values)
	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, []string{reimportCredAPITarball}, fetcher.urls)
	assert.Equal(t, []string{"Bearer fake-hub-cred"}, fetcher.auths)
	assert.Contains(t, logBuf.String(), "secret=GH_ACME_CORP")
	assertNoCredentialInLog(t, logBuf.String(), values)
	assertNoCredentialInLog(t, rec.Body.String(), values)
}

// TestTemplateReimport_UserScopeSendsOnlyThatUsersCredential: a user
// template is reimported with that user's own credential when the caller is
// that user, and with no credential (none even read) when the template's
// scope belongs to anyone else.
func TestTemplateReimport_UserScopeSendsOnlyThatUsersCredential(t *testing.T) {
	values := map[string]string{"GITHUB_TOKEN": "fake-user-cred"}

	t.Run("caller is the scope's user", func(t *testing.T) {
		srv, s, _ := testTemplateBootstrapServer(t)
		owner := &store.User{ID: tid("user-reimport-cred-owner"), Email: "reimport-cred-owner@test.com", DisplayName: "Owner", Role: store.UserRoleMember}
		require.NoError(t, s.CreateUser(context.Background(), owner))
		ensureHubMembership(context.Background(), s, owner.ID)
		tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-user", "my-template", store.TemplateScopeUser, owner.ID, reimportCredSource)
		tmpl.OwnerID = owner.ID
		require.NoError(t, s.UpdateTemplate(context.Background(), tmpl))
		_, logBuf := useReimportCredBackend(srv, secret.ScopeUser, owner.ID, values)
		fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
		srv.templateSourceFetcher = fetcher

		rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		assert.Equal(t, []string{reimportCredAPITarball}, fetcher.urls)
		assert.Equal(t, []string{"Bearer fake-user-cred"}, fetcher.auths)
		assertNoCredentialInLog(t, logBuf.String(), values)
		assertNoCredentialInLog(t, rec.Body.String(), values)
	})

	t.Run("caller is not the scope's user", func(t *testing.T) {
		// Whatever the endpoint's checks allow, the fetch itself never
		// spends a user-scope credential unless the caller is that user:
		// it is not even read, and the public archive is fetched.
		const scopeUser = "user-reimport-cred-scope"
		for name, ctx := range map[string]context.Context{
			"no caller":      context.Background(),
			"another caller": contextWithIdentity(context.Background(), NewAuthenticatedUser("user-reimport-cred-else", "e@test.com", "E", "member", "web")),
		} {
			t.Run(name, func(t *testing.T) {
				srv, _, _ := testTemplateBootstrapServer(t)
				backend, _ := useReimportCredBackend(srv, secret.ScopeUser, scopeUser, values)
				fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
				srv.templateSourceFetcher = fetcher
				src, err := parseTemplateGitHubSource(reimportCredSource)
				require.NoError(t, err)
				tmpl := &store.Template{Scope: store.TemplateScopeUser, ScopeID: scopeUser, OwnerID: scopeUser}

				_, err = srv.fetchTemplateSourceArchive(ctx, src, reimportCredSource, tmpl)
				require.NoError(t, err)
				assert.Equal(t, []string{reimportCredArchive}, fetcher.urls)
				assert.Equal(t, []string{""}, fetcher.auths)
				assert.Zero(t, backend.gets, "another user's credentials must not be read")
			})
		}
	})
}

// TestTemplateReimport_NoCredentialFetchesPublicArchive: with no credential
// configured the public archive URL is fetched with no Authorization header.
func TestTemplateReimport_NoCredentialFetchesPublicArchive(t *testing.T) {
	srv, s, project, _ := setupWorkspaceProject(t, "reimport-cred-none")
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-none", "my-template", store.TemplateScopeProject, project.ID, reimportCredSource)
	// Credentials for another owner do not apply to acme-corp.
	useReimportCredBackend(srv, secret.ScopeProject, project.ID, map[string]string{"GH_OTHER_ORG": "fake-other-cred"})
	fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
	srv.templateSourceFetcher = fetcher

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{reimportCredArchive}, fetcher.urls)
	assert.Equal(t, []string{""}, fetcher.auths)
}

// TestTemplateReimport_NonGitHubSourceReadsNoCredential: a source reimport
// does not support is refused before any credential is read or any fetch.
func TestTemplateReimport_NonGitHubSourceReadsNoCredential(t *testing.T) {
	for name, source := range map[string]string{
		"archive url": "https://github.com/acme-corp/private.templates/archive/refs/heads/main.tar.gz",
		"other host":  "https://example.com/acme-corp/templates.tgz",
		"rclone":      ":gcs:bucket/templates",
		"gitlab":      "https://gitlab.com/acme-corp/private.templates/tree/main/templates",
	} {
		t.Run(name, func(t *testing.T) {
			srv, s, project, _ := setupWorkspaceProject(t, "reimport-cred-nongh")
			admin := newReimportAdmin(t, s)
			tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-nongh", "my-template", store.TemplateScopeProject, project.ID, source)
			backend, _ := useReimportCredBackend(srv, secret.ScopeProject, project.ID, map[string]string{"GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"})
			fetcher := &fakeTemplateSourceFetcher{body: twoTemplateArchive(t, "v2")}
			srv.templateSourceFetcher = fetcher

			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Empty(t, fetcher.urls)
			assert.Zero(t, backend.gets, "no credential may be read for a refused source")
		})
	}
}

// TestTemplateReimport_CredentialedFailureLogsNoValue: when the credentialed
// download fails, neither the response nor the resource log carries the
// credential value.
func TestTemplateReimport_CredentialedFailureLogsNoValue(t *testing.T) {
	srv, s, project, _ := setupWorkspaceProject(t, "reimport-cred-fail")
	admin := newReimportAdmin(t, s)
	tmpl := createReimportTemplate(t, s, "tmpl-reimport-cred-fail", "my-template", store.TemplateScopeProject, project.ID, reimportCredSource)
	values := map[string]string{"GH_ACME_CORP__PRIVATE_TEMPLATES": "fake-repo-cred"}
	_, logBuf := useReimportCredBackend(srv, secret.ScopeProject, project.ID, values)
	fetcher := &fakeTemplateSourceFetcher{err: &remotefetch.Error{Reason: remotefetch.ReasonStatus, Detail: "status 404"}}
	srv.templateSourceFetcher = fetcher

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"Bearer fake-repo-cred"}, fetcher.auths)
	assert.Contains(t, logBuf.String(), "template reimport: source download failed")
	assertNoCredentialInLog(t, logBuf.String(), values)
	assertNoCredentialInLog(t, rec.Body.String(), values)
}

// TestTemplateSourceFetcher_AuthorizedIsGuarded: the credentialed fetch of the
// default fetcher keeps the same URL rules and host allow-list.
func TestTemplateSourceFetcher_AuthorizedIsGuarded(t *testing.T) {
	f := (&Server{}).getTemplateSourceFetcher()
	for name, tc := range map[string]struct {
		url  string
		want remotefetch.Reason
	}{
		"http":       {"http://api.github.com/repos/acme/repo/tarball/refs/heads/main", remotefetch.ReasonScheme},
		"other host": {"https://example.com/repos/acme/repo/tarball/refs/heads/main", remotefetch.ReasonHostNotAllowed},
		"metadata":   {"https://169.254.169.254/latest/meta-data/", remotefetch.ReasonHostNotAllowed},
		"userinfo":   {"https://u:p@api.github.com/repos/acme/repo/tarball/refs/heads/main", remotefetch.ReasonUserinfo},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.FetchBytesAuthorized(context.Background(), tc.url, "Bearer fake-cred")
			var fe *remotefetch.Error
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tc.want, fe.Reason)
		})
	}
}
