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

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// readSecretFunc reads a password/secret from a file descriptor with echo disabled.
// It is a variable so tests can override it.
var readSecretFunc = func(fd int) ([]byte, error) {
	return term.ReadPassword(fd)
}

// isInteractiveTerminal reports whether the process is running in an
// interactive terminal. It is a variable so tests can override it.
var isInteractiveTerminal = func() bool {
	return util.IsTerminal()
}

var (
	templateName          string
	agentImage            string
	noAuth                bool
	attach                bool
	branch                string
	workspace             string
	runtimeBrokerID       string
	harnessConfigFlag     string
	harnessAuthFlag       string
	startNoNotify         bool
	startNotifyDeprecated bool
	enableTelemetry       bool
	disableTelemetry      bool
	inlineConfigPath      string
)

// loadInlineConfig loads a ScionConfig from the --config flag path.
// If path is "-", reads from stdin. Supports YAML and JSON formats.
// Returns (nil, nil) if no inline config path is set.
func loadInlineConfig(path string) (*api.ScionConfig, string, error) {
	if path == "" {
		return nil, "", nil
	}

	var data []byte
	var configDir string
	var err error

	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read config from stdin: %w", err)
		}
		// When reading from stdin, use CWD as the config dir for relative file:// URIs
		configDir, _ = os.Getwd()
	} else {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, "", fmt.Errorf("failed to resolve config path: %w", err)
		}
		data, err = os.ReadFile(absPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read config file %s: %w", absPath, err)
		}
		configDir = filepath.Dir(absPath)
	}

	if len(data) == 0 {
		return nil, "", fmt.Errorf("config file is empty")
	}

	var cfg api.ScionConfig

	// Try JSON first (if it starts with '{'), otherwise YAML
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config as JSON: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config as YAML: %w", err)
		}
	}

	// Store the config directory for file:// URI resolution
	cfg.ConfigDir = configDir

	return &cfg, configDir, nil
}

// resolveInlineConfigContent resolves file:// URIs in system_prompt and
// agent_instructions fields of an inline config. The configDir is used
// as the base for relative file:// URIs.
func resolveInlineConfigContent(cfg *api.ScionConfig, configDir string) error {
	if cfg.SystemPrompt != "" {
		resolved, err := api.ResolveContent(cfg.SystemPrompt, configDir)
		if err != nil {
			return fmt.Errorf("failed to resolve system_prompt: %w", err)
		}
		cfg.SystemPrompt = resolved
	}
	if cfg.AgentInstructions != "" {
		resolved, err := api.ResolveContent(cfg.AgentInstructions, configDir)
		if err != nil {
			return fmt.Errorf("failed to resolve agent_instructions: %w", err)
		}
		cfg.AgentInstructions = resolved
	}
	return nil
}

// HubContext holds the context for Hub operations.
type HubContext struct {
	Client      hubclient.Client
	Endpoint    string
	Settings    *config.Settings
	ProjectID   string
	BrokerID    string
	ProjectPath string
	IsGlobal    bool
}

// getHubAccessToken returns an access token for authenticating to the Hub.
// It checks OAuth credentials first, then falls back to dev-auth tokens.
// This mirrors the auth resolution order used by hubsync.createHubClient.
func getHubAccessToken(endpoint string) string {
	// Priority 1: OAuth credentials from scion hub auth login
	if token := credentials.GetAccessToken(endpoint); token != "" {
		return token
	}

	// Priority 2: Dev auth token
	return apiclient.ResolveDevToken()
}

// CheckHubAvailability checks if Hub integration is enabled and returns a ready-to-use
// Hub context if available. Returns nil if Hub should not be used (not enabled or --no-hub flag is set).
//
// IMPORTANT: When Hub is enabled, this function will return an error if the Hub is
// unavailable or misconfigured. There is NO silent fallback to local mode - this is
// by design to ensure users always know which mode they're operating in.
//
// This function now performs full Hub sync checks via hubsync.EnsureHubReady:
// - Verifies project registration (prompts to register if not)
// - Compares local and Hub agents (prompts to sync if mismatched)
func CheckHubAvailability(projectPath string) (*HubContext, error) {
	return CheckHubAvailabilityWithOptions(projectPath, false)
}

// CheckHubAvailabilityWithOptions is like CheckHubAvailability but allows skipping sync.
func CheckHubAvailabilityWithOptions(projectPath string, skipSync bool) (*HubContext, error) {
	return CheckHubAvailabilityForAgents(projectPath, nil, skipSync)
}

// CheckHubAvailabilityForAgent checks Hub availability for an operation on a specific agent.
// The targetAgent parameter specifies the agent being operated on, which will be excluded
// from sync requirements. This allows operations like delete to proceed without first
// syncing the target agent (e.g., deleting a local-only agent without registering it).
func CheckHubAvailabilityForAgent(projectPath, targetAgent string, skipSync bool) (*HubContext, error) {
	return CheckHubAvailabilityForAgents(projectPath, []string{targetAgent}, skipSync)
}

// CheckHubAvailabilityForAgents checks Hub availability for operations on one or more agents.
// All target agents are excluded from sync requirements so batch operations are not blocked
// by sync drift on the exact agents being modified.
func CheckHubAvailabilityForAgents(projectPath string, excludedAgents []string, skipSync bool) (*HubContext, error) {
	targetAgent := ""
	if len(excludedAgents) > 0 {
		targetAgent = excludedAgents[0] // compatibility field for older internals
	}

	opts := hubsync.EnsureHubReadyOptions{
		AutoConfirm:      autoConfirm,
		NoHub:            noHub,
		EndpointOverride: hubEndpoint,
		SkipSync:         skipSync,
		TargetAgent:      targetAgent,
		ExcludedAgents:   excludedAgents,
	}

	hubCtx, err := hubsync.EnsureHubReady(projectPath, opts)
	if err != nil {
		return nil, err
	}

	if hubCtx == nil {
		return nil, nil
	}

	// Convert hubsync.HubContext to cmd.HubContext
	return &HubContext{
		Client:    hubCtx.Client,
		Endpoint:  hubCtx.Endpoint,
		Settings:  hubCtx.Settings,
		ProjectID:   hubCtx.ProjectID,
		BrokerID:  hubCtx.BrokerID,
		ProjectPath: hubCtx.ProjectPath,
		IsGlobal:  hubCtx.IsGlobal,
	}, nil
}

