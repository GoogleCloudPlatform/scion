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
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-plugin/runner"
)

// Manager owns the lifecycle of all loaded plugins.
// It handles discovery, loading, dispensing, and shutdown of plugin processes.
type Manager struct {
	clients         map[string]*goplugin.Client // "type:name" -> client
	dispensed       map[string]interface{}      // "type:name" -> dispensed interface (cached)
	selfManaged     map[string]bool             // "type:name" -> true if self-managed
	configs         map[string]DiscoveredPlugin // "type:name" -> original config (for reconnection)
	mu              sync.RWMutex
	logger          *slog.Logger
	brokerCallbacks *HostCallbacksForwarder // lazily-wired host callbacks for broker plugins
}

// NewManager creates a new plugin manager.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		clients:         make(map[string]*goplugin.Client),
		dispensed:       make(map[string]interface{}),
		selfManaged:     make(map[string]bool),
		configs:         make(map[string]DiscoveredPlugin),
		logger:          logger,
		brokerCallbacks: &HostCallbacksForwarder{},
	}
}

// SetBrokerHostCallbacks sets the HostCallbacks implementation that broker
// plugins can use to request/cancel subscriptions. Typically called after the
// MessageBrokerProxy is created, which implements HostCallbacks.
func (m *Manager) SetBrokerHostCallbacks(cb HostCallbacks) {
	m.brokerCallbacks.Set(cb)
}

// HostCallbacksForwarder lazily forwards HostCallbacks calls to a target
// implementation. It is created immediately with the Manager but the target
// is set later (after the MessageBrokerProxy is created). Calls made before
// the target is set return an error.
type HostCallbacksForwarder struct {
	mu sync.RWMutex
	cb HostCallbacks
}

// Set wires the forwarder to the real HostCallbacks implementation.
func (f *HostCallbacksForwarder) Set(cb HostCallbacks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cb = cb
}

func (f *HostCallbacksForwarder) RequestSubscription(pattern string) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cb == nil {
		return fmt.Errorf("host callbacks not yet available")
	}
	return f.cb.RequestSubscription(pattern)
}

func (f *HostCallbacksForwarder) CancelSubscription(pattern string) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cb == nil {
		return fmt.Errorf("host callbacks not yet available")
	}
	return f.cb.CancelSubscription(pattern)
}

// LoadAll discovers and loads all plugins from the given configuration and plugins directory.
func (m *Manager) LoadAll(cfg PluginsConfig, pluginsDir string) error {
	discovered := DiscoverPlugins(cfg, pluginsDir, m.logger)

	for _, dp := range discovered {
		if err := m.loadPlugin(dp); err != nil {
			m.logger.Error("Failed to load plugin",
				"type", dp.Type,
				"name", dp.Name,
				"path", dp.Path,
				"error", err,
			)
			continue
		}
		m.logger.Info("Loaded plugin",
			"type", dp.Type,
			"name", dp.Name,
			"path", dp.Path,
		)
	}

	return nil
}

// LoadOne loads a single plugin by type and name from the given configuration.
func (m *Manager) LoadOne(pluginType, name string, entry PluginEntry, pluginsDir string) error {
	if entry.SelfManaged {
		return m.loadPlugin(DiscoveredPlugin{
			Name:        name,
			Type:        pluginType,
			Config:      entry.Config,
			FromConfig:  true,
			SelfManaged: true,
			Address:     entry.Address,
		})
	}
	path := resolvePluginPath(name, pluginType, entry.Path, pluginsDir, m.logger)
	if path == "" {
		return fmt.Errorf("plugin binary not found: %s/%s", pluginType, name)
	}
	return m.loadPlugin(DiscoveredPlugin{
		Name:       name,
		Type:       pluginType,
		Path:       path,
		Config:     entry.Config,
		FromConfig: true,
	})
}

