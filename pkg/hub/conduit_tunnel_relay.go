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
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
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
func (ts *conduitTunnelSession) openForRelay(ctx context.Context, rt *conduitExternalRuntime, sessionID string, body []byte, exclude []string) (*conduitTunnelResponse, *relay.HubTunnelOpen, error) {
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
	agent, _, log, err := ts.checkTunnelTarget(ctx, log, req.Agent, kind, params)
	if err != nil {
		return nil, nil, err
	}
	if rt == nil || rt.router == nil {
		return nil, nil, tunnelRefusal(tunnelConduitNotServing, "conduit is not serving on this hub node")
	}

	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	res, err := rt.router.Resolve(ctx, router.Request{
		Op:    router.OpStream,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID, Capability: kind},
		Agent: agentIncarnationFacts(agent),
	}, skip)
	if err != nil {
		return nil, nil, conduitTunnelOpenError(err)
	}
	rs, ok := res.Session.(*relay.RemoteSession)
	if !ok {
		// External relay mode resolves only to relay-held sessions.
		return nil, nil, fmt.Errorf("tunnel target: %w", errConduitLeafNotRelayHeld)
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
		ViaTunnel: true,
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

// relayTunnelRPC answers a POST /v1/tunnels forwarded from the relay-held
// user session sessionID, for identity: a refusal (no instruction), or the
// success answer with the instruction to open the tunnel.
func (s *Server) relayTunnelRPC(ctx context.Context, rt *conduitExternalRuntime, identity UserIdentity, credExpiry time.Time, sessionID string, rpc *conduitv1.RpcRequest, exclude []string) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
	if rpc.GetMethod() != http.MethodPost {
		return withRPCHeader(conduitRPCError(http.StatusMethodNotAllowed, ErrCodeInvalidRequest, "method not allowed"), "Allow", http.MethodPost), nil
	}
	ts := newConduitTunnelSession(s, identity, credExpiry)
	resp, open, err := ts.openForRelay(ctx, rt, sessionID, rpc.GetBody(), exclude)
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
