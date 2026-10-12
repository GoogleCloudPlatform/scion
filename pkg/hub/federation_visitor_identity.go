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

// VisitorIdentity is a user of a trusted external issuer whose verified
// (issuer, subject) pair is linked to an active local federated account
// (store.UserKindFederated). ID() is that account's local user ID, so role
// bindings, DM keys and audit records key it like any user.
//
// It implements UserIdentity and FederatedIdentity. It is never a session,
// interactive or user access token identity: credentialContextForIdentity
// classifies it as CredentialKindFederation, and principalContextForIdentity
// as PrincipalKindVisitor (store principal type "user" for binding lookups,
// see NormalizePrincipalType). Only the federated branch of the auth
// middleware constructs one, and only while the federation.project_visitors
// experiment is on.
type VisitorIdentity struct {
	userID      string
	issuerURL   string
	subject     string
	email       string
	displayName string
}

// NewVisitorIdentity constructs a VisitorIdentity for the federated account
// userID, linked to (issuerURL, subject). email is the user's email as
// verified by the issuer (the account's federated email), never the
// account's synthetic address.
func NewVisitorIdentity(userID, issuerURL, subject, email, displayName string) *VisitorIdentity {
	return &VisitorIdentity{
		userID:      userID,
		issuerURL:   issuerURL,
		subject:     subject,
		email:       email,
		displayName: displayName,
	}
}

// ID returns the local user ID of the federated account.
func (v *VisitorIdentity) ID() string { return v.userID }

// Type returns the identity type ("visitor"). Informational only.
func (v *VisitorIdentity) Type() string { return "visitor" }

// Email returns the user's email as verified by the issuer.
func (v *VisitorIdentity) Email() string { return v.email }

// DisplayName returns the display name from the issuer's token.
func (v *VisitorIdentity) DisplayName() string { return v.displayName }

// Role returns "viewer": a visitor holds no hub role.
func (v *VisitorIdentity) Role() string { return "viewer" }

// IssuerURL returns the trusted issuer's URL.
func (v *VisitorIdentity) IssuerURL() string { return v.issuerURL }

// Subject returns the issuer's subject claim.
func (v *VisitorIdentity) Subject() string { return v.subject }

// isVisitorIdentity reports whether identity is a non-nil *VisitorIdentity.
func isVisitorIdentity(identity Identity) bool {
	v, ok := identity.(*VisitorIdentity)
	return ok && v != nil
}
