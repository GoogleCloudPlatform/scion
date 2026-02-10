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
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/spf13/cobra"
)

// templatesCmd represents the templates command
var templatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "Manage agent templates",
	Long:  `List and inspect templates used to provision new agents.`,
}

var templatesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available templates",
	RunE:  runTemplateList,
}

func runTemplateList(cmd *cobra.Command, args []string) error {
	// Get local templates grouped by scope
	localGlobal, localGrove, err := config.ListTemplatesGrouped()
	if err != nil {
		return err
	}

	// Check if Hub is available (suppress errors, just skip Hub if not available)
	var hubCtx *HubContext
	var hubGlobal, hubGrove []hubclient.Template
	hubAvailable := false

	if !noHub {
		hubCtx, _ = CheckHubAvailabilityWithOptions(grovePath, true)
		if hubCtx != nil {
			hubAvailable = true
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Get grove ID for filtering grove-scoped templates
			groveID, _ := GetGroveID(hubCtx)

			// Fetch global templates from Hub
			globalResp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
				Scope:  "global",
				Status: "active",
			})
			if err == nil {
				hubGlobal = globalResp.Templates
			}

			// Fetch grove templates from Hub (if we have a grove ID)
			if groveID != "" {
				groveResp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
					Scope:   "grove",
					GroveID: groveID,
					Status:  "active",
				})
				if err == nil {
					hubGrove = groveResp.Templates
				}
			}

			// Sort hub templates by name for consistent output
			sort.Slice(hubGlobal, func(i, j int) bool {
				return hubGlobal[i].Name < hubGlobal[j].Name
			})
			sort.Slice(hubGrove, func(i, j int) bool {
				return hubGrove[i].Name < hubGrove[j].Name
			})
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if hubAvailable {
		// Hub mode: group by local/hub, then by global/grove
		printTemplateListHubMode(w, localGlobal, localGrove, hubGlobal, hubGrove)
	} else {
		// Local mode: group by global/grove
		printTemplateListLocalMode(w, localGlobal, localGrove)
	}

	w.Flush()
	return nil
}

