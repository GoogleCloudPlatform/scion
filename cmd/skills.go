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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage skill bank skills",
	Long:  `List, create, publish, and resolve skills from the Hub skill bank.`,
}

var skillsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available skills",
	RunE:  runSkillsList,
}

func runSkillsList(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scope, _ := cmd.Flags().GetString("scope")
	search, _ := cmd.Flags().GetString("search")
	tags, _ := cmd.Flags().GetString("tags")

	opts := &hubclient.ListSkillsOptions{
		Scope:  scope,
		Search: search,
		Status: "active",
	}
	if tags != "" {
		opts.Tags = strings.Split(tags, ",")
	}

	resp, err := hubCtx.Client.Skills().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list skills: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(resp.Skills)
	}

	if len(resp.Skills) == 0 {
		fmt.Println("No skills found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSCOPE\tSTATUS\tTAGS\tDESCRIPTION")
	for _, s := range resp.Skills {
		desc := s.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}
		tags := strings.Join(s.Tags, ",")
		if len(tags) > 20 {
			tags = tags[:17] + "..."
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Scope, s.Status, tags, desc)
	}
	return w.Flush()
}

var skillsShowCmd = &cobra.Command{
	Use:   "show <name-or-id>",
	Short: "Show skill details",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsShow,
}

func runSkillsShow(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nameOrID := args[0]

	skill, err := hubCtx.Client.Skills().Get(ctx, nameOrID)
	if err != nil {
		// Try search by name
		listResp, listErr := hubCtx.Client.Skills().List(ctx, &hubclient.ListSkillsOptions{
			Name: nameOrID,
		})
		if listErr != nil || len(listResp.Skills) == 0 {
			return fmt.Errorf("skill %q not found: %w", nameOrID, err)
		}
		skill = &listResp.Skills[0]
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(skill)
	}

	fmt.Printf("Skill: %s\n", skill.Name)
	fmt.Printf("ID: %s\n", skill.ID)
	fmt.Printf("Scope: %s\n", skill.Scope)
	if skill.ScopeID != "" {
		fmt.Printf("Scope ID: %s\n", skill.ScopeID)
	}
	if skill.Description != "" {
		fmt.Printf("Description: %s\n", skill.Description)
	}
	if len(skill.Tags) > 0 {
		fmt.Printf("Tags: %s\n", strings.Join(skill.Tags, ", "))
	}
	fmt.Printf("Status: %s\n", skill.Status)
	fmt.Printf("Visibility: %s\n", skill.Visibility)
	fmt.Printf("Created: %s\n", skill.Created.Format(time.RFC3339))

	// Show versions
	versions, err := hubCtx.Client.Skills().ListVersions(ctx, skill.ID)
	if err == nil && len(versions.Items) > 0 {
		fmt.Println("\nVersions:")
		for _, v := range versions.Items {
			line := fmt.Sprintf("  %-10s (%s)   downloads: %d", v.Version, v.Status, v.DownloadCount)
			if v.Status == "deprecated" && v.DeprecationMessage != "" {
				line += "  ⚠ " + v.DeprecationMessage
			}
			fmt.Println(line)
		}
	}

	return nil
}

var skillsCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new skill (scaffolds local directory)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsCreate,
}

func runSkillsCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	dir := filepath.Join(".", name)

	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("directory %q already exists", dir)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	skillMD := fmt.Sprintf(`---
name: %s
description: <describe your skill>
---

# %s

[Your skill instructions here]
`, name, skillDisplayName(name))

	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("failed to write SKILL.md: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"name": name,
			"path": dir,
		})
	}

	fmt.Printf("Created skill directory: %s/\n", dir)
	fmt.Printf("  %s/SKILL.md  (edit this file)\n", name)
	return nil
}

var skillsPublishCmd = &cobra.Command{
	Use:   "publish <path>",
	Short: "Publish a skill directory to the Hub",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsPublish,
}

