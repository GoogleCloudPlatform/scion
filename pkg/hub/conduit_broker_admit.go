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
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Runtime Broker sessions on GET /api/v1/conduit, behind hub.conduit.
//
// A broker holds its conduit session alongside the control channel
// (/api/v1/runtime-brokers/connect). The control channel stays the only
// path for broker RPC, sync operations, PTY and broker presence; the
// conduit session is registered in the conduit registry but carries no
// work, and router.Resolve keeps answering broker requests through the
// legacy control-channel adapter (Config.Brokers).
//
// Both endpoints authenticate the broker with authenticateBrokerUpgrade,
// so the two cannot accept different credentials.

// authenticateBrokerUpgrade returns the id of the Runtime Broker that signed
// the upgrade request r, or writes a 401 and returns false. The broker
// identity installed by BrokerAuthMiddleware is used when present;
// otherwise the HMAC signature is verified here. It is the one broker
// authentication step of both the control channel and the conduit
// endpoint.
func (s *Server) authenticateBrokerUpgrade(w http.ResponseWriter, r *http.Request) (string, bool) {
	if broker := GetBrokerIdentityFromContext(r.Context()); broker != nil {
		return broker.ID(), true
	}
	brokerID := r.Header.Get(HeaderBrokerID)
	if brokerID == "" {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Broker authentication required", nil)
		return "", false
	}
	if s.brokerAuthService == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Broker authentication not enabled", nil)
		return "", false
	}
	if _, err := s.brokerAuthService.ValidateBrokerSignature(r.Context(), r); err != nil {
		slog.Error("HMAC validation failed for broker", "brokerID", brokerID, "error", err)
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed, "Invalid broker signature", nil)
		return "", false
	}
	return brokerID, true
}

// brokerExecScope is the authoritative exec_scope of a broker's conduit
// sessions. Design §3.4: exec_scope is the "runtime execution scope (e.g.
// cluster/atespace) — two brokers sharing an ID but not an exec_scope are
// NOT interchangeable for stateful ops". The hub's record of that scope is
// the runtime target of a flat Runtime Broker; a profile-based broker
// (no runtime target) is unscoped (""). The broker presents the same
// value in Hello, and admission refuses a different one with 4403.
func brokerExecScope(b *store.RuntimeBroker) string {
	if b == nil || b.RuntimeTarget == nil {
		return ""
	}
	return b.RuntimeTarget.ID
}

// handleConduitBroker serves a Runtime Broker's conduit session. The
// caller (handleConduit) has already checked hub.conduit and the method.
// The broker's endpoint incarnation is the process id its Hello presents
// (the hub keeps no authoritative value for it), and its exec_scope comes
// from the broker row.
func (s *Server) handleConduitBroker(w http.ResponseWriter, r *http.Request) {
	brokerID, ok := s.authenticateBrokerUpgrade(w, r)
	if !ok {
		return
	}
	rt := s.conduit.Load()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "Conduit relay is not running on this hub node (hub.conduit was enabled after startup; restart required)", nil)
		return
	}
	broker, err := s.store.GetRuntimeBroker(r.Context(), brokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Runtime broker no longer exists", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	p := relay.Principal{
		Kind:      registry.PrincipalBroker,
		ID:        broker.ID,
		ExecScope: brokerExecScope(broker),
	}
	// The session outlives no request deadline: it ends when the
	// connection closes or the relay drains.
	ctx := context.WithoutCancel(r.Context())
	if err := rt.relay.Serve(ctx, conn, p); err != nil && !errors.Is(err, relay.ErrNotServing) {
		slog.Debug("Conduit session ended", "broker_id", broker.ID, "error", err)
	}
	// Unlike the agent path, a broker removed during the handshake keeps
	// its epoch row: broker ids are reused, and the design keeps broker
	// epoch rows so a reused id never restarts at an older epoch (§3.4).
}

// errConduitBrokerGone is the revalidation failure of a deleted broker.
var errConduitBrokerGone = errors.New("runtime broker no longer exists")

// conduitRevalidateBroker re-reads the broker row once a broker session is
// admitted and registered: a broker deleted after handleConduitBroker's
// read is closed with 4401. It only reads. A read error other than
// not-found keeps the session (the reapers still cover it).
func (s *Server) conduitRevalidateBroker(ctx context.Context, p relay.Principal) error {
	_, err := s.store.GetRuntimeBroker(ctx, p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errConduitBrokerGone
	case err != nil:
		slog.Warn("Conduit: re-reading the runtime broker after admission failed; keeping the session", "broker_id", p.ID, "error", err)
	}
	return nil
}
