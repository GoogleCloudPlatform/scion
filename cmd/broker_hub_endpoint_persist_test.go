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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const persistTestBrokerID = "11111111-2222-3333-4444-555555555555"

// readSettingsTree parses the settings file at path.
func readSettingsTree(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var tree map[string]any
	require.NoError(t, yaml.Unmarshal(data, &tree))
	return tree
}

// brokerHubEndpointIn returns server.broker.hub_endpoint from tree.
func brokerHubEndpointIn(tree map[string]any) (string, bool) {
	server, _ := tree["server"].(map[string]any)
	broker, _ := server["broker"].(map[string]any)
	v, ok := broker["hub_endpoint"].(string)
	return v, ok
}

// hubEndpointIn returns the CLI's hub.endpoint from tree.
func hubEndpointIn(tree map[string]any) (string, bool) {
	hub, _ := tree["hub"].(map[string]any)
	v, ok := hub["endpoint"].(string)
	return v, ok
}

func TestPersistBrokerHubSettings_WritesServerBrokerHubEndpoint(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	path := writeServerHubEndpointTestSettings(t, globalDir, "")

	var w bytes.Buffer
	persistBrokerHubSettings(&w, globalDir, "https://hub.example.com", persistTestBrokerID, "hub-example-com")
	assert.Empty(t, w.String(), "no warnings")

	tree := readSettingsTree(t, path)
	got, ok := brokerHubEndpointIn(tree)
	require.True(t, ok, "server.broker.hub_endpoint is written: %v", tree)
	assert.Equal(t, "https://hub.example.com", got)
	_, ok = hubEndpointIn(tree)
	assert.False(t, ok, "hub.endpoint is never written")

	// The server reads it back as the broker's reporting hub.
	cfg, err := config.LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, "https://hub.example.com", cfg.RuntimeBroker.HubEndpoint)
	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, persistTestBrokerID, gs.Hub.BrokerID, "the broker ID is still saved")
	assert.Empty(t, gs.Hub.Endpoint)
}

func TestPersistBrokerHubSettings_KeepsExistingServerBrokerHubEndpoint(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	path := writeServerHubEndpointTestSettings(t, globalDir, "server:\n  broker:\n    hub_endpoint: https://first.example.com\n")

	var w bytes.Buffer
	persistBrokerHubSettings(&w, globalDir, "https://second.example.com", persistTestBrokerID, "second-example-com")
	assert.Empty(t, w.String())

	tree := readSettingsTree(t, path)
	got, _ := brokerHubEndpointIn(tree)
	assert.Equal(t, "https://first.example.com", got, "a later registration does not move the broker's reporting hub")
	_, ok := hubEndpointIn(tree)
	assert.False(t, ok)
}

func TestPersistBrokerHubSettings_CoLocatedWorkstationHubSkipsWrite(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	path := writeServerHubEndpointTestSettings(t, globalDir, "")
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "dev-token"), []byte("scion_dev_persist_test_token\n"), 0600))

	var w bytes.Buffer
	persistBrokerHubSettings(&w, globalDir, "https://hub.example.com", persistTestBrokerID, "hub-example-com")
	assert.Empty(t, w.String(), "the skip prints nothing")

	tree := readSettingsTree(t, path)
	_, ok := brokerHubEndpointIn(tree)
	assert.False(t, ok, "server.broker.hub_endpoint is not written on a workstation hub host: %v", tree)
	_, ok = hubEndpointIn(tree)
	assert.False(t, ok, "hub.endpoint is never written")
	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, persistTestBrokerID, gs.Hub.BrokerID)
}

// TestPersistBrokerHubSettings_LeavesHubEndpointAlone checks that an
// existing CLI hub.endpoint is left byte for byte, and that the only other
// change is the intended broker keys.
func TestPersistBrokerHubSettings_LeavesHubEndpointAlone(t *testing.T) {
	globalDir := isolateServerHubEndpointEnv(t)
	path := writeServerHubEndpointTestSettings(t, globalDir, "hub:\n  endpoint: https://cli.example.com\n")

	var w bytes.Buffer
	persistBrokerHubSettings(&w, globalDir, "https://hub.example.com", persistTestBrokerID, "hub-example-com")
	assert.Empty(t, w.String())

	tree := readSettingsTree(t, path)
	got, ok := hubEndpointIn(tree)
	require.True(t, ok)
	assert.Equal(t, "https://cli.example.com", got, "the CLI's hub.endpoint is unchanged")
	be, _ := brokerHubEndpointIn(tree)
	assert.Equal(t, "https://hub.example.com", be)

	// Removing the two intended keys leaves the original tree.
	server := tree["server"].(map[string]any)
	broker := server["broker"].(map[string]any)
	delete(broker, "hub_endpoint")
	delete(broker, "broker_id")
	if len(broker) == 0 {
		delete(server, "broker")
	}
	if len(server) == 0 {
		delete(tree, "server")
	}
	assert.Equal(t, map[string]any{
		"schema_version": "1",
		"hub":            map[string]any{"endpoint": "https://cli.example.com"},
	}, tree)
}
