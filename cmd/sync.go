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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectsync"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

var (
	syncDryRun  bool
	syncExclude []string
	syncForce   bool
)

// syncCmd represents the sync command.
// When invoked bare (no subcommand, no agent), it performs bidirectional project-level sync.
var syncCmd = &cobra.Command{
	Use:   "sync [push|pull|to|from] [agent-name]",
	Short: "Sync project or agent workspace",
	Long: `Synchronizes the workspace for a project or a specific agent.

Project-level sync (requires Hub):
  scion sync                   Bidirectional sync (newer file wins)
  scion sync push              Push local workspace to hub
  scion sync pull              Pull hub workspace to local

Agent-level sync (existing behavior):
  scion sync to <agent-name>   Push to a running agent's workspace
  scion sync from <agent-name> Pull from a running agent's workspace

Options:
  --dry-run                    Preview what would be synced
  --exclude "pattern"          Additional glob patterns to exclude
  --force                      Bypass safety checks (bisync max-delete)

Examples:
  # Bidirectional project sync against hub
  scion sync

  # Push local project workspace to hub
  scion sync push

  # Pull hub workspace to local
  scion sync pull

  # Preview project sync
  scion sync --dry-run

  # Sync with specific project
  scion sync -g /path/to/project push

  # Agent-level sync (unchanged)
  scion sync from my-agent
  scion sync to my-agent --exclude "*.log"`,
	Args: cobra.MaximumNArgs(2),
	RunE: runSync,
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be synced without making changes")
	syncCmd.Flags().StringArrayVar(&syncExclude, "exclude", nil, "Glob patterns to exclude from sync (can be specified multiple times)")
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "Bypass safety checks (e.g., bisync max-delete limit)")
}

func runSync(cmd *cobra.Command, args []string) error {
	// Parse arguments to determine scope and direction
	if len(args) == 0 {
		// Bare `scion sync` → project-level bidirectional
		return runProjectSync(projectsync.DirBisync)
	}

	direction := args[0]

	// Project-level subcommands: push, pull
	switch direction {
	case "push":
		if len(args) > 1 {
			return fmt.Errorf("'push' does not take an agent name; use 'scion sync to <agent>' for agent-level sync")
		}
		return runProjectSync(projectsync.DirPush)
	case "pull":
		if len(args) > 1 {
			return fmt.Errorf("'pull' does not take an agent name; use 'scion sync from <agent>' for agent-level sync")
		}
		return runProjectSync(projectsync.DirPull)
	}

	// Agent-level subcommands: to, from
	if direction == "to" || direction == "from" {
		if len(args) < 2 {
			return fmt.Errorf("agent-level sync requires an agent name: scion sync %s <agent-name>", direction)
		}
		return runAgentSync(args)
	}

	// Single arg that isn't a direction → treat as agent name (legacy compat)
	return runAgentSync(args)
}

// runProjectSync performs project-level workspace sync against the Hub's WebDAV endpoint.
func runProjectSync(direction projectsync.Direction) error {
	// Check Hub availability (project-level sync requires a hub)
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("project-level sync requires a Hub connection.\nUse 'scion sync to/from <agent>' for agent-level sync in solo mode")
	}

	PrintUsingHub(hubCtx.Endpoint)

	// Get the project ID
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Resolve local workspace path
	workspacePath, err := resolveProjectWorkspacePath()
	if err != nil {
		return err
	}

	// Get auth token for WebDAV
	authToken := getHubAccessToken(hubCtx.Endpoint)
	if authToken == "" {
		return fmt.Errorf("no authentication token available for hub; run 'scion hub auth login'")
	}

	dirLabel := string(direction)
	if direction == projectsync.DirBisync {
		dirLabel = "bidirectional sync"
	}
	statusf("Starting project %s: %s ↔ hub\n", dirLabel, workspacePath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := projectsync.Sync(ctx, projectsync.Options{
		LocalPath:       workspacePath,
		HubEndpoint:     hubCtx.Endpoint,
		ProjectID:         projectID,
		AuthToken:       authToken,
		Direction:       direction,
		DryRun:          syncDryRun,
		ExcludePatterns: syncExclude,
		Force:           syncForce,
	})
	if err != nil {
		return fmt.Errorf("project sync failed: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":    "success",
			"command":   "sync",
			"scope":     "project",
			"direction": string(result.Direction),
			"projectId":   projectID,
			"dryRun":    result.DryRun,
		})
	}

	if result.DryRun {
		statusln("Dry run complete.")
	} else {
		statusln("Project sync complete.")
	}

	return nil
}

