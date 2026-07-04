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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadStandaloneConfig_RejectsPollMode(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("INBOUND_MODE", "poll")

	_, err := loadStandaloneConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HA/standalone Telegram requires webhook mode")
}

func TestLoadStandaloneConfig_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")

	_, err := loadStandaloneConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL is required")
}

func TestLoadStandaloneConfig_RequiresBotToken(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")

	_, err := loadStandaloneConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BOT_TOKEN is required")
}

func TestLoadStandaloneConfig_RequiresWebhookURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("INBOUND_MODE", "")

	_, err := loadStandaloneConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WEBHOOK_URL is required")
}

func TestLoadStandaloneConfig_ValidConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("WEBHOOK_SECRET", "secret123")
	t.Setenv("WEBHOOK_LISTEN", ":8080")
	t.Setenv("INBOUND_MODE", "")

	cfg, err := loadStandaloneConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://localhost/test", cfg.DatabaseURL)
	assert.Equal(t, "test-token", cfg.BotToken)
	assert.Equal(t, "https://example.com/webhook", cfg.WebhookURL)
	assert.Equal(t, "secret123", cfg.WebhookSecret)
	assert.Equal(t, ":8080", cfg.WebhookListen)
	assert.Equal(t, "webhook", cfg.InboundMode)
}

func TestLoadStandaloneConfig_DefaultWebhookListen(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("WEBHOOK_LISTEN", "")
	t.Setenv("INBOUND_MODE", "")

	cfg, err := loadStandaloneConfig()
	require.NoError(t, err)
	assert.Equal(t, ":9094", cfg.WebhookListen)
}

func TestLoadStandaloneConfig_WebhookModeExplicit(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("INBOUND_MODE", "webhook")

	cfg, err := loadStandaloneConfig()
	require.NoError(t, err)
	assert.Equal(t, "webhook", cfg.InboundMode)
}

func TestLoadStandaloneConfig_RejectsPollCaseInsensitive(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("INBOUND_MODE", "Poll")

	_, err := loadStandaloneConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HA/standalone Telegram requires webhook mode")
}
