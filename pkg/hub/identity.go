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
	GroveID() string
	Scopes() []AgentTokenScope
	HasScope(scope AgentTokenScope) bool
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

// agentIdentityWrapper wraps AgentTokenClaims to implement AgentIdentity.
type agentIdentityWrapper struct {
	*AgentTokenClaims
}

// ID returns the agent ID (from JWT subject).
func (a *agentIdentityWrapper) ID() string { return a.Subject }

// Type returns the identity type ("agent").
func (a *agentIdentityWrapper) Type() string { return "agent" }

// GroveID returns the grove ID.
func (a *agentIdentityWrapper) GroveID() string { return a.AgentTokenClaims.GroveID }

// Scopes returns the agent scopes.
func (a *agentIdentityWrapper) Scopes() []AgentTokenScope { return a.AgentTokenClaims.Scopes }

// identityContextKey is the key for storing identity in the request context.
type identityContextKey struct{}

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
