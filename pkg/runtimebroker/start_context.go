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

package runtimebroker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gopkg.in/yaml.v3"
)

// startContext holds all the resolved state needed to start an agent.
// It is built by buildStartContext from the various handler-specific inputs,
// unifying project path resolution, env merging, template hydration, and
// manager selection into a single code path.
type startContext struct {
	Opts         api.StartOptions
	TemplateSlug string
	Manager      agent.Manager

	// RuntimeType is the runtime type this dispatch resolved to through
	// resolveManagerForOpts (dispatchRuntimeType), which can differ from the
	// broker's default runtime. The create path reports it to the hub.
	RuntimeType string

	// EnvClassifications is the merged provenance map: what the hub sent,
	// plus the broker-written keys classified in buildStartContext. Nil means
	// the hub sent none — see api.EnvKind's three-state contract. No consumer
	// yet (GoogleCloudPlatform/scion#127 P3b); this exists so the broker's
	// own classifications survive the function that computes them.
	EnvClassifications map[string]api.EnvKind

	// AssignSelection is the profile and runtime entry a GCP identity
	// "assign" dispatch on Kubernetes read its ServiceAccount mapping and
	// namespace from, or nil when no ServiceAccount was resolved. The start
	// and restart handlers compare it with their own later resolution.
	AssignSelection *dispatchProfileSelection
}

// startContextInputs captures the handler-specific fields that vary across
// createAgent, startAgent, and restartAgent. Each handler populates this from
// its own request structure, then calls buildStartContext. A finalize-env
// dispatch reaches the broker as a full create request (see
// DispatchFinalizeEnv on the Hub side), so it takes the createAgent path.
type startContextInputs struct {
	// Agent identity
	Name    string
	AgentID string // Hub UUID (for env injection and logging)
	Slug    string

	// Project
	ProjectPath string
	ProjectSlug string
	ProjectID   string
	// ProjectPathFromContainer is set when ProjectPath was recovered from
	// the agent's listed container rather than sent by the caller. That
	// value is the resolved .scion directory the container recorded, not a
	// project root, so it is used for settings and saved-profile resolution
	// only: the hub-managed marker block and host-side worktree provisioning,
	// which treat ProjectPath as a project root, skip it.
	ProjectPathFromContainer bool
	// HubGlobalProject is set when the hub marked this dispatch as its
	// global project by sending the global slug alongside ProjectPath (see
	// splitHubGlobalSlug). ProjectSlug is then left empty, so the path
	// alone resolves the project, as for any dispatch with a path.
	HubGlobalProject bool

	// Config from CreateAgentConfig (nil for startAgent/restartAgent)
	Config *CreateAgentConfig

	// InlineConfig for provisioning
	InlineConfig *api.ScionConfig

	// SharedDirs from project
	SharedDirs []api.SharedDir

	// Hub auth
	HubEndpoint string
	AgentToken  string
	CreatorName string

	// Env
	ResolvedEnv        map[string]string
	EnvClassifications map[string]api.EnvKind
	ResolvedSecrets    []api.ResolvedSecret

	// Behavior
	NoAuth bool
	Attach bool

	// WorkspaceMode is the resolved workspace sharing mode for the project
	// (e.g. "worktree-per-agent"). Threaded from CreateAgentRequest so the
	// broker can branch dispatch without re-deriving from labels.
	WorkspaceMode string

	// WorkspaceStoragePath is the create request's GCS bootstrap path. The
	// workspace is downloaded after buildStartContext (createAgent /
	// runLaunch), so it is threaded here only so the workspace-source
	// checks see it as the explicit source it is.
	WorkspaceStoragePath string
	// RunID is the hub-minted per-run identity for this dispatch
	// (ptone/scion#2550), passed to StartOptions.RunID. Empty from an older
	// hub; pkg/agent then mints one itself.
	RunID string
	// TemplateName is the agent's template slug as sent on start and
	// restart. It is used for naming only (StartOptions.TemplateName) and
	// never to locate or load a template; a create's Config.Template slug
	// takes precedence. A content hash is ignored.
	TemplateName string

	// HTTP request (for hub connection resolution)
	HTTPRequest *http.Request

	// Operation identifies which dispatch path is calling buildStartContext,
	// so the hub endpoint resolver is chosen explicitly by the caller rather
	// than inferred from request shape (see resolveEffectiveHubEndpoint).
	// Required: buildStartContext rejects the zero value.
	Operation startOperation
}