// PrintUsingHub prints the informational message about using the Hub to stderr.
// It is suppressed entirely in JSON output mode to keep stdout clean.
func PrintUsingHub(endpoint string) {
	if isJSONOutput() {
		return
	}
	fmt.Fprintf(os.Stderr, "Using hub: %s\n", endpoint)
}

// wrapHubError wraps a Hub error with guidance to disable Hub integration.
func wrapHubError(err error) error {
	if apiclient.IsUnauthorizedError(err) {
		return fmt.Errorf("authentication failed, login to hub with 'scion hub auth login'")
	}
	return fmt.Errorf("%w\n\nTo use local-only mode, run: scion hub disable", err)
}

// GetProjectID looks up the project ID from HubContext or settings.
// Priority:
//  1. ProjectID field in HubContext (set by EnsureHubReady)
//  2. Local project_id from settings (for non-git projects or explicit configuration)
//  3. Git remote lookup via Hub API
//
// Returns the project ID if found, or an error if the project is not registered.
func GetProjectID(hubCtx *HubContext) (string, error) {
	// First, check if ProjectID is already set in the context
	if hubCtx.ProjectID != "" {
		return hubCtx.ProjectID, nil
	}

	// Check if there's a hub project ID or local project_id in settings
	if hubCtx.Settings != nil {
		if hgid := hubCtx.Settings.GetHubProjectID(); hgid != "" {
			return hgid, nil
		}
		if hubCtx.Settings.ProjectID != "" {
			return hubCtx.Settings.ProjectID, nil
		}
	}

	// Fall back to git remote lookup
	gitRemote := util.GetGitRemote()
	if gitRemote == "" {
		return "", fmt.Errorf("no git origin remote found for this project.\n\nThe Hub uses the origin remote URL to identify projects.\nRun 'scion hub link' to link this project with the Hub, or use --no-hub for local-only mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Look up projects by git remote
	resp, err := hubCtx.Client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		GitRemote: util.NormalizeGitRemote(gitRemote),
	})
	if err != nil {
		return "", fmt.Errorf("failed to look up project by git remote: %w", err)
	}

	if len(resp.Projects) == 0 {
		return "", fmt.Errorf("no project found for git remote: %s\n\nRun 'scion hub link' to link this project with the Hub", gitRemote)
	}

	// Return the first matching project
	return resp.Projects[0].ID, nil
}

