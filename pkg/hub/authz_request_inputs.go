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

// Request-local authorization input reuse.
//
// This file memoizes principal-bound authorization INPUTS — never
// decisions — for one bounded, read-only evaluation phase of one request,
// so that ComputeCapabilitiesBatch and the list handlers do not reload the
// same principal closure, role bindings, role definitions and
// access-constraint table once per (resource, action) decision.
//
// Non-goals, restated because they bound every function below: no decision,
// capability list or kernel result is ever cached; nothing crosses requests
// or principals; the delegation ceiling's delegator side (getEffectivePermissions,
// resolveUserDelegatorAuthority/evaluateUserDelegatorAuthority/userRelationshipAuthority,
// checkAgentHoldsPermission, IsSystemAdmin, migrationSentinelCeiling) is
// untouched and runs on a masked ctx that this memo is invisible to, except
// for delegation edges, which are the one ceiling input shared per phase.
// decide's relationship candidates run with both keys masked, so lookups
// made there for a progeny source user or a source agent's delegation chain
// never see the requester's memo. The one exception is the project-access
// stage (relationshipProjectAccessStage): it evaluates the requester's own
// project access and reads the requester's memo through the request
// context; every other relationship stage stays masked.

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errAuthzInputsOutOfOrder is returned by principalInputs.Bindings or
// .RoleDefs when called before their dependency has been loaded. This is a
// programming error, never a runtime condition, and the caller (decide,
// ResolveListScopes) fails closed on any non-nil error exactly as it does
// for a store failure.
var errAuthzInputsOutOfOrder = errors.New("authz input handle: dependency not loaded")

// principalKey identifies one principal's memo entry: the normalized
// principal type (NormalizePrincipalType(identity.Type())) and its ID. This
// is exactly the pair authorizationPrincipals uses to key its group lookup
// (in authz.go; match by name, not line number, since the call site moves
// as unrelated code lands above it), so two
// references to the same principal — the decision's own principal, a
// messageability target, a delegator reached through a different path —
// always land in the same entry, and different principals never collide.
type principalKey struct{ normType, id string }

// memoEntry holds one principal's memoized, UNFILTERED authorization inputs:
// the full closure from authorizationPrincipals, the bindings for exactly
// that closure with (nil, nil) scope filters, and the role definitions for
// exactly those bindings. Each slot is populated at most once per entry
// (first successful store wins); a failed load is never stored. roleDefs is
// read-only once stored — callers must maps.Clone it before mutating.
type memoEntry struct {
	refs   []store.PrincipalRef
	refsOK bool

	bindings   []*store.RoleBinding
	bindingsOK bool

	roleDefs   map[string]*RolePermissions
	roleDefsOK bool
}

// authzInputMemo memoizes principal-bound authorization inputs for one
// bounded, read-only evaluation phase of one request. Successful loads
// only. A failed load is returned to the caller and not stored; the next
// decision retries.
//
// The mutex guards map and slot access only; loads run outside the lock and
// there is no single-flight. Every production batch path evaluates
// decisions sequentially today, so concurrent access is defensive, not
// load-bearing — it is exercised by test X2b under -race.
type authzInputMemo struct {
	mu sync.Mutex

	principals map[principalKey]*memoEntry

	// constraints is the memoized global access-constraint table. nil means
	// not yet loaded. It is a pointer so that a successfully loaded, empty
	// slice is distinguishable from "not loaded".
	constraints *[]*store.AccessConstraint

	// edges memoizes delegation-edge lookups, keyed by "delegateType:delegateID"
	// (the exact argument pair GetDelegationEdgesForDelegate takes). Success
	// only. This is the ONLY ceiling input this memo ever serves — see
	// maskAuthzInputs below.
	edges map[string][]*store.DelegationEdge
}

// authzMemoHolder is the value stored under each of the two context keys
// below. masked=true represents a sticky mask: no memo is visible through
// this key, and the mask cannot be undone by a later withAuthzInputMemo
// call. When masked is false, memo is the shared *authzInputMemo for this
// phase (never nil in that case).
type authzMemoHolder struct {
	memo   *authzInputMemo
	masked bool
}

type authzInputsContextKey struct{}
type authzEdgesContextKey struct{}

