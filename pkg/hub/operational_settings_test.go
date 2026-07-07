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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// --- fake HubSettingStore ---

type fakeHubSettingStore struct {
	mu       sync.Mutex
	settings map[string]*store.HubSetting
	nextRev  map[string]int64
}

func newFakeHubSettingStore() *fakeHubSettingStore {
	return &fakeHubSettingStore{
		settings: make(map[string]*store.HubSetting),
		nextRev:  make(map[string]int64),
	}
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

func (f *fakeHubSettingStore) UpsertHubSetting(_ context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.settings[section]
	if ok {
		if expectedRevision > 0 && existing.Revision != expectedRevision {
			return nil, store.ErrRevisionConflict
		}
		if expectedRevision == 0 {
			return nil, store.ErrRevisionConflict
		}
	} else {
		if expectedRevision > 0 {
			return nil, store.ErrRevisionConflict
		}
	}

	rev := int64(1)
	if existing != nil {
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

// helper to seed a section directly.
func (f *fakeHubSettingStore) seed(section string, doc json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[section] = &store.HubSetting{
		ID:       section,
		Section:  section,
		Value:    doc,
		Revision: 1,
	}
}

// --- helpers ---

func newFileKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build file koanf: %v", err)
	}
	return k
}

func newEnvKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build env koanf: %v", err)
	}
	return k
}

func emptyKoanf() *koanf.Koanf {
	return koanf.New(".")
}

// --- Tests ---

func TestSnapshot_Precedence_EnvOverDBOverFileOverDefault(t *testing.T) {
	// File says admin_emails = ["file@example.com"]
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"file@example.com"},
		"server.auth.user_access_mode": "open",
	})

	// DB says admin_emails = ["db@example.com"]
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["db@example.com"],"user_access_mode":"domain_restricted"}`))

	// Env says admin_emails = ["env@example.com"]
	envK := newEnvKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@example.com"},
	})

	ops := NewOperationalSettings(fakeStore, fileK, envK)
	_, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := ops.Snapshot()

	// env > DB > file: admin_emails should be from env
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "env@example.com" {
		t.Errorf("AdminEmails: want [env@example.com], got %v", snap.AdminEmails)
	}

	// user_access_mode not overridden by env, so DB wins
	if snap.UserAccessMode != "domain_restricted" {
		t.Errorf("UserAccessMode: want domain_restricted, got %s", snap.UserAccessMode)
	}
}

func TestSnapshot_DBRowFullyOwnsSection(t *testing.T) {
	// File has admin_emails AND authorized_domains
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":        []interface{}{"file@example.com"},
		"server.auth.authorized_domains": []interface{}{"example.com"},
		"server.auth.user_access_mode":   "open",
	})

	// DB has access section with ONLY admin_emails — authorized_domains is absent.
	// Per design: "a DB row present in DB fully owns its section (its omitted
	// fields fall to compiled defaults, not to the file)."
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["db@example.com"]}`))

	ops := NewOperationalSettings(fakeStore, fileK, emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()

	// admin_emails from DB
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "db@example.com" {
		t.Errorf("AdminEmails: want [db@example.com], got %v", snap.AdminEmails)
	}

	// authorized_domains should be empty (compiled default), NOT ["example.com"] (file)
	if len(snap.AuthorizedDomains) != 0 {
		t.Errorf("AuthorizedDomains: want [] (compiled default), got %v", snap.AuthorizedDomains)
	}
}