// buildStartContext unifies the common startup logic shared by createAgent,
// startAgent, and restartAgent:
//   - Hub-managed project path resolution (ProjectSlug → ~/.scion.projects/<slug>/)
//   - Merged env assembly (resolved env + config env + auth + hub endpoint + broker identity)
//   - Template hydration
//   - Git-clone env injection
//   - Telemetry override translation
//   - Resolved secrets passthrough
//   - Manager resolution
//
// The caller may further customize the returned startContext before calling
// mgr.Start or mgr.Provision.
func (s *Server) buildStartContext(ctx context.Context, in startContextInputs) (*startContext, error) {
	// The caller must name its operation explicitly: resolveEffectiveHubEndpoint's
	// ranking depends on it, and there is no safe default to infer from
	// request shape. Checked first, before any directory or file side effect.
	if !in.Operation.valid() {
		return nil, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("buildStartContext: Operation not set or unrecognized: %q", in.Operation),
		}
	}

	ctx, span := tracer.Start(ctx, "broker.agent.provision")
	defer span.End()
	span.SetAttributes(attribute.String("scion.agent.name", in.Name))

	// --- Hub-managed project path resolution ---
	if in.ProjectSlug != "" && in.ProjectPath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, &startContextError{Status: http.StatusInternalServerError, Message: "Failed to resolve the global config directory", OriginalErr: err}
		}
		in.ProjectPath = filepath.Join(globalDir, "projects", in.ProjectSlug)
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Resolved hub-managed project path from slug",
				"agent_id", in.AgentID, "slug", in.ProjectSlug, "path", in.ProjectPath)
		}
	}

	// The broker's global directory belongs to the global project. A
	// dispatch that names another project with that path must fail before
	// the marker block below, which would otherwise rewrite the global
	// marker and create project-configs entries for that project.
	if in.ProjectPath != "" && !in.ProjectPathFromContainer {
		if msg := globalDirProjectConflict(in.ProjectPath, in.ProjectID, in.HubGlobalProject); msg != "" {
			span.SetStatus(codes.Error, msg)
			return nil, &startContextError{Status: http.StatusConflict, Message: msg}
		}
	}

	// Ensure hub-managed projects have a .scion marker with project-id for
	// external split storage. When the hub dispatches to a broker without a
	// LocalPath (e.g. auto-provided embedded broker for a linked project), the
	// broker creates the workspace at ~/.scion.projects/<slug>/. Without a
	// project-id, agents are provisioned inside that workspace directory.
	// Writing the hub's project ID enables split storage so agent homes go to
	// ~/.scion.project-configs/<slug>__<uuid>/.scion/agents/ instead.
	//
	// The .scion path may be a marker file (hub-managed/workspace marker) or
	// a directory (git project). This block handles both forms.
	//
	// This block also handles the case where the createAgent handler already
	// resolved ProjectPath (for env-gather) before calling buildStartContext,
	// which would skip the resolution block above.
	//
	// A path recovered from the agent's container is already a .scion
	// directory; it is not a project root to initialize, so it skips this
	// block (see ProjectPathFromContainer).
	if in.ProjectPath != "" && !in.ProjectPathFromContainer && (in.ProjectSlug != "" || in.ProjectID != "") {
		scionPath := filepath.Join(in.ProjectPath, config.DotScion)

		if config.IsProjectMarkerFile(scionPath) {
			// .scion is a marker file — project-id is already recorded.
			if marker, err := config.ReadProjectMarker(scionPath); err == nil && marker.ProjectID != "" {
				// Detect stale marker: hub's project ID differs and the old
				// external config dir was cleaned up (project was deleted and
				// recreated with the same name — miller79/scion#28).
				// The global marker is only rewritten for the global project
				// itself (see globalDirProjectConflict above).
				if in.ProjectID != "" && marker.ProjectID != in.ProjectID && canRewriteProjectMarker(in.ProjectPath, in.ProjectID, in.HubGlobalProject) {
					extPath, _ := marker.ExternalProjectPath()
					if isStaleExternalDir(extPath) {
						slug := marker.ProjectSlug
						if in.ProjectSlug != "" {
							slug = in.ProjectSlug
						}
						updated := &config.ProjectMarker{
							ProjectID:   in.ProjectID,
							ProjectName: slug,
							ProjectSlug: slug,
						}
						if wErr := config.WriteProjectMarker(scionPath, updated); wErr != nil {
							s.agentLifecycleLog.Warn("Failed to update stale .scion marker",
								"agent_id", in.AgentID, "old_id", marker.ProjectID, "new_id", in.ProjectID, "error", wErr)
						} else {
							s.agentLifecycleLog.Info("Updated stale .scion marker with current project ID",
								"agent_id", in.AgentID, "old_id", marker.ProjectID, "new_id", in.ProjectID, "path", scionPath)
							marker = updated
						}
					}
				}
				// Ensure external split storage directories exist.
				if extPath, err := marker.ExternalProjectPath(); err == nil && extPath != "" {
					_ = os.MkdirAll(extPath, 0755)
					_ = os.MkdirAll(filepath.Join(extPath, "agents"), 0755)
				}
				if s.config.Debug {
					s.agentLifecycleLog.Debug("Hub-managed project has marker with split storage",
						"agent_id", in.AgentID, "slug", in.ProjectSlug, "project_id", marker.ProjectID, "path", scionPath)
				}
			}
		} else if info, statErr := os.Stat(scionPath); statErr == nil && info.IsDir() {
			// .scion is a directory (git project) — use file-based project-id
			if in.ProjectID != "" {
				existingID, readErr := config.ReadProjectID(scionPath)
				shouldWrite := readErr != nil || existingID == ""
				if !shouldWrite && existingID != in.ProjectID {
					// Existing ID differs from hub's — overwrite only if the old
					// external config dir was cleaned up (project deleted and
					// recreated — miller79/scion#28). When the dir still exists
					// this is a first-link scenario; preserve the local ID.
					extDir, extErr := config.GetGitProjectExternalConfigDir(scionPath)
					shouldWrite = extErr != nil || extDir == "" || isStaleExternalDir(extDir)
				}
				if shouldWrite {
					oldID := existingID
					if wErr := config.WriteProjectID(scionPath, in.ProjectID); wErr != nil {
						s.agentLifecycleLog.Warn("Failed to write project-id for hub-managed project",
							"agent_id", in.AgentID, "project_id", in.ProjectID, "error", wErr)
					} else {
						if oldID != "" && oldID != in.ProjectID {
							s.agentLifecycleLog.Info("Updated stale project-id with current project ID",
								"agent_id", in.AgentID, "old_id", oldID, "new_id", in.ProjectID, "path", scionPath)
						}
						if extAgents, err := config.GetGitProjectExternalAgentsDir(scionPath); err == nil && extAgents != "" {
							_ = os.MkdirAll(extAgents, 0755)
						}
						if extConfig, err := config.GetGitProjectExternalConfigDir(scionPath); err == nil && extConfig != "" {
							_ = os.MkdirAll(extConfig, 0755)
						}
						if s.config.Debug {
							s.agentLifecycleLog.Debug("Initialized git project with split storage",
								"agent_id", in.AgentID, "slug", in.ProjectSlug, "project_id", in.ProjectID, "path", scionPath)
						}
					}
				}
			}
		} else if in.ProjectID != "" {
			// .scion doesn't exist — create project dir and write a marker file
			if err := os.MkdirAll(in.ProjectPath, 0755); err != nil {
				s.agentLifecycleLog.Warn("Failed to create project dir for hub-managed project",
					"agent_id", in.AgentID, "slug", in.ProjectSlug, "path", in.ProjectPath, "error", err)
			} else {
				marker := &config.ProjectMarker{
					ProjectID:   in.ProjectID,
					ProjectName: in.ProjectSlug,
					ProjectSlug: in.ProjectSlug,
				}
				if wErr := config.WriteProjectMarker(scionPath, marker); wErr != nil {
					s.agentLifecycleLog.Warn("Failed to write .scion marker for hub-managed project",
						"agent_id", in.AgentID, "project_id", in.ProjectID, "error", wErr)
				} else {
					if extPath, err := marker.ExternalProjectPath(); err == nil && extPath != "" {
						_ = os.MkdirAll(extPath, 0755)
						_ = os.MkdirAll(filepath.Join(extPath, "agents"), 0755)
					}
					if s.config.Debug {
						s.agentLifecycleLog.Debug("Initialized hub-managed project with split storage",
							"agent_id", in.AgentID, "slug", in.ProjectSlug, "project_id", in.ProjectID, "path", scionPath)
					}
				}
			}
		}
	}

	// --- GCP identity mode: resolve and reject "block" on Kubernetes before
	// any pod or env is built. ---
	//
	// This runs after the project-directory/marker resolution above (needed
	// so settings and the saved profile below are read from the final,
	// post-update location — a fresh hub-managed project or a stale-marker
	// rewrite would otherwise resolve against the pre-update location) and
	// before any env or pod work, which starts immediately below. Its inputs
	// are in.Config.GCPIdentity, in.ResolvedEnv, in.Config.Env, and the
	// resolved runtime, read directly here rather than from the merged `env`
	// map, which does not exist yet.
	//
	// The runtime that matters is the one THIS dispatch actually uses, not
	// the broker's default: a broker can register more than one profile
	// (e.g. both a "docker" and a "kubernetes" profile), and a dispatch's
	// profile selects which one it runs on. mgr and dispatchRuntimeType are
	// resolved exactly once here, via resolveManagerForOpts (handlers.go) —
	// the same function that ultimately selects the manager this function
	// returns — and reused below instead of re-resolving, so within this one
	// buildStartContext call the GCP check and the manager it returns cannot
	// disagree, and settings are loaded only once per call. (start/restart
	// perform their own, later, second resolution after this function
	// returns — see the Manager resolution comment near the end of this
	// function for why that one can still differ from this one.)
	//
	// The profile fed into that resolution must match opts.Profile below
	// exactly (opts.Profile = in.Config.Profile, unconditionally) on create:
	// a create/re-provision does not fall back to the agent's saved profile
	// anywhere else in this function (ProvisionAgent, image resolution, the
	// saved profile written back by provision.go all key off opts.Profile),
	// so resolving the manager here from a different, saved-profile-derived
	// value would let a create run on one runtime while everything else
	// about it — including the GCP identity classification above — is
	// computed for another. Only start/restart fall back to the agent's own
	// saved profile (agent.GetSavedProfile): their Config is nil (no profile
	// to read from a request body that doesn't exist), and handlers.go's
	// startAgent/restartAgent already resolve the saved profile themselves,
	// re-running this same check after their own, later resolution.
	gcpIdentityProfile := ""
	if in.Config != nil {
		gcpIdentityProfile = in.Config.Profile
	}
	if gcpIdentityProfile == "" && in.Operation != opCreate {
		gcpIdentityProfile = agent.GetSavedProfile(in.Name, in.ProjectPath)
	}
	mgr, dispatchRuntimeType := s.resolveManagerForOpts(api.StartOptions{
		Name:        in.Name,
		ProjectPath: in.ProjectPath,
		Profile:     gcpIdentityProfile,
	})
	isKubernetes := isKubernetesRuntimeName(dispatchRuntimeType)

	// Default when no GCP identity config is provided at all: "block" on
	// every runtime except Kubernetes, so agents cannot access the underlying
	// compute identity via the GCE metadata server unless the hub explicitly
	// sets "passthrough" or "assign". Kubernetes does not support "block"
	// (ptone/scion#2328 phase 1), so an unconfigured Kubernetes dispatch
	// defaults to "passthrough" instead — the Hub's own resolution ladder
	// leaves the mode unset (rather than writing an explicit "block") for
	// exactly this case, precisely so the runtime-appropriate default can be
	// applied here. Shared with the auth preflight's own GCP-credential
	// check (handlers.go's extractRequiredEnvKeys), via effectiveGCPMetadataMode,
	// so the two agree on whether GCP credentials will be available.
	gcpMetadataMode := effectiveGCPMetadataMode(isKubernetes, in.Config, in.ResolvedEnv)
	// "block" is not offered on the Kubernetes runtime. The rejection applies
	// to any "block" that actually resolved — an explicit agent-level
	// request, a project or hub default that is itself explicitly "block"
	// and arrives via resolvedEnv, or an existing agent's own stored
	// GCPIdentity from before this ruling (an earlier hub or web UI version
	// could write an explicit "block" for what the caller intended as
	// "nothing configured"; that stored value is not migrated — see
	// ptone/scion#2328). It does not apply to the runtime-aware default
	// above: a dispatch that names no GCP identity at all already defaults
	// to "passthrough" on Kubernetes, so "block" reaching this check on
	// Kubernetes always means an explicit choice somewhere in the chain. The
	// message below names both fixes — edit this agent's own mode, or change
	// the project/hub default for agents created after this — since the
	// caller cannot tell from here which one applies to them.
	if sce := rejectKubernetesBlock(dispatchRuntimeType, gcpMetadataMode); sce != nil {
		return nil, sce
	}

	// GCP identity mode "assign" on the Kubernetes runtime uses Workload
	// Identity instead of the sciontool metadata emulator: the pod runs as a
	// Kubernetes ServiceAccount (KSA) the operator has already bound to the
	// requested GSA via Workload Identity, out of band. Scion never creates,
	// annotates, or binds KSAs itself (see docs-site/.../ha/setup-gcp.md).
	// Resolved here, alongside the block rejection above and before any env
	// or pod work, for the same reason: it can reject the dispatch outright.
	// It rewrites gcpMetadataMode to "passthrough" so the env-emission switch
	// below sets SCION_METADATA_MODE with no new switch arm;
	// assignIdentity.SAEmail/ProjectID carry the SA email and
	// project ID forward to that switch, which sets them as informational
	// env, since the passthrough case does not set them on its own.
	assignIdentity, sce := s.resolveKubernetesAssignIdentity(in, isKubernetes, gcpMetadataMode, gcpIdentityProfile)
	if sce != nil {
		return nil, sce
	}
	resolvedKSAName := assignIdentity.KSAName
	if resolvedKSAName != "" {
		gcpMetadataMode = store.GCPMetadataModePassthrough
	}

	// --- Build merged environment ---
	env := make(map[string]string)

	// Inherit hub-side classifications (nil-safe: old hubs send nothing).
	// Broker-set keys are classified below. The three-state semantics of
	// the nil map are preserved: if in.EnvClassifications is nil (old hub),
	// envCls stays nil and the caller can distinguish "unavailable" from
	// "all classified" (#127, P3a).
	var envCls map[string]api.EnvKind
	if in.EnvClassifications != nil {
		envCls = make(map[string]api.EnvKind, len(in.EnvClassifications))
		for k, v := range in.EnvClassifications {
			envCls[k] = v
		}
	}

	// classifyBrokerEnv sets a classification for a broker-written key.
	// If the hub didn't send classifications (nil map), the broker must
	// also leave them nil — creating a non-nil map here would collapse
	// state 3 (unavailable) into state 2 (all classified), producing the
	// version-skew outage the three-state design prevents.
	classifyBrokerEnv := func(key string, kind api.EnvKind) {
		if envCls == nil {
			return
		}
		envCls[key] = kind
	}

	// 1. Resolved env from Hub
	for k, v := range in.ResolvedEnv {
		env[k] = v
	}

	// 2. Config.Env (takes precedence)
	if in.Config != nil {
		for _, e := range in.Config.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				env[parts[0]] = parts[1]
				classifyBrokerEnv(parts[0], api.EnvKindPlain)
			}
		}
	}

	// 3. Hub auth token. Precedence (highest first):
	//   1. in.AgentToken — the explicit hub-provided dedicated field (create path).
	//   2. an existing env["SCION_AUTH_TOKEN"] already populated from in.ResolvedEnv
	//      above — on the start/resume path the hub mints the agent JWT into
	//      resolvedEnv, so it is already present here and must be kept.
	//   3. the broker's own dev SCION_AUTH_TOKEN — last resort only.
	// The dev-token fallback must NOT clobber a token resolved from the hub:
	// resume mints a valid JWT into resolvedEnv, and overwriting it with the
	// broker's dev token caused 401s ("compact JWS format must have three parts").
	if in.AgentToken != "" {
		env["SCION_AUTH_TOKEN"] = in.AgentToken
		// Bootstrap: NOT in argv. Diverted to ~/.scion/scion-token by
		// pkg/agent/run.go:761-777; read by pkg/hubsync/sync.go:1329.
		classifyBrokerEnv("SCION_AUTH_TOKEN", api.EnvKindSecretBootstrap)
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_AUTH_TOKEN set from agent token", "agent_id", in.AgentID, "length", len(in.AgentToken))
		}
	} else if env["SCION_AUTH_TOKEN"] != "" {
		// Token already resolved from the hub via resolvedEnv (start/resume path); keep it.
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_AUTH_TOKEN kept from resolved env", "agent_id", in.AgentID, "length", len(env["SCION_AUTH_TOKEN"]))
		}
	} else if devToken := os.Getenv("SCION_AUTH_TOKEN"); devToken != "" {
		env["SCION_AUTH_TOKEN"] = devToken
		// Bootstrap: NOT in argv (same diversion as AgentToken path above).
		classifyBrokerEnv("SCION_AUTH_TOKEN", api.EnvKindSecretBootstrap)
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_AUTH_TOKEN set from broker env", "agent_id", in.AgentID, "length", len(devToken))
		}
	}

	// 4. Hub endpoint
	// The hub endpoint (its container-bridge rewrite and the cloudrun-family
	// overrides) and the colocated extra hosts all depend on where this
	// agent runs, so they key off the runtime this dispatch resolved to
	// (dispatchRuntimeType, above), not the broker's default runtime: a
	// docker-default broker can dispatch an agent to a kubernetes profile,
	// and that pod must not receive a docker-bridge hub URL or docker
	// --add-host entries.
	runtimeName := dispatchRuntimeType

	// Resolve hub connection early — needed for colocated detection and
	// template hydration below. The connection-endpoint header applies to
	// any HTTP request tunneled through a remote control channel, not just
	// create, so it is resolved once here and fed into every HTTP operation.
	var hubConn *HubConnection
	var connectionHubEndpoint string
	if in.HTTPRequest != nil {
		hubConn = s.resolveHubConnection(in.HTTPRequest)
		connectionHubEndpoint = s.resolveHubEndpointFromRequest(in.HTTPRequest)
	}

	hubEndpoint, hubEndpointTrusted, err := resolveEffectiveHubEndpoint(ctx, hubEndpointInputs{
		Op:                    in.Operation,
		ReqHubEndpoint:        in.HubEndpoint,
		ConnectionHubEndpoint: connectionHubEndpoint,
		BrokerHubEndpoint:     s.config.HubEndpoint,
		ResolvedEnv:           in.ResolvedEnv,
		ProjectPath:           in.ProjectPath,
		ContainerHubEndpoint:  s.config.ContainerHubEndpoint,
		RuntimeName:           runtimeName,
		HubListenPort:         s.config.HubListenPort,
	})
	if err != nil {
		return nil, &startContextError{
			Status:      http.StatusInternalServerError,
			Message:     "Failed to resolve the hub endpoint",
			OriginalErr: err,
		}
	}
	if hubEndpoint != "" {
		env["SCION_HUB_ENDPOINT"] = hubEndpoint
		classifyBrokerEnv("SCION_HUB_ENDPOINT", api.EnvKindPlain)
		env["SCION_HUB_URL"] = hubEndpoint // legacy compat
		classifyBrokerEnv("SCION_HUB_URL", api.EnvKindPlain)
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_HUB_ENDPOINT set", "agent_id", in.AgentID, "endpoint", hubEndpoint)
		}
	}
	// trustedHubEndpoint feeds api.StartOptions.TrustedHubEndpoint below —
	// the one hub value Substrate's egress allowlist may trust
	// (pkg/agent/run.go, pkg/runtime/substrate_egress.go). It is the SAME
	// value delivered into the agent's own SCION_HUB_ENDPOINT env above when
	// hubEndpointTrusted is true, and empty (fail closed) when it is false —
	// never a value read back out of env, which by this point may already
	// carry a creator-controlled ResolvedEnv/Config.Env value.
	var trustedHubEndpoint string
	if hubEndpointTrusted {
		trustedHubEndpoint = hubEndpoint
	}

	// Colocated bridge override: when the hub and broker are on the same
	// machine, Docker bridge containers cannot reach the hub's public domain
	// via hairpin NAT (e.g. on GCE). Map the domain to host-gateway so the
	// container routes through the Docker bridge.
	isColocated := hubConn != nil && hubConn.IsColocated
	extraHosts := colocatedExtraHosts(hubEndpoint, isColocated, runtimeName)

	// 5. Agent identity env
	if in.Slug != "" {
		env["SCION_AGENT_SLUG"] = in.Slug
		classifyBrokerEnv("SCION_AGENT_SLUG", api.EnvKindPlain)
	}
	if in.AgentID != "" {
		env["SCION_AGENT_ID"] = in.AgentID
		classifyBrokerEnv("SCION_AGENT_ID", api.EnvKindPlain)
	}
	// SCION_LAUNCH_ID is broker-owned: Manager.Start sets it from the run
	// ID it labels the container with, so a resolved-env value is never
	// passed through.
	delete(env, "SCION_LAUNCH_ID")
	delete(envCls, "SCION_LAUNCH_ID")
	if in.ProjectID != "" {
		env["SCION_PROJECT_ID"] = in.ProjectID
		classifyBrokerEnv("SCION_PROJECT_ID", api.EnvKindPlain)
	}
	if in.ProjectPath != "" {
		env["SCION_PROJECT_PATH"] = in.ProjectPath
		classifyBrokerEnv("SCION_PROJECT_PATH", api.EnvKindPlain)
	}

	// Emit canonical workspace sharing mode.
	// On the create path, in.WorkspaceMode carries the wire label and we
	// resolve it to the canonical value here. On start/restart paths,
	// in.WorkspaceMode is empty but the hub pre-resolves the canonical value
	// into resolvedEnv["SCION_WORKSPACE_MODE"] (see httpdispatcher.go); it was
	// already merged into env in step 1 above, so the var is already present.
	// Either way, we ensure a guaranteed non-empty value with a safe default.
	if in.WorkspaceMode != "" {
		env["SCION_WORKSPACE_MODE"] = string(store.ResolveWorkspaceSharingMode(in.WorkspaceMode))
		classifyBrokerEnv("SCION_WORKSPACE_MODE", api.EnvKindPlain)
	}
	if env["SCION_WORKSPACE_MODE"] == "" {
		// Empty or unrecognized — default to shared-plain for backward compatibility.
		env["SCION_WORKSPACE_MODE"] = string(store.SharingModeSharedPlain)
		classifyBrokerEnv("SCION_WORKSPACE_MODE", api.EnvKindPlain)
	}

	// 6. Broker identity
	if s.config.BrokerName != "" {
		env["SCION_BROKER_NAME"] = s.config.BrokerName
		classifyBrokerEnv("SCION_BROKER_NAME", api.EnvKindPlain)
	}
	if s.config.BrokerID != "" {
		env["SCION_BROKER_ID"] = s.config.BrokerID
		classifyBrokerEnv("SCION_BROKER_ID", api.EnvKindPlain)
	}
	if in.CreatorName != "" {
		env["SCION_CREATOR"] = in.CreatorName
		classifyBrokerEnv("SCION_CREATOR", api.EnvKindPlain)
	}

	// 7. Debug
	if s.config.Debug {
		env["SCION_DEBUG"] = "1"
		classifyBrokerEnv("SCION_DEBUG", api.EnvKindPlain)
	}

	// 8. GCP identity metadata server configuration: write env vars for the
	// mode resolved and validated earlier in this function, after the
	// project-marker/directory resolution block and before any env or pod
	// work — see the comment there for why the Kubernetes/"block" check
	// runs where it does.
	//
	// gcpMetadataMode itself was already resolved earlier in this function
	// (effectiveGCPMetadataMode) and validated against the Kubernetes/"block"
	// check above. requireLocalRuntime follows the identical struct-or-env
	// precedence — see downgradeUnverifiedHubDefaultPassthrough's doc comment
	// for what it means and how it's used below and in
	// recheckHubDefaultPassthrough.
	requireLocalRuntime := false
	if in.Config != nil && in.Config.GCPIdentity != nil {
		requireLocalRuntime = in.Config.GCPIdentity.RequireLocalRuntime
	} else if v := env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"]; v != "" {
		requireLocalRuntime = v == "true"
	}
	// Allow-list, not a deny-list. The previous form tested for the two modes
	// that need the redirect and let everything else fall through untouched,
	// which meant an unrecognised mode — a typo, a value from a newer hub, an
	// empty MetadataMode on a non-nil GCPIdentity — silently left
	// GCE_METADATA_HOST unset and the container talking to the real GCE
	// metadata server. That is the fail-open direction on the exact control
	// this block exists to enforce, so unknown modes now fail the start.
	switch gcpMetadataMode {
	case store.GCPMetadataModeAssign, store.GCPMetadataModeBlock:
		env["SCION_METADATA_MODE"] = gcpMetadataMode
		classifyBrokerEnv("SCION_METADATA_MODE", api.EnvKindPlain)
		env["SCION_METADATA_PORT"] = "18380"
		classifyBrokerEnv("SCION_METADATA_PORT", api.EnvKindPlain)
		if gcpMetadataMode == store.GCPMetadataModeAssign && in.Config != nil && in.Config.GCPIdentity != nil {
			env["SCION_METADATA_SA_EMAIL"] = in.Config.GCPIdentity.SAEmail
			classifyBrokerEnv("SCION_METADATA_SA_EMAIL", api.EnvKindPlain)
			env["SCION_METADATA_PROJECT_ID"] = in.Config.GCPIdentity.ProjectID
			classifyBrokerEnv("SCION_METADATA_PROJECT_ID", api.EnvKindPlain)
		}
		// The metadata emulator runs inside the sandbox (started by
		// sciontool init), so localhost is correct — the emulator and the
		// harness share the same network namespace.
		env["GCE_METADATA_HOST"] = "localhost:18380"
		classifyBrokerEnv("GCE_METADATA_HOST", api.EnvKindPlain)
		// gcloud CLI uses GCE_METADATA_ROOT (not GCE_METADATA_HOST) to locate
		// the metadata server during its initial configuration detection.
		// Both must point at the same address.
		env["GCE_METADATA_ROOT"] = "localhost:18380"
		classifyBrokerEnv("GCE_METADATA_ROOT", api.EnvKindPlain)
	case store.GCPMetadataModePassthrough:
		// Deliberately no redirect: passthrough means the agent is meant to
		// reach the real GCE metadata server. Listed explicitly so it is a
		// recognised mode rather than an unhandled one — the distinction is
		// the whole point of the default arm below.
		//
		// Still record the mode itself (without the redirect). It is the only
		// channel through which "a GCP SA is reachable via passthrough"
		// crosses into pkg/agent's Start(), which has no structured
		// GCPIdentity of its own — auth-type auto-detection (e.g. selecting
		// vertex-ai for antigravity) depends on this env var actually being
		// present on the create path, not just on start/restart where the
		// hub separately injects it via resolvedEnv. See ptone/scion#1873.
		env["SCION_METADATA_MODE"] = gcpMetadataMode
		classifyBrokerEnv("SCION_METADATA_MODE", api.EnvKindPlain)
		// resolvedKSAName is only non-empty when a Kubernetes "assign"
		// dispatch rewrote gcpMetadataMode to "passthrough" above (see that
		// block's comment). The hub's start/restart path already injects
		// SCION_METADATA_SA_EMAIL/PROJECT_ID into resolvedEnv unconditionally
		// for assign, and at least one harness (harnesses/grok-build)
		// consumes SCION_METADATA_PROJECT_ID directly for Vertex AI
		// autodetection and project fallback, independent of the metadata
		// emulator. Setting them here too, from the same values that block
		// already resolved, makes create produce the same env as
		// start/restart instead of differing by dispatch path. A genuine
		// (non-rewritten) passthrough dispatch never reaches this branch, so
		// it is unaffected.
		if resolvedKSAName != "" {
			env["SCION_METADATA_SA_EMAIL"] = assignIdentity.SAEmail
			classifyBrokerEnv("SCION_METADATA_SA_EMAIL", api.EnvKindPlain)
			env["SCION_METADATA_PROJECT_ID"] = assignIdentity.ProjectID
			classifyBrokerEnv("SCION_METADATA_PROJECT_ID", api.EnvKindPlain)
		}
	default:
		return nil, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf("Invalid GCP metadata mode %q: must be one of %q, %q, %q",
				gcpMetadataMode,
				store.GCPMetadataModeAssign, store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough),
		}
	}
	// Re-stamp into env (absent when false) for recheckHubDefaultPassthrough
	// to read back later on the start/restart paths — see its doc comment.
	if requireLocalRuntime {
		env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"] = "true"
		classifyBrokerEnv("SCION_METADATA_REQUIRE_LOCAL_RUNTIME", api.EnvKindPlain)
	}

	// Debug log final env
	if s.config.Debug {
		s.agentLifecycleLog.Debug("Final environment count", "agent_id", in.AgentID, "count", len(env))
		for k, v := range env {
			s.agentLifecycleLog.Debug("  ENV", "agent_id", in.AgentID, "key", k, "value", redactEnvValueForLog(k, v))
		}
	}

	// --- Build StartOptions ---
	opts := api.StartOptions{
		Name:               in.Name,
		BrokerMode:         true,
		ProjectPath:        in.ProjectPath,
		NoAuth:             in.NoAuth,
		TrustedHubEndpoint: trustedHubEndpoint,
		// FreshProvision is true only for a create dispatch: GetAgent wipes
		// and re-clones an existing populated workspace only in that case,
		// never on start or restart (GoogleCloudPlatform/scion#1931).
		FreshProvision: in.Operation == opCreate,
		RunID:          in.RunID,
	}

	if in.Attach {
		opts.Detached = boolPtr(false)
	} else {
		opts.Detached = boolPtr(true)
	}

	if in.Config != nil {
		opts.Template = in.Config.Template
		opts.Image = in.Config.Image
		opts.HarnessConfig = in.Config.HarnessConfig
		opts.HarnessAuth = in.Config.HarnessAuth
		opts.Task = in.Config.Task
		opts.Workspace = in.Config.Workspace
		opts.Profile = in.Config.Profile
		opts.Branch = in.Config.Branch
		opts.SharedWorkspace = in.Config.SharedWorkspace
		opts.ProjectPreStartHookScript = in.Config.ProjectPreStartHookScript
	}

	if in.InlineConfig != nil {
		opts.InlineConfig = in.InlineConfig
	}

	// ResolvedKubernetesServiceAccountName (not InlineConfig) is how the
	// mapped KSA reaches the pod: for an existing agent, GetAgent
	// (pkg/agent/provision.go) builds its config from the template chain
	// plus the persisted scion-agent.json and never consults InlineConfig,
	// so a value injected only into opts.InlineConfig would silently not
	// apply on start/restart of an agent that already has a persisted
	// config. This field is read directly in pkg/agent/run.go's Kubernetes
	// RunConfig builder, after GetAgent, so it applies on create and on
	// start/restart alike. It is never persisted (not part of ScionConfig),
	// so it is recomputed fresh on every dispatch, the same way
	// SCION_METADATA_MODE itself is — a later mapping change takes effect on
	// the next start without touching scion-agent.json.
	opts.ResolvedKubernetesServiceAccountName = resolvedKSAName

	if len(in.SharedDirs) > 0 {
		opts.SharedDirs = in.SharedDirs
	}

	if len(extraHosts) > 0 {
		opts.ExtraHosts = extraHosts
	}

	// Save template slug before hydration may replace opts.Template
	templateSlug := ""
	if in.Config != nil {
		templateSlug = in.Config.Template
	}

	// --- Template hydration ---
	if hubConn != nil && in.Config != nil {
		templatePath, err := s.hydrateTemplate(ctx, in.Config, hubConn)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, &startContextError{
				Status:      http.StatusInternalServerError,
				Message:     "Failed to hydrate template: " + err.Error(),
				IsHubError:  true,
				OriginalErr: err,
			}
		}
		if templatePath != "" {
			opts.Template = templatePath
			if s.config.Debug {
				s.agentLifecycleLog.Debug("Using hydrated template", "agent_id", in.AgentID, "path", templatePath)
			}
		}
	}

	// --- Harness-config hydration ---
	// Resolve a Hub-managed harness-config to a local directory so provisioning
	// can use it even on a broker that lacks the config on its local filesystem.
	if hubConn != nil && in.Config != nil {
		hcPath, err := s.hydrateHarnessConfig(ctx, in.Config, hubConn)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, &startContextError{
				Status:      http.StatusInternalServerError,
				Message:     "Failed to hydrate harness-config: " + err.Error(),
				IsHubError:  true,
				OriginalErr: err,
			}
		}
		if hcPath != "" {
			opts.HarnessConfigPath = hcPath
			if s.config.Debug {
				s.agentLifecycleLog.Debug("Using hydrated harness-config", "agent_id", in.AgentID, "path", hcPath)
			}
		}
	}

	if templateSlug == "" && !transfer.IsContentHash(in.TemplateName) {
		templateSlug = in.TemplateName
	}
	if templateSlug != "" {
		opts.TemplateName = templateSlug
	}

	// --- Shared workspace mode (git-workspace hybrid) ---
	// Deprecated: SCION_SHARED_WORKSPACE — superseded by SCION_WORKSPACE_MODE +
	// SCION_WORKSPACE_GIT. Kept for compatibility during the transition period.
	// Removal is tracked in https://github.com/ptone/scion/issues/575.
	if in.Config != nil && in.Config.SharedWorkspace {
		env["SCION_SHARED_WORKSPACE"] = "true"
		classifyBrokerEnv("SCION_SHARED_WORKSPACE", api.EnvKindPlain)
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Shared workspace mode enabled", "agent_id", in.AgentID)
		}
	}

	// --- Worktree-per-agent mode ---
	// When the hub sets WorkspaceMode to worktree-per-agent and the project
	// is git-backed, provision a shared base clone + per-agent worktree on
	// the host BEFORE the container starts, then dual-mount it. This avoids
	// the full in-container clone. Falls through to clone-per-agent on error
	// or if git is too old (< 2.47) — but only when this call has not yet
	// created the agent's own worktree; see tryProvisionWorktree.
	worktreeProvisioned := false
	if in.Config != nil && in.Config.GitClone != nil && in.WorkspaceMode == store.WorkspaceModeWorktreePerAgent {
		var err error
		worktreeProvisioned, err = s.tryProvisionWorktree(ctx, in, &opts, env, dispatchRuntimeType)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, &startContextError{Status: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	// --- Git clone mode ---
	if !worktreeProvisioned && in.Config != nil && in.Config.GitClone != nil {
		gc := in.Config.GitClone
		env["SCION_GIT_CLONE_URL"] = gc.URL
		// Clone URL may embed credentials — the original #127 bug.
		classifyBrokerEnv("SCION_GIT_CLONE_URL", api.EnvKindSecretInjected)
		if gc.Branch != "" {
			env["SCION_GIT_BRANCH"] = gc.Branch
			classifyBrokerEnv("SCION_GIT_BRANCH", api.EnvKindPlain)
		}
		if gc.Depth != nil {
			env["SCION_GIT_DEPTH"] = strconv.Itoa(*gc.Depth)
			classifyBrokerEnv("SCION_GIT_DEPTH", api.EnvKindPlain)
		}
		if in.Config.Branch != "" {
			env["SCION_AGENT_BRANCH"] = in.Config.Branch
			classifyBrokerEnv("SCION_AGENT_BRANCH", api.EnvKindPlain)
		}
		opts.Workspace = ""
		// Keep opts.ProjectPath so that ProvisionAgent can resolve the correct
		// agent directory (e.g. ~/.scion.projects/<slug>/) instead of falling
		// back to the global project. The git-clone check in ProvisionAgent
		// runs before the worktree logic, so no worktree will be created.
		opts.GitClone = gc
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Git clone mode enabled", "agent_id", in.AgentID,
				"cloneURL", redactCloneURL(gc.URL), "branch", gc.Branch, "depth", gc.Depth)
		}
	}

	// --- Empty-per-agent mode (design #2703) ---
	// The hub sends the canonical "empty-per-agent" on create and start (or
	// pre-resolves it into resolvedEnv, already merged into env above). The
	// agent gets a private, initially empty, non-git directory at
	// <projectDir>/agents/<slug>/workspace; any other workspace source in the
	// same request is contradictory and refused rather than guessed at.
	emptyPerAgent := store.SharingModeEmptyPerAgent == store.WorkspaceSharingMode(env["SCION_WORKSPACE_MODE"])
	if emptyPerAgent {
		if msg := emptyPerAgentConflict(in); msg != "" {
			span.SetStatus(codes.Error, msg)
			return nil, &startContextError{Status: http.StatusBadRequest, Message: msg}
		}
		opts.EmptyPerAgentWorkspace = true
	} else if msg := ambiguousNonGitWorkspace(in, worktreeProvisioned); msg != "" {
		span.SetStatus(codes.Error, msg)
		return nil, &startContextError{Status: http.StatusBadRequest, Message: msg}
	}

	// --- SCION_WORKSPACE_GIT ---
	// Emit when the workspace is (or will be) a git repository. Mode alone is
	// insufficient because shared-plain may or may not be git-backed.
	//
	// Priority:
	//  1. worktreeProvisioned: host-side worktree was set up — always git.
	//  2. opts.GitClone != nil: in-container clone configured — always git.
	//  3. opts.Workspace on disk: shared-plain git workspace (check .git).
	//  4. in.ResolvedEnv fallback: hub-injected on start/restart paths before
	//     the on-disk workspace exists (e.g. clone-per-agent pre-clone).
	isGitWorkspace := worktreeProvisioned || opts.GitClone != nil
	if !isGitWorkspace && opts.Workspace != "" {
		isGitWorkspace = util.IsGitRepoDir(opts.Workspace)
	}
	if !isGitWorkspace {
		isGitWorkspace = in.ResolvedEnv["SCION_WORKSPACE_GIT"] == "true"
	}
	if emptyPerAgent {
		// Never git, whatever a stale resolvedEnv claims.
		isGitWorkspace = false
		delete(env, "SCION_WORKSPACE_GIT")
		delete(envCls, "SCION_WORKSPACE_GIT")
	}
	if isGitWorkspace {
		env["SCION_WORKSPACE_GIT"] = "true"
		classifyBrokerEnv("SCION_WORKSPACE_GIT", api.EnvKindPlain)
	}
	// Absent when false — avoids encoding a "false" string agents must parse.

	// --- Env + telemetry + secrets ---
	opts.Env = env

	if v, ok := env["SCION_TELEMETRY_ENABLED"]; ok {
		enabled := v == "true" || v == "1"
		opts.TelemetryOverride = &enabled
	}

	if in.NoAuth {
		opts.ResolvedSecrets = nil
	} else if len(in.ResolvedSecrets) > 0 {
		// Defense in depth for rows that predate the hub's create/patch
		// reserved-target check and dispatch-time drop, or that reached this
		// call through some other path: never attach an environment-type
		// secret whose target is reserved for scion's own control-plane env
		// vars to the runtime config.
		opts.ResolvedSecrets = make([]api.ResolvedSecret, 0, len(in.ResolvedSecrets))
		for _, rs := range in.ResolvedSecrets {
			if (rs.Type == "environment" || rs.Type == "") && secret.IsReservedEnvTarget(rs.Target) {
				if s.config.Debug {
					s.envSecretLog.Debug("Dropping reserved-target resolved secret", "name", rs.Name, "target", rs.Target)
				}
				continue
			}
			opts.ResolvedSecrets = append(opts.ResolvedSecrets, rs)
		}
		if s.config.Debug {
			s.envSecretLog.Debug("Received resolved secrets", "count", len(in.ResolvedSecrets))
		}
	}

	// In co-located (workstation) hub mode, auto-inject the host ADC file as a
	// gcloud-adc file secret so agents can authenticate with GCP without requiring
	// the user to manually upload the credential file.
	// Only inject if the user has explicitly opted in via settings.
	if isColocated {
		if adcGlobalDir, gErr := config.GetGlobalDir(); gErr == nil {
			if vs, loadErr := config.LoadSingleFileVersioned(adcGlobalDir); loadErr == nil &&
				vs != nil && vs.AutoInjectGcloudADC {
				home, _ := os.UserHomeDir()
				adcPath := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
				if data, err := os.ReadFile(adcPath); err == nil {
					alreadyHave := false
					for _, s := range opts.ResolvedSecrets {
						if s.Name == "gcloud-adc" {
							alreadyHave = true
							break
						}
					}
					if !alreadyHave {
						opts.ResolvedSecrets = append(opts.ResolvedSecrets, api.ResolvedSecret{
							Name:   "gcloud-adc",
							Type:   "file",
							Target: "/home/scion/.config/gcloud/application_default_credentials.json",
							Value:  string(data),
							Source: "runtime_broker",
						})
					}
				}
			}
		}
	}

	// --- Manager resolution ---
	// mgr and dispatchRuntimeType were already resolved once, above, for the
	// GCP identity check — reused here rather than calling
	// resolveManagerForOpts a second time. sc.Manager, returned below, is
	// only read by the create path (createAgent, handlers.go): start and
	// restart discard it and perform their own, later resolution instead
	// (opts.Profile is "" here on those two paths — their Config is nil —
	// while the GCP check above used the dispatch's profile, Config.Profile
	// on create or the agent's saved profile otherwise, so this resolution
	// and theirs can legitimately see different profiles; see the
	// downgradeUnverifiedHubDefaultPassthrough comment just below).

	// See downgradeUnverifiedHubDefaultPassthrough's doc comment: on the
	// create path this is the only, authoritative resolution; start/restart
	// re-run this same check after their own, later resolution.
	downgradeUnverifiedHubDefaultPassthrough(env, envCls, gcpMetadataMode, requireLocalRuntime, dispatchRuntimeType)

	sc := &startContext{
		Opts:               opts,
		TemplateSlug:       templateSlug,
		Manager:            mgr,
		RuntimeType:        dispatchRuntimeType,
		EnvClassifications: envCls,
	}
	if resolvedKSAName != "" {
		sel := assignIdentity.Selection
		sc.AssignSelection = &sel
	}
	return sc, nil
}

