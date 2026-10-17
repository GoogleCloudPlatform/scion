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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunTemplateImport_ReportsWriteFailures(t *testing.T) {
	for _, format := range []string{"plain", "json"} {
		for _, tc := range []struct {
			name     string
			existing int
			force    bool
			dryRun   bool
			wantErr  bool
			statuses []string
		}{
			{name: "success", statuses: []string{"imported", "imported"}},
			{name: "partial_failure", existing: 1, wantErr: true, statuses: []string{"error", "imported"}},
			{name: "all_failed", existing: 2, wantErr: true, statuses: []string{"error", "error"}},
			{name: "force_overwrite", existing: 2, force: true, statuses: []string{"imported", "imported"}},
			{name: "dry_run", existing: 1, dryRun: true, statuses: []string{"would_import", "would_import"}},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				oldGlobal, oldFormat := globalMode, outputFormat
				t.Cleanup(func() { globalMode, outputFormat = oldGlobal, oldFormat })
				globalMode, outputFormat = false, format
				projectDir := t.TempDir()
				t.Chdir(projectDir)
				templatesDir := filepath.Join(projectDir, ".scion", "templates")
				require.NoError(t, os.MkdirAll(templatesDir, 0755))
				sourceDir := filepath.Join(projectDir, "source")
				names := []string{"alpha", "beta"}
				const sourceConfig = "schema_version: \"1\"\nmodel: imported-model\n"
				const originalConfig = "schema_version: \"1\"\nmodel: original-model\n"
				for i, name := range names {
					source := filepath.Join(sourceDir, name)
					require.NoError(t, os.MkdirAll(source, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(source, "scion-agent.yaml"), []byte(sourceConfig), 0644))
					if i < tc.existing {
						dest := filepath.Join(templatesDir, name)
						require.NoError(t, os.MkdirAll(dest, 0755))
						require.NoError(t, os.WriteFile(filepath.Join(dest, "scion-agent.yaml"), []byte(originalConfig), 0644))
					}
				}

				command := &cobra.Command{}
				command.Flags().Bool("all", true, "")
				command.Flags().Bool("force", tc.force, "")
				command.Flags().Bool("dry-run", tc.dryRun, "")
				command.Flags().String("harness", "", "")
				command.Flags().String("name", "", "")
				var runErr error
				output := captureStdout(t, func() {
					runErr = runTemplateImport(command, []string{sourceDir})
				})
				if tc.wantErr {
					assert.Error(t, runErr, "write failures must fail the command")
				} else {
					assert.NoError(t, runErr)
				}
				assert.Equal(t, tc.wantErr && format == "json", isReportedInJSON(runErr))

				if format == "json" {
					var result struct {
						Status  string
						Command string
						Details struct {
							Templates []importResult
							DryRun    bool
						}
					}
					require.NoError(t, json.Unmarshal([]byte(output), &result), "stdout must remain one JSON document")
					wantStatus := "success"
					if tc.wantErr {
						wantStatus = "partial_error"
					}
					assert.Equal(t, wantStatus, result.Status)
					assert.Equal(t, "templates import", result.Command)
					assert.Equal(t, tc.dryRun, result.Details.DryRun)
					require.Len(t, result.Details.Templates, len(names))
					for i, result := range result.Details.Templates {
						assert.Equal(t, names[i], result.Name)
						assert.Equal(t, tc.statuses[i], result.Status)
						if result.Status == "error" {
							assert.Contains(t, result.Error, "already exists")
						}
					}
				} else {
					for i, name := range names {
						switch tc.statuses[i] {
						case "error":
							assert.Contains(t, output, "Error importing '"+name+"'")
						case "imported":
							assert.Contains(t, output, "Imported template '"+name+"'")
						case "would_import":
							assert.Contains(t, output, "[dry-run] Would import template '"+name+"'")
						}
					}
				}
				for i, name := range names {
					path := filepath.Join(templatesDir, name, "scion-agent.yaml")
					if tc.dryRun && i >= tc.existing {
						assert.NoFileExists(t, path)
						continue
					}
					want := sourceConfig
					if tc.statuses[i] != "imported" {
						want = originalConfig
					}
					data, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, want, string(data))
				}
			})
		}
	}
}

func TestParseSingleFile_Claude(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "reviewer.md")
	content := `---
name: code-reviewer
description: Reviews code
tools: Read, Glob, Grep
model: sonnet
---

You are a code reviewer.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	agent, err := parseSingleFile(file, "claude")
	require.NoError(t, err)
	assert.Equal(t, "code-reviewer", agent.Name)
	assert.Equal(t, "claude", agent.Harness)
	assert.Equal(t, "sonnet", agent.Model)
}

func TestParseSingleFile_Gemini(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "auditor.md")
	content := `---
name: security-auditor
description: Audits security
kind: local
tools:
  - read_file
model: gemini-2.5-pro
---

You are a security auditor.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	agent, err := parseSingleFile(file, "gemini")
	require.NoError(t, err)
	assert.Equal(t, "security-auditor", agent.Name)
	assert.Equal(t, "gemini", agent.Harness)
}

