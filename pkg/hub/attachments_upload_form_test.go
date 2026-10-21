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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Spooled upload forms are removed when the upload handler is done
// (ptone/scion#4596).

// spoolingUploadRequest returns a POST to the upload route of two files of
// 20 MiB each: together they exceed uploadFormMemory, so ParseMultipartForm
// spools the second to a temporary file.
func spoolingUploadRequest(t *testing.T, projectID string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if projectID != "" {
		require.NoError(t, mw.WriteField("project_id", projectID))
	}
	chunk := strings.Repeat("a", 20<<20)
	for _, name := range []string{"one.txt", "two.txt"} {
		part, err := mw.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="files"; filename="` + name + `"`},
			"Content-Type":        {"text/plain"},
		})
		require.NoError(t, err)
		_, err = part.Write([]byte(chunk))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// spoolDir points the temporary directory at a fresh one for this test and
// returns it.
func spoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	require.Equal(t, dir, os.TempDir())
	return dir
}

// requireNoSpooledForm fails when a multipart spool file is left in dir.
func requireNoSpooledForm(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), "multipart-"), "spooled upload form left behind: %s", e.Name())
	}
}

// TestAgentUpload_RemovesSpooledForm: an agent upload large enough to be
// spooled leaves no temporary file once the handler has answered.
func TestAgentUpload_RemovesSpooledForm(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dir := spoolDir(t)
	rec := f.agentRequest(t, f.aa, spoolingUploadRequest(t, ""))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireNoSpooledForm(t, dir)
}

// TestUserUpload_RemovesSpooledForm: the same for a user upload.
func TestUserUpload_RemovesSpooledForm(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	dir := spoolDir(t)
	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := spoolingUploadRequest(t, f.projA.ID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireNoSpooledForm(t, dir)
}
