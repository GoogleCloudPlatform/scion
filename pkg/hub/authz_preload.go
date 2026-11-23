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

const (
	// authzPreloadPrincipalChunkSize bounds principals per
	// ListRoleBindingsForPrincipals call (each principal adds 2 bind params).
	authzPreloadPrincipalChunkSize = 500

	// authzPreloadRoleDefChunkSize bounds role definition IDs per
	// GetRoleDefinitionsByIDs call.
	authzPreloadRoleDefChunkSize = 500
)

// preloadedUserInputs holds one user's preloaded, per-user-clamped
// authorization inputs produced by preloadUserAuthzInputs.
type preloadedUserInputs struct {
	bindings    []*store.RoleBinding
	rawRoleDefs map[string]*store.RoleDefinition
	err         error // per-user fail-closed error (e.g. unrecognized User.Kind)
}

// preloadUserAuthzInputs loads authorization inputs for users (already fetched
// rows, typically <=500) in a fixed number of batched queries and seeds the
// request-local authzInputMemo (when present in ctx) so subsequent CheckAccess
// calls for these users issue no per-user store queries.
//
// Callers are expected to pass a freshly installed authzInputMemo (via
// withAuthzInputMemo). If a principal's memo entry is already partially or
// fully populated, preloadUserAuthzInputs leaves that entry untouched so it
// never pairs preloaded bindings or role definitions with a different closure.
//
// Security invariant: raw role bindings are fetched through the unwrapped
// store and partitioned by principal in memory, then clamped strictly ONE USER
// AT A TIME via clampAllWith using classifyUserKind(users[u]) (zero GetUser
// queries). A multi-user principal list is never passed to the clamp, so a
// test identity or expired test admin in the batch cannot affect any other
// user. An unclassifiable user (unknown User.Kind) fails closed for that user
// only (entry.err != nil, memo not seeded), never for the rest of the batch.
func (a *AuthzService) preloadUserAuthzInputs(ctx context.Context, users map[string]*store.User) (map[string]*preloadedUserInputs, error) {
	if a == nil || a.store == nil {
		return nil, fmt.Errorf("authz preload: store is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return map[string]*preloadedUserInputs{}, nil
	}

	userIDs := make([]string, 0, len(users))
	for uid := range users {
		userIDs = append(userIDs, uid)
	}
	sort.Strings(userIDs)

	out := make(map[string]*preloadedUserInputs, len(userIDs))
	validUserIDs := make([]string, 0, len(userIDs))
	userKinds := make(map[string]principalKind, len(userIDs))

	for _, uid := range userIDs {
		u := users[uid]
		if u == nil {
			out[uid] = &preloadedUserInputs{err: store.ErrNotFound}
			continue
		}
		k, kerr := classifyUserKind(u)
		if kerr != nil {
			out[uid] = &preloadedUserInputs{err: kerr}
			continue
		}
		validUserIDs = append(validUserIDs, uid)
		userKinds[uid] = k
	}
	if len(validUserIDs) == 0 {
		return out, nil
	}

	// Unwrap the store-level clamp so batch ListRoleBindingsForPrincipals is
	// never evaluated across a multi-user principal list. The underlying store
	// (including perfAuthzStore when enabled) is preserved, and the server's
	// clamp cache (admin tier switch, clock, roleAllowed cache) is reused.
	var (
		rawStore store.Store
		clamp    *testFixtureGrantClamp
	)
	if c, ok := a.store.(*testFixtureGrantClamp); ok {
		rawStore = c.Store
		cache := c.cache
		if cache == nil {
			cache = &testFixtureClampCache{}
		}
		clamp = &testFixtureGrantClamp{Store: rawStore, cache: cache}
	} else {
		rawStore = a.store
		clamp = &testFixtureGrantClamp{Store: rawStore, cache: &testFixtureClampCache{}}
	}

	// 1. Batch effective groups for all valid users.
	groupsByUser, err := rawStore.GetEffectiveGroupsForUsers(ctx, validUserIDs)
	if err != nil {
		return nil, fmt.Errorf("authz preload: get effective groups: %w", err)
	}

	refsByUser := make(map[string][]store.PrincipalRef, len(validUserIDs))
	seenPrincipals := make(map[store.PrincipalRef]struct{}, len(validUserIDs)*2)
	allPrincipals := make([]store.PrincipalRef, 0, len(validUserIDs)*2)

	for _, uid := range validUserIDs {
		groups := groupsByUser[uid]
		refs := make([]store.PrincipalRef, 0, 1+len(groups))
		userRef := store.PrincipalRef{Type: store.RoleBindingPrincipalUser, ID: uid}
		refs = append(refs, userRef)
		if _, ok := seenPrincipals[userRef]; !ok {
			seenPrincipals[userRef] = struct{}{}
			allPrincipals = append(allPrincipals, userRef)
		}
		for _, gid := range groups {
			gRef := store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid}
			refs = append(refs, gRef)
			if _, ok := seenPrincipals[gRef]; !ok {
				seenPrincipals[gRef] = struct{}{}
				allPrincipals = append(allPrincipals, gRef)
			}
		}
		refsByUser[uid] = refs
	}

	// 2. Batch raw (unclamped) role bindings across the union of user and group
	// principals, chunked to stay well under SQL bind-parameter limits.
	bindingsByPrincipal := make(map[store.PrincipalRef][]*store.RoleBinding, len(allPrincipals))
	for start := 0; start < len(allPrincipals); start += authzPreloadPrincipalChunkSize {
		end := start + authzPreloadPrincipalChunkSize
		if end > len(allPrincipals) {
			end = len(allPrincipals)
		}
		chunkBindings, berr := rawStore.ListRoleBindingsForPrincipals(ctx, allPrincipals[start:end], nil, nil)
		if berr != nil {
			return nil, fmt.Errorf("authz preload: list role bindings: %w", berr)
		}
		for _, rb := range chunkBindings {
			if rb == nil {
				continue
			}
			ref := store.PrincipalRef{Type: rb.PrincipalType, ID: rb.PrincipalID}
			bindingsByPrincipal[ref] = append(bindingsByPrincipal[ref], rb)
		}
	}

	// 3. Partition bindings per user and apply test-identity clamps strictly
	// one user at a time using the already-classified principalKind.
	clampedByUser := make(map[string][]*store.RoleBinding, len(validUserIDs))
	roleDefIDSeen := make(map[string]struct{})
	allRoleDefIDs := make([]string, 0)

	for _, uid := range validUserIDs {
		refs := refsByUser[uid]
		rawUserBindings := make([]*store.RoleBinding, 0)
		for _, ref := range refs {
			rawUserBindings = append(rawUserBindings, bindingsByPrincipal[ref]...)
		}

		uk := userKinds[uid]
		expectedUID := uid
		kindOf := func(_ context.Context, principalUserID string) (principalKind, error) {
			if principalUserID != expectedUID {
				return principalKind{}, fmt.Errorf("authz preload: unexpected user principal %s while clamping %s", principalUserID, expectedUID)
			}
			return uk, nil
		}

		clamped, cerr := clamp.clampAllWith(ctx, refs, rawUserBindings, kindOf)
		if cerr != nil {
			out[uid] = &preloadedUserInputs{err: cerr}
			continue
		}
		clampedByUser[uid] = clamped
		for _, rb := range clamped {
			if rb == nil || rb.RoleDefinitionID == "" {
				continue
			}
			if _, ok := roleDefIDSeen[rb.RoleDefinitionID]; !ok {
				roleDefIDSeen[rb.RoleDefinitionID] = struct{}{}
				allRoleDefIDs = append(allRoleDefIDs, rb.RoleDefinitionID)
			}
		}
	}

	// 4. Batch role definitions for the union of referenced IDs.
	rawDefs := make(map[string]*store.RoleDefinition, len(allRoleDefIDs))
	for start := 0; start < len(allRoleDefIDs); start += authzPreloadRoleDefChunkSize {
		end := start + authzPreloadRoleDefChunkSize
		if end > len(allRoleDefIDs) {
			end = len(allRoleDefIDs)
		}
		chunkDefs, derr := rawStore.GetRoleDefinitionsByIDs(ctx, allRoleDefIDs[start:end])
		if derr != nil {
			return nil, fmt.Errorf("authz preload: get role definitions: %w", derr)
		}
		for id, rd := range chunkDefs {
			rawDefs[id] = rd
		}
	}

	rolePerms := roleDefinitionsToPermissions(rawDefs)

	// 5. Build per-user outputs and seed the request-local authzInputMemo.
	memo := authzInputMemoFromContext(ctx)
	for _, uid := range validUserIDs {
		if _, failed := out[uid]; failed {
			continue
		}
		refs := refsByUser[uid]
		clamped := clampedByUser[uid]

		userRoleDefs := make(map[string]*RolePermissions)
		userRawDefs := make(map[string]*store.RoleDefinition)
		for _, rb := range clamped {
			if rb == nil || rb.RoleDefinitionID == "" {
				continue
			}
			if rp, ok := rolePerms[rb.RoleDefinitionID]; ok {
				userRoleDefs[rb.RoleDefinitionID] = rp
			}
			if rd, ok := rawDefs[rb.RoleDefinitionID]; ok {
				userRawDefs[rb.RoleDefinitionID] = rd
			}
		}

		out[uid] = &preloadedUserInputs{
			bindings:    clamped,
			rawRoleDefs: userRawDefs,
		}

		if memo != nil {
			entry := memo.entryFor(principalKey{normType: store.RoleBindingPrincipalUser, id: uid})
			memo.mu.Lock()
			if _, ok := memo.users[uid]; !ok {
				memo.users[uid] = memoUser{user: users[uid]}
			}
			// Seed refs, bindings, and roleDefs atomically only when the entry
			// is completely empty, so preloaded bindings/roleDefs can never be
			// paired with a pre-existing closure from another load.
			if !entry.refsOK && !entry.bindingsOK && !entry.roleDefsOK {
				entry.refs = refs
				entry.refsOK = true
				entry.bindings = clamped
				entry.bindingsOK = true
				entry.roleDefs = userRoleDefs
				entry.roleDefsOK = true
			}
			memo.mu.Unlock()
		}
	}

	return out, nil
}

// splitUserAndGroupBindings splits a user's combined clamped role-binding list
// into direct user bindings and group bindings.
func splitUserAndGroupBindings(bindings []*store.RoleBinding) (direct, group []*store.RoleBinding) {
	for _, b := range bindings {
		if b == nil {
			continue
		}
		switch b.PrincipalType {
		case store.RoleBindingPrincipalUser:
			direct = append(direct, b)
		case store.RoleBindingPrincipalGroup:
			group = append(group, b)
		}
	}
	return direct, group
}
