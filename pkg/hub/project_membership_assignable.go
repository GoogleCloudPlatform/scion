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
	"fmt"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// AssignableRoles — the read-only "which project roles could this actor
// grant" view behind GET …/members/assignable-roles (ptone/scion#2529).
//
// It writes nothing and decides nothing on its own: for each role it runs
// memberRoleDecision, the per-role decision SetMemberRoles Phase P also
// uses (project_membership_role_decision.go), with every check selected, and
// reports the decision the PUT would return when the role is newly created
// for a principal. Custom-role authority comes only from
// customRoleAuthorityFromStore; the structural role_binding.* refusal only
// from checkNoRoleBindingPermissionInCreatedCustomRoles.
// ---------------------------------------------------------------------------

// AssignableProjectRole is one project-scoped role definition with the
// calling actor's ability to grant it.
type AssignableProjectRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RoleKind    string `json:"roleKind"` // "builtin" | "custom"
	Grantable   bool   `json:"grantable"`
	// Reason is empty when Grantable is true; otherwise it is the reason the
	// members PUT would give when refusing to create this role.
	Reason string `json:"reason"`
	// DenialCode and Details mirror the PUT's error code and error details
	// for the same refusal (for example details.requiredPermission on a
	// custom-role authority denial), so a client keys off structured fields
	// rather than reason text. Omitted when Grantable is true.
	DenialCode string                 `json:"denialCode,omitempty"`
	Details    map[string]interface{} `json:"details,omitempty"`
}

// AssignableRoles lists every project-scoped role definition (the built-in
// membership roles first in tier order, then custom roles by name) with
// whether actor may newly grant it in projectID. System-scoped roles are
// never listed. The result is principal-agnostic: principal-type
// eligibility (owner is user-only; admin is user or group; custom roles are
// user or group) is left to the client, as on the PUT it depends on the
// target principal.
//
// The grantable flag reports the PUT's decision for creating the role on a
// principal that does not already hold it:
//   - built-in roles: the governance matrix for op=add (with the
//     system-scope hub override), then CanDelegate;
//   - custom roles: not role_binding.*-bearing, then
//     customRoleAuthorityFromStore(role_binding.create), then CanDelegate.
//
// A built-in role CHANGE on an existing member is governed on the PUT as an
// update of both the old and new role, and CanDelegate runs only on an
// increase; this view does not model that per-principal case.
func (svc *ProjectMembershipService) AssignableRoles(ctx context.Context, actor UserIdentity, projectID string) ([]AssignableProjectRole, error) {
	all, err := svc.store.ListRoleDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role definitions: %w", err)
	}
	var roles []*store.RoleDefinition
	for _, rd := range all {
		if rd != nil && rd.ScopeType == store.RoleScopeProject {
			roles = append(roles, rd)
		}
	}
	sort.SliceStable(roles, func(i, j int) bool {
		iBuiltIn := store.IsBuiltInProjectMembershipRole(roles[i].Name)
		jBuiltIn := store.IsBuiltInProjectMembershipRole(roles[j].Name)
		if iBuiltIn != jBuiltIn {
			return iBuiltIn
		}
		if iBuiltIn {
			return projectRoleLevel(roles[i].Name) > projectRoleLevel(roles[j].Name)
		}
		if roles[i].Name != roles[j].Name {
			return roles[i].Name < roles[j].Name
		}
		return roles[i].ID < roles[j].ID
	})

	authority, err := svc.assignableActorAuthority(ctx, actor, projectID, roles)
	if err != nil {
		return nil, err
	}

	out := make([]AssignableProjectRole, 0, len(roles))
	for _, rd := range roles {
		item := AssignableProjectRole{
			ID:          rd.ID,
			Name:        rd.Name,
			Description: rd.Description,
			RoleKind:    projectRoleKind(rd.Name),
			Grantable:   true,
		}
		// Every check, for this one role, using the same per-check logic
		// and refusal constructors the PUT uses when the role is newly
		// created for a principal. The order is memberRoleDecision's; the
		// PUT's own stage order lives in SetMemberRoles and is pinned
		// against this by TestAssignableRoles_ConsistentWithPut.
		if d, _ := svc.memberRoleDecision(ctx, actor, projectID, authority, planChange{op: MembershipOpAdd, roleName: rd.Name}, rd, memberRoleCheckAll); d != nil {
			item.Grantable = false
			item.Reason = d.Reason
			item.DenialCode = d.DenialCode
			item.Details = d.Details
		}
		out = append(out, item)
	}
	return out, nil
}

// assignableActorAuthority evaluates the actor-side checks of
// SetMemberRoles Phase P once, for a plan that creates a binding: the
// credential gate, then memberActorAuthorityPreTx (asking custom-role
// create authority only when a custom role is listed).
func (svc *ProjectMembershipService) assignableActorAuthority(ctx context.Context, actor UserIdentity, projectID string, roles []*store.RoleDefinition) (*memberActorAuthority, error) {
	if denial := svc.checkMembershipCredential(ctx, actor.ID()); denial != nil {
		return &memberActorAuthority{credentialDenial: denial}, nil
	}

	hasCustom := false
	for _, rd := range roles {
		if !store.IsBuiltInProjectMembershipRole(rd.Name) {
			hasCustom = true
			break
		}
	}
	return svc.memberActorAuthorityPreTx(ctx, actor.ID(), projectID, true, false, hasCustom, false)
}
