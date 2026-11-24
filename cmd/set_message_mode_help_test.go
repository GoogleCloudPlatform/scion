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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetMessageModeHelp_DocumentsDefaultsAndDirectTreeRules pins the help text
// to the Hub's real behavior: the default order in createAgentInProject and the
// direct and tree rules in evaluateSameProjectModes.
func TestSetMessageModeHelp_DocumentsDefaultsAndDirectTreeRules(t *testing.T) {
	long := setMessageModeCmd.Long
	for _, line := range []string{"  none     - ", "  owner    - ", "  direct   - ", "  tree     - ", "  project  - ", "  hub      - "} {
		assert.Contains(t, long, line)
	}
	assert.Contains(t, long, "Direct rule:")
	assert.Contains(t, long, "Tree rule:")
	assert.Contains(t, long, "scion set-message-mode foo tree --cascade")

	flag := strings.Index(long, "the --message-mode flag")
	tmpl := strings.Index(long, "the template's message_mode")
	parent := strings.Index(long, "the parent agent's mode")
	hubDef := strings.Index(long, "  4. the Hub's default message mode (server.hub.default_message_mode")
	require.True(t, flag >= 0 && tmpl >= 0 && parent >= 0 && hubDef >= 0, long)
	assert.True(t, flag < tmpl && tmpl < parent && parent < hubDef, "default order must be flag, template, parent, hub default")
}

// TestMessageModeFlagHelp_ListsAllModes checks that create and start list every
// mode validateMessageMode accepts and the real default order.
func TestMessageModeFlagHelp_ListsAllModes(t *testing.T) {
	for name, c := range map[string]*cobra.Command{"create": createCmd, "start": startCmd} {
		f := c.Flags().Lookup("message-mode")
		require.NotNil(t, f, name)
		assert.Contains(t, f.Usage, "none, owner, direct, tree, project, hub", name)
		assert.Contains(t, f.Usage, "the template's mode, else the parent agent's, else the Hub's default message mode (server.hub.default_message_mode", name)
	}
}

func TestValidateMessageMode_RetiredHints(t *testing.T) {
	for _, valid := range []string{"", "none", "owner", "direct", "tree", "project", "hub"} {
		require.NoError(t, validateMessageMode(valid))
	}
	err := validateMessageMode("lineage")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner")

	err = validateMessageMode("branch")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "direct")
}
