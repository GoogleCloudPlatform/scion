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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestIsCanonicalResourceFilePath(t *testing.T) {
	for _, p := range []string{"a", "config.yaml", "dir/file.txt", "a/b/c", "..x", "dir/..x", ".hidden", "dir/.hidden"} {
		assert.True(t, isCanonicalResourceFilePath(p), "expected %q to be canonical", p)
	}
	for _, p := range []string{"", ".", "..", "../a", "a/..", "a/../b", "./a", "a/./b", "a//b", "a/", "/a", `a\b`, "a\x00b"} {
		assert.False(t, isCanonicalResourceFilePath(p), "expected %q not to be canonical", p)
	}
}

func TestValidateManifestFilePaths_RejectsRepeatedPath(t *testing.T) {
	err := validateManifestFilePaths([]store.TemplateFile{{Path: "a"}, {Path: "b"}, {Path: "a"}})
	var pathErr *invalidFilePathError
	require.ErrorAs(t, err, &pathErr)
	assert.True(t, pathErr.duplicate)
	assert.NoError(t, validateManifestFilePaths([]store.TemplateFile{{Path: "a"}, {Path: "b"}}))
}

// pathMarker is part of every non-canonical upload path below, so a response
// body can be checked for any trace of the submitted value.
const pathMarker = "marker-q7z"

// nonCanonicalUploadPaths are requested upload paths that must be rejected.
var nonCanonicalUploadPaths = []string{
	"../../../../" + pathMarker + ".txt",
	"./" + pathMarker + ".txt",
	"dir//" + pathMarker + ".txt",
	"dir/../" + pathMarker + ".txt",
	"dir/" + pathMarker + "/",
	"/" + pathMarker + ".txt",
	`dir\` + pathMarker + ".txt",
	pathMarker + "\x00.txt",
	"",
}

// nonCanonicalManifests are finalize manifests that must be rejected. Each
// starts with the canonical file "SKILL.md". Every other listed path names an
// object that exists in local storage (see seedFinalizeObjects), except the
// NUL and empty cases, which local storage cannot hold; so only the path
// rules, not the storage existence check, can reject them.
var nonCanonicalManifests = map[string][]string{
	"dot prefix":       {"SKILL.md", "./SKILL.md"},
	"empty segment":    {"SKILL.md", "dir//f.txt"},
	"inner dotdot":     {"SKILL.md", "dir/../SKILL.md"},
	"parent reference": {"SKILL.md", "../sibling.txt"},
	"absolute":         {"SKILL.md", "/SKILL.md"},
	"backslash":        {"SKILL.md", `dir\f.txt`},
	"trailing slash":   {"SKILL.md", "dir/"},
	"repeated path":    {"SKILL.md", "SKILL.md"},
	"nul byte":         {"SKILL.md", "dir/f\x00.txt"},
	"empty":            {"SKILL.md", ""},
}

// seedFinalizeObjects writes the objects named by nonCanonicalManifests
// below basePath in stor.
func seedFinalizeObjects(t *testing.T, stor storage.Storage, basePath string) {
	t.Helper()
	for _, p := range []string{"SKILL.md", "dir/f.txt", `dir\f.txt`, "../sibling.txt"} {
		_, err := stor.Upload(context.Background(), basePath+"/"+p, strings.NewReader("x\n"), storage.UploadOptions{})
		require.NoError(t, err, "seed %s", p)
	}
}

func useLocalStorage(t *testing.T, srv *Server) storage.Storage {
	t.Helper()
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)
	return stor
}

func uploadFiles(paths ...string) []FileUploadRequest {
	files := make([]FileUploadRequest, 0, len(paths))
	for _, p := range paths {
		files = append(files, FileUploadRequest{Path: p, Size: 2})
	}
	return files
}

func manifestFiles(paths ...string) []store.TemplateFile {
	files := make([]store.TemplateFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, store.TemplateFile{Path: p, Size: 2, Hash: "sha256:placeholder"})
	}
	return files
}

// assertPathRejected checks for a 400 validation_error whose body does not
// contain the submitted path.
func assertPathRejected(t *testing.T, rec *httptest.ResponseRecorder, submitted string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, ErrCodeValidationError)
	assert.NotContains(t, body, pathMarker)
	if submitted != "" {
		quoted, _ := json.Marshal(submitted)
		assert.NotContains(t, body, strings.Trim(string(quoted), `"`))
	}
}

