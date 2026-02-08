package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/agentcache"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	listAll bool
)

// listCmd represents the list command
var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List running scion agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Check if Hub should be used
		hubCtx, err := CheckHubAvailability(grovePath)
		if err != nil {
			// Check if this is because Hub is enabled but grove not linked
			if handleUnlinkedGrovePrompt(cmd, args) {
				// User chose to link or disable - retry
				hubCtx, err = CheckHubAvailability(grovePath)
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
	rt := runtime.GetRuntime(grovePath, profile)
	mgr := agent.NewManager(rt)

	filters := map[string]string{
		"scion.agent": "true",
	}

	if listAll {
		// Cross-grove listing might need a way to find all groves.
		// For now, mgr.List handles current grove and what's provided in filters.
	} else {
		projectDir, _ := config.GetResolvedProjectDir(grovePath)
		if projectDir != "" {
			filters["scion.grove_path"] = projectDir
			filters["scion.grove"] = config.GetGroveName(projectDir)
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

	opts := &hubclient.ListAgentsOptions{}
	agentSvc := hubCtx.Client.Agents()

	if !listAll {
		// Get the grove ID for the current project
		groveID, err := GetGroveID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		opts.GroveID = groveID
		agentSvc = hubCtx.Client.GroveAgents(groveID)
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
	groveIDs := make(map[string]struct{})
	for _, a := range agents {
		if a.RuntimeBrokerName == "" && a.RuntimeBrokerID != "" {
			brokerIDs[a.RuntimeBrokerID] = struct{}{}
		}
		if a.Grove == "" && a.GroveID != "" {
			groveIDs[a.GroveID] = struct{}{}
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
	groveNames := make(map[string]string)
	for id := range groveIDs {
		if grove, err := client.Groves().Get(ctx, id); err == nil {
			groveNames[id] = grove.Name
		}
	}

	// Apply enrichment
	for i := range agents {
		if agents[i].RuntimeBrokerName == "" {
			if name, ok := brokerNames[agents[i].RuntimeBrokerID]; ok {
				agents[i].RuntimeBrokerName = name
			}
		}
		if agents[i].Grove == "" {
			if name, ok := groveNames[agents[i].GroveID]; ok {
				agents[i].Grove = name
			}
		}
	}
}

// hubAgentToAgentInfo converts a Hub API Agent to a local AgentInfo
func hubAgentToAgentInfo(a hubclient.Agent) api.AgentInfo {
	info := api.AgentInfo{
		ID:                a.ID,
		AgentID:           a.AgentID,
		Name:              a.Name,
		Template:          a.Template,
		Grove:             a.Grove,
		GroveID:           a.GroveID,
		Labels:            a.Labels,
		Annotations:       a.Annotations,
		Status:            a.Status,
		ContainerStatus:   a.ContainerStatus,
		SessionStatus:     a.SessionStatus,
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
		CreatedBy:         a.CreatedBy,
		OwnerID:           a.OwnerID,
		Visibility:        a.Visibility,
		StateVersion:      a.StateVersion,
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
// hubMode indicates if the listing is from Hub (shows LAST SEEN column)
func displayAgents(agents []api.AgentInfo, all bool, hubMode bool) error {
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
		fmt.Fprintln(w, "NAME\tTEMPLATE\tRUNTIME\tGROVE\tBROKER\tAGENT STATUS\tSESSION\tCONTAINER\tLAST SEEN")
	} else {
		fmt.Fprintln(w, "NAME\tTEMPLATE\tRUNTIME\tGROVE\tBROKER\tAGENT STATUS\tSESSION\tCONTAINER")
	}
	for _, a := range agents {
		agentStatus := a.Status
		if agentStatus == "" {
			agentStatus = "IDLE"
		}
		sessionStatus := a.SessionStatus
		if sessionStatus == "" {
			sessionStatus = "-"
		}
		containerStatus := a.ContainerStatus
		if containerStatus == "created" && a.ID == "" {
			containerStatus = "none"
		}
		// Use broker name if available, otherwise fall back to ID
		broker := a.RuntimeBrokerName
		if broker == "" {
			broker = a.RuntimeBrokerID
		}
		if hubMode {
			lastSeen := formatLastSeen(a.LastSeen)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, a.Runtime, a.Grove, broker, agentStatus, sessionStatus, containerStatus, lastSeen)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Template, a.Runtime, a.Grove, broker, agentStatus, sessionStatus, containerStatus)
		}
	}
	w.Flush()
	return nil
}

// formatLastSeen formats a timestamp as a human-readable relative time
func formatLastSeen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	d := time.Since(t)
	if d < 0 {
		return "now"
	}

	switch {
	case d < time.Minute:
		secs := int(d.Seconds())
		if secs <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%ds ago", secs)
	case d < time.Hour:
		mins := int(d.Minutes())
		if mins == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", mins)
	case d < 24*time.Hour:
		hours := int(d.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// handleUnlinkedGrovePrompt checks if the error is due to an unlinked grove and prompts the user.
// Returns true if the user made a choice that might resolve the issue (link or disable).
func handleUnlinkedGrovePrompt(cmd *cobra.Command, args []string) bool {
	// Resolve grove path to check settings
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
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

	// Check if grove is registered
	groveID := settings.GroveID
	if groveID == "" {
		groveID = config.GenerateGroveIDForDir(resolvedPath)
	}

	linked, err := isGroveLinkedToHub(ctx, client, groveID)
	if err != nil || linked {
		return false // Error checking or grove is already linked
	}

	// Get grove name for display
	var groveName string
	if isGlobal {
		groveName = "global"
	} else {
		groveName = config.GetGroveName(resolvedPath)
	}

	// Show prompt
	choice := hubsync.ShowLinkOrDisablePrompt(groveName, autoConfirm)

	switch choice {
	case hubsync.LinkOrDisableLink:
		// Run the link command
		if err := runHubLink(cmd, args); err != nil {
			fmt.Printf("Failed to link grove: %v\n", err)
			return false
		}
		return true
	case hubsync.LinkOrDisableDisable:
		// Disable Hub for this grove
		if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
			fmt.Printf("Failed to disable Hub: %v\n", err)
			return false
		}
		fmt.Println("Hub integration disabled for this grove.")
		return true
	default:
		return false
	}
}

// isGroveLinkedToHub checks if a grove is registered on the Hub.
func isGroveLinkedToHub(ctx context.Context, client hubclient.Client, groveID string) (bool, error) {
	if groveID == "" {
		return false, nil
	}

	_, err := client.Groves().Get(ctx, groveID)
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
	resolvedPath, _ := config.GetResolvedProjectDir(grovePath)
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
}
