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
	"net/http"
)

// Broker request nonces on the conduit ingress (external relay mode).
//
// A Runtime Broker signs each request with a nonce, which
// BrokerAuthService.ValidateBrokerSignature records so the same signed
// request is accepted once. In external relay mode a broker's conduit dial
// to a hub (GET /api/v1/conduit) is forwarded unchanged by the ingress to a
// separate relay, which asks a hub to Admit it; Admit replays the handshake
// through the full middleware chain (hubAdmit, conduit_relay_admit.go).
// The ingress hop therefore verifies the broker's request without recording
// its nonce, and Admit's replayed request records it, exactly once.

// conduitAdmitNonceKey marks the request hubAdmit rebuilds from a relay's
// Admit call. It is unexported and set only by withConduitAdmitNonce, so no
// request header can set it.
type conduitAdmitNonceKey struct{}

// withConduitAdmitNonce returns ctx marked as Admit's replayed handshake,
// whose broker nonce is recorded. Only hubAdmit calls it, after the calling
// relay has been authenticated as a relay peer.
func withConduitAdmitNonce(ctx context.Context) context.Context {
	return context.WithValue(ctx, conduitAdmitNonceKey{}, true)
}

// conduitAdmitNonce reports whether ctx carries the Admit marker.
func conduitAdmitNonce(ctx context.Context) bool {
	marked, _ := ctx.Value(conduitAdmitNonceKey{}).(bool)
	return marked
}

// conduitIngressDefersBrokerNonce reports whether a broker request's nonce
// is left to Admit: in external relay mode, for a conduit dial
// (GET /api/v1/conduit) that is not Admit's replayed handshake.
//
// INVARIANT this relies on: in external relay mode handleConduit never acts
// on the broker identity locally. It either proxies the dial unchanged to a
// separate relay (serveConduitIngress), whose Admit records the nonce, or
// refuses it (refuseConduitExternal, or not found / method not allowed).
// If an external-mode branch ever serves a dial locally, this predicate
// must exclude that branch.
func (s *Server) conduitIngressDefersBrokerNonce(r *http.Request) bool {
	if s.conduitExternal.Load() == nil {
		return false
	}
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/conduit" {
		return false
	}
	return !conduitAdmitNonce(r.Context())
}
