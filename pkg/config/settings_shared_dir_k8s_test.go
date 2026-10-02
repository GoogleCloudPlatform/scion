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

package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Tests for the Kubernetes shared-dir PVC defaults (shared_dir_storage_class,
// shared_dir_size) on settings runtime and profile entries (ptone/scion#2634).

const sharedDirK8sSettingsYAML = `schema_version: "1"
active_profile: gke
runtimes:
  gke-autopilot:
    type: kubernetes
    namespace: scion-agents
    shared_dir_storage_class: standard-rwx
    shared_dir_size: 1Ti
profiles:
  gke:
    runtime: gke-autopilot
    shared_dir_size: 2Ti
`

// writeSharedDirK8sGlobalSettings writes content as the global settings file
// under a temp HOME and returns a project .scion dir to load from.
func writeSharedDirK8sGlobalSettings(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(content), 0644))
	projectDir := filepath.Join(tmpDir, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	return projectDir
}

// The keys parse from settings.yaml onto both the runtime and the profile
// entry, and are not reported as unrecognized keys.
func TestSharedDirK8sSettings_ParseFromFile_NoUnknownKeyWarning(t *testing.T) {
	resetWarnedUnusedKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	projectDir := writeSharedDirK8sGlobalSettings(t, sharedDirK8sSettingsYAML)
	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	rt := vs.Runtimes["gke-autopilot"]
	assert.Equal(t, "standard-rwx", rt.SharedDirStorageClass)
	assert.Equal(t, "1Ti", rt.SharedDirSize)
	p := vs.Profiles["gke"]
	assert.Equal(t, "", p.SharedDirStorageClass)
	assert.Equal(t, "2Ti", p.SharedDirSize)

	// LoadSettings carries the unrecognized-key check; it must not flag the
	// new keys (the legacy decode misses them, the v1 decode must not).
	_, err = LoadSettings(projectDir)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "shared_dir_",
		"shared-dir keys must be recognized, not warned about: %s", buf.String())
}

// A misspelled key on a runtime entry is still reported through the
// existing unrecognized-key warning (not silently dropped).
func TestSharedDirK8sSettings_TypoOnRuntimeEntry_Warns(t *testing.T) {
	resetWarnedUnusedKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	projectDir := writeSharedDirK8sGlobalSettings(t, `schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_clas: standard-rwx
`)
	// LoadSettings is the loader that carries the unrecognized-key check
	// (it runs on every CLI invocation and at broker startup).
	_, err := LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "shared_dir_storage_clas")
}

// A settings file without schema_version that uses the keys on a runtime
// entry is detected as v1, so the legacy loader (whose RuntimeConfig lacks
// the fields) does not drop them.
func TestSharedDirK8sSettings_NoSchemaVersion_DetectedAsV1(t *testing.T) {
	for _, key := range []string{"shared_dir_storage_class", "shared_dir_size"} {
		t.Run(key, func(t *testing.T) {
			data := []byte("runtimes:\n  k8s:\n    " + key + ": x\n")
			version, isLegacy := DetectSettingsFormat(data)
			assert.Equal(t, "1", version)
			assert.False(t, isLegacy)
		})
	}

	projectDir := writeSharedDirK8sGlobalSettings(t, `runtimes:
  k8s:
    shared_dir_storage_class: standard-rwx
profiles:
  k8s:
    runtime: k8s
`)
	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "standard-rwx", vs.Runtimes["k8s"].SharedDirStorageClass)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "shared_dir_storage_class")
}

// The keys survive a YAML and a JSON marshal/unmarshal round trip on both
// entry types (the DB overlay and the admin API carry them as JSON).
func TestSharedDirK8sSettings_RoundTrip(t *testing.T) {
	in := VersionedSettings{
		SchemaVersion: "1",
		Runtimes: map[string]V1RuntimeConfig{
			"k8s": {Type: "kubernetes", SharedDirStorageClass: "standard-rwx", SharedDirSize: "1Ti"},
		},
		Profiles: map[string]V1ProfileConfig{
			"gke": {Runtime: "k8s", SharedDirStorageClass: "premium-rwx", SharedDirSize: "2560Gi"},
		},
	}

	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(y), "shared_dir_storage_class: standard-rwx")
	var fromYAML VersionedSettings
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))

	j, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(j), `"shared_dir_storage_class":"premium-rwx"`)
	var fromJSON VersionedSettings
	require.NoError(t, json.Unmarshal(j, &fromJSON))

	for name, got := range map[string]VersionedSettings{"yaml": fromYAML, "json": fromJSON} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, in.Runtimes["k8s"], got.Runtimes["k8s"])
			assert.Equal(t, in.Profiles["gke"], got.Profiles["gke"])
		})
	}
}

// The embedded JSON schema accepts the keys on runtime and profile entries
// (both use additionalProperties: false).
func TestSharedDirK8sSettings_SchemaAccepts(t *testing.T) {
	errs, err := ValidateSettings([]byte(sharedDirK8sSettingsYAML+`    shared_dir_storage_class: premium-rwx
`), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)
}

// `scion config get` reads the keys through the generic scalar lookup.
func TestSharedDirK8sSettings_ConfigGet(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"k8s": {SharedDirStorageClass: "standard-rwx"}},
		Profiles: map[string]V1ProfileConfig{"gke": {Runtime: "k8s", SharedDirSize: "1Ti"}},
	}
	got, err := GetVersionedSettingValue(vs, "runtimes.k8s.shared_dir_storage_class")
	require.NoError(t, err)
	assert.Equal(t, "standard-rwx", got)
	got, err = GetVersionedSettingValue(vs, "profiles.gke.shared_dir_size")
	require.NoError(t, err)
	assert.Equal(t, "1Ti", got)
}

