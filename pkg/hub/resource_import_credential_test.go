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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// importCredSecretBackend is a secret backend serving project-scope values
// from a map. Only Get is implemented; the embedded nil interface panics on
// any other call, which keeps the test honest about what importCredential
// touches.
type importCredSecretBackend struct {
	secret.SecretBackend
	projectID string
	values    map[string]string
	getErr    error
}

func (b *importCredSecretBackend) Get(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	if scope != secret.ScopeProject || scopeID != b.projectID {
		return nil, store.ErrNotFound
	}
	val, ok := b.values[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name}, Value: val}, nil
}

// newImportCredServer returns a server whose secret backend serves values for
// projectID and whose resource log is captured in the returned buffer. The
// project does not exist in the store, so no GitHub App token is minted.
func newImportCredServer(t *testing.T, projectID string, values map[string]string) (*Server, *bytes.Buffer) {
	t.Helper()
	srv, _ := testServer(t)
	srv.SetSecretBackend(&importCredSecretBackend{projectID: projectID, values: values})
	var logBuf bytes.Buffer
	srv.resourceLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return srv, &logBuf
}

// assertNoCredentialInLog fails if any credential value appears in the log.
func assertNoCredentialInLog(t *testing.T, log string, values map[string]string) {
	t.Helper()
	for name, val := range values {
		if strings.Contains(log, val) {
			t.Errorf("resource log contains the value of %s: %s", name, log)
		}
	}
}

// TestImportCredential_GitHubCascade verifies the project-scope cascade for
// GitHub folder URLs: the repo-specific key beats the owner-level key, which
// beats GITHUB_TOKEN; with none set the fetch is unauthenticated.
func TestImportCredential_GitHubCascade(t *testing.T) {
	const (
		projectID = "project-import-cred"
		sourceURL = "https://github.com/acme-corp/private.templates/tree/main/.scion/templates"
		repoKey   = "GH_ACME_CORP__PRIVATE_TEMPLATES"
		ownerKey  = "GH_ACME_CORP"
	)
	tests := []struct {
		name    string
		values  map[string]string
		want    string
		wantLog string
	}{
		{
			name:    "repo key beats owner and default",
			values:  map[string]string{repoKey: "fake-repo-cred", ownerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-repo-cred",
			wantLog: repoKey,
		},
		{
			name:    "owner key beats default",
			values:  map[string]string{ownerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-owner-cred",
			wantLog: ownerKey,
		},
		{
			name:    "default when no convention key",
			values:  map[string]string{"GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-default-cred",
			wantLog: "GITHUB_TOKEN",
		},
		{
			name:    "empty convention values fall through",
			values:  map[string]string{repoKey: "", ownerKey: "", "GITHUB_TOKEN": "fake-default-cred"},
			want:    "fake-default-cred",
			wantLog: "GITHUB_TOKEN",
		},
		{
			name:   "keys for another owner are ignored",
			values: map[string]string{"GH_OTHER_ORG": "fake-other-cred", "GH_OTHER_ORG__PRIVATE_TEMPLATES": "fake-other-repo-cred"},
			want:   "",
		},
		{
			name:   "unauthenticated when nothing is set",
			values: map[string]string{},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, logBuf := newImportCredServer(t, projectID, tt.values)
			got := srv.importCredential(context.Background(), projectID, sourceURL)
			if got != tt.want {
				t.Errorf("importCredential = %q, want %q", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(logBuf.String(), "secret="+tt.wantLog) {
				t.Errorf("resource log does not name secret %s: %s", tt.wantLog, logBuf.String())
			}
			assertNoCredentialInLog(t, logBuf.String(), tt.values)
		})
	}
}

// TestImportCredential_NonGitHubURLsUnaffected verifies that URLs which are
// not GitHub folders (archives, even on github.com, rclone and other hosts)
// never consult the GH_ convention keys and keep the GITHUB_TOKEN fallback.
func TestImportCredential_NonGitHubURLsUnaffected(t *testing.T) {
	const projectID = "project-import-cred-nongh"
	values := map[string]string{
		"GH_ACME_CORP__TEMPLATES": "fake-repo-cred",
		"GH_ACME_CORP":            "fake-owner-cred",
		"GITHUB_TOKEN":            "fake-default-cred",
	}
	for _, sourceURL := range []string{
		"https://github.com/acme-corp/templates/archive/main.tar.gz",
		"https://example.com/acme-corp/templates.tgz",
		":gcs:bucket/acme-corp/templates",
	} {
		t.Run(sourceURL, func(t *testing.T) {
			srv, logBuf := newImportCredServer(t, projectID, values)
			if got := srv.importCredential(context.Background(), projectID, sourceURL); got != "fake-default-cred" {
				t.Errorf("importCredential = %q, want the GITHUB_TOKEN value", got)
			}
			assertNoCredentialInLog(t, logBuf.String(), values)
		})
	}
}

// TestImportCredential_NoProject verifies that a global-scope import (no
// project) stays unauthenticated even when convention keys exist.
func TestImportCredential_NoProject(t *testing.T) {
	srv, _ := newImportCredServer(t, "", map[string]string{"GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"})
	if got := srv.importCredential(context.Background(), "", "https://github.com/acme-corp/templates"); got != "" {
		t.Errorf("importCredential = %q, want unauthenticated", got)
	}
}

// TestImportCredential_BackendErrorFallsThrough verifies that a secret
// backend failure is logged without any credential value and leaves the
// fetch unauthenticated rather than failing it.
func TestImportCredential_BackendErrorFallsThrough(t *testing.T) {
	const projectID = "project-import-cred-err"
	srv, logBuf := newImportCredServer(t, projectID, nil)
	srv.SetSecretBackend(&importCredSecretBackend{projectID: projectID, getErr: errors.New("backend unavailable")})
	if got := srv.importCredential(context.Background(), projectID, "https://github.com/acme-corp/templates"); got != "" {
		t.Errorf("importCredential = %q, want unauthenticated", got)
	}
	if !strings.Contains(logBuf.String(), "secret=GH_ACME_CORP__TEMPLATES") {
		t.Errorf("resource log does not report the failed lookup: %s", logBuf.String())
	}
}

// TestFetchRemoteForImport_RepoKeyOnTheWire verifies end to end that a
// template or harness-config import sends the repo-specific project secret,
// stored through the real local secret backend, as the Bearer credential.
func TestFetchRemoteForImport_RepoKeyOnTheWire(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()
	projectID := skillDiscoverProject(t, srv, s, "ghcred", "fake-default-cred")
	if _, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
		Name: "GH_ACME_CORP__TEMPLATES", Value: "fake-repo-cred", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeProject, ScopeID: projectID,
	}); err != nil {
		t.Fatalf("failed to store repo credential: %v", err)
	}

	var capturedAuth string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/templates/my-template/scion-agent.yaml": "harness: claude\n",
	}, func(req *http.Request) {
		capturedAuth = req.Header.Get("Authorization")
	})()

	cachePath, err := srv.fetchRemoteForImport(ctx, projectID, "https://github.com/acme-corp/templates/tree/main/templates")
	if err != nil {
		t.Fatalf("fetchRemoteForImport: %v", err)
	}
	defer os.RemoveAll(cachePath)
	if capturedAuth != "Bearer fake-repo-cred" {
		t.Errorf("Authorization header = %q, want the repo-specific credential", capturedAuth)
	}
}
