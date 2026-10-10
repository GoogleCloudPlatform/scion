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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DelegatedAgentIdentity is the identity the authentication middleware
// builds for a request that presents an agent delegated credential
// (Authorization: Bearer scion_adt_...). It is produced only by
// newDelegatedAgentIdentity from stored rows.
//
// It deliberately implements neither UserIdentity nor AgentIdentity, so no
// user or agent shortcut (session gates, agent self-access exemptions, agent
// scope checks) can match it, and it carries no local-provenance marker, so
// AncestryIsHubAttested is false for it. Every authorization decision for it
// goes through decideAgentDelegation.
type DelegatedAgentIdentity struct {
	agentID             string
	agentProjectID      string
	authorizingUserID   string
	grantID             string
	credentialID        string
	exchangeAgentCredID string
	boundary            TokenBoundary
	// ceiling is the delegated credential's own permission set, a subset
	// of the grant's.
	ceiling permissions.FrozenPermissionCeiling
}

// ID returns the actor agent's ID.
func (d *DelegatedAgentIdentity) ID() string { return d.agentID }

// Type returns "agent_delegated". Type is informational: classification is
// by concrete type (principalContextForIdentity), never by this string.
func (d *DelegatedAgentIdentity) Type() string { return string(PrincipalKindAgentDelegated) }

// AgentProjectID returns the project of the actor agent, as recorded on the
// grant.
func (d *DelegatedAgentIdentity) AgentProjectID() string { return d.agentProjectID }

// AuthorizingUserID returns the grant issuer's user ID.
func (d *DelegatedAgentIdentity) AuthorizingUserID() string { return d.authorizingUserID }

// GrantID returns the agent delegation grant's ID.
func (d *DelegatedAgentIdentity) GrantID() string { return d.grantID }

// CredentialID returns the delegated credential's ID.
func (d *DelegatedAgentIdentity) CredentialID() string { return d.credentialID }

// ExchangeAgentCredentialID returns the agent credential verified when the
// delegated credential was exchanged.
func (d *DelegatedAgentIdentity) ExchangeAgentCredentialID() string { return d.exchangeAgentCredID }

// Boundary returns the grant's boundary.
func (d *DelegatedAgentIdentity) Boundary() TokenBoundary { return d.boundary }

// Ceiling returns a copy of the delegated credential's permission ceiling.
func (d *DelegatedAgentIdentity) Ceiling() permissions.FrozenPermissionCeiling {
	return permissions.FrozenPermissionCeiling{
		Version:       d.ceiling.Version,
		PermissionIDs: append([]string(nil), d.ceiling.PermissionIDs...),
	}
}

// newDelegatedAgentIdentity builds the identity for cred, exchanged from
// grant. Every field comes from the stored rows.
func newDelegatedAgentIdentity(cred *store.AgentDelegatedCredential, grant *store.AgentDelegationGrant) *DelegatedAgentIdentity {
	return &DelegatedAgentIdentity{
		agentID:             grant.AgentID,
		agentProjectID:      grant.AgentProjectID,
		authorizingUserID:   grant.IssuerUserID,
		grantID:             grant.ID,
		credentialID:        cred.ID,
		exchangeAgentCredID: cred.ExchangeAgentCredentialID,
		boundary:            TokenBoundary{Kind: BoundaryKind(grant.BoundaryKind), ProjectID: grant.BoundaryProjectID},
		ceiling: permissions.FrozenPermissionCeiling{
			Version:       permissions.CeilingVersionV1,
			PermissionIDs: append([]string(nil), cred.CeilingPermissionIDs...),
		},
	}
}

// isDelegatedAgentIdentity reports whether identity is a non-nil
// *DelegatedAgentIdentity.
func isDelegatedAgentIdentity(identity Identity) bool {
	d, ok := identity.(*DelegatedAgentIdentity)
	return ok && d != nil
}

// delegatedRequestState is the request-scoped state the authentication
// middleware loads once for a delegated request: the credential, grant,
// issuer, agent and exchange agent credential rows, and the issuer
// principal built from the stored issuer row. It is held under a private
// context key; the issuer principal is never placed in the identity slot.
type delegatedRequestState struct {
	identity     *DelegatedAgentIdentity
	credential   *store.AgentDelegatedCredential
	grant        *store.AgentDelegationGrant
	issuer       *store.User
	agent        *store.Agent
	exchangeCred *store.AgentCredential
	// issuerPC is the issuer principal for EvaluateBearerCeiling and
	// ProjectTargetAdmission, built with NewAuthenticatedUser from the
	// stored issuer row (nil Identity when the row is missing).
	issuerPC PrincipalContext
	// memo is the request's project-admission memo, shared by
	// decideAgentDelegation's own admission check and the bearer
	// evaluator.
	memo *ProjectAdmissionCache
}

type delegatedRequestStateKey struct{}

func contextWithDelegatedState(ctx context.Context, st *delegatedRequestState) context.Context {
	return context.WithValue(ctx, delegatedRequestStateKey{}, st)
}

func delegatedStateFromContext(ctx context.Context) *delegatedRequestState {
	st, _ := ctx.Value(delegatedRequestStateKey{}).(*delegatedRequestState)
	return st
}

// delegatedRouteAdmission is the admission table entry routeGuard matched
// for a delegated request. Only routeGuard sets it.
type delegatedRouteAdmissionKey struct{}

func contextWithDelegatedAdmission(ctx context.Context, entry agentDelegationAdmission) context.Context {
	return context.WithValue(ctx, delegatedRouteAdmissionKey{}, entry)
}

func delegatedAdmissionFromContext(ctx context.Context) (agentDelegationAdmission, bool) {
	entry, ok := ctx.Value(delegatedRouteAdmissionKey{}).(agentDelegationAdmission)
	return entry, ok && entry.Operation != ""
}
