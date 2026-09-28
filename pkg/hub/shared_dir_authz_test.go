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
			jsonBody: ProjectWorkspaceWriteRequest{Content: "other-user content"}},
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
		return doSharedDirUploadAsUser(t, srv, user, tc.method, path, map[string][]byte{"uploaded.txt": []byte("other-user upload")})
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

	project, workspacePath := createTestHubManagedProject(t, srv, "Target Project")
	addSharedDirToProject(t, srv, project.ID, "secrets")
	sdPath = resolveTestSharedDirPath(t, workspacePath, "secrets")
	require.NoError(t, os.MkdirAll(sdPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "secret.txt"), []byte("ORIGINAL SECRET"), 0o644))

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
			// original file is untouched, and nothing new was planted.
			data, err := os.ReadFile(filepath.Join(sdPath, "secret.txt"))
			require.NoError(t, err, "original file must still exist")
			assert.Equal(t, "ORIGINAL SECRET", string(data), "original file must be unmodified")
			assert.NoFileExists(t, filepath.Join(sdPath, "planted.txt"))
			assert.NoFileExists(t, filepath.Join(sdPath, "uploaded.txt"))
		})
	}
}

// sharedDirAuthzAllowStatus is the expected success status for each case in
// sharedDirAuthzCases when the caller is actually allowed through.
func sharedDirAuthzAllowStatus() map[string]int {
	return map[string]int{
		"list files":    http.StatusOK,
		"download file": http.StatusOK,
		"archive":       http.StatusOK,
		"upload file":   http.StatusOK,
		"write file":    http.StatusOK,
		"delete file":   http.StatusNoContent,
	}
}

// TestSharedDirRoutes_OwnerAllowed proves the gate is a gate and not a
// blanket deny: the project owner performing the same six operations still
// succeeds.
func TestSharedDirRoutes_OwnerAllowed(t *testing.T) {
	wantStatus := sharedDirAuthzAllowStatus()

	for _, tc := range sharedDirAuthzCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "Owner Project")

			addSharedDirToProject(t, srv, project.ID, "secrets")
			sdPath := resolveTestSharedDirPath(t, workspacePath, "secrets")
			require.NoError(t, os.MkdirAll(sdPath, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sdPath, "secret.txt"), []byte("ORIGINAL SECRET"), 0o644))

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
// token names a different project gets a 404, not a 403, for a target
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

// TestSharedDirRoutes_MemberReadOnlyOwnerFull pins the GET/HEAD→read,
// everything-else→update split itself, not just its endpoints. A non-member
// and the dev token both land on the same result whichever way that split is
// mutated (denied-regardless, allowed-regardless), so neither proves the
// mapping is right. A real project member sits between them: reads must
// succeed and writes must be refused (project-member has project:read but
// not project:update), while a real project owner succeeds on all six.
func TestSharedDirRoutes_MemberReadOnlyOwnerFull(t *testing.T) {
	allowStatus := sharedDirAuthzAllowStatus()

	for _, tc := range sharedDirAuthzCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, sdPath, _ := setupSharedDirAuthzFixture(t)
			base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets", project.ID)

			member := makeProjectMemberUser(t, s, project, tid("sdauthz-member-"+tc.name), "Member", store.GroupMemberRoleMember)
			isRead := tc.method == http.MethodGet

			memberRec := doSharedDirAuthzRequest(t, srv, member, base, tc)
			if isRead {
				assert.Equal(t, http.StatusOK, memberRec.Code, "member %s: body: %s", tc.name, memberRec.Body.String())
			} else {
				assert.Equal(t, http.StatusForbidden, memberRec.Code, "member %s: body: %s", tc.name, memberRec.Body.String())

				// As in the non-member case, a refusal must be provable on
				// disk, not just by status code.
				data, err := os.ReadFile(filepath.Join(sdPath, "secret.txt"))
				require.NoError(t, err, "original file must still exist")
				assert.Equal(t, "ORIGINAL SECRET", string(data), "original file must be unmodified")
				assert.NoFileExists(t, filepath.Join(sdPath, "planted.txt"))
				assert.NoFileExists(t, filepath.Join(sdPath, "uploaded.txt"))
			}

			owner := makeProjectMemberUser(t, s, project, tid("sdauthz-owner-"+tc.name), "Owner", store.GroupMemberRoleOwner)
			ownerRec := doSharedDirAuthzRequest(t, srv, owner, base, tc)
			assert.Equal(t, allowStatus[tc.name], ownerRec.Code, "owner %s: body: %s", tc.name, ownerRec.Body.String())
		})
	}
}

