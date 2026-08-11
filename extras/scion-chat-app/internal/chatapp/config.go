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

package chatapp

import (
	"fmt"
	"strconv"
	"time"
)

// Duration is a time.Duration that supports human-readable YAML values
// like "100ms", "5s", or "2m30s" via time.ParseDuration. Bare integers
// are treated as nanoseconds for backward compatibility.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler to parse human-readable durations.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("duration must be a string (e.g. \"100ms\") or integer nanoseconds: %w", err)
	}

	// Try human-readable duration first (e.g. "100ms", "5s").
	if parsed, err := time.ParseDuration(s); err == nil {
		d.Duration = parsed
		return nil
	}

	// Fall back to bare integer as nanoseconds for backward compatibility.
	if ns, err := strconv.ParseInt(s, 10, 64); err == nil {
		d.Duration = time.Duration(ns)
		return nil
	}

	return fmt.Errorf("invalid duration %q: must be a duration string (e.g. \"100ms\") or integer nanoseconds", s)
}

// Config holds the chat app configuration.
type Config struct {
	Hub           HubConfig           `yaml:"hub"`
	Plugin        PluginConfig        `yaml:"plugin"`
	Platforms     PlatformsConfig     `yaml:"platforms"`
	State         StateConfig         `yaml:"state"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Logging       LoggingConfig       `yaml:"logging"`
}

// HubConfig holds connection details for the Scion Hub.
type HubConfig struct {
	Endpoint         string `yaml:"endpoint"`
	User             string `yaml:"user"`
	Project          string `yaml:"project"`
	SigningKey       string `yaml:"signing_key"`
	SigningKeySecret string `yaml:"signing_key_secret"`
}

// PluginConfig holds broker plugin RPC server settings.
type PluginConfig struct {
	ListenAddress string `yaml:"listen_address"`
}

// PlatformsConfig holds per-platform adapter configurations.
type PlatformsConfig struct {
	GoogleChat GoogleChatConfig `yaml:"google_chat"`
	Slack      SlackConfig      `yaml:"slack"`
}

// GoogleChatConfig holds settings for the Google Chat adapter.
type GoogleChatConfig struct {
	Enabled             bool              `yaml:"enabled"`
	ProjectID           string            `yaml:"project_id"`
	Credentials         string            `yaml:"credentials"`
	ListenAddress       string            `yaml:"listen_address"`
	ExternalURL         string            `yaml:"external_url"`
	ServiceAccountEmail string            `yaml:"service_account_email"`
	CommandIDMap        map[string]string `yaml:"command_id_map"`
	SendQueueSize       int               `yaml:"send_queue_size"` // default 100
	SendMinDelay        Duration          `yaml:"send_min_delay"`  // default "100ms"
}

// SlackConfig holds settings for the Slack adapter (future).
type SlackConfig struct {
	Enabled       bool   `yaml:"enabled"`
	BotToken      string `yaml:"bot_token"`
	SigningSecret string `yaml:"signing_secret"`
	ListenAddress string `yaml:"listen_address"`
}

// StateConfig holds local state database settings.
type StateConfig struct {
	Database string `yaml:"database"`
}

// NotificationsConfig controls which broker activities trigger chat notifications.
type NotificationsConfig struct {
	TriggerActivities []string `yaml:"trigger_activities"`
}

// LoggingConfig controls structured logging output.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}
