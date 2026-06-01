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

package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UserDomain describes the user entity for the CRUD-parity oracle.
//
// Add `RunDomain(t, factory, UserDomain())` to RunStoreSuite (in domains.go)
// to cover it across all backends. It is kept in this separate file so the
// user/allowlist port can land without contending on domains.go.
func UserDomain() Domain[store.User] {
	return Domain[store.User]{
		Name: "user",
		Make: func(seq int) *store.User {
			id := uuid.NewString()
			return &store.User{
				ID:          id,
				Email:       fmt.Sprintf("user-%d-%s@example.com", seq, id[:8]),
				DisplayName: fmt.Sprintf("User %d", seq),
				Role:        store.UserRoleMember,
				Status:      "active",
			}
		},
		GetID: func(u *store.User) string { return u.ID },
		Create: func(ctx context.Context, s store.Store, u *store.User) error {
			return s.CreateUser(ctx, u)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.User, error) {
			return s.GetUser(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.User], error) {
			return s.ListUsers(ctx, store.UserFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.User) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Email, got.Email)
			assert.Equal(t, want.DisplayName, got.DisplayName)
			assert.Equal(t, want.Role, got.Role)
			assert.Equal(t, want.Status, got.Status)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(u *store.User) {
			u.DisplayName = "Renamed " + u.DisplayName
			u.Role = store.UserRoleAdmin
		},
		Update: func(ctx context.Context, s store.Store, u *store.User) error {
			return s.UpdateUser(ctx, u)
		},
		VerifyMutated: func(t *testing.T, got *store.User) {
			assert.Contains(t, got.DisplayName, "Renamed ")
			assert.Equal(t, store.UserRoleAdmin, got.Role)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteUser(ctx, id)
		},
		// Users are hard-deleted (no SoftDelete spec).
		Filters: []FilterCase[store.User]{
			{
				Name: "ByRole",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateUser(ctx, &store.User{
						ID: uuid.NewString(), Email: "admin-" + uuid.NewString()[:8] + "@example.com",
						DisplayName: "Admin", Role: store.UserRoleAdmin, Status: "active",
					}))
					require.NoError(t, s.CreateUser(ctx, &store.User{
						ID: uuid.NewString(), Email: "member-" + uuid.NewString()[:8] + "@example.com",
						DisplayName: "Member", Role: store.UserRoleMember, Status: "active",
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.User], error) {
					return s.ListUsers(ctx, store.UserFilter{Role: store.UserRoleAdmin}, store.ListOptions{})
				},
				WantCount: 1,
			},
			{
				Name: "ByStatus",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateUser(ctx, &store.User{
						ID: uuid.NewString(), Email: "active-" + uuid.NewString()[:8] + "@example.com",
						DisplayName: "Active", Role: store.UserRoleMember, Status: "active",
					}))
					require.NoError(t, s.CreateUser(ctx, &store.User{
						ID: uuid.NewString(), Email: "suspended-" + uuid.NewString()[:8] + "@example.com",
						DisplayName: "Suspended", Role: store.UserRoleMember, Status: "suspended",
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.User], error) {
					return s.ListUsers(ctx, store.UserFilter{Status: "suspended"}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// AllowListDomain describes the email allow-list entry for the CRUD-parity
// oracle. The allow list is keyed by email rather than ID: Get and Delete
// operate on the (normalized) email address, so GetID returns the email.
func AllowListDomain() Domain[store.AllowListEntry] {
	return Domain[store.AllowListEntry]{
		Name: "allowlist",
		Make: func(seq int) *store.AllowListEntry {
			id := uuid.NewString()
			return &store.AllowListEntry{
				ID:      id,
				Email:   fmt.Sprintf("allow-%d-%s@example.com", seq, id[:8]),
				Note:    fmt.Sprintf("note %d", seq),
				AddedBy: "admin",
			}
		},
		GetID: func(e *store.AllowListEntry) string { return e.Email },
		Create: func(ctx context.Context, s store.Store, e *store.AllowListEntry) error {
			return s.AddAllowListEntry(ctx, e)
		},
		Get: func(ctx context.Context, s store.Store, email string) (*store.AllowListEntry, error) {
			return s.GetAllowListEntry(ctx, email)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.AllowListEntry], error) {
			return s.ListAllowListEntries(ctx, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.AllowListEntry) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Email, got.Email)
			assert.Equal(t, want.Note, got.Note)
			assert.Equal(t, want.AddedBy, got.AddedBy)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		// AllowListStore has no general-purpose update (only invite-id linking),
		// so the Update category is skipped.
		Delete: func(ctx context.Context, s store.Store, email string) error {
			return s.RemoveAllowListEntry(ctx, email)
		},
		// Allow-list entries are hard-deleted (no SoftDelete spec).
	}
}

// InviteCodeDomain describes the invite-code entity for the CRUD-parity oracle.
func InviteCodeDomain() Domain[store.InviteCode] {
	return Domain[store.InviteCode]{
		Name: "invitecode",
		Make: func(seq int) *store.InviteCode {
			id := uuid.NewString()
			return &store.InviteCode{
				ID:         id,
				CodeHash:   fmt.Sprintf("hash-%d-%s", seq, id),
				CodePrefix: fmt.Sprintf("scion_in%d", seq),
				MaxUses:    5,
				UseCount:   0,
				ExpiresAt:  time.Now().Add(24 * time.Hour),
				CreatedBy:  "admin",
				Note:       fmt.Sprintf("invite %d", seq),
			}
		},
		GetID: func(i *store.InviteCode) string { return i.ID },
		Create: func(ctx context.Context, s store.Store, i *store.InviteCode) error {
			return s.CreateInviteCode(ctx, i)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.InviteCode, error) {
			return s.GetInviteCode(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.InviteCode], error) {
			return s.ListInviteCodes(ctx, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.InviteCode) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.CodePrefix, got.CodePrefix)
			assert.Equal(t, want.MaxUses, got.MaxUses)
			assert.Equal(t, want.CreatedBy, got.CreatedBy)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		// InviteCodeStore exposes targeted mutators (revoke, increment) rather
		// than a general update, so the Update category is skipped.
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteInviteCode(ctx, id)
		},
		// Invite codes are hard-deleted (no SoftDelete spec).
	}
}
