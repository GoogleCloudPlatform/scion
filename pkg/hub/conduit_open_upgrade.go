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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Targets without stream renewal (design §3.5 "Targets without
// stream_renewal.v2", Q14; contracts §2 "Target lacks stream renewal"). In
// external relay mode with server.hub.conduit.target_stream_deadlines =
// enforce, the hub opens a user stream only to a target session that
// advertises stream_renewal.v2: such a stream ends at its authorization
// deadline unless a renewal moves it. A user stream to any other target
// (no renewal feature, only the earlier stream_renewal, or a legacy broker
// session) is refused before any grant is minted:
//
//   - POST /v1/tunnels (hub-held and relay-held) and the port proxy: 409
//     agent_upgrade_required, reason stream_renewal_unsupported;
//   - PTY: the path is skipped in resolvePTYPath, and with no path left the
//     attach is 503 runtime_attach_unsupported, reason
//     stream_renewal_unsupported, with no WebSocket upgrade. An agent with
//     no pty session (Resolve finds none) gets the temporary 503
//     registry_unavailable instead (design v2.21.46).
//
// The decision is conduitTargetUpgradeRequired, separate from the
// renewal-path decision (conduitRenewalPathUnavailable), which applies only
// to targets that do renew. In-process profiles and off are unaffected.

// errConduitAgentUpgradeRequired refuses a user stream open, before any
// grant is minted, to a target session that does not renew stream
// authorization while target-side deadlines are enforced in external relay
// mode.
var errConduitAgentUpgradeRequired = errors.New("conduit: the agent must be upgraded to open this stream")

// conduitReasonStreamRenewalUnsupported is the error.details.reason of
// errConduitAgentUpgradeRequired, for tunnels, the port proxy and PTY.
const conduitReasonStreamRenewalUnsupported = "stream_renewal_unsupported"

// conduitAgentUpgradeRequiredMessage is the client message of the refusal
// (contracts §2): the agent must be restarted on a newer image. It names no
// hub configuration.
const conduitAgentUpgradeRequiredMessage = "The agent must be restarted on an image whose sciontool supports stream renewal"

// conduitCauseTargetRenewalUnsupported is the cause label of the refusal,
// in logs and metrics only.
const conduitCauseTargetRenewalUnsupported = "target_renewal_unsupported"

// conduitTargetUpgradeRequired reports whether a stream open by identity to
// a target session that renews (targetRenews, conduitTargetRenews) or does
// not is refused: the hub runs in external relay mode, target-side deadlines
// are enforced, the stream is a user stream and the target does not renew.
// Every user stream open path calls it on the resolved row before the mint,
// and mintConduitGrant calls it again as the backstop. A refusal is logged
// with the route and counted by route, reason and cause.
func (s *Server) conduitTargetUpgradeRequired(ctx context.Context, identity Identity, targetRenews bool, route, agentID string) bool {
	if !s.conduitTargetUpgradeRequiredFor(identity, targetRenews) {
		return false
	}
	s.recordConduitUpgradeRefused(ctx, route, agentID)
	return true
}

// recordConduitUpgradeRefused logs and counts one open refused because the
// target does not renew stream authorization.
func (s *Server) recordConduitUpgradeRefused(ctx context.Context, route, agentID string) {
	slog.WarnContext(ctx, "Conduit stream open refused: the target does not renew stream authorization",
		"route", route, "agent_id", agentID, "reason", conduitReasonStreamRenewalUnsupported,
		"cause", conduitCauseTargetRenewalUnsupported)
	if r := s.conduitOpenRefusedMetrics.Load(); r != nil {
		(*r).RecordConduitOpenRefused(route, conduitReasonStreamRenewalUnsupported, conduitCauseTargetRenewalUnsupported)
	}
}

// conduitTargetUpgradeRequiredFor is the condition of
// conduitTargetUpgradeRequired, without logging or counting.
func (s *Server) conduitTargetUpgradeRequiredFor(identity Identity, targetRenews bool) bool {
	if targetRenews || !s.conduitUpgradeGateOn() {
		return false
	}
	return conduitIsUserIdentity(identity)
}

// conduitUpgradeGateOn reports whether user streams to targets without
// stream renewal are refused: external relay mode with target-side deadlines
// enforced.
func (s *Server) conduitUpgradeGateOn() bool {
	return s.ConduitExternal() &&
		s.conduitTargetStreamDeadlines() == config.ConduitTargetStreamDeadlinesEnforce
}

// conduitLegacyRouteRefusedFor reports whether conduitLegacyRouteRefused
// refuses a legacy route for identity's stream. It logs and records
// nothing, so a caller can ask before any route is tried.
func (s *Server) conduitLegacyRouteRefusedFor(identity Identity) bool {
	return s.conduitUpgradeGateOn() && conduitIsUserIdentity(identity)
}

// conduitIsUserIdentity reports whether identity opens user streams.
func conduitIsUserIdentity(identity Identity) bool {
	if isNilIdentity(identity) {
		return false
	}
	_, user := identity.(UserIdentity)
	return user
}

// No path without a target-side deadline (design §3.5 Q14 v2.21.46;
// contracts §2 "No conduit session (6.6b-3d)"): in external relay mode with
// enforce, decided independently of the hub.conduit flag, a user stream is
// never served on a legacy route (the port-forward tunnel or the runtime
// broker's terminal). The port proxy answers with its routing-unavailable
// 503 and the web PTY with its temporary-failure 503, both retryable, and
// no new caller-facing value. The cause is in logs and metrics only. Off
// and in-process profiles keep the legacy fallback.

// Causes of a legacy-route refusal (log and metric labels only).
const (
	// conduitCauseNoConduitSession: the agent has no conduit session
	// (ErrNoSession). Without one the hub cannot tell an agent image
	// without conduit from a session that is briefly reconnecting.
	conduitCauseNoConduitSession = "no_conduit_session"
	// conduitCauseLegacyOnlyPort: the exposed port is served only over the
	// port-forward tunnel (a non-loopback host).
	conduitCauseLegacyOnlyPort = "legacy_only_port"
	// conduitCauseConduitNotRoutable: conduit is not routable on this hub
	// node, or the hub.conduit flag is off at run time.
	conduitCauseConduitNotRoutable = "conduit_not_routable"
)

// Client messages of the legacy-route refusals. They name no hub
// configuration.
const (
	conduitNoSessionMessage   = "The agent has no conduit session right now; if this persists, restart the agent on a current image"
	conduitLegacyOnlyMessage  = "This agent port is not reachable through conduit"
	conduitNotRoutableMessage = "Conduit routing is unavailable"
)

// conduitLegacyRouteRefused reports whether a user stream open by identity
// that only a legacy route could serve is refused instead, and if so logs
// and counts it with route, reasonLabel (the caller-facing value: the
// error code, or the PTY reason) and cause.
func (s *Server) conduitLegacyRouteRefused(ctx context.Context, identity Identity, route, reasonLabel, cause, agentID string) bool {
	if !s.conduitLegacyRouteRefusedFor(identity) {
		return false
	}
	slog.WarnContext(ctx, "Conduit stream open refused: no legacy route is used for user streams",
		"route", route, "agent_id", agentID, "reason", reasonLabel, "cause", cause)
	if r := s.conduitOpenRefusedMetrics.Load(); r != nil {
		(*r).RecordConduitOpenRefused(route, reasonLabel, cause)
	}
	return true
}
