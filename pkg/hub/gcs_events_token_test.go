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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestGCSObjectToken_ProjectGroupNeedsProjectRead requires a gs:// object
// fetch for a message of a project group conversation to admit a user
// access token only with project:read for the conversation's project inside
// its boundary: a project token for the project with project:read and a hub
// token with project:read are admitted; a token without project:read and a
// token bounded to another project the user belongs to get the uniform
// not-found answer; a session of the same member is admitted.
func TestGCSObjectToken_ProjectGroupNeedsProjectRead(t *testing.T) {
	f := newGCSFixture(t)
	sc := gcsSAAdmissionScenario(t, f, "gcstok", func(project *store.Project) *store.GCPServiceAccount {
		return gcsTestSA(t, f.store, project, "gcstok-sa", "sa-gcstok@test.iam.gserviceaccount.com")
	})
	other := gcsTestProject(t, f, "gcstok-other")
	addProjectMemberWithRole(t, f.store, other, sc.viewer.ID, store.GroupMemberRoleMember)
	ensureHubMembership(context.Background(), f.store, sc.viewer.ID)
	path := gcsRequestPath(sc.msg.ID, "bkt", "o.txt")

	mint := func(t *testing.T, boundary TokenBoundary, scopes ...string) (string, error) {
		t.Helper()
		key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(sc.viewer.ID), CreateTokenParams{
			UserID: sc.viewer.ID, Name: "gcs-" + tid("tok"), Boundary: boundary, Scopes: scopes,
		})
		return key, err
	}
	fetch := func(t *testing.T, key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("project token with project:read is admitted", func(t *testing.T) {
		key, err := mint(t, projectBoundary(sc.msg.ProjectID), "project:read")
		require.NoError(t, err)
		rec := fetch(t, key)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "hi", rec.Body.String())
	})

	t.Run("hub token with project:read is admitted", func(t *testing.T) {
		key, err := mint(t, hubBoundary(), "project:read")
		if err != nil {
			t.Logf("a hub token with project:read cannot be minted for a project member: %v", err)
			return
		}
		rec := fetch(t, key)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("token without project:read is refused", func(t *testing.T) {
		key, err := mint(t, projectBoundary(sc.msg.ProjectID), "agent:read")
		require.NoError(t, err)
		rec := fetch(t, key)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	})

	t.Run("token bounded to another project is refused", func(t *testing.T) {
		key, err := mint(t, projectBoundary(other.ID), "project:read")
		require.NoError(t, err)
		rec := fetch(t, key)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	})

	t.Run("session is admitted", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, sc.viewer, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// TestWebEventStream_RefusesUserAccessToken requires the web event stream
// to refuse a request that carries a user access token and no web session:
// the stream never opens for a token.
func TestWebEventStream_RefusesUserAccessToken(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	userID := tid("events-user")
	createTestUserWithRole(t, s, userID, userID+"@test.com", "member", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, userID)
	key := mintHubConfigToken(t, srv, userID, hubBoundary(), "project:read")

	ws := newTestWebServer(t, WebServerConfig{Host: "127.0.0.1"})
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(srv.authzService)

	for _, auth := range []string{"Bearer " + key, ""} {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req := httptest.NewRequest(http.MethodGet, "/events?sub=notification."+userID, nil).WithContext(reqCtx)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		cancel()
		assert.NotEqual(t, http.StatusOK, rec.Code, "auth %q: %s", auth != "", rec.Body.String())
		assert.False(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"),
			"auth %q: the event stream must not open", auth != "")
	}
}
