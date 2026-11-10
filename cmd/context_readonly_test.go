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
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertNoContextFiles fails when context.yaml or its lock exists under
// the test HOME's global directory.
func assertNoContextFiles(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".scion")
	for _, name := range []string{"context.yaml", "context.yaml.lock"} {
		_, err := os.Stat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(err), "%s exists (stat error %v)", name, err)
	}
}

// The read-only config commands apply the one-time import of the global
// hub.endpoint in memory only: they see the imported hub, and never create
// context.yaml or its lock (ptone/scion#5483). A command that writes
// settings still saves the import.
func TestReadOnlyConfigCommands_DoNotWriteContext(t *testing.T) {
	paths := make([]string, 0, len(contextReadOnlyCommands))
	for path := range contextReadOnlyCommands {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			setupContextTest(t)
			enableGlobalHubImport(t)
			writeHomeGlobalSettings(t, "schema_version: \"1\"\nhub:\n  endpoint: https://global.example.com\n")
			args := strings.Fields(path)[1:]
			c := findCommand(t, args...)
			require.Equal(t, path, c.CommandPath(), "contextReadOnlyCommands names a command that does not exist")

			require.NoError(t, resolveInvocationTarget(c, ModeHuman))
			assertNoContextFiles(t)
			// The import applies in memory: the command sees the hub the
			// next saving command imports.
			assert.Equal(t, "https://global.example.com", hubEndpoint)
			assert.Equal(t, targetSourceContext, hubSource)
		})
	}

	t.Run("config set still imports", func(t *testing.T) {
		setupContextTest(t)
		enableGlobalHubImport(t)
		writeHomeGlobalSettings(t, "schema_version: \"1\"\nhub:\n  endpoint: https://global.example.com\n")
		require.NoError(t, resolveInvocationTarget(findCommand(t, "config", "set"), ModeHuman))
		_, err := os.Stat(contextFilePath())
		assert.NoError(t, err, "the import should have written context.yaml")
	})
}
