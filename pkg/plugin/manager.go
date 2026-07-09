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
	"os"
	"os/exec"
	"path/filepath"
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
	grpcAdapters    map[string]GRPCBrokerClient // "type:name" -> gRPC adapter
	configs         map[string]DiscoveredPlugin // "type:name" -> original config (for reconnection)
	pluginEntries   map[string]PluginEntry      // "type:name" -> original PluginEntry (for mode resolution)
	mu              sync.RWMutex
	logger          *slog.Logger
	brokerCallbacks *HostCallbacksForwarder // lazily-wired host callbacks for broker plugins

	// NewGRPCBrokerAdapter creates a GRPCBrokerClient for a given plugin entry.
	// Injected at startup to avoid an import cycle with pkg/plugin/grpcbroker.
	NewGRPCBrokerAdapter func(entry PluginEntry, logger *slog.Logger) (GRPCBrokerClient, error)
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
		grpcAdapters:    make(map[string]GRPCBrokerClient),
		configs:         make(map[string]DiscoveredPlugin),
		pluginEntries:   make(map[string]PluginEntry),
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
	// Handle gRPC mode entries before standard discovery.
	for name, entry := range cfg.Broker {
		if entry.ResolvedDeploymentMode() == DeploymentModeHA {
			if err := m.loadGRPCPlugin(PluginTypeBroker, name, entry); err != nil {
				m.logger.Error("Failed to load gRPC plugin",
					"name", name,
					"address", entry.Address,
					"error", err,
				)
			}
		}
	}

	discovered := DiscoverPlugins(cfg, pluginsDir, m.logger)

	for _, dp := range discovered {
		// Skip entries already loaded as gRPC plugins.
		key := dp.Type + ":" + dp.Name
		m.mu.RLock()
		_, isGRPC := m.grpcAdapters[key]
		m.mu.RUnlock()
		if isGRPC {
			continue
		}

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
	if entry.ConfigFile != "" {
		if entry.Config == nil {
			entry.Config = make(map[string]string)
		}
		entry.Config["config_file"] = entry.ConfigFile
	}
	if entry.ResolvedDeploymentMode() == DeploymentModeHA {
		return m.loadGRPCPlugin(pluginType, name, entry)
	}
	if entry.SelfManaged || entry.ResolvedDeploymentMode() == DeploymentModeExternal {
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

// loadGRPCPlugin creates a GRPCBrokerClient for a plugin configured with mode: grpc.
func (m *Manager) loadGRPCPlugin(pluginType, name string, entry PluginEntry) error {
	if entry.Address == "" {
		return fmt.Errorf("address is required for gRPC mode plugin %s/%s", pluginType, name)
	}
	if m.NewGRPCBrokerAdapter == nil {
		return fmt.Errorf("gRPC broker adapter factory not configured")
	}

	adapter, err := m.NewGRPCBrokerAdapter(entry, m.logger)
	if err != nil {
		return fmt.Errorf("create gRPC adapter for %s/%s: %w", pluginType, name, err)
	}

	key := pluginType + ":" + name
	m.mu.Lock()
	if old, ok := m.grpcAdapters[key]; ok {
		_ = old.Close()
	}
	m.grpcAdapters[key] = adapter
	m.pluginEntries[key] = entry
	m.mu.Unlock()

	m.logger.Info("Loaded gRPC plugin",
		"type", pluginType, "name", name, "address", entry.Address)
	return nil
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
// For gRPC mode plugins, it returns the GRPCBrokerAdapter directly (which has
// its own reconnection logic).
func (m *Manager) GetBroker(name string) (eventbus.EventBus, error) {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	adapter, isGRPC := m.grpcAdapters[key]
	m.mu.RUnlock()

	if isGRPC {
		return adapter, nil
	}

	raw, err := m.Get(PluginTypeBroker, name)
	if err != nil {
		return nil, err
	}

	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not a broker plugin", name)
	}

	rpcAdapter := NewBrokerPluginAdapter(rpcClient)
	if m.IsSelfManaged(PluginTypeBroker, name) {
		return newReconnectingBrokerAdapter(m, name, rpcAdapter, m.logger), nil
	}
	return rpcAdapter, nil
}

// ConfigureBroker re-configures a loaded broker plugin by merging extra
// key-value pairs into the plugin's original settings.yaml config. This
// is used to inject runtime credentials (hub_url, hmac_key, broker_id)
// that are not available at initial plugin load time.
func (m *Manager) ConfigureBroker(name string, extra map[string]string) error {
	key := PluginTypeBroker + ":" + name

	// Check for gRPC adapter first.
	m.mu.RLock()
	adapter, isGRPC := m.grpcAdapters[key]
	m.mu.RUnlock()

	if isGRPC {
		return adapter.Configure(extra)
	}

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

// ReplaceBrokerConfig updates the stored config for a broker plugin and pushes
// the exact cfg to the running plugin, with no underlay from boot-time config.
func (m *Manager) ReplaceBrokerConfig(name string, cfg map[string]string) error {
	key := PluginTypeBroker + ":" + name
	m.mu.Lock()
	if dp, ok := m.configs[key]; ok {
		dp.Config = cfg
		m.configs[key] = dp
	}
	m.mu.Unlock()

	m.mu.RLock()
	adapter, isGRPC := m.grpcAdapters[key]
	m.mu.RUnlock()

	if isGRPC {
		return adapter.Configure(cfg)
	}

	m.mu.RLock()
	raw, ok := m.dispensed[key]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("broker plugin not loaded: %s", name)
	}

	rpcClient, ok := raw.(*BrokerRPCClient)
	if !ok {
		return fmt.Errorf("plugin %s is not a broker RPC client", name)
	}

	merged := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		merged[k] = v
	}
	if rpcClient.hostCallbacksAvailable {
		merged[hostCallbacksConfigKey] = "true"
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
	entry, hasEntry := m.pluginEntries[key]
	m.mu.RUnlock()

	if ok {
		out := make(map[string]string, len(dp.Config))
		for k, v := range dp.Config {
			out[k] = v
		}
		return out
	}
	if hasEntry {
		out := make(map[string]string, len(entry.Config))
		for k, v := range entry.Config {
			out[k] = v
		}
		return out
	}
	return nil
}

// HasPlugin returns true if a plugin with the given type and name is loaded.
func (m *Manager) HasPlugin(pluginType, name string) bool {
	key := pluginType + ":" + name
	m.mu.RLock()
	_, ok := m.clients[key]
	if !ok {
		_, ok = m.grpcAdapters[key]
	}
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

	seen := make(map[string]bool, len(m.clients)+len(m.grpcAdapters))
	keys := make([]string, 0, len(m.clients)+len(m.grpcAdapters))
	for k := range m.clients {
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for k := range m.grpcAdapters {
		if !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	return keys
}

// GetDeploymentMode returns the deployment mode for a loaded plugin.
func (m *Manager) GetDeploymentMode(pluginType, name string) DeploymentMode {
	key := pluginType + ":" + name
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, ok := m.grpcAdapters[key]; ok {
		return DeploymentModeHA
	}
	if m.selfManaged[key] {
		return DeploymentModeExternal
	}
	if _, ok := m.clients[key]; ok {
		return DeploymentModePlugin
	}
	return DeploymentModePlugin
}

// GetGRPCBrokerAdapter returns the GRPCBrokerClient for the named broker if it
// is running in gRPC/HA mode, or nil otherwise.
func (m *Manager) GetGRPCBrokerAdapter(name string) GRPCBrokerClient {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.grpcAdapters[key]
}

// BrokerHealthCheck returns the health status of a named broker plugin as
// primitive types so callers need not import plugin-package structs.
func (m *Manager) BrokerHealthCheck(name string) (status, message string, details map[string]string, err error) {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	adapter, isGRPC := m.grpcAdapters[key]
	m.mu.RUnlock()

	if isGRPC {
		hs, hErr := adapter.HealthCheck()
		if hErr != nil {
			return "", "", nil, hErr
		}
		if hs == nil {
			return "unknown", "", nil, nil
		}
		return hs.Status, hs.Message, hs.Details, nil
	}

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
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	adapter, isGRPC := m.grpcAdapters[key]
	m.mu.RUnlock()

	if isGRPC {
		info, iErr := adapter.GetInfo()
		if iErr != nil {
			return "", "", nil, iErr
		}
		if info == nil {
			return "", "", nil, nil
		}
		return info.Version, info.ChannelID, info.Capabilities, nil
	}

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

// UpdatePlugin rebuilds a hub-managed (non-self-managed) plugin binary from
// source and restarts it. Self-managed plugins cannot be updated this way.
func (m *Manager) UpdatePlugin(name string, repoPath string) error {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	dp, ok := m.configs[key]
	isSelf := m.selfManaged[key]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("plugin %q is not loaded", name)
	}
	if isSelf {
		return fmt.Errorf("plugin %q is self-managed and cannot be updated this way", name)
	}

	sourceDir := filepath.Join(repoPath, "extras", "scion-"+name)
	if _, err := os.Stat(sourceDir); err != nil {
		return fmt.Errorf("plugin source directory not found: %s", sourceDir)
	}

	binaryPath := dp.Path
	tmpBinaryPath := binaryPath + ".tmp"

	m.logger.Info("Building plugin from source",
		"name", name, "source", sourceDir, "binary", binaryPath)

	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Dir = sourceDir
	if output, err := tidyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod tidy failed for plugin %q: %w\n%s", name, err, string(output))
	}

	buildCmd := exec.Command("go", "build", "-o", tmpBinaryPath, "./cmd/scion-plugin-"+name)
	buildCmd.Dir = sourceDir
	if output, err := buildCmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpBinaryPath)
		return fmt.Errorf("go build failed for plugin %q: %w\n%s", name, err, string(output))
	}
	defer func() { _ = os.Remove(tmpBinaryPath) }()

	m.mu.Lock()
	if client, hasClient := m.clients[key]; hasClient {
		delete(m.dispensed, key)
		delete(m.clients, key)
		client.Kill()
	}
	m.mu.Unlock()

	if err := os.Rename(tmpBinaryPath, binaryPath); err != nil {
		return fmt.Errorf("failed to move new binary into place for plugin %q: %w", name, err)
	}

	return m.loadPlugin(dp)
}

// InstallPlugin builds a plugin binary from source and loads it into the manager.
// Used for first-time installation of plugins not yet present on the system.
func (m *Manager) InstallPlugin(name, repoPath, pluginsDir, configFile string) error {
	key := PluginTypeBroker + ":" + name
	m.mu.RLock()
	_, alreadyLoaded := m.clients[key]
	m.mu.RUnlock()

	if alreadyLoaded {
		return fmt.Errorf("plugin %q is already installed", name)
	}

	sourceDir := filepath.Join(repoPath, "extras", "scion-"+name)
	if _, err := os.Stat(sourceDir); err != nil {
		return fmt.Errorf("plugin source directory not found: %s", sourceDir)
	}

	targetDir := filepath.Join(pluginsDir, PluginTypeBroker)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create plugins directory %s: %w", targetDir, err)
	}

	targetPath := filepath.Join(targetDir, PluginBinaryPrefix+name)

	m.logger.Info("Building plugin from source (first-time install)",
		"name", name, "source", sourceDir, "binary", targetPath)

	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Dir = sourceDir
	if output, err := tidyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod tidy failed for plugin %q: %w\n%s", name, err, string(output))
	}

	buildCmd := exec.Command("go", "build", "-o", targetPath, "./cmd/scion-plugin-"+name)
	buildCmd.Dir = sourceDir
	if output, err := buildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build failed for plugin %q: %w\n%s", name, err, string(output))
	}

	return m.LoadOne(PluginTypeBroker, name, PluginEntry{Path: targetPath, ConfigFile: configFile}, pluginsDir)
}

// Shutdown kills all plugin processes gracefully.
// Self-managed plugins are disconnected but their processes are not terminated.
// gRPC adapters are closed (connection terminated, remote process unaffected).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, client := range m.clients {
		if m.selfManaged[key] {
			m.logger.Info("Disconnecting self-managed plugin", "plugin", key)
		} else {
			m.logger.Info("Shutting down plugin", "plugin", key)
		}
		client.Kill()
	}

	for key, adapter := range m.grpcAdapters {
		m.logger.Info("Closing gRPC broker adapter", "plugin", key)
		if err := adapter.Close(); err != nil {
			m.logger.Error("Failed to close gRPC adapter", "plugin", key, "error", err)
		}
	}

	m.clients = make(map[string]*goplugin.Client)
	m.dispensed = make(map[string]interface{})
	m.selfManaged = make(map[string]bool)
	m.grpcAdapters = make(map[string]GRPCBrokerClient)
	m.configs = make(map[string]DiscoveredPlugin)
	m.pluginEntries = make(map[string]PluginEntry)

	goplugin.CleanupClients()
}
