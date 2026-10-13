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
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// The CLI prints harness-config load warnings (unknown top-level config.yaml
// keys) to stderr and carries on (ptone/scion#4975).

const futureKeyWarning = "config.yaml: unknown top-level field(s) future_block ignored"

func TestPrintHarnessConfigWarnings(t *testing.T) {
	var buf bytes.Buffer
	printHarnessConfigWarnings(&buf, "claude", nil)
	assert.Empty(t, buf.String())

	printHarnessConfigWarnings(&buf, "claude", []string{"first", "second"})
	assert.Equal(t, "warning: claude: first\nwarning: claude: second\n", buf.String())
}

func TestHarnessConfigInstall_UnknownTopLevelKeyWarns(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()
	tmpHome := t.TempDir()
	require.NoError(t, os.Setenv("HOME", tmpHome))
	globalMode = false
	noHub = true
	autoConfirm = true

	project := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(project, 0o755))
	projectPath = project

	src := filepath.Join(tmpHome, "source", "future-hc")
	createTestHarnessConfig(t, src)
	f, err := os.OpenFile(filepath.Join(src, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("future_block:\n  enabled: true\n")
	require.NoError(t, err)

	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("force", false, "")
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	_ = captureStdout(t, func() {
		require.NoError(t, runHarnessConfigInstall(cmd, []string{"file://" + src}))
	})

	assert.Contains(t, stderr.String(), "warning: claude: "+futureKeyWarning)
	installed, err := os.ReadFile(filepath.Join(project, "harness-configs", "claude", "config.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(installed), "future_block:", "install must copy config.yaml unchanged")
}

func TestHarnessConfigUpgrade_UnknownTopLevelKeyWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, config.GlobalDir, "harness-configs", "claude")
	require.NoError(t, config.SeedHarnessConfigFromDir(dir, harness.HarnessesFS(), "claude", true))
	f, err := os.OpenFile(filepath.Join(dir, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("\nfuture_block:\n  enabled: true\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, harnessConfigUpgradeCmd.Flags().Set("dry-run", "true"))
	t.Cleanup(func() { _ = harnessConfigUpgradeCmd.Flags().Set("dry-run", "false") })
	var stderr bytes.Buffer
	harnessConfigUpgradeCmd.SetErr(&stderr)
	t.Cleanup(func() { harnessConfigUpgradeCmd.SetErr(nil) })

	var runErr error
	_ = captureStdout(t, func() {
		runErr = harnessConfigUpgradeCmd.RunE(harnessConfigUpgradeCmd, []string{"claude"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr.String(), "warning: claude: "+futureKeyWarning)
}
