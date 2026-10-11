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

package hub

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inviteFlowStore is a minimal in-memory store for testing the auth gate
// changes related to the invite flow. It implements only the methods
// used by checkUserAuthorized and provisionUser.
type inviteFlowStore struct {
	store.Store
	users map[string]*store.User
}

func newInviteFlowStore() *inviteFlowStore {
	return &inviteFlowStore{users: make(map[string]*store.User)}
}

func (s *inviteFlowStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	for _, u := range s.users {
		if u.Email == email {
			return u, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *inviteFlowStore) CreateUser(_ context.Context, user *store.User) error {
	s.users[user.ID] = user
	return nil
}

func (s *inviteFlowStore) UpdateUser(_ context.Context, user *store.User) error {
	s.users[user.ID] = user
	return nil
}

func (s *inviteFlowStore) IsUserInvitedOrActive(_ context.Context, email string) (bool, error) {
	for _, u := range s.users {
		if u.Email == email && (u.Status == store.UserStatusInvited || u.Status == store.UserStatusActive) {
			return true, nil
		}
	}
	return false, nil
}

func (s *inviteFlowStore) IsEmailAllowListed(_ context.Context, _ string) (bool, error) {
	return false, nil // should not be called in new code path
}

func (s *inviteFlowStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, store.ErrNotFound
}

func (s *inviteFlowStore) AddGroupMember(_ context.Context, _ *store.GroupMember) error {
	return nil
}

// ============================================================================
// checkUserAuthorized tests
// ============================================================================

func TestCheckUserAuthorized_InviteOnly_InvitedUser(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{
		ID:     "u1",
		Email:  "invited@example.com",
		Status: store.UserStatusInvited,
	}

	result := checkUserAuthorized(context.Background(), "invited@example.com", nil, nil, "invite_only", st)
	assert.True(t, result, "invited user should be authorized in invite_only mode")
}

func TestCheckUserAuthorized_InviteOnly_ActiveUser(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{
		ID:     "u1",
		Email:  "active@example.com",
		Status: store.UserStatusActive,
	}

	result := checkUserAuthorized(context.Background(), "active@example.com", nil, nil, "invite_only", st)
	assert.True(t, result, "active user should be authorized in invite_only mode")
}

func TestCheckUserAuthorized_InviteOnly_SuspendedUser(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{
		ID:     "u1",
		Email:  "suspended@example.com",
		Status: store.UserStatusSuspended,
	}

	result := checkUserAuthorized(context.Background(), "suspended@example.com", nil, nil, "invite_only", st)
	assert.False(t, result, "suspended user should NOT be authorized in invite_only mode")
}

func TestCheckUserAuthorized_InviteOnly_NoUser(t *testing.T) {
	st := newInviteFlowStore()

	result := checkUserAuthorized(context.Background(), "unknown@example.com", nil, nil, "invite_only", st)
	assert.False(t, result, "nonexistent user should NOT be authorized in invite_only mode")
}

func TestCheckUserAuthorized_OpenMode(t *testing.T) {
	st := newInviteFlowStore()

	result := checkUserAuthorized(context.Background(), "anyone@example.com", nil, nil, "open", st)
	assert.True(t, result, "any user should be authorized in open mode")
}

func TestCheckUserAuthorized_DomainRestricted_Allowed(t *testing.T) {
	st := newInviteFlowStore()

	result := checkUserAuthorized(context.Background(), "user@allowed.com", []string{"allowed.com"}, nil, "domain_restricted", st)
	assert.True(t, result, "user in authorized domain should be authorized")
}

func TestCheckUserAuthorized_DomainRestricted_Blocked(t *testing.T) {
	st := newInviteFlowStore()

	result := checkUserAuthorized(context.Background(), "user@blocked.com", []string{"allowed.com"}, nil, "domain_restricted", st)
	assert.False(t, result, "user NOT in authorized domain should be rejected")
}

func TestCheckUserAuthorized_AdminBypass(t *testing.T) {
	st := newInviteFlowStore()

	result := checkUserAuthorized(context.Background(), "admin@example.com", nil, []string{"admin@example.com"}, "invite_only", st)
	assert.True(t, result, "admin email should bypass all checks")
}

// ============================================================================
// provisionUser invited→active transition tests
// ============================================================================

func TestProvisionUser_InvitedToActive(t *testing.T) {
	st := newInviteFlowStore()
	invitedBy := "admin@co.com"
	st.users["u1"] = &store.User{
		ID:        "u1",
		Email:     "invited@example.com",
		Status:    store.UserStatusInvited,
		Role:      store.UserRoleMember,
		InvitedBy: &invitedBy,
		Created:   time.Now(),
	}

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config: ServerConfig{
			UserAccessMode: "invite_only",
		},
	}

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{
		Email:       "invited@example.com",
		DisplayName: "Alice Smith",
		AvatarURL:   "https://example.com/alice.png",
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, store.UserStatusActive, user.Status, "user should transition to active")
	assert.Equal(t, "Alice Smith", user.DisplayName, "display name should be populated")
	assert.Equal(t, "https://example.com/alice.png", user.AvatarURL, "avatar should be populated")
	// InvitedBy provenance should be preserved.
	require.NotNil(t, user.InvitedBy)
	assert.Equal(t, "admin@co.com", *user.InvitedBy)
}

func TestProvisionUser_SuspendedUser_Rejected(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{
		ID:     "u1",
		Email:  "suspended@example.com",
		Status: store.UserStatusSuspended,
		Role:   store.UserRoleMember,
	}

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config: ServerConfig{
			UserAccessMode: "open", // suspended rejection is independent of access mode
		},
	}

	_, err := srv.provisionUser(context.Background(), &ExternalUserInfo{
		Email: "suspended@example.com",
	})
	assert.ErrorIs(t, err, ErrUserSuspended)
}

func TestProvisionUser_NewUser_OpenMode(t *testing.T) {
	st := newInviteFlowStore()

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config: ServerConfig{
			UserAccessMode: "open",
		},
	}

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{
		Email:       "newuser@example.com",
		DisplayName: "New User",
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, store.UserStatusActive, user.Status, "new user in open mode should be active")
	assert.Equal(t, "New User", user.DisplayName)
}

// Verify that existing behavior is unchanged: in open mode, provisionUser
// creates a new user with status=active.
func TestProvisionUser_ExistingBehavior_ActiveUserLogin(t *testing.T) {
	st := newInviteFlowStore()
	st.users["u1"] = &store.User{
		ID:          "u1",
		Email:       "existing@example.com",
		DisplayName: "Existing",
		Status:      store.UserStatusActive,
		Role:        store.UserRoleMember,
		Created:     time.Now(),
	}

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config: ServerConfig{
			UserAccessMode: "open",
		},
	}

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{
		Email:       "existing@example.com",
		DisplayName: "Existing Updated",
	})
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, user.Status)
	// Display name should NOT be overwritten for active users (only backfill empty).
	assert.Equal(t, "Existing", user.DisplayName)
}

// Test that provisionUser sets a UUID-style ID for new users.
func TestProvisionUser_NewUser_HasID(t *testing.T) {
	st := newInviteFlowStore()

	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		config: ServerConfig{
			UserAccessMode: "open",
		},
	}

	user, err := srv.provisionUser(context.Background(), &ExternalUserInfo{
		Email: "brandnew@example.com",
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	// ID should be a valid UUID.
	_, err = uuid.Parse(user.ID)
	assert.NoError(t, err, "new user ID should be a valid UUID")
}
