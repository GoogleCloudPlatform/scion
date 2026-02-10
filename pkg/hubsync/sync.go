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

package hubsync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/credentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/util"
	"gopkg.in/yaml.v3"
)

// debugf prints a debug message if debug mode is enabled.
func debugf(format string, args ...interface{}) {
	util.DebugfTagged("hubsync", format, args...)
}

// AgentRef holds both name and ID for an agent.
// Name is used for display, ID is used for API calls.
type AgentRef struct {
	Name string
	ID   string
}

// SyncResult represents the result of comparing local and Hub agents.
type SyncResult struct {
	ToRegister []string   // Local agents to register on Hub
	ToRemove   []AgentRef // Hub agents (for this broker) to remove (with IDs for API)
	InSync     []string   // Agents already in sync
	Pending    []AgentRef // Hub agents in pending status (not yet started, no local artifacts expected)
}

// IsInSync returns true if there are no agents to sync.
func (r *SyncResult) IsInSync() bool {
	return len(r.ToRegister) == 0 && len(r.ToRemove) == 0
}

// ExcludeAgent returns a new SyncResult with the specified agent excluded from
// ToRegister, ToRemove, and Pending lists. This is used when operating on a specific agent
// so that the sync check doesn't require syncing the target of the operation.
func (r *SyncResult) ExcludeAgent(agentName string) *SyncResult {
	if agentName == "" {
		return r
	}

	result := &SyncResult{
		InSync: r.InSync,
	}

	for _, name := range r.ToRegister {
		if name != agentName {
			result.ToRegister = append(result.ToRegister, name)
		}
	}

	for _, ref := range r.ToRemove {
		if ref.Name != agentName {
			result.ToRemove = append(result.ToRemove, ref)
		}
	}

	for _, ref := range r.Pending {
		if ref.Name != agentName {
			result.Pending = append(result.Pending, ref)
		}
	}

	return result
}

// HubContext holds the context for Hub operations.
type HubContext struct {
	Client     hubclient.Client
	Endpoint   string
	Settings   *config.Settings
	GroveID    string
	BrokerID string
	GrovePath  string
	IsGlobal   bool
}

// EnsureHubReadyOptions configures the behavior of EnsureHubReady.
type EnsureHubReadyOptions struct {
	// AutoConfirm auto-confirms all prompts.
	AutoConfirm bool
	// NoHub disables Hub integration for this invocation.
	NoHub bool
	// SkipSync skips agent synchronization check.
	SkipSync bool
	// TargetAgent is the agent being operated on. If set, this agent is excluded
	// from sync requirements since the current operation will change its state.
	// For delete: the agent won't be required to be registered on Hub first.
	// For create: the agent won't be required to be removed from Hub first.
	TargetAgent string
}