// loadPlugin starts a plugin process (or connects to a self-managed one) and stores its client.
func (m *Manager) loadPlugin(dp DiscoveredPlugin) error {
	var protocolVersion uint
	var pluginMap map[string]goplugin.Plugin

	switch dp.Type {
	case PluginTypeBroker:
		protocolVersion = BrokerPluginProtocolVersion
		pluginMap = map[string]goplugin.Plugin{
			BrokerPluginName: &BrokerPlugin{HostCallbacks: m.brokerCallbacks},
		}
	default:
		return fmt.Errorf("unknown plugin type: %s", dp.Type)
	}

	var client *goplugin.Client
	if dp.SelfManaged {
		client = m.loadSelfManagedPlugin(dp, protocolVersion, pluginMap)
	} else {
		client = goplugin.NewClient(&goplugin.ClientConfig{
			HandshakeConfig: goplugin.HandshakeConfig{
				ProtocolVersion:  protocolVersion,
				MagicCookieKey:   MagicCookieKey,
				MagicCookieValue: MagicCookieValue,
			},
			Plugins: pluginMap,
			Cmd:     exec.Command(dp.Path),
			Logger:  newHclogAdapter(m.logger),
		})
	}

	// Connect to the plugin process and get the RPC client
	rpcClient, err := client.Client()
	if err != nil {
		if !dp.SelfManaged {
			client.Kill()
		}
		return fmt.Errorf("failed to connect to plugin %s/%s: %w", dp.Type, dp.Name, err)
	}

	// Dispense the plugin interface
	var dispenseName string
	switch dp.Type {
	case PluginTypeBroker:
		dispenseName = BrokerPluginName
	}

	raw, err := rpcClient.Dispense(dispenseName)
	if err != nil {
		if !dp.SelfManaged {
			client.Kill()
		}
		return fmt.Errorf("failed to dispense plugin %s/%s: %w", dp.Type, dp.Name, err)
	}

	// For broker plugins, configure them immediately
	if dp.Type == PluginTypeBroker {
		if brokerClient, ok := raw.(*BrokerRPCClient); ok {
			config := dp.Config
			if config == nil {
				config = make(map[string]string)
			}
			if brokerClient.hostCallbacksAvailable {
				config[hostCallbacksConfigKey] = "true"
			}
			if err := brokerClient.Configure(config); err != nil {
				if !dp.SelfManaged {
					client.Kill()
				}
				return fmt.Errorf("failed to configure broker plugin %s: %w", dp.Name, err)
			}
		}
	}

	key := dp.Type + ":" + dp.Name
	m.mu.Lock()
	// Kill any existing plugin with the same key (only if not self-managed)
	if existing, ok := m.clients[key]; ok {
		if !m.selfManaged[key] {
			existing.Kill()
		}
		delete(m.dispensed, key)
	}
	m.clients[key] = client
	m.selfManaged[key] = dp.SelfManaged
	m.configs[key] = dp
	// Cache the dispensed interface so subsequent Get() calls don't
	// trigger a second Dispense (which would start another AcceptAndServe
	// on the same MuxBroker stream ID, causing a timeout).
	m.dispensed[key] = raw
	m.mu.Unlock()

	return nil
}

// loadSelfManagedPlugin creates a go-plugin client that connects to an
// already-running plugin process at the configured address. The Hub does not
// own the process — Kill() will not terminate it.
func (m *Manager) loadSelfManagedPlugin(dp DiscoveredPlugin, protocolVersion uint, pluginMap map[string]goplugin.Plugin) *goplugin.Client {
	addr, err := net.ResolveTCPAddr("tcp", dp.Address)
	if err != nil {
		addr = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
		m.logger.Warn("Failed to resolve self-managed plugin address",
			"address", dp.Address, "error", err)
	}

	pluginAddr := addr // capture for closure
	return goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: goplugin.HandshakeConfig{
			ProtocolVersion:  protocolVersion,
			MagicCookieKey:   MagicCookieKey,
			MagicCookieValue: MagicCookieValue,
		},
		Plugins: pluginMap,
		Reattach: &goplugin.ReattachConfig{
			Protocol:        goplugin.ProtocolNetRPC,
			ProtocolVersion: int(protocolVersion),
			Addr:            pluginAddr,
			Test:            true, // Prevents Kill() from terminating the process
			ReattachFunc: func() (runner.AttachedRunner, error) {
				return &selfManagedRunner{id: dp.Name}, nil
			},
		},
		Logger: newHclogAdapter(m.logger),
	})
}

