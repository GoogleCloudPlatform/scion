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
	"slices"
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
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	forceResume           bool
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
	labelFlags            []string
	modelFlag             string
	thinkingLevelFlag     int = -1
	agentRoleFlag         string
	messageModeFlag       string
	serviceAccountFlag    string
)

func validateMessageMode(mode string) error {
	if mode == "" {
		return nil
	}
	switch mode {
	case "none", "lineage", "branch", "project", "hub":
		return nil
	default:
		return fmt.Errorf("invalid message mode %q: must be one of none, lineage, branch, project, hub", mode)
	}
}

func validateAgentRole(role string) error {
	if role == "" {
		return nil
	}
	switch role {
	case "none", "readonly", "baseline", "full":
		return nil
	default:
		return fmt.Errorf("invalid role %q: must be one of none, readonly, baseline, full", role)
	}
}

func parseLabels(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(raw))
	for _, s := range raw {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label %q: must be key=value", s)
		}
		m[k] = v
	}
	if err := labels.Validate(m); err != nil {
		return nil, fmt.Errorf("invalid label: %w", err)
	}
	return m, nil
}

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
	// CredentialKind mirrors hubsync.HubContext.CredentialKind — see its doc
	// for what it records and why (ptone/scion#2146). Zero value
	// (hubsync.CredentialKindUnknown) on any HubContext not built via
	// CheckHubAvailability* (e.g. a test double).
	CredentialKind hubsync.CredentialKind
}

// getHubAccessToken returns an access token for authenticating to the Hub over
// WebSocket (attach, message streaming). It checks OAuth login credentials
// first, then a SCION_HUB_TOKEN bearer token, then the dev-auth token —
// mirroring the *bearer-token* order the REST hub client uses (getHubClient in
// hub.go). The REST path's agent-token sources (scion-token file,
// SCION_AUTH_TOKEN) are intentionally omitted here: an agent identity token
// cannot authenticate a human attach.
func getHubAccessToken(endpoint string) string {
	// Priority 1: OAuth credentials from scion hub auth login
	if token := credentials.GetAccessToken(endpoint); token != "" {
		return token
	}

	// Priority 2: SCION_HUB_TOKEN env — a user access token (scion_pat_…) or
	// bootstrap bearer token. The REST hub client already honors this; the
	// WebSocket auth paths (attach, message streaming) must too, or a
	// dev-auth-off hub driven purely by SCION_HUB_TOKEN answers REST calls but
	// rejects every attach with "no access token found for Hub".
	if token := os.Getenv("SCION_HUB_TOKEN"); token != "" {
		return token
	}

	// Priority 3: Dev auth token
	return apiclient.ResolveDevToken()
}

// explicitProjectTarget reports whether the user named the project with the
// --project / -g or --global flag. It reads the flag variables, not the
// path a caller passes on, so a caller that resolved the cwd project itself
// is not treated as explicit (ptone/scion#3123).
func explicitProjectTarget() bool {
	return projectPath != "" || globalMode
}

// explicitProjectTargetFor reports whether path is a project the user named
// with a flag. An empty path is never explicit: a caller that clears the
// path (a cross-project message send) resolves its own project from the
// environment.
func explicitProjectTargetFor(path string) bool {
	return path != "" && explicitProjectTarget()
}

// loadSettingsForTarget loads settings for resolvedPath. When the user named
// the project with a flag, SCION_PROJECT_ID in the environment does not
// override that project's own ID (ptone/scion#3123).
func loadSettingsForTarget(resolvedPath string) (*config.Settings, error) {
	if explicitProjectTarget() {
		return config.LoadSettingsIgnoringEnvProjectID(resolvedPath)
	}
	return config.LoadSettings(resolvedPath)
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
		ExplicitProject:  explicitProjectTargetFor(projectPath),
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
		Client:         hubCtx.Client,
		Endpoint:       hubCtx.Endpoint,
		Settings:       hubCtx.Settings,
		ProjectID:      hubCtx.ProjectID,
		BrokerID:       hubCtx.BrokerID,
		ProjectPath:    hubCtx.ProjectPath,
		IsGlobal:       hubCtx.IsGlobal,
		CredentialKind: hubCtx.CredentialKind,
	}, nil
}

// detectCrossProjectTarget reports the target project when the caller is an
// agent that set --project to a DIFFERENT project than its own. Returns ""
// when the caller is not an agent, --project was not set, or --project
// names the agent's own project — i.e. whenever the send is same-project.
//
// Shared by `scion message` (which routes a non-empty result through
// sendCrossProjectMessage) and `scion keys` (which refuses a non-empty
// result outright — keys must not work as a cross-project command).
func detectCrossProjectTarget(cmd *cobra.Command) string {
	if os.Getenv("SCION_AGENT_NAME") == "" || !cmd.Flags().Changed("project") {
		return ""
	}
	ownProjectSlug := os.Getenv("SCION_PROJECT")
	ownProjectID := os.Getenv("SCION_PROJECT_ID")
	isSameProject := (ownProjectSlug != "" && projectPath == ownProjectSlug) ||
		(ownProjectID != "" && projectPath == ownProjectID)
	if isSameProject {
		return ""
	}
	return projectPath
}

