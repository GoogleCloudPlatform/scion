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

package plugin

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager(t *testing.T) {
	mgr := NewManager(nil)
	require.NotNil(t, mgr)
	assert.Empty(t, mgr.ListPlugins())
}

func TestManagerWithLogger(t *testing.T) {
	logger := slog.Default()
	mgr := NewManager(logger)
	require.NotNil(t, mgr)
}

func TestManagerHasPlugin_NotLoaded(t *testing.T) {
	mgr := NewManager(nil)
	assert.False(t, mgr.HasPlugin(PluginTypeBroker, "nats"))
}

func TestManagerGet_NotLoaded(t *testing.T) {
	mgr := NewManager(nil)

	_, err := mgr.Get(PluginTypeBroker, "nats")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "plugin not loaded")
}

func TestManagerGetBroker_NotLoaded(t *testing.T) {
	mgr := NewManager(nil)

	_, err := mgr.GetBroker("nats")
	assert.Error(t, err)
}

func TestManagerShutdown_Empty(t *testing.T) {
	mgr := NewManager(nil)
	mgr.Shutdown() // Should not panic
	assert.Empty(t, mgr.ListPlugins())
}

func TestManagerLoadAll_EmptyConfig(t *testing.T) {
	mgr := NewManager(nil)
	dir := t.TempDir()

	err := mgr.LoadAll(PluginsConfig{}, dir)
	assert.NoError(t, err)
	assert.Empty(t, mgr.ListPlugins())
}