func runSkillsPublish(cmd *cobra.Command, args []string) error {
	skillDir := args[0]
	version, _ := cmd.Flags().GetString("version")
	scope, _ := cmd.Flags().GetString("scope")
	skillID, _ := cmd.Flags().GetString("skill-id")

	if version == "" {
		return fmt.Errorf("--version is required")
	}

	// Verify SKILL.md exists
	skillMDPath := filepath.Join(skillDir, "SKILL.md")
	if _, err := os.Stat(skillMDPath); os.IsNotExist(err) {
		return fmt.Errorf("SKILL.md not found in %s", skillDir)
	}

	// Collect files
	var files []publishFile
	err := filepath.Walk(skillDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == ".DS_Store" || base == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == ".DS_Store" || filepath.Base(path) == ".gitignore" {
			return nil
		}
		relPath, _ := filepath.Rel(skillDir, path)
		relPath = filepath.ToSlash(relPath)
		files = append(files, publishFile{
			path:    relPath,
			absPath: path,
			size:    info.Size(),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to collect files: %w", err)
	}

	// Validate limits
	if len(files) > 50 {
		return fmt.Errorf("too many files (%d, max 50)", len(files))
	}
	var totalSize int64
	for _, f := range files {
		if f.size > 10*1024*1024 {
			return fmt.Errorf("file %q exceeds 10MB limit (%d bytes)", f.path, f.size)
		}
		totalSize += f.size
	}
	if totalSize > 50*1024*1024 {
		return fmt.Errorf("total size exceeds 50MB limit (%d bytes)", totalSize)
	}

	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	skillSvc := hubCtx.Client.Skills()

	// Create skill if no ID provided — try to find by name first
	if skillID == "" {
		// Extract name from SKILL.md frontmatter or directory name
		name := filepath.Base(filepath.Clean(skillDir))

		listResp, err := skillSvc.List(ctx, &hubclient.ListSkillsOptions{Name: name})
		if err == nil && len(listResp.Skills) > 0 {
			skillID = listResp.Skills[0].ID
		} else {
			if scope == "" {
				scope = "global"
			}
			createResp, err := skillSvc.Create(ctx, &hubclient.CreateSkillRequest{
				Name:  name,
				Scope: scope,
			})
			if err != nil {
				return fmt.Errorf("failed to create skill: %w", err)
			}
			skillID = createResp.Skill.ID
			fmt.Printf("Created skill %q (ID: %s)\n", name, skillID)
		}
	}

	// Publish version
	fileReqs := make([]hubclient.FileUploadRequest, len(files))
	for i, f := range files {
		fileReqs[i] = hubclient.FileUploadRequest{Path: f.path, Size: f.size}
	}

	pubResp, err := skillSvc.PublishVersion(ctx, skillID, &hubclient.PublishVersionRequest{
		Version: version,
		Files:   fileReqs,
	})
	if err != nil {
		return fmt.Errorf("failed to create version: %w", err)
	}

	// Upload files
	for _, uploadInfo := range pubResp.UploadURLs {
		// Find the matching local file
		var localPath string
		for _, f := range files {
			if f.path == uploadInfo.Path {
				localPath = f.absPath
				break
			}
		}
		if localPath == "" {
			continue
		}

		file, err := os.Open(localPath)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", localPath, err)
		}
		err = skillSvc.UploadFile(ctx, uploadInfo.URL, uploadInfo.Method, uploadInfo.Headers, file)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("failed to upload %s: %w", uploadInfo.Path, err)
		}
		fmt.Printf("  Uploaded %s\n", uploadInfo.Path)
	}

	// Build manifest with file hashes
	manifest := &hubclient.SkillManifest{
		Files: make([]hubclient.TemplateFile, len(files)),
	}
	for i, f := range files {
		hash, err := hashFile(f.absPath)
		if err != nil {
			return fmt.Errorf("failed to hash %s: %w", f.path, err)
		}
		manifest.Files[i] = hubclient.TemplateFile{
			Path: f.path,
			Size: f.size,
			Hash: hash,
		}
	}

	// Finalize
	sv, err := skillSvc.FinalizeVersion(ctx, skillID, &hubclient.FinalizeSkillVersionRequest{
		Version:  version,
		Manifest: manifest,
	})
	if err != nil {
		return fmt.Errorf("failed to finalize version: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(sv)
	}

	fmt.Printf("Published %s v%s (hash: %s)\n", filepath.Base(filepath.Clean(skillDir)), sv.Version, sv.ContentHash)
	return nil
}

type publishFile struct {
	path    string
	absPath string
	size    int64
}

func skillDisplayName(slug string) string {
	words := strings.Split(strings.ReplaceAll(slug, "-", " "), " ")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

var skillsDeleteCmd = &cobra.Command{
	Use:     "delete <name-or-id>",
	Aliases: []string{"rm"},
	Short:   "Delete a skill (soft delete)",
	Args:    cobra.ExactArgs(1),
	RunE:    runSkillsDelete,
}

func runSkillsDelete(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nameOrID := args[0]

	// Try direct delete by ID first
	err = hubCtx.Client.Skills().Delete(ctx, nameOrID)
	if err != nil {
		// Try finding by name
		listResp, listErr := hubCtx.Client.Skills().List(ctx, &hubclient.ListSkillsOptions{
			Name: nameOrID,
		})
		if listErr != nil || len(listResp.Skills) == 0 {
			return fmt.Errorf("skill %q not found: %w", nameOrID, err)
		}
		err = hubCtx.Client.Skills().Delete(ctx, listResp.Skills[0].ID)
		if err != nil {
			return fmt.Errorf("failed to delete skill: %w", err)
		}
		nameOrID = listResp.Skills[0].Name
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"deleted": nameOrID})
	}

	fmt.Printf("Deleted skill %q\n", nameOrID)
	return nil
}

