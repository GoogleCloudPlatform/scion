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
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/cmd/internal/cliutil"
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

func TestRootPersistentPreRunEStoresRootOptions(t *testing.T) {
	origAutoConfirm, origNonInteractive, origFormat := autoConfirm, nonInteractive, outputFormat
	defer func() {
		autoConfirm, nonInteractive, outputFormat = origAutoConfirm, origNonInteractive, origFormat
	}()
	t.Setenv("SCION_CLI_MODE", "human")

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
