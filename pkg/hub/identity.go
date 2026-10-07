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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Identity represents an authenticated identity (user or agent).
type Identity interface {
	ID() string
	// Type returns a human-readable identity kind, e.g. "user", "agent",
	// "dev", "federated_user", "federated_agent", "federated_service", or
	// "broker". It is informational only — new concrete types are free to
	// return any string, including one already used by another type — and
	// must never be trusted for authorization classification. Principal and
	// credential kind, and ancestry attestation, are decided through
	// explicit type assertions and opt-in markers (principalContextForIdentity,
	// credentialContextForIdentity, AncestryIsHubAttested), which fail closed
	// on any identity they don't explicitly recognize.
	Type() string
}

// UserIdentity represents an authenticated user.
type UserIdentity interface {
	Identity
	Email() string
	DisplayName() string
	Role() string
}

// AgentIdentity represents an authenticated agent.
type AgentIdentity interface {
	Identity
	ProjectID() string
	Scopes() []AgentTokenScope
	HasScope(scope AgentTokenScope) bool
	Ancestry() []string   // Ordered ancestor chain: [root_user, ..., parent_agent]
	OriginUserID() string // Returns Ancestry[0] if present, empty string otherwise
	TokenID() string      // JWT ID (jti) of the current token
}

// AuthenticatedUser implements UserIdentity.
type AuthenticatedUser struct {
	id          string
	email       string
	displayName string
	role        string
	clientType  string // "web", "cli", "api"
}

// NewAuthenticatedUser creates a new AuthenticatedUser.
func NewAuthenticatedUser(id, email, displayName, role, clientType string) *AuthenticatedUser {
	return &AuthenticatedUser{
		id:          id,
		email:       email,
		displayName: displayName,
		role:        role,
		clientType:  clientType,
	}
}

// ID returns the user ID.
func (u *AuthenticatedUser) ID() string { return u.id }

// Type returns the identity type ("user").
func (u *AuthenticatedUser) Type() string { return "user" }

// localAncestryProvenance reports that a local user is the root of its own
// ancestry chain: it opts AuthenticatedUser into AncestryIsHubAttested.
func (u *AuthenticatedUser) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceLocalUser
}

// Email returns the user email.
func (u *AuthenticatedUser) Email() string { return u.email }

// DisplayName returns the user display name.
func (u *AuthenticatedUser) DisplayName() string { return u.displayName }

// Role returns the user role.
func (u *AuthenticatedUser) Role() string { return u.role }

// ClientType returns the client type (web, cli, api).
func (u *AuthenticatedUser) ClientType() string { return u.clientType }

// ScopedUserIdentity wraps a UserIdentity with project and scope constraints.
// It is produced when authenticating with a User Access Token (UAT).
type ScopedUserIdentity struct {
	UserIdentity
	boundary     TokenBoundary
	scopes       []string
	credentialID string
	ceiling      permissions.FrozenPermissionCeiling

	// decoration holds descriptive credential metadata: populated only by
	// UserAccessTokenService.ValidateToken from the server-validated token
	// row, and never by any other caller. nil for identities not backed by
	// a validated UAT row (e.g. constructed directly by older tests/callers).
	decoration *CredentialDecoration
}

// newScopedUserIdentity is the single constructor body every
// NewScopedUserIdentity* variant below funnels through, so the field set
// cannot drift between them.
func newScopedUserIdentity(user UserIdentity, boundary TokenBoundary, scopes []string, credentialID string, ceiling permissions.FrozenPermissionCeiling, decoration *CredentialDecoration) *ScopedUserIdentity {
	return &ScopedUserIdentity{
		UserIdentity: user,
		boundary:     boundary,
		scopes:       scopes,
		credentialID: credentialID,
		ceiling:      ceiling,
		decoration:   decoration,
	}
}

