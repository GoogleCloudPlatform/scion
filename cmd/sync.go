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

	"github.com/dustin/go-humanize"
	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/transfer"
	"github.com/spf13/cobra"
)

var (
	syncDryRun  bool
	syncExclude []string
)

// syncCmd represents the sync command
var syncCmd = &cobra.Command{
	Use:   "sync [to|from] <agent-name>",
	Short: "Sync agent workspace",
	Long: `Triggers a synchronization of the workspace for the specified agent.

In solo mode, behavior depends on the configured sync mode (e.g., mutagen or tar).
In hosted mode, syncs via the Hub using signed URLs for direct storage access.

For tar sync and hosted mode, direction (to or from) must be specified.

Examples:
  # Sync workspace FROM remote agent to local
  scion sync from my-agent

  # Sync workspace TO remote agent from local
  scion sync to my-agent

  # Preview what would be synced (dry-run)
  scion sync from my-agent --dry-run

  # Exclude patterns from sync
  scion sync to my-agent --exclude "*.log" --exclude "tmp/**"`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentName string
		var direction runtime.SyncDirection = runtime.SyncUnspecified

		if len(args) == 2 {
			dirStr := args[0]
			if dirStr != "to" && dirStr != "from" {
				return fmt.Errorf("invalid direction '%s', must be 'to' or 'from'", dirStr)
			}
			direction = runtime.SyncDirection(dirStr)
			agentName = args[1]
		} else {
			agentName = args[0]
		}

		// Check if Hub should be used
		hubCtx, err := CheckHubAvailability(grovePath)
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
		effectiveProfile := profile
		if effectiveProfile == "" {
			effectiveProfile = agent.GetSavedProfile(agentName, grovePath)
		}

		effectiveRuntime := effectiveProfile
		if effectiveRuntime == "" {
			effectiveRuntime = agent.GetSavedRuntime(agentName, grovePath)
		}

		rt := runtime.GetRuntime(grovePath, effectiveRuntime)

		return rt.Sync(context.Background(), agentName, direction)
	},
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be synced without making changes")
	syncCmd.Flags().StringArrayVar(&syncExclude, "exclude", nil, "Glob patterns to exclude from sync (can be specified multiple times)")
}

// syncViaHub performs workspace sync using Hub API.
func syncViaHub(hubCtx *HubContext, agentName string, direction runtime.SyncDirection) error {
	PrintUsingHub(hubCtx.Endpoint)

	// Get the grove ID
	groveID, err := GetGroveID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	// Resolve agent name to agent ID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentID, err := resolveAgentID(ctx, hubCtx.Client, groveID, agentName)
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
	fmt.Printf("Requesting workspace sync from agent '%s'...\n", agentName)

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
		fmt.Println("Workspace is empty, nothing to sync.")
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
		fmt.Printf("Would download %d files (%s):\n", len(toDownload), humanize.Bytes(uint64(downloadSize)))
		for _, url := range toDownload {
			status := "(new)"
			if _, exists := localHashes[url.Path]; exists {
				status = "(modified)"
			}
			fmt.Printf("  %s %s\n", url.Path, status)
		}
		if skipCount > 0 {
			fmt.Printf("Would skip %d unchanged files\n", skipCount)
		}
		return nil
	}

	if len(toDownload) == 0 {
		fmt.Println("Workspace is up to date, nothing to sync.")
		return nil
	}

	fmt.Printf("Downloading %d files (%s)...\n", len(toDownload), humanize.Bytes(uint64(downloadSize)))

	// Create transfer client and download files
	transferClient := transfer.NewClient(nil)

	var downloadedCount int
	var downloadedBytes int64

	progress := func(file transfer.FileInfo, bytesTransferred int64) error {
		downloadedCount++
		downloadedBytes += bytesTransferred
		fmt.Printf("  %s (%s) done\n", file.Path, humanize.Bytes(uint64(file.Size)))
		return nil
	}

	if err := transferClient.DownloadFiles(ctx, toDownload, localPath, progress); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	fmt.Printf("Sync complete: %d files, %s transferred\n", downloadedCount, humanize.Bytes(uint64(downloadedBytes)))
	if skipCount > 0 {
		fmt.Printf("Skipped %d unchanged files\n", skipCount)
	}

	return nil
}

