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

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestYAMLConfigProvider_LoadNonExistent(t *testing.T) {
	p, err := NewYAMLConfigProvider(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	config, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() should not error for missing file: %v", err)
	}
	if len(config) != 0 {
		t.Fatalf("expected empty map, got %v", config)
	}
}

func TestYAMLConfigProvider_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-config.yaml")
	p, err := NewYAMLConfigProvider(path)
	if err != nil {
		t.Fatal(err)
	}

	input := map[string]string{
		"bot_token":    "secret-token",
		"inbound_mode": "webhook",
		"webhook_url":  "https://example.com/webhook",
	}

	if err := p.Save(context.Background(), input); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	// Verify file was created with restricted permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 permissions, got %o", info.Mode().Perm())
	}

	loaded, err := p.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	for k, want := range input {
		if got := loaded[k]; got != want {
			t.Errorf("key %q: got %q, want %q", k, got, want)
		}
	}
}

func TestYAMLConfigProvider_EmptyPath(t *testing.T) {
	_, err := NewYAMLConfigProvider("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestYAMLConfigProvider_TildePath(t *testing.T) {
	p, err := NewYAMLConfigProvider("~/configs/telegram.yaml")
	if err != nil {
		t.Fatal(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(home, "configs", "telegram.yaml")
	if p.Path() != expected {
		t.Errorf("expected path %q, got %q", expected, p.Path())
	}
}

func TestLoadPluginConfigFile_Empty(t *testing.T) {
	inline := map[string]string{"key": "value"}
	result, err := LoadPluginConfigFile("", inline)
	if err != nil {
		t.Fatal(err)
	}
	if result["key"] != "value" {
		t.Errorf("expected inline config passthrough, got %v", result)
	}
}

func TestLoadPluginConfigFile_MergeWithInlineOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"inbound_mode": "poll",
		"db_path":      "/tmp/test.db",
	}); err != nil {
		t.Fatal(err)
	}

	inline := map[string]string{
		"inbound_mode": "webhook",
	}

	result, err := LoadPluginConfigFile(path, inline)
	if err != nil {
		t.Fatal(err)
	}

	if result["inbound_mode"] != "webhook" {
		t.Errorf("inline should override file: got %q", result["inbound_mode"])
	}
	if result["db_path"] != "/tmp/test.db" {
		t.Errorf("file config should be included: got %q", result["db_path"])
	}
}

func TestResolvePluginConfig_NoConfigFile_FallbackToInline(t *testing.T) {
	inline := map[string]string{
		"inbound_mode": "webhook",
		"db_path":      "/tmp/test.db",
		"mode":         "plugin",
	}
	result, err := ResolvePluginConfig("", inline)
	if err != nil {
		t.Fatal(err)
	}
	if result["inbound_mode"] != "webhook" {
		t.Errorf("expected inline fallback, got %q", result["inbound_mode"])
	}
	if result["mode"] != "plugin" {
		t.Errorf("expected wiring key preserved, got %q", result["mode"])
	}
}

func TestResolvePluginConfig_FileWinsForNonWiringKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"inbound_mode": "poll",
		"db_path":      "/tmp/file.db",
	}); err != nil {
		t.Fatal(err)
	}

	inline := map[string]string{
		"inbound_mode": "webhook",
		"db_path":      "/tmp/inline.db",
		"mode":         "plugin",
		"address":      "localhost:9090",
	}

	result, err := ResolvePluginConfig(path, inline)
	if err != nil {
		t.Fatal(err)
	}

	if result["inbound_mode"] != "poll" {
		t.Errorf("file should win for non-wiring key: got %q, want %q", result["inbound_mode"], "poll")
	}
	if result["db_path"] != "/tmp/file.db" {
		t.Errorf("file should win for non-wiring key: got %q, want %q", result["db_path"], "/tmp/file.db")
	}
	if result["mode"] != "plugin" {
		t.Errorf("wiring key should come from inline: got %q, want %q", result["mode"], "plugin")
	}
	if result["address"] != "localhost:9090" {
		t.Errorf("wiring key should come from inline: got %q, want %q", result["address"], "localhost:9090")
	}
}