func RunAgent(cmd *cobra.Command, args []string, resume bool) error {
	agentName := api.Slugify(args[0])
	task := strings.Join(args[1:], " ")

	// Reject --format json with --attach (mutually exclusive)
	if isJSONOutput() && attach {
		return fmt.Errorf("--format json and --attach are mutually exclusive")
	}

	// Reject --enable-telemetry with --disable-telemetry (mutually exclusive)
	if enableTelemetry && disableTelemetry {
		return fmt.Errorf("--enable-telemetry and --disable-telemetry are mutually exclusive")
	}

	// Validate --harness-auth value
	if harnessAuthFlag != "" {
		switch harnessAuthFlag {
		case "api-key", "oauth-token", "auth-file", "vertex-ai":
			// valid
		default:
			return fmt.Errorf("invalid --harness-auth value %q: must be one of api-key, oauth-token, auth-file, vertex-ai", harnessAuthFlag)
		}
	}

	// Check if Hub should be used, excluding the target agent from sync requirements.
	// Load inline config if --config was specified (needed for both local and Hub paths)
	var inlineCfg *api.ScionConfig
	var inlineConfigDir string
	if inlineConfigPath != "" {
		var err error
		inlineCfg, inlineConfigDir, err = loadInlineConfig(inlineConfigPath)
		if err != nil {
			return err
		}
		// Resolve file:// URIs in content fields
		if err := resolveInlineConfigContent(inlineCfg, inlineConfigDir); err != nil {
			return err
		}
	}

	// This allows starting/resuming an agent even if it exists on Hub but not locally
	// (will be created via Hub) or if other agents are out of sync.
	hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
	if err != nil {
		return err
	}

	if hubCtx != nil {
		return startAgentViaHub(hubCtx, agentName, task, resume, inlineCfg)
	}

	// Local mode
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	// Check if already running and we want to attach
	if attach {
		agents, err := rt.List(context.Background(), map[string]string{"scion.name": agentName})
		if err == nil {
			for _, a := range agents {
				if strings.EqualFold(a.Name, agentName) || a.ID == agentName || strings.EqualFold(strings.TrimPrefix(a.Name, "/"), agentName) {
					status := strings.ToLower(a.ContainerStatus)
					isRunning := strings.HasPrefix(status, "up") || status == "running"
					if isRunning {
						fmt.Printf("Agent '%s' is already running. Attaching...\n", agentName)
						return rt.Attach(context.Background(), agentName)
					}
				}
			}
		}
	}

	// Flag takes ultimate precedence
	resolvedImage := agentImage

	var detached *bool
	if attach {
		val := false
		detached = &val
	}

	// Apply inline config overrides to CLI options
	effectiveBranch := branch
	effectiveTask := strings.TrimSpace(task)
	effectiveHarnessConfig := harnessConfigFlag
	effectiveHarnessAuth := harnessAuthFlag

	if inlineCfg != nil {
		if effectiveBranch == "" && inlineCfg.Branch != "" {
			effectiveBranch = inlineCfg.Branch
		}
		if effectiveTask == "" && inlineCfg.Task != "" {
			effectiveTask = inlineCfg.Task
		}
		if effectiveHarnessConfig == "" && inlineCfg.HarnessConfig != "" {
			effectiveHarnessConfig = inlineCfg.HarnessConfig
		}
		if effectiveHarnessAuth == "" && inlineCfg.AuthSelectedType != "" {
			effectiveHarnessAuth = inlineCfg.AuthSelectedType
		}
		if resolvedImage == "" && inlineCfg.Image != "" {
			resolvedImage = inlineCfg.Image
		}
	}

	// Determine harness resume behavior from the saved phase.
	// Suspended agents always get harness resume (--continue/--resume),
	// even when invoked via 'start' (implicit resume). Stopped agents
	// always get a fresh session, even when invoked via 'resume'.
	effectiveResume := resume
	savedPhase := agent.GetSavedPhase(agentName, projectPath)
	if savedPhase == string(state.PhaseSuspended) {
		effectiveResume = true
		if !resume {
			statusf("Resuming agent '%s'...\n", agentName)
		}
	} else if resume && savedPhase == string(state.PhaseStopped) {
		effectiveResume = false
	}

	opts := api.StartOptions{
		Name:          agentName,
		Task:          effectiveTask,
		Template:      templateName,
		Profile:       profile,
		HarnessConfig: effectiveHarnessConfig,
		HarnessAuth:   effectiveHarnessAuth,
		Image:         resolvedImage,
		ProjectPath:     projectPath,
		Resume:        effectiveResume,
		Detached:      detached,
		NoAuth:        noAuth,
		Branch:        effectiveBranch,
		Workspace:     workspace,
		InlineConfig:  inlineCfg,
	}

	// Apply telemetry override from CLI flags
	if enableTelemetry {
		val := true
		opts.TelemetryOverride = &val
	} else if disableTelemetry {
		val := false
		opts.TelemetryOverride = &val
	}

	// Propagate debug mode to container so sciontool logs debug info
	if debugMode {
		opts.Env = map[string]string{
			"SCION_DEBUG": "1",
		}
	}

	// Thread CLI-resolved hub endpoint so locally-started agents get
	// hub connectivity. The --hub flag and host SCION_HUB_ENDPOINT env
	// var are resolved here; the agent's scion-agent.yaml can override
	// inside Start() via hub.endpoint or env.SCION_HUB_ENDPOINT.
	if IsHubEnabled() {
		if cliSettings, err := config.LoadSettings(projectPath); err == nil {
			if !cliSettings.IsHubExplicitlyDisabled() {
				if ep := GetHubEndpoint(cliSettings); ep != "" {
					if opts.Env == nil {
						opts.Env = make(map[string]string)
					}
					opts.Env["SCION_HUB_ENDPOINT"] = ep
					opts.Env["SCION_HUB_URL"] = ep
				}
			}
		}
	}

	// Preview auth credentials in debug mode
	if debugMode && !noAuth {
		localAuth := harness.GatherAuth()
		util.Debugf("[auth] local credential preview:")
		util.Debugf("[auth]   hasGeminiAPIKey=%t, hasGoogleAPIKey=%t", localAuth.GeminiAPIKey != "", localAuth.GoogleAPIKey != "")
		util.Debugf("[auth]   hasAnthropicAPIKey=%t", localAuth.AnthropicAPIKey != "")
		util.Debugf("[auth]   hasOAuthCreds=%t (%s)", localAuth.OAuthCreds != "", localAuth.OAuthCreds)
		util.Debugf("[auth]   hasGoogleAppCredentials=%t", localAuth.GoogleAppCredentials != "")
		util.Debugf("[auth]   cloudProject=%q, cloudRegion=%q", localAuth.GoogleCloudProject, localAuth.GoogleCloudRegion)
	}

	// We still might want to show some progress in the CLI
	if resume {
		statusf("Resuming agent '%s'...\n", agentName)
	} else {
		statusf("Starting agent '%s'...\n", agentName)
	}

	info, err := mgr.Start(context.Background(), opts)
	if err != nil {
		return err
	}

	for _, w := range info.Warnings {
		fmt.Fprintln(os.Stderr, w)
	}

	if !info.Detached {
		statusf("Attaching to agent '%s'...\n", agentName)

		// Use the container ID for exec/attach operations. Container names are
		// now project-scoped (e.g. "scion--agent") but agentName is just the agent
		// name. The container ID is the reliable identifier for runtime operations.
		containerID := info.ContainerID
		if containerID == "" {
			containerID = agentName // fallback for runtimes that don't return an ID
		}

		// Wait for the container to be ready before attaching.
		// After container start, sciontool init needs time to set up the user,
		// run pre-start hooks, and launch the child process. The tmux session
		// must exist before we can attach.
		if err := waitForTmuxSession(rt, containerID); err != nil {
			return err
		}

		return rt.Attach(context.Background(), containerID)
	}

	displayStatus := "launched"
	if resume {
		displayStatus = "resumed"
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:   "success",
			Command:  cmd.Name(),
			Agent:    agentName,
			Message:  fmt.Sprintf("Agent '%s' %s successfully.", agentName, displayStatus),
			Warnings: info.Warnings,
			Details: map[string]interface{}{
				"id":       info.ID,
				"detached": info.Detached,
			},
		})
	}

	statusf("Agent '%s' %s successfully (ID: %s)\n", agentName, displayStatus, info.ID)

	return nil
}