// Precedence among the settings tiers: profile wins over runtime, per field;
// the active profile is used when no profile is named.
func TestResolveSharedDirDefaults_Precedence(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "both",
		Runtimes: map[string]V1RuntimeConfig{
			"k8s":   {Type: "kubernetes", SharedDirStorageClass: "rt-class", SharedDirSize: "1Gi"},
			"plain": {Type: "kubernetes"},
		},
		Profiles: map[string]V1ProfileConfig{
			"both":         {Runtime: "k8s", SharedDirStorageClass: "prof-class", SharedDirSize: "2Gi"},
			"class-only":   {Runtime: "k8s", SharedDirStorageClass: "prof-class"},
			"none":         {Runtime: "k8s"},
			"plain":        {Runtime: "plain"},
			"missing-rt":   {Runtime: "nope", SharedDirSize: "3Gi"},
			"profile-only": {Runtime: "plain", SharedDirStorageClass: "prof-class"},
		},
	}
	tests := []struct {
		profile, wantClass, wantSize string
	}{
		{"both", "prof-class", "2Gi"},
		{"", "prof-class", "2Gi"}, // active profile
		{"class-only", "prof-class", "1Gi"},
		{"none", "rt-class", "1Gi"},
		{"plain", "", ""},
		{"missing-rt", "", "3Gi"},
		{"profile-only", "prof-class", ""},
		{"unknown", "", ""},
	}
	for _, tt := range tests {
		t.Run("profile="+tt.profile, func(t *testing.T) {
			c, s := vs.ResolveSharedDirDefaults(tt.profile)
			assert.Equal(t, tt.wantClass, c)
			assert.Equal(t, tt.wantSize, s)
		})
	}

	var nilVS *VersionedSettings
	c, s := nilVS.ResolveSharedDirDefaults("x")
	assert.Empty(t, c)
	assert.Empty(t, s)
}

// The template/agent kubernetes block wins over settings defaults, per field,
// and the input is never mutated.
func TestApplySharedDirDefaults(t *testing.T) {
	t.Run("nil base, no defaults stays nil", func(t *testing.T) {
		assert.Nil(t, ApplySharedDirDefaults(nil, "", ""))
	})
	t.Run("nil base gets defaults", func(t *testing.T) {
		got := ApplySharedDirDefaults(nil, "standard-rwx", "1Ti")
		require.NotNil(t, got)
		assert.Equal(t, "standard-rwx", got.SharedDirStorageClass)
		assert.Equal(t, "1Ti", got.SharedDirSize)
	})
	t.Run("explicit template values win per field", func(t *testing.T) {
		base := &api.KubernetesConfig{SharedDirStorageClass: "tpl-class", Namespace: "ns"}
		got := ApplySharedDirDefaults(base, "standard-rwx", "1Ti")
		assert.Equal(t, "tpl-class", got.SharedDirStorageClass)
		assert.Equal(t, "1Ti", got.SharedDirSize)
		assert.Equal(t, "ns", got.Namespace)
		// base untouched
		assert.Equal(t, "", base.SharedDirSize)
		assert.NotSame(t, base, got)
	})
}

// The settings key the size came from is reported with the resolved size.
func TestResolveSharedDirDefaultsWithSource(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"k8s": {SharedDirSize: "1Gi"}},
		Profiles: map[string]V1ProfileConfig{
			"own":     {Runtime: "k8s", SharedDirSize: "2Gi"},
			"inherit": {Runtime: "k8s", SharedDirStorageClass: "c"},
			"none":    {Runtime: "missing"},
		},
	}
	for profile, want := range map[string][2]string{
		"own":     {"2Gi", "profiles.own.shared_dir_size"},
		"inherit": {"1Gi", "runtimes.k8s.shared_dir_size"},
		"none":    {"", ""},
	} {
		_, size, key := vs.ResolveSharedDirDefaultsWithSource(profile)
		assert.Equal(t, want[0], size, profile)
		assert.Equal(t, want[1], key, profile)
	}
}

// shared_dir_size must be a Kubernetes quantity; errors name the key.
func TestValidateSharedDirSizes(t *testing.T) {
	assert.NoError(t, ValidateSharedDirSize(""))
	assert.NoError(t, ValidateSharedDirSize("10Gi"))
	assert.NoError(t, ValidateSharedDirSize("1Ti"))
	assert.Error(t, ValidateSharedDirSize("1TB"))
	for _, v := range []string{"0", "-1Gi"} {
		err := ValidateSharedDirSize(v)
		require.Error(t, err, v)
		assert.Contains(t, err.Error(), "positive", v)
	}

	errs := ValidateSharedDirSizes(
		map[string]V1RuntimeConfig{"ok": {SharedDirSize: "1Ti"}, "bad": {SharedDirSize: "1TB"}},
		map[string]V1ProfileConfig{"p": {SharedDirSize: "lots"}, "q": {}},
	)
	require.Len(t, errs, 2)
	assert.Equal(t, "profiles.p.shared_dir_size", errs[0].Path)
	assert.Equal(t, "runtimes.bad.shared_dir_size", errs[1].Path)
	assert.Contains(t, errs[1].Error(), `"1TB"`)
}

// ValidateSettings (used by scion config validate) reports an invalid
// shared_dir_size with its key.
func TestSharedDirK8sSettings_ValidateSettingsRejectsBadSize(t *testing.T) {
	data := strings.Replace(sharedDirK8sSettingsYAML, "shared_dir_size: 2Ti", "shared_dir_size: 2TB", 1)
	errs, err := ValidateSettings([]byte(data), "1")
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Equal(t, "profiles.gke.shared_dir_size", errs[0].Path)
}
