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
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readUpdateProjectSelectors are project read and update plus the agent
// selectors: the "project read/write" token the issue text describes.
var readUpdateProjectSelectors = []string{
	"project:read", "project:update",
	"agent:create", "agent:read", "agent:list", "agent:lifecycle", "agent:delete", "agent:message", "agent:attach",
}

// recordedTokenSelectors7068 are the selectors of the tokens actually
// inspected for ptone/scion#7068 (user_access_tokens on the reporting hub):
// project:manage and the agent selectors, with no project:read and no
// project:list.
var recordedTokenSelectors7068 = []string{
	"agent:create", "agent:delete", "agent:lifecycle", "agent:list", "agent:message", "agent:read", "project:manage",
}

// getProjectByID fetches /api/v1/projects/<id> with a token and returns the
// status and, on 200, the decoded project ID.
func getProjectByID(t *testing.T, f *inboxFixture, key, projectID string) (int, string) {
	t.Helper()
	rec := f.call(t, key, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec.Code, body.ID
}

// TestProjectTokenGet_OwnProjectOnly pins GET /api/v1/projects/<uuid> for a
// project-boundary user access token (ptone/scion#7068): a token with
// project:read reads its own project and nothing else. A token for project
// A gets 404 on project B even when its holder can read B, a token without
// project:read gets 404 on its own project, and a holder who loses project
// access loses the read.
func TestProjectTokenGet_OwnProjectOnly(t *testing.T) {
	f := newInboxFixture(t)

	t.Run("member token reads its project", func(t *testing.T) {
		code, id := getProjectByID(t, f, f.mint(t, f.member, projectBoundary(f.projA), "project:read"), f.projA)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, f.projA, id)
	})
	t.Run("read/update selectors read the project", func(t *testing.T) {
		code, id := getProjectByID(t, f, f.mint(t, f.admin, projectBoundary(f.projA), readUpdateProjectSelectors...), f.projA)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, f.projA, id)
	})
	t.Run("recorded #7068 token neither reads nor lists the project", func(t *testing.T) {
		// project:manage is not a read/write alias: without project:read
		// the token gets 404 on its own project, and without project:list
		// the list is empty (the list rule is also pinned by
		// TestProjectListToken_RequiresProjectListSelector).
		key := f.mint(t, f.admin, projectBoundary(f.projA), recordedTokenSelectors7068...)
		code, _ := getProjectByID(t, f, key, f.projA)
		assert.Equal(t, http.StatusNotFound, code)
		rec := f.call(t, key, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var list struct {
			Projects   []json.RawMessage `json:"projects"`
			TotalCount int               `json:"totalCount"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list), rec.Body.String())
		assert.Empty(t, list.Projects)
		assert.Equal(t, 0, list.TotalCount)
	})
	t.Run("super-admin without membership reads its project", func(t *testing.T) {
		admin := uuid.NewString()
		createTestUserWithRole(t, f.s, admin, admin+"@test.com", "admin", store.SystemRoleSuperAdmin)
		ensureHubMembership(context.Background(), f.s, admin)
		code, id := getProjectByID(t, f, f.mint(t, admin, projectBoundary(f.projA), "project:read"), f.projA)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, f.projA, id)
	})
	t.Run("token for A gets nothing on B", func(t *testing.T) {
		// f.admin owns both projects: only the boundary keeps B out.
		code, _ := getProjectByID(t, f, f.mint(t, f.admin, projectBoundary(f.projA), "project:read"), f.projB)
		assert.Equal(t, http.StatusNotFound, code)
		// The member cannot read B at all; the admin case above is the
		// boundary pin, this one checks the plain no-access case.
		code, _ = getProjectByID(t, f, f.mint(t, f.member, projectBoundary(f.projA), "project:read"), f.projB)
		assert.Equal(t, http.StatusNotFound, code)
	})
	t.Run("token without project:read gets nothing", func(t *testing.T) {
		code, _ := getProjectByID(t, f, f.mint(t, f.admin, projectBoundary(f.projA), "agent:read", "agent:list"), f.projA)
		assert.Equal(t, http.StatusNotFound, code)
	})
	t.Run("removed member loses the read", func(t *testing.T) {
		key := f.mint(t, f.member, projectBoundary(f.projA), "project:read")
		code, _ := getProjectByID(t, f, key, f.projA)
		require.Equal(t, http.StatusOK, code)
		removeUserProjectBindings(t, f.s, f.member, f.projA)
		code, _ = getProjectByID(t, f, key, f.projA)
		assert.Equal(t, http.StatusNotFound, code)
	})
}
