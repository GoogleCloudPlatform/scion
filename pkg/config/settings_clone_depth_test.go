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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for clone_depth on settings profiles and on templates/agents.

const cloneDepthSettingsYAML = `schema_version: "1"
active_profile: gke
runtimes:
  k8s:
    type: kubernetes
profiles:
  gke:
    runtime: k8s
    clone_depth: full
  gke-50:
    runtime: k8s
    clone_depth: 50
  plain:
    runtime: k8s
`

func TestCloneDepthSettings_ParseFromFile_NoUnknownKeyWarning(t *testing.T) {
	resetWarnedUnusedKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	projectDir := writeSharedDirK8sGlobalSettings(t, cloneDepthSettingsYAML)
	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	assert.Equal(t, api.CloneDepth("full"), vs.Profiles["gke"].CloneDepth)
	assert.Equal(t, api.CloneDepth("50"), vs.Profiles["gke-50"].CloneDepth)
	assert.Equal(t, api.CloneDepth(""), vs.Profiles["plain"].CloneDepth)

	_, err = LoadSettings(projectDir)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "clone_depth",
		"clone_depth must be recognized, not warned about: %s", buf.String())
}

// The value survives a JSON round trip (the DB overlay and admin API
// carry JSON).
func TestCloneDepthSettings_JSONRoundTrip(t *testing.T) {
	in := VersionedSettings{
		SchemaVersion: "1",
		Profiles: map[string]V1ProfileConfig{
			"gke":  {Runtime: "k8s", CloneDepth: "full"},
			"deep": {Runtime: "k8s", CloneDepth: "50"},
		},
	}
	j, err := json.Marshal(in)
	require.NoError(t, err)
	var out VersionedSettings
	require.NoError(t, json.Unmarshal(j, &out))
	assert.Equal(t, in.Profiles, out.Profiles)

	// A bare JSON integer decodes too.
	var fromInt VersionedSettings
	require.NoError(t, json.Unmarshal([]byte(`{"profiles":{"p":{"runtime":"k8s","clone_depth":7}}}`), &fromInt))
	assert.Equal(t, api.CloneDepth("7"), fromInt.Profiles["p"].CloneDepth)
}

func TestCloneDepthSettings_Schema(t *testing.T) {
	errs, err := ValidateSettings([]byte(cloneDepthSettingsYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	for _, bad := range []string{"0", "-1", "shallow", "\"0\"", "true"} {
		doc := "schema_version: \"1\"\nprofiles:\n  p:\n    runtime: k8s\n    clone_depth: " + bad + "\n"
		errs, err := ValidateSettings([]byte(doc), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "clone_depth %s must be rejected", bad)
	}
}

func TestCloneDepthAgentSchema(t *testing.T) {
	for _, ok := range []string{
		`{"clone_depth": "full"}`,
		`{"clone_depth": 10}`,
		`{"clone_depth": "10"}`,
	} {
		errs, err := ValidateAgentConfig([]byte(ok), "1")
		require.NoError(t, err)
		assert.Empty(t, errs, ok)
	}
	for _, bad := range []string{`{"clone_depth": 0}`, `{"clone_depth": "shallow"}`} {
		errs, err := ValidateAgentConfig([]byte(bad), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, bad)
	}
}

func TestResolveCloneDepthWithSource(t *testing.T) {
	projectDir := writeSharedDirK8sGlobalSettings(t, cloneDepthSettingsYAML)
	vs, _, err := LoadEffectiveSettings(projectDir)
	require.NoError(t, err)

	tests := []struct {
		profile    string
		want       api.CloneDepth
		wantSource string
	}{
		{profile: "gke", want: "full", wantSource: "profiles.gke.clone_depth"},
		{profile: "gke-50", want: "50", wantSource: "profiles.gke-50.clone_depth"},
		{profile: "plain", want: "", wantSource: ""},
		{profile: "missing", want: "", wantSource: ""},
		// Empty name uses the active profile (gke).
		{profile: "", want: "full", wantSource: "profiles.gke.clone_depth"},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			got, source := vs.ResolveCloneDepthWithSource(tt.profile)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSource, source)
		})
	}

	var nilVS *VersionedSettings
	got, source := nilVS.ResolveCloneDepthWithSource("gke")
	assert.Empty(t, got)
	assert.Empty(t, source)
}

// A child template's clone_depth wins over its parent's; an unset child
// keeps the parent's value.
func TestMergeScionConfig_CloneDepth(t *testing.T) {
	base := &api.ScionConfig{CloneDepth: "full"}
	assert.Equal(t, api.CloneDepth("5"), MergeScionConfig(base, &api.ScionConfig{CloneDepth: "5"}).CloneDepth)
	assert.Equal(t, api.CloneDepth("full"), MergeScionConfig(base, &api.ScionConfig{}).CloneDepth)
}

func TestTemplateLoadConfig_CloneDepth(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          api.CloneDepth
		wantErr       bool
	}{
		{name: "full", content: "clone_depth: full\n", want: "full"},
		{name: "integer", content: "clone_depth: 20\n", want: "20"},
		{name: "unset", content: "harness: claude\n", want: ""},
		{name: "zero rejected", content: "clone_depth: 0\n", wantErr: true},
		{name: "word rejected", content: "clone_depth: shallow\n", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(tt.content), 0644))
			cfg, err := (&Template{Path: dir}).LoadConfig()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "clone_depth")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.CloneDepth)
		})
	}
}
