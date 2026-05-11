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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcache"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	listAll     bool
	listDeleted bool
	listRunning bool
	sortByTime  bool
)

// listCmd represents the list command
var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List running scion agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if Hub should be used
		hubCtx, err := CheckHubAvailability(projectPath)
		if err != nil {
			// Check if this is because Hub is enabled but grove not linked
			if handleUnlinkedProjectPrompt(cmd, args) {
				// User chose to link or disable - retry
				hubCtx, err = CheckHubAvailability(projectPath)
				if err != nil {
					return err
				}
			} else {
				return err
			}
		}

		if hubCtx != nil {
			return listAgentsViaHub(hubCtx)
		}

		// Local mode
		return listAgentsLocal()
	},
}

// listAgentsLocal lists agents using the local runtime
func listAgentsLocal() error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)

	filters := map[string]string{
		"scion.agent": "true",
	}

	if listAll {
		// Cross-grove listing might need a way to find all groves.
		// For now, mgr.List handles current grove and what's provided in filters.
	} else {
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if projectDir != "" {
			filters["scion.project_path"] = projectDir
			filters["scion.project"] = config.GetProjectName(projectDir)
		}
	}

	agents, err := mgr.List(context.Background(), filters)
	if err != nil {
		return err
	}

	return displayAgents(agents, listAll, false)
}

// listAgentsViaHub lists agents using the Hub API
func listAgentsViaHub(hubCtx *HubContext) error {
	PrintUsingHub(hubCtx.Endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListAgentsOptions{
		IncludeDeleted: listDeleted,
	}
	agentSvc := hubCtx.Client.Agents()

	if !listAll {
		// Get the grove ID for the current project
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		opts.ProjectID = projectID
		agentSvc = hubCtx.Client.ProjectAgents(projectID)
	}

	resp, err := agentSvc.List(ctx, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}

	// Convert Hub agents to local AgentInfo format
	agents := make([]api.AgentInfo, len(resp.Agents))
	for i, a := range resp.Agents {
		agents[i] = hubAgentToAgentInfo(a)
	}

	// Update agent name cache for completion
	updateAgentNameCache(resp.Agents)

	// Client-side enrichment: fetch broker/grove names if not provided by Hub
	enrichAgentsClientSide(ctx, hubCtx.Client, agents)

	return displayAgents(agents, listAll, true)
}

// enrichAgentsClientSide populates Grove and RuntimeBrokerName fields client-side
// when the Hub doesn't provide them (for backwards compatibility with older Hubs).
func enrichAgentsClientSide(ctx context.Context, client hubclient.Client, agents []api.AgentInfo) {
	// Collect unique IDs that need enrichment
	brokerIDs := make(map[string]struct{})
	projectIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.RuntimeBrokerName == "" && a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.Project == "" && a.ProjectID != "" {
			projectIDs[a.ProjectID] = struct{}{}
		}
	}

	// Fetch broker names
	brokerNames := make(map[string]string)
	for id := range brokerIDs {
		if broker, err := client.RuntimeBrokers().Get(ctx, id); err == nil {
			brokerNames[id] = broker.Name
		}
	}

	// Fetch grove names
	projectNames := make(map[string]string)
	for id := range projectIDs {
		if grove, err := client.Projects().Get(ctx, id); err == nil {
			projectNames[id] = grove.Name
		}
	}

	// Apply enrichment
	for i := range agents {
		if agents[i].RuntimeBrokerName == "" {
			if name, ok := brokerNames[agents[i].RuntimeBrokerID]; ok {
				agents[i].RuntimeBrokerName = name
			}
		}
		if agents[i].Project == "" {
			if name, ok := projectNames[agents[i].ProjectID]; ok {
				agents[i].Project = name
			}
		}
	}
}

