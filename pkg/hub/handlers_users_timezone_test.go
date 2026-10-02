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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests cover tz-refactor task 10, AC6: the user display-timezone preference
// round-trips through PATCH /api/v1/users/{id} with a per-key merge (not a
// whole-struct replace), validates against time.LoadLocation, and is visible
// only to the owning user or a hub admin.

// TestUpdateUser_Preferences_PerKeyMerge verifies that PATCHing one
// preference key (theme) does not clear an unrelated one (timezone) already
// on the record, and that sending only "timezone" sets it without touching
// theme.
func TestUpdateUser_Preferences_PerKeyMerge(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	// Seed an initial timezone directly via the store.
	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	// PATCH only {theme}. timezone must survive.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"theme": "dark"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "dark", updated.Preferences.Theme)
	assert.Equal(t, "Asia/Tokyo", updated.Preferences.Timezone, "PATCH {theme} must not clear timezone")

	// PATCH only {timezone}. theme must survive.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": "America/New_York"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err = s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "dark", updated.Preferences.Theme, "PATCH {timezone} must not clear theme")
	assert.Equal(t, "America/New_York", updated.Preferences.Timezone)
}

// TestUpdateUser_Preferences_TimezoneEmptyClears verifies that an explicit ""
// clears the stored timezone (Auto).
func TestUpdateUser_Preferences_TimezoneEmptyClears(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": ""}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "", updated.Preferences.Timezone)
}

// TestUpdateUser_Preferences_TimezoneNullClears verifies the PR body's other
// documented clearing form: a JSON `null` for timezone clears it the same
// way an explicit "" does (review round 2, R2-4).
func TestUpdateUser_Preferences_TimezoneNullClears(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": nil}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "", updated.Preferences.Timezone)
}

// TestUpdateUser_Preferences_TopLevelNullIsNoop verifies the PR body's other
// documented null behaviour: a top-level `"preferences": null` is a no-op
// (200, nothing cleared), unlike a sub-field null or "" (review round 2,
// R2-4). A future change that treated it as "clear everything" would
// silently break the stated contract without this test.
func TestUpdateUser_Preferences_TopLevelNullIsNoop(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	devUser.Preferences = &store.UserPreferences{Theme: "dark", Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": nil})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "dark", updated.Preferences.Theme, "top-level preferences:null must not clear theme")
	assert.Equal(t, "Asia/Tokyo", updated.Preferences.Timezone, "top-level preferences:null must not clear timezone")
}

// TestUpdateUser_Preferences_EmptyObjectIsNoop verifies that
// PATCH {"preferences": {}} is a true no-op: it must not create an empty
// store.UserPreferences record for a user that had none (Gemini review on
// GoogleCloudPlatform/scion#2241, G1 second half).
func TestUpdateUser_Preferences_EmptyObjectIsNoop(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)
	require.Nil(t, devUser.Preferences, "test setup: dev user must start with no preferences")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	assert.Nil(t, updated.Preferences, "an empty preferences object must not initialize a preferences record")
}

// TestUpdateUser_Preferences_UnknownKeysOnlyIsNoop verifies that a
// preferences PATCH containing only unrecognized keys (e.g. a casing typo)
// is a true no-op: the unknown key is silently ignored (contract, see the
// PR body), and the ignoring must not create an empty preferences record
// (Gemini review on GoogleCloudPlatform/scion#2241, G1 second half).
func TestUpdateUser_Preferences_UnknownKeysOnlyIsNoop(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)
	require.Nil(t, devUser.Preferences, "test setup: dev user must start with no preferences")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timeZone": "Asia/Tokyo"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	assert.Nil(t, updated.Preferences, "an all-unknown-key preferences object must not initialize a preferences record")
}

