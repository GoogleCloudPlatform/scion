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

// Tests for the delivery credential gate (ptone/scion#2228): a deliver
// permission is admitted only for a credential kind in
// deliveryCredentialKinds, and the gate runs before every grant stage.

import (
	"context"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withDeliveryCredentialKinds replaces the delivery credential set for the
// duration of a test. Callers must not use t.Parallel.
func withDeliveryCredentialKinds(t *testing.T, kinds ...CredentialKind) {
	t.Helper()
	saved := deliveryCredentialKinds
	set := map[CredentialKind]struct{}{}
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	deliveryCredentialKinds = set
	t.Cleanup(func() { deliveryCredentialKinds = saved })
}

// deliveryGateKindCases lists every credential kind the gate is evaluated
// against, with whether the kind is a delivery credential. Registering a
// delivery credential kind adds it to deliveryCredentialKinds and adds a
// row here with delivery set to true.
var deliveryGateKindCases = []struct {
	kind     CredentialKind
	delivery bool
}{
	{CredentialKindInteractive, false},
	{CredentialKindUAT, false},
	{CredentialKindAgentJWT, false},
	{CredentialKindBroker, false},
	{CredentialKindFederation, false},
	{CredentialKindDev, false},
	{"unrecognized", false},
}

// The delivery set holds exactly the kinds marked delivery in the table.
func TestDeliveryGate_KindSetMatchesTable(t *testing.T) {
	want := map[CredentialKind]struct{}{}
	for _, tc := range deliveryGateKindCases {
		if tc.delivery {
			want[tc.kind] = struct{}{}
		}
	}
	assert.Equal(t, want, deliveryCredentialKinds)
	for k := range deliveryCredentialKinds {
		found := false
		for _, tc := range deliveryGateKindCases {
			found = found || tc.kind == k
		}
		assert.True(t, found, "delivery kind %q has no table row", k)
	}
}

// The gated permission set is every registry row with the deliver action.
func TestDeliveryGate_PermissionSetFromRegistry(t *testing.T) {
	var got []string
	for id := range deliverPermissionIDs {
		got = append(got, id)
	}
	sort.Strings(got)
	var want []string
	for _, p := range permissions.Registry {
		if p.Action == permissions.ActionDeliver {
			want = append(want, p.ID)
		}
	}
	sort.Strings(want)
	require.NotEmpty(t, want)
	assert.Equal(t, want, got)
	assert.Subset(t, got, []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver"})
}

func TestDeliveryGate_Predicate(t *testing.T) {
	for _, tc := range deliveryGateKindCases {
		assert.Equal(t, tc.delivery, deliveryCredentialAdmitted("secret.deliver", ActionDeliver, tc.kind), "kind %q", tc.kind)
		assert.Equal(t, tc.delivery, deliveryCredentialAdmitted("secret.deliver", ActionRead, tc.kind),
			"kind %q: the registered deliver permission is gated whatever the request action", tc.kind)
		assert.Equal(t, tc.delivery, deliveryCredentialAdmitted("", ActionDeliver, tc.kind),
			"kind %q: the deliver action is gated whatever the permission", tc.kind)
		assert.True(t, deliveryCredentialAdmitted("secret.use", ActionUse, tc.kind), "kind %q: use is not gated", tc.kind)
		assert.True(t, deliveryCredentialAdmitted(permissionProjectSecretRead, ActionRead, tc.kind), "kind %q: read is not gated", tc.kind)
	}
	assert.False(t, deliveryCredentialAdmitted("secret.deliver", ActionDeliver, ""), "empty kind")
}

func deliveryGateRequest(identity Identity, kind CredentialKind, res Resource, perm string) AuthzRequest {
	return AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: CredentialContext{Kind: kind},
		Resource:   res,
		Action:     ActionDeliver,
		Permission: perm,
		Explain:    true,
	}
}

func assertDeliveryGateDenied(t *testing.T, d Decision, msg string) {
	t.Helper()
	assert.False(t, d.Allowed, "%s: reason %q", msg, d.Reason)
	assert.Equal(t, deliveryGateReason, d.Reason, msg)
	require.NotNil(t, d.Provenance, msg)
	assert.Empty(t, d.Provenance.Grants, "%s: the gate precedes role binding evaluation", msg)
	assert.Empty(t, d.Provenance.Relationships, "%s: the gate precedes relationship evaluation", msg)
	assert.Equal(t, []string{deliveryGateReason}, d.Provenance.DenyReasons, msg)
}

// A super-admin holding a hub-wide role binding is denied every deliver
// permission on its real interactive identity, while secret.use on the same
// secret is allowed through the role binding.
func TestDeliveryGate_SuperAdminDenied(t *testing.T) {
	f := newGoldenFixture(t)
	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")
	secret := Resource{Type: "secret", ID: f.secretID}

	d := f.authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(admin),
		Credential: credentialContextForIdentity(admin),
		Resource:   secret,
		Action:     ActionDeliver,
		Permission: "secret.deliver",
		Explain:    true,
	})
	assertDeliveryGateDenied(t, d, "super-admin secret.deliver")
	assert.Equal(t, string(CredentialKindInteractive), d.CredentialKind)

	d = f.authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(admin),
		Credential: credentialContextForIdentity(admin),
		Resource:   secret,
		Action:     ActionDeliver,
	})
	assert.False(t, d.Allowed, "resolved from resource and action: reason %q", d.Reason)

	for _, perm := range []string{"env_var.deliver", "skill_injection.deliver"} {
		d = f.authz.Decide(context.Background(), deliveryGateRequest(admin, CredentialKindInteractive,
			Resource{Type: permissionResource(t, perm), ID: tid("dg-" + perm)}, perm))
		assertDeliveryGateDenied(t, d, "super-admin "+perm)
	}

	d = decidePerm(f.authz, admin, secret, ActionUse, "secret.use", false)
	assert.True(t, d.Allowed, "super-admin secret.use: reason %q", d.Reason)
}

