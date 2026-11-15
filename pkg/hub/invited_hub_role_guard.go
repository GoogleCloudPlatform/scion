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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errHubRoleOnInvitedUser is returned when an API or service mutation attempts
// to grant a built-in system hub role or hub-members group membership to an
// invited user before first sign-in.
var errHubRoleOnInvitedUser = errors.New("hub roles are assigned when the user first signs in")

// isBuiltInSystemHubRoleDef reports whether rd is one of the seeded system-scoped
// built-in hub roles (super-admin, hub-admin, hub-member, hub-viewer,
// global-catalog-author). Custom system roles (rd.System == false) are allowed
// on invited users and take effect at first sign-in.
func isBuiltInSystemHubRoleDef(rd *store.RoleDefinition) bool {
	if rd == nil || !rd.System || rd.ScopeType != store.RoleScopeSystem {
		return false
	}
	switch rd.Name {
	case store.SystemRoleSuperAdmin,
		store.SystemRoleHubAdmin,
		store.SystemRoleHubMember,
		store.SystemRoleHubViewer,
		store.SystemRoleGlobalCatalogAuthor:
		return true
	default:
		return false
	}
}

// rejectInvitedHubRoleBinding refuses binding a built-in system hub role to an
// invited user, either directly or through a group that contains an invited
// user. Store lookup failures other than missing target records fail closed.
func rejectInvitedHubRoleBinding(ctx context.Context, st store.Store, rb *store.RoleBinding) error {
	if rb == nil || rb.ScopeType != store.RoleScopeSystem {
		return nil
	}

	rd, err := st.GetRoleDefinition(ctx, rb.RoleDefinitionID)
	if err != nil {
		return err
	}
	if !isBuiltInSystemHubRoleDef(rd) {
		return nil
	}
	// Preserve the existing 403 restriction when a non-reconciler caller tries
	// to bind super-admin.
	if rd.Name == store.SystemRoleSuperAdmin &&
		!store.IsSuperAdminBindingAllowed(rb.CreatedBy) &&
		rb.CreatedBy != store.TestIdentityIssuanceCreatedBy {
		return store.ErrSuperAdminBindingRestricted
	}

	switch rb.PrincipalType {
	case store.RoleBindingPrincipalUser:
		inv, err := isUserInvited(ctx, st, rb.PrincipalID)
		if err != nil {
			return err
		}
		if inv {
			return errHubRoleOnInvitedUser
		}
	case store.RoleBindingPrincipalGroup:
		users, err := transitiveGroupMemberUsers(ctx, st, rb.PrincipalID)
		if err != nil {
			return err
		}
		inv, err := anyUserInvited(ctx, st, users)
		if err != nil {
			return err
		}
		if inv {
			return errHubRoleOnInvitedUser
		}
	}
	return nil
}

// rejectInvitedGroupHubRoleGrant refuses adding an invited user (or a child
// group containing an invited user) to hub-members or to any group whose
// ancestor closure includes hub-members or holds a built-in system hub role.
// Store lookup failures fail closed.
func rejectInvitedGroupHubRoleGrant(ctx context.Context, st store.Store, targetGroupID, memberType, memberID string) error {
	switch memberType {
	case store.GroupMemberTypeUser:
		inv, err := isUserInvited(ctx, st, memberID)
		if err != nil {
			return err
		}
		if !inv {
			return nil
		}
		carries, err := groupClosureCarriesBuiltInHubRole(ctx, st, targetGroupID)
		if err != nil {
			return err
		}
		if carries {
			return errHubRoleOnInvitedUser
		}
	case store.GroupMemberTypeGroup:
		carries, err := groupClosureCarriesBuiltInHubRole(ctx, st, targetGroupID)
		if err != nil {
			return err
		}
		if !carries {
			return nil
		}
		users, err := transitiveGroupMemberUsers(ctx, st, memberID)
		if err != nil {
			return err
		}
		inv, err := anyUserInvited(ctx, st, users)
		if err != nil {
			return err
		}
		if inv {
			return errHubRoleOnInvitedUser
		}
	}
	return nil
}

func isUserInvited(ctx context.Context, st store.Store, userID string) (bool, error) {
	u, err := st.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("look up user %s for hub role guard: %w", userID, err)
	}
	return u != nil && u.Status == store.UserStatusInvited, nil
}

func anyUserInvited(ctx context.Context, st store.Store, userIDs []string) (bool, error) {
	for _, uid := range userIDs {
		inv, err := isUserInvited(ctx, st, uid)
		if err != nil {
			return false, err
		}
		if inv {
			return true, nil
		}
	}
	return false, nil
}

// groupClosureCarriesBuiltInHubRole reports whether groupID or any group that
// transitively contains it is the canonical hub-members group or holds a
// system-scoped built-in hub role binding.
func groupClosureCarriesBuiltInHubRole(ctx context.Context, st store.Store, groupID string) (bool, error) {
	g, err := st.GetGroup(ctx, groupID)
	if err != nil {
		return false, fmt.Errorf("look up group %s for hub role guard: %w", groupID, err)
	}
	if g == nil {
		return false, fmt.Errorf("group %s not found", groupID)
	}
	if g.Slug == hubMembersSlug {
		return true, nil
	}

	parents, err := st.GetParentGroups(ctx, groupID)
	if err != nil {
		return false, fmt.Errorf("expand parent groups of %s for hub role guard: %w", groupID, err)
	}

	hubMembers, err := st.GetGroupBySlug(ctx, hubMembersSlug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("look up %s group for hub role guard: %w", hubMembersSlug, err)
	}
	if err == nil && hubMembers != nil {
		if groupID == hubMembers.ID {
			return true, nil
		}
		for _, pid := range parents {
			if pid == hubMembers.ID {
				return true, nil
			}
		}
	}

	principals := make([]store.PrincipalRef, 0, 1+len(parents))
	principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: groupID})
	for _, pid := range parents {
		principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: pid})
	}
	bindings, err := st.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		return false, fmt.Errorf("list bindings on group closure of %s: %w", groupID, err)
	}

	roles := map[string]*store.RoleDefinition{}
	for _, b := range bindings {
		if b == nil || b.ScopeType != store.RoleScopeSystem {
			continue
		}
		rd, seen := roles[b.RoleDefinitionID]
		if !seen {
			var rdErr error
			rd, rdErr = st.GetRoleDefinition(ctx, b.RoleDefinitionID)
			if rdErr != nil && !errors.Is(rdErr, store.ErrNotFound) {
				return false, fmt.Errorf("load role definition %s: %w", b.RoleDefinitionID, rdErr)
			}
			if rdErr != nil || rd == nil {
				return false, fmt.Errorf("role definition %s of group role binding %s does not resolve", b.RoleDefinitionID, b.ID)
			}
			roles[b.RoleDefinitionID] = rd
		}
		if isBuiltInSystemHubRoleDef(rd) {
			return true, nil
		}
	}
	return false, nil
}
