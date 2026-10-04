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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAnnotatedGroup creates a group with the given project (empty for
// none) and annotations, returning its UUID string.
func (e *roleTestEnv) createAnnotatedGroup(t *testing.T, slug, projectID string, annotations map[string]string) string {
	t.Helper()
	uid := uuid.New()
	create := e.client.Group.Create().
		SetID(uid).
		SetName(slug).
		SetSlug(slug)
	if projectID != "" {
		create.SetProjectID(uuid.MustParse(projectID))
	}
	if annotations != nil {
		create.SetAnnotations(annotations)
	}
	_, err := create.Save(context.Background())
	require.NoError(t, err)
	return uid.String()
}

func TestCreateRoleBinding_RefusesProjectMembersGroupPrincipal(t *testing.T) {
	ctx := context.Background()
	env := newRoleTestEnv(t)
	customDef, err := env.roleStore.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "members-guard-custom",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	type binding struct {
		name      string
		roleDefID string
		scopeType string
		scopeID   string
	}
	bindings := []binding{
		{"project-builtin", env.projectRoleDef.ID, store.RoleScopeProject, env.projectID},
		{"project-custom", customDef.ID, store.RoleScopeProject, env.projectID},
		{"system", env.systemRoleDef.ID, store.RoleScopeSystem, ""},
	}

	refused := map[string]string{
		"canonical-key": env.createAnnotatedGroup(t, "mg-canonical", env.projectID,
			map[string]string{store.AnnotationProjectMembersGroup: "true"}),
		"legacy-key": env.createAnnotatedGroup(t, "mg-legacy", env.projectID,
			map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"}),
	}
	allowed := map[string]string{
		"unmarked-with-project": env.createAnnotatedGroup(t, "mg-plain", env.projectID, nil),
		"marker-false": env.createAnnotatedGroup(t, "mg-false", env.projectID,
			map[string]string{store.AnnotationProjectMembersGroup: "false"}),
		"marker-without-project": env.createAnnotatedGroup(t, "mg-noproject", "",
			map[string]string{store.AnnotationProjectMembersGroup: "true"}),
		"hub-members": env.createAnnotatedGroup(t, "hub-members", "", nil),
	}

	for kind, groupID := range refused {
		for _, b := range bindings {
			t.Run("refused/"+kind+"/"+b.name, func(t *testing.T) {
				_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: b.roleDefID,
					PrincipalType:    store.RoleBindingPrincipalGroup,
					PrincipalID:      groupID,
					ScopeType:        b.scopeType,
					ScopeID:          b.scopeID,
					CreatedBy:        "test",
				})
				require.Error(t, err)
				assert.True(t, errors.Is(err, store.ErrProjectMembersGroupPrincipal), "got: %v", err)
				assert.True(t, errors.Is(err, store.ErrInvalidInput), "must wrap ErrInvalidInput: %v", err)
				remaining, lErr := env.roleStore.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, groupID)
				require.NoError(t, lErr)
				assert.Empty(t, remaining)
			})
		}
	}
	for kind, groupID := range allowed {
		for _, b := range bindings {
			t.Run("allowed/"+kind+"/"+b.name, func(t *testing.T) {
				_, err := env.roleStore.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: b.roleDefID,
					PrincipalType:    store.RoleBindingPrincipalGroup,
					PrincipalID:      groupID,
					ScopeType:        b.scopeType,
					ScopeID:          b.scopeID,
					CreatedBy:        "test",
				})
				require.NoError(t, err)
			})
		}
	}
}

func TestAddGroupMember_RefusesProjectMembersGroupAsChild(t *testing.T) {
	ctx := context.Background()
	env := newRoleTestEnv(t)
	groups := NewGroupStore(env.client)
	parentID := env.createGroup(t, "mg-parent")

	for kind, key := range map[string]string{
		"canonical-key": store.AnnotationProjectMembersGroup,
		"legacy-key":    store.LegacyAnnotationProjectMembersGroup,
	} {
		t.Run("refused/"+kind, func(t *testing.T) {
			childID := env.createAnnotatedGroup(t, "mg-child-"+kind, env.projectID, map[string]string{key: "true"})
			err := groups.AddGroupMember(ctx, &store.GroupMember{
				GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID,
				Role: store.GroupMemberRoleMember,
			})
			require.Error(t, err)
			assert.True(t, errors.Is(err, store.ErrProjectMembersGroupPrincipal), "got: %v", err)
			members, lErr := groups.GetGroupMembers(ctx, parentID)
			require.NoError(t, lErr)
			assert.Empty(t, members, "no child edge may be created")

			// Adding a user into a members group is still allowed at the
			// store level.
			require.NoError(t, groups.AddGroupMember(ctx, &store.GroupMember{
				GroupID: childID, MemberType: store.GroupMemberTypeUser, MemberID: env.userID,
				Role: store.GroupMemberRoleMember,
			}))
		})
	}

	t.Run("allowed/ordinary-child", func(t *testing.T) {
		childID := env.createAnnotatedGroup(t, "mg-ordinary-child", env.projectID, nil)
		require.NoError(t, groups.AddGroupMember(ctx, &store.GroupMember{
			GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID,
			Role: store.GroupMemberRoleMember,
		}))
	})
}
