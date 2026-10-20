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
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Tunnels on relay-held user sessions (design §3.9 "Separate relay"): the
// hub authorizes the tunnel request, resolves the target session, mints the
// open grant for exactly that session, and returns an instruction to the
// relay holding the user's session. That relay opens the target stream
// (locally, or through one bridge to the owner relay), opens the user
// stream and splices them. No tunnel byte passes through the hub.
//
// A target the relay finds stale comes back in the same request's retry
// with the session excluded; the hub then resolves again, re-checks and
// mints a new grant for the session it picks.

// openForRelay runs a tunnel request from the relay-held user session
// sessionID and returns the success answer (its stream id left for the
// relay to set) with the instruction to open the tunnel. Refusals are
// returned as errors, as open returns them.
func (ts *conduitTunnelSession) openForRelay(ctx context.Context, rt *conduitExternalRuntime, sessionID string, body []byte, in relay.HubRPCRequest) (*conduitTunnelResponse, *relay.HubTunnelOpen, error) {
	log := slog.Default().With("subsystem", "hub.conduit", "user_id", ts.identity.ID(), "session_id", sessionID, "relay_held", true)
	if !ts.credExpiry.IsZero() && !ts.now().Before(ts.credExpiry) {
		log.Info("Conduit tunnel refused", "cause", "credential_expired")
		return nil, nil, tunnelRefusal(tunnelUnauthorized, "credential refused")
	}
	if err := ts.userActive(ctx); err != nil {
		var te *conduitTunnelError
		if errors.As(err, &te) {
			log.Info("Conduit tunnel refused", "cause", te.cause)
		}
		return nil, nil, err
	}
	req, kind, params, err := parseConduitTunnelRequest(body)
	if err != nil {
		return nil, nil, err
	}
	// The session's tunnel cap, at the point open applies it: the relay
	// holding the session counts its open and opening tunnels.
	if int64(in.OpenTunnels) >= conduitTunnelStreamCap.Load() {
		return nil, nil, tunnelRefusal(tunnelRateLimited, "too many tunnels on this session")
	}
	agent, _, log, err := ts.checkTunnelTarget(ctx, log, req.Agent, kind, params)
	if err != nil {
		return nil, nil, err
	}
	if rt == nil || rt.router == nil {
		return nil, nil, tunnelRefusal(tunnelConduitNotServing, "conduit is not serving on this hub node")
	}

	// Targets an earlier attempt found stale, and every session held by a
	// relay it could not reach, are ruled out (as router.Do does).
	res, err := rt.router.ResolveExcluding(ctx, router.Request{
		Op:    router.OpStream,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID, Capability: kind},
		Agent: agentIncarnationFacts(agent),
	}, stringSet(in.ExcludeTargets), stringSet(in.ExcludeRelays))
	if err != nil {
		return nil, nil, conduitTunnelOpenError(err)
	}
	return ts.mintRelayTunnel(ctx, log, sessionID, agent, kind, params, res)
}

