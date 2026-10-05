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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GroupMembershipCleanupConformance exercises the group-membership cleanup
// contract across backends (ptone/scion#2769): DeleteGroupMembershipsForUser,
// DeleteGroupMembershipsForAgents, DeleteOrphanedGroupMemberships, and the
// rule that CountGroupMembersByRole and GetGroupMembers ignore orphaned rows
// (both user and agent cleared by ON DELETE SET NULL). It is hand-written
// rather than a Domain[T] descriptor because memberships are not an entity
// with its own ID and CRUD lifecycle.
func GroupMembershipCleanupConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	type fixture struct {
		s              store.Store
		group          string
		alice, bob     string
		agentA, agentB string
	}

	setup := func(t *testing.T) fixture {
		t.Helper()
		s := factory(t)
		seedAgentProject(t, ctx, s)
		f := fixture{s: s}
		for _, id := range []*string{&f.alice, &f.bob} {
			*id = uuid.NewString()
			require.NoError(t, s.CreateUser(ctx, &store.User{
				ID:          *id,
				Email:       fmt.Sprintf("gm-%s@example.com", (*id)[:8]),
				DisplayName: "Group Member",
				Role:        store.UserRoleMember,
				Status:      "active",
			}))
		}
		a := newOracleAgent("gm-a")
		require.NoError(t, s.CreateAgent(ctx, a))
		b := newOracleAgent("gm-b")
		require.NoError(t, s.CreateAgent(ctx, b))
		f.agentA, f.agentB = a.ID, b.ID

		f.group = uuid.NewString()
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID:        f.group,
			Name:      "Membership Cleanup",
			Slug:      "membership-cleanup-" + f.group[:8],
			GroupType: store.GroupTypeExplicit,
		}))
		for _, m := range []store.GroupMember{
			{MemberType: store.GroupMemberTypeUser, MemberID: f.alice, Role: store.GroupMemberRoleOwner},
			{MemberType: store.GroupMemberTypeUser, MemberID: f.bob, Role: store.GroupMemberRoleOwner},
			{MemberType: store.GroupMemberTypeAgent, MemberID: f.agentA, Role: store.GroupMemberRoleMember},
			{MemberType: store.GroupMemberTypeAgent, MemberID: f.agentB, Role: store.GroupMemberRoleMember},
		} {
			m.GroupID = f.group
			require.NoError(t, s.AddGroupMember(ctx, &m))
		}
		return f
	}

	memberIDs := func(t *testing.T, s store.Store, groupID string) []string {
		t.Helper()
		members, err := s.GetGroupMembers(ctx, groupID)
		require.NoError(t, err)
		ids := make([]string, 0, len(members))
		for _, m := range members {
			require.NotEmpty(t, m.MemberType, "listing must not carry a blank member")
			require.NotEmpty(t, m.MemberID, "listing must not carry a blank member")
			ids = append(ids, m.MemberID)
		}
		return ids
	}

	t.Run("group_membership_cleanup", func(t *testing.T) {
		t.Run("delete for user removes only that user's rows", func(t *testing.T) {
			f := setup(t)
			n, err := f.s.DeleteGroupMembershipsForUser(ctx, f.alice)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.ElementsMatch(t, []string{f.bob, f.agentA, f.agentB}, memberIDs(t, f.s, f.group))

			n, err = f.s.DeleteGroupMembershipsForUser(ctx, f.alice)
			require.NoError(t, err)
			assert.Equal(t, 0, n, "second delete is a no-op")
		})

		t.Run("delete for agents removes only those agents' rows", func(t *testing.T) {
			f := setup(t)
			n, err := f.s.DeleteGroupMembershipsForAgents(ctx, []string{f.agentA})
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.ElementsMatch(t, []string{f.alice, f.bob, f.agentB}, memberIDs(t, f.s, f.group))

			n, err = f.s.DeleteGroupMembershipsForAgents(ctx, nil)
			require.NoError(t, err)
			assert.Equal(t, 0, n, "empty ids is a no-op")
		})

		t.Run("orphaned rows are not counted or listed, and the sweep removes only them", func(t *testing.T) {
			f := setup(t)
			// Delete the principals without the membership cleanup, the way
			// a pre-fix delete path left them: ON DELETE SET NULL turns
			// their rows into orphans.
			require.NoError(t, f.s.DeleteUser(ctx, f.bob))
			require.NoError(t, f.s.DeleteAgent(ctx, f.agentB))

			owners, err := f.s.CountGroupMembersByRole(ctx, f.group, store.GroupMemberRoleOwner)
			require.NoError(t, err)
			assert.Equal(t, 1, owners, "a deleted user's orphaned row must not count as an owner")
			assert.ElementsMatch(t, []string{f.alice, f.agentA}, memberIDs(t, f.s, f.group))

			n, err := f.s.DeleteOrphanedGroupMemberships(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "only bob's row is orphaned; DeleteAgent removes the agent's own rows")

			n, err = f.s.DeleteOrphanedGroupMemberships(ctx)
			require.NoError(t, err)
			assert.Equal(t, 0, n, "sweep is idempotent")
			assert.ElementsMatch(t, []string{f.alice, f.agentA}, memberIDs(t, f.s, f.group))
		})
	})
}
