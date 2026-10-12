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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testFixtureGrantClamp caps the hub-level authority of a hub test identity
// (store.User.Kind == test_fixture) where the authorization service reads
// role bindings, so the cap holds whatever path produced a grant: a direct
// binding, a group the identity was added to, a group that gains a binding
// later, or a group the identity created.
//
// For a binding list that includes a test-fixture user principal, every
// system-scoped binding is dropped except those of the hub-member and
// hub-viewer system roles (the grants syncHubRoleGrants gives a member or
// viewer). Project-scoped bindings are untouched: a test identity works in
// projects like any member. Lists that contain no test-fixture user
// principal pass through unchanged, so no other principal's grants change.
//
// Invariant: the clamp recognizes a test identity only by a USER principal
// present in the same list (it reads that principal's kind whenever the
// list holds a system-scoped binding, before classifying any role). A
// caller that evaluates a group's bindings in a list without the member
// user, and then merges the result into a user's authority outside the
// clamp, bypasses it. Callers must resolve a user's
// authority with the user principal and its groups in one list (as the
// authorization service does), never by merging group-only results.
//
// The clamp is keyed only on the stored kind. It costs nothing for a list
// with no system-scoped binding; otherwise it reads each user principal's
// kind once per process, and classifies system roles (once per role) only
// for a test identity (testFixtureClampCache). Other principals never cause
// a role-definition read, so request-level memo counts are unaffected.
type testFixtureGrantClamp struct {
	store.Store
	cache *testFixtureClampCache
}

// testFixtureClampCache holds the clamp's lookups. One cache is shared by
// the server's authorization service and every transaction-bound one
// (Server.authzFor), so each user's kind and each role's class are read
// once.
type testFixtureClampCache struct {
	mu sync.Mutex
	// kinds caches each user's stored kind and identity expiry, read by
	// kindForPrincipalVia. Both are immutable (pkg/ent/schema/user.go), so
	// an entry never goes stale.
	kinds map[string]principalKind
	// fixture caches whether a user ID is a test fixture. It is derived
	// from kinds (written whenever kinds is) and kept for the member
	// clamp's readers.
	fixture map[string]bool
	// roleAllowed caches, per role definition ID, whether it is the
	// hub-member or hub-viewer system role a test identity may hold. Only
	// successful lookups are cached; a failed lookup is retried on the next
	// read and, meanwhile, its bindings are dropped.
	roleAllowed map[string]bool
	// adminTier reports whether an admin test identity kind is enabled on
	// this hub (Server.testAdminTierEnabled). Nil means every admin kind is
	// off, so an authorization service built without the server's wiring
	// gives an admin-kind identity no authority.
	adminTier func(kind string) bool
	// now is the clock of the admin-kind expiry checks. Nil means
	// time.Now.
	now func() time.Time
	// policyDenied, when set, is told about every test-admin policy
	// refusal decide makes (Server.recordTestAdminPolicyDenial).
	policyDenied func(ctx context.Context, userID, permissionID string)
}

// adminTierOn reports whether any admin test identity kind is enabled.
func (c *testFixtureClampCache) adminTierOn() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	tier := c.adminTier
	c.mu.Unlock()
	return tier != nil && (tier(store.UserKindTestHubAdmin) || tier(store.UserKindTestSuperAdmin))
}

// setAuthzTestAdminPolicyDenied wires the hook decide calls on a test-admin
// policy refusal. Server.New calls it once; transaction-bound services
// share the cache.
func setAuthzTestAdminPolicyDenied(svc *AuthzService, fn func(ctx context.Context, userID, permissionID string)) {
	c := svc.kindCache()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.policyDenied = fn
	c.mu.Unlock()
}

// testFixtureClampCacheMax bounds the kind cache; it is reset when full.
const testFixtureClampCacheMax = 10000

// wrapAuthzStoreWithTestFixtureClamp wraps s with the clamp and a new cache.
// A nil store is returned unchanged.
func wrapAuthzStoreWithTestFixtureClamp(s store.Store) store.Store {
	if s == nil {
		return nil
	}
	return &testFixtureGrantClamp{Store: s, cache: &testFixtureClampCache{}}
}

// shareTestFixtureClampCache makes dst's clamp use src's cache. Both must
// be clamped stores; otherwise it does nothing. The cache carries the admin
// tier switch and clock, so a transaction-bound service evaluates admin
// kinds exactly like the server's.
func shareTestFixtureClampCache(dst, src store.Store) {
	d, ok1 := dst.(*testFixtureGrantClamp)
	s, ok2 := src.(*testFixtureGrantClamp)
	if ok1 && ok2 && s.cache != nil {
		d.cache = s.cache
	}
}

