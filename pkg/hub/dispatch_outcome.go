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

// errBrokerResponseTimeout marks a broker request that was sent but got
// no response within the transport's own request timeout (the control
// channel's TunnelRequest, the conduit tunnel).
var errBrokerResponseTimeout = errors.New("broker response timeout")

// errBrokerConnectionLost marks a control-channel request that was sent to
// the broker when the connection closed before a response arrived.
var errBrokerConnectionLost = errors.New("broker connection lost")

// brokerTransportError is a transport error with a fixed text, the text the
// transport always returned, that also matches its sentinel (errors.Is), so
// the error text callers and logs see is unchanged.
type brokerTransportError struct {
	msg      string
	sentinel error
}

func (e *brokerTransportError) Error() string        { return e.msg }
func (e *brokerTransportError) Is(target error) bool { return target == e.sentinel }

// keepCredentialOnUnknownKey marks a context whose caller keeps the agent
// when a create dispatch ends with an unknown outcome.
type keepCredentialOnUnknownKey struct{}

// withKeepCredentialOnUnknownOutcome marks ctx for a create dispatch whose
// caller keeps the agent on an unknown outcome (keepUnknownOutcomeStart):
// the dispatcher then keeps the credential it minted on such an outcome,
// since the broker may be starting the agent with it. Every other caller
// keeps the dispatcher's contract of revoking on any error return, so a
// caller that rolls the create back never leaves a live credential. The
// node owning the broker connection marks a cross-node create or finalize
// too, since the requesting node keeps the agent when its wait expires.
func withKeepCredentialOnUnknownOutcome(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepCredentialOnUnknownKey{}, true)
}

// keepCredentialOnDispatchError reports whether a create dispatch that
// returned err keeps the credential it minted: only for an unknown outcome
// under a context marked by withKeepCredentialOnUnknownOutcome.
func keepCredentialOnDispatchError(ctx context.Context, err error) bool {
	keep, _ := ctx.Value(keepCredentialOnUnknownKey{}).(bool)
	return keep && isUnknownDispatchOutcome(err)
}

// isUnknownDispatchOutcome reports whether err, returned by a synchronous
// create dispatch (a create, or the env finalize that starts the agent),
// leaves the start's outcome unknown: the request may have reached the
// broker, which may still be starting the agent, or have started it
// (ptone/scion#4817). The create is then kept, under the unconfirmed start
// claim that the start-claim reaper settles from the broker's inventory,
// instead of being rolled back and deleted.
//
// It is a strict subset of what the start claim treats as unconfirmed
// (startOutcomeOf), so a kept agent always holds an unconfirmed claim and
// is never left unclaimed. Within that, it deliberately lists the unknown
// cases (startOutcomeOf's default is unconfirmed); every other error keeps
// today's rollback. The unknown cases are
//   - the dispatch's own deadline (context.DeadlineExceeded): the
//     syncDispatchTimeout bound, on a context detached from the client;
//   - the transports' response timeout, or the control channel's
//     connection closing after the request was sent
//     (errBrokerResponseTimeout, errBrokerConnectionLost);
//   - the cross-node wait expiring (ErrDispatchFailed): the intent is still
//     open, and the node holding the broker connection may still send it.
//     That node runs the dispatch with the keep flag too
//     (execDispatchCreate, execDispatchFinalizeEnv), so it keeps the
//     credential on its own unknown outcome; on a definite failure it
//     revokes, and so does this node's dispatch (revoke by agent);
//   - a network timeout from the direct HTTP transport (net.Error with
//     Timeout), other than a dial failure.
//
// A dial failure is checked first: a dial timeout also matches
// context.DeadlineExceeded (net's timeout errors implement Is), but nothing
// was sent. A request known not to have been sent, a pre-send hub error
// (startOutcomeOf releases most; a credential that could not be recorded,
// errAgentTokenRecord, is never handed to the broker either), and a
// cancelled context are never
// unknown: the dispatch context is detached from the client, so a
// cancellation comes from the hub itself (a lost start claim, handled
// before this check).
func isUnknownDispatchOutcome(err error) bool {
	if err == nil || startOutcomeOf(err) != startUnconfirmed {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return false
	}
	if errors.Is(err, errStartRequestNotSent) || errors.Is(err, errStartBrokerNotConnected) ||
		errors.Is(err, errAgentTokenRecord) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDispatchFailed) ||
		errors.Is(err, errBrokerResponseTimeout) || errors.Is(err, errBrokerConnectionLost) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
