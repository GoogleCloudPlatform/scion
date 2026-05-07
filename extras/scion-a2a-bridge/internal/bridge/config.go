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

package bridge

import "time"

// Config holds the complete bridge configuration.
type Config struct {
	Bridge    BridgeConfig    `yaml:"bridge"`
	Hub       HubConfig       `yaml:"hub"`
	Plugin    PluginConfig    `yaml:"plugin"`
	Auth      AuthConfig      `yaml:"auth"`
	Groves    []GroveConfig   `yaml:"groves"`
	State     StateConfig     `yaml:"state"`
	Timeouts  TimeoutConfig   `yaml:"timeouts"`
	Logging   LoggingConfig   `yaml:"logging"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
}

// BridgeConfig holds the A2A protocol server settings.
type BridgeConfig struct {
	ListenAddress  string         `yaml:"listen_address"`
	ExternalURL    string         `yaml:"external_url"`
	MaxSubscribers int            `yaml:"max_subscribers"`
	Provider       ProviderConfig `yaml:"provider"`
}

// ProviderConfig describes the bridge operator.
type ProviderConfig struct {
	Organization string `yaml:"organization"`
	URL          string `yaml:"url"`
}

// HubConfig holds connection details for the Scion Hub.
type HubConfig struct {
	Endpoint         string `yaml:"endpoint"`
	User             string `yaml:"user"`
	UserID           string `yaml:"user_id"`
	SigningKey       string `yaml:"signing_key"`
	SigningKeySecret string `yaml:"signing_key_secret"`
}

// PluginConfig holds broker plugin RPC server settings.
type PluginConfig struct {
	ListenAddress string `yaml:"listen_address"`
	AllowRemote   bool   `yaml:"allow_remote"`
}

// AuthConfig holds external authentication settings for A2A clients.
type AuthConfig struct {
	Scheme string `yaml:"scheme"`
	APIKey string `yaml:"api_key"`
}

// GroveConfig configures a grove exposed via the bridge.
type GroveConfig struct {
	Slug            string   `yaml:"slug"`
	DefaultTemplate string   `yaml:"default_template"`
	AutoProvision   bool     `yaml:"auto_provision"`
	ExposedAgents   []string `yaml:"exposed_agents"`
}

// StateConfig holds local state database settings.
type StateConfig struct {
	Database string `yaml:"database"`
}

// TimeoutConfig holds various timeout durations.
type TimeoutConfig struct {
	SendMessage  time.Duration `yaml:"send_message"`
	SSEKeepalive time.Duration `yaml:"sse_keepalive"`
	PushRetryMax int           `yaml:"push_retry_max"`
}

// LoggingConfig controls structured logging output.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// RateLimitConfig controls request rate limiting.
type RateLimitConfig struct {
	Enabled        bool    `yaml:"enabled"`
	RequestsPerSec float64 `yaml:"requests_per_sec"`
	Burst          int     `yaml:"burst"`
	TrustProxy     bool    `yaml:"trust_proxy"`
}