// --- harness-config ---

func TestHarnessConfigCreate_RequiresCanonicalUploadPaths(t *testing.T) {
	srv, s := testServer(t)
	useLocalStorage(t, srv)
	ctx := context.Background()

	for i, bad := range nonCanonicalUploadPaths {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
			Name: "hc-paths", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
			Files: uploadFiles("config.yaml", bad),
		})
		assertPathRejected(t, rec, bad)
		_, err := s.GetHarnessConfigBySlug(ctx, "hc-paths", store.HarnessConfigScopeGlobal, "")
		assert.ErrorIs(t, err, store.ErrNotFound, "case %d: no record may be created", i)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "hc-paths", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
		Files: uploadFiles("config.yaml", "scripts/provision.py"),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateHarnessConfigResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UploadURLs, 2)
}

func TestHarnessConfigUpload_RequiresCanonicalPaths(t *testing.T) {
	srv, s, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
	ctx := context.Background()

	for _, bad := range nonCanonicalUploadPaths {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/upload",
			UploadRequest{Files: uploadFiles("config.yaml", bad)})
		assertPathRejected(t, rec, bad)
	}
	got, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, hc.Files, got.Files)
	assert.Equal(t, hc.ContentHash, got.ContentHash)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/upload",
		UploadRequest{Files: uploadFiles("config.yaml", "scripts/provision.py")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp UploadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UploadURLs, 2)
}

func TestHarnessConfigFinalize_RequiresCanonicalManifest(t *testing.T) {
	for name, paths := range nonCanonicalManifests {
		t.Run(name, func(t *testing.T) {
			srv, s, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
			seedFinalizeObjects(t, srv.GetStorage(), hc.StoragePath)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize",
				map[string]interface{}{"manifest": HarnessConfigManifest{Files: manifestFiles(paths...)}})
			assertPathRejected(t, rec, paths[1])

			got, err := s.GetHarnessConfig(context.Background(), hc.ID)
			require.NoError(t, err)
			assert.Equal(t, hc.Files, got.Files, "record must be unchanged")
			assert.Equal(t, hc.ContentHash, got.ContentHash, "record must be unchanged")
		})
	}
}

// --- template ---

// createLocalTemplate creates a global template through the API with the
// canonical file "SKILL.md" and seeds its finalize objects.
func createLocalTemplate(t *testing.T, srv *Server, stor storage.Storage) *store.Template {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "tpl-paths", Harness: "claude", Scope: store.TemplateScopeGlobal,
		Files: uploadFiles("SKILL.md"),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateTemplateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.UploadURLs, 1)
	seedFinalizeObjects(t, stor, resp.Template.StoragePath)
	return resp.Template
}

func TestTemplateCreate_RequiresCanonicalUploadPaths(t *testing.T) {
	srv, s := testServer(t)
	useLocalStorage(t, srv)
	ctx := context.Background()

	for i, bad := range nonCanonicalUploadPaths {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
			Name: "tpl-paths", Harness: "claude", Scope: store.TemplateScopeGlobal,
			Files: uploadFiles("SKILL.md", bad),
		})
		assertPathRejected(t, rec, bad)
		_, err := s.GetTemplateBySlug(ctx, "tpl-paths", store.TemplateScopeGlobal, "")
		assert.ErrorIs(t, err, store.ErrNotFound, "case %d: no record may be created", i)
	}
}

