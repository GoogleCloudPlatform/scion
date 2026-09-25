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
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedDirAuthzCase describes one of the six operations reachable under the
// project shared-dirs subtree, in a form that can be issued as either a
// non-member or a project owner.
type sharedDirAuthzCase struct {
	name        string
	method      string
	subPath     string // appended to ".../shared-dirs/secrets"
	isMultipart bool
	jsonBody    interface{}
}

func sharedDirAuthzCases() []sharedDirAuthzCase {
	return []sharedDirAuthzCase{
		{name: "list files", method: http.MethodGet, subPath: "/files"},
		{name: "download file", method: http.MethodGet, subPath: "/files/secret.txt"},
		{name: "archive", method: http.MethodGet, subPath: "/archive"},
		{name: "upload file", method: http.MethodPost, subPath: "/files", isMultipart: true},
		{name: "write file", method: http.MethodPut, subPath: "/files/planted.txt",
			jsonBody: ProjectWorkspaceWriteRequest{Content: "attacker content"}},
		{name: "delete file", method: http.MethodDelete, subPath: "/files/secret.txt"},
	}
}

// doSharedDirUploadAsUser issues a multipart file upload as a specific user,
// mirroring doMultipartRequest (which is always the dev user) and
// doRequestAsUser (which has no multipart support). Unlike
// doMultipartRequestAsUser (skill_multipart_test.go), the field name here is
// the file's relative path within the shared dir, matching what
// handleProjectWorkspaceUpload expects.
func doSharedDirUploadAsUser(t *testing.T, srv *Server, user *store.User, method, path string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for fieldName, content := range files {
		part, err := writer.CreateFormFile(fieldName, fieldName)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doSharedDirAuthzRequest issues the request for one sharedDirAuthzCase as the
// given user against base (e.g. ".../shared-dirs/secrets").
func doSharedDirAuthzRequest(t *testing.T, srv *Server, user *store.User, base string, tc sharedDirAuthzCase) *httptest.ResponseRecorder {
	t.Helper()
	path := base + tc.subPath
	if tc.isMultipart {
		return doSharedDirUploadAsUser(t, srv, user, tc.method, path, map[string][]byte{"uploaded.txt": []byte("attacker upload")})
	}
	return doRequestAsUser(t, srv, user, tc.method, path, tc.jsonBody)
}

// setupSharedDirAuthzFixture creates a hub-managed project with a "secrets"
// shared dir containing one file, plus a non-member user with no role
// binding on the project at all. Returns the server, store, project, the
// resolved on-disk shared-dir path, and the non-member user.
func setupSharedDirAuthzFixture(t *testing.T) (srv *Server, s store.Store, project *store.Project, sdPath string, nonMember *store.User) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project, workspacePath := createTestHubManagedProject(t, srv, "Victim Project")
	addSharedDirToProject(t, srv, project.ID, "secrets")
	sdPath = resolveTestSharedDirPath(t, workspacePath, "secrets")
	require.NoError(t, os.MkdirAll(sdPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "secret.txt"), []byte("VICTIM SECRET"), 0o644))

	nonMember = &store.User{
		ID: tid("sdauthz-nonmember"), Email: "nonmember@test.com", DisplayName: "Non Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, nonMember))

	return srv, s, project, sdPath, nonMember
}

// TestSharedDirRoutes_NonMemberDenied is the deny side of the gate: a user
// with no role binding of any kind on the project is refused every operation
// under the shared-dirs subtree, and — this is the part a status code alone
// cannot prove — none of those refused requests touch the filesystem.
func TestSharedDirRoutes_NonMemberDenied(t *testing.T) {
	for _, tc := range sharedDirAuthzCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, project, sdPath, nonMember := setupSharedDirAuthzFixture(t)
			base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets", project.ID)

			rec := doSharedDirAuthzRequest(t, srv, nonMember, base, tc)
			assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

			// The filesystem must be unchanged, not just the status code: the
			// victim's file is untouched, and nothing new was planted.
			data, err := os.ReadFile(filepath.Join(sdPath, "secret.txt"))
			require.NoError(t, err, "victim file must still exist")
			assert.Equal(t, "VICTIM SECRET", string(data), "victim file must be unmodified")
			assert.NoFileExists(t, filepath.Join(sdPath, "planted.txt"))
			assert.NoFileExists(t, filepath.Join(sdPath, "uploaded.txt"))
		})
	}
}

// TestSharedDirRoutes_OwnerAllowed proves the gate is a gate and not a
// blanket deny: the project owner performing the same six operations still
// succeeds.
func TestSharedDirRoutes_OwnerAllowed(t *testing.T) {
	wantStatus := map[string]int{
		"list files":    http.StatusOK,
		"download file": http.StatusOK,
		"archive":       http.StatusOK,
		"upload file":   http.StatusOK,
		"write file":    http.StatusOK,
		"delete file":   http.StatusNoContent,
	}

	for _, tc := range sharedDirAuthzCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "Owner Project")

			addSharedDirToProject(t, srv, project.ID, "secrets")
			sdPath := resolveTestSharedDirPath(t, workspacePath, "secrets")
			require.NoError(t, os.MkdirAll(sdPath, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sdPath, "secret.txt"), []byte("VICTIM SECRET"), 0o644))

			base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets", project.ID)

			var rec *httptest.ResponseRecorder
			if tc.isMultipart {
				rec = doMultipartRequest(t, srv, tc.method, base+tc.subPath, map[string][]byte{"uploaded.txt": []byte("owner upload")})
			} else {
				rec = doRequest(t, srv, tc.method, base+tc.subPath, tc.jsonBody)
			}
			assert.Equal(t, wantStatus[tc.name], rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// TestSharedDirRoutes_CrossProjectAgent404 proves the agent project-isolation
// check still runs ahead of the authorization check: an agent identity whose
// token names a different project gets a 404, not a 403, for a victim
// project it otherwise knows nothing about — a 403 would confirm the
// project's existence to a caller outside it.
func TestSharedDirRoutes_CrossProjectAgent404(t *testing.T) {
	srv, _, project, _, _ := setupSharedDirAuthzFixture(t)

	other, _ := createTestHubManagedProject(t, srv, "Other Project")

	token, err := srv.GenerateAgentToken(tid("sdauthz-cross-agent"), other.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets/files", project.ID), nil, token)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
}