// NewScopedUserIdentity creates a ScopedUserIdentity confined to a project
// boundary. The ceiling is derived from scopes via the frozen legacy
// normalization (permissions.NormalizeLegacyUATScopes) — the same
// interpretation a real CeilingVersionUnspecified token gets — so callers
// that construct an identity directly from raw scope strings (most test
// fixtures) exercise the same permission-ID-based restriction that
// production applies. An empty projectID yields an invalid boundary (see
// TokenBoundary.Valid()), which every consumer of Boundary() must treat as
// fail-closed. A caller minting a real token should use
// NewScopedUserIdentityWithBoundaryAndDecoration with the token's actual
// persisted boundary and store.UserAccessToken.NormalizedCeiling() instead,
// so a CeilingVersionV1+ ceiling is not silently reinterpreted as legacy.
func NewScopedUserIdentity(user UserIdentity, projectID string, scopes []string) *ScopedUserIdentity {
	return NewScopedUserIdentityWithCredentialID(user, projectID, scopes, "")
}

// NewScopedUserIdentityWithCredentialID creates a project-boundary,
// UAT-backed identity with its persisted credential ID available for
// authorization audit context. See NewScopedUserIdentity for how the
// ceiling is derived and how projectID becomes a boundary.
func NewScopedUserIdentityWithCredentialID(user UserIdentity, projectID string, scopes []string, credentialID string) *ScopedUserIdentity {
	return NewScopedUserIdentityWithCeiling(user, projectID, scopes, credentialID, permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes(scopes),
	})
}

// NewScopedUserIdentityWithCeiling creates a project-boundary, UAT-backed
// identity carrying an explicit, already-normalized FrozenPermissionCeiling.
// Prefer NewScopedUserIdentityWithBoundaryAndDecoration for a token whose
// persisted boundary may be hub, not project.
func NewScopedUserIdentityWithCeiling(user UserIdentity, projectID string, scopes []string, credentialID string, ceiling permissions.FrozenPermissionCeiling) *ScopedUserIdentity {
	return newScopedUserIdentity(user, TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, scopes, credentialID, ceiling, nil)
}

// NewScopedUserIdentityWithDecoration creates a project-boundary, UAT-backed
// identity carrying descriptive credential decoration alongside its
// credential ID. The ceiling is derived from scopes the same way
// NewScopedUserIdentityWithCredentialID derives it; see
// NewScopedUserIdentityWithBoundaryAndDecoration for the canonical
// constructor that also takes an explicit boundary.
func NewScopedUserIdentityWithDecoration(user UserIdentity, projectID string, scopes []string, credentialID string, decoration *CredentialDecoration) *ScopedUserIdentity {
	return NewScopedUserIdentityWithCeilingAndDecoration(user, projectID, scopes, credentialID, permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes(scopes),
	}, decoration)
}

// NewScopedUserIdentityWithCeilingAndDecoration creates a project-boundary,
// UAT-backed identity carrying both an explicit, already-normalized
// FrozenPermissionCeiling and descriptive credential decoration. Prefer
// NewScopedUserIdentityWithBoundaryAndDecoration for a token whose persisted
// boundary may be hub, not project.
func NewScopedUserIdentityWithCeilingAndDecoration(user UserIdentity, projectID string, scopes []string, credentialID string, ceiling permissions.FrozenPermissionCeiling, decoration *CredentialDecoration) *ScopedUserIdentity {
	return newScopedUserIdentity(user, TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, scopes, credentialID, ceiling, decoration)
}

// NewScopedUserIdentityWithBoundary creates a UAT-backed identity carrying an
// explicit TokenBoundary (project or hub) and an explicit, already-normalized
// FrozenPermissionCeiling, with no credential decoration. It is for callers
// that rebuild an identity from a stored token row outside a request, such as
// scheduled dispatch, and have no decoration to carry.
func NewScopedUserIdentityWithBoundary(user UserIdentity, boundary TokenBoundary, scopes []string, credentialID string, ceiling permissions.FrozenPermissionCeiling) *ScopedUserIdentity {
	return newScopedUserIdentity(user, boundary, scopes, credentialID, ceiling, nil)
}

