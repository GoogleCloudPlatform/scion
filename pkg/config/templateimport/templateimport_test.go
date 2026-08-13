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

package templateimport

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// --- Front Matter Tests ---

func TestParseFrontMatter_Valid(t *testing.T) {
	data := []byte(`---
name: test-agent
description: A test agent
model: sonnet
---

You are a test agent.
`)
	fm, body, err := ParseFrontMatter(data)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", fm["name"])
	assert.Equal(t, "A test agent", fm["description"])
	assert.Equal(t, "sonnet", fm["model"])
	assert.Contains(t, body, "You are a test agent.")
}

func TestParseFrontMatter_Empty(t *testing.T) {
	data := []byte(`---
---

Just body content.
`)
	fm, body, err := ParseFrontMatter(data)
	require.NoError(t, err)
	assert.NotNil(t, fm)
	assert.Empty(t, fm)
	assert.Contains(t, body, "Just body content.")
}

func TestParseFrontMatter_NoFrontMatter(t *testing.T) {
	data := []byte(`# Just a markdown file

No front matter here.
`)
	fm, body, err := ParseFrontMatter(data)
	require.NoError(t, err)
	assert.Nil(t, fm)
	assert.Contains(t, body, "# Just a markdown file")
}

func TestParseFrontMatter_Unclosed(t *testing.T) {
	data := []byte(`---
name: broken
`)
	_, _, err := ParseFrontMatter(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unclosed front matter")
}

func TestParseFrontMatter_InvalidYAML(t *testing.T) {
	data := []byte(`---
name: [invalid yaml
  broken: {
---

body
`)
	_, _, err := ParseFrontMatter(data)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse front matter YAML")
}

// --- Detection Tests ---

func TestDetectFromPath_Claude(t *testing.T) {
	result := detectFromPath("/project/.claude/agents/reviewer.md")
	assert.Equal(t, "claude", result)
}

func TestDetectFromPath_Gemini(t *testing.T) {
	result := detectFromPath("/project/.gemini/agents/auditor.md")
	assert.Equal(t, "gemini", result)
}

func TestDetectFromPath_Unknown(t *testing.T) {
	result := detectFromPath("/project/agents/something.md")
	assert.Empty(t, result)
}

func TestDetectFromFrontMatter_Claude(t *testing.T) {
	fm := map[string]any{
		"name":           "reviewer",
		"tools":          "Read, Glob, Grep, Bash",
		"permissionMode": "default",
	}
	result := detectFromFrontMatter(fm)
	assert.Equal(t, "claude", result)
}

func TestDetectFromFrontMatter_Gemini(t *testing.T) {
	fm := map[string]any{
		"name":         "auditor",
		"tools":        []any{"read_file", "grep_search"},
		"kind":         "local",
		"timeout_mins": 5,
	}
	result := detectFromFrontMatter(fm)
	assert.Equal(t, "gemini", result)
}

func TestDetectFromFrontMatter_Ambiguous(t *testing.T) {
	fm := map[string]any{
		"name":        "agent",
		"description": "some agent",
	}
	result := detectFromFrontMatter(fm)
	assert.Empty(t, result)
}

func TestDetectHarness_FileBasedClaude(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0755))

	file := filepath.Join(agentsDir, "reviewer.md")
	content := `---
name: reviewer
description: Reviews code
tools: Read, Glob
---

Review the code.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	result, err := DetectHarness(file)
	require.NoError(t, err)
	assert.Equal(t, "claude", result)
}

func TestDetectHarness_FileBasedGemini(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".gemini", "agents")
	require.NoError(t, os.MkdirAll(agentsDir, 0755))

	file := filepath.Join(agentsDir, "auditor.md")
	content := `---
name: auditor
description: Audits code
kind: local
tools:
  - read_file
  - grep_search
---

Audit the code.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	result, err := DetectHarness(file)
	require.NoError(t, err)
	assert.Equal(t, "gemini", result)
}

