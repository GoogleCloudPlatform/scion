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

// setEnvForTest sets an env var and returns a cleanup function.
func setEnvForTest(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

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

func (f *fakeHubSettingStore) UpsertHubSetting(_ context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
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

func (f *fakeHubSettingStore) seedWithOrigin(section string, doc json.RawMessage, origin string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[section] = &store.HubSetting{
		ID:       section,
		Section:  section,
		Value:    doc,
		Revision: 1,
		Origin:   origin,
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

func TestSnapshot_Precedence_DBOverBootstrap(t *testing.T) {
	// Bootstrap merge says admin_emails = ["bootstrap@example.com"]
	bootstrapK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":      []interface{}{"bootstrap@example.com"},
		"server.auth.user_access_mode": "open",
	})

	// DB says admin_emails = ["db@example.com"]
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["db@example.com"],"user_access_mode":"domain_restricted"}`))

	// Env koanf is provided but NOT merged on top — only used for override detection.
	envK := newEnvKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@example.com"},
	})

	ops := NewOperationalSettings(fakeStore, bootstrapK, envK)
	_, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := ops.Snapshot()

	// DB wins over bootstrap (env is NOT merged on top).
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "db@example.com" {
		t.Errorf("AdminEmails: want [db@example.com], got %v", snap.AdminEmails)
	}

	if snap.UserAccessMode != "domain_restricted" {
		t.Errorf("UserAccessMode: want domain_restricted, got %s", snap.UserAccessMode)
	}

	// Env overrides are still detected (for the GET response) even though
	// they don't affect Snapshot merge.
	if len(snap.EnvOverrides) != 1 || snap.EnvOverrides[0] != "server.hub.admin_emails" {
		t.Errorf("EnvOverrides: want [server.hub.admin_emails], got %v", snap.EnvOverrides)
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
	rev, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
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
	_, err := ops.Update(context.Background(), "access", doc, "test@user.com", -1, "managed")
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
	// After H1 generalization, all env keys are reported (including Layer-0).
	if !overrides["server.hub.port"] {
		t.Error("expected server.hub.port in EnvOverrides (all env keys reported)")
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

	// ApplySnapshot must NOT touch MaintenanceState — maintenance is runtime/
	// API-owned state handled separately by ApplyMaintenanceFromSnapshot.
	if srv.maintenance.IsEnabled() {
		t.Error("MaintenanceState.IsEnabled: want false (ApplySnapshot must not touch maintenance)")
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

// --- NB5 regression tests ---

func TestApplyMaintenanceFromSnapshot_NoOpWhenNoDBRow(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(true, "startup-value"),
	}

	snap := Layer1Snapshot{
		AdminMode:         false,
		HasMaintenanceRow: false, // no DB row
	}

	ApplyMaintenanceFromSnapshot(srv, snap)

	// No-op when HasMaintenanceRow is false — retains startup state.
	if !srv.maintenance.IsEnabled() {
		t.Error("want maintenance enabled (no-op when HasMaintenanceRow=false)")
	}
	if srv.maintenance.Message() != "startup-value" {
		t.Errorf("want message 'startup-value', got %q", srv.maintenance.Message())
	}
}

func TestApplyMaintenanceFromSnapshot_DBRowApplied(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	snap := Layer1Snapshot{
		AdminMode:          true,
		MaintenanceMessage: "DB maintenance",
		HasMaintenanceRow:  true,
	}

	ApplyMaintenanceFromSnapshot(srv, snap)

	if !srv.maintenance.IsEnabled() {
		t.Error("want maintenance enabled from DB row")
	}
	if srv.maintenance.Message() != "DB maintenance" {
		t.Errorf("want message 'DB maintenance', got %q", srv.maintenance.Message())
	}
}

func TestApplyMaintenanceFromSnapshot_EnvDoesNotOverrideDB(t *testing.T) {
	// Env vars are set, but maintenance comes from DB — env no longer
	// force-wins in HA mode (cluster-consistency requirement).
	setEnvForTest(t, "SCION_SERVER_ADMIN_MODE", "true")
	setEnvForTest(t, "SCION_SERVER_MAINTENANCE_MESSAGE", "env-msg")

	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	snap := Layer1Snapshot{
		AdminMode:          false,
		MaintenanceMessage: "from-db",
		HasMaintenanceRow:  true,
	}

	ApplyMaintenanceFromSnapshot(srv, snap)

	// DB wins — env does NOT override.
	if srv.maintenance.IsEnabled() {
		t.Error("want maintenance disabled (DB says false, env no longer overrides)")
	}
	if srv.maintenance.Message() != "from-db" {
		t.Errorf("want message 'from-db', got %q", srv.maintenance.Message())
	}
}

func TestApplySnapshot_FileMode_DoesNotModifyMaintenance(t *testing.T) {
	// B2 regression test: simulate the file-mode scenario where maintenance
	// is enabled via API, then a config reload (PUT /admin/server-config)
	// triggers reloadSettings → BuildLayer1SnapshotFromFile → ApplySnapshot.
	// Maintenance must NOT be reset.

	// 1. Initialize server with maintenance enabled (as if set via PUT /admin/maintenance).
	srv := &Server{
		maintenance: NewMaintenanceState(true, "Enabled via API"),
	}

	// 2. Build a file-mode snapshot with AdminMode=false (typical settings.yaml).
	gc := &config.GlobalConfig{
		Hub: config.HubServerConfig{
			AdminEmails: []string{"admin@file.com"},
		},
		Auth: config.DevAuthConfig{
			UserAccessMode: "open",
		},
		AdminMode: false, // file says not in maintenance
	}
	snap := BuildLayer1SnapshotFromFile(gc)

	// 3. Apply snapshot (file-mode path — reloadSettings calls this).
	ApplySnapshot(srv, snap)

	// 4. Assert: maintenance state is unchanged (still enabled from API).
	if !srv.maintenance.IsEnabled() {
		t.Error("maintenance should remain enabled after file-mode ApplySnapshot (B2 regression)")
	}
	if srv.maintenance.Message() != "Enabled via API" {
		t.Errorf("maintenance message should remain 'Enabled via API', got %q", srv.maintenance.Message())
	}
}

func TestConcurrentRefreshAndSnapshot(t *testing.T) {
	// NB5(c): concurrent Refresh+Snapshot race test.
	// Run with -race to detect data races.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["a@b.com"]}`))
	fakeStore.seed("lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`))
	fakeStore.seed("maintenance", json.RawMessage(`{"admin_mode":true,"maintenance_message":"test"}`))

	fileK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"file@example.com"},
	})
	ops := NewOperationalSettings(fakeStore, fileK, emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	// Run concurrent Refresh and Snapshot calls.
	var wg sync.WaitGroup
	const goroutines = 10
	const iterations = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = ops.Refresh(context.Background())
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				snap := ops.Snapshot()
				// Access fields to ensure no race on reads.
				_ = snap.AdminEmails
				_ = snap.AdminMode
				_ = snap.HasMaintenanceRow
				_ = snap.MaintenanceMessage
				_ = snap.AutoSuspendStalled
				_ = snap.EnvOverrides
			}
		}()
	}

	wg.Wait()
	// If we reach here without the race detector firing, the test passes.
}

