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
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for ptone/scion#2769 item 4: group memberships of deleted users and
// agents (ON DELETE SET NULL leaves rows with both IDs NULL).

// newGroupWithMembers creates an explicit group with the given user members.
func newGroupWithMembers(t *testing.T, s store.Store, slug string, members map[string]string) *store.Group {
	t.Helper()
	ctx := context.Background()
	g := &store.Group{ID: tid(slug), Name: slug, Slug: slug, GroupType: store.GroupTypeExplicit,
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateGroup(ctx, g))
	for userID, role := range members {
		permSeedUser(t, ctx, s, userID)
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: role, AddedAt: time.Now(),
		}))
	}
	return g
}

// requireNoOrphanedMemberships asserts that no membership row with both IDs
// NULL exists. The listing and count paths hide such rows, so the check uses
// the sweep's own count (it would be non-zero if a row were left behind).
func requireNoOrphanedMemberships(t *testing.T, s store.Store) {
	t.Helper()
	n, err := s.DeleteOrphanedGroupMemberships(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "a NULL group-membership row was left behind")
}

func TestDeleteUser_RemovesGroupMemberships(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	keep, gone := tid("gm-keep"), tid("gm-gone")
	g := newGroupWithMembers(t, s, "gm-delete-user", map[string]string{
		keep: store.GroupMemberRoleOwner,
		gone: store.GroupMemberRoleOwner,
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+gone, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	requireNoOrphanedMemberships(t, s)
	members, err := s.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, keep, members[0].MemberID)
}

func TestDeprecatedAllowListDelete_RemovesGroupMemberships(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	keep := tid("gm-al-keep")
	g := newGroupWithMembers(t, s, "gm-allow-list", map[string]string{keep: store.GroupMemberRoleOwner})
	carol := newInvitedUser(t, s, "gm-al-carol", "gm-carol@test.com")
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: carol.ID, Role: store.GroupMemberRoleMember,
	}))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	requireNoOrphanedMemberships(t, s)
	members, err := s.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, keep, members[0].MemberID)
}

// TestStartupSweep_RemovesOrphanedGroupMemberships: rows orphaned before
// this fix (user deleted, membership row left with both IDs NULL) are
// removed when the server starts; real rows stay.
func TestStartupSweep_RemovesOrphanedGroupMemberships(t *testing.T) {
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))

	keep, gone := tid("gm-sweep-keep"), tid("gm-sweep-gone")
	g := newGroupWithMembers(t, s, "gm-sweep", map[string]string{
		keep: store.GroupMemberRoleOwner,
		gone: store.GroupMemberRoleMember,
	})
	// Store-level delete, as the pre-fix delete paths did: the FK turns the
	// membership into an orphan.
	require.NoError(t, s.DeleteUser(ctx, gone))

	_, s = testServerWithStore(t, s) // New() runs the startup sweep.

	requireNoOrphanedMemberships(t, s)
	members, err := s.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, keep, members[0].MemberID, "the real membership survives the sweep")
}

// sweepFailingStore fails DeleteOrphanedGroupMemberships and passes every
// other call through.
type sweepFailingStore struct {
	store.Store
	calls int
}

// DB forwards to the wrapped store's raw *sql.DB so New()'s D4
// membership-index migration runs against the real store.
func (s *sweepFailingStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *sweepFailingStore) DeleteOrphanedGroupMemberships(context.Context) (int, error) {
	s.calls++
	return 0, errors.New("sweep failed")
}

// TestStartupSweep_ErrorIsNonFatal: a failing startup sweep is logged at Warn
// and server construction still succeeds.
func TestStartupSweep_ErrorIsNonFatal(t *testing.T) {
	inner, err := newTestStore(":memory:")
	require.NoError(t, err)
	wrapped := &sweepFailingStore{Store: inner}
	logs := captureSlog(t)

	srv, _ := testServerWithStore(t, wrapped) // fails the test if New() errors
	require.NotNil(t, srv)

	assert.Equal(t, 1, wrapped.calls, "New() ran the startup sweep")
	var warned bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") &&
			strings.Contains(line, "failed to delete orphaned group memberships") &&
			strings.Contains(line, "sweep failed") {
			warned = true
		}
	}
	assert.True(t, warned, "the sweep error is logged at Warn")
}

// TestSweepOrphanedGroupMemberships_Idempotent: a second run finds nothing.
func TestSweepOrphanedGroupMemberships_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	keep, gone := tid("gm-idem-keep"), tid("gm-idem-gone")
	g := newGroupWithMembers(t, s, "gm-idem", map[string]string{
		keep: store.GroupMemberRoleOwner,
		gone: store.GroupMemberRoleMember,
	})
	require.NoError(t, s.DeleteUser(ctx, gone))

	sweepOrphanedGroupMemberships(ctx, s)
	sweepOrphanedGroupMemberships(ctx, s)

	requireNoOrphanedMemberships(t, s)
	members, err := s.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
}

// TestRemoveGroupMember_LastOwnerDeniedWhenOtherOwnerDeleted: the ordinary
// group last-owner check counts owners; a deleted user's orphaned owner row
// must not count, so removing the only real owner is still denied.
func TestRemoveGroupMember_LastOwnerDeniedWhenOtherOwnerDeleted(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	alice, bob := tid("gm-lo-alice"), tid("gm-lo-bob")
	g := newGroupWithMembers(t, s, "gm-last-owner", map[string]string{
		alice: store.GroupMemberRoleOwner,
		bob:   store.GroupMemberRoleOwner,
	})
	// Store-level delete leaves bob's owner row orphaned (pre-fix state).
	require.NoError(t, s.DeleteUser(ctx, bob))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+g.ID+"/members/user/"+alice, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "last owner")

	_, err := s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, alice)
	require.NoError(t, err, "alice is still an owner")
}
