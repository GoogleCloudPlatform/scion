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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newInviteFlowStore() *inviteFlowStore {
	return &inviteFlowStore{users: make(map[string]*store.User)}
}

// inviteFlowStore is a minimal in-memory store for testing the auth gate
// changes related to the invite flow. It implements only the methods
// used by checkUserAuthorized and provisionUser.
type inviteFlowStore struct {
	store.Store
	users map[string]*store.User
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