func TestTemplateUpload_RequiresCanonicalPaths(t *testing.T) {
	srv, s := testServer(t)
	stor := useLocalStorage(t, srv)
	tmpl := createLocalTemplate(t, srv, stor)

	for _, bad := range nonCanonicalUploadPaths {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload",
			UploadRequest{Files: uploadFiles("SKILL.md", bad)})
		assertPathRejected(t, rec, bad)
	}
	got, err := s.GetTemplate(context.Background(), tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.Files, got.Files)
	assert.Equal(t, tmpl.Status, got.Status)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload",
		UploadRequest{Files: uploadFiles("SKILL.md", "dir/f.txt")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func TestTemplateFinalize_RequiresCanonicalManifest(t *testing.T) {
	for name, paths := range nonCanonicalManifests {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			stor := useLocalStorage(t, srv)
			tmpl := createLocalTemplate(t, srv, stor)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize",
				FinalizeRequest{Manifest: &TemplateManifest{Files: manifestFiles(paths...)}})
			assertPathRejected(t, rec, paths[1])

			got, err := s.GetTemplate(context.Background(), tmpl.ID)
			require.NoError(t, err)
			assert.Empty(t, got.Files, "record must be unchanged")
			assert.Equal(t, store.TemplateStatusPending, got.Status, "record must be unchanged")
		})
	}

	t.Run("canonical", func(t *testing.T) {
		srv, s := testServer(t)
		stor := useLocalStorage(t, srv)
		tmpl := createLocalTemplate(t, srv, stor)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize",
			FinalizeRequest{Manifest: &TemplateManifest{Files: manifestFiles("SKILL.md", "dir/f.txt")}})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		got, err := s.GetTemplate(context.Background(), tmpl.ID)
		require.NoError(t, err)
		assert.Len(t, got.Files, 2)
		assert.Equal(t, store.TemplateStatusActive, got.Status)
	})
}

// --- user template ---

func createLocalUserTemplate(t *testing.T, srv *Server, stor storage.Storage, user *store.User) *store.Template {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/users/me/templates", CreateTemplateRequest{
		Name: "user-tpl-paths", Harness: "claude", Files: uploadFiles("SKILL.md"),
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateTemplateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.UploadURLs, 1)
	seedFinalizeObjects(t, stor, resp.Template.StoragePath)
	return resp.Template
}

func TestUserTemplateCreate_RequiresCanonicalUploadPaths(t *testing.T) {
	srv, s, alice, _ := setupUserTemplateTest(t)
	useLocalStorage(t, srv)
	ctx := context.Background()

	for i, bad := range nonCanonicalUploadPaths {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", CreateTemplateRequest{
			Name: "user-tpl-paths", Harness: "claude", Files: uploadFiles("SKILL.md", bad),
		})
		assertPathRejected(t, rec, bad)
		_, err := s.GetTemplateBySlug(ctx, "user-tpl-paths", store.TemplateScopeUser, alice.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "case %d: no record may be created", i)
	}
}

func TestUserTemplateUpload_RequiresCanonicalPaths(t *testing.T) {
	srv, s, alice, _ := setupUserTemplateTest(t)
	stor := useLocalStorage(t, srv)
	tmpl := createLocalUserTemplate(t, srv, stor, alice)

	for _, bad := range nonCanonicalUploadPaths {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates/"+tmpl.ID+"/upload",
			UploadRequest{Files: uploadFiles("SKILL.md", bad)})
		assertPathRejected(t, rec, bad)
	}
	got, err := s.GetTemplate(context.Background(), tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.Files, got.Files)
	assert.Equal(t, tmpl.Status, got.Status)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates/"+tmpl.ID+"/upload",
		UploadRequest{Files: uploadFiles("SKILL.md", "dir/f.txt")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func TestUserTemplateFinalize_RequiresCanonicalManifest(t *testing.T) {
	for name, paths := range nonCanonicalManifests {
		t.Run(name, func(t *testing.T) {
			srv, s, alice, _ := setupUserTemplateTest(t)
			stor := useLocalStorage(t, srv)
			tmpl := createLocalUserTemplate(t, srv, stor, alice)

			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates/"+tmpl.ID+"/finalize",
				FinalizeRequest{Manifest: &TemplateManifest{Files: manifestFiles(paths...)}})
			assertPathRejected(t, rec, paths[1])

			got, err := s.GetTemplate(context.Background(), tmpl.ID)
			require.NoError(t, err)
			assert.Empty(t, got.Files, "record must be unchanged")
			assert.Equal(t, store.TemplateStatusPending, got.Status, "record must be unchanged")
		})
	}

	t.Run("canonical", func(t *testing.T) {
		srv, s, alice, _ := setupUserTemplateTest(t)
		stor := useLocalStorage(t, srv)
		tmpl := createLocalUserTemplate(t, srv, stor, alice)

		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates/"+tmpl.ID+"/finalize",
			FinalizeRequest{Manifest: &TemplateManifest{Files: manifestFiles("SKILL.md", "dir/f.txt")}})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		got, err := s.GetTemplate(context.Background(), tmpl.ID)
		require.NoError(t, err)
		assert.Len(t, got.Files, 2)
	})
}

// --- skill ---

