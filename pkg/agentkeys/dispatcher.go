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

package agentkeys

import (
	"context"
	"time"
)

// BrokerClient is the transport-level interface for reaching a single
// runtime broker's dedicated keys route. Task 1.2 implements it once per
// transport — HTTP, control-channel, hybrid, and the authenticated wrapper
// (mirroring AuthenticatedBrokerClient over brokerHTTPTransport,
// pkg/hub/brokerclient.go:27-38) — the same layering
// pkg/hub.RuntimeBrokerClient already has for messages. It is its own
// interface rather than a new method on RuntimeBrokerClient, so that
// freezing this contract does not require touching (or re-implementing in
// every existing mock of) that already-heavily-implemented interface. See
// the contract doc's "Go contract types: placement" section.
//
// brokerID and brokerEndpoint identify and address the target broker the
// same way every existing RuntimeBrokerClient method does. agentSlug is the
// broker-resolution identifier (BrokerRoutePath's "{id}" segment) — a
// separate parameter from req, exactly mirroring how
// RuntimeBrokerClient.MessageAgent takes its "agentID" (also actually a
// slug) as a positional parameter distinct from the request body.
//
// A non-nil error means "no BrokerResult was produced"; see
// ClassifyDispatchError for how to turn it into an Outcome. Implementations
// return ErrNotDispatched (wrapped or bare) for failures the Hub side proves
// on its own before sending — network refused, no synchronous route, or an
// already-expired execute-before deadline found before send — and a
// *BrokerOutcomeError for a well-formed, allow-listed decision the broker
// reported about itself; see BrokerOutcomeError's doc for exactly which
// response shapes qualify, including the one 404-without-a-parseable-body
// exception that means "this broker doesn't support keys at all." They must
// **never** return ErrTargetNotFound, ErrAgentNotRunning or
// ErrTerminalNotReady: those three are manager-level and
// broker-process-internal (see their doc comments in broker.go) — a
// BrokerClient implementation sits on the Hub side of the wire and only ever
// sees the broker's already-translated HTTP response, never the manager's
// raw Go error, so it has nothing of that shape to wrap in the first place.
// Every implementation must be single-attempt (no retry, no redirect
// following, no reconnect resend).
type BrokerClient interface {
	ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req BrokerRequest) (BrokerResult, error)
}

// Dispatcher is the Hub-side dispatch abstraction the ExecuteAgentKeys
// operation (task 2.2) calls. Task 1.2 implements it, mirroring
// pkg/hub.AgentDispatcher's DispatchAgentMessage for the existing message
// path: from target.RuntimeBrokerID it looks up the broker's endpoint and
// authentication secret (the same store lookups DispatchAgentMessage already
// performs via getBrokerEndpoint) — the *only* store access this method may
// perform — then selects a BrokerClient transport and constructs the
// BrokerRequest sent to it, with ProjectID and AgentID copied from target
// and OperationID/ExecuteBefore/Keys from this method's own arguments. It is
// deliberately its own interface for the same reason BrokerClient is: adding
// a method to AgentDispatcher would force every existing implementation and
// test double of that interface to grow a keys method before any route
// exists, which is exactly the "behaviour wiring" this contract-freezing
// task must not do.
//
// DispatchAgentKeys takes the already-resolved, already-authorized target;
// it does not itself authorize or re-resolve the *agent* (no second
// store.GetAgent call to re-derive facts 2.2 already gathered and
// authorized against — a second read there could observe a target that
// changed since those checks ran). The broker-endpoint/secret lookup above
// is the one exception, because it is infrastructure metadata about the
// broker, not a fact about the authorized target agent, and every existing
// dispatcher method already performs the identical lookup on every call.
//
// Target is the only identity input to this method — there is no separate
// agentID/projectID parameter alongside it, and no BrokerRequest parameter
// carrying its own copy of the same two fields: implementations build
// BrokerRequest.ProjectID/AgentID from target directly, so there is nothing
// left that could disagree with itself.
type Dispatcher interface {
	DispatchAgentKeys(ctx context.Context, target Target, operationID string, executeBefore time.Time, keys string) (BrokerResult, error)
}
