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

package entadapter

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestUserStore_PreferencesTimezone_RoundTrip verifies that
// preferences.timezone survives a store write and read, alongside the other
// preference fields, on whichever backend enttest.NewClient selects (SQLite by
// default; Postgres under `-tags integration` with SCION_TEST_POSTGRES_URL
// set). This is tz-refactor task 10's store half of AC6.
func TestUserStore_PreferencesTimezone_RoundTrip(t *testing.T) {
	ctx := context.Background()
	us := NewUserStore(newTestEntClient(t))

	id := uuid.NewString()
	require.NoError(t, us.CreateUser(ctx, &store.User{
		ID:          id,
		Email:       "tz-prefs@example.com",
		DisplayName: "TZ Prefs",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{
			Theme:    "dark",
			Timezone: "Asia/Tokyo",
		},
	}))

	got, err := us.GetUser(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.Preferences)
	require.Equal(t, "dark", got.Preferences.Theme)
	require.Equal(t, "Asia/Tokyo", got.Preferences.Timezone)

	// Update to a different zone, leaving other fields alone via a
	// store-level read-modify-write (the handler-level per-key merge is
	// tested separately in pkg/hub).
	got.Preferences.Timezone = "Asia/Kathmandu"
	require.NoError(t, us.UpdateUser(ctx, got))

	got2, err := us.GetUser(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got2.Preferences)
	require.Equal(t, "dark", got2.Preferences.Theme, "unrelated preference survives the update")
	require.Equal(t, "Asia/Kathmandu", got2.Preferences.Timezone)

	// Clearing to "" (Auto) round-trips as the empty string, not as absent.
	got2.Preferences.Timezone = ""
	require.NoError(t, us.UpdateUser(ctx, got2))

	got3, err := us.GetUser(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got3.Preferences)
	require.Equal(t, "", got3.Preferences.Timezone)
}