// hubDefaultPassthroughRuntimeTypes mirrors pkg/hub's map of the same name
// (default_gcp_identity.go): the runtime types the hub-default passthrough
// rung may apply to. Kept as the broker's own independent copy rather than a
// shared import — the hub and broker are separate deployables, and this is
// the broker's own second-line check on a value the hub already decided,
// not a re-import of that decision.
var hubDefaultPassthroughRuntimeTypes = map[string]bool{
	"docker": true,
	"podman": true,
}

// downgradeUnverifiedHubDefaultPassthrough re-checks a hub-default-granted
// GCP identity passthrough once the broker knows the runtime this agent will
// actually run under (resolvedRuntimeType, from resolveManagerForOpts), and
// rewrites env to the same "block" bundle buildStartContext's own block/
// assign case sets, if that runtime is not a local container runtime and not
// Kubernetes. Kubernetes does not support "block" (ptone/scion#2328): a
// Kubernetes resolution is left exactly as granted (passthrough) instead,
// which is already the correct outcome by construction — it is the same
// value the runtime-aware default (effectiveGCPMetadataMode) would have
// produced had the hub left this agent's identity unset instead of granting
// an unverified passthrough. Rewriting it to "block" here would both
// contradict that default and immediately re-fail buildStartContext's own
// earlier Kubernetes/"block" rejection on every subsequent start or restart.
//
// The hub's hub-default rung resolves an agent's runtime from the broker's
// own registration-time data (RuntimeBroker.Profiles/DefaultProfile), which
// can disagree with what this broker resolves for the same dispatch against
// its own, current project-effective settings (in-repo overrides, a DB
// settings overlay, or just registration drift). requireLocalRuntime is the
// signal that a passthrough grant came from that rung specifically —
// store.GCPIdentityConfig.RequireLocalRuntime on the create path, or the
// SCION_METADATA_REQUIRE_LOCAL_RUNTIME env var buildStartContext re-stamps
// for the start/restart paths, which never receive that struct at all (see
// its call sites for why each path needs its own call to this function).
// Explicit and project-level passthrough are never flagged, so this is a
// no-op for them by construction — this is strictly a second line of
// defense behind the hub-side gate (hubDefaultRuntimeAllowed,
// pkg/hub/default_gcp_identity.go), not a replacement for it.
//
// Called three times: once in buildStartContext, which is the only,
// authoritative resolution on the create path; and once each in startAgent
// and restartAgent (via the recheckHubDefaultPassthrough function), which
// re-resolve the manager a second time after a saved-profile lookup
// buildStartContext cannot see, making that second resolution the
// authoritative one on those two paths.
//
// env and envCls are mutated in place. envCls may be nil (the hub did not
// send classifications for this request); a nil map is left nil, matching
// classifyBrokerEnv's own rule elsewhere in this file. env itself is
// produced by buildStartContext as a non-nil map on every current call
// path: the direct call above assigns it from the same make() map just
// built; startAgent and restartAgent each take opts := sc.Opts from that
// same buildStartContext call and pass opts.Env on to
// recheckHubDefaultPassthrough without reassigning it, so they inherit the
// identical non-nil map by reference. A nil env is still handled: writing
// into a nil map would panic, so this returns without making any change —
// which, if that invariant were ever broken, is a fail-open outcome for a
// passthrough grant that should have been downgraded, not a fail-closed
// one. The Warn below exists so that break would be visible instead of
// silent.
func downgradeUnverifiedHubDefaultPassthrough(env map[string]string, envCls map[string]api.EnvKind, currentMetadataMode string, requireLocalRuntime bool, resolvedRuntimeType string) {
	if env == nil {
		slog.Warn("hub-default-passthrough-downgrade: nil env, returning without changes",
			"requireLocalRuntime", requireLocalRuntime, "resolvedRuntimeType", resolvedRuntimeType)
		return
	}
	if !requireLocalRuntime || currentMetadataMode != store.GCPMetadataModePassthrough {
		return
	}
	if hubDefaultPassthroughRuntimeTypes[resolvedRuntimeType] || isKubernetesRuntimeName(resolvedRuntimeType) {
		return
	}
	set := func(key, value string) {
		env[key] = value
		if envCls != nil {
			envCls[key] = api.EnvKindPlain
		}
	}
	set("SCION_METADATA_MODE", store.GCPMetadataModeBlock)
	set("SCION_METADATA_PORT", "18380")
	set("GCE_METADATA_HOST", "localhost:18380")
	set("GCE_METADATA_ROOT", "localhost:18380")
}

