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
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var harnessConfigCmd = &cobra.Command{
	Use:     "harness-config",
	Aliases: []string{"hc"},
	Short:   "Manage harness configurations",
	Long:    `List and manage harness-config directories that define runtime settings for each harness type.`,
}

var harnessConfigListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available harness configurations",
	RunE: func(cmd *cobra.Command, args []string) error {
		var gp string
		if projectPath != "" {
			resolved, err := config.GetResolvedProjectDir(projectPath)
			if err == nil {
				gp = resolved
			}
		} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
			gp = projectDir
		}

		configs, err := config.ListHarnessConfigDirs(gp)
		if err != nil {
			return fmt.Errorf("failed to list harness configs: %w", err)
		}

		// Check for --hub flag
		showHub, _ := cmd.Flags().GetBool("hub")

		type hcEntry struct {
			Name    string `json:"name"`
			Harness string `json:"harness"`
			Image   string `json:"image,omitempty"`
			Path    string `json:"path,omitempty"`
			Source  string `json:"source"` // "local" or "hub"
			ID      string `json:"id,omitempty"`
			Status  string `json:"status,omitempty"`
		}

		entries := make([]hcEntry, 0, len(configs))
		for _, hc := range configs {
			entries = append(entries, hcEntry{
				Name:    hc.Name,
				Harness: hc.Config.Harness,
				Image:   hc.Config.Image,
				Path:    hc.Path,
				Source:  "local",
			})
		}

		// Include Hub results if requested
		if showHub {
			hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
			if err == nil {
				hubResp, err := hubCtx.Client.HarnessConfigs().List(context.Background(), &hubclient.ListHarnessConfigsOptions{
					Status: "active",
				})
				if err == nil {
					// Merge Hub results (avoid duplicates by name)
					localNames := make(map[string]bool)
					for _, e := range entries {
						localNames[e.Name] = true
					}
					for _, hc := range hubResp.HarnessConfigs {
						if !localNames[hc.Name] {
							entries = append(entries, hcEntry{
								Name:    hc.Name,
								Harness: hc.Harness,
								Source:  "hub",
								ID:      hc.ID,
								Status:  hc.Status,
							})
						}
					}
				}
			}
		}

		if len(entries) == 0 {
			fmt.Println("No harness configurations found.")
			fmt.Println("Run 'scion init --machine' to seed default harness configurations.")
			return nil
		}

		if isJSONOutput() {
			return outputJSON(entries)
		}

		if showHub {
			fmt.Printf("%-20s %-12s %-8s %s\n", "NAME", "HARNESS", "SOURCE", "IMAGE")
			for _, e := range entries {
				image := e.Image
				if len(image) > 50 {
					image = "..." + image[len(image)-47:]
				}
				fmt.Printf("%-20s %-12s %-8s %s\n", e.Name, e.Harness, e.Source, image)
			}
		} else {
			fmt.Printf("%-20s %-12s %s\n", "NAME", "HARNESS", "IMAGE")
			for _, e := range entries {
				image := e.Image
				if len(image) > 60 {
					image = "..." + image[len(image)-57:]
				}
				fmt.Printf("%-20s %-12s %s\n", e.Name, e.Harness, image)
			}
		}
		return nil
	},
}

