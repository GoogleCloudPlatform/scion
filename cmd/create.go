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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

// createCmd represents the create command
var createCmd = &cobra.Command{
	Use:   "create <agent-name> [task...]",
	Short: "Provision a new scion agent without starting it",
	Long: `Provision a new isolated LLM agent directory to perform a specific task.
The agent will be created from a template.

The agent-name is required as the first argument. All subsequent arguments
form the task prompt, which will be written to prompt.md. If no task
arguments are provided, an empty prompt.md is created for later editing.

The agent is provisioned but not started, even when a task is given. Run
'scion start <agent-name>' to start it.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])
		task := strings.TrimSpace(strings.Join(args[1:], " "))

		if err := validateHarnessAuthFlag(harnessAuthFlag); err != nil {
			return err
		}

		// Check if Hub should be used, excluding the target agent from sync requirements.
		// This allows creating an agent even if it already exists on Hub (recreate scenario)
		// or if other agents are out of sync.
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return createAgentViaHub(hubCtx, agentName, task)
		}

		// Load inline config if --config was specified
		var inlineCfg *api.ScionConfig
		if inlineConfigPath != "" {
			var inlineConfigDir string
			var loadErr error
			inlineCfg, inlineConfigDir, loadErr = loadInlineConfig(inlineConfigPath)
			if loadErr != nil {
				return loadErr
			}
			if loadErr := resolveInlineConfigContent(inlineCfg, inlineConfigDir); loadErr != nil {
				return loadErr
			}
		}

		// Local mode
		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)

		// Apply inline config overrides to CLI options
		effectiveBranch := branch
		effectiveTask := task
		effectiveHarnessConfig := harnessConfigFlag
		effectiveImage := agentImage

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
			if effectiveImage == "" && inlineCfg.Image != "" {
				effectiveImage = inlineCfg.Image
			}
		}

		opts := api.StartOptions{
			Name:          agentName,
			Task:          effectiveTask,
			Template:      templateName,
			Profile:       profile,
			HarnessConfig: effectiveHarnessConfig,
			Image:         effectiveImage,
			ProjectPath:   projectPath,
			Branch:        effectiveBranch,
			Workspace:     workspace,
			InlineConfig:  inlineCfg,
		}

		// Check if agent already exists (directory on disk or running container)
		projectDir, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return err
		}
		agentDir := filepath.Join(projectDir, "agents", agentName)
		if _, err := os.Stat(agentDir); err == nil {
			return fmt.Errorf("agent '%s' already exists. Use 'scion delete %s' first to recreate it", agentName, agentName)
		}

		ctx := context.Background()
		// Attempt Hub connection for skill resolution in local mode.
		// If Hub is not configured, this returns nil and provisioning
		// proceeds without a resolver (S1 fail-closed for required skills).
		hctx, hubErr := hubsync.EnsureHubReady(projectPath, skillResolverHubOptions(projectPath))
		if hubErr == nil && hctx != nil && hctx.Client != nil {
			var flushResolutions func()
			ctx, flushResolutions = withLocalSkillResolution(ctx, hctx.Client.Skills(), hctx.Client.SkillRegistries(),
				hctx.ProjectID, os.Getenv("GITHUB_TOKEN"), nil)
			// Write resolutions to the disk cache before this process exits,
			// rather than relying on the cache's delayed write.
			defer flushResolutions()
		}

		_, err = mgr.Provision(ctx, opts)
		if err != nil {
			return err
		}

		if isJSONOutput() {
			return outputJSON(localCreateResult(agentName))
		}
		writeLocalCreateResult(os.Stdout, agentName)
		return nil
	},
}

// withLocalSkillResolution returns ctx set up to resolve skills for a local
// create: a routing resolver that sends skill:// refs and bare names to the
// Hub, gh:// refs to a GitHub resolver using ghToken, and gcp-skill:// refs
// to a resolver that looks registries up through the Hub; the credentials
// the install step needs for gh:// downloads; and projectID as the resolve
// project, when set. cache is the GitHub resolution cache to use, or nil for
// the default one. The returned func writes pending resolution cache entries
// to disk and must be called before the process exits.
func withLocalSkillResolution(
	ctx context.Context,
	skills hubclient.SkillService,
	registries hubclient.SkillRegistryService,
	projectID, ghToken string,
	cache *agent.GitHubResolutionCache,
) (context.Context, func()) {
	resolver := agent.NewRoutingSkillResolver(agent.NewHubSkillResolver(skills))
	ghResolver := agent.NewGitHubSkillResolverWithCredentials(ghToken, nil, cache)
	resolver.Register("gh", ghResolver)

	gcpLookup := func(ctx context.Context, name string) (*agent.RegistryLookupResult, error) {
		reg, err := registries.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if reg == nil {
			return nil, fmt.Errorf("registry %q not found", name)
		}
		return &agent.RegistryLookupResult{
			Name:     reg.Name,
			Endpoint: reg.Endpoint,
			Type:     reg.Type,
			Status:   reg.Status,
		}, nil
	}
	resolver.Register("gcp-skill", agent.NewGCPSkillResolver(gcpLookup))

	ctx = agent.ContextWithSkillResolver(ctx, resolver)
	// Credentials for install-phase downloads of gh:// skills: the default
	// for skills the Hub resolved, and the GitHub resolver's own lookup for
	// skills it served from its disk cache.
	ctx = ghResolver.WithInstallCredentials(ctx, ghToken)
	if projectID != "" {
		ctx = agent.ContextWithResolveProjectID(ctx, projectID)
	}
	return ctx, ghResolver.FlushCache
}

// scion create provisions an agent and never starts it; scion start launches
// it. The helpers below make every create output say so and name the
// follow-up command.

// createStartCommand returns the command that launches an agent made by
// scion create.
func createStartCommand(agentName string) string {
	return "scion start " + agentName
}

// createNotStartedHint is the line that tells the user the agent was
// provisioned but not started, and how to start it.
func createNotStartedHint(agentName string) string {
	return fmt.Sprintf("Agent '%s' is provisioned but not started. Run '%s' to start it.", agentName, createStartCommand(agentName))
}

// createNotProvisionedHint is the line printed instead of
// createNotStartedHint when the Hub kept the agent record but the runtime
// broker could not provision it.
func createNotProvisionedHint(agentName string) string {
	return fmt.Sprintf("Agent '%s' was not fully provisioned (see the warning). Run '%s' to retry provisioning and start it.", agentName, createStartCommand(agentName))
}

// createHint returns the closing line of a scion create output.
func createHint(agentName string, provisioned bool) string {
	if provisioned {
		return createNotStartedHint(agentName)
	}
	return createNotProvisionedHint(agentName)
}

// hubCreateProvisioned reports whether a Hub create provisioned the agent,
// that is, whether none of the warnings says provisioning failed.
func hubCreateProvisioned(warnings []string) bool {
	for _, w := range warnings {
		if strings.HasPrefix(w, api.ProvisionFailedWarningPrefix) {
			return false
		}
	}
	return true
}

// addCreateNotStartedDetails records in JSON output details that the agent
// was not started, whether it was provisioned, and the command that starts
// it.
func addCreateNotStartedDetails(details map[string]interface{}, agentName string, provisioned bool) {
	details["started"] = false
	details["provisioned"] = provisioned
	details["startCommand"] = createStartCommand(agentName)
}

// localCreateResult is the JSON result of a local scion create.
func localCreateResult(agentName string) ActionResult {
	details := map[string]interface{}{}
	addCreateNotStartedDetails(details, agentName, true)
	return ActionResult{
		Status:  "success",
		Command: "create",
		Agent:   agentName,
		Message: fmt.Sprintf("Agent '%s' created successfully. ", agentName) + createNotStartedHint(agentName),
		Details: details,
	}
}

// writeLocalCreateResult prints the text result of a local scion create.
func writeLocalCreateResult(w io.Writer, agentName string) {
	_, _ = fmt.Fprintf(w, "Agent '%s' created successfully.\n%s\n", agentName, createNotStartedHint(agentName))
}

// hubCreateResult is the JSON result of a scion create through a Hub.
func hubCreateResult(agentName string, resp *hubclient.CreateAgentResponse) ActionResult {
	provisioned := hubCreateProvisioned(resp.Warnings)
	result := ActionResult{
		Status:   "success",
		Command:  "create",
		Agent:    agentName,
		Message:  fmt.Sprintf("Agent '%s' created via Hub. ", agentName) + createHint(agentName, provisioned),
		Warnings: resp.Warnings,
		Details:  map[string]interface{}{},
	}
	addCreateNotStartedDetails(result.Details, agentName, provisioned)
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
		if resp.Agent.RuntimeBrokerName != "" {
			result.Details["runtimeBrokerName"] = resp.Agent.RuntimeBrokerName
		}
	}
	return result
}

// writeHubCreateText prints the text result of a scion create through a
// Hub: the agent summary, any warnings, then the closing hint. agentDir is
// printed when not empty.
func writeHubCreateText(w io.Writer, agentName string, resp *hubclient.CreateAgentResponse, agentDir string) {
	var b strings.Builder
	if resp.Agent != nil {
		brokerInfo := ""
		if resp.Agent.RuntimeBrokerName != "" {
			brokerInfo = fmt.Sprintf(" on broker %s", resp.Agent.RuntimeBrokerName)
		} else if resp.Agent.RuntimeBrokerID != "" {
			brokerInfo = fmt.Sprintf(" on broker %s", resp.Agent.RuntimeBrokerID)
		}
		fmt.Fprintf(&b, "Agent '%s' created via Hub%s.\n", agentName, brokerInfo)
		fmt.Fprintf(&b, "Agent Slug: %s\n", resp.Agent.Slug)
		phase, _ := hubAgentPhaseActivity(resp.Agent.Phase, resp.Agent.Activity, resp.Agent.Status)
		fmt.Fprintf(&b, "Phase: %s\n", provisionedPhaseLabel(phase, resp.Agent.ProvisionedOnly))
		if agentDir != "" {
			fmt.Fprintf(&b, "Agent directory: %s\n", agentDir)
		}
	} else {
		fmt.Fprintf(&b, "Agent '%s' created via Hub.\n", agentName)
	}
	for _, warning := range resp.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", warning)
	}
	fmt.Fprintf(&b, "%s\n", createHint(agentName, hubCreateProvisioned(resp.Warnings)))
	_, _ = io.WriteString(w, b.String())
}

func createAgentViaHub(hubCtx *HubContext, agentName string, task string) error {
	PrintUsingHub(hubCtx.Endpoint)

	// Get the project ID for this project
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Resolve template if specified
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

	// Build create request — always provision-only (create does not start the agent)
	req := &hubclient.CreateAgentRequest{
		Name:            agentName,
		ProjectID:       projectID,
		Template:        resolvedTemplate,
		HarnessConfig:   harnessConfigFlag,
		HarnessAuth:     harnessAuthFlag,
		RuntimeBrokerID: runtimeBrokerID,
		Task:            task,
		Branch:          branch,
		Labels:          parsedLabels,
		ProvisionOnly:   true,
		AgentRole:       agentRoleFlag,
		MessageMode:     messageModeFlag,
	}

	// Wire --service-account flag into the GCP identity assignment.
	applyServiceAccountFlag(req, serviceAccountFlag)

	if agentImage != "" {
		req.Config = &api.ScionConfig{
			Image: agentImage,
		}
	}

	if debugMode {
		util.Debugf("[env-gather] createAgentViaHub: provision-only create for agent %q (template=%q, broker=%q)", agentName, resolvedTemplate, runtimeBrokerID)
		util.Debugf("[env-gather] createAgentViaHub: no env vars sent — create (provision-only) does not trigger env-gather flow")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := createAgentWithBrokerResolution(ctx, hubCtx, projectID, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to create agent via Hub: %w", err))
	}

	// Advance watermark to the hub-assigned creation time so this agent
	// won't trigger a sync warning on the next 'scion ls'.
	if resp.Agent != nil && !resp.Agent.Created.IsZero() {
		hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, resp.Agent.Created)
		hubsync.AddSyncedAgent(hubCtx.ProjectPath, agentName)
	}

	// Print info line when broker was auto-resolved (not explicitly specified)
	printAutoResolvedBroker(ctx, hubCtx, runtimeBrokerID, req.RuntimeBrokerID, resp)

	if isJSONOutput() {
		return outputJSON(hubCreateResult(agentName, resp))
	}

	// For a local broker, print the agent directory path so the user can
	// inspect or tweak its files.
	agentDir := ""
	if resp.Agent != nil && hubCtx.BrokerID != "" && hubCtx.ProjectPath != "" {
		agentDir = filepath.Join(hubCtx.ProjectPath, "agents", agentName)
	}
	writeHubCreateText(os.Stderr, agentName, resp, agentDir)

	return nil
}

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVarP(&templateName, "type", "t", "", "Template to use")
	createCmd.Flags().StringVarP(&agentImage, "image", "i", "", "Container image to use (overrides template)")
	createCmd.Flags().StringVarP(&branch, "branch", "b", "", "Git branch to use for the agent workspace")
	createCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Host path or project-relative subdirectory to mount as /workspace")
	createCmd.Flags().StringVar(&runtimeBrokerID, "broker", "", "Preferred runtime broker ID, name, or slug")
	createCmd.Flags().StringVar(&harnessConfigFlag, "harness-config", "", "Named harness configuration to use")
	createCmd.Flags().StringVar(&harnessConfigFlag, "harness", "", "Named harness configuration to use (alias for --harness-config)")

	createCmd.Flags().StringVar(&harnessAuthFlag, "harness-auth", "", "Override auth method for the harness (api-key, oauth-token, auth-file, vertex-ai)")

	// Template resolution flags for Hub mode (Section 9.4)
	createCmd.Flags().BoolVar(&uploadTemplate, "upload-template", false, "Automatically upload local template to Hub if not found")
	createCmd.Flags().BoolVar(&noUpload, "no-upload", false, "Fail if template requires upload (never prompt)")
	createCmd.Flags().StringVar(&templateScope, "template-scope", "project", "Scope for uploaded template (global, project, user)")

	// Inline config flag
	createCmd.Flags().StringVar(&inlineConfigPath, "config", "", "Path to inline agent config file (YAML/JSON), or '-' for stdin")

	// Label flags
	createCmd.Flags().StringArrayVar(&labelFlags, "label", nil, "Label in key=value format (repeatable)")

	// Agent role flag
	createCmd.Flags().StringVar(&agentRoleFlag, "role", "",
		"Agent role for Hub API access: none, readonly, baseline, full")

	// Agent message mode flag
	createCmd.Flags().StringVar(&messageModeFlag, "message-mode", "",
		"Agent message mode: none, lineage, branch, project")

	// GCP service account assignment flag
	createCmd.Flags().StringVar(&serviceAccountFlag, "service-account", "", "GCP service account ID to assign to this agent (requires Hub mode)")
}

// skillResolverHubOptions returns the EnsureHubReady options for the hub
// context that create uses for skill resolution. A project named with a
// flag keeps its own ID over SCION_PROJECT_ID (ptone/scion#3123).
func skillResolverHubOptions(projectPath string) hubsync.EnsureHubReadyOptions {
	return hubsync.EnsureHubReadyOptions{
		NoHub:           noHub,
		AutoConfirm:     true,
		SkipSync:        true,
		ExplicitProject: explicitProjectTargetFor(projectPath),
	}
}
