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

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles loads the
// settings.yaml embedded in the REAL deploy/substrate/broker.yaml (envsubst
// placeholders filled with a dummy value) on top of the embedded defaults,
// and requires every profile to resolve to a substrate runtime. Unlike
// substrate_broker_profiles_test.go's
// substrateBrokerProfilesOverride (a hand-maintained copy of the same
// stanza, which can drift from the manifest silently), this fails if the
// manifest's local/remote repoint is ever removed or edited incorrectly.
func TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "substrate", "broker.yaml"))
	require.NoError(t, err)
	expanded := os.Expand(string(raw), func(string) string { return "placeholder" })

	var settings string
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
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
}