// startContextError is returned by buildStartContext for errors that need
// specific HTTP status codes or special handling (e.g. hub connectivity).
type startContextError struct {
	Status      int
	Message     string
	IsHubError  bool
	OriginalErr error
}

func (e *startContextError) Error() string {
	return e.Message
}

// isKubernetesRuntimeName reports whether name is a recognized spelling for
// the Kubernetes runtime: "kubernetes", the "k8s" alias, or "remote" (a
// settings.yaml runtime-type alias that pkg/runtime.GetRuntime normalizes to
// "kubernetes" — see factory.go). resolveManagerForOpts (handlers.go)
// constructs a real runtime via that normalization, so it never actually
// returns "remote"; resolveRuntimeNameForOpts, its side-effect-free sibling,
// returns settings.yaml's raw, unnormalized value without constructing
// anything, so it can. "remote" is checked here explicitly for that case,
// and kept for the former even though it is currently unreachable there, so
// the two functions keep classifying identically if that ever changes.
func isKubernetesRuntimeName(name string) bool {
	return name == "kubernetes" || name == "k8s" || name == "remote"
}

// effectiveGCPMetadataMode resolves the GCP identity mode that applies for a
// dispatch, given whether it targets Kubernetes and its GCP identity inputs
// (an explicit cfg.GCPIdentity, or a mode carried via resolvedEnv/cfg.Env —
// the same two raw sources buildStartContext's own merged `env` map is later
// built from). It mirrors buildStartContext's resolution without
// buildStartContext's validation/rejection: callers that need the "block is
// not offered on Kubernetes" check (ptone/scion#2328 phase 1) must still go
// through buildStartContext itself. This helper only answers "what mode
// would apply" — shared with the auth preflight's own GCP-credential check
// (handlers.go's extractRequiredEnvKeys), which needs to know whether GCP
// credentials will be available, not whether the dispatch is valid.
func effectiveGCPMetadataMode(isKubernetesDispatch bool, cfg *CreateAgentConfig, resolvedEnv map[string]string) string {
	mode := store.GCPMetadataModeBlock // secure default (non-Kubernetes)
	if isKubernetesDispatch {
		mode = store.GCPMetadataModePassthrough
	}
	if cfg != nil && cfg.GCPIdentity != nil {
		return cfg.GCPIdentity.MetadataMode
	}
	raw := resolvedEnv["SCION_METADATA_MODE"]
	source := resolvedEnv["SCION_METADATA_MODE_SOURCE"]
	if cfg != nil {
		for _, e := range cfg.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "SCION_METADATA_MODE":
				raw = parts[1]
			case "SCION_METADATA_MODE_SOURCE":
				source = parts[1]
			}
		}
	}
	if raw != "" {
		// The hub supplies SCION_METADATA_MODE (and SA details) via
		// resolvedEnv when dispatching a start for a provisioned agent. This
		// is also how a resolved project or hub default GCP identity mode
		// reaches the broker.
		//
		// The current hub always writes SCION_METADATA_MODE_SOURCE=hub
		// alongside its own authoritative mode (DispatchAgentStart,
		// DispatchAgentRestart, buildCreateRequest). A hub old enough to
		// predate that write won't send the marker at all, and on such a hub
		// this value could be whatever a stray stored env var or secret
		// happened to contain rather than a real dispatch decision. Downgrade
		// an elevated (non-block) mode to the secure default in that case; an
		// already out-of-range mode still falls through to the allow-list
		// rejection elsewhere unchanged, marker or not.
		elevated := raw == store.GCPMetadataModeAssign || raw == store.GCPMetadataModePassthrough
		if elevated && source != "hub" {
			return store.GCPMetadataModeBlock
		}
		return raw
	}
	return mode
}

