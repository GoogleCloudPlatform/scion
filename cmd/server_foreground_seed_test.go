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
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// fakeHubSettingStore is a test double for store.HubSettingStore.
type fakeHubSettingStore struct {
	mu       sync.Mutex
	settings map[string]*store.HubSetting
}

func newFakeHubSettingStore() *fakeHubSettingStore {
	return &fakeHubSettingStore{settings: make(map[string]*store.HubSetting)}
}

func (f *fakeHubSettingStore) GetHubSetting(_ context.Context, section string) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.settings[section]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeHubSettingStore) ListHubSettings(_ context.Context) ([]store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.HubSetting, 0, len(f.settings))
	for _, s := range f.settings {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeHubSettingStore) UpsertHubSetting(_ context.Context, section string, value json.RawMessage, updatedBy string, _ int64) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.settings[section]
	rev := int64(1)
	if ok {
		rev = existing.Revision + 1
	}
	s := &store.HubSetting{
		ID:        section,
		Section:   section,
		Value:     value,
		Revision:  rev,
		UpdatedBy: updatedBy,
	}
	f.settings[section] = s
	return s, nil
}

func (f *fakeHubSettingStore) DeleteHubSetting(_ context.Context, section string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.settings[section]; !ok {
		return store.ErrNotFound
	}
	delete(f.settings, section)
	return nil
}

// --- seedHubSettingsIfNeeded tests ---

func TestSeedHubSettingsIfNeeded_MetaSentinel(t *testing.T) {
	// When _meta row already exists, seeding should be skipped entirely.
	fs := newFakeHubSettingStore()
	fs.settings["_meta"] = &store.HubSetting{
		ID:       "_meta",
		Section:  "_meta",
		Value:    json.RawMessage(`{"seeded_from":"/test","seeded_at":"2026-01-01T00:00:00Z","seed_version":"1"}`),
		Revision: 1,
	}

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"admin@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	err := seedHubSettingsIfNeeded(context.Background(), fs, k, "/tmp/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No sections should have been seeded.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for section := range fs.settings {
		if section != "_meta" {
			t.Errorf("unexpected section seeded: %s", section)
		}
	}
}

func TestSeedHubSettingsIfNeeded_ExtractsSections(t *testing.T) {
	// With no _meta row, seeding should extract sections from the koanf file.
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":         []interface{}{"admin@test.com"},
		"server.auth.user_access_mode":    "invite_only",
		"server.hub.auto_suspend_stalled": true,
	}, "."), nil)

	err := seedHubSettingsIfNeeded(context.Background(), fs, k, "/tmp/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// _meta should be written
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.settings["_meta"]; !ok {
		t.Error("expected _meta sentinel to be written")
	}

	// access section should have been seeded with admin_emails
	access, ok := fs.settings["access"]
	if !ok {
		t.Fatal("expected access section to be seeded")
	}
	if !strings.Contains(string(access.Value), "admin@test.com") {
		t.Errorf("access section missing admin_emails: %s", access.Value)
	}
}

func TestSeedHubSettingsIfNeeded_MaintenanceSkipped(t *testing.T) {
	// Maintenance section has no koanf paths — it should not be seeded.
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"admin@test.com"},
	}, "."), nil)

	err := seedHubSettingsIfNeeded(context.Background(), fs, k, "/tmp/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.settings["maintenance"]; ok {
		t.Error("maintenance section should not be seeded (no koanf paths)")
	}
}

func TestSeedHubSettingsIfNeeded_GitHubAppNoSecrets(t *testing.T) {
	// The seeded github_app section should not contain private_key or webhook_secret.
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.github_app.app_id":           int64(123),
		"server.github_app.api_base_url":     "https://api.github.com",
		"server.github_app.private_key_path": "/path/to/key.pem",
	}, "."), nil)

	err := seedHubSettingsIfNeeded(context.Background(), fs, k, "/tmp/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	gh, ok := fs.settings["github_app"]
	if !ok {
		t.Fatal("expected github_app section to be seeded")
	}

	docStr := string(gh.Value)
	// The GitHubAppSettings struct does not have private_key or webhook_secret
	// fields, so they are structurally excluded from the seeded document.
	if strings.Contains(docStr, "private_key\"") && !strings.Contains(docStr, "private_key_path") {
		t.Errorf("github_app doc should not contain bare private_key: %s", docStr)
	}
	if strings.Contains(docStr, "webhook_secret") {
		t.Errorf("github_app doc should not contain webhook_secret: %s", docStr)
	}
}