// --- Claude Importer Tests ---

func TestClaudeImporter_Parse(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "reviewer.md")
	content := `---
name: code-reviewer
description: Reviews code for quality
tools: Read, Glob, Grep, Bash
model: sonnet
maxTurns: 10
permissionMode: default
---

You are a code reviewer. Analyze the code and provide feedback.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	importer := &ClaudeImporter{}
	agent, err := importer.Parse(file)
	require.NoError(t, err)

	assert.Equal(t, "code-reviewer", agent.Name)
	assert.Equal(t, "Reviews code for quality", agent.Description)
	assert.Equal(t, "claude", agent.Harness)
	assert.Equal(t, "sonnet", agent.Model)
	assert.Equal(t, []string{"Read", "Glob", "Grep", "Bash"}, agent.Tools)
	assert.Equal(t, 10, agent.MaxTurns)
	assert.Contains(t, agent.SystemPrompt, "You are a code reviewer")
}

func TestClaudeImporter_ParseNoFrontMatter(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain.md")
	require.NoError(t, os.WriteFile(file, []byte("# Just markdown\n"), 0644))

	importer := &ClaudeImporter{}
	_, err := importer.Parse(file)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no front matter")
}

func TestClaudeImporter_ParseInheritModel(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent.md")
	content := `---
name: helper
description: A helper
model: inherit
---

Help the user.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	importer := &ClaudeImporter{}
	agent, err := importer.Parse(file)
	require.NoError(t, err)
	assert.Empty(t, agent.Model, "inherit model should be omitted")
}

func TestClaudeImporter_ParseDir(t *testing.T) {
	dir := t.TempDir()

	// Valid agent file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent1.md"), []byte(`---
name: agent-one
description: First agent
---

Prompt one.
`), 0644))

	// Another valid agent file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent2.md"), []byte(`---
name: agent-two
description: Second agent
tools: Read
---

Prompt two.
`), 0644))

	// Non-agent file (no front matter)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# README\n"), 0644))

	// Non-markdown file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0644))

	importer := &ClaudeImporter{}
	agents, err := importer.ParseDir(dir)
	require.NoError(t, err)
	assert.Len(t, agents, 2)
}

func TestClaudeImporter_Detect(t *testing.T) {
	importer := &ClaudeImporter{}

	// Claude-style front matter
	fm := map[string]any{
		"tools":          "Read, Glob",
		"permissionMode": "default",
	}
	score := importer.Detect("/some/file.md", fm)
	assert.GreaterOrEqual(t, score, 6)

	// Gemini-style front matter
	fm2 := map[string]any{
		"tools": []any{"read_file"},
		"kind":  "local",
	}
	score2 := importer.Detect("/some/file.md", fm2)
	assert.Equal(t, 0, score2)
}

// --- Gemini Importer Tests ---

func TestGeminiImporter_Parse(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "auditor.md")
	content := `---
name: security-auditor
description: Finds security vulnerabilities
kind: local
tools:
  - read_file
  - grep_search
model: gemini-2.5-pro
temperature: 0.2
max_turns: 10
timeout_mins: 5
---

You are a security auditor. Find vulnerabilities.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	importer := &GeminiImporter{}
	agent, err := importer.Parse(file)
	require.NoError(t, err)

	assert.Equal(t, "security-auditor", agent.Name)
	assert.Equal(t, "Finds security vulnerabilities", agent.Description)
	assert.Equal(t, "gemini", agent.Harness)
	assert.Equal(t, "gemini-2.5-pro", agent.Model)
	assert.Equal(t, []string{"read_file", "grep_search"}, agent.Tools)
	assert.Equal(t, 10, agent.MaxTurns)
	assert.InDelta(t, 0.2, agent.Temperature, 0.001)
	assert.Contains(t, agent.SystemPrompt, "You are a security auditor")
}

