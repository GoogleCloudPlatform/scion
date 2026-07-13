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

func (f *fakeHubSettingStore) UpsertHubSetting(_ context.Context, section string, value json.RawMessage, updatedBy string, _ int64, origin string) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.settings[section]
	rev := int64(1)
	if ok {
		rev = existing.Revision + 1
	}
	if origin == "" {
		origin = "seeded"
	}
	s := &store.HubSetting{
		ID:        section,
		Section:   section,
		Value:     value,
		Revision:  rev,
		UpdatedBy: updatedBy,
		Origin:    origin,
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

func (f *fakeHubSettingStore) BackfillOrigin(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for section, s := range f.settings {
		if section == "_meta" {
			continue
		}
		if s.UpdatedBy != "seed" && s.Origin == "seeded" {
			s.Origin = "managed"
		}
	}
	return nil
}

// --- syncHubSettings tests ---

func TestSyncHubSettings_SeedsNewSections(t *testing.T) {
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":         []interface{}{"admin@test.com"},
		"server.auth.user_access_mode":    "invite_only",
		"server.hub.auto_suspend_stalled": true,
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// _meta should be written.
	if _, ok := fs.settings["_meta"]; !ok {
		t.Error("expected _meta sentinel to be written")
	}

	// access section should have been seeded with admin_emails.
	access, ok := fs.settings["access"]
	if !ok {
		t.Fatal("expected access section to be seeded")
	}
	if !strings.Contains(string(access.Value), "admin@test.com") {
		t.Errorf("access section missing admin_emails: %s", access.Value)
	}
	if access.Origin != "seeded" {
		t.Errorf("access origin: want seeded, got %s", access.Origin)
	}
}

func TestSyncHubSettings_SkipsManagedSections(t *testing.T) {
	fs := newFakeHubSettingStore()

	// Pre-populate access as managed (admin-written).
	fs.settings["access"] = &store.HubSetting{
		ID:        "access",
		Section:   "access",
		Value:     json.RawMessage(`{"admin_emails":["admin-custom@test.com"]}`),
		Revision:  5,
		UpdatedBy: "admin@test.com",
		Origin:    "managed",
	}

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"bootstrap@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// access should NOT have been overwritten — still has custom value.
	access := fs.settings["access"]
	if !strings.Contains(string(access.Value), "admin-custom@test.com") {
		t.Errorf("managed access section was overwritten: %s", access.Value)
	}
	if access.Revision != 5 {
		t.Errorf("managed access revision changed: want 5, got %d", access.Revision)
	}
}

func TestSyncHubSettings_UpdatesSeededWhenChanged(t *testing.T) {
	fs := newFakeHubSettingStore()

	// Pre-populate access as seeded with old bootstrap value.
	fs.settings["access"] = &store.HubSetting{
		ID:        "access",
		Section:   "access",
		Value:     json.RawMessage(`{"admin_emails":["old@test.com"],"user_access_mode":"open"}`),
		Revision:  1,
		UpdatedBy: "seed",
		Origin:    "seeded",
	}

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"new@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	access := fs.settings["access"]
	if !strings.Contains(string(access.Value), "new@test.com") {
		t.Errorf("seeded access section was not updated: %s", access.Value)
	}
	if access.Revision != 2 {
		t.Errorf("seeded access revision: want 2 (bumped), got %d", access.Revision)
	}
}

func TestSyncHubSettings_SkipsWriteOnEquality(t *testing.T) {
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"admin@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	// First sync — creates the rows.
	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	fs.mu.Lock()
	accessRev := fs.settings["access"].Revision
	fs.mu.Unlock()

	// Second sync with identical bootstrap — revision should NOT bump.
	err = syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.settings["access"].Revision != accessRev {
		t.Errorf("skip-write-on-equality failed: access revision bumped from %d to %d", accessRev, fs.settings["access"].Revision)
	}
}

func TestSyncHubSettings_SkipsWriteOnSemanticEquality(t *testing.T) {
	// Simulates Postgres jsonb behavior: the stored value has different
	// whitespace/key ordering but is semantically identical.
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"admin@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	// First sync — creates the rows.
	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	fs.mu.Lock()
	accessRev := fs.settings["access"].Revision
	// Simulate jsonb re-serialization: re-order keys and change whitespace.
	origVal := fs.settings["access"].Value
	var parsed map[string]interface{}
	if err := json.Unmarshal(origVal, &parsed); err != nil {
		t.Fatalf("unmarshal stored value: %v", err)
	}
	// Re-marshal (Go map iteration is random, but we also add indentation
	// to guarantee byte-level difference from compact json.Marshal output).
	reordered, _ := json.MarshalIndent(parsed, "", "  ")
	if string(reordered) == string(origVal) {
		// Force a difference by adding whitespace.
		// That won't unmarshal the same, so use a subtler approach:
		reordered = json.RawMessage(`{ "user_access_mode" : "open" }`)
	}
	fs.settings["access"].Value = reordered
	fs.mu.Unlock()

	// Second sync — values are semantically equal despite byte differences.
	err = syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.settings["access"].Revision != accessRev {
		t.Errorf("semantic equality check failed: access revision bumped from %d to %d (jsonb re-serialization not handled)", accessRev, fs.settings["access"].Revision)
	}
}

func TestJsonEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", `{"a":1}`, `{"a":1}`, true},
		{"key reorder", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"whitespace", `{"a": 1}`, `{"a":1}`, true},
		{"different values", `{"a":1}`, `{"a":2}`, false},
		{"extra key", `{"a":1}`, `{"a":1,"b":2}`, false},
		{"empty objects", `{}`, `{}`, true},
		{"nested equal", `{"a":{"b":1}}`, `{"a":{"b":1}}`, true},
		{"nested different", `{"a":{"b":1}}`, `{"a":{"b":2}}`, false},
		{"arrays equal", `[1,2,3]`, `[1,2,3]`, true},
		{"arrays differ order", `[1,2,3]`, `[3,2,1]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jsonEqual(json.RawMessage(tt.a), json.RawMessage(tt.b))
			if got != tt.want {
				t.Errorf("jsonEqual(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSyncHubSettings_MaintenanceSkipped(t *testing.T) {
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"admin@test.com"},
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.settings["maintenance"]; ok {
		t.Error("maintenance section should not be synced (no koanf paths)")
	}
}

func TestSyncHubSettings_GitHubAppNoSecrets(t *testing.T) {
	fs := newFakeHubSettingStore()

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.github_app.app_id":           int64(123),
		"server.github_app.api_base_url":     "https://api.github.com",
		"server.github_app.private_key_path": "/path/to/key.pem",
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
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
	if strings.Contains(docStr, "private_key\"") && !strings.Contains(docStr, "private_key_path") {
		t.Errorf("github_app doc should not contain bare private_key: %s", docStr)
	}
	if strings.Contains(docStr, "webhook_secret") {
		t.Errorf("github_app doc should not contain webhook_secret: %s", docStr)
	}
}

func TestSyncHubSettings_EveryBootRunsEvenWithMeta(t *testing.T) {
	fs := newFakeHubSettingStore()

	// Pre-populate _meta (simulating a previous boot's sync).
	fs.settings["_meta"] = &store.HubSetting{
		ID:       "_meta",
		Section:  "_meta",
		Value:    json.RawMessage(`{"synced_at":"2026-01-01T00:00:00Z","seed_version":"2"}`),
		Revision: 1,
		Origin:   "seeded",
	}

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"admin@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Unlike the old sentinel-gated approach, sync should still run and
	// seed missing sections even when _meta already exists.
	if _, ok := fs.settings["access"]; !ok {
		t.Error("expected access section to be seeded even though _meta existed")
	}
}

func TestSyncHubSettings_BackfillsPreOriginRows(t *testing.T) {
	fs := newFakeHubSettingStore()

	// Pre-populate with rows that have no origin set (simulating pre-origin data).
	fs.settings["access"] = &store.HubSetting{
		ID:        "access",
		Section:   "access",
		Value:     json.RawMessage(`{"admin_emails":["admin@custom.com"]}`),
		Revision:  3,
		UpdatedBy: "admin@test.com",
		Origin:    "seeded", // column default — but updated_by is not "seed"
	}

	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"bootstrap@test.com"},
		"server.auth.user_access_mode": "open",
	}, "."), nil)

	err := syncHubSettings(context.Background(), fs, k)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	// BackfillOrigin should have flipped this to "managed" because
	// updated_by != "seed". Then syncHubSettings should skip it.
	access := fs.settings["access"]
	if access.Origin != "managed" {
		t.Errorf("expected backfill to set origin=managed, got %s", access.Origin)
	}
	if !strings.Contains(string(access.Value), "admin@custom.com") {
		t.Errorf("managed row should not have been overwritten: %s", access.Value)
	}
}
