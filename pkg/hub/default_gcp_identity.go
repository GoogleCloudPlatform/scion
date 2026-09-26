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

// resolveDefaultSAAssignmentCore is the transport-independent body shared by
// every default-tier assign rung of the GCP identity ladder: the HTTP
// project-default and hub-default rungs in the interactive/API create path
// (via resolveDefaultSAAssignment below), and both rungs on the scheduled
// dispatch path (applyScheduledProjectDefaultGCPIdentity, server.go), which
// has no http.ResponseWriter to write to. Routing all four call sites through
// one function is what keeps them from drifting apart (#1927).
//
// r is used only to annotate authorization-denial logs with the request
// path and may be nil for the scheduler, which authorizes against the
// identity already on ctx (the schedule's creator) rather than an HTTP
// request; evaluateSAAssignment accepts a nil request.
//
// A default that names an unavailable, unverified, or unauthorized account
// fails resolution rather than silently falling back to block (P10): the
// operator set the default, so the operator needs to hear that it is broken.
// The returned error is either a plain error (not available / not verified)
// or a *saAssignDenial (authorization gate), so a caller with an HTTP
// response can render either the same way resolveDefaultSAAssignment does.
func (s *Server) resolveDefaultSAAssignmentCore(ctx context.Context, r *http.Request, projectID, saID, surface, tier string) (*store.GCPIdentityConfig, error) {
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
		return nil, fmt.Errorf("%s default GCP service account is not available in this project; "+
			"update the %s's default GCP identity setting", tier, tier)
	}
	if !sa.Verified {
		slog.Warn(tier+"-default SA assignment failed: service account not verified",
			"surface", surface,
			"project_id", projectID,
			"sa_id", sa.ID, "sa_email", sa.Email)
		return nil, fmt.Errorf("%s default GCP service account is not verified; "+
			"verify it before it can be assigned to agents", tier)
	}

	// P10: Authorization gate for default SA assignment.
	//
	// Design §4.5 (ruled by ptone): default assignment checks the immediate
	// agent creator. The principal is:
	//   - for a human-created agent: the human creator;
	//   - for agent-creates-agent: the creating agent's assigned SA;
	//   - for a scheduled dispatch: the schedule's immediate creator, resolved
	//     by scheduledCreatorIdentity and placed on ctx before this runs —
	//     the same principal the project-default rung already authorizes
	//     against on this path, so the hub-default rung mirrors it exactly.
	//
	// The operator selected an available default, but did not grant every
	// future creator permission to act as it. The same holds one tier down:
	// a hub-configured default is no more a grant than a project one.
	//
	// evaluateSAAssignment runs:
	//   1. Hub-scoped mode coupling (D4) — denies hub-scoped SAs
	//      when gcpIamCheckMode != enforce.
	//   2. Hub ActionAssign authorization.
	//   3. GCP actAs check via callerPrincipal, using the identity on ctx.
	//   4. Audit record via EvaluateActAs with the given surface.
	if denial := s.evaluateSAAssignment(ctx, r, sa, surface); denial != nil {
		slog.Warn(tier+"-default SA assignment denied by authorization gate",
			"surface", surface,
			"project_id", projectID,
			"sa_id", sa.ID, "sa_email", sa.Email)
		return nil, denial
	}

	return &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
		ProjectID:           sa.ProjectID,
	}, nil
}

// resolveDefaultSAAssignment is the HTTP-transport wrapper around
// resolveDefaultSAAssignmentCore for the interactive/API create path: on
// failure it writes the appropriate HTTP error (the core's plain errors as a
// 400 validation error, a *saAssignDenial through its own write method) and
// returns ok=false.
func (s *Server) resolveDefaultSAAssignment(ctx context.Context, w http.ResponseWriter, r *http.Request, projectID, saID, surface, tier string) (*store.GCPIdentityConfig, bool) {
	cfg, err := s.resolveDefaultSAAssignmentCore(ctx, r, projectID, saID, surface, tier)
	if err == nil {
		return cfg, true
	}
	if denial, ok := err.(*saAssignDenial); ok {
		denial.write(w)
		return nil, false
	}
	writeError(w, http.StatusBadRequest, ErrCodeValidationError, err.Error(), nil)
	return nil, false
}

