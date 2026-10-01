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
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Typed-nil identity guards: IsUnscopedLocalPlatformAdmin and the context
// getters in identity.go.
// =============================================================================

// TestIsUnscopedLocalPlatformAdmin_TypedNilTreatedAsMissing covers, for every
// concrete UserIdentity type in identityInventoryExpectation, a UserIdentity
// interface value that is not == nil but holds a nil pointer of that type
// (e.g. a UserIdentity holding (*AuthenticatedUser)(nil)). The plain
// `user == nil` check used before isNilIdentity does not catch this case,
// and user.Role() dereferences the nil pointer next. isNilIdentity makes
// IsUnscopedLocalPlatformAdmin deny before reaching user.Role().
func TestIsUnscopedLocalPlatformAdmin_TypedNilTreatedAsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		user UserIdentity
	}{
		{"AuthenticatedUser", (*AuthenticatedUser)(nil)},
		{"ScopedUserIdentity", (*ScopedUserIdentity)(nil)},
		{"DevUser", (*DevUser)(nil)},
		{"FederatedUserIdentity", (*FederatedUserIdentity)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A plain Go interface comparison, not require.NotNil/assert.NotNil:
			// testify's NotNil unwraps pointer kinds via reflection and would
			// report this typed-nil value as nil, defeating the point of the
			// case.
			require.True(t, tc.user != nil, "the interface value under test must be typed-nil, not a nil interface")

			var result bool
			require.NotPanics(t, func() {
				result = IsUnscopedLocalPlatformAdmin(tc.user)
			}, "IsUnscopedLocalPlatformAdmin must not panic on a typed-nil %s", tc.name)
			assert.False(t, result, "a typed-nil %s must not be treated as an unscoped local platform admin", tc.name)
		})
	}
}

// TestGetIdentityFromContext_TypedNilTreatedAsMissing covers, for every
// concrete type in identityInventoryExpectation, a context carrying a
// non-nil Identity interface value that holds a nil concrete pointer of that
// type under identityContextKey. GetIdentityFromContext's first branch reads
// that value straight back out of the context; without isNilIdentity it
// would hand the typed-nil value to the caller instead of a true nil
// interface.
func TestGetIdentityFromContext_TypedNilTreatedAsMissing(t *testing.T) {
	for name := range identityInventoryExpectation {
		t.Run(name, func(t *testing.T) {
			var stored Identity
			switch name {
			case "AuthenticatedUser":
				stored = (*AuthenticatedUser)(nil)
			case "ScopedUserIdentity":
				stored = (*ScopedUserIdentity)(nil)
			case "DevUser":
				stored = (*DevUser)(nil)
			case "agentIdentityWrapper":
				stored = (*agentIdentityWrapper)(nil)
			case "storedAgentIdentity":
				stored = (*storedAgentIdentity)(nil)
			case "peerAgentIdentity":
				stored = (*peerAgentIdentity)(nil)
			case "explainAgentIdentity":
				stored = (*explainAgentIdentity)(nil)
			case "hubDeliveryIdentity":
				stored = (*hubDeliveryIdentity)(nil)
			case "brokerIdentityImpl":
				stored = (*brokerIdentityImpl)(nil)
			case "FederatedUserIdentity":
				stored = (*FederatedUserIdentity)(nil)
			case "FederatedAgentIdentity":
				stored = (*FederatedAgentIdentity)(nil)
			case "FederatedServiceIdentity":
				stored = (*FederatedServiceIdentity)(nil)
			default:
				t.Fatalf("no typed-nil case constructed for inventory type %q; add one here", name)
			}
			require.True(t, stored != nil, "the interface value under test must be typed-nil, not a nil interface")

			ctx := contextWithIdentity(context.Background(), stored)

			var got Identity
			require.NotPanics(t, func() {
				got = GetIdentityFromContext(ctx)
			}, "GetIdentityFromContext must not panic on a typed-nil %s in context", name)
			// A plain interface comparison: got must be a true nil interface,
			// not a typed-nil value that merely reads as "missing" under some
			// other check.
			require.True(t, got == nil, "GetIdentityFromContext must normalize a typed-nil %s to a nil interface value", name)
		})
	}
}

// TestGetUserIdentityFromContext_TypedNilTreatedAsMissing covers, for every
// concrete UserIdentity type in identityInventoryExpectation, a context
// carrying a typed-nil UserIdentity. Without isNilIdentity, the type
// assertion to UserIdentity in GetUserIdentityFromContext would succeed on
// the typed-nil value and hand it back to the caller.
func TestGetUserIdentityFromContext_TypedNilTreatedAsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		user UserIdentity
	}{
		{"AuthenticatedUser", (*AuthenticatedUser)(nil)},
		{"ScopedUserIdentity", (*ScopedUserIdentity)(nil)},
		{"DevUser", (*DevUser)(nil)},
		{"FederatedUserIdentity", (*FederatedUserIdentity)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, tc.user != nil, "the interface value under test must be typed-nil, not a nil interface")

			ctx := contextWithIdentity(context.Background(), tc.user)

			var got UserIdentity
			require.NotPanics(t, func() {
				got = GetUserIdentityFromContext(ctx)
			}, "GetUserIdentityFromContext must not panic on a typed-nil %s in context", tc.name)
			require.True(t, got == nil, "GetUserIdentityFromContext must normalize a typed-nil %s to a nil interface value", tc.name)
		})
	}
}

// TestGetAgentIdentityFromContext_TypedNilTreatedAsMissing covers, for every
// concrete AgentIdentity type in identityInventoryExpectation, a context
// carrying a typed-nil AgentIdentity. Without isNilIdentity, the type
// assertion to AgentIdentity in GetAgentIdentityFromContext would succeed on
// the typed-nil value and hand it back to the caller.
func TestGetAgentIdentityFromContext_TypedNilTreatedAsMissing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent AgentIdentity
	}{
		{"agentIdentityWrapper", (*agentIdentityWrapper)(nil)},
		{"storedAgentIdentity", (*storedAgentIdentity)(nil)},
		{"peerAgentIdentity", (*peerAgentIdentity)(nil)},
		{"explainAgentIdentity", (*explainAgentIdentity)(nil)},
		{"hubDeliveryIdentity", (*hubDeliveryIdentity)(nil)},
		{"FederatedAgentIdentity", (*FederatedAgentIdentity)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, tc.agent != nil, "the interface value under test must be typed-nil, not a nil interface")

			ctx := contextWithIdentity(context.Background(), tc.agent)

			var got AgentIdentity
			require.NotPanics(t, func() {
				got = GetAgentIdentityFromContext(ctx)
			}, "GetAgentIdentityFromContext must not panic on a typed-nil %s in context", tc.name)
			require.True(t, got == nil, "GetAgentIdentityFromContext must normalize a typed-nil %s to a nil interface value", tc.name)
		})
	}
}

// TestRequestAuthAttrs_TypedNilTreatedAsMissing covers requestAuthAttrs's use
// of isNilIdentity: a context carrying a typed-nil identity must not reach
// identity.ID(), and must produce no request-log auth attributes.
func TestRequestAuthAttrs_TypedNilTreatedAsMissing(t *testing.T) {
	var nilUser *AuthenticatedUser
	ctx := contextWithIdentity(context.Background(), nilUser)

	var attrs []slog.Attr
	require.NotPanics(t, func() {
		attrs = requestAuthAttrs(ctx)
	}, "requestAuthAttrs must not panic on a typed-nil identity in context")
	assert.Empty(t, attrs, "a typed-nil identity must not produce request-log auth attributes")
}
