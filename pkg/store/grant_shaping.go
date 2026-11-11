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

package store

import (
	"context"
	"errors"
	"time"
)

// Grant shaping (ptone/scion#4240, admin tier phase A3).
//
// A test identity with an admin role (users.kind test_hub_admin or
// test_super_admin) may create authority on its test hub, but nothing it
// creates may outlive it. Every grant row its requests write carries the
// identity's issuance ID, and a grant that has an expiry gets one no later
// than the identity's own. The hub puts both on the request context
// (ContextWithGrantShaping) for each request such an identity makes; the
// store applies them to every grant write: role bindings, group
// memberships, custom role definitions, invite codes and broker join
// tokens.
//
// Fail-closed: the hub also records the request's authenticated user on
// the context (ContextWithGrantActor), independently of the shaping. A
// grant write whose actor (that user, or the user the row records as its
// creator) is an admin-kind test identity, and whose context carries no
// shaping for that identity, is refused with ErrTestAdminGrantUnshaped.
// Writes with no admin-kind actor (system paths: reconcile, issuance, the
// reaper, migrations) are not affected.

// GrantShaping is what the store applies to a grant written for an
// admin-kind test identity.
type GrantShaping struct {
	// IdentityID is the admin-kind test identity's user ID.
	IdentityID string
	// IssuanceID is stamped on every grant row (issuance_id).
	IssuanceID string
	// ExpiresAt is the identity's expiry: the latest expiry a grant may
	// carry. It is never zero for a valid shaping.
	ExpiresAt time.Time
}

// Valid reports whether g can be applied: all three fields set.
func (g GrantShaping) Valid() bool {
	return g.IdentityID != "" && g.IssuanceID != "" && !g.ExpiresAt.IsZero()
}

// ClampExpiry returns the expiry to store for a grant that requested
// requested (nil: no expiry): the earlier of requested and the identity's
// expiry. The result is never nil.
func (g GrantShaping) ClampExpiry(requested *time.Time) *time.Time {
	exp := g.ExpiresAt
	if requested != nil && requested.Before(exp) {
		exp = *requested
	}
	return &exp
}

type grantShapingKey struct{}

type grantActorKey struct{}

// ContextWithGrantShaping returns ctx carrying g. Only the hub's request
// middleware calls it, for a request made by an admitted admin-kind test
// identity.
func ContextWithGrantShaping(ctx context.Context, g GrantShaping) context.Context {
	return context.WithValue(ctx, grantShapingKey{}, g)
}

// GrantShapingFromContext returns the shaping on ctx, if any.
func GrantShapingFromContext(ctx context.Context) (GrantShaping, bool) {
	g, ok := ctx.Value(grantShapingKey{}).(GrantShaping)
	return g, ok
}

// ContextWithGrantActor returns ctx recording userID as the authenticated
// user of the request, for the store's fail-closed grant check.
func ContextWithGrantActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, grantActorKey{}, userID)
}

// GrantActorFromContext returns the user recorded by ContextWithGrantActor,
// or "".
func GrantActorFromContext(ctx context.Context) string {
	id, _ := ctx.Value(grantActorKey{}).(string)
	return id
}

var (
	// ErrTestAdminGrantUnshaped is returned for a grant write whose actor
	// is an admin-kind test identity when the context carries no grant
	// shaping for that identity, or the actor's kind cannot be read.
	ErrTestAdminGrantUnshaped = errors.New("a grant by a test identity with an admin role must carry its issuance and expiry (fail-closed)")

	// ErrTestAdminGrantRefused is returned for a grant write that an
	// admin-kind test identity may not make at all because it cannot be
	// made to expire with the identity: nesting a group it did not create
	// into another group, changing the role of a group membership it did
	// not add, and changing a role definition it did not create.
	ErrTestAdminGrantRefused = errors.New("a test identity with an admin role cannot make this change: it cannot be bounded by the identity's expiry")

	// ErrStampedRoleInUse is returned by DeleteIssuanceStampedGrant for a
	// role definition that role bindings still reference.
	ErrStampedRoleInUse = errors.New("the role definition is still bound")
)

// IssuanceGrantKind names a table grant shaping stamps.
type IssuanceGrantKind string

// Issuance-stamped grant kinds, in purge order.
const (
	IssuanceGrantRoleBinding     IssuanceGrantKind = "role_binding"
	IssuanceGrantGroupMembership IssuanceGrantKind = "group_membership"
	IssuanceGrantRoleDefinition  IssuanceGrantKind = "role_definition"
	IssuanceGrantInviteCode      IssuanceGrantKind = "invite_code"
	IssuanceGrantBrokerJoinToken IssuanceGrantKind = "broker_join_token"
)

// IssuanceStampedGrant is one row carrying an issuance stamp. ID is the
// row's ID (the broker ID for a join token, the membership row's ID for a
// group membership). The descriptive fields are set where the kind has
// them; they are for reports and audit summaries only. It never carries a
// secret (no code or token hash).
type IssuanceStampedGrant struct {
	Kind             IssuanceGrantKind `json:"kind"`
	ID               string            `json:"id"`
	RoleDefinitionID string            `json:"roleDefinitionId,omitempty"`
	PrincipalType    string            `json:"principalType,omitempty"`
	PrincipalID      string            `json:"principalId,omitempty"`
	ScopeType        string            `json:"scopeType,omitempty"`
	ScopeID          string            `json:"scopeId,omitempty"`
	GroupID          string            `json:"groupId,omitempty"`
	Name             string            `json:"name,omitempty"`
	ExpiresAt        *time.Time        `json:"expiresAt,omitempty"`
	// BoundOutsideIssuance is set on a role definition that a role binding
	// without this issuance's stamp names (for example a person's): the
	// purge keeps such a role, and the residue report lists it.
	BoundOutsideIssuance bool `json:"boundOutsideIssuance,omitempty"`
}
