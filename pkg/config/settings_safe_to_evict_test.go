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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Tests for the Kubernetes safe_to_evict setting on settings runtime and
// profile entries and kubernetes.safeToEvict on templates and agents.

const safeToEvictSettingsYAML = `schema_version: "1"
active_profile: gke
runtimes:
  gke-autopilot:
    type: kubernetes
    safe_to_evict: false
  docker:
    type: docker
profiles:
  gke:
    runtime: gke-autopilot
  gke-evictable:
    runtime: gke-autopilot
    safe_to_evict: true
  local:
    runtime: docker
`

func TestSafeToEvictSettings_ParseFromFile_NoUnknownKeyWarning(t *testing.T) {
	resetWarnedUnusedKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	projectDir := writeSharedDirK8sGlobalSettings(t, safeToEvictSettingsYAML)
	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	require.NotNil(t, vs.Runtimes["gke-autopilot"].SafeToEvict)
	assert.False(t, *vs.Runtimes["gke-autopilot"].SafeToEvict)
	assert.Nil(t, vs.Runtimes["docker"].SafeToEvict)
	assert.Nil(t, vs.Profiles["gke"].SafeToEvict)
	require.NotNil(t, vs.Profiles["gke-evictable"].SafeToEvict)
	assert.True(t, *vs.Profiles["gke-evictable"].SafeToEvict)

	_, err = LoadSettings(projectDir)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "safe_to_evict",
		"safe_to_evict must be recognized, not warned about: %s", buf.String())
}

// A settings file without schema_version that sets safe_to_evict on a
// runtime entry is loaded as v1 so the key is not dropped.
func TestSafeToEvictSettings_NoSchemaVersion_DetectedAsV1(t *testing.T) {
	version, isLegacy := DetectSettingsFormat([]byte("runtimes:\n  k8s:\n    safe_to_evict: false\n"))
	assert.Equal(t, "1", version)
	assert.False(t, isLegacy)

	projectDir := writeSharedDirK8sGlobalSettings(t, "runtimes:\n  k8s:\n    safe_to_evict: false\nprofiles:\n  k8s:\n    runtime: k8s\n")
	vs, warnings, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Runtimes["k8s"].SafeToEvict)
	assert.False(t, *vs.Runtimes["k8s"].SafeToEvict)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "safe_to_evict")
}

// false and true both survive YAML and JSON round trips (the DB overlay and
// admin API carry JSON); unset stays unset and is omitted.
func TestSafeToEvictSettings_RoundTrip(t *testing.T) {
	in := VersionedSettings{
		SchemaVersion: "1",
		Runtimes: map[string]V1RuntimeConfig{
			"k8s":   {Type: "kubernetes", SafeToEvict: boolPtr(false)},
			"unset": {Type: "kubernetes"},
		},
		Profiles: map[string]V1ProfileConfig{
			"gke": {Runtime: "k8s", SafeToEvict: boolPtr(true)},
		},
	}
	y, err := yaml.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(y), "safe_to_evict: false")
	assert.Contains(t, string(y), "safe_to_evict: true")
	var fromYAML VersionedSettings
	require.NoError(t, yaml.Unmarshal(y, &fromYAML))

	j, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(j), `"safe_to_evict":false`)
	var fromJSON VersionedSettings
	require.NoError(t, json.Unmarshal(j, &fromJSON))

	for name, got := range map[string]VersionedSettings{"yaml": fromYAML, "json": fromJSON} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, in.Runtimes["k8s"], got.Runtimes["k8s"])
			assert.Equal(t, in.Profiles["gke"], got.Profiles["gke"])
			assert.Nil(t, got.Runtimes["unset"].SafeToEvict)
		})
	}
}