// waitForTmuxSession polls the container until the tmux session "scion" is
// available. After starting a container, sciontool init needs time to
// synchronize UID/GID, run pre-start hooks, and launch the tmux session.
// Without this wait, an immediate attach would fail with "no sessions".
func waitForTmuxSession(rt runtime.Runtime, agentName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for tmux session in agent '%s' to become ready", agentName)
		case <-ticker.C:
			_, err := rt.Exec(ctx, agentName, []string{"tmux", "has-session", "-t", "scion"})
			if err == nil {
				return nil
			}
			util.Debugf("waiting for tmux session in '%s': %v", agentName, err)
		}
	}
}

func startAgentViaHub(hubCtx *HubContext, agentName, task string, resume bool, inlineCfg *api.ScionConfig) error {
	PrintUsingHub(hubCtx.Endpoint)

	// Get the project ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Check if this is a git-based project. When hub is enabled, all git-based
	// projects use clone-based provisioning (HTTPS + GitHub token) rather than
	// local worktrees. Inform the user and validate early.
	if !isJSONOutput() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		project, projectErr := hubCtx.Client.Projects().Get(ctx, projectID)
		cancel()
		if projectErr == nil && project != nil && project.GitRemote != "" {
			cloneURL := project.Labels["scion.dev/clone-url"]
			if cloneURL == "" {
				cloneURL = "https://" + project.GitRemote + ".git"
			}
			fmt.Fprintf(os.Stderr, "Using hub, cloning repo %s\n", cloneURL)
			fmt.Fprintf(os.Stderr, "  (Hub mode uses HTTPS clone with GITHUB_TOKEN; local worktrees are not used)\n")
		}
	}

	// Resolve template if specified (Section 9.4 - Local Template Resolution)
	var resolvedTemplate string
	if templateName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		result, err := ResolveTemplateForHub(ctx, hubCtx, templateName)
		if err != nil {
			return wrapHubError(fmt.Errorf("template resolution failed: %w", err))
		}

		// Use the template ID if available, otherwise fall back to name
		if result.TemplateID != "" {
			resolvedTemplate = result.TemplateID
		} else {
			resolvedTemplate = result.TemplateName
		}
	}

	// Build create request (Hub creates and starts in one operation)
	req := &hubclient.CreateAgentRequest{
		Name:            agentName,
		ProjectID:         projectID,
		Template:        resolvedTemplate,
		HarnessConfig:   harnessConfigFlag,
		HarnessAuth:     harnessAuthFlag,
		RuntimeBrokerID: runtimeBrokerID,
		Profile:         profile,
		Task:            task,
		Branch:          branch,
		Workspace:       workspace,
		Resume:          resume,
		Attach:          attach,
		GatherEnv:       true, // Enable env-gather flow
		Notify:          !startNoNotify,
	}

	// Thread inline config from --config flag into the Hub request.
	// The inline config is the base; CLI flags override specific fields.
	if inlineCfg != nil {
		req.Config = inlineCfg
		// CLI flags override inline config fields
		if agentImage != "" {
			req.Config.Image = agentImage
		}
	} else if agentImage != "" || debugMode || enableTelemetry || disableTelemetry {
		// Build config from CLI flags alone
		req.Config = &api.ScionConfig{
			Image: agentImage,
		}
	}

	// Add debug/telemetry env vars to config
	if req.Config != nil && (debugMode || enableTelemetry || disableTelemetry) {
		configEnv := req.Config.Env
		if configEnv == nil {
			configEnv = make(map[string]string)
		}
		if debugMode {
			configEnv["SCION_DEBUG"] = "1"
		}
		if enableTelemetry {
			configEnv["SCION_TELEMETRY_ENABLED"] = "true"
		} else if disableTelemetry {
			configEnv["SCION_TELEMETRY_ENABLED"] = "false"
		}
		if len(configEnv) > 0 {
			req.Config.Env = configEnv
		}
	}

	// Debug: log the env vars being sent with the create request
	if debugMode {
		util.Debugf("[env-gather] startAgentViaHub: building create request for agent %q", agentName)
		util.Debugf("[env-gather] startAgentViaHub: template=%q, broker=%q", resolvedTemplate, runtimeBrokerID)
		if req.Config != nil && len(req.Config.Env) > 0 {
			util.Debugf("[env-gather] startAgentViaHub: CLI is sending %d env var(s) in request config:", len(req.Config.Env))
			for k := range req.Config.Env {
				util.Debugf("[env-gather]   config env key: %s", k)
			}
		} else {
			util.Debugf("[env-gather] startAgentViaHub: no env vars in request config (Hub/Broker must supply all)")
		}
		util.Debugf("[env-gather] startAgentViaHub: GatherEnv=true — broker will evaluate env completeness; CLI may be asked to supply missing keys")

		// Preview auth credentials visible from the CLI host
		localAuth := harness.GatherAuth()
		util.Debugf("[auth] CLI-side credential preview (what the broker will see via env/secrets):")
		util.Debugf("[auth]   hasGeminiAPIKey=%t, hasGoogleAPIKey=%t", localAuth.GeminiAPIKey != "", localAuth.GoogleAPIKey != "")
		util.Debugf("[auth]   hasAnthropicAPIKey=%t", localAuth.AnthropicAPIKey != "")
		util.Debugf("[auth]   hasOAuthCreds=%t (%s)", localAuth.OAuthCreds != "", localAuth.OAuthCreds)
		util.Debugf("[auth]   hasGoogleAppCredentials=%t", localAuth.GoogleAppCredentials != "")
		util.Debugf("[auth]   cloudProject=%q, cloudRegion=%q", localAuth.GoogleCloudProject, localAuth.GoogleCloudRegion)
	}

	// Detect non-git project for workspace bootstrap
	var workspaceFiles []transfer.FileInfo
	if hubCtx.ProjectPath != "" && !hubCtx.IsGlobal {
		projectDir := filepath.Dir(hubCtx.ProjectPath) // parent of .scion
		if _, statErr := os.Stat(projectDir); statErr == nil && !util.IsGitRepoDir(projectDir) {
			files, err := transfer.CollectFiles(projectDir, transfer.DefaultExcludePatterns)
			if err != nil {
				return fmt.Errorf("failed to collect workspace files: %w", err)
			}
			if len(files) > 0 {
				req.WorkspaceFiles = files
				workspaceFiles = files
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check if the agent is suspended on the hub; if so, start implicitly
	// resumes the session. This check is best-effort — if it fails (agent
	// doesn't exist yet), we fall through to "Starting".
	if !resume {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		existing, getErr := hubCtx.Client.ProjectAgents(projectID).Get(checkCtx, agentName)
		checkCancel()
		if getErr == nil && existing != nil && existing.Phase == string(state.PhaseSuspended) {
			resume = true
			req.Resume = true
		}
	}

	if !isJSONOutput() {
		action := "Starting"
		if resume {
			action = "Resuming"
		}
		fmt.Printf("%s agent '%s'...\n", action, agentName)
	}

	resp, err := createAgentWithBrokerResolution(ctx, hubCtx, projectID, req)
	if err != nil {
		if debugMode {
			util.Debugf("[env-gather] startAgentViaHub: create request failed: %v", err)
		}
		return wrapHubError(fmt.Errorf("failed to start agent via Hub: %w", err))
	}

	// Debug: log the response from Hub
	if debugMode {
		util.Debugf("[env-gather] startAgentViaHub: Hub returned successfully")
		if resp.Agent != nil {
			util.Debugf("[env-gather]   agent id=%s slug=%s phase/status=%s", resp.Agent.ID, resp.Agent.Slug, resp.Agent.Status)
			if resp.Agent.RuntimeBrokerName != "" {
				util.Debugf("[env-gather]   broker=%s (id=%s)", resp.Agent.RuntimeBrokerName, resp.Agent.RuntimeBrokerID)
			}
		}
		if len(resp.Warnings) > 0 {
			util.Debugf("[env-gather]   warnings from Hub: %v", resp.Warnings)
		}
	}

	// Handle env-gather: if the Hub returned env requirements, gather from local env and submit
	if resp.EnvGather != nil {
		if debugMode {
			util.Debugf("[env-gather] startAgentViaHub: Hub returned 202 with env requirements")
			util.Debugf("[env-gather]   required: %v", resp.EnvGather.Required)
			util.Debugf("[env-gather]   needs: %v", resp.EnvGather.Needs)
		}

		// Resolve agent ID for cleanup in case env-gather is aborted.
		envGatherAgentID := ""
		if resp.Agent != nil {
			envGatherAgentID = resp.Agent.ID
		}
		if envGatherAgentID == "" && resp.EnvGather != nil {
			envGatherAgentID = resp.EnvGather.AgentID
		}

		// cleanupProvisioningAgent removes a partially-created agent from the Hub.
		cleanupProvisioningAgent := func() {
			if envGatherAgentID == "" {
				return
			}
			delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer delCancel()
			delErr := hubCtx.Client.ProjectAgents(projectID).Delete(delCtx, envGatherAgentID, &hubclient.DeleteAgentOptions{
				DeleteFiles: true,
			})
			if delErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to clean up provisioning agent %s: %v\n", envGatherAgentID, delErr)
			} else if debugMode {
				util.Debugf("[env-gather] cleaned up provisioning agent %s after env-gather failure", envGatherAgentID)
			}
		}

		// Install a signal handler so Ctrl+C during interactive prompting
		// still cleans up the provisioning agent instead of leaving it orphaned.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		// Watch for interrupt in the background; clean up and exit if triggered.
		envGatherDone := make(chan struct{})
		go func() {
			select {
			case <-sigCh:
				fmt.Fprintln(os.Stderr, "\nInterrupted. Cleaning up provisioning agent...")
				cleanupProvisioningAgent()
				os.Exit(130) // 128 + SIGINT(2)
			case <-envGatherDone:
				// Normal completion — goroutine exits.
			}
		}()

		submitResp, err := gatherAndSubmitEnv(ctx, hubCtx, projectID, resp)

		// Stop the signal handler and goroutine now that env-gather is done.
		signal.Stop(sigCh)
		close(envGatherDone)

		if err != nil {
			cleanupProvisioningAgent()
			return fmt.Errorf("env-gather failed: %w", err)
		}
		// Replace response with the finalized one
		resp = submitResp
	}

	// Advance watermark to the hub-assigned creation time so this agent
	// won't trigger a sync warning on the next 'scion ls'.
	if resp.Agent != nil && !resp.Agent.Created.IsZero() {
		hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, resp.Agent.Created, hubCtx.IsGlobal)
		hubsync.AddSyncedAgent(hubCtx.ProjectPath, agentName)
	}

	// Print info line when broker was auto-resolved (not explicitly specified)
	if !isJSONOutput() {
		printAutoResolvedBroker(ctx, hubCtx, runtimeBrokerID, req.RuntimeBrokerID, resp)
	}

	// Workspace bootstrap: upload files and finalize
	if len(workspaceFiles) > 0 && len(resp.UploadURLs) == 0 {
		statusln("Using local workspace on broker.")
	}
	if len(resp.UploadURLs) > 0 && len(workspaceFiles) > 0 {
		statusf("Uploading workspace (%d files)...\n", len(workspaceFiles))
		tc := transfer.NewClient(nil)
		uploadErr := tc.UploadFiles(ctx, workspaceFiles, resp.UploadURLs, func(file transfer.FileInfo, bytesTransferred int64) error {
			if bytesTransferred == file.Size {
				statusf("  Uploaded: %s\n", file.Path)
			}
			return nil
		})
		if uploadErr != nil {
			return fmt.Errorf("failed to upload workspace files: %w", uploadErr)
		}

		// Finalize: triggers broker dispatch
		manifest := transfer.BuildManifest(workspaceFiles)
		agentSlug := agentName
		if resp.Agent != nil && resp.Agent.Slug != "" {
			agentSlug = resp.Agent.Slug
		}

		finalizeResp, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentSlug, manifest)
		if err != nil {
			return fmt.Errorf("failed to finalize workspace bootstrap: %w", err)
		}
		statusf("Workspace uploaded: %d files\n", finalizeResp.FilesApplied)

		// Poll until agent is running
		statusf("Waiting for agent '%s' to start...\n", agentName)
		pollCtx, pollCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer pollCancel()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-pollCtx.Done():
				return fmt.Errorf("timed out waiting for agent '%s' to start", agentName)
			case <-ticker.C:
				agent, err := hubCtx.Client.ProjectAgents(projectID).Get(pollCtx, agentName)
				if err != nil {
					continue
				}
				agentPhase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
				if agentPhase == string(state.PhaseRunning) {
					statusf("Agent '%s' started via Hub.\n", agentName)
					if !attach {
						return nil
					}
					// Fall through to attach logic below
					agentID := agent.ID
					if agentID == "" {
						agentID = agentName
					}
					token := getHubAccessToken(hubCtx.Endpoint)
					if token == "" {
						return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
					}
					statusf("Attaching to agent '%s' via Hub...\n", agentName)
					return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID)
				}
				if agentPhase == string(state.PhaseError) || agentPhase == string(state.PhaseStopped) {
					return fmt.Errorf("agent '%s' failed to start (phase: %s)", agentName, agentPhase)
				}
			}
		}
	}

	displayStatus := "started"
	if resume {
		displayStatus = "resumed"
	}

	if isJSONOutput() {
		result := ActionResult{
			Status:   "success",
			Command:  "start",
			Agent:    agentName,
			Message:  fmt.Sprintf("Agent '%s' %s via Hub.", agentName, displayStatus),
			Warnings: resp.Warnings,
			Details:  map[string]interface{}{},
		}
		if resp.Agent != nil {
			result.Details["slug"] = resp.Agent.Slug
			phase, activity := hubAgentPhaseActivity(resp.Agent.Phase, resp.Agent.Activity, resp.Agent.Status)
			result.Details["phase"] = phase
			if activity != "" {
				result.Details["activity"] = activity
			}
			if resp.Agent.RuntimeBrokerID != "" {
				result.Details["runtimeBrokerId"] = resp.Agent.RuntimeBrokerID
			}
		}
		return outputJSON(result)
	}

	statusf("Agent '%s' %s via Hub.\n", agentName, displayStatus)
	if resp.Agent != nil {
		statusf("Agent Slug: %s\n", resp.Agent.Slug)
		phase, _ := hubAgentPhaseActivity(resp.Agent.Phase, resp.Agent.Activity, resp.Agent.Status)
		statusf("Phase: %s\n", phase)
	}
	for _, w := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}

	if !attach {
		return nil
	}

	// Attach mode: wait for agent to be running, then attach via WebSocket
	agentID := ""
	if resp.Agent != nil {
		agentID = resp.Agent.ID
	}
	if agentID == "" {
		agentID = agentName
	}

	// Poll until the agent is running
	statusf("Waiting for agent '%s' to be ready...\n", agentName)
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer pollCancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("timed out waiting for agent '%s' to become ready", agentName)
		case <-ticker.C:
			agent, err := hubCtx.Client.ProjectAgents(projectID).Get(pollCtx, agentName)
			if err != nil {
				continue // Retry on transient errors
			}
			agentPhase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
			if agentPhase == string(state.PhaseRunning) {
				// Use the agent's ID from the latest fetch
				if agent.ID != "" {
					agentID = agent.ID
				}
				goto ready
			}
			if agentPhase == string(state.PhaseError) || agentPhase == string(state.PhaseStopped) {
				statusInfo := agent.Status
				if agent.ContainerStatus != "" {
					statusInfo += fmt.Sprintf(", container: %s", agent.ContainerStatus)
				}
				return fmt.Errorf("agent '%s' failed to start (phase: %s)", agentName, statusInfo)
			}
		}
	}