func TestGeminiImporter_ParseSkipsRemote(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "remote-agent.md")
	content := `---
name: remote-helper
description: A remote agent
kind: remote
agent_card_url: https://example.com/agent
---

Remote agent instructions.
`
	require.NoError(t, os.WriteFile(file, []byte(content), 0644))

	importer := &GeminiImporter{}
	_, err := importer.Parse(file)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}

func TestGeminiImporter_ParseDir(t *testing.T) {
	dir := t.TempDir()

	// Valid local agent
	require.NoError(t, os.WriteFile(filepath.Join(dir, "local.md"), []byte(`---
name: local-agent
description: A local agent
kind: local
---

Instructions.
`), 0644))

	// Remote agent (should be skipped)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "remote.md"), []byte(`---
name: remote-agent
description: A remote agent
kind: remote
---

Remote instructions.
`), 0644))

	// Valid agent without kind (defaults to local)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default.md"), []byte(`---
name: default-agent
description: Default kind agent
---

Default instructions.
`), 0644))

	importer := &GeminiImporter{}
	agents, err := importer.ParseDir(dir)
	require.NoError(t, err)
	assert.Len(t, agents, 2, "should import 2 agents, skipping remote")
}

func TestGeminiImporter_Detect(t *testing.T) {
	importer := &GeminiImporter{}

	// Gemini-style front matter
	fm := map[string]any{
		"tools": []any{"read_file"},
		"kind":  "local",
	}
	score := importer.Detect("/some/file.md", fm)
	assert.GreaterOrEqual(t, score, 6)

	// Claude-style front matter
	fm2 := map[string]any{
		"tools":          "Read, Glob",
		"permissionMode": "default",
	}
	score2 := importer.Detect("/some/file.md", fm2)
	assert.Equal(t, 0, score2)
}

// --- Writer Tests ---

func TestWriteTemplate_Claude(t *testing.T) {
	templatesDir := t.TempDir()

	agent := &ImportedAgent{
		Name:         "test-claude",
		Description:  "A test Claude agent",
		Harness:      "claude",
		Model:        "sonnet",
		SystemPrompt: "You are a test agent.\n\nDo good work.",
	}

	path, err := WriteTemplate(agent, templatesDir, false)
	require.NoError(t, err)
	assert.DirExists(t, path)

	// Check scion-agent.yaml exists and has correct default_harness_config
	configPath := filepath.Join(path, "scion-agent.yaml")
	assert.FileExists(t, configPath)
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var cfg api.ScionConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	assert.Equal(t, "claude", cfg.DefaultHarnessConfig)
	assert.Empty(t, cfg.Harness, "agnostic templates should not have harness field")
	assert.Equal(t, "sonnet", cfg.Model)

	// Check instruction file
	instructionPath := filepath.Join(path, "home", ".claude", "CLAUDE.md")
	assert.FileExists(t, instructionPath)
	content, err := os.ReadFile(instructionPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "You are a test agent.")

	// Check home directory structure
	assert.DirExists(t, filepath.Join(path, "home"))
	assert.DirExists(t, filepath.Join(path, "home", ".claude"))
}

func TestWriteTemplate_Gemini(t *testing.T) {
	templatesDir := t.TempDir()

	agent := &ImportedAgent{
		Name:         "test-gemini",
		Description:  "A test Gemini agent",
		Harness:      "gemini",
		Model:        "gemini-2.5-pro",
		SystemPrompt: "You are a test Gemini agent.",
	}

	path, err := WriteTemplate(agent, templatesDir, false)
	require.NoError(t, err)
	assert.DirExists(t, path)

	// Check scion-agent.yaml
	configPath := filepath.Join(path, "scion-agent.yaml")
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var cfg api.ScionConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	assert.Equal(t, "gemini-cli", cfg.DefaultHarnessConfig)
	assert.Empty(t, cfg.Harness, "agnostic templates should not have harness field")
	assert.Equal(t, "gemini-2.5-pro", cfg.Model)

	// Check instruction file
	instructionPath := filepath.Join(path, "home", ".gemini", "GEMINI.md")
	assert.FileExists(t, instructionPath)
	content, err := os.ReadFile(instructionPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "You are a test Gemini agent.")
}

func TestWriteTemplate_AlreadyExists(t *testing.T) {
	templatesDir := t.TempDir()
	existing := filepath.Join(templatesDir, "existing-agent")
	require.NoError(t, os.MkdirAll(existing, 0755))

	agent := &ImportedAgent{
		Name:    "existing-agent",
		Harness: "claude",
	}

	_, err := WriteTemplate(agent, templatesDir, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestWriteTemplate_ForceOverwrite(t *testing.T) {
	templatesDir := t.TempDir()
	existing := filepath.Join(templatesDir, "existing-agent")
	require.NoError(t, os.MkdirAll(existing, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(existing, "old-file.txt"), []byte("old"), 0644))

	agent := &ImportedAgent{
		Name:         "existing-agent",
		Harness:      "claude",
		SystemPrompt: "New prompt.",
	}

	path, err := WriteTemplate(agent, templatesDir, true)
	require.NoError(t, err)
	assert.DirExists(t, path)

	// Old file should be gone
	assert.NoFileExists(t, filepath.Join(path, "old-file.txt"))
}

func TestWriteTemplate_NoModel(t *testing.T) {
	templatesDir := t.TempDir()

	agent := &ImportedAgent{
		Name:         "no-model",
		Harness:      "claude",
		SystemPrompt: "A prompt.",
	}

	path, err := WriteTemplate(agent, templatesDir, false)
	require.NoError(t, err)

	// Config should have default_harness_config but no model
	configPath := filepath.Join(path, "scion-agent.yaml")
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var cfg api.ScionConfig
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	assert.Equal(t, "claude", cfg.DefaultHarnessConfig)
	assert.Empty(t, cfg.Harness, "agnostic templates should not have harness field")
	assert.Empty(t, cfg.Model)
}

// --- Scion Format Tests ---

func TestIsScionTemplate(t *testing.T) {
	dir := t.TempDir()

	// Not a scion template (no scion-agent.yaml)
	assert.False(t, IsScionTemplate(dir))

	// Create scion-agent.yaml
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "Test template"
default_harness_config: claude
`), 0644))

	assert.True(t, IsScionTemplate(dir))
}