// hubDefaultPassthroughAllowed reports whether a hub-default "passthrough"
// may be applied to an agent named agentName, dispatched to runtimeBrokerID
// under the given profileName — the effective profile for this dispatch
// (request profile, else the project's active profile; see
// effectiveRuntimeProfileName), which may still be empty when neither the
// request nor the project named one. When allowed, the second return value
// is the specific profile name that was checked; callers should pin
// AppliedConfig.Profile to it before dispatch so the broker cannot resolve a
// different profile than the one this gate evaluated.
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
// The broker check is isEmbeddedBroker, which compares against the embedded
// broker ID the server records at startup (SetEmbeddedBrokerID). It
// deliberately does not trust the scion.io/broker-role label: broker labels
// are writable by the broker's owner through the runtime-broker update API,
// so the label is not an authoritative signal for this gate — the recorded
// embedded broker ID is.
//
// Co-located registration runs after the Hub listener starts, so startup
// marks the embedded broker as expected (ExpectEmbeddedBroker) and this gate
// waits, bounded, for registration rather than permanently writing block
// into an agent created in that window. When the result is still negative,
// the log line names the cause: registration failed, still pending, no
// embedded broker at all, or a different broker.
//
// Being the embedded broker is necessary but no longer sufficient: the
// embedded broker can itself run more than one runtime profile (for example
// a local container runtime alongside a kubernetes-type one), so this also
// calls hubDefaultRuntimeAllowed to resolve which profile this agent's
// dispatch actually uses and confine the default to the runtimes it was
// designed for. See that function's doc comment for the runtime rule.
func (s *Server) hubDefaultPassthroughAllowed(ctx context.Context, runtimeBrokerID, projectID, agentName, profileName string) (bool, string) {
	if runtimeBrokerID == "" {
		slog.Info("hub-default GCP passthrough not applied: no runtime broker resolved; using block",
			"surface", SurfaceHubDefault, "project_id", projectID)
		return false, ""
	}
	state := s.waitForEmbeddedBroker(ctx)
	if state.id != "" && state.id == runtimeBrokerID {
		return s.hubDefaultRuntimeAllowed(ctx, runtimeBrokerID, profileName, agentName, projectID)
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
	return false, ""
}

// hubDefaultPassthroughRuntimeTypes are the runtime profile types the
// hub-default passthrough rung may apply to: local container runtimes that
// run on the broker's own host and therefore share its metadata server.
//
// Kept separate from nodeBoundProfileTypes (harness_config_handlers.go),
// which drives image-status aggregation for an unrelated purpose. Widening
// that map for image reasons must not silently widen this security gate, so
// this gate keeps its own list. docker and podman are runtimes that exec a
// local daemon on the broker host with no network boundary between the
// container and the host's metadata server. "container" (Apple Container,
// macOS-only) is deliberately excluded: it has no GCE metadata server to
// reach in the first place, so there is nothing to gain by allowlisting it,
// and it fails closed here like any other unlisted type.
var hubDefaultPassthroughRuntimeTypes = map[string]bool{
	"docker": true,
	"podman": true,
}

// hubDefaultRuntimeAllowed reports whether the runtime profile an agent's
// dispatch resolves to on runtimeBrokerID is one the hub-default passthrough
// rung may apply to: a local container runtime that shares its host's
// metadata server (hubDefaultPassthroughRuntimeTypes, above). A
// kubernetes-type profile, or a profile that cannot be resolved at all,
// fails closed to block.
//
// Unlike brokerHasCloudRunSandboxProfile (handlers_agents_core.go), which
// treats a broker as qualifying when any one of its profiles matches, this
// resolves the single profile this agent's dispatch will actually use
// (resolveAgentRuntimeProfileType), because one broker can run more than one
// runtime profile and only one of them applies to a given agent. On a grant
// it returns that profile's name so the caller can pin AppliedConfig.Profile
// to it.
func (s *Server) hubDefaultRuntimeAllowed(ctx context.Context, runtimeBrokerID, profileName, agentName, projectID string) (bool, string) {
	broker, err := s.store.GetRuntimeBroker(ctx, runtimeBrokerID)
	if err != nil || broker == nil {
		slog.Info("hub-default GCP passthrough downgraded to block: runtime broker unavailable",
			"surface", SurfaceHubDefault, "project_id", projectID, "agent", agentName,
			"broker", runtimeBrokerID, "profile", profileName)
		return false, ""
	}
	resolvedProfile, runtimeType, ok := resolveAgentRuntimeProfileType(broker, profileName)
	if !ok {
		slog.Info("hub-default GCP passthrough downgraded to block: runtime profile could not be resolved",
			"surface", SurfaceHubDefault, "project_id", projectID, "agent", agentName,
			"broker", runtimeBrokerID, "profile", profileName)
		return false, ""
	}
	if !hubDefaultPassthroughRuntimeTypes[runtimeType] {
		slog.Info("hub-default GCP passthrough downgraded to block: runtime is not a local-container runtime",
			"surface", SurfaceHubDefault, "project_id", projectID, "agent", agentName,
			"broker", runtimeBrokerID, "profile", resolvedProfile, "runtime_type", runtimeType)
		return false, ""
	}
	return true, resolvedProfile
}

// resolveAgentRuntimeProfileType resolves the profile name and BrokerProfile
// type the hub believes an agent's dispatch will run under: the profile
// named profileName when the caller supplied one (the effective profile —
// request, else project active profile), else the broker's own default
// profile (DefaultProfile, recorded at registration from the broker's local
// active_profile setting — see registerGlobalProjectAndBroker). A named
// profile the broker does not have, a default profile the broker does not
// have or has not reported, or a broker with no profiles at all are all
// unresolvable, and the caller must fail closed rather than guess.
//
// This is registration-time data, so it can disagree with what the broker
// itself resolves for the same profile name at dispatch time against its
// own, current project-effective settings (an in-repo override, a DB
// settings overlay, or just registration drift) — this function approximates
// the broker's resolution, it does not guarantee it. hubDefaultRuntimeAllowed
// marks its grant accordingly (RequireLocalRuntime) so the broker can
// re-verify the resolved runtime itself before honoring passthrough.
func resolveAgentRuntimeProfileType(broker *store.RuntimeBroker, profileName string) (resolvedProfile, runtimeType string, ok bool) {
	if profileName == "" {
		profileName = broker.DefaultProfile
	}
	if profileName != "" {
		for _, p := range broker.Profiles {
			if p.Name == profileName {
				return p.Name, p.Type, true
			}
		}
		return "", "", false
	}
	// Neither the caller nor the broker's own registration named a profile
	// (an old broker record predating DefaultProfile, most likely). A single
	// configured profile is unambiguous even without a name for it; more than
	// one is a guess, and the caller must fail closed rather than make it.
	if len(broker.Profiles) == 1 {
		return broker.Profiles[0].Name, broker.Profiles[0].Type, true
	}
	return "", "", false
}

// effectiveRuntimeProfileName resolves the profile name the hub-default
// passthrough gate should evaluate, at the same precedence
// applyProjectDefaults (project_settings_handlers.go) later applies to
// AppliedConfig.Profile: the request's explicit profile first, else the
// project's active-profile setting. It deliberately does not fall further to
// the broker's own default profile — that step happens once inside
// resolveAgentRuntimeProfileType, which needs the broker record.
//
// Both the create path (handlers_agents_core.go) and the scheduled dispatch
// path (server.go) call this before hubDefaultPassthroughAllowed, which runs
// before deriveAgentConfig/applyProjectDefaults would otherwise stamp
// AppliedConfig.Profile from the project setting. Without this, the gate
// would evaluate a different, less specific profile (or none) than the one
// the agent actually dispatches under.
func effectiveRuntimeProfileName(requestProfile string, project *store.Project) string {
	if requestProfile != "" {
		return requestProfile
	}
	if project == nil {
		return ""
	}
	return projectSettingsFromAnnotations(project).ActiveProfile
}