func permissionResource(t *testing.T, id string) string {
	t.Helper()
	for _, p := range permissions.Registry {
		if p.ID == id {
			return p.Resource
		}
	}
	t.Fatalf("permission %q not registered", id)
	return ""
}

// Every credential kind outside the delivery set is denied secret.deliver,
// for the super-admin identity and for identities matching the kind:
// session user, UAT-scoped user, agent JWT and federated agent.
func TestDeliveryGate_EveryNonDeliveryKindDenied(t *testing.T) {
	f := newGoldenFixture(t)
	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")
	secret := Resource{Type: "secret", ID: f.secretID}

	for _, tc := range deliveryGateKindCases {
		if tc.delivery {
			continue
		}
		t.Run(string(tc.kind), func(t *testing.T) {
			assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), deliveryGateRequest(admin, tc.kind, secret, "secret.deliver")), "super-admin")
		})
	}

	owner := NewAuthenticatedUser(f.projectOwnerID, "owner@golden.test", "Owner", "member", "web")
	uat := NewScopedUserIdentity(owner, f.projectBeta.ID, []string{"secret.deliver", "secret.read"})
	agent := progenyPairAgent(tid("dg-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	fed := NewFederatedAgentIdentity("https://peer.example", tid("dg-fed"), f.projectBeta.ID,
		"fed", f.projectOwnerID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	for name, id := range map[string]Identity{"session": owner, "uat": uat, "agent_jwt": agent, "federation": fed} {
		t.Run("identity/"+name, func(t *testing.T) {
			cred := credentialContextForIdentity(id)
			req := deliveryGateRequest(id, cred.Kind, secret, "secret.deliver")
			req.Credential = cred
			assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), req), name)
		})
	}
}

// With a kind placed in the delivery set, the gate hands the request to
// grant evaluation: the super-admin role binding admits secret.deliver.
// Registering the delivery credential kind needs only the set entry.
func TestDeliveryGate_DeliveryKindReachesGrantEvaluation(t *testing.T) {
	f := newGoldenFixture(t)
	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")
	secret := Resource{Type: "secret", ID: f.secretID}
	const testKind CredentialKind = "test_delivery_kind"

	assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), deliveryGateRequest(admin, testKind, secret, "secret.deliver")), "kind outside the set")

	withDeliveryCredentialKinds(t, testKind)
	d := f.authz.Decide(context.Background(), deliveryGateRequest(admin, testKind, secret, "secret.deliver"))
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	assert.NotEqual(t, deliveryGateReason, d.Reason)

	assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), deliveryGateRequest(admin, CredentialKindInteractive, secret, "secret.deliver")), "interactive")
}
