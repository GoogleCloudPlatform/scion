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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
)

// TestAncestryIsHubAttested_EquivalenceTable pins AncestryIsHubAttested's
// result for every concrete Identity type declared in package hub, production
// and test alike, in every state that reaches a different branch: a typed-nil
// pointer, a federated identity, a federated identity that also carries the
// ancestry marker, a type embedding a marker holder, and a *ScopedUserIdentity
// wrapping each kind of user. It is split in two so that it runs in every
// build: this file covers every production type and the test fakes declared
// in untagged files, and runs under no_sqlite too;
// TestAncestryIsHubAttested_EquivalenceTableSQLiteFakes
// (identity_attestation_equivalence_sqlite_test.go) covers the test fakes
// declared in files built only without no_sqlite. Neither needs a store.
// It records today's decisions so that a change in how attestation is
// implemented (for example an explicit type switch in place of a marker
// method) can be shown to decide exactly the same way.
func TestAncestryIsHubAttested_EquivalenceTable(t *testing.T) {
	storeAgent := func(name string) *store.Agent {
		return &store.Agent{ID: tid(name), ProjectID: tid("attest-project"), Ancestry: []string{tid("attest-user")}}
	}
	localUser := NewAuthenticatedUser(tid("attest-user"), "u@example.com", "U", "member", "cli")
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	federatedUser := NewFederatedUserIdentity("https://issuer.example", "sub", "fu@example.com", "FU", "member", nil)
	scoped := func(user UserIdentity) *ScopedUserIdentity {
		return NewScopedUserIdentity(user, tid("attest-project"), []string{"agent:read"})
	}

	cases := []struct {
		name     string
		identity Identity
		want     bool
	}{
		{"nil interface", nil, false},

		// Production types that move to the principal package.
		{"*AuthenticatedUser", localUser, true},
		{"typed-nil *AuthenticatedUser", (*AuthenticatedUser)(nil), false},
		{"*ScopedUserIdentity wrapping *AuthenticatedUser", scoped(localUser), true},
		{"*ScopedUserIdentity wrapping *DevUser", scoped(devUser), true},
		{"*ScopedUserIdentity wrapping *FederatedUserIdentity", scoped(federatedUser), false},
		{"*ScopedUserIdentity wrapping *ScopedUserIdentity wrapping *AuthenticatedUser", scoped(scoped(localUser)), true},
		{"*ScopedUserIdentity wrapping *ScopedUserIdentity wrapping *FederatedUserIdentity", scoped(scoped(federatedUser)), true},
		{"*ScopedUserIdentity wrapping nil UserIdentity", scoped(nil), true},
		{"*ScopedUserIdentity wrapping typed-nil *AuthenticatedUser", scoped((*AuthenticatedUser)(nil)), true},
		{"*ScopedUserIdentity wrapping typed-nil *FederatedUserIdentity", scoped((*FederatedUserIdentity)(nil)), false},
		{"typed-nil *ScopedUserIdentity", (*ScopedUserIdentity)(nil), false},
		// A ScopedUserIdentity value is an Identity too (its embedded
		// UserIdentity promotes ID and Type into the value method set), but
		// the marker is declared on the pointer receiver, so it is not
		// attested.
		{"ScopedUserIdentity (value) wrapping *AuthenticatedUser", *scoped(localUser), false},
		{"ScopedUserIdentity (value) wrapping nil UserIdentity", ScopedUserIdentity{}, false},

		// Production types that stay in package hub.
		{"*DevUser", devUser, true},
		{"typed-nil *DevUser", (*DevUser)(nil), false},
		{"*agentIdentityWrapper", &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "agent-1", ID: "jti-1"}}}, true},
		{"*agentIdentityWrapper with nil claims", &agentIdentityWrapper{}, true},
		{"typed-nil *agentIdentityWrapper", (*agentIdentityWrapper)(nil), false},
		{"*storedAgentIdentity", &storedAgentIdentity{agent: storeAgent("attest-stored")}, true},
		{"typed-nil *storedAgentIdentity", (*storedAgentIdentity)(nil), false},
		{"*peerAgentIdentity", &peerAgentIdentity{agent: storeAgent("attest-peer")}, true},
		{"typed-nil *peerAgentIdentity", (*peerAgentIdentity)(nil), false},
		{"*explainAgentIdentity", newAgentIdentityFromStore(storeAgent("attest-explain")), true},
		{"typed-nil *explainAgentIdentity", (*explainAgentIdentity)(nil), false},
		{"*hubDeliveryIdentity", &hubDeliveryIdentity{agentID: tid("attest-hd"), projectID: tid("attest-project"), boundAgentID: tid("attest-hd"), evidence: &storedAgentIdentity{agent: storeAgent("attest-hd")}}, false},
		{"typed-nil *hubDeliveryIdentity", (*hubDeliveryIdentity)(nil), false},
		{"*brokerIdentityImpl", NewBrokerIdentity(tid("attest-broker")), false},
		{"typed-nil *brokerIdentityImpl", (*brokerIdentityImpl)(nil), false},
		{"*FederatedUserIdentity", federatedUser, false},
		{"typed-nil *FederatedUserIdentity", (*FederatedUserIdentity)(nil), false},
		{"*FederatedAgentIdentity", NewFederatedAgentIdentity("https://issuer.example", "remote-agent", "remote-project", "Remote Agent", tid("attest-user"), []string{tid("attest-user")}, nil), false},
		{"typed-nil *FederatedAgentIdentity", (*FederatedAgentIdentity)(nil), false},
		{"*FederatedServiceIdentity", NewFederatedServiceIdentity("https://issuer.example", "sub", "sa@example.com", nil), false},
		{"typed-nil *FederatedServiceIdentity", (*FederatedServiceIdentity)(nil), false},
		{"*DelegatedAgentIdentity", &DelegatedAgentIdentity{agentID: tid("attest-delegated")}, false},
		{"typed-nil *DelegatedAgentIdentity", (*DelegatedAgentIdentity)(nil), false},

		// Test fakes that carry the ancestry marker.
		{"*testRelAgentIdentity", &testRelAgentIdentity{id: tid("attest-rel")}, true},
		{"typed-nil *testRelAgentIdentity", (*testRelAgentIdentity)(nil), false},
		{"federated fake carrying the marker", &federatedWithAncestryMarkerTestIdentity{id: tid("attest-fed-marker")}, false},
		{"typed-nil federated fake carrying the marker", (*federatedWithAncestryMarkerTestIdentity)(nil), false},

		// Test fakes without the marker.
		{"*recognizedPrincipalEmptyCredentialMockIdentity", &recognizedPrincipalEmptyCredentialMockIdentity{}, false},
		{"typed-nil *recognizedPrincipalEmptyCredentialMockIdentity", (*recognizedPrincipalEmptyCredentialMockIdentity)(nil), false},
		{"*unattestedMockAgentIdentity", &unattestedMockAgentIdentity{id: tid("attest-unattested"), ancestry: []string{tid("attest-user")}}, false},
		{"typed-nil *unattestedMockAgentIdentity", (*unattestedMockAgentIdentity)(nil), false},
		{"*unclassifiedMockIdentity", &unclassifiedMockIdentity{}, false},
		{"typed-nil *unclassifiedMockIdentity", (*unclassifiedMockIdentity)(nil), false},
		{"*userShapedMockIdentity", &userShapedMockIdentity{}, false},
		{"typed-nil *userShapedMockIdentity", (*userShapedMockIdentity)(nil), false},
		{"adtFakeTypeIdentity (value)", adtFakeTypeIdentity{}, false},
		{"*adtFakeTypeIdentity", &adtFakeTypeIdentity{}, false},
		{"typed-nil *adtFakeTypeIdentity", (*adtFakeTypeIdentity)(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AncestryIsHubAttested(tc.identity))
		})
	}

	// A value AuthenticatedUser or DevUser is not an Identity at all: every
	// Identity method is declared on the pointer receiver, so only the
	// pointer form can reach AncestryIsHubAttested. (A ScopedUserIdentity
	// value is an Identity; it is pinned as not attested in the table.)
	identityType := reflect.TypeOf((*Identity)(nil)).Elem()
	for _, v := range []any{AuthenticatedUser{}, DevUser{}} {
		assert.Falsef(t, reflect.TypeOf(v).Implements(identityType), "value %T must not implement Identity", v)
		assert.Truef(t, reflect.PointerTo(reflect.TypeOf(v)).Implements(identityType), "*%T must implement Identity", v)
	}
}
