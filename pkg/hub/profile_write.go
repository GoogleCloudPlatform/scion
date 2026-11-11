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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Profile and other non-agent resource writes.
//
// A user's profile resources are the records the hub keeps for the user
// outside any project: user-scope templates, skills and harness configs
// (whichever route writes them, including resource import), user-scope env
// vars and secrets, chat preferences and presence, chat attachments uploaded
// outside a project, and chat account links. They belong to hub members.
// Redeeming an invite makes the caller a hub member. A federated caller has
// visitor rights on the project and hub resources it is granted, but it is
// not a hub member: it does not create or manage these resources and does
// not redeem invites, so each of these writes refuses every federated
// identity (requireProfileWriter). Every other caller is unchanged: an
// interactive session or a dev credential passes, and a user access
// credential keeps its existing scope rules. Reads are not affected.

// profileWriteReasonFederated is the deny reason logged when a federated
// identity calls a profile or other non-agent resource write.
const profileWriteReasonFederated = "federated identity does not manage profile or other non-agent resources"

// isFederatedCaller reports whether the request's caller is a federated
// identity: the identity implements FederatedIdentity (a federated user,
// agent or service account), a user access credential identity wraps one,
// or the credential recorded by the authentication middleware is a
// federation credential. Only the federation credential path creates these; a
// user signed in through the hub's own login is never one. A request with
// no identity is not a federated caller.
func isFederatedCaller(ctx context.Context) bool {
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	if _, federated := identity.(FederatedIdentity); federated {
		return true
	}
	// IssuerURL is not promoted through ScopedUserIdentity's embedded
	// UserIdentity, so a wrapped federated identity is checked explicitly
	// (as in AncestryIsHubAttested).
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		if _, federated := scoped.UserIdentity.(FederatedIdentity); federated {
			return true
		}
	}
	return GetCredentialContextFromContext(ctx).Kind == CredentialKindFederation
}

// requireProfileWriter is the check every profile or other non-agent
// resource write runs once the caller is known. It writes 403, with
// resource_type "user" and denied_action "update", and returns false for a
// federated caller (isFederatedCaller). Every other caller passes; the route's own rules
// still apply after it.
func requireProfileWriter(w http.ResponseWriter, r *http.Request) bool {
	if !isFederatedCaller(r.Context()) {
		return true
	}
	logAuthzDenial(r, GetIdentityFromContext(r.Context()), Resource{Type: "user"}, ActionUpdate, profileWriteReasonFederated)
	writeForbiddenStructured(w, "", "user", ActionUpdate)
	return false
}

// requireTemplateProfileWriter applies requireProfileWriter to a write on
// template t when t is a user-scope template, whichever route reaches it
// (/api/v1/templates or /api/v1/users/me/templates). Templates in other
// scopes are not profile resources and pass.
func requireTemplateProfileWriter(w http.ResponseWriter, r *http.Request, t *store.Template) bool {
	if t.Scope != store.TemplateScopeUser {
		return true
	}
	return requireProfileWriter(w, r)
}

// requireSkillProfileWriter applies requireProfileWriter to a write on
// skill sk when sk is a user-scope skill. Skills in other scopes pass.
func requireSkillProfileWriter(w http.ResponseWriter, r *http.Request, sk *store.Skill) bool {
	if sk == nil || sk.Scope != store.SkillScopeUser {
		return true
	}
	return requireProfileWriter(w, r)
}
