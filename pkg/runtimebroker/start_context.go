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
	"log/slog"
	"net/http"
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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// startContext holds all the resolved state needed to start an agent.
// It is built by buildStartContext from the various handler-specific inputs,
// unifying project path resolution, env merging, template hydration, and
// manager selection into a single code path.
type startContext struct {
	Opts         api.StartOptions
	TemplateSlug string
	Manager      agent.Manager
}

// startContextInputs captures the handler-specific fields that vary across
// createAgent, startAgent, restartAgent, and finalizeEnv. Each handler
// populates this from its own request structure, then calls buildStartContext.
type startContextInputs struct {
	// Agent identity
	Name    string
	AgentID string // Hub UUID (for env injection and logging)
	Slug    string

	// Project
	ProjectPath string
	ProjectSlug string
	ProjectID   string

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
	ResolvedEnv     map[string]string
	ResolvedSecrets []api.ResolvedSecret

	// Behavior
	Attach bool

	// WorkspaceMode is the resolved workspace sharing mode for the project
	// (e.g. "worktree-per-agent"). Threaded from CreateAgentRequest so the
	// broker can branch dispatch without re-deriving from labels.
	WorkspaceMode string

	// HTTP request (for hub connection resolution)
	HTTPRequest *http.Request
}

// buildStartContext unifies the common startup logic shared by createAgent,
// startAgent, restartAgent, and finalizeEnv:
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
	// --- Hub-managed project path resolution ---
	if in.ProjectSlug != "" && in.ProjectPath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return nil, &startContextError{Status: http.StatusInternalServerError, Message: "Failed to get global dir: " + err.Error()}
		}
		projectsPath := filepath.Join(globalDir, "projects", in.ProjectSlug)
		if !hasWorkspaceContent(projectsPath) {
			// fallback to groves/ for backward compatibility
			legacyPath := filepath.Join(globalDir, "groves", in.ProjectSlug)
			if hasWorkspaceContent(legacyPath) {
				projectsPath = legacyPath
			}
		}
		in.ProjectPath = projectsPath
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Resolved hub-managed project path from slug",
				"agent_id", in.AgentID, "slug", in.ProjectSlug, "path", in.ProjectPath)
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
	if in.ProjectSlug != "" && in.ProjectPath != "" {
		scionPath := filepath.Join(in.ProjectPath, config.DotScion)

		if config.IsProjectMarkerFile(scionPath) {
			// .scion is a marker file — project-id is already recorded.
			// Ensure external split storage directories exist.
			if marker, err := config.ReadProjectMarker(scionPath); err == nil && marker.ProjectID != "" {
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
				if existingID, err := config.ReadProjectID(scionPath); err != nil || existingID == "" {
					if wErr := config.WriteProjectID(scionPath, in.ProjectID); wErr != nil {
						s.agentLifecycleLog.Warn("Failed to write project-id for hub-managed project",
							"agent_id", in.AgentID, "project_id", in.ProjectID, "error", wErr)
					} else {
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

	// --- Build merged environment ---
	env := make(map[string]string)

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
			}
		}
	}

	// 3. Hub auth token
	if in.AgentToken != "" {
		env["SCION_AUTH_TOKEN"] = in.AgentToken
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_AUTH_TOKEN set from agent token", "agent_id", in.AgentID, "length", len(in.AgentToken))
		}
	} else if devToken := os.Getenv("SCION_AUTH_TOKEN"); devToken != "" {
		env["SCION_AUTH_TOKEN"] = devToken
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_AUTH_TOKEN set from broker env", "agent_id", in.AgentID, "length", len(devToken))
		}
	}

	// 4. Hub endpoint
	runtimeName := ""
	if s.runtime != nil {
		runtimeName = s.runtime.Name()
	}

	// Resolve hub connection early — needed for colocated detection and
	// template hydration below.
	var hubConn *HubConnection
	if in.HTTPRequest != nil {
		hubConn = s.resolveHubConnection(in.HTTPRequest)
	}

	var hubEndpoint string
	if in.HTTPRequest != nil {
		// Full create path: request-level, connection-level, and broker-level fallbacks
		hubEndpoint = resolveHubEndpointForCreate(
			in.HubEndpoint,
			s.resolveHubEndpointFromRequest(in.HTTPRequest),
			s.config.HubEndpoint,
			in.ResolvedEnv,
			in.ProjectPath,
			s.config.ContainerHubEndpoint,
			runtimeName,
		)
	} else {
		// Start/restart/finalize path: broker-level and settings fallbacks
		hubEndpoint = resolveHubEndpointForStart(
			s.config.HubEndpoint,
			in.ResolvedEnv,
			in.ProjectPath,
			s.config.ContainerHubEndpoint,
			runtimeName,
		)
	}
	if hubEndpoint != "" {
		env["SCION_HUB_ENDPOINT"] = hubEndpoint
		env["SCION_HUB_URL"] = hubEndpoint // legacy compat
		if s.config.Debug {
			s.agentLifecycleLog.Debug("SCION_HUB_ENDPOINT set", "agent_id", in.AgentID, "endpoint", hubEndpoint)
		}
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
	}
	if in.AgentID != "" {
		env["SCION_AGENT_ID"] = in.AgentID
	}
	if in.ProjectID != "" {
		env["SCION_GROVE_ID"] = in.ProjectID
		env["SCION_PROJECT_ID"] = in.ProjectID
	}
	if in.ProjectPath != "" {
		env["SCION_GROVE_PATH"] = in.ProjectPath
		env["SCION_PROJECT_PATH"] = in.ProjectPath
	}

	// 6. Broker identity
	if s.config.BrokerName != "" {
		env["SCION_BROKER_NAME"] = s.config.BrokerName
	}
	if s.config.BrokerID != "" {
		env["SCION_BROKER_ID"] = s.config.BrokerID
	}
	if in.CreatorName != "" {
		env["SCION_CREATOR"] = in.CreatorName
	}

	// 7. Debug
	if s.config.Debug {
		env["SCION_DEBUG"] = "1"
	}

	// 8. GCP identity metadata server configuration.
	// Default to "block" when no GCP identity config is provided, so agents
	// cannot access the underlying compute identity via the GCE metadata
	// server unless the hub explicitly sets "passthrough" or "assign".
	//
	// Priority: explicit Config.GCPIdentity (create path) > resolvedEnv
	// values injected by the hub (start path) > secure "block" default.
	gcpMetadataMode := "block" // secure default
	if in.Config != nil && in.Config.GCPIdentity != nil {
		gcpMetadataMode = in.Config.GCPIdentity.MetadataMode
	} else if mode := env["SCION_METADATA_MODE"]; mode != "" {
		// The hub injects SCION_METADATA_MODE (and SA details) via
		// resolvedEnv when dispatching a start for a provisioned agent.
		gcpMetadataMode = mode
	}
	if gcpMetadataMode == "assign" || gcpMetadataMode == "block" {
		env["SCION_METADATA_MODE"] = gcpMetadataMode
		env["SCION_METADATA_PORT"] = "18380"
		if gcpMetadataMode == "assign" && in.Config != nil && in.Config.GCPIdentity != nil {
			env["SCION_METADATA_SA_EMAIL"] = in.Config.GCPIdentity.SAEmail
			env["SCION_METADATA_PROJECT_ID"] = in.Config.GCPIdentity.ProjectID
		}
		env["GCE_METADATA_HOST"] = "localhost:18380"
		// gcloud CLI uses GCE_METADATA_ROOT (not GCE_METADATA_HOST) to locate
		// the metadata server during its initial configuration detection.
		env["GCE_METADATA_ROOT"] = "localhost:18380"
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
		Name:        in.Name,
		BrokerMode:  true,
		ProjectPath: in.ProjectPath,
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
	}

	if in.InlineConfig != nil {
		opts.InlineConfig = in.InlineConfig
	}

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

	if templateSlug != "" {
		opts.TemplateName = templateSlug
	}

	// --- Shared workspace mode (git-workspace hybrid) ---
	if in.Config != nil && in.Config.SharedWorkspace {
		env["SCION_SHARED_WORKSPACE"] = "true"
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Shared workspace mode enabled", "agent_id", in.AgentID)
		}
	}

	// --- Worktree-per-agent mode ---
	// When the hub sets WorkspaceMode to worktree-per-agent and the project
	// is git-backed, provision a shared base clone + per-agent worktree on
	// the host BEFORE the container starts, then dual-mount it. This avoids
	// the full in-container clone. Falls through to clone-per-agent on error
	// or if git is too old (< 2.47).
	worktreeProvisioned := false
	if in.Config != nil && in.Config.GitClone != nil && in.WorkspaceMode == store.WorkspaceModeWorktreePerAgent {
		worktreeProvisioned = s.tryProvisionWorktree(ctx, in, &opts, env)
	}

	// --- Git clone mode ---
	if !worktreeProvisioned && in.Config != nil && in.Config.GitClone != nil {
		gc := in.Config.GitClone
		env["SCION_GIT_CLONE_URL"] = gc.URL
		if gc.Branch != "" {
			env["SCION_GIT_BRANCH"] = gc.Branch
		}
		if gc.Depth > 0 {
			env["SCION_GIT_DEPTH"] = strconv.Itoa(gc.Depth)
		}
		if in.Config.Branch != "" {
			env["SCION_AGENT_BRANCH"] = in.Config.Branch
		}
		opts.Workspace = ""
		// Keep opts.ProjectPath so that ProvisionAgent can resolve the correct
		// agent directory (e.g. ~/.scion.projects/<slug>/) instead of falling
		// back to the global project. The git-clone check in ProvisionAgent
		// runs before the worktree logic, so no worktree will be created.
		opts.GitClone = gc
		if s.config.Debug {
			s.agentLifecycleLog.Debug("Git clone mode enabled", "agent_id", in.AgentID,
				"cloneURL", gc.URL, "branch", gc.Branch, "depth", gc.Depth)
		}
	}

	// --- Env + telemetry + secrets ---
	opts.Env = env

	if v, ok := env["SCION_TELEMETRY_ENABLED"]; ok {
		enabled := v == "true" || v == "1"
		opts.TelemetryOverride = &enabled
	}

	if len(in.ResolvedSecrets) > 0 {
		opts.ResolvedSecrets = in.ResolvedSecrets
		if s.config.Debug {
			s.envSecretLog.Debug("Received resolved secrets", "count", len(in.ResolvedSecrets))
		}
	}

	// --- Manager resolution ---
	mgr := s.resolveManagerForOpts(opts)

	return &startContext{
		Opts:         opts,
		TemplateSlug: templateSlug,
		Manager:      mgr,
	}, nil
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

