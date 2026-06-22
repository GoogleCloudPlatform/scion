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
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doMultipartRequestAsUser builds a multipart request, signs it as the given
// user, and executes it against the test server.
func doMultipartRequestAsUser(
	t *testing.T,
	srv *Server,
	user *store.User,
	method, path string,
	fields map[string]string,
	files map[string][]byte,
) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	for name, content := range files {
		part, err := writer.CreateFormFile("file", name)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doMultipartRequestAsUserOrdered builds a multipart request with ordered file
// parts (needed when testing duplicate filenames or large file counts).
func doMultipartRequestAsUserOrdered(
	t *testing.T,
	srv *Server,
	user *store.User,
	method, path string,
	version string,
	fileParts []struct {
		Name    string
		Content []byte
	},
) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	require.NoError(t, writer.WriteField("version", version))
	for _, fp := range fileParts {
		part, err := writer.CreateFormFile("file", fp.Name)
		require.NoError(t, err)
		_, err = part.Write(fp.Content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupMultipartTest creates a test server with storage configured, a user, a
// project, and a skill ready for multipart uploads.
func setupMultipartTest(t *testing.T) (srv *Server, s store.Store, alice *store.User, skill *store.Skill) {
	t.Helper()
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)
	skill = createTestSkill(t, s, "mp-skill-"+api.NewUUID()[:8], store.SkillScopeProject, project.ID, alice.ID)
	return srv, s, alice, skill
}

// TestMultipartPublish_HappyPath verifies that a well-formed multipart upload
// creates a published version with correct file manifest and content hash.
func TestMultipartPublish_HappyPath(t *testing.T) {
	srv, s, alice, skill := setupMultipartTest(t)

	files := map[string][]byte{
		"SKILL.md":  []byte("---\nname: test\n---\n# Test Skill"),
		"helper.sh": []byte("#!/bin/bash\necho hello"),
	}
	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "1.0.0"},
		files,
	)

	assert.Equal(t, http.StatusCreated, rec.Code, "expected 201; body: %s", rec.Body.String())

	var resp PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "1.0.0", resp.Version.Version)
	assert.Equal(t, store.SkillVersionStatusPublished, resp.Version.Status)
	assert.Len(t, resp.Version.Files, 2)
	assert.Empty(t, resp.UploadURLs, "multipart path should not return upload URLs")
	assert.NotEmpty(t, resp.Version.ContentHash, "content hash should be computed")

	// Verify each file has a valid hash and size.
	for _, f := range resp.Version.Files {
		assert.NotEmpty(t, f.Hash, "file %s should have a hash", f.Path)
		assert.Greater(t, f.Size, int64(0), "file %s should have a positive size", f.Path)
	}

	// Verify the version is persisted and published.
	sv, err := s.GetSkillVersionByNumber(context.Background(), skill.ID, "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, store.SkillVersionStatusPublished, sv.Status)
	assert.Len(t, sv.Files, 2)
}

// TestMultipartPublish_MissingSKILLMD verifies that omitting SKILL.md returns 400.
func TestMultipartPublish_MissingSKILLMD(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	files := map[string][]byte{
		"helper.sh": []byte("#!/bin/bash\necho hello"),
	}
	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "1.0.0"},
		files,
	)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "SKILL.md")
}

// TestMultipartPublish_InvalidSemver verifies that a bad version string returns 400.
func TestMultipartPublish_InvalidSemver(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	files := map[string][]byte{
		"SKILL.md": []byte("---\nname: test\n---\n# Test"),
	}
	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "bad"},
		files,
	)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid semver")
}

// TestMultipartPublish_DuplicateVersion verifies that re-publishing an already
// published version returns 409 Conflict.
func TestMultipartPublish_DuplicateVersion(t *testing.T) {
	srv, s, alice, skill := setupMultipartTest(t)

	// Pre-create a published version.
	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skill.ID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))

	files := map[string][]byte{
		"SKILL.md": []byte("---\nname: test\n---\n# Test"),
	}
	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "1.0.0"},
		files,
	)

	assert.Equal(t, http.StatusConflict, rec.Code, "expected 409; body: %s", rec.Body.String())
}

// TestMultipartPublish_PathTraversal verifies that filenames containing ".."
// are rejected. Go's multipart parser (since Go 1.22) sanitizes most path
// traversal patterns (e.g. "../evil.txt" -> "evil.txt") via filepath.Base, but
// the bare filename ".." passes through. The server-side check provides
// defense-in-depth against future parser changes or non-Go clients.
func TestMultipartPublish_PathTraversal(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	// Go's multipart parser keeps ".." as a bare filename, so we use a raw
	// Content-Disposition header to submit it.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("version", "1.0.0"))

	// Write SKILL.md normally.
	part, err := writer.CreateFormFile("file", "SKILL.md")
	require.NoError(t, err)
	_, err = part.Write([]byte("---\nname: test\n---\n# Test"))
	require.NoError(t, err)

	// Write a file whose name is literally ".." — Go's parser does not
	// sanitize this.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename=".."`)
	h.Set("Content-Type", "application/octet-stream")
	evilPart, err := writer.CreatePart(h)
	require.NoError(t, err)
	_, err = evilPart.Write([]byte("pwned"))
	require.NoError(t, err)

	require.NoError(t, writer.Close())

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		alice.ID, alice.Email, alice.DisplayName, alice.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/"+skill.ID+"/versions", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "path traversal")
}

// TestMultipartPublish_DuplicateFilenames verifies that two files with the same
// name are rejected.
func TestMultipartPublish_DuplicateFilenames(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	parts := []struct {
		Name    string
		Content []byte
	}{
		{Name: "SKILL.md", Content: []byte("---\nname: test\n---\n# First")},
		{Name: "SKILL.md", Content: []byte("---\nname: test\n---\n# Second")},
	}

	rec := doMultipartRequestAsUserOrdered(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		"1.0.0",
		parts,
	)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "duplicate filename")
}

// TestMultipartPublish_TooManyFiles verifies that uploading more than 50 files
// returns 400.
func TestMultipartPublish_TooManyFiles(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	parts := make([]struct {
		Name    string
		Content []byte
	}, 51)
	parts[0] = struct {
		Name    string
		Content []byte
	}{Name: "SKILL.md", Content: []byte("---\nname: test\n---\n# Test")}
	for i := 1; i < 51; i++ {
		parts[i] = struct {
			Name    string
			Content []byte
		}{Name: fmt.Sprintf("file-%d.txt", i), Content: []byte("content")}
	}

	rec := doMultipartRequestAsUserOrdered(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		"1.0.0",
		parts,
	)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "too many files")
}

// TestMultipartPublish_JSONPathStillWorks verifies that a JSON Content-Type
// request continues to use the existing two-phase flow.
func TestMultipartPublish_JSONPathStillWorks(t *testing.T) {
	srv, _, alice, skill := setupMultipartTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		PublishVersionRequest{
			Version: "1.0.0",
			Files: []FileUploadRequest{
				{Path: "SKILL.md", Size: 100},
			},
		},
	)

	assert.Equal(t, http.StatusCreated, rec.Code, "expected 201; body: %s", rec.Body.String())

	var resp PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "1.0.0", resp.Version.Version)
	assert.Equal(t, store.SkillVersionStatusDraft, resp.Version.Status,
		"JSON path should create a draft, not immediately publish")
}
