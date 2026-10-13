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
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Stream re-checks for separate relays (design §3.9 "Active
// authorization", §3.5 "Re-check (hub side)"). The relay holding a user's
// session schedules re-checks of the streams it holds and presents each
// stream's latest hub-signed grant. The hub verifies that grant in the
// re-check mode, takes the principal, target, kind and params only from
// its signed claims, re-evaluates authorization with the in-process rules
// (authorizeConduitUserStream) and either mints a renewal grant or refuses.
// The relay forwards a renewal unchanged to the target as
// AuthRefresh{target stream id}; on a refusal it closes the stream (4401).
// When the hub cannot evaluate, nothing is renewed and the stream ends at
// its deadline.
//
// Verifying grants in the re-check mode and minting renewal grants belong
// to the target-side deadlines work (conduitRenewalGrants); until a
// conduitRenewalGrants is set, every re-check is unavailable.

func init() {
	hubInternalOps[relay.HubStreamRecheckPath] = (*Server).hubRelayStreamRecheck
}

// conduitRenewalBinding is what the relay says a latest grant is for: the
// stream on the target's session and the open grant's jti. Verification
// checks the grant's signed claims against it; it never replaces them.
type conduitRenewalBinding struct {
	TargetSessionID string
	TargetStreamID  uint32
	OpenJTI         string
}

// conduitRecheckClaims are the claims of a verified latest grant that a
// re-check decides on.
type conduitRecheckClaims struct {
	// UserID is the grant's principal (sub).
	UserID string
	// AgentID and ProjectID name the target agent.
	AgentID   string
	ProjectID string
	// Kind and Params are the stream's kind and params.
	Kind   string
	Params map[string]string
	// ViaTunnel marks a stream opened through the tunnel path.
	ViaTunnel bool
}

// conduitRenewalMint is the input to a renewal grant.
type conduitRenewalMint struct {
	UserID          string
	UserSessionID   string
	TargetSessionID string
	TargetStreamID  uint32
	OpenJTI         string
	Kind            string
	Params          map[string]string
	// LatestGrant is the grant the re-check verified.
	LatestGrant []byte
}

// Renewal failures. errConduitRenewalRefused (wrapped with the reason) is a
// refusal: the stream closes. errConduitRenewalUnavailable is a failure to
// evaluate: nothing is renewed, nothing closes.
var (
	errConduitRenewalRefused     = errors.New("renewal refused")
	errConduitRenewalUnavailable = errors.New("renewal unavailable")
)

// conduitRenewalGrants verifies latest grants in the re-check mode and
// mints renewal grants (design §3.5). VerifyForRecheck checks the grant's
// signature, issuer and audience, its binding to b, and the re-check
// mode's expiry rule, and returns its claims; a grant that fails is
// errConduitRenewalRefused. Mint returns the compact renewal JWS and its
// authz_until.
type conduitRenewalGrants interface {
	VerifyForRecheck(ctx context.Context, latestGrant []byte, b conduitRenewalBinding) (conduitRecheckClaims, error)
	Mint(ctx context.Context, in conduitRenewalMint) (renewalJWS []byte, authzUntil time.Time, err error)
}

// setConduitRenewalGrants installs the renewal grant source.
func (s *Server) setConduitRenewalGrants(g conduitRenewalGrants) {
	if g == nil {
		s.conduitRenewal.Store(nil)
		return
	}
	s.conduitRenewal.Store(&g)
}

func (s *Server) renewalGrants() conduitRenewalGrants {
	if p := s.conduitRenewal.Load(); p != nil {
		return *p
	}
	return nil
}

// hubRelayStreamRecheck serves POST streams/recheck.
func (s *Server) hubRelayStreamRecheck(w http.ResponseWriter, r *http.Request, peer string, body []byte) {
	rt := s.conduitExternal.Load()
	if rt == nil {
		http.Error(w, "not in external relay mode", http.StatusServiceUnavailable)
		return
	}
	var req relay.HubStreamRecheckRequest
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
	resp := relay.HubStreamRecheckResponse{Results: make([]relay.HubStreamRecheckResult, len(req.Items))}
	for i, it := range req.Items {
		resp.Results[i] = s.recheckRelayStream(r.Context(), peer, row.Generation, it)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// recheckRelayStream re-checks one relay-held user stream.
func (s *Server) recheckRelayStream(ctx context.Context, peer string, relayGen int64, it relay.HubStreamRecheckItem) relay.HubStreamRecheckResult {
	refuse := func(code uint32, reason string) relay.HubStreamRecheckResult {
		return relay.HubStreamRecheckResult{Close: true, CloseCode: code, Reason: reason}
	}
	unavailable := relay.HubStreamRecheckResult{Reason: "unavailable"}

	// The user session the stream's leaf is on: its admission names the
	// principal the grant must name too.
	c, err := s.verifyAdmissionToken(it.AdmissionToken, admissionCheck{sessionID: it.UserSessionID, relay: peer, relayGen: relayGen, allowExpired: true}, conduitRelayNow())
	if err != nil || c.Kind != "user" {
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	g := s.renewalGrants()
	if g == nil {
		return unavailable
	}
	claims, err := g.VerifyForRecheck(ctx, it.LatestGrant, conduitRenewalBinding{TargetSessionID: it.TargetSessionID, TargetStreamID: it.TargetStreamID, OpenJTI: it.OpenJTI})
	switch {
	case errors.Is(err, errConduitRenewalRefused):
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	case err != nil:
		return unavailable
	case claims.UserID != c.ID:
		// The grant was issued to another principal than the session's.
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	identity, verdict, _ := s.relayUserIdentity(ctx, c)
	switch verdict {
	case conduitAuthzAllowed:
	case conduitAuthzUnavailable:
		return unavailable
	default:
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	agent, err := s.store.GetAgent(ctx, claims.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse(relay.CloseTargetNotFound, "target_not_found")
	case err != nil:
		return unavailable
	case !agent.DeletedAt.IsZero():
		return refuse(relay.CloseTargetNotFound, "target_not_found")
	}
	port := 0
	if claims.Kind == grant.StreamKindTCP {
		port, _ = strconv.Atoi(claims.Params[grant.ParamPort])
	}
	switch verdict, _, _ := s.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{
		Identity: identity, Agent: agent, Kind: claims.Kind, ViaTunnel: claims.ViaTunnel, Port: port,
	}); verdict {
	case conduitAuthzAllowed:
	case conduitAuthzUnavailable:
		return unavailable
	default:
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	jws, until, err := g.Mint(ctx, conduitRenewalMint{
		UserID: c.ID, UserSessionID: it.UserSessionID,
		TargetSessionID: it.TargetSessionID, TargetStreamID: it.TargetStreamID, OpenJTI: it.OpenJTI,
		Kind: claims.Kind, Params: claims.Params, LatestGrant: it.LatestGrant,
	})
	switch {
	case errors.Is(err, errConduitRenewalRefused):
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	case err != nil:
		return unavailable
	}
	return relay.HubStreamRecheckResult{Renew: true, RenewalGrant: jws, AuthzUntil: until}
}
