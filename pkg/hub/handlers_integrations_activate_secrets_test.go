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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// migrationSecretBackend is an in-memory secret backend that records writes,
// used to observe whether a code path migrates plugin secrets.
type migrationSecretBackend struct {
	values map[string]string
	sets   []secret.SetSecretInput
}

func newMigrationSecretBackend() *migrationSecretBackend {
	return &migrationSecretBackend{values: make(map[string]string)}
}

func (m *migrationSecretBackend) Get(_ context.Context, name, _, _ string) (*secret.SecretWithValue, error) {
	v, ok := m.values[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name}, Value: v}, nil
}

func (m *migrationSecretBackend) Set(_ context.Context, in *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	m.sets = append(m.sets, *in)
	m.values[in.Name] = in.Value
	return true, nil, nil
}

func (m *migrationSecretBackend) Delete(context.Context, string, string, string) error { return nil }

func (m *migrationSecretBackend) List(context.Context, secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (m *migrationSecretBackend) GetMeta(_ context.Context, name, _, _ string) (*secret.SecretMeta, error) {
	if _, ok := m.values[name]; !ok {
		return nil, store.ErrNotFound
	}
	return &secret.SecretMeta{Name: name}, nil
}

func (m *migrationSecretBackend) UpdateMeta(_ context.Context, _ *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}

func (m *migrationSecretBackend) Resolve(context.Context, string, string, string, *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}

func (m *migrationSecretBackend) HubID() string { return "test-hub" }

// newActivationServer returns a Server wired with a mock plugin manager and
// the given secret backend, plus a temporary HOME so plugin dir resolution
// stays inside the test sandbox.
func newActivationServer(t *testing.T, sb secret.SecretBackend) (*Server, *mockIntegrationManager) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr
	srv.secretBackend = sb
	return srv, mgr
}

// TestActivateInstalledIntegration_MigratesInlineSecret pins the bug from
// ptone/scion#1017: a user who put bot_token in settings.yaml and activated
// the plugin from the admin UI without restarting had the token stripped by
// ResolvePluginConfig and never written to the backend.
func TestActivateInstalledIntegration_MigratesInlineSecret(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	entry := &config.V1PluginEntry{
		Path:   "./telegram",
		Config: map[string]string{"bot_token": "inline-token", "mode": "plugin"},
	}

	if err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", entry); err != nil {
		t.Fatalf("activateInstalledIntegration: %v", err)
	}

	if got := sb.values[config.SecretTelegramBotToken]; got != "inline-token" {
		t.Errorf("%s in backend = %q, want %q — the inline secret was not migrated",
			config.SecretTelegramBotToken, got, "inline-token")
	}

	// The activated plugin must still receive the token: migration happens
	// before stripping, and the value is read back out of the backend.
	if got := mgr.plugins["telegram"]["bot_token"]; got != "inline-token" {
		t.Errorf("bot_token passed to LoadOne = %q, want %q", got, "inline-token")
	}
}

func TestActivateInstalledIntegration_MigratesConfigFileSecret(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	configFile := filepath.Join(t.TempDir(), "scion-telegram.yaml")
	if err := os.WriteFile(configFile, []byte("bot_token: file-token\nchat_id: \"42\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	entry := &config.V1PluginEntry{Path: "./telegram", ConfigFile: configFile}

	if err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", entry); err != nil {
		t.Fatalf("activateInstalledIntegration: %v", err)
	}

	if got := sb.values[config.SecretTelegramBotToken]; got != "file-token" {
		t.Errorf("%s in backend = %q, want %q — the config-file secret was not migrated",
			config.SecretTelegramBotToken, got, "file-token")
	}
}

func TestActivateInstalledIntegration_DoesNotOverwriteMigratedSecret(t *testing.T) {
	sb := newMigrationSecretBackend()
	sb.values[config.SecretTelegramBotToken] = "backend-token"
	srv, mgr := newActivationServer(t, sb)

	entry := &config.V1PluginEntry{
		Path:   "./telegram",
		Config: map[string]string{"bot_token": "stale-inline-token"},
	}

	if err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", entry); err != nil {
		t.Fatalf("activateInstalledIntegration: %v", err)
	}

	if len(sb.sets) != 0 {
		t.Errorf("expected no backend writes when the secret already exists, got %+v", sb.sets)
	}
	if got := sb.values[config.SecretTelegramBotToken]; got != "backend-token" {
		t.Errorf("backend value = %q, want the existing %q preserved", got, "backend-token")
	}
	// The backend copy is authoritative once it exists.
	if got := mgr.plugins["telegram"]["bot_token"]; got != "backend-token" {
		t.Errorf("bot_token passed to LoadOne = %q, want %q", got, "backend-token")
	}
}

