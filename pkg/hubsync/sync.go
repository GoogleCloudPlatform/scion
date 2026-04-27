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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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
	RemoteOnly []AgentRef // Hub agents created by other brokers after our last sync (no action needed)
	StaleLocal []string   // Local artifacts that are older than/equal to last sync (informational only)
	ServerTime time.Time  // Hub server time from the list response (for clock-skew-safe watermarks)
}

// IsInSync returns true if there are no agents to sync.
func (r *SyncResult) IsInSync() bool {
	return len(r.ToRegister) == 0 && len(r.ToRemove) == 0
}

// ExcludeAgent returns a new SyncResult with the specified agent excluded from
// ToRegister, ToRemove, and Pending lists. This is used when operating on a specific agent
// so that the sync check doesn't require syncing the target of the operation.
func (r *SyncResult) ExcludeAgent(agentName string) *SyncResult {
	return r.ExcludeAgents([]string{agentName})
}

// ExcludeAgents returns a new SyncResult with the specified agents excluded from
// all actionable and informational buckets. This is used when operating on one
// or more agents so sync gating does not block on the current targets.
func (r *SyncResult) ExcludeAgents(agentNames []string) *SyncResult {
	if len(agentNames) == 0 {
		return r
	}

	excluded := make(map[string]struct{}, len(agentNames))
	for _, name := range agentNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		excluded[name] = struct{}{}
	}
	if len(excluded) == 0 {
		return r
	}

	result := &SyncResult{
		InSync:     r.InSync,
		ServerTime: r.ServerTime,
	}

	for _, name := range r.ToRegister {
		if _, skip := excluded[name]; !skip {
			result.ToRegister = append(result.ToRegister, name)
		}
	}

	for _, ref := range r.ToRemove {
		if _, skip := excluded[ref.Name]; !skip {
			result.ToRemove = append(result.ToRemove, ref)
		}
	}

	for _, ref := range r.Pending {
		if _, skip := excluded[ref.Name]; !skip {
			result.Pending = append(result.Pending, ref)
		}
	}

	for _, ref := range r.RemoteOnly {
		if _, skip := excluded[ref.Name]; !skip {
			result.RemoteOnly = append(result.RemoteOnly, ref)
		}
	}

	for _, name := range r.StaleLocal {
		if _, skip := excluded[name]; !skip {
			result.StaleLocal = append(result.StaleLocal, name)
		}
	}

	return result
}

// HubContext holds the context for Hub operations.
type HubContext struct {
	Client    hubclient.Client
	Endpoint  string
	Settings  *config.Settings
	GroveID   string
	BrokerID  string
	GrovePath string
	IsGlobal  bool
}