// EnsureHubReady performs all Hub pre-flight checks before agent operations.
// Returns HubContext if Hub is ready, nil if Hub should not be used.
// This function will:
// 1. Check --no-hub flag
// 2. Load settings
// 3. Check hub.local_only setting
// 4. Check hub.enabled setting
// 5. Ensure grove_id exists (generate if missing)
// 6. Check Hub connectivity
// 7. Check grove registration (prompt to register if not)
// 8. Compare and sync agents (unless SkipSync is true)
func EnsureHubReady(grovePath string, opts EnsureHubReadyOptions) (*HubContext, error) {
	debugf("EnsureHubReady: grovePath=%s, opts=%+v", grovePath, opts)

	// Check if --no-hub flag is set
	if opts.NoHub {
		debugf("NoHub flag set, returning nil")
		return nil, nil
	}

	// Resolve grove path
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve grove path: %w", err)
	}

	// If no explicit grove path was given and we fell back to global,
	// that means no project grove was found. In this case, skip Hub sync
	// to avoid trying to register a non-existent grove. The user should
	// either run 'scion init' or use --global explicitly.
	if grovePath == "" && isGlobal {
		debugf("No project grove found (fell back to global), skipping Hub sync")
		return nil, nil
	}

	// Clean up stale broker credentials from grove settings.
	// These should only exist in global settings, not grove-specific settings.
	// Earlier versions incorrectly wrote them to grove settings.
	if !isGlobal {
		cleanupGroveBrokerCredentials(resolvedPath)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
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
	endpoint := getEndpoint(settings)
	if endpoint == "" {
		return nil, wrapHubError(fmt.Errorf("Hub is enabled but no endpoint configured.\n\nConfigure via: scion config set hub.endpoint <url>"))
	}

	// Ensure grove_id exists
	groveID := settings.GroveID
	if groveID == "" {
		// Generate grove_id for groves that don't have one
		groveID = config.GenerateGroveIDForDir(filepath.Dir(resolvedPath))
		if err := config.UpdateSetting(resolvedPath, "grove_id", groveID, isGlobal); err != nil {
			return nil, fmt.Errorf("failed to save grove_id: %w", err)
		}
		// Reload settings to get the updated grove_id
		settings, err = config.LoadSettings(resolvedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to reload settings: %w", err)
		}
	}

	// Create Hub client
	client, err := createHubClient(settings, endpoint)
	if err != nil {
		return nil, wrapHubError(fmt.Errorf("failed to create Hub client: %w", err))
	}

	// Check health
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		return nil, wrapHubError(fmt.Errorf("Hub at %s is not responding: %w", endpoint, err))
	}

	// Get broker ID
	brokerID := ""
	if settings.Hub != nil {
		brokerID = settings.Hub.BrokerID
	}

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  endpoint,
		Settings:  settings,
		GroveID:   groveID,
		BrokerID:    brokerID,
		GrovePath: resolvedPath,
		IsGlobal:  isGlobal,
	}

	debugf("HubContext created: endpoint=%s, groveID=%s, brokerID=%s, grovePath=%s, isGlobal=%v",
		endpoint, groveID, brokerID, resolvedPath, isGlobal)

	// Check grove registration
	registered, err := isGroveRegistered(ctx, hubCtx)
	if err != nil {
		return nil, wrapHubError(err)
	}

	if !registered {
		// Get grove name for the prompt
		groveName := getGroveName(resolvedPath, isGlobal)

		// Check for existing groves with the same name
		matches, err := findMatchingGroves(ctx, hubCtx, groveName)
		if err != nil {
			debugf("Warning: failed to search for matching groves: %v", err)
			// Continue with registration - the hub will handle matching
		}

		if len(matches) > 0 {
			// Found matching groves - ask user what to do
			choice, selectedID := ShowMatchingGrovesPrompt(groveName, matches, opts.AutoConfirm)
			switch choice {
			case GroveChoiceCancel:
				return nil, fmt.Errorf("registration cancelled")
			case GroveChoiceLink:
				// Update local grove_id to the selected grove
				if err := config.UpdateSetting(resolvedPath, "grove_id", selectedID, isGlobal); err != nil {
					return nil, fmt.Errorf("failed to update local grove_id: %w", err)
				}
				hubCtx.GroveID = selectedID
				debugf("Updated local grove_id to: %s", selectedID)
			case GroveChoiceRegisterNew:
				// Generate a new grove ID to avoid linking to existing grove
				newID := config.GenerateGroveIDForDir(filepath.Dir(resolvedPath))
				if err := config.UpdateSetting(resolvedPath, "grove_id", newID, isGlobal); err != nil {
					return nil, fmt.Errorf("failed to update local grove_id: %w", err)
				}
				hubCtx.GroveID = newID
				debugf("Generated new grove_id: %s", newID)
			}
		} else {
			// No matching groves - ask for confirmation
			if !ShowLinkPrompt(groveName, opts.AutoConfirm) {
				return nil, fmt.Errorf("grove must be linked to Hub to perform this operation\n\n" +
					"Link this grove: scion hub link\n" +
					"Or use local-only mode: scion --no-hub <command>")
			}
		}

		// Register the grove
		if err := registerGrove(context.Background(), hubCtx, groveName, isGlobal); err != nil {
			return nil, wrapHubError(fmt.Errorf("failed to register grove: %w", err))
		}
		// Reload settings to get updated broker ID and grove_id
		settings, err = config.LoadSettings(resolvedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to reload settings: %w", err)
		}
		hubCtx.Settings = settings
		hubCtx.GroveID = settings.GroveID
		if settings.Hub != nil {
			hubCtx.BrokerID = settings.Hub.BrokerID
		}
	}

	// Skip sync if requested
	if opts.SkipSync {
		return hubCtx, nil
	}

	// Compare and sync agents
	syncResult, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		return nil, wrapHubError(fmt.Errorf("failed to compare agents: %w", err))
	}

	// If we're operating on a specific agent, exclude it from sync requirements.
	// This allows operations like delete to proceed without first syncing the
	// target agent (e.g., you can delete a local-only agent without registering it).
	effectiveSyncResult := syncResult
	if opts.TargetAgent != "" {
		effectiveSyncResult = syncResult.ExcludeAgent(opts.TargetAgent)
	}

	if !effectiveSyncResult.IsInSync() {
		// Check if there are agents to register but no brokers available
		if len(effectiveSyncResult.ToRegister) > 0 {
			hasOnlineBroker, err := checkBrokerAvailability(context.Background(), hubCtx)
			if err != nil {
				debugf("Warning: failed to check broker availability: %v", err)
				// Continue with sync attempt - the error will surface during ExecuteSync
			} else if !hasOnlineBroker {
				// No brokers available - print warning and skip sync
				fmt.Println()
				fmt.Println("Warning: No runtime brokers are available for this grove.")
				fmt.Println("Agent sync cannot be performed without an online broker.")
				fmt.Println()
				fmt.Println("Local agents not synced to Hub:")
				for _, name := range effectiveSyncResult.ToRegister {
					fmt.Printf("  + %s\n", name)
				}
				fmt.Println()
				fmt.Println("To sync agents, ensure a runtime broker is running and connected.")
				fmt.Println()
				// Continue without syncing - this allows read operations like list to proceed
				return hubCtx, nil
			}
		}

		if ShowSyncPlan(effectiveSyncResult, opts.AutoConfirm) {
			if err := ExecuteSync(context.Background(), hubCtx, effectiveSyncResult, opts.AutoConfirm); err != nil {
				return nil, wrapHubError(fmt.Errorf("failed to sync agents: %w", err))
			}
		} else {
			return nil, fmt.Errorf("agents must be synchronized with Hub to perform this operation\n\n" +
				"Sync agents: scion hub sync\n" +
				"Or use local-only mode: scion --no-hub <command>")
		}
	}

	return hubCtx, nil
}