// rejectKubernetesBlock returns the actionable error for "block" on the
// Kubernetes runtime (ptone/scion#2328) when metadataMode is explicitly
// "block" and resolvedRuntimeType is a recognized Kubernetes spelling, or nil
// otherwise. Shared by buildStartContext's own early check and by
// startAgent/restartAgent's late recheck (handlers.go) against the second,
// more specific manager resolution those two paths perform after a
// saved-profile lookup buildStartContext cannot see — the same two-resolution
// pattern recheckHubDefaultPassthrough already uses for the passthrough
// downgrade, applied here so a profile that only resolves to Kubernetes after
// that later lookup is rejected too, not just one visible to the early check.
func rejectKubernetesBlock(resolvedRuntimeType, metadataMode string) *startContextError {
	if metadataMode != store.GCPMetadataModeBlock || !isKubernetesRuntimeName(resolvedRuntimeType) {
		return nil
	}
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"GCP identity mode %q is not supported on the Kubernetes runtime; edit this agent's GCP identity mode to %q or %q, or change the project or hub default GCP identity mode for agents created after this",
			store.GCPMetadataModeBlock, store.GCPMetadataModeAssign, store.GCPMetadataModePassthrough),
	}
}

// redactCloneURL returns gc's clone URL with any userinfo removed, for
// logging. This covers both a user:pass URL and a username-only token URL
// (https://TOKEN@host) — net/url's Redacted() masks only a password, leaving
// a username-only token visible. On a parse failure, the raw URL is never
// logged: "<unparseable>" is returned instead.
func redactCloneURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	// A query string or fragment can carry a bare access token (e.g.
	// "?access_token=..."), the same way userinfo can — clear both.
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// sanitizeCloneErrorText strips a clone URL's credentials, query string, and
// fragment from errText. provision.ProvisionShared's own clone errors embed
// the raw URL verbatim (e.g. "git clone <url>: ..."), and git's own stderr
// output can separately echo the query string or fragment even when it
// omits userinfo on its own — this covers both by replacing the exact raw
// URL wholesale, then removing the userinfo, query, and fragment
// substrings individually so a reformatted echo of the same URL is caught
// too. Used to keep a raw clone URL (and any credential or token it
// carries) out of server-side logs, alongside the already-redacted
// `clone_url` attribute logged next to it.
func sanitizeCloneErrorText(errText, rawURL string) string {
	if rawURL == "" || errText == "" {
		return errText
	}
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		// Even when rawURL itself cannot be parsed, this package always
		// embeds it verbatim into its own error text, so the exact
		// occurrence is still stripped rather than left in place.
		return strings.ReplaceAll(errText, rawURL, "<unparseable>")
	}
	out := strings.ReplaceAll(errText, rawURL, redactCloneURL(rawURL))
	if u.User != nil {
		if pw, _ := u.User.Password(); u.User.Username() != "" || pw != "" {
			out = strings.ReplaceAll(out, u.User.String()+"@", "")
		}
	}
	if u.RawQuery != "" {
		out = strings.ReplaceAll(out, "?"+u.RawQuery, "")
	}
	if u.Fragment != "" {
		out = strings.ReplaceAll(out, "#"+u.Fragment, "")
	}
	return out
}

