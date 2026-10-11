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

// This file holds ancestry attestation: the hub-side decision of whether an
// identity's ancestry chain can be trusted. It stays in package hub, apart
// from the principal types in identity.go, so that the unexported marker
// below cannot be implemented or asserted from outside package hub.

// ancestryProvenance names the recognized source of an identity's local
// ancestry chain, for explain/audit and tests. It is unexported: the value
// itself carries no authority, only the presence of a
// localAncestryProvenanceIdentity implementation does.
type ancestryProvenance string

const (
	// ancestryProvenanceLocalUser marks a local user as the root of its own
	// ancestry chain. Only the dev user carries it: *AuthenticatedUser and
	// *ScopedUserIdentity are listed in AncestryIsHubAttested instead.
	ancestryProvenanceLocalUser ancestryProvenance = "local_user"
	// ancestryProvenanceAgentJWT marks an ancestry chain carried in a hub-signed
	// agent JWT.
	ancestryProvenanceAgentJWT ancestryProvenance = "agent_jwt"
	// ancestryProvenanceStoreAgent marks an ancestry chain read back from a
	// hub-persisted agent record (not from the JWT that authenticated the
	// request).
	ancestryProvenanceStoreAgent ancestryProvenance = "store_agent"
)

// localAncestryProvenanceIdentity is implemented only by identity wrappers
// whose ancestry chain has recognized local provenance: signed by this hub
// (agent JWT) or persisted by this hub (store-derived wrappers), or is
// itself the root of the chain (the dev user). The method is unexported so
// that a type outside package hub cannot implement it, and so that a type
// inside package hub — including a test fake — must opt in explicitly
// rather than acquiring attestation by accident (e.g. by merely returning
// Type() == "agent"). The local user types declared with the principal
// types, *AuthenticatedUser and *ScopedUserIdentity, do not implement it:
// AncestryIsHubAttested lists them explicitly instead (see
// ancestryAttestedPrincipalTypes in the guard test).
type localAncestryProvenanceIdentity interface {
	localAncestryProvenance() ancestryProvenance
}

// AncestryIsHubAttested returns true when the identity's ancestry chain has
// recognized local provenance: signed by this hub (agent JWT) or persisted
// by this hub (store-derived wrappers), or is itself the root of the chain
// (a local user). Federated agent ancestry is a remote claim about local
// principal IDs and must not be used for delegation matching or ceiling
// evaluation, so FederatedIdentity is rejected first, before any local
// allow path.
//
// This is the single predicate for ancestry trust. There will be more
// consumers of ancestry after F1.7, and each one must answer this
// question the same way — not via scattered Type() comparisons.
//
// The parameter is typed as Identity (not interface{}) so that callers
// cannot accidentally pass an unrelated type. Nil, unknown, and unrecognized
// identity types all return false (fail closed): an identity is attested
// only if it is one of the explicitly listed local user types
// (*AuthenticatedUser, *ScopedUserIdentity) or implements
// localAncestryProvenanceIdentity; unlike Type(), neither can be satisfied
// by an arbitrary or future type string. A
// typed-nil concrete identity is treated the same as a nil interface: see
// isNilIdentity.
//
// *hubDeliveryIdentity (ptone/scion#2228 part 2) never implements
// localAncestryProvenanceIdentity, so this always returns false for it: the
// credential is minted by the hub for one delivery and attests nothing
// about how the agent came to exist. For a deliver permission, relationship
// stage 2 and the progeny fact read ancestry evidence explicitly from the
// stored agent record (hubDeliveryIdentity.evidence) instead — see
// relationshipStageAncestryAttested (authz_delivery_credential.go).
// ProgenyListPredicate and EvaluateProgeny call this function directly, so
// they return not-attested / match-nothing for a hub_delivery principal.
// Every other consumer of this function (the
// step-10 pre-backfill allow, messaging, material_grants, material_runtime)
// stays on its not-attested path too, so no consumer extends trust to the
// credential without an explicit arm.
func AncestryIsHubAttested(identity Identity) bool {
	if isNilIdentity(identity) {
		return false
	}
	// All FederatedIdentity types (FederatedAgentIdentity,
	// FederatedUserIdentity, FederatedServiceIdentity) are NOT
	// hub-attested. Test the interface, not a single concrete type. This
	// check comes first: federated ancestry must never reach the local
	// allow path below, however it is packaged.
	if _, isFederated := identity.(FederatedIdentity); isFederated {
		return false
	}
	// A *ScopedUserIdentity is attested unconditionally by the type switch
	// below, whatever UserIdentity it wraps — because IssuerURL is not part
	// of the UserIdentity interface's method set, Go does not promote it
	// through the embedded field, so a ScopedUserIdentity wrapping a
	// FederatedUserIdentity would not satisfy the FederatedIdentity check
	// above. Unwrap explicitly instead of trusting the outer type. Production
	// only ever wraps *AuthenticatedUser (useraccesstoken.go), so this path
	// is not reachable today; it exists so a future caller cannot silently
	// attest a UAT issued against a federated identity.
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		if _, wrappedFederated := scoped.UserIdentity.(FederatedIdentity); wrappedFederated {
			return false
		}
	}
	// Local users are the root of their own ancestry chain: an interactive
	// *AuthenticatedUser, and a UAT-backed *ScopedUserIdentity, which is
	// still a local user. They are listed here by exact pointer type rather
	// than through the marker below, so attestation stays decided in package
	// hub even where those types are declared. Only the pointer forms are
	// listed: a ScopedUserIdentity value is not attested, and a type that
	// embeds either one is a different type and is not attested by this
	// case. TestAncestryAttestationGuard keeps this list, the marker
	// implementations and the UserIdentity implementations in step.
	switch identity.(type) {
	case *AuthenticatedUser, *ScopedUserIdentity:
		return true
	}
	_, ok := identity.(localAncestryProvenanceIdentity)
	return ok
}
