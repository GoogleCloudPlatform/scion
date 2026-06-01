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

// Package plugin provides runtime plugin loading and management for scion.
// It supports loading external message broker implementations as separate
// processes using hashicorp/go-plugin.
package plugin

const (
	// PluginTypeBroker is the plugin type for message broker implementations.
	PluginTypeBroker = "broker"

	// BrokerPluginProtocolVersion is the protocol version for broker plugins.
	// Bump this when RPC method signatures, argument types, or semantics change.
	BrokerPluginProtocolVersion = 1

	// MagicCookieKey is the magic cookie key for go-plugin handshake.
	// This prevents users from accidentally executing plugin binaries.
	MagicCookieKey = "SCION_PLUGIN"

	// MagicCookieValue is the magic cookie value for go-plugin handshake.
	MagicCookieValue = "scion-plugin-v1"

	// PluginBinaryPrefix is the naming convention prefix for plugin binaries.
	PluginBinaryPrefix = "scion-plugin-"
)

// PluginsConfig holds configuration for all plugins, loaded from settings.
type PluginsConfig struct {
	Broker map[string]PluginEntry `json:"broker,omitempty" yaml:"broker,omitempty" koanf:"broker"`
}

// PluginEntry holds configuration for a single plugin.
type PluginEntry struct {
	// Path is the explicit filesystem path to the plugin binary.
	// If empty, discovery will attempt to find it automatically.
	// Ignored when SelfManaged is true.
	Path string `json:"path,omitempty" yaml:"path,omitempty" koanf:"path"`

	// Config is an opaque key-value map passed to the plugin via Configure().
	// The plugin validates its own config and returns clear errors for invalid values.
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty" koanf:"config"`

	// SelfManaged indicates the plugin manages its own process lifecycle.
	// The Hub connects to the plugin's RPC server rather than starting it.
	// The plugin is responsible for its own startup and shutdown.
	SelfManaged bool `json:"self_managed,omitempty" yaml:"self_managed,omitempty" koanf:"self_managed"`

	// Address is the RPC address for self-managed plugins (e.g. "localhost:9090").
	// Required when SelfManaged is true.
	Address string `json:"address,omitempty" yaml:"address,omitempty" koanf:"address"`
}

// PluginInfo contains metadata reported by a plugin via the GetInfo() RPC call.
type PluginInfo struct {
	// Name is the plugin's self-reported name.
	Name string

	// Version is the plugin's version string.
	Version string

	// MinScionVersion is the minimum scion version this plugin targets.
	// Scion logs a warning if the plugin targets a newer version.
	MinScionVersion string

	// ChannelID is the message channel identifier this broker plugin handles.
	// When set, the FanOutEventBus uses this value (instead of the plugin's
	// registered name) to route outbound messages with a matching
	// msg.Channel field. For example, a plugin registered as "chat-app" can
	// set ChannelID to "gchat" so that messages with Channel="gchat" are
	// routed to it.
	ChannelID string

	// Capabilities lists optional capabilities the plugin supports.
	Capabilities []string
}
