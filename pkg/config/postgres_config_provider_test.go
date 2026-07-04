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

package config_test

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestPostgresConfigProvider_LoadEmpty(t *testing.T) {
	client := enttest.NewClient(t)
	p := config.NewPostgresConfigProvider(client, "discord")

	cfg, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() should not error for missing row: %v", err)
	}
	if len(cfg) != 0 {
		t.Fatalf("expected empty map, got %v", cfg)
	}
}

func TestPostgresConfigProvider_SaveAndLoad(t *testing.T) {
	client := enttest.NewClient(t)
	p := config.NewPostgresConfigProvider(client, "discord")
	ctx := context.Background()

	input := map[string]string{
		"application_id": "12345",
		"guild_id":       "67890",
	}

	if err := p.Save(ctx, input); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	loaded, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for k, want := range input {
		if got := loaded[k]; got != want {
			t.Errorf("key %q: got %q, want %q", k, got, want)
		}
	}
}

func TestPostgresConfigProvider_SaveUpsert(t *testing.T) {
	client := enttest.NewClient(t)
	p := config.NewPostgresConfigProvider(client, "telegram")
	ctx := context.Background()

	// Initial save
	if err := p.Save(ctx, map[string]string{"inbound_mode": "poll"}); err != nil {
		t.Fatalf("first Save() failed: %v", err)
	}

	// Upsert with updated value
	if err := p.Save(ctx, map[string]string{"inbound_mode": "webhook", "webhook_url": "https://example.com"}); err != nil {
		t.Fatalf("second Save() failed: %v", err)
	}

	loaded, err := p.Load(ctx)
	if err != nil {
		t.Fatalf("Load() after upsert failed: %v", err)
	}

	if got := loaded["inbound_mode"]; got != "webhook" {
		t.Errorf("inbound_mode: got %q, want %q", got, "webhook")
	}
	if got := loaded["webhook_url"]; got != "https://example.com" {
		t.Errorf("webhook_url: got %q, want %q", got, "https://example.com")
	}
}

func TestPostgresConfigProvider_IsolatedByIntegration(t *testing.T) {
	client := enttest.NewClient(t)
	discordP := config.NewPostgresConfigProvider(client, "discord")
	telegramP := config.NewPostgresConfigProvider(client, "telegram")
	ctx := context.Background()

	if err := discordP.Save(ctx, map[string]string{"app_id": "discord-123"}); err != nil {
		t.Fatalf("discord Save() failed: %v", err)
	}
	if err := telegramP.Save(ctx, map[string]string{"bot_name": "telegram-bot"}); err != nil {
		t.Fatalf("telegram Save() failed: %v", err)
	}

	discordCfg, err := discordP.Load(ctx)
	if err != nil {
		t.Fatalf("discord Load() failed: %v", err)
	}
	telegramCfg, err := telegramP.Load(ctx)
	if err != nil {
		t.Fatalf("telegram Load() failed: %v", err)
	}

	if discordCfg["app_id"] != "discord-123" {
		t.Errorf("discord config wrong: %v", discordCfg)
	}
	if _, ok := discordCfg["bot_name"]; ok {
		t.Error("discord config should not contain telegram keys")
	}

	if telegramCfg["bot_name"] != "telegram-bot" {
		t.Errorf("telegram config wrong: %v", telegramCfg)
	}
	if _, ok := telegramCfg["app_id"]; ok {
		t.Error("telegram config should not contain discord keys")
	}
}

func TestPostgresConfigProvider_ImplementsInterface(t *testing.T) {
	client := enttest.NewClient(t)
	var _ config.IntegrationConfigProvider = config.NewPostgresConfigProvider(client, "test")
}