func TestParseSingleFile_AutoDetect(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(claudeDir, 0755))

	file := filepath.Join(claudeDir, "agent.md")
	content := `---
name: test-agent
description: A test
tools: Read
---

Prompt.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	agent, err := parseSingleFile(file, "")
	require.NoError(t, err)
	assert.Equal(t, "claude", agent.Harness)
}

func TestParseSingleFile_UnsupportedHarness(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent.md")
	content := `---
name: agent
description: test
---

Prompt.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	_, err := parseSingleFile(file, "unknown")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported harness")
}

func TestDiscoverAgents_ProjectRoot(t *testing.T) {
	dir := t.TempDir()

	// Create .claude/agents/ with an agent
	claudeDir := filepath.Join(dir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(claudeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "reviewer.md"), []byte(`---
name: reviewer
description: Reviews code
tools: Read
---

Review code.
`), 0644))

	// Create .gemini/agents/ with an agent
	geminiDir := filepath.Join(dir, ".gemini", "agents")
	require.NoError(t, os.MkdirAll(geminiDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(geminiDir, "auditor.md"), []byte(`---
name: auditor
description: Audits code
kind: local
---

Audit code.
`), 0644))

	agents, err := discoverAgents(dir, "", true)
	require.NoError(t, err)
	assert.Len(t, agents, 2)
}

func TestDiscoverAgents_FilterByHarness(t *testing.T) {
	dir := t.TempDir()

	// Create both directories
	claudeDir := filepath.Join(dir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(claudeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "agent.md"), []byte(`---
name: claude-agent
description: test
---

Prompt.
`), 0644))

	geminiDir := filepath.Join(dir, ".gemini", "agents")
	require.NoError(t, os.MkdirAll(geminiDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(geminiDir, "agent.md"), []byte(`---
name: gemini-agent
description: test
---

Prompt.
`), 0644))

	// Filter to claude only
	agents, err := discoverAgents(dir, "claude", true)
	require.NoError(t, err)
	assert.Len(t, agents, 1)
	assert.Equal(t, "claude", agents[0].Harness)
}

func TestDiscoverAgents_ScionTemplates(t *testing.T) {
	dir := t.TempDir()

	// Create a scion templates directory with two templates
	templatesDir := filepath.Join(dir, ".scion", "templates")
	for _, name := range []string{"frontend", "backend"} {
		templateDir := filepath.Join(templatesDir, name)
		require.NoError(t, os.MkdirAll(filepath.Join(templateDir, "home"), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "`+name+` agent"
default_harness_config: claude
`), 0644))
	}

	// Discover from project root (should find .scion/templates/)
	agents, err := discoverAgents(dir, "", true)
	require.NoError(t, err)
	assert.Len(t, agents, 2)
	for _, agent := range agents {
		assert.True(t, agent.ScionFormat)
	}
}

func TestDiscoverAgents_ScionTemplatesDirectDir(t *testing.T) {
	dir := t.TempDir()

	// Create scion templates directly in the target directory
	for _, name := range []string{"api-agent", "ui-agent"} {
		templateDir := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(templateDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "`+name+`"
default_harness_config: claude
`), 0644))
	}

	// Discover from templates directory directly
	agents, err := discoverAgents(dir, "", true)
	require.NoError(t, err)
	assert.Len(t, agents, 2)
}

func TestDiscoverAgents_MixedFormats(t *testing.T) {
	dir := t.TempDir()

	// Create .claude/agents/ with an agent
	claudeDir := filepath.Join(dir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(claudeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "reviewer.md"), []byte(`---
name: reviewer
description: Reviews code
tools: Read
---

Review code.
`), 0644))

	// Also create .scion/templates/ with a template
	scionDir := filepath.Join(dir, ".scion", "templates", "my-template")
	require.NoError(t, os.MkdirAll(scionDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "My template"
default_harness_config: claude
`), 0644))

	agents, err := discoverAgents(dir, "", true)
	require.NoError(t, err)
	assert.Len(t, agents, 2, "should find both claude agent and scion template")
}

func TestResultStatus(t *testing.T) {
	allOk := []importResult{
		{Status: "imported"},
		{Status: "imported"},
	}
	assert.Equal(t, "success", resultStatus(allOk))

	withError := []importResult{
		{Status: "imported"},
		{Status: "error"},
	}
	assert.Equal(t, "partial_error", resultStatus(withError))
}

func TestResultMessage(t *testing.T) {
	results := []importResult{
		{Status: "imported"},
		{Status: "imported"},
	}
	assert.Equal(t, "Imported 2 template(s)", resultMessage(results, false))
	assert.Equal(t, "Would import 2 template(s)", resultMessage(results, true))
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	assert.True(t, dirExists(dir))
	assert.False(t, dirExists(filepath.Join(dir, "nonexistent")))

	// File is not a directory
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
	assert.False(t, dirExists(file))
}
