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
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Default-tier labels for the agent-creation GCP identity ladder
// (explicit request -> project default -> hub default -> block). They name
// the tier in log lines and in the user-facing error text, so an operator
// can tell which setting to fix.
const (
	defaultTierProject = "project"
	defaultTierHub     = "hub"
)

// resolveDefaultSAAssignment resolves a configured default service account
// (project or hub tier) into the GCPIdentityConfig applied to a new agent.
// Both the project-default and hub-default rungs of the ladder go through
// here so the two cannot drift apart.
//
// On failure it writes the HTTP error and returns ok=false. A default that
// names an unavailable, unverified, or unauthorized account fails agent
// creation rather than silently falling back to block (P10): the operator set
// the default, so the operator needs to hear that it is broken.
func (s *Server) resolveDefaultSAAssignment(ctx context.Context, w http.ResponseWriter, r *http.Request, projectID, saID, surface, tier string) (*store.GCPIdentityConfig, bool) {
	sa, err := s.store.GetGCPServiceAccount(ctx, saID)
	// Scope-aware admissibility (P4 item F), same predicate as the two
	// caller-supplied assign sites. A default may legitimately nominate a
	// hub-scoped account; a project-scoped one is only usable in its own
	// project.
	if err != nil || sa == nil || !sa.ReachableFromProject(projectID) {
		slog.Warn(tier+"-default SA assignment failed: service account not available",
			"surface", surface,
			"project_id", projectID,
			"sa_id", saID,
			"err", err)
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("%s default GCP service account is not available in this project; "+
				"update the %s's default GCP identity setting", tier, tier), nil)
		return nil, false
	}
	if !sa.Verified {
		slog.Warn(tier+"-default SA assignment failed: service account not verified",
			"surface", surface,
			"project_id", projectID,
			"sa_id", sa.ID, "sa_email", sa.Email)
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			fmt.Sprintf("%s default GCP service account is not verified; "+
				"verify it before it can be assigned to agents", tier), nil)
		return nil, false
	}

	// P10: Authorization gate for default SA assignment.
	//
	// Design §4.5 (ruled by ptone): default assignment checks the immediate
	// agent creator. The principal is:
	//   - for a human-created agent: the human creator;
	//   - for agent-creates-agent: the creating agent's assigned SA.
	//
	// The operator selected an available default, but did not grant every
	// future creator permission to act as it. The same holds one tier down:
	// a hub-configured default is no more a grant than a project one.
	//
	// authorizeSAAssignment runs:
	//   1. Hub-scoped mode coupling (D4) — denies hub-scoped SAs
	//      when gcpIamCheckMode != enforce.
	//   2. Hub ActionAssign authorization.
	//   3. GCP actAs check via callerPrincipal.
	//   4. Audit record via EvaluateActAs with the given surface.
	if !s.authorizeSAAssignment(w, r, sa, surface) {
		slog.Warn(tier+"-default SA assignment denied by authorization gate",
			"surface", surface,
			"project_id", projectID,
			"sa_id", sa.ID, "sa_email", sa.Email)
		return nil, false
	}

	return &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
		ProjectID:           sa.ProjectID,
	}, true
}

// hubDefaultPassthroughAllowed reports whether a hub-default "passthrough"
// may be applied to an agent dispatched to runtimeBrokerID.
//
// Explicit passthrough requests go through authorizePassthroughIdentity
// (broker owner or admin, plus actAs on the broker host SA). A hub-wide
// default cannot run that gate meaningfully on behalf of the admin who set
// it, and applying it unconditionally would expose the host identity of
// every broker on the hub — including remote brokers registered by other
// users — to every agent creator. The intended use is the single-node VM,
// whose broker is the embedded (co-located) one, so the hub default is
// confined to that broker. Anything else falls back to block, and the reason
// is logged so an operator can see why the hub default did not take effect.
//
// The check is isEmbeddedBroker, which compares against the embedded broker
// ID the server records at startup (SetEmbeddedBrokerID). It deliberately
// does not trust the scion.io/broker-role label: broker labels are writable
// by the broker's owner through the runtime-broker update API, so any user
// who registers a broker could claim "embedded" and pull the hub default's
// passthrough onto their own host.
//
// Co-located registration runs after the Hub listener starts, so startup
// marks the embedded broker as expected (ExpectEmbeddedBroker) and this gate
// waits, bounded, for registration rather than permanently writing block
// into an agent created in that window. When the result is still negative,
// the log line names the cause: registration failed, still pending, no
// embedded broker at all, or a different broker.
func (s *Server) hubDefaultPassthroughAllowed(ctx context.Context, runtimeBrokerID, projectID string) bool {
	if runtimeBrokerID == "" {
		slog.Info("hub-default GCP passthrough not applied: no runtime broker resolved; using block",
			"surface", SurfaceHubDefault, "project_id", projectID)
		return false
	}
	state := s.waitForEmbeddedBroker(ctx)
	if state.id != "" && state.id == runtimeBrokerID {
		return true
	}
	switch {
	case state.regErr != "":
		slog.Warn("hub-default GCP passthrough not applied: co-located broker registration failed at startup, so the hub has no embedded broker; using block",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID, "registration_error", state.regErr)
	case state.pending:
		slog.Warn("hub-default GCP passthrough not applied: co-located broker registration still pending; using block",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID, "waited", embeddedBrokerWaitTimeout)
	case state.id == "":
		slog.Info("hub-default GCP passthrough not applied: hub has no embedded broker registered; using block",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID)
	default:
		slog.Info("hub-default GCP passthrough not applied: broker is not the hub's embedded broker; using block",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID, "embedded_broker", state.id)
	}
	return false
}