func TestSnapshot_DeletedRowRestoresFileFallback(t *testing.T) {
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"file@example.com"},
		"server.auth.user_access_mode": "invite_only",
	})

	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["db@example.com"],"user_access_mode":"open"}`))

	ops := NewOperationalSettings(fakeStore, fileK, emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	// Confirm DB values active
	snap := ops.Snapshot()
	if snap.UserAccessMode != "open" {
		t.Fatalf("pre-delete: want open, got %s", snap.UserAccessMode)
	}

	// Delete the access section
	_ = fakeStore.DeleteHubSetting(context.Background(), "access")
	_, _ = ops.Refresh(context.Background())

	// File fallback should be restored
	snap = ops.Snapshot()
	if snap.UserAccessMode != "invite_only" {
		t.Errorf("post-delete: want invite_only (file), got %s", snap.UserAccessMode)
	}
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "file@example.com" {
		t.Errorf("post-delete AdminEmails: want [file@example.com], got %v", snap.AdminEmails)
	}
}

func TestRefresh_DetectsChangedSections(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@b.com"]}`))
	fakeStore.seed("lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())

	// First refresh — everything is new.
	changed, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh 1: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("Refresh 1: want 2 changed sections, got %d: %v", len(changed), changed)
	}

	// Second refresh with no changes — should return empty.
	changed2, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh 2: %v", err)
	}
	if len(changed2) != 0 {
		t.Errorf("Refresh 2: want 0 changed, got %d: %v", len(changed2), changed2)
	}

	// Update access section revision.
	fakeStore.mu.Lock()
	fakeStore.settings["access"].Revision = 2
	fakeStore.settings["access"].Value = json.RawMessage(`{"admin_emails":["c@d.com"]}`)
	fakeStore.mu.Unlock()

	changed3, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh 3: %v", err)
	}
	if len(changed3) != 1 || changed3[0] != "access" {
		t.Errorf("Refresh 3: want [access], got %v", changed3)
	}
}

func TestRefresh_DetectsDeletedSections(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@b.com"]}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	// Delete the section from the fake store.
	fakeStore.mu.Lock()
	delete(fakeStore.settings, "access")
	fakeStore.mu.Unlock()

	changed, _ := ops.Refresh(context.Background())
	found := false
	for _, c := range changed {
		if c == "access" {
			found = true
		}
	}
	if !found {
		t.Errorf("want 'access' in changed after deletion, got %v", changed)
	}
}

func TestUpdate_ValidatesAndCaches(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())

	// Valid access doc
	doc := json.RawMessage(`{"admin_emails":["admin@test.com"],"user_access_mode":"open"}`)
	rev, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if rev != 1 {
		t.Errorf("want revision 1, got %d", rev)
	}

	// Verify cache was updated
	snap := ops.Snapshot()
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "admin@test.com" {
		t.Errorf("after Update, AdminEmails: want [admin@test.com], got %v", snap.AdminEmails)
	}
}

func TestUpdate_ValidationFailure(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())

	// Invalid access doc (admin_emails should be an array, not a string)
	doc := json.RawMessage(`{"admin_emails":"not-an-array"}`)
	_, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

func TestSnapshot_MaintenanceFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":true,"maintenance_message":"Upgrading..."}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if !snap.AdminMode {
		t.Error("want AdminMode true")
	}
	if snap.MaintenanceMessage != "Upgrading..." {
		t.Errorf("want 'Upgrading...', got %q", snap.MaintenanceMessage)
	}
}

func TestSnapshot_MaintenanceAbsentMeansDefaults(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	// No maintenance row seeded.

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.AdminMode {
		t.Error("want AdminMode false when no maintenance row")
	}
	if snap.MaintenanceMessage != "" {
		t.Errorf("want empty maintenance message, got %q", snap.MaintenanceMessage)
	}
}

func TestSnapshot_EnvOverrides_Listed(t *testing.T) {
	envK := newEnvKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"env@example.com"},
		"server.auth.user_access_mode": "open",
		"server.hub.port":              9999, // Layer-0, should not appear in overrides
	})

	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), envK)
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	overrides := make(map[string]bool)
	for _, k := range snap.EnvOverrides {
		overrides[k] = true
	}

	if !overrides["server.hub.admin_emails"] {
		t.Error("expected server.hub.admin_emails in EnvOverrides")
	}
	if !overrides["server.auth.user_access_mode"] {
		t.Error("expected server.auth.user_access_mode in EnvOverrides")
	}
	// Layer-0 key should NOT be in EnvOverrides (it's not a Layer-1 key)
	if overrides["server.hub.port"] {
		t.Error("server.hub.port is Layer-0 and should not be in EnvOverrides")
	}
}

