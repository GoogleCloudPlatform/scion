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

// Package relay is the conduit relay role (design §3.1, §3.5, §3.9): it
// terminates conduit sessions from targets (agents now, brokers in Phase 3),
// records them in the registry, keeps them live, and exposes them to the
// other relays of the deployment through the internal relay API.
//
// The relay is transport- and hub-agnostic. The hub authenticates the HTTP
// request (agent JWT, HMAC, ...) and hands the relay a transport.Conn plus
// the authenticated Principal; the relay never evaluates end-user policy.
//
// Internal relay API (served by InternalHandler, dialed by RemoteSession):
//
//	GET  /internal/v1/conduit/self                       instance id (self-check probe)
//	POST /internal/v1/conduit/sessions/{id}/rpc          RpcRequest → RpcResponse (120s cap)
//	GET  /internal/v1/conduit/sessions/{id}/stream       one WebSocket per bridged stream
//
// Every hop carries framed conduit messages; the relay terminates and
// re-frames on both sides and never splices raw bytes between sockets
// (design §3.10).
package relay
