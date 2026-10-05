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

// Package agentkeys holds the frozen wire and dispatch contract for the
// dedicated agent keys (terminal keystroke injection) operation.
//
// This package is intentionally a leaf: it must not import pkg/hub or
// pkg/runtimebroker, so that both sides of the Hub-to-broker boundary (and
// pkg/hubclient, cmd, and pkg/agent) can depend on it without creating an
// import cycle. It contains only types, constants and pure functions — no
// HTTP handlers, no store access, no I/O.
//
// The authoritative design and rationale live in
// .design/agent-keys-contract.md (task ptone/scion#2191, "Keys 0.1"). This
// package freezes the Go shapes that document describes; it does not wire
// any route, authorizer or dispatcher — that is later tasks' job (see the
// contract doc's "Ownership and sequencing" section):
//
//   - 1.1 implements the broker-side execution primitive (agent.Manager's
//     keystroke-injection method, matching the frozen SendKeys signature and
//     sentinel errors in broker.go's doc comments, and the runtime broker's
//     dedicated keys HTTP handler).
//   - 1.2 implements Dispatcher and BrokerClient below — HTTP,
//     control-channel, hybrid, and the authenticated wrapper transports —
//     the Hub-side counterpart to 1.1.
//   - 2.1 implements authorization (ExecuteAgentKeys's auth gate).
//   - 2.2 implements the ExecuteAgentKeys Hub operation, consuming Dispatcher,
//     Request/Response, and the rate-limit and admission-window constants
//     defined here.
//   - Raw keystroke delivery through message requests has been removed;
//     message ingresses reject the retired raw field with
//     OutcomeRawInputRemoved.
package agentkeys