// resolveProjectWorkspacePath resolves the local workspace path for project-level sync.
// It finds the project root directory containing .scion/.
func resolveProjectWorkspacePath() (string, error) {
	if projectPath != "" {
		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return "", fmt.Errorf("failed to resolve project path: %w", err)
		}
		// The workspace is the parent of the .scion directory (for project projects)
		// or a recorded path (for external projects)
		return resolveProjectWorkspace(resolvedPath)
	}

	// Use current directory — find the project root
	cwd, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}

	projectRoot, found := config.FindProjectRoot()
	if !found {
		return cwd, nil
	}

	return projectRoot, nil
}

// runAgentSync handles agent-level sync (to/from a specific agent).
func runAgentSync(args []string) error {
	var agentName string
	var direction runtime.SyncDirection = runtime.SyncUnspecified

	if len(args) == 2 {
		dirStr := args[0]
		if dirStr != "to" && dirStr != "from" {
			return fmt.Errorf("invalid direction '%s', must be 'to' or 'from'", dirStr)
		}
		direction = runtime.SyncDirection(dirStr)
		agentName = api.Slugify(args[1])
	} else {
		agentName = api.Slugify(args[0])
	}

	// Check if Hub should be used
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return err
	}

	if hubCtx != nil {
		// Hosted mode requires direction
		if direction == runtime.SyncUnspecified {
			return fmt.Errorf("hosted mode requires sync direction: scion sync [to|from] %s", agentName)
		}
		return syncViaHub(hubCtx, agentName, direction)
	}

	// Solo mode: use existing local sync
	rt := runtime.GetRuntime(projectPath, profile)

	return rt.Sync(context.Background(), agentName, direction)
}

// syncViaHub performs workspace sync using Hub API.
func syncViaHub(hubCtx *HubContext, agentName string, direction runtime.SyncDirection) error {
	PrintUsingHub(hubCtx.Endpoint) // writes to stderr

	// Get the project ID
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Resolve agent name to agent ID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentID, err := resolveAgentID(ctx, hubCtx.Client, projectID, agentName)
	if err != nil {
		return wrapHubError(err)
	}

	// Resolve local workspace path
	workspacePath, err := resolveLocalWorkspacePath(agentName)
	if err != nil {
		return err
	}

	switch direction {
	case runtime.SyncFrom:
		return syncFromViaHub(hubCtx, agentID, agentName, workspacePath)
	case runtime.SyncTo:
		return syncToViaHub(hubCtx, agentID, agentName, workspacePath)
	default:
		return fmt.Errorf("unknown sync direction: %s", direction)
	}
}

