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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Long-lived streams re-check the credential that opened them.
//
// A stream authorized once at open keeps running after the request's
// authorization was decided. A stream that admits user access tokens
// re-checks the request's credential on a fixed interval and ends when the
// check fails, so a token that is revoked, expires, belongs to a suspended
// user or loses the permission stops receiving data within one
// interval.

// streamCredentialRecheckInterval is how often a long-lived stream
// re-checks its credential. Tests shorten it.
var streamCredentialRecheckInterval = 15 * time.Second

// streamCredentialEndedEvent is the SSE event a stream writes before it
// ends because its credential stops authorizing it. It is the same for
// every cause.
const streamCredentialEndedEvent = "event: error\ndata: {\"message\":\"stream ended: the credential does not authorize this stream\"}\n\n"

// streamCredentialStillAuthorized reports whether the credential of the
// stream request in ctx still authorizes the stream. The request is built
// as the route guard evaluated it: the credential recorded on the request
// (AuthzRequestFromContext), with the identity's credential only when none
// is recorded. The authorization decision for permission on resource must
// be allowed again and, when the request's credential is a user access
// token, the stored token must not be revoked or expired, must still have
// a valid boundary and must belong to the same active user. A missing
// identity, a non-user identity, a token credential without a token ID or
// any lookup error fails the check.
func (s *Server) streamCredentialStillAuthorized(ctx context.Context, resource Resource, action Action, permission string) bool {
	if s.authzService == nil || s.store == nil {
		return false
	}
	user, ok := GetIdentityFromContext(ctx).(UserIdentity)
	if !ok || isNilIdentity(user) {
		return false
	}
	req := AuthzRequestFromContext(ctx, resource, action)
	req.Permission = permission
	if req.Credential.Kind == CredentialKindUAT {
		if req.Credential.ID == "" {
			return false
		}
		token, err := s.store.GetUserAccessToken(ctx, req.Credential.ID)
		if err != nil || token == nil || token.Revoked || token.UserID != user.ID() {
			return false
		}
		if token.ExpiresAt != nil && time.Until(*token.ExpiresAt) <= 0 {
			return false
		}
		if token.ValidateBoundary() != nil {
			return false
		}
		owner, err := s.store.GetUser(ctx, token.UserID)
		if err != nil || owner == nil || owner.Status == store.UserStatusSuspended {
			return false
		}
	}
	return s.authzService.Decide(ctx, req).Allowed
}
