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

import "context"

// IssuerType determines how tokens from a trusted issuer are parsed
// and what identity type they produce.
type IssuerType string

const (
	// IssuerTypeHub is a Scion Hub OIDC IdP. Tokens carry project_id,
	// agent_name, ancestry, root_user claims. Produces FederatedAgentIdentity.
	IssuerTypeHub IssuerType = "hub"

	// IssuerTypeServiceAccount is a cloud provider issuing SA tokens
	// (e.g. accounts.google.com). Tokens carry sub, email, aud.
	// Produces FederatedServiceIdentity.
	IssuerTypeServiceAccount IssuerType = "service_account"

	// IssuerTypeUser is an identity provider issuing user tokens
	// (e.g. Firebase Auth, Google Sign-In). Tokens carry sub, email,
	// optionally name. Produces FederatedUserIdentity.
	IssuerTypeUser IssuerType = "user"
)

// FederatedIdentity is implemented by all identity types that originate from
// an external OIDC issuer. It provides the common federation metadata.
type FederatedIdentity interface {
	Identity
	IssuerURL() string
}

// GetFederatedIdentityFromContext returns the identity if it implements FederatedIdentity.
func GetFederatedIdentityFromContext(ctx context.Context) (FederatedIdentity, bool) {
	id := GetIdentityFromContext(ctx)
	if id == nil {
		return nil, false
	}
	fed, ok := id.(FederatedIdentity)
	return fed, ok
}

// FederatedServiceIdentity represents a machine identity authenticated via OIDC
// from a trusted external issuer (e.g. GCP service account).
// Implements Identity and FederatedIdentity but NOT AgentIdentity — service
// accounts are not agents.
type FederatedServiceIdentity struct {
	issuerURL string
	subject   string
	email     string
	scopes    []AgentTokenScope
}

// NewFederatedServiceIdentity constructs a new FederatedServiceIdentity.
func NewFederatedServiceIdentity(issuerURL, subject, email string,
	scopes []AgentTokenScope) *FederatedServiceIdentity {
	return &FederatedServiceIdentity{
		issuerURL: issuerURL,
		subject:   subject,
		email:     email,
		scopes:    scopes,
	}
}

// ID returns the unique identifier, combining issuer URL and subject.
func (f *FederatedServiceIdentity) ID() string { return f.issuerURL + ":" + f.subject }

// Type returns the identity type ("federated_service").
func (f *FederatedServiceIdentity) Type() string { return "federated_service" }

// IssuerURL returns the OIDC issuer URL.
func (f *FederatedServiceIdentity) IssuerURL() string { return f.issuerURL }

// Email returns the service account email (e.g. sa@project.iam.gserviceaccount.com).
func (f *FederatedServiceIdentity) Email() string { return f.email }

// Subject returns the OIDC subject claim.
func (f *FederatedServiceIdentity) Subject() string { return f.subject }

// Scopes returns the configured default scopes for this issuer.
func (f *FederatedServiceIdentity) Scopes() []AgentTokenScope { return f.scopes }

// HasScope reports whether the given scope is granted.
func (f *FederatedServiceIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range f.scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// FederatedUserIdentity represents a human user authenticated via OIDC from
// a trusted external identity provider (e.g. Firebase Auth).
// Implements UserIdentity and FederatedIdentity.
type FederatedUserIdentity struct {
	issuerURL   string
	subject     string
	email       string
	displayName string
	role        string
	scopes      []AgentTokenScope
}

// NewFederatedUserIdentity constructs a new FederatedUserIdentity.
func NewFederatedUserIdentity(issuerURL, subject, email, displayName, role string,
	scopes []AgentTokenScope) *FederatedUserIdentity {
	return &FederatedUserIdentity{
		issuerURL:   issuerURL,
		subject:     subject,
		email:       email,
		displayName: displayName,
		role:        role,
		scopes:      scopes,
	}
}

// ID returns the unique identifier, combining issuer URL and subject.
func (f *FederatedUserIdentity) ID() string { return f.issuerURL + ":" + f.subject }

// Type returns the identity type ("federated_user").
func (f *FederatedUserIdentity) Type() string { return "federated_user" }

// Email returns the user email.
func (f *FederatedUserIdentity) Email() string { return f.email }

// DisplayName returns the user display name.
func (f *FederatedUserIdentity) DisplayName() string { return f.displayName }

// Role returns the role assigned to this federated user.
func (f *FederatedUserIdentity) Role() string { return f.role }

// IssuerURL returns the OIDC issuer URL.
func (f *FederatedUserIdentity) IssuerURL() string { return f.issuerURL }

// Subject returns the OIDC subject claim.
func (f *FederatedUserIdentity) Subject() string { return f.subject }

// Scopes returns the configured default scopes for this issuer.
func (f *FederatedUserIdentity) Scopes() []AgentTokenScope { return f.scopes }

// HasScope reports whether the given scope is granted.
func (f *FederatedUserIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range f.scopes {
		if s == scope {
			return true
		}
	}
	return false
}