// syncFromViaHub downloads workspace from agent to local directory.
func syncFromViaHub(hubCtx *HubContext, agentID, agentName, localPath string) error {
	statusf("Requesting workspace sync from agent '%s'...\n", agentName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Build sync options
	var opts *hubclient.SyncFromOptions
	if len(syncExclude) > 0 {
		opts = &hubclient.SyncFromOptions{
			ExcludePatterns: syncExclude,
		}
	}

	// Initiate sync-from - this triggers Runtime Broker to upload to GCS
	resp, err := hubCtx.Client.Workspace().SyncFrom(ctx, agentID, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to initiate sync: %w", err))
	}

	if resp.Manifest == nil || len(resp.Manifest.Files) == 0 {
		statusln("Workspace is empty, nothing to sync.")
		return nil
	}

	// Build local file hash map for incremental sync
	localFiles, err := transfer.CollectFiles(localPath, transfer.DefaultExcludePatterns)
	if err != nil && syncDryRun {
		// In dry-run mode, local path may not exist
		localFiles = nil
	} else if err != nil {
		return fmt.Errorf("failed to scan local workspace: %w", err)
	}

	localHashes := make(map[string]string)
	for _, f := range localFiles {
		localHashes[f.Path] = f.Hash
	}

	// Identify files to download (incremental)
	var toDownload []transfer.DownloadURLInfo
	var skipCount int
	var downloadSize int64

	for _, url := range resp.DownloadURLs {
		if localHash, exists := localHashes[url.Path]; exists && localHash == url.Hash {
			skipCount++
			continue
		}
		toDownload = append(toDownload, url)
		downloadSize += url.Size
	}

	// Report what will be synced
	if syncDryRun {
		statusf("Would download %d files (%s):\n", len(toDownload), humanize.Bytes(uint64(downloadSize)))
		for _, url := range toDownload {
			status := "(new)"
			if _, exists := localHashes[url.Path]; exists {
				status = "(modified)"
			}
			statusf("  %s %s\n", url.Path, status)
		}
		if skipCount > 0 {
			statusf("Would skip %d unchanged files\n", skipCount)
		}
		return nil
	}

	if len(toDownload) == 0 {
		statusln("Workspace is up to date, nothing to sync.")
		return nil
	}

	statusf("Downloading %d files (%s)...\n", len(toDownload), humanize.Bytes(uint64(downloadSize)))

	// Create transfer client and download files
	transferClient := transfer.NewClient(nil)

	var downloadedCount int
	var downloadedBytes int64

	progress := func(file transfer.FileInfo, bytesTransferred int64) error {
		downloadedCount++
		downloadedBytes += bytesTransferred
		statusf("  %s (%s) done\n", file.Path, humanize.Bytes(uint64(file.Size)))
		return nil
	}

	if err := transferClient.DownloadFiles(ctx, toDownload, localPath, progress); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":           "success",
			"command":          "sync",
			"direction":        "from",
			"agent":            agentName,
			"filesDownloaded":  downloadedCount,
			"bytesTransferred": downloadedBytes,
			"filesSkipped":     skipCount,
		})
	}

	statusf("Sync complete: %d files, %s transferred\n", downloadedCount, humanize.Bytes(uint64(downloadedBytes)))
	if skipCount > 0 {
		statusf("Skipped %d unchanged files\n", skipCount)
	}

	return nil
}

