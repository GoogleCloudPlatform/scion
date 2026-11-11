/*
Copyright 2026 The Scion Authors.
*/

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeKeyWithSequenceKeyYAML is a mapping with a "<<" merge key next to a
// non-scalar mapping key. The YAML decoder panics on it when decoding into
// a map.
const mergeKeyWithSequenceKeyYAML = "<<: {}\n? [a]\n: c\n"

func TestUnmarshalSettingsYAML_DecoderPanicIsError(t *testing.T) {
	var out map[string]interface{}
	var err error
	require.NotPanics(t, func() { err = unmarshalSettingsYAML([]byte(mergeKeyWithSequenceKeyYAML), &out) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid YAML")
	assert.NotContains(t, err.Error(), "goroutine", "the error must not carry a stack")
}

// writeMergeKeySettings writes a v1 settings file containing the merge-key
// shape under a temp HOME and returns the project .scion dir.
func writeMergeKeySettings(t *testing.T, global bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	dir := projectDir
	if global {
		dir = globalDir
	}
	content := "schema_version: \"1\"\n" + mergeKeyWithSequenceKeyYAML
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
	return projectDir
}

func TestLoadSingleFileVersioned_MergeKeyPanicIsParseError(t *testing.T) {
	projectDir := writeMergeKeySettings(t, false)
	var err error
	require.NotPanics(t, func() { _, err = LoadSingleFileVersioned(projectDir) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse YAML settings at")
}

func TestUpdateVersionedSetting_MergeKeyPanicIsParseError(t *testing.T) {
	projectDir := writeMergeKeySettings(t, false)
	before, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)
	require.NotPanics(t, func() { err = UpdateVersionedSetting(projectDir, "default_template", "x") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse YAML settings at")
	after, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a file that does not parse is not rewritten")
}

// The layered loader reads files through the koanf parser, which reports
// this shape as an error; this pins that it never panics.
func TestLoadVersionedSettings_MergeKeyPanicIsError(t *testing.T) {
	for _, global := range []bool{false, true} {
		name := "project"
		if global {
			name = "global"
		}
		t.Run(name, func(t *testing.T) {
			projectDir := writeMergeKeySettings(t, global)
			var err error
			require.NotPanics(t, func() { _, err = LoadVersionedSettings(projectDir) })
			require.Error(t, err)
		})
	}
}

func TestDetectSettingsFormat_MergeKeyPanicIsUnparseable(t *testing.T) {
	data := []byte("schema_version: \"1\"\n" + mergeKeyWithSequenceKeyYAML)
	var version string
	var isLegacy bool
	require.NotPanics(t, func() { version, isLegacy = DetectSettingsFormat(data) })
	assert.Equal(t, "", version)
	assert.False(t, isLegacy)
}

func TestValidateSettings_MergeKeyPanicIsError(t *testing.T) {
	data := []byte("schema_version: \"1\"\n" + mergeKeyWithSequenceKeyYAML)
	var err error
	require.NotPanics(t, func() { _, err = ValidateSettings(data, "1") })
	require.Error(t, err)
}

func TestUpdateSettingGlobal_MergeKeyPanicIsError(t *testing.T) {
	writeMergeKeySettings(t, true)
	var err error
	require.NotPanics(t, func() { err = UpdateSetting("", "default_template", "x", true) })
	require.Error(t, err)
}

func TestMigrateSettingsFile_MergeKeyPanicIsError(t *testing.T) {
	projectDir := writeMergeKeySettings(t, false)
	var err error
	require.NotPanics(t, func() { _, err = MigrateSettingsFile(projectDir, true) })
	require.Error(t, err)
}
