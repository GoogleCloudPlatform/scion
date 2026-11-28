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
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeImportGitHub is an httptest fake GitHub server for Phase 2 acceptance
// tests covering import, discover, harness-config reimport, and skills
// discover-directory.
type fakeImportGitHub struct {
	mu           sync.Mutex
	refs         map[string]string // "refs/heads/main" / "refs/tags/v1.0" -> 40-hex sha
	filesByPath  map[string]map[string]string
	requireToken string // when non-empty, returns 404/401 if Authorization != "Bearer "+requireToken
	forceStatus  int
	forceHeaders map[string]string
	forceBody    string
	delay        time.Duration
	requests     []fakeGHRequest
}

type fakeGHRequest struct {
	Path string
	Auth string
}

func (f *fakeImportGitHub) record(r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeGHRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
	f.mu.Unlock()
}

func (f *fakeImportGitHub) reqs() []fakeGHRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeGHRequest(nil), f.requests...)
}

func (f *fakeImportGitHub) resetReqs() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

func (f *fakeImportGitHub) client(t *testing.T) *githubfetch.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		delay := f.delay
		forceStatus := f.forceStatus
		forceHeaders := f.forceHeaders
		forceBody := f.forceBody
		reqTok := f.requireToken
		f.mu.Unlock()

		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}

		if forceStatus != 0 {
			for k, v := range forceHeaders {
				w.Header().Set(k, v)
			}
			w.WriteHeader(forceStatus)
			_, _ = w.Write([]byte(forceBody))
			return
		}

		if reqTok != "" && r.Header.Get("Authorization") != "Bearer "+reqTok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		p := r.URL.Path
		switch {
		case strings.Contains(p, "/git/matching-refs/heads/"):
			var entries []map[string]any
			for refName, sha := range f.refs {
				if strings.HasPrefix(refName, "refs/heads/") {
					entries = append(entries, map[string]any{
						"ref":    refName,
						"object": map[string]any{"type": "commit", "sha": sha},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(entries)
		case strings.Contains(p, "/git/matching-refs/tags/"):
			var entries []map[string]any
			for refName, sha := range f.refs {
				if strings.HasPrefix(refName, "refs/tags/") {
					entries = append(entries, map[string]any{
						"ref":    refName,
						"object": map[string]any{"type": "commit", "sha": sha},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(entries)
		case strings.Contains(p, "/commits/"):
			idx := strings.Index(p, "/commits/")
			ref := p[idx+len("/commits/"):]
			sha, ok := f.refs[ref]
			if !ok {
				sha, ok = f.refs["refs/heads/"+ref]
			}
			if !ok {
				sha, ok = f.refs["refs/tags/"+ref]
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(sha))
		case strings.Contains(p, "/git/trees/"):
			idx := strings.Index(p, "/git/trees/")
			treeish := p[idx+len("/git/trees/"):]
			subPath := ""
			if _, after, ok := strings.Cut(treeish, ":"); ok {
				subPath = strings.Trim(after, "/")
			}
			files, ok := f.filesByPath[subPath]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var treeEntries []map[string]any
			for rel, body := range files {
				h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
				_, _ = fmt.Fprintf(h, "blob %d\x00", len(body))
				_, _ = h.Write([]byte(body))
				treeEntries = append(treeEntries, map[string]any{
					"path": rel,
					"mode": "100644",
					"type": "blob",
					"sha":  hex.EncodeToString(h.Sum(nil)),
					"size": len(body),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":       "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				"truncated": false,
				"tree":      treeEntries,
			})
		case strings.Contains(p, "/tarball/") || strings.Contains(p, "/archive/"):
			var sha string
			if idx := strings.Index(p, "/tarball/"); idx >= 0 {
				sha = p[idx+len("/tarball/"):]
			} else if idx := strings.Index(p, "/archive/"); idx >= 0 {
				sha = strings.TrimSuffix(p[idx+len("/archive/"):], ".tar.gz")
			}
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			_ = tw.WriteHeader(&tar.Header{
				Typeflag:   tar.TypeXGlobalHeader,
				Name:       "pax_global_header",
				PAXRecords: map[string]string{"comment": sha},
			})
			for subPath, files := range f.filesByPath {
				prefix := "repo-" + sha + "/"
				if subPath != "" {
					prefix += subPath + "/"
				}
				for rel, body := range files {
					_ = tw.WriteHeader(&tar.Header{
						Typeflag: tar.TypeReg,
						Name:     prefix + rel,
						Mode:     0o644,
						Size:     int64(len(body)),
					})
					_, _ = tw.Write([]byte(body))
				}
			}
			_ = tw.Close()
			_ = gzw.Close()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf.Bytes())
		default:
			// Raw file request: /<owner>/<repo>/<sha>/<subPath>/<rel>
			parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 4)
			if len(parts) == 4 {
				fullPath := strings.Trim(parts[3], "/")
				for subPath, files := range f.filesByPath {
					for rel, body := range files {
						cand := strings.Trim(rel, "/")
						if subPath != "" {
							cand = strings.Trim(subPath, "/") + "/" + cand
						}
						if cand == fullPath {
							w.WriteHeader(http.StatusOK)
							_, _ = w.Write([]byte(body))
							return
						}
					}
				}
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	doer := &fakeImportDoer{client: srv.Client()}
	ep := githubfetch.Endpoint{APIBase: srv.URL, RawBase: srv.URL, WebHost: srv.URL, Codeload: srv.URL}
	return githubfetch.New(githubfetch.Options{
		Endpoint:   ep,
		Doer:       doer,
		Cooldown:   githubfetch.NewCooldown(nil),
		MaxRetries: 0,
	})
}

type fakeImportDoer struct {
	client *http.Client
}

func (d *fakeImportDoer) Open(ctx context.Context, r githubfetch.Request) (*githubfetch.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	if r.Authorization != "" {
		req.Header.Set("Authorization", r.Authorization)
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	return &githubfetch.Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// TestGitHubFetchPath2_RefsCredentialsAndAnonArchive exercises import,
// discover, harness-config reimport, and skills discover-directory against an
// httptest fake GitHub across:
//   - refs: main, slashed feat/x/y (with branch/dir prefix collision), tag, 40-hex SHA
//   - credentials: project GH_REPO, GH_OWNER, GITHUB_TOKEN, user secret, hub secret
//   - anonymous: archive URL with no Authorization header
func TestGitHubFetchPath2_RefsCredentialsAndAnonArchive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "") // No git binary required.

	const sha = "5555555555555555555555555555555555555555"
	fake := &fakeImportGitHub{
		refs: map[string]string{
			"refs/heads/main":     sha,
			"refs/heads/feat/x":   "1111111111111111111111111111111111111111", // shorter prefix collision!
			"refs/heads/feat/x/y": sha,                                        // longest match wins!
			"refs/tags/v2.0.0":    sha,
		},
		filesByPath: map[string]map[string]string{
			"templates": {
				"alpha/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			},
			"harness-configs": {
				"my-hc/config.yaml": "harness: claude\n",
			},
			"skills": {
				"my-skill/SKILL.md": "---\nname: my-skill\n---\n# Skill\n",
			},
		},
	}

	srv, s, _ := testTemplateBootstrapServer(t)
	srv.templateGitHubClient = fake.client(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "", "test-secret"))
	ctx := context.Background()

	admin := newReimportAdmin(t, s)
	proj := &store.Project{ID: tid("gf-p2-proj"), Name: "gf-p2", Slug: "gf-p2"}
	require.NoError(t, s.CreateProject(ctx, proj))
	grantUserActionOnResource(t, s, admin.ID, "template", proj.ID, ActionCreate)
	grantUserActionOnResource(t, s, admin.ID, "harness_config", proj.ID, ActionCreate)

	// 1. Anonymous public discover sends no Authorization header and never uses /tarball/.
	t.Run("anonymous discover sends no Authorization", func(t *testing.T) {
		fake.resetReqs()
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/discover-templates", map[string]string{
			"sourceUrl": "https://github.com/acme/public-repo/tree/main/templates",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"alpha"`)

		sawDownload := false
		for _, r := range fake.reqs() {
			assert.Empty(t, r.Auth, "anonymous request must not send Authorization: %s", r.Path)
			if strings.Contains(r.Path, "/archive/"+sha+".tar.gz") || strings.Contains(r.Path, "/"+sha+"/templates/") {
				sawDownload = true
			}
			assert.NotContains(t, r.Path, "/tarball/", "anonymous fetch must not use REST tarball")
		}
		assert.True(t, sawDownload, "expected anonymous raw or archive download request")
	})

	// 2. Private repo across project secrets (GITHUB_TOKEN < GH_OWNER < GH_REPO),
	//    hub secret (global scope), and user secret (user scope).
	t.Run("private repo credentials across project, global, and user scopes", func(t *testing.T) {
		sb := srv.GetSecretBackend()
		_, _, err := sb.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: "tok-proj-default", SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeProject, ScopeID: proj.ID,
		})
		require.NoError(t, err)
		fake.requireToken = "tok-proj-default"

		// Refs: main, slashed feat/x/y (longest prefix over feat/x), tag v2.0.0, 40-hex SHA.
		for _, url := range []string{
			"https://github.com/acme/priv/tree/main/templates",
			"https://github.com/acme/priv/tree/feat/x/y/templates",
			"https://github.com/acme/priv/tree/v2.0.0/templates",
			"https://github.com/acme/priv/tree/" + sha + "/templates",
		} {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/import-templates", map[string]string{
				"sourceUrl": url,
			})
			require.Equal(t, http.StatusOK, rec.Code, "url=%s body=%s", url, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"alpha"`)
		}

		// GH_OWNER overrides GITHUB_TOKEN.
		_, _, err = sb.Set(ctx, &secret.SetSecretInput{
			Name: "GH_ACME", Value: "tok-proj-owner", SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeProject, ScopeID: proj.ID,
		})
		require.NoError(t, err)
		fake.requireToken = "tok-proj-owner"
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/discover-harness-configs", map[string]string{
			"sourceUrl": "https://github.com/acme/priv/tree/feat/x/y/harness-configs",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"my-hc"`)

		// GH_OWNER__REPO overrides GH_OWNER.
		_, _, err = sb.Set(ctx, &secret.SetSecretInput{
			Name: "GH_ACME__PRIV", Value: "tok-proj-repo", SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeProject, ScopeID: proj.ID,
		})
		require.NoError(t, err)
		fake.requireToken = "tok-proj-repo"
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/skills/discover-directory", map[string]string{
			"sourceUrl": "https://github.com/acme/priv/tree/main/skills",
			"projectId": proj.ID,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"my-skill"`)

		// Global (hub) secret for global scope.
		_, _, err = sb.Set(ctx, &secret.SetSecretInput{
			Name: "GH_ACME__PRIV", Value: "tok-hub-global", SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeHub, ScopeID: srv.HubID(),
		})
		require.NoError(t, err)
		fake.requireToken = "tok-hub-global"
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/discover", map[string]string{
			"kind": "template", "scope": "global",
			"sourceUrl": "https://github.com/acme/priv/tree/main/templates",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"alpha"`)

		// User secret for user scope.
		_, _, err = sb.Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: "tok-user-own", SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeUser, ScopeID: admin.ID,
		})
		require.NoError(t, err)
		fake.requireToken = "tok-user-own"
		rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/discover", map[string]string{
			"kind": "template", "scope": "user",
			"sourceUrl": "https://github.com/acme/priv/tree/main/templates",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"alpha"`)

		fake.requireToken = ""
	})

	// 2b. GitHub App installation credential on project scope.
	t.Run("GitHub App installation credential", func(t *testing.T) {
		const appToken = "ghs_p2_app_installation_token"
		appAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/app" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":          42,
					"slug":        "fake-scion-app",
					"permissions": map[string]string{"contents": "read"},
				})
				return
			}
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":      appToken,
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(appAPI.Close)

		appProj := &store.Project{
			ID:        tid("gf-p2-app-proj"),
			Name:      "gf-p2-app",
			Slug:      "gf-p2-app",
			GitRemote: appAPI.URL + "/acme/app-priv",
			CreatedBy: admin.ID,
		}
		const instID int64 = 954901
		instIDCopy := instID
		appProj.GitHubInstallationID = &instIDCopy
		require.NoError(t, s.CreateProject(ctx, appProj))
		require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: instID,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			Repositories:   []string{"acme/app-priv"},
			Status:         store.GitHubInstallationStatusActive,
		}))
		grantUserActionOnResource(t, s, admin.ID, "template", appProj.ID, ActionCreate)

		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privKey),
		})
		srv.mu.Lock()
		prevAppCfg := srv.config.GitHubAppConfig
		srv.config.GitHubAppConfig.APIBaseURL = appAPI.URL
		srv.config.GitHubAppConfig.AppID = 42
		srv.config.GitHubAppConfig.PrivateKey = string(pemBytes)
		srv.mu.Unlock()
		prevAccessCheck := srv.githubRepoAccessCheck
		srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
			return githubapp.RepoAccess{Pull: true}, nil
		}
		t.Cleanup(func() {
			srv.mu.Lock()
			srv.config.GitHubAppConfig = prevAppCfg
			srv.mu.Unlock()
			srv.githubRepoAccessCheck = prevAccessCheck
		})

		fake.requireToken = appToken
		t.Cleanup(func() { fake.requireToken = "" })

		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+appProj.ID+"/discover-templates", map[string]string{
			"sourceUrl": "https://github.com/acme/app-priv/tree/main/templates",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"alpha"`)
	})

	// 3. Harness-config reimport works via githubfetch.
	t.Run("harness-config reimport succeeds via githubfetch", func(t *testing.T) {
		// Import the harness-config first.
		rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/import-harness-configs", map[string]string{
			"sourceUrl": "https://github.com/acme/public-repo/tree/main/harness-configs",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		hc, err := s.GetHarnessConfigBySlug(ctx, "my-hc", store.HarnessConfigScopeProject, proj.ID)
		require.NoError(t, err)

		rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport", map[string]string{
			"sourceUrl": "https://github.com/acme/public-repo/tree/feat/x/y/harness-configs",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"my-hc"`)
	})
}

