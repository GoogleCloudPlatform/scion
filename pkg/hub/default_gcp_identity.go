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

// Default-tier labels for the agent-creation GCP identity ladder (explicit
// request -> per-profile default -> project default -> hub default -> unset,
// the broker applies its runtime default). They name the tier in log lines and in the user-facing
// error text, so an operator can tell which setting to fix.
//
// A defaultTier is a struct rather than a string so the per-profile rung can
// name the profile its entry belongs to in the user-facing error text.
type defaultTier struct {
	// name labels the tier in log lines ("project", "hub", "project profile").
	name string
	// profile is the broker profile of a per-profile default, "" otherwise.
	profile string
}

var (
	defaultTierProject = defaultTier{name: "project"}
	defaultTierHub     = defaultTier{name: "hub"}
)

// profileDefaultTier is the tier of the per-profile default for profile.
func profileDefaultTier(profile string) defaultTier {
	return defaultTier{name: "project profile", profile: profile}
}

// subject names the setting at fault in an error, e.g. "project default" or
// `project default for profile "remote"`.
func (t defaultTier) subject() string {
	if t.profile != "" {
		return fmt.Sprintf("project default for profile %q", t.profile)
	}
	return t.name + " default"
}

// setting names what the operator should update.
func (t defaultTier) setting() string {
	if t.profile != "" {
		return fmt.Sprintf("the project's per-profile default service account for profile %q", t.profile)
	}
	return "the " + t.name + "'s default GCP identity setting"
}

// projectProfileDefaultSA returns the per-profile default GCP service
// account ID (ProjectSettings.DefaultGCPIdentityServiceAccountIDByProfile)
// for the profile the agent runs under, and that profile's name. Both are
// empty when the project sets no per-profile default or has no entry for
// that profile, and the caller then applies the rest of the ladder
// unchanged.
//
// The profile is resolved with the same helpers the hub-default passthrough
// rung uses, so both rungs agree on which profile an agent dispatches under:
// effectiveRuntimeProfileName (request profile, then the project's active
// profile), then resolveAgentRuntimeProfileType against the broker record
// (falling back to the broker's default profile, or its single profile).
// When the broker record is unavailable or does not list a named profile,
// the named profile itself is used: the agent is dispatched under that name
// either way. The caller pins the returned profile onto the agent (see
// pinResolvedProfile) so the broker cannot dispatch it under another one.
func (s *Server) projectProfileDefaultSA(ctx context.Context, runtimeBrokerID string, project *store.Project, requestProfile string) (profile, saID string) {
	if project == nil {
		return "", ""
	}
	byProfile := projectSettingsFromAnnotations(project).DefaultGCPIdentityServiceAccountIDByProfile
	if len(byProfile) == 0 {
		return "", ""
	}
	profile = effectiveRuntimeProfileName(requestProfile, project)
	if runtimeBrokerID != "" {
		broker, err := s.store.GetRuntimeBroker(ctx, runtimeBrokerID)
		if err == nil && broker != nil {
			if resolved, _, ok := resolveAgentRuntimeProfileType(broker, profile); ok {
				profile = resolved
			}
		} else {
			slog.Debug("per-profile default GCP service account: runtime broker unavailable, using the named profile only",
				"project_id", project.ID, "broker", runtimeBrokerID, "profile", profile, "error", err)
		}
	}
	if profile == "" {
		return "", ""
	}
	saID = byProfile[profile]
	if saID == "" {
		return "", ""
	}
	return profile, saID
}

