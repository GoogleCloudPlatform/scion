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

package secretmigration

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeBackend is an in-memory secret.SecretBackend that records writes.
type fakeBackend struct {
	values  map[string]secret.SecretWithValue
	sets    []secret.SetSecretInput
	getErr  error // returned by Get for any key when non-nil
	setErr  error // returned by Set when non-nil
	getKeys []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{values: make(map[string]secret.SecretWithValue)}
}

func (f *fakeBackend) seed(name, value string) {
	f.values[name] = secret.SecretWithValue{
		SecretMeta: secret.SecretMeta{Name: name},
		Value:      value,
	}
}

func (f *fakeBackend) Get(_ context.Context, name, _, _ string) (*secret.SecretWithValue, error) {
	f.getKeys = append(f.getKeys, name)
	if f.getErr != nil {
		return nil, f.getErr
	}
	sv, ok := f.values[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &sv, nil
}

func (f *fakeBackend) Set(_ context.Context, in *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	if f.setErr != nil {
		return false, nil, f.setErr
	}
	f.sets = append(f.sets, *in)
	f.values[in.Name] = secret.SecretWithValue{
		SecretMeta: secret.SecretMeta{Name: in.Name, Description: in.Description},
		Value:      in.Value,
	}
	return true, nil, nil
}

func (f *fakeBackend) Delete(context.Context, string, string, string) error { return nil }

func (f *fakeBackend) List(context.Context, secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (f *fakeBackend) GetMeta(context.Context, string, string, string) (*secret.SecretMeta, error) {
	return nil, nil
}

func (f *fakeBackend) Resolve(context.Context, string, string, string, *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}

func (f *fakeBackend) HubID() string { return "test-hub" }

// writeConfigFile writes a per-plugin YAML config file and returns its path.
func writeConfigFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// captureLog collects everything the code under test logs while fn runs, via
// either slog or the standard logger.
//
// Redirecting slog explicitly rather than relying on its default handler
// routing through the standard logger means the capture keeps working if this
// package's logging is reconfigured. Note the format that implies: assertions
// see slog's text form (`level=WARN msg="..." key=value`), not the standard
// logger's.
//
// slog.SetDefault has the side effect of rewiring the standard logger's output
// and clearing its flags, so all three are snapshotted and restored together —
// restoring them in the wrong order leaves log.Flags() at 0 for the rest of the
// run. This mutates process-global state, so callers must not use t.Parallel.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer

	prevDefault := slog.Default()
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	defer func() {
		slog.SetDefault(prevDefault)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	log.SetOutput(&buf)

	fn()
	return buf.String()
}

func TestMigratePluginSecrets_InlineSecretMigrated(t *testing.T) {
	sb := newFakeBackend()

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token", "mode": "plugin"}, "")

	got, ok := sb.values[config.SecretTelegramBotToken]
	if !ok {
		t.Fatalf("expected %s to be written to the backend, got keys %v",
			config.SecretTelegramBotToken, sb.values)
	}
	if got.Value != "inline-token" {
		t.Errorf("migrated value = %q, want %q", got.Value, "inline-token")
	}
	if len(sb.sets) != 1 {
		t.Fatalf("expected exactly 1 Set call, got %d", len(sb.sets))
	}
	in := sb.sets[0]
	if in.Scope != store.ScopeHub || in.ScopeID != "test-hub" {
		t.Errorf("secret written to scope %q/%q, want %q/%q", in.Scope, in.ScopeID, store.ScopeHub, "test-hub")
	}
	if in.SecretType != secret.TypeVariable {
		t.Errorf("SecretType = %q, want %q", in.SecretType, secret.TypeVariable)
	}
	if !strings.Contains(in.Description, "settings.yaml") {
		t.Errorf("Description = %q, want it to name the inline source", in.Description)
	}
}

func TestMigratePluginSecrets_ConfigFileSecretMigrated(t *testing.T) {
	sb := newFakeBackend()
	configFile := writeConfigFile(t, "scion-telegram.yaml", "bot_token: file-token\nchat_id: \"42\"\n")

	MigratePluginSecrets(context.Background(), sb, "telegram", nil, configFile)

	got, ok := sb.values[config.SecretTelegramBotToken]
	if !ok {
		t.Fatalf("expected %s to be written to the backend", config.SecretTelegramBotToken)
	}
	if got.Value != "file-token" {
		t.Errorf("migrated value = %q, want %q", got.Value, "file-token")
	}
	if !strings.Contains(sb.sets[0].Description, "scion-telegram.yaml") {
		t.Errorf("Description = %q, want it to name the source file", sb.sets[0].Description)
	}
	// The description must not leak the operator's home directory layout.
	if strings.Contains(sb.sets[0].Description, string(filepath.Separator)) {
		t.Errorf("Description = %q, want base name only, not a host path", sb.sets[0].Description)
	}
}

// The two sinks have opposite requirements: the operator-facing log must name
// the full path so the file can be found, while the persisted backend
// description must not, because it would leak the operator's username and
// directory layout to anyone who can read secret metadata.
func TestMigratePluginSecrets_LogsFullPathButDescriptionOmitsIt(t *testing.T) {
	sb := newFakeBackend()
	configFile := writeConfigFile(t, "scion-telegram.yaml", "bot_token: \"file-token\"\n")

	out := captureLog(t, func() {
		MigratePluginSecrets(context.Background(), sb, "telegram", nil, configFile)
	})

	if !strings.Contains(out, configFile) {
		t.Errorf("expected the migration log to name the full path %q, got: %q", configFile, out)
	}
	desc := sb.sets[0].Description
	if !strings.Contains(desc, "scion-telegram.yaml") {
		t.Errorf("Description = %q, want it to name the config file", desc)
	}
	if strings.Contains(desc, filepath.Dir(configFile)) {
		t.Errorf("Description = %q, must not contain the host path %q", desc, filepath.Dir(configFile))
	}
}

func TestMigratePluginSecrets_ConfigFileWinsOverInline(t *testing.T) {
	// When config_file is set, ResolvePluginConfig drops secret keys from
	// inline config and the plugin runs on the file's value — so the file's
	// value is what must reach the backend.
	sb := newFakeBackend()
	configFile := writeConfigFile(t, "scion-telegram.yaml", "bot_token: file-token\n")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, configFile)

	if got := sb.values[config.SecretTelegramBotToken].Value; got != "file-token" {
		t.Errorf("migrated value = %q, want the config file value %q", got, "file-token")
	}
}

func TestMigratePluginSecrets_EmptyFileValueFallsBackToInline(t *testing.T) {
	sb := newFakeBackend()
	configFile := writeConfigFile(t, "scion-telegram.yaml", "bot_token: \"\"\nchat_id: \"42\"\n")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, configFile)

	if got := sb.values[config.SecretTelegramBotToken].Value; got != "inline-token" {
		t.Errorf("migrated value = %q, want the inline value %q", got, "inline-token")
	}
}

