package hub

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workspaceSecret is planted in a victim project's workspace. No response to a
// non-member may contain it.
const workspaceSecret = "PRIVATE-WORKSPACE-BYTES-do-not-serve-this"

// newVictimWorkspace creates a project with a populated workspace and returns
// the project plus its on-disk path.
func newVictimWorkspace(t *testing.T, srv *Server, name string) (projectID, wsPath string) {
	t.Helper()

	project, ws := createTestHubManagedProject(t, srv, name)
	require.NoError(t, os.MkdirAll(ws, 0o755))
	for _, f := range []string{"SECRET.md", "DOOMED.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(ws, f), []byte(workspaceSecret), 0o644))
	}
	return project.ID, ws
}

// TestProjectWorkspaceAuthz_NonMemberDenied is the regression test for the
// finding that every project workspace route was reachable by any
// authenticated user. Each case asserts three things: the request is refused,
// the response body does not carry workspace content, and the workspace on
// disk is untouched. A 403 that still leaked bytes, or still wrote the file,
// would pass a status-code-only assertion.
func TestProjectWorkspaceAuthz_NonMemberDenied(t *testing.T) {
	srv, _, _, bob, _ := setupTemplateAuthzTest(t)
	projectID, ws := newVictimWorkspace(t, srv, "Victim Workspace")
	base := "/api/v1/projects/" + projectID

	cases := []struct {
		name, method, url string
		body              any
	}{
		{"list files", http.MethodGet, base + "/workspace/files", nil},
		{"read file", http.MethodGet, base + "/workspace/files/SECRET.md", nil},
		{"archive", http.MethodGet, base + "/workspace/archive", nil},
		{"upload", http.MethodPost, base + "/workspace/files", nil},
		{"write file", http.MethodPut, base + "/workspace/files/OWNED.md",
			map[string]string{"content": "OWNED"}},
		{"delete file", http.MethodDelete, base + "/workspace/files/DOOMED.md", nil},
		{"pull", http.MethodPost, base + "/workspace/pull", nil},
		{"sync status", http.MethodGet, base + "/sync/status", nil},
		{"cache status", http.MethodGet, base + "/workspace/cache/status", nil},
		{"cache refresh", http.MethodPost, base + "/workspace/cache/refresh", nil},
		{"cache notify", http.MethodPost, base + "/workspace/cache/notify", nil},
		{"dav read", http.MethodGet, base + "/dav/SECRET.md", nil},
		{"dav propfind", "PROPFIND", base + "/dav/", nil},
		{"dav write", http.MethodPut, base + "/dav/DAV-OWNED.md", nil},
		{"dav delete", http.MethodDelete, base + "/dav/SECRET.md", nil},
		{"dav mkcol", "MKCOL", base + "/dav/newdir", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, bob, tc.method, tc.url, tc.body)

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"non-member must be refused; body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), workspaceSecret,
				"response to a non-member leaked workspace content")
		})
	}

	// The workspace must be exactly as it was left: nothing created, nothing
	// removed. Asserted once after the whole matrix so that a write admitted
	// by any single case is caught.
	for _, f := range []string{"SECRET.md", "DOOMED.md"} {
		assert.FileExists(t, filepath.Join(ws, f),
			"a non-member request deleted %s from another project's workspace", f)
	}
	for _, f := range []string{"OWNED.md", "DAV-OWNED.md", "newdir"} {
		_, err := os.Stat(filepath.Join(ws, f))
		assert.True(t, os.IsNotExist(err),
			"a non-member request created %s in another project's workspace", f)
	}
}

// TestProjectWorkspaceAuthz_LegacyGrovesAlias covers the second door onto the
// same dispatcher. Gating only the /projects prefix would leave this open, and
// nothing about the /projects tests would have noticed.
func TestProjectWorkspaceAuthz_LegacyGrovesAlias(t *testing.T) {
	srv, _, _, bob, _ := setupTemplateAuthzTest(t)
	projectID, _ := newVictimWorkspace(t, srv, "Victim Groves Alias")

	for _, url := range []string{
		"/api/v1/groves/" + projectID + "/workspace/files",
		"/api/v1/groves/" + projectID + "/workspace/files/SECRET.md",
		"/api/v1/groves/" + projectID + "/dav/SECRET.md",
	} {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "url: %s", url)
		assert.NotContains(t, rec.Body.String(), workspaceSecret, "url: %s", url)
	}
}

// TestProjectWorkspaceAuthz_MemberStillAllowed is the other half. A gate that
// refuses everyone would pass every assertion above, so the allow path is
// asserted explicitly: the identity that owns the project keeps full access.
func TestProjectWorkspaceAuthz_MemberStillAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, ws := createTestHubManagedProject(t, srv, "Owned Workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "SECRET.md"), []byte(workspaceSecret), 0o644))
	base := "/api/v1/projects/" + project.ID

	t.Run("list", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, base+"/workspace/files", nil)
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("read", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, base+"/workspace/files/SECRET.md", nil)
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), workspaceSecret,
			"owner must still be able to read their own workspace")
	})

	t.Run("write", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, base+"/workspace/files/MINE.md",
			map[string]string{"content": "mine"})
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.FileExists(t, filepath.Join(ws, "MINE.md"))
	})

	t.Run("dav read", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, base+"/dav/SECRET.md", nil)
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), workspaceSecret)
	})

	t.Run("dav write", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, base+"/dav/DAV-MINE.md", nil)
		assert.Contains(t, []int{http.StatusCreated, http.StatusNoContent, http.StatusOK}, rec.Code,
			"body: %s", rec.Body.String())
		assert.FileExists(t, filepath.Join(ws, "DAV-MINE.md"))
	})
}

// TestProjectWorkspaceAction pins the method-to-permission mapping, including
// the two classifications flagged for review. If PROPFIND or LOCK is
// reclassified as a read, this test is the place that records the decision.
func TestProjectWorkspaceAction(t *testing.T) {
	reads := []string{http.MethodGet, http.MethodHead, http.MethodOptions}
	writes := []string{
		http.MethodPut, http.MethodPost, http.MethodDelete,
		"MKCOL", "MOVE", "COPY", "PROPPATCH",
		"PROPFIND", // flagged for review: arguably a read
		"LOCK",     // flagged for review: arguably a read
		"WHATEVER", // unknown verb must default to the restrictive side
	}
	for _, m := range reads {
		assert.Equal(t, ActionRead, projectWorkspaceAction(m), "method %s", m)
	}
	for _, m := range writes {
		assert.Equal(t, ActionUpdate, projectWorkspaceAction(m), "method %s", m)
	}
}
