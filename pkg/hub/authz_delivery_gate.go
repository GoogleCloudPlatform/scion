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

import "github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"

// Delivery credential gate (ptone/scion#2228).
//
// A request whose action is ActionDeliver, or whose permission is registered
// with the deliver action, is admitted only when the credential kind is a
// member of deliveryCredentialKinds. Decide applies the gate after resolving
// the permission and before any grant stage (role bindings, agent synthetic
// bindings, relationship candidates), so no grant path can admit a deliver
// permission for a credential kind outside the set. This covers every
// principal, including hub administrators holding a role binding.
//
// The set is empty. The gate is a precondition for deliver, never a grant:
// a request that passes it still needs every later stage to admit it.
//
// The internal delivery credential (ptone/scion#2228 part 2) adds its kind
// to this set together with the rest of its contract, which a kind-only set
// cannot express:
//
//   - BoundAgentID: the delivery credential names the agent it delivers
//     to, and a deliver request is denied unless BoundAgentID equals the
//     principal agent ID.
//   - Non-attestation: the delivery credential attests nothing about
//     ancestry, scopes or roles; it only restricts. It is not a source of
//     hub-attested facts for relationship evaluation.
//   - Non-substitution: holding a *.deliver permission in a role never
//     substitutes for the association, progeny or skill-default grant
//     required for the selected item. A role binding alone must not admit
//     deliver, for any credential kind, including the delivery kind; part 2
//     enforces this before its kind joins the set, and the skipped
//     TestDeliveryGate_Part2RoleDoesNotSubstituteForItemGrant pins it.
//
// Binding preconditions for adding any kind to this set (ptone/scion#2228
// part 2 scope):
//
//   - The kind is bound to the credential type. Decide takes the gated
//     kind from the unexported delivery credential type or its
//     constructor, or rejects a request whose supplied Credential.Kind
//     does not match the identity, in the same way the principal kind is
//     cross-checked. The free request field alone never admits deliver.
//   - The F design section 4.6 deliver effect ceiling is enforced: the
//     EffectCeiling contains the exact deliver permission, and the source
//     authority is live. The generic Step 10 delegation ceiling ("the
//     delegator holds the permission") does not meet this.
//
// Skipped tests in authz_delivery_gate_test.go (TestDeliveryGate_Part2*)
// state these rules and cite ptone/scion#2228.
var deliveryCredentialKinds = map[CredentialKind]struct{}{}

// deliveryGateReason is the deny reason recorded when the gate rejects a
// request.
const deliveryGateReason = "deliver permissions require a delivery credential"

// deliverPermissionIDs lists the registered permissions whose action is
// deliver. Derived from the registry so a new deliver permission is gated
// without a separate list.
var deliverPermissionIDs = func() map[string]struct{} {
	ids := map[string]struct{}{}
	for _, p := range permissions.Registry {
		if Action(p.Action) == ActionDeliver {
			ids[p.ID] = struct{}{}
		}
	}
	return ids
}()

// isDeliverRequest reports whether the request action or the resolved
// permission is a deliver operation.
func isDeliverRequest(permissionID string, action Action) bool {
	if action == ActionDeliver {
		return true
	}
	_, ok := deliverPermissionIDs[permissionID]
	return ok
}

// deliveryCredentialAdmitted reports whether a deliver request may proceed
// to grant evaluation for the given credential kind. Non-deliver requests
// are always admitted by this gate.
func deliveryCredentialAdmitted(permissionID string, action Action, kind CredentialKind) bool {
	if !isDeliverRequest(permissionID, action) {
		return true
	}
	_, ok := deliveryCredentialKinds[kind]
	return ok
}