// NewScopedUserIdentityWithBoundaryAndDecoration creates a UAT-backed
// identity carrying an explicit TokenBoundary (project or hub), an explicit,
// already-normalized FrozenPermissionCeiling, and descriptive credential
// decoration. UserAccessTokenService.ValidateToken — the single point that
// has the server-validated token row, including its persisted boundary, in
// hand — uses this constructor, so a hub-boundary token is never
// misrepresented as project-scoped and a CeilingVersionV1+ ceiling is never
// silently reinterpreted as legacy.
func NewScopedUserIdentityWithBoundaryAndDecoration(user UserIdentity, boundary TokenBoundary, scopes []string, credentialID string, ceiling permissions.FrozenPermissionCeiling, decoration *CredentialDecoration) *ScopedUserIdentity {
	return newScopedUserIdentity(user, boundary, scopes, credentialID, ceiling, decoration)
}

// Decoration returns a deep copy of the descriptive credential metadata
// attached at authentication time, or nil if none was derived. Callers may
// freely mutate the returned value (including its Labels map) without
// affecting this identity's stored decoration.
func (s *ScopedUserIdentity) Decoration() *CredentialDecoration {
	if s.decoration == nil {
		return nil
	}
	d := s.decoration.clone()
	return &d
}

// Boundary returns the credential-side boundary (project or hub) this
// identity's UAT was issued under.
func (s *ScopedUserIdentity) Boundary() TokenBoundary { return s.boundary }

// ScopedScopes returns the action scopes this identity is limited to.
func (s *ScopedUserIdentity) ScopedScopes() []string { return s.scopes }

// CredentialID returns the persisted ID of the UAT that authenticated this identity.
func (s *ScopedUserIdentity) CredentialID() string { return s.credentialID }

// localAncestryProvenance reports that a UAT-backed identity is still a local
// user: the wrapped UserIdentity is the root of its own ancestry chain. It is
// declared directly (not inherited through the embedded UserIdentity field)
// because Go only promotes methods declared by an embedded interface's own
// method set, and localAncestryProvenance is not part of UserIdentity.
//
// This method's mere presence is what AncestryIsHubAttested tests for, so it
// cannot itself refuse to be "implemented" when the wrapped identity turns
// out to be federated — see AncestryIsHubAttested's explicit unwrap check for
// where that case is actually rejected. Production only ever wraps
// *AuthenticatedUser (useraccesstoken.go), so this path is not reachable
// today; it exists so a future caller cannot silently attest a UAT issued
// against a federated identity.
func (s *ScopedUserIdentity) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceLocalUser
}

// Ceiling returns the normalized, frozen permission ceiling this identity's
// credential carries. Every credential-scope restriction (Decide step 7a,
// CanDelegate's intersectCredentialCaveats) reads this instead of
// re-deriving permission IDs from raw scopes, so legacy and current-version
// tokens are evaluated through the exact same "empty/malformed/unknown
// denies" rule (FrozenPermissionCeiling.Allows).
func (s *ScopedUserIdentity) Ceiling() permissions.FrozenPermissionCeiling { return s.ceiling }

// IsScopedUserIdentity reports whether an identity is backed by a scoped UAT.
// Scoped credentials must not use role-only administrative bypasses.
func IsScopedUserIdentity(identity Identity) bool {
	_, ok := identity.(*ScopedUserIdentity)
	return ok
}

// IsUnscopedLocalPlatformAdmin reports whether a local, non-bearer-scoped user
// may use platform-admin bypasses. Federated identities are never local
// platform administrators, regardless of an issuer-provided role claim.
//
// Phase 1F: This function uses User.Role == "admin" as a performance fast-path.
// The startup reconciliation (ReconcileSuperAdminBindings) ensures
// bidirectional consistency: User.Role == "admin" is always backed by a
// system-scoped super-admin role binding, and a user removed from AdminEmails
// has both User.Role demoted and the super-admin binding deleted.
//
// D11-fix2: Super-admin binding removal now also happens at login time
// (provisionUser → deleteSuperAdminBinding) when demotion actually occurs,
// closing the window where IsSystemAdmin could return true despite Role
// being demoted. For contexts that need an explicit role-binding check, use
// AuthzService.IsSystemAdmin instead.
func IsUnscopedLocalPlatformAdmin(user UserIdentity) bool {
	// isNilIdentity treats a typed-nil UserIdentity (for example
	// (*AuthenticatedUser)(nil)) as missing, denying here rather than
	// reaching user.Role() below.
	if isNilIdentity(user) || user.Role() != "admin" || IsScopedUserIdentity(user) {
		return false
	}
	_, federated := user.(FederatedIdentity)
	return !federated
}