// syncToViaHub uploads workspace from local directory to agent.
func syncToViaHub(hubCtx *HubContext, agentID, agentName, localPath string) error {
	fmt.Printf("Scanning local workspace...\n")

	// Collect local files
	excludePatterns := append([]string{}, transfer.DefaultExcludePatterns...)
	excludePatterns = append(excludePatterns, syncExclude...)

	localFiles, err := transfer.CollectFiles(localPath, excludePatterns)
	if err != nil {
		return fmt.Errorf("failed to scan local workspace: %w", err)
	}

	if len(localFiles) == 0 {
		fmt.Println("No files to sync.")
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
		fmt.Printf("Would upload %d changed files (%s):\n", len(toUpload), humanize.Bytes(uint64(uploadSize)))
		for _, file := range toUpload {
			fmt.Printf("  %s (%s)\n", file.Path, humanize.Bytes(uint64(file.Size)))
		}
		if len(resp.ExistingFiles) > 0 {
			fmt.Printf("Would skip %d unchanged files\n", len(resp.ExistingFiles))
		}
		return nil
	}

	if len(toUpload) == 0 {
		fmt.Println("All files are up to date on remote, nothing to upload.")
		// Still need to finalize to apply the manifest to the agent
		manifest := transfer.BuildManifest(localFiles)
		if _, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentID, manifest); err != nil {
			return wrapHubError(fmt.Errorf("failed to finalize sync: %w", err))
		}
		fmt.Println("Workspace sync applied to agent.")
		return nil
	}

	fmt.Printf("Uploading %d files (%s)...\n", len(toUpload), humanize.Bytes(uint64(uploadSize)))

	// Create transfer client and upload files
	transferClient := transfer.NewClient(nil)

	var uploadedCount int
	var uploadedBytes int64

	progress := func(file transfer.FileInfo, bytesTransferred int64) error {
		uploadedCount++
		uploadedBytes += bytesTransferred
		fmt.Printf("  %s (%s) done\n", file.Path, humanize.Bytes(uint64(file.Size)))
		return nil
	}

	if err := transferClient.UploadFiles(ctx, toUpload, resp.UploadURLs, progress); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// Finalize the sync
	fmt.Println("Applying workspace to agent...")
	manifest := transfer.BuildManifest(localFiles)
	finalizeResp, err := hubCtx.Client.Workspace().FinalizeSyncTo(ctx, agentID, manifest)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to finalize sync: %w", err))
	}

	fmt.Printf("Sync complete: %d files uploaded, %s transferred\n", uploadedCount, humanize.Bytes(uint64(uploadedBytes)))
	if len(resp.ExistingFiles) > 0 {
		fmt.Printf("Skipped %d unchanged files\n", len(resp.ExistingFiles))
	}
	if finalizeResp.Applied {
		fmt.Printf("Applied %d files to agent workspace\n", finalizeResp.FilesApplied)
	}

	return nil
}

// resolveAgentID resolves an agent name to an agent ID.
func resolveAgentID(ctx context.Context, client hubclient.Client, groveID, agentName string) (string, error) {
	// List agents in the grove and find by name
	resp, err := client.GroveAgents(groveID).List(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to look up agent: %w", err)
	}

	// Find agent by name
	for _, agent := range resp.Agents {
		if agent.Name == agentName {
			// Check agent status
			if agent.Status != "running" {
				return "", fmt.Errorf("agent '%s' is not running (status: %s)", agentName, agent.Status)
			}
			return agent.Slug, nil
		}
	}

	return "", fmt.Errorf("agent '%s' not found in grove", agentName)
}

// resolveLocalWorkspacePath resolves the local workspace path for an agent.
func resolveLocalWorkspacePath(agentName string) (string, error) {
	// Resolve grove path
	var groveDir string
	if grovePath != "" {
		groveDir = grovePath
	} else {
		// Use current directory
		cwd, err := filepath.Abs(".")
		if err != nil {
			return ".", nil
		}
		groveDir = cwd
	}

	// Get grove name from the directory
	groveName := filepath.Base(groveDir)

	// Check for standard worktree location: {parent}/.scion_worktrees/{grove}/{agent}
	groveParent := filepath.Dir(groveDir)
	worktreePath := filepath.Join(groveParent, ".scion_worktrees", groveName, agentName)

	// If the worktree exists, use it
	if info, err := os.Stat(worktreePath); err == nil && info.IsDir() {
		return worktreePath, nil
	}

	// Fall back to current directory
	return ".", nil
}