// tryProvisionWorktree attempts to provision a per-agent worktree on the host
// for worktree-per-agent mode. On success it sets opts.Workspace to the
// worktree path and returns true (opts.GitClone is NOT set, suppressing the
// in-container clone). On failure or if git is too old, it logs a warning and
// returns false so the caller falls through to clone-per-agent.
func (s *Server) tryProvisionWorktree(ctx context.Context, in startContextInputs, opts *api.StartOptions, env map[string]string) bool {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: in.WorkspaceMode,
		GitClone:      in.Config.GitClone,
		ProjectPath:   in.ProjectPath,
		ProjectID:     in.ProjectID,
		ProjectSlug:   in.ProjectSlug,
		AgentID:       in.AgentID,
		AgentName:     in.Name,
		Branch:        in.Config.Branch,
	})

	if !result.ShouldProvision {
		if result.Reason != "" {
			slog.Warn("worktree-per-agent: falling back to clone-per-agent",
				"agent_id", in.AgentID, "reason", result.Reason)
		}
		return false
	}

	// Set Ctx from the buildStartContext context.
	result.ProvisionInput.Ctx = ctx

	// Serialize same-project provisioning on this node to prevent concurrent
	// ProvisionShared calls from racing on the shared base clone.
	mu := s.projectProvisionMutex(in.ProjectID, in.ProjectPath)
	mu.Lock()
	defer mu.Unlock()

	if err := provision.ProvisionShared(result.ProvisionInput); err != nil {
		slog.Warn("worktree-per-agent: provisioning failed, falling back to clone-per-agent",
			"agent_id", in.AgentID, "error", err)
		// Clean up ONLY this agent's partial worktree — never result.ProjectRoot,
		// the shared base clone holding the common .git and every other agent's
		// worktree under worktrees/<agentID>. Removing the base would destroy the
		// workspaces of all other running agents for this project. A partial base
		// clone is self-healed by provision.gitCloneWorkspace on retry.
		//
		// Use `git worktree remove --force` so the worktree's admin metadata in
		// the base's .git/worktrees/<id> is unregistered too — a bare os.RemoveAll
		// would leave a stale registration that makes git refuse to recreate the
		// worktree at that path on retry. Fall back to os.RemoveAll + prune.
		if result.WorktreePath != "" && result.ProjectRoot != "" {
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
		return false
	}

	// Write .scion workspace marker so the in-container CLI discovers project context.
	if in.ProjectID != "" && in.ProjectSlug != "" {
		if err := config.WriteWorkspaceMarker(result.WorktreePath, in.ProjectID, in.ProjectSlug, in.ProjectSlug); err != nil {
			slog.Warn("worktree-per-agent: failed to write workspace marker (non-fatal)",
				"path", result.WorktreePath, "error", err)
		}
	}

	opts.Workspace = result.WorktreePath
	if s.config.Debug {
		s.agentLifecycleLog.Debug("Worktree-per-agent mode enabled",
			"agent_id", in.AgentID,
			"workspace", result.WorktreePath,
			"project_root", result.ProjectRoot)
	}
	return true
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

	eligCheck := runtime.WorktreeModeEligible
	if in.eligibilityOverride != nil {
		eligCheck = in.eligibilityOverride
	}
	eligible, reason := eligCheck()
	if !eligible {
		return worktreeProvisionResult{Reason: reason}
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
	// full clone (Depth -1 ≡ no --depth flag). The shared base needs full
	// history for coordinator merges, git log, and git blame (design §4.2a).
	gcCopy := *in.GitClone
	gcCopy.Depth = -1

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

// hasWorkspaceContent returns true if dir exists and contains meaningful
// workspace files beyond just infrastructure directories.
func hasWorkspaceContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		switch e.Name() {
		case "shared-dirs", ".scion":
			continue
		default:
			return true
		}
	}
	return false
}