// TestGitHubFetchPath2_ErrorClassification verifies that hosted import,
// discover, harness-config reimport, and skills discover-directory map each
// GitHub status kind to the specified HTTP status, API code, fixed message,
// Retry-After header (on 429), and logged kind (for skills discover-directory).
func TestGitHubFetchPath2_ErrorClassification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeImportGitHub{refs: map[string]string{}}

	srv, s, _ := testTemplateBootstrapServer(t)
	srv.templateGitHubClient = fake.client(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "", "test-secret"))
	var logBuf bytes.Buffer
	srv.resourceLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	admin := newReimportAdmin(t, s)
	proj := &store.Project{ID: tid("gf-p2-err-proj"), Name: "gf-p2-err", Slug: "gf-p2-err"}
	require.NoError(t, s.CreateProject(ctx, proj))
	grantUserActionOnResource(t, s, admin.ID, "template", proj.ID, ActionCreate)
	grantUserActionOnResource(t, s, admin.ID, "harness_config", proj.ID, ActionCreate)

	hc := &store.HarnessConfig{
		ID:        tid("gf-p2-hc"),
		Name:      "my-hc",
		Slug:      "my-hc",
		Harness:   "claude",
		Scope:     store.HarnessConfigScopeProject,
		ScopeID:   proj.ID,
		SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
		Status:    store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	cases := []struct {
		name           string
		withCred       bool
		status         int
		headers        map[string]string
		wantHTTP       int
		wantImportCode string
		wantDiscCode   string
		wantReimpCode  string
		wantMsgSubstr  string
		wantRetryAfter string
		wantLogKind    string
	}{
		{
			name:           "404 anonymous",
			status:         http.StatusNotFound,
			wantHTTP:       http.StatusBadRequest,
			wantImportCode: "import_failed",
			wantDiscCode:   ErrCodeDiscoverFailed,
			wantReimpCode:  "reimport_failed",
			wantMsgSubstr:  "no repository credential is configured",
			wantLogKind:    "not_found",
		},
		{
			name:           "404 with credential",
			withCred:       true,
			status:         http.StatusNotFound,
			wantHTTP:       http.StatusBadRequest,
			wantImportCode: "import_failed",
			wantDiscCode:   ErrCodeDiscoverFailed,
			wantReimpCode:  "reimport_failed",
			wantMsgSubstr:  "configured repository credential cannot read it",
			wantLogKind:    "not_found",
		},
		{
			name:           "401 auth rejected",
			withCred:       true,
			status:         http.StatusUnauthorized,
			wantHTTP:       http.StatusBadRequest,
			wantImportCode: "import_failed",
			wantDiscCode:   ErrCodeDiscoverFailed,
			wantReimpCode:  "reimport_failed",
			wantMsgSubstr:  "rejected the configured repository credential",
			wantLogKind:    "auth_rejected",
		},
		{
			name:           "403 plain forbidden",
			withCred:       true,
			status:         http.StatusForbidden,
			wantHTTP:       http.StatusBadRequest,
			wantImportCode: "import_failed",
			wantDiscCode:   ErrCodeDiscoverFailed,
			wantReimpCode:  "reimport_failed",
			wantMsgSubstr:  "GitHub denied access to the repository",
			wantLogKind:    "forbidden",
		},
		{
			name:           "403 SSO forbidden",
			withCred:       true,
			status:         http.StatusForbidden,
			headers:        map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/acme/sso"},
			wantHTTP:       http.StatusBadRequest,
			wantImportCode: "import_failed",
			wantDiscCode:   ErrCodeDiscoverFailed,
			wantReimpCode:  "reimport_failed",
			wantMsgSubstr:  "SAML SSO",
			wantLogKind:    "forbidden",
		},
		{
			name:     "403 rate limited via X-RateLimit-Remaining 0",
			withCred: true,
			status:   http.StatusForbidden,
			headers: map[string]string{
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     fmt.Sprintf("%d", time.Now().Add(90*time.Second).Unix()),
			},
			wantHTTP:       http.StatusTooManyRequests,
			wantImportCode: "rate_limited",
			wantDiscCode:   "rate_limited",
			wantReimpCode:  "rate_limited",
			wantMsgSubstr:  "rate limit exceeded",
			wantLogKind:    "rate_limited",
		},
		{
			name:           "429 rate limited",
			status:         http.StatusTooManyRequests,
			headers:        map[string]string{"Retry-After": "90"},
			wantHTTP:       http.StatusTooManyRequests,
			wantImportCode: "rate_limited",
			wantDiscCode:   "rate_limited",
			wantReimpCode:  "rate_limited",
			wantMsgSubstr:  "rate limit exceeded",
			wantRetryAfter: "90",
			wantLogKind:    "rate_limited",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := srv.GetSecretBackend()
			if tc.withCred {
				_, _, err := sb.Set(ctx, &secret.SetSecretInput{
					Name: "GITHUB_TOKEN", Value: "tok-err-case", SecretType: secret.TypeEnvironment,
					Scope: secret.ScopeProject, ScopeID: proj.ID,
				})
				require.NoError(t, err)
			} else {
				_ = sb.Delete(ctx, "GITHUB_TOKEN", secret.ScopeProject, proj.ID)
			}

			// Fresh client per subtest so cooldown from 429 doesn't leak.
			fake.mu.Lock()
			fake.forceStatus = tc.status
			fake.forceHeaders = tc.headers
			fake.mu.Unlock()
			srv.templateGitHubClient = fake.client(t)

			// Import
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/import-templates", map[string]string{
				"sourceUrl": "https://github.com/acme/repo/tree/main/templates",
			})
			assert.Equal(t, tc.wantHTTP, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantImportCode)
			assert.Contains(t, rec.Body.String(), tc.wantMsgSubstr)
			assert.NotContains(t, rec.Body.String(), errImportDownloadFailed.Error(), "must not return legacy generic download error")
			if tc.wantRetryAfter != "" {
				assert.Equal(t, tc.wantRetryAfter, rec.Header().Get("Retry-After"))
			} else if tc.wantHTTP == http.StatusTooManyRequests {
				assert.NotEmpty(t, rec.Header().Get("Retry-After"))
			}

			// Discover
			srv.templateGitHubClient = fake.client(t)
			rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/discover", map[string]string{
				"kind": "template", "scope": "project", "scopeId": proj.ID,
				"sourceUrl": "https://github.com/acme/repo/tree/main/templates",
			})
			assert.Equal(t, tc.wantHTTP, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantDiscCode)
			assert.Contains(t, rec.Body.String(), tc.wantMsgSubstr)

			// HC Reimport
			srv.templateGitHubClient = fake.client(t)
			rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport", map[string]string{})
			assert.Equal(t, tc.wantHTTP, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantReimpCode)
			assert.Contains(t, rec.Body.String(), tc.wantMsgSubstr)

			// Skills discover-directory: keeps fixed message, logs kind on the
			// "skill discovery fetch failed" record itself.
			logBuf.Reset()
			srv.templateGitHubClient = fake.client(t)
			rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/skills/discover-directory", map[string]string{
				"sourceUrl": "https://github.com/acme/repo/tree/main/skills",
				"projectId": proj.ID,
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Failed to fetch remote skills; check the URL and repository access")
			foundSkillLog := false
			for _, line := range strings.Split(logBuf.String(), "\n") {
				if strings.Contains(line, "skill discovery fetch failed") && strings.Contains(line, "kind="+tc.wantLogKind) {
					foundSkillLog = true
					break
				}
			}
			assert.True(t, foundSkillLog, "expected 'skill discovery fetch failed' log record with kind=%s; got:\n%s", tc.wantLogKind, logBuf.String())
		})
	}
}