// ancestryProvenance names the recognized source of an identity's local
// ancestry chain, for explain/audit and tests. It is unexported: the value
// itself carries no authority, only the presence of a
// localAncestryProvenanceIdentity implementation does.
type ancestryProvenance string

const (
	// ancestryProvenanceLocalUser marks a local user (interactive or
	// UAT-backed) as the root of its own ancestry chain.
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
// itself the root of the chain (a local user). The method is unexported so
// that a type outside package hub cannot implement it, and so that a type
// inside package hub — including a test fake — must opt in explicitly
// rather than acquiring attestation by accident (e.g. by merely returning
// Type() == "agent").
type localAncestryProvenanceIdentity interface {
	localAncestryProvenance() ancestryProvenance
}

// isNilIdentity (scheduled_initiator.go) reports whether identity is nil at
// the interface level, or is a non-nil Identity interface value holding a
// nil concrete pointer — for example an Identity holding
// (*ScopedUserIdentity)(nil), which is never == nil even though a type
// assertion or type switch against it succeeds with a nil concrete value and
// a method call or field read on that value then dereferences a nil
// pointer. Every classifier in this package (principalContextForIdentity,
// credentialContextForIdentity, AncestryIsHubAttested) and decide's entry
// check treats that case identically to a nil interface, before doing
// anything else with identity.

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
// only if it implements localAncestryProvenanceIdentity, which — unlike
// Type() — cannot be satisfied by an arbitrary or future type string. A
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
	// A *ScopedUserIdentity is attested unconditionally by the marker check
	// below, whatever UserIdentity it wraps — because IssuerURL is not part
	// of the UserIdentity interface's method set, Go does not promote it
	// through the embedded field, so a ScopedUserIdentity wrapping a
	// FederatedUserIdentity would not satisfy the FederatedIdentity check
	// above. Unwrap explicitly instead of trusting the outer type's marker.
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		if _, wrappedFederated := scoped.UserIdentity.(FederatedIdentity); wrappedFederated {
			return false
		}
	}
	_, ok := identity.(localAncestryProvenanceIdentity)
	return ok
}

// explicitIdentityClassification is an opt-in marker for identity types that
// need a PrincipalKind/CredentialKind from principalContextForIdentity and
// credentialContextForIdentity without being one of the explicitly classified
// concrete production types those functions switch on directly
// (AuthenticatedUser, ScopedUserIdentity, DevUser, agentIdentityWrapper,
// storedAgentIdentity, peerAgentIdentity, explainAgentIdentity,
// brokerIdentityImpl, FederatedUserIdentity, FederatedAgentIdentity,
// FederatedServiceIdentity, hubDeliveryIdentity — the last classifies to
// PrincipalKindAgent / CredentialKindHubDelivery and is never hub-attested,
// see AncestryIsHubAttested). Its only current implementers are package-hub
// test fakes that stand in for one of those types (ptone/scion#2123). The
// method is unexported for the same reason localAncestryProvenance is: no
// type outside package hub can implement it, so classification can never be
// forged by an external caller, and a package-hub test fake must opt in with
// an explicit, classified method rather than acquiring a kind by accident —
// in particular, never by returning a Type() string that happens to match a
// recognized one. A type that does not implement this interface, and is not
// one of the concrete types above, is classified with an empty
// PrincipalKind/CredentialKind, which Decide's fail-closed entry check
// denies.
type explicitIdentityClassification interface {
	authzClassification() (PrincipalKind, CredentialKind)
}

// HasScope returns true if this identity has the given scope.
func (s *ScopedUserIdentity) HasScope(scope string) bool {
	for _, sc := range s.scopes {
		if sc == scope {
			return true
		}
	}
	return false
}

