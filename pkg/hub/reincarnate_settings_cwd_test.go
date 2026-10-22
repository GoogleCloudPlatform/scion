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

package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/require"
)

// The reincarnate plan's image settings come from the hub's global
// settings, never from a project found from the process working directory
// (ptone/scion#5363).
func TestReincarnateImageSettings_IgnoresWorkingDirectoryProject(t *testing.T) {
	for _, k := range []string{"SCION_PROJECT", "SCION_PROJECT_ID", "SCION_HUB_PROJECT_ID", "SCION_ACTIVE_PROFILE"} {
		t.Setenv(k, "")
	}
	// No process-wide DB overlay: the settings files alone decide.
	oldOverlay := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(oldOverlay) })
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(`schema_version: "1"
harness_configs:
  cwd-hc:
    harness: claude
    image: global-image:v1
`), 0o644))

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".scion", "settings.yaml"), []byte(`schema_version: "1"
harness_configs:
  cwd-hc:
    harness: claude
    image: project-image:v2
`), 0o644))
	t.Chdir(root)

	// A logger as the other hub tests set one: if the settings load fails,
	// reincarnateImageSettings logs a warning, which must report the error
	// rather than panic on a nil logger.
	vs := (&Server{agentLifecycleLog: slog.Default()}).reincarnateImageSettings()
	require.NotNil(t, vs)
	require.Equal(t, "global-image:v1", settingsHarnessConfigImage(vs, "cwd-hc", ""),
		"the image must come from the global settings, not the working directory's project")
}
