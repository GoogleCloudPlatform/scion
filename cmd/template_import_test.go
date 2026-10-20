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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestParseSingleFile_PrefixedHarnessDirectories(t *testing.T) {
	tests := []struct {
		dir         string
		frontMatter string
		harness     string
		tools       []string
	}{
		{
			dir:         "backup.claude",
			frontMatter: "tools: [read_file, grep_search]\nmax_turns: 7\n",
			harness:     "gemini",
			tools:       []string{"read_file", "grep_search"},
		},
		{
			dir:         "backup.gemini",
			frontMatter: "tools: Read, Grep\nmaxTurns: 7\n",
			harness:     "claude",
			tools:       []string{"Read", "Grep"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tt.dir, "agents")
			require.NoError(t, os.MkdirAll(dir, 0755))
			file := filepath.Join(dir, "reviewer.md")
			content := "---\nname: reviewer\n" + tt.frontMatter + "---\n\nReview the code.\n"
			require.NoError(t, os.WriteFile(file, []byte(content), 0644))

			agent, err := parseSingleFile(file, "")
			require.NoError(t, err)
			assert.Equal(t, tt.harness, agent.Harness)
			assert.Equal(t, tt.tools, agent.Tools)
			assert.Equal(t, 7, agent.MaxTurns)
			assert.Equal(t, "Review the code.", agent.SystemPrompt)
		})
	}
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