// hubAgentToAgentInfo converts a Hub API Agent to a local AgentInfo
func hubAgentToAgentInfo(a hubclient.Agent) api.AgentInfo {
	// Map to Phase/Activity for api.AgentInfo.
	// Prefer structured Phase/Activity fields; fall back to legacy Status field.
	phase, activity := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)

	// Prefer slug for display name to ensure consistent case-insensitive naming
	displayName := a.Slug
	if displayName == "" {
		displayName = a.Name
	}
	info := api.AgentInfo{
		ID:                a.ID,
		Slug:              a.Slug,
		ContainerID:       a.ContainerID,
		Name:              displayName,
		Template:          a.Template,
		HarnessConfig:     a.HarnessConfig,
		HarnessAuth:       a.HarnessAuth,
		Project:             a.Project,
		ProjectID:           a.ProjectID,
		Labels:            a.Labels,
		Annotations:       a.Annotations,
		Phase:             phase,
		Activity:          activity,
		ContainerStatus:   a.ContainerStatus,
		Image:             a.Image,
		Detached:          a.Detached,
		Runtime:           a.Runtime,
		RuntimeBrokerID:   a.RuntimeBrokerID,
		RuntimeBrokerName: a.RuntimeBrokerName,
		RuntimeBrokerType: a.RuntimeBrokerType,
		RuntimeState:      a.RuntimeState,
		WebPTYEnabled:     a.WebPTYEnabled,
		TaskSummary:       a.TaskSummary,
		Created:           a.Created,
		Updated:           a.Updated,
		LastSeen:          a.LastSeen,
		DeletedAt:         a.DeletedAt,
		CreatedBy:         a.CreatedBy,
		OwnerID:           a.OwnerID,
		Visibility:        a.Visibility,
		StateVersion:      a.StateVersion,
	}

	// Fall back to AppliedConfig fields if top-level fields are empty
	// (for backward compatibility with older Hubs that don't enrich these)
	if info.HarnessConfig == "" && a.AppliedConfig != nil && a.AppliedConfig.HarnessConfig != "" {
		info.HarnessConfig = a.AppliedConfig.HarnessConfig
	}
	if info.HarnessAuth == "" && a.AppliedConfig != nil && a.AppliedConfig.HarnessAuth != "" {
		info.HarnessAuth = a.AppliedConfig.HarnessAuth
	}

	// Convert Kubernetes info if present
	if a.Kubernetes != nil {
		info.Kubernetes = &api.AgentK8sMetadata{
			Cluster:   a.Kubernetes.Cluster,
			Namespace: a.Kubernetes.Namespace,
			PodName:   a.Kubernetes.PodName,
			SyncedAt:  a.Kubernetes.SyncedAt,
		}
	}

	return info
}

