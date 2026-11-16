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
	"net/http"
)

// deferredAuthoringCredentialRefusedMessage is the message of the refusal
// written by authorizeDeferredAuthoringCredential.
const deferredAuthoringCredentialRefusedMessage = "this credential kind cannot create, update or resume scheduled events, schedules or triggers"

// scheduleAuthoringCredentialRefusedMessage aliases
// deferredAuthoringCredentialRefusedMessage for schedule call sites and tests.
const scheduleAuthoringCredentialRefusedMessage = deferredAuthoringCredentialRefusedMessage

// deferredAuthoringCredentialAllowed reports whether the request's
// credential may author deferred work: create a scheduled event, recurring
// schedule, or trigger, or update or resume a schedule or trigger. The rule is
// an allowlist of (identity type, credential kind) pairs:
//   - an agent identity with an agent JWT;
//   - a user or dev identity with an interactive session or a dev
//     credential (sessionCredentialAllowed), or with a broker credential
//     acting on the user's behalf;
//   - a scoped user identity with a user access token;
//   - a federated user identity with a federation credential.
//
// Any other credential kind, including an empty or unknown one, is refused.
// A context with no credential record falls back to the identity's own
// credential classification, as AuthzRequestFromContext does.
//
// An admitted user access token is then held to its ceiling and boundary by
// the resource permission check (authorizeScheduledEventAccess /
// authorizeTriggerAccess) and by the action type's target checks, and the
// revision records the token's ceiling and boundary (revisionAuthorityCeiling).
func deferredAuthoringCredentialAllowed(r *http.Request) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		return false
	}
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind == "" {
		credential = credentialContextForIdentity(identity)
	}
	if IsScopedUserIdentity(identity) {
		return credential.Kind == CredentialKindUAT
	}
	switch identity.Type() {
	case "agent":
		return credential.Kind == CredentialKindAgentJWT
	case "user", "dev":
		if _, ok := identity.(UserIdentity); !ok {
			return false
		}
		return allowedMutationCredentials[credential.Kind] || credential.Kind == CredentialKindBroker
	case "federated_user":
		if _, ok := identity.(UserIdentity); !ok {
			return false
		}
		return credential.Kind == CredentialKindFederation
	default:
		return false
	}
}

func scheduleAuthoringCredentialAllowed(r *http.Request) bool {
	return deferredAuthoringCredentialAllowed(r)
}

// authorizeDeferredAuthoringCredential is the credential gate of deferred
// work authoring (deferredAuthoringCredentialAllowed). It runs before any
// target lookup or body read. On a refusal it writes 401 for a request with
// no identity, otherwise 403, and returns false.
func authorizeDeferredAuthoringCredential(w http.ResponseWriter, r *http.Request) bool {
	if GetIdentityFromContext(r.Context()) == nil {
		Unauthorized(w)
		return false
	}
	if !deferredAuthoringCredentialAllowed(r) {
		writeForbidden(w, deferredAuthoringCredentialRefusedMessage)
		return false
	}
	return true
}

// authorizeScheduleAuthoringCredential aliases
// authorizeDeferredAuthoringCredential for scheduled event and schedule routes.
func authorizeScheduleAuthoringCredential(w http.ResponseWriter, r *http.Request) bool {
	return authorizeDeferredAuthoringCredential(w, r)
}

// authorizeScheduledDispatchAgentAuthoring is the authoring precondition of
// a dispatch_agent scheduled event or schedule (create, any update, resume or
// re-target that changes what a future dispatch does or who it runs as): the
// request must carry an identity whose credential may author scheduled work
// (authorizeScheduleAuthoringCredential).
//
// An admitted revision records its author's frozen effect ceiling with its
// attribution (revisionAuthorityCeiling), and each fire bounds the scheduled
// child's delegation edge by that ceiling (resolveScheduledAuthority,
// scheduledEffectCeiling). The caller's own authorization is checked
// separately: create, update and resume also require authorizeAgentCreate.
func (s *Server) authorizeScheduledDispatchAgentAuthoring(w http.ResponseWriter, r *http.Request) bool {
	return authorizeScheduleAuthoringCredential(w, r)
}
