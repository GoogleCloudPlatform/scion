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

import "net/http"

// authorizeScheduledDispatchAgentAuthoring is the authoring precondition of
// a dispatch_agent scheduled event or schedule (create, any update, resume or
// re-target that changes what a future dispatch does or who it runs as): the
// request must carry an identity.
//
// A scoped UAT may author a dispatch_agent revision. The revision records the
// token's frozen effect ceiling with its attribution (revisionAuthorityCeiling),
// and each fire requires the token to be live and bounds the scheduled child's
// delegation edge by that ceiling (resolveScheduledAuthority,
// scheduledEffectCeiling), so the token's restrictions are applied at
// execution time. The caller's own authorization is checked separately: create
// and update also require authorizeAgentCreate, and resume requires it too.
func (s *Server) authorizeScheduledDispatchAgentAuthoring(w http.ResponseWriter, r *http.Request) bool {
	if GetIdentityFromContext(r.Context()) == nil {
		Unauthorized(w)
		return false
	}
	return true
}

// scopedUATDeniedForFutureDispatchAuthoring reports whether identity is a
// scoped UAT that must be denied when authoring or changing what a future
// scheduled dispatch does or who it runs as. The scheduler persists only the
// creator's identity, not the authoring credential's boundary and scopes, so
// a scoped credential's restrictions cannot be reconstructed and re-applied
// when the event fires. Used by authorizeScheduledMessageAuthoring
// (authorize_scheduled_message.go); dispatch_agent revisions record the
// credential's ceiling instead (authorizeScheduledDispatchAgentAuthoring).
func scopedUATDeniedForFutureDispatchAuthoring(identity Identity) bool {
	return IsScopedUserIdentity(identity)
}
