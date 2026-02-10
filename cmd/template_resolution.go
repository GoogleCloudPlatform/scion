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
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubclient"
)

// Template resolution flags for agent creation
var (
	uploadTemplate  bool   // --upload-template: auto-upload local template
	noUpload        bool   // --no-upload: fail if template requires upload
	templateScope   string // --template-scope: scope for uploaded template
)

// TemplateResolutionResult contains the result of template resolution.
type TemplateResolutionResult struct {
	// TemplateID is the Hub template ID (if found on Hub)
	TemplateID string
	// TemplateName is the template name (for display and fallback)
	TemplateName string
	// WasUploaded indicates if the template was uploaded during resolution
	WasUploaded bool
}

// ResolveTemplateForHub resolves a template name for Hub-based agent creation.
// This implements Section 9.4 of the hosted-templates.md design document.
//
// Resolution flow:
// 1. Check if template exists on Hub (by name, in applicable scopes)
// 2. If found on Hub:
//   - If local version exists and differs, optionally prompt for update
//   - Return Hub template reference
//
// 3. If NOT found on Hub:
//   - Check local filesystem
//   - If found locally, prompt (or auto-upload based on flags)
//   - Return Hub template reference after upload
//
// 4. If not found anywhere, return error with guidance
func ResolveTemplateForHub(ctx context.Context, hubCtx *HubContext, templateName string) (*TemplateResolutionResult, error) {
	if templateName == "" {
		return nil, fmt.Errorf("template name is required")
	}

	// Parse scope prefix if present (e.g., "global:claude", "grove:custom")
	scope, name := parseTemplateScope(templateName)

	// Get grove ID for grove-scoped lookups
	groveID, err := GetGroveID(hubCtx)
	if err != nil && scope == "grove" {
		return nil, fmt.Errorf("failed to determine grove ID for template resolution: %w", err)
	}

	// Step 1: Check if template exists on Hub
	hubTemplate, err := findTemplateOnHub(ctx, hubCtx, name, scope, groveID)
	if err != nil {
		return nil, fmt.Errorf("failed to search Hub for template: %w", err)
	}

	// Step 2: Check if template exists locally
	localTemplate, _ := config.FindTemplate(name)

	// Case 1: Template exists on Hub
	if hubTemplate != nil {
		// If also exists locally, check if content matches
		if localTemplate != nil {
			return handleHubAndLocalTemplate(ctx, hubCtx, hubTemplate, localTemplate, groveID)
		}
		// Only on Hub, use it
		return &TemplateResolutionResult{
			TemplateID:   hubTemplate.ID,
			TemplateName: hubTemplate.Name,
		}, nil
	}

	// Case 2: Template NOT on Hub, but exists locally
	if localTemplate != nil {
		return handleLocalOnlyTemplate(ctx, hubCtx, localTemplate, scope, groveID)
	}

	// Case 3: Template not found anywhere
	return nil, formatTemplateNotFoundError(name, hubCtx.GrovePath)
}

// parseTemplateScope extracts scope prefix from template name.
// Examples: "global:claude" -> ("global", "claude"), "custom" -> ("", "custom")
func parseTemplateScope(templateName string) (scope, name string) {
	if idx := strings.Index(templateName, ":"); idx != -1 {
		prefix := templateName[:idx]
		// Check if it's a known scope prefix
		switch prefix {
		case "global", "grove", "user":
			return prefix, templateName[idx+1:]
		}
	}
	return "", templateName
}

