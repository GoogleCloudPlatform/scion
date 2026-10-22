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

package relayrole

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// MaxTunnelReResolves bounds how many times one tunnel request goes back to
// the hub after the target session it named turned out stale (the same
// budget the router gives one resolution).
const MaxTunnelReResolves = 2

// Tunnel is a tunnel this relay opened for a user session it holds, on the
// hub's instruction: the user stream, the target stream it is spliced to,
// and the open grant the hub minted for the target.
type Tunnel struct {
	// UserSessionID and UserStreamID name the user stream.
	UserSessionID string
	UserStreamID  uint32
	// Target is the stream on the target's own session: this relay's own
	// view when it holds the target session, otherwise the owner relay's
	// assertion in the bridge's StreamAccept.
	Target relay.TargetStream
	// OpenJTI and OpenGrant are the hub's open grant, kept unparsed.
	OpenJTI   string
	OpenGrant []byte
	// Bridge is the one internal stream to the owner relay when the
	// target session is held by another relay (nil when it is held here).
	// Renewals for the target go on it (the stream scheduler sends them); no
	// second stream is opened for them.
	Bridge relay.Bridge
}

type tunnelKey struct {
	session string
	stream  uint32
}

// tunnelTable holds the open tunnels of a Role, and the tunnel requests
// still being answered, per user session.
type tunnelTable struct {
	mu      sync.Mutex
	m       map[tunnelKey]Tunnel
	pending map[string]int
}

// reserve counts a tunnel request on the user session: it returns how many
// tunnels the session has open or is opening besides this one, and the
// release to call once the request is answered (an opened tunnel is then
// counted by its entry).
func (t *tunnelTable) reserve(session string) (others int, release func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = map[string]int{}
	}
	others = t.pending[session]
	for k := range t.m {
		if k.session == session {
			others++
		}
	}
	t.pending[session]++
	var once sync.Once
	return others, func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.pending[session]--; t.pending[session] <= 0 {
				delete(t.pending, session)
			}
		})
	}
}

func (t *tunnelTable) add(tn Tunnel) (remove func()) {
	k := tunnelKey{tn.UserSessionID, tn.UserStreamID}
	t.mu.Lock()
	if t.m == nil {
		t.m = map[tunnelKey]Tunnel{}
	}
	t.m[k] = tn
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		delete(t.m, k)
		t.mu.Unlock()
	}
}

// Tunnel returns the open tunnel on the user stream (userSessionID,
// userStreamID).
func (ro *Role) Tunnel(userSessionID string, userStreamID uint32) (Tunnel, bool) {
	ro.tunnels.mu.Lock()
	defer ro.tunnels.mu.Unlock()
	tn, ok := ro.tunnels.m[tunnelKey{userSessionID, userStreamID}]
	return tn, ok
}

// Tunnels returns the open tunnels.
func (ro *Role) Tunnels() []Tunnel {
	ro.tunnels.mu.Lock()
	defer ro.tunnels.mu.Unlock()
	out := make([]Tunnel, 0, len(ro.tunnels.m))
	for _, tn := range ro.tunnels.m {
		out = append(out, tn)
	}
	return out
}

// tunnelStaleError reports a target session the owner no longer holds or
// no longer admits (the hub resolves again without that session), or an
// owner relay that could not be reached (relayUnreachable: the hub resolves
// again without any session that relay holds, as router.Do does).
type tunnelStaleError struct {
	relayUnreachable bool
	err              error
}

func (e *tunnelStaleError) Error() string {
	return "conduit relay: the tunnel's target is stale: " + e.err.Error()
}

func (e *tunnelStaleError) Unwrap() error { return e.err }

