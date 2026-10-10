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
// The clamp is keyed only on the stored kind. It costs nothing for a list
// whose system-scoped bindings are all hub-member or hub-viewer (the common
// case); otherwise it reads each user principal's row once.
type testFixtureGrantClamp struct {
	store.Store

	mu      sync.Mutex
	allowed map[string]bool // role definition IDs of hub-member and hub-viewer
	// fixture caches whether a user ID is a test fixture. A user's kind is
	// immutable (pkg/ent/schema/user.go), so an entry never goes stale.
	fixture map[string]bool
}

// testFixtureClampCacheMax bounds the kind cache; it is reset when full.
const testFixtureClampCacheMax = 10000

// isFixture reports whether userID is a test-fixture user, reading the row
// once per user. A missing user is not a fixture and is not cached.
func (c *testFixtureGrantClamp) isFixture(ctx context.Context, userID string) (bool, error) {
	c.mu.Lock()
	v, ok := c.fixture[userID]
	c.mu.Unlock()
	if ok {
		return v, nil
	}
	u, err := c.Store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return false, nil
		}
		return false, err
	}
	v = u.IsTestFixture()
	c.mu.Lock()
	if c.fixture == nil || len(c.fixture) >= testFixtureClampCacheMax {
		c.fixture = make(map[string]bool)
	}
	c.fixture[userID] = v
	c.mu.Unlock()
	return v, nil
}

// wrapAuthzStoreWithTestFixtureClamp wraps s with the clamp. A nil store is
// returned unchanged.
func wrapAuthzStoreWithTestFixtureClamp(s store.Store) store.Store {
	if s == nil {
		return nil
	}
	return &testFixtureGrantClamp{Store: s}
}

// allowedRoleIDs returns the role definition IDs a test identity may hold at
// system scope. The result is cached once both lookups succeed; until then
// an empty set is returned, which only makes the clamp check rows more often.
func (c *testFixtureGrantClamp) allowedRoleIDs(ctx context.Context) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allowed != nil {
		return c.allowed
	}
	ids := map[string]bool{}
	for _, name := range []string{store.SystemRoleHubMember, store.SystemRoleHubViewer} {
		rd, err := c.Store.GetRoleDefinitionByName(ctx, name, store.RoleScopeSystem)
		if err != nil || rd == nil {
			return ids
		}
		ids[rd.ID] = true
	}
	c.allowed = ids
	return ids
}

// clamp drops the system-scoped bindings a test identity may not hold when
// principals include a test-fixture user.
func (c *testFixtureGrantClamp) clamp(ctx context.Context, principals []store.PrincipalRef, bindings []*store.RoleBinding) ([]*store.RoleBinding, error) {
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
