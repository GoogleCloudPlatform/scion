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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// scheduleAuthoringCredentialRefusedMessage is the message of the
// session-only refusal written by authorizeScheduleAuthoringCredential.
const scheduleAuthoringCredentialRefusedMessage = "access tokens cannot create, update or resume scheduled events or schedules; use an interactive session"

// scheduleAuthoringCredentialAllowed reports whether the request's
// credential may author scheduled work: create a scheduled event or a
// recurring schedule, update a schedule, or resume one. The rule is an
// allowlist of (identity type, credential kind) pairs:
//   - an agent identity with an agent JWT;
//   - a user or dev identity with an interactive session or a dev
//     credential (sessionCredentialAllowed), or with a broker credential
//     acting on the user's behalf;
//   - a federated user identity with a federation credential.
//
// Every user access token is refused, for every event type, as is any other
// credential kind, including an empty or unknown one. A context with no
// credential record falls back to the identity's own credential
// classification, as AuthzRequestFromContext does.
func scheduleAuthoringCredentialAllowed(r *http.Request) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil || IsScopedUserIdentity(identity) {
		return false
	}
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind == "" {
		credential = credentialContextForIdentity(identity)
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

// authorizeScheduleAuthoringCredential is the credential gate of scheduled
// work authoring (scheduleAuthoringCredentialAllowed). It runs before any
// target lookup or body read. On a refusal it writes 401 for a request with
// no identity, otherwise a session-only 403 with the GOV_PENDING reason, and
// returns false.
func authorizeScheduleAuthoringCredential(w http.ResponseWriter, r *http.Request) bool {
	if GetIdentityFromContext(r.Context()) == nil {
		Unauthorized(w)
		return false
	}
	if !scheduleAuthoringCredentialAllowed(r) {
		writeSessionOnlyDenial(w, ErrCodeForbidden, scheduleAuthoringCredentialRefusedMessage,
			authzop.ReasonGovernancePending)
		return false
	}
	return true
}

// authorizeScheduledDispatchAgentAuthoring is the authoring precondition of
// a dispatch_agent scheduled event or schedule (create, any update, resume or
// re-target that changes what a future dispatch does or who it runs as): the
// request must carry an identity whose credential may author scheduled work
// (authorizeScheduleAuthoringCredential). A user access token is refused.
//
// An admitted revision records its author's frozen effect ceiling with its
// attribution (revisionAuthorityCeiling), and each fire bounds the scheduled
// child's delegation edge by that ceiling (resolveScheduledAuthority,
// scheduledEffectCeiling). The caller's own authorization is checked
// separately: create, update and resume also require authorizeAgentCreate.
func (s *Server) authorizeScheduledDispatchAgentAuthoring(w http.ResponseWriter, r *http.Request) bool {
	return authorizeScheduleAuthoringCredential(w, r)
}