func TestMigratePluginSecrets_AlreadyMigratedNotOverwritten(t *testing.T) {
	sb := newFakeBackend()
	sb.seed(config.SecretTelegramBotToken, "backend-token")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, "")

	if len(sb.sets) != 0 {
		t.Errorf("expected no writes when the secret already exists, got %+v", sb.sets)
	}
	if got := sb.values[config.SecretTelegramBotToken].Value; got != "backend-token" {
		t.Errorf("backend value = %q, want the existing %q to be preserved", got, "backend-token")
	}
}

func TestMigratePluginSecrets_EmptyBackendValueIsMigrated(t *testing.T) {
	// A secret that exists but holds an empty value is not a real credential;
	// migration should fill it.
	sb := newFakeBackend()
	sb.seed(config.SecretTelegramBotToken, "")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, "")

	if got := sb.values[config.SecretTelegramBotToken].Value; got != "inline-token" {
		t.Errorf("backend value = %q, want %q", got, "inline-token")
	}
}

func TestMigratePluginSecrets_NilBackendIsNoOp(t *testing.T) {
	// Must not panic when no secret backend is configured.
	MigratePluginSecrets(context.Background(), nil, "telegram",
		map[string]string{"bot_token": "inline-token"}, "")
}

func TestMigratePluginSecrets_UnknownPluginIsNoOp(t *testing.T) {
	sb := newFakeBackend()

	MigratePluginSecrets(context.Background(), sb, "not-a-known-plugin",
		map[string]string{"bot_token": "inline-token"}, "")

	if len(sb.sets) != 0 {
		t.Errorf("expected no writes for an unknown plugin, got %+v", sb.sets)
	}
	if len(sb.getKeys) != 0 {
		t.Errorf("expected the backend not to be consulted, got Gets for %v", sb.getKeys)
	}
}

