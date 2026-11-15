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

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

// errConduitSeveralHubs: the broker is connected to more than one hub and
// cannot tell which one owns the agent, so it keeps exec.
var errConduitSeveralHubs = errors.New("conduit delivery: several hub connections")

// hubConduitDeliverer is the agent.ConduitDeliverer of the broker: it asks
// the hub to run a deliver-message flush (design v2.22 §2.1). Phase A uses
// it only on a broker with exactly one hub connection; otherwise it reports
// the request as not sent (agent.ErrConduitUnavailable), and the delivery
// uses exec.
type hubConduitDeliverer struct {
	s *Server
}

func (d hubConduitDeliverer) DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	var conns []*HubConnection
	for _, c := range d.s.hubConnectionList() {
		if c.HubClient != nil {
			conns = append(conns, c)
		}
	}
	switch len(conns) {
	case 0:
		return 0, agentcontrol.DeliverResponse{}, fmt.Errorf("%w: no hub connection", agent.ErrConduitUnavailable)
	case 1:
	default:
		return 0, agentcontrol.DeliverResponse{}, fmt.Errorf("%w: %w", agent.ErrConduitUnavailable, errConduitSeveralHubs)
	}
	status, resp, err := conns[0].HubClient.RuntimeBrokers().DeliverMessage(ctx, conns[0].BrokerID, agentID, req)
	if err != nil && notSent(err) {
		err = fmt.Errorf("%w: %w", agent.ErrConduitNotSent, err)
	}
	return status, resp, err
}

// notSent reports whether a transport error happened before the request
// was written: the dial failed (refused, unreachable, DNS).
func notSent(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns) || errors.Is(err, syscall.ECONNREFUSED)
}

// conduitDeliverSetter is the agent manager's hook for conduit delivery.
type conduitDeliverSetter interface {
	SetConduitDeliverer(agent.ConduitDeliverer)
}

// enableConduitDelivery gives mgr the broker's conduit deliverer. The
// manager uses it only for agents launched with hub.conduit_deliver on a
// runtime that delivers over conduit (agent.usesConduitTransport).
func (s *Server) enableConduitDelivery(mgr agent.Manager) {
	if cd, ok := mgr.(conduitDeliverSetter); ok {
		cd.SetConduitDeliverer(hubConduitDeliverer{s: s})
	}
}
