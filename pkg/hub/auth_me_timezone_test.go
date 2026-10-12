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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// erroringGetUserStore wraps a store.Store and makes GetUser return a fixed
// error unconditionally, for testing that /auth/me degrades to its
// session/token fields with no preferences on a store error rather than
// failing the request (review round 1, R1-3). Every other method delegates
// to the embedded store.
type erroringGetUserStore struct {
	store.Store
	err error
}

func (e *erroringGetUserStore) GetUser(_ context.Context, _ string) (*store.User, error) {
	return nil, e.err
}

// TestAPIAuthMe_IncludesPreferencesTimezone verifies that GET
// /api/v1/auth/me (the Hub API, used for CLI/API parity per the design)
// includes preferences.timezone, reading it live from the store (tz-refactor
// task 10, AC6).
func TestAPIAuthMe_IncludesPreferencesTimezone(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Kathmandu"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/me", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp UserResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Preferences)
	assert.Equal(t, "Asia/Kathmandu", resp.Preferences.Timezone)
}

// TestWebAuthMe_IncludesPreferencesTimezone verifies that GET /auth/me (the
// web server's session endpoint, loaded by the SPA at start) includes
// preferences.timezone, read live from the store on every request rather
// than cached on the session (tz-refactor task 10, AC6).
func TestWebAuthMe_IncludesPreferencesTimezone(t *testing.T) {
	ws := newDevAuthWebServer(t)

	devStore, ok := ws.store.(*proxyAuthStore)
	require.True(t, ok, "newDevAuthWebServer installs a *proxyAuthStore")
	devStore.users[DevUserID].Preferences = &store.UserPreferences{Timezone: "Asia/Kathmandu"}

	handler := ws.Handler()
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Preferences *store.UserPreferences `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Preferences)
	assert.Equal(t, "Asia/Kathmandu", resp.Preferences.Timezone)

	// A PATCH from another tab/device must be visible on the very next load,
	// with no session-level caching of preferences.
	devStore.users[DevUserID].Preferences = &store.UserPreferences{Timezone: "America/New_York"}
	req2 := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	for _, c := range rec.Result().Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	var resp2 struct {
		Preferences *store.UserPreferences `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.NotNil(t, resp2.Preferences)
	assert.Equal(t, "America/New_York", resp2.Preferences.Timezone)
}

// TestWebAuthMe_SessionCookieFallback_IncludesPreferencesTimezone covers the
// second of handleAuthMe's two return paths: no context-user value (so
// getWebSessionUser returns nil), falling back to loading the session
// directly via ws.sessionStore.Get. Calling ws.handleAuthMe directly, instead
// of going through ws.Handler(), is what reaches this branch: the full
// handler chain's sessionAuthMiddleware would otherwise populate the
// context-user value first (review round 1, R1-3).
func TestWebAuthMe_SessionCookieFallback_IncludesPreferencesTimezone(t *testing.T) {
	ws := newDevAuthWebServer(t)
	devStore, ok := ws.store.(*proxyAuthStore)
	require.True(t, ok, "newDevAuthWebServer installs a *proxyAuthStore")
	devStore.users[DevUserID].Preferences = &store.UserPreferences{Timezone: "Europe/Paris"}

	cookies := loginSession(t, ws, DevUserID, "dev@localhost", "admin")
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.handleAuthMe(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		UserID      string                 `json:"id"`
		Preferences *store.UserPreferences `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, DevUserID, resp.UserID)
	require.NotNil(t, resp.Preferences)
	assert.Equal(t, "Europe/Paris", resp.Preferences.Timezone)
}

// TestWebAuthMe_StoreErrorDegradesToSessionFieldsOnly verifies that a store
// error on the live preferences read degrades to the session fields alone
// (200, no preferences), rather than failing the /auth/me request (review
// round 1, R1-3). Uses the session-cookie fallback path (see above) so the
// error is exercised with no other middleware-level store call in the way.
func TestWebAuthMe_StoreErrorDegradesToSessionFieldsOnly(t *testing.T) {
	ws := newDevAuthWebServer(t)
	devStore, ok := ws.store.(*proxyAuthStore)
	require.True(t, ok, "newDevAuthWebServer installs a *proxyAuthStore")
	devStore.users[DevUserID].Preferences = &store.UserPreferences{Timezone: "Europe/Paris"}

	cookies := loginSession(t, ws, DevUserID, "dev@localhost", "admin")
	ws.store = &erroringGetUserStore{Store: devStore, err: errors.New("boom")}

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.handleAuthMe(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		UserID      string                 `json:"id"`
		Email       string                 `json:"email"`
		Preferences *store.UserPreferences `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, DevUserID, resp.UserID)
	assert.Equal(t, "dev@localhost", resp.Email)
	assert.Nil(t, resp.Preferences, "a store error must degrade to no preferences, not fail the request")
}

// TestAPIAuthMe_StoreErrorDegradesToNoPreferences mirrors the web test above
// for the Hub API endpoint (review round 1, R1-3).
func TestAPIAuthMe_StoreErrorDegradesToNoPreferences(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)
	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	srv.store = &erroringGetUserStore{Store: s, err: errors.New("boom")}

	identity := NewAuthenticatedUser(devUser.ID, devUser.Email, devUser.DisplayName, devUser.Role, "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	srv.handleAuthMe(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp UserResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, devUser.ID, resp.ID)
	assert.Equal(t, devUser.Email, resp.Email)
	assert.Nil(t, resp.Preferences, "a store error must degrade to no preferences, not fail the request")
}