ready:
	// Get access token for WebSocket authentication
	token := getHubAccessToken(hubCtx.Endpoint)
	if token == "" {
		return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
	}

	statusf("Attaching to agent '%s' via Hub...\n", agentName)
	return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID)
}

func createAgentWithBrokerResolution(ctx context.Context, hubCtx *HubContext, projectID string, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	for {
		if debugMode {
			util.Debugf("[env-gather] createAgentWithBrokerResolution: sending create request to Hub (project=%s, agent=%s, broker=%s)", projectID, req.Name, req.RuntimeBrokerID)
			if req.Config != nil && len(req.Config.Env) > 0 {
				for k := range req.Config.Env {
					util.Debugf("[env-gather]   request env key: %s", k)
				}
			}
		}

		resp, err := hubCtx.Client.ProjectAgents(projectID).Create(ctx, req)
		if err == nil {
			if debugMode {
				if resp.EnvGather != nil {
					util.Debugf("[env-gather] createAgentWithBrokerResolution: Hub returned 202 with env-gather requirements")
					util.Debugf("[env-gather]   required: %v", resp.EnvGather.Required)
					util.Debugf("[env-gather]   hubHas: %d keys", len(resp.EnvGather.HubHas))
					util.Debugf("[env-gather]   needs: %v", resp.EnvGather.Needs)
				} else {
					util.Debugf("[env-gather] createAgentWithBrokerResolution: Hub returned success — all env satisfied (no gather needed)")
				}
			}
			return resp, nil
		}

		var apiErr *apiclient.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "no_runtime_broker" {
			if debugMode {
				util.Debugf("[env-gather] createAgentWithBrokerResolution: Hub returned error: %v", err)
			}
			return nil, err
		}

		// Handle ambiguous broker
		availableBrokers, ok := apiErr.Details["availableBrokers"].([]interface{})
		if !ok || len(availableBrokers) == 0 {
			return nil, err
		}

		// Only prompt if interactive and not auto-confirm
		if autoConfirm || !util.IsTerminal() {
			return nil, fmt.Errorf("multiple runtime brokers available, specify a broker with --broker <id>")
		}

		reader := bufio.NewReader(os.Stdin)

		if len(availableBrokers) == 1 {
			// Single broker available - simple confirmation
			brokerMap, _ := availableBrokers[0].(map[string]interface{})
			name, _ := brokerMap["name"].(string)
			status, _ := brokerMap["status"].(string)
			isDefault, _ := brokerMap["isDefault"].(bool)

			defaultLabel := ""
			if isDefault {
				defaultLabel = " (default)"
			}
			fmt.Printf("\nUse runtime broker %s (%s)%s? [y/N]: ", name, status, defaultLabel)
			input, err := reader.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("failed to read input: %w", err)
			}
			input = strings.TrimSpace(strings.ToLower(input))
			if input != "y" && input != "yes" {
				return nil, fmt.Errorf("operation cancelled")
			}
			req.RuntimeBrokerID, _ = brokerMap["id"].(string)
		} else {
			// Multiple brokers - selection prompt
			fmt.Printf("\nMultiple runtime brokers available for project:\n")
			for i, h := range availableBrokers {
				brokerMap, _ := h.(map[string]interface{})
				name, _ := brokerMap["name"].(string)
				status, _ := brokerMap["status"].(string)
				isDefault, _ := brokerMap["isDefault"].(bool)
				defaultLabel := ""
				if isDefault {
					defaultLabel = " (default)"
				}
				fmt.Printf("  [%d] %s (%s)%s\n", i+1, name, status, defaultLabel)
			}
			fmt.Println()

			for {
				fmt.Print("Select a broker (or 'c' to cancel): ")
				input, err := reader.ReadString('\n')
				if err != nil {
					return nil, fmt.Errorf("failed to read input: %w", err)
				}

				input = strings.TrimSpace(strings.ToLower(input))
				if input == "c" || input == "cancel" {
					return nil, fmt.Errorf("operation cancelled")
				}

				var choice int
				if _, err := fmt.Sscanf(input, "%d", &choice); err != nil || choice < 1 || choice > len(availableBrokers) {
					fmt.Printf("Invalid choice. Please enter 1-%d.\n", len(availableBrokers))
					continue
				}

				selectedBroker, _ := availableBrokers[choice-1].(map[string]interface{})
				req.RuntimeBrokerID, _ = selectedBroker["id"].(string)
				break
			}
		}
		// Loop and retry with selected broker
	}
}