// selfManagedRunner implements runner.AttachedRunner for self-managed plugins.
// It is a no-op runner that does not own or manage the plugin process.
type selfManagedRunner struct {
	id string
}

func (r *selfManagedRunner) Wait(_ context.Context) error { return nil }
func (r *selfManagedRunner) Kill(_ context.Context) error { return nil }
func (r *selfManagedRunner) ID() string                   { return r.id }

func (r *selfManagedRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) {
	return pluginNet, pluginAddr, nil
}

func (r *selfManagedRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) {
	return hostNet, hostAddr, nil
}

// Get returns the dispensed plugin interface for the given type and name.
// It returns a cached instance from loadPlugin if available, avoiding a
// second Dispense call that would create a duplicate MuxBroker AcceptAndServe.
func (m *Manager) Get(pluginType, name string) (interface{}, error) {
	key := pluginType + ":" + name
	m.mu.RLock()
	cached, hasCached := m.dispensed[key]
	_, ok := m.clients[key]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("plugin not loaded: %s/%s", pluginType, name)
	}

	if hasCached {
		return cached, nil
	}

	// Fallback: dispense fresh (should not normally happen since loadPlugin
	// always caches, but keeps the API robust).
	m.logger.Warn("dispensing plugin without cache (unexpected)",
		"type", pluginType, "name", name)

	client := m.clients[key]
	rpcClient, err := client.Client()
	if err != nil {
		return nil, fmt.Errorf("failed to get RPC client for %s/%s: %w", pluginType, name, err)
	}

	var dispenseName string
	switch pluginType {
	case PluginTypeBroker:
		dispenseName = BrokerPluginName
	default:
		return nil, fmt.Errorf("unknown plugin type: %s", pluginType)
	}

	return rpcClient.Dispense(dispenseName)
}

// Reconnect reloads a self-managed plugin by establishing a fresh connection
// to its RPC address. This is used when a self-managed plugin process restarts
// and the existing connection is dead.
func (m *Manager) Reconnect(pluginType, name string) error {
	key := pluginType + ":" + name
	m.mu.RLock()
	dp, ok := m.configs[key]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no stored config for plugin %s/%s (not self-managed?)", pluginType, name)
	}
	m.logger.Info("Reconnecting to self-managed plugin",
		"type", pluginType, "name", name, "address", dp.Address)
	return m.loadPlugin(dp)
}

// GetBroker returns an eventbus.EventBus backed by the named broker plugin.
// For self-managed plugins, it returns a reconnecting adapter that automatically
// re-establishes the connection if the plugin process restarts.
func (m *Manager) GetBroker(name string) (eventbus.EventBus, error) {
	raw, err := m.Get(PluginTypeBroker, name)
	if err != nil {
		return nil, err
	}

	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not a broker plugin", name)
	}

	adapter := NewBrokerPluginAdapter(rpcClient)
	if m.IsSelfManaged(PluginTypeBroker, name) {
		return newReconnectingBrokerAdapter(m, name, adapter, m.logger), nil
	}
	return adapter, nil
}