func TestSnapshot_MixedSource_DBAndFile(t *testing.T) {
	// Part A item 6: section A present in DB, section B file-only.
	// Assert A = DB values, B = file values in one snapshot.

	// File has both access and lifecycle values.
	fileK := newFileKoanf(t, map[string]interface{}{
		"server.hub.admin_emails":          []interface{}{"file@example.com"},
		"server.auth.user_access_mode":     "open",
		"server.hub.auto_suspend_stalled":  false,
		"server.hub.soft_delete_retention": "30d",
	})

	// DB has access section only (DB-provided values).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("access", json.RawMessage(`{"admin_emails":["db@example.com"],"user_access_mode":"domain_restricted"}`))
	// lifecycle is NOT in DB — should fall back to file.

	ops := NewOperationalSettings(fakeStore, fileK, emptyKoanf())
	_, err := ops.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := ops.Snapshot()

	// Section A (access): values must come from DB.
	if len(snap.AdminEmails) != 1 || snap.AdminEmails[0] != "db@example.com" {
		t.Errorf("AdminEmails: want [db@example.com] (from DB), got %v", snap.AdminEmails)
	}
	if snap.UserAccessMode != "domain_restricted" {
		t.Errorf("UserAccessMode: want domain_restricted (from DB), got %s", snap.UserAccessMode)
	}

	// Section B (lifecycle): values must come from file.
	if snap.AutoSuspendStalled != false {
		t.Error("AutoSuspendStalled: want false (from file)")
	}
	if snap.SoftDeleteRetention != "30d" {
		t.Errorf("SoftDeleteRetention: want '30d' (from file), got %q", snap.SoftDeleteRetention)
	}
}
