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

package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Grant shaping in the store (ptone/scion#4240, admin tier A3; see
// store.GrantShaping). Every grant write calls grantShapingFor before it
// builds the row:
//
//   - RoleStore.createRoleBinding (role bindings, every scope, including
//     project membership and visitor grants);
//   - GroupStore.AddGroupMember (user and agent memberships; group nesting)
//     and GroupStore.UpdateGroupMemberRole;
//   - RoleStore.CreateRoleDefinition and RoleStore.UpdateRoleDefinition
//     (custom roles);
//   - InviteCodeStore create (allowlist_store.go: CreateInviteCode);
//   - BrokerSecretStore join tokens (UpsertJoinToken, CreateJoinToken).
//
// TestGrantShapingBuilderCensus (grant_shaping_test.go) fails when a create
// or update builder for one of those tables appears in a function that does
// not call grantShapingFor.

// grantShapingFor returns the shaping to apply to a grant write, or nil
// when the write is not shaped. recorded are the user IDs the row records
// as its creator (created_by, added_by); the request's authenticated user
// (store.GrantActorFromContext) is checked as well. When any of them is an
// admin-kind test identity, the context must carry a valid shaping for that
// identity, otherwise the write is refused with
// store.ErrTestAdminGrantUnshaped. A kind read error, or a kind this binary
// does not know, refuses the same way. IDs that are not user IDs (system
// sentinels such as system-reconcile) and IDs with no user row are not
// actors. The returned expiry is at the stored timestamp precision.
func grantShapingFor(ctx context.Context, client *ent.Client, recorded ...string) (*store.GrantShaping, error) {
	shaping, shaped := store.GrantShapingFromContext(ctx)
	if shaped && !shaping.Valid() {
		return nil, fmt.Errorf("%w: incomplete shaping on the request", store.ErrTestAdminGrantUnshaped)
	}
	candidates := append([]string{store.GrantActorFromContext(ctx)}, recorded...)
	seen := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uid, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		row, err := client.User.Query().Where(user.IDEQ(uid)).Select(user.FieldKind).Only(ctx)
		if ent.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: reading the actor's kind: %v", store.ErrTestAdminGrantUnshaped, err)
		}
		switch string(row.Kind) {
		case store.UserKindHuman, store.UserKindTestFixture, store.UserKindFederated:
		case store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin:
			if !shaped || shaping.IdentityID != id {
				return nil, fmt.Errorf("%w: actor %s", store.ErrTestAdminGrantUnshaped, id)
			}
		default:
			return nil, fmt.Errorf("%w: actor %s has unknown kind %q", store.ErrTestAdminGrantUnshaped, id, row.Kind)
		}
	}
	if !shaped {
		return nil, nil
	}
	out := shaping
	out.ExpiresAt = testAdminExpiry(shaping.ExpiresAt)
	return &out, nil
}

// issuanceStamp is the issuance_id to store for a write under shaping g:
// the shaping's issuance ID, or nil when the write is not shaped. A value
// the caller put on the model is never stored.
func issuanceStamp(g *store.GrantShaping) *string {
	if g == nil {
		return nil
	}
	id := g.IssuanceID
	return &id
}

// shapedByIssuance reports whether a row whose issuance_id is stamp was
// created under shaping g (the same issuance).
func shapedByIssuance(g *store.GrantShaping, stamp *string) bool {
	return g != nil && stamp != nil && *stamp == g.IssuanceID
}