var harnessConfigResetCmd = &cobra.Command{
	Use:   "reset <name>",
	Short: "Reset a harness configuration to its embedded defaults",
	Long: `Restores a harness-config directory to the embedded defaults.
This overwrites config.yaml and home directory files with the built-in versions.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Resolve the target directory (always global since that's where harness-configs live)
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to resolve global directory: %w", err)
		}

		targetDir := filepath.Join(globalDir, "harness-configs", name)

		// Load existing config to determine harness type
		hcDir, err := config.LoadHarnessConfigDir(targetDir)
		if err != nil {
			return fmt.Errorf("harness-config %q not found at %s: %w", name, targetDir, err)
		}

		// Reset always seeds from the built-in defaults of the underlying
		// harness type, so use the legacy New() shim here rather than
		// Resolve(); the latter would dispatch container-script when
		// activated, but reset must restore the embedded built-in fileset.
		h := harness.New(hcDir.Config.Harness)

		// Reset by re-seeding with force=true
		if err := config.SeedHarnessConfig(targetDir, h, true); err != nil {
			return fmt.Errorf("failed to reset harness-config %q: %w", name, err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "harness-config reset",
				Message: fmt.Sprintf("Harness-config %q reset to defaults.", name),
				Details: map[string]interface{}{
					"name":    name,
					"harness": hcDir.Config.Harness,
				},
			})
		}

		fmt.Printf("Harness-config %q reset to defaults.\n", name)
		return nil
	},
}

var harnessConfigUpgradeCmd = &cobra.Command{
	Use:   "upgrade [name]",
	Short: "Additively upgrade local harness-config directories",
	Long: `Adds missing embedded support files and merges missing declarative metadata into
local harness-config config.yaml files without clobbering existing user values.

By default this does not activate container-script provisioning. Use
--activate-script with a named harness-config after reviewing the staged files.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		activateScript, _ := cmd.Flags().GetBool("activate-script")
		force, _ := cmd.Flags().GetBool("force")
		if activateScript && len(args) == 0 {
			return fmt.Errorf("--activate-script requires a harness-config name")
		}

		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to resolve global directory: %w", err)
		}
		parentDir := filepath.Join(globalDir, "harness-configs")

		var names []string
		if len(args) == 1 {
			names = []string{args[0]}
		} else {
			entries, err := os.ReadDir(parentDir)
			if err != nil {
				return fmt.Errorf("failed to read harness-configs directory: %w", err)
			}
			for _, entry := range entries {
				if entry.IsDir() {
					names = append(names, entry.Name())
				}
			}
			sort.Strings(names)
		}

		plans := make([]*config.HarnessConfigUpgradePlan, 0, len(names))
		for _, name := range names {
			targetDir := filepath.Join(parentDir, name)
			hcDir, err := config.LoadHarnessConfigDir(targetDir)
			if err != nil {
				return fmt.Errorf("failed to load harness-config %q: %w", name, err)
			}
			h := harness.New(hcDir.Config.Harness)
			plan, err := config.UpgradeHarnessConfig(targetDir, h, config.HarnessConfigUpgradeOptions{
				DryRun:         dryRun,
				ActivateScript: activateScript,
				Force:          force,
			})
			if err != nil {
				return err
			}
			plans = append(plans, plan)
		}

		if isJSONOutput() {
			return outputJSON(plans)
		}

		if len(plans) == 0 {
			fmt.Println("No harness configurations found.")
			return nil
		}
		for _, plan := range plans {
			fmt.Println(config.SummarizeHarnessConfigUpgradePlan(plan))
			if len(plan.Backups) > 0 {
				for _, backup := range plan.Backups {
					fmt.Printf("  backup: %s\n", backup)
				}
			}
		}
		return nil
	},
}

