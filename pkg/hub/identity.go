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

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Identity represents an authenticated identity (user or agent).
type Identity interface {
	ID() string
	Type() string // "user", "agent", "dev"
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
	projectID    string
	scopes       []string
	credentialID string
}

// NewScopedUserIdentity creates a ScopedUserIdentity.
func NewScopedUserIdentity(user UserIdentity, projectID string, scopes []string) *ScopedUserIdentity {
	return NewScopedUserIdentityWithCredentialID(user, projectID, scopes, "")
}

// NewScopedUserIdentityWithCredentialID creates a UAT-backed identity with
// its persisted credential ID available for authorization audit context.
func NewScopedUserIdentityWithCredentialID(user UserIdentity, projectID string, scopes []string, credentialID string) *ScopedUserIdentity {
	return &ScopedUserIdentity{
		UserIdentity: user,
		projectID:    projectID,
		scopes:       scopes,
		credentialID: credentialID,
	}
}

// ScopedProjectID returns the project this identity is restricted to.
func (s *ScopedUserIdentity) ScopedProjectID() string { return s.projectID }

// ScopedScopes returns the action scopes this identity is limited to.
func (s *ScopedUserIdentity) ScopedScopes() []string { return s.scopes }

// CredentialID returns the persisted ID of the UAT that authenticated this identity.
func (s *ScopedUserIdentity) CredentialID() string { return s.credentialID }

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
	if user == nil || user.Role() != "admin" || IsScopedUserIdentity(user) {
		return false
	}
	_, federated := user.(FederatedIdentity)
	return !federated
}

// AncestryIsHubAttested returns true when the identity's ancestry chain
// was signed by this hub and can be trusted for delegation decisions.
// Federated agent ancestry is a remote claim about local principal IDs
// and must not be used for delegation matching or ceiling evaluation.
//
// This is the single predicate for ancestry trust. There will be more
// consumers of ancestry after F1.7, and each one must answer this
// question the same way — not via scattered Type() comparisons.
//
// The parameter is typed as Identity (not interface{}) so that callers
// cannot accidentally pass an unrelated type. Unknown identity types
// return false (fail closed — unknown is not attested).
func AncestryIsHubAttested(identity Identity) bool {
	if identity == nil {
		return false
	}
	// All FederatedIdentity types (FederatedAgentIdentity,
	// FederatedUserIdentity, FederatedServiceIdentity) are NOT
	// hub-attested. Test the interface, not a single concrete type.
	_, isFederated := identity.(FederatedIdentity)
	return !isFederated
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

// ProjectID returns the project ID.
func (a *agentIdentityWrapper) ProjectID() string { return a.AgentTokenClaims.ProjectID }

// Scopes returns the agent scopes.
func (a *agentIdentityWrapper) Scopes() []AgentTokenScope { return a.AgentTokenClaims.Scopes }

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
	if identity == nil {
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
	if identity == nil {
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

// contextWithCredentialContext records credential caveats for request-based authorization.
func contextWithCredentialContext(ctx context.Context, credential CredentialContext) context.Context {
	return context.WithValue(ctx, credentialContextKey{}, credential)
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
)

// contextWithAuthType returns a new context with the auth type set.
func contextWithAuthType(ctx context.Context, authType string) context.Context {
	return context.WithValue(ctx, logging.AuthTypeKey{}, authType)
}
