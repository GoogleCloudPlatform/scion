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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectListLocalArgs are the arguments of the machine-local project
// list, the listing of project directories on this machine.
var projectListLocalArgs = []string{"project", "list"}

// projectListGoldenEnv names the variable that makes
// TestProjectListLocal_Golden write its golden files instead of comparing
// against them. It is a SCION_TEST_* variable because the cmd tests' TestMain
// clears every other SCION_* variable.
const projectListGoldenEnv = "SCION_TEST_UPDATE_GOLDEN"

// writeLocalProjectFixture creates, under HOME, one project directory of
// each kind the machine-local list shows: the global project, an external
// project whose workspace is gone, a git project with agents, a legacy git
// project (agents next to the config directory) and an orphaned leftover.
func writeLocalProjectFixture(t *testing.T) {
	t.Helper()
	home := os.Getenv("HOME")
	global := filepath.Join(home, ".scion")
	configs := filepath.Join(global, "project-configs")
	for _, dir := range []string{
		filepath.Join(global, "agents", "a1"),
		filepath.Join(configs, "ext__9abc", ".scion"),
		filepath.Join(configs, "gitcfg__def0", ".scion", "agents", "a"),
		filepath.Join(configs, "gitcfg__def0", ".scion", "agents", "b"),
		filepath.Join(configs, "legacy__5678", "agents", "bob"),
		filepath.Join(configs, "orphan__1234"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(configs, "ext__9abc", ".scion", "settings.yaml"),
		[]byte("workspace_path: "+filepath.Join(home, "missing")+"\nproject_id: 11111111-0000-4000-8000-000000000001\n"), 0o644))
}

// TestProjectListLocal_Golden pins the machine-local project list, text and
// JSON, byte for byte. The golden files were written from the listing as it
// was before 'project list' became the list across hubs; run the test with
// SCION_TEST_UPDATE_GOLDEN=1 to write them again.
func TestProjectListLocal_Golden(t *testing.T) {
	// The setup below changes the working directory; the golden files live
	// in this package's testdata directory.
	pkgDir, err := os.Getwd()
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		args   []string
		golden string
	}{
		{"text", nil, "project-list-local.txt"},
		{"json", []string{"--format", "json"}, "project-list-local.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			restoreAllSilenceUsage(t)
			t.Cleanup(func() { rootCmd.SetArgs(nil) })
			writeLocalProjectFixture(t)

			args := append(append([]string{}, projectListLocalArgs...), tc.args...)
			stdout, _, err := runScion(t, args...)
			require.NoError(t, err)
			got := strings.ReplaceAll(stdout, os.Getenv("HOME"), "$HOME")

			path := filepath.Join(pkgDir, "testdata", tc.golden)
			if os.Getenv(projectListGoldenEnv) == "1" {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(want), got)
		})
	}
}
