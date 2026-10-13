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
	"net"
)

// errBrokerResponseTimeout marks a control-channel request that was sent
// to the broker but got no response within the channel's own request
// timeout (BrokerConnection.TunnelRequest).
var errBrokerResponseTimeout = errors.New("broker response timeout")

// errBrokerConnectionLost marks a control-channel request that was sent to
// the broker when the connection closed before a response arrived.
var errBrokerConnectionLost = errors.New("broker connection lost")

// isUnknownDispatchOutcome reports whether err, returned by a synchronous
// create dispatch (a create, or the env finalize that starts the agent),
// leaves the start's outcome unknown: the request may have reached the
// broker, which may still be starting the agent, or have started it
// (ptone/scion#4817). The create is then kept, under an unconfirmed start
// claim that the start-claim reaper settles from the broker's inventory,
// instead of being rolled back and deleted.
//
// It deliberately lists the unknown cases, unlike startOutcomeOf, whose
// default is unconfirmed: every other error keeps today's rollback. The
// unknown cases are
//   - the dispatch's own deadline (context.DeadlineExceeded): the
//     syncDispatchTimeout bound, on a context detached from the client;
//   - the control channel's response timeout, or its connection closing
//     after the request was sent (errBrokerResponseTimeout,
//     errBrokerConnectionLost);
//   - a network timeout from the direct HTTP transport (net.Error with
//     Timeout), other than a dial failure.
//
// A request known not to have been sent (errStartRequestNotSent,
// errStartBrokerNotConnected) is never unknown, and neither is a cancelled
// context: the dispatch context is detached from the client, so a
// cancellation comes from the hub itself (a lost start claim, handled
// before this check) and is not an unanswered broker request.
func isUnknownDispatchOutcome(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errStartRequestNotSent) || errors.Is(err, errStartBrokerNotConnected) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errBrokerResponseTimeout) || errors.Is(err, errBrokerConnectionLost) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
