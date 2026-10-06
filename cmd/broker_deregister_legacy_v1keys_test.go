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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrokerDeregister_LegacySettingsKeepV1OnlyKeys (ptone/scion#3497): a
// legacy-format global settings file that also holds v1-only top-level keys
// (server, image_registry) keeps them through deregister. Deregister takes
// both legacy write paths: config.DeleteHubConnection removes the
// hub_connections entry (under the settings lock deregister holds), then
// config.UpdateSetting clears the broker identity, auto-migrating the file
// to v1 through MigrateSettingsFile.
func TestBrokerDeregister_LegacySettingsKeepV1OnlyKeys(t *testing.T) {
	content := `active_profile: local
image_registry: ghcr.io/example/scion
harnesses:
  claude:
    image: example.com/custom-claude:legacy
    user: scion
hub:
  endpoint: HUB_URL
  brokerId: BROKER_ID
hub_connections:
  myhub:
    endpoint: HUB_URL
  other:
    endpoint: https://other.example.com
server:
  broker:
    port: 19800
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
`
	settingsPath, before := runDeregisterWithSettings(t, "settings.yaml", content)
	after := readFlatYAML(t, settingsPath)

	for _, key := range []string{
		"image_registry",
		"server.broker.port",
		"server.broker.instances.0.key",
		"server.broker.instances.0.name",
		"server.broker.instances.0.runtime_target.type",
	} {
		assert.Equal(t, before[key], after[key], "deregister must keep %s", key)
	}
	assert.NotContains(t, after, "server.broker.instances.1.key", "instances must not grow")
	assert.NotContains(t, after, "hub_connections.myhub.endpoint")
	assert.NotContains(t, after, "server.broker.broker_id", "deregister clears the broker identity")
	assert.Equal(t, "1", after["schema_version"], "the follow-up settings write migrates the file")

	vs, _, err := config.LoadGlobalSettings()
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, 19800, vs.Server.Broker.Port, "the broker port must not fall back to the default")
	assert.Equal(t, "ghcr.io/example/scion", vs.ImageRegistry)
	require.Contains(t, vs.HarnessConfigs, "claude")
	assert.Equal(t, "example.com/custom-claude:legacy", vs.HarnessConfigs["claude"].Image)
}
