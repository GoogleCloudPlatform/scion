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

// authorizeScheduledDispatchAgentAuthoring requires that authoring a
// dispatch_agent scheduled event or schedule — creating one, or any update,
// resume, or re-target that changes what a future dispatch does or who it
// runs as — use a credential whose scope can still be applied when the event
// fires. It reuses scopedUATDeniedForFutureDispatchAuthoring, the same
// predicate the scheduled-message authoring gate uses in
// authorize_scheduled_message.go, so both event kinds enforce one rule
// instead of two independently maintained checks.
//
// This gate applies at authoring time (ptone/scion#2121). Fire-time
// authorization is performed separately by authorizeScheduledAgentCreate in
// server.go.
//
// The gate denies only scoped UATs. It supplements, and does not replace,
// the caller's own authorization: create and update also require
// authorizeAgentCreate, and resume requires schedule update access.
func (s *Server) authorizeScheduledDispatchAgentAuthoring(w http.ResponseWriter, r *http.Request) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if scopedUATDeniedForFutureDispatchAuthoring(identity) {
		// Session-only with the GOV_PENDING reason (session_only_gate.go).
		writeSessionOnlyDenial(w, ErrCodeForbidden,
			"scheduled agent creation requires a credential whose scope can be applied at execution time",
			authzop.ReasonGovernancePending)
		return false
	}
	return true
}

// scopedUATDeniedForFutureDispatchAuthoring reports whether identity is a
// scoped UAT that must be denied when authoring or changing what a future
// scheduled dispatch does or who it runs as. The scheduler persists only the
// creator's identity, not the authoring credential's boundary and scopes, so
// a scoped credential's restrictions cannot be reconstructed and re-applied
// when the event fires. Shared by authorizeScheduledMessageAuthoring
// (authorize_scheduled_message.go) and
// authorizeScheduledDispatchAgentAuthoring, so both event kinds enforce the
// same rule through one predicate rather than two independently maintained
// checks.
func scopedUATDeniedForFutureDispatchAuthoring(identity Identity) bool {
	return IsScopedUserIdentity(identity)
}