// findTemplateOnHub searches for a template on the Hub.
// It implements the resolution order from Section 3.2 of the design doc:
// 1. Grove scope (if applicable)
// 2. User scope
// 3. Global scope
func findTemplateOnHub(ctx context.Context, hubCtx *HubContext, name, scope, groveID string) (*hubclient.Template, error) {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// If explicit scope is provided, search only that scope
	if scope != "" {
		opts := &hubclient.ListTemplatesOptions{
			Name:   name,
			Scope:  scope,
			Status: "active",
		}
		if scope == "grove" && groveID != "" {
			opts.GroveID = groveID
		}

		resp, err := hubCtx.Client.Templates().List(listCtx, opts)
		if err != nil {
			return nil, err
		}

		// Return first match by name or slug
		for i := range resp.Templates {
			t := &resp.Templates[i]
			if t.Name == name || t.Slug == name {
				return t, nil
			}
		}
		return nil, nil
	}

	// No explicit scope - follow resolution order: grove -> user -> global
	// First, check grove scope if we have a grove ID
	if groveID != "" {
		opts := &hubclient.ListTemplatesOptions{
			Name:    name,
			Scope:   "grove",
			GroveID: groveID,
			Status:  "active",
		}

		resp, err := hubCtx.Client.Templates().List(listCtx, opts)
		if err != nil {
			return nil, err
		}

		for i := range resp.Templates {
			t := &resp.Templates[i]
			if t.Name == name || t.Slug == name {
				return t, nil
			}
		}
	}

	// Check global scope
	globalOpts := &hubclient.ListTemplatesOptions{
		Name:   name,
		Scope:  "global",
		Status: "active",
	}

	globalResp, err := hubCtx.Client.Templates().List(listCtx, globalOpts)
	if err != nil {
		return nil, err
	}

	for i := range globalResp.Templates {
		t := &globalResp.Templates[i]
		if t.Name == name || t.Slug == name {
			return t, nil
		}
	}

	return nil, nil
}

// handleHubAndLocalTemplate handles the case where template exists on both Hub and locally.
func handleHubAndLocalTemplate(ctx context.Context, hubCtx *HubContext, hubTemplate *hubclient.Template, localTemplate *config.Template, groveID string) (*TemplateResolutionResult, error) {
	// Compute local content hash
	files, err := hubclient.CollectFiles(localTemplate.Path, nil)
	if err != nil {
		// Can't compute hash, just use Hub version
		return &TemplateResolutionResult{
			TemplateID:   hubTemplate.ID,
			TemplateName: hubTemplate.Name,
		}, nil
	}

	localHash := computeLocalContentHash(files)

	// If hashes match, no action needed
	if localHash == hubTemplate.ContentHash {
		return &TemplateResolutionResult{
			TemplateID:   hubTemplate.ID,
			TemplateName: hubTemplate.Name,
		}, nil
	}

	// Hashes differ - prompt user unless flags override
	if noUpload {
		// Use Hub version, ignore local changes
		return &TemplateResolutionResult{
			TemplateID:   hubTemplate.ID,
			TemplateName: hubTemplate.Name,
		}, nil
	}

	if uploadTemplate || autoConfirm {
		// Auto-update Hub with local version
		return updateHubTemplate(ctx, hubCtx, hubTemplate, localTemplate, files, groveID)
	}

	// Interactive prompt
	return promptForTemplateHashMismatch(ctx, hubCtx, hubTemplate, localTemplate, files, localHash, groveID)
}

// handleLocalOnlyTemplate handles the case where template exists only locally.
func handleLocalOnlyTemplate(ctx context.Context, hubCtx *HubContext, localTemplate *config.Template, scope, groveID string) (*TemplateResolutionResult, error) {
	// Check flags
	if noUpload {
		return nil, fmt.Errorf("template '%s' exists locally but not on Hub, and --no-upload is set\n\n"+
			"To upload this template, run:\n"+
			"  scion template sync %s\n\n"+
			"Or use --upload-template to auto-upload during agent creation",
			localTemplate.Name, localTemplate.Name)
	}

	// Determine harness type from template config
	harnessType, err := detectHarnessType(localTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to detect harness type for template '%s': %w\n\n"+
			"Please ensure the template has a valid scion-agent.json with a 'harness' field",
			localTemplate.Name, err)
	}

	if uploadTemplate || autoConfirm {
		// Auto-upload
		return uploadLocalTemplate(ctx, hubCtx, localTemplate, scope, groveID, harnessType)
	}

	// Interactive prompt
	return promptForLocalTemplateUpload(ctx, hubCtx, localTemplate, scope, groveID, harnessType)
}

