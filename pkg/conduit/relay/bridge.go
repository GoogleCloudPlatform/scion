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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Owner names the relay holding a target session: its instance id, the
// generation it was resolved at, and its registered internal endpoint.
type Owner struct {
	InstanceID string
	Generation int64
	Endpoint   string
}

// Bridge is one stream to a target session held by another relay, carried
// on one relay-peer authenticated internal stream to the owner relay
// (design §3.9, "Delivery" and "Internal-hop StreamAccept").
//
// Its holder (a relay splicing a user stream, or the hub node holding a
// port proxy or PTY leaf) keeps exactly this one internal stream for the
// stream's life. It is the one stream renewals for the target use
// (Renew), so none needs a second stream. The hub node holding a port
// proxy or PTY leaf sends that leaf's renewals on it; a relay splicing a
// relay-opened tunnel does not send renewals on it yet.
type Bridge interface {
	conduit.Stream
	// Renew sends AuthRefresh{stream_id, credential} on this hop; the
	// owner relay forwards it unchanged to the target stream.
	conduit.StreamRenewer
	// Target is the stream on the target's own session, as the owner
	// relay asserted it in the hop's StreamAccept. It is always valid.
	Target() TargetStream
	// Owner is the relay the bridge was opened to.
	Owner() Owner
}

// ErrBridgeNoTarget refuses a bridge whose owner relay's accept did not
// name the stream on the resolved target session.
var ErrBridgeNoTarget = errors.New("conduit relay: the owner relay's accept names no stream on the target session")

// bridge is a Bridge over a caller-side stream hop.
type bridge struct {
	*wsStream
	owner Owner
}

var (
	_ Bridge                          = (*bridge)(nil)
	_ conduit.Resizable               = (*bridge)(nil)
	_ targetStreamer                  = (*bridge)(nil)
	_ interface{ CloseWrite() error } = (*bridge)(nil)
)

// Target implements Bridge.
func (b *bridge) Target() TargetStream { return b.target }

// Owner implements Bridge.
func (b *bridge) Owner() Owner { return b.owner }

// OpenBridge opens a stream on rec, the session the caller resolved, through
// its owner relay at owner.Endpoint. owner must name the relay instance and
// generation rec was resolved at. The request is signed with pc's relay-peer
// auth (there is no unsigned fallback), and the owner re-checks admission for
// want before it opens the target stream.
//
// The target-side ids come only from the owner's accept: an accept that
// names no stream, or one on another session, is refused with 4400
// bad_frame and the hop is closed, which closes the target stream too.
func OpenBridge(ctx context.Context, pc *PeerClient, owner Owner, rec registry.SessionRecord, want registry.Want, open *conduitv1.StreamOpen) (Bridge, error) {
	if pc == nil || pc.Auth == nil {
		return nil, errors.New("conduit relay: no relay-peer auth configured")
	}
	if owner.Endpoint == "" || owner.InstanceID != rec.RelayInstanceID || owner.Generation != rec.RelayGeneration {
		return nil, &StaleRouteError{Reason: "owner does not match the resolved session"}
	}
	return NewRemoteSession(pc, owner.Endpoint, rec, want).OpenBridge(ctx, open)
}

// OpenBridge opens a stream on the session as OpenStream does, and returns
// it as a Bridge. Unlike OpenStream it refuses an owner accept that does not
// name the stream on this session (ErrBridgeNoTarget, as a 4400 close).
func (s *RemoteSession) OpenBridge(ctx context.Context, open *conduitv1.StreamOpen) (Bridge, error) {
	hop, err := s.openHop(ctx, open)
	if err != nil {
		return nil, err
	}
	if !hop.target.valid() {
		why := reason(ReasonBadFrame, "accept names no target stream")
		_ = hop.CloseWithCode(conduit.CloseProtocolError, why)
		return nil, fmt.Errorf("%w: %w", ErrBridgeNoTarget, &conduit.CloseError{Code: conduit.CloseProtocolError, Reason: why})
	}
	return &bridge{wsStream: hop, owner: Owner{
		InstanceID: s.rec.RelayInstanceID,
		Generation: s.rec.RelayGeneration,
		Endpoint:   strings.TrimRight(s.endpoint, "/"),
	}}, nil
}
