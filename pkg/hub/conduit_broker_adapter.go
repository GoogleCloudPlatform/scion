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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// Brokers do not hold conduit sessions until Phase 3, so the router
// resolves broker targets through this legacy adapter over the broker
// control channel. It is owner-only: it resolves a broker only when this
// hub process holds the broker's control channel, and answers
// router.ErrNoSession otherwise, so the caller keeps its pre-conduit
// behaviour. It never reads the conduit registry and never assumes a
// broker conduit capability.

// legacyBrokerTransport is the Transport recorded on a legacy broker
// resolution's Record.
const legacyBrokerTransport = "legacy-control-channel"

// legacyBrokerResolver is the router.BrokerResolver over a
// ControlChannelManager.
type legacyBrokerResolver struct {
	cc         *ControlChannelManager
	instanceID string // this process's relay instance id
}

var _ router.BrokerResolver = (*legacyBrokerResolver)(nil)

// newLegacyBrokerResolver returns the adapter for cc (which may be nil: a
// node without a control channel serves no broker). instanceID is this
// process's relay instance id, recorded as the resolution's owner.
func newLegacyBrokerResolver(cc *ControlChannelManager, instanceID string) *legacyBrokerResolver {
	return &legacyBrokerResolver{cc: cc, instanceID: instanceID}
}

// ResolveBroker resolves req's broker to the control channel this process
// holds, or returns router.ErrNoSession when it holds none.
func (a *legacyBrokerResolver) ResolveBroker(_ context.Context, req router.Request) (router.Resolved, error) {
	if a.cc == nil || req.Kind != registry.PrincipalBroker {
		return router.Resolved{}, router.ErrNoSession
	}
	hc := a.cc.GetConnection(req.ID)
	if hc == nil {
		return router.Resolved{}, router.ErrNoSession
	}
	return router.Resolved{
		Legacy: &legacyBrokerSession{cc: a.cc, conn: hc},
		Record: registry.SessionRecord{
			SessionID:       hc.GetSessionID(),
			PrincipalKind:   registry.PrincipalBroker,
			PrincipalID:     req.ID,
			RelayInstanceID: a.instanceID,
			Transport:       legacyBrokerTransport,
		},
		Want:  req.Want,
		Local: true,
	}, nil
}

// legacyBrokerSession is a resolved broker control channel. It is bound
// to the connection it was resolved on: once the broker reconnects (a new
// control channel session) or disconnects, its operations fail with
// relay.ErrStaleRoute so router.Do re-resolves within its budget.
type legacyBrokerSession struct {
	cc   *ControlChannelManager
	conn *BrokerConnection
}

var _ router.LegacySession = (*legacyBrokerSession)(nil)

// LegacyTarget implements router.LegacySession.
func (s *legacyBrokerSession) LegacyTarget() (kind, id string) {
	return registry.PrincipalBroker, s.conn.GetBrokerID()
}

// BrokerID returns the broker the session reaches.
func (s *legacyBrokerSession) BrokerID() string { return s.conn.GetBrokerID() }

// SessionID returns the control channel session id.
func (s *legacyBrokerSession) SessionID() string { return s.conn.GetSessionID() }

// current reports whether the resolved connection is still the broker's
// live control channel on this process.
func (s *legacyBrokerSession) current() bool {
	return s.cc.GetConnection(s.conn.GetBrokerID()) == s.conn
}

// OpenStream opens a multiplexed control channel stream on the resolved
// connection, exactly as ControlChannelManager.OpenStream does for the
// broker's current connection.
func (s *legacyBrokerSession) OpenStream(ctx context.Context, streamType, agentSlug, projectID string, cols, rows int) (*StreamProxy, error) {
	if !s.current() {
		return nil, &relay.StaleRouteError{Reason: "broker control channel replaced or closed"}
	}
	return s.conn.OpenStream(ctx, streamType, agentSlug, projectID, cols, rows)
}
