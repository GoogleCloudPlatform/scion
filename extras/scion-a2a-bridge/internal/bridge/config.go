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

import (
	"time"
)

// Config holds the complete bridge configuration.
type Config struct {
	Bridge    BridgeConfig    `yaml:"bridge"`
	Hub       HubConfig       `yaml:"hub"`
	Plugin    PluginConfig    `yaml:"plugin"`
	Auth      AuthConfig      `yaml:"auth"`
	Projects  []ProjectConfig `yaml:"projects"`
	Groves    []ProjectConfig `yaml:"groves,omitempty"` // Legacy field for backward compatibility
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
	// Scheme selects the auth mode.
	// Supported: "apiKey" | "bearer" | "none" | "hubUAT" | "hubJWT" | "federation" | "oauth"
	Scheme string `yaml:"scheme"`

	// APIKey is the shared static key for "apiKey" and "bearer" schemes.
	// Not used for hubUAT, hubJWT, federation, or oauth.
	APIKey string `yaml:"api_key"`

	// UATCacheTTL is the UAT introspection cache TTL for hubUAT mode.
	// Default: 60s. Maximum: 300s.
	UATCacheTTL time.Duration `yaml:"uat_cache_ttl"`

	// OAuth configures OAuth 2.0 discovery and token validation when Scheme is "oauth".
	OAuth OAuthConfig `yaml:"oauth"`
}

// OAuthConfig holds OAuth 2.0 discovery and token validation settings.
type OAuthConfig struct {
	// AuthorizationURL is the OAuth 2.0 authorization endpoint presented in AgentCard.securitySchemes.
	// Default: "https://accounts.google.com/o/oauth2/v2/auth"
	AuthorizationURL string `yaml:"authorization_url"`

	// TokenURL is the OAuth 2.0 token endpoint presented in AgentCard.securitySchemes.
	// Default: "https://oauth2.googleapis.com/token"
	TokenURL string `yaml:"token_url"`

	// Scopes lists the required OAuth 2.0 scopes presented in AgentCard.securitySchemes and security.
	// Default: ["openid", "email", "profile"]
	Scopes []string `yaml:"scopes"`

	// UserInfoURL is the OIDC UserInfo or TokenInfo endpoint used to validate OAuth access tokens.
	// Default: "https://openidconnect.googleapis.com/v1/userinfo"
	// Set to "jwt_decode" to decode JWT payload claims locally without network call.
	UserInfoURL string `yaml:"userinfo_url"`

	// CacheTTL is the cache duration for validated OAuth tokens. Default: 60s.
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// ProjectConfig configures a project exposed via the bridge.
type ProjectConfig struct {
	Slug            string   `yaml:"slug" json:"slug"`
	DefaultTemplate string   `yaml:"default_template" json:"default_template"`
	AutoProvision   bool     `yaml:"auto_provision" json:"auto_provision"`
	ExposedAgents   []string `yaml:"exposed_agents" json:"exposed_agents"`
}

// GroveConfig is a legacy alias for ProjectConfig.
type GroveConfig = ProjectConfig

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
