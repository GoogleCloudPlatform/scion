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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// substrateBrokerProfilesOverride mirrors the "profiles"/"runtimes" stanza in
// deploy/substrate/broker.yaml's scion-substrate-broker-settings ConfigMap:
// a broker-scoped settings layer that repoints
// profiles.local and profiles.remote at the same substrate runtime the
// broker already defaults to, instead of the embedded defaults' "container"
// and "kubernetes" runtimes (pkg/config/embeds/default_settings.yaml).
const substrateBrokerProfilesOverride = `
schema_version: "1"
active_profile: substrate
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: api.ate-system.svc:443
      router_endpoint: http://atenet-router.ate-system.svc:80
profiles:
  substrate:
    runtime: substrate-prod
  local:
    runtime: substrate-prod
  remote:
    runtime: substrate-prod
`

// TestLoadVersionedSettings_SubstrateBrokerProfiles_NoDockerOrKubernetesRuntimeType
// is the config-layer verification the design calls for: loading this
// broker's settings layer on top of the embedded defaults must leave every
// profile resolving to a runtime of type "substrate" — none left pointing
// at "container" or "kubernetes" the way the unmodified embedded defaults
// do. That is exactly the condition
// pkg/runtimebroker's discoverAuxiliaryRuntimes checks per profile
// (registering an auxiliary runtime only when a profile's resolved type
// differs from the broker's own default), so this proves the ConfigMap
// change is sufficient for it to register nothing extra on this broker,
// without needing a live broker or a real substrate/docker/Kubernetes
// backend to observe it.
func TestLoadVersionedSettings_SubstrateBrokerProfiles_NoDockerOrKubernetesRuntimeType(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(globalScionDir, "settings.yaml"),
		[]byte(substrateBrokerProfilesOverride), 0644))

	projectDir := filepath.Join(tmpDir, "no-project", ".scion")

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)

	if len(vs.Profiles) == 0 {
		t.Fatal("LoadVersionedSettings() produced no profiles at all — test setup is broken")
	}
	for name, profile := range vs.Profiles {
		rt, ok := vs.Runtimes[profile.Runtime]
		if !ok {
			t.Errorf("profile %q references runtime %q, which is not in vs.Runtimes", name, profile.Runtime)
			continue
		}
		if rt.Type != "substrate" {
			t.Errorf("profile %q -> runtime %q has type %q, want \"substrate\" (a %q or %q type here is exactly what pulls in an unwanted auxiliary runtime)",
				name, profile.Runtime, rt.Type, "container", "kubernetes")
		}
	}

	assert.Equal(t, "substrate-prod", vs.Profiles["local"].Runtime)
	assert.Equal(t, "substrate-prod", vs.Profiles["remote"].Runtime)
	assert.Equal(t, "substrate", vs.Runtimes["substrate-prod"].Type)
}
