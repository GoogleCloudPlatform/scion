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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project members groups (store.IsProjectMembersGroup) are system-managed.
// They cannot be the principal of a role binding, on any scope or route, and
// cannot be nested as a child of another group. The hub refuses these writes
// with a dedicated error (principal_ineligible plus
// details.reason=project_members_group); the store refuses them again with
// store.ErrProjectMembersGroupPrincipal as a backstop for any caller that
// skips the hub check. Deleting an existing binding or edge is never refused.

// projectMembersGroupPrincipalMessage is the user-facing reason returned when
// a write would make a project members group a role-binding principal or a
// child group.
const projectMembersGroupPrincipalMessage = "project members groups cannot be granted roles; add the project's members individually or use a dedicated group"

// projectMembersGroupDetailReason is the details.reason value for the hub
// refusal.
const projectMembersGroupDetailReason = "project_members_group"

// projectMembersGroupPrincipalDetails returns the structured error details
// for a refusal naming groupID.
func projectMembersGroupPrincipalDetails(groupID string) map[string]interface{} {
	return map[string]interface{}{
		"reason":  projectMembersGroupDetailReason,
		"groupId": groupID,
	}
}

// projectMembersGroupPrincipalDecision is the membership-service refusal for
// a new binding whose principal is a project members group.
func projectMembersGroupPrincipalDecision(groupID string) *MembershipDecision {
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodePrincipalIneligible,
		Reason:     projectMembersGroupPrincipalMessage,
		HTTPStatus: http.StatusBadRequest,
		Details:    projectMembersGroupPrincipalDetails(groupID),
	}
}

// isProjectMembersGroupPrincipal reports whether the principal is a group
// that is a project members group. A lookup failure reports false: the
// caller's normal path (and the store backstop) handles a missing group.
func isProjectMembersGroupPrincipal(ctx context.Context, st store.Store, principalType, principalID string) bool {
	if principalType != store.RoleBindingPrincipalGroup || principalID == "" {
		return false
	}
	g, err := st.GetGroup(ctx, principalID)
	if err != nil {
		return false
	}
	return store.IsProjectMembersGroup(g)
}

// legacyMembershipDenialDetails returns the details the single-binding
// membership endpoints (POST /members, PATCH /members/{id} and the built-in
// route of POST /admin/role-bindings) render for a refusal. Those endpoints
// historically render no details, so only the project members group refusal
// carries them; every other refusal body is unchanged.
func legacyMembershipDenialDetails(d *MembershipDecision) map[string]interface{} {
	if d == nil || d.DenialCode != ErrCodePrincipalIneligible || d.Details == nil {
		return nil
	}
	if d.Details["reason"] != projectMembersGroupDetailReason {
		return nil
	}
	return d.Details
}

// storeMembersGroupPrincipalMessage is the client-facing message for a store
// refusal (store.ErrProjectMembersGroupPrincipal), shared by writeErrorFromErr
// and storeMembersGroupPrincipalDecision so both routes return the same text.
const storeMembersGroupPrincipalMessage = "Project members groups cannot be role-binding principals or child groups"

// storeMembersGroupPrincipalDecision maps a store refusal
// (store.ErrProjectMembersGroupPrincipal) raised inside a membership
// transaction to a 400 decision, so it never surfaces as a 500. It returns
// nil for any other error.
func storeMembersGroupPrincipalDecision(err error) *MembershipDecision {
	if !errors.Is(err, store.ErrProjectMembersGroupPrincipal) {
		return nil
	}
	return &MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeInvalidRequest,
		Reason:     storeMembersGroupPrincipalMessage,
		HTTPStatus: http.StatusBadRequest,
	}
}
