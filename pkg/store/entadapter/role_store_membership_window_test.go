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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/rolebinding"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membershipWindowNow is the fixed store clock for the membership window
// tests. Whole seconds, so stored bounds compare exactly after a round trip.
var membershipWindowNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func newMembershipWindowEnv(t *testing.T) *roleTestEnv {
	t.Helper()
	env := newRoleTestEnv(t)
	env.roleStore.now = func() time.Time { return membershipWindowNow }
	return env
}

// bindProject creates a project-scoped binding for env.userID on
// env.projectID with the given role and activation window.
func (e *roleTestEnv) bindProject(t *testing.T, roleDefID string, notBefore, expiresAt *time.Time) *store.RoleBinding {
	t.Helper()
	rb, err := e.roleStore.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      e.userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          e.projectID,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return rb
}

func timePtr(t time.Time) *time.Time { return &t }

func TestGetProjectMembership_ExpiredBindingNotFound(t *testing.T) {
	env := newMembershipWindowEnv(t)
	ctx := context.Background()
	env.bindProject(t, env.projectRoleDef.ID, nil, timePtr(membershipWindowNow.Add(-time.Hour)))

	_, err := env.roleStore.GetProjectMembership(ctx, env.projectID, env.userID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "expected ErrNotFound, got %v", err)

	members, err := env.roleStore.ListProjectMembers(ctx, env.projectID)
	require.NoError(t, err)
	assert.Empty(t, members)

	ok, err := env.roleStore.IsProjectMember(ctx, env.projectID, env.userID)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGetProjectMembership_FutureNotBeforeNotFound(t *testing.T) {
	env := newMembershipWindowEnv(t)
	ctx := context.Background()
	env.bindProject(t, env.projectRoleDef.ID, timePtr(membershipWindowNow.Add(time.Hour)), nil)

	_, err := env.roleStore.GetProjectMembership(ctx, env.projectID, env.userID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "expected ErrNotFound, got %v", err)

	members, err := env.roleStore.ListProjectMembers(ctx, env.projectID)
	require.NoError(t, err)
	assert.Empty(t, members)

	ok, err := env.roleStore.IsProjectMember(ctx, env.projectID, env.userID)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGetProjectMembership_UnboundedAndInWindowBindingsCount(t *testing.T) {
	for name, window := range map[string][2]*time.Time{
		"unbounded": {nil, nil},
		"in window": {timePtr(membershipWindowNow.Add(-time.Hour)), timePtr(membershipWindowNow.Add(time.Hour))},
	} {
		t.Run(name, func(t *testing.T) {
			env := newMembershipWindowEnv(t)
			ctx := context.Background()
			rb := env.bindProject(t, env.projectRoleDef.ID, window[0], window[1])

			m, err := env.roleStore.GetProjectMembership(ctx, env.projectID, env.userID)
			require.NoError(t, err)
			assert.Equal(t, store.ProjectRoleMember, m.Role)
			assert.Equal(t, rb.ID, m.RoleBindingID)

			members, err := env.roleStore.ListProjectMembers(ctx, env.projectID)
			require.NoError(t, err)
			require.Len(t, members, 1)
			assert.Equal(t, env.userID, members[0].UserID)

			ok, err := env.roleStore.IsProjectMember(ctx, env.projectID, env.userID)
			require.NoError(t, err)
			assert.True(t, ok)
		})
	}
}

// An expired owner binding next to an active member binding resolves to
// the member role, not the higher-ranked owner role. The owner row is
// written without the built-in membership marker, as rows written before
// that marker existed are, since the store refuses a second built-in role.
func TestGetProjectMembership_ExpiredOwnerBesideActiveMemberReturnsMember(t *testing.T) {
	env := newMembershipWindowEnv(t)
	ctx := context.Background()

	ownerDefID := uuid.MustParse(env.projectOwnerDef.ID)
	_, err := env.client.RoleBinding.Create().
		SetNillableRoleDefinitionID(&ownerDefID).
		SetPrincipalType(rolebinding.PrincipalTypeUser).
		SetPrincipalID(env.userID).
		SetScopeType(rolebinding.ScopeTypeProject).
		SetScopeID(env.projectID).
		SetExpiresAt(membershipWindowNow.Add(-time.Minute)).
		Save(ctx)
	require.NoError(t, err)
	memberRB := env.bindProject(t, env.projectRoleDef.ID, nil, nil)

	m, err := env.roleStore.GetProjectMembership(ctx, env.projectID, env.userID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, m.Role)
	assert.Equal(t, memberRB.ID, m.RoleBindingID)

	members, err := env.roleStore.ListProjectMembers(ctx, env.projectID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, store.ProjectRoleMember, members[0].Role)
	assert.Equal(t, memberRB.ID, members[0].RoleBindingID)
}

// The boundary instants follow the authorization kernel's rule
// (evaluateActivation): NotBefore is inclusive, ExpiresAt is exclusive.
func TestGetProjectMembership_BoundaryInstantsMatchKernel(t *testing.T) {
	cases := []struct {
		name      string
		notBefore *time.Time
		expiresAt *time.Time
		want      bool
	}{
		{"now equals NotBefore is active", timePtr(membershipWindowNow), nil, true},
		{"now one second before NotBefore is inactive", timePtr(membershipWindowNow.Add(time.Second)), nil, false},
		{"now equals ExpiresAt is inactive", nil, timePtr(membershipWindowNow), false},
		{"now one second before ExpiresAt is active", nil, timePtr(membershipWindowNow.Add(time.Second)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMembershipWindowEnv(t)
			ctx := context.Background()
			env.bindProject(t, env.projectRoleDef.ID, tc.notBefore, tc.expiresAt)

			_, err := env.roleStore.GetProjectMembership(ctx, env.projectID, env.userID)
			if tc.want {
				require.NoError(t, err)
			} else {
				assert.True(t, errors.Is(err, store.ErrNotFound), "expected ErrNotFound, got %v", err)
			}

			members, err := env.roleStore.ListProjectMembers(ctx, env.projectID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, len(members) == 1)

			ok, err := env.roleStore.IsProjectMember(ctx, env.projectID, env.userID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}

func TestRoleBindingActiveAt(t *testing.T) {
	now := membershipWindowNow
	cases := []struct {
		name      string
		now       time.Time
		notBefore *time.Time
		expiresAt *time.Time
		want      bool
	}{
		{"unbounded", now, nil, nil, true},
		{"unbounded with zero now", time.Time{}, nil, nil, true},
		{"bounded with zero now", time.Time{}, nil, timePtr(now.Add(time.Hour)), false},
		{"NotBefore inclusive", now, timePtr(now), nil, true},
		{"before NotBefore", now, timePtr(now.Add(time.Nanosecond)), nil, false},
		{"ExpiresAt exclusive", now, nil, timePtr(now), false},
		{"before ExpiresAt", now, nil, timePtr(now.Add(time.Nanosecond)), true},
		{"after ExpiresAt", now, nil, timePtr(now.Add(-time.Nanosecond)), false},
		{"inside both bounds", now, timePtr(now.Add(-time.Hour)), timePtr(now.Add(time.Hour)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb := &ent.RoleBinding{NotBefore: tc.notBefore, ExpiresAt: tc.expiresAt}
			assert.Equal(t, tc.want, roleBindingActiveAt(rb, tc.now))
		})
	}
}