func setupLocalSkill(t *testing.T) (*Server, store.Store, *store.User, *store.Skill, storage.Storage) {
	t.Helper()
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	stor := useLocalStorage(t, srv)
	skill := createTestSkill(t, s, "path-skill", store.SkillScopeProject, project.ID, alice.ID)
	return srv, s, alice, skill, stor
}

// publishLocalSkillDraft creates draft version 1.0.0 through the API with the
// canonical file "SKILL.md" and seeds its finalize objects.
func publishLocalSkillDraft(t *testing.T, srv *Server, stor storage.Storage, user *store.User, skill *store.Skill) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
		PublishVersionRequest{Version: "1.0.0", Files: uploadFiles("SKILL.md")})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	seedFinalizeObjects(t, stor, skill.StoragePath+"/1.0.0")
}

func TestSkillPublish_RequiresCanonicalUploadPaths(t *testing.T) {
	srv, s, alice, skill, _ := setupLocalSkill(t)
	ctx := context.Background()

	for i, bad := range nonCanonicalUploadPaths {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
			PublishVersionRequest{Version: "1.0.0", Files: uploadFiles("SKILL.md", bad)})
		assertPathRejected(t, rec, bad)
		_, err := s.GetSkillVersionByNumber(ctx, skill.ID, "1.0.0")
		assert.ErrorIs(t, err, store.ErrNotFound, "case %d: no version may be created", i)
	}

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
		PublishVersionRequest{Version: "1.0.0", Files: uploadFiles("SKILL.md", "dir/f.txt")})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UploadURLs, 2)
}

func TestSkillUpload_RequiresCanonicalPaths(t *testing.T) {
	srv, s, alice, skill, stor := setupLocalSkill(t)
	publishLocalSkillDraft(t, srv, stor, alice, skill)
	ctx := context.Background()
	before, err := s.GetSkillVersionByNumber(ctx, skill.ID, "1.0.0")
	require.NoError(t, err)

	for _, bad := range nonCanonicalUploadPaths {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload",
			map[string]interface{}{"version": "1.0.0", "files": uploadFiles("SKILL.md", bad)})
		assertPathRejected(t, rec, bad)
	}
	after, err := s.GetSkillVersionByNumber(ctx, skill.ID, "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, before.Files, after.Files)
	assert.Equal(t, before.Status, after.Status)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload",
		map[string]interface{}{"version": "1.0.0", "files": uploadFiles("SKILL.md", "dir/f.txt")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp UploadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UploadURLs, 2)
}

func TestSkillUpload_RequiresSemverVersion(t *testing.T) {
	srv, _, alice, skill, _ := setupLocalSkill(t)

	for _, version := range []string{"latest", "1.0.0/extra", "not-a-version"} {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload",
			map[string]interface{}{"version": version, "files": uploadFiles("SKILL.md")})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "version %q: body: %s", version, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
	}
}

func TestSkillFinalize_RequiresCanonicalManifest(t *testing.T) {
	for name, paths := range nonCanonicalManifests {
		t.Run(name, func(t *testing.T) {
			srv, s, alice, skill, stor := setupLocalSkill(t)
			publishLocalSkillDraft(t, srv, stor, alice, skill)

			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/finalize",
				FinalizeSkillVersionRequest{Version: "1.0.0", Manifest: &SkillManifest{Files: manifestFiles(paths...)}})
			assertPathRejected(t, rec, paths[1])

			sv, err := s.GetSkillVersionByNumber(context.Background(), skill.ID, "1.0.0")
			require.NoError(t, err)
			assert.Empty(t, sv.Files, "version must be unchanged")
			assert.Equal(t, store.SkillVersionStatusDraft, sv.Status, "version must be unchanged")
		})
	}

	t.Run("canonical", func(t *testing.T) {
		srv, s, alice, skill, stor := setupLocalSkill(t)
		publishLocalSkillDraft(t, srv, stor, alice, skill)

		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/finalize",
			FinalizeSkillVersionRequest{Version: "1.0.0", Manifest: &SkillManifest{Files: manifestFiles("SKILL.md", "dir/f.txt")}})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		sv, err := s.GetSkillVersionByNumber(context.Background(), skill.ID, "1.0.0")
		require.NoError(t, err)
		assert.Len(t, sv.Files, 2)
		assert.Equal(t, store.SkillVersionStatusPublished, sv.Status)
	})
}