// setAuthzTestAdminTier wires the admin test identity tier switch and clock
// into svc's clamp cache. Server.New calls it once, after building the
// authorization service; transaction-bound services share the cache.
func setAuthzTestAdminTier(svc *AuthzService, enabled func(kind string) bool, now func() time.Time) {
	// The cache is shared by pointer, so writing through kindCache()
	// wires the service and every transaction-bound one.
	c := svc.kindCache()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.adminTier = enabled
	c.now = now
	c.mu.Unlock()
}

// isFixture reports whether userID is a test-fixture user, reading the row
// once per user (kindForPrincipalVia). A missing user is not a fixture and
// is not cached; a read error is returned and never cached.
func (c *testFixtureGrantClamp) isFixture(ctx context.Context, userID string) (bool, error) {
	k, err := kindForPrincipalVia(ctx, c.Store, c.cache, userID)
	if err != nil {
		return false, err
	}
	return k.Found && k.Kind == store.UserKindTestFixture, nil
}

// principalKind is the stored kind of a user principal, as read by
// kindForPrincipalVia, the one place hub code reads it for an
// authorization or containment decision.
type principalKind struct {
	// Found is false when the user has no row (or the ID is malformed).
	// A principal that was not found is never human: callers that admit
	// only humans refuse it.
	Found bool
	// Kind is one of the store.UserKind* values when Found.
	Kind string
	// ExpiresAt is the identity's immutable expiry (test identity kinds).
	ExpiresAt *time.Time
}

// isHuman reports whether the principal is an existing human user.
func (k principalKind) isHuman() bool { return k.Found && k.Kind == store.UserKindHuman }

// isTestAdmin reports whether the principal is an admin-kind test identity.
func (k principalKind) isTestAdmin() bool { return k.Found && store.IsTestAdminKind(k.Kind) }

// isTestIdentity reports whether the principal is any test identity kind.
func (k principalKind) isTestIdentity() bool {
	return k.Found && (k.Kind == store.UserKindTestFixture || store.IsTestAdminKind(k.Kind))
}

// errUnknownPrincipalKind is returned for a user row whose kind this binary
// does not recognize. Callers fail closed on it as on a read error: an
// unknown kind never resolves to human or to full authority.
var errUnknownPrincipalKind = errors.New("user principal has an unrecognized kind")

// classifyUserKind classifies a user row already read from the store. An
// empty kind reads as human (the column default); any value outside the
// known kinds is errUnknownPrincipalKind.
func classifyUserKind(u *store.User) (principalKind, error) {
	if u == nil {
		return principalKind{}, nil
	}
	switch u.Kind {
	case "", store.UserKindHuman:
		return principalKind{Found: true, Kind: store.UserKindHuman}, nil
	case store.UserKindTestFixture, store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin:
		return principalKind{Found: true, Kind: u.Kind, ExpiresAt: u.ExpiresAt}, nil
	}
	return principalKind{}, fmt.Errorf("%w: user %s", errUnknownPrincipalKind, u.ID)
}

// kindForPrincipalVia is the one kind read behind every kind-driven site:
// the member and admin grant clamps, the JWT row block, CanDelegate's
// grantee refusal, the issuer refusal, the access-token create refusal,
// super-admin reconciliation and the creation-time provenance stamp. It
// reads through users and caches each successful read of an existing row
// in cache (nil means uncached). It fails closed: a store error other than
// a missing user, or an unrecognized kind, is returned and never cached,
// and callers deny on it. A missing or malformed user ID reports
// Found=false with no error and is not cached.
func kindForPrincipalVia(ctx context.Context, users store.UserStore, cache *testFixtureClampCache, userID string) (principalKind, error) {
	if cache != nil {
		cache.mu.Lock()
		k, ok := cache.kinds[userID]
		cache.mu.Unlock()
		if ok {
			return k, nil
		}
	}
	k, _, err := kindAndRowForPrincipal(ctx, users, userID)
	if err != nil || !k.Found {
		return k, err
	}
	if cache != nil {
		cache.mu.Lock()
		if cache.kinds == nil || len(cache.kinds) >= testFixtureClampCacheMax {
			cache.kinds = make(map[string]principalKind)
			cache.fixture = make(map[string]bool)
		}
		if cache.fixture == nil {
			cache.fixture = make(map[string]bool)
		}
		cache.kinds[userID] = k
		cache.fixture[userID] = k.Kind == store.UserKindTestFixture
		cache.mu.Unlock()
	}
	return k, nil
}

// kindAndRowForPrincipal is kindForPrincipalVia's uncached read, for the
// callers that also need the row's other immutable test identity fields
// (the provenance stamp needs issued_by). Same fail-closed contract.
func kindAndRowForPrincipal(ctx context.Context, users store.UserStore, userID string) (principalKind, *store.User, error) {
	u, found, err := lookupUserForKind(ctx, users, userID)
	if err != nil || !found {
		return principalKind{}, nil, err
	}
	k, err := classifyUserKind(u)
	if err != nil {
		return principalKind{}, nil, err
	}
	return k, u, nil
}

