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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Account-status gate for decide.
//
// A role binding grants nothing to a user whose account is not active. An
// invited user's memberships and role bindings therefore take effect at
// first sign-in, when the same users row becomes active, and a suspended
// or removed user's bindings grant nothing while that remains so.
//
// The rule is activeUserPredicate, the same one requireActiveUser applies
// for each of its callers in authz_boundary.go, so the two cannot drift:
//   - local users (user, dev) need an existing, active users row;
//   - federated users with no users row pass through to the binding check;
//     an existing row that is not active denies;
//   - principals that are not users (agents, groups, ...) are not checked
//     here; an agent's authority is bounded by its delegation chain, which
//     already requires an active delegating user.
//
// One exception, for the gate only: the reserved grantor of an offline
// test-infra delegation has no users row by design. A missing row for that
// principal passes only on a request that decide Step 5b4 grants
// (offlineGrantorStatusExempt, test_identity_offline_delegation.go). An
// existing row with that id still goes through activeUserPredicate, and a
// lookup fault still denies.
//
// decide evaluates the gate once, before the bearer gate and every grant
// stage, so a principal that is not active is denied with a single reason
// whichever stage would otherwise have denied or admitted it.

const (
	// principalNotActiveReason is the deny reason for a user principal
	// whose account is not active (invited, suspended or missing). The
	// cases share one reason on purpose.
	principalNotActiveReason = "user is not active"

	// principalStatusLookupReason is the deny reason when the users-row
	// lookup fails. It is tagged DenyCauseResolutionError.
	principalStatusLookupReason = "user status resolution error (fail-closed)"
)

// principalStatusGate applies activeUserPredicate to the decision's own
// principal. It returns ("", "") when the principal may proceed to the
// grant stages, and otherwise the deny reason and cause. A lookup fault
// denies. credential and permissionID are used only for the offline
// grantor exception (offlineGrantorStatusExempt).
func (a *AuthzService) principalStatusGate(ctx context.Context, principal PrincipalContext, credential CredentialContext, permissionID string) (string, DenyCause) {
	if NormalizePrincipalType(string(principal.Kind)) != store.RoleBindingPrincipalUser {
		return "", ""
	}
	if a.store == nil {
		return principalStatusLookupReason, DenyCauseResolutionError
	}
	user, err := a.userForStatusGate(ctx, principal.ID)
	if errors.Is(err, store.ErrNotFound) && offlineGrantorStatusExempt(ctx, principal, credential, permissionID) {
		return "", ""
	}
	if gateErr := activeUserPredicate(principal, user, err); gateErr != nil {
		if isProjectAccessLookupFault(gateErr) {
			a.logger.Warn("failed to resolve user status for authorization (fail-closed)",
				"principal_id", principal.ID, "error", err)
			return principalStatusLookupReason, DenyCauseResolutionError
		}
		return principalNotActiveReason, ""
	}
	return "", ""
}

// userForStatusGate loads the users row for id. Without a request-local
// authz input memo in ctx it is one GetUser per decision. With one, the
// result (a found row or store.ErrNotFound) is shared by every decision in
// the phase; a store fault is returned and not stored, so the next decision
// retries.
func (a *AuthzService) userForStatusGate(ctx context.Context, id string) (*store.User, error) {
	memo := authzInputMemoFromContext(ctx)
	if memo == nil || ctx.Err() != nil {
		return a.store.GetUser(ctx, id)
	}
	memo.mu.Lock()
	cached, ok := memo.users[id]
	memo.mu.Unlock()
	if ok {
		if cached.notFound {
			return nil, store.ErrNotFound
		}
		return cached.user, nil
	}
	user, err := a.store.GetUser(ctx, id)
	switch {
	case err == nil:
		memo.mu.Lock()
		if _, ok := memo.users[id]; !ok {
			memo.users[id] = memoUser{user: user}
		}
		memo.mu.Unlock()
	case errors.Is(err, store.ErrNotFound):
		memo.mu.Lock()
		if _, ok := memo.users[id]; !ok {
			memo.users[id] = memoUser{notFound: true}
		}
		memo.mu.Unlock()
	}
	return user, err
}