var harnessConfigSyncCmd = &cobra.Command{
	Use:   "sync <name>",
	Short: "Sync a local harness-config to the Hub",
	Long:  `Uploads a local harness-config directory to the Hub for use by remote Runtime Brokers.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		var gp string
		if projectPath != "" {
			resolved, err := config.GetResolvedProjectDir(projectPath)
			if err == nil {
				gp = resolved
			}
		} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
			gp = projectDir
		}

		// Find the local harness-config
		hcDir, err := config.FindHarnessConfigDir(name, gp)
		if err != nil {
			return fmt.Errorf("harness-config %q not found: %w", name, err)
		}

		hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
		if err != nil {
			return err
		}

		PrintUsingHub(hubCtx.Endpoint)

		scope := "global"

		hubName, _ := cmd.Flags().GetString("name")
		if hubName == "" {
			hubName = name
		}

		return syncHarnessConfigToHub(hubCtx, hubName, hcDir.Path, scope, "", hcDir.Config.Harness)
	},
}

var harnessConfigPushCmd = &cobra.Command{
	Use:   "push <name>",
	Short: "Push a local harness-config to the Hub (alias for sync)",
	Args:  cobra.ExactArgs(1),
	RunE:  harnessConfigSyncCmd.RunE,
}

var harnessConfigPullCmd = &cobra.Command{
	Use:   "pull <name>",
	Short: "Download a harness-config from the Hub",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		var gp string
		if projectPath != "" {
			resolved, err := config.GetResolvedProjectDir(projectPath)
			if err == nil {
				gp = resolved
			}
		} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
			gp = projectDir
		}

		hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
		if err != nil {
			return err
		}

		PrintUsingHub(hubCtx.Endpoint)

		// Find the harness-config on the Hub
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
			Name:   name,
			Status: "active",
		})
		if err != nil {
			return fmt.Errorf("failed to search Hub: %w", err)
		}

		var match *hubclient.HarnessConfig
		for i := range resp.HarnessConfigs {
			if resp.HarnessConfigs[i].Name == name || resp.HarnessConfigs[i].Slug == name {
				match = &resp.HarnessConfigs[i]
				break
			}
		}

		if match == nil {
			return fmt.Errorf("harness-config %q not found on Hub", name)
		}

		toPath, _ := cmd.Flags().GetString("to")
		return pullHarnessConfigFromHub(hubCtx, match, toPath)
	},
}

var harnessConfigShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details of a harness configuration",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		var gp string
		if projectPath != "" {
			resolved, err := config.GetResolvedProjectDir(projectPath)
			if err == nil {
				gp = resolved
			}
		} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
			gp = projectDir
		}

		// Try local first
		hcDir, localErr := config.FindHarnessConfigDir(name, gp)
		if localErr == nil {
			if isJSONOutput() {
				return outputJSON(map[string]interface{}{
					"source":  "local",
					"name":    hcDir.Name,
					"harness": hcDir.Config.Harness,
					"image":   hcDir.Config.Image,
					"path":    hcDir.Path,
				})
			}
			fmt.Printf("Name:    %s\n", hcDir.Name)
			fmt.Printf("Source:  local\n")
			fmt.Printf("Harness: %s\n", hcDir.Config.Harness)
			fmt.Printf("Image:   %s\n", hcDir.Config.Image)
			fmt.Printf("Path:    %s\n", hcDir.Path)
			return nil
		}

		// Try Hub
		hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
		if err != nil {
			return fmt.Errorf("harness-config %q not found locally and Hub unavailable: %w", name, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
			Name:   name,
			Status: "active",
		})
		if err != nil {
			return fmt.Errorf("failed to search Hub: %w", err)
		}

		for _, hc := range resp.HarnessConfigs {
			if hc.Name == name || hc.Slug == name {
				if isJSONOutput() {
					return outputJSON(hc)
				}
				fmt.Printf("Name:         %s\n", hc.Name)
				fmt.Printf("Source:       hub\n")
				fmt.Printf("ID:           %s\n", hc.ID)
				fmt.Printf("Harness:      %s\n", hc.Harness)
				fmt.Printf("Status:       %s\n", hc.Status)
				fmt.Printf("Content Hash: %s\n", truncateHash(hc.ContentHash))
				fmt.Printf("Scope:        %s\n", hc.Scope)
				fmt.Printf("Files:        %d\n", len(hc.Files))
				return nil
			}
		}

		return fmt.Errorf("harness-config %q not found", name)
	},
}

var harnessConfigDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a harness-config from the Hub",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		var gp string
		if projectPath != "" {
			resolved, err := config.GetResolvedProjectDir(projectPath)
			if err == nil {
				gp = resolved
			}
		} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
			gp = projectDir
		}

		hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
		if err != nil {
			return err
		}

		PrintUsingHub(hubCtx.Endpoint)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Find the harness-config on Hub
		resp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
			Name:   name,
			Status: "active",
		})
		if err != nil {
			return fmt.Errorf("failed to search Hub: %w", err)
		}

		var match *hubclient.HarnessConfig
		for i := range resp.HarnessConfigs {
			if resp.HarnessConfigs[i].Name == name || resp.HarnessConfigs[i].Slug == name {
				match = &resp.HarnessConfigs[i]
				break
			}
		}

		if match == nil {
			return fmt.Errorf("harness-config %q not found on Hub", name)
		}

		if err := hubCtx.Client.HarnessConfigs().Delete(ctx, match.ID); err != nil {
			return fmt.Errorf("failed to delete harness-config: %w", err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "harness-config delete",
				Message: fmt.Sprintf("Harness-config '%s' deleted from Hub.", name),
				Details: map[string]interface{}{
					"id":   match.ID,
					"name": name,
				},
			})
		}

		fmt.Printf("Harness-config '%s' deleted from Hub.\n", name)
		return nil
	},
}

// syncHarnessConfigToHub creates or updates a harness config in the Hub.
func syncHarnessConfigToHub(hubCtx *HubContext, name, localPath, scope, scopeID, harnessType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if scope == "" {
		scope = "global"
	}

	// Collect local files
	fmt.Printf("Scanning harness-config files in %s...\n", localPath)
	files, err := hubclient.CollectFiles(localPath, nil)
	if err != nil {
		return fmt.Errorf("failed to scan harness-config files: %w", err)
	}
	fmt.Printf("Found %d files\n", len(files))

	fileReqs := make([]hubclient.FileUploadRequest, len(files))
	for i, f := range files {
		fileReqs[i] = hubclient.FileUploadRequest{
			Path: f.Path,
			Size: f.Size,
		}
	}

	// Check if it already exists
	var hcID string
	existingResp, err := hubCtx.Client.HarnessConfigs().List(ctx, &hubclient.ListHarnessConfigsOptions{
		Name:    name,
		Scope:   scope,
		ScopeID: scopeID,
		Status:  "active",
	})
	if err != nil {
		return fmt.Errorf("failed to check for existing harness-config: %w", err)
	}

	var existing *hubclient.HarnessConfig
	for i := range existingResp.HarnessConfigs {
		if existingResp.HarnessConfigs[i].Name == name {
			existing = &existingResp.HarnessConfigs[i]
			break
		}
	}

	localFileMap := make(map[string]*hubclient.FileInfo)
	for i := range files {
		localFileMap[files[i].Path] = &files[i]
	}

	var filesToUpload []hubclient.FileUploadRequest

	if existing != nil {
		hcID = existing.ID

		fmt.Printf("Checking for changes in harness-config '%s'...\n", name)
		downloadResp, err := hubCtx.Client.HarnessConfigs().RequestDownloadURLs(ctx, hcID)

		needsFullUpload := false
		if err != nil {
			if isHarnessConfigNoFilesError(err) {
				fmt.Printf("Harness-config '%s' exists but has no files. Uploading all files...\n", name)
				needsFullUpload = true
				filesToUpload = fileReqs
			} else {
				return fmt.Errorf("failed to get existing manifest: %w", err)
			}
		}

		if !needsFullUpload {
			remoteHashes := make(map[string]string)
			for _, f := range downloadResp.Files {
				remoteHashes[f.Path] = f.Hash
			}

			for _, localFile := range files {
				remoteHash, exists := remoteHashes[localFile.Path]
				if !exists || remoteHash != localFile.Hash {
					filesToUpload = append(filesToUpload, hubclient.FileUploadRequest{
						Path: localFile.Path,
						Size: localFile.Size,
					})
				}
			}

			if len(filesToUpload) == 0 {
				fmt.Printf("Harness-config '%s' is already up to date.\n", name)
				fmt.Printf("  ID: %s\n", hcID)
				fmt.Printf("  Content Hash: %s\n", truncateHash(existing.ContentHash))
				return nil
			}

			fmt.Printf("Found %d changed file(s), updating...\n", len(filesToUpload))
		}
	} else {
		fmt.Printf("Creating harness-config '%s' in Hub...\n", name)
		createReq := &hubclient.CreateHarnessConfigRequest{
			Name:    name,
			Harness: harnessType,
			Scope:   scope,
			ScopeID: scopeID,
		}

		resp, err := hubCtx.Client.HarnessConfigs().Create(ctx, createReq)
		if err != nil {
			return fmt.Errorf("failed to create harness-config: %w", err)
		}

		hcID = resp.HarnessConfig.ID
		fmt.Printf("Harness-config created with ID: %s\n", hcID)
		filesToUpload = fileReqs
	}

	// Request upload URLs
	fmt.Printf("Requesting upload URLs for %d file(s)...\n", len(filesToUpload))
	uploadResp, err := hubCtx.Client.HarnessConfigs().RequestUploadURLs(ctx, hcID, filesToUpload)
	if err != nil {
		return fmt.Errorf("failed to get upload URLs: %w", err)
	}

	// Upload files
	fmt.Printf("Uploading %d file(s)...\n", len(uploadResp.UploadURLs))
	if err := uploadHarnessConfigFiles(ctx, hubCtx.Client.HarnessConfigs(), hcID, localFileMap, filesToUpload, uploadResp.UploadURLs); err != nil {
		return err
	}

	// Build manifest
	manifest := &hubclient.HarnessConfigManifest{
		Version: "1.0",
		Harness: harnessType,
		Files:   make([]hubclient.TemplateFile, len(files)),
	}
	for i, f := range files {
		manifest.Files[i] = hubclient.TemplateFile{
			Path: f.Path,
			Size: f.Size,
			Hash: f.Hash,
			Mode: f.Mode,
		}
	}

	// Finalize
	fmt.Println("Finalizing harness-config...")
	hc, err := hubCtx.Client.HarnessConfigs().Finalize(ctx, hcID, manifest)
	if err != nil {
		if !isHarnessConfigMissingFileError(err) {
			return fmt.Errorf("failed to finalize: %w", err)
		}

		// Retry with all files
		fmt.Println("Some files missing from storage, re-uploading all files...")
		retryResp, retryErr := hubCtx.Client.HarnessConfigs().RequestUploadURLs(ctx, hcID, fileReqs)
		if retryErr != nil {
			return fmt.Errorf("failed to get upload URLs for retry: %w", retryErr)
		}
		for _, urlInfo := range retryResp.UploadURLs {
			fileInfo := localFileMap[urlInfo.Path]
			if fileInfo == nil {
				continue
			}
			if err := uploadHarnessConfigFileBySignedURL(ctx, hubCtx.Client.HarnessConfigs(), fileInfo, urlInfo); err != nil {
				return err
			}
			fmt.Printf("  Re-uploaded: %s\n", fileInfo.Path)
		}
		hc, err = hubCtx.Client.HarnessConfigs().Finalize(ctx, hcID, manifest)
		if err != nil {
			return fmt.Errorf("failed to finalize after retry: %w", err)
		}
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "harness-config sync",
			Message: fmt.Sprintf("Harness-config '%s' synced successfully.", name),
			Details: map[string]interface{}{
				"id":            hc.ID,
				"name":          name,
				"status":        hc.Status,
				"contentHash":   hc.ContentHash,
				"scope":         scope,
				"filesUploaded": len(filesToUpload),
			},
		})
	}

	fmt.Printf("Harness-config '%s' synced successfully!\n", name)
	fmt.Printf("  ID: %s\n", hc.ID)
	fmt.Printf("  Status: %s\n", hc.Status)
	fmt.Printf("  Content Hash: %s\n", truncateHash(hc.ContentHash))

	return nil
}

func isHarnessConfigNoFilesError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "harness config has no files")
}

func isHarnessConfigMissingFileError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "file not found")
}

// pullHarnessConfigFromHub downloads a harness config from the Hub to local disk.
//
// Phase 3: each downloaded file is hashed and compared against the
// fileInfo.Hash announced by the Hub. A mismatch aborts the pull before any
// file is written, so a tampered or corrupted artifact never reaches an
// agent's harness-config dir. Files without a Hub-side hash are accepted
// (legacy artifacts pre-date file-level hashes); operators can detect those
// by inspecting the manifest's overall ContentHash, which is still printed
// after a successful sync.
func pullHarnessConfigFromHub(hubCtx *HubContext, hc *hubclient.HarnessConfig, toPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	name := hc.Name

	destPath := toPath
	if destPath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to get global directory: %w", err)
		}
		destPath = filepath.Join(globalDir, "harness-configs", name)
	} else {
		var err error
		destPath, err = filepath.Abs(toPath)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}
	}

	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	fmt.Printf("Requesting download URLs for harness-config '%s'...\n", name)
	downloadResp, err := hubCtx.Client.HarnessConfigs().RequestDownloadURLs(ctx, hc.ID)
	if err != nil {
		return fmt.Errorf("failed to get download URLs: %w", err)
	}

	// Two-phase download: fetch + verify before writing anything to disk so
	// a hash mismatch never leaves a partially-installed harness-config.
	fmt.Printf("Downloading %d files to %s...\n", len(downloadResp.Files), destPath)
	useHubFileRead := hasLocalDownloadURLs(downloadResp.Files)
	type pendingFile struct {
		path    string
		content []byte
		relPath string
	}
	pending := make([]pendingFile, 0, len(downloadResp.Files))
	for _, fileInfo := range downloadResp.Files {
		filePath := filepath.Join(destPath, filepath.FromSlash(fileInfo.Path))

		content, err := downloadHarnessConfigContent(ctx, hubCtx.Client.HarnessConfigs(), hc.ID, fileInfo, useHubFileRead)
		if err != nil {
			return err
		}
		if err := verifyHarnessConfigArtifactHash(fileInfo, content); err != nil {
			return fmt.Errorf("harness-config %q: %w", name, err)
		}
		pending = append(pending, pendingFile{path: filePath, content: content, relPath: fileInfo.Path})
	}
	for _, f := range pending {
		if err := ensureParentDir(f.path); err != nil {
			return err
		}
		if err := writeHarnessConfigFile(f.path, f.content); err != nil {
			return err
		}
		fmt.Printf("  Downloaded: %s\n", f.relPath)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "harness-config pull",
			Message: fmt.Sprintf("Harness-config '%s' pulled successfully.", name),
			Details: map[string]interface{}{
				"name":        name,
				"id":          hc.ID,
				"destination": destPath,
				"filesCount":  len(downloadResp.Files),
			},
		})
	}

	fmt.Printf("Harness-config '%s' pulled successfully to %s\n", name, destPath)

	return nil
}

func init() {
	rootCmd.AddCommand(harnessConfigCmd)
	harnessConfigCmd.AddCommand(harnessConfigListCmd)
	harnessConfigCmd.AddCommand(harnessConfigResetCmd)
	harnessConfigCmd.AddCommand(harnessConfigUpgradeCmd)
	harnessConfigCmd.AddCommand(harnessConfigSyncCmd)
	harnessConfigCmd.AddCommand(harnessConfigPushCmd)
	harnessConfigCmd.AddCommand(harnessConfigPullCmd)
	harnessConfigCmd.AddCommand(harnessConfigShowCmd)
	harnessConfigCmd.AddCommand(harnessConfigDeleteCmd)
	harnessConfigCmd.AddCommand(harnessConfigInstallCmd)

	// Flags for list command
	harnessConfigListCmd.Flags().Bool("hub", false, "Include Hub results")

	// Flags for upgrade command
	harnessConfigUpgradeCmd.Flags().Bool("dry-run", false, "Show planned changes without writing files")
	harnessConfigUpgradeCmd.Flags().Bool("activate-script", false, "Activate container-script provisioning for the named harness-config")
	harnessConfigUpgradeCmd.Flags().Bool("force", false, "Reset harness-configs to embedded defaults before future additive migration steps")

	// Flags for sync command
	harnessConfigSyncCmd.Flags().String("name", "", "Name for the harness-config on the Hub")

	// Flags for push command
	harnessConfigPushCmd.Flags().String("name", "", "Name for the harness-config on the Hub")

	// Flags for pull command
	harnessConfigPullCmd.Flags().String("to", "", "Destination path for downloaded harness-config")
}
