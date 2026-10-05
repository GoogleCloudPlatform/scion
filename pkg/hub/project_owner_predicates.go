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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project-owner predicates (ptone/scion#2769).
//
// Two different questions are asked about a project's owners, and they must
// not be merged:
//
//   - "Does the project have a usable owner?" (projectHasUsableOwner): a
//     direct user project-owner binding inside its active window whose user
//     exists and is active. The last-owner rule uses this, so a project is
//     never left without someone who can actually manage it.
//   - "Does the project have any owner binding at all?" (projectOwnerBindingCount
//     here, and projectHasOwnerBinding in seed.go, the startup backfill gate):
//     any principal, any validity window, any user status. The last-owner
//     rule also refuses to drop this count to zero, because the startup
//     backfill re-grants Project.CreatedBy to a project with zero owner
//     bindings (ptone/scion#2554).
//
// NEVER use projectHasUsableOwner as the backfill gate: a project whose only
// owner binding is expired, or held by a suspended or deleted user, would
// then re-grant a creator who was deliberately removed.

// projectOwnerRoleDefinitionID resolves the project-owner role definition
// through s, which may be a transactional store.
func projectOwnerRoleDefinitionID(ctx context.Context, s store.Store) (string, error) {
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		return "", fmt.Errorf("resolve project-owner role definition: %w", err)
	}
	if rd == nil {
		return "", fmt.Errorf("resolve project-owner role definition: %w", store.ErrNotFound)
	}
	return rd.ID, nil
}

// bindingIsUsableOwner reports whether b is a usable owner binding: the
// project-owner role (ownerRDID), a user principal, inside its active window
// at now, and a user that exists with Status active. ErrNotFound from GetUser
// means not usable. Any other lookup error is returned, so a store fault is
// never mistaken for a policy fact (decision D1 of ptone/scion#2769: the
// caller fails the request and changes nothing).
func bindingIsUsableOwner(ctx context.Context, s store.Store, b *store.RoleBinding, ownerRDID string, now time.Time) (bool, error) {
	if b == nil || b.RoleDefinitionID != ownerRDID || b.PrincipalType != store.RoleBindingPrincipalUser {
		return false, nil
	}
	if !isBindingActive(b, now) {
		return false, nil
	}
	return userIsActive(ctx, s, b.PrincipalID)
}

// userIsActive reports whether userID names an existing user with Status
// active. ErrNotFound is false with no error; any other error is returned.
func userIsActive(ctx context.Context, s store.Store, userID string) (bool, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("look up user %s: %w", userID, err)
	}
	return u != nil && u.Status == store.UserStatusActive, nil
}

// projectHasUsableOwner reports whether projectID, read through s (which may
// be a transactional store), has at least one usable owner (see
// bindingIsUsableOwner) other than excludeUserID ("" excludes nobody).
// A lookup error other than ErrNotFound is returned.
//
// NEVER use this as the startup backfill gate (ptone/scion#2554); see the
// comment at the top of this file.
func projectHasUsableOwner(ctx context.Context, s store.Store, projectID string, now time.Time, excludeUserID string) (bool, error) {
	ids, err := usableOwnerPrincipals(ctx, s, projectID, now, excludeUserID, true)
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// usableOwnerPrincipals returns, in principal ID order, the distinct
// principals on projectID (read through s) that hold a usable owner binding
// (bindingIsUsableOwner, the single definition of the predicate), skipping
// excludeUserID ("" excludes nobody). With firstOnly it stops at the first
// usable principal.
//
// The project's bindings are grouped by principal first, and each
// principal's bindings are checked in turn until one is usable, so a
// principal is counted once however many owner bindings it holds. Only
// bindings inside their active window reach GetUser, so the cost is in
// practice one GetUser per active owner.
func usableOwnerPrincipals(ctx context.Context, s store.Store, projectID string, now time.Time, excludeUserID string, firstOnly bool) ([]string, error) {
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	if err != nil {
		return nil, err
	}
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return nil, fmt.Errorf("list bindings for project %s: %w", projectID, err)
	}
	byPrincipal := make(map[string][]*store.RoleBinding)
	for _, b := range bindings {
		if b == nil || (excludeUserID != "" && b.PrincipalID == excludeUserID) {
			continue
		}
		byPrincipal[b.PrincipalID] = append(byPrincipal[b.PrincipalID], b)
	}
	ids := make([]string, 0, len(byPrincipal))
	for id := range byPrincipal {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		for _, b := range byPrincipal[id] {
			ok, err := bindingIsUsableOwner(ctx, s, b, ownerRDID, now)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, id)
				break
			}
		}
		if firstOnly && len(out) > 0 {
			return out, nil
		}
	}
	return out, nil
}