func TestSafeToEvictSettings_Schema(t *testing.T) {
	errs, err := ValidateSettings([]byte(safeToEvictSettingsYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	for _, bad := range []string{
		"schema_version: \"1\"\nruntimes:\n  k8s:\n    type: kubernetes\n    safe_to_evict: \"false\"\n",
		"schema_version: \"1\"\nprofiles:\n  p:\n    runtime: k8s\n    safe_to_evict: 0\n",
	} {
		errs, err := ValidateSettings([]byte(bad), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "non-boolean safe_to_evict must be rejected: %s", bad)
	}
}

func TestSafeToEvictAgentSchema(t *testing.T) {
	for _, ok := range []string{
		`{"kubernetes": {"safeToEvict": false}}`,
		`{"kubernetes": {"safeToEvict": true}}`,
	} {
		errs, err := ValidateAgentConfig([]byte(ok), "1")
		require.NoError(t, err)
		assert.Empty(t, errs, ok)
	}
	errs, err := ValidateAgentConfig([]byte(`{"kubernetes": {"safeToEvict": "false"}}`), "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errs)
}

// Precedence among settings tiers: profile, then runtime entry; the active
// profile is used when none is named; the source names the key.
func TestResolveSafeToEvictWithSource(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "gke",
		Runtimes: map[string]V1RuntimeConfig{
			"rt-false": {Type: "kubernetes", SafeToEvict: boolPtr(false)},
			"rt-unset": {Type: "kubernetes"},
		},
		Profiles: map[string]V1ProfileConfig{
			"gke":        {Runtime: "rt-false"},
			"prof-true":  {Runtime: "rt-false", SafeToEvict: boolPtr(true)},
			"prof-false": {Runtime: "rt-unset", SafeToEvict: boolPtr(false)},
			"none":       {Runtime: "rt-unset"},
			"dangling":   {Runtime: "missing"},
		},
	}
	tests := []struct {
		profile    string
		want       *bool
		wantSource string
	}{
		{"", boolPtr(false), "runtimes.rt-false.safe_to_evict"},
		{"gke", boolPtr(false), "runtimes.rt-false.safe_to_evict"},
		{"prof-true", boolPtr(true), "profiles.prof-true.safe_to_evict"},
		{"prof-false", boolPtr(false), "profiles.prof-false.safe_to_evict"},
		{"none", nil, ""},
		{"dangling", nil, ""},
		{"unknown", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			got, source := vs.ResolveSafeToEvictWithSource(tt.profile)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSource, source)
			assert.Equal(t, tt.want, vs.ResolveSafeToEvict(tt.profile))
		})
	}

	var nilVS *VersionedSettings
	got, source := nilVS.ResolveSafeToEvictWithSource("x")
	assert.Nil(t, got)
	assert.Empty(t, source)
}

// The resolver returns a fresh pointer; writing through it does not change
// the settings.
func TestResolveSafeToEvict_ReturnsCopy(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"k8s": {SafeToEvict: boolPtr(false)}},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "k8s"}, "q": {Runtime: "k8s", SafeToEvict: boolPtr(false)}},
	}
	for _, p := range []string{"p", "q"} {
		got := vs.ResolveSafeToEvict(p)
		require.NotNil(t, got)
		*got = true
	}
	assert.False(t, *vs.Runtimes["k8s"].SafeToEvict)
	assert.False(t, *vs.Profiles["q"].SafeToEvict)
}

func TestApplySafeToEvictDefault(t *testing.T) {
	// nil default: base returned as is.
	assert.Nil(t, ApplySafeToEvictDefault(nil, nil))
	base := &api.KubernetesConfig{Namespace: "ns"}
	assert.Same(t, base, ApplySafeToEvictDefault(base, nil))

	// nil base gets a new block carrying the default.
	got := ApplySafeToEvictDefault(nil, boolPtr(false))
	require.NotNil(t, got)
	require.NotNil(t, got.SafeToEvict)
	assert.False(t, *got.SafeToEvict)

	// unset on base: filled from the default, base untouched.
	got = ApplySafeToEvictDefault(base, boolPtr(false))
	require.NotNil(t, got.SafeToEvict)
	assert.False(t, *got.SafeToEvict)
	assert.Equal(t, "ns", got.Namespace)
	assert.Nil(t, base.SafeToEvict, "base must not be modified")
	assert.NotSame(t, base, got)

	// explicit template/agent true wins over a settings false.
	tmpl := &api.KubernetesConfig{SafeToEvict: boolPtr(true)}
	got = ApplySafeToEvictDefault(tmpl, boolPtr(false))
	assert.True(t, *got.SafeToEvict)
	assert.True(t, *tmpl.SafeToEvict)

	// explicit template/agent false wins over a settings true.
	tmpl = &api.KubernetesConfig{SafeToEvict: boolPtr(false)}
	got = ApplySafeToEvictDefault(tmpl, boolPtr(true))
	assert.False(t, *got.SafeToEvict)

	// the default pointer is not aliased into the result.
	def := boolPtr(false)
	got = ApplySafeToEvictDefault(nil, def)
	*got.SafeToEvict = true
	assert.False(t, *def)
}