func printTemplateListLocalMode(w *tabwriter.Writer, global, grove []*config.Template) {
	hasGlobal := len(global) > 0
	hasGrove := len(grove) > 0

	if !hasGlobal && !hasGrove {
		fmt.Fprintln(w, "No templates found.")
		return
	}

	if hasGlobal {
		fmt.Fprintln(w, "Global Templates:")
		fmt.Fprintln(w, "  NAME\tPATH")
		for _, t := range global {
			fmt.Fprintf(w, "  %s\t%s\n", t.Name, t.Path)
		}
	}

	if hasGrove {
		if hasGlobal {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Grove Templates:")
		fmt.Fprintln(w, "  NAME\tPATH")
		for _, t := range grove {
			fmt.Fprintf(w, "  %s\t%s\n", t.Name, t.Path)
		}
	}
}

func printTemplateListHubMode(w *tabwriter.Writer, localGlobal, localGrove []*config.Template, hubGlobal, hubGrove []hubclient.Template) {
	hasLocalGlobal := len(localGlobal) > 0
	hasLocalGrove := len(localGrove) > 0
	hasHubGlobal := len(hubGlobal) > 0
	hasHubGrove := len(hubGrove) > 0

	hasLocal := hasLocalGlobal || hasLocalGrove
	hasHub := hasHubGlobal || hasHubGrove

	if !hasLocal && !hasHub {
		fmt.Fprintln(w, "No templates found.")
		return
	}

	// Local section
	if hasLocal {
		fmt.Fprintln(w, "Local Templates:")
		if hasLocalGlobal {
			fmt.Fprintln(w, "  Global:")
			fmt.Fprintln(w, "    NAME\tPATH")
			for _, t := range localGlobal {
				fmt.Fprintf(w, "    %s\t%s\n", t.Name, t.Path)
			}
		}
		if hasLocalGrove {
			if hasLocalGlobal {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "  Grove:")
			fmt.Fprintln(w, "    NAME\tPATH")
			for _, t := range localGrove {
				fmt.Fprintf(w, "    %s\t%s\n", t.Name, t.Path)
			}
		}
	}

	// Hub section
	if hasHub {
		if hasLocal {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Hub Templates:")
		if hasHubGlobal {
			fmt.Fprintln(w, "  Global:")
			fmt.Fprintln(w, "    NAME\tID")
			for _, t := range hubGlobal {
				fmt.Fprintf(w, "    %s\t%s\n", t.Name, t.ID)
			}
		}
		if hasHubGrove {
			if hasHubGlobal {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "  Grove:")
			fmt.Fprintln(w, "    NAME\tID")
			for _, t := range hubGrove {
				fmt.Fprintf(w, "    %s\t%s\n", t.Name, t.ID)
			}
		}
	}
}

var templatesShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show template configuration",
	Args:  cobra.ExactArgs(1),
	RunE:  runTemplateShow,
}

func runTemplateShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Get flags - handle nil cmd for testing
	var localOnly, hubOnly bool
	if cmd != nil {
		localOnly, _ = cmd.Flags().GetBool("local")
		hubOnly, _ = cmd.Flags().GetBool("hub")
	}

	// Build resolution options
	opts := &ResolveOpts{
		LocalOnly:   localOnly,
		HubOnly:     hubOnly,
		GroveOnly:   false,
		GlobalOnly:  globalMode,
		AutoConfirm: autoConfirm,
	}

	// Check if Hub is available (suppress errors for read operations)
	var hubCtx *HubContext
	if !noHub && !localOnly {
		hubCtx, _ = CheckHubAvailabilityWithOptions(grovePath, true)
	}

	ctx := context.Background()
	match, err := ResolveTemplate(ctx, name, hubCtx, opts, "show")
	if err != nil {
		return err
	}

	// Display based on whether local or Hub template
	if match.IsLocal() {
		// Load and display local template
		tpl := &config.Template{Name: match.Name, Path: match.LocalPath}
		cfg, err := tpl.LoadConfig()
		if err != nil {
			return err
		}

		fmt.Printf("Template: %s\n", tpl.Name)
		fmt.Printf("Location: %s\n", match.DisplayLocation())
		fmt.Printf("Path:     %s\n", tpl.Path)
		fmt.Println("Configuration (scion-agent.json):")

		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(cfg)
	}

	// Hub template - display hub info
	t := match.HubTemplate
	fmt.Printf("Template: %s\n", t.Name)
	fmt.Printf("Location: %s\n", match.DisplayLocation())
	fmt.Printf("ID:       %s\n", t.ID)
	fmt.Printf("Harness:  %s\n", t.Harness)
	fmt.Printf("Scope:    %s\n", t.Scope)
	fmt.Printf("Status:   %s\n", t.Status)
	if t.ContentHash != "" {
		fmt.Printf("Hash:     %s\n", truncateHash(t.ContentHash))
	}
	if t.Description != "" {
		fmt.Printf("Description: %s\n", t.Description)
	}
	fmt.Printf("Created:  %s\n", t.Created.Format(time.RFC3339))
	fmt.Printf("Updated:  %s\n", t.Updated.Format(time.RFC3339))

	return nil
}

var templatesCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		global, _ := cmd.Flags().GetBool("global")
		harnessName, _ := cmd.Flags().GetString("harness")
		if harnessName == "" {
			harnessName = "gemini"
		}

		h := harness.New(harnessName)

		err := config.CreateTemplate(name, h, global)
		if err != nil {
			return err
		}
		fmt.Printf("Template %s created successfully.\n", name)
		return nil
	},
}

var templatesDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm"},
	Short:   "Delete a template",
	Args:    cobra.ExactArgs(1),
	RunE:    runTemplateDelete,
}

// runTemplateDelete implements the delete command with confirmation prompts.
// It searches all 4 locations for the template, then prompts for which to delete.
func runTemplateDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Get flags - handle nil cmd for testing
	var localOnly, hubOnly bool
	if cmd != nil {
		localOnly, _ = cmd.Flags().GetBool("local")
		hubOnly, _ = cmd.Flags().GetBool("hub")
	}

	// Build resolution options
	opts := &ResolveOpts{
		LocalOnly:   localOnly,
		HubOnly:     hubOnly,
		GroveOnly:   false,
		GlobalOnly:  globalMode,
		AutoConfirm: autoConfirm,
	}

	// Check if Hub is available (suppress errors for delete operations)
	var hubCtx *HubContext
	if !noHub && !localOnly {
		hubCtx, _ = CheckHubAvailabilityWithOptions(grovePath, true)
	}

	ctx := context.Background()
	matches, err := ResolveTemplateForDelete(ctx, name, hubCtx, opts)
	if err != nil {
		return err
	}

	// Delete each selected template
	for _, match := range matches {
		if err := deleteTemplateMatch(ctx, &match, hubCtx); err != nil {
			return err
		}
	}

	return nil
}

