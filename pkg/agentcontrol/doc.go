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

// Package agentcontrol holds the wire contract of the hub-to-agent control
// routes that carry a body: the JSON a route accepts and answers, its
// bounds, and its validation. The hub (which calls a route over the agent's
// Conduit session), the Runtime Broker (which asks the hub to call
// deliver-message) and sciontool (which serves the routes) share it, so the
// three sides cannot drift.
//
// Conventions every route here follows (Conduit design §3.7, as amended by
// v2.22 for keyed idempotency; .design/conduit/v2.22-deliver-message.md):
//
//   - POST only, a JSON body, Content-Type application/json; unknown fields
//     and trailing data are refused with 400.
//   - A request that changes something carries an idempotency key; a replay
//     of a finished key answers the stored outcome with "replayed": true and
//     never acts twice.
//   - Answers are JSON objects with an "outcome" (and, for a 400, the
//     "field" that failed). Callers key on the status code and the outcome,
//     never on diagnostic fields such as "stage".
//   - No credential travels in either direction, and no error text from the
//     agent's commands is returned: errors are fixed strings.
//
// It imports only the standard library and pkg/agent/tmuxdeliver (for the
// key allowlist and route bounds), so sciontool can use it.
package agentcontrol