// withAuthzInputMemo installs a fresh authorization-input memo for the
// current ctx, unless one (or a mask) is already present, in which case it
// is a no-op — installation is idempotent and the mask is sticky.
//
// Install only where no store write that could change authorization inputs
// happens between the install and the last use of the resulting ctx, and
// never inside CanMintSelector or anything it calls (see maskAllAuthzMemo).
// The value dies with the call or request: there is no package-level
// variable and no AuthzService field, so nothing here can leak across
// requests or principals.
func withAuthzInputMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(authzInputsContextKey{}).(*authzMemoHolder); ok {
		return ctx
	}
	holder := &authzMemoHolder{memo: &authzInputMemo{
		principals: make(map[principalKey]*memoEntry),
		edges:      make(map[string][]*store.DelegationEdge),
	}}
	ctx = context.WithValue(ctx, authzInputsContextKey{}, holder)
	ctx = context.WithValue(ctx, authzEdgesContextKey{}, holder)
	return ctx
}

// authzInputMemoFromContext returns the principal/constraint memo installed
// by withAuthzInputMemo, or nil if absent or masked (maskAuthzInputs,
// maskAllAuthzMemo). A nil return means every caller must perform today's
// load, exactly as if no memo code existed.
func authzInputMemoFromContext(ctx context.Context) *authzInputMemo {
	h, ok := ctx.Value(authzInputsContextKey{}).(*authzMemoHolder)
	if !ok || h.masked {
		return nil
	}
	return h.memo
}

// delegationEdgesMemoFromContext returns the memo's edges slot, or nil if
// absent or masked. This key is left untouched by maskAuthzInputs, which is
// exactly how the delegation ceiling shares edges across decisions while
// every other ceiling input (the delegator's own closure, bindings, role
// definitions and constraints) stays per-decision and unmemoized. Read only
// by getCachedDelegationEdges.
func delegationEdgesMemoFromContext(ctx context.Context) *authzInputMemo {
	h, ok := ctx.Value(authzEdgesContextKey{}).(*authzMemoHolder)
	if !ok || h.masked {
		return nil
	}
	return h.memo
}

// maskAuthzInputs hides the principal/constraint memo (inputsKey) from
// everything reached from the returned ctx, without touching the edges key.
// It is applied in three places: decide wraps the ctx passed into
// checkDelegationCeiling (the step-10 call site), and the chain walk
// (walkDelegationChainWithCause) and the execution-project stage
// (executionProjectAdmission) apply it to their own ctx at entry, so every
// current and future caller of either gets it. The delegator side of the
// ceiling — both getEffectivePermissions calls and their constraint loads,
// IsSystemAdmin, GetUser, resolveUserDelegatorAuthority/
// evaluateUserDelegatorAuthority/userRelationshipAuthority and
// migrationSentinelCeiling — and the execution source user's project
// admission therefore see no input memo. Only delegation edges remain
// shared under this mask.
//
// decide's relationship-candidate step (step 9) runs under the stronger
// maskAllAuthzMemo instead, so nothing reached from it — including the
// chain walk via relationshipSourceDelegationHolds — observes either key,
// except the project-access stage (relationshipProjectAccessStage), which
// evaluates the requester's own access on the request context and so reads
// the requester's memo.
func maskAuthzInputs(ctx context.Context) context.Context {
	return context.WithValue(ctx, authzInputsContextKey{}, &authzMemoHolder{masked: true})
}

// maskAllAuthzMemo hides both the principal/constraint memo and the edges
// memo. CanMintSelector calls this at entry, defensively: it has no
// production caller today, only tests, but never installing
// authzInputMemo inside CanMintSelector or its callees is an invariant, and
// this makes it true even if a caller is added later or an outer memo is
// already present in the ctx. The mask is sticky (withAuthzInputMemo is a
// no-op under it), so nothing reached underneath can re-enable a memo
// (see TestParity_X6_MaskStickinessUnit).
func maskAllAuthzMemo(ctx context.Context) context.Context {
	holder := &authzMemoHolder{masked: true}
	ctx = context.WithValue(ctx, authzInputsContextKey{}, holder)
	ctx = context.WithValue(ctx, authzEdgesContextKey{}, holder)
	return ctx
}

// entryFor returns (creating if necessary) the memo entry for key.
func (m *authzInputMemo) entryFor(key principalKey) *memoEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.principals[key]
	if !ok {
		e = &memoEntry{}
		m.principals[key] = e
	}
	return e
}