// EnsureHubReadyOptions configures the behavior of EnsureHubReady.
type EnsureHubReadyOptions struct {
	// AutoConfirm auto-confirms all prompts.
	AutoConfirm bool
	// NoHub disables Hub integration for this invocation.
	NoHub bool
	// EndpointOverride is the explicit Hub endpoint supplied by the caller.
	// It takes precedence over grove/global settings for this invocation.
	EndpointOverride string
	// SkipSync skips agent synchronization check.
	SkipSync bool
	// TargetAgent is the agent being operated on. If set, this agent is excluded
	// from sync requirements since the current operation will change its state.
	// For delete: the agent won't be required to be registered on Hub first.
	// For create: the agent won't be required to be removed from Hub first.
	TargetAgent string
	// ExcludedAgents extends TargetAgent to support multi-agent operations.
	// Any excluded agent is filtered from sync gating checks.
	ExcludedAgents []string
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
		if grovePath != "" && IsHubGroveRef(grovePath) {
			return nil, fmt.Errorf("cannot use --no-hub with a hub grove reference (%s)\n\n"+
				"Hub grove references (slugs, names, git URLs) require hub connectivity.", grovePath)
		}
		debugf("NoHub flag set, returning nil")
		return nil, nil
	}

	// Check if grovePath is a hub grove reference (slug, name, UUID, or git URL)
	if grovePath != "" && IsHubGroveRef(grovePath) {
		return resolveHubGroveRef(grovePath, opts)
	}

	// Resolve grove path
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve grove path: %w", err)
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

	// Check if hub is explicitly enabled via settings OR if we're inside
	// a hub-connected container (env vars like SCION_HUB_ENDPOINT are set).
	// Inside containers, hub.enabled is not written to settings files, but
	// the hub env vars signal that the Hub API should be used.
	hubContext := config.IsHubContext()
	if !settings.IsHubEnabled() && !hubContext {
		return nil, nil
	}

	// When running inside a hub-connected container, always skip sync checks —
	// containers cannot register groves or reconcile agents.
	if hubContext {
		opts.SkipSync = true
	}

	// Hub is enabled - from here on, any failure is an error (no silent fallback)
	endpoint := opts.EndpointOverride
	if endpoint == "" {
		endpoint = getEndpoint(settings)
	}
	// In hub context, settings loading may not pick up the env var (e.g. if the
	// grove path resolves to a synthetic or tmpfs directory without a settings file
	// and koanf doesn't populate the pointer struct). Fall back to the env var.
	if endpoint == "" && hubContext {
		endpoint = os.Getenv("SCION_HUB_ENDPOINT")
		if endpoint == "" {
			endpoint = os.Getenv("SCION_HUB_URL")
		}
	}
	if endpoint == "" {
		return nil, wrapHubError(fmt.Errorf("Hub is enabled but no endpoint configured.\n\nConfigure via: scion config set hub.endpoint <url>"))
	}

	// Ensure grove_id exists.
	// In hub context, SCION_GROVE_ID takes priority over settings.GroveID
	// because the dispatcher sets it to the authoritative grove for this
	// agent. The workspace may contain a cloned repo whose .scion/settings
	// has a different grove_id (e.g. template-sync from an external repo).
	var groveID string
	if hubContext {
		groveID = os.Getenv("SCION_GROVE_ID")
	}
	if groveID == "" {
		groveID = settings.GroveID
	}
	if groveID == "" {
		if hubContext {
			// Inside a container without SCION_GROVE_ID — we can't generate
			// and persist a grove ID. The Hub client can still be constructed
			// for cross-grove operations like list --all.
			debugf("hub context without grove_id — grove-scoped operations may fail")
		} else {
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

	// Prefer hub.groveId (explicit link to a hub grove) over grove_id
	// (deterministic local identity). For hub API calls, we need the ID
	// the hub knows the grove by.
	effectiveGroveID := groveID
	if hgid := settings.GetHubGroveID(); hgid != "" {
		effectiveGroveID = hgid
	}

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  endpoint,
		Settings:  settings,
		GroveID:   effectiveGroveID,
		BrokerID:  brokerID,
		GrovePath: resolvedPath,
		IsGlobal:  isGlobal,
	}

	debugf("HubContext created: endpoint=%s, groveID=%s (local=%s), brokerID=%s, grovePath=%s, isGlobal=%v",
		endpoint, effectiveGroveID, groveID, brokerID, resolvedPath, isGlobal)

	// Inside a hub-connected container, skip grove registration, provider path,
	// and sync checks — the container should only query the Hub API, not manage
	// grove state. Return the context directly.
	if hubContext {
		return hubCtx, nil
	}

	// Check grove registration
	registered, err := isGroveRegistered(ctx, hubCtx)
	if err != nil {
		return nil, wrapHubError(err)
	}

	if !registered {
		// Get grove name for the prompt
		groveName := getGroveName(resolvedPath, isGlobal)

		// Check for an exact ID match on the Hub first.
		// A grove with the same UUID is definitively the same grove,
		// regardless of name differences (e.g., when running inside a
		// container where the grove name resolves to "workspace").
		idMatchGrove := findGroveByID(ctx, hubCtx)

		if idMatchGrove != nil {
			// Exact ID match found - this is the same grove, no prompt needed
			debugf("Found grove with exact matching ID on Hub: %s (name: %s)", idMatchGrove.ID, idMatchGrove.Name)
			fmt.Printf("Linked to existing grove: %s (ID: %s)\n", idMatchGrove.Name, idMatchGrove.ID)
		} else {
			// No ID match - fall back to name-based matching
			matches, err := findMatchingGroves(ctx, hubCtx, groveName)
			if err != nil {
				debugf("Warning: failed to search for matching groves: %v", err)
				// Continue with registration - the hub will handle matching
			}

			if len(matches) > 0 {
				// Check if any name-based match has the same ID as our local grove.
				// This is a defensive check for cases where the ID-based lookup
				// above failed transiently but the list endpoint succeeded.
				idMatched := false
				for _, m := range matches {
					if m.ID == hubCtx.GroveID {
						debugf("Found exact ID match in name-based results: %s", m.ID)
						fmt.Printf("Linked to existing grove: %s (ID: %s)\n", m.Name, m.ID)
						idMatched = true
						break
					}
				}

				if !idMatched {
					// No ID match - ask user what to do
					baseSlug := api.Slugify(groveName)
					nextSlug := NextSlugFromMatches(baseSlug, matches)
					choice, selectedID := ShowMatchingGrovesPrompt(groveName, matches, nextSlug, opts.AutoConfirm)
					switch choice {
					case GroveChoiceCancel:
						return nil, fmt.Errorf("registration cancelled")
					case GroveChoiceLink:
						// Store the hub grove ID separately — don't overwrite
						// the deterministic local grove_id.
						if err := config.UpdateSetting(resolvedPath, "hub.groveId", selectedID, isGlobal); err != nil {
							return nil, fmt.Errorf("failed to save hub grove ID: %w", err)
						}
						hubCtx.GroveID = selectedID
						debugf("Stored hub.groveId: %s", selectedID)
					case GroveChoiceRegisterNew:
						// Register as new grove with the existing local grove_id.
						// The hub will assign its own ID if needed.
						debugf("Registering new grove with existing grove_id: %s", hubCtx.GroveID)
					}
				}
			} else {
				// No matching groves - ask for confirmation
				if !ShowLinkPrompt(groveName, opts.AutoConfirm) {
					return nil, fmt.Errorf("grove must be linked to Hub to perform this operation\n\n" +
						"Link this grove: scion hub link\n" +
						"Or use local-only mode: scion --no-hub <command>")
				}
			}
		}

		// Register the grove
		if err := registerGrove(context.Background(), hubCtx, groveName, isGlobal); err != nil {
			return nil, wrapHubError(fmt.Errorf("failed to register grove: %w", err))
		}
		// Reload settings to get updated broker ID and hub.groveId
		settings, err = config.LoadSettings(resolvedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to reload settings: %w", err)
		}
		hubCtx.Settings = settings
		// Prefer hub.groveId (explicit link) over grove_id (deterministic local ID)
		if hgid := settings.GetHubGroveID(); hgid != "" {
			hubCtx.GroveID = hgid
		} else {
			hubCtx.GroveID = settings.GroveID
		}
		if settings.Hub != nil {
			hubCtx.BrokerID = settings.Hub.BrokerID
		}
	}

	// Ensure the local broker is registered as a provider with the correct local path.
	// Auto-provide may have linked the broker without a local_path, so we always
	// check and update if needed.
	if err := ensureProviderPath(context.Background(), hubCtx); err != nil {
		debugf("Warning: failed to ensure provider path: %v", err)
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
	excludedAgents := append([]string{}, opts.ExcludedAgents...)
	if opts.TargetAgent != "" {
		excludedAgents = append(excludedAgents, opts.TargetAgent)
	}

	effectiveSyncResult := syncResult
	if len(excludedAgents) > 0 {
		effectiveSyncResult = syncResult.ExcludeAgents(excludedAgents)
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
	} else {
		// Already in sync — update the watermark and synced agents to keep current
		UpdateLastSyncedAt(hubCtx.GrovePath, syncResult.ServerTime, hubCtx.IsGlobal)
		UpdateSyncedAgents(hubCtx.GrovePath, collectSyncedAgentNames(syncResult))
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

// UpdateLastSyncedAt updates the lastSyncedAt watermark in state.yaml.
// Uses hubTime if non-zero (preferred), otherwise falls back to local time.
var lastSyncedAtMu sync.Mutex

func UpdateLastSyncedAt(grovePath string, hubTime time.Time, isGlobal bool) {
	_ = isGlobal // retained for API compatibility

	if strings.TrimSpace(grovePath) == "" {
		debugf("Warning: skipping lastSyncedAt update: empty grove path")
		return
	}

	var ts time.Time
	if !hubTime.IsZero() {
		ts = hubTime.UTC()
	} else {
		ts = time.Now().UTC()
	}

	lastSyncedAtMu.Lock()
	defer lastSyncedAtMu.Unlock()

	currentState, err := config.LoadGroveState(grovePath)
	if err != nil {
		debugf("Warning: failed to load current state.yaml for watermark update: %v", err)
		currentState = &config.GroveState{}
	}

	if currentState.LastSyncedAt != "" {
		existingTS, parseErr := time.Parse(time.RFC3339Nano, currentState.LastSyncedAt)
		if parseErr != nil {
			debugf("Warning: failed to parse existing lastSyncedAt %q: %v", currentState.LastSyncedAt, parseErr)
		} else if existingTS.After(ts) {
			ts = existingTS
		}
	}

	currentState.LastSyncedAt = ts.Format(time.RFC3339Nano)

	if err := saveGroveStateAtomic(grovePath, currentState); err != nil {
		debugf("Warning: failed to save lastSyncedAt to state.yaml: %v", err)
	}
}

// UpdateSyncedAgents records the set of agent names currently known to be
// synced with the hub. This is used to detect agents that were deleted from the
// hub: a local-only agent whose name appears in SyncedAgents was previously
// registered and has since been removed hub-side.
func UpdateSyncedAgents(grovePath string, agents []string) {
	if strings.TrimSpace(grovePath) == "" {
		return
	}

	lastSyncedAtMu.Lock()
	defer lastSyncedAtMu.Unlock()

	currentState, err := config.LoadGroveState(grovePath)
	if err != nil {
		debugf("Warning: failed to load state.yaml for synced agents update: %v", err)
		currentState = &config.GroveState{}
	}

	sorted := make([]string, len(agents))
	copy(sorted, agents)
	// Sort for deterministic output
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	currentState.SyncedAgents = sorted

	if err := saveGroveStateAtomic(grovePath, currentState); err != nil {
		debugf("Warning: failed to save synced agents to state.yaml: %v", err)
	}
}

// AddSyncedAgent adds a single agent name to the synced agents list in state.yaml.
func AddSyncedAgent(grovePath, agentName string) {
	if strings.TrimSpace(grovePath) == "" || strings.TrimSpace(agentName) == "" {
		return
	}

	lastSyncedAtMu.Lock()
	defer lastSyncedAtMu.Unlock()

	currentState, err := config.LoadGroveState(grovePath)
	if err != nil {
		currentState = &config.GroveState{}
	}

	for _, name := range currentState.SyncedAgents {
		if name == agentName {
			return // already present
		}
	}
	currentState.SyncedAgents = append(currentState.SyncedAgents, agentName)

	if err := saveGroveStateAtomic(grovePath, currentState); err != nil {
		debugf("Warning: failed to add synced agent to state.yaml: %v", err)
	}
}

// RemoveSyncedAgent removes a single agent name from the synced agents list in state.yaml.
func RemoveSyncedAgent(grovePath, agentName string) {
	if strings.TrimSpace(grovePath) == "" || strings.TrimSpace(agentName) == "" {
		return
	}

	lastSyncedAtMu.Lock()
	defer lastSyncedAtMu.Unlock()

	currentState, err := config.LoadGroveState(grovePath)
	if err != nil {
		return
	}

	filtered := currentState.SyncedAgents[:0]
	for _, name := range currentState.SyncedAgents {
		if name != agentName {
			filtered = append(filtered, name)
		}
	}
	currentState.SyncedAgents = filtered

	if err := saveGroveStateAtomic(grovePath, currentState); err != nil {
		debugf("Warning: failed to remove synced agent from state.yaml: %v", err)
	}
}

// CompareAgents compares local agents with all Hub agents in the grove.
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

	// Get all Hub agents in the grove (not filtered by broker ID).
	// We fetch all agents so that agents created from the Hub UI or assigned
	// to a different/stale broker identity are visible during comparison.
	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	opts := &hubclient.ListAgentsOptions{
		GroveID: hubCtx.GroveID,
	}

	resp, err := hubCtx.Client.GroveAgents(hubCtx.GroveID).List(ctxTimeout, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list Hub agents: %w", err)
	}

	result.ServerTime = resp.ServerTime

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

	// Parse lastSyncedAt from state.yaml (preferred) or legacy settings (fallback).
	var lastSyncedAt time.Time
	var lastSyncedAtStr string

	// Try state.yaml first
	groveState, err := config.LoadGroveState(hubCtx.GrovePath)
	if err == nil && groveState.LastSyncedAt != "" {
		lastSyncedAtStr = groveState.LastSyncedAt
		debugf("lastSyncedAt from state.yaml: %s", lastSyncedAtStr)
	}

	// Build set of previously synced agents from state.yaml.
	// Agents in this set were registered on the hub during a prior sync cycle.
	// If they are local-only now, it means they were deleted from the hub.
	previouslySynced := make(map[string]bool, len(groveState.SyncedAgents))
	for _, name := range groveState.SyncedAgents {
		previouslySynced[name] = true
	}

	// Fall back to legacy settings if state.yaml doesn't have it
	if lastSyncedAtStr == "" && hubCtx.Settings.Hub != nil && hubCtx.Settings.Hub.LastSyncedAt != "" {
		lastSyncedAtStr = hubCtx.Settings.Hub.LastSyncedAt
		debugf("lastSyncedAt from legacy settings (hub.lastSyncedAt): %s", lastSyncedAtStr)
	}

	if lastSyncedAtStr != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, lastSyncedAtStr); err == nil {
			lastSyncedAt = parsed.UTC()
			debugf("lastSyncedAt: %s", lastSyncedAt.Format(time.RFC3339))
		} else {
			debugf("Warning: failed to parse lastSyncedAt %q: %v", lastSyncedAtStr, err)
		}
	}

	// Find local-only agents. Distinguish between genuinely new/updated local
	// changes and stale local artifacts left behind after earlier hub-side actions.
	for _, name := range localAgents {
		if hubAgentMap[name] {
			result.InSync = append(result.InSync, name)
			continue
		}

		// If the agent was previously synced with the hub but is no longer on the
		// hub, it was deleted hub-side. Mark it stale regardless of timestamps.
		if previouslySynced[name] {
			result.StaleLocal = append(result.StaleLocal, name)
			debugf("Agent %s local-only but previously synced (deleted from hub), marking StaleLocal", name)
			continue
		}

		localInfo := getLocalAgentInfo(hubCtx.GrovePath, name)
		localTS := getLocalAgentTimestamp(localInfo)

		if lastSyncedAt.IsZero() || localTS.IsZero() || localTS.After(lastSyncedAt) {
			result.ToRegister = append(result.ToRegister, name)
			debugf("Agent %s local-only and newer than watermark/unknown timestamp, marking ToRegister", name)
			continue
		}

		result.StaleLocal = append(result.StaleLocal, name)
		debugf("Agent %s local-only but stale (local=%s, watermark=%s), marking StaleLocal",
			name, localTS.Format(time.RFC3339Nano), lastSyncedAt.Format(time.RFC3339Nano))
	}

	// Find agents on Hub but not locally present.
	// Use lastSyncedAt to distinguish between:
	// - Agents created by other brokers after our last sync → RemoteOnly (no action)
	// - Agents that existed at last sync but were deleted locally → ToRemove
	// - On first sync (no lastSyncedAt) → all hub-only agents are RemoteOnly (register-only mode)
	// Skip agents in "pending" status - these are created on Hub but not yet started.
	for _, a := range resp.Agents {
		if !localAgentMap[a.Name] {
			if a.Status == "pending" {
				result.Pending = append(result.Pending, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) is pending, not requiring sync", a.Name, a.ID)
			} else if a.RuntimeBrokerID != hubCtx.BrokerID {
				// Agent belongs to a different broker — always treat as remote-only.
				result.RemoteOnly = append(result.RemoteOnly, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) assigned to different broker %s, treating as remote-only",
					a.Name, a.ID, a.RuntimeBrokerID)
			} else if lastSyncedAt.IsZero() || !a.Created.Before(lastSyncedAt) {
				// First sync or agent created at/after our last sync — another broker
				// created it, or we just created it via Hub (watermark set to creation
				// time). Uses !Before instead of After to include agents created at
				// exactly the watermark time, which occurs when startAgentViaHub sets
				// the watermark to resp.Agent.Created.
				result.RemoteOnly = append(result.RemoteOnly, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) created at/after last sync or first sync, treating as remote-only", a.Name, a.ID)
			} else {
				result.ToRemove = append(result.ToRemove, AgentRef{Name: a.Name, ID: a.ID})
				debugf("Agent %s (id=%s) existed at last sync but not local, marking for removal", a.Name, a.ID)
			}
		}
	}

	debugf("Sync result: toRegister=%v, staleLocal=%v, toRemove=%d, pending=%d, remoteOnly=%d, inSync=%d",
		result.ToRegister, result.StaleLocal, len(result.ToRemove), len(result.Pending), len(result.RemoteOnly), len(result.InSync))

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

		// Read local agent info to populate template and harness
		localInfo := getLocalAgentInfo(hubCtx.GrovePath, name)
		if localInfo != nil {
			if localInfo.Template != "" {
				req.Template = localInfo.Template
			}
			if localInfo.HarnessConfig != "" {
				req.HarnessConfig = localInfo.HarnessConfig
			}
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
				return fmt.Errorf("failed to register agent '%s': multiple runtime brokers available, specify a broker with --broker <id>", name)
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

	// Update lastSyncedAt watermark after successful sync
	UpdateLastSyncedAt(hubCtx.GrovePath, result.ServerTime, hubCtx.IsGlobal)

	// Record the set of agents now known to be on the hub for this broker.
	// After sync: InSync + newly registered + RemoteOnly + Pending are all on hub.
	UpdateSyncedAgents(hubCtx.GrovePath, collectSyncedAgentNames(result))

	return nil
}