// lookupUserKind reports whether a user is a member-tier test identity
// (kind test_fixture), and whether the user row exists, through
// kindForPrincipalVia (uncached). It fails closed: a store error other
// than a missing user, or an unrecognized kind, returns the error, and
// callers must treat that as "clamp or deny", never as an ordinary user. A
// missing or malformed user ID has no row and so no grants to clamp; it
// reports isFixture=false, found=false.
func lookupUserKind(ctx context.Context, users store.UserStore, userID string) (isFixture, found bool, err error) {
	k, err := kindForPrincipalVia(ctx, users, nil, userID)
	if err != nil || !k.Found {
		return false, k.Found, err
	}
	return k.Kind == store.UserKindTestFixture, true, nil
}

// lookupUserForKind is the user-row read behind kindForPrincipalVia. It has
// the same fail-closed contract: a missing or malformed user ID reports
// found=false with no error; any other store error is returned.
func lookupUserForKind(ctx context.Context, users store.UserStore, userID string) (u *store.User, found bool, err error) {
	u, err = users.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return u, true, nil
}

// allowedRoles reports, for each role definition ID in ids, whether it is
// the hub-member or hub-viewer system role. IDs it cannot classify (a
// lookup failed or the role is missing) are reported false, so their
// bindings are dropped; only successful lookups are cached.
func (c *testFixtureGrantClamp) allowedRoles(ctx context.Context, ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	var missing []string
	c.cache.mu.Lock()
	for _, id := range ids {
		if v, ok := c.cache.roleAllowed[id]; ok {
			out[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	c.cache.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	defs, err := c.GetRoleDefinitionsByIDs(ctx, missing)
	if err != nil {
		return out
	}
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if c.cache.roleAllowed == nil || len(c.cache.roleAllowed) >= testFixtureClampCacheMax {
		c.cache.roleAllowed = make(map[string]bool)
	}
	for _, id := range missing {
		rd, ok := defs[id]
		if !ok || rd == nil {
			continue
		}
		// Only the seeded (System) hub-member and hub-viewer roles count.
		v := rd.System && rd.ScopeType == store.RoleScopeSystem && (rd.Name == store.SystemRoleHubMember || rd.Name == store.SystemRoleHubViewer)
		c.cache.roleAllowed[id] = v
		out[id] = v
	}
	return out
}

// clamp drops the system-scoped bindings a test identity may not hold when
// principals include a test-fixture user. When a list has a system-scoped
// binding it reads each user principal's kind (cached per user); only for a
// test identity does it classify the system roles (cached per role), so
// any other principal costs at most one cached kind read and no role
// lookup, and its bindings are returned unchanged. If a user's kind cannot
// be read it returns the error: every caller then fails closed (the request
// is denied), so an unreadable kind never leaves grants unclamped.
func (c *testFixtureGrantClamp) clamp(ctx context.Context, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	var systemRoleIDs []string
	for _, b := range bindings {
		if b != nil && b.ScopeType == store.RoleScopeSystem {
			systemRoleIDs = append(systemRoleIDs, b.RoleDefinitionID)
		}
	}
	if len(systemRoleIDs) == 0 {
		return bindings, nil
	}
	fixture := false
	for _, p := range principals {
		if p.Type != store.RoleBindingPrincipalUser {
			continue
		}
		isFx, err := c.isFixture(ctx, p.ID)
		if err != nil {
			// Fail closed by design: on a cold cache, an unreadable kind
			// denies this request for ANY principal whose binding list has
			// a system-scoped binding (members and viewers included, since
			// the kind is read before roles are classified), not only for
			// test identities.
			return nil, err
		}
		if isFx {
			fixture = true
			break
		}
	}
	if !fixture {
		return bindings, nil
	}
	allowed := c.allowedRoles(ctx, systemRoleIDs)
	out := make([]*store.RoleBinding, 0, len(bindings))
	for _, b := range bindings {
		if b != nil && b.ScopeType == store.RoleScopeSystem && !allowed[b.RoleDefinitionID] {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// adminTierEnabled reports whether the admin test identity kind is enabled,
// per the cache's wiring. Unwired means off.
func (c *testFixtureClampCache) adminTierEnabled(kind string) bool {
	c.mu.Lock()
	fn := c.adminTier
	c.mu.Unlock()
	return fn != nil && fn(kind)
}

// clock returns the cache's clock, or time.Now.
func (c *testFixtureClampCache) clock() time.Time {
	c.mu.Lock()
	fn := c.now
	c.mu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// clampTestAdmin is the admin-kind sibling of clamp, applied at the same
// store-wrap point to every binding list the authorization service reads
// (ptone/scion#4240, admin tier). It changes nothing for a list without an
// admin-kind (test_hub_admin or test_super_admin) user principal. For a
// list with one it applies the decide-time checks:
//
//   - the identity's kind is not enabled on this hub (the startup gate is
//     off, for example after a restart without the flag): every binding is
//     dropped, so the identity has no authority, even on a path that did
//     not run the JWT row block;
//   - the identity has no expiry, or has expired: every binding is dropped;
//   - a system-scoped binding held directly by the identity with no expiry,
//     an expiry after the identity's, or outside its activation window
//     (isBindingActive) is dropped. The issuance binding carries the
//     identity's expiry, so it stays until then.
//
// Group and project-scoped bindings of a live, enabled identity pass
// through: the identity is faithful to a human of its role there. Like
// clamp, it reads each user principal's kind (cached) and fails closed on a
// read error or an unrecognized kind. It shares clamp's invariant: it sees
// an admin-kind identity only through a USER principal in the same list,
// so callers must evaluate a user's group bindings in one list with the
// user principal.
func (c *testFixtureGrantClamp) clampTestAdmin(ctx context.Context, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	if len(bindings) == 0 {
		return bindings, nil
	}
	var admins map[string]principalKind
	for _, p := range principals {
		if p.Type != store.RoleBindingPrincipalUser {
			continue
		}
		k, err := kindForPrincipalVia(ctx, c.Store, c.cache, p.ID)
		if err != nil {
			return nil, err
		}
		if k.isTestAdmin() {
			if admins == nil {
				admins = make(map[string]principalKind, 1)
			}
			admins[p.ID] = k
		}
	}
	if len(admins) == 0 {
		return bindings, nil
	}
	now := c.cache.clock()
	for _, k := range admins {
		if !c.cache.adminTierEnabled(k.Kind) || k.ExpiresAt == nil || !now.Before(*k.ExpiresAt) {
			return []*store.RoleBinding{}, nil
		}
	}
	out := make([]*store.RoleBinding, 0, len(bindings))
	for _, b := range bindings {
		if b == nil {
			continue
		}
		if b.ScopeType == store.RoleScopeSystem && b.PrincipalType == store.RoleBindingPrincipalUser {
			if k, ok := admins[b.PrincipalID]; ok {
				if b.ExpiresAt == nil || b.ExpiresAt.After(*k.ExpiresAt) || !isBindingActive(b, now) {
					continue
				}
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// clampAll applies the member clamp and then its admin-kind sibling.
func (c *testFixtureGrantClamp) clampAll(ctx context.Context, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	bindings, err := c.clamp(ctx, principals, bindings)
	if err != nil {
		return nil, err
	}
	return c.clampTestAdmin(ctx, principals, bindings)
}

func (c *testFixtureGrantClamp) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	bindings, err := c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
	if err != nil {
		return bindings, err
	}
	return c.clampAll(ctx, principals, bindings)
}

func (c *testFixtureGrantClamp) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	bindings, err := c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil {
		return bindings, err
	}
	return c.clampAll(ctx, []store.PrincipalRef{{Type: principalType, ID: principalID}}, bindings)
}

// clampTestIdentityBindings applies the test identity grant clamps (member
// and admin kind) to bindings read through st, a store handle other than
// the authorization service's (for example a governance check reading
// inside its own transaction). The admin tier switch and clock come from
// authz's clamp, so an admin-kind identity is evaluated exactly as the
// authorization service evaluates it; a nil authz, or one without the
// wiring, leaves every admin kind off. Kind reads go through st, uncached.
// principals must be the list the bindings were read for. A kind-read error
// is returned.
func clampTestIdentityBindings(ctx context.Context, st store.Store, authz *AuthzService, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	cache := &testFixtureClampCache{}
	if src := authz.kindCache(); src != nil {
		src.mu.Lock()
		cache.adminTier, cache.now = src.adminTier, src.now
		src.mu.Unlock()
	}
	c := &testFixtureGrantClamp{Store: st, cache: cache}
	return c.clampAll(ctx, principals, bindings)
}

// clampBindingsForUser applies the test identity clamps (member and admin
// kind) to bindings read for userID outside the authorization service:
// the user's direct bindings, or the bindings of the user's groups read in
// a separate list. The user principal is passed to the clamps explicitly,
// so a group-only read is still evaluated with its member user (the
// clamps' invariant). authz supplies the admin tier wiring (see
// clampTestIdentityBindings). A kind-read error is returned; callers fail
// closed.
func clampBindingsForUser(ctx context.Context, st store.Store, authz *AuthzService, userID string, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	return clampTestIdentityBindings(ctx, st, authz, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: userID}}, bindings)
}
