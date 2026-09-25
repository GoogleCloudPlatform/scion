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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubSecretMigrateCmd_GCPProjectFlagRenamed locks in the rename: the
// local --project flag (GCP project ID) became --gcp-project so it no longer
// shadows the root --project/-g scion-project selector.
func TestHubSecretMigrateCmd_GCPProjectFlagRenamed(t *testing.T) {
	assert.NotNil(t, hubSecretMigrateCmd.Flags().Lookup("gcp-project"), "migrate command should register --gcp-project")
	assert.Nil(t, hubSecretMigrateCmd.Flags().Lookup("project"), "migrate command must not register a local --project flag")
}

// TestHubSecretMigrateCmd_RootProjectAndGCPProjectFlagsParseIndependently is
// the regression test for the flag shadowing: `-g <scion-project>
// --gcp-project <gcp>` must parse both values into their own variables
// instead of one shadowing the other.
func TestHubSecretMigrateCmd_RootProjectAndGCPProjectFlagsParseIndependently(t *testing.T) {
	origProjectPath, origMigrateProject := projectPath, migrateProject
	defer func() { projectPath, migrateProject = origProjectPath, origMigrateProject }()
	defer resetFlagChanged(hubSecretMigrateCmd.Flags(), "project", "gcp-project")

	err := hubSecretMigrateCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	assert.Equal(t, "my-scion-project", projectPath)
	assert.Equal(t, "my-gcp-project", migrateProject)
}

// TestHubSecretMigrateCmd_OldProjectFlagHint verifies that
// `migrate ... --project my-gcp` (the pre-rename invocation) produces the
// targeted hint rather than a generic "required flag(s) ... not set" error.
func TestHubSecretMigrateCmd_OldProjectFlagHint(t *testing.T) {
	origProjectPath, origMigrateProject := projectPath, migrateProject
	defer func() { projectPath, migrateProject = origProjectPath, origMigrateProject }()
	defer resetFlagChanged(hubSecretMigrateCmd.Flags(), "project", "gcp-project")

	err := hubSecretMigrateCmd.ParseFlags([]string{"--project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, hubSecretMigrateCmd.PreRunE, "migrate command must wire the hint check as PreRunE for it to actually run")
	err = hubSecretMigrateCmd.PreRunE(hubSecretMigrateCmd, nil)
	require.Error(t, err)
	assert.Equal(t, gcpProjectFlagHint, err.Error())
}

// TestHubSecretMigrateCmd_NoHintWhenGCPProjectSet ensures the hint only
// fires for the old-flag case, not whenever --project happens to be set
// alongside a correctly-specified --gcp-project.
func TestHubSecretMigrateCmd_NoHintWhenGCPProjectSet(t *testing.T) {
	origProjectPath, origMigrateProject := projectPath, migrateProject
	defer func() { projectPath, migrateProject = origProjectPath, origMigrateProject }()
	defer resetFlagChanged(hubSecretMigrateCmd.Flags(), "project", "gcp-project")

	err := hubSecretMigrateCmd.ParseFlags([]string{"-g", "my-scion-project", "--gcp-project", "my-gcp-project"})
	require.NoError(t, err)

	require.NotNil(t, hubSecretMigrateCmd.PreRunE)
	require.NoError(t, hubSecretMigrateCmd.PreRunE(hubSecretMigrateCmd, nil))
}

// TestHubSecretMigrateCmd_NoHintWhenNoProjectFlags ensures a bare
// invocation, with neither --project nor --gcp-project set, does not
// trigger the hint: that case must fall through to cobra's normal
// required-flag error instead.
func TestHubSecretMigrateCmd_NoHintWhenNoProjectFlags(t *testing.T) {
	origProjectPath, origMigrateProject := projectPath, migrateProject
	defer func() { projectPath, migrateProject = origProjectPath, origMigrateProject }()
	defer resetFlagChanged(hubSecretMigrateCmd.Flags(), "project", "gcp-project")

	err := hubSecretMigrateCmd.ParseFlags([]string{})
	require.NoError(t, err)

	require.NotNil(t, hubSecretMigrateCmd.PreRunE)
	require.NoError(t, hubSecretMigrateCmd.PreRunE(hubSecretMigrateCmd, nil))
}