// agentIdentityWrapper wraps AgentTokenClaims to implement AgentIdentity.
type agentIdentityWrapper struct {
	*AgentTokenClaims
}

// ID returns the agent ID (from JWT subject).
func (a *agentIdentityWrapper) ID() string { return a.Subject }

// Type returns the identity type ("agent").
func (a *agentIdentityWrapper) Type() string { return "agent" }

// localAncestryProvenance reports that this ancestry chain came from a
// hub-signed agent JWT.
func (a *agentIdentityWrapper) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceAgentJWT
}

// ProjectID returns the project ID.
func (a *agentIdentityWrapper) ProjectID() string { return a.AgentTokenClaims.ProjectID }

// Scopes returns the agent scopes.
func (a *agentIdentityWrapper) Scopes() []AgentTokenScope { return a.AgentTokenClaims.Scopes }

// HasScope checks whether this agent identity has a given scope.
func (a *agentIdentityWrapper) HasScope(scope AgentTokenScope) bool {
	for _, s := range a.AgentTokenClaims.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Ancestry returns the ordered ancestor chain from the token claims.
func (a *agentIdentityWrapper) Ancestry() []string { return a.AgentTokenClaims.Ancestry }

// OriginUserID returns the originating user ID (first element of ancestry).
func (a *agentIdentityWrapper) TokenID() string { return a.AgentTokenClaims.ID }

func (a *agentIdentityWrapper) OriginUserID() string {
	if len(a.AgentTokenClaims.Ancestry) > 0 {
		return a.AgentTokenClaims.Ancestry[0]
	}
	return ""
}

// identityContextKey is the key for storing identity in the request context.
type identityContextKey struct{}

// credentialContextKey is the key for request credential metadata.
type credentialContextKey struct{}

// GetIdentityFromContext returns the authenticated identity (user or agent).
func GetIdentityFromContext(ctx context.Context) Identity {
	// First check for identity set by unified auth middleware
	if identity, ok := ctx.Value(identityContextKey{}).(Identity); ok {
		// A typed-nil identity (for example an Identity holding
		// (*ScopedUserIdentity)(nil)) is treated as missing, the same as a
		// nil interface; see isNilIdentity.
		if isNilIdentity(identity) {
			return nil
		}
		return identity
	}
	// Fall back to checking individual context keys for backwards compatibility
	if user := GetUserFromContext(ctx); user != nil {
		return user
	}
	if agent := GetAgentFromContext(ctx); agent != nil {
		return &agentIdentityWrapper{agent}
	}
	return nil
}

// GetUserIdentityFromContext returns the user identity if present.
func GetUserIdentityFromContext(ctx context.Context) UserIdentity {
	identity := GetIdentityFromContext(ctx)
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) is treated as missing here too, before the type
	// assertion below hands a nil concrete value to the caller.
	if isNilIdentity(identity) {
		return nil
	}
	if user, ok := identity.(UserIdentity); ok {
		return user
	}
	return nil
}

// GetAgentIdentityFromContext returns the agent identity if present.
func GetAgentIdentityFromContext(ctx context.Context) AgentIdentity {
	identity := GetIdentityFromContext(ctx)
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) is treated as missing here too, before the type
	// assertion below hands a nil concrete value to the caller.
	if isNilIdentity(identity) {
		return nil
	}
	if agent, ok := identity.(AgentIdentity); ok {
		return agent
	}
	return nil
}

// contextWithIdentity returns a new context with the identity set.
func contextWithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// GetCredentialContextFromContext returns the credential metadata recorded by
// authentication middleware. It intentionally returns a zero value when a
// legacy test or internal caller set only an identity.
func GetCredentialContextFromContext(ctx context.Context) CredentialContext {
	credential, _ := ctx.Value(credentialContextKey{}).(CredentialContext)
	return credential
}

// credentialSubjectContextKey records the identity that was current in the
// context when its credential context was recorded.
type credentialSubjectContextKey struct{}

