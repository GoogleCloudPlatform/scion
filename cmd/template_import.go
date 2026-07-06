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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/templateimport"
	"github.com/spf13/cobra"
)

var templatesImportCmd = &cobra.Command{
	Use:   "import <source>",
	Short: "Import agent definitions as scion templates",
	Long: `Import agent or sub-agent definitions from Claude Code (.claude/agents/*.md),
Gemini CLI (.gemini/agents/*.md), or existing scion templates and add them to your
current grove or global templates.

Source can be:
  - A single .md agent definition file
  - A directory containing agent definition files
  - A project root (auto-discovers .claude/agents/, .gemini/agents/, .scion/templates/)
  - A scion template directory (contains scion-agent.yaml)
  - A directory of scion templates (subdirectories with scion-agent.yaml)
  - A GitHub URL pointing to a repository or subdirectory

For non-scion formats, the harness type (claude/gemini) is auto-detected from file
path and content. Use --harness to override detection.

Scion-format templates are copied directly without conversion.

Examples:
  # Import a single Claude sub-agent definition
  scion templates import .claude/agents/code-reviewer.md

  # Import all agents from a directory
  scion templates import --all .gemini/agents/

  # Auto-detect and import all agents from project root
  scion templates import --all .

  # Import scion templates from another project
  scion templates import --all https://github.com/org/repo/tree/main/.scion/templates

  # Import a single scion template directory
  scion templates import path/to/.scion/templates/my-template

  # Import from a GitHub URL with explicit branch
  scion templates import --all https://github.com/org/repo/tree/main/.claude/agents

  # Import with explicit harness and custom name
  scion templates import --harness gemini --name my-auditor agents/security.md

  # Preview import without writing
  scion templates import --dry-run .claude/agents/code-reviewer.md`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplateImport,
}

func runTemplateImport(cmd *cobra.Command, args []string) error {
	source := args[0]
	harnessFlag, _ := cmd.Flags().GetString("harness")
	nameOverride, _ := cmd.Flags().GetString("name")
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	all, _ := cmd.Flags().GetBool("all")

	// Check if source is a remote URI (GitHub URL, archive, rclone)
	var remoteCachePath string
	if config.IsRemoteURI(source) {
		if err := config.ValidateRemoteURI(source); err != nil {
			return fmt.Errorf("invalid remote source: %w", err)
		}

		fmt.Printf("Fetching remote source: %s\n", source)
		cachePath, err := config.FetchRemoteTemplate(context.Background(), source)
		if err != nil {
			return fmt.Errorf("failed to fetch remote source: %w", err)
		}
		remoteCachePath = cachePath
		defer func() { _ = os.RemoveAll(remoteCachePath) }()
		source = cachePath
	}

	// Resolve source path
	absSource, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("failed to resolve source path: %w", err)
	}

	info, err := os.Stat(absSource)
	if err != nil {
		return fmt.Errorf("source not found: %s", source)
	}

	// Determine templates directory
	var templatesDir string
	if globalMode {
		templatesDir, err = config.GetGlobalTemplatesDir()
	} else {
		templatesDir, err = config.GetProjectTemplatesDir()
	}
	if err != nil {
		return fmt.Errorf("failed to resolve templates directory: %w", err)
	}

	// Collect agents to import
	var agents []*templateimport.ImportedAgent

	if info.IsDir() {
		// Check if source is itself a single scion template
		if templateimport.IsScionTemplate(absSource) {
			agent, err := templateimport.ParseScionTemplate(absSource)
			if err != nil {
				return fmt.Errorf("failed to parse scion template: %w", err)
			}
			agents = append(agents, agent)
		} else {
			agents, err = discoverAgents(absSource, harnessFlag, all)
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				return fmt.Errorf("no importable agent definitions found in %s", source)
			}
			if !all && len(agents) > 1 {
				return fmt.Errorf("found %d agent definitions in %s; use --all to import all of them, or specify a single file", len(agents), source)
			}
		}
	} else {
		// Single file
		agent, err := parseSingleFile(absSource, harnessFlag)
		if err != nil {
			return err
		}
		agents = append(agents, agent)
	}

	// Apply name override (only valid for single agent import)
	if nameOverride != "" {
		if len(agents) > 1 {
			return fmt.Errorf("--name cannot be used when importing multiple agents")
		}
		agents[0].Name = nameOverride
	}

	// Process each agent
	var results []importResult
	for _, agent := range agents {
		result := importResult{Name: agent.Name, Harness: agent.Harness}

		if dryRun {
			result.Status = "would_import"
			result.TemplatePath = filepath.Join(templatesDir, agent.Name)
			if agent.Model != "" {
				result.Model = agent.Model
			}
		} else {
			path, err := templateimport.WriteTemplate(agent, templatesDir, force)
			if err != nil {
				result.Status = "error"
				result.Error = err.Error()
			} else {
				result.Status = "imported"
				result.TemplatePath = path
				if agent.Model != "" {
					result.Model = agent.Model
				}
			}
		}
		results = append(results, result)
	}

	// Output results
	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  resultStatus(results),
			Command: "templates import",
			Message: resultMessage(results, dryRun),
			Details: map[string]interface{}{
				"templates": results,
				"dryRun":    dryRun,
			},
		})
	}

	for _, r := range results {
		switch r.Status {
		case "imported":
			fmt.Printf("Imported template '%s' (harness: %s) → %s\n", r.Name, r.Harness, r.TemplatePath)
		case "would_import":
			fmt.Printf("[dry-run] Would import template '%s' (harness: %s) → %s\n", r.Name, r.Harness, r.TemplatePath)
		case "error":
			fmt.Printf("Error importing '%s': %s\n", r.Name, r.Error)
		}
	}

	return nil
}