// displayAgents displays agents in the requested format
// hubMode indicates if the listing is from Hub (shows BROKER column)
// filterRunningAgents returns only agents whose phase is not stopped or error.
func filterRunningAgents(agents []api.AgentInfo) []api.AgentInfo {
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		p := state.Phase(a.Phase)
		if p == state.PhaseStopped || p == state.PhaseError {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

func displayAgents(agents []api.AgentInfo, all bool, hubMode bool) error {
	if listRunning {
		agents = filterRunningAgents(agents)
	}

	// Resolve human-friendly template names from raw values that may
	// contain cache paths or remote URIs (mirrors the 813307c fix for
	// the tmux footer).
	for i := range agents {
		agents[i].Template = config.FriendlyTemplateName(agents[i].Template)
	}

	if sortByTime {
		sort.Slice(agents, func(i, j int) bool {
			return agents[i].LastSeen.After(agents[j].LastSeen)
		})
	}

	if outputFormat == "json" {
		if agents == nil {
			agents = []api.AgentInfo{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(agents)
	}

	if len(agents) == 0 {
		if all {
			fmt.Println("No active agents found across any groves.")
		} else {
			fmt.Println("No active agents found in the current grove.")
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if hubMode {
		fmt.Fprintln(w, "NAME\tTEMPLATE\tHARNESS-CFG\tRUNTIME\tPROJECT\tBROKER\tPHASE\tCONTAINER\tLAST ACTIVITY")
	} else {
		fmt.Fprintln(w, "NAME\tTEMPLATE\tHARNESS-CFG\tRUNTIME\tPROJECT\tPHASE\tCONTAINER\tLAST ACTIVITY")
	}
	for _, a := range agents {
		phase := a.Phase
		if phase == "" {
			phase = "unknown"
		}
		if phase == string(state.PhaseStopped) && state.Activity(a.Activity).IsTerminal() {
			phase = a.Activity
		}
		containerStatus := a.ContainerStatus
		if containerStatus == "created" && a.ID == "" {
			containerStatus = "none"
		}
		harnessConfig := a.HarnessConfig
		if harnessConfig == "" {
			harnessConfig = "-"
		}
		lastActivity := formatLastActivity(a.Activity, a.LastSeen)
		// Use broker name if available, otherwise fall back to ID
		broker := a.RuntimeBrokerName
		if broker == "" {
			broker = a.RuntimeBrokerID
		}
		if hubMode {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, harnessConfig, a.Runtime, a.Project, broker, phase, containerStatus, lastActivity)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, harnessConfig, a.Runtime, a.Project, phase, containerStatus, lastActivity)
		}
	}
	w.Flush()
	return nil
}

// formatLastSeen formats a timestamp as a human-readable relative time.
func formatLastSeen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	d := time.Since(t)
	if d < 0 {
		return "just now"
	}

	switch {
	case d < time.Minute:
		secs := int(d.Seconds())
		if secs <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%d seconds ago", secs)
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

// formatLastActivity formats a status and timestamp as a combined "activity, time ago" string.
func formatLastActivity(status string, t time.Time) string {
	timePart := formatLastSeen(t)
	if status == "" || status == "IDLE" || status == "idle" {
		return timePart
	}
	if timePart == "-" {
		return status
	}
	return fmt.Sprintf("%s, %s", status, timePart)
}

// handleUnlinkedProjectPrompt checks if the error is due to an unlinked grove and prompts the user.
// Returns true if the user made a choice that might resolve the issue (link or disable).
func handleUnlinkedProjectPrompt(cmd *cobra.Command, args []string) bool {
	// Resolve grove path to check settings
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return false
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return false
	}

	// Only handle this case if Hub is enabled but grove is not linked
	if !settings.IsHubEnabled() {
		return false
	}

	// Check if grove is actually registered on the Hub
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return false
	}

	client, err := getHubClient(settings)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check Hub connectivity first
	if _, err := client.Health(ctx); err != nil {
		return false // Hub not reachable, different error
	}

	// Check if grove is registered — prefer hub.groveId over grove_id
	projectID := settings.GetHubProjectID()
	if projectID == "" {
		projectID = settings.ProjectID
	}
	if projectID == "" {
		projectID = config.GenerateProjectIDForDir(resolvedPath)
	}

	linked, err := isProjectLinkedToHub(ctx, client, projectID)
	if err != nil || linked {
		return false // Error checking or grove is already linked
	}

	// Get grove name for display
	var projectName string
	if isGlobal {
		projectName = "global"
	} else {
		projectName = config.GetProjectName(resolvedPath)
	}

	// Show prompt
	choice := hubsync.ShowProjectLinkOrDisablePrompt(projectName, autoConfirm)

	switch choice {
	case hubsync.LinkOrDisableLink:
		// Run the link command
		if err := runHubLink(cmd, args); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to link grove: %v\n", err)
			return false
		}
		return true
	case hubsync.LinkOrDisableDisable:
		// Disable Hub for this grove
		if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to disable Hub: %v\n", err)
			return false
		}
		statusln("Hub integration disabled for this grove.")
		return true
	default:
		return false
	}
}

// isProjectLinkedToHub checks if a grove is linked to the Hub.
func isProjectLinkedToHub(ctx context.Context, client hubclient.Client, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}

	_, err := client.Projects().Get(ctx, projectID)
	if err != nil {
		errStr := err.Error()
		if containsStr(errStr, "404") || containsStr(errStr, "not found") {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// containsStr is a simple case-sensitive substring check.
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// hubAgentPhaseActivity returns the phase and activity for a Hub agent,
// preferring the structured Phase/Activity fields from the API response
// and falling back to deriving them from the legacy Status field.
func hubAgentPhaseActivity(phase, activity, status string) (string, string) {
	if phase != "" {
		return phase, activity
	}
	return hubStatusToPhaseActivity(status)
}

// hubStatusToPhaseActivity maps a hubclient Status string to Phase and Activity.
// The Hub API may return a single Status field that represents either a phase
// or an activity (e.g. "running", "stopped", "waiting_for_input").
func hubStatusToPhaseActivity(status string) (string, string) {
	// Terminal activities (crashed, limits_exceeded) belong to the stopped phase.
	a := state.Activity(status)
	if a.IsTerminal() {
		return string(state.PhaseStopped), status
	}
	// Check if the status is a known activity (only valid during running phase)
	if a.IsValid() && a != "" {
		return string(state.PhaseRunning), status
	}
	// Check if it is a known phase
	p := state.Phase(status)
	if p.IsValid() {
		return status, ""
	}
	// Unknown value — treat as phase for backward compat
	if status == "" {
		return "", ""
	}
	return status, ""
}

// updateAgentNameCache updates the agent name cache with the given Hub agents.
// This is called after successful Hub API calls to keep the completion cache fresh.
func updateAgentNameCache(agents []hubclient.Agent) {
	if len(agents) == 0 {
		return
	}

	// Extract agent names
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}

	// Generate cache key for the current grove path
	resolvedPath, _ := config.GetResolvedProjectDir(projectPath)
	if resolvedPath == "" {
		return
	}

	cacheKey := agentcache.GenerateCacheKey(resolvedPath)

	// Write to cache (silently ignore errors)
	_ = agentcache.WriteCache(cacheKey, names)
}

func init() {
	rootCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "List all agents across all groves")
	listCmd.Flags().BoolVar(&listDeleted, "deleted", false, "Include soft-deleted agents in listing")
	listCmd.Flags().BoolVarP(&listRunning, "running", "r", false, "Only show agents that are not stopped or errored")
	listCmd.Flags().BoolVarP(&sortByTime, "time", "t", false, "Sort by last activity, most recent first")
}
