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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

var (
	syncDryRun  bool
	syncExclude []string
)

// projectSyncRemovedMessage is the error for the project-level forms of
// scion sync (bare, push and pull). They synced a project workspace with the
// hub's WebDAV endpoint, which has been removed.
const projectSyncRemovedMessage = "project-level sync ('scion sync', 'scion sync push', 'scion sync pull') has been removed along with the hub's WebDAV endpoint. " +
	"To sync with an agent, use 'scion sync to <agent>' or 'scion sync from <agent>'. " +
	"To move project files, use git, or the project's Files tab in the web UI (upload, download or workspace archive)."

// syncCmd represents the sync command.
var syncCmd = &cobra.Command{
	Use:   "sync to|from <agent-name>",
	Short: "Sync an agent workspace",
	Long: `Synchronizes the local workspace with a running agent's workspace.

The direction is required:
  scion sync to <agent-name>   Push to a running agent's workspace
  scion sync from <agent-name> Pull from a running agent's workspace

Options:
  --dry-run                    Preview what would be synced
  --exclude "pattern"          Additional glob patterns to exclude

Project-level sync (bare 'scion sync', 'scion sync push' and
'scion sync pull') has been removed and exits with an error.

Examples:
  scion sync from my-agent
  scion sync to my-agent --exclude "*.log"
  scion sync to my-agent --dry-run`,
	Args: cobra.MaximumNArgs(2),
	RunE: runSync,
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be synced without making changes")
	syncCmd.Flags().StringArrayVar(&syncExclude, "exclude", nil, "Glob patterns to exclude from sync (can be specified multiple times)")
}

func runSync(cmd *cobra.Command, args []string) error {
	// The project-level forms were removed with the hub's WebDAV endpoint.
	if len(args) == 0 || args[0] == "push" || args[0] == "pull" {
		return newUsageError("%s", projectSyncRemovedMessage)
	}

	direction := args[0]

	if direction == "to" || direction == "from" {
		if len(args) < 2 {
			return newUsageError("agent-level sync requires an agent name: scion sync %s <agent-name>", direction)
		}
		return runAgentSync(runtime.SyncDirection(direction), api.Slugify(args[1]))
	}

	if len(args) == 2 {
		return newUsageError("invalid direction '%s', must be 'to' or 'from'", direction)
	}
	// A bare agent name: agent-level sync needs a direction.
	return newUsageError("agent-level sync requires a direction: scion sync to|from %s", api.Slugify(direction))
}

// runAgentSync handles agent-level sync (to/from a specific agent).
func runAgentSync(direction runtime.SyncDirection, agentName string) error {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return err
	}
	return syncViaHub(hubCtx, agentName, direction)
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
	localFiles, err := collectWorkspaceFiles(localPath, nil)
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
	localFiles, err := collectWorkspaceFiles(localPath, syncExclude)
	if err != nil {
		return fmt.Errorf("failed to scan local workspace: %w", err)
	}

	if len(localFiles) == 0 {
		statusln("No files to sync.")
		return nil
	}
	// Uploads read through this root; see transfer.Client.UploadFiles.
	localRoot, err := os.OpenRoot(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local workspace %s: %w", localPath, err)
	}
	defer func() { _ = localRoot.Close() }()

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
		finalizeResp, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentID, manifest)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to finalize sync: %w", err))
		}
		for _, line := range syncToResultLines(0, 0, 0, true, finalizeResp) {
			statusln(line)
		}
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

	if err := transferClient.UploadFiles(ctx, localRoot, toUpload, resp.UploadURLs, progress); err != nil {
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
		result := map[string]interface{}{
			"status":           "success",
			"command":          "sync",
			"direction":        "to",
			"agent":            agentName,
			"filesUploaded":    uploadedCount,
			"bytesTransferred": uploadedBytes,
			"filesSkipped":     len(resp.ExistingFiles),
			"filesApplied":     finalizeResp.FilesApplied,
		}
		if len(finalizeResp.Warnings) > 0 {
			result["warnings"] = finalizeResp.Warnings
		}
		return outputJSON(result)
	}
	for _, line := range syncToResultLines(uploadedCount, uploadedBytes, len(resp.ExistingFiles), false, finalizeResp) {
		statusln(line)
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

// syncToResultLines returns the status lines to show after a sync-to
// finalize. When the hub reports that it ignored the workspace files
// (empty-per-agent), its warning replaces the "applied" / "Sync complete"
// lines, which would be misleading. Other hub warnings are always shown.
func syncToResultLines(uploadedCount int, uploadedBytes int64, skipped int, nothingToUpload bool, resp *hubclient.SyncToFinalizeResponse) []string {
	if resp == nil {
		// No finalize body: nothing ignored or applied, but the upload
		// summary still applies.
		resp = &hubclient.SyncToFinalizeResponse{}
	}
	ignored, rest := splitFilesIgnoredWarning(resp.Warnings)
	var lines []string
	if ignored {
		lines = append(lines, "Warning: "+api.WarningEmptyPerAgentWorkspaceFilesIgnored)
	}
	for _, w := range rest {
		lines = append(lines, "Warning: "+w)
	}
	if ignored {
		return lines
	}
	if nothingToUpload {
		return append(lines, "Workspace sync applied to agent.")
	}
	lines = append(lines, fmt.Sprintf("Sync complete: %d files uploaded, %s transferred", uploadedCount, humanize.Bytes(uint64(uploadedBytes))))
	if skipped > 0 {
		lines = append(lines, fmt.Sprintf("Skipped %d unchanged files", skipped))
	}
	if resp.Applied {
		lines = append(lines, fmt.Sprintf("Applied %d files to agent workspace", resp.FilesApplied))
	}
	return lines
}