// isStrictWorktreeChild reports whether path is a real descendant of
// base's "worktrees" directory — never that directory itself, and never
// outside it. This is the only shape of path tryProvisionWorktree is ever
// allowed to pass to `git worktree remove` or os.RemoveAll: base's
// "worktrees" directory holds every agent's worktree for the project, so a
// resolver bug that ever produces that directory itself (or a path outside
// it) must never reach a removal call.
func isStrictWorktreeChild(base, path string) bool {
	if base == "" || path == "" {
		return false
	}
	worktreesDir := filepath.Clean(filepath.Join(base, "worktrees"))
	cleanPath := filepath.Clean(path)
	if cleanPath == worktreesDir {
		return false
	}
	rel, err := filepath.Rel(worktreesDir, cleanPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// shouldCleanupPartialWorktree reports whether tryProvisionWorktree's
// failure-cleanup path may remove worktreePath: never when it pre-existed
// (it may hold un-pushed work — the preExisted branch above already returns
// before reaching this call, but the check is repeated here so this
// function is correct on its own, independent of caller ordering), and only
// when the path is a real descendant of projectRoot's "worktrees" directory,
// never that directory itself. This is what a resolver bug producing an
// empty AgentID (GoogleCloudPlatform/scion#1931) must never be able to turn
// into a removal of every agent's worktree.
func shouldCleanupPartialWorktree(projectRoot, worktreePath string, preExisted bool) bool {
	if preExisted {
		return false
	}
	return worktreePath != "" && projectRoot != "" && isStrictWorktreeChild(projectRoot, worktreePath)
}

// validateMountedWorktree is the gate applied to the final resolved
// workspace path for a worktree-per-agent dispatch, right before it is set
// as opts.Workspace and mounted into the container. workspacePath itself —
// the exact string, not a resolved form of it — must already be the
// canonical "worktrees/<name>" path, and must also be, once symlinks are
// resolved, a direct child of base's own "worktrees" directory, and a real
// git worktree of base (provision.IsRealWorktreeDir). The "worktrees"
// directory itself is checked to confirm it is not a symlink; that check
// does not by itself say anything about workspacePath's own location.
func validateMountedWorktree(workspacePath, base string) error {
	worktreesDir := filepath.Join(base, "worktrees")
	wtInfo, err := os.Lstat(worktreesDir)
	if err != nil {
		return fmt.Errorf("%s: %w", worktreesDir, err)
	}
	if wtInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", worktreesDir)
	}

	// The exact string is what is stored and mounted, and downstream
	// consumers clean it lexically rather than resolving symlinks, so it
	// must already be the canonical worktrees/<name> form.
	if filepath.Clean(workspacePath) != workspacePath || filepath.Dir(workspacePath) != worktreesDir {
		return fmt.Errorf("%s must be the literal, canonical form of a direct child of %s", workspacePath, worktreesDir)
	}

	resolvedWorktreesDir, err := filepath.EvalSymlinks(worktreesDir)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", worktreesDir, err)
	}

	if !provision.IsRealWorktreeDir(workspacePath, base) {
		return fmt.Errorf("%s is not a git worktree of this checkout", workspacePath)
	}

	resolvedWorkspace, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", workspacePath, err)
	}
	// Only a direct child of resolvedWorktreesDir is accepted: workspacePath
	// itself, not any subdirectory of it, must sit beneath resolvedWorktreesDir.
	// This is deliberately stricter than "any descendant" — an agent's own
	// worktree and the shared .git are the only paths ever mounted read-write
	// into a container, and neither is a subdirectory of another worktree, so
	// a real worktree can never resolve to anything but a direct child here.
	if resolvedWorkspace == resolvedWorktreesDir || filepath.Dir(resolvedWorkspace) != resolvedWorktreesDir {
		return fmt.Errorf("%s must resolve to a direct child of %s", workspacePath, worktreesDir)
	}
	return nil
}

// worktreeBaseIsProvisioned reports whether the shared base clone for a
// worktree-per-agent project has already completed first-time provisioning:
// it returns true only when both the provisioning sentinel and the base's own
// .git are present, and false if either one is missing. It mirrors the check
// provision.ProvisionShared makes internally (its "sentinel exists" step). A
// caller that already knows a worktree it must not touch exists uses this to
// decide not to call ProvisionShared at all when either is missing —
// ProvisionShared's own self-heal (gitCloneWorkspace's removeDirContents)
// assumes no worktree can exist yet whenever the sentinel is missing, and
// would otherwise wipe every worktree under the shared base.
//
// The sentinel is looked for in exactly the directories ProvisionShared
// checks (in.SentinelDirs: the sentinel directory, the base's parent by
// default, and LegacyDir when set), so the two never disagree.
func worktreeBaseIsProvisioned(in provision.ProvisionInput) bool {
	if !provision.SentinelPresent(in.SentinelDirs()...) {
		return false
	}
	if _, err := os.Stat(filepath.Join(in.Resolved.HostPath, ".git")); err != nil {
		return false
	}
	return true
}

