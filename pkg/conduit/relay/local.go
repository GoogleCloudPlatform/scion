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

package relay

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// OpenLocal opens a stream on a target session this relay holds, with the
// checks the owner side of an internal stream hop applies (serveStream):
// the relay is serving, it holds the session, the session is not a user
// session, and the registry admits it for want with the stream's kind as
// the capability. It returns the stream and its target-side ids, which are
// this relay's own view of the session and the stream on it.
//
// A session this relay no longer holds or no longer admits for want is a
// stale route (ErrStaleRoute): the caller re-resolves. A registry read
// error fails closed with 4504 registry_unavailable.
func (r *Relay) OpenLocal(ctx context.Context, sessionID string, want registry.Want, open *conduitv1.StreamOpen) (conduit.Stream, TargetStream, error) {
	st, ids, _, err := r.OpenLocalWithSession(ctx, sessionID, want, open)
	return st, ids, err
}

// OpenLocalWithSession is OpenLocal that also returns the target's session,
// on which renewals for the stream are delivered (AuthRefresh{stream_id}).
func (r *Relay) OpenLocalWithSession(ctx context.Context, sessionID string, want registry.Want, open *conduitv1.StreamOpen) (conduit.Stream, TargetStream, conduit.LocalSession, error) {
	kind, err := conduit.StreamKindFromProto(open.GetKind())
	if err != nil {
		return nil, TargetStream{}, nil, closeErr(conduit.CloseProtocolError, ReasonBadFrame, err.Error())
	}
	want.Capability = string(kind)
	if !r.serving() {
		return nil, TargetStream{}, nil, &StaleRouteError{Reason: "relay_not_serving"}
	}
	ls, rec, ok := r.Local(ctx, sessionID)
	if !ok {
		return nil, TargetStream{}, nil, &StaleRouteError{Reason: "not_local"}
	}
	if rec.PrincipalKind == registry.PrincipalUser {
		// User sessions are never a routing target (design §3.10).
		return nil, TargetStream{}, nil, closeErr(conduit.CloseForbidden, ReasonForbidden, "user sessions are not routable")
	}
	d, err := r.cfg.Registry.Admission(ctx, sessionID, want)
	switch {
	case err != nil && d.Reason == registry.ReasonReadError:
		return nil, TargetStream{}, nil, closeErr(conduit.CloseRelayTimeout, ReasonRegistryUnavailable, "")
	case err != nil:
		return nil, TargetStream{}, nil, err
	case !d.Admissible:
		return nil, TargetStream{}, nil, &StaleRouteError{Reason: string(d.Reason)}
	}
	st, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{
		Kind:          open.GetKind(),
		Params:        open.GetParams(),
		Grant:         open.GetGrant(),
		OpenTimeoutMs: open.GetOpenTimeoutMs(),
	})
	if err != nil {
		if errors.Is(err, conduit.ErrDraining) {
			return nil, TargetStream{}, nil, closeErr(conduit.CloseRelayRestart, ReasonDraining, "")
		}
		return nil, TargetStream{}, nil, err
	}
	return st, TargetStream{SessionID: ls.Info().SessionID, StreamID: st.ID()}, ls, nil
}

// Splice copies between two streams until both directions finish, as an
// internal stream hop's owner does: a half-close on one side becomes
// CloseWrite on the other, a close code closes both sides with that code,
// and resizes are forwarded both ways. It returns when both streams have
// ended.
func Splice(a, b conduit.Stream) { splice(a, b) }

// PeerClient returns a client for other relays' internal APIs, signed with
// this relay's relay-peer auth and using its clock and close wait.
func (r *Relay) PeerClient() *PeerClient { return r.peerClient() }