// pinResolvedProfile pins profile onto the agent's applied config and its
// CreateInputs (what reincarnate replays) when they name none yet, so the
// agent dispatches under the profile a profile-scoped default was chosen
// for. An explicit profile already set is never overwritten.
func pinResolvedProfile(ac *store.AgentAppliedConfig, profile string) {
	if ac == nil || profile == "" {
		return
	}
	if ac.Profile == "" {
		ac.Profile = profile
	}
	if ac.CreateInputs != nil && ac.CreateInputs.Profile == "" {
		ac.CreateInputs.Profile = profile
	}
}

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
func (s *Server) resolveDefaultSAAssignmentCore(ctx context.Context, r *http.Request, projectID, saID, surface string, tier defaultTier) (*store.GCPIdentityConfig, error) {
	sa, err := s.store.GetGCPServiceAccount(ctx, saID)
	// Scope-aware admissibility (P4 item F), same predicate as the two
	// caller-supplied assign sites. A default may legitimately nominate a
	// hub-scoped account; a project-scoped one is only usable in its own
	// project.
	if err != nil || sa == nil || !sa.ReachableFromProject(projectID) {
		slog.Warn(tier.name+"-default SA assignment failed: service account not available",
			"surface", surface,
			"project_id", projectID,
			"sa_id", saID,
			"err", err)
		return nil, fmt.Errorf("%s GCP service account is not available in this project; "+
			"update %s", tier.subject(), tier.setting())
	}
	if !gcpServiceAccountVerified(sa) {
		slog.Warn(tier.name+"-default SA assignment failed: service account not verified",
			"surface", surface,
			"project_id", projectID,
			"sa_id", sa.ID, "sa_email", sa.Email)
		return nil, fmt.Errorf("%s GCP service account is not verified; "+
			"verify it before it can be assigned to agents", tier.subject())
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
		slog.Warn(tier.name+"-default SA assignment denied by authorization gate",
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
func (s *Server) resolveDefaultSAAssignment(ctx context.Context, w http.ResponseWriter, r *http.Request, projectID, saID, surface string, tier defaultTier) (*store.GCPIdentityConfig, bool) {
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
// it, and granting it unconditionally would hand every broker's host
// identity — including remote brokers registered by other users — to every
// agent creator on the hub. The intended use is the single-node VM, whose
// broker is the embedded (co-located) one, so the hub default is confined to
// that broker. Anything else is denied, and the reason is logged so an
// operator can see why the hub default did not take effect; the caller
// leaves AppliedConfig.GCPIdentity unset on denial (ptone/scion#2328) rather
// than writing an explicit "block", so the broker applies its own
// runtime-aware default instead (unchanged "block" on most runtimes,
// "passthrough" on Kubernetes, which does not accept an explicit "block"
// here even if this gate wrote one).
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
// waits, bounded, for registration rather than denying an agent created in
// that window. When the result is still negative, the log line names the
// cause: registration failed, still pending, no embedded broker at all, or a
// different broker.
//
// Being the embedded broker is necessary but no longer sufficient: the
// embedded broker can itself run more than one runtime profile (for example
// a local container runtime alongside a kubernetes-type one), so this also
// calls hubDefaultRuntimeAllowed to resolve which profile this agent's
// dispatch actually uses and confine the default to the runtimes it was
// designed for. See that function's doc comment for the runtime rule.
func (s *Server) hubDefaultPassthroughAllowed(ctx context.Context, runtimeBrokerID, projectID, agentName, profileName string) (bool, string) {
	if runtimeBrokerID == "" {
		slog.Info("hub-default GCP passthrough denied: no runtime broker resolved",
			"surface", SurfaceHubDefault, "project_id", projectID)
		return false, ""
	}
	state := s.waitForEmbeddedBroker(ctx)
	if state.id != "" && state.id == runtimeBrokerID {
		return s.hubDefaultRuntimeAllowed(ctx, runtimeBrokerID, profileName, agentName, projectID)
	}
	switch {
	case state.regErr != "":
		slog.Warn("hub-default GCP passthrough denied: co-located broker registration failed at startup, so the hub has no embedded broker",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID, "registration_error", state.regErr)
	case state.pending:
		slog.Warn("hub-default GCP passthrough denied: co-located broker registration still pending",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID, "waited", embeddedBrokerWaitTimeout)
	case state.id == "":
		slog.Info("hub-default GCP passthrough denied: hub has no embedded broker registered",
			"surface", SurfaceHubDefault, "project_id", projectID,
			"broker", runtimeBrokerID)
	default:
		slog.Info("hub-default GCP passthrough denied: broker is not the hub's embedded broker",
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
// kubernetes-type profile, or a profile that cannot be resolved at all, is
// denied: the caller then leaves the agent's GCP identity unset, the hub
// sends no metadata mode, and the broker applies its runtime default
// ("block" on most runtimes, "passthrough" on Kubernetes).
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
		slog.Info("hub-default GCP passthrough denied: runtime broker unavailable",
			"surface", SurfaceHubDefault, "project_id", projectID, "agent", agentName,
			"broker", runtimeBrokerID, "profile", profileName, "error", err)
		return false, ""
	}
	resolvedProfile, runtimeType, ok := resolveAgentRuntimeProfileType(broker, profileName)
	if !ok {
		slog.Info("hub-default GCP passthrough denied: runtime profile could not be resolved",
			"surface", SurfaceHubDefault, "project_id", projectID, "agent", agentName,
			"broker", runtimeBrokerID, "profile", profileName)
		return false, ""
	}
	if !hubDefaultPassthroughRuntimeTypes[runtimeType] {
		slog.Info("hub-default GCP passthrough denied: runtime is not a local-container runtime",
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