// gatherAndSubmitEnv handles the env-gather flow: checks the local environment
// for missing keys and submits gathered values back to the Hub.
func gatherAndSubmitEnv(ctx context.Context, hubCtx *HubContext, projectID string, resp *hubclient.CreateAgentResponse) (*hubclient.CreateAgentResponse, error) {
	gather := resp.EnvGather
	agentID := gather.AgentID
	if agentID == "" && resp.Agent != nil {
		agentID = resp.Agent.ID
	}

	// Display Hub warnings about resolution mismatches
	if len(gather.HubWarnings) > 0 {
		for _, w := range gather.HubWarnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
		}
	}

	// Print summary of env status
	if len(gather.HubHas) > 0 {
		statusln("\nEnvironment variables already provided:")
		for _, src := range gather.HubHas {
			statusf("  %s — provided by Hub (%s)\n", src.Key, src.Scope)
		}
	}
	// Try to satisfy needed keys from local environment
	gatheredEnv := make(map[string]string)
	var missingKeys []string
	for _, key := range gather.Needs {
		if val := os.Getenv(key); val != "" {
			gatheredEnv[key] = val
		} else {
			missingKeys = append(missingKeys, key)
		}
	}

	if len(gatheredEnv) > 0 {
		statusln("\nRequired variables for this agent were not provided in the template, hub, or broker, but are available in our current environment:")
		for key := range gatheredEnv {
			statusf("  %s — found in local environment\n", key)
		}
	}

	// Categorize missing keys into promptable secrets, file secrets, and env-only
	var secretEligible []string // can prompt interactively
	var fileSecrets []string    // file-type secrets, guide to scion hub secret set --type file
	var envOnly []string        // not secret-eligible, must fail
	for _, key := range missingKeys {
		if gather.SecretInfo != nil {
			if info, ok := gather.SecretInfo[key]; ok {
				if info.Type == "file" {
					fileSecrets = append(fileSecrets, key)
				} else {
					secretEligible = append(secretEligible, key)
				}
				continue
			}
		}
		envOnly = append(envOnly, key)
	}

	// Interactive prompting for secret-eligible keys
	var promptedCount int
	if len(secretEligible) > 0 && isInteractiveTerminal() && !nonInteractive {
		statusln("\nThe following secrets are required and not in the local environment.")
		statusln("You can enter them now (input is hidden):")
		for _, key := range secretEligible {
			if gather.SecretInfo != nil {
				if info, ok := gather.SecretInfo[key]; ok && info.Description != "" {
					statusf("  %s: %s\n", key, info.Description)
				}
			}
			fmt.Fprintf(os.Stderr, "  Enter value for %s: ", key)
			secret, err := readSecretFunc(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr) // newline after hidden input
			if err != nil {
				return nil, fmt.Errorf("failed to read secret for %s: %w", key, err)
			}
			val := strings.TrimSpace(string(secret))
			if val == "" {
				// Empty input — will be handled below as unsatisfied
				continue
			}
			gatheredEnv[key] = val
			promptedCount++
		}
	}

	// Determine which keys remain unsatisfied after prompting
	var unsatisfied []string
	for _, key := range secretEligible {
		if _, ok := gatheredEnv[key]; !ok {
			unsatisfied = append(unsatisfied, key)
		}
	}

	// File-type secrets cannot be entered interactively
	if len(fileSecrets) > 0 && !isJSONOutput() {
		fmt.Fprintln(os.Stderr)
		for _, key := range fileSecrets {
			desc := ""
			if gather.SecretInfo != nil {
				if info, ok := gather.SecretInfo[key]; ok && info.Description != "" {
					desc = ": " + info.Description
				}
			}
			fmt.Fprintf(os.Stderr, "  %s — file-type secret, cannot be entered interactively%s\n", key, desc)
			fmt.Fprintf(os.Stderr, "    scion hub secret set --type file %s @./path/to/file\n", key)
		}
	}

	// Fail if there are any unsatisfied keys
	allUnsatisfied := append(append(envOnly, fileSecrets...), unsatisfied...)
	if len(allUnsatisfied) > 0 {
		if !isJSONOutput() {
			if len(envOnly) > 0 {
				fmt.Fprintln(os.Stderr)
				for _, key := range envOnly {
					fmt.Fprintf(os.Stderr, "  %s — MISSING (not in local environment)\n", key)
				}
			}
			if len(unsatisfied) > 0 {
				fmt.Fprintln(os.Stderr)
				for _, key := range unsatisfied {
					fmt.Fprintf(os.Stderr, "  %s — MISSING (secret not entered)\n", key)
				}
			}
			fmt.Fprintf(os.Stderr, "\nTo set missing variables on the Hub, use:\n")
			for _, key := range allUnsatisfied {
				if gather.SecretInfo != nil {
					if info, ok := gather.SecretInfo[key]; ok {
						if info.Type == "file" {
							fmt.Fprintf(os.Stderr, "  scion hub secret set --type file %s @./path/to/file\n", key)
						} else {
							fmt.Fprintf(os.Stderr, "  scion hub secret set %s <value>\n", key)
						}
						continue
					}
				}
				fmt.Fprintf(os.Stderr, "  scion hub env set %s <value>\n", key)
			}
			fmt.Fprintln(os.Stderr)
		}
		return nil, fmt.Errorf("cannot satisfy required environment variables: %v", allUnsatisfied)
	}

	if len(gatheredEnv) == 0 {
		// All keys were already satisfied by Hub/Broker — should not reach here
		return resp, nil
	}

	// In interactive mode, confirm before sending env vars
	if isInteractiveTerminal() && !autoConfirm {
		reader := bufio.NewReader(os.Stdin)
		fmt.Fprintf(os.Stderr, "\nSend %d environment variable(s) to use in provisioning this agent? (will not be stored) [Y/n]: ", len(gatheredEnv))
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read input: %w", err)
		}
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "n" || input == "no" {
			return nil, fmt.Errorf("env-gather cancelled by user")
		}
	}

	statusf("Submitting %d gathered environment variable(s)...\n", len(gatheredEnv))

	// Submit gathered env to Hub
	submitResp, err := hubCtx.Client.ProjectAgents(projectID).SubmitEnv(ctx, agentID, &hubclient.SubmitEnvRequest{
		Env: gatheredEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to submit gathered env: %w", err)
	}

	if debugMode {
		util.Debugf("[env-gather] gatherAndSubmitEnv: submitted %d env var(s), agent should now start", len(gatheredEnv))
	}

	// Show persistence tip if any secrets were entered interactively
	if promptedCount > 0 {
		statusln("\nTip: To avoid entering secrets each time, store them permanently:")
		for _, key := range secretEligible {
			if _, ok := gatheredEnv[key]; ok {
				statusf("  scion hub secret set %s <value>\n", key)
			}
		}
	}

	return submitResp, nil
}

// printAutoResolvedBroker prints an info line when the broker was auto-resolved
// (i.e., the user didn't explicitly pass --broker and didn't interactively select one).
// This covers the "default broker" and "single provider" auto-selection cases.
func printAutoResolvedBroker(ctx context.Context, hubCtx *HubContext, flagBrokerID, reqBrokerID string, resp *hubclient.CreateAgentResponse) {
	// Only print when the broker was auto-resolved (not explicitly specified or interactively selected)
	if flagBrokerID != "" || reqBrokerID != "" {
		return
	}
	if resp == nil || resp.Agent == nil || resp.Agent.RuntimeBrokerID == "" {
		return
	}

	brokerName := resp.Agent.RuntimeBrokerName
	if brokerName == "" {
		// Fallback: fetch broker name from Hub
		if broker, err := hubCtx.Client.RuntimeBrokers().Get(ctx, resp.Agent.RuntimeBrokerID); err == nil {
			brokerName = broker.Name
		}
	}
	if brokerName != "" {
		statusf("Using default broker %s\n", brokerName)
	} else {
		statusf("Using default broker %s\n", resp.Agent.RuntimeBrokerID)
	}
}