// principalInputs is a per-decision handle over one principal's UNFILTERED
// authorization inputs, returned by AuthzService.inputsFor. It serves no
// query shape other than the one authorizationPrincipals / decide steps
// 2-4 use: the full closure, its bindings with (nil, nil) scope filters, and
// role definitions for exactly those bindings. Callers that filter by scope
// or query a different ref set — authz_candelegate.go, authz_boundary.go's
// project-scoped grant resolvers, hasActiveSystemRole, getEffectivePermissions
// — must NOT use it; they keep their own direct loads, which this design
// does not touch. ProjectMembershipEvidence uses it (via inputsForPrincipal)
// because it issues exactly this unfiltered closure-plus-bindings query.
//
// A handle is used for exactly one decision (or one ResolveListScopes call)
// and must not be reused across decisions or shared between goroutines.
type principalInputs struct {
	ctx      context.Context
	identity Identity
	a        *AuthzService
	memo     *authzInputMemo // nil when no memo is in ctx. ctx doneness is checked per call, not at construction.
	key      principalKey

	principalsLoaded bool
	principalsVal    []store.PrincipalRef
	principalsErr    error
	// principalsEntry is the memo entry this handle's Principals() value was
	// served from or stored into. nil means "no entry to consume from" —
	// either there is no memo, the principal type isn't memoized, the ctx
	// was done, or this handle lost the first-store race (detached; see
	// Principals).
	principalsEntry *memoEntry

	bindingsLoaded bool
	bindingsVal    []*store.RoleBinding
	bindingsErr    error
	bindingsEntry  *memoEntry // consumable by RoleDefs only if non-nil
}

// inputsFor returns a handle over identity's authorization inputs for ctx.
// See principalInputs for what it may and may not be used for.
func (a *AuthzService) inputsFor(ctx context.Context, identity Identity) *principalInputs {
	h := &principalInputs{ctx: ctx, identity: identity, a: a}
	if identity != nil {
		h.key = principalKey{normType: NormalizePrincipalType(identity.Type()), id: identity.ID()}
	}
	h.memo = authzInputMemoFromContext(ctx)
	return h
}

// inputsForPrincipal returns a memo-backed handle for principal, or nil when
// no memo is visible in ctx, principal carries no Identity, the principal
// type is not memoized, or principal.Identity does not resolve to the same
// principalKey as principal.Kind/ID. A nil return means the caller keeps its
// own direct load. Used by ProjectMembershipEvidence (authz_boundary.go),
// whose closure and unscoped-bindings query is the same shape this memo
// serves for decide.
func (a *AuthzService) inputsForPrincipal(ctx context.Context, principal PrincipalContext) *principalInputs {
	if principal.Identity == nil || authzInputMemoFromContext(ctx) == nil {
		return nil
	}
	h := a.inputsFor(ctx, principal.Identity)
	want := principalKey{normType: NormalizePrincipalType(string(principal.Kind)), id: principal.ID}
	if h.key != want || !h.memoEligible() {
		return nil
	}
	return h
}

// memoEligible reports whether this handle's principal type is one that
// authorizationPrincipals actually resolves groups for. Every other type
// (e.g. "group", "system", "broker") makes no store call today
// (authz.go, match by name, not line number), so the handle passes it
// straight through unmemoized rather than paying for entry bookkeeping
// that would never be read twice.
func (h *principalInputs) memoEligible() bool {
	return h.key.normType == "user" || h.key.normType == "agent"
}

// Principals loads the principal's closure (step 2). It must be called
// before Bindings.
func (h *principalInputs) Principals() ([]store.PrincipalRef, error) {
	if h.principalsLoaded {
		return h.principalsVal, h.principalsErr
	}
	h.principalsLoaded = true

	// Done-ctx bypass: on a cancelled/expired ctx, the memo is bypassed
	// entirely. Today's store call is made with today's ctx and its result
	// is returned verbatim; nothing is stored. This is what makes
	// post-cancellation behaviour identical to today's by construction,
	// whatever the store does with a done ctx.
	if h.memo == nil || !h.memoEligible() || h.ctx.Err() != nil {
		h.principalsVal, h.principalsErr = h.a.authorizationPrincipals(h.ctx, h.identity)
		return h.principalsVal, h.principalsErr
	}

	entry := h.memo.entryFor(h.key)

	h.memo.mu.Lock()
	if entry.refsOK {
		refs := entry.refs
		h.memo.mu.Unlock()
		h.principalsVal = refs
		h.principalsEntry = entry
		return refs, nil
	}
	h.memo.mu.Unlock()

	refs, err := h.a.authorizationPrincipals(h.ctx, h.identity)
	if err != nil {
		h.principalsErr = err
		return nil, err
	}

	h.memo.mu.Lock()
	if entry.refsOK {
		// Lost the first-store race: use our own freshly loaded value for
		// this decision, but do not consume or overwrite the winner's slot.
		// This handle is now detached for its dependent steps.
		h.memo.mu.Unlock()
		h.principalsVal = refs
		h.principalsEntry = nil
		return refs, nil
	}
	entry.refs = refs
	entry.refsOK = true
	h.memo.mu.Unlock()

	h.principalsVal = refs
	h.principalsEntry = entry
	return refs, nil
}

