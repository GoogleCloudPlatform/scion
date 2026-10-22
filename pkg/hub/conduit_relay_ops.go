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
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// The relay-to-hub operations after Admit (design §3.9): RPC forward,
// credential refresh and session re-check. Each takes the session's
// principal only from its admission token, re-checks the credential's
// current status and the principal's current permissions, and answers as
// the in-process path would for that principal kind.

func init() {
	hubInternalSessionOps["rpc"] = (*Server).hubRelayRPC
	hubInternalSessionOps["refresh"] = (*Server).hubRelayRefresh
	hubInternalOps[relay.HubSessionRecheckPath] = (*Server).hubRelaySessionRecheck
}

// newConduitRelayRPCHandler builds the RPC handler of a relay-held user
// session: the unchanged in-process handler, for the identity the
// admission token names (a test seam).
var newConduitRelayRPCHandler = func(s *Server, identity UserIdentity, credExpiry time.Time) conduit.RPCHandler {
	return newConduitTunnelSession(s, identity, credExpiry)
}

// relayOpClaims verifies an operation's admission token for sessionID
// against the calling relay's current registration. On failure it writes
// the answer and returns false: 503 when the registry cannot be read, 409
// relay_target_mismatch for another relay instance or generation, 401
// otherwise. The token never appears in a log or an answer.
func (s *Server) relayOpClaims(w http.ResponseWriter, r *http.Request, rt *conduitExternalRuntime, peer, sessionID, token string, allowExpired bool) (conduitAdmissionClaims, bool) {
	row, ok, err := rt.relayRow(r.Context(), peer)
	if err != nil {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return conduitAdmissionClaims{}, false
	}
	if !ok {
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "relay is not registered", http.StatusConflict)
		return conduitAdmissionClaims{}, false
	}
	c, err := s.verifyAdmissionToken(token, admissionCheck{sessionID: sessionID, relay: peer, relayGen: row.Generation, allowExpired: allowExpired}, conduitRelayNow())
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, errAdmissionTokenRelay):
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "admission is for another relay", http.StatusConflict)
	default:
		http.Error(w, "admission refused", http.StatusUnauthorized)
	}
	return conduitAdmissionClaims{}, false
}

// conduitRelayVerdict is the per-operation re-check of an admission.
type conduitRelayVerdict struct {
	verdict   conduitAuthzVerdict
	reason    string
	closeCode uint32
	// identity is the user principal rebuilt from current state (user
	// sessions with an allowed verdict only).
	identity UserIdentity
}

