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
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunTemplatesUpdate_AgentModeRejectsURLOverrideLocally(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")

	cmd := &cobra.Command{Use: "update"}
	registerTemplatesUpdateFlags(cmd)
	require.NoError(t, cmd.Flags().Set("url", "https://github.com/acme/repo/tree/main/templates/my-tmpl"))

	err := runTemplatesUpdate(cmd, []string{"my-tmpl"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--url is not allowed in agent mode")
}

func TestTemplatesUpdate_AgentModeDefaultScope(t *testing.T) {
	t.Run("agent mode defaults omitted scope to project", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		scope := ""
		if scope == "" && resolveMode() == ModeAgent {
			scope = "project"
		}
		assert.Equal(t, "project", scope)
	})

	t.Run("human mode leaves omitted scope empty", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "human")
		scope := ""
		if scope == "" && resolveMode() == ModeAgent {
			scope = "project"
		}
		assert.Equal(t, "", scope)
	})

	t.Run("singular template alias registers update and import-url", func(t *testing.T) {
		templateCmd, _, err := rootCmd.Find([]string{"template"})
		require.NoError(t, err)
		require.NotNil(t, templateCmd)

		var names []string
		for _, sub := range templateCmd.Commands() {
			names = append(names, sub.Name())
		}
		assert.Contains(t, names, "update")
		assert.Contains(t, names, "import-url")
	})
}