func TestManagerLoadOne_MissingBinary(t *testing.T) {
	mgr := NewManager(nil)
	dir := t.TempDir()

	err := mgr.LoadOne(PluginTypeBroker, "nats", PluginEntry{}, dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestManagerGet_UnknownType(t *testing.T) {
	mgr := NewManager(nil)

	_, err := mgr.Get("unknown", "foo")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not loaded")
}

func TestManagerIsSelfManaged_NotLoaded(t *testing.T) {
	mgr := NewManager(nil)
	assert.False(t, mgr.IsSelfManaged(PluginTypeBroker, "googlechat"))
}

func TestManagerLoadOne_SelfManaged_UnreachableAddress(t *testing.T) {
	mgr := NewManager(nil)
	dir := t.TempDir()

	// Self-managed plugin with unreachable address — loadPlugin will create the
	// client but connecting to it (client.Client()) should fail since nothing
	// is listening.
	err := mgr.LoadOne(PluginTypeBroker, "googlechat", PluginEntry{
		SelfManaged: true,
		Address:     "localhost:19999",
		Config:      map[string]string{"project_id": "test"},
	}, dir)
	// The connection should fail since no server is running
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to plugin")
}

func TestManagerLoadOne_SelfManaged_NoBinaryNeeded(t *testing.T) {
	mgr := NewManager(nil)
	dir := t.TempDir()

	// A self-managed plugin should not require a binary path.
	// The error should be about connection, not "plugin binary not found".
	err := mgr.LoadOne(PluginTypeBroker, "googlechat", PluginEntry{
		SelfManaged: true,
		Address:     "localhost:19999",
	}, dir)
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "plugin binary not found")
}

func TestHostCallbacksForwarder_BeforeSet(t *testing.T) {
	fwd := &HostCallbacksForwarder{}

	err := fwd.RequestSubscription("scion.grove.test.>")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not yet available")

	err = fwd.CancelSubscription("scion.grove.test.>")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not yet available")
}

func TestHostCallbacksForwarder_AfterSet(t *testing.T) {
	fwd := &HostCallbacksForwarder{}

	var requestedPattern, cancelledPattern string
	mock := &mockHostCallbacks{
		onRequest: func(p string) error { requestedPattern = p; return nil },
		onCancel:  func(p string) error { cancelledPattern = p; return nil },
	}
	fwd.Set(mock)

	err := fwd.RequestSubscription("scion.grove.prod.>")
	assert.NoError(t, err)
	assert.Equal(t, "scion.grove.prod.>", requestedPattern)

	err = fwd.CancelSubscription("scion.grove.prod.>")
	assert.NoError(t, err)
	assert.Equal(t, "scion.grove.prod.>", cancelledPattern)
}

func TestManagerSetBrokerHostCallbacks(t *testing.T) {
	mgr := NewManager(nil)

	var called bool
	mock := &mockHostCallbacks{
		onRequest: func(p string) error { called = true; return nil },
	}
	mgr.SetBrokerHostCallbacks(mock)

	err := mgr.brokerCallbacks.RequestSubscription("test")
	assert.NoError(t, err)
	assert.True(t, called)
}

type mockHostCallbacks struct {
	onRequest func(string) error
	onCancel  func(string) error
}

func (m *mockHostCallbacks) RequestSubscription(pattern string) error {
	if m.onRequest != nil {
		return m.onRequest(pattern)
	}
	return nil
}

func (m *mockHostCallbacks) CancelSubscription(pattern string) error {
	if m.onCancel != nil {
		return m.onCancel(pattern)
	}
	return nil
}

func TestGetPluginConfig_ConfigFilePropagated(t *testing.T) {
	mgr := NewManager(nil)

	mgr.mu.Lock()
	mgr.configs["broker:telegram"] = DiscoveredPlugin{
		Name: "telegram",
		Type: PluginTypeBroker,
		Config: map[string]string{
			"config_file":    "/etc/scion/telegram.yaml",
			"webhook_listen": ":9094",
		},
	}
	mgr.mu.Unlock()

	cfg := mgr.GetPluginConfig(PluginTypeBroker, "telegram")
	require.NotNil(t, cfg)
	assert.Equal(t, "/etc/scion/telegram.yaml", cfg["config_file"])
	assert.Equal(t, ":9094", cfg["webhook_listen"])
}

func TestGetPluginConfig_NoConfigFile(t *testing.T) {
	mgr := NewManager(nil)

	mgr.mu.Lock()
	mgr.configs["broker:nats"] = DiscoveredPlugin{
		Name:   "nats",
		Type:   PluginTypeBroker,
		Config: map[string]string{"url": "nats://localhost:4222"},
	}
	mgr.mu.Unlock()

	cfg := mgr.GetPluginConfig(PluginTypeBroker, "nats")
	require.NotNil(t, cfg)
	assert.Empty(t, cfg["config_file"])
}

func TestConfigFilePropagation_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()

	brokerDir := filepath.Join(dir, "broker")
	require.NoError(t, os.MkdirAll(brokerDir, 0755))
	pluginPath := filepath.Join(brokerDir, "scion-plugin-telegram")
	require.NoError(t, os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755))

	cfg := PluginsConfig{
		Broker: map[string]PluginEntry{
			"telegram": {
				ConfigFile: "/etc/scion/telegram.yaml",
				Config:     map[string]string{"webhook_listen": ":9094"},
			},
		},
	}

	discovered := DiscoverPlugins(cfg, dir, logger)
	require.Len(t, discovered, 1)

	mgr := NewManager(logger)
	mgr.mu.Lock()
	mgr.configs["broker:telegram"] = discovered[0]
	mgr.mu.Unlock()

	pluginCfg := mgr.GetPluginConfig(PluginTypeBroker, "telegram")
	require.NotNil(t, pluginCfg)
	assert.Equal(t, "/etc/scion/telegram.yaml", pluginCfg["config_file"],
		"config_file must be propagated from PluginEntry through DiscoverPlugins to GetPluginConfig")
}

func TestManagerShutdown_SelfManagedTracking(t *testing.T) {
	mgr := NewManager(nil)

	// Verify self-managed tracking is cleared on shutdown
	mgr.mu.Lock()
	mgr.selfManaged["broker:test"] = true
	mgr.mu.Unlock()

	mgr.Shutdown()

	mgr.mu.RLock()
	assert.Empty(t, mgr.selfManaged)
	mgr.mu.RUnlock()
}