// projectOwnerBindingCount returns the number of project-owner bindings of
// any kind (any principal, any validity window, any user status) on
// projectID, read through s, skipping user bindings held by excludeUserID
// ("" excludes nobody). This is the I2 floor of ptone/scion#2769: no
// removal may take it to zero, because the startup backfill
// (projectHasOwnerBinding) re-grants the creator of a project with zero
// owner bindings (ptone/scion#2554).
func projectOwnerBindingCount(ctx context.Context, s store.Store, projectID, excludeUserID string) (int, error) {
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	if err != nil {
		return 0, err
	}
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	if err != nil {
		return 0, fmt.Errorf("list bindings for project %s: %w", projectID, err)
	}
	n := 0
	for _, b := range bindings {
		if b == nil || b.RoleDefinitionID != ownerRDID {
			continue
		}
		if excludeUserID != "" && b.PrincipalType == store.RoleBindingPrincipalUser && b.PrincipalID == excludeUserID {
			continue
		}
		n++
	}
	return n, nil
}

// ownerBindingsAmong returns the project-owner bindings in bindings (any
// principal), resolving the owner role through s.
func ownerBindingsAmong(ctx context.Context, s store.Store, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []*store.RoleBinding
	for _, b := range bindings {
		if b != nil && b.RoleDefinitionID == ownerRDID {
			out = append(out, b)
		}
	}
	return out, nil
}

// anyUsableOwnerBinding reports whether at least one of bindings is a usable
// owner binding (bindingIsUsableOwner). Callers pass the owner bindings a
// mutation is about to remove or demote, read before the mutation, to get
// removedUsable for enforceOwnerRemovalTx.
func anyUsableOwnerBinding(ctx context.Context, s store.Store, bindings []*store.RoleBinding, now time.Time) (bool, error) {
	if len(bindings) == 0 {
		return false, nil
	}
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		ok, err := bindingIsUsableOwner(ctx, s, b, ownerRDID, now)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// enforceOwnerRemovalTx applies the last-owner rule (ptone/scion#2769, D2)
// to the post-state of a mutation that removed or demoted at least one
// project-owner binding. It must run inside the WithTx, after
// LockProjectForMembership and after the mutation has been applied through
// tx, so it reads the real post-state; a non-nil return rolls the
// transaction back.
//
//   - removedUsable (computed before the mutation) and no usable owner
//     remains: *lastOwnerError (I1).
//   - zero owner bindings of any kind remain: *lastOwnerError (I2), whatever
//     was removed.
//
// Removing an unusable owner binding (expired, scheduled, suspended,
// invited or deleted user) is therefore allowed whenever another owner
// binding remains, even in a project that has no usable owner.
// A lookup error is returned as is, so the request fails with 500 (D1).
func enforceOwnerRemovalTx(ctx context.Context, tx store.Store, projectID string, now time.Time, removedUsable bool) error {
	if removedUsable {
		ok, err := projectHasUsableOwner(ctx, tx, projectID, now, "")
		if err != nil {
			return fmt.Errorf("cannot verify usable owner: %w", err)
		}
		if !ok {
			return &lastOwnerError{projectID: projectID}
		}
		// A usable owner is itself an owner binding, so I2 holds too.
		return nil
	}
	n, err := projectOwnerBindingCount(ctx, tx, projectID, "")
	if err != nil {
		return fmt.Errorf("cannot verify owner bindings: %w", err)
	}
	if n == 0 {
		return &lastOwnerError{projectID: projectID}
	}
	return nil
}