// collectSyncedAgentNames returns the names of all agents that should be
// remembered as "previously synced" in state.yaml. This includes agents
// currently on the hub (InSync, ToRegister, RemoteOnly, Pending) as well as
// StaleLocal agents — local artifacts whose hub record was deleted but whose
// local files have not yet been cleaned up. Keeping StaleLocal agents in the
// list prevents them from being misclassified as new local agents on the next
// sync check.
func collectSyncedAgentNames(result *SyncResult) []string {
	seen := make(map[string]bool)
	for _, name := range result.InSync {
		seen[name] = true
	}
	for _, name := range result.ToRegister {
		seen[name] = true
	}
	for _, ref := range result.RemoteOnly {
		seen[ref.Name] = true
	}
	for _, ref := range result.Pending {
		seen[ref.Name] = true
	}
	for _, name := range result.StaleLocal {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
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

// getLocalAgentInfo reads local agent config files to extract template and harness info.
// Returns nil if the info cannot be read.
func getLocalAgentInfo(grovePath, agentName string) *api.AgentInfo {
	agentDir := filepath.Join(grovePath, "agents", agentName)

	// Try agent-info.json first (written by the container at runtime)
	agentInfoPath := filepath.Join(agentDir, "home", "agent-info.json")
	if data, err := os.ReadFile(agentInfoPath); err == nil {
		var info api.AgentInfo
		if err := json.Unmarshal(data, &info); err == nil {
			applyFileTimestampFallback(&info, agentInfoPath)
			return &info
		}
	}

	// Fallback to scion-agent.json (legacy)
	scionJSONPath := filepath.Join(agentDir, "scion-agent.json")
	if data, err := os.ReadFile(scionJSONPath); err == nil {
		var cfg api.ScionConfig
		if err := json.Unmarshal(data, &cfg); err == nil {
			// Build a minimal AgentInfo from ScionConfig
			info := &api.AgentInfo{
				HarnessConfig: cfg.HarnessConfig,
			}
			if info.HarnessConfig == "" {
				info.HarnessConfig = cfg.Harness
			}
			applyFileTimestampFallback(info, scionJSONPath)
			return info
		}
	}

	// Fallback to scion-agent.yaml
	scionYAMLPath := filepath.Join(agentDir, "scion-agent.yaml")
	if data, err := os.ReadFile(scionYAMLPath); err == nil {
		var cfg api.ScionConfig
		if err := yaml.Unmarshal(data, &cfg); err == nil {
			info := &api.AgentInfo{
				HarnessConfig: cfg.HarnessConfig,
			}
			if info.HarnessConfig == "" {
				info.HarnessConfig = cfg.Harness
			}
			applyFileTimestampFallback(info, scionYAMLPath)
			return info
		}
	}

	return nil
}

func getLocalAgentTimestamp(info *api.AgentInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	if !info.Updated.IsZero() {
		return info.Updated.UTC()
	}
	if !info.Created.IsZero() {
		return info.Created.UTC()
	}
	return time.Time{}
}

func applyFileTimestampFallback(info *api.AgentInfo, path string) {
	if info == nil {
		return
	}
	if !info.Updated.IsZero() || !info.Created.IsZero() {
		return
	}
	stat, err := os.Stat(path)
	if err != nil {
		return
	}
	mtime := stat.ModTime().UTC()
	info.Updated = mtime
	info.Created = mtime
}

func saveGroveStateAtomic(grovePath string, state *config.GroveState) error {
	statePath := filepath.Join(grovePath, "state.yaml")
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(statePath), "state.yaml.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

// isGroveRegistered checks if the grove is registered with the Hub.
// ensureProviderPath checks if the local broker is a provider for the grove
// and ensures its local_path is set. Auto-provide creates provider records without
// a local_path, which causes agents to be provisioned in the global grove.
func ensureProviderPath(ctx context.Context, hubCtx *HubContext) error {
	if hubCtx.BrokerID == "" || hubCtx.GroveID == "" || hubCtx.GrovePath == "" {
		return nil
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Check existing providers to see if our broker already has the correct path
	providersResp, err := hubCtx.Client.Groves().ListProviders(ctxTimeout, hubCtx.GroveID)
	if err != nil {
		return fmt.Errorf("failed to list providers: %w", err)
	}

	for _, p := range providersResp.Providers {
		if p.BrokerID == hubCtx.BrokerID {
			if p.LocalPath == hubCtx.GrovePath {
				// Already correct
				debugf("Provider path already set correctly: %s", p.LocalPath)
				return nil
			}
			// Path is missing or wrong — update it
			debugf("Updating provider path from %q to %q", p.LocalPath, hubCtx.GrovePath)
			break
		}
	}

	// Add/update the provider with the correct local path
	ctxAdd, cancelAdd := context.WithTimeout(ctx, 10*time.Second)
	defer cancelAdd()

	_, err = hubCtx.Client.Groves().AddProvider(ctxAdd, hubCtx.GroveID, &hubclient.AddProviderRequest{
		BrokerID:  hubCtx.BrokerID,
		LocalPath: hubCtx.GrovePath,
	})
	if err != nil {
		return fmt.Errorf("failed to update provider path: %w", err)
	}

	debugf("Provider path set to %s for broker %s", hubCtx.GrovePath, hubCtx.BrokerID)
	return nil
}

func isGroveRegistered(ctx context.Context, hubCtx *HubContext) (bool, error) {
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Try to get the grove by ID
	_, err := hubCtx.Client.Groves().Get(ctxTimeout, hubCtx.GroveID)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check grove registration: %w", err)
	}

	return true, nil
}

// findGroveByID attempts to find a grove on the Hub with the exact same ID
// as the local grove. This check runs before name-based matching to handle
// cases where the grove name differs (e.g., "workspace" inside a container)
// but the grove_id is the same. Returns nil if no match is found.
func findGroveByID(ctx context.Context, hubCtx *HubContext) *hubclient.Grove {
	ctxTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	grove, err := hubCtx.Client.Groves().Get(ctxTimeout, hubCtx.GroveID)
	if err != nil {
		debugf("findGroveByID: no grove found with ID %s: %v", hubCtx.GroveID, err)
		return nil
	}
	return grove
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
			Slug:      g.Slug,
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
	}
	// Store the hub grove ID separately if it differs from the local grove_id.
	// Don't overwrite grove_id — for git groves it's a deterministic UUID v5
	// and changing it shifts the external config directory, orphaning settings.
	if resp.Grove.ID != hubCtx.GroveID {
		if err := config.UpdateSetting(hubCtx.GrovePath, "hub.groveId", resp.Grove.ID, isGlobal); err != nil {
			fmt.Printf("Warning: failed to save hub grove ID: %v\n", err)
		} else {
			hubCtx.GroveID = resp.Grove.ID
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
	return config.GetGroveName(grovePath)
}

// getEndpoint returns the Hub endpoint from settings.
func getEndpoint(settings *config.Settings) string {
	if settings.Hub != nil {
		return settings.Hub.Endpoint
	}
	return ""
}

// readAgentTokenFile reads the canonical agent token from ~/.scion/scion-token.
func readAgentTokenFile() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".scion", "scion-token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// createHubClient creates a new Hub client with proper authentication.
// Note: hub.token and hub.apiKey are deprecated and no longer used for auth.
// Auth priority: OAuth credentials > scion-token file > SCION_AUTH_TOKEN env > auto dev auth.
// Exception: for localhost endpoints, dev auth takes priority over non-dev agent tokens
// to avoid stale scion-token files from previous remote hub connections.
func createHubClient(settings *config.Settings, endpoint string) (hubclient.Client, error) {
	var opts []hubclient.Option

	// Add authentication - check in priority order
	authConfigured := false

	// 1. Check for OAuth credentials from scion hub auth login
	if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
		opts = append(opts, hubclient.WithBearerToken(accessToken))
		authConfigured = true
	}

	// 2. Check for agent token from canonical token file, then bootstrap env var
	if !authConfigured {
		if token := readAgentTokenFile(); token != "" {
			if !apiclient.IsDevToken(token) && isLocalhostEndpoint(endpoint) {
				if devToken := apiclient.ResolveDevToken(); devToken != "" {
					opts = append(opts, hubclient.WithBearerToken(devToken))
					authConfigured = true
				}
			}
			if !authConfigured {
				opts = append(opts, hubclient.WithAgentToken(token))
				authConfigured = true
			}
		} else if token := os.Getenv("SCION_AUTH_TOKEN"); token != "" {
			opts = append(opts, hubclient.WithAgentToken(token))
			authConfigured = true
		}
	}

	// 3. Fallback to auto dev auth
	if !authConfigured {
		opts = append(opts, hubclient.WithAutoDevAuth())
	}

	opts = append(opts, hubclient.WithTimeout(30*time.Second))

	return hubclient.New(endpoint, opts...)
}

func isLocalhostEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
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

// cleanupGroveBrokerCredentials removes stale broker credentials from grove settings.
// These should only exist in global settings, not grove-specific.
// Earlier versions of scion incorrectly wrote them to grove settings.
//
// For legacy files: removes hub.brokerId and hub.brokerToken
// For v1 files: removes server.broker.broker_id and server.broker.broker_token
func cleanupGroveBrokerCredentials(grovePath string) {
	settingsPath := config.GetSettingsPath(grovePath)
	if settingsPath == "" {
		return
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}

	// Detect format
	version, _ := config.DetectSettingsFormat(data)

	if version != "" {
		// V1 versioned format: check for broker_id/broker_token under server.broker
		content := string(data)
		if !strings.Contains(content, "broker_id") && !strings.Contains(content, "broker_token") {
			return
		}

		vs, err := config.LoadSingleFileVersioned(grovePath)
		if err != nil {
			debugf("Warning: failed to load v1 grove settings: %v", err)
			return
		}

		if vs.Server == nil || vs.Server.Broker == nil {
			return
		}

		modified := false
		if vs.Server.Broker.BrokerID != "" {
			vs.Server.Broker.BrokerID = ""
			modified = true
			debugf("Removed stale server.broker.broker_id from grove settings")
		}
		if vs.Server.Broker.BrokerToken != "" {
			vs.Server.Broker.BrokerToken = ""
			modified = true
			debugf("Removed stale server.broker.broker_token from grove settings")
		}

		if !modified {
			return
		}

		if err := config.SaveVersionedSettings(grovePath, vs); err != nil {
			debugf("Warning: failed to write cleaned v1 settings: %v", err)
		}
		return
	}

	// Legacy format: check for brokerId/brokerToken under hub
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
