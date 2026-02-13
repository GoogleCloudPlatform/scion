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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/credentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/transfer"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/ptone/scion-agent/pkg/wsclient"
	"github.com/spf13/cobra"
)

var (
	templateName  string
	agentImage    string
	noAuth        bool
	attach        bool
	branch        string
	workspace     string
	runtimeBrokerID string
)

// HubContext holds the context for Hub operations.
type HubContext struct {
	Client    hubclient.Client
	Endpoint  string
	Settings  *config.Settings
	GroveID   string
	BrokerID string
	GrovePath string
	IsGlobal  bool
}

// CheckHubAvailability checks if Hub integration is enabled and returns a ready-to-use
// Hub context if available. Returns nil if Hub should not be used (not enabled or --no-hub flag is set).
//
// IMPORTANT: When Hub is enabled, this function will return an error if the Hub is
// unavailable or misconfigured. There is NO silent fallback to local mode - this is
// by design to ensure users always know which mode they're operating in.
//
// This function now performs full Hub sync checks via hubsync.EnsureHubReady:
// - Verifies grove registration (prompts to register if not)
// - Compares local and Hub agents (prompts to sync if mismatched)
func CheckHubAvailability(grovePath string) (*HubContext, error) {
	return CheckHubAvailabilityWithOptions(grovePath, false)
}

// CheckHubAvailabilityWithOptions is like CheckHubAvailability but allows skipping sync.
func CheckHubAvailabilityWithOptions(grovePath string, skipSync bool) (*HubContext, error) {
	return CheckHubAvailabilityForAgent(grovePath, "", skipSync)
}

// CheckHubAvailabilityForAgent checks Hub availability for an operation on a specific agent.
// The targetAgent parameter specifies the agent being operated on, which will be excluded
// from sync requirements. This allows operations like delete to proceed without first
// syncing the target agent (e.g., deleting a local-only agent without registering it).
func CheckHubAvailabilityForAgent(grovePath, targetAgent string, skipSync bool) (*HubContext, error) {
	opts := hubsync.EnsureHubReadyOptions{
		AutoConfirm: autoConfirm,
		NoHub:       noHub,
		SkipSync:    skipSync,
		TargetAgent: targetAgent,
	}

	hubCtx, err := hubsync.EnsureHubReady(grovePath, opts)
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
		GroveID:   hubCtx.GroveID,
		BrokerID:    hubCtx.BrokerID,
		GrovePath: hubCtx.GrovePath,
		IsGlobal:  hubCtx.IsGlobal,
	}, nil
}

// CheckHubAvailabilitySimple checks Hub availability without sync checks.
// Use this for read-only operations that don't need full sync verification.
// Deprecated: prefer CheckHubAvailabilityWithOptions with skipSync=true
func CheckHubAvailabilitySimple(grovePath string) (*HubContext, error) {
	// Check if --no-hub flag is set
	if noHub {
		return nil, nil
	}

	settings, err := config.LoadSettings(grovePath)
	if err != nil {
		// If we can't load settings, return the error
		return nil, err
	}

	// Check if hub.local_only is set
	if settings.IsHubLocalOnly() {
		return nil, fmt.Errorf("this grove is configured for local-only mode (hub.local_only=true)\n\n" +
			"To perform this operation:\n" +
			"  - Use --no-hub flag to skip Hub integration\n" +
			"  - Or set hub.local_only=false to enable Hub sync checks")
	}

	// Check if hub is explicitly enabled
	if !settings.IsHubEnabled() {
		return nil, nil
	}

	// Hub is enabled - from here on, any failure is an error (no silent fallback)
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return nil, wrapHubError(fmt.Errorf("Hub is enabled but no endpoint configured.\n\nConfigure via: scion config set hub.endpoint <url>"))
	}

	// Hub is enabled and configured, try to connect
	client, err := getHubClient(settings)
	if err != nil {
		return nil, wrapHubError(fmt.Errorf("failed to create Hub client: %w", err))
	}

	// Check health
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		return nil, wrapHubError(fmt.Errorf("Hub at %s is not responding: %w", endpoint, err))
	}

	return &HubContext{
		Client:   client,
		Endpoint: endpoint,
		Settings: settings,
		GroveID:  settings.GroveID,
	}, nil
}

// PrintUsingHub prints the informational message about using the Hub.
func PrintUsingHub(endpoint string) {
	fmt.Printf("Using hub: %s\n", endpoint)
}

// wrapHubError wraps a Hub error with guidance to disable Hub integration.
func wrapHubError(err error) error {
	if apiclient.IsUnauthorizedError(err) {
		return fmt.Errorf("authentication failed, login to hub with 'scion hub auth login'")
	}
	return fmt.Errorf("%w\n\nTo use local-only mode, run: scion hub disable", err)
}

