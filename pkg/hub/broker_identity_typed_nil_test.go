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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Typed-nil BrokerIdentity guards (ptone/scion#2475): GetBrokerIdentityFromContext,
// brokerOnBehalfOfAuthorizes, brokerMayReadCatalogResource, and
// authorizedForBrokerRotate all hold a BrokerIdentity interface value at
// some point and must not treat a typed-nil *brokerIdentityImpl as present.
// See isNilIdentity (scheduled_initiator.go) for why a plain == nil
// comparison does not catch this case.
// =============================================================================

// requireNoNilDereference runs fn and fails the test if fn triggers a nil
// pointer dereference, recovering it directly so the failure message can name
// the specific condition under test instead of a generic recovery message.
func requireNoNilDereference(t *testing.T, msg string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s (recovered: %v)", msg, r)
		}
	}()
	fn()
}

// TestGetBrokerIdentityFromContext_TypedNilTreatedAsMissing covers both
// context keys GetBrokerIdentityFromContext reads: a context carrying a
// non-nil BrokerIdentity interface value that holds a nil *brokerIdentityImpl
// must normalize to a true nil interface, for the dedicated broker-identity
// key and for the generic identity key.
func TestGetBrokerIdentityFromContext_TypedNilTreatedAsMissing(t *testing.T) {
	// typedNil is a non-nil interface value holding a nil *brokerIdentityImpl.
	var typedNil BrokerIdentity = (*brokerIdentityImpl)(nil)

	t.Run("brokerIdentityContextKey", func(t *testing.T) {
		ctx := contextWithBrokerIdentity(context.Background(), typedNil)

		var got BrokerIdentity
		requireNoNilDereference(t, "GetBrokerIdentityFromContext must not hit a nil dereference on a typed-nil broker identity", func() {
			got = GetBrokerIdentityFromContext(ctx)
		})
		require.True(t, got == nil, "GetBrokerIdentityFromContext must normalize a typed-nil broker identity to a nil interface value")
	})

	t.Run("genericIdentityContextKey", func(t *testing.T) {
		ctx := contextWithIdentity(context.Background(), typedNil)

		var got BrokerIdentity
		requireNoNilDereference(t, "GetBrokerIdentityFromContext must not hit a nil dereference on a typed-nil broker identity under the generic identity key", func() {
			got = GetBrokerIdentityFromContext(ctx)
		})
		require.True(t, got == nil, "GetBrokerIdentityFromContext must normalize a typed-nil broker identity under the generic identity key to a nil interface value")
	})
}

// TestBrokerOnBehalfOfAuthorizes_TypedNilBrokerDenied pins the ctx broker
// identity path end to end, getter plus guard together: a context carrying a
// typed-nil BrokerIdentity under the broker-identity key must deny before
// reaching the ctx broker's ID() call later in the function. The guard at
// that call site reads its value through GetBrokerIdentityFromContext, which
// normalizes a typed nil first, so the guard alone is not reachable by a
// typed nil through this path; it stays as defense in depth, and this test
// exercises the getter and the guard as a pair. A valid OBO marker is
// installed so that, absent both, evaluation would proceed past the Type()
// call (which does not dereference the receiver and so would not itself
// fail) into the obo.BrokerID != broker.ID() comparison, which does.
func TestBrokerOnBehalfOfAuthorizes_TypedNilBrokerDenied(t *testing.T) {
	ctx := contextWithBrokerIdentity(context.Background(), (*brokerIdentityImpl)(nil))
	ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{
		Broker:   NewBrokerIdentity("broker-1"),
		BrokerID: "broker-1",
	})
	principal := PrincipalContext{ID: "user-1"}
	supplied := CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"}

	var result bool
	requireNoNilDereference(t, "brokerOnBehalfOfAuthorizes must not hit a nil dereference when the ctx broker identity is typed-nil", func() {
		result = brokerOnBehalfOfAuthorizes(ctx, principal, supplied)
	})
	assert.False(t, result, "a typed-nil ctx broker identity must not authorize on-behalf-of")
}

