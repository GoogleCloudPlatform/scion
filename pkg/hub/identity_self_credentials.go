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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// Credential rules for the caller's own identity reads and for identity
// binding.
//
// Self reads (admin status, self explain, the caller's groups) return the
// caller's own records. A user access token reads them limited to its
// ceiling and boundary. Identity binding (invitation redemption, chat
// account linking) is session-only with the IDENTITY_BINDING reason: a
// token does not bind a new identity to its holder.

// requestCredentialKind returns the credential kind the authentication
// middleware recorded for the request. A context with no credential record
// falls back to the identity's own classification, as
// AuthzRequestFromContext does.
func requestCredentialKind(ctx context.Context, identity Identity) CredentialKind {
	kind := GetCredentialContextFromContext(ctx).Kind
	if kind == "" {
		kind = credentialContextForIdentity(identity).Kind
	}
	return kind
}

// catalogCredentialRequestKinds maps a catalog credential kind to the
// request credential kinds it names: a session JWT is an interactive
// session or a dev credential.
var catalogCredentialRequestKinds = map[authzop.CredentialKind][]CredentialKind{
	authzop.CredentialSessionJWT: {CredentialKindInteractive, CredentialKindDev},
	authzop.CredentialScopedUAT:  {CredentialKindUAT},
	authzop.CredentialAgentJWT:   {CredentialKindAgentJWT},
}

// operationAdmitsRequestKind reports whether the catalog operation op
// declares a credential that names the request credential kind. An
// unknown operation admits nothing.
func operationAdmitsRequestKind(op authzop.OperationID, kind CredentialKind) bool {
	spec, ok := catalogOperationByID(op)
	if !ok {
		return false
	}
	for _, declared := range spec.Credentials {
		for _, k := range catalogCredentialRequestKinds[declared] {
			if k == kind {
				return true
			}
		}
	}
	return false
}

// selfReadCredential classifies the credential of a self read against the
// credentials the catalog operation op declares. ok is false for a kind
// the operation does not declare (an empty or unknown kind included), for
// a user access token kind without a token identity, and for a token
// identity with any other kind. token is the caller's token identity when
// the request carries a user access token, and nil otherwise; a token
// reads its holder's records limited to its ceiling and boundary.
func selfReadCredential(ctx context.Context, identity Identity, op authzop.OperationID) (token *ScopedUserIdentity, ok bool) {
	if isNilIdentity(identity) {
		return nil, false
	}
	kind := requestCredentialKind(ctx, identity)
	scoped, isToken := identity.(*ScopedUserIdentity)
	if isToken || kind == CredentialKindUAT {
		if !isToken || kind != CredentialKindUAT || !operationAdmitsRequestKind(op, kind) {
			return nil, false
		}
		return scoped, true
	}
	return nil, operationAdmitsRequestKind(op, kind)
}

// refuseSelfReadCredential logs and writes the 403 for a self read whose
// credential selfReadCredential refused.
func refuseSelfReadCredential(w http.ResponseWriter, r *http.Request, identity Identity, resourceType string) {
	logAuthzDenial(r, identity, Resource{Type: resourceType}, ActionRead, "credential kind not admitted for a self read")
	writeForbiddenStructured(w, "", resourceType, ActionRead)
}

// tokenAllowsPermission reports whether a token may use permissionID at
// all: its ceiling allows the permission, and the permission is eligible
// for the token's boundary kind.
func tokenAllowsPermission(token *ScopedUserIdentity, permissionID string) bool {
	if token == nil || !token.Boundary().Valid() {
		return false
	}
	return token.Ceiling().Allows(permissionID) && permissionEligibleForBoundary(permissionID, token.Boundary().Kind)
}

// filterPermissionsForToken keeps the permission IDs a token may use
// (tokenAllowsPermission), in their original order.
func filterPermissionsForToken(token *ScopedUserIdentity, permissionIDs []string) []string {
	out := make([]string, 0, len(permissionIDs))
	for _, id := range permissionIDs {
		if tokenAllowsPermission(token, id) {
			out = append(out, id)
		}
	}
	return out
}

// tokenBoundaryProjectMember reports whether the holder of a
// project-boundary token is currently a member of its boundary project
// (ProjectMembershipEvidence), checked on every request. A lookup error
// counts as not a member.
func (s *Server) tokenBoundaryProjectMember(ctx context.Context, token *ScopedUserIdentity) bool {
	if token == nil || s.authzService == nil {
		return false
	}
	boundary := token.Boundary()
	if boundary.Kind != BoundaryKindProject || boundary.ProjectID == "" {
		return false
	}
	member, _, err := s.authzService.ProjectMembershipEvidence(ctx, principalContextForIdentity(token), boundary.ProjectID)
	return err == nil && member
}

// identityBindingCredentialAllowed reports whether the request may bind an
// identity to its user: an interactive session or a dev credential of an
// identity that is not a user access token.
func identityBindingCredentialAllowed(ctx context.Context, identity Identity) bool {
	if isNilIdentity(identity) || IsScopedUserIdentity(identity) {
		return false
	}
	return allowedMutationCredentials[requestCredentialKind(ctx, identity)]
}

// requireIdentityBindingCredential writes a session-only refusal with the
// IDENTITY_BINDING reason and returns false unless
// identityBindingCredentialAllowed admits the request.
func requireIdentityBindingCredential(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if identityBindingCredentialAllowed(ctx, GetIdentityFromContext(ctx)) {
		return true
	}
	writeSessionOnlyDenial(w, ErrCodeForbidden,
		"binding an identity to the account requires an interactive session",
		authzop.ReasonIdentityBinding)
	return false
}