// contextWithCredentialContext records credential caveats for request-based authorization.
// It also records the identity the context holds at that moment (the
// authentication middleware sets the identity first), so
// requestCredentialBindsIdentity can tell whether the identity was later
// replaced.
func contextWithCredentialContext(ctx context.Context, credential CredentialContext) context.Context {
	ctx = context.WithValue(ctx, credentialContextKey{}, credential)
	return context.WithValue(ctx, credentialSubjectContextKey{}, ctx.Value(identityContextKey{}))
}

// requestCredentialBindsIdentity reports whether ctx's current identity is
// the very identity the authentication middleware derived from the request's
// credentials: a credential context is present, and the identity recorded
// with it is the same object (the same pointer) as the identity ctx holds
// now. A context that
// holds only an identity, or whose identity was replaced after
// authentication (contextWithIdentity on a request context), does not bind,
// even when the replacement names the same principal.
func requestCredentialBindsIdentity(ctx context.Context) bool {
	if GetCredentialContextFromContext(ctx).Kind == "" {
		return false
	}
	subject, ok := ctx.Value(credentialSubjectContextKey{}).(Identity)
	if !ok || isNilIdentity(subject) {
		return false
	}
	current, ok := ctx.Value(identityContextKey{}).(Identity)
	if !ok || isNilIdentity(current) {
		return false
	}
	return sameIdentityObject(subject, current)
}

// sameIdentityObject reports whether a and b are the same identity object:
// both non-nil pointers of the same type to the same address. Every identity
// the authentication middleware creates is a pointer type. A value-typed
// identity never matches, even an identical copy of itself, because two
// equal values cannot be told apart from an in-process reconstruction.
func sameIdentityObject(a, b Identity) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() != reflect.Pointer || vb.Kind() != reflect.Pointer || va.IsNil() || vb.IsNil() {
		return false
	}
	return va.Type() == vb.Type() && va.Pointer() == vb.Pointer()
}

// BrokerOnBehalfOf is the hub-set marker proving that a broker-authenticated
// request's effective identity was substituted by BrokerAuthMiddleware (or
// its audited variant) after HMAC verification and a successful
// X-Scion-On-Behalf-Of resolution — never merely by the presence of the
// header or a caller-supplied broker credential. BrokerID duplicates
// Broker.ID() so a compatibility check can bind Credential.ID to it without
// re-deriving it from the interface value.
type BrokerOnBehalfOf struct {
	Broker   BrokerIdentity
	BrokerID string
}

// brokerOnBehalfOfContextKey is the context key for the BrokerOnBehalfOf marker.
type brokerOnBehalfOfContextKey struct{}

// contextWithBrokerOnBehalfOf records the BrokerOnBehalfOf marker. It is
// unexported: the only caller is the shared authenticated-broker/OBO context
// helper in brokerauth.go, invoked only after HMAC verification and a
// successful resolveOnBehalfOf. An invalid HMAC or a bare/unresolved header
// must never reach this function.
func contextWithBrokerOnBehalfOf(ctx context.Context, obo BrokerOnBehalfOf) context.Context {
	return context.WithValue(ctx, brokerOnBehalfOfContextKey{}, obo)
}

// BrokerOnBehalfOfFromContext returns the BrokerOnBehalfOf marker set by the
// broker authentication middleware, and whether one was set at all.
func BrokerOnBehalfOfFromContext(ctx context.Context) (BrokerOnBehalfOf, bool) {
	obo, ok := ctx.Value(brokerOnBehalfOfContextKey{}).(BrokerOnBehalfOf)
	return obo, ok
}

// ExecutorContext identifies what is currently executing a request, as
// distinct from the principal/credential that originally authorized the work
// (plan §3.5). It is set only by deferred-execution entry points — for
// example a scheduler firing a persisted scheduled event, or a schedule
// evaluator materializing a recurrence (E.2b) — never by an ordinary live
// request. An empty ExecutorContext is exactly what a live request looks
// like; that emptiness is the discriminator audit/log readers use to tell a
// live request from deferred execution of an earlier one.
//
// Authorization code must not read this: it exists purely for audit/log
// attribution, mirroring the same rule as CredentialDecoration.
type ExecutorContext struct {
	// Kind names the executing subsystem, e.g. "scheduler",
	// "schedule_evaluator", "broker_dispatch", or "system:<job>".
	Kind string
	// ID identifies the specific unit of deferred work, e.g.
	// "scheduled_event:<id>", "schedule:<id>", or a dispatch ID.
	ID string
}