// checkBrokerAvailability checks if there are any online brokers for the grove.
func checkBrokerAvailability(ctx context.Context, hubCtx *HubContext) (bool, error) {
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := hubCtx.Client.Groves().ListProviders(ctxTimeout, hubCtx.GroveID)
	if err != nil {
		return false, fmt.Errorf("failed to list grove providers: %w", err)
	}

	for _, provider := range resp.Providers {
		if provider.Status == "online" {
			return true, nil
		}
	}

	return false, nil
}

// CompareAgents compares local agents with Hub agents for the current broker.
func CompareAgents(ctx context.Context, hubCtx *HubContext) (*SyncResult, error) {
	result := &SyncResult{}

	debugf("CompareAgents starting: groveID=%s, brokerID=%s, grovePath=%s",
		hubCtx.GroveID, hubCtx.BrokerID, hubCtx.GrovePath)

	// Get local agents
	localAgents, err := GetLocalAgents(hubCtx.GrovePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get local agents: %w", err)
	}
	debugf("Local agents found: %v", localAgents)

	// Get Hub agents for this grove and broker
	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	opts := &hubclient.ListAgentsOptions{
		GroveID:       hubCtx.GroveID,
		RuntimeBrokerID: hubCtx.BrokerID,
	}

	resp, err := hubCtx.Client.GroveAgents(hubCtx.GroveID).List(ctxTimeout, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list Hub agents: %w", err)
	}

	debugf("Hub agents found: %d total", len(resp.Agents))
	for _, a := range resp.Agents {
		debugf("  - Hub agent: name=%s, id=%s, status=%s, brokerID=%s",
			a.Name, a.ID, a.Status, a.RuntimeBrokerID)
	}

	// Build map of Hub agents
	hubAgentMap := make(map[string]bool)
	for _, a := range resp.Agents {
		hubAgentMap[a.Name] = true
	}

	// Build map of local agents
	localAgentMap := make(map[string]bool)
	for _, name := range localAgents {
		localAgentMap[name] = true
	}

	// Find agents to register (local but not on Hub)
	for _, name := range localAgents {
		if hubAgentMap[name] {
			result.InSync = append(result.InSync, name)
		} else {
			result.ToRegister = append(result.ToRegister, name)
		}
	}

	// Find agents to remove (on Hub for this broker but not local)
	// Skip agents in "pending" status - these are created on Hub but not yet started,
	// so they're expected to not have local representation until the container is started.
	for _, a := range resp.Agents {
		if !localAgentMap[a.Name] {
			if a.Status == "pending" {
				// Track pending agents separately - they don't require sync
				result.Pending = append(result.Pending, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) is pending, not requiring sync", a.Name, a.ID)
			} else {
				result.ToRemove = append(result.ToRemove, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) on Hub but not local, marking for removal", a.Name, a.ID)
			}
		}
	}

	debugf("Sync result: toRegister=%v, toRemove=%d, pending=%d, inSync=%d",
		result.ToRegister, len(result.ToRemove), len(result.Pending), len(result.InSync))

	return result, nil
}

