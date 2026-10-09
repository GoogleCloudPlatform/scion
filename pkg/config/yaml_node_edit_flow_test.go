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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestUpdateVersionedSetting_ClearThenSetBrokerIDKeepsBlockStyle covers
// ptone/scion#3535: deregister clears server.broker.broker_id (leaving
// `broker: {}`), and the next unregistered broker start sets a new ID. The
// new ID must be written in block style, not as `broker: {broker_id: x}`.
func TestUpdateVersionedSetting_ClearThenSetBrokerIDKeepsBlockStyle(t *testing.T) {
	const src = "schema_version: \"1\"\n# user comment\nserver:\n  broker: # set by registration\n    broker_id: old-id\nhub:\n  endpoint: https://h\n"
	dir := writeSettingsFixture(t, src)

	require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", ""))
	assert.Equal(t, "schema_version: \"1\"\n# user comment\nserver:\n  broker: {} # set by registration\nhub:\n  endpoint: https://h\n", readSettingsFile(t, dir))

	require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new-id"))
	assert.Equal(t, "schema_version: \"1\"\n# user comment\nserver:\n  broker: # set by registration\n    broker_id: new-id\nhub:\n  endpoint: https://h\n", readSettingsFile(t, dir))

	vs, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "new-id", vs.Server.Broker.BrokerID)
}

func TestSetYAMLPath_EmptyFlowMappingBecomesBlock(t *testing.T) {
	tests := []struct {
		name string
		src  string
		path []string
		want string
	}{
		{"empty flow parent", "a: 1\nhub: {}\nz: 1\n", []string{"hub", "endpoint"}, "a: 1\nhub:\n  endpoint: x\nz: 1\n"},
		{"empty flow grandparent", "server: {}\n", []string{"server", "broker", "broker_id"}, "server:\n  broker:\n    broker_id: x\n"},
		{"comment returns to key", "hub: {} # c\n", []string{"hub", "endpoint"}, "hub: # c\n  endpoint: x\n"},
		{"non-empty flow mapping kept", "hub: {linked: true}\n", []string{"hub", "endpoint"}, "hub: {linked: true, endpoint: x}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := parseYAMLMappingDocument([]byte(tt.src))
			require.NoError(t, err)
			changed, err := setYAMLPath(doc.Content[0], tt.path, newYAMLStringScalar("x"))
			require.NoError(t, err)
			require.True(t, changed)
			out, err := encodeYAMLDocument(doc, 2)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(out))
		})
	}
}

// TestSetYAMLPath_BrokerInstancesIntoEmptyFlowBroker writes a non-scalar
// server.broker.instances value into the `broker: {}` that clearing the last
// broker key leaves behind; it must come out as a block mapping.
func TestSetYAMLPath_BrokerInstancesIntoEmptyFlowBroker(t *testing.T) {
	doc, err := parseYAMLMappingDocument([]byte("schema_version: \"1\"\nserver:\n  broker: {} # cleared\n  log_level: info\n"))
	require.NoError(t, err)

	var instances yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("local:\n  port: 9800\n"), &instances))
	changed, err := setYAMLPath(doc.Content[0], []string{"server", "broker", "instances"}, instances.Content[0])
	require.NoError(t, err)
	require.True(t, changed)

	out, err := encodeYAMLDocument(doc, 2)
	require.NoError(t, err)
	assert.Equal(t, "schema_version: \"1\"\nserver:\n  broker: # cleared\n    instances:\n      local:\n        port: 9800\n  log_level: info\n", string(out))
}