var skillsDeprecateCmd = &cobra.Command{
	Use:   "deprecate <name-or-id>",
	Short: "Deprecate a skill version",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsDeprecate,
}

func runSkillsDeprecate(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nameOrID := args[0]
	version, _ := cmd.Flags().GetString("version")
	message, _ := cmd.Flags().GetString("message")
	replacement, _ := cmd.Flags().GetString("replacement")

	if version == "" {
		return fmt.Errorf("--version is required")
	}
	if message == "" {
		return fmt.Errorf("--message is required")
	}

	skillSvc := hubCtx.Client.Skills()

	// Find skill
	skill, err := skillSvc.Get(ctx, nameOrID)
	if err != nil {
		listResp, listErr := skillSvc.List(ctx, &hubclient.ListSkillsOptions{Name: nameOrID})
		if listErr != nil || len(listResp.Skills) == 0 {
			return fmt.Errorf("skill %q not found: %w", nameOrID, err)
		}
		skill = &listResp.Skills[0]
	}

	// Find the specific version
	versions, err := skillSvc.ListVersions(ctx, skill.ID)
	if err != nil {
		return fmt.Errorf("failed to list versions: %w", err)
	}

	var versionID string
	for _, v := range versions.Items {
		if v.Version == version {
			versionID = v.ID
			break
		}
	}
	if versionID == "" {
		return fmt.Errorf("version %q not found for skill %q", version, skill.Name)
	}

	sv, err := skillSvc.DeprecateVersion(ctx, skill.ID, versionID, &hubclient.DeprecateVersionRequest{
		Message:        message,
		ReplacementURI: replacement,
	})
	if err != nil {
		return fmt.Errorf("failed to deprecate version: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(sv)
	}

	fmt.Printf("Version %s of %q marked as deprecated.\n", sv.Version, skill.Name)
	if replacement != "" {
		fmt.Printf("Replacement: %s\n", replacement)
	}
	return nil
}

var skillsVersionsCmd = &cobra.Command{
	Use:   "versions <name-or-id>",
	Short: "List versions of a skill",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsVersions,
}

func runSkillsVersions(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nameOrID := args[0]

	// Try direct ID first
	skill, err := hubCtx.Client.Skills().Get(ctx, nameOrID)
	if err != nil {
		listResp, listErr := hubCtx.Client.Skills().List(ctx, &hubclient.ListSkillsOptions{
			Name: nameOrID,
		})
		if listErr != nil || len(listResp.Skills) == 0 {
			return fmt.Errorf("skill %q not found: %w", nameOrID, err)
		}
		skill = &listResp.Skills[0]
	}

	versions, err := hubCtx.Client.Skills().ListVersions(ctx, skill.ID)
	if err != nil {
		return fmt.Errorf("failed to list versions: %w", err)
	}

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(versions.Items)
	}

	if len(versions.Items) == 0 {
		fmt.Println("No versions found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tSTATUS\tCREATED\tCONTENT HASH")
	for _, v := range versions.Items {
		hash := v.ContentHash
		if len(hash) > 20 {
			hash = hash[:20] + "..."
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Version, v.Status, v.Created.Format("2006-01-02"), hash)
	}
	return w.Flush()
}

var skillsResolveCmd = &cobra.Command{
	Use:   "resolve <uri>",
	Short: "Resolve a skill URI to a specific version",
	Args:  cobra.ExactArgs(1),
	RunE:  runSkillsResolve,
}

func runSkillsResolve(cmd *cobra.Command, args []string) error {
	hubCtx, err := CheckHubAvailability(projectPath)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uri := args[0]

	resp, err := hubCtx.Client.Skills().Resolve(ctx, &hubclient.ResolveSkillsRequest{
		Skills: []hubclient.ResolveSkillRef{{URI: uri}},
	})
	if err != nil {
		return fmt.Errorf("failed to resolve skill: %w", err)
	}

	if len(resp.Errors) > 0 {
		return fmt.Errorf("resolution error: %s", resp.Errors[0].Message)
	}

	if len(resp.Resolved) == 0 {
		return fmt.Errorf("no resolution result for %q", uri)
	}

	result := resp.Resolved[0]

	if isJSONOutput() {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	fmt.Printf("URI: %s\n", result.URI)
	fmt.Printf("Name: %s\n", result.Name)
	fmt.Printf("Resolved Version: %s\n", result.ResolvedVersion)
	fmt.Printf("Content Hash: %s\n", result.ContentHash)
	if len(result.Files) > 0 {
		fmt.Println("Files:")
		for _, f := range result.Files {
			fmt.Printf("  %s (%d bytes)\n", f.Path, f.Size)
		}
	}

	return nil
}

func init() {
	rootCmd.AddCommand(skillsCmd)
	skillsCmd.AddCommand(skillsListCmd)
	skillsCmd.AddCommand(skillsShowCmd)
	skillsCmd.AddCommand(skillsCreateCmd)
	skillsCmd.AddCommand(skillsPublishCmd)
	skillsCmd.AddCommand(skillsDeleteCmd)
	skillsCmd.AddCommand(skillsDeprecateCmd)
	skillsCmd.AddCommand(skillsVersionsCmd)
	skillsCmd.AddCommand(skillsResolveCmd)

	// Flags for list command
	skillsListCmd.Flags().String("scope", "", "Filter by scope (core, global, project, user)")
	skillsListCmd.Flags().String("search", "", "Search skills by name, description, or tags")
	skillsListCmd.Flags().String("tags", "", "Filter by tags (comma-separated, AND semantics)")

	// Flags for deprecate command
	skillsDeprecateCmd.Flags().String("version", "", "Version to deprecate (required)")
	skillsDeprecateCmd.Flags().String("message", "", "Deprecation message (required)")
	skillsDeprecateCmd.Flags().String("replacement", "", "Replacement skill URI")

	// Flags for publish command
	skillsPublishCmd.Flags().String("version", "", "Semver version to publish (required)")
	skillsPublishCmd.Flags().String("scope", "", "Scope for new skills (core, global, project, user)")
	skillsPublishCmd.Flags().String("skill-id", "", "Existing skill ID to publish a version for")

	// Also add a 'skill' alias (singular) for convenience
	skillCmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skill bank skills (alias for 'skills')",
		Long:  `List, create, publish, and resolve skills from the Hub skill bank.`,
	}
	rootCmd.AddCommand(skillCmd)
	skillCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List available skills",
		RunE:  runSkillsList,
	})
}
