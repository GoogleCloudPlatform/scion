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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestDeleteUser_CascadesSkillInjections verifies that deleting a user removes
// all user-scoped skill injection entries, while leaving other scopes unaffected.
func TestDeleteUser_CascadesSkillInjections(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a test user to delete.
	user := &store.User{
		ID:          tid("user-cascade"),
		Email:       "cascade@example.com",
		DisplayName: "Cascade User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Create a second user whose skill injections should NOT be deleted.
	otherUser := &store.User{
		ID:          tid("user-other"),
		Email:       "other@example.com",
		DisplayName: "Other User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	// Add user-scoped skill injections for the target user.
	si1 := &store.SkillInjection{
		ID:       api.NewUUID(),
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  user.ID,
		SkillURI: "skill://org/skill-a@1.0.0",
	}
	si2 := &store.SkillInjection{
		ID:       api.NewUUID(),
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  user.ID,
		SkillURI: "skill://org/skill-b@1.0.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si1))
	require.NoError(t, s.AddSkillInjection(ctx, si2))

	// Add a skill injection for the other user (should be unaffected).
	siOther := &store.SkillInjection{
		ID:       api.NewUUID(),
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  otherUser.ID,
		SkillURI: "skill://org/skill-other@1.0.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, siOther))

	// Verify entries exist before deletion.
	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, user.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// Delete the user via API.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+user.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify user skill injections were cascade-deleted.
	list, err = s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, user.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "user skill injections should be cascade-deleted on user deletion")

	// Verify the other user's skill injections were NOT affected.
	otherList, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, otherUser.ID)
	require.NoError(t, err)
	assert.Len(t, otherList, 1, "other user's skill injections should be unaffected")
}