// TestSharedDirRoutes_NonMemberDeniedOnList covers the one route whose leaf
// no longer has an authorization check of its own: GET /shared-dirs (list)
// used to be gated redundantly by both the dispatcher and
// handleProjectSharedDirs's own copy of the same check; now only the
// dispatcher gates it, so this asserts that gate alone is still sufficient.
func TestSharedDirRoutes_NonMemberDeniedOnList(t *testing.T) {
	srv, _, project, _, nonMember := setupSharedDirAuthzFixture(t)

	rec := doRequestAsUser(t, srv, nonMember, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// TestSharedDirRoutes_InProjectAgentDeniedOnConfigMutation pins, end to end,
// that an agent cannot mutate a project's shared-dir configuration even from
// inside its own project. It is doubly denied today: the dispatcher requires
// project:update, which no agent scope maps to, and the leaf's UserIdentity
// check refuses agents outright regardless. Either layer denying is enough
// for the caller-visible behavior, but the config itself — not just the
// status code — must be provably unchanged.
func TestSharedDirRoutes_InProjectAgentDeniedOnConfigMutation(t *testing.T) {
	srv, s, project, _, _ := setupSharedDirAuthzFixture(t)
	ctx := context.Background()

	token, err := srv.GenerateAgentToken(tid("sdauthz-inproject-agent"), project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID),
		map[string]interface{}{"name": "nonmember-dir"}, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, "POST body: %s", rec.Body.String())

	rec = doRequestWithAgentToken(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets", project.ID), nil, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, "DELETE body: %s", rec.Body.String())

	reloaded, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	names := make([]string, 0, len(reloaded.SharedDirs))
	for _, d := range reloaded.SharedDirs {
		names = append(names, d.Name)
	}
	assert.Contains(t, names, "secrets", "existing shared-dir config must survive the denied delete")
	assert.NotContains(t, names, "nonmember-dir", "no shared dir should have been added by the denied post")
}

// TestSharedDirRoutes_ByNameMethodNotAllowed proves the by-name leaf's method
// check runs ahead of its own identity/authorization checks and ahead of the
// name lookup: a caller who is fully authorized past the dispatcher gate
// still gets 405, not 403, for a method the route does not support, and the
// response does not depend on whether the named shared dir exists.
func TestSharedDirRoutes_ByNameMethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Method Check Project")
	addSharedDirToProject(t, srv, project.ID, "secrets")

	base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/secrets", project.ID)
	rec := doRequest(t, srv, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "body: %s", rec.Body.String())

	// Same result for a name that does not exist: the method check runs
	// before the name lookup, so the response never varies with existence.
	missing := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/does-not-exist", project.ID)
	rec = doRequest(t, srv, http.MethodPut, missing, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "body: %s", rec.Body.String())
}

// TestSharedDirRoutes_ByNameMethodCheckStaysBehindDispatcherGate guards the
// leaf's method check against being hoisted ahead of the dispatcher's
// authorization/isolation gate: if it ever moved that far up, a cross-project
// agent would get 405 instead of 404 for a name it should know nothing about,
// re-opening project-existence disclosure. The check must stay behind that
// gate for every unsupported method, not just DELETE, and independent of
// whether the named directory exists.
func TestSharedDirRoutes_ByNameMethodCheckStaysBehindDispatcherGate(t *testing.T) {
	srv, _, project, _, nonMember := setupSharedDirAuthzFixture(t)
	other, _ := createTestHubManagedProject(t, srv, "Other Project")
	token, err := srv.GenerateAgentToken(tid("sdauthz-isolation-agent"), other.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	for _, name := range []string{"secrets", "does-not-exist"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			path := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/%s", project.ID, name)

			rec := doRequestAsUser(t, srv, nonMember, method, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "non-member %s %s: body: %s", method, name, rec.Body.String())

			rec = doRequestWithAgentToken(t, srv, method, path, nil, token)
			assert.Equal(t, http.StatusNotFound, rec.Code, "cross-project agent %s %s: body: %s", method, name, rec.Body.String())
		}
	}
}