// openTunnel opens the tunnel the hub authorized for the user session
// userSessionID: the target stream (on a session this relay holds, or
// through one bridge to its owner relay), then the user stream, spliced
// together. accepted is the hub's success answer; the user stream's id is
// set in its body. It returns a *tunnelStaleError when the target is stale.
func (ro *Role) openTunnel(ctx context.Context, userSessionID string, accepted *conduitv1.RpcResponse, in *relay.HubTunnelOpen) (*conduitv1.RpcResponse, error) {
	unavailable := tunnelUnavailable(in)
	if ro.relay == nil {
		return unavailable, nil
	}
	kind := conduit.StreamKind(in.Kind).Proto()
	open := &conduitv1.StreamOpen{Kind: kind, Params: in.Params, Grant: in.Grant}
	owner := in.Owner()
	var (
		target   conduit.Stream
		ids      relay.TargetStream
		bridge   relay.Bridge
		targetLS conduit.LocalSession
		err      error
	)
	if owner.InstanceID == ro.relay.InstanceID() {
		// Same relay: the target stream is opened here, with no bridge.
		if owner.Generation != ro.relay.Generation() {
			return nil, &tunnelStaleError{err: &relay.StaleRouteError{Reason: "relay_target_mismatch"}}
		}
		target, ids, targetLS, err = ro.relay.OpenLocalWithSession(ctx, in.TargetSessionID, in.Want(), open)
	} else {
		rec := registry.SessionRecord{
			SessionID:       in.TargetSessionID,
			PrincipalKind:   registry.PrincipalAgent,
			RelayInstanceID: owner.InstanceID,
			RelayGeneration: owner.Generation,
		}
		bridge, err = relay.OpenBridge(ctx, ro.relay.PeerClient(), owner, rec, in.Want(), open)
		if err == nil {
			target, ids = bridge, bridge.Target()
		}
	}
	var ce *conduit.CloseError
	switch {
	case errors.Is(err, relay.ErrOwnerUnreachable):
		ro.log.Debug("Conduit tunnel owner relay unreachable", "session_id", userSessionID, "owner_relay", owner.InstanceID, "error", err)
		return nil, &tunnelStaleError{relayUnreachable: true, err: err}
	case errors.Is(err, relay.ErrStaleRoute):
		ro.log.Debug("Conduit tunnel target stale", "session_id", userSessionID, "target_session_id", in.TargetSessionID, "error", err)
		return nil, &tunnelStaleError{err: err}
	case errors.As(err, &ce) && in.TargetRefusals[ce.Code] != nil:
		// The target refused the stream (target not found, forbidden): the
		// hub's answer for that close code, as on a hub-held session.
		ro.log.Info("Conduit tunnel target refused the stream", "session_id", userSessionID, "target_session_id", in.TargetSessionID, "close_code", ce.Code)
		return targetRefusal(in, ce.Code, unavailable), nil
	case err != nil:
		ro.log.Info("Conduit tunnel target could not be opened", "session_id", userSessionID, "target_session_id", in.TargetSessionID, "error", err)
		return unavailable, nil
	}

	ls, _, ok := ro.relay.Local(ctx, userSessionID)
	if !ok {
		_ = target.CloseWithCode(conduit.CloseCancelled, relay.ReasonCancelled)
		return unavailable, nil
	}
	user, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: kind, Params: in.Params})
	if err != nil {
		_ = target.CloseWithCode(conduit.CloseCancelled, relay.ReasonCancelled)
		if errors.Is(err, conduit.ErrDraining) {
			return &conduitv1.RpcResponse{Status: http.StatusServiceUnavailable, Headers: map[string]string{conduit.RetryAfterHeader: "0"}}, nil
		}
		return unavailable, nil
	}
	body, err := withTunnelStreamID(accepted.GetBody(), user.ID())
	if err != nil {
		_ = user.CloseWithCode(conduit.CloseCancelled, relay.ReasonCancelled)
		_ = target.CloseWithCode(conduit.CloseCancelled, relay.ReasonCancelled)
		return unavailable, nil
	}
	// The tunnel is recorded and handed to the stream scheduler before the
	// answer goes back, so it is re-checked for its whole life. Its timing
	// comes from the unsigned scheduling hints alone; the grant is stored and
	// passed on unparsed.
	firstRecheck, interval, fallback := hintSchedule(ro.clk.Now(), in.AuthzUntil, in.AuthzIntervalS)
	if fallback != "" {
		ro.Streams.countHintFallback(fallback)
		ro.log.Debug("Conduit tunnel: no usable scheduling hint; the stream is re-checked by the sweep", "session_id", userSessionID, "stream_id", user.ID(), "reason", fallback)
	}
	var renewer Renewer
	if targetLS != nil {
		renewer = LocalRenewer(targetLS, ids.StreamID)
	} else {
		renewer = HopRenewer(bridge)
	}
	// AuthzUntil is left unset: an unsigned hint only times re-checks and
	// never arms the relay's own deadline close. The target enforces the
	// signed deadline; a renewal result from the hub sets the relay's.
	untrack := ro.Streams.Track(TrackedStream{
		UserSessionID:  userSessionID,
		Target:         ids,
		OpenJTI:        in.OpenJTI,
		LatestGrant:    in.Grant,
		Interval:       interval,
		FirstRecheckAt: firstRecheck,
		Renewer:        renewer,
		Close: func(code uint32, reason string) {
			_ = user.CloseWithCode(code, reason)
			_ = target.CloseWithCode(code, reason)
		},
	})
	remove := ro.tunnels.add(Tunnel{
		UserSessionID: userSessionID,
		UserStreamID:  user.ID(),
		Target:        ids,
		OpenJTI:       in.OpenJTI,
		OpenGrant:     in.Grant,
		Bridge:        bridge,
	})
	go func() {
		defer func() {
			untrack()
			remove()
			if h := ro.testHookTunnelEnded; h != nil {
				h()
			}
		}()
		relay.Splice(user, target)
	}()
	ro.log.Info("Conduit tunnel opened", "session_id", userSessionID, "stream_id", user.ID(),
		"target_session_id", ids.SessionID, "bridged", bridge != nil)
	out := proto.Clone(accepted).(*conduitv1.RpcResponse)
	out.Body = body
	return out, nil
}

// tunnelUnavailable decodes the hub's answer for a tunnel that cannot be
// opened, falling back to a plain 503.
func tunnelUnavailable(in *relay.HubTunnelOpen) *conduitv1.RpcResponse {
	resp := &conduitv1.RpcResponse{}
	if len(in.Unavailable) == 0 || proto.Unmarshal(in.Unavailable, resp) != nil || resp.GetStatus() == 0 {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "agent_session_unavailable", "message": "agent session unavailable"}})
		return &conduitv1.RpcResponse{Status: http.StatusServiceUnavailable, Body: body}
	}
	return resp
}

// targetRefusal decodes the hub's answer for a target refusal with code,
// falling back to fallback.
func targetRefusal(in *relay.HubTunnelOpen, code uint32, fallback *conduitv1.RpcResponse) *conduitv1.RpcResponse {
	resp := &conduitv1.RpcResponse{}
	if proto.Unmarshal(in.TargetRefusals[code], resp) != nil || resp.GetStatus() == 0 {
		return fallback
	}
	return resp
}

// withTunnelStreamID sets the user stream's id in the JSON body of the
// hub's success answer.
func withTunnelStreamID(body []byte, streamID uint32) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("conduit relay: empty tunnel answer")
	}
	m[relay.TunnelResponseStreamIDKey] = json.RawMessage(strconv.FormatUint(uint64(streamID), 10))
	return json.Marshal(m)
}