func TestActivateInstalledIntegration_NoSecretBackendIsNoOp(t *testing.T) {
	// No backend configured: activation must still proceed without panicking.
	srv, mgr := newActivationServer(t, nil)

	entry := &config.V1PluginEntry{
		Path:   "./telegram",
		Config: map[string]string{"bot_token": "inline-token", "mode": "plugin"},
	}

	if err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", entry); err != nil {
		t.Fatalf("activateInstalledIntegration: %v", err)
	}
	if len(mgr.loadOneCalls) != 1 {
		t.Errorf("expected the plugin to be loaded once, got %v", mgr.loadOneCalls)
	}
}

// A nil entry must be rejected up front rather than panicking part-way through
// activation, which could leave the plugin manager holding partial state.
func TestActivateInstalledIntegration_NilEntryReturnsError(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", nil)
	if err == nil {
		t.Fatal("expected an error for a nil entry, got nil")
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Errorf("error = %q, want it to name the integration", err)
	}
	if len(mgr.loadOneCalls) != 0 {
		t.Errorf("expected no load attempt for a nil entry, got %v", mgr.loadOneCalls)
	}
	if len(sb.sets) != 0 {
		t.Errorf("expected no backend writes for a nil entry, got %+v", sb.sets)
	}
}

func TestActivateInstalledIntegration_UnknownPluginDoesNotMigrate(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	entry := &config.V1PluginEntry{
		Path:   "./custom",
		Config: map[string]string{"bot_token": "inline-token"},
	}

	if err := srv.activateInstalledIntegration(context.Background(), mgr, "custom-broker", entry); err != nil {
		t.Fatalf("activateInstalledIntegration: %v", err)
	}

	if len(sb.sets) != 0 {
		t.Errorf("expected no migration for a plugin with no secret key mapping, got %+v", sb.sets)
	}
}

// TestGetIntegration_DoesNotMigrateSecrets guards the boundary: reading an
// integration must never write to the secret backend. Migration belongs to
// activation, not to inspection.
func TestGetIntegration_DoesNotMigrateSecrets(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	configFile := filepath.Join(t.TempDir(), "scion-telegram.yaml")
	if err := os.WriteFile(configFile, []byte("bot_token: file-token\nchat_id: \"42\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr.plugins["telegram"] = map[string]string{"config_file": configFile, "bot_token": "runtime-token"}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Positive assertion first: chat_id is a non-secret key that only exists in
	// the config file, so seeing it proves the handler actually resolved the
	// file. Without this the no-write assertion below could pass simply because
	// the read path never ran.
	var detail IntegrationDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := detail.Settings["chat_id"]; got != "42" {
		t.Fatalf("chat_id in response = %q, want %q — the config file was not "+
			"resolved, so the no-write assertion below proves nothing", got, "42")
	}

	if len(sb.sets) != 0 {
		t.Errorf("GET must not write to the secret backend, got %+v", sb.sets)
	}
}

// TestLoadTeamsConfig_DoesNotMigrateSecrets covers the other read-only
// ResolvePluginConfig caller (Teams manifest generation).
func TestLoadTeamsConfig_DoesNotMigrateSecrets(t *testing.T) {
	sb := newMigrationSecretBackend()
	srv, mgr := newActivationServer(t, sb)

	configFile := filepath.Join(t.TempDir(), "scion-teams.yaml")
	if err := os.WriteFile(configFile, []byte("app_secret: file-secret\napp_id: abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr.plugins["teams"] = map[string]string{"config_file": configFile}

	cfg, err := srv.loadTeamsConfig()
	if err != nil {
		t.Fatalf("loadTeamsConfig: %v", err)
	}
	// Positive assertion first: without it this test would pass vacuously if
	// loadTeamsConfig stopped reading the config file at all.
	if got := cfg["app_id"]; got != "abc" {
		t.Fatalf("app_id = %q, want %q — the config file was not read, so the "+
			"no-write assertion below proves nothing", got, "abc")
	}
	if len(sb.sets) != 0 {
		t.Errorf("manifest generation must not write to the secret backend, got %+v", sb.sets)
	}
}
