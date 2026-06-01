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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

// entUserAllowStore is a test-only store that routes the user and
// allowlist/invite domains to the Ent-backed adapters, while delegating
// everything else to the embedded CompositeStore. It previews the wiring that
// P2-collapse will fold into composite.go, letting the shared CRUD-parity
// oracle run directly against the new adapters.
type entUserAllowStore struct {
	*CompositeStore
	users *UserStore
	allow *AllowListStore
}

// UserStore overrides.

func (s *entUserAllowStore) CreateUser(ctx context.Context, u *store.User) error {
	return s.users.CreateUser(ctx, u)
}
func (s *entUserAllowStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	return s.users.GetUser(ctx, id)
}
func (s *entUserAllowStore) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	return s.users.GetUserByEmail(ctx, email)
}
func (s *entUserAllowStore) UpdateUser(ctx context.Context, u *store.User) error {
	return s.users.UpdateUser(ctx, u)
}
func (s *entUserAllowStore) UpdateUserLastSeen(ctx context.Context, id string, t time.Time) error {
	return s.users.UpdateUserLastSeen(ctx, id, t)
}
func (s *entUserAllowStore) DeleteUser(ctx context.Context, id string) error {
	return s.users.DeleteUser(ctx, id)
}
func (s *entUserAllowStore) ListUsers(ctx context.Context, filter store.UserFilter, opts store.ListOptions) (*store.ListResult[store.User], error) {
	return s.users.ListUsers(ctx, filter, opts)
}

// AllowListStore overrides.

func (s *entUserAllowStore) AddAllowListEntry(ctx context.Context, entry *store.AllowListEntry) error {
	return s.allow.AddAllowListEntry(ctx, entry)
}
func (s *entUserAllowStore) RemoveAllowListEntry(ctx context.Context, email string) error {
	return s.allow.RemoveAllowListEntry(ctx, email)
}
func (s *entUserAllowStore) GetAllowListEntry(ctx context.Context, email string) (*store.AllowListEntry, error) {
	return s.allow.GetAllowListEntry(ctx, email)
}
func (s *entUserAllowStore) ListAllowListEntries(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.AllowListEntry], error) {
	return s.allow.ListAllowListEntries(ctx, opts)
}
func (s *entUserAllowStore) IsEmailAllowListed(ctx context.Context, email string) (bool, error) {
	return s.allow.IsEmailAllowListed(ctx, email)
}
func (s *entUserAllowStore) BulkAddAllowListEntries(ctx context.Context, entries []*store.AllowListEntry) (int, int, error) {
	return s.allow.BulkAddAllowListEntries(ctx, entries)
}
func (s *entUserAllowStore) ListEmailDomains(ctx context.Context) ([]string, error) {
	return s.allow.ListEmailDomains(ctx)
}
func (s *entUserAllowStore) UpdateAllowListEntryInviteID(ctx context.Context, email string, inviteID string) error {
	return s.allow.UpdateAllowListEntryInviteID(ctx, email, inviteID)
}
func (s *entUserAllowStore) ListAllowListEntriesWithInvites(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.AllowListEntryWithInvite], error) {
	return s.allow.ListAllowListEntriesWithInvites(ctx, opts)
}

// InviteCodeStore overrides.

func (s *entUserAllowStore) CreateInviteCode(ctx context.Context, invite *store.InviteCode) error {
	return s.allow.CreateInviteCode(ctx, invite)
}
func (s *entUserAllowStore) GetInviteCodeByHash(ctx context.Context, codeHash string) (*store.InviteCode, error) {
	return s.allow.GetInviteCodeByHash(ctx, codeHash)
}
func (s *entUserAllowStore) GetInviteCode(ctx context.Context, id string) (*store.InviteCode, error) {
	return s.allow.GetInviteCode(ctx, id)
}
func (s *entUserAllowStore) ListInviteCodes(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.InviteCode], error) {
	return s.allow.ListInviteCodes(ctx, opts)
}
func (s *entUserAllowStore) IncrementInviteUseCount(ctx context.Context, id string) error {
	return s.allow.IncrementInviteUseCount(ctx, id)
}
func (s *entUserAllowStore) RevokeInviteCode(ctx context.Context, id string) error {
	return s.allow.RevokeInviteCode(ctx, id)
}
func (s *entUserAllowStore) DeleteInviteCode(ctx context.Context, id string) error {
	return s.allow.DeleteInviteCode(ctx, id)
}
func (s *entUserAllowStore) GetInviteStats(ctx context.Context) (*store.InviteStats, error) {
	return s.allow.GetInviteStats(ctx)
}

// entUserAllowFactory builds a store backed by the Ent adapters for the user
// and allowlist/invite domains.
func entUserAllowFactory(t *testing.T) store.Store {
	t.Helper()

	entClient := enttest.NewClient(t)

	cs := NewCompositeStore(entClient)
	t.Cleanup(func() { _ = cs.Close() })

	return &entUserAllowStore{
		CompositeStore: cs,
		users:          NewUserStore(entClient),
		allow:          NewAllowListStore(entClient),
	}
}

// TestEntAdapter_UserAllowlist_CRUDParity runs the shared CRUD-parity oracle
// against the Ent-backed user and allowlist/invite adapters.
func TestEntAdapter_UserAllowlist_CRUDParity(t *testing.T) {
	storetest.RunDomain(t, entUserAllowFactory, storetest.UserDomain())
	storetest.RunDomain(t, entUserAllowFactory, storetest.AllowListDomain())
	storetest.RunDomain(t, entUserAllowFactory, storetest.InviteCodeDomain())
}
