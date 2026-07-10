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

// Package opsettings defines the Layer-1 operational settings section registry
// for the two-tier settings architecture. Each section maps to a Go struct,
// a JSON-schema fragment, and a set of koanf key paths — providing the single
// source of truth for Layer-0 vs Layer-1 classification.
package opsettings

import (
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// AccessSettings holds Layer-1 access control settings.
type AccessSettings struct {
	AdminEmails       []string `json:"admin_emails,omitempty"`
	UserAccessMode    string   `json:"user_access_mode,omitempty"`
	AuthorizedDomains []string `json:"authorized_domains,omitempty"`
}

// LifecycleSettings holds Layer-1 agent lifecycle settings.
type LifecycleSettings struct {
	AutoSuspendStalled    *bool  `json:"auto_suspend_stalled,omitempty"`
	SoftDeleteRetention   string `json:"soft_delete_retention,omitempty"`
	SoftDeleteRetainFiles *bool  `json:"soft_delete_retain_files,omitempty"`
}

// MaintenanceSettings holds Layer-1 maintenance/admin-mode settings.
type MaintenanceSettings struct {
	AdminMode          bool   `json:"admin_mode,omitempty"`
	MaintenanceMessage string `json:"maintenance_message,omitempty"`
}

// TelemetrySettings holds the Layer-1 telemetry configuration.
// It reuses the existing V1TelemetryConfig to preserve full fidelity.
type TelemetrySettings struct {
	config.V1TelemetryConfig
}

// AgentDefaultsSettings holds Layer-1 default agent configuration.
type AgentDefaultsSettings struct {
	DefaultTemplate      string            `json:"default_template,omitempty"`
	DefaultHarnessConfig string            `json:"default_harness_config,omitempty"`
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`
}

// EndpointsSettings holds Layer-1 endpoint configuration.
type EndpointsSettings struct {
	PublicURL     string `json:"public_url,omitempty"`
	HubName       string `json:"hub_name,omitempty"`
	ImageRegistry string `json:"image_registry,omitempty"`
}

// GitHubAppSettings holds the Layer-1 GitHub App configuration.
// Secret material (private_key, webhook_secret) is excluded — stays in secret backend.
type GitHubAppSettings struct {
	AppID           int64  `json:"app_id,omitempty"`
	APIBaseURL      string `json:"api_base_url,omitempty"`
	WebhooksEnabled bool   `json:"webhooks_enabled,omitempty"`
	InstallationURL string `json:"installation_url,omitempty"`
	PrivateKeyPath  string `json:"private_key_path,omitempty"`
}

// NotificationsSettings holds the Layer-1 notification channel configuration.
type NotificationsSettings struct {
	NotificationChannels []config.V1NotificationChannelConfig `json:"notification_channels,omitempty"`
}
