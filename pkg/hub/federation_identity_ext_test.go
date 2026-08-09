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
	"context"
	"testing"
)

// Compile-time interface compliance checks.
var (
	_ Identity          = (*FederatedServiceIdentity)(nil)
	_ FederatedIdentity = (*FederatedServiceIdentity)(nil)

	_ Identity          = (*FederatedUserIdentity)(nil)
	_ UserIdentity      = (*FederatedUserIdentity)(nil)
	_ FederatedIdentity = (*FederatedUserIdentity)(nil)

	// Existing type satisfies the new interface without modification.
	_ FederatedIdentity = (*FederatedAgentIdentity)(nil)
)

// --- FederatedServiceIdentity tests ---

func TestFederatedServiceIdentity_ID(t *testing.T) {
	f := NewFederatedServiceIdentity(
		"https://accounts.google.com", "123456789",
		"deploy-bot@my-project.iam.gserviceaccount.com",
		[]AgentTokenScope{ScopeAgentStatusUpdate},
	)

	want := "https://accounts.google.com:123456789"
	if got := f.ID(); got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
}

func TestFederatedServiceIdentity_Type(t *testing.T) {
	f := NewFederatedServiceIdentity(
		"https://accounts.google.com", "123456789",
		"deploy-bot@my-project.iam.gserviceaccount.com", nil,
	)

	if got := f.Type(); got != "federated_service" {
		t.Errorf("Type() = %q, want %q", got, "federated_service")
	}
}

func TestFederatedServiceIdentity_Accessors(t *testing.T) {
	issuer := "https://accounts.google.com"
	subject := "123456789"
	email := "deploy-bot@my-project.iam.gserviceaccount.com"

	f := NewFederatedServiceIdentity(issuer, subject, email, nil)

	if got := f.Email(); got != email {
		t.Errorf("Email() = %q, want %q", got, email)
	}
	if got := f.Subject(); got != subject {
		t.Errorf("Subject() = %q, want %q", got, subject)
	}
	if got := f.IssuerURL(); got != issuer {
		t.Errorf("IssuerURL() = %q, want %q", got, issuer)
	}
}