type importResult struct {
	Name         string `json:"name"`
	Harness      string `json:"harness"`
	Status       string `json:"status"`
	TemplatePath string `json:"templatePath,omitempty"`
	Model        string `json:"model,omitempty"`
	Error        string `json:"error,omitempty"`
}

// discoverAgents finds all importable agent definitions in a directory.
func discoverAgents(dir, harnessFlag string, all bool) ([]*templateimport.ImportedAgent, error) {
	var agents []*templateimport.ImportedAgent

	// Check if this is a project root with .claude/agents/ or .gemini/agents/
	claudeAgentsDir := filepath.Join(dir, ".claude", "agents")
	geminiAgentsDir := filepath.Join(dir, ".gemini", "agents")

	hasClaude := dirExists(claudeAgentsDir)
	hasGemini := dirExists(geminiAgentsDir)

	if hasClaude && (harnessFlag == "" || harnessFlag == "claude") {
		importer := &templateimport.ClaudeImporter{}
		found, err := importer.ParseDir(claudeAgentsDir)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Claude agents directory: %w", err)
		}
		agents = append(agents, found...)
	}

	if hasGemini && (harnessFlag == "" || harnessFlag == "gemini") {
		importer := &templateimport.GeminiImporter{}
		found, err := importer.ParseDir(geminiAgentsDir)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Gemini agents directory: %w", err)
		}
		agents = append(agents, found...)
	}

	// Check for scion-format templates (directories containing scion-agent.yaml).
	// First check the directory itself (e.g., .scion/templates/), then check
	// for a .scion/templates/ subdirectory if this is a project root.
	if templateimport.IsScionTemplatesDir(dir) {
		found, err := templateimport.DiscoverScionTemplates(dir)
		if err != nil {
			return nil, fmt.Errorf("failed to discover scion templates: %w", err)
		}
		agents = append(agents, found...)
	} else {
		scionTemplatesDir := filepath.Join(dir, ".scion", "templates")
		if dirExists(scionTemplatesDir) && templateimport.IsScionTemplatesDir(scionTemplatesDir) {
			found, err := templateimport.DiscoverScionTemplates(scionTemplatesDir)
			if err != nil {
				return nil, fmt.Errorf("failed to discover scion templates: %w", err)
			}
			agents = append(agents, found...)
		}
	}

	// If no known format was found, try the directory as loose .md files
	if !hasClaude && !hasGemini && len(agents) == 0 {
		found, err := parseDirectoryAsAgents(dir, harnessFlag)
		if err != nil {
			return nil, err
		}
		agents = append(agents, found...)
	}

	return agents, nil
}

// parseDirectoryAsAgents parses .md files directly in a directory.
func parseDirectoryAsAgents(dir, harnessFlag string) ([]*templateimport.ImportedAgent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var agents []*templateimport.ImportedAgent
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		agent, err := parseSingleFile(filepath.Join(dir, e.Name()), harnessFlag)
		if err != nil {
			continue
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

// parseSingleFile parses a single .md file, detecting or using the specified harness.
func parseSingleFile(path, harnessFlag string) (*templateimport.ImportedAgent, error) {
	harnessName := harnessFlag

	if harnessName == "" {
		detected, err := templateimport.DetectHarness(path)
		if err != nil {
			return nil, fmt.Errorf("failed to detect harness for %s: %w", path, err)
		}
		if detected == "" {
			return nil, fmt.Errorf("could not detect harness type for %s; use --harness to specify", path)
		}
		harnessName = detected
	}

	var importer templateimport.Importer
	switch harnessName {
	case "claude":
		importer = &templateimport.ClaudeImporter{}
	case "gemini":
		importer = &templateimport.GeminiImporter{}
	default:
		return nil, fmt.Errorf("unsupported harness type: %s", harnessName)
	}

	return importer.Parse(path)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func resultStatus(results []importResult) string {
	for _, r := range results {
		if r.Status == "error" {
			return "partial_error"
		}
	}
	return "success"
}

func resultMessage(results []importResult, dryRun bool) string {
	if dryRun {
		return fmt.Sprintf("Would import %d template(s)", len(results))
	}
	imported := 0
	for _, r := range results {
		if r.Status == "imported" {
			imported++
		}
	}
	return fmt.Sprintf("Imported %d template(s)", imported)
}
