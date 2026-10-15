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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Grant shaping (ptone/scion#4240, admin tier phase A3; design §4.5).
//
// A test identity with an admin role may create authority on its test hub,
// but nothing it creates outlives it. The store applies the rule to every
// grant write (store.GrantShaping, entadapter grantShapingFor): a role
// binding at any scope (including project owner and admin grants through
// project membership), a group membership, an invite code or a broker join
// token gets an expiry no later than the identity's and its issuance ID;
// a custom role gets the issuance ID. The identity's purge removes every
// row carrying it (purgeTestAdminGrants).
//
// This file wires the request side:
//
//   - grantShapingMiddleware puts the shaping on the context of every
//     request an admitted admin-kind identity makes (from the row the JWT
//     row block read, contextWithTestAdminActor), and records every
//     authenticated user as the request's grant actor
//     (store.ContextWithGrantActor), from the identity, independently of
//     the row block. The store refuses a grant write whose actor is an
//     admin-kind identity without a matching shaping.
//   - refuseTestAdminUnshapedGrant is the same check at CanDelegate.

// grantShapingMiddleware records the request's grant actor and, for an
// admitted admin-kind test identity, its grant shaping. It runs right after
// authentication.
func (s *Server) grantShapingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := contextWithGrantShaping(r.Context())
		if ctx != r.Context() {
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// contextWithGrantShaping returns ctx with the grant actor (any
// authenticated user) and, for an admin-kind actor marked by the row block,
// the grant shaping. An admin-kind actor whose row lacks an issuance ID or
// an expiry gets no shaping, so the store refuses its grant writes.
func contextWithGrantShaping(ctx context.Context) context.Context {
	if u := GetUserIdentityFromContext(ctx); !isNilIdentity(u) && u.ID() != "" {
		ctx = store.ContextWithGrantActor(ctx, u.ID())
	}
	if ta, ok := testAdminActorFromContext(ctx); ok {
		g := store.GrantShaping{IdentityID: ta.IdentityID, IssuanceID: ta.IssuanceID, ExpiresAt: ta.ExpiresAt}
		if g.Valid() {
			ctx = store.ContextWithGrantShaping(ctx, g)
		}
	}
	return ctx
}

// Reason of refuseTestAdminUnshapedGrant.
const testAdminDelegationReasonUnshaped = "test-admin policy: the grant cannot be bounded by the test identity's expiry (fail-closed)"

// refuseTestAdminUnshapedGrant denies a grant by an admin-kind test
// identity when the request context carries no grant shaping for that
// identity. Every grant such an identity makes must expire with it; the
// store enforces this on the write as well. Agent delegations are not
// written as grant rows: their bound is the effect ceiling
// (testAdminSourceEffectCeiling).
func (a *AuthzService) refuseTestAdminUnshapedGrant(ctx context.Context, actor Identity, grant GrantDescriptor) *Decision {
	user, ok := actor.(UserIdentity)
	if !ok || isNilIdentity(user) || grant.Type == GrantTypeAgentDelegation {
		return nil
	}
	caged, err := a.principalIsCagedTestAdmin(ctx, user.ID())
	if err != nil {
		return &Decision{Allowed: false, Reason: testAdminDelegationReasonUnverified, DeniedBy: DeniedByTestAdminPolicy}
	}
	if !caged {
		return nil
	}
	if g, ok := store.GrantShapingFromContext(ctx); ok && g.Valid() && g.IdentityID == user.ID() {
		return nil
	}
	a.reportTestAdminPolicyDenial(ctx, user.ID(), "delegation", string(grant.Type)+":unshaped")
	return &Decision{Allowed: false, Reason: testAdminDelegationReasonUnshaped, DeniedBy: DeniedByTestAdminPolicy}
}

// Agent delegation clamp (admin tier A3; carry items 2 and 7).
//
// An agent whose provenance root (fixture_root_id, set by the hub at create
// and inherited by every sub-agent) is an admin-kind test identity is never
// issued an agent token scope that covers a permission the test-admin
// policy does not allow. Decide step 0c never fires on an agent principal,
// so this bounds what such an agent can do by construction: the scopes are
// removed at every mint and refresh (ceilingFilteredAgentScopes), and the
// coverage of a sub-agent's edge ceiling is computed from the same
// filtered scopes, so no descendant regains them.

// testAdminScopeAllowed reports whether scope may be issued to an agent
// rooted at an admin-kind test identity: every registry permission it
// covers is allow-classified. A scope with no registry coverage is allowed
// only through its reviewed zeroCoverageScopeMapping permission, when that
// permission is allow-classified; any other scope fails closed.
func testAdminScopeAllowed(scope AgentTokenScope) bool {
	covered := agentScopeCoverage([]AgentTokenScope{scope})
	if len(covered) == 0 {
		perm, ok := zeroCoverageMappedPermission(scope)
		return ok && !testAdminPermissionDenied(perm)
	}
	for _, p := range covered {
		if testAdminPermissionDenied(p) {
			return false
		}
	}
	return true
}

// clampTestAdminRootedScopes returns scopes without the ones
// testAdminScopeAllowed refuses, when agent is rooted at an admin-kind test
// identity; otherwise scopes unchanged. The root is the transitive
// provenance root (fixture_root_id, inherited by every sub-agent) and, as
// defence in depth, the head of the agent's ancestry (Ancestry[0], the
// root the material path and A2's dispatch rule key on): either one being
// an admin kind clamps. A provenance root whose row cannot be found is
// clamped (fail-closed: only test identities are provenance roots); an
// ancestry head with no row is not (humans head ancestries too). A kind
// read error is returned.
func (a *AuthzService) clampTestAdminRootedScopes(ctx context.Context, agent *store.Agent, scopes []AgentTokenScope) ([]AgentTokenScope, error) {
	if agent == nil {
		return scopes, nil
	}
	clamp := false
	if agent.Fixture.RootID != "" {
		clamp = true
		if a != nil {
			k, err := a.kindForPrincipal(ctx, agent.Fixture.RootID)
			if err != nil {
				return nil, err
			}
			clamp = !k.Found || k.isTestAdmin()
		}
	}
	if !clamp && len(agent.Ancestry) > 0 && agent.Ancestry[0] != "" && a != nil {
		k, err := a.kindForPrincipal(ctx, agent.Ancestry[0])
		if err != nil {
			return nil, err
		}
		clamp = k.isTestAdmin()
	}
	if !clamp {
		return scopes, nil
	}
	out := make([]AgentTokenScope, 0, len(scopes))
	for _, sc := range scopes {
		if testAdminScopeAllowed(sc) {
			out = append(out, sc)
		}
	}
	return out, nil
}