// promptForLocalTemplateUpload prompts the user to upload a local-only template.
func promptForLocalTemplateUpload(ctx context.Context, hubCtx *HubContext, localTemplate *config.Template, scope, groveID, harnessType string) (*TemplateResolutionResult, error) {
	effectiveScope := scope
	if effectiveScope == "" {
		effectiveScope = templateScope
	}
	if effectiveScope == "" {
		effectiveScope = "grove"
	}

	fmt.Printf("\nTemplate '%s' was not found on the Hub but exists locally at:\n", localTemplate.Name)
	fmt.Printf("  %s\n\n", localTemplate.Path)
	fmt.Println("The Runtime Broker cannot access local templates. Would you like to:")
	fmt.Printf("  [U] Upload template to Hub (%s scope) and continue\n", effectiveScope)
	fmt.Println("  [C] Cancel agent creation")
	fmt.Println()

	choice, err := promptChoice("Choice", "U", []string{"U", "C"})
	if err != nil {
		return nil, err
	}

	if strings.ToUpper(choice) == "C" {
		return nil, fmt.Errorf("agent creation cancelled by user")
	}

	return uploadLocalTemplate(ctx, hubCtx, localTemplate, effectiveScope, groveID, harnessType)
}

// promptForTemplateHashMismatch prompts when local and Hub templates differ.
func promptForTemplateHashMismatch(ctx context.Context, hubCtx *HubContext, hubTemplate *hubclient.Template, localTemplate *config.Template, files []hubclient.FileInfo, localHash, groveID string) (*TemplateResolutionResult, error) {
	fmt.Printf("\nTemplate '%s' exists on Hub but local version differs:\n", localTemplate.Name)
	fmt.Printf("  Hub hash:   %s\n", truncateHash(hubTemplate.ContentHash))
	fmt.Printf("  Local hash: %s\n\n", truncateHash(localHash))
	fmt.Println("Would you like to:")
	fmt.Println("  [U] Update Hub template with local version")
	fmt.Println("  [H] Use existing Hub template (ignore local changes)")
	fmt.Println("  [C] Cancel agent creation")
	fmt.Println()

	choice, err := promptChoice("Choice", "H", []string{"U", "H", "C"})
	if err != nil {
		return nil, err
	}

	switch strings.ToUpper(choice) {
	case "U":
		return updateHubTemplate(ctx, hubCtx, hubTemplate, localTemplate, files, groveID)
	case "H":
		return &TemplateResolutionResult{
			TemplateID:   hubTemplate.ID,
			TemplateName: hubTemplate.Name,
		}, nil
	default:
		return nil, fmt.Errorf("agent creation cancelled by user")
	}
}

// uploadLocalTemplate uploads a local template to the Hub.
func uploadLocalTemplate(ctx context.Context, hubCtx *HubContext, localTemplate *config.Template, scope, groveID, harnessType string) (*TemplateResolutionResult, error) {
	effectiveScope := scope
	if effectiveScope == "" {
		effectiveScope = templateScope
	}
	if effectiveScope == "" {
		effectiveScope = "grove"
	}

	fmt.Printf("Uploading template '%s' to Hub (%s scope)...\n", localTemplate.Name, effectiveScope)

	// Use the existing syncTemplateToHub function
	err := syncTemplateToHub(hubCtx, localTemplate.Name, localTemplate.Path, effectiveScope, harnessType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload template: %w", err)
	}

	// Now find the template on Hub to get its ID
	hubTemplate, err := findTemplateOnHub(ctx, hubCtx, localTemplate.Name, effectiveScope, groveID)
	if err != nil || hubTemplate == nil {
		return nil, fmt.Errorf("template was uploaded but could not be found on Hub")
	}

	return &TemplateResolutionResult{
		TemplateID:   hubTemplate.ID,
		TemplateName: hubTemplate.Name,
		WasUploaded:  true,
	}, nil
}

