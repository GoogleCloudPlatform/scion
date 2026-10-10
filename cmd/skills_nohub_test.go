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

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateNoHubForTest gives the test a temp HOME and a project with no hub
// configured, and points projectPath at that project. With noHubFlag set it
// also sets the --no-hub flag variable. Ambient SCION_* variables are already
// cleared for the whole test binary by TestMain (clearAmbientScionEnv).
// Package state it changes is restored when the test ends.
func isolateNoHubForTest(t *testing.T, noHubFlag bool) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	origProjectPath, origGlobal := projectPath, globalMode
	origFormat, origYes, origNonInteractive, origNoHub, origEndpoint := outputFormat, autoConfirm, nonInteractive, noHub, hubEndpoint
	t.Cleanup(func() {
		projectPath, globalMode = origProjectPath, origGlobal
		outputFormat, autoConfirm, nonInteractive, noHub, hubEndpoint = origFormat, origYes, origNonInteractive, origNoHub, origEndpoint
	})
	projectPath = projectDir
	globalMode = false
	outputFormat = ""
	autoConfirm = true
	nonInteractive = true
	noHub = noHubFlag
	hubEndpoint = ""
}

// noHubModes are the two ways no hub is in use: no hub in settings, and the
// --no-hub flag.
var noHubModes = []struct {
	name      string
	noHubFlag bool
}{
	{name: "hub-not-enabled", noHubFlag: false},
	{name: "no-hub-flag", noHubFlag: true},
}

// setCmdFlagForTest sets a flag on cmd for the duration of the test.
func setCmdFlagForTest(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	f := cmd.Flags().Lookup(name)
	require.NotNil(t, f, "flag %s", name)
	orig, origChanged := f.Value.String(), f.Changed
	require.NoError(t, cmd.Flags().Set(name, value))
	t.Cleanup(func() {
		_ = f.Value.Set(orig)
		f.Changed = origChanged
	})
}

// TestSkillsCommands_NoHubConfigured runs every hub-backed skills command
// with no hub configured, both with hub settings absent and with --no-hub.
// Each must return errHubNotConfigured instead of dereferencing a nil hub
// context.
func TestSkillsCommands_NoHubConfigured(t *testing.T) {
	tests := []struct {
		name  string
		cmd   *cobra.Command
		args  []string
		setup func(t *testing.T) []string // optional; returns args
	}{
		{name: "list", cmd: skillsListCmd},
		{name: "show", cmd: skillsShowCmd, args: []string{"my-skill"}},
		{name: "publish", cmd: skillsPublishCmd, setup: func(t *testing.T) []string {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644))
			setCmdFlagForTest(t, skillsPublishCmd, "version", "1.0.0")
			return []string{dir}
		}},
		{name: "delete", cmd: skillsDeleteCmd, args: []string{"my-skill"}},
		{name: "deprecate", cmd: skillsDeprecateCmd, args: []string{"my-skill"}},
		{name: "versions", cmd: skillsVersionsCmd, args: []string{"my-skill"}},
		{name: "resolve", cmd: skillsResolveCmd, args: []string{"scion://skills/my-skill"}},
	}
	for _, tt := range tests {
		for _, mode := range noHubModes {
			t.Run(tt.name+"/"+mode.name, func(t *testing.T) {
				isolateNoHubForTest(t, mode.noHubFlag)
				args := tt.args
				if tt.setup != nil {
					args = tt.setup(t)
				}

				var err error
				assert.NotPanics(t, func() { err = tt.cmd.RunE(tt.cmd, args) })
				require.Error(t, err)
				assert.ErrorIs(t, err, errHubNotConfigured)
				assert.Contains(t, err.Error(), "this command needs a hub")
			})
		}
	}
}
