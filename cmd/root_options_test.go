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
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/cmd/internal/cliutil"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
)

// withRootOptions makes cmd's invocations in this test see opts as the root
// flag values, the way root's PersistentPreRunE hands them over, and restores
// cmd's previous context when the test ends.
func withRootOptions(t *testing.T, cmd *cobra.Command, opts cliutil.RootOptions) {
	t.Helper()
	prev := cmd.Context()
	cmd.SetContext(cliutil.WithRootOptions(context.Background(), opts))
	t.Cleanup(func() { cmd.SetContext(prev) })
}

// isolateRootHook prepares a test that runs root's real PersistentPreRunE:
// HOME and the working directory become empty temp dirs, so no user or
// project settings are read, the CLI mode is pinned to human, and every
// package-level value the hook writes is restored when the test ends.
func isolateRootHook(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("SCION_CLI_MODE", "human")
	t.Setenv("SCION_HOST_UID", "")

	origProjectPath, origGlobalMode := projectPath, globalMode
	origAutoConfirm, origNonInteractive := autoConfirm, nonInteractive
	origFormat, origAutoHelp := outputFormat, autoHelp
	origZone := clitime.Zone()
	t.Cleanup(func() {
		projectPath, globalMode = origProjectPath, origGlobalMode
		autoConfirm, nonInteractive = origAutoConfirm, origNonInteractive
		outputFormat, autoHelp = origFormat, origAutoHelp
		clitime.SetZone(origZone)
	})
}

func TestRootPersistentPreRunEStoresRootOptions(t *testing.T) {
	isolateRootHook(t)

	autoConfirm = false
	nonInteractive = true
	outputFormat = "json"

	cmd := &cobra.Command{Use: "whoami"}
	require.NoError(t, rootCmd.PersistentPreRunE(cmd, nil))

	opts, ok := cliutil.RootOptionsFrom(cmd.Context())
	require.True(t, ok, "root hook must store RootOptions on the command context")
	assert.True(t, opts.NonInteractive)
	assert.True(t, opts.AutoConfirm, "snapshot must be taken after --non-interactive implies --yes")
	assert.Equal(t, "json", opts.OutputFormat)
	assert.Equal(t, snapshotRootOptions(), opts)
}

// TestRootOptionsReachRunEThroughCobra runs a converted command through the
// real cobra dispatch (flag parsing, root's PersistentPreRunE, RunE) and
// checks that the root flag values the hook stores reach RunE. Use it as the
// template for the end-to-end test of each converted group.
func TestRootOptionsReachRunEThroughCobra(t *testing.T) {
	isolateRootHook(t)
	t.Setenv("SCION_AGENT_SLUG", "e2e-agent")
	t.Setenv("SCION_AGENT_NAME", "E2E Agent")
	t.Setenv("SCION_AGENT_ID", "e2e-id")

	prevCtx, prevSilence := whoamiCmd.Context(), whoamiCmd.SilenceUsage
	t.Cleanup(func() {
		whoamiCmd.SetContext(prevCtx)
		whoamiCmd.SilenceUsage = prevSilence
		if f := rootCmd.PersistentFlags().Lookup("format"); f != nil {
			f.Changed = false
		}
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	var cobraOut bytes.Buffer
	rootCmd.SetOut(&cobraOut)
	rootCmd.SetErr(&cobraOut)
	rootCmd.SetArgs([]string{"whoami", "--format", "json"})

	out := captureStdout(t, func() {
		require.NoError(t, rootCmd.Execute())
	})

	var result WhoamiResult
	require.NoError(t, json.Unmarshal([]byte(out), &result), "stdout must be JSON, got %q", out)
	assert.Equal(t, "e2e-agent", result.Slug)
	assert.Equal(t, "E2E Agent", result.Name)
	assert.Equal(t, "e2e-id", result.ID)

	opts, ok := cliutil.RootOptionsFrom(whoamiCmd.Context())
	require.True(t, ok, "cobra must hand the hook's context to RunE's command")
	assert.Equal(t, "json", opts.OutputFormat)
}

func TestRootOptionsFallsBackToGlobals(t *testing.T) {
	origFormat, origProfile := outputFormat, profile
	defer func() { outputFormat, profile = origFormat, origProfile }()
	outputFormat = "json"
	profile = "p1"

	opts := rootOptions(&cobra.Command{Use: "x"})
	assert.Equal(t, "json", opts.OutputFormat)
	assert.Equal(t, "p1", opts.Profile)
}

func TestRootOptionsPrefersContext(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()
	outputFormat = "json"

	cmd := &cobra.Command{Use: "x"}
	withRootOptions(t, cmd, cliutil.RootOptions{OutputFormat: "plain"})
	assert.Equal(t, "plain", rootOptions(cmd).OutputFormat)
}
