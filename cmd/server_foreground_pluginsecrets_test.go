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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeSecretBackend is an in-memory secret.SecretBackend for migration tests.
// Only Get, Set and HubID are exercised by secretmigration.MigratePluginSecrets.
type fakeSecretBackend struct {
	hubID  string
	values map[string]string
}

func newFakeSecretBackend() *fakeSecretBackend {
	return &fakeSecretBackend{hubID: "hub-1", values: map[string]string{}}
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

func (f *fakeSecretBackend) UpdateMeta(_ context.Context, _ *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}

func (f *fakeSecretBackend) Resolve(context.Context, string, string, string, *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
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