// recheckRelayAdmission re-checks an admission's credential and principal
// against current state: the user row and access token, the agent row and
// its run, or the broker registration and secret. The close codes are the
// in-process ones: 4404 for a deleted agent, 4401 otherwise.
func (s *Server) recheckRelayAdmission(ctx context.Context, c conduitAdmissionClaims) conduitRelayVerdict {
	deny := func(code uint32, reason string) conduitRelayVerdict {
		return conduitRelayVerdict{verdict: conduitAuthzDenied, reason: reason, closeCode: code}
	}
	unavailable := func(reason string) conduitRelayVerdict {
		return conduitRelayVerdict{verdict: conduitAuthzUnavailable, reason: reason}
	}
	switch c.Kind {
	case registry.PrincipalUser:
		identity, verdict, reason := s.relayUserIdentity(ctx, c)
		switch verdict {
		case conduitAuthzAllowed:
			return conduitRelayVerdict{verdict: conduitAuthzAllowed, identity: identity}
		case conduitAuthzUnavailable:
			return unavailable(reason)
		default:
			return deny(conduit.CloseUnauthenticated, reason)
		}
	case registry.PrincipalAgent:
		a, err := s.store.GetAgent(ctx, c.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return conduitRelayVerdict{verdict: conduitAuthzTargetGone, reason: "agent deleted", closeCode: relay.CloseTargetNotFound}
		case err != nil:
			return unavailable("agent lookup failed")
		case !a.DeletedAt.IsZero():
			return conduitRelayVerdict{verdict: conduitAuthzTargetGone, reason: "agent deleted", closeCode: relay.CloseTargetNotFound}
		case a.ProjectID != c.ProjectID:
			return deny(conduit.CloseUnauthenticated, "agent moved to another project")
		case c.TokenRunEnforce && c.TokenRunID != a.RunID:
			return deny(conduit.CloseUnauthenticated, "credential run superseded")
		}
		// The admitting agent token's current status and the agent's
		// hold, as agent-token authentication checks them on every
		// request.
		if cs := s.authConfig.CredentialStore; cs != nil && c.CredentialID != "" {
			if _, _, err := evaluateAgentCredentialStatus(ctx, cs, c.CredentialID); errors.Is(err, errAgentCredentialRevoked) {
				return deny(conduit.CloseUnauthenticated, "agent credential revoked")
			} else if err != nil {
				return unavailable("agent credential lookup failed")
			}
		}
		if hs := s.authConfig.HoldStore; hs != nil {
			held, err := hs.HasActiveAgentHold(ctx, c.ID)
			switch {
			case err != nil:
				return unavailable("agent hold lookup failed")
			case held:
				return deny(conduit.CloseUnauthenticated, "agent held")
			}
		}
		return conduitRelayVerdict{verdict: conduitAuthzAllowed}
	case registry.PrincipalBroker:
		var fp conduitBrokerSecretFingerprint
		b, err := hex.DecodeString(c.CredentialID)
		if err != nil || len(b) != len(fp) {
			return deny(conduit.CloseUnauthenticated, "broker credential unknown")
		}
		copy(fp[:], b)
		switch verdict, reason := s.conduitBrokerCredentialVerdict(ctx, c.ID, fp); verdict {
		case conduitAuthzAllowed:
			return conduitRelayVerdict{verdict: conduitAuthzAllowed}
		case conduitAuthzUnavailable:
			return unavailable(reason)
		default:
			return deny(conduit.CloseUnauthenticated, reason)
		}
	}
	return deny(conduit.CloseUnauthenticated, "principal kind not served")
}

// relayUserIdentity rebuilds a relay-held user session's identity from
// current state: the user row (the account-status rule, checkUserAccount:
// gone or not active is denied) and, for an access token, the token row
// (revoked, expired or another owner's: denied), as scheduled dispatch
// rebuilds one. The development user is held to the same account-status
// rule as any other user and, once its row passes, is rebuilt as itself.
func (s *Server) relayUserIdentity(ctx context.Context, c conduitAdmissionClaims) (UserIdentity, conduitAuthzVerdict, string) {
	devToken := c.AuthType == AuthTypeDevToken
	if devToken && c.ID != DevUserID {
		return nil, conduitAuthzDenied, "development credential for another user"
	}
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: c.ID}
	if devToken {
		principal = devUserPrincipal()
	}
	u, err := checkUserAccount(ctx, s.store, principal)
	switch refusal := classifyUserAccount(u, err); refusal {
	case accountAllowed:
	case accountLookupFault:
		return nil, conduitAuthzUnavailable, accountRefusalReason(refusal)
	default:
		return nil, conduitAuthzDenied, accountRefusalReason(refusal)
	}
	if devToken {
		return NewDevUser(s.authConfig.DevUserCfg), conduitAuthzAllowed, ""
	}
	base := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeAPI))
	if c.AuthType != AuthTypeUAT {
		return base, conduitAuthzAllowed, ""
	}
	tok, err := s.store.GetUserAccessToken(ctx, c.CredentialID)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && tok == nil):
		return nil, conduitAuthzDenied, "access token not found"
	case err != nil:
		return nil, conduitAuthzUnavailable, "access token lookup failed"
	case tok.Revoked:
		return nil, conduitAuthzDenied, "access token revoked"
	case tok.ExpiresAt == nil || conduitRelayNow().After(*tok.ExpiresAt):
		return nil, conduitAuthzDenied, "access token expired"
	case tok.UserID != u.ID:
		return nil, conduitAuthzDenied, "access token owner does not match"
	}
	boundary := TokenBoundary{Kind: BoundaryKind(tok.BoundaryKind), ProjectID: tok.ProjectID}
	return NewScopedUserIdentityWithBoundary(base, boundary, tok.Scopes, tok.ID, tok.NormalizedCeiling()), conduitAuthzAllowed, ""
}

