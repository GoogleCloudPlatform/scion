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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Stream re-checks for separate relays (design §3.9 "Active
// authorization"; v2.21.40 "One evaluation, three entry points"). The
// relay holding a user's session schedules re-checks of the streams it
// holds and presents each stream's latest hub-signed grant. The hub
// evaluates it exactly as it evaluates a hub-held stream:
// recheckRelayStream is the relay-held entry point, a decision-free
// adapter over the renewal re-check (renewConduitStream). It builds the
// request from the relay's call, with the admitted principal of the
// session holding the stream as the principal the grant must name, and
// maps the typed result back: a renewal grant, which the relay forwards
// unchanged to the target as AuthRefresh{target stream id}; a close (4401
// on a deny, 4404 when the agent is gone); or neither, when the hub
// cannot evaluate: nothing is renewed and the stream ends at its deadline.

func init() {
	hubInternalOps[relay.HubStreamRecheckPath] = (*Server).hubRelayStreamRecheck
}

// errConduitRenewalUnavailable marks a renewal the hub declines for a
// reason other than an authorization deny: nothing is renewed, nothing
// closes, and the stream keeps its current deadline.
var errConduitRenewalUnavailable = errors.New("renewal unavailable")

// conduitStreamRenewer is the renewal path of relay-held streams: the
// renewal re-check (renewConduitStream). StartConduitExternal installs it;
// without it every relay stream re-check is unavailable, so with
// target_stream_deadlines enforce external relay mode refuses to start
// without it (design v2.21.40 §3.9, Startup).
type conduitStreamRenewer func(ctx context.Context, req conduitRenewalRequest) (*conduitRenewal, error)

// setConduitStreamRenewer installs the renewal path of relay-held streams
// (nil removes it).
func (s *Server) setConduitStreamRenewer(r conduitStreamRenewer) {
	if r == nil {
		s.conduitStreamRenewer.Store(nil)
		return
	}
	s.conduitStreamRenewer.Store(&r)
}

func (s *Server) streamRenewer() conduitStreamRenewer {
	if p := s.conduitStreamRenewer.Load(); p != nil {
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

// recheckRelayStream is the relay-held entry point of the stream
// re-check. The admission token names the principal of the user session
// holding the stream (an expired token is accepted, with its MAC, session
// and relay binding still checked, because the re-check evaluates current
// state; design v2.21.40 §3.9, Lifetime). Everything else is decided by
// renewConduitStream and classified by ClassifyConduitRenewal.
func (s *Server) recheckRelayStream(ctx context.Context, peer string, relayGen int64, it relay.HubStreamRecheckItem) relay.HubStreamRecheckResult {
	refuse := func(code uint32, reason string) relay.HubStreamRecheckResult {
		return relay.HubStreamRecheckResult{Close: true, CloseCode: code, Reason: reason}
	}
	unavailable := relay.HubStreamRecheckResult{Reason: "unavailable"}

	c, err := s.verifyAdmissionToken(it.AdmissionToken, admissionCheck{sessionID: it.UserSessionID, relay: peer, relayGen: relayGen, allowExpired: true}, conduitRelayNow())
	if err != nil || c.Kind != registry.PrincipalUser {
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	renew := s.streamRenewer()
	if renew == nil {
		return unavailable
	}
	r, err := renew(ctx, conduitRenewalRequest{
		LatestGrant:     it.LatestGrant,
		TargetSessionID: it.TargetSessionID,
		TargetStreamID:  it.TargetStreamID,
		OpenJTI:         it.OpenJTI,
		PrincipalID:     c.ID,
		RelayHeld:       true,
	})
	if err == nil {
		return relay.HubStreamRecheckResult{Renew: true, RenewalGrant: r.Token, AuthzUntil: r.AuthzUntil}
	}
	switch ClassifyConduitRenewal(err) {
	case ConduitRenewalDeny:
		return refuse(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	case ConduitRenewalTargetGone:
		return refuse(relay.CloseTargetNotFound, relay.ReasonTargetNotFound)
	default:
		return unavailable
	}
}
