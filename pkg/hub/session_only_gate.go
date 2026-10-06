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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// Session-only operations.
//
// An operation that refuses every user access token is session-only. Its
// refusal is a 403 with the site's existing error code and message, plus
// two detail fields:
//
//	details.reason     the authzop.SessionOnlyReason recorded for the
//	                   operation in the catalog
//	details.credential "session_required"
//
// The reason lets a client (and the bearer disposition matrix) tell a
// session-only refusal apart from a ceiling or authority denial.

// sessionRequiredCredential is the details.credential value of every
// session-only refusal.
const sessionRequiredCredential = "session_required"

// sessionOnlyDenialDetails returns the detail fields of a session-only
// refusal for reason.
func sessionOnlyDenialDetails(reason authzop.SessionOnlyReason) map[string]interface{} {
	return map[string]interface{}{
		"reason":     string(reason),
		"credential": sessionRequiredCredential,
	}
}

// isSessionOnlyDenialDetails reports whether details are the detail fields
// of a session-only refusal.
func isSessionOnlyDenialDetails(details map[string]interface{}) bool {
	return details != nil && details["credential"] == sessionRequiredCredential
}

// withSessionOnlyDenialDetails copies the session-only reason fields from
// src into dst when src is a session-only refusal, and returns dst. It
// keeps every other field of dst. A nil dst is allocated when needed.
func withSessionOnlyDenialDetails(dst, src map[string]interface{}) map[string]interface{} {
	if !isSessionOnlyDenialDetails(src) {
		return dst
	}
	if dst == nil {
		dst = make(map[string]interface{}, 2)
	}
	dst["reason"] = src["reason"]
	dst["credential"] = src["credential"]
	return dst
}

// writeSessionOnlyDenial writes a session-only refusal: 403 with code and
// message, and the reason detail fields.
func writeSessionOnlyDenial(w http.ResponseWriter, code, message string, reason authzop.SessionOnlyReason) {
	writeError(w, http.StatusForbidden, code, message, sessionOnlyDenialDetails(reason))
}

// sessionCredentialAllowed reports whether the request's credential is one
// a session-only operation accepts: an interactive session or a dev
// credential. An empty or unknown credential kind is refused.
func sessionCredentialAllowed(ctx context.Context) bool {
	return allowedMutationCredentials[GetCredentialContextFromContext(ctx).Kind]
}

// requireSessionCredentialFor admits only an interactive session or dev
// credential of a user, for a session-only operation with the given
// reason. It returns the acting user on success. On failure it writes the
// response and returns false:
//   - no authorization service: 500;
//   - no identity: 401;
//   - an identity that is not a user: 403 with no reason;
//   - any other credential kind: 403 with the session-only reason.
func (s *Server) requireSessionCredentialFor(w http.ResponseWriter, ctx context.Context, reason authzop.SessionOnlyReason) (UserIdentity, bool) {
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return nil, false
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}

	actor, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}

	if !sessionCredentialAllowed(ctx) {
		writeSessionOnlyDenial(w, ErrCodeForbidden,
			fmt.Sprintf("user mutations require an interactive session; credential kind %q is not allowed",
				GetCredentialContextFromContext(ctx).Kind),
			reason)
		return nil, false
	}

	return actor, true
}
