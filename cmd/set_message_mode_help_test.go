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

// TestSetMessageModeHelp_DocumentsDefaultsAndBranchRule pins the help text
// to the Hub's real behavior (ptone/scion#4411): the D10 default order in
// createAgentInProject and the branch rule in evaluateSameProjectModes.
func TestSetMessageModeHelp_DocumentsDefaultsAndBranchRule(t *testing.T) {
	long := setMessageModeCmd.Long
	assert.Contains(t, long, "  hub      - ")
	assert.Contains(t, long, "both are in branch mode and one is the other's direct parent")

	flag := strings.Index(long, "the --message-mode flag")
	tmpl := strings.Index(long, "the template's message_mode")
	parent := strings.Index(long, "the parent agent's mode")
	proj := strings.Index(long, "  4. project")
	require.True(t, flag >= 0 && tmpl >= 0 && parent >= 0 && proj >= 0, long)
	assert.True(t, flag < tmpl && tmpl < parent && parent < proj, "default order must be flag, template, parent, project")
}

// TestMessageModeFlagHelp_ListsHub checks that create and start list every
// mode validateMessageMode accepts, including hub, and the real default order.
func TestMessageModeFlagHelp_ListsHub(t *testing.T) {
	for name, c := range map[string]*cobra.Command{"create": createCmd, "start": startCmd} {
		f := c.Flags().Lookup("message-mode")
		require.NotNil(t, f, name)
		assert.Contains(t, f.Usage, "none, lineage, branch, project, hub", name)
		assert.Contains(t, f.Usage, "the template's mode, else the parent agent's, else 'project'", name)
	}
}