// Bindings loads the role bindings for exactly the closure this handle's
// Principals() produced (step 3). Calling it before Principals is a
// programming error and fails closed.
func (h *principalInputs) Bindings() ([]*store.RoleBinding, error) {
	if !h.principalsLoaded {
		return nil, errAuthzInputsOutOfOrder
	}
	if h.bindingsLoaded {
		return h.bindingsVal, h.bindingsErr
	}
	h.bindingsLoaded = true

	if h.principalsErr != nil {
		h.bindingsErr = h.principalsErr
		return nil, h.principalsErr
	}

	// Bypass whenever there is nothing to consume from (no memo, principal
	// type not memoized, this handle detached on Principals) or the ctx is
	// done.
	if h.memo == nil || h.principalsEntry == nil || h.ctx.Err() != nil {
		h.bindingsVal, h.bindingsErr = h.a.store.ListRoleBindingsForPrincipals(h.ctx, h.principalsVal, nil, nil)
		return h.bindingsVal, h.bindingsErr
	}

	entry := h.principalsEntry
	memo := h.memo

	memo.mu.Lock()
	if entry.bindingsOK {
		bindings := entry.bindings
		memo.mu.Unlock()
		h.bindingsVal = bindings
		h.bindingsEntry = entry
		return bindings, nil
	}
	memo.mu.Unlock()

	bindings, err := h.a.store.ListRoleBindingsForPrincipals(h.ctx, h.principalsVal, nil, nil)
	if err != nil {
		h.bindingsErr = err
		return nil, err
	}

	memo.mu.Lock()
	if entry.bindingsOK {
		memo.mu.Unlock()
		h.bindingsVal = bindings
		h.bindingsEntry = nil
		return bindings, nil
	}
	entry.bindings = bindings
	entry.bindingsOK = true
	memo.mu.Unlock()

	h.bindingsVal = bindings
	h.bindingsEntry = entry
	return bindings, nil
}

// RoleDefs loads the role definitions for exactly the bindings this handle's
// Bindings() produced (step 4), keyed by the definition IDs those bindings
// reference. Calling it before Bindings is a programming error and fails
// closed. The returned map is always a fresh maps.Clone when served from the
// memo, because decide writes synthetic roles into its own copy
// (in authz.go; match by name, not line number)
// and the memo's stored map must stay pristine for every other decision
// that reads it.
func (h *principalInputs) RoleDefs() (map[string]*RolePermissions, error) {
	if !h.bindingsLoaded {
		return nil, errAuthzInputsOutOfOrder
	}
	if h.bindingsErr != nil {
		return nil, h.bindingsErr
	}

	roleDefIDs := collectRoleDefinitionIDs(h.bindingsVal)

	if h.memo == nil || h.bindingsEntry == nil || h.ctx.Err() != nil {
		return h.a.loadRoleDefinitions(h.ctx, roleDefIDs)
	}

	entry := h.bindingsEntry
	memo := h.memo

	memo.mu.Lock()
	if entry.roleDefsOK {
		rd := entry.roleDefs
		memo.mu.Unlock()
		return maps.Clone(rd), nil
	}
	memo.mu.Unlock()

	defs, err := h.a.loadRoleDefinitions(h.ctx, roleDefIDs)
	if err != nil {
		return nil, err
	}

	memo.mu.Lock()
	if !entry.roleDefsOK {
		entry.roleDefs = defs
		entry.roleDefsOK = true
	}
	stored := entry.roleDefs
	memo.mu.Unlock()

	return maps.Clone(stored), nil
}
