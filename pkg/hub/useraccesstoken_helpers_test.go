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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockUserStore implements store.UserStore for testing (minimal).
type mockUserStore struct {
	users map[string]*store.User
}

func (m *mockUserStore) GetUser(_ context.Context, id string) (*store.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}
func (m *mockUserStore) GetUsersByIDs(_ context.Context, ids []string) (map[string]*store.User, error) {
	out := make(map[string]*store.User, len(ids))
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}
func (m *mockUserStore) GetUserByEmail(context.Context, string) (*store.User, error) {
	return nil, store.ErrNotFound
}
func (m *mockUserStore) CreateUser(context.Context, *store.User) error { return nil }
func (m *mockUserStore) UpdateUser(context.Context, *store.User) error { return nil }
func (m *mockUserStore) ListUsers(context.Context, store.UserFilter, store.ListOptions) (*store.ListResult[store.User], error) {
	return nil, nil
}
func (m *mockUserStore) DeleteUser(context.Context, string) error                     { return nil }
func (m *mockUserStore) LockUserRow(context.Context, string, bool) error              { return nil }
func (m *mockUserStore) UpdateUserLastSeen(context.Context, string, time.Time) error  { return nil }
func (m *mockUserStore) UpdateUserLastLogin(context.Context, string, time.Time) error { return nil }
func (m *mockUserStore) IsUserInvitedOrActive(context.Context, string) (bool, error) {
	return false, nil
}
func (m *mockUserStore) IncrementSessionGeneration(context.Context, string) error { return nil }
func (m *mockUserStore) CreateTestFixtureUser(context.Context, *store.User) error { return nil }
func (m *mockUserStore) CountLiveTestFixtureUsers(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (m *mockUserStore) ListTestFixtureUsers(context.Context, string, time.Time, int) ([]store.User, error) {
	return nil, nil
}
func (m *mockUserStore) ListExpiredTestFixtureUsers(context.Context, store.ExpiredTestFixtureQuery) ([]store.User, error) {
	return nil, nil
}
func (m *mockUserStore) LockTestFixtureIssuance(context.Context) error { return nil }
func (m *mockUserStore) ReplaceTestIdentityLoginCode(context.Context, *store.TestIdentityLoginCode) error {
	return nil
}
func (m *mockUserStore) ConsumeTestIdentityLoginCode(context.Context, string, time.Time) (*store.TestIdentityLoginCode, error) {
	return nil, store.ErrNotFound
}
func (m *mockUserStore) CountLiveTestAdminUsers(context.Context, string, string, time.Time) (int, error) {
	return 0, nil
}
func (m *mockUserStore) ListTestAdminUsers(context.Context) ([]store.User, error) { return nil, nil }
