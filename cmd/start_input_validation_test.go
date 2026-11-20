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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateAgentName(t *testing.T) {
	valid := []struct {
		raw      string
		wantSlug string
	}{
		{"worker-1", "worker-1"},
		{"My_Agent", "my-agent"},
		{"agent.v2", "agent-v2"},
		{"UPPERCASE", "uppercase"},
		{"café", "cafe"},
		{strings.Repeat("a", 63), strings.Repeat("a", 63)},
	}
	for _, tc := range valid {
		t.Run("valid_"+tc.raw, func(t *testing.T) {
			slug, err := validateCreateAgentName(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSlug, slug)
		})
	}

	invalid := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{"empty", "", "must not be empty"},
		{"contains space", "foo bar", "whitespace"},
		{"leading space", " foo", "whitespace"},
		{"trailing space", "foo ", "whitespace"},
		{"contains tab", "foo\tbar", "whitespace"},
		{"contains newline", "foo\nbar", "whitespace"},
		{"too long", strings.Repeat("a", 64), "too long"},
		{"unsupported slash", "foo/bar", "unsupported character"},
		{"unsupported at", "foo@bar", "unsupported character"},
		{"unsupported bang", "foo!", "unsupported character"},
		{"empty slug from dashes", "---", "empty key"},
		{"empty slug from dots", "...", "empty key"},
		{"reserved admin", "admin", "reserved word"},
		{"reserved scion mixed case", "Scion", "reserved word"},
		{"reserved hub", "hub", "reserved word"},
		{"reserved user", "user", "reserved word"},
		{"reserved system", "system", "reserved word"},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			_, err := validateCreateAgentName(tc.raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
			assert.NotContains(t, err.Error(), "display name")
			assert.NotContains(t, err.Error(), "space, ")
		})
	}
}

func TestParseCreateLabels(t *testing.T) {
	t.Run("valid labels", func(t *testing.T) {
		got, err := parseCreateLabels([]string{"issue=ptone/scion#8364", "area=cli"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"issue": "ptone/scion#8364", "area": "cli"}, got)
	})

	t.Run("empty value rejected", func(t *testing.T) {
		_, err := parseCreateLabels([]string{"issue="})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
		assert.Contains(t, err.Error(), `key "issue"`)
	})

	t.Run("whitespace-only value rejected", func(t *testing.T) {
		_, err := parseCreateLabels([]string{"issue=   "})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
	})

	t.Run("missing equals rejected", func(t *testing.T) {
		_, err := parseCreateLabels([]string{"issue"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be key=value")
	})

	t.Run("empty key rejected", func(t *testing.T) {
		_, err := parseCreateLabels([]string{"=value"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be key=value")
	})

	t.Run("parseLabels still allows empty value for list filters", func(t *testing.T) {
		got, err := parseLabels([]string{"marker="})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"marker": ""}, got)
	})
}

func TestStartAndCreate_InputValidationRejectsBeforeHub(t *testing.T) {
	resetHubStartGlobals(t)
	origTF, origCfg, origLabels := taskFilePath, inlineConfigPath, labelFlags
	t.Cleanup(func() {
		taskFilePath, inlineConfigPath, labelFlags = origTF, origCfg, origLabels
	})

	missingFile := filepath.Join(t.TempDir(), "does-not-exist.md")

	commands := []struct {
		name string
		run  func(agentArg string) error
	}{
		{"start", func(agentArg string) error { return RunAgent(startCmd, []string{agentArg}, false) }},
		{"create", func(agentArg string) error { return createCmd.RunE(createCmd, []string{agentArg}) }},
	}

	for _, c := range commands {
		t.Run(c.name+"_agent_name_with_space", func(t *testing.T) {
			taskFilePath, inlineConfigPath, labelFlags = "", "", nil
			err := c.run("bad name")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "whitespace")
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "want usageError, got %T: %v", err, err)
		})

		t.Run(c.name+"_agent_name_reserved", func(t *testing.T) {
			taskFilePath, inlineConfigPath, labelFlags = "", "", nil
			err := c.run("admin")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "reserved word")
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "want usageError, got %T: %v", err, err)
		})

		t.Run(c.name+"_empty_label_value", func(t *testing.T) {
			taskFilePath, inlineConfigPath, labelFlags = "", "", []string{"issue="}
			err := c.run("good-agent")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must not be empty")
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "want usageError, got %T: %v", err, err)
		})

		t.Run(c.name+"_missing_task_file", func(t *testing.T) {
			taskFilePath, inlineConfigPath, labelFlags = missingFile, "", nil
			err := c.run("good-agent")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--task-file")
			assert.True(t, errors.Is(err, os.ErrNotExist), "want os.ErrNotExist, got %v", err)
			var ue *usageError
			assert.True(t, errors.As(err, &ue), "want usageError, got %T: %v", err, err)
		})

		t.Run(c.name+"_missing_config_file", func(t *testing.T) {
			taskFilePath, inlineConfigPath, labelFlags = "", missingFile, nil
			err := c.run("good-agent")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to read config file")
			assert.True(t, errors.Is(err, os.ErrNotExist), "want os.ErrNotExist, got %v", err)
		})
	}
}

// Resume targets existing agents and must not reject names with the new
// create-time name validator, so existing agents remain addressable.
func TestResume_DoesNotRejectSpaceInAgentName(t *testing.T) {
	resetHubStartGlobals(t)
	origTF, origCfg, origLabels := taskFilePath, inlineConfigPath, labelFlags
	t.Cleanup(func() {
		taskFilePath, inlineConfigPath, labelFlags = origTF, origCfg, origLabels
	})

	// Use an invalid --template-scope so RunAgent fails deterministically on
	// a later flag check without needing a Hub, proving it passed the name
	// step without triggering validateCreateAgentName.
	origScope := templateScope
	t.Cleanup(func() { templateScope = origScope })
	templateScope = "invalid-scope"
	taskFilePath, inlineConfigPath, labelFlags = "", "", nil

	err := RunAgent(resumeCmd, []string{"existing agent"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template scope")
	assert.NotContains(t, err.Error(), "whitespace")
}
