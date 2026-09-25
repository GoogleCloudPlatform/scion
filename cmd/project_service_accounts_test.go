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

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetFlagChanged clears the Changed bit on the named flags so a shared,
// package-level cobra command can be reused across test cases without
// carrying state from a previous ParseFlags call.
func resetFlagChanged(fs *pflag.FlagSet, names ...string) {
	for _, name := range names {
		if f := fs.Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

// TestSAAddCmd_GCPProjectFlagRenamed locks in the rename: the local --project
// flag (GCP project ID) became --gcp-project so it no longer shadows the root
// --project/-g scion-project selector. No alias for the old name is
// registered: an alias called "project" would recreate the shadowing.
func TestSAAddCmd_GCPProjectFlagRenamed(t *testing.T) {
	assert.NotNil(t, saAddCmd.Flags().Lookup("gcp-project"), "add command should register --gcp-project")
	assert.Nil(t, saAddCmd.Flags().Lookup("project"), "add command must not register a local --project flag")
}

// TestSAAddCmd_RootProjectAndGCPProjectFlagsParseIndependently is the
// regression test for the flag shadowing: with the local flag renamed,
// `-g <scion-project> --gcp-project <gcp>` must parse both values into
// their own variables instead of one shadowing the other.
func TestSAAddCmd_RootProjectAndGCPProjectFlagsParseIndependently(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	assert.Equal(t, "my-scion-project", projectPath)
	assert.Equal(t, "my-gcp-project", saProjectID)
}

// TestSAAddCmd_OldProjectFlagHint verifies that `add ... --project my-gcp`
// (the pre-rename invocation) produces the targeted hint rather than a
// generic "required flag(s) ... not set" error.
func TestSAAddCmd_OldProjectFlagHint(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"--project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.PreRunE, "add command must wire the hint check as PreRunE for it to actually run")
	err = saAddCmd.PreRunE(saAddCmd, nil)
	require.Error(t, err)
	assert.Equal(t, gcpProjectFlagHint, err.Error())
}

// TestSAAddCmd_NoHintWhenGCPProjectSet ensures the hint only fires for the
// old-flag case, not whenever --project happens to be set alongside a
// correctly-specified --gcp-project.
func TestSAAddCmd_NoHintWhenGCPProjectSet(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.PreRunE)
	require.NoError(t, saAddCmd.PreRunE(saAddCmd, nil))
}

// TestSAAddCmd_NoHintWhenNoProjectFlags ensures a bare invocation, with
// neither --project nor --gcp-project set, does not trigger the hint: that
// case must fall through to cobra's normal required-flag error instead.
func TestSAAddCmd_NoHintWhenNoProjectFlags(t *testing.T) {
	origProjectPath, origSAProjectID := projectPath, saProjectID
	defer func() { projectPath, saProjectID = origProjectPath, origSAProjectID }()
	defer resetFlagChanged(saAddCmd.Flags(), "project", "gcp-project")

	err := saAddCmd.ParseFlags([]string{})
	require.NoError(t, err)

	require.NotNil(t, saAddCmd.PreRunE)
	require.NoError(t, saAddCmd.PreRunE(saAddCmd, nil))
}