func TestApplySnapshot_RegressionParity(t *testing.T) {
	// Build a snapshot identical to what file-mode reloadSettings would produce.
	telEnabled := true
	snap := Layer1Snapshot{
		AdminEmails:           []string{"admin@test.com", "admin2@test.com"},
		UserAccessMode:        "domain_restricted",
		AutoSuspendStalled:    true,
		TelemetryEnabled:      &telEnabled,
		TelemetryConfig:       &config.V1TelemetryConfig{Enabled: &telEnabled},
		GitHubAppID:           12345,
		GitHubAPIBaseURL:      "https://api.github.com",
		GitHubWebhooksEnabled: true,
		GitHubInstallationURL: "https://github.com/apps/test",
		GitHubPrivateKeyPath:  "/path/to/key.pem",
		AdminMode:             true,
		MaintenanceMessage:    "Maintenance in progress",
	}

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	results := ApplySnapshot(srv, snap)

	// Check config fields
	if len(srv.config.AdminEmails) != 2 || srv.config.AdminEmails[0] != "admin@test.com" {
		t.Errorf("AdminEmails: want [admin@test.com admin2@test.com], got %v", srv.config.AdminEmails)
	}
	if srv.config.UserAccessMode != "domain_restricted" {
		t.Errorf("UserAccessMode: want domain_restricted, got %s", srv.config.UserAccessMode)
	}
	if !srv.config.AutoSuspendStalled {
		t.Error("AutoSuspendStalled: want true")
	}
	if srv.config.TelemetryDefault == nil || !*srv.config.TelemetryDefault {
		t.Error("TelemetryDefault: want true")
	}
	if srv.config.GitHubAppConfig.AppID != 12345 {
		t.Errorf("GitHubApp.AppID: want 12345, got %d", srv.config.GitHubAppConfig.AppID)
	}
	if srv.config.GitHubAppConfig.PrivateKeyPath != "/path/to/key.pem" {
		t.Errorf("GitHubApp.PrivateKeyPath: want /path/to/key.pem, got %s", srv.config.GitHubAppConfig.PrivateKeyPath)
	}

	// Check maintenance state
	if !srv.maintenance.IsEnabled() {
		t.Error("MaintenanceState.IsEnabled: want true")
	}
	if srv.maintenance.Message() != "Maintenance in progress" {
		t.Errorf("MaintenanceState.Message: want 'Maintenance in progress', got %q", srv.maintenance.Message())
	}

	// Check results structure
	applied, ok := results["applied"].([]string)
	if !ok {
		t.Fatal("results['applied'] is not []string")
	}
	if len(applied) == 0 {
		t.Error("expected some applied fields")
	}

	restart, ok := results["requires_restart"].([]string)
	if !ok {
		t.Fatal("results['requires_restart'] is not []string")
	}
	if len(restart) == 0 {
		t.Error("expected some requires_restart fields")
	}
}

func TestApplySnapshot_UserAccessModeCleared(t *testing.T) {
	srv := &Server{
		config: ServerConfig{
			UserAccessMode: "domain_restricted",
		},
		maintenance: NewMaintenanceState(false, ""),
	}

	// Snapshot with empty UserAccessMode should clear the config value.
	snap := Layer1Snapshot{
		UserAccessMode: "",
	}

	ApplySnapshot(srv, snap)

	if srv.config.UserAccessMode != "" {
		t.Errorf("want empty UserAccessMode after clearing, got %q", srv.config.UserAccessMode)
	}
}