// syncToViaHub uploads workspace from local directory to agent.
func syncToViaHub(hubCtx *HubContext, agentID, agentName, localPath string) error {
	statusf("Scanning local workspace...\n")

	// Collect local files
	excludePatterns := append([]string{}, transfer.DefaultExcludePatterns...)
	excludePatterns = append(excludePatterns, syncExclude...)

	localFiles, err := transfer.CollectFiles(localPath, excludePatterns)
	if err != nil {
		return fmt.Errorf("failed to scan local workspace: %w", err)
	}

	if len(localFiles) == 0 {
		statusln("No files to sync.")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Request upload URLs from Hub
	resp, err := hubCtx.Client.Workspace().SyncTo(ctx, agentID, localFiles)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to initiate sync: %w", err))
	}

	// Build existing files set
	existingSet := make(map[string]bool)
	for _, path := range resp.ExistingFiles {
		existingSet[path] = true
	}

	// Identify files to upload
	var toUpload []transfer.FileInfo
	var uploadSize int64
	for _, file := range localFiles {
		if !existingSet[file.Path] {
			toUpload = append(toUpload, file)
			uploadSize += file.Size
		}
	}

	// Report what will be synced
	if syncDryRun {
		statusf("Would upload %d changed files (%s):\n", len(toUpload), humanize.Bytes(uint64(uploadSize)))
		for _, file := range toUpload {
			statusf("  %s (%s)\n", file.Path, humanize.Bytes(uint64(file.Size)))
		}
		if len(resp.ExistingFiles) > 0 {
			statusf("Would skip %d unchanged files\n", len(resp.ExistingFiles))
		}
		return nil
	}

	if len(toUpload) == 0 {
		statusln("All files are up to date on remote, nothing to upload.")
		// Still need to finalize to apply the manifest to the agent
		manifest := transfer.BuildManifest(localFiles)
		if _, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentID, manifest); err != nil {
			return wrapHubError(fmt.Errorf("failed to finalize sync: %w", err))
		}
		statusln("Workspace sync applied to agent.")
		return nil
	}

	statusf("Uploading %d files (%s)...\n", len(toUpload), humanize.Bytes(uint64(uploadSize)))

	// Create transfer client and upload files
	transferClient := transfer.NewClient(nil)

	var uploadedCount int
	var uploadedBytes int64

	progress := func(file transfer.FileInfo, bytesTransferred int64) error {
		uploadedCount++
		uploadedBytes += bytesTransferred
		statusf("  %s (%s) done\n", file.Path, humanize.Bytes(uint64(file.Size)))
		return nil
	}

	if err := transferClient.UploadFiles(ctx, toUpload, resp.UploadURLs, progress); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// Finalize the sync
	statusln("Applying workspace to agent...")
	manifest := transfer.BuildManifest(localFiles)
	finalizeResp, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentID, manifest)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to finalize sync: %w", err))
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":           "success",
			"command":          "sync",
			"direction":        "to",
			"agent":            agentName,
			"filesUploaded":    uploadedCount,
			"bytesTransferred": uploadedBytes,
			"filesSkipped":     len(resp.ExistingFiles),
			"filesApplied":     finalizeResp.FilesApplied,
		})
	}

	statusf("Sync complete: %d files uploaded, %s transferred\n", uploadedCount, humanize.Bytes(uint64(uploadedBytes)))
	if len(resp.ExistingFiles) > 0 {
		statusf("Skipped %d unchanged files\n", len(resp.ExistingFiles))
	}
	if finalizeResp.Applied {
		statusf("Applied %d files to agent workspace\n", finalizeResp.FilesApplied)
	}

	return nil
}

// resolveAgentID resolves an agent name to an agent ID.
func resolveAgentID(ctx context.Context, client hubclient.Client, projectID, agentName string) (string, error) {
	// List agents in the project and find by name
	resp, err := client.ProjectAgents(projectID).List(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to look up agent: %w", err)
	}

	// Find agent by name
	for _, agent := range resp.Agents {
		if agent.Name == agentName {
			// Check agent phase — must be running to sync
			agentPhase, _ := hubAgentPhaseActivity(agent.Phase, agent.Activity, agent.Status)
			if agentPhase != string(state.PhaseRunning) {
				return "", fmt.Errorf("agent '%s' is not running (phase: %s)", agentName, agentPhase)
			}
			return agent.Slug, nil
		}
	}

	return "", fmt.Errorf("agent '%s' not found in project", agentName)
}

// resolveLocalWorkspacePath resolves the local workspace path for an agent.
func resolveLocalWorkspacePath(agentName string) (string, error) {
	// Resolve project path
	var projectDir string
	if projectPath != "" {
		projectDir = projectPath
	} else {
		// Use current directory
		cwd, err := filepath.Abs(".")
		if err != nil {
			return ".", nil
		}
		projectDir = cwd
	}

	// Get project name from the directory
	projectName := filepath.Base(projectDir)

	// Check for standard worktree location: {parent}/.scion_worktrees/{project}/{agent}
	projectParent := filepath.Dir(projectDir)
	worktreePath := filepath.Join(projectParent, ".scion_worktrees", projectName, agentName)

	// If the worktree exists, use it
	if info, err := os.Stat(worktreePath); err == nil && info.IsDir() {
		return worktreePath, nil
	}

	// Fall back to current directory
	return ".", nil
}
