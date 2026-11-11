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

// newProjectFlagCommand builds a bare cobra.Command with a "project" flag
// bound to the package-level projectPath var, mirroring how rootCmd binds
// its persistent --project/-g flag. Tests use this to exercise
// detectCrossProjectTarget without going through the real command tree.
func newProjectFlagCommand(t *testing.T) *cobra.Command {
	t.Helper()
	origProjectPath := projectPath
	t.Cleanup(func() { projectPath = origProjectPath })

	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVarP(&projectPath, "project", "g", "", "")
	return cmd
}

func TestDetectCrossProjectTarget_NotAnAgent(t *testing.T) {
	// Explicitly clear SCION_AGENT_NAME: this test asserts human-caller
	// behavior, and the test process itself may be running inside an agent
	// container where the variable is already set in the ambient environment.
	t.Setenv("SCION_AGENT_NAME", "")

	cmd := newProjectFlagCommand(t)
	require.NoError(t, cmd.Flags().Set("project", "other-project"))

	got := detectCrossProjectTarget(cmd)
	assert.Empty(t, got, "a human caller (no SCION_AGENT_NAME) is never cross-project")
}

func TestDetectCrossProjectTarget_ProjectFlagNotSet(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")

	cmd := newProjectFlagCommand(t)
	// --project was never set — no cross-project detection should fire.

	got := detectCrossProjectTarget(cmd)
	assert.Empty(t, got)
}

func TestDetectCrossProjectTarget_SameProjectBySlug(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")

	cmd := newProjectFlagCommand(t)
	require.NoError(t, cmd.Flags().Set("project", "own-project"))

	got := detectCrossProjectTarget(cmd)
	assert.Empty(t, got, "--project matching the agent's own project slug is same-project")
}

func TestDetectCrossProjectTarget_SameProjectByID(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT_ID", "own-project-id")

	cmd := newProjectFlagCommand(t)
	require.NoError(t, cmd.Flags().Set("project", "own-project-id"))

	got := detectCrossProjectTarget(cmd)
	assert.Empty(t, got, "--project matching the agent's own project ID is same-project")
}

func TestDetectCrossProjectTarget_DifferentProject(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")
	t.Setenv("SCION_PROJECT_ID", "own-project-id")

	cmd := newProjectFlagCommand(t)
	require.NoError(t, cmd.Flags().Set("project", "other-project"))

	got := detectCrossProjectTarget(cmd)
	assert.Equal(t, "other-project", got,
		"--project naming a different project than the agent's own must be reported as the cross-project target")
}