// ExecuteSync performs the synchronization based on SyncResult.
func ExecuteSync(ctx context.Context, hubCtx *HubContext, result *SyncResult, autoConfirm bool) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	debugf("ExecuteSync starting: groveID=%s, brokerID=%s", hubCtx.GroveID, hubCtx.BrokerID)

	// Register local agents on Hub
	// Note: We don't specify a runtime broker ID - the hub will resolve it based on
	// available grove providers (single provider = auto-select, multiple = error)
	for _, name := range result.ToRegister {
		fmt.Printf("Registering agent '%s' on Hub...\n", name)
		debugf("Creating agent: name=%s, groveID=%s (hub will resolve runtime broker)", name, hubCtx.GroveID)
		req := &hubclient.CreateAgentRequest{
			Name:    name,
			GroveID: hubCtx.GroveID,
		}

		for {
			resp, err := hubCtx.Client.GroveAgents(hubCtx.GroveID).Create(ctxTimeout, req)
			if err == nil {
				debugf("Agent '%s' created with ID: %s", name, resp.Agent.ID)
				break
			}

			var apiErr *apiclient.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != "no_runtime_broker" {
				debugf("Failed to register agent '%s': %v", name, err)
				return fmt.Errorf("failed to register agent '%s': %w", name, err)
			}

			// Handle ambiguous broker
			availableBrokers, ok := apiErr.Details["availableBrokers"].([]interface{})
			if !ok || len(availableBrokers) == 0 {
				return fmt.Errorf("failed to register agent '%s': %w", name, err)
			}

			// Only prompt if interactive and not auto-confirm
			if autoConfirm || !util.IsTerminal() {
				brokerWord := "broker"
				if len(availableBrokers) > 1 {
					brokerWord = "brokers"
				}
				return fmt.Errorf("failed to register agent '%s': %d runtime %s available, specify a broker via Hub config or --broker flag (original error: %w)", name, len(availableBrokers), brokerWord, err)
			}

			reader := bufio.NewReader(os.Stdin)

			if len(availableBrokers) == 1 {
				// Single broker available - simple confirmation
				brokerMap, _ := availableBrokers[0].(map[string]interface{})
				brokerName, _ := brokerMap["name"].(string)
				status, _ := brokerMap["status"].(string)
				isDefault, _ := brokerMap["isDefault"].(bool)

				defaultLabel := ""
				if isDefault {
					defaultLabel = " (default)"
				}
				fmt.Printf("\nUse runtime broker %s (%s)%s for agent '%s'? [y/N]: ", brokerName, status, defaultLabel, name)
				input, err := reader.ReadString('\n')
				if err != nil {
					return fmt.Errorf("failed to read input: %w", err)
				}
				input = strings.TrimSpace(strings.ToLower(input))
				if input != "y" && input != "yes" {
					return fmt.Errorf("registration cancelled")
				}
				req.RuntimeBrokerID, _ = brokerMap["id"].(string)
			} else {
				// Multiple brokers - selection prompt
				fmt.Printf("\nMultiple runtime brokers available for grove:\n")
				for i, h := range availableBrokers {
					brokerMap, _ := h.(map[string]interface{})
					brokerName, _ := brokerMap["name"].(string)
					status, _ := brokerMap["status"].(string)
					isDefault, _ := brokerMap["isDefault"].(bool)
					defaultLabel := ""
					if isDefault {
						defaultLabel = " (default)"
					}
					fmt.Printf("  [%d] %s (%s)%s\n", i+1, brokerName, status, defaultLabel)
				}
				fmt.Println()

				for {
					fmt.Print("Select a broker for agent registration (or 'c' to cancel): ")
					input, err := reader.ReadString('\n')
					if err != nil {
						return fmt.Errorf("failed to read input: %w", err)
					}

					input = strings.TrimSpace(strings.ToLower(input))
					if input == "c" || input == "cancel" {
						return fmt.Errorf("registration cancelled")
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

	// Remove Hub agents that are not on this broker
	for _, ref := range result.ToRemove {
		fmt.Printf("Removing agent '%s' from Hub...\n", ref.Name)
		debugf("Deleting agent via grove-scoped endpoint: name=%s, id=%s, groveID=%s",
			ref.Name, ref.ID, hubCtx.GroveID)
		// Use grove-scoped endpoint which supports both ID and slug lookup
		if err := hubCtx.Client.Groves().DeleteAgent(ctxTimeout, hubCtx.GroveID, ref.ID, nil); err != nil {
			debugf("Failed to remove agent '%s' (id=%s): %v", ref.Name, ref.ID, err)
			return fmt.Errorf("failed to remove agent '%s': %w", ref.Name, err)
		}
		debugf("Agent '%s' removed successfully", ref.Name)
	}

	if len(result.ToRegister) > 0 || len(result.ToRemove) > 0 {
		fmt.Println("Agent synchronization complete.")
	}

	return nil
}

// GetLocalAgents returns agent names from .scion/agents/.
func GetLocalAgents(grovePath string) ([]string, error) {
	agentsDir := filepath.Join(grovePath, "agents")

	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var agents []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Check if it has a scion-agent config file (YAML or JSON)
		yamlPath := filepath.Join(agentsDir, entry.Name(), "scion-agent.yaml")
		jsonPath := filepath.Join(agentsDir, entry.Name(), "scion-agent.json")
		if _, err := os.Stat(yamlPath); err == nil {
			agents = append(agents, entry.Name())
		} else if _, err := os.Stat(jsonPath); err == nil {
			agents = append(agents, entry.Name())
		}
	}

	return agents, nil
}

// isGroveRegistered checks if the grove is registered with the Hub.
func isGroveRegistered(ctx context.Context, hubCtx *HubContext) (bool, error) {
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Try to get the grove by ID
	_, err := hubCtx.Client.Groves().Get(ctxTimeout, hubCtx.GroveID)
	if err != nil {
		// Check if it's a "not found" error
		errStr := err.Error()
		if containsIgnoreCase(errStr, "404") || containsIgnoreCase(errStr, "not found") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check grove registration: %w", err)
	}

	return true, nil
}

// findMatchingGroves finds groves with the same name on the Hub.
func findMatchingGroves(ctx context.Context, hubCtx *HubContext, groveName string) ([]GroveMatch, error) {
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := hubCtx.Client.Groves().List(ctxTimeout, &hubclient.ListGrovesOptions{
		Name: groveName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for matching groves: %w", err)
	}

	var matches []GroveMatch
	for _, g := range resp.Groves {
		matches = append(matches, GroveMatch{
			ID:        g.ID,
			Name:      g.Name,
			GitRemote: g.GitRemote,
		})
	}

	return matches, nil
}

// registerGrove registers the grove with the Hub.
func registerGrove(ctx context.Context, hubCtx *HubContext, groveName string, isGlobal bool) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get git remote (optional)
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemote()
	}

	// Get hostname
	brokerName, err := os.Hostname()
	if err != nil {
		brokerName = "local-broker"
	}

	req := &hubclient.RegisterGroveRequest{
		ID:        hubCtx.GroveID,
		Name:      groveName,
		GitRemote: util.NormalizeGitRemote(gitRemote),
		Path:      hubCtx.GrovePath,
		Broker: &hubclient.BrokerInfo{
			ID:   hubCtx.BrokerID,
			Name: brokerName,
		},
	}

	resp, err := hubCtx.Client.Groves().Register(ctxTimeout, req)
	if err != nil {
		return err
	}

	// Save the broker token and ID to GLOBAL settings only.
	// These are broker-level credentials, not grove-specific.
	globalDir, globalErr := config.GetGlobalDir()
	if globalErr != nil {
		fmt.Printf("Warning: failed to get global directory: %v\n", globalErr)
	} else {
		if resp.BrokerToken != "" {
			if err := config.UpdateSetting(globalDir, "hub.brokerToken", resp.BrokerToken, true); err != nil {
				fmt.Printf("Warning: failed to save broker token: %v\n", err)
			}
		}
		if resp.Broker != nil && resp.Broker.ID != "" {
			if err := config.UpdateSetting(globalDir, "hub.brokerId", resp.Broker.ID, true); err != nil {
				fmt.Printf("Warning: failed to save broker ID: %v\n", err)
			}
		}
	}

	if resp.Created {
		fmt.Printf("Created new grove: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
	} else {
		fmt.Printf("Linked to existing grove: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
		// Update local grove_id to match the hub grove's ID
		if resp.Grove.ID != hubCtx.GroveID {
			if err := config.UpdateSetting(hubCtx.GrovePath, "grove_id", resp.Grove.ID, isGlobal); err != nil {
				fmt.Printf("Warning: failed to update local grove_id: %v\n", err)
			} else {
				hubCtx.GroveID = resp.Grove.ID
			}
		}
	}
	if resp.Broker != nil {
		fmt.Printf("Broker registered: %s (ID: %s)\n", resp.Broker.Name, resp.Broker.ID)
	}

	return nil
}

// getGroveName returns a human-readable grove name.
func getGroveName(grovePath string, isGlobal bool) string {
	if isGlobal {
		return "global"
	}
	gitRemote := util.GetGitRemote()
	if gitRemote != "" {
		return util.ExtractRepoName(gitRemote)
	}
	return filepath.Base(filepath.Dir(grovePath))
}

// getEndpoint returns the Hub endpoint from settings.
func getEndpoint(settings *config.Settings) string {
	if settings.Hub != nil {
		return settings.Hub.Endpoint
	}
	return ""
}

// createHubClient creates a new Hub client with proper authentication.
func createHubClient(settings *config.Settings, endpoint string) (hubclient.Client, error) {
	var opts []hubclient.Option

	// Add authentication - check in priority order
	authConfigured := false
	if settings.Hub != nil {
		if settings.Hub.Token != "" {
			opts = append(opts, hubclient.WithBearerToken(settings.Hub.Token))
			authConfigured = true
		} else if settings.Hub.APIKey != "" {
			opts = append(opts, hubclient.WithAPIKey(settings.Hub.APIKey))
			authConfigured = true
		}
	}

	// Check for OAuth credentials from scion hub auth login
	if !authConfigured {
		if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
			opts = append(opts, hubclient.WithBearerToken(accessToken))
			authConfigured = true
		}
	}

	// Fallback to auto dev auth
	if !authConfigured {
		opts = append(opts, hubclient.WithAutoDevAuth())
	}

	opts = append(opts, hubclient.WithTimeout(30*time.Second))

	return hubclient.New(endpoint, opts...)
}

// wrapHubError wraps a Hub error with guidance to disable Hub integration.
func wrapHubError(err error) error {
	if apiclient.IsUnauthorizedError(err) {
		return fmt.Errorf("authentication failed, login to hub with 'scion hub auth login'")
	}
	return fmt.Errorf("%w\n\nTo use local-only mode, use: scion --no-hub <command>", err)
}

// containsIgnoreCase checks if a string contains a substring (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// cleanupGroveBrokerCredentials removes stale hub.brokerId and hub.brokerToken from
// grove settings. These should only exist in global settings, not grove-specific.
// Earlier versions of scion incorrectly wrote them to grove settings.
func cleanupGroveBrokerCredentials(grovePath string) {
	settingsPath := config.GetSettingsPath(grovePath)
	if settingsPath == "" {
		return
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}

	// Check if the file contains brokerId or brokerToken
	content := string(data)
	if !strings.Contains(content, "brokerId") && !strings.Contains(content, "brokerToken") {
		return
	}

	// Parse and check if hub section has these keys
	var settings map[string]interface{}
	ext := filepath.Ext(settingsPath)
	isYAML := ext == ".yaml" || ext == ".yml"

	if isYAML {
		if err := yaml.Unmarshal(data, &settings); err != nil {
			debugf("Warning: failed to parse grove settings YAML: %v", err)
			return
		}
	} else {
		if err := util.UnmarshalJSONC(data, &settings); err != nil {
			debugf("Warning: failed to parse grove settings JSON: %v", err)
			return
		}
	}

	hubSection, ok := settings["hub"].(map[string]interface{})
	if !ok {
		return
	}

	modified := false
	if _, hasHostId := hubSection["brokerId"]; hasHostId {
		delete(hubSection, "brokerId")
		modified = true
		debugf("Removed stale hub.brokerId from grove settings")
	}
	if _, hasHostToken := hubSection["brokerToken"]; hasHostToken {
		delete(hubSection, "brokerToken")
		modified = true
		debugf("Removed stale hub.brokerToken from grove settings")
	}

	if !modified {
		return
	}

	// Write back the cleaned settings in the same format
	var newData []byte
	if isYAML {
		newData, err = yaml.Marshal(settings)
		if err != nil {
			debugf("Warning: failed to marshal cleaned settings as YAML: %v", err)
			return
		}
	} else {
		newData, err = json.MarshalIndent(settings, "", "  ")
		if err != nil {
			debugf("Warning: failed to marshal cleaned settings as JSON: %v", err)
			return
		}
	}

	if err := os.WriteFile(settingsPath, newData, 0644); err != nil {
		debugf("Warning: failed to write cleaned settings: %v", err)
	}
}