// TestUpdateUser_Preferences_EmptyObjectCrossUserNoAuthzRequired verifies
// that because {"preferences": {}} is a true no-op, a cross-user PATCH of it
// does not require user.update permission: it returns 200, not 403, and
// does not touch the target's stored preferences. This documents that the
// G1 no-op fix does not change cross-user authorization for an actual
// preference change (TestUpdateUser_Preferences_CrossUserPatchForbidden
// above still asserts 403 for a real value).
func TestUpdateUser_Preferences_EmptyObjectCrossUserNoAuthzRequired(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("prefs-noop-actor"),
		Email:       "prefs-noop-actor@example.com",
		DisplayName: "Prefs Noop Actor",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("prefs-noop-target"),
		Email:       "prefs-noop-target@example.com",
		DisplayName: "Prefs Noop Target",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{Timezone: "Asia/Tokyo"},
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]any{"preferences": map[string]any{}})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, target.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "Asia/Tokyo", updated.Preferences.Timezone,
		"a no-op preferences PATCH must not change the target's stored value")
}

// TestUpdateUser_Preferences_InvalidTimezoneRejected verifies that an
// unparseable IANA zone name is rejected with 400 and not persisted.
func TestUpdateUser_Preferences_InvalidTimezoneRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": "Not/AZone"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, devUser.ID)
	require.NoError(t, err)
	if updated.Preferences != nil {
		assert.Equal(t, "", updated.Preferences.Timezone, "invalid timezone must not be persisted")
	}
}

// TestUpdateUser_Preferences_LocalRejected verifies that "Local" is rejected,
// even though time.LoadLocation("Local") itself succeeds: "Local" names the
// server process's zone, not a portable one, and only "" (Auto) and real IANA
// names are accepted (design §3 A "Storage and API").
func TestUpdateUser_Preferences_LocalRejected(t *testing.T) {
	srv, s := testServer(t)
	devUser := getDevUser(t, srv, s)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": "Local"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestListUsers_PreferencesVisibility verifies that a member listing users
// does not see another user's preferences, but does see their own, and an
// admin sees everyone's.
func TestListUsers_PreferencesVisibility(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("member-viewer"),
		Email:       "member-viewer@example.com",
		DisplayName: "Member Viewer",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{Timezone: "Asia/Tokyo"},
	}
	require.NoError(t, s.CreateUser(ctx, member))

	other := &store.User{
		ID:          tid("member-other"),
		Email:       "member-other@example.com",
		DisplayName: "Member Other",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{Timezone: "Europe/Paris"},
	}
	require.NoError(t, s.CreateUser(ctx, other))

	// A plain member needs the hub-member role (user.read/user.list) to list
	// users at all; s.CreateUser alone does not grant it (that normally
	// happens at login).
	ensureHubMembership(ctx, s, member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/users", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListUsersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	foundSelf, foundOther := false, false
	for _, u := range resp.Users {
		switch u.ID {
		case member.ID:
			foundSelf = true
			require.NotNil(t, u.Preferences, "self must see own preferences")
			assert.Equal(t, "Asia/Tokyo", u.Preferences.Timezone)
		case other.ID:
			foundOther = true
			assert.Nil(t, u.Preferences, "member must not see another member's preferences")
		}
	}
	assert.True(t, foundSelf)
	assert.True(t, foundOther)

	// Admin (dev user, via doRequest) sees everyone's preferences.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	foundOtherAsAdmin := false
	for _, u := range resp.Users {
		if u.ID == other.ID {
			foundOtherAsAdmin = true
			require.NotNil(t, u.Preferences, "admin must see another member's preferences")
			assert.Equal(t, "Europe/Paris", u.Preferences.Timezone)
		}
	}
	assert.True(t, foundOtherAsAdmin)
}

// TestGetUser_PreferencesVisibility mirrors TestListUsers_PreferencesVisibility
// for the single-user GET endpoint.
func TestGetUser_PreferencesVisibility(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("get-member-viewer"),
		Email:       "get-member-viewer@example.com",
		DisplayName: "Get Member Viewer",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, member))

	other := &store.User{
		ID:          tid("get-member-other"),
		Email:       "get-member-other@example.com",
		DisplayName: "Get Member Other",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{Timezone: "Europe/Paris"},
	}
	require.NoError(t, s.CreateUser(ctx, other))
	ensureHubMembership(ctx, s, member.ID)

	// Member viewing another user: preferences absent.
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/users/"+other.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got UserWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Nil(t, got.Preferences, "member must not see another member's preferences")

	// Admin viewing another user: preferences present.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/users/"+other.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.Preferences, "admin must see another member's preferences")
	assert.Equal(t, "Europe/Paris", got.Preferences.Timezone)
}

