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

// The preselectRecommendedChoices preference (ptone/scion#7842) is a
// boolean key of the per-key preferences PATCH merge, returned by
// /api/v1/auth/me. Off (absent) by default.

func TestUpdateUser_Preferences_PreselectRecommendedChoices(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	devUser := getDevUser(t, srv, s)

	devUser.Preferences = &store.UserPreferences{Timezone: "Asia/Tokyo"}
	require.NoError(t, s.UpdateUser(ctx, devUser))

	patch := func(v any) {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
			map[string]any{"preferences": map[string]any{"preselectRecommendedChoices": v}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	get := func() *store.UserPreferences {
		t.Helper()
		u, err := s.GetUser(ctx, devUser.ID)
		require.NoError(t, err)
		require.NotNil(t, u.Preferences)
		return u.Preferences
	}

	patch(true)
	p := get()
	assert.True(t, p.PreselectRecommendedChoices)
	assert.Equal(t, "Asia/Tokyo", p.Timezone, "the per-key merge keeps other preferences")

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/me", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me struct {
		Preferences map[string]any `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, true, me.Preferences["preselectRecommendedChoices"])

	patch(false)
	assert.False(t, get().PreselectRecommendedChoices)

	patch(true)
	patch(nil)
	assert.False(t, get().PreselectRecommendedChoices, "null clears the preference")

	// A timezone PATCH leaves the boolean alone.
	patch(true)
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]any{"preferences": map[string]any{"timezone": "Europe/Paris"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, get().PreselectRecommendedChoices)
}

func TestUpdateUser_Preferences_NonBoolPreselectRejected(t *testing.T) {
	srv, s := testServer(t)
	devUser := getDevUser(t, srv, s)

	for _, v := range []any{"true", 1, map[string]any{}} {
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
			map[string]any{"preferences": map[string]any{"preselectRecommendedChoices": v}})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", v, rec.Body.String())
	}
}