// TestTemplateReimport_WholeOperationTimeBudget verifies that
// reimportTemplateSourceWithLimits bounds ResolveRef, ListTree, and
// FetchFolderResolved under one shared lim.Timeout deadline.
func TestTemplateReimport_WholeOperationTimeBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const sha = "6666666666666666666666666666666666666666"
	fake := &fakeImportGitHub{
		refs:        map[string]string{"refs/heads/main": sha},
		filesByPath: map[string]map[string]string{"templates/t": {"scion-agent.yaml": "harness: claude\n"}},
		delay:       120 * time.Millisecond, // Each HTTP call takes 120ms; 3 calls = 360ms > 200ms total budget.
	}

	srv, _, _ := testTemplateBootstrapServer(t)
	srv.templateGitHubClient = fake.client(t)

	src, err := githubfetch.ParseURL("https://github.com/acme/repo/tree/main/templates/t")
	require.NoError(t, err)
	tmpl := &store.Template{
		ID:        tid("budget-tpl"),
		Name:      "t",
		Slug:      "t",
		Scope:     store.TemplateScopeGlobal,
		SourceURL: "https://github.com/acme/repo/tree/main/templates/t",
	}
	lim := githubfetch.ImportHostedLimits()
	lim.Timeout = 200 * time.Millisecond

	_, err = srv.reimportTemplateSourceWithLimits(context.Background(), src, tmpl.SourceURL, tmpl, nil, lim)
	require.Error(t, err)
	var rfe *reimportFetchError
	require.ErrorAs(t, err, &rfe)
	assert.Equal(t, githubfetch.KindTimeout, rfe.Kind, "combined ResolveRef+ListTree+FetchFolder exceeding lim.Timeout must fail as timeout")
}

