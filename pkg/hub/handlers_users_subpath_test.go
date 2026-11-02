// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Routing of sub-paths under /api/v1/users/{id}/: only the bare ID,
// revoke-sessions and delete-blockers are resources, matching the catalog.
// Every other sub-path answers 404 and does not reach the user handlers.

// requireUserUnchanged asserts the stored user matches before: it
// exists, and the fields a GET, PATCH, DELETE or session revoke would read or
// change are the same.
func requireUserUnchanged(t *testing.T, s store.Store, before *store.User, msg string) {
	t.Helper()
	after, err := s.GetUser(context.Background(), before.ID)
	require.NoError(t, err, "%s: the user is not deleted", msg)
	assert.Equal(t, before.DisplayName, after.DisplayName, msg)
	assert.Equal(t, before.Role, after.Role, msg)
	assert.Equal(t, before.Status, after.Status, msg)
	assert.Equal(t, before.SessionGeneration, after.SessionGeneration, msg)
	assert.Equal(t, before.Preferences, after.Preferences, msg)
}

// An unknown sub-path under a user answers 404 for GET, PATCH and DELETE,
// and the user is not returned, changed or deleted.
func TestUserByID_UnknownSubPathNotFound(t *testing.T) {
	srv, s := testServer(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	before, err := s.GetUser(context.Background(), dave.ID)
	require.NoError(t, err)

	base := "/api/v1/users/" + dave.ID
	patch := map[string]interface{}{"displayName": "renamed"}
	for _, tc := range []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodGet, base + "/unknown", nil},
		{http.MethodGet, base + "/unknown/deeper", nil},
		{http.MethodGet, base + "/revoke-sessions/extra", nil},
		{http.MethodGet, base + "/revoke-sessions/", nil},
		{http.MethodPatch, base + "/unknown", patch},
		{http.MethodPatch, base + "/revoke-sessions/extra", patch},
		{http.MethodPatch, base + "/revoke-sessions/", patch},
		{http.MethodPatch, base + "/delete-blockers/extra", patch},
		{http.MethodDelete, base + "/unknown", nil},
		{http.MethodDelete, base + "/revoke-sessions/extra", nil},
		{http.MethodDelete, base + "/revoke-sessions/", nil},
		{http.MethodDelete, base + "/delete-blockers/extra", nil},
		{http.MethodPost, base + "/unknown", nil},
	} {
		msg := tc.method + " " + tc.path
		rec := doRequest(t, srv, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", msg, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), dave.Email, "%s must not return the user", msg)
		requireUserUnchanged(t, s, before, msg)
	}
}

// POST below revoke-sessions answers 404 and does not revoke the user's
// sessions.
func TestUserByID_RevokeSessionsTrailingSegmentNotFound(t *testing.T) {
	srv, s := testServer(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	before, err := s.GetUser(context.Background(), dave.ID)
	require.NoError(t, err)

	base := "/api/v1/users/" + dave.ID + "/revoke-sessions"
	for _, path := range []string{base + "/extra", base + "/", base + "/a/b"} {
		rec := doRequest(t, srv, http.MethodPost, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "POST %s: %s", path, rec.Body.String())
		requireUserUnchanged(t, s, before, "POST "+path)
	}

	// The bare route revokes.
	rec := doRequest(t, srv, http.MethodPost, base, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after, err := s.GetUser(context.Background(), dave.ID)
	require.NoError(t, err)
	assert.Greater(t, after.SessionGeneration, before.SessionGeneration)
}

// The known sub-routes answer as they did: the bare ID (with or without a
// trailing slash) for GET, PATCH and DELETE, revoke-sessions for POST and
// delete-blockers for GET, and 405 with the same Allow header for any other
// method on each.
func TestUserByID_KnownSubRoutesUnchanged(t *testing.T) {
	srv, s := testServer(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	base := "/api/v1/users/" + dave.ID

	for _, tc := range []struct {
		name      string
		method    string
		path      string
		body      interface{}
		wantCode  int
		wantAllow string
		wantUser  bool
	}{
		{"get user", http.MethodGet, base, nil, http.StatusOK, "", true},
		{"get user trailing slash", http.MethodGet, base + "/", nil, http.StatusOK, "", true},
		{"patch user", http.MethodPatch, base, map[string]interface{}{"displayName": "Dave"}, http.StatusOK, "", true},
		{"patch user trailing slash", http.MethodPatch, base + "/", map[string]interface{}{"displayName": "Dave B"}, http.StatusOK, "", true},
		{"user other method", http.MethodPut, base, nil, http.StatusMethodNotAllowed, "GET, PATCH, DELETE", false},
		{"revoke sessions", http.MethodPost, base + "/revoke-sessions", nil, http.StatusOK, "", false},
		{"revoke sessions other method", http.MethodGet, base + "/revoke-sessions", nil, http.StatusMethodNotAllowed, "POST", false},
		{"delete blockers", http.MethodGet, base + "/delete-blockers", nil, http.StatusOK, "", false},
		{"delete blockers other method", http.MethodPost, base + "/delete-blockers", nil, http.StatusMethodNotAllowed, "GET", false},
		{"delete blockers trailing segment", http.MethodGet, base + "/delete-blockers/extra", nil, http.StatusNotFound, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, tc.method, tc.path, tc.body)
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			assert.Equal(t, tc.wantAllow, rec.Header().Get("Allow"))
			if tc.wantUser {
				var got UserWithCapabilities
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
				assert.Equal(t, dave.ID, got.ID)
				assert.Equal(t, dave.Email, got.Email)
			}
		})
	}

	// DELETE on the bare ID, with and without a trailing slash, deletes the
	// user (last: each consumes a fixture).
	erin := newActiveMember(t, s, "user-erin", "erin@test.com")
	for _, tc := range []struct{ id, path string }{
		{erin.ID, "/api/v1/users/" + erin.ID + "/"},
		{dave.ID, base},
	} {
		rec := doRequest(t, srv, http.MethodDelete, tc.path, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, "DELETE %s: %s", tc.path, rec.Body.String())
		_, err := s.GetUser(context.Background(), tc.id)
		require.ErrorIs(t, err, store.ErrNotFound, "DELETE %s", tc.path)
	}
}