func TestIsScionTemplatesDir(t *testing.T) {
	dir := t.TempDir()

	// Empty directory
	assert.False(t, IsScionTemplatesDir(dir))

	// Create a subdirectory without scion-agent.yaml
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "not-a-template"), 0755))
	assert.False(t, IsScionTemplatesDir(dir))

	// Create a valid scion template subdirectory
	templateDir := filepath.Join(dir, "my-template")
	require.NoError(t, os.MkdirAll(templateDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "A template"
`), 0644))

	assert.True(t, IsScionTemplatesDir(dir))
}

func TestDiscoverScionTemplates(t *testing.T) {
	dir := t.TempDir()

	// Create two scion templates
	for _, name := range []string{"frontend", "backend"} {
		templateDir := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Join(templateDir, "home"), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte(fmt.Sprintf(`
schema_version: "1"
description: "%s agent template"
default_harness_config: claude
model: sonnet
`, name)), 0644))
	}

	// Create a non-template directory (should be skipped)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "README.md"), []byte("# Docs\n"), 0644))

	agents, err := DiscoverScionTemplates(dir)
	require.NoError(t, err)
	assert.Len(t, agents, 2)

	for _, agent := range agents {
		assert.True(t, agent.ScionFormat)
		assert.Equal(t, "claude", agent.Harness)
		assert.Equal(t, "sonnet", agent.Model)
		assert.NotEmpty(t, agent.SourcePath)
	}
}

func TestParseScionTemplate(t *testing.T) {
	dir := t.TempDir()
	templateDir := filepath.Join(dir, "my-agent")
	require.NoError(t, os.MkdirAll(templateDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(templateDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "My custom agent"
default_harness_config: gemini
model: gemini-2.5-pro
`), 0644))

	agent, err := ParseScionTemplate(templateDir)
	require.NoError(t, err)
	assert.Equal(t, "my-agent", agent.Name)
	assert.Equal(t, "My custom agent", agent.Description)
	assert.Equal(t, "gemini", agent.Harness)
	assert.Equal(t, "gemini-2.5-pro", agent.Model)
	assert.True(t, agent.ScionFormat)
	assert.Equal(t, templateDir, agent.SourcePath)
}

