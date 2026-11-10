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

package hubtestutil

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// PvSeedGroupMember adds a member to a group (preview-test-scoped helper).
func PvSeedGroupMember(t *testing.T, s store.Store, groupID, memberType, memberID string) {
	t.Helper()
	err := s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID:    groupID,
		MemberType: memberType,
		MemberID:   memberID,
		Role:       "member",
		AddedBy:    "test",
	})
	require.NoError(t, err)
}

// PvSeedRoleBinding creates a role binding (preview-test-scoped helper).
func PvSeedRoleBinding(t *testing.T, s store.Store, roleDefID, principalType, principalID, scopeType, scopeID string) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}
