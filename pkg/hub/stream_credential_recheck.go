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
	"io"
	"net/http"
	"time"
)

// Long-lived streams re-check the credential that opened them.
//
// A stream authorized once at open keeps running after the request's
// authorization was decided. A stream that admits user access tokens
// re-checks the request's credential on a fixed interval and ends when the
// check fails, so a token that is revoked, expires, belongs to a suspended
// user or loses the permission stops receiving data within one
// interval. The diagnostics log stream re-checks every credential
// (streamCredentialStillAuthorized); the agent and project streams and
// PTY sessions re-check only a user access token
// (streamTokenStillAuthorized), so a session keeps its stream.

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
// token, the token must still validate (bearerStillValid). A missing
// identity, a non-user identity or a failed token check fails the check.
func (s *Server) streamCredentialStillAuthorized(ctx context.Context, resource Resource, action Action, permission string) bool {
	if s.authzService == nil || s.store == nil {
		return false
	}
	user, ok := GetIdentityFromContext(ctx).(UserIdentity)
	if !ok || isNilIdentity(user) {
		return false
	}
	if !s.bearerStillValid(ctx) {
		return false
	}
	req := AuthzRequestFromContext(ctx, resource, action)
	req.Permission = permission
	return s.authzService.Decide(ctx, req).Allowed
}

// streamTokenStillAuthorized is the re-check of a stream that ends only
// for a user access token: a request whose credential is a user access
// token must pass streamCredentialStillAuthorized; any other credential
// passes, so sessions, dev and agent credentials keep their stream.
func (s *Server) streamTokenStillAuthorized(ctx context.Context, resource Resource, action Action, permission string) bool {
	if !requestCredentialIsUAT(ctx) {
		return true
	}
	return s.streamCredentialStillAuthorized(ctx, resource, action, permission)
}

// streamTokenRecheck returns the channel a stream selects on to re-check a
// user access token credential every streamCredentialRecheckInterval, and
// the function that stops it. For any other credential the channel is nil
// and never fires.
func streamTokenRecheck(ctx context.Context) (<-chan time.Time, func()) {
	if !requestCredentialIsUAT(ctx) {
		return nil, func() {}
	}
	t := time.NewTicker(streamCredentialRecheckInterval)
	return t.C, t.Stop
}

// endStreamForCredential writes streamCredentialEndedEvent unless the
// request is already cancelled. A re-check on a cancelled request can fail;
// such a stream ends without the event.
func endStreamForCredential(ctx context.Context, w io.Writer, flusher http.Flusher) {
	if ctx.Err() != nil {
		return
	}
	_, _ = fmt.Fprint(w, streamCredentialEndedEvent)
	flusher.Flush()
}
