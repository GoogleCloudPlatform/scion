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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcache"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

const (
	// completionTimeout is the maximum time to wait for Hub API response during completion.
	// Shell completions must be fast, so we use a short timeout.
	completionTimeout = 500 * time.Millisecond
)

// getMultiAgentNames provides completion for commands that accept multiple agent names.
// It excludes already-provided args from the suggestions.
func getMultiAgentNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeAgentNames(cmd, args, toComplete)
}

func getAgentNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeAgentNames(cmd, args, toComplete)
}

func completeAgentNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	var names []string
	seen := make(map[string]bool)

	// Exclude already-provided arguments from suggestions
	for _, arg := range args {
		seen[arg] = true
	}

	// Helper to add names with deduplication and prefix matching
	addNames := func(agentNames []string) {
		for _, name := range agentNames {
			if strings.HasPrefix(name, toComplete) && !seen[name] {
				names = append(names, name)
				seen[name] = true
			}
		}
	}

	// Try to get project path from flag if specified by user in the command line so far
	currentProjectPath, _ := cmd.Flags().GetString("project")

	// If global flag is set
	global, _ := cmd.Flags().GetBool("global")
	if global {
		currentProjectPath = "global"
	}

	resolvedPath, _ := config.GetResolvedProjectDir(currentProjectPath)

	// Fetch Hub agents (when a hub endpoint is configured)
	addNames(fetchHubAgentsForCompletion(cmd, resolvedPath))

	return names, cobra.ShellCompDirectiveNoFileComp
}

// completionTargetResolver resolves the hub of a shell completion
// (resolveCompletionTarget). It is assigned in init(), not called
// directly: the resolver reads command variables (commandRequiresProject
// reads reincarnateCmd), and the completion functions are themselves set on
// command variables (ValidArgsFunction), so a direct call would be a
// package initialization cycle.
var completionTargetResolver func(cmd *cobra.Command) error

func init() { completionTargetResolver = resolveCompletionTarget }

// fetchHubAgentsForCompletion fetches agent names from the Hub for shell completion.
// It uses a short timeout and falls back to cache if the Hub is slow or unavailable.
// This function is designed to be silent - it never returns errors to avoid breaking completion.
// Completion skips PersistentPreRunE, so it resolves the hub itself (from
// the hub list, without imports or output; see completionTargetResolver).
func fetchHubAgentsForCompletion(cmd *cobra.Command, projectPath string) []string {
	settings, err := config.LoadSettings(projectPath)
	if err != nil {
		return nil
	}
	if err := completionTargetResolver(cmd); err != nil {
		return nil
	}

	// Without a hub there is nothing to ask
	if !hubConfigured() {
		return nil
	}
	endpoint := invocationHubEndpoint()

	// Generate cache key for this project
	cacheKey := agentcache.GenerateCacheKey(projectPath)

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
	// An explicit token from the environment first (tokens in settings
	// files are not read).
	authConfigured := false
	if token := os.Getenv("SCION_HUB_TOKEN"); token != "" {
		opts = append(opts, hubclient.WithBearerToken(token))
		authConfigured = true
	}

	// Check for OAuth credentials (non-interactive)
	if !authConfigured {
		if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
			opts = append(opts, hubclient.WithBearerToken(accessToken))
			authConfigured = true
		}
	}

	// Fallback to auto dev auth (checks env var and file), for a loopback
	// hub only (see scopeDevAuth)
	if !authConfigured && isLocalhostEndpoint(endpoint) {
		opts = append(opts, hubclient.WithAutoDevAuth())
	}

	client, err := newUserHubClient(endpoint, opts...)
	if err != nil {
		return nil, err
	}

	// Determine project ID for filtering
	var agentService hubclient.AgentService
	if settings.Hub != nil && settings.Hub.ProjectID != "" {
		agentService = client.ProjectAgents(settings.Hub.ProjectID)
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

// loadCompletionContext reads context.yaml for shell completion, or
// returns nil inside an agent (which never uses the hub list) or when the
// file cannot be read. Completion never writes the file and never prints.
func loadCompletionContext() *config.CLIContext {
	if contextDisabled(resolveMode()) {
		return nil
	}
	c, err := config.LoadCLIContext()
	if err != nil {
		return nil
	}
	return c
}

// completeHubNames completes --hub with the nicknames in the hub list.
func completeHubNames(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	c := loadCompletionContext()
	if c == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, name := range c.HubNames() {
		if strings.HasPrefix(name, toComplete) {
			names = append(names, name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeUseProjects completes 'scion use <project>' and 'scion project
// use <project>' with the slugs in the known-project list, offline. With
// --hub, only that hub's projects are offered.
func completeUseProjects(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c := loadCompletionContext()
	if c == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Completion runs without the persistent pre-run, so hubEndpoint holds
	// the raw --hub value: a nickname or a URL.
	hubName := ""
	if hubEndpoint != "" {
		hubName, _ = c.LookupHub(hubEndpoint)
		if hubName == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
	var slugs []string
	seen := map[string]bool{}
	for _, p := range c.Projects {
		if p.Slug == "" || seen[p.Slug] || !strings.HasPrefix(p.Slug, toComplete) {
			continue
		}
		if hubName != "" && p.Hub != hubName {
			continue
		}
		seen[p.Slug] = true
		slugs = append(slugs, p.Slug)
	}
	sort.Strings(slugs)
	return slugs, cobra.ShellCompDirectiveNoFileComp
}
