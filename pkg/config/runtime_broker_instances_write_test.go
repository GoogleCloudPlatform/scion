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

//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
)

const writerTestSettings = `# operator comment, kept
schema_version: "1"
hub:
  endpoint: https://hub.example.test
server:
  broker:
    broker_id: legacy-id
`

func writerTestInstances() []V1RuntimeBrokerInstanceConfig {
	return []V1RuntimeBrokerInstanceConfig{{
		Key:           "docker",
		Name:          "docker",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeDocker},
	}}
}

func writeWriterTestSettings(t *testing.T, mode os.FileMode) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte(writerTestSettings), mode))
	require.NoError(t, os.Chmod(path, mode))
	return dir, path
}

func TestWriteRuntimeBrokerInstances_WritesOnlyTheKey(t *testing.T) {
	dir, path := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "# operator comment, kept\n"), "bytes outside the key are kept")
	assert.Contains(t, string(data), "broker_id: legacy-id")
	got, err := strictInstancesFromYAML(data)
	require.NoError(t, err)
	assert.Equal(t, writerTestInstances(), got)

	// Equal content is a no-op.
	before, _ := os.Stat(path)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	after, _ := os.Stat(path)
	assert.True(t, os.SameFile(before, after), "an equal write does not replace the file")
}

// Named mutant target: settings written 0644 instead of the original mode.
func TestWriteRuntimeBrokerInstances_KeepsMode0600(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640} {
		dir, path := writeWriterTestSettings(t, mode)
		require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
		fi, err := os.Lstat(path)
		require.NoError(t, err)
		assert.Equal(t, mode, fi.Mode().Perm(), "apply keeps the settings file mode")

		require.NoError(t, RemoveRuntimeBrokerInstances(dir, writerTestInstances()))
		fi, err = os.Lstat(path)
		require.NoError(t, err)
		assert.Equal(t, mode, fi.Mode().Perm(), "rollback keeps the settings file mode")
	}
}

func TestWriteRuntimeBrokerInstances_RefusesNonEmptyDifferentList(t *testing.T) {
	dir, path := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	orig, _ := os.ReadFile(path)

	other := writerTestInstances()
	other[0].Name = "renamed"
	err := WriteRuntimeBrokerInstances(dir, other)
	var mm *InstancesMismatchError
	require.ErrorAs(t, err, &mm)
	assert.Equal(t, []string{"docker"}, mm.Keys)
	after, _ := os.ReadFile(path)
	assert.Equal(t, orig, after, "a refused write changes nothing")
}

func TestRemoveRuntimeBrokerInstances_InverseKeepsOtherChanges(t *testing.T) {
	dir, path := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))

	// An unrelated change the operator made after apply survives.
	data, _ := os.ReadFile(path)
	data = []byte(strings.Replace(string(data), "endpoint: https://hub.example.test", "endpoint: https://hub2.example.test", 1))
	require.NoError(t, os.WriteFile(path, data, 0o600))

	require.NoError(t, RemoveRuntimeBrokerInstances(dir, writerTestInstances()))
	data, _ = os.ReadFile(path)
	assert.Contains(t, string(data), "hub2.example.test")
	assert.NotContains(t, string(data), "instances")
	got, err := strictInstancesFromYAML(data)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Absent: no-op.
	require.NoError(t, RemoveRuntimeBrokerInstances(dir, writerTestInstances()))
}

func TestRemoveRuntimeBrokerInstances_RefusesChangedList(t *testing.T) {
	dir, _ := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	expected := writerTestInstances()
	expected[0].Name = "something-else"
	err := RemoveRuntimeBrokerInstances(dir, expected)
	var mm *InstancesMismatchError
	require.ErrorAs(t, err, &mm)
	assert.Equal(t, []string{"docker"}, mm.Keys)
}

func TestWriteRuntimeBrokerInstances_RefusesLinksAndOtherFiles(t *testing.T) {
	t.Run("symlink at the settings path", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(t.TempDir(), "real.yaml")
		require.NoError(t, os.WriteFile(real, []byte(writerTestSettings), 0o600))
		require.NoError(t, os.Symlink(real, filepath.Join(dir, "settings.yaml")))
		require.Error(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
		data, _ := os.ReadFile(real)
		assert.Equal(t, writerTestSettings, string(data))
	})
	t.Run("hard link", func(t *testing.T) {
		dir, path := writeWriterTestSettings(t, 0o600)
		require.NoError(t, os.Link(path, filepath.Join(t.TempDir(), "other")))
		require.Error(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	})
	t.Run("settings.json", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"schema_version":"1"}`), 0o600))
		assert.ErrorIs(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()), ErrInstancesSettingsJSON)
	})
	t.Run("settings.yml", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yml"), []byte(writerTestSettings), 0o600))
		assert.ErrorIs(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()), ErrInstancesSettingsNotYAML)
	})
	t.Run("not schema_version 1", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("hub:\n  endpoint: x\n"), 0o600))
		assert.ErrorIs(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()), ErrInstancesSettingsNotV1)
	})
}

func TestWriteRuntimeBrokerInstances_NoTempFileLeft(t *testing.T) {
	dir, _ := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-")
	}
}

// Design erratum E1: the explicit writers run while the no-write guard is
// on; the guard stops read-path rewrites only.
func TestWriteRuntimeBrokerInstances_SucceedsUnderNoWriteGuard(t *testing.T) {
	writesonread.IsolateForTest(t)
	writesonread.Set()
	require.True(t, writesonread.Disabled())
	dir, _ := writeWriterTestSettings(t, 0o600)
	require.NoError(t, WriteRuntimeBrokerInstances(dir, writerTestInstances()))
	require.NoError(t, RemoveRuntimeBrokerInstances(dir, writerTestInstances()))
}