// IsZero reports whether ec carries no executor attribution (the live-request
// case).
func (ec ExecutorContext) IsZero() bool {
	return ec.Kind == "" && ec.ID == ""
}

// executorContextKey is the context key for ExecutorContext.
type executorContextKey struct{}

// ContextWithExecutor returns a new context carrying the given executor
// attribution. Deferred-execution entry points (E.2b) call this before
// invoking authorization/audit code for the unit of work they are executing.
func ContextWithExecutor(ctx context.Context, ec ExecutorContext) context.Context {
	return context.WithValue(ctx, executorContextKey{}, ec)
}

// ExecutorContextFromContext returns the executor attribution recorded on
// ctx, if any. Ordinary live requests have none.
func ExecutorContextFromContext(ctx context.Context) (ExecutorContext, bool) {
	ec, ok := ctx.Value(executorContextKey{}).(ExecutorContext)
	return ec, ok
}

// AuthType constants for request logging.
const (
	AuthTypeJWT        = "jwt"
	AuthTypeUAT        = "uat"
	AuthTypeDevToken   = "dev-token"
	AuthTypeAgent      = "agent"
	AuthTypeBroker     = "broker"
	AuthTypeProxy      = "proxy"
	AuthTypeFederation = "federation"
	// AuthTypeSignedURL labels a credential-less request admitted on the shape
	// of a skill file capability URL (#1792). It carries no identity.
	AuthTypeSignedURL = "signed-url"
	// AuthTypeExternalBearer marks a Hub user authenticated with a bearer
	// token issued by a trusted external issuer (e.g. a Google ID token)
	// rather than a Hub-issued credential. See auth_external_bearer.go.
	AuthTypeExternalBearer = "external-bearer"
)

// contextWithAuthType returns a new context with the auth type set.
//
// E.2a (ptone/scion#2127, plan §3.1): every UnifiedAuthMiddleware branch calls
// this exactly once, after it has already called contextWithIdentity and (for
// branches that establish a credential) contextWithCredentialContext — so by
// the time this runs, ctx reflects the branch's full outcome. This is
// therefore also the single, centralized place to populate the request log's
// mutable auth fields (logging.SetRequestAuth) for every successful
// authentication branch, instead of one hand-written call per branch: a
// branch that is ever added or reordered cannot forget to log auth
// attribution, because the outcome is derived from ctx rather than
// hand-carried. Rejections (no identity ever gets set) are logged separately,
// at the point of rejection — see auth.go's UAT branch.
func contextWithAuthType(ctx context.Context, authType string) context.Context {
	ctx = context.WithValue(ctx, logging.AuthTypeKey{}, authType)
	logging.SetRequestAuth(ctx, authType, requestAuthAttrs(ctx)...)
	return ctx
}

// requestAuthAttrs builds the request-log attributes for the principal and
// credential established on ctx. See contextWithAuthType.
func requestAuthAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	// isNilIdentity, not a plain interface comparison: a typed-nil identity
	// (see isNilIdentity) must not reach identity.ID() below.
	if identity := GetIdentityFromContext(ctx); !isNilIdentity(identity) {
		attrs = append(attrs, slog.String(logging.AttrUserID, identity.ID()))
		if pc := principalContextForIdentity(identity); pc.Kind != "" {
			attrs = append(attrs, slog.String("principal_kind", string(pc.Kind)))
		}
	}
	// The "credential" group is E.1's descriptive decoration (currently UAT
	// only); other credential kinds are already fully identified by
	// auth_type and user_id, per plan §3.1(2) ("emit them only when set").
	if cc := GetCredentialContextFromContext(ctx); cc.Decoration != nil {
		attrs = append(attrs, slog.Any("credential", *cc.Decoration))
	}
	return attrs
}
