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

// reportedProjectTokenSelectors are the selectors of the token in
// ptone/scion#7068: project read and update plus the agent selectors.
var reportedProjectTokenSelectors = []string{
	"project:read", "project:update",
	"agent:create", "agent:read", "agent:list", "agent:lifecycle", "agent:delete", "agent:message", "agent:attach",
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
	t.Run("reported selectors read the project", func(t *testing.T) {
		code, id := getProjectByID(t, f, f.mint(t, f.admin, projectBoundary(f.projA), reportedProjectTokenSelectors...), f.projA)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, f.projA, id)
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
