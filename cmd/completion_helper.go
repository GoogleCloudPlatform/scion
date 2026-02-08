package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/agentcache"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/credentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	// completionTimeout is the maximum time to wait for Hub API response during completion.
	// Shell completions must be fast, so we use a short timeout.
	completionTimeout = 500 * time.Millisecond
)

func getAgentNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var names []string
	seen := make(map[string]bool)

	// Helper to add names with deduplication and prefix matching
	addNames := func(agentNames []string) {
		for _, name := range agentNames {
			if strings.HasPrefix(name, toComplete) && !seen[name] {
				names = append(names, name)
				seen[name] = true
			}
		}
	}

	// Helper to scan a grove directory for local agents
	scanGrove := func(groveDir string) {
		if groveDir == "" {
			return
		}
		agentsDir := filepath.Join(groveDir, "agents")
		entries, err := os.ReadDir(agentsDir)
		if err != nil {
			return
		}

		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), toComplete) {
				// Verify it looks like an agent (has scion-agent.json)
				// This check might be too slow for thousands of dirs, but for typical usage it's fine
				// and prevents completing random directories.
				if _, err := os.Stat(filepath.Join(agentsDir, e.Name(), "scion-agent.json")); err == nil {
					if !seen[e.Name()] {
						names = append(names, e.Name())
						seen[e.Name()] = true
					}
				}
			}
		}
	}

	// Try to get grove from flag if specified by user in the command line so far
	currentGrovePath, _ := cmd.Flags().GetString("grove")

	// If global flag is set
	global, _ := cmd.Flags().GetBool("global")
	if global {
		currentGrovePath = "global"
	}

	resolvedPath, _ := config.GetResolvedProjectDir(currentGrovePath)

	// 1. Scan local/current grove
	scanGrove(resolvedPath)

	// 2. Scan global grove if not already scanned
	globalDir, _ := config.GetGlobalDir()
	if globalDir != "" && globalDir != resolvedPath {
		scanGrove(globalDir)
	}

	// 3. Fetch Hub agents (if enabled)
	// Check --no-hub flag
	noHubFlag, _ := cmd.Flags().GetBool("no-hub")
	if !noHubFlag {
		hubAgents := fetchHubAgentsForCompletion(resolvedPath)
		addNames(hubAgents)
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

// fetchHubAgentsForCompletion fetches agent names from the Hub for shell completion.
// It uses a short timeout and falls back to cache if the Hub is slow or unavailable.
// This function is designed to be silent - it never returns errors to avoid breaking completion.
func fetchHubAgentsForCompletion(grovePath string) []string {
	// Load settings to check if Hub is enabled
	settings, err := config.LoadSettings(grovePath)
	if err != nil {
		return nil
	}

	// Check if Hub is enabled in settings
	if !settings.IsHubEnabled() {
		return nil
	}

	// Get Hub endpoint
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return nil
	}

	// Generate cache key for this grove
	cacheKey := agentcache.GenerateCacheKey(grovePath)

	// Try to fetch from Hub with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()

	agents, err := fetchHubAgents(ctx, endpoint, settings)
	if err == nil && len(agents) > 0 {
		// Success - update cache and return
		_ = agentcache.WriteCache(cacheKey, agents)
		return agents
	}

	// Hub call failed or returned empty - try cache
	cached, _ := agentcache.ReadCache(cacheKey)
	return cached
}

// fetchHubAgents fetches agent names from the Hub API.
// Returns only agent names, not full agent objects.
func fetchHubAgents(ctx context.Context, endpoint string, settings *config.Settings) ([]string, error) {
	// Create Hub client with short timeout for completion
	opts := []hubclient.Option{
		hubclient.WithTimeout(completionTimeout),
	}

	// Add authentication - non-interactive only
	// Check settings for explicit auth first
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

	// Check for OAuth credentials (non-interactive)
	if !authConfigured {
		if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
			opts = append(opts, hubclient.WithBearerToken(accessToken))
			authConfigured = true
		}
	}

	// Fallback to auto dev auth (checks env var and file)
	if !authConfigured {
		opts = append(opts, hubclient.WithAutoDevAuth())
	}

	client, err := hubclient.New(endpoint, opts...)
	if err != nil {
		return nil, err
	}

	// Determine grove ID for filtering
	var agentService hubclient.AgentService
	if settings.Hub != nil && settings.Hub.GroveID != "" {
		agentService = client.GroveAgents(settings.Hub.GroveID)
	} else {
		agentService = client.Agents()
	}

	// Fetch agents
	resp, err := agentService.List(ctx, nil)
	if err != nil {
		return nil, err
	}

	// Extract just the names
	var names []string
	for _, agent := range resp.Agents {
		if agent.Name != "" {
			names = append(names, agent.Name)
		}
	}

	return names, nil
}