// TestHostedImport_GitHubArchiveRedirect verifies that a hosted Hub import of a
// user-supplied https://github.com/<o>/<r>/archive/...tar.gz URL succeeds when
// github.com 302s to codeload.github.com, refuses redirects to other hosts, and
// never sends an Authorization header even when a project credential exists.
func TestHostedImport_GitHubArchiveRedirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Build a minimal template tarball served by the codeload-like server.
	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	content := []byte("schema_version: \"1\"\nharness: claude\n")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "repo-main/alpha/scion-agent.yaml",
		Mode:     0o644,
		Size:     int64(len(content)),
	}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	tarBytes := tarBuf.Bytes()

	var authHeaders []string
	var authMu sync.Mutex
	recordAuth := func(r *http.Request) {
		authMu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		authMu.Unlock()
	}

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordAuth(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(codeloadSrv.Close)

	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordAuth(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(evilSrv.Close)

	var redirectTarget string
	githubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordAuth(r)
		w.Header().Set("Location", redirectTarget)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(githubSrv.Close)

	// Map githubArchiveHosts onto our httptest servers while enforcing the
	// exact host allowlist and hop cap of newImportDownloader.
	hostMap := map[string]string{
		"github.com":          githubSrv.URL,
		"codeload.github.com": codeloadSrv.URL,
	}
	prevOverride := importDownloadOverride
	t.Cleanup(func() { importDownloadOverride = prevOverride })
	importDownloadOverride = func(ctx context.Context, rawURL, auth string) ([]byte, error) {
		assert.Empty(t, auth, "archive download must never receive an Authorization value")
		if !isGitHubArchiveDownloadURL(rawURL) {
			return nil, errImportDownloadFailed
		}
		cur := rawURL
		for hops := 0; hops <= importGitHubArchiveMaxRedirects; hops++ {
			u, parseErr := url.Parse(cur)
			if parseErr != nil {
				return nil, errImportDownloadFailed
			}
			host := strings.ToLower(u.Hostname())
			allowed := false
			for _, h := range githubArchiveHosts {
				if host == h {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, errImportDownloadFailed
			}
			base, ok := hostMap[host]
			if !ok {
				return nil, errImportDownloadFailed
			}
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, base+u.RequestURI(), nil)
			if reqErr != nil {
				return nil, errImportDownloadFailed
			}
			client := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
			resp, doErr := client.Do(req)
			if doErr != nil {
				return nil, errImportDownloadFailed
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				loc := resp.Header.Get("Location")
				_ = resp.Body.Close()
				cur = loc
				continue
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK {
				return nil, errImportDownloadFailed
			}
			return body, nil
		}
		return nil, errImportDownloadFailed
	}

	srv, s, _ := testTemplateBootstrapServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "", "test-secret"))
	ctx := context.Background()

	admin := newReimportAdmin(t, s)
	proj := &store.Project{ID: tid("gf-p2-arch-proj"), Name: "gf-p2-arch", Slug: "gf-p2-arch"}
	require.NoError(t, s.CreateProject(ctx, proj))
	grantUserActionOnResource(t, s, admin.ID, "template", proj.ID, ActionCreate)

	// Configure a project GITHUB_TOKEN to prove archive URLs never send it.
	_, _, err = srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
		Name: "GITHUB_TOKEN", Value: "tok-must-not-be-sent", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeProject, ScopeID: proj.ID,
	})
	require.NoError(t, err)

	// 1. 302 to codeload.github.com succeeds, imports alpha, sends no Authorization.
	redirectTarget = "https://codeload.github.com/acme/repo/tar.gz/refs/heads/main"
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/import-templates", map[string]string{
		"sourceUrl": "https://github.com/acme/repo/archive/main.tar.gz",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"alpha"`)
	authMu.Lock()
	require.Len(t, authHeaders, 2, "expected github.com hop + codeload.github.com hop")
	for i, h := range authHeaders {
		assert.Empty(t, h, "hop %d must not send Authorization", i)
	}
	authMu.Unlock()

	// 2. 302 to an off-allowlist host is refused.
	redirectTarget = "https://evil.example/payload.tar.gz"
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/import-templates", map[string]string{
		"sourceUrl": "https://github.com/acme/repo/archive/main.tar.gz",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), errImportDownloadFailed.Error())
}

// TestGitHubFetchPath2_HostedVsWorkstationFileLimit pins that hosted Hubs apply
// githubfetch.ImportHostedLimits() (MaxFiles=2000) while workstation Hubs apply
// githubfetch.ImportLocalLimits() (MaxFiles=20000) on Path 2 GitHub imports.
func TestGitHubFetchPath2_HostedVsWorkstationFileLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")

	const sha = "7777777777777777777777777777777777777777"
	files := make(map[string]string, 2001)
	files["alpha/scion-agent.yaml"] = "schema_version: \"1\"\nharness: claude\n"
	for i := 0; i < 2000; i++ {
		files[fmt.Sprintf("alpha/f-%04d.txt", i)] = "x"
	}
	fake := &fakeImportGitHub{
		refs: map[string]string{"refs/heads/main": sha},
		filesByPath: map[string]map[string]string{
			"templates": files,
		},
	}

	srv, s, _ := testTemplateBootstrapServer(t)
	srv.templateGitHubClient = fake.client(t)
	ctx := context.Background()

	admin := newReimportAdmin(t, s)
	proj := &store.Project{ID: tid("gf-p2-lim-proj"), Name: "gf-p2-lim", Slug: "gf-p2-lim"}
	require.NoError(t, s.CreateProject(ctx, proj))
	grantUserActionOnResource(t, s, admin.ID, "template", proj.ID, ActionCreate)

	// 1. Hosted Hub (MaxFiles=2000) rejects a 2,001-file folder as too large.
	srv.workstation = false
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/discover-templates", map[string]string{
		"sourceUrl": "https://github.com/acme/large-repo/tree/main/templates",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeDiscoverFailed)
	assert.Contains(t, rec.Body.String(), "GitHub source exceeds the size or file count limit")

	// 2. Workstation Hub (MaxFiles=20000) accepts the same 2,001-file folder.
	srv.workstation = true
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+proj.ID+"/discover-templates", map[string]string{
		"sourceUrl": "https://github.com/acme/large-repo/tree/main/templates",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"alpha"`)
}
