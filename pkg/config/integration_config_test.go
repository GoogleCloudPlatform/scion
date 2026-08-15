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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	// Callers pass live maps (entry.Config from in-memory settings). Stripping
	// the original would erase the credential from settings and risk writing
	// the loss back to settings.yaml.
	if _, ok := inline["bot_token"]; !ok {
		t.Error("ResolvePluginConfig must not mutate the caller's inline map")
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

func TestResolvePluginConfig_SecretKeysStrippedFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"bot_token":          "secret-from-file",
		"TELEGRAM_BOT_TOKEN": "backend-name-from-file",
		"inbound_mode":       "poll",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := ResolvePluginConfig(path, map[string]string{"mode": "plugin"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := result["bot_token"]; ok {
		t.Error("secret config key bot_token should be stripped from config file")
	}
	if _, ok := result["TELEGRAM_BOT_TOKEN"]; ok {
		t.Error("backend secret key name should be stripped from config file")
	}
	if result["inbound_mode"] != "poll" {
		t.Errorf("file non-secret key should be present: got %q", result["inbound_mode"])
	}
}

func TestResolvePluginConfig_SecretKeysStrippedFromConfigFileWarns(t *testing.T) {
	// A credential silently vanishing from the resolved config is the failure
	// mode this warning exists to prevent — assert it actually fires.
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(path)
	if err := p.Save(context.Background(), map[string]string{
		"bot_token":    "secret-from-file",
		"inbound_mode": "poll",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolvePluginConfig(path, nil); err != nil {
		t.Fatal(err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "bot_token") {
		t.Errorf("expected a WARN naming the stripped key, got: %s", logged)
	}
	if !strings.Contains(logged, path) {
		t.Errorf("expected the warning to name the config file %q, got: %s", path, logged)
	}
	if strings.Contains(logged, "secret-from-file") {
		t.Errorf("warning must not leak the credential value, got: %s", logged)
	}

	// ResolvePluginConfig runs on request-serving paths; repeat calls must not
	// let a client drive unbounded log volume.
	buf.Reset()
	if _, err := ResolvePluginConfig(path, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("repeat resolve should not warn again, got: %s", buf.String())
	}
}

func TestResolvePluginConfig_StrippedSecretWarningDedupesByResolvedPath(t *testing.T) {
	// "~/.scion/plugin.yaml", "plugin.yaml" and the absolute path all name the
	// same file; the warning should fire once, against the resolved path.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, GlobalDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "plugin.yaml")
	p, _ := NewYAMLConfigProvider(abs)
	if err := p.Save(context.Background(), map[string]string{
		"bot_token": "secret-from-file",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	for _, spelling := range []string{abs, "~/" + GlobalDir + "/plugin.yaml", "plugin.yaml"} {
		if _, err := ResolvePluginConfig(spelling, nil); err != nil {
			t.Fatalf("resolve %q: %v", spelling, err)
		}
	}

	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Errorf("expected exactly 1 warning across path spellings, got %d: %s", got, buf.String())
	}
	if !strings.Contains(buf.String(), abs) {
		t.Errorf("warning should name the resolved path %q, got: %s", abs, buf.String())
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

// setupTestHome sets HOME to a temp dir and creates the .scion subdirectory
// so that GetGlobalDir() returns a predictable path. Returns the .scion dir
// and the settings.yaml path within it.
func setupTestHome(t *testing.T) (globalDir, settingsPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir = filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath = filepath.Join(globalDir, "settings.yaml")
	return globalDir, settingsPath
}

func TestAddPluginToMessageBrokerTypes_NewPlugin(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	// Seed a minimal settings.yaml.
	if err := os.WriteFile(settingsPath, []byte("server:\n  plugins: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := AddPluginToMessageBrokerTypes("telegram"); err != nil {
		t.Fatalf("AddPluginToMessageBrokerTypes() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Parse back and verify.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	mb := server["message_broker"].(map[string]interface{})

	if enabled, ok := mb["enabled"].(bool); !ok || !enabled {
		t.Error("expected message_broker.enabled = true")
	}

	typesRaw, ok := mb["types"].([]interface{})
	if !ok {
		t.Fatal("expected message_broker.types to be a list")
	}
	if len(typesRaw) != 1 || typesRaw[0] != "telegram" {
		t.Errorf("expected types=[telegram], got %v", typesRaw)
	}
}

func TestAddPluginToMessageBrokerTypes_Idempotent(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	if err := os.WriteFile(settingsPath, []byte("server:\n  message_broker:\n    enabled: true\n    types:\n      - telegram\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := AddPluginToMessageBrokerTypes("telegram"); err != nil {
		t.Fatalf("AddPluginToMessageBrokerTypes() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	mb := server["message_broker"].(map[string]interface{})
	typesRaw := mb["types"].([]interface{})
	if len(typesRaw) != 1 {
		t.Errorf("expected types list to remain length 1 (idempotent), got %v", typesRaw)
	}
}

func TestAddPluginToMessageBrokerTypes_PluginPresentButDisabled(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	// Plugin is already in types but enabled is false.
	if err := os.WriteFile(settingsPath, []byte("server:\n  message_broker:\n    enabled: false\n    types:\n      - telegram\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := AddPluginToMessageBrokerTypes("telegram"); err != nil {
		t.Fatalf("AddPluginToMessageBrokerTypes() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	mb := server["message_broker"].(map[string]interface{})

	if enabled, ok := mb["enabled"].(bool); !ok || !enabled {
		t.Error("expected message_broker.enabled = true after re-enabling")
	}

	typesRaw := mb["types"].([]interface{})
	if len(typesRaw) != 1 || typesRaw[0] != "telegram" {
		t.Errorf("expected types=[telegram] (no duplicate), got %v", typesRaw)
	}
}

func TestAddPluginToMessageBrokerTypes_AppendsSecondPlugin(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	if err := os.WriteFile(settingsPath, []byte("server:\n  message_broker:\n    enabled: true\n    types:\n      - telegram\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := AddPluginToMessageBrokerTypes("discord"); err != nil {
		t.Fatalf("AddPluginToMessageBrokerTypes() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	mb := server["message_broker"].(map[string]interface{})
	typesRaw := mb["types"].([]interface{})
	if len(typesRaw) != 2 {
		t.Fatalf("expected 2 types, got %v", typesRaw)
	}
	if typesRaw[0] != "telegram" || typesRaw[1] != "discord" {
		t.Errorf("expected [telegram discord], got %v", typesRaw)
	}
}

func TestAddPluginToMessageBrokerTypes_NoSettingsFile(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	// No settings.yaml exists yet — the function should create one.
	if err := AddPluginToMessageBrokerTypes("slack"); err != nil {
		t.Fatalf("AddPluginToMessageBrokerTypes() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	mb := server["message_broker"].(map[string]interface{})

	if enabled, ok := mb["enabled"].(bool); !ok || !enabled {
		t.Error("expected message_broker.enabled = true")
	}

	typesRaw := mb["types"].([]interface{})
	if len(typesRaw) != 1 || typesRaw[0] != "slack" {
		t.Errorf("expected types=[slack], got %v", typesRaw)
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

func TestPluginSecretKeyMap_A2ABridge(t *testing.T) {
	mappings, ok := PluginSecretKeyMap["a2a-bridge"]
	if !ok {
		t.Fatal("a2a-bridge not in PluginSecretKeyMap")
	}
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].SecretKey != SecretA2AAPIKey {
		t.Errorf("expected SecretKey=%q, got %q", SecretA2AAPIKey, mappings[0].SecretKey)
	}
	if mappings[0].ConfigKey != "api_key" {
		t.Errorf("expected ConfigKey=%q, got %q", "api_key", mappings[0].ConfigKey)
	}
}

func TestIsSecretConfigKey_A2AApiKey(t *testing.T) {
	if !IsSecretConfigKey("api_key") {
		t.Error("expected api_key to be a secret config key")
	}
}

func TestAddSelfManagedPluginToSettings(t *testing.T) {
	_, settingsPath := setupTestHome(t)

	// Seed a minimal settings.yaml.
	if err := os.WriteFile(settingsPath, []byte("server:\n  plugins: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	entry := SelfManagedPluginEntry{
		Name:       "a2a-bridge",
		Address:    "localhost:9090",
		ConfigFile: "~/.scion/scion-a2a-bridge-admin.yaml",
	}

	if err := AddSelfManagedPluginToSettings(entry); err != nil {
		t.Fatalf("AddSelfManagedPluginToSettings() error: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	server := raw["server"].(map[string]interface{})
	plugins := server["plugins"].(map[string]interface{})
	broker := plugins["broker"].(map[string]interface{})

	a2aEntry, ok := broker["a2a-bridge"].(map[string]interface{})
	if !ok {
		t.Fatal("a2a-bridge entry not found in broker map")
	}

	if sm, ok := a2aEntry["self_managed"].(bool); !ok || !sm {
		t.Error("expected self_managed = true")
	}
	if mode, ok := a2aEntry["mode"].(string); !ok || mode != "self-managed" {
		t.Errorf("expected mode = self-managed, got %v", a2aEntry["mode"])
	}
	if addr, ok := a2aEntry["address"].(string); !ok || addr != "localhost:9090" {
		t.Errorf("expected address = localhost:9090, got %v", a2aEntry["address"])
	}
	if cf, ok := a2aEntry["config_file"].(string); !ok || cf != "~/.scion/scion-a2a-bridge-admin.yaml" {
		t.Errorf("expected config_file, got %v", a2aEntry["config_file"])
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