func TestBuildLayer1SnapshotFromFile(t *testing.T) {
	telEnabled := true
	gc := &config.GlobalConfig{
		Hub: config.HubServerConfig{
			AdminEmails: []string{"admin@file.com"},
		},
		Auth: config.DevAuthConfig{
			UserAccessMode:    "open",
			AuthorizedDomains: []string{"file.com"},
		},
		TelemetryEnabled: &telEnabled,
		TelemetryConfig:  &config.V1TelemetryConfig{Enabled: &telEnabled},
		AdminMode:        true,
		GitHubApp: config.GitHubAppConfig{
			AppID: 99,
		},
	}

	snap := BuildLayer1SnapshotFromFile(gc)

	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "admin@file.com" {
		t.Errorf("AdminEmails: want [admin@file.com], got %v", snap.AdminEmails)
	}
	if snap.UserAccessMode != "open" {
		t.Errorf("UserAccessMode: want open, got %s", snap.UserAccessMode)
	}
	if !snap.AdminMode {
		t.Error("AdminMode: want true")
	}
	if snap.GitHubAppID != 99 {
		t.Errorf("GitHubAppID: want 99, got %d", snap.GitHubAppID)
	}
}

func TestRefresh_IgnoresMetaRow(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("_meta", json.RawMessage(`{"seeded_from":"/path/to/settings.yaml","seeded_at":"2026-07-07T00:00:00Z","seed_version":"1"}`))
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@b.com"]}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	changed, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Only "access" should be in changed, not "_meta".
	for _, c := range changed {
		if c == "_meta" {
			t.Error("_meta should not appear in changed sections")
		}
	}
	if len(changed) != 1 || changed[0] != "access" {
		t.Errorf("want [access], got %v", changed)
	}
}

func TestSnapshot_TelemetryFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("telemetry", json.RawMessage(`{"enabled":true,"hub":{"enabled":true,"report_interval":"30s"}}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.TelemetryEnabled == nil || !*snap.TelemetryEnabled {
		t.Error("want TelemetryEnabled true")
	}
	if snap.TelemetryConfig == nil {
		t.Fatal("want non-nil TelemetryConfig")
	}
}

func TestSnapshot_AgentDefaultsFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("agent_defaults", json.RawMessage(`{"default_template":"my-tmpl","default_max_turns":50}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.DefaultTemplate != "my-tmpl" {
		t.Errorf("want 'my-tmpl', got %q", snap.DefaultTemplate)
	}
	if snap.DefaultMaxTurns != 50 {
		t.Errorf("want 50, got %d", snap.DefaultMaxTurns)
	}
}

func TestSnapshot_EndpointsFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("endpoints", json.RawMessage(`{"public_url":"https://hub.example.com","image_registry":"ghcr.io/org"}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.PublicURL != "https://hub.example.com" {
		t.Errorf("want 'https://hub.example.com', got %q", snap.PublicURL)
	}
	if snap.ImageRegistry != "ghcr.io/org" {
		t.Errorf("want 'ghcr.io/org', got %q", snap.ImageRegistry)
	}
}

func TestSnapshot_GitHubAppFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("github_app", json.RawMessage(`{"app_id":123,"api_base_url":"https://api.github.com","webhooks_enabled":true}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.GitHubAppID != 123 {
		t.Errorf("want 123, got %d", snap.GitHubAppID)
	}
	if snap.GitHubAPIBaseURL != "https://api.github.com" {
		t.Errorf("want 'https://api.github.com', got %q", snap.GitHubAPIBaseURL)
	}
	if !snap.GitHubWebhooksEnabled {
		t.Error("want WebhooksEnabled true")
	}
}

func TestSnapshot_GitHubAppExcludesSecrets(t *testing.T) {
	// Even if someone puts secrets in the DB row, the snapshot should never
	// carry private_key or webhook_secret — those fields don't exist on the
	// GitHubAppSettings struct and the schema rejects additionalProperties.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("github_app", json.RawMessage(`{
		"app_id":123,
		"private_key_path":"/path/to/key.pem"
	}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.GitHubAppID != 123 {
		t.Errorf("want 123, got %d", snap.GitHubAppID)
	}
	if snap.GitHubPrivateKeyPath != "/path/to/key.pem" {
		t.Errorf("want '/path/to/key.pem', got %q", snap.GitHubPrivateKeyPath)
	}
	// The Layer1Snapshot struct does not have fields for private_key or
	// webhook_secret, so they are structurally excluded.
}
