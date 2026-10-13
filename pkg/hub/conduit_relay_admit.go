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
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Admit (design §3.9 "Admission"): a separate relay forwards the
// handshake's credential headers and the hub admits the dialer exactly as
// it admits one on its own GET /api/v1/conduit. The forwarded request runs
// through the hub's public middleware chain (every credential verifier,
// the broker HMAC replay store included) and the route guard, then through
// the same pre-upgrade decision the public handshake uses; a refusal is
// the same status and body. An admitted dialer gets its principal and an
// admission token bound to the relay's session.

func init() {
	hubInternalOps[relay.HubAdmitPath] = (*Server).hubAdmit
}

// conduitRelayAdmitRemoteAddr is the remote address of a forwarded
// handshake: a documentation address that no trusted-proxy rule matches,
// so proxy identity is never taken from a relay.
const conduitRelayAdmitRemoteAddr = "192.0.2.1:0"

// maxRelaySessionIDLen bounds a relay-chosen session id.
const maxRelaySessionIDLen = 128

// conduitRelayNow is the clock of admission tokens (tests replace it).
var conduitRelayNow = time.Now

func (s *Server) hubAdmit(w http.ResponseWriter, r *http.Request, peer string, body []byte) {
	rt := s.conduitExternal.Load()
	if rt == nil {
		http.Error(w, "not in external relay mode", http.StatusServiceUnavailable)
		return
	}
	var req relay.HubAdmitRequest
	if err := json.Unmarshal(body, &req); err != nil || req.SessionID == "" || len(req.SessionID) > maxRelaySessionIDLen || req.Host == "" {
		http.Error(w, "invalid admit request", http.StatusBadRequest)
		return
	}
	if req.RelayInstanceID != peer {
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "admit for another relay", http.StatusConflict)
		return
	}
	row, ok, err := rt.relayRow(r.Context(), peer)
	switch {
	case err != nil:
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	case !ok || row.Generation != req.RelayGeneration:
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "relay instance or generation is not registered", http.StatusConflict)
		return
	}
	// The dialer reached this relay at its registered public URL; a
	// request naming another host is not one this relay received.
	if !relayServesHost(row.PublicEndpoint, req.Host) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "The relay does not serve this host", nil)
		return
	}
	u := &url.URL{Scheme: "http", Host: req.Host, Path: "/api/v1/conduit", RawQuery: req.RawQuery}
	fwd, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, "invalid admit request", http.StatusBadRequest)
		return
	}
	fwd.Host = req.Host
	fwd.RemoteAddr = conduitRelayAdmitRemoteAddr
	for k, v := range relay.ForwardCredentialHeaders(http.Header(req.Headers)) {
		fwd.Header[k] = v
	}
	// The relay has already taken the dialer's WebSocket upgrade.
	fwd.Header.Set("Connection", "Upgrade")
	fwd.Header.Set("Upgrade", "websocket")
	admit := func(w http.ResponseWriter, fr *http.Request) {
		a, ok := s.decideConduitFor(w, fr, true)
		if !ok {
			return
		}
		s.writeRelayAdmission(w, fr, a, req, peer)
	}
	s.applyMiddleware(s.guarded("/api/v1/conduit", admit)).ServeHTTP(w, fwd)
}

// writeRelayAdmission answers an admitted dialer: its principal and the
// admission token for the relay's session.
func (s *Server) writeRelayAdmission(w http.ResponseWriter, r *http.Request, a conduitAdmission, req relay.HubAdmitRequest, peer string) {
	now := conduitRelayNow()
	p := a.principal
	c := conduitAdmissionClaims{
		SessionID:       req.SessionID,
		Relay:           peer,
		RelayGen:        req.RelayGeneration,
		Kind:            p.Kind,
		ID:              p.ID,
		ProjectID:       p.ProjectID,
		ExecScope:       p.ExecScope,
		AgentLaunchID:   p.Agent.LaunchID,
		AgentGeneration: p.Agent.Generation,
		AdmittedAt:      now.Unix(),
	}
	if p.TokenRun != nil {
		c.TokenRunID, c.TokenRunEnforce = p.TokenRun.RunID, p.TokenRun.Enforce
	}
	c.AuthType, _ = r.Context().Value(logging.AuthTypeKey{}).(string)
	if p.Kind == registry.PrincipalUser && !relayUserAuthTypes[c.AuthType] {
		// Every later operation rebuilds the user from current state; only
		// these credential kinds can be rebuilt.
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "This credential cannot open sessions held by a separate relay", nil)
		return
	}
	switch p.Kind {
	case registry.PrincipalBroker:
		c.CredentialID = hex.EncodeToString(a.brokerSecret[:])
	case registry.PrincipalUser:
		if !a.credentialExpiry.IsZero() {
			c.CredentialExpiry = a.credentialExpiry.Unix()
		}
		if scoped, ok := a.user.(*ScopedUserIdentity); ok {
			c.CredentialID, c.ScopeProject, c.Scopes = scoped.CredentialID(), scoped.Boundary().ProjectID, scoped.ScopedScopes()
		}
	}
	setAdmissionLifetime(&c, now)
	token, err := s.mintAdmissionToken(c)
	if err != nil {
		slog.Error("Conduit relay admission: minting the admission token failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Admission failed", nil)
		return
	}
	resp := relay.HubAdmitResponse{
		AdmissionToken:  token,
		PrincipalKind:   p.Kind,
		PrincipalID:     p.ID,
		ProjectID:       p.ProjectID,
		ExecScope:       p.ExecScope,
		AgentLaunchID:   p.Agent.LaunchID,
		AgentGeneration: p.Agent.Generation,
		TokenRunID:      c.TokenRunID,
		TokenRunEnforce: c.TokenRunEnforce,
	}
	if c.CredentialExpiry > 0 {
		resp.CredentialExpiry = time.Unix(c.CredentialExpiry, 0).UTC()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// relayUserAuthTypes are the user credential kinds a separate relay's
// session may be admitted with.
var relayUserAuthTypes = map[string]bool{AuthTypeJWT: true, AuthTypeUAT: true, AuthTypeDevToken: true}

// relayServesHost reports whether host (as the dialer sent it) is the host
// of the relay's registered public endpoint. A relay without one serves no
// dialers.
func relayServesHost(publicEndpoint, host string) bool {
	if publicEndpoint == "" {
		return false
	}
	u, err := url.Parse(publicEndpoint)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host) || (u.Port() == "" && strings.EqualFold(u.Hostname(), host))
}

// relayRow returns the calling relay's registry row (false when it has
// none).
func (rt *conduitExternalRuntime) relayRow(ctx context.Context, relayID string) (registry.RelayInstance, bool, error) {
	relays, err := rt.store.ListRelays(ctx)
	if err != nil {
		return registry.RelayInstance{}, false, err
	}
	for _, ri := range relays {
		if ri.InstanceID == relayID {
			return ri, true, nil
		}
	}
	return registry.RelayInstance{}, false, nil
}
