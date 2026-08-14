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

package cmd

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeSecretBackend is an in-memory secret.SecretBackend for migration tests.
// Only Get, Set and HubID are exercised by migratePluginSecrets.
type fakeSecretBackend struct {
	hubID        string
	values       map[string]string
	descriptions map[string]string
	sets         int
}

func newFakeSecretBackend() *fakeSecretBackend {
	return &fakeSecretBackend{hubID: "hub-1", values: map[string]string{}, descriptions: map[string]string{}}
}

func (f *fakeSecretBackend) HubID() string { return f.hubID }

func (f *fakeSecretBackend) Get(_ context.Context, name, _, _ string) (*secret.SecretWithValue, error) {
	v, ok := f.values[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name}, Value: v}, nil
}

func (f *fakeSecretBackend) Set(_ context.Context, in *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	f.sets++
	f.descriptions[in.Name] = in.Description
	_, existed := f.values[in.Name]
	f.values[in.Name] = in.Value
	return !existed, &secret.SecretMeta{Name: in.Name}, nil
}

func (f *fakeSecretBackend) Delete(context.Context, string, string, string) error { return nil }

func (f *fakeSecretBackend) List(context.Context, secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (f *fakeSecretBackend) GetMeta(context.Context, string, string, string) (*secret.SecretMeta, error) {
	return nil, store.ErrNotFound
}

func (f *fakeSecretBackend) Resolve(context.Context, string, string, string, *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}

// writePluginConfigFile writes a per-plugin YAML config file and returns its path.
func writePluginConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scion-telegram.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// captureLog collects everything written to the standard logger while fn runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestMigratePluginSecrets_FromConfigFile(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\ninbound_mode: \"webhook\"\n")

	migratePluginSecrets(context.Background(), sb, "telegram", nil, cfgFile)

	if got := sb.values[config.SecretTelegramBotToken]; got != "file-token" {
		t.Errorf("expected bot_token from config file to be migrated, got %q", got)
	}
}

// The secret description must name the config file without exposing the host
// path, which would leak the operator's username and directory layout.
func TestMigratePluginSecrets_DescriptionOmitsHostPath(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\n")

	migratePluginSecrets(context.Background(), sb, "telegram", nil, cfgFile)

	desc := sb.descriptions[config.SecretTelegramBotToken]
	if !strings.Contains(desc, "scion-telegram.yaml") {
		t.Errorf("expected description to name the config file, got %q", desc)
	}
	if strings.Contains(desc, filepath.Dir(cfgFile)) {
		t.Errorf("description must not contain the host path, got %q", desc)
	}
}

func TestMigratePluginSecrets_FromInlineConfig(t *testing.T) {
	sb := newFakeSecretBackend()
	inline := map[string]string{"bot_token": "inline-token"}

	migratePluginSecrets(context.Background(), sb, "telegram", inline, "")

	if got := sb.values[config.SecretTelegramBotToken]; got != "inline-token" {
		t.Errorf("expected inline bot_token to be migrated, got %q", got)
	}
}

// When config_file is set, ResolvePluginConfig runs the plugin on the file's
// value, so that is the credential the backend must receive.
func TestMigratePluginSecrets_ConfigFileWinsOverInline(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\n")
	inline := map[string]string{"bot_token": "inline-token", "webhook_secret": "inline-secret"}

	migratePluginSecrets(context.Background(), sb, "telegram", inline, cfgFile)

	if got := sb.values[config.SecretTelegramBotToken]; got != "file-token" {
		t.Errorf("expected config file value to win, got %q", got)
	}
	// Keys absent from the file still migrate from inline config.
	if got := sb.values[config.SecretTelegramWebhookKey]; got != "inline-secret" {
		t.Errorf("expected webhook_secret from inline config, got %q", got)
	}
}

// An empty value in the config file is not a value — inline is used instead.
func TestMigratePluginSecrets_EmptyConfigFileValueFallsBackToInline(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, "bot_token: \"\"\n")
	inline := map[string]string{"bot_token": "inline-token"}

	migratePluginSecrets(context.Background(), sb, "telegram", inline, cfgFile)

	if got := sb.values[config.SecretTelegramBotToken]; got != "inline-token" {
		t.Errorf("expected fallback to inline value, got %q", got)
	}
}

