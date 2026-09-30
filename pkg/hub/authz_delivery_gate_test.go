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

// Tests for the delivery credential gate (ptone/scion#2228): a deliver
// permission is admitted only for a credential kind in
// deliveryCredentialKinds, and the gate runs before every grant stage.

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withDeliveryCredentialKinds replaces the delivery credential set for the
// duration of a test. Callers must not use t.Parallel.
func withDeliveryCredentialKinds(t *testing.T, kinds ...CredentialKind) {
	t.Helper()
	set := map[CredentialKind]struct{}{}
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	deliveryCredentialKindsMu.Lock()
	saved := deliveryCredentialKinds
	deliveryCredentialKinds = set
	deliveryCredentialKindsMu.Unlock()
	t.Cleanup(func() {
		deliveryCredentialKindsMu.Lock()
		deliveryCredentialKinds = saved
		deliveryCredentialKindsMu.Unlock()
	})
}

// deliveryGateKindCases lists every credential kind the gate is evaluated
// against, with whether the kind is a delivery credential. A kind joins
// deliveryCredentialKinds, and gains a row here with delivery set to true,
// only together with the rest of the ptone/scion#2228 part 2 contract (see
// authz_delivery_gate.go): the bound target agent, non-attestation and
// non-substitution for the item grant, and the two binding preconditions
// listed there: the kind is bound to the credential type, and the section
// 4.6 deliver effect ceiling applies.
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
	deliveryCredentialKindsMu.RLock()
	got := deliveryCredentialKinds
	deliveryCredentialKindsMu.RUnlock()
	assert.Equal(t, want, got)
	for k := range got {
		found := false
		for _, tc := range deliveryGateKindCases {
			found = found || tc.kind == k
		}
		assert.True(t, found, "delivery kind %q has no table row", k)
	}
}

// TestDeliveryGate_ConcurrentReadAndSwap runs deliveryCredentialAdmitted
// in a goroutine while withDeliveryCredentialKinds swaps and restores the
// set; under -race it fails if either side skips deliveryCredentialKindsMu.
func TestDeliveryGate_ConcurrentReadAndSwap(t *testing.T) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = deliveryCredentialAdmitted("", ActionDeliver, CredentialKindUAT)
			}
		}
	}()
	for i := 0; i < 50; i++ {
		t.Run("swap", func(t *testing.T) { withDeliveryCredentialKinds(t, CredentialKindUAT) })
	}
	close(stop)
	wg.Wait()
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
	assert.NotEmpty(t, d.Provenance.Permission, "%s: explain output names the gated permission", msg)
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
	assert.Equal(t, "secret.deliver", d.Provenance.Permission)
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
		assert.Equal(t, perm, d.Provenance.Permission)
	}

	// An explicit deliver permission is gated whatever the request action:
	// the gate reads the resolved permission, not only the action.
	for _, perm := range []string{"secret.deliver", "env_var.deliver"} {
		for _, action := range []Action{ActionRead, ActionUse} {
			req := deliveryGateRequest(admin, CredentialKindInteractive,
				Resource{Type: permissionResource(t, perm), ID: tid("dg-act-" + perm)}, perm)
			req.Action = action
			d = f.authz.Decide(context.Background(), req)
			msg := "super-admin " + perm + " with action " + string(action)
			assertDeliveryGateDenied(t, d, msg)
			assert.Equal(t, perm, d.Provenance.Permission, msg)
		}
	}

	d = decidePerm(f.authz, admin, secret, ActionUse, "secret.use", false)
	assert.True(t, d.Allowed, "super-admin secret.use: reason %q", d.Reason)
}