// CheckAgentsGitignore verifies that .scion/agents/ is listed in .gitignore
// when running inside a git repo with a project-local project directory.
// This runs once before any agent provisioning so the user gets a single
// clear error instead of one per agent.
func CheckAgentsGitignore(projectPath string) error {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return nil
	}

	if !util.IsGitRepoDir(projectDir) {
		return nil
	}

	if os.Getenv("SCION_HOST_UID") != "" {
		return nil
	}

	root, err := util.RepoRootDir(projectDir)
	if err != nil {
		return nil
	}

	rel, err := filepath.Rel(root, projectDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}

	agentsPath := filepath.ToSlash(filepath.Join(rel, "agents"))
	if !util.IsIgnored(root, agentsPath+"/") {
		return fmt.Errorf("security error: '%s/' must be in .gitignore when using a project-local project.\n\nRun 'scion init' to set up the project, or manually add '%s/' to your .gitignore", agentsPath, agentsPath)
	}

	return nil
}

// PrintUsingHub prints the informational message about using the Hub to stderr.
// It is suppressed entirely in JSON output mode to keep stdout clean.
func PrintUsingHub(endpoint string) {
	if isJSONOutput() {
		return
	}
	fmt.Fprintf(os.Stderr, "Using hub: %s\n", endpoint)
}

// localOnlyHint is appended to hub errors where switching to local-only mode
// is a plausible remedy (the hub is unreachable or failing).
const localOnlyHint = "\n\nTo use local-only mode, run: scion hub disable"

// hubError marks an error that has been through wrapHubError. Error() is the
// fully rendered message (including any hint); Unwrap exposes the original
// cause so callers can still use errors.Is / errors.As on it. Execute uses the
// marker to suppress the Usage block for hub failures, which are runtime
// errors rather than usage errors.
type hubError struct {
	msg string
	err error
}

func (e *hubError) Error() string { return e.msg }
func (e *hubError) Unwrap() error { return e.err }

// isHubFailure reports whether err came from talking to the Hub: it went
// through wrapHubError, it is a Hub launch outcome (launchFailedError,
// launchWaitTimeoutError, launchWaitInterruptedError), or it wraps a
// structured *apiclient.APIError.
func isHubFailure(err error) bool {
	var he *hubError
	if errors.As(err, &he) {
		return true
	}
	// Launch outcomes reported by the Hub (an incomplete or failed create,
	// a wait that timed out or was interrupted) are runtime results, not
	// usage errors, even though they do not wrap an APIError.
	var lf *launchFailedError
	var lt *launchWaitTimeoutError
	var li *launchWaitInterruptedError
	if errors.As(err, &lf) || errors.As(err, &lt) || errors.As(err, &li) {
		return true
	}
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr)
}

// wrapHubError annotates an error from a Hub operation with user guidance.
//
//   - 401: replaced with a "login with scion hub auth login" hint (or, inside a
//     hub-managed agent, a credentials-rejected message that keeps the cause).
//   - Connectivity failures (anything that is not an *apiclient.APIError, e.g.
//     a transport error or timeout) and 5xx responses: the "scion hub disable"
//     local-only hint is appended, since the hub being down is the case where
//     falling back to local mode can help.
//   - Other API errors (4xx such as 400/403/404/409/422): returned as-is. The
//     hub answered and the message is about the request, so suggesting that
//     the user disable the hub would be misleading noise.
//
// Inside a hub-managed agent the local-only hint is never added, because
// disabling the Hub would break orchestration connectivity.
func wrapHubError(err error) error {
	if err == nil {
		return nil
	}
	var already *hubError
	if errors.As(err, &already) {
		// Already annotated further down the call chain; don't add a
		// second hint.
		return err
	}
	if apiclient.IsUnauthorizedError(err) {
		// `scion hub` is filtered out of the command tree in agent mode, so a
		// hub-managed agent cannot run `scion hub auth login` and must not be
		// told to. Keep the underlying error instead of discarding it.
		if config.IsHubManagedAgent() {
			return &hubError{msg: "hub rejected this agent's credentials: " + err.Error(), err: err}
		}
		return &hubError{msg: "authentication failed, login to hub with 'scion hub auth login'", err: err}
	}
	if config.IsHubManagedAgent() || !shouldSuggestLocalOnly(err) {
		return &hubError{msg: err.Error(), err: err}
	}
	return &hubError{msg: err.Error() + localOnlyHint, err: err}
}

// shouldSuggestLocalOnly reports whether the local-only hint is relevant for
// err: true for connectivity failures (no structured API response) and 5xx.
func shouldSuggestLocalOnly(err error) bool {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.IsServerError()
}

