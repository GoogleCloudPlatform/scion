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
	// Renewals for the target go on it.
	Bridge relay.Bridge
}

type tunnelKey struct {
	session string
	stream  uint32
}

// tunnelTable holds the open tunnels of a Role.
type tunnelTable struct {
	mu sync.Mutex
	m  map[tunnelKey]Tunnel
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

// errTunnelStale reports a target session the owner no longer holds or no
// longer admits, or an owner relay that cannot be reached: the hub
// resolves the tunnel's target again without it.
var errTunnelStale = errors.New("conduit relay: the tunnel's target session is stale")

// openTunnel opens the tunnel the hub authorized for the user session
// userSessionID: the target stream (on a session this relay holds, or
// through one bridge to its owner relay), then the user stream, spliced
// together. accepted is the hub's success answer; the user stream's id is
// set in its body. It returns errTunnelStale when the target is stale.
func (ro *Role) openTunnel(ctx context.Context, userSessionID string, accepted *conduitv1.RpcResponse, in *relay.HubTunnelOpen) (*conduitv1.RpcResponse, error) {
	unavailable := tunnelUnavailable(in)
	if ro.relay == nil {
		return unavailable, nil
	}
	kind := conduit.StreamKind(in.Kind).Proto()
	open := &conduitv1.StreamOpen{Kind: kind, Params: in.Params, Grant: in.Grant}
	owner := in.Owner()
	var (
		target conduit.Stream
		ids    relay.TargetStream
		bridge relay.Bridge
		err    error
	)
	if owner.InstanceID == ro.relay.InstanceID() {
		// Same relay: the target stream is opened here, with no bridge.
		if owner.Generation != ro.relay.Generation() {
			return nil, errTunnelStale
		}
		target, ids, err = ro.relay.OpenLocal(ctx, in.TargetSessionID, in.Want(), open)
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
	switch {
	case errors.Is(err, relay.ErrStaleRoute), errors.Is(err, relay.ErrOwnerUnreachable):
		ro.log.Debug("Conduit tunnel target stale", "session_id", userSessionID, "target_session_id", in.TargetSessionID, "error", err)
		return nil, errTunnelStale
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
	remove := ro.tunnels.add(Tunnel{
		UserSessionID: userSessionID,
		UserStreamID:  user.ID(),
		Target:        ids,
		OpenJTI:       in.OpenJTI,
		OpenGrant:     in.Grant,
		Bridge:        bridge,
	})
	go func() {
		defer remove()
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
