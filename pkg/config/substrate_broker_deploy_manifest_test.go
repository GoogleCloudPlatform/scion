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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func loadExpandedSubstrateManifestWith(t *testing.T, overrides map[string]string, relParts ...string) []byte {
	t.Helper()
	parts := append([]string{"..", ".."}, relParts...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	require.NoError(t, err)
	return []byte(os.Expand(string(raw), func(name string) string {
		if v, ok := overrides[name]; ok {
			return v
		}
		return "placeholder"
	}))
}

func loadExpandedSubstrateManifest(t *testing.T, relParts ...string) []byte {
	t.Helper()
	return loadExpandedSubstrateManifestWith(t, nil, relParts...)
}

// TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles loads the
// settings.yaml embedded in the REAL deploy/substrate/broker.yaml (envsubst
// placeholders filled with a dummy value) on top of the embedded defaults,
// and requires every profile to resolve to a substrate runtime. Unlike
// substrate_broker_profiles_test.go's
// substrateBrokerProfilesOverride (a hand-maintained copy of the same
// stanza, which can drift from the manifest silently), this fails if the
// manifest's local/remote repoint is ever removed or edited incorrectly.
func TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "broker.yaml")

	var settings string
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "ConfigMap" && doc.Metadata.Name == "scion-substrate-broker-settings" {
			settings = doc.Data["settings.yaml"]
		}
	}
	require.NotEmpty(t, settings, "scion-substrate-broker-settings ConfigMap settings.yaml not found in broker.yaml")

	valErrors, err := ValidateSettings([]byte(settings), "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "broker ConfigMap settings.yaml should validate against v1 schema, got: %v", valErrors)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), []byte(settings), 0o644))

	vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
	require.NoError(t, err)
	require.NotEmpty(t, vs.Profiles)
	for name, p := range vs.Profiles {
		rt, ok := vs.Runtimes[p.Runtime]
		if !ok || rt.Type != "substrate" {
			t.Errorf("profile %q -> runtime %q (type %q), want a substrate runtime", name, p.Runtime, rt.Type)
		}
	}
	require.Equal(t, "substrate-prod", vs.Profiles["local"].Runtime)
	require.Equal(t, "substrate-prod", vs.Profiles["remote"].Runtime)

	sub := vs.Runtimes["substrate-prod"].Substrate
	require.NotNil(t, sub)
	assert.Empty(t, sub.SandboxClass)
	assert.Empty(t, sub.SandboxConfigName)
}

func TestDeploySubstrateBrokerYAML_NamespaceAPIAccessLabel(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "broker.yaml")

	var foundNS bool
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name   string            `yaml:"name"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "Namespace" {
			foundNS = true
			assert.Equal(t, "true", doc.Metadata.Labels["ate.dev/api-access"], "broker Namespace must carry ate.dev/api-access=true for Substrate v1")
		}
	}
	assert.True(t, foundNS)
}

func TestDeploySubstrateSettingsExampleYAML_ValidatesAndLoads(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "settings.example.yaml")

	valErrors, err := ValidateSettings(expanded, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "settings.example.yaml should validate against v1 schema, got: %v", valErrors)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), expanded, 0o644))

	vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
	require.NoError(t, err)
	sub := vs.Runtimes["substrate-prod"].Substrate
	require.NotNil(t, sub)
	assert.Empty(t, sub.SandboxClass)
	assert.Empty(t, sub.SandboxConfigName)
}

func TestDeploySubstrateClusterYAML_V1WorkerPool(t *testing.T) {
	wpExpanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "cluster", "workerpool.yaml")
	var foundWorkerPool bool
	wpDec := yaml.NewDecoder(bytes.NewReader(wpExpanded))
	for {
		var doc struct {
			APIVersion string                 `yaml:"apiVersion"`
			Kind       string                 `yaml:"kind"`
			Spec       map[string]interface{} `yaml:"spec"`
		}
		if err := wpDec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "WorkerPool" {
			foundWorkerPool = true
			assert.Equal(t, "ate.dev/v1", doc.APIVersion)
			_, hasLegacySandboxClass := doc.Spec["sandboxClass"]
			assert.False(t, hasLegacySandboxClass, "v1 WorkerPool must use spec.sandboxClasses rather than sandboxClass")
			classes, ok := doc.Spec["sandboxClasses"].([]interface{})
			require.True(t, ok)
			require.NotEmpty(t, classes)
			first, ok := classes[0].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "gvisor", first["name"])
			cfgRef, ok := first["configRef"].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "placeholder", cfgRef["name"])
		}
	}
	assert.True(t, foundWorkerPool)
}

func TestDeploySubstrateManifests_EmptySnapshotStorageValidatesAsEmptyString(t *testing.T) {
	emptyOverrides := map[string]string{"SNAPSHOT_STORAGE_URI": ""}

	t.Run("broker.yaml", func(t *testing.T) {
		expanded := loadExpandedSubstrateManifestWith(t, emptyOverrides, "deploy", "substrate", "broker.yaml")
		var settings string
		dec := yaml.NewDecoder(bytes.NewReader(expanded))
		for {
			var doc struct {
				Kind     string                `yaml:"kind"`
				Metadata struct{ Name string } `yaml:"metadata"`
				Data     map[string]string     `yaml:"data"`
			}
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else {
				require.NoError(t, err)
			}
			if doc.Kind == "ConfigMap" && doc.Metadata.Name == "scion-substrate-broker-settings" {
				settings = doc.Data["settings.yaml"]
			}
		}
		require.NotEmpty(t, settings)

		valErrors, err := ValidateSettings([]byte(settings), "1")
		require.NoError(t, err)
		assert.Empty(t, valErrors, "empty SNAPSHOT_STORAGE_URI must serialize as empty string, not YAML null: %v", valErrors)

		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), []byte(settings), 0o644))

		vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
		require.NoError(t, err)
		require.NotNil(t, vs.Runtimes["substrate-prod"].Substrate)
		assert.Equal(t, "", vs.Runtimes["substrate-prod"].Substrate.SnapshotStorage)
	})

	t.Run("settings.example.yaml", func(t *testing.T) {
		expanded := loadExpandedSubstrateManifestWith(t, emptyOverrides, "deploy", "substrate", "settings.example.yaml")

		valErrors, err := ValidateSettings(expanded, "1")
		require.NoError(t, err)
		assert.Empty(t, valErrors, "empty SNAPSHOT_STORAGE_URI must serialize as empty string, not YAML null: %v", valErrors)

		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), expanded, 0o644))

		vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
		require.NoError(t, err)
		require.NotNil(t, vs.Runtimes["substrate-prod"].Substrate)
		assert.Equal(t, "", vs.Runtimes["substrate-prod"].Substrate.SnapshotStorage)
	})
}