// Backend-style key names (TELEGRAM_BOT_TOKEN) are stripped from file config by
// LoadPluginConfigFile, so they are not a migration source — only the plugin
// config key (bot_token) is.
func TestMigratePluginSecrets_IgnoresBackendKeyNameInConfigFile(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, config.SecretTelegramBotToken+": \"backend-style-token\"\n")

	migratePluginSecrets(context.Background(), sb, "telegram", nil, cfgFile)

	if sb.sets != 0 {
		t.Errorf("expected no migration from a backend-style key, got %d Set calls", sb.sets)
	}
}

func TestMigratePluginSecrets_MissingConfigFile(t *testing.T) {
	sb := newFakeSecretBackend()
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	inline := map[string]string{"bot_token": "inline-token"}

	migratePluginSecrets(context.Background(), sb, "telegram", inline, missing)

	if got := sb.values[config.SecretTelegramBotToken]; got != "inline-token" {
		t.Errorf("missing config file should not block inline migration, got %q", got)
	}
}

func TestMigratePluginSecrets_DoesNotOverwriteExisting(t *testing.T) {
	sb := newFakeSecretBackend()
	sb.values[config.SecretTelegramBotToken] = "already-migrated"
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\n")

	migratePluginSecrets(context.Background(), sb, "telegram", map[string]string{"bot_token": "inline-token"}, cfgFile)

	if got := sb.values[config.SecretTelegramBotToken]; got != "already-migrated" {
		t.Errorf("existing secret must not be overwritten, got %q", got)
	}
	if sb.sets != 0 {
		t.Errorf("expected no Set calls, got %d", sb.sets)
	}
}

func TestMigratePluginSecrets_UnknownPluginNoOp(t *testing.T) {
	sb := newFakeSecretBackend()
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\n")

	migratePluginSecrets(context.Background(), sb, "not-a-known-plugin", map[string]string{"bot_token": "x"}, cfgFile)

	if sb.sets != 0 {
		t.Errorf("expected no migration for unknown plugin, got %d Set calls", sb.sets)
	}
}

func TestMigratePluginSecrets_MalformedConfigFileFallsBackToInline(t *testing.T) {
	sb := newFakeSecretBackend()
	// bot_token appears only in the file, webhook_secret only inline: if the file
	// parsed, bot_token would migrate too.
	cfgFile := writePluginConfigFile(t, "bot_token: \"file-token\"\nwebhook_secret: [unterminated\n")
	inline := map[string]string{"webhook_secret": "inline-secret"}

	out := captureLog(t, func() {
		migratePluginSecrets(context.Background(), sb, "telegram", inline, cfgFile)
	})

	if got := sb.values[config.SecretTelegramWebhookKey]; got != "inline-secret" {
		t.Errorf("malformed config file should not block inline migration, got %q", got)
	}
	if got, ok := sb.values[config.SecretTelegramBotToken]; ok {
		t.Errorf("unparseable config file must yield no file values, got %q", got)
	}
	if !strings.Contains(out, "failed to read config file") {
		t.Errorf("expected a warning about the unreadable config file, got log: %q", out)
	}
}

// initPluginManager must run the migration for a plugin whose only raw config is
// a config_file — the inline config map is nil in that case.
func TestInitPluginManager_MigratesConfigFileOnlyPlugin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(globalDir, "scion-telegram.yaml")
	if err := os.WriteFile(cfgFile, []byte("bot_token: \"file-only-token\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := "server:\n  plugins:\n    broker:\n      telegram:\n        config_file: " + cfgFile + "\n"
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}

	sb := newFakeSecretBackend()
	captureLog(t, func() {
		initPluginManager(context.Background(), sb, nil)
	})

	if got := sb.values[config.SecretTelegramBotToken]; got != "file-only-token" {
		t.Errorf("expected config-file-only secret to be migrated, got %q", got)
	}
}