// deleteTemplateMatch deletes a single template match after confirmation.
func deleteTemplateMatch(ctx context.Context, match *TemplateMatch, hubCtx *HubContext) error {
	// Confirm deletion
	prompt := fmt.Sprintf("Delete template '%s' from %s?", match.Name, match.DisplayLocation())
	if !hubsync.ConfirmAction(prompt, true, autoConfirm) {
		fmt.Println("Skipped.")
		return nil
	}

	if match.IsLocal() {
		// Delete local template
		isGlobal := match.Location == LocationLocalGlobal
		if err := config.DeleteTemplate(match.Name, isGlobal); err != nil {
			return fmt.Errorf("failed to delete local template: %w", err)
		}
		fmt.Printf("Local template '%s' deleted successfully.\n", match.Name)
	} else {
		// Delete hub template
		if hubCtx == nil {
			return fmt.Errorf("Hub context not available for deleting Hub template")
		}
		deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := hubCtx.Client.Templates().Delete(deleteCtx, match.HubTemplate.ID); err != nil {
			return fmt.Errorf("failed to delete Hub template: %w", err)
		}
		fmt.Printf("Hub template '%s' deleted successfully.\n", match.Name)
	}

	return nil
}

var templatesCloneCmd = &cobra.Command{
	Use:   "clone <src-name> <dest-name>",
	Short: "Clone an existing template",
	Args:  cobra.ExactArgs(2),
	RunE:  runTemplateClone,
}

func runTemplateClone(cmd *cobra.Command, args []string) error {
	srcName := args[0]
	destName := args[1]

	// Get flags - handle nil cmd for testing
	var localOnly, hubOnly bool
	if cmd != nil {
		localOnly, _ = cmd.Flags().GetBool("local")
		hubOnly, _ = cmd.Flags().GetBool("hub")
	}

	// Destination scope from root's --global flag
	destGlobal := globalMode

	// Build resolution options
	opts := &ResolveOpts{
		LocalOnly:   localOnly,
		HubOnly:     hubOnly,
		AutoConfirm: autoConfirm,
	}

	// Check if Hub is available for cloning from Hub templates
	var hubCtx *HubContext
	if !noHub && !localOnly {
		hubCtx, _ = CheckHubAvailabilityWithOptions(grovePath, true)
	}

	ctx := context.Background()
	match, err := ResolveTemplate(ctx, srcName, hubCtx, opts, "clone")
	if err != nil {
		return err
	}

	// If source is a Hub template, we need to pull it first then clone
	if match.IsHub() {
		// Pull the Hub template to a temp location, then clone
		return cloneFromHubTemplate(hubCtx, match, destName, destGlobal)
	}

	// Local source - use existing clone function with the resolved path
	// We need to use the resolved path directly
	if err := cloneLocalTemplate(match.LocalPath, destName, destGlobal); err != nil {
		return err
	}

	fmt.Printf("Template '%s' cloned to '%s' successfully.\n", srcName, destName)
	return nil
}

// cloneLocalTemplate clones from a local path to a destination.
func cloneLocalTemplate(srcPath, destName string, destGlobal bool) error {
	var destTemplatesDir string
	var err error

	if destGlobal {
		destTemplatesDir, err = config.GetGlobalTemplatesDir()
	} else {
		destTemplatesDir, err = config.GetProjectTemplatesDir()
	}
	if err != nil {
		return err
	}

	destPath := filepath.Join(destTemplatesDir, destName)
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("template %s already exists at %s", destName, destPath)
	}

	return util.CopyDir(srcPath, destPath)
}