// tryProvisionWorktree attempts to provision a per-agent worktree on the host
// for worktree-per-agent mode. On success it sets opts.Workspace to the
// worktree path and returns true (opts.GitClone is NOT set, suppressing the
// in-container clone). On failure or if git is too old, it logs a warning and
// returns false so the caller falls through to clone-per-agent.
//
// runtimeName is the runtime this dispatch resolved to (buildStartContext's
// dispatchRuntimeType), not the broker's default runtime: host-side
// provisioning is skipped for an agent dispatched to kubernetes even when the
// broker's default runtime is docker.
func (s *Server) tryProvisionWorktree(ctx context.Context, in startContextInputs, opts *api.StartOptions, env map[string]string, runtimeName string) (bool, error) {
	// A path recovered from the agent's container is a .scion directory,
	// not the project root a worktree base is created under; provisioning
	// sees no project path for it, as it did before that path was recovered
	// ahead of this call (see ProjectPathFromContainer).
	worktreeProjectPath := in.ProjectPath
	if in.ProjectPathFromContainer {
		worktreeProjectPath = ""
	}
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: in.WorkspaceMode,
		GitClone:      in.Config.GitClone,
		ProjectPath:   worktreeProjectPath,
		ProjectID:     in.ProjectID,
		ProjectSlug:   in.ProjectSlug,
		AgentID:       in.AgentID,
		AgentName:     in.Name,
		Branch:        in.Config.Branch,
		RuntimeName:   runtimeName,
	})

	if !result.ShouldProvision {
		// A start dispatch (never a create) with no valid agent identity
		// fails closed instead of silently falling back to an in-container
		// clone: falling back would mount a fresh, empty workspace over
		// whatever this agent's real worktree holds, and there is no way to
		// tell from here whether one exists.
		if result.MissingIdentity && in.Operation != opCreate {
			return false, fmt.Errorf("worktree-per-agent: agent identity is not available for this start dispatch")
		}
		if result.Reason != "" {
			slog.Warn("worktree-per-agent: falling back to clone-per-agent",
				"agent_id", in.AgentID, "reason", result.Reason)
		}
		return false, nil
	}

	// Set Ctx from the buildStartContext context.
	result.ProvisionInput.Ctx = ctx

	// The sharer-registry key: the branch this agent's worktree is (or would
	// be) checked out on. Computed once, ahead of ProvisionShared, so the
	// same value can be used below both to detect a pre-existing JOIN target
	// and to resolve the authoritative workspace path afterward.
	branch := result.ProvisionInput.AgentName
	if branch == "" {
		branch = in.AgentID
	}

	// Serialize same-project provisioning on this node to prevent concurrent
	// ProvisionShared calls from racing on the shared base clone.
	mu := s.projectProvisionMutex(in.ProjectID, in.ProjectPath)
	mu.Lock()
	defer mu.Unlock()

	// The shared "worktrees" directory must be a real directory, not a
	// symlink, before any provisioning is attempted against it: git (and
	// the rest of this function) resolves through an intermediate symlink
	// like any other filesystem path, so a symlink here would silently
	// create or find worktrees somewhere other than under the base this
	// project owns. A missing "worktrees" directory is fine — it is created
	// fresh by ensureWorktree.
	worktreesDir := filepath.Join(result.ProjectRoot, "worktrees")
	if wtInfo, statErr := os.Lstat(worktreesDir); statErr == nil && wtInfo.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("worktree-per-agent: %s must not be a symlink", worktreesDir)
	}

	// Record whether this agent's own worktree, or the worktree of another
	// agent it is about to JOIN (an existing sharer registration for the
	// same branch), already exists — checked under the same lock held below,
	// so a concurrent dispatch cannot create the worktree in the gap between
	// this check and ProvisionShared. Either kind of pre-existing worktree
	// may hold un-pushed work and must never be removed, or silently
	// abandoned for a fresh in-container clone, on a later failure.
	preExisted := false
	if result.WorktreePath != "" {
		if _, statErr := os.Lstat(result.WorktreePath); statErr == nil {
			preExisted = true
		}
	}
	if !preExisted {
		if _, regPath, err := provision.ListSharers(result.ProjectRoot, branch); err == nil && regPath != "" {
			if _, statErr := os.Lstat(regPath); statErr == nil {
				preExisted = true
			}
		}
	}

	// When a worktree that must not be touched already exists, ProvisionShared
	// must never be allowed to reach its own self-heal path: gitCloneWorkspace's
	// removeDirContents can fire once the provisioning sentinel is missing, and
	// wipes every worktree under the shared base — including this one — while
	// ProvisionShared still returns success. Fail closed instead whenever
	// either the sentinel or the shared base's .git is missing, which is a
	// superset of that trigger condition.
	if preExisted && !worktreeBaseIsProvisioned(result.ProvisionInput) {
		return false, fmt.Errorf("worktree-per-agent: the existing worktree for agent %q is missing its provisioning marker or the shared base's .git; refusing to provision to avoid replacing it", in.AgentID)
	}

	if err := provision.ProvisionShared(result.ProvisionInput); err != nil {
		cloneURL := ""
		rawCloneURL := ""
		if result.ProvisionInput.GitClone != nil {
			rawCloneURL = result.ProvisionInput.GitClone.URL
			cloneURL = redactCloneURL(rawCloneURL)
		}
		sanitizedErr := sanitizeCloneErrorText(err.Error(), rawCloneURL)
		if preExisted {
			// The agent's own worktree, or the JOIN target's worktree,
			// already existed: it may hold un-pushed work, so it is never
			// removed or cleaned up here. Fail the dispatch instead of
			// falling back to an in-container clone, which would otherwise
			// mount a fresh, empty workspace in place of the existing one.
			// The detailed error is sanitized before logging (it can
			// otherwise embed the clone URL via git's own error text) and
			// logged server-side only; the client sees a generic message.
			slog.Error("worktree-per-agent: provisioning failed for an existing worktree; refusing to remove it or fall back to a fresh clone",
				"agent_id", in.AgentID, "path", result.WorktreePath, "clone_url", cloneURL, "error", sanitizedErr)
			return false, fmt.Errorf("worktree-per-agent: provisioning failed for the existing worktree of agent %q; the existing workspace was left untouched", in.AgentID)
		}
		slog.Warn("worktree-per-agent: provisioning failed, falling back to clone-per-agent",
			"agent_id", in.AgentID, "clone_url", cloneURL, "error", sanitizedErr)
		// Clean up ONLY this agent's partial worktree — never result.ProjectRoot,
		// the shared base clone holding the common .git and every other agent's
		// worktree under worktrees/<agentID>. Removing the base would destroy the
		// workspaces of all other running agents for this project. A partial base
		// clone is self-healed by provision.gitCloneWorkspace on retry.
		//
		// Reached only when neither this agent's own worktree nor a JOIN
		// target existed before this call (preExisted is false), so there is
		// nothing but this call's own partial state at that path to clean up.
		// isStrictWorktreeChild additionally guards that the path is a real
		// descendant of <base>/worktrees and never that directory itself, so
		// a resolver bug can never turn this into a removal of every agent's
		// worktree.
		if shouldCleanupPartialWorktree(result.ProjectRoot, result.WorktreePath, preExisted) {
			rm := exec.CommandContext(ctx, "git", "-C", result.ProjectRoot,
				"worktree", "remove", "--force", result.WorktreePath)
			if out, rmErr := rm.CombinedOutput(); rmErr != nil {
				slog.Warn("worktree-per-agent: git worktree remove failed, falling back to os.RemoveAll+prune",
					"agent_id", in.AgentID, "path", result.WorktreePath,
					"error", rmErr, "output", strings.TrimSpace(string(out)))
				if cleanErr := os.RemoveAll(result.WorktreePath); cleanErr != nil {
					slog.Warn("worktree-per-agent: failed to clean up partial worktree",
						"agent_id", in.AgentID, "path", result.WorktreePath, "error", cleanErr)
				}
				// Prune the now-stale .git/worktrees/<id> registration so retries succeed.
				_ = exec.CommandContext(ctx, "git", "-C", result.ProjectRoot, "worktree", "prune").Run()
			} else {
				slog.Info("worktree-per-agent: cleaned up partial worktree and unregistered from git",
					"agent_id", in.AgentID, "path", result.WorktreePath)
			}
		}
		return false, nil
	}

	// Source the authoritative worktree path from the sharer registry.
	// For a JOIN, the agent shares an existing worktree rather than having
	// its own at WorktreePath(base, agentID).
	actualWorkspace := result.WorktreePath
	if _, regPath, err := provision.ListSharers(result.ProjectRoot, branch); err == nil && regPath != "" {
		actualWorkspace = regPath
	}

	// The authoritative gate before mounting: whichever path actualWorkspace
	// turned out to be — this agent's own worktree, or a sharer-registry
	// JOIN target — it must be a real git worktree of this base, physically
	// located inside the base's own "worktrees" directory once symlinks are
	// resolved. Applied here, after both ProvisionShared and the
	// sharer-registry resolution have run, for both start and create, so it
	// covers every way actualWorkspace can be produced.
	if err := validateMountedWorktree(actualWorkspace, result.ProjectRoot); err != nil {
		slog.Error("worktree-per-agent: resolved workspace failed validation; refusing to mount it",
			"agent_id", in.AgentID, "path", actualWorkspace, "error", err)
		return false, fmt.Errorf("worktree-per-agent: the resolved workspace for agent %q failed validation", in.AgentID)
	}

	// Write .scion workspace marker so the in-container CLI discovers project context.
	if in.ProjectID != "" && in.ProjectSlug != "" {
		if err := config.WriteWorkspaceMarker(actualWorkspace, in.ProjectID, in.ProjectSlug, in.ProjectSlug); err != nil {
			slog.Warn("worktree-per-agent: failed to write workspace marker (non-fatal)",
				"path", actualWorkspace, "error", err)
		}
	}

	opts.Workspace = actualWorkspace
	if s.config.Debug {
		s.agentLifecycleLog.Debug("Worktree-per-agent mode enabled",
			"agent_id", in.AgentID,
			"workspace", result.WorktreePath,
			"project_root", result.ProjectRoot)
	}
	return true, nil
}