// A gated deliver deny produces exactly one decision audit record through
// Decide, with the audit PermissionID set only from a caller-supplied
// registered permission (never from the permission resolved from resource
// and action).
func TestDeliveryGate_DenyAuditedOnce(t *testing.T) {
	f := newGoldenFixture(t)
	emitter := &recordingDecisionAuditEmitter{}
	f.authz.SetDecisionAuditEmitter(emitter)
	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")
	secret := Resource{Type: "secret", ID: f.secretID}

	cases := []struct {
		name       string
		permission string
		wantPermID string
	}{
		{"explicit permission", "secret.deliver", "secret.deliver"},
		{"resolved from resource and action", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(emitter.records)
			d := f.authz.Decide(context.Background(), AuthzRequest{
				Principal:  principalContextForIdentity(admin),
				Credential: credentialContextForIdentity(admin),
				Resource:   secret,
				Action:     ActionDeliver,
				Permission: tc.permission,
			})
			require.False(t, d.Allowed)
			require.Equal(t, deliveryGateReason, d.Reason)
			require.Len(t, emitter.records[before:], 1, "a gated deny emits exactly one decision audit record")
			rec := emitter.records[len(emitter.records)-1]
			assert.Equal(t, "deny", rec.Result)
			assert.Equal(t, tc.wantPermID, rec.PermissionID)
		})
	}
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
// grant evaluation. This test pins the hand-off only, not the outcome of
// grant evaluation: the decision reason is not the gate's, and the
// later stages ran (role grants for the super-admin, relationship
// candidates for a progeny agent). It deliberately does not assert
// Allowed. Under the ptone/scion#2228 contract a role holding a deliver
// permission never substitutes for the item grant, so a super-admin role
// binding is not a positive control for deliver; see
// TestDeliveryGate_Part2RoleDoesNotSubstituteForItemGrant.
func TestDeliveryGate_DeliveryKindReachesGrantEvaluation(t *testing.T) {
	f := newGoldenFixture(t)
	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")
	secret := Resource{Type: "secret", ID: f.secretID}
	const testKind CredentialKind = "test_delivery_kind"

	assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), deliveryGateRequest(admin, testKind, secret, "secret.deliver")), "kind outside the set")

	withDeliveryCredentialKinds(t, testKind)
	d := f.authz.Decide(context.Background(), deliveryGateRequest(admin, testKind, secret, "secret.deliver"))
	assert.NotEqual(t, deliveryGateReason, d.Reason)
	require.NotNil(t, d.Provenance)
	assert.NotContains(t, d.Provenance.DenyReasons, deliveryGateReason)
	assert.NotEmpty(t, d.Provenance.Grants, "role binding evaluation ran after the gate")

	// A progeny agent reaches relationship evaluation for the same
	// permission once its kind is in the set.
	agent := progenyPairAgent(tid("dg-handoff-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	withDeliveryCredentialKinds(t, testKind, CredentialKindAgentJWT)
	d = decidePerm(f.authz, agent, secret, ActionDeliver, "secret.deliver", true)
	assert.NotEqual(t, deliveryGateReason, d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.Equal(t, "secret.deliver", r.Permission)

	assertDeliveryGateDenied(t, f.authz.Decide(context.Background(), deliveryGateRequest(admin, CredentialKindInteractive, secret, "secret.deliver")), "interactive")
}

// The tests below state the ptone/scion#2228 part 2 contract. They are
// skipped because the internal delivery credential kind, its unexported
// constructor and BoundAgentID do not exist in this change. Part 2
// un-skips them together with adding the kind to deliveryCredentialKinds
// and a delivery=true row to deliveryGateKindCases.

// A role holding a deliver permission, presented with a valid delivery
// credential, is denied without the association, progeny or skill-default
// grant for the selected item.
func TestDeliveryGate_Part2RoleDoesNotSubstituteForItemGrant(t *testing.T) {
	t.Skip("depends on the internal delivery credential kind, ptone/scion#2228 part 2: " +
		"assert a super-admin role holding secret.deliver, with a valid delivery credential bound to the agent, " +
		"is denied secret.deliver on a secret with no item grant")
}

// A delivery credential whose BoundAgentID differs from the principal
// agent ID is denied.
func TestDeliveryGate_Part2WrongBoundAgentDenied(t *testing.T) {
	t.Skip("depends on the internal delivery credential kind and BoundAgentID, ptone/scion#2228 part 2: " +
		"assert a delivery credential bound to agent A is denied secret.deliver and env_var.deliver for principal agent B, " +
		"even with a valid progeny grant for B")
}

// A valid internal delivery credential, bound to the principal agent, with
// a valid item grant, is admitted; the same request without the grant is
// denied.
func TestDeliveryGate_Part2ValidInternalDeliveryAdmitted(t *testing.T) {
	t.Skip("depends on the internal delivery credential kind, ptone/scion#2228 part 2: " +
		"assert a delivery credential bound to the principal agent, with a progeny grant on the secret and the " +
		"section 4.6 deliver ceiling met, is admitted for secret.deliver; the credential is not a source of attested facts")
}

// The gated kind is bound to the credential type: a request whose supplied
// Credential.Kind names the delivery kind while the identity is not a
// delivery credential is denied.
func TestDeliveryGate_Part2KindBoundToCredentialType(t *testing.T) {
	t.Skip("depends on the internal delivery credential kind, ptone/scion#2228 part 2: " +
		"assert an interactive, UAT, agent JWT or federated identity with Credential.Kind set to the delivery kind " +
		"is denied every deliver permission; the kind comes from the credential type, not the request field")
}

// A progeny deliver pair admitted by the gate is bounded by the F design
// section 4.6 deliver effect ceiling, not by the generic delegation
// ceiling.
func TestDeliveryGate_Part2DeliverEffectCeilingRequired(t *testing.T) {
	t.Skip("depends on the section 4.6 deliver effect ceiling, a precondition of ptone/scion#2228 part 2: " +
		"assert secret.deliver via progeny is denied when the EffectCeiling lacks the exact deliver permission, " +
		"or when the source authority is not live, even though the delegator holds secret.deliver")
}
