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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doRawRequestAsUser sends a raw-body request to a hub URL (absolute or
// path-only), authenticated as user.
func doRawRequestAsUser(t *testing.T, srv *Server, user *store.User, method, rawURL string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func setupLocalStorageSkillTest(t *testing.T) (*Server, *store.User, *store.User, *store.Skill) {
	t.Helper()
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)
	skill := createTestSkill(t, s, "files-skill-"+api.NewUUID()[:8], store.SkillScopeProject, project.ID, alice.ID)
	return srv, alice, bob, skill
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// TestSkillFiles_LocalStorageRoundTrip exercises the full two-phase publish
// and download flow against local storage, following the hub-proxied URLs
// returned by the API (regression test for #1785).
func TestSkillFiles_LocalStorageRoundTrip(t *testing.T) {
	srv, alice, _, skill := setupLocalStorageSkillTest(t)

	files := map[string][]byte{
		"SKILL.md":          []byte("---\nname: test\n---\n# Test"),
		"scripts/helper.sh": []byte("#!/bin/sh\necho hi\n"),
	}
	var uploadReqs []FileUploadRequest
	var manifest []store.TemplateFile
	for p, c := range files {
		uploadReqs = append(uploadReqs, FileUploadRequest{Path: p, Size: int64(len(c))})
		manifest = append(manifest, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: sha256Hex(c)})
	}

	// Phase 1: create draft version and get upload URLs.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
		PublishVersionRequest{Version: "1.0.0", Files: uploadReqs})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&pub))
	require.Len(t, pub.UploadURLs, len(files))

	// Phase 2: PUT each file to the returned hub proxy URL.
	for _, u := range pub.UploadURLs {
		assert.Contains(t, u.URL, "/api/v1/skills/"+skill.ID+"/files/"+u.Path)
		assert.Contains(t, u.URL, "version=1.0.0")
		rec := doRawRequestAsUser(t, srv, alice, u.Method, u.URL, files[u.Path])
		require.Equal(t, http.StatusOK, rec.Code, "upload %s: %s", u.Path, rec.Body.String())
	}

	// The /upload endpoint must produce equivalent URLs.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/upload",
		map[string]interface{}{"version": "1.0.0", "files": uploadReqs})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var up UploadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&up))
	for _, u := range up.UploadURLs {
		assert.Contains(t, u.URL, "version=1.0.0")
	}

	// Phase 3: finalize — verifies the files landed at the versioned path.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+skill.ID+"/finalize",
		FinalizeSkillVersionRequest{Version: "1.0.0", Manifest: &SkillManifest{Files: manifest}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Download: follow the proxy URLs and compare content.
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dl DownloadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&dl))
	require.Len(t, dl.Files, len(files))
	for _, f := range dl.Files {
		assert.Contains(t, f.URL, "raw=1&version=1.0.0")
		rec := doRawRequestAsUser(t, srv, alice, http.MethodGet, f.URL, nil)
		require.Equal(t, http.StatusOK, rec.Code, "download %s: %s", f.Path, rec.Body.String())
		assert.Equal(t, files[f.Path], rec.Body.Bytes(), f.Path)
	}

	// Published versions are immutable through the proxy.
	rec = doRawRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/skills/"+skill.ID+"/files/SKILL.md?version=1.0.0", []byte("overwrite"))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestSkillFiles_Validation(t *testing.T) {
	srv, alice, bob, skill := setupLocalStorageSkillTest(t)
	base := "/api/v1/skills/" + skill.ID + "/files/"

	tests := []struct {
		name   string
		user   *store.User
		method string
		url    string
		want   int
	}{
		{"missing version", alice, http.MethodPut, base + "SKILL.md", http.StatusBadRequest},
		{"bad version", alice, http.MethodPut, base + "SKILL.md?version=../x", http.StatusBadRequest},
		// ".." segments are cleaned by ServeMux (307) before reaching the
		// handler; backslash paths reach it and must be rejected.
		{"backslash path", alice, http.MethodPut, base + "a%5C..%5Cx?version=1.0.0", http.StatusBadRequest},
		{"empty path", alice, http.MethodGet, "/api/v1/skills/" + skill.ID + "/files", http.StatusNotFound},
		{"method", alice, http.MethodPost, base + "SKILL.md?version=1.0.0", http.StatusMethodNotAllowed},
		{"non-owner write", bob, http.MethodPut, base + "SKILL.md?version=1.0.0", http.StatusForbidden},
		{"unknown version read", alice, http.MethodGet, base + "SKILL.md?version=9.9.9", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRawRequestAsUser(t, srv, tc.user, tc.method, tc.url, []byte("x"))
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

func TestAppendSkillVersionQuery(t *testing.T) {
	assert.Equal(t, "http://h/api/v1/skills/id/files/a?version=1.0.0",
		appendSkillVersionQuery("http://h/api/v1/skills/id/files/a", "1.0.0"))
	assert.Equal(t, "http://h/api/v1/skills/id/files/a?raw=1&version=1.0.0",
		appendSkillVersionQuery("http://h/api/v1/skills/id/files/a?raw=1", "1.0.0"))
	// Cloud signed URLs are untouched.
	assert.Equal(t, "https://storage.googleapis.com/b/o?sig=x",
		appendSkillVersionQuery("https://storage.googleapis.com/b/o?sig=x", "1.0.0"))
}

// publishLocalSkillFile publishes version 1.0.0 of skill containing a single
// SKILL.md with content, via the hub-proxied local storage flow.
func publishLocalSkillFile(t *testing.T, srv *Server, owner *store.User, skill *store.Skill, content []byte) {
	t.Helper()
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions",
		PublishVersionRequest{Version: "1.0.0", Files: []FileUploadRequest{{Path: "SKILL.md", Size: int64(len(content))}}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&pub))
	require.Len(t, pub.UploadURLs, 1)
	rec = doRawRequestAsUser(t, srv, owner, pub.UploadURLs[0].Method, pub.UploadURLs[0].URL, content)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/skills/"+skill.ID+"/finalize",
		FinalizeSkillVersionRequest{Version: "1.0.0", Manifest: &SkillManifest{Files: []store.TemplateFile{
			{Path: "SKILL.md", Size: int64(len(content)), Hash: sha256Hex(content)},
		}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// doAnonymousRequest sends a request directly to the mux, bypassing the auth
// middleware, so the handler sees a nil identity (defense-in-depth).
func doAnonymousRequest(srv *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// TestSkillFiles_ReadAuthz verifies that skill file reads and /download
// enforce visibility: private skills require an identity with read access,
// public skills are readable anonymously.
func TestSkillFiles_ReadAuthz(t *testing.T) {
	srv, alice, bob, skill := setupLocalStorageSkillTest(t)
	content := []byte("---\nname: secret\n---\n# Secret")
	publishLocalSkillFile(t, srv, alice, skill, content)

	fileURL := "/api/v1/skills/" + skill.ID + "/files/SKILL.md?version=1.0.0"
	downloadURL := "/api/v1/skills/" + skill.ID + "/download?version=1.0.0"

	// Private skill.
	t.Run("private/anonymous file read denied", func(t *testing.T) {
		rec := doAnonymousRequest(srv, fileURL)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "Secret")
	})
	t.Run("private/anonymous download denied", func(t *testing.T) {
		rec := doAnonymousRequest(srv, downloadURL)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "/files/")
	})
	t.Run("private/no-access user file read denied", func(t *testing.T) {
		rec := doRawRequestAsUser(t, srv, bob, http.MethodGet, fileURL, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
	t.Run("private/no-access user download denied", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, downloadURL, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
	t.Run("private/authorized user file read allowed", func(t *testing.T) {
		rec := doRawRequestAsUser(t, srv, alice, http.MethodGet, fileURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, content, rec.Body.Bytes())
	})
	t.Run("private/authorized user download allowed", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, downloadURL, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	// Public skill: readable without identity.
	skill.Visibility = store.VisibilityPublic
	require.NoError(t, srv.store.UpdateSkill(context.Background(), skill))

	t.Run("public/anonymous file read allowed", func(t *testing.T) {
		rec := doAnonymousRequest(srv, fileURL)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, content, rec.Body.Bytes())
	})
	t.Run("public/anonymous download allowed", func(t *testing.T) {
		rec := doAnonymousRequest(srv, downloadURL)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}