// GetProjectID looks up the project ID from HubContext or settings.
// Priority:
//  1. ProjectID field in HubContext (set by EnsureHubReady)
//  2. Local project_id from settings (for non-git projects or explicit configuration)
//  3. Git remote lookup via Hub API
//
// Returns the project ID if found, or an error if the project is not registered.
//
// When several projects share the same git remote, this silently picks the
// first one (resp.Projects[0]) — a pre-existing, deliberately unchanged
// behavior for every caller of this function except the keys command (see
// getProjectIDForKeys below: ambiguous resolution must fail rather than
// guess for the keys path specifically). Widening that fail-closed behavior
// to every command here would be a much larger, separately-scoped change.
func GetProjectID(hubCtx *HubContext) (string, error) {
	return resolveProjectIDByGitRemote(hubCtx, false)
}

// getProjectIDForKeys is GetProjectID's keys-path variant, scoped to the
// keys CLI path only (ptone/scion#2200 contract C§3: "ambiguous resolution
// fails rather than guessing"). It shares every step of GetProjectID's resolution order
// except the final git-remote-ambiguity case, where it fails closed with a
// message naming --project instead of silently picking the first match.
func getProjectIDForKeys(hubCtx *HubContext) (string, error) {
	return resolveProjectIDByGitRemote(hubCtx, true)
}