// updateHubTemplate updates an existing Hub template with local files.
func updateHubTemplate(ctx context.Context, hubCtx *HubContext, hubTemplate *hubclient.Template, localTemplate *config.Template, files []hubclient.FileInfo, groveID string) (*TemplateResolutionResult, error) {
	fmt.Printf("Updating Hub template '%s' with local version...\n", hubTemplate.Name)

	// Build file upload request
	fileReqs := make([]hubclient.FileUploadRequest, len(files))
	for i, f := range files {
		fileReqs[i] = hubclient.FileUploadRequest{
			Path: f.Path,
			Size: f.Size,
		}
	}

	uploadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Request upload URLs
	uploadResp, err := hubCtx.Client.Templates().RequestUploadURLs(uploadCtx, hubTemplate.ID, fileReqs)
	if err != nil {
		return nil, fmt.Errorf("failed to get upload URLs: %w", err)
	}

	// Upload files
	for _, urlInfo := range uploadResp.UploadURLs {
		var fileInfo *hubclient.FileInfo
		for i := range files {
			if files[i].Path == urlInfo.Path {
				fileInfo = &files[i]
				break
			}
		}
		if fileInfo == nil {
			continue
		}

		f, err := os.Open(fileInfo.FullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", fileInfo.Path, err)
		}

		err = hubCtx.Client.Templates().UploadFile(uploadCtx, urlInfo.URL, urlInfo.Method, urlInfo.Headers, f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to upload %s: %w", fileInfo.Path, err)
		}
	}

	// Build and finalize manifest
	manifest := &hubclient.TemplateManifest{
		Version: "1.0",
		Harness: hubTemplate.Harness,
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

	updated, err := hubCtx.Client.Templates().Finalize(uploadCtx, hubTemplate.ID, manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to finalize template update: %w", err)
	}

	fmt.Printf("Template '%s' updated on Hub (hash: %s)\n", updated.Name, truncateHash(updated.ContentHash))

	return &TemplateResolutionResult{
		TemplateID:   updated.ID,
		TemplateName: updated.Name,
		WasUploaded:  true,
	}, nil
}

// detectHarnessType attempts to determine the harness type from template config.
func detectHarnessType(tpl *config.Template) (string, error) {
	cfg, err := tpl.LoadConfig()
	if err != nil {
		return "", err
	}

	if cfg.Harness != "" {
		return cfg.Harness, nil
	}

	// Try to infer from template name
	name := strings.ToLower(tpl.Name)
	switch {
	case strings.Contains(name, "claude"):
		return "claude", nil
	case strings.Contains(name, "gemini"):
		return "gemini", nil
	case strings.Contains(name, "codex"):
		return "codex", nil
	case strings.Contains(name, "opencode"):
		return "opencode", nil
	}

	return "", fmt.Errorf("harness type not specified in template config and could not be inferred from name")
}

// computeLocalContentHash computes the content hash for local files.
func computeLocalContentHash(files []hubclient.FileInfo) string {
	templateFiles := make([]hubclient.TemplateFile, len(files))
	for i, f := range files {
		templateFiles[i] = hubclient.TemplateFile{
			Path: f.Path,
			Hash: f.Hash,
		}
	}
	return hubclient.ComputeContentHash(templateFiles)
}

// truncateHash returns a shortened version of a hash for display.
func truncateHash(hash string) string {
	if len(hash) > 20 {
		return hash[:20] + "..."
	}
	return hash
}

// promptChoice prompts the user for a choice from a list of options.
func promptChoice(prompt, defaultChoice string, validChoices []string) (string, error) {
	reader := bufio.NewReader(os.Stdin)

	for {
		if defaultChoice != "" {
			fmt.Printf("%s [%s]: ", prompt, strings.ToLower(defaultChoice))
		} else {
			fmt.Printf("%s: ", prompt)
		}

		input, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("failed to read input: %w", err)
		}

		choice := strings.TrimSpace(input)
		if choice == "" && defaultChoice != "" {
			return defaultChoice, nil
		}

		choice = strings.ToUpper(choice)
		for _, valid := range validChoices {
			if choice == strings.ToUpper(valid) {
				return choice, nil
			}
		}

		fmt.Printf("Invalid choice. Please enter one of: %s\n", strings.Join(validChoices, ", "))
	}
}

// formatTemplateNotFoundError creates a helpful error message when template is not found.
func formatTemplateNotFoundError(name, grovePath string) error {
	var locations []string

	// Check grove-specific locations
	if grovePath != "" {
		locations = append(locations, "  - Hub (grove scope) - not found")
	}
	locations = append(locations, "  - Hub (global) - not found")

	// Check local locations
	if projectDir, err := config.GetProjectTemplatesDir(); err == nil {
		locations = append(locations, fmt.Sprintf("  - Local (%s/%s) - not found", projectDir, name))
	}
	if globalDir, err := config.GetGlobalTemplatesDir(); err == nil {
		locations = append(locations, fmt.Sprintf("  - Local (%s/%s) - not found", globalDir, name))
	}

	return fmt.Errorf("template '%s' not found\n\n"+
		"Searched locations:\n%s\n\n"+
		"To sync a local template to the Hub:\n"+
		"  scion template sync %s",
		name, strings.Join(locations, "\n"), name)
}