// mintRelayTunnel mints the open grant for res, the target session the
// router resolved, and builds the instruction to open the tunnel and the
// success answer. A resolution that is not a relay-held session (external
// relay mode resolves only to those; a nil session is treated the same way)
// is an internal invariant breaking: it is logged at Warn and answered 500
// internal error (contracts §2), before anything is minted.
func (ts *conduitTunnelSession) mintRelayTunnel(ctx context.Context, log *slog.Logger, sessionID string, agent *store.Agent, kind string, params map[string]string, res router.Resolved) (*conduitTunnelResponse, *relay.HubTunnelOpen, error) {
	rs, ok := relayHeldSession(res)
	if !ok {
		log.Warn("Conduit tunnel: external relay mode resolved a target that is not a relay-held session")
		return nil, nil, conduitTunnelOpenError(fmt.Errorf("tunnel target: %w", errConduitLeafNotRelayHeld))
	}
	// A relay-held stream is renewed through the relay's re-check, which
	// this hub does not serve yet: no renewal can be delivered on this
	// route, so with enforce a stream to a renewal-capable target is
	// refused here rather than left to end at its first deadline.
	renews, canRenew := conduitTargetRenews(res), false
	if ts.s.conduitRenewalPathUnavailable(ctx, ts.identity, renews, canRenew, conduitRouteRelayTunnel, agent.ID) {
		return nil, nil, conduitTunnelOpenError(errConduitStreamRenewalUnavailable)
	}
	// The grant binds the row Resolve picked: its session, connection
	// epoch and incarnation.
	tok, claims, err := ts.s.mintConduitGrant(ctx, conduitGrantRequest{
		Identity: ts.identity,
		Agent:    agent,
		Stream:   grant.StreamHeader{Kind: kind, Params: params},
		Target: grant.Target{
			Kind:                grant.TargetKindAgent,
			ID:                  agent.ID,
			EndpointIncarnation: res.Want.Incarnation,
			SessionID:           res.Record.SessionID,
			ConnectionEpoch:     res.Record.ConnectionEpoch,
		},
		Via:          grant.ViaTunnel,
		TargetRenews: renews,
		CanRenew:     canRenew,
	})
	if err != nil {
		if !conduitTunnelMintErrPassesThrough(err) {
			err = fmt.Errorf("%w: %w", errConduitTunnelGrantKeys, err)
		}
		return nil, nil, conduitTunnelOpenError(err)
	}
	unavailable, err := proto.Marshal(tunnelRefusal(tunnelSessionUnavailable, "agent session unavailable").response())
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the unavailable answer: %w", err)
	}
	refusals, err := relayTunnelTargetRefusals()
	if err != nil {
		return nil, nil, err
	}
	open := &relay.HubTunnelOpen{
		TargetSessionID: res.Record.SessionID,
		OwnerInstanceID: res.Record.RelayInstanceID,
		OwnerGeneration: res.Record.RelayGeneration,
		OwnerEndpoint:   rs.Endpoint(),
		WantProjectID:   res.Want.ProjectID,
		WantExecScope:   res.Want.ExecScope,
		WantIncarnation: res.Want.Incarnation,
		Kind:            kind,
		Params:          params,
		Grant:           tok,
		Unavailable:     unavailable,
		TargetRefusals:  refusals,
	}
	if claims != nil {
		open.OpenJTI = claims.JTI
	}
	log.Info("Conduit tunnel authorized for a relay-held session", "target_session_id", res.Record.SessionID, "owner_relay", res.Record.RelayInstanceID)
	return &conduitTunnelResponse{
		SessionID: sessionID,
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Kind:      kind,
	}, open, nil
}

// conduitRelayTunnelsMessage refuses POST /v1/tunnels forwarded by a relay
// that does not open the tunnels the hub authorizes (one without
// relay.HubFeatureTunnels: an older relay).
const conduitRelayTunnelsMessage = "Tunnels on this session need a newer separate relay"

// relayTunnelRPC answers a POST /v1/tunnels forwarded from the relay-held
// user session sessionID, for identity: a refusal (no instruction), or the
// success answer with the instruction to open the tunnel. A relay that does
// not list relay.HubFeatureTunnels gets 503 unavailable (conduit_not_serving)
// and no instruction, as before relays opened tunnels.
func (s *Server) relayTunnelRPC(ctx context.Context, rt *conduitExternalRuntime, identity UserIdentity, credExpiry time.Time, sessionID string, rpc *conduitv1.RpcRequest, in relay.HubRPCRequest) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
	if !slices.Contains(in.Features, relay.HubFeatureTunnels) {
		return tunnelRefusal(tunnelConduitNotServing, conduitRelayTunnelsMessage).response(), nil
	}
	if rpc.GetMethod() != http.MethodPost {
		return withRPCHeader(conduitRPCError(http.StatusMethodNotAllowed, ErrCodeInvalidRequest, "method not allowed"), "Allow", http.MethodPost), nil
	}
	ts := newConduitTunnelSession(s, identity, credExpiry)
	resp, open, err := ts.openForRelay(ctx, rt, sessionID, rpc.GetBody(), in)
	if err != nil {
		var te *conduitTunnelError
		if errors.As(err, &te) {
			return te.response(), nil
		}
		slog.Warn("Conduit tunnel request failed", "user_id", identity.ID(), "session_id", sessionID, "relay_held", true, "error", err)
		return tunnelRefusal(tunnelInternal, "tunnel request failed").response(), nil
	}
	return conduitRPCJSON(http.StatusOK, resp), open
}

// relayTunnelTargetRefusals are the answers a relay gives when the target
// refuses the tunnel's stream, by close code, from the one mapping a tunnel
// on a hub-held session uses too (conduitTunnelTargetCloses): target not
// found is 404 agent_not_found and forbidden is 403 forbidden.
func relayTunnelTargetRefusals() (map[uint32][]byte, error) {
	out := make(map[uint32][]byte, len(conduitTunnelTargetCloses))
	for code := range conduitTunnelTargetCloses {
		b, err := proto.Marshal(conduitTunnelTargetRefusal(code).response())
		if err != nil {
			return nil, fmt.Errorf("encoding the target refusal for %d: %w", code, err)
		}
		out[code] = b
	}
	return out, nil
}

// stringSet returns the set of ids (nil when there are none).
func stringSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}