// resolveProjectIDByGitRemote implements GetProjectID's resolution order.
// failOnAmbiguousGitRemote selects between the two callers' divergent
// behavior for exactly one case: more than one project sharing the queried
// git remote. Every other branch (context/settings short-circuit, missing
// remote, zero matches) is identical for both callers.
func resolveProjectIDByGitRemote(hubCtx *HubContext, failOnAmbiguousGitRemote bool) (string, error) {
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
	if gitRemote == "" && hubCtx.IsGlobal {
		// Falling back to the local global directory with no project ID
		// (ptone/scion#3124): say how to reach a hub project instead of
		// pointing at a git remote the global directory never has.
		return "", errors.New("the local global project is not linked to a hub project.\n\n" +
			"Link it with 'scion hub link', or pass --project <slug|id> to target a hub project")
	}
	if gitRemote == "" {
		msg := "no git origin remote found for this project.\n\nThe Hub uses the origin remote URL to identify projects.\nRun 'scion hub link' to link this project with the Hub"
		if !config.IsHubManagedAgent() {
			msg += ", or use --no-hub for local-only mode"
		}
		return "", errors.New(msg)
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

	if failOnAmbiguousGitRemote && len(resp.Projects) > 1 {
		return "", fmt.Errorf("multiple projects match git remote %s; pass --project to disambiguate", gitRemote)
	}

	// Return the first matching project
	return resp.Projects[0].ID, nil
}

// localResumeDecision mirrors the Hub's resumeInPlaceDecision
// (pkg/hub/handlers_agent_create_helpers.go) for local (non-Hub) mode, so the
// two paths present the same contract to the operator.
//
// Rules:
//   - Suspended agents always get harness resume (--continue/--resume), even
//     when invoked via 'start' (implicit resume).
//   - Stopped agents always get a fresh session, even when invoked via
//     'resume'. --force does not change this: a clean shutdown is not a crash.
//   - Error-phase agents are refused without --force, matching the Hub's 409.
//     With --force they continue the session interrupted mid-run.
//
// Returns the resume flag to hand the harness, plus whether this was an
// implicit resume or a forced recovery (both purely for operator messaging).
func localResumeDecision(savedPhase string, resume, force bool, agentName string) (effectiveResume, implicitResume, forcedRecovery bool, err error) {
	if resume && savedPhase == string(state.PhaseError) && !force {
		return false, false, false, fmt.Errorf("agent '%s' is in the error phase (e.g. after a host crash) and cannot be resumed without --force", agentName)
	}

	switch {
	case savedPhase == string(state.PhaseSuspended):
		return true, !resume, false, nil
	case resume && force && savedPhase == string(state.PhaseError):
		return true, false, true, nil
	case resume && savedPhase == string(state.PhaseStopped):
		return false, false, false, nil
	}
	return resume, false, false, nil
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

	if err := validateLaunchWaitFlags(); err != nil {
		return err
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

	// Pre-flight: verify .scion/agents/ is gitignored (once, before any provisioning).
	if err := CheckAgentsGitignore(projectPath); err != nil {
		return err
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

	if modelFlag != "" {
		normalizedModel := config.NormalizeModelAlias(modelFlag)
		if inlineCfg == nil {
			inlineCfg = &api.ScionConfig{}
		}
		inlineCfg.Model = normalizedModel
	}
	if thinkingLevelFlag != -1 {
		if thinkingLevelFlag < 0 || thinkingLevelFlag > 100 {
			return fmt.Errorf("invalid --thinking-level value %d: must be between 0 and 100", thinkingLevelFlag)
		}
		if inlineCfg == nil {
			inlineCfg = &api.ScionConfig{}
		}
		val := thinkingLevelFlag
		inlineCfg.ThinkingLevel = &val
	}

	// This allows starting/resuming an agent even if it exists on Hub but not locally
	// (will be created via Hub) or if other agents are out of sync.
	hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
	if err != nil {
		return err
	}

	if hubCtx != nil {
		return startAgentViaHub(cmd, hubCtx, agentName, task, resume, inlineCfg)
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
					isRunning := a.Phase == string(state.PhaseRunning)
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
	savedPhase := agent.GetSavedPhase(agentName, projectPath)
	effectiveResume, implicitResume, forcedRecovery, err := localResumeDecision(savedPhase, resume, forceResume, agentName)
	if err != nil {
		return err
	}
	if implicitResume {
		statusf("Resuming agent '%s'...\n", agentName)
	}
	if forcedRecovery {
		statusf("Force-resuming agent '%s' (saved phase: %s)...\n", agentName, savedPhase)
	}

	opts := api.StartOptions{
		Name:          agentName,
		Task:          effectiveTask,
		Template:      templateName,
		Profile:       profile,
		HarnessConfig: effectiveHarnessConfig,
		HarnessAuth:   effectiveHarnessAuth,
		Image:         resolvedImage,
		ProjectPath:   projectPath,
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
		util.Debugf("[auth]   hasGoogleAppCredentials=%t", localAuth.GoogleAppCredentials != "")
		util.Debugf("[auth]   cloudProject=%q, cloudRegion=%q", localAuth.GoogleCloudProject, localAuth.GoogleCloudRegion)
		util.Debugf("[auth]   envVarCount=%d, fileCount=%d", len(localAuth.EnvVars), len(localAuth.Files))
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

// applyServiceAccountFlag wires the --service-account CLI flag into a
// CreateAgentRequest's GCP identity assignment.
func applyServiceAccountFlag(req *hubclient.CreateAgentRequest, saFlag string) {
	if saFlag != "" {
		req.GCPIdentity = &hubclient.GCPIdentityConfig{
			MetadataMode:     "assign",
			ServiceAccountID: saFlag,
		}
	}
}

// splitFilesIgnoredWarning reports whether warnings contain the Hub's
// api.WarningEmptyPerAgentWorkspaceFilesIgnored and returns the other
// warnings unchanged.
func splitFilesIgnoredWarning(warnings []string) (ignored bool, rest []string) {
	if !slices.Contains(warnings, api.WarningEmptyPerAgentWorkspaceFilesIgnored) {
		return false, warnings
	}
	// rest stays nil when the warning was the only entry, as before.
	for _, w := range warnings {
		if w != api.WarningEmptyPerAgentWorkspaceFilesIgnored {
			rest = append(rest, w)
		}
	}
	return true, rest
}

// workspaceBootstrapNotice returns the status lines to show after an agent
// create that collected non-git workspace files (sentFiles > 0), and the Hub
// warnings still to be printed in the usual place. When the Hub returned no
// upload URLs it either uses the broker's local workspace ("Using local
// workspace on broker.") or, for an empty-per-agent project, ignored the
// files and said so with api.WarningEmptyPerAgentWorkspaceFilesIgnored. That
// warning replaces the local-workspace line, which would be misleading; all
// other warnings are returned unchanged.
func workspaceBootstrapNotice(sentFiles, uploadURLs int, warnings []string) (lines, remaining []string) {
	if sentFiles == 0 || uploadURLs > 0 {
		return nil, warnings
	}
	ignored, rest := splitFilesIgnoredWarning(warnings)
	if ignored {
		return []string{"Warning: " + api.WarningEmptyPerAgentWorkspaceFilesIgnored}, rest
	}
	return []string{"Using local workspace on broker."}, warnings
}

// workspaceFinalizeNotice returns the status lines to show after a
// workspace bootstrap finalize (sync-to/finalize). If the Hub ignored the
// uploaded files (empty-per-agent), its warning replaces "Workspace uploaded:
// N files"; any other warnings follow as "Warning: ..." lines.
func workspaceFinalizeNotice(filesApplied int, warnings []string) []string {
	ignored, rest := splitFilesIgnoredWarning(warnings)
	var lines []string
	if ignored {
		lines = append(lines, "Warning: "+api.WarningEmptyPerAgentWorkspaceFilesIgnored)
	} else {
		lines = append(lines, fmt.Sprintf("Workspace uploaded: %d files", filesApplied))
	}
	for _, w := range rest {
		lines = append(lines, "Warning: "+w)
	}
	return lines
}

// printHubWorkspaceSource prints where the hub gets the agent workspace from
// for a git-based project. It mirrors the hub's provisioning decision
// (pkg/hub/handlers_agent_create_helpers.go): shared-workspace projects mount
// the shared workspace and are not cloned; every other git project is cloned
// from the URL the hub resolves (same normalization, ptone/scion#1915).
func printHubWorkspaceSource(project *hubclient.Project) {
	if project.GitRemote == "" {
		return
	}
	if project.Labels[store.LabelWorkspaceMode] == store.WorkspaceModeShared {
		fmt.Fprintf(os.Stderr, "Using hub, shared workspace for repo %s\n", project.GitRemote)
		fmt.Fprintf(os.Stderr, "  (agents mount the project's shared workspace on the runtime broker; no per-agent clone)\n")
		return
	}
	cloneURL := util.ResolveCloneURL(project.Labels[store.LabelCloneURL], project.GitRemote)
	fmt.Fprintf(os.Stderr, "Using hub, cloning repo %s\n", cloneURL)
	fmt.Fprintf(os.Stderr, "  (%s; local worktrees are not used)\n", hubCloneTransportNote(cloneURL))
}

// hubCloneTransportNote describes how the hub clones cloneURL (as resolved by
// util.ResolveCloneURL), for the "Using hub, cloning repo" log line. The
// broker's git credential helper supplies GITHUB_TOKEN for HTTP(S) clones
// only.
func hubCloneTransportNote(cloneURL string) string {
	lower := strings.ToLower(cloneURL)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return "Hub mode uses HTTPS clone with GITHUB_TOKEN"
	case strings.HasPrefix(lower, "http://"):
		return "Hub mode uses plain HTTP clone with GITHUB_TOKEN"
	case strings.HasPrefix(lower, "ssh://"), strings.HasPrefix(cloneURL, "git@"):
		return "Hub mode uses SSH clone; GITHUB_TOKEN is not used"
	case strings.HasPrefix(lower, "git://"):
		return "Hub mode uses unauthenticated git:// clone; GITHUB_TOKEN is not used"
	default:
		return "Hub mode clones this path on the runtime broker"
	}
}

// hubStartImpliesResume reports whether `scion start` on an existing hub agent
// in the given phase should be sent as a resume request. Suspended agents
// resume their session; stopped agents restart in place with a fresh session
// (the hub's resume-in-place path). Error-phase agents are deliberately not
// included: the hub only restarts them in place with --force
// (resumeInPlaceDecision), so `start` on them still reports a conflict.
func hubStartImpliesResume(phase string) bool {
	return phase == string(state.PhaseSuspended) || phase == string(state.PhaseStopped)
}

// hubCreateReusesExistingAgent mirrors the hub's handleExistingAgent decision
// tree (pkg/hub/handlers_agent_create_helpers.go) and reports whether a create
// request for an agent already in the given phase will start, resume or
// restart that agent in place rather than create a new one. On those paths the
// hub applies only the task and attach mode; all other create configuration
// is ignored.
func hubCreateReusesExistingAgent(phase string, resume, force bool) bool {
	switch phase {
	case string(state.PhaseSuspended), string(state.PhaseCreated):
		return true
	case string(state.PhaseStopped):
		return resume
	case string(state.PhaseError):
		return resume && force
	default:
		// Not found, running (conflict), provisioning (deleted and
		// re-created by env-gather, so flags do apply), or unknown.
		return false
	}
}

// hubStartActionWord returns the verb printed before a hub start request so it
// reflects what the hub will actually do with an agent in the given phase.
func hubStartActionWord(phase string, resume, force bool) string {
	switch {
	case phase == string(state.PhaseStopped) && resume:
		// The hub restarts a stopped agent with a fresh harness session.
		return "Restarting"
	case resume && force:
		return "Force-resuming"
	case resume:
		return "Resuming"
	default:
		return "Starting"
	}
}

// configFlagsNotAppliedToExistingAgent lists the start/resume flags that carry
// agent configuration. When the hub reuses an existing agent in place it
// applies only the task and --attach, so explicitly setting any of these has
// no effect.
var configFlagsNotAppliedToExistingAgent = []string{
	"type", "harness-config", "harness", "harness-auth", "image", "model",
	"thinking-level", "config", "broker", "label", "role", "message-mode",
	"branch", "workspace", "service-account", "enable-telemetry",
	"disable-telemetry", "no-auth", "profile",
}

// warnFlagsIgnoredForExistingAgent prints one stderr warning naming the
// configuration flags the user explicitly set that the hub will not apply
// because the agent already exists and is being reused in place.
func warnFlagsIgnoredForExistingAgent(cmd *cobra.Command, agentName, phase string) {
	if cmd == nil {
		return
	}
	var ignored []string
	noAuthIgnored := false
	for _, name := range configFlagsNotAppliedToExistingAgent {
		// cmd.Flag also finds inherited persistent flags (--profile).
		f := cmd.Flag(name)
		if f == nil || !f.Changed {
			continue
		}
		ignored = append(ignored, "--"+name)
		if name == "no-auth" {
			noAuthIgnored = true
		}
	}
	if len(ignored) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: agent '%s' already exists (phase: %s) and is reused in place; these flags are not applied to an existing agent: %s\n",
		agentName, phase, strings.Join(ignored, ", "))
	if noAuthIgnored {
		fmt.Fprintf(os.Stderr, "  --no-auth is not applied when resuming or restarting an existing agent (see ptone/scion#1855); delete and re-create the agent to apply it.\n")
	}
}

// startAgentViaHub creates/starts (or resumes/restarts in place) an agent via
// the Hub. cmd is used only to detect which flags the user explicitly set (for
// the "not applied to an existing agent" warning); it may be nil.
func startAgentViaHub(cmd *cobra.Command, hubCtx *HubContext, agentName, task string, resume bool, inlineCfg *api.ScionConfig) error {
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
		if projectErr == nil && project != nil {
			printHubWorkspaceSource(project)
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

	parsedLabels, err := parseLabels(labelFlags)
	if err != nil {
		return err
	}

	// Validate --role flag if provided
	if err := validateAgentRole(agentRoleFlag); err != nil {
		return err
	}

	// Validate --message-mode flag if provided
	if err := validateMessageMode(messageModeFlag); err != nil {
		return err
	}

	// Build create request (Hub creates and starts in one operation)
	req := &hubclient.CreateAgentRequest{
		Name:            agentName,
		ProjectID:       projectID,
		Template:        resolvedTemplate,
		HarnessConfig:   harnessConfigFlag,
		HarnessAuth:     harnessAuthFlag,
		RuntimeBrokerID: runtimeBrokerID,
		Profile:         profile,
		Task:            task,
		Branch:          branch,
		Workspace:       workspace,
		Labels:          parsedLabels,
		Resume:          resume,
		ForceResume:     resume && forceResume,
		Attach:          attach,
		GatherEnv:       true, // Enable env-gather flow
		Notify:          !startNoNotify,
		AgentRole:       agentRoleFlag,
		MessageMode:     messageModeFlag,
		NoAuth:          noAuth,
		// Opt in to a non-blocking launch; the CLI waits for it below. A
		// Hub without async launch ignores this and answers synchronously.
		AcceptAsyncLaunch: true,
	}

	// Wire --service-account flag into the GCP identity assignment.
	applyServiceAccountFlag(req, serviceAccountFlag)

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
		util.Debugf("[auth]   hasGoogleAppCredentials=%t", localAuth.GoogleAppCredentials != "")
		util.Debugf("[auth]   cloudProject=%q, cloudRegion=%q", localAuth.GoogleCloudProject, localAuth.GoogleCloudRegion)
		util.Debugf("[auth]   envVarCount=%d, fileCount=%d", len(localAuth.EnvVars), len(localAuth.Files))
	}

	// Detect non-git project for workspace bootstrap.
	// Skip when running inside a hub-connected container — the broker already
	// has the workspace mounted and CollectFiles would walk and SHA-256 hash
	// every file in the project directory, which is extremely expensive for
	// large workspaces.
	var workspaceFiles []transfer.FileInfo
	if hubCtx.ProjectPath != "" && !hubCtx.IsGlobal && !config.IsHubContext() {
		projectDir := filepath.Dir(hubCtx.ProjectPath) // parent of .scion
		if _, statErr := os.Stat(projectDir); statErr == nil && !util.IsGitRepoDir(projectDir) {
			if debugMode {
				util.Debugf("[workspace-scan] non-git project detected at %s, collecting files...", projectDir)
			}
			scanStart := time.Now()
			files, err := transfer.CollectFiles(projectDir, transfer.DefaultExcludePatterns)
			if debugMode {
				util.Debugf("[workspace-scan] collected %d files in %s", len(files), time.Since(scanStart))
			}
			if err != nil {
				return fmt.Errorf("failed to collect workspace files: %w", err)
			}
			if len(files) > 0 {
				req.WorkspaceFiles = files
				workspaceFiles = files
			}
		}
	} else if debugMode && config.IsHubContext() {
		util.Debugf("[workspace-scan] skipped: running in hub context (broker has workspace access)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check whether the agent already exists on the hub. A suspended or
	// stopped agent is resumed/restarted in place (parity with local mode and
	// `scion resume`, ptone/scion#1911). This check is best-effort — if it
	// fails (agent doesn't exist yet), we fall through to "Starting".
	existingCheckStart := time.Now()
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	existing, getErr := hubCtx.Client.ProjectAgents(projectID).Get(checkCtx, agentName)
	checkCancel()
	if debugMode {
		util.Debugf("[startup] existing-agent check completed in %s (err=%v)", time.Since(existingCheckStart), getErr)
	}
	existingPhase := ""
	if getErr == nil && existing != nil {
		existingPhase = existing.Phase
	}
	if !resume && hubStartImpliesResume(existingPhase) {
		resume = true
		req.Resume = true
	}
	// Warn about flags the hub will not apply, but only once the hub has
	// accepted the request: a refused start must not claim the agent was
	// reused.
	reusesExisting := hubCreateReusesExistingAgent(existingPhase, req.Resume, req.ForceResume)

	if !isJSONOutput() {
		action := hubStartActionWord(existingPhase, resume, req.ForceResume)
		fmt.Printf("%s agent '%s'...\n", action, agentName)
	}

	resp, err := createAgentWithBrokerResolution(ctx, hubCtx, projectID, req)
	if err != nil {
		if debugMode {
			util.Debugf("[env-gather] startAgentViaHub: create request failed: %v", err)
		}
		if apiErr, ok := asIncompleteCreate(err); ok {
			return incompleteCreateError(agentName, apiErr)
		}
		return wrapHubError(fmt.Errorf("failed to start agent via Hub: %w", err))
	}
	if reusesExisting {
		warnFlagsIgnoredForExistingAgent(cmd, agentName, existingPhase)
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
		hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, resp.Agent.Created)
		hubsync.AddSyncedAgent(hubCtx.ProjectPath, agentName)
	}

	// Print info line when broker was auto-resolved (not explicitly specified)
	if !isJSONOutput() {
		printAutoResolvedBroker(ctx, hubCtx, runtimeBrokerID, req.RuntimeBrokerID, resp)
	}

	// Workspace bootstrap: upload files and finalize
	workspaceFinalized := false
	bootstrapLines, remainingWarnings := workspaceBootstrapNotice(len(workspaceFiles), len(resp.UploadURLs), resp.Warnings)
	for _, line := range bootstrapLines {
		statusln(line)
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
			if apiErr, ok := asIncompleteCreate(err); ok {
				return incompleteCreateError(agentName, apiErr)
			}
			return fmt.Errorf("failed to finalize workspace bootstrap: %w", err)
		}
		for _, line := range workspaceFinalizeNotice(finalizeResp.FilesApplied, finalizeResp.Warnings) {
			statusln(line)
		}
		workspaceFinalized = true
	}

	// A stopped agent is restarted in place with a fresh session; report it
	// with the same word as the "Restarting agent" line above.
	restarted := hubStartActionWord(existingPhase, resume, req.ForceResume) == "Restarting"
	return finishHubStart(hubCtx, projectID, agentName, resume, restarted, resp, remainingWarnings, workspaceFinalized)
}

// finishHubStart completes a Hub start after the create (and any workspace
// finalize): it waits for the launch when needed, reports the result, and
// attaches with --attach. restarted reports a stopped agent restarted in
// place, so the success line matches the "Restarting" action word.
// textWarnings are the create warnings still to print in text output (those
// not already shown as workspace notices); JSON output carries all of
// resp.Warnings.
func finishHubStart(hubCtx *HubContext, projectID, agentName string, resume, restarted bool, resp *hubclient.CreateAgentResponse, textWarnings []string, workspaceFinalized bool) error {
	// Decide whether to wait for the agent to reach running:
	//   - after a workspace finalize, which dispatches the start;
	//   - when the Hub accepted the create for an asynchronous launch;
	//   - with --attach, which needs a running agent.
	// A synchronous answer from a Hub without async launch needs no wait,
	// exactly as before. --no-wait returns once the Hub has accepted.
	needWait := workspaceFinalized || launchActive(resp.Agent) || (attach && !isJSONOutput())
	if startNoWait && !attach {
		needWait = false
	}

	finalAgent := resp.Agent
	if needWait {
		statusf("Waiting for agent '%s' to start...\n", agentName)
		var progress io.Writer
		if !isJSONOutput() {
			progress = os.Stderr
		}
		// The create response's agent seeds the wait budget unless it was
		// superseded by a workspace finalize.
		budgetFrom := resp.Agent
		if workspaceFinalized {
			budgetFrom = nil
		}
		// Ctrl-C (or SIGTERM) stops waiting only; the launch continues on
		// the Hub.
		waited, err := waitForAgentLaunchWithSignals(launchWaitOptions{
			AgentName:  agentName,
			BudgetFrom: budgetFrom,
			Get: func(ctx context.Context) (*hubclient.Agent, error) {
				return hubCtx.Client.ProjectAgents(projectID).Get(ctx, agentName)
			},
			Timeout:  startWaitTimeout,
			Progress: progress,
		})
		if err != nil {
			for _, w := range textWarnings {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
			}
			return err
		}
		finalAgent = waited
	}

	// After a finalize without waiting, the create answer predates the
	// dispatched start; report the agent's current state instead.
	phaseKnown := true
	if workspaceFinalized && !needWait {
		getCtx, getCancel := context.WithTimeout(context.Background(), launchFetchTimeout)
		current, getErr := hubCtx.Client.ProjectAgents(projectID).Get(getCtx, agentName)
		getCancel()
		if getErr == nil && current != nil {
			finalAgent = current
		} else {
			phaseKnown = false
		}
	}

	displayStatus := "started"
	switch {
	case restarted:
		displayStatus = "restarted"
	case resume:
		displayStatus = "resumed"
	}
	launching := !needWait && launchActive(finalAgent)
	if workspaceFinalized && !needWait && !agentIsRunning(finalAgent, phaseKnown) {
		launching = true
	}
	message := fmt.Sprintf("Agent '%s' %s via Hub.", agentName, displayStatus)
	if launching {
		message = fmt.Sprintf("Agent '%s' accepted by Hub and launching.", agentName)
	}

	// With --attach after a workspace upload, the session is attached
	// without a JSON document, as before.
	if isJSONOutput() && (!attach || !workspaceFinalized) {
		result := ActionResult{
			Status:   "success",
			Command:  "start",
			Agent:    agentName,
			Message:  message,
			Warnings: resp.Warnings,
			Details:  map[string]interface{}{},
		}
		if finalAgent != nil {
			result.Details["slug"] = finalAgent.Slug
			phase, activity := hubAgentPhaseActivity(finalAgent.Phase, finalAgent.Activity, finalAgent.Status)
			if phaseKnown {
				result.Details["phase"] = phase
				if activity != "" {
					result.Details["activity"] = activity
				}
			}
			if finalAgent.RuntimeBrokerID != "" {
				result.Details["runtimeBrokerId"] = finalAgent.RuntimeBrokerID
			}
			if launching && finalAgent.Launch != nil {
				result.Details["launchId"] = finalAgent.Launch.ID
				if finalAgent.Launch.Deadline != nil {
					result.Details["launchDeadline"] = finalAgent.Launch.Deadline.UTC().Format(time.RFC3339)
				}
			}
		}
		return outputJSON(result)
	}

	statusf("%s\n", message)
	if finalAgent != nil {
		statusf("Agent Slug: %s\n", finalAgent.Slug)
		if phaseKnown {
			phase, _ := hubAgentPhaseActivity(finalAgent.Phase, finalAgent.Activity, finalAgent.Status)
			statusf("Phase: %s\n", phase)
		}
	}
	if launching {
		statusf("Follow the launch with: scion start %s (waits until it is running)\n", agentName)
	}
	for _, w := range textWarnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}

	if !attach {
		return nil
	}

	// Attach mode: the wait above returned a running agent.
	agentID := ""
	if resp.Agent != nil {
		agentID = resp.Agent.ID
	}
	// agentID keeps the create response's ID unless the fetch returned a
	// non-empty one. Runtime, broker and profile always take the fetched
	// value, even if empty: "" is itself a meaningful attach-is-supported
	// value to attachUnsupportedErr.
	var agentRuntime, agentBrokerID, agentProfile string
	if finalAgent != nil {
		if finalAgent.ID != "" {
			agentID = finalAgent.ID
		}
		agentRuntime = finalAgent.Runtime
		agentBrokerID = finalAgent.RuntimeBrokerID
		agentProfile = agentProfileName(finalAgent)
	}
	if agentID == "" {
		agentID = agentName
	}

	attachCtx, attachCancel := context.WithTimeout(context.Background(), launchFetchTimeout)
	defer attachCancel()
	if err := attachUnsupportedErr(attachCtx, hubCtx, agentRuntime, agentBrokerID, agentProfile); err != nil {
		return err
	}

	// Resolve transport auth for IAP/Cloud Run traversal FIRST — in IAP mode
	// there is no application-level token by design, so transport auth must be
	// determined before deciding whether an app token is required.
	attachOpts, transportSrc, err := resolveAttachOptions()
	if err != nil {
		return err
	}

	// Get access token for WebSocket authentication.
	// Only require an application token when no transport source is configured.
	token := getHubAccessToken(hubCtx.Endpoint)
	if token == "" && transportSrc == nil {
		return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
	}

	statusf("Attaching to agent '%s' via Hub...\n", agentName)
	return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID, attachOpts...)
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

// splitCommaList normalises a repeatable string-slice flag into a flat list of
// values. Each occurrence of the flag is additionally split on commas, so the
// historical comma-separated form and the repeatable form are both accepted and
// may be freely mixed:
//
//	--scopes a,b            -> ["a", "b"]
//	--scopes a --scopes b   -> ["a", "b"]
//	--scopes a,b --scopes c -> ["a", "b", "c"]
//
// Surrounding whitespace is trimmed and empty entries are dropped.
func splitCommaList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// agentIsRunning reports whether a is known to be in the running phase.
func agentIsRunning(a *hubclient.Agent, phaseKnown bool) bool {
	if a == nil || !phaseKnown {
		return false
	}
	phase, _ := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)
	return phase == string(state.PhaseRunning)
}
