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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/settingslockhook"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigSet_SettingsLockTimeoutIsAClearError: while another holder keeps
// the settings lock, `scion config set` returns ErrSettingsLockTimeout after
// the bounded wait (shortened here; pkg/config tests the wait itself), as an
// error naming the lock file and saying to try again (cobra prints it and
// exits non-zero); it neither hangs, retries forever nor writes the setting.
func TestConfigSet_SettingsLockTimeoutIsAClearError(t *testing.T) {
	setupProjectSettings(t, &config.VersionedSettings{SchemaVersion: "1"})
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	// Another process's lock: a separate open file description conflicts
	// with the settings lock's flock even within this process.
	held, err := filelock.Acquire(filepath.Join(globalDir, ".settings.lock"), 0o644, 0)
	require.NoError(t, err)
	defer held.Unlock()

	// A short wait, so the test does not pay the 10s default.
	t.Cleanup(settingslockhook.Set(300 * time.Millisecond))

	origGlobal := configGlobal
	t.Cleanup(func() { configGlobal = origGlobal })
	configGlobal = true

	start := time.Now()
	var runErr error
	out := captureStdout(t, func() {
		runErr = configSetCmd.RunE(configSetCmd, []string{"default_template", "locked-out"})
	})
	require.Error(t, runErr)
	assert.True(t, errors.Is(runErr, config.ErrSettingsLockTimeout), "err = %v", runErr)
	assert.Contains(t, runErr.Error(), ".settings.lock")
	assert.Contains(t, runErr.Error(), "try again")
	assert.Less(t, time.Since(start), 5*time.Second, "the shortened wait was not used")
	assert.Empty(t, out)
	if data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml")); err == nil {
		assert.False(t, strings.Contains(string(data), "locked-out"), "setting written despite the timeout")
	}
}