// projectProvisionMutex returns the per-project mutex for serializing worktree
// provisioning. Uses ProjectID as key, falling back to ProjectPath if empty.
func (s *Server) projectProvisionMutex(projectID, projectPath string) *sync.Mutex {
	key := projectID
	if key == "" {
		key = projectPath
	}
	actual, _ := s.projectProvisionMu.LoadOrStore(key, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// worktreeProvisionInput holds the fields needed to decide whether to
// provision a worktree and to build the ProvisionInput. Factored out
// for testability (no Server dependency).
type worktreeProvisionInput struct {
	WorkspaceMode string
	GitClone      *api.GitCloneConfig
	ProjectPath   string
	ProjectID     string
	ProjectSlug   string
	AgentID       string
	AgentName     string
	Branch        string

	// RuntimeName is the name of the container runtime ("kubernetes", "docker",
	// etc.) this dispatch resolved to. Used to reject host-side worktree provisioning
	// on Kubernetes where pods cannot bind-mount host worktrees — worktree-per-agent
	// on K8s requires the NFS backend (init-container path).
	RuntimeName string

	// eligibilityOverride, when non-nil, replaces the runtime.WorktreeModeEligible
	// check. Used in tests to simulate git-too-old without requiring a specific
	// git binary.
	eligibilityOverride func() (bool, string)
}

// worktreeProvisionResult holds the outcome of resolveWorktreeProvision.
type worktreeProvisionResult struct {
	ShouldProvision bool
	Reason          string
	ProvisionInput  provision.ProvisionInput
	WorktreePath    string
	ProjectRoot     string

	// MissingIdentity is true when ShouldProvision is false specifically
	// because AgentID or ProjectID was empty or not a valid single path
	// component — as opposed to any other ineligibility reason (git too
	// old, Kubernetes, wrong mode, non-git project). A start dispatch
	// (never a create) treats this case as fatal rather than falling back
	// to an in-container clone: see tryProvisionWorktree.
	MissingIdentity bool
}

// resolveWorktreeProvision is the pure decision function: given the dispatch
// inputs, it determines whether worktree provisioning should proceed and
// builds the ProvisionInput. It checks the git-version gate and resolves
// the workspace backend. No side effects — all provisioning happens in the
// caller.
func resolveWorktreeProvision(in worktreeProvisionInput) worktreeProvisionResult {
	if in.WorkspaceMode != store.WorkspaceModeWorktreePerAgent {
		return worktreeProvisionResult{Reason: "workspace mode is not worktree-per-agent"}
	}
	if in.GitClone == nil {
		return worktreeProvisionResult{Reason: "project is not git-backed"}
	}
	// AgentID and ProjectID are what keep this agent's worktree path
	// distinct from every other agent's, and from the shared "worktrees"
	// parent directory itself (provision.WorktreePath(base, "") resolves to
	// that parent when agentID is empty). Both must also be valid single
	// path components: neither one is decoded or otherwise interpreted
	// before being joined into a filesystem path, so a value containing a
	// separator, or equal to "." or "..", can otherwise place the resulting
	// path anywhere on the host — not just outside the intended worktree,
	// but potentially outside the project directory entirely. Refuse to
	// provision or touch anything on disk without both — skip to
	// clone-per-agent instead, the same as any other ineligibility reason
	// below (MissingIdentity distinguishes this case for the caller, which
	// treats it as fatal on a start dispatch instead of falling back).
	if !isSingleCleanPathElement(in.AgentID) || !isSingleCleanPathElement(in.ProjectID) {
		return worktreeProvisionResult{
			Reason:          "AgentID and ProjectID must both be present and valid for worktree-per-agent provisioning",
			MissingIdentity: true,
		}
	}

	eligCheck := runtime.WorktreeModeEligible
	if in.eligibilityOverride != nil {
		eligCheck = in.eligibilityOverride
	}
	eligible, reason := eligCheck()
	if !eligible {
		return worktreeProvisionResult{Reason: reason}
	}

	// On Kubernetes, host-side worktree provisioning does not work: pods
	// cannot bind-mount a host worktree. Worktree-per-agent on K8s is
	// supported only via the NFS backend (init-container path in
	// k8s_runtime.go). When the broker's host-side path is reached for a
	// K8s runtime, fall back to clone-per-agent.
	if isKubernetesRuntimeName(in.RuntimeName) {
		return worktreeProvisionResult{
			Reason: "worktree-per-agent on Kubernetes requires the NFS backend; " +
				"node-local host-side provisioning is not supported (pods cannot bind-mount host worktrees)",
		}
	}

	mode := store.SharingModeWorktreePerAgent
	backend := runtime.SelectWorkspaceBackend(nil, mode)
	resolved, err := backend.Resolve(runtime.ResolveInput{
		ProjectDir: in.ProjectPath,
		ProjectID:  in.ProjectID,
		AgentID:    in.AgentID,
		Mode:       mode,
	})
	if err != nil {
		return worktreeProvisionResult{Reason: "backend resolve failed: " + err.Error()}
	}

	agentName := in.AgentName
	if in.Branch != "" {
		agentName = in.Branch
	}
	if agentName == "" {
		agentName = in.AgentID
	}

	worktreePath := provision.WorktreePath(resolved.HostPath, in.AgentID)

	// Copy GitClone config so we don't mutate the shared pointer, and force a
	// full clone (Depth 0 = no --depth flag). The shared base needs full
	// history for coordinator merges, git log, and git blame (design §4.2a).
	fullCloneDepth := 0
	gcCopy := *in.GitClone
	gcCopy.Depth = &fullCloneDepth

	return worktreeProvisionResult{
		ShouldProvision: true,
		ProvisionInput: provision.ProvisionInput{
			Resolved:  resolved,
			Mode:      mode,
			GitClone:  &gcCopy,
			ProjectID: in.ProjectID,
			AgentID:   in.AgentID,
			AgentName: agentName,
			Locker:    nil,
		},
		WorktreePath: worktreePath,
		ProjectRoot:  resolved.HostPath,
	}
}

// globalProjectSlug is the hub slug of the global project.
const globalProjectSlug = "global"

// splitHubGlobalSlug separates the hub's global-project mark from the slug.
// The hub sends the global slug together with a project path only for its
// global project; the path still resolves the project, so the slug is
// dropped (returned empty) and reported as the mark instead. Any other
// slug is returned unchanged.
func splitHubGlobalSlug(projectPath, projectSlug string) (slug string, hubGlobal bool) {
	if projectPath != "" && projectSlug == globalProjectSlug {
		return "", true
	}
	return projectSlug, false
}

// globalDirProjectConflict returns a non-empty error message when
// projectPath is the broker's global scion directory (or a project root whose
// .scion entry is that directory) and the dispatch is not for the global
// project. The global project is the one the hub marks as global
// (hubGlobal), the "global" id, an empty id, or the hub id this broker has
// recorded for its global project (see isGlobalDirProjectID).
func globalDirProjectConflict(projectPath, projectID string, hubGlobal bool) string {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	if !config.IsGlobalProjectDir(projectPath) && !config.IsGlobalProjectDir(filepath.Join(projectPath, config.DotScion)) {
		return ""
	}
	if hubGlobal || projectID == "" || projectID == "global" || isGlobalDirProjectID(globalDir, projectID) {
		return ""
	}
	return fmt.Sprintf("project path %q is this broker's global scion directory, which cannot hold project %s. "+
		"Re-register this broker as a provider without a local path (scion runtime-broker provide --project <project>) "+
		"or with the project's own directory (--path)", projectPath, projectID)
}

// canRewriteProjectMarker reports whether the stale-marker branch may rewrite
// the .scion marker under projectPath for projectID. Any project directory
// other than the global directory may be rewritten. The global marker may
// only be rewritten for the global project: the one the hub marks as global,
// or the id the global settings record.
func canRewriteProjectMarker(projectPath, projectID string, hubGlobal bool) bool {
	if !config.IsGlobalProjectDir(projectPath) {
		return true
	}
	if hubGlobal {
		return true
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	id := globalSettingsProjectID(globalDir)
	return id != "" && id == projectID
}

// isGlobalDirProjectID reports whether projectID is the hub id this broker
// has recorded for its global project. The global settings decide when they
// record an id; a global .scion marker that disagrees with them is ignored.
// Without a settings id, the marker's project-id is used.
func isGlobalDirProjectID(globalDir, projectID string) bool {
	if id := globalSettingsProjectID(globalDir); id != "" {
		return id == projectID
	}
	// The global marker may carry an empty slug, which ReadProjectMarker
	// rejects, so only its project-id is read here.
	markerPath := filepath.Join(globalDir, config.DotScion)
	if !config.IsProjectMarkerFile(markerPath) {
		return false
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false
	}
	var marker config.ProjectMarker
	return yaml.Unmarshal(data, &marker) == nil && marker.ProjectID != "" && marker.ProjectID == projectID
}

// globalSettingsProjectID returns the hub project id recorded in the global
// settings, or "" when none is recorded.
func globalSettingsProjectID(globalDir string) string {
	settings, err := config.LoadSettings(globalDir)
	if err != nil {
		return ""
	}
	if id := settings.GetHubProjectID(); id != "" {
		return id
	}
	return settings.ProjectID
}

// isStaleExternalDir returns true if the external project config directory
// no longer exists on disk. This indicates the project was deleted (which
// cleans up external config) and recreated with a new ID
// (miller79/scion#28). An empty extDir is treated as stale.
func isStaleExternalDir(extDir string) bool {
	if extDir == "" {
		return true
	}
	_, statErr := os.Stat(extDir)
	return os.IsNotExist(statErr)
}

// withHubAgentDefaults attaches the hub's operational agent_defaults from a
// create request to the provisioning context, so ProvisionAgent can apply them
// at its own low-precedence tier (below template and inline config, above this
// broker's settings.yaml defaults).
//
// The value rides the context rather than a new ProvisionAgent parameter,
// following ContextWithBrokerMode / ContextWithHarnessConfigPath /
// ContextWithGitClone — ProvisionAgent already takes eleven arguments.
//
// Returns ctx unchanged when the hub sent nothing, which is every local
// dispatch, every file-mode hub (design §3.2.4), and every hub that predates
// the wire field. An empty-but-non-nil value is treated as absent too, so a
// downstream nil check is the only gate needed.
func withHubAgentDefaults(ctx context.Context, cfg *CreateAgentConfig) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return ctx
	}
	return withStartHubAgentDefaults(ctx, cfg.HubAgentDefaults)
}

// withStartHubAgentDefaults attaches hub defaults decoded from a start or
// restart request body (or a create request's config) to ctx, so
// Manager.Start's buildAgentEnv applies their env entries at its lowest tier.
// Returns ctx unchanged for a nil or empty value.
func withStartHubAgentDefaults(ctx context.Context, d *api.HubAgentDefaults) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.IsEmpty() {
		return ctx
	}
	return api.ContextWithHubAgentDefaults(ctx, d)
}

// emptyPerAgentConflict returns a client-facing message when an
// empty-per-agent request (design #2703) also names another workspace
// source, or "" when it does not. The hub never sends these together; a
// request that does is refused rather than resolved in favour of either.
func emptyPerAgentConflict(in startContextInputs) string {
	if in.WorkspaceStoragePath != "" {
		return "empty-per-agent workspaces cannot be seeded from a workspace upload"
	}
	cfg := in.Config
	if cfg == nil {
		return ""
	}
	switch {
	case cfg.Workspace != "":
		return "empty-per-agent workspaces do not take a workspace path"
	case cfg.GitClone != nil:
		return "empty-per-agent workspaces cannot be combined with a git clone"
	case cfg.SharedWorkspace:
		return "empty-per-agent workspaces cannot be combined with a shared workspace"
	}
	return ""
}

// ambiguousNonGitWorkspace returns a client-facing message when a create for
// a hub-managed project (ProjectPath is ~/.scion/projects/<slug>) names no
// workspace source at all: no workspace mode, no workspace path, no GCS
// workspace upload, no git clone, no shared workspace and no worktree. A
// current hub always sends one of them (the hub-managed project path, or for
// a remote broker with hub storage the upload's workspaceStoragePath, for a
// shared non-git project; the empty-per-agent mode otherwise), so such a
// request means the mode was lost on the way (or the hub could not compute
// the project path, which also fails closed here).
// Provisioning would then fall back to the shared project directory, which
// for an empty-per-agent agent breaks isolation, so it is refused (design
// #2703 P2). Start and restart are not checked: they legitimately omit the
// config, and the hub re-sends the mode on them.
func ambiguousNonGitWorkspace(in startContextInputs, worktreeProvisioned bool) string {
	if in.Operation != opCreate || in.ProjectSlug == "" || in.WorkspaceMode != "" || worktreeProvisioned {
		return ""
	}
	// Only the conventional hub-managed path (which createAgent resolves
	// from the slug before buildStartContext) is checked; a linked
	// project's own ProjectPath keeps its existing resolution.
	globalDir, err := config.GetGlobalDir()
	if err != nil || filepath.Clean(in.ProjectPath) != filepath.Join(globalDir, "projects", in.ProjectSlug) {
		return ""
	}
	if in.WorkspaceStoragePath != "" {
		return ""
	}
	if in.Config != nil && (in.Config.Workspace != "" || in.Config.GitClone != nil || in.Config.SharedWorkspace) {
		return ""
	}
	return "ambiguous workspace for hub-managed project " + in.ProjectSlug +
		": the request has no workspace mode, workspace path or git clone; refusing to fall back to the shared project directory"
}
