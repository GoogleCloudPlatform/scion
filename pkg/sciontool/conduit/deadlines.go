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

package conduit

import (
	"context"
	"errors"
	"strings"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Stream authorization deadlines at the target (design v2.21.16 §3.5
// "Target-side deadlines"). A stream whose open grant carries authz_until
// is closed with 4401 authz_expired at authz_until + 30s unless a renewal
// grant from the hub moves the deadline first. In enforce mode (from the
// grant-key fetch) a user stream's open grant must carry authz_until.

// errNoDeadline refuses, in enforce mode, a user stream's open grant that
// carries no authorization deadline.
var errNoDeadline = errors.New("user stream grant without authz_until")

// requireDeadline checks claims against the enforcement mode.
func (a *Agent) requireDeadline(claims *grant.Claims) error {
	if a.enforceDeadlines.Load() && strings.HasPrefix(claims.Subject, grant.SubjectUserPrefix) && claims.AuthzUntil.IsZero() {
		return errNoDeadline
	}
	return nil
}

// trackDeadline enforces the deadline of the accepted stream st, opened
// with claims, until the returned function is called. Streams whose grant
// carries no deadline are not tracked.
func (a *Agent) trackDeadline(ctx context.Context, st core.Stream, claims *grant.Claims) (untrack func()) {
	w := core.WelcomeFromContext(ctx)
	if w == nil || claims == nil || claims.AuthzUntil.IsZero() {
		return func() {}
	}
	id := st.ID()
	return a.deadlines.Track(target.BindingFromWelcome(w), id, claims, func(code uint32, reason string) {
		log.Info("Conduit: stream %d reached its authorization deadline; closing", id)
		_ = st.CloseWithCode(code, reason)
	})
}

// renewalQueueSize bounds the renewals waiting for a key fetch; a renewal
// that finds the queue full is refused.
const renewalQueueSize = 16

// pendingRenewal is a renewal waiting for a key fetch.
type pendingRenewal struct {
	ctx        context.Context
	binding    target.Binding
	streamID   uint32
	credential []byte
}

// handleRenewal is the session's OnStreamRenewal hook: it applies the
// renewal grant in credential to the stream streamID. It runs on the
// session's frame reader and never does I/O: verification is local, and a
// renewal naming an unknown kid is queued for renewalWorker, which
// fetches the keys and verifies once more. A notice without a grant
// changes nothing. A refused renewal is logged and changes nothing.
func (a *Agent) handleRenewal(ctx context.Context, streamID uint32, credential []byte) {
	if len(credential) == 0 {
		return
	}
	w := core.WelcomeFromContext(ctx)
	if w == nil {
		return
	}
	b := target.BindingFromWelcome(w)
	_, err := a.deadlines.Renew(ctx, b, streamID, credential)
	if errors.Is(err, grant.ErrUnknownKey) {
		p := pendingRenewal{ctx: ctx, binding: b, streamID: streamID, credential: append([]byte(nil), credential...)}
		select {
		case a.renewals <- p:
		default:
			log.Warn("Conduit: renewal for stream %d refused: too many renewals waiting for a key fetch", streamID)
		}
		return
	}
	if err != nil {
		log.Warn("Conduit: renewal for stream %d refused: %v", streamID, err)
	}
}

// renewalWorker serves queued renewals one at a time until ctx ends: one
// key fetch for an unknown kid (fetchForUnknownKid, so the global budget
// applies), then one more verification.
func (a *Agent) renewalWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-a.renewals:
			if p.ctx.Err() != nil {
				continue // the session ended
			}
			a.fetchForUnknownKid(p.ctx)
			if _, err := a.deadlines.Renew(p.ctx, p.binding, p.streamID, p.credential); err != nil {
				log.Warn("Conduit: renewal for stream %d refused: %v", p.streamID, err)
			}
		}
	}
}