// TestBrokerOnBehalfOfAuthorizes_TypedNilOboBrokerDenied covers the
// BrokerOnBehalfOf.Broker field: a genuine, non-nil ctx broker identity
// paired with a marker whose Broker field is typed-nil must deny, not call
// obo.Broker.ID() on the nil pointer. obo.BrokerID is set to match the ctx
// broker's ID so evaluation reaches the obo.Broker check instead of denying
// earlier on an ID mismatch.
func TestBrokerOnBehalfOfAuthorizes_TypedNilOboBrokerDenied(t *testing.T) {
	broker := NewBrokerIdentity("broker-1")
	ctx := contextWithBrokerIdentity(context.Background(), broker)
	ctx = contextWithBrokerOnBehalfOf(ctx, BrokerOnBehalfOf{
		Broker:   (*brokerIdentityImpl)(nil),
		BrokerID: broker.ID(),
	})
	principal := PrincipalContext{ID: "user-1"}
	supplied := CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"}

	var result bool
	requireNoNilDereference(t, "brokerOnBehalfOfAuthorizes must not hit a nil dereference when the OBO marker's Broker field is typed-nil", func() {
		result = brokerOnBehalfOfAuthorizes(ctx, principal, supplied)
	})
	assert.False(t, result, "a typed-nil OBO marker broker must not authorize on-behalf-of")
}

// fakeProjectProviderStore implements only GetProjectProvider; every other
// store.Store method is left to the embedded nil interface and must not be
// called by the code path under test here.
type fakeProjectProviderStore struct {
	store.Store
}

func (fakeProjectProviderStore) GetProjectProvider(ctx context.Context, projectID, brokerID string) (*store.ProjectProvider, error) {
	return nil, store.ErrNotFound
}

// TestBrokerMayReadCatalogResource_TypedNilBrokerDenied covers the project
// scope branch, which calls broker.BrokerID() as part of the
// GetProjectProvider argument list: a typed-nil broker must deny before that
// call. s.store is a non-nil fake so the project-scope branch reaches the
// broker.BrokerID() call instead of returning early on a nil store. Every
// scope is covered, including global (which allows outright without ever
// touching the broker value) and user/unrecognized (which deny without
// touching it either), so the guard is exercised at the top of the function
// regardless of which branch would otherwise run.
func TestBrokerMayReadCatalogResource_TypedNilBrokerDenied(t *testing.T) {
	s := &Server{store: fakeProjectProviderStore{}}
	var typedNil BrokerIdentity = (*brokerIdentityImpl)(nil)

	for _, scope := range []string{"global", "project", "user", "unrecognized"} {
		t.Run(scope, func(t *testing.T) {
			var result bool
			requireNoNilDereference(t, "brokerMayReadCatalogResource must not hit a nil dereference on a typed-nil broker identity", func() {
				result = s.brokerMayReadCatalogResource(context.Background(), typedNil, scope, "some-project")
			})
			assert.False(t, result, "a typed-nil broker identity must not be granted catalog read access")
		})
	}
}

// TestAuthorizedForBrokerRotate_TypedNilBrokerIdentDoesNotShortcut
// covers the brokerIdent.BrokerID() shortcut: a typed-nil brokerIdent must
// not take it. With no user supplied, every other branch is already closed,
// so the call must simply deny without a nil dereference.
func TestAuthorizedForBrokerRotate_TypedNilBrokerIdentDoesNotShortcut(t *testing.T) {
	s := &Server{}
	var typedNil BrokerIdentity = (*brokerIdentityImpl)(nil)
	fetchBroker := func() (*store.RuntimeBroker, error) {
		t.Fatal("fetchBroker must not be called when no user is supplied")
		return nil, nil
	}

	var allowed bool
	var err error
	requireNoNilDereference(t, "authorizedForBrokerRotate must not hit a nil dereference when brokerIdent is typed-nil", func() {
		allowed, err = s.authorizedForBrokerRotate(context.Background(), nil, typedNil, "broker-1", fetchBroker)
	})
	require.NoError(t, err)
	assert.False(t, allowed, "a typed-nil brokerIdent must not satisfy the broker-owner shortcut")
}
