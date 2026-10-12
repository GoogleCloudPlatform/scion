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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runHubStatusForTest runs 'scion hub status' for the global project with
// globalSettings as its settings file and returns stdout.
func runHubStatusForTest(t *testing.T, globalSettings, format string) string {
	t.Helper()
	home := os.Getenv("HOME")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(globalSettings), 0o644))
	t.Chdir(home)
	origProjectPath, origGlobal, origFormat, origJSON, origEP := projectPath, globalMode, outputFormat, hubOutputJSON, hubEndpoint
	t.Cleanup(func() {
		projectPath, globalMode, outputFormat, hubOutputJSON, hubEndpoint = origProjectPath, origGlobal, origFormat, origJSON, origEP
	})
	projectPath, globalMode, outputFormat, hubOutputJSON, hubEndpoint = "global", true, format, false, ""

	var err error
	stdout, _ := captureStdIO(t, func() { err = runHubStatus(hubStatusCmd, nil) })
	require.NoError(t, err)
	return stdout
}

func TestHubStatus_NoEndpoint(t *testing.T) {
	hermeticCLIEnv(t)
	out := runHubStatusForTest(t, "project_id: status-p\n", "")
	assert.Contains(t, out, "Hub Status")
	assert.Contains(t, out, "Endpoint:   (not configured)")
	assert.Contains(t, out, "No hub configured. Start the local hub with 'scion server start'")
	assert.NotContains(t, out, "Enabled:")
	assert.NotContains(t, out, "Local workstation:")
}

func TestHubStatus_LinkWording(t *testing.T) {
	for _, tc := range []struct {
		name     string
		linked   string
		want     string
		dontWant string
	}{
		{name: "never linked", linked: "", want: "Run 'scion hub link' to link this project with the Hub.", dontWant: "re-link"},
		{name: "unlinked", linked: "  linked: false\n", want: "Run 'scion hub link' to re-link this project with the Hub.", dontWant: "to link this project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeHub(t)
			settings := "project_id: status-p\n"
			if tc.linked != "" {
				settings += "hub:\n" + tc.linked
			}
			out := runHubStatusForTest(t, settings, "")
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, tc.dontWant)
			assert.Contains(t, out, "Local workstation: yes", "a loopback hub reached with the dev token")
			assert.NotContains(t, out, "Enabled:")
		})
	}
}

func TestHubStatus_DeprecatedKeysNote(t *testing.T) {
	withFakeHub(t)
	out := runHubStatusForTest(t, "project_id: status-p\nhub:\n  enabled: false\n  local_only: true\n", "")
	assert.Contains(t, out, "hub.enabled, hub.local_only set in settings; deprecated and ignored")
}

func TestHubStatus_JSON(t *testing.T) {
	withFakeHub(t)
	out := runHubStatusForTest(t, "project_id: status-p\nhub:\n  enabled: true\n", "json")
	var status map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &status), "output: %s", out)
	for _, gone := range []string{"enabled", "enabledScope", "enabledInherited", "cliOverride"} {
		assert.NotContains(t, status, gone)
	}
	assert.Equal(t, true, status["localWorkstation"])
	assert.Equal(t, true, status["configured"])
	assert.Equal(t, []interface{}{"hub.enabled"}, status["deprecatedSettings"])
}
