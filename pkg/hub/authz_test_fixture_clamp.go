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
	"sync"

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
	// fixture caches whether a user ID is a test fixture. A user's kind is
	// immutable (pkg/ent/schema/user.go), so an entry never goes stale.
	fixture map[string]bool
	// roleAllowed caches, per role definition ID, whether it is the
	// hub-member or hub-viewer system role a test identity may hold. Only
	// successful lookups are cached; a failed lookup is retried on the next
	// read and, meanwhile, its bindings are dropped.
	roleAllowed map[string]bool
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
// be clamped stores; otherwise it does nothing.
func shareTestFixtureClampCache(dst, src store.Store) {
	d, ok1 := dst.(*testFixtureGrantClamp)
	s, ok2 := src.(*testFixtureGrantClamp)
	if ok1 && ok2 && s.cache != nil {
		d.cache = s.cache
	}
}

// isFixture reports whether userID is a test-fixture user, reading the row
// once per user. Only successful reads of an existing row are cached (a
// kind is immutable); a missing user is not a fixture and is not cached; a
// read error is returned and never cached.
func (c *testFixtureGrantClamp) isFixture(ctx context.Context, userID string) (bool, error) {
	c.cache.mu.Lock()
	v, ok := c.cache.fixture[userID]
	c.cache.mu.Unlock()
	if ok {
		return v, nil
	}
	v, found, err := lookupUserKind(ctx, c.Store, userID)
	if err != nil {
		return false, err
	}
	if !found {
		// No row: not a fixture, and not cached (only successful reads
		// of an existing, immutable kind are cached).
		return false, nil
	}
	c.cache.mu.Lock()
	if c.cache.fixture == nil || len(c.cache.fixture) >= testFixtureClampCacheMax {
		c.cache.fixture = make(map[string]bool)
	}
	c.cache.fixture[userID] = v
	c.cache.mu.Unlock()
	return v, nil
}

// lookupUserKind is the one place authorization reads whether a user is a
// hub test identity, and whether the user row exists (so callers cache only
// successful reads). It fails closed: a store error other than a missing
// user returns the error, and callers must treat that as "clamp or deny",
// never as an ordinary user. A missing or malformed user ID has no row and
// so no grants to clamp; it reports isFixture=false, found=false.
func lookupUserKind(ctx context.Context, users store.UserStore, userID string) (isFixture, found bool, err error) {
	u, err := users.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return false, false, nil
		}
		return false, false, err
	}
	return u.IsTestFixture(), true, nil
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

func (c *testFixtureGrantClamp) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	bindings, err := c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
	if err != nil {
		return bindings, err
	}
	return c.clamp(ctx, principals, bindings)
}

func (c *testFixtureGrantClamp) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	bindings, err := c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil {
		return bindings, err
	}
	return c.clamp(ctx, []store.PrincipalRef{{Type: principalType, ID: principalID}}, bindings)
}