// GetGroveID looks up the grove ID from HubContext or settings.
// Priority:
//  1. GroveID field in HubContext (set by EnsureHubReady)
//  2. Local grove_id from settings (for non-git groves or explicit configuration)
//  3. Git remote lookup via Hub API
//
// Returns the grove ID if found, or an error if the grove is not registered.
func GetGroveID(hubCtx *HubContext) (string, error) {
	// First, check if GroveID is already set in the context
	if hubCtx.GroveID != "" {
		return hubCtx.GroveID, nil
	}

	// Check if there's a local grove_id in settings
	if hubCtx.Settings != nil && hubCtx.Settings.GroveID != "" {
		return hubCtx.Settings.GroveID, nil
	}

	// Fall back to git remote lookup
	gitRemote := util.GetGitRemote()
	if gitRemote == "" {
		return "", fmt.Errorf("no git origin remote found for this project.\n\nThe Hub uses the origin remote URL to identify groves.\nRun 'scion hub link' to link this grove with the Hub, or use --no-hub for local-only mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Look up groves by git remote
	resp, err := hubCtx.Client.Groves().List(ctx, &hubclient.ListGrovesOptions{
		GitRemote: util.NormalizeGitRemote(gitRemote),
	})
	if err != nil {
		return "", fmt.Errorf("failed to look up grove by git remote: %w", err)
	}

	if len(resp.Groves) == 0 {
		return "", fmt.Errorf("no grove found for git remote: %s\n\nRun 'scion hub link' to link this grove with the Hub", gitRemote)
	}

	// Return the first matching grove
	return resp.Groves[0].ID, nil
}

func RunAgent(cmd *cobra.Command, args []string, resume bool) error {
	agentName := args[0]
	task := strings.Join(args[1:], " ")

	// Reject --format json with --attach (mutually exclusive)
	if isJSONOutput() && attach {
		return fmt.Errorf("--format json and --attach are mutually exclusive")
	}

	// Check if Hub should be used, excluding the target agent from sync requirements.
	// This allows starting/resuming an agent even if it exists on Hub but not locally
	// (will be created via Hub) or if other agents are out of sync.
	hubCtx, err := CheckHubAvailabilityForAgent(grovePath, agentName, false)
	if err != nil {
		return err
	}

	if hubCtx != nil {
		return startAgentViaHub(hubCtx, agentName, task, resume)
	}

	// Local mode
	effectiveProfile := profile
	if effectiveProfile == "" {
		// If no profile flag, check if we have a saved profile for this agent
		effectiveProfile = agent.GetSavedProfile(agentName, grovePath)
	}

	rt := agent.ResolveRuntime(grovePath, agentName, profile)
	mgr := agent.NewManager(rt)

	// Check if already running and we want to attach
	if attach {
		agents, err := rt.List(context.Background(), map[string]string{"scion.name": agentName})
		if err == nil {
			for _, a := range agents {
				if a.Name == agentName || a.ID == agentName || strings.TrimPrefix(a.Name, "/") == agentName {
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

	opts := api.StartOptions{
		Name:      agentName,
		Task:      strings.TrimSpace(task),
		Template:  templateName,
		Profile:   effectiveProfile,
		Image:     resolvedImage,
		GrovePath: grovePath,
		Resume:    resume,
		Detached:  detached,
		NoAuth:    noAuth,
		Branch:    branch,
		Workspace: workspace,
	}

	// Propagate debug mode to container so sciontool logs debug info
	if debugMode {
		opts.Env = map[string]string{
			"SCION_DEBUG": "1",
		}
	}

	// We still might want to show some progress in the CLI
	if !isJSONOutput() {
		if resume {
			fmt.Printf("Resuming agent '%s'...\n", agentName)
		} else {
			fmt.Printf("Starting agent '%s'...\n", agentName)
		}
	}

	info, err := mgr.Start(context.Background(), opts)
	if err != nil {
		return err
	}

	if !isJSONOutput() {
		for _, w := range info.Warnings {
			fmt.Fprintln(os.Stderr, w)
		}
	}

	if !info.Detached {
		fmt.Printf("Attaching to agent '%s'...\n", agentName)
		return rt.Attach(context.Background(), agentName)
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

	fmt.Printf("Agent '%s' %s successfully (ID: %s)\n", agentName, displayStatus, info.ID)

	return nil
}

func startAgentViaHub(hubCtx *HubContext, agentName, task string, resume bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Get the grove ID for this project
	groveID, err := GetGroveID(hubCtx)
	if err != nil {
		return wrapHubError(err)
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
		Name:          agentName,
		GroveID:       groveID,
		Template:      resolvedTemplate,
		RuntimeBrokerID: runtimeBrokerID,
		Task:          task,
		Branch:        branch,
		Workspace:     workspace,
		Resume:        resume,
		Attach:        attach,
	}

	// Build config if we have image override or debug mode
	if agentImage != "" || debugMode {
		req.Config = &hubclient.AgentConfig{
			Image: agentImage,
		}
		// Pass debug mode to agent via env var
		if debugMode {
			req.Config.Env = map[string]string{
				"SCION_DEBUG": "1",
			}
		}
	}

	// Detect non-git grove for workspace bootstrap
	var workspaceFiles []transfer.FileInfo
	if hubCtx.GrovePath != "" {
		groveDir := filepath.Dir(hubCtx.GrovePath) // parent of .scion
		if !util.IsGitRepoDir(groveDir) {
			files, err := transfer.CollectFiles(groveDir, transfer.DefaultExcludePatterns)
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

	if !isJSONOutput() {
		action := "Starting"
		if resume {
			action = "Resuming"
		}
		fmt.Printf("%s agent '%s'...\n", action, agentName)
	}

	resp, err := createAgentWithBrokerResolution(ctx, hubCtx, groveID, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to start agent via Hub: %w", err))
	}

	// Print info line when broker was auto-resolved (not explicitly specified)
	if !isJSONOutput() {
		printAutoResolvedBroker(ctx, hubCtx, runtimeBrokerID, req.RuntimeBrokerID, resp)
	}

	// Workspace bootstrap: upload files and finalize
	if len(workspaceFiles) > 0 && len(resp.UploadURLs) == 0 {
		fmt.Println("Using local workspace on broker.")
	}
	if len(resp.UploadURLs) > 0 && len(workspaceFiles) > 0 {
		fmt.Printf("Uploading workspace (%d files)...\n", len(workspaceFiles))
		tc := transfer.NewClient(nil)
		uploadErr := tc.UploadFiles(ctx, workspaceFiles, resp.UploadURLs, func(file transfer.FileInfo, bytesTransferred int64) error {
			if bytesTransferred == file.Size {
				fmt.Printf("  Uploaded: %s\n", file.Path)
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
		fmt.Printf("Workspace uploaded: %d files\n", finalizeResp.FilesApplied)

		// Poll until agent is running
		fmt.Printf("Waiting for agent '%s' to start...\n", agentName)
		pollCtx, pollCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer pollCancel()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-pollCtx.Done():
				return fmt.Errorf("timed out waiting for agent '%s' to start", agentName)
			case <-ticker.C:
				agent, err := hubCtx.Client.GroveAgents(groveID).Get(pollCtx, agentName)
				if err != nil {
					continue
				}
				if agent.Status == "running" {
					fmt.Printf("Agent '%s' started via Hub.\n", agentName)
					if !attach {
						return nil
					}
					// Fall through to attach logic below
					agentID := agent.ID
					if agentID == "" {
						agentID = agentName
					}
					token := credentials.GetAccessToken(hubCtx.Endpoint)
					if token == "" {
						return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
					}
					fmt.Printf("Attaching to agent '%s' via Hub...\n", agentName)
					return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID)
				}
				if agent.Status == "error" || agent.Status == "stopped" {
					return fmt.Errorf("agent '%s' failed to start (status: %s)", agentName, agent.Status)
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
			result.Details["status"] = resp.Agent.Status
			if resp.Agent.RuntimeBrokerID != "" {
				result.Details["runtimeBrokerId"] = resp.Agent.RuntimeBrokerID
			}
		}
		return outputJSON(result)
	}

	fmt.Printf("Agent '%s' %s via Hub.\n", agentName, displayStatus)
	if resp.Agent != nil {
		fmt.Printf("Agent Slug: %s\n", resp.Agent.Slug)
		fmt.Printf("Status: %s\n", resp.Agent.Status)
	}
	for _, w := range resp.Warnings {
		fmt.Printf("Warning: %s\n", w)
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
	fmt.Printf("Waiting for agent '%s' to be ready...\n", agentName)
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer pollCancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("timed out waiting for agent '%s' to become ready", agentName)
		case <-ticker.C:
			agent, err := hubCtx.Client.GroveAgents(groveID).Get(pollCtx, agentName)
			if err != nil {
				continue // Retry on transient errors
			}
			if agent.Status == "running" {
				// Use the agent's ID from the latest fetch
				if agent.ID != "" {
					agentID = agent.ID
				}
				goto ready
			}
			if agent.Status == "error" || agent.Status == "failed" || agent.Status == "stopped" {
				statusInfo := agent.Status
				if agent.ContainerStatus != "" {
					statusInfo += fmt.Sprintf(", container: %s", agent.ContainerStatus)
				}
				return fmt.Errorf("agent '%s' failed to start (status: %s)", agentName, statusInfo)
			}
		}
	}

ready:
	// Get access token for WebSocket authentication
	token := credentials.GetAccessToken(hubCtx.Endpoint)
	if token == "" {
		return fmt.Errorf("no access token found for Hub\n\nPlease login first: scion hub auth login")
	}

	fmt.Printf("Attaching to agent '%s' via Hub...\n", agentName)
	return wsclient.AttachToAgent(context.Background(), hubCtx.Endpoint, token, agentID)
}

func createAgentWithBrokerResolution(ctx context.Context, hubCtx *HubContext, groveID string, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	for {
		resp, err := hubCtx.Client.GroveAgents(groveID).Create(ctx, req)
		if err == nil {
			return resp, nil
		}

		var apiErr *apiclient.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "no_runtime_broker" {
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
			fmt.Printf("\nMultiple runtime brokers available for grove:\n")
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
		fmt.Printf("Using default broker %s\n", brokerName)
	} else {
		fmt.Printf("Using default broker %s\n", resp.Agent.RuntimeBrokerID)
	}
}