// cloneFromHubTemplate pulls a Hub template and clones it locally.
func cloneFromHubTemplate(hubCtx *HubContext, match *TemplateMatch, destName string, destGlobal bool) error {
	if hubCtx == nil {
		return fmt.Errorf("Hub context not available for cloning Hub template")
	}

	// Determine destination path
	var destTemplatesDir string
	var err error

	if destGlobal {
		destTemplatesDir, err = config.GetGlobalTemplatesDir()
	} else {
		destTemplatesDir, err = config.GetProjectTemplatesDir()
	}
	if err != nil {
		return err
	}

	destPath := filepath.Join(destTemplatesDir, destName)
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("template %s already exists at %s", destName, destPath)
	}

	// Pull directly to destination path
	fmt.Printf("Cloning Hub template '%s' to local '%s'...\n", match.Name, destName)
	return pullTemplateFromHubMatch(hubCtx, match, destPath)
}

var templatesUpdateDefaultCmd = &cobra.Command{
	Use:   "update-default",
	Short: "Update default templates with the latest from the binary",
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		harnesses := harness.All()
		err := config.UpdateDefaultTemplates(global, harnesses)
		if err != nil {
			return err
		}
		fmt.Println("Default templates updated successfully.")
		return nil
	},
}

// templatesSyncCmd creates or updates a template in the Hub (upsert).
var templatesSyncCmd = &cobra.Command{
	Use:   "sync <template>",
	Short: "Create or update a template in the Hub (Hub only)",
	Long: `Sync a local template to the Hub. Creates the template if it doesn't exist,
or updates it if it does. This is an upsert operation.

The harness type is automatically detected from the template's configuration file.
Use the root --global flag to sync to global scope instead of grove scope.

Examples:
  # Sync a local template to the Hub (grove scope by default)
  scion templates sync custom-claude

  # Sync with global scope
  scion --global templates sync custom-claude

  # Sync with a different name on the Hub
  scion templates sync custom-claude --name my-team-claude`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateSync,
}

// templatesPushCmd is a semantic alias for sync.
var templatesPushCmd = &cobra.Command{
	Use:   "push <template>",
	Short: "Upload local template to Hub (alias for sync)",
	Long: `Push a local template to the Hub. This is a semantic alias for 'sync'.

Examples:
  # Push a local template to the Hub
  scion templates push custom-claude

  # Push with global scope
  scion --global templates push custom-claude`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateSync,
}

// runTemplateSync implements the shared logic for sync and push commands.
func runTemplateSync(cmd *cobra.Command, args []string) error {
	localTemplateName := args[0]

	// Get flags - handle nil cmd for testing
	var hubName string
	if cmd != nil {
		hubName, _ = cmd.Flags().GetString("name")
	}

	// Determine destination scope from root's --global flag
	destScope := "grove"
	if globalMode {
		destScope = "global"
	}

	// If no explicit Hub name, use the local template name
	if hubName == "" {
		hubName = localTemplateName
	}

	// Check Hub availability first (we need it for sync anyway)
	hubCtx, err := CheckHubAvailability(grovePath)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("Hub integration is not enabled. Use 'scion hub enable' first")
	}

	PrintUsingHub(hubCtx.Endpoint)

	// Build resolution options - local only for source, since we're syncing TO hub
	opts := &ResolveOpts{
		LocalOnly:   true,
		AutoConfirm: autoConfirm,
	}

	ctx := context.Background()
	match, err := ResolveTemplate(ctx, localTemplateName, nil, opts, "sync")
	if err != nil {
		return fmt.Errorf("template '%s' not found locally: %w", localTemplateName, err)
	}

	if !match.IsLocal() {
		return fmt.Errorf("internal error: expected local template but got hub")
	}

	// Create template object for harness detection
	tpl := &config.Template{Name: match.Name, Path: match.LocalPath}

	// Detect harness type from template config
	harnessType, err := detectHarnessType(tpl)
	if err != nil {
		return fmt.Errorf("failed to detect harness type: %w\n\n"+
			"Ensure the template has a valid scion-agent.json with a 'harness' field", err)
	}

	return syncTemplateToHub(hubCtx, hubName, tpl.Path, destScope, harnessType)
}


// templatesPullCmd downloads a template from the Hub.
var templatesPullCmd = &cobra.Command{
	Use:   "pull <name>",
	Short: "Download a template from Hub to local cache (Hub only)",
	Long: `Pull a template from the Hub to the local filesystem.

Examples:
  # Pull a template from Hub
  scion template pull custom-claude

  # Pull to a specific location
  scion template pull custom-claude --to .scion/templates/custom`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplatePull,
}

