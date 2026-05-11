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
arguments are provided, an empty prompt.md is created for later editing.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])
		task := strings.TrimSpace(strings.Join(args[1:], " "))

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
		// This allows creating an agent even if it already exists on Hub (recreate scenario)
		// or if other agents are out of sync.
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
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
			ProjectPath:     projectPath,
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

		_, err = mgr.Provision(context.Background(), opts)
		if err != nil {
			return err
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "create",
				Agent:   agentName,
				Message: fmt.Sprintf("Agent '%s' created successfully.", agentName),
			})
		}
		fmt.Printf("Agent '%s' created successfully.\n", agentName)
		return nil
	},
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

	// Build create request — always provision-only (create does not start the agent)
	req := &hubclient.CreateAgentRequest{
		Name:            agentName,
		ProjectID:         projectID,
		Template:        resolvedTemplate,
		HarnessConfig:   harnessConfigFlag,
		HarnessAuth:     harnessAuthFlag,
		RuntimeBrokerID: runtimeBrokerID,
		Task:            task,
		Branch:          branch,
		ProvisionOnly:   true,
	}

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
		hubsync.UpdateLastSyncedAt(hubCtx.ProjectPath, resp.Agent.Created, hubCtx.IsGlobal)
		hubsync.AddSyncedAgent(hubCtx.ProjectPath, agentName)
	}

	// Print info line when broker was auto-resolved (not explicitly specified)
	printAutoResolvedBroker(ctx, hubCtx, runtimeBrokerID, req.RuntimeBrokerID, resp)

	if isJSONOutput() {
		result := ActionResult{
			Status:   "success",
			Command:  "create",
			Agent:    agentName,
			Message:  fmt.Sprintf("Agent '%s' created via Hub.", agentName),
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
			if resp.Agent.RuntimeBrokerName != "" {
				result.Details["runtimeBrokerName"] = resp.Agent.RuntimeBrokerName
			}
		}
		return outputJSON(result)
	}

	if resp.Agent != nil {
		brokerInfo := ""
		if resp.Agent.RuntimeBrokerName != "" {
			brokerInfo = fmt.Sprintf(" on broker %s", resp.Agent.RuntimeBrokerName)
		} else if resp.Agent.RuntimeBrokerID != "" {
			brokerInfo = fmt.Sprintf(" on broker %s", resp.Agent.RuntimeBrokerID)
		}
		statusf("Agent '%s' created via Hub%s.\n", agentName, brokerInfo)
		statusf("Agent Slug: %s\n", resp.Agent.Slug)
		phase, _ := hubAgentPhaseActivity(resp.Agent.Phase, resp.Agent.Activity, resp.Agent.Status)
		statusf("Phase: %s\n", phase)

		// For local broker, print the agent directory path so the user can inspect/tweak files
		if hubCtx.BrokerID != "" && hubCtx.ProjectPath != "" {
			agentDir := filepath.Join(hubCtx.ProjectPath, "agents", agentName)
			statusf("Agent directory: %s\n", agentDir)
		}
	} else {
		statusf("Agent '%s' created via Hub.\n", agentName)
	}
	for _, w := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}

	return nil
}

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVarP(&templateName, "type", "t", "", "Template to use")
	createCmd.Flags().StringVarP(&agentImage, "image", "i", "", "Container image to use (overrides template)")
	createCmd.Flags().StringVarP(&branch, "branch", "b", "", "Git branch to use for the agent workspace")
	createCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Host path to mount as /workspace")
	createCmd.Flags().StringVar(&runtimeBrokerID, "broker", "", "Preferred runtime broker ID or name")
	createCmd.Flags().StringVar(&harnessConfigFlag, "harness-config", "", "Named harness configuration to use")
	createCmd.Flags().StringVar(&harnessConfigFlag, "harness", "h", "Named harness configuration to use (alias for --harness-config)")

	createCmd.Flags().StringVar(&harnessAuthFlag, "harness-auth", "", "Override auth method for the harness (api-key, oauth-token, auth-file, vertex-ai)")

	// Template resolution flags for Hub mode (Section 9.4)
	createCmd.Flags().BoolVar(&uploadTemplate, "upload-template", false, "Automatically upload local template to Hub if not found")
	createCmd.Flags().BoolVar(&noUpload, "no-upload", false, "Fail if template requires upload (never prompt)")
	createCmd.Flags().StringVar(&templateScope, "template-scope", "project", "Scope for uploaded template (global, project, user)")


	// Inline config flag
	createCmd.Flags().StringVar(&inlineConfigPath, "config", "", "Path to inline agent config file (YAML/JSON), or '-' for stdin")
}
