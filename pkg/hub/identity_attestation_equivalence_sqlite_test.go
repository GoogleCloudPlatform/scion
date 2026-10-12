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

//go:build !no_sqlite && (!hubshard || hubshard_1)

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAncestryIsHubAttested_EquivalenceTableSQLiteFakes is the second half
// of TestAncestryIsHubAttested_EquivalenceTable: the rows for test fakes
// declared in files that are built only without the no_sqlite tag. It needs
// no store; it takes the !no_sqlite constraint only because those fake types
// do not exist in a no_sqlite build.
func TestAncestryIsHubAttested_EquivalenceTableSQLiteFakes(t *testing.T) {
	localUser := NewAuthenticatedUser(tid("attest-user"), "u@example.com", "U", "member", "cli")
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	scoped := func(user UserIdentity) *ScopedUserIdentity {
		return NewScopedUserIdentity(user, tid("attest-project"), []string{"agent:read"})
	}

	cases := []struct {
		name     string
		identity Identity
		want     bool
	}{
		// *ScopedUserIdentity wrapping test fakes.
		{"*ScopedUserIdentity wrapping federated test fake with IssuerURL", scoped(&bindableFederatedAdmin{UserIdentity: localUser, issuerURL: "https://issuer.example"}), false},
		{"*ScopedUserIdentity wrapping *federatedTestIdentity (no IssuerURL)", scoped(&federatedTestIdentity{id: tid("attest-fed-test")}), true},
		{"*ScopedUserIdentity wrapping *devTypedUserIdentity", scoped(&devTypedUserIdentity{id: tid("attest-dev-typed")}), true},
		{"*ScopedUserIdentity wrapping *bypassAgentsDevIdentity", scoped(&bypassAgentsDevIdentity{UserIdentity: localUser}), true},

		// Test fakes that carry the ancestry marker.
		{"*testProgenyAgentIdentity", &testProgenyAgentIdentity{id: tid("attest-progeny")}, true},
		{"typed-nil *testProgenyAgentIdentity", (*testProgenyAgentIdentity)(nil), false},
		{"*grantGuardAgentIdentity", &grantGuardAgentIdentity{id: tid("attest-grant-guard")}, true},
		{"typed-nil *grantGuardAgentIdentity", (*grantGuardAgentIdentity)(nil), false},
		{"*testAgentIdentity", &testAgentIdentity{id: tid("attest-test-agent")}, true},
		{"typed-nil *testAgentIdentity", (*testAgentIdentity)(nil), false},
		{"*wakeDMTestIdentity", &wakeDMTestIdentity{id: tid("attest-wake-dm")}, true},
		{"typed-nil *wakeDMTestIdentity", (*wakeDMTestIdentity)(nil), false},

		// A test type that embeds a marker holder: the promoted marker
		// method attests it.
		{"*devUserEmbeddingWrapper embedding *DevUser", &devUserEmbeddingWrapper{DevUser: devUser}, true},
		{"*devUserEmbeddingWrapper embedding nil *DevUser", &devUserEmbeddingWrapper{}, true},
		{"typed-nil *devUserEmbeddingWrapper", (*devUserEmbeddingWrapper)(nil), false},
		{"*wakeLifecycleAgentIdentity embedding wakeDMTestIdentity", &wakeLifecycleAgentIdentity{wakeDMTestIdentity: wakeDMTestIdentity{id: tid("attest-wake-lifecycle")}}, true},
		{"typed-nil *wakeLifecycleAgentIdentity", (*wakeLifecycleAgentIdentity)(nil), false},

		// Test fakes without the marker.
		{"*bypassAgentsDevIdentity embedding *AuthenticatedUser", &bypassAgentsDevIdentity{UserIdentity: localUser}, false},
		{"typed-nil *bypassAgentsDevIdentity", (*bypassAgentsDevIdentity)(nil), false},
		{"*bindableFederatedAdmin embedding *AuthenticatedUser", &bindableFederatedAdmin{UserIdentity: localUser, issuerURL: "https://issuer.example"}, false},
		{"typed-nil *bindableFederatedAdmin", (*bindableFederatedAdmin)(nil), false},
		{"*agentKeysFakeIdentity", &agentKeysFakeIdentity{}, false},
		{"typed-nil *agentKeysFakeIdentity", (*agentKeysFakeIdentity)(nil), false},
		{"*devLookAlikeIdentity", &devLookAlikeIdentity{id: DevUserID}, false},
		{"typed-nil *devLookAlikeIdentity", (*devLookAlikeIdentity)(nil), false},
		{"*devTypedUserIdentity", &devTypedUserIdentity{id: DevUserID}, false},
		{"typed-nil *devTypedUserIdentity", (*devTypedUserIdentity)(nil), false},
		{"fakeIdentity (value)", fakeIdentity{}, false},
		{"*fakeIdentity", &fakeIdentity{}, false},
		{"typed-nil *fakeIdentity", (*fakeIdentity)(nil), false},
		{"*fakeNonBrokerTypeIdentity", &fakeNonBrokerTypeIdentity{}, false},
		{"typed-nil *fakeNonBrokerTypeIdentity", (*fakeNonBrokerTypeIdentity)(nil), false},
		{"*federatedAgentIdentityForTest", &federatedAgentIdentityForTest{}, false},
		{"typed-nil *federatedAgentIdentityForTest", (*federatedAgentIdentityForTest)(nil), false},
		{"*federatedTestIdentity", &federatedTestIdentity{id: tid("attest-fed-test")}, false},
		{"typed-nil *federatedTestIdentity", (*federatedTestIdentity)(nil), false},
		{"gcsTestIdentity (value)", gcsTestIdentity{}, false},
		{"*gcsTestIdentity", &gcsTestIdentity{}, false},
		{"typed-nil *gcsTestIdentity", (*gcsTestIdentity)(nil), false},
		{"*nonUserIdentity", &nonUserIdentity{}, false},
		{"typed-nil *nonUserIdentity", (*nonUserIdentity)(nil), false},
		{"*unknownTestIdentity", &unknownTestIdentity{}, false},
		{"typed-nil *unknownTestIdentity", (*unknownTestIdentity)(nil), false},
		{"fakeAgentIdentity (value)", fakeAgentIdentity{}, false},
		{"*fakeAgentIdentity", &fakeAgentIdentity{}, false},
		{"typed-nil *fakeAgentIdentity", (*fakeAgentIdentity)(nil), false},
		{"fakeNonUserIdentity (value)", fakeNonUserIdentity{}, false},
		{"*fakeNonUserIdentity", &fakeNonUserIdentity{}, false},
		{"typed-nil *fakeNonUserIdentity", (*fakeNonUserIdentity)(nil), false},
		{"valueIdentity (value)", valueIdentity{}, false},
		{"*valueIdentity", &valueIdentity{}, false},
		{"typed-nil *valueIdentity", (*valueIdentity)(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AncestryIsHubAttested(tc.identity))
		})
	}
}