func TestCopyScionTemplate(t *testing.T) {
	// Create source template
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "home", ".claude"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "Source template"
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "home", ".claude", "CLAUDE.md"), []byte("# Instructions\n"), 0644))

	destBase := t.TempDir()
	destDir := filepath.Join(destBase, "copied-template")

	path, err := CopyScionTemplate(srcDir, destDir, false)
	require.NoError(t, err)
	assert.Equal(t, destDir, path)

	// Verify files were copied
	assert.FileExists(t, filepath.Join(destDir, "scion-agent.yaml"))
	assert.FileExists(t, filepath.Join(destDir, "home", ".claude", "CLAUDE.md"))

	content, err := os.ReadFile(filepath.Join(destDir, "home", ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Instructions\n", string(content))
}

func TestCopyScionTemplate_AlreadyExists(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "scion-agent.yaml"), []byte("test\n"), 0644))

	destBase := t.TempDir()
	destDir := filepath.Join(destBase, "existing")
	require.NoError(t, os.MkdirAll(destDir, 0755))

	_, err := CopyScionTemplate(srcDir, destDir, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCopyScionTemplate_ForceOverwrite(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "scion-agent.yaml"), []byte("new\n"), 0644))

	destBase := t.TempDir()
	destDir := filepath.Join(destBase, "existing")
	require.NoError(t, os.MkdirAll(destDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(destDir, "old-file.txt"), []byte("old"), 0644))

	path, err := CopyScionTemplate(srcDir, destDir, true)
	require.NoError(t, err)
	assert.Equal(t, destDir, path)
	assert.NoFileExists(t, filepath.Join(destDir, "old-file.txt"))
	assert.FileExists(t, filepath.Join(destDir, "scion-agent.yaml"))
}

func TestWriteTemplate_ScionFormat(t *testing.T) {
	// Create source scion template
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "home"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "scion-agent.yaml"), []byte(`
schema_version: "1"
description: "Scion template"
default_harness_config: claude
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "system-prompt.md"), []byte("# Prompt\n"), 0644))

	templatesDir := t.TempDir()

	agent := &ImportedAgent{
		Name:        "scion-copy",
		Harness:     "claude",
		SourcePath:  srcDir,
		ScionFormat: true,
	}

	path, err := WriteTemplate(agent, templatesDir, false)
	require.NoError(t, err)
	assert.DirExists(t, path)
	assert.FileExists(t, filepath.Join(path, "scion-agent.yaml"))
	assert.FileExists(t, filepath.Join(path, "system-prompt.md"))
}

// --- Helper Tests ---

func TestToInt(t *testing.T) {
	assert.Equal(t, 10, toInt(10))
	assert.Equal(t, 10, toInt(float64(10)))
	assert.Equal(t, 10, toInt(int64(10)))
	assert.Equal(t, 0, toInt("not a number"))
	assert.Equal(t, 0, toInt(nil))
}

func TestToFloat(t *testing.T) {
	assert.InDelta(t, 0.5, toFloat(0.5), 0.001)
	assert.InDelta(t, 10.0, toFloat(10), 0.001)
	assert.InDelta(t, 10.0, toFloat(int64(10)), 0.001)
	assert.InDelta(t, 0.0, toFloat("not a number"), 0.001)
	assert.InDelta(t, 0.0, toFloat(nil), 0.001)
}