// writeRelayDenied answers an operation whose re-check denied the session:
// 401, with the close code the relay closes the session with.
func writeRelayDenied(w http.ResponseWriter, v conduitRelayVerdict) {
	w.Header().Set(relay.HeaderCloseCode, itoaUint32(v.closeCode))
	http.Error(w, "session no longer authorized", http.StatusUnauthorized)
}

// hubRelayRPC serves POST sessions/{id}/rpc: an RPC the principal of a
// relay-held user session sent, run by the unchanged in-process handler
// for the identity the token names. It requires an unexpired token.
func (s *Server) hubRelayRPC(w http.ResponseWriter, r *http.Request, peer, sessionID string, body []byte) {
	rt := s.conduitExternal.Load()
	if rt == nil {
		http.Error(w, "not in external relay mode", http.StatusServiceUnavailable)
		return
	}
	var req relay.HubRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid rpc request", http.StatusBadRequest)
		return
	}
	c, ok := s.relayOpClaims(w, r, rt, peer, sessionID, req.AdmissionToken, false)
	if !ok {
		return
	}
	if c.Kind != registry.PrincipalUser {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Only user sessions send RPCs to the hub", nil)
		return
	}
	v := s.recheckRelayAdmission(r.Context(), c)
	switch v.verdict {
	case conduitAuthzAllowed:
	case conduitAuthzUnavailable:
		http.Error(w, "authorization temporarily unavailable", http.StatusServiceUnavailable)
		return
	default:
		writeRelayDenied(w, v)
		return
	}
	rpc := &conduitv1.RpcRequest{}
	if err := proto.Unmarshal(req.RPC, rpc); err != nil {
		http.Error(w, "malformed rpc", http.StatusBadRequest)
		return
	}
	var credExpiry time.Time
	if c.CredentialExpiry > 0 {
		credExpiry = time.Unix(c.CredentialExpiry, 0)
	}
	var (
		resp   *conduitv1.RpcResponse
		tunnel *relay.HubTunnelOpen
	)
	if rpc.GetPath() == conduitTunnelsPath {
		// The relay holding the session opens the tunnel the hub
		// authorizes (conduit_tunnel_relay.go).
		resp, tunnel = s.relayTunnelRPC(r.Context(), rt, v.identity, credExpiry, sessionID, rpc, req)
	} else {
		resp = newConduitRelayRPCHandler(s, v.identity, credExpiry).HandleRPC(r.Context(), rpc)
	}
	resp.RequestId = rpc.GetRequestId()
	out, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "encoding rpc response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(relay.HubRPCResponse{RPC: out, Tunnel: tunnel})
}

