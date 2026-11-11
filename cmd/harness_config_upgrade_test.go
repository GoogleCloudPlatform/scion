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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// With no name given, "harness-config upgrade" reports a custom harness type
// that has no bundled source as skipped and still upgrades the bundled ones;
// naming that config directly returns the not-bundled error
// (ptone/scion#3133).
func TestHarnessConfigUpgrade_SkipsNotBundled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := filepath.Join(home, config.GlobalDir, "harness-configs")

	require.NoError(t, config.SeedHarnessConfigFromDir(
		filepath.Join(parent, "claude"), harness.HarnessesFS(), "claude", true))
	customDir := filepath.Join(parent, "zz-custom")
	require.NoError(t, os.MkdirAll(customDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(customDir, "config.yaml"),
		[]byte("harness: zz-custom\nimage: img:latest\nuser: scion\n"), 0o644))

	require.NoError(t, harnessConfigUpgradeCmd.Flags().Set("dry-run", "true"))
	t.Cleanup(func() { _ = harnessConfigUpgradeCmd.Flags().Set("dry-run", "false") })

	var runErr error
	out := captureStdout(t, func() {
		runErr = harnessConfigUpgradeCmd.RunE(harnessConfigUpgradeCmd, nil)
	})
	require.NoError(t, runErr)
	assert.Contains(t, out, "claude: ")
	assert.Contains(t, out, "zz-custom: skipped (not bundled)")

	_ = captureStdout(t, func() {
		runErr = harnessConfigUpgradeCmd.RunE(harnessConfigUpgradeCmd, []string{"zz-custom"})
	})
	require.ErrorIs(t, runErr, config.ErrHarnessConfigNotBundled)
}