func runTemplatePull(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Get flags - handle nil cmd for testing
	var toPath string
	if cmd != nil {
		toPath, _ = cmd.Flags().GetString("to")
	}

	// Check Hub availability
	hubCtx, err := CheckHubAvailability(grovePath)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("Hub integration is not enabled. Use 'scion hub enable' first")
	}

	PrintUsingHub(hubCtx.Endpoint)

	// Build resolution options - Hub only for pull
	opts := &ResolveOpts{
		HubOnly:     true,
		GroveOnly:   false,
		GlobalOnly:  globalMode,
		AutoConfirm: autoConfirm,
	}

	ctx := context.Background()
	match, err := ResolveTemplate(ctx, name, hubCtx, opts, "pull")
	if err != nil {
		return err
	}

	if !match.IsHub() {
		return fmt.Errorf("internal error: expected Hub template but got local")
	}

	return pullTemplateFromHubMatch(hubCtx, match, toPath)
}

// pullTemplateFromHubMatch downloads a template from a resolved Hub match.
func pullTemplateFromHubMatch(hubCtx *HubContext, match *TemplateMatch, toPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	template := match.HubTemplate
	name := template.Name

	// Determine destination path
	destPath := toPath
	if destPath == "" {
		// Default to project templates directory
		projectTemplatesDir, err := config.GetProjectTemplatesDir()
		if err != nil {
			return fmt.Errorf("failed to get templates directory: %w", err)
		}
		destPath = filepath.Join(projectTemplatesDir, name)
	} else {
		var err error
		destPath, err = filepath.Abs(toPath)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}
	}

	// Create destination directory
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Request download URLs
	fmt.Printf("Requesting download URLs for template '%s'...\n", name)
	downloadResp, err := hubCtx.Client.Templates().RequestDownloadURLs(ctx, template.ID)
	if err != nil {
		return fmt.Errorf("failed to get download URLs: %w", err)
	}

	// Download files
	fmt.Printf("Downloading %d files to %s...\n", len(downloadResp.Files), destPath)
	for _, fileInfo := range downloadResp.Files {
		filePath := filepath.Join(destPath, filepath.FromSlash(fileInfo.Path))

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", fileInfo.Path, err)
		}

		// Download file content
		content, err := hubCtx.Client.Templates().DownloadFile(ctx, fileInfo.URL)
		if err != nil {
			return fmt.Errorf("failed to download %s: %w", fileInfo.Path, err)
		}

		// Write file
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", fileInfo.Path, err)
		}
		fmt.Printf("  Downloaded: %s\n", fileInfo.Path)
	}

	fmt.Printf("Template '%s' pulled successfully to %s\n", name, destPath)

	return nil
}