func TestMigratePluginSecrets_NoSecretsInConfigIsNoOp(t *testing.T) {
	sb := newFakeBackend()

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"mode": "plugin", "path": "./telegram"}, "")

	if len(sb.sets) != 0 {
		t.Errorf("expected no writes when config holds no secrets, got %+v", sb.sets)
	}
}

func TestMigratePluginSecrets_MissingConfigFileStillMigratesInline(t *testing.T) {
	sb := newFakeBackend()
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, missing)

	if got := sb.values[config.SecretTelegramBotToken].Value; got != "inline-token" {
		t.Errorf("migrated value = %q, want %q", got, "inline-token")
	}
}

func TestMigratePluginSecrets_MalformedConfigFileStillMigratesInline(t *testing.T) {
	sb := newFakeBackend()
	// bot_token appears only in the file and webhook_secret only inline: if the
	// file parsed, bot_token would migrate too.
	configFile := writeConfigFile(t, "scion-telegram.yaml",
		"bot_token: \"file-token\"\nwebhook_secret: [unterminated\n")

	out := captureLog(t, func() {
		MigratePluginSecrets(context.Background(), sb, "telegram",
			map[string]string{"webhook_secret": "inline-secret"}, configFile)
	})

	if got := sb.values[config.SecretTelegramWebhookKey].Value; got != "inline-secret" {
		t.Errorf("migrated value = %q, want the inline fallback %q", got, "inline-secret")
	}
	if _, ok := sb.values[config.SecretTelegramBotToken]; ok {
		t.Errorf("an unparseable config file must yield no file values, got %q",
			sb.values[config.SecretTelegramBotToken].Value)
	}
	if !strings.Contains(out, "failed to read config file") {
		t.Errorf("expected a warning about the unreadable config file, got log: %q", out)
	}
	// `level=WARN` is the slog text handler's form and is absent from the
	// standard logger's, so this also proves captureLog's slog redirection is
	// what caught the line rather than the default routing happening to work.
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected the line to be logged at WARN via slog, got log: %q", out)
	}
	// The operator needs the path to know which file to fix, so the warning
	// carries the full path even though the backend description does not.
	if !strings.Contains(out, configFile) {
		t.Errorf("expected the warning to name the full config file path %q, got log: %q", configFile, out)
	}
}

func TestMigratePluginSecrets_BackendKeyNameInFileIsNotASource(t *testing.T) {
	// TELEGRAM_BOT_TOKEN is the backend key name, not a plugin config key.
	// LoadPluginConfigFile strips it, so it must not act as a migration source.
	sb := newFakeBackend()
	configFile := writeConfigFile(t, "scion-telegram.yaml", "TELEGRAM_BOT_TOKEN: file-token\n")

	MigratePluginSecrets(context.Background(), sb, "telegram", nil, configFile)

	if len(sb.sets) != 0 {
		t.Errorf("expected no writes for a backend-style key, got %+v", sb.sets)
	}
}

func TestMigratePluginSecrets_GetErrorSkipsWrite(t *testing.T) {
	// A backend that cannot be read must not be overwritten blindly — the
	// existing value is unknown and could be clobbered.
	sb := newFakeBackend()
	sb.getErr = errors.New("backend unavailable")

	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "inline-token"}, "")

	if len(sb.sets) != 0 {
		t.Errorf("expected no writes when the backend read fails, got %+v", sb.sets)
	}
}

func TestMigratePluginSecrets_SetErrorDoesNotStopRemainingKeys(t *testing.T) {
	sb := newFakeBackend()
	sb.setErr = errors.New("write rejected")

	// Both telegram keys are present; a failure on the first must not abort
	// the second. Neither is stored, but both must have been attempted.
	MigratePluginSecrets(context.Background(), sb, "telegram",
		map[string]string{"bot_token": "t", "webhook_secret": "w"}, "")

	if len(sb.getKeys) != 2 {
		t.Errorf("expected both mapped keys to be attempted, got Gets for %v", sb.getKeys)
	}
}

func TestMigratePluginSecrets_MultipleKeysForPlugin(t *testing.T) {
	sb := newFakeBackend()

	MigratePluginSecrets(context.Background(), sb, "slack", map[string]string{
		"bot_token":      "xoxb-1",
		"app_token":      "xapp-1",
		"signing_secret": "sign-1",
	}, "")

	for key, want := range map[string]string{
		config.SecretSlackBotToken:      "xoxb-1",
		config.SecretSlackAppToken:      "xapp-1",
		config.SecretSlackSigningSecret: "sign-1",
	} {
		if got := sb.values[key].Value; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
