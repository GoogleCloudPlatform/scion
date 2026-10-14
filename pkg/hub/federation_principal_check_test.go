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

//go:build !no_sqlite

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The principal check on federation credentials (ptone/scion#5080): a
// credential naming a test identity or the platform service account is
// refused, whatever its claims say. The peer signs these credentials with
// its own key, as a peer that issues them anyway would.

// peerTokPlatformAuthSA is the receiving hub's platform service account in
// these tests.
const peerTokPlatformAuthSA = "platform-transport@hub-recv.iam.gserviceaccount.com"

// A user access credential from a peer hub naming a test identity is
// refused, for every reserved test identity domain.
func TestFederationPrincipalCheck_TestIdentityRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")

	for name, email := range map[string]string{
		"member fixture":      "driver-1@scion-fixture.invalid",
		"hub admin":           "driver-2@scion-test-hub-admin.invalid",
		"super admin":         "driver-3@scion-test-super-admin.invalid",
		"mixed case":          "Driver-4@SCION-Fixture.Invalid",
		"subdomain":           "driver-5@run.scion-fixture.invalid",
		"federated synthetic": "fed+abc123@idp.example.com.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-"+name, email)))
		})
	}
}

// The reserved domains match by exact domain or subdomain suffix
// (store.IsReservedTestIdentityEmail, emailInDomain in pkg/store), not by
// substring: look-alike domains are accepted like any other email.
func TestFederationPrincipalCheck_LookAlikeDomainsAccepted(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	for _, email := range []string{
		"a@scion-fixture.invalid.example.com",
		"b@xscion-fixture.invalid",
		"c@scion-test-hub-admin.invalid.example.com",
		"d@xscion-test-hub-admin.invalid",
		"e@scion-test-super-admin.invalid.example.com",
		"f@xscion-test-super-admin.invalid",
	} {
		t.Run(email, func(t *testing.T) {
			assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-"+email, email)), "sub-"+email, email)
		})
	}
}

// A user access credential from a peer hub naming this hub's platform
// service account is refused (email compared case-insensitively); a normal
// user's credential is still accepted with the account configured.
func TestFederationPrincipalCheck_PlatformServiceAccountRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	f.srv.authConfig.PlatformAuthSA = peerTokPlatformAuthSA

	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-platform", peerTokPlatformAuthSA)))
	assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-platform-upper", "Platform-Transport@HUB-RECV.iam.gserviceaccount.com")))

	assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-human", "human@corp.com")), "sub-human", "human@corp.com")
}

// A credential minted by the peer for a normal human user is still accepted
// with the platform service account configured, and agent credentials are
// unchanged.
func TestFederationPrincipalCheck_HumanAndAgentAccepted(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	f.srv.authConfig.PlatformAuthSA = peerTokPlatformAuthSA

	token, u := f.mintUserToken(t)
	assertPeerTokUser(t, f, token, u.ID, u.Email)

	fa := assertPeerTokAgent(t, f, f.mintAgentToken(t))
	assert.Equal(t, f.peerAgent.Subject, fa.RemoteAgentID())
}

// federationPrincipalRefusal by identity type: it reads the email of user
// and service account identities, and never refuses an agent identity.
func TestFederationPrincipalRefusal(t *testing.T) {
	opts := FederationAuthOptions{PlatformAuthSA: peerTokPlatformAuthSA}
	const iss = "https://issuer.example.com"
	for _, tc := range []struct {
		name     string
		identity FederatedIdentity
		opts     FederationAuthOptions
		refused  bool
	}{
		{"user: human", NewFederatedUserIdentity(iss, "s1", "human@corp.com", "H", "viewer", nil), opts, false},
		{"user: test identity", NewFederatedUserIdentity(iss, "s2", "x@scion-fixture.invalid", "X", "viewer", nil), opts, true},
		{"user: platform service account", NewFederatedUserIdentity(iss, "s3", peerTokPlatformAuthSA, "P", "viewer", nil), opts, true},
		{"service: platform service account", NewFederatedServiceIdentity(iss, "s4", peerTokPlatformAuthSA, nil), opts, true},
		{"service: platform service account, none configured", NewFederatedServiceIdentity(iss, "s5", peerTokPlatformAuthSA, nil), FederationAuthOptions{}, false},
		{"service: other service account", NewFederatedServiceIdentity(iss, "s6", "worker@proj.iam.gserviceaccount.com", nil), opts, false},
		{"service: test identity", NewFederatedServiceIdentity(iss, "s7", "x@scion-test-hub-admin.invalid", nil), opts, true},
		{"agent", NewFederatedAgentIdentity(iss, "agent-1", "project-a", "worker", "user:root", []string{"user:root"}, nil), opts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := federationPrincipalRefusal(tc.identity, tc.opts)
			if tc.refused {
				assert.NotEmpty(t, reason)
			} else {
				assert.Empty(t, reason)
			}
		})
	}
}
