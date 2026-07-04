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

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)
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

// DeploymentMode describes how a plugin is deployed and communicates with the hub.
type DeploymentMode string

const (
	// DeploymentModePlugin is the default: hub-managed go-plugin subprocess.
	DeploymentModePlugin DeploymentMode = "plugin"

	// DeploymentModeExternal is the legacy self-managed mode: external process
	// using go-plugin net/rpc over raw TCP.
	DeploymentModeExternal DeploymentMode = "external"

	// DeploymentModeHA is the gRPC standalone mode for HA deployments.
	DeploymentModeHA DeploymentMode = "ha"
)
// PluginEntry holds configuration for a single plugin.
type PluginEntry struct {
	// Path is the explicit filesystem path to the plugin binary.
	// If empty, discovery will attempt to find it automatically.
	// Ignored when SelfManaged is true or Mode is "grpc".
	Path string `json:"path,omitempty" yaml:"path,omitempty" koanf:"path"`

	// Config is an opaque key-value map passed to the plugin via Configure().
	// The plugin validates its own config and returns clear errors for invalid values.
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty" koanf:"config"`

	// ConfigFile is the path to a standalone YAML config file for this plugin.
	// When set, the hub reads non-sensitive settings from this file and merges
	// them into the Config map before calling Configure(). Secrets are loaded
	// separately via SecretBackend. If empty, the inline Config map is used
	// as-is (backward compatible).
	ConfigFile string `json:"config_file,omitempty" yaml:"config_file,omitempty" koanf:"config_file"`

	// SelfManaged indicates the plugin manages its own process lifecycle.
	// The Hub connects to the plugin's RPC server rather than starting it.
	// The plugin is responsible for its own startup and shutdown.
	// Deprecated: use Mode="grpc" for new standalone deployments.
	SelfManaged bool `json:"self_managed,omitempty" yaml:"self_managed,omitempty" koanf:"self_managed"`

	// Mode selects the transport: "" or "plugin" (default go-plugin subprocess),
	// "self-managed" (legacy external net/rpc), "grpc" (standalone gRPC service).
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`

	// Address is the RPC address for self-managed or gRPC plugins (e.g. "localhost:9090").
	// Required when SelfManaged is true or Mode is "grpc".
	Address string `json:"address,omitempty" yaml:"address,omitempty" koanf:"address"`

	// TLS configuration for gRPC mode connections.

	// TLSCertFile is the path to the client TLS certificate for mTLS.
	TLSCertFile string `json:"tls_cert_file,omitempty" yaml:"tls_cert_file,omitempty" koanf:"tls_cert_file"`

	// TLSKeyFile is the path to the client TLS private key for mTLS.
	TLSKeyFile string `json:"tls_key_file,omitempty" yaml:"tls_key_file,omitempty" koanf:"tls_key_file"`

	// TLSCAFile is the path to the CA certificate for verifying the server.
	TLSCAFile string `json:"tls_ca_file,omitempty" yaml:"tls_ca_file,omitempty" koanf:"tls_ca_file"`

	// TLSSkipVerify disables TLS certificate verification (for development).
	TLSSkipVerify bool `json:"tls_skip_verify,omitempty" yaml:"tls_skip_verify,omitempty" koanf:"tls_skip_verify"`
}

// ResolvedDeploymentMode returns the deployment mode for this entry,
// considering both the new Mode field and the legacy SelfManaged flag.
func (e *PluginEntry) ResolvedDeploymentMode() DeploymentMode {
	switch e.Mode {
	case "grpc":
		return DeploymentModeHA
	case "self-managed":
		return DeploymentModeExternal
	case "plugin":
		return DeploymentModePlugin
	default:
		if e.SelfManaged {
			return DeploymentModeExternal
		}
		return DeploymentModePlugin
	}
}

// GRPCBrokerClient is the interface that a gRPC broker adapter must implement.
// It combines eventbus.EventBus with broker-specific methods (Configure, GetInfo,
// HealthCheck) so the Manager can use it without importing the grpcbroker package.
type GRPCBrokerClient interface {
	// Publish sends a message to the remote broker.
	Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error
	// Subscribe registers a handler for messages matching a topic pattern.
	Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error)
	// Close shuts down the connection.
	Close() error
	// Configure sends configuration to the remote broker.
	Configure(config map[string]string) error
	// GetInfo retrieves plugin metadata.
	GetInfo() (*PluginInfo, error)
	// HealthCheck retrieves health status.
	HealthCheck() (*HealthStatus, error)
	// OnReconnect registers a callback invoked after a successful reconnect.
	OnReconnect(fn func())
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