// syncTemplateToHub creates or updates a template in the Hub.
func syncTemplateToHub(hubCtx *HubContext, name, localPath, scope, harnessType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Default scope
	if scope == "" {
		scope = "grove"
	}

	// Collect local files
	fmt.Printf("Scanning template files in %s...\n", localPath)
	files, err := hubclient.CollectFiles(localPath, nil)
	if err != nil {
		return fmt.Errorf("failed to scan template files: %w", err)
	}
	fmt.Printf("Found %d files\n", len(files))

	// Build file upload request
	fileReqs := make([]hubclient.FileUploadRequest, len(files))
	for i, f := range files {
		fileReqs[i] = hubclient.FileUploadRequest{
			Path: f.Path,
			Size: f.Size,
		}
	}

	// Get grove ID for grove scope
	var groveID string
	if scope == "grove" {
		groveID, err = GetGroveID(hubCtx)
		if err != nil {
			return err
		}
	}

	// Check if a template with this name already exists in the same scope
	var templateID string
	existingResp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
		Name:    name,
		Scope:   scope,
		GroveID: groveID,
		Status:  "active",
	})
	if err != nil {
		return fmt.Errorf("failed to check for existing template: %w", err)
	}

	// Find exact name match
	var existingTemplate *hubclient.Template
	for i := range existingResp.Templates {
		if existingResp.Templates[i].Name == name {
			existingTemplate = &existingResp.Templates[i]
			break
		}
	}

	// Build a map of local files by path for easy lookup
	localFileMap := make(map[string]*hubclient.FileInfo)
	for i := range files {
		localFileMap[files[i].Path] = &files[i]
	}

	// Track which files need to be uploaded
	var filesToUpload []hubclient.FileUploadRequest

	if existingTemplate != nil {
		templateID = existingTemplate.ID

		// Fetch existing file manifest to compare hashes
		fmt.Printf("Checking for changes in template '%s'...\n", name)
		downloadResp, err := hubCtx.Client.Templates().RequestDownloadURLs(ctx, templateID)

		// Check if the template exists but has no files (e.g., due to previous storage misconfiguration)
		// In this case, treat it like a new template that needs all files uploaded
		templateNeedsFullUpload := false
		if err != nil {
			// Check for "template has no files" error - this means the template record exists
			// but was never finalized (e.g., storage was misconfigured during initial sync)
			if strings.Contains(err.Error(), "template has no files") {
				fmt.Printf("Template '%s' exists but has no files (possibly from incomplete previous sync).\n", name)
				fmt.Printf("Uploading all files...\n")
				templateNeedsFullUpload = true
				filesToUpload = fileReqs
			} else {
				return fmt.Errorf("failed to get existing template manifest: %w", err)
			}
		}

		if !templateNeedsFullUpload {
			// Build map of remote file hashes
			remoteHashes := make(map[string]string)
			for _, f := range downloadResp.Files {
				remoteHashes[f.Path] = f.Hash
			}

			// Compare local vs remote - find changed/new files
			for _, localFile := range files {
				remoteHash, exists := remoteHashes[localFile.Path]
				if !exists || remoteHash != localFile.Hash {
					filesToUpload = append(filesToUpload, hubclient.FileUploadRequest{
						Path: localFile.Path,
						Size: localFile.Size,
					})
				}
			}

			// Check if anything changed
			if len(filesToUpload) == 0 {
				fmt.Printf("Template '%s' is already up to date.\n", name)
				fmt.Printf("  ID: %s\n", templateID)
				fmt.Printf("  Content Hash: %s\n", existingTemplate.ContentHash)
				return nil
			}

			fmt.Printf("Found %d changed file(s), updating template...\n", len(filesToUpload))
		}
	} else {
		// Create new template - upload all files
		fmt.Printf("Creating template '%s' in Hub...\n", name)
		createReq := &hubclient.CreateTemplateRequest{
			Name:    name,
			Harness: harnessType,
			Scope:   scope,
			GroveID: groveID,
		}

		resp, err := hubCtx.Client.Templates().Create(ctx, createReq)
		if err != nil {
			return fmt.Errorf("failed to create template: %w", err)
		}

		templateID = resp.Template.ID
		fmt.Printf("Template created with ID: %s\n", templateID)

		// All files need to be uploaded for new templates
		filesToUpload = fileReqs
	}

	// Request upload URLs only for files that need uploading
	fmt.Printf("Requesting upload URLs for %d file(s)...\n", len(filesToUpload))
	uploadResp, err := hubCtx.Client.Templates().RequestUploadURLs(ctx, templateID, filesToUpload)
	if err != nil {
		return fmt.Errorf("failed to get upload URLs: %w", err)
	}

	// Upload files
	fmt.Printf("Uploading %d file(s)...\n", len(uploadResp.UploadURLs))
	for _, urlInfo := range uploadResp.UploadURLs {
		fileInfo := localFileMap[urlInfo.Path]
		if fileInfo == nil {
			fmt.Printf("  Warning: no matching file for %s\n", urlInfo.Path)
			continue
		}

		// Open and upload file
		f, err := os.Open(fileInfo.FullPath)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", fileInfo.Path, err)
		}

		err = hubCtx.Client.Templates().UploadFile(ctx, urlInfo.URL, urlInfo.Method, urlInfo.Headers, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("failed to upload %s: %w", fileInfo.Path, err)
		}
		fmt.Printf("  Uploaded: %s\n", fileInfo.Path)
	}

	// Build manifest
	manifest := &hubclient.TemplateManifest{
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

	// Finalize template
	fmt.Println("Finalizing template...")
	template, err := hubCtx.Client.Templates().Finalize(ctx, templateID, manifest)
	if err != nil {
		return fmt.Errorf("failed to finalize template: %w", err)
	}

	fmt.Printf("Template '%s' synced successfully!\n", name)
	fmt.Printf("  ID: %s\n", template.ID)
	fmt.Printf("  Status: %s\n", template.Status)
	fmt.Printf("  Content Hash: %s\n", template.ContentHash)

	return nil
}