// ConfigureBroker re-configures a loaded broker plugin by merging extra
// key-value pairs into the plugin's original settings.yaml config. This
// is used to inject runtime credentials (hub_url, hmac_key, broker_id)
// that are not available at initial plugin load time.
func (m *Manager) ConfigureBroker(name string, extra map[string]string) error {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	raw, ok := m.dispensed[key]
	dp, hasDP := m.configs[key]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("broker plugin not loaded: %s", name)
	}

	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return fmt.Errorf("plugin %s is not a broker RPC client", name)
	}

	// Start from the original plugin config and layer the extra values on top.
	merged := make(map[string]string)
	if hasDP {
		for k, v := range dp.Config {
			merged[k] = v
		}
	}
	if rpcClient.hostCallbacksAvailable {
		merged[hostCallbacksConfigKey] = "true"
	}
	for k, v := range extra {
		merged[k] = v
	}

	return rpcClient.Configure(merged)
}

// GetPluginConfig returns a copy of the stored config map for the named plugin,
// or nil if the plugin is not loaded. The returned map is safe to read without
// affecting the manager's internal state.
func (m *Manager) GetPluginConfig(pluginType, name string) map[string]string {
	key := pluginType + ":" + name
	m.mu.RLock()
	dp, ok := m.configs[key]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	out := make(map[string]string, len(dp.Config))
	for k, v := range dp.Config {
		out[k] = v
	}
	return out
}

// HasPlugin returns true if a plugin with the given type and name is loaded.
func (m *Manager) HasPlugin(pluginType, name string) bool {
	key := pluginType + ":" + name
	m.mu.RLock()
	_, ok := m.clients[key]
	m.mu.RUnlock()
	return ok
}

// IsSelfManaged returns true if the named plugin is loaded in self-managed mode.
func (m *Manager) IsSelfManaged(pluginType, name string) bool {
	key := pluginType + ":" + name
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selfManaged[key]
}

// ListPlugins returns a list of all loaded plugin keys ("type:name").
func (m *Manager) ListPlugins() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.clients))
	for k := range m.clients {
		keys = append(keys, k)
	}
	return keys
}

// BrokerHealthCheck returns the health status of a named broker plugin as
// primitive types so callers need not import plugin-package structs.
func (m *Manager) BrokerHealthCheck(name string) (status, message string, details map[string]string, err error) {
	raw, err := m.Get(PluginTypeBroker, name)
	if err != nil {
		return "", "", nil, err
	}
	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return "", "", nil, fmt.Errorf("plugin %s is not a broker RPC client", name)
	}
	hs, err := rpcClient.HealthCheck()
	if err != nil {
		return "", "", nil, err
	}
	if hs == nil {
		return "unknown", "", nil, nil
	}
	return hs.Status, hs.Message, hs.Details, nil
}

// BrokerInfo returns plugin metadata for a named broker plugin as primitive
// types so callers need not import plugin-package structs.
func (m *Manager) BrokerInfo(name string) (version, channelID string, capabilities []string, err error) {
	raw, err := m.Get(PluginTypeBroker, name)
	if err != nil {
		return "", "", nil, err
	}
	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return "", "", nil, fmt.Errorf("plugin %s is not a broker RPC client", name)
	}
	info, err := rpcClient.GetInfo()
	if err != nil {
		return "", "", nil, err
	}
	if info == nil {
		return "", "", nil, nil
	}
	return info.Version, info.ChannelID, info.Capabilities, nil
}

// Shutdown kills all plugin processes gracefully.
// Self-managed plugins are disconnected but their processes are not terminated.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, client := range m.clients {
		if m.selfManaged[key] {
			m.logger.Info("Disconnecting self-managed plugin", "plugin", key)
			// For self-managed plugins, Kill() with Test=true in the
			// ReattachConfig will close the RPC connection without
			// terminating the external process.
		} else {
			m.logger.Info("Shutting down plugin", "plugin", key)
		}
		client.Kill()
	}
	m.clients = make(map[string]*goplugin.Client)
	m.dispensed = make(map[string]interface{})
	m.selfManaged = make(map[string]bool)
	m.configs = make(map[string]DiscoveredPlugin)

	goplugin.CleanupClients()
}
