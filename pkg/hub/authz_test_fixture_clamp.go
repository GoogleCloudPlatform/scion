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
// present in the same list. A caller that evaluates a group's bindings in a
// list without the member user, and then merges the result into a user's
// authority outside the clamp, bypasses it. Callers must resolve a user's
// authority with the user principal and its groups in one list (as the
// authorization service does), never by merging group-only results.
//
// The clamp is keyed only on the stored kind. It costs nothing for a list
// with no system-scoped binding or whose system-scoped bindings are all
// hub-member or hub-viewer (the common case); otherwise it reads each user
// principal's kind once per process (testFixtureClampCache).
type testFixtureGrantClamp struct {
	store.Store
	cache *testFixtureClampCache
}

// testFixtureClampCache holds the clamp's lookups. One cache is shared by
// the server's authorization service and every transaction-bound one
// (Server.authzFor), so each user's kind is read once.
type testFixtureClampCache struct {
	mu sync.Mutex
	// allowed is the set of role definition IDs of hub-member and
	// hub-viewer. allowedComplete is false when a lookup failed; such a
	// partial set is retried after testFixtureClampAllowedRetry.
	allowed         map[string]bool
	allowedComplete bool
	allowedAt       time.Time
	// fixture caches whether a user ID is a test fixture. A user's kind is
	// immutable (pkg/ent/schema/user.go), so an entry never goes stale.
	fixture map[string]bool
}

// testFixtureClampCacheMax bounds the kind cache; it is reset when full.
const testFixtureClampCacheMax = 10000

// testFixtureClampAllowedRetry is how long a partial allowed-role set is
// kept before the lookups are retried. A partial set only drops more.
const testFixtureClampAllowedRetry = 30 * time.Second

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
// once per user. A missing user is not a fixture and is not cached.
func (c *testFixtureGrantClamp) isFixture(ctx context.Context, userID string) (bool, error) {
	c.cache.mu.Lock()
	v, ok := c.cache.fixture[userID]
	c.cache.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := lookupUserIsTestFixture(ctx, c.Store, userID)
	if err != nil {
		return false, err
	}
	c.cache.mu.Lock()
	if c.cache.fixture == nil || len(c.cache.fixture) >= testFixtureClampCacheMax {
		c.cache.fixture = make(map[string]bool)
	}
	c.cache.fixture[userID] = v
	c.cache.mu.Unlock()
	return v, nil
}

// lookupUserIsTestFixture is the one place authorization reads whether a
// user is a hub test identity. It fails closed: a store error other than a
// missing user returns the error, and callers must treat that as "clamp or
// deny", never as an ordinary user. A missing or malformed user ID has no
// row and so no grants to clamp; it reports false.
func lookupUserIsTestFixture(ctx context.Context, users store.UserStore, userID string) (bool, error) {
	u, err := users.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return false, nil
		}
		return false, err
	}
	return u.IsTestFixture(), nil
}

// allowedRoleIDs returns the role definition IDs a test identity may hold at
// system scope. A complete set is cached for the process; a partial one
// (a lookup failed) is kept for testFixtureClampAllowedRetry and then
// retried. A partial set only makes the clamp drop more.
func (c *testFixtureGrantClamp) allowedRoleIDs(ctx context.Context) map[string]bool {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if c.cache.allowed != nil && (c.cache.allowedComplete || time.Since(c.cache.allowedAt) < testFixtureClampAllowedRetry) {
		return c.cache.allowed
	}
	ids := map[string]bool{}
	complete := true
	for _, name := range []string{store.SystemRoleHubMember, store.SystemRoleHubViewer} {
		rd, err := c.Store.GetRoleDefinitionByName(ctx, name, store.RoleScopeSystem)
		if err != nil || rd == nil {
			complete = false
			continue
		}
		ids[rd.ID] = true
	}
	c.cache.allowed, c.cache.allowedComplete, c.cache.allowedAt = ids, complete, time.Now()
	return ids
}

// clamp drops the system-scoped bindings a test identity may not hold when
// principals include a test-fixture user. If a user's kind cannot be read,
// it returns the error: every caller then fails closed (the request is
// denied), so an unreadable kind never leaves grants unclamped.
func (c *testFixtureGrantClamp) clamp(ctx context.Context, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
	hasSystem := false
	for _, b := range bindings {
		if b != nil && b.ScopeType == store.RoleScopeSystem {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		return bindings, nil
	}
	allowed := c.allowedRoleIDs(ctx)
	privileged := false
	for _, b := range bindings {
		if b != nil && b.ScopeType == store.RoleScopeSystem && !allowed[b.RoleDefinitionID] {
			privileged = true
			break
		}
	}
	if !privileged {
		return bindings, nil
	}
	fixture := false
	for _, p := range principals {
		if p.Type != store.RoleBindingPrincipalUser {
			continue
		}
		isFx, err := c.isFixture(ctx, p.ID)
		if err != nil {
			// Fail closed by design: an unreadable kind denies this
			// request, even for a non-fixture admin on a cold cache.
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