// hubRelayRefresh serves POST sessions/{id}/refresh: an agent's in-band
// credential refresh on a relay-held session, checked by the in-process
// refresh checks for the principal the token names. A refusal answers 401
// (the relay closes the session with 4401); a check that cannot be
// evaluated answers 503 (the session stays). An accepted refresh returns
// a token re-issued from the refreshed credential. An expired token is
// accepted: a refresh re-evaluates current status.
func (s *Server) hubRelayRefresh(w http.ResponseWriter, r *http.Request, peer, sessionID string, body []byte) {
	rt := s.conduitExternal.Load()
	if rt == nil {
		http.Error(w, "not in external relay mode", http.StatusServiceUnavailable)
		return
	}
	var req relay.HubRefreshRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid refresh request", http.StatusBadRequest)
		return
	}
	c, ok := s.relayOpClaims(w, r, rt, peer, sessionID, req.AdmissionToken, true)
	if !ok {
		return
	}
	p := relay.Principal{
		Kind:      c.Kind,
		ID:        c.ID,
		ProjectID: c.ProjectID,
		Agent:     relay.AgentIncarnationFacts{LaunchID: c.AgentLaunchID, Generation: c.AgentGeneration},
	}
	sess := relay.RefreshSession{SessionID: sessionID, EndpointIncarnation: c.AgentLaunchID}
	outcome, exp := s.conduitAgentRefreshCheck(r.Context(), p, sess, &conduitv1.AuthRefresh{Credential: req.Credential})
	switch outcome {
	case conduitRefreshOK:
	case conduitRefreshUnevaluated:
		http.Error(w, "refresh could not be checked", http.StatusServiceUnavailable)
		return
	default:
		w.Header().Set(relay.HeaderCloseCode, itoaUint32(conduit.CloseUnauthenticated))
		http.Error(w, "refresh refused", http.StatusUnauthorized)
		return
	}
	// The refreshed credential must not outlive the session's re-check of
	// the agent itself.
	if v := s.recheckRelayAdmission(r.Context(), c); v.verdict != conduitAuthzAllowed {
		if v.verdict == conduitAuthzUnavailable {
			http.Error(w, "refresh could not be checked", http.StatusServiceUnavailable)
			return
		}
		writeRelayDenied(w, v)
		return
	}
	if exp.IsZero() {
		// An agent credential without an expiry: the token keeps none.
		c.CredentialExpiry = 0
	}
	token, err := s.reissueAdmissionToken(c, exp, conduitRelayNow())
	if err != nil {
		slog.Error("Conduit relay refresh: re-issuing the admission token failed", "error", err)
		http.Error(w, "refresh failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(relay.HubRefreshResponse{AdmissionToken: token, CredentialExpiry: exp})
}

// hubRelaySessionRecheck serves POST sessions/recheck: a batch of relay-held
// sessions to re-check. Each item is checked on its own; a bad token closes
// that session (4401), a token for another relay instance or generation
// closes it too (the relay is not the one that admitted it), and an item
// the hub cannot evaluate is left open. A healthy session whose token has
// less than half of its lifetime left gets a re-issued token. Expired
// tokens are accepted: a re-check re-evaluates current status.
func (s *Server) hubRelaySessionRecheck(w http.ResponseWriter, r *http.Request, peer string, body []byte) {
	rt := s.conduitExternal.Load()
	if rt == nil {
		http.Error(w, "not in external relay mode", http.StatusServiceUnavailable)
		return
	}
	var req relay.HubSessionRecheckRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Items) > relay.MaxHubRecheckBatch {
		http.Error(w, "invalid recheck request", http.StatusBadRequest)
		return
	}
	row, ok, err := rt.relayRow(r.Context(), peer)
	if err != nil {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		w.Header().Set(relay.HeaderStaleReason, "relay_target_mismatch")
		http.Error(w, "relay is not registered", http.StatusConflict)
		return
	}
	now := conduitRelayNow()
	resp := relay.HubSessionRecheckResponse{Results: make([]relay.HubSessionRecheckResult, len(req.Items))}
	for i, it := range req.Items {
		res := relay.HubSessionRecheckResult{SessionID: it.SessionID}
		c, err := s.verifyAdmissionToken(it.AdmissionToken, admissionCheck{sessionID: it.SessionID, relay: peer, relayGen: row.Generation, allowExpired: true}, now)
		if err != nil {
			res.Close, res.CloseCode, res.Reason = true, conduit.CloseUnauthenticated, "admission refused"
			resp.Results[i] = res
			continue
		}
		v := s.recheckRelayAdmission(r.Context(), c)
		switch v.verdict {
		case conduitAuthzAllowed:
			if c.Kind == registry.PrincipalBroker && s.relayBrokerSessionSuperseded(r.Context(), rt.registry, now, c) {
				res.Close, res.CloseCode, res.Reason = true, relay.CloseSupersededIncarnation, relay.SupersededIncarnationReason()
				break
			}
			res.OK = true
			// Re-issued only while the credential itself is valid.
			if admissionHalfLifeLeft(c, now) && (c.CredentialExpiry == 0 || now.Unix() < c.CredentialExpiry) {
				if tok, err := s.reissueAdmissionToken(c, time.Time{}, now); err == nil {
					res.AdmissionToken = tok
				}
			}
		case conduitAuthzUnavailable:
			res.Reason = "unavailable"
		default:
			res.Close, res.CloseCode, res.Reason = true, v.closeCode, v.reason
		}
		resp.Results[i] = res
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// itoaUint32 formats a close code for a header.
func itoaUint32(v uint32) string {
	return strconv.FormatUint(uint64(v), 10)
}