func TestResolvePluginConfig_InlineNonWiringIgnoredWithConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"db_path": "/tmp/file.db",
	}); err != nil {
		t.Fatal(err)
	}

	inline := map[string]string{
		"custom_setting": "should-be-ignored",
		"mode":           "plugin",
	}

	result, err := ResolvePluginConfig(path, inline)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := result["custom_setting"]; ok {
		t.Error("non-wiring inline key should be ignored when config_file is set")
	}
	if result["mode"] != "plugin" {
		t.Errorf("wiring key should be preserved: got %q", result["mode"])
	}
	if result["db_path"] != "/tmp/file.db" {
		t.Errorf("file key should be present: got %q", result["db_path"])
	}
}

func TestResolvePluginConfig_SecretKeysStrippedFromInline(t *testing.T) {
	inline := map[string]string{
		"bot_token":      "stale-token",
		"signing_secret": "old-secret",
		"inbound_mode":   "webhook",
		"mode":           "plugin",
	}

	result, err := ResolvePluginConfig("", inline)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := result["bot_token"]; ok {
		t.Error("secret config key bot_token should be stripped from inline")
	}
	if _, ok := result["signing_secret"]; ok {
		t.Error("secret config key signing_secret should be stripped from inline")
	}
	if result["inbound_mode"] != "webhook" {
		t.Errorf("non-secret key should be preserved: got %q", result["inbound_mode"])
	}
	if result["mode"] != "plugin" {
		t.Errorf("wiring key should be preserved: got %q", result["mode"])
	}
}

func TestResolvePluginConfig_SecretKeysStrippedWithConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"inbound_mode": "poll",
	}); err != nil {
		t.Fatal(err)
	}

	inline := map[string]string{
		"bot_token": "stale-inline-token",
		"mode":      "plugin",
	}

	result, err := ResolvePluginConfig(path, inline)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := result["bot_token"]; ok {
		t.Error("secret config key bot_token should be stripped from inline even with config_file")
	}
	if result["inbound_mode"] != "poll" {
		t.Errorf("file non-wiring key should be present: got %q", result["inbound_mode"])
	}
}

func TestResolvePluginConfig_BackendKeyNamesStrippedFromBothSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"TELEGRAM_BOT_TOKEN": "should-not-survive",
		"inbound_mode":       "poll",
	}); err != nil {
		t.Fatal(err)
	}

	inline := map[string]string{
		"DISCORD_BOT_TOKEN": "should-not-survive",
		"mode":              "plugin",
	}

	result, err := ResolvePluginConfig(path, inline)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "telegram_bot_token", "DISCORD_BOT_TOKEN", "discord_bot_token"} {
		if _, ok := result[key]; ok {
			t.Errorf("backend secret key %q should have been filtered", key)
		}
	}
	if result["inbound_mode"] != "poll" {
		t.Errorf("non-secret file key should survive: got %q", result["inbound_mode"])
	}
}

func TestIsSecretConfigKey(t *testing.T) {
	for _, key := range []string{"bot_token", "webhook_secret", "public_key", "app_token", "signing_secret", "signing_key"} {
		if !IsSecretConfigKey(key) {
			t.Errorf("expected %q to be a secret config key", key)
		}
	}
	for _, key := range []string{"inbound_mode", "db_path", "mode", "address"} {
		if IsSecretConfigKey(key) {
			t.Errorf("expected %q to NOT be a secret config key", key)
		}
	}
}

func TestLoadPluginConfigFile_FiltersSecretKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"bot_token":          "should-stay",
		"TELEGRAM_BOT_TOKEN": "should-be-filtered",
		"telegram_bot_token": "should-be-filtered",
		"DISCORD_BOT_TOKEN":  "should-be-filtered",
		"GCHAT_SIGNING_KEY":  "should-be-filtered",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := LoadPluginConfigFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	// bot_token is a plugin-level config key, not a well-known secret constant
	if result["bot_token"] != "should-stay" {
		t.Errorf("bot_token should be preserved: got %q", result["bot_token"])
	}

	for _, key := range []string{
		"TELEGRAM_BOT_TOKEN", "telegram_bot_token",
		"DISCORD_BOT_TOKEN", "GCHAT_SIGNING_KEY",
	} {
		if _, ok := result[key]; ok {
			t.Errorf("secret key %q should have been filtered", key)
		}
	}
}