// TestStripPreferencesForViewer_NilCapDoesNotPanic is a direct unit test of
// stripPreferencesForViewer with cap == nil (Gemini review on
// GoogleCloudPlatform/scion#2241, G2). Over HTTP, every route that reaches
// listUsers/getUser requires authentication (an unauthenticated request
// gets 401 from the auth middleware before the handler runs — see
// auth.go's "missing authorization header" gate), and
// ComputeCapabilities/ComputeCapabilitiesBatch never return a nil element
// for a non-nil identity, so a nil cap cannot reach this function on any
// currently live HTTP path. It is tested directly anyway: this codebase has
// a known history of a typed-nil identity slipping through context (see
// identity_typed_nil_test.go and commit "auth: treat a typed-nil identity
// as missing at classification and entry"), and the guard is a one-line,
// zero-risk defense against that same bug class reappearing here.
func TestStripPreferencesForViewer_NilCapDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	u := &store.User{
		ID:          "prefs-nilcap-user",
		Preferences: &store.UserPreferences{Timezone: "Europe/Paris"},
	}

	require.NotPanics(t, func() {
		stripPreferencesForViewer(ctx, u, nil)
	})
	assert.Nil(t, u.Preferences, "a nil cap and no identity in context must strip preferences, not panic")
}

// TestUpdateUser_Preferences_CrossUserPatchForbidden verifies the write side
// of the same visibility rule: a plain member PATCHing another user's
// preferences gets 403, and the target's stored value is unchanged (review
// round 1, R1-3).
func TestUpdateUser_Preferences_CrossUserPatchForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	actor := &store.User{
		ID:          tid("prefs-actor"),
		Email:       "prefs-actor@example.com",
		DisplayName: "Prefs Actor",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("prefs-target"),
		Email:       "prefs-target@example.com",
		DisplayName: "Prefs Target",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{Timezone: "Asia/Tokyo"},
	}
	require.NoError(t, s.CreateUser(ctx, target))

	rec := doRequestAsUser(t, srv, actor, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]any{"preferences": map[string]any{"timezone": "Europe/Paris"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	updated, err := s.GetUser(ctx, target.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Preferences)
	assert.Equal(t, "Asia/Tokyo", updated.Preferences.Timezone,
		"a forbidden cross-user PATCH must not change the target's stored value")
}

// TestUpdateUser_Preferences_NonStringTimezoneRejected verifies that a
// non-string JSON value for preferences.timezone is a 400, not a panic or a
// silently-ignored field (review round 1, R1-3).
func TestUpdateUser_Preferences_NonStringTimezoneRejected(t *testing.T) {
	srv, s := testServer(t)
	devUser := getDevUser(t, srv, s)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": 123}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestValidateUserTimezone covers the rejected-name denylist added in review
// round 1 (R1-1): time.LoadLocation accepts several zoneinfo entries that
// are not portable IANA zone names and that task 11's Intl-based formatters
// cannot render, so they must be rejected the same way "Local" already is.
func TestValidateUserTimezone(t *testing.T) {
	cases := []struct {
		name    string
		tz      string
		wantErr bool
	}{
		{"empty string is Auto", "", false},
		{"UTC", "UTC", false},
		{"IANA area/location zone", "US/Pacific", false},
		{"IANA Etc fixed-offset zone", "Etc/GMT+5", false},
		{"Local is rejected", "Local", true},
		{"localtime is rejected", "localtime", true},
		{"posixrules is rejected", "posixrules", true},
		{"Factory is rejected", "Factory", true},
		{"unknown zone is rejected", "Not/AZone", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUserTimezone(tc.tz)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