func TestFederatedServiceIdentity_HasScope(t *testing.T) {
	f := NewFederatedServiceIdentity(
		"https://accounts.google.com", "123456789",
		"deploy-bot@my-project.iam.gserviceaccount.com",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	tests := []struct {
		scope AgentTokenScope
		want  bool
	}{
		{ScopeAgentStatusUpdate, true},
		{ScopeAgentLogAppend, true},
		{ScopeProjectSecretRead, false},
		{ScopeAgentCreate, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.scope), func(t *testing.T) {
			if got := f.HasScope(tt.scope); got != tt.want {
				t.Errorf("HasScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestFederatedServiceIdentity_ContextIntegration(t *testing.T) {
	f := NewFederatedServiceIdentity(
		"https://accounts.google.com", "123456789",
		"deploy-bot@my-project.iam.gserviceaccount.com",
		[]AgentTokenScope{ScopeAgentStatusUpdate},
	)

	ctx := context.Background()
	ctx = contextWithIdentity(ctx, f)

	// GetIdentityFromContext returns it.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		t.Fatal("GetIdentityFromContext() returned nil")
	}
	if identity.ID() != "https://accounts.google.com:123456789" {
		t.Errorf("identity.ID() = %q, want %q", identity.ID(), "https://accounts.google.com:123456789")
	}

	// GetFederatedIdentityFromContext returns it.
	fed, ok := GetFederatedIdentityFromContext(ctx)
	if !ok || fed == nil {
		t.Fatal("GetFederatedIdentityFromContext() returned nil or false")
	}
	if fed.IssuerURL() != "https://accounts.google.com" {
		t.Errorf("fed.IssuerURL() = %q, want %q", fed.IssuerURL(), "https://accounts.google.com")
	}

	// GetAgentIdentityFromContext returns nil — service accounts are not agents.
	if agentID := GetAgentIdentityFromContext(ctx); agentID != nil {
		t.Errorf("GetAgentIdentityFromContext() = %v, want nil", agentID)
	}

	// GetUserIdentityFromContext returns nil — service accounts are not users.
	if userID := GetUserIdentityFromContext(ctx); userID != nil {
		t.Errorf("GetUserIdentityFromContext() = %v, want nil", userID)
	}

	// GetAgentFromContext returns nil — no AgentTokenClaims.
	if claims := GetAgentFromContext(ctx); claims != nil {
		t.Errorf("GetAgentFromContext() = %v, want nil", claims)
	}
}

// --- FederatedUserIdentity tests ---

func TestFederatedUserIdentity_ID(t *testing.T) {
	f := NewFederatedUserIdentity(
		"https://securetoken.google.com/my-project", "abcdef123456",
		"user@example.com", "Test User", "viewer",
		[]AgentTokenScope{ScopeAgentStatusUpdate},
	)

	want := "https://securetoken.google.com/my-project:abcdef123456"
	if got := f.ID(); got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
}

func TestFederatedUserIdentity_Type(t *testing.T) {
	f := NewFederatedUserIdentity(
		"https://securetoken.google.com/my-project", "abcdef123456",
		"user@example.com", "Test User", "viewer", nil,
	)

	if got := f.Type(); got != "federated_user" {
		t.Errorf("Type() = %q, want %q", got, "federated_user")
	}
}

func TestFederatedUserIdentity_Accessors(t *testing.T) {
	issuer := "https://securetoken.google.com/my-project"
	subject := "abcdef123456"
	email := "user@example.com"
	displayName := "Test User"
	role := "viewer"

	f := NewFederatedUserIdentity(issuer, subject, email, displayName, role, nil)

	if got := f.Email(); got != email {
		t.Errorf("Email() = %q, want %q", got, email)
	}
	if got := f.DisplayName(); got != displayName {
		t.Errorf("DisplayName() = %q, want %q", got, displayName)
	}
	if got := f.Role(); got != role {
		t.Errorf("Role() = %q, want %q", got, role)
	}
	if got := f.Subject(); got != subject {
		t.Errorf("Subject() = %q, want %q", got, subject)
	}
	if got := f.IssuerURL(); got != issuer {
		t.Errorf("IssuerURL() = %q, want %q", got, issuer)
	}
}

func TestFederatedUserIdentity_HasScope(t *testing.T) {
	f := NewFederatedUserIdentity(
		"https://securetoken.google.com/my-project", "abcdef123456",
		"user@example.com", "Test User", "viewer",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	tests := []struct {
		scope AgentTokenScope
		want  bool
	}{
		{ScopeAgentStatusUpdate, true},
		{ScopeAgentLogAppend, true},
		{ScopeProjectSecretRead, false},
		{ScopeAgentCreate, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.scope), func(t *testing.T) {
			if got := f.HasScope(tt.scope); got != tt.want {
				t.Errorf("HasScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestFederatedUserIdentity_ContextIntegration(t *testing.T) {
	f := NewFederatedUserIdentity(
		"https://securetoken.google.com/my-project", "abcdef123456",
		"user@example.com", "Test User", "viewer",
		[]AgentTokenScope{ScopeAgentStatusUpdate},
	)

	ctx := context.Background()
	ctx = contextWithIdentity(ctx, f)

	// GetIdentityFromContext returns it.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		t.Fatal("GetIdentityFromContext() returned nil")
	}
	if identity.ID() != "https://securetoken.google.com/my-project:abcdef123456" {
		t.Errorf("identity.ID() = %q, want %q", identity.ID(), "https://securetoken.google.com/my-project:abcdef123456")
	}

	// GetFederatedIdentityFromContext returns it.
	fed, ok := GetFederatedIdentityFromContext(ctx)
	if !ok || fed == nil {
		t.Fatal("GetFederatedIdentityFromContext() returned nil or false")
	}
	if fed.IssuerURL() != "https://securetoken.google.com/my-project" {
		t.Errorf("fed.IssuerURL() = %q, want %q", fed.IssuerURL(), "https://securetoken.google.com/my-project")
	}

	// GetUserIdentityFromContext returns it — federated users implement UserIdentity.
	userID := GetUserIdentityFromContext(ctx)
	if userID == nil {
		t.Fatal("GetUserIdentityFromContext() returned nil, expected FederatedUserIdentity")
	}
	if userID.Email() != "user@example.com" {
		t.Errorf("userID.Email() = %q, want %q", userID.Email(), "user@example.com")
	}
	if userID.DisplayName() != "Test User" {
		t.Errorf("userID.DisplayName() = %q, want %q", userID.DisplayName(), "Test User")
	}
	if userID.Role() != "viewer" {
		t.Errorf("userID.Role() = %q, want %q", userID.Role(), "viewer")
	}

	// GetAgentIdentityFromContext returns nil — federated users are not agents.
	if agentID := GetAgentIdentityFromContext(ctx); agentID != nil {
		t.Errorf("GetAgentIdentityFromContext() = %v, want nil", agentID)
	}

	// GetAgentFromContext returns nil — no AgentTokenClaims.
	if claims := GetAgentFromContext(ctx); claims != nil {
		t.Errorf("GetAgentFromContext() = %v, want nil", claims)
	}
}

// --- FederatedAgentIdentity satisfies FederatedIdentity ---

func TestFederatedAgentIdentity_SatisfiesFederatedIdentity(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub-a.example.com", "agent-42", "proj-99", "test-agent", "user-origin",
		[]string{"user-origin"}, []AgentTokenScope{ScopeAgentStatusUpdate},
	)

	ctx := context.Background()
	ctx = contextWithIdentity(ctx, f)

	// GetFederatedIdentityFromContext returns the FederatedAgentIdentity.
	fed, ok := GetFederatedIdentityFromContext(ctx)
	if !ok || fed == nil {
		t.Fatal("GetFederatedIdentityFromContext() returned nil or false for FederatedAgentIdentity")
	}
	if fed.IssuerURL() != "https://hub-a.example.com" {
		t.Errorf("fed.IssuerURL() = %q, want %q", fed.IssuerURL(), "https://hub-a.example.com")
	}
	if fed.ID() != "https://hub-a.example.com:agent-42" {
		t.Errorf("fed.ID() = %q, want %q", fed.ID(), "https://hub-a.example.com:agent-42")
	}
}

// --- GetFederatedIdentityFromContext edge cases ---

func TestGetFederatedIdentityFromContext_EmptyContext(t *testing.T) {
	ctx := context.Background()
	fed, ok := GetFederatedIdentityFromContext(ctx)
	if ok || fed != nil {
		t.Errorf("GetFederatedIdentityFromContext(empty) = (%v, %v), want (nil, false)", fed, ok)
	}
}

func TestGetFederatedIdentityFromContext_NonFederatedIdentity(t *testing.T) {
	user := NewAuthenticatedUser("user-1", "alice@example.com", "Alice", "admin", "web")
	ctx := context.Background()
	ctx = contextWithIdentity(ctx, user)

	fed, ok := GetFederatedIdentityFromContext(ctx)
	if ok || fed != nil {
		t.Errorf("GetFederatedIdentityFromContext(local user) = (%v, %v), want (nil, false)", fed, ok)
	}
}