func TestSafeToEvictIgnoredWarnings(t *testing.T) {
	runtimes := map[string]V1RuntimeConfig{
		"gke":        {Type: "kubernetes", SafeToEvict: boolPtr(false)},
		"k8s":        {SafeToEvict: boolPtr(false)},
		"docker":     {Type: "docker", SafeToEvict: boolPtr(false)},
		"podman":     {},
		"cloudrun-x": {Type: "cloudrun"},
		"remote-k8s": {Type: "remote", SafeToEvict: boolPtr(false)},
		"remote":     {SafeToEvict: boolPtr(true)},
	}
	profiles := map[string]V1ProfileConfig{
		"on-gke":     {Runtime: "gke", SafeToEvict: boolPtr(false)},
		"on-podman":  {Runtime: "podman", SafeToEvict: boolPtr(true)},
		"on-cr":      {Runtime: "cloudrun-x"},
		"on-missing": {Runtime: "missing", SafeToEvict: boolPtr(false)},
		"on-remote":  {Runtime: "remote-k8s", SafeToEvict: boolPtr(true)},
	}
	got := SafeToEvictIgnoredWarnings(runtimes, profiles)
	require.Len(t, got, 2, "%v", got)
	assert.Contains(t, got[0], "profiles.on-podman.safe_to_evict")
	assert.Contains(t, got[1], "runtimes.docker.safe_to_evict")

	w := SettingsWarnings([]byte(safeToEvictSettingsYAML+"    safe_to_evict: false\n"), "1")
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "profiles.local.safe_to_evict")
	assert.Empty(t, SettingsWarnings([]byte(safeToEvictSettingsYAML), "1"))
	assert.Empty(t, SettingsWarnings([]byte(safeToEvictSettingsYAML), "2"))
}

// The template/agent merge carries safeToEvict: an override wins (true or
// false), an unset override keeps the base, and the result does not alias
// the override's pointer.
func TestMergeKubernetesConfig_SafeToEvict(t *testing.T) {
	base := &api.KubernetesConfig{Namespace: "ns", SafeToEvict: boolPtr(false)}

	got := mergeKubernetesConfig(base, &api.KubernetesConfig{Context: "c"})
	require.NotNil(t, got.SafeToEvict)
	assert.False(t, *got.SafeToEvict)

	override := &api.KubernetesConfig{SafeToEvict: boolPtr(true)}
	got = mergeKubernetesConfig(base, override)
	assert.True(t, *got.SafeToEvict)
	assert.False(t, *base.SafeToEvict)
	*got.SafeToEvict = false
	assert.True(t, *override.SafeToEvict, "merged result must not alias the override")

	got = mergeKubernetesConfig(nil, &api.KubernetesConfig{SafeToEvict: boolPtr(false)})
	require.NotNil(t, got.SafeToEvict)
	assert.False(t, *got.SafeToEvict)

	// Through MergeScionConfig (template, then agent config).
	merged := MergeScionConfig(
		&api.ScionConfig{Kubernetes: &api.KubernetesConfig{SafeToEvict: boolPtr(false)}},
		&api.ScionConfig{Kubernetes: &api.KubernetesConfig{Namespace: "agents"}},
	)
	require.NotNil(t, merged.Kubernetes)
	require.NotNil(t, merged.Kubernetes.SafeToEvict)
	assert.False(t, *merged.Kubernetes.SafeToEvict)
	assert.Equal(t, "agents", merged.Kubernetes.Namespace)
}

// The DB-backed settings overlay (co-located hub and broker) carries
// safe_to_evict on runtime and profile entries into LoadEffectiveSettings,
// replacing the file's entries, and the resolver sees it.
func TestSafeToEvictSettings_DBOverlay(t *testing.T) {
	projectDir := writeSharedDirK8sGlobalSettings(t, safeToEvictSettingsYAML)
	o := NewSettingsOverlay()
	o.Update(
		map[string]V1RuntimeConfig{"db-k8s": {Type: "kubernetes", SafeToEvict: boolPtr(false)}},
		map[string]V1ProfileConfig{
			"db":      {Runtime: "db-k8s"},
			"db-true": {Runtime: "db-k8s", SafeToEvict: boolPtr(true)},
		},
		nil, "")
	SetGlobalSettingsOverlay(o)
	t.Cleanup(func() { SetGlobalSettingsOverlay(nil) })

	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)
	got, source := vs.ResolveSafeToEvictWithSource("db")
	require.NotNil(t, got)
	assert.False(t, *got)
	assert.Equal(t, "runtimes.db-k8s.safe_to_evict", source)
	got, source = vs.ResolveSafeToEvictWithSource("db-true")
	require.NotNil(t, got)
	assert.True(t, *got)
	assert.Equal(t, "profiles.db-true.safe_to_evict", source)
	_, fileEntry := vs.Runtimes["gke-autopilot"]
	assert.False(t, fileEntry, "overlay runtimes replace the file's")
}