func init() {
	rootCmd.AddCommand(templatesCmd)
	templatesCmd.AddCommand(templatesListCmd)
	templatesCmd.AddCommand(templatesShowCmd)
	templatesCmd.AddCommand(templatesCreateCmd)
	templatesCmd.AddCommand(templatesCloneCmd)
	templatesCmd.AddCommand(templatesDeleteCmd)
	templatesCmd.AddCommand(templatesUpdateDefaultCmd)

	// Hub-only commands
	templatesCmd.AddCommand(templatesSyncCmd)
	templatesCmd.AddCommand(templatesPushCmd)
	templatesCmd.AddCommand(templatesPullCmd)

	// Flags for show command
	templatesShowCmd.Flags().Bool("local", false, "Only search local filesystem")
	templatesShowCmd.Flags().Bool("hub", false, "Only search Hub")

	// Flags for delete command
	templatesDeleteCmd.Flags().Bool("local", false, "Only search local filesystem")
	templatesDeleteCmd.Flags().Bool("hub", false, "Only search Hub")

	// Flags for clone command
	templatesCloneCmd.Flags().Bool("local", false, "Only search local filesystem for source")
	templatesCloneCmd.Flags().Bool("hub", false, "Only search Hub for source")

	// Flags for create command
	templatesCreateCmd.Flags().StringP("harness", "H", "", "Harness type (e.g. gemini, claude)")

	// Flags for sync command (--global is inherited from root)
	templatesSyncCmd.Flags().String("name", "", "Name for the template on the Hub (defaults to local template name)")

	// Flags for push command (same as sync, since push is an alias)
	templatesPushCmd.Flags().String("name", "", "Name for the template on the Hub (defaults to local template name)")

	// Flags for pull command
	templatesPullCmd.Flags().String("to", "", "Destination path for downloaded template")

	// Also add a 'template' alias (singular) for convenience
	templateCmd := &cobra.Command{
		Use:   "template",
		Short: "Manage agent templates (alias for 'templates')",
		Long:  `List and inspect templates used to provision new agents.`,
	}
	rootCmd.AddCommand(templateCmd)
	templateCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List available templates",
		RunE:  runTemplateList,
	})
	showAlias := &cobra.Command{
		Use:   "show <name>",
		Short: "Show template configuration",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplateShow,
	}
	showAlias.Flags().Bool("local", false, "Only search local filesystem")
	showAlias.Flags().Bool("hub", false, "Only search Hub")
	templateCmd.AddCommand(showAlias)

	deleteAlias := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a template",
		Args:    cobra.ExactArgs(1),
		RunE:    runTemplateDelete,
	}
	deleteAlias.Flags().Bool("local", false, "Only search local filesystem")
	deleteAlias.Flags().Bool("hub", false, "Only search Hub")
	templateCmd.AddCommand(deleteAlias)

	cloneAlias := &cobra.Command{
		Use:   "clone <src-name> <dest-name>",
		Short: "Clone an existing template",
		Args:  cobra.ExactArgs(2),
		RunE:  runTemplateClone,
	}
	cloneAlias.Flags().Bool("local", false, "Only search local filesystem for source")
	cloneAlias.Flags().Bool("hub", false, "Only search Hub for source")
	templateCmd.AddCommand(cloneAlias)

	// Add sync, push, pull to singular alias (--global is inherited from root)
	syncAlias := &cobra.Command{
		Use:   "sync <template>",
		Short: "Create or update a template in the Hub (Hub only)",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplateSync,
	}
	syncAlias.Flags().String("name", "", "Name for the template on the Hub (defaults to local template name)")
	templateCmd.AddCommand(syncAlias)

	pushAlias := &cobra.Command{
		Use:   "push <template>",
		Short: "Upload local template to Hub (alias for sync)",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplateSync,
	}
	pushAlias.Flags().String("name", "", "Name for the template on the Hub (defaults to local template name)")
	templateCmd.AddCommand(pushAlias)

	pullAlias := &cobra.Command{
		Use:   "pull <name>",
		Short: "Download a template from Hub to local cache (Hub only)",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplatePull,
	}
	pullAlias.Flags().String("to", "", "Destination path for downloaded template")
	templateCmd.AddCommand(pullAlias)
}
