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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func seedRoleDefinitions(ctx context.Context, s store.Store) {
	reconcileBuiltInRoles(ctx, s)
}

// hubRoleGrantState is the observable hub-level grant state for a user.
type hubRoleGrantState struct {
	InHubMembers      bool
	HubViewerBindings int
	SuperAdminBinding int
}

func observeHubRoleGrants(t *testing.T, s store.Store, userID string) hubRoleGrantState {
	t.Helper()
	ctx := context.Background()

	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	_, err = s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, userID)
	inGroup := err == nil

	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	st := hubRoleGrantState{InHubMembers: inGroup}
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem {
			continue
		}
		switch b.RoleDefinitionID {
		case viewerRD.ID:
			st.HubViewerBindings++
		case superRD.ID:
			st.SuperAdminBinding++
		}
	}
	return st
}
