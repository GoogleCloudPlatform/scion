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
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// errConduitLeafNotRelayHeld refuses, in external relay mode, a resolved
// session that is not held by a separate relay.
var errConduitLeafNotRelayHeld = errors.New("conduit: in external relay mode the target session must be held by a relay")

// openConduitTargetStream opens the target stream of a stream this hub node
// holds the leaf of (port proxy, PTY, tunnel) on res, the session the
// router resolved.
//
// In external relay mode every target session is held by a separate relay,
// and the stream is a strict bridge to it (relay.RemoteSession.OpenBridge):
// one relay-peer authenticated internal stream, whose owner relay must name
// the target-side stream in its accept. The bridge is returned so the leaf
// keeps it as its only stream to the target. In-process profiles open the
// stream as before and return no bridge.
func (s *Server) openConduitTargetStream(ctx context.Context, res router.Resolved, open *conduitv1.StreamOpen) (conduit.Stream, relay.Bridge, error) {
	if s.conduitExternal.Load() != nil {
		rs, ok := res.Session.(*relay.RemoteSession)
		if !ok {
			return nil, nil, errConduitLeafNotRelayHeld
		}
		b, err := rs.OpenBridge(ctx, open)
		if err != nil {
			return nil, nil, err
		}
		return b, b, nil
	}
	st, err := res.Session.OpenStream(ctx, open)
	if err != nil {
		return nil, nil, err
	}
	return st, nil, nil
}

// conduitLeafTarget returns the target-side ids of st, opened on res: the
// owner relay's assertion when the stream is a bridge, otherwise as
// conduitTargetOf.
func conduitLeafTarget(res router.Resolved, st conduit.Stream, b relay.Bridge) relay.TargetStream {
	if b != nil {
		return b.Target()
	}
	return conduitTargetOf(res, st)
}

// conduitBridgeOf returns st as a bridge when it is one (nil otherwise).
func conduitBridgeOf(st conduit.Stream) relay.Bridge {
	b, _ := st.(relay.Bridge)
	return b
}
