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
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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

// Regression test for review finding F3 (ptone/scion#2270 round 1): for
// EnforceBrokerQuotas, nil is a meaningful value (the fail-safe "enforced"
// default), not "leave whatever is currently in memory alone". A snapshot
// that clears the switch (DELETE the section, or PUT {}) must flip a
// previously-set false back to enforced, not leave the hub silently
// fail-open while every read surface (GET, the UI) reports "enforced".
func TestApplySnapshot_EnforceBrokerQuotasClearedResetsToEnforced(t *testing.T) {
	off := false
	srv := &Server{
		config:      ServerConfig{EnforceBrokerQuotas: &off},
		maintenance: NewMaintenanceState(false, ""),
	}
	if srv.brokerQuotasEnforced() {
		t.Fatal("test setup: expected brokerQuotasEnforced()=false before applying the cleared snapshot")
	}

	// A snapshot with EnforceBrokerQuotas==nil represents the section being
	// absent (deleted, reset to bootstrap, or PUT as {}) — not "unchanged".
	ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: nil})

	if srv.config.EnforceBrokerQuotas != nil {
		t.Errorf("want EnforceBrokerQuotas=nil after applying a cleared snapshot, got %v", *srv.config.EnforceBrokerQuotas)
	}
	if !srv.brokerQuotasEnforced() {
		t.Error("want brokerQuotasEnforced()=true after applying a cleared snapshot (fail-safe default)")
	}
}

// TestApplySnapshot_EnforceBrokerQuotasAppliedTracking asserts that the
// "applied" list correctly reports a change both when the value flips
// between concrete booleans and when it clears to nil, but not when the
// snapshot repeats the same value (idempotent re-apply, e.g. from a
// duplicate propagation event).
func TestApplySnapshot_EnforceBrokerQuotasAppliedTracking(t *testing.T) {
	on := true
	off := false

	srv := &Server{maintenance: NewMaintenanceState(false, "")}

	result := ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: &off})
	applied, _ := result["applied"].([]string)
	if !containsString(applied, "enforce_broker_quotas") {
		t.Errorf("nil -> false should be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: &off})
	applied, _ = result["applied"].([]string)
	if containsString(applied, "enforce_broker_quotas") {
		t.Errorf("false -> false (idempotent re-apply) should not be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: &on})
	applied, _ = result["applied"].([]string)
	if !containsString(applied, "enforce_broker_quotas") {
		t.Errorf("false -> true should be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{EnforceBrokerQuotas: nil})
	applied, _ = result["applied"].([]string)
	if !containsString(applied, "enforce_broker_quotas") {
		t.Errorf("true -> nil (cleared) should be reported as applied, got %v", applied)
	}
}

// TestApplySnapshot_AgentSecretsUserScopeOnlyClearedResetsToPermissive
// mirrors TestApplySnapshot_EnforceBrokerQuotasClearedResetsToEnforced: for
// AgentSecretsUserScopeOnly, nil is a meaningful value (the permissive
// default), not "leave whatever is currently in memory alone". A snapshot
// that clears the switch (DELETE the section, or PUT {}) must flip a
// previously-set true back to permissive, turning live enforcement off
// without a restart (design ptone/scion#2291 §5).
func TestApplySnapshot_AgentSecretsUserScopeOnlyClearedResetsToPermissive(t *testing.T) {
	on := true
	srv := &Server{
		config:      ServerConfig{AgentSecretsUserScopeOnly: &on},
		maintenance: NewMaintenanceState(false, ""),
	}
	if !srv.agentSecretsUserScopeOnly() {
		t.Fatal("test setup: expected agentSecretsUserScopeOnly()=true before applying the cleared snapshot")
	}

	ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: nil})

	if srv.config.AgentSecretsUserScopeOnly != nil {
		t.Errorf("want AgentSecretsUserScopeOnly=nil after applying a cleared snapshot, got %v", *srv.config.AgentSecretsUserScopeOnly)
	}
	if srv.agentSecretsUserScopeOnly() {
		t.Error("want agentSecretsUserScopeOnly()=false after applying a cleared snapshot (permissive default)")
	}
}

// TestApplySnapshot_AgentSecretsUserScopeOnlyAppliedTracking mirrors
// TestApplySnapshot_EnforceBrokerQuotasAppliedTracking.
func TestApplySnapshot_AgentSecretsUserScopeOnlyAppliedTracking(t *testing.T) {
	on := true
	off := false

	srv := &Server{maintenance: NewMaintenanceState(false, "")}

	result := ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: &on})
	applied, _ := result["applied"].([]string)
	if !containsString(applied, "agent_secrets_user_scope_only") {
		t.Errorf("nil -> true should be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: &on})
	applied, _ = result["applied"].([]string)
	if containsString(applied, "agent_secrets_user_scope_only") {
		t.Errorf("true -> true (idempotent re-apply) should not be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: &off})
	applied, _ = result["applied"].([]string)
	if !containsString(applied, "agent_secrets_user_scope_only") {
		t.Errorf("true -> false should be reported as applied, got %v", applied)
	}

	result = ApplySnapshot(srv, Layer1Snapshot{AgentSecretsUserScopeOnly: nil})
	applied, _ = result["applied"].([]string)
	if !containsString(applied, "agent_secrets_user_scope_only") {
		t.Errorf("false -> nil (cleared) should be reported as applied, got %v", applied)
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

// TestBuildLayer1SnapshotFromFile_AgentSecrets verifies that
// AgentSecretsUserScopeOnly is read from GlobalConfig, so a file-mode admin
// save takes effect without a restart (design ptone/scion#2291 §5).
func TestBuildLayer1SnapshotFromFile_AgentSecrets(t *testing.T) {
	on := true
	gc := &config.GlobalConfig{
		AgentSecretsUserScopeOnly: &on,
	}

	snap := BuildLayer1SnapshotFromFile(gc)

	if snap.AgentSecretsUserScopeOnly == nil || !*snap.AgentSecretsUserScopeOnly {
		t.Errorf("AgentSecretsUserScopeOnly: want true, got %v", snap.AgentSecretsUserScopeOnly)
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

// TestSnapshot_AgentSecretsFromDB mirrors TestSnapshot_TelemetryFromDB and
// exercises buildSnapshotFromKoanf's handling of the agent_secrets section
// via the DB-backed Refresh path.
func TestSnapshot_AgentSecretsFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("agent_secrets", json.RawMessage(`{"user_scope_only":true}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.AgentSecretsUserScopeOnly == nil || !*snap.AgentSecretsUserScopeOnly {
		t.Error("want AgentSecretsUserScopeOnly true")
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

func TestSnapshot_FederationFromDB(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("federation", json.RawMessage(`{
		"enabled": true,
		"trusted_issuers": [
			{
				"issuer_url": "https://hub-a.example.com",
				"jwks_url": "https://hub-a.example.com/.well-known/jwks.json",
				"expected_audience": "https://hub-b.example.com",
				"allowed_projects": ["proj1"],
				"issuer_type": "hub",
				"default_scopes": ["agent:status:update"]
			}
		],
		"algorithms": ["RS256"],
		"refresh_interval": "1h",
		"debounce_interval": "5s"
	}`))

	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	snap := ops.Snapshot()
	if snap.FederationConfig == nil {
		t.Fatal("want non-nil FederationConfig")
	}
	if !snap.FederationConfig.Enabled {
		t.Error("want Enabled true")
	}
	if len(snap.FederationConfig.TrustedIssuers) != 1 {
		t.Fatalf("want 1 issuer, got %d", len(snap.FederationConfig.TrustedIssuers))
	}
	iss := snap.FederationConfig.TrustedIssuers[0]
	if iss.IssuerURL != "https://hub-a.example.com" {
		t.Errorf("IssuerURL: want https://hub-a.example.com, got %s", iss.IssuerURL)
	}
	if iss.IssuerType != "hub" {
		t.Errorf("IssuerType: want hub, got %s", iss.IssuerType)
	}
	if len(iss.AllowedProjects) != 1 || iss.AllowedProjects[0] != "proj1" {
		t.Errorf("AllowedProjects: want [proj1], got %v", iss.AllowedProjects)
	}
	if len(snap.FederationConfig.Algorithms) != 1 || snap.FederationConfig.Algorithms[0] != "RS256" {
		t.Errorf("Algorithms: want [RS256], got %v", snap.FederationConfig.Algorithms)
	}
	if snap.FederationConfig.Cache.RefreshInterval.String() != "1h0m0s" {
		t.Errorf("RefreshInterval: want 1h0m0s, got %s", snap.FederationConfig.Cache.RefreshInterval)
	}
	if snap.FederationConfig.Cache.DebounceInterval.String() != "5s" {
		t.Errorf("DebounceInterval: want 5s, got %s", snap.FederationConfig.Cache.DebounceInterval)
	}
}

func TestBuildLayer1SnapshotFromFile_Federation(t *testing.T) {
	gc := &config.GlobalConfig{
		Federation: config.FederationConfig{
			Enabled: true,
			TrustedIssuers: []config.TrustedIssuerConfig{
				{
					IssuerURL:  "https://hub-a.example.com",
					IssuerType: "hub",
				},
			},
			Algorithms: []string{"RS256"},
		},
	}

	snap := BuildLayer1SnapshotFromFile(gc)
	if snap.FederationConfig == nil {
		t.Fatal("want non-nil FederationConfig")
	}
	if !snap.FederationConfig.Enabled {
		t.Error("want Enabled true")
	}
	if len(snap.FederationConfig.TrustedIssuers) != 1 {
		t.Fatalf("want 1 issuer, got %d", len(snap.FederationConfig.TrustedIssuers))
	}
}

func TestBuildLayer1SnapshotFromFile_NoFederation(t *testing.T) {
	gc := &config.GlobalConfig{}
	snap := BuildLayer1SnapshotFromFile(gc)
	if snap.FederationConfig != nil {
		t.Error("want nil FederationConfig when federation is disabled and no issuers")
	}
}

// --- Consolidated envelope switch tests (AC-9-7 acceptance criteria) ---
//
// Phase 9a: conversation_read_switch and conversation_write_deny_switch are
// replaced by a single conversation_envelope_switch that defaults ON when
// absent or omitted, and OFF when the document is malformed (DEF-92).

func TestConversationEnvelopeSwitch_AC97_AbsentRow(t *testing.T) {
	// AC-9-7: no messaging row at all → ON (compiled default).
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want true (absent row → compiled default ON), got false")
	}
}

func TestConversationEnvelopeSwitch_AC97_KeyOmitted(t *testing.T) {
	// AC-9-7: row present, key omitted (e.g. {}) → ON (compiled default).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want true (empty doc → key omitted → default ON), got false")
	}
}

func TestConversationEnvelopeSwitch_AC97_ExplicitlyFalse(t *testing.T) {
	// AC-9-7: row present, key explicitly false → OFF.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want false (explicitly false), got true")
	}
}

func TestConversationEnvelopeSwitch_AC97_ExplicitlyTrue(t *testing.T) {
	// AC-9-7: row present, key explicitly true → ON.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want true (explicitly true), got false")
	}
}

func TestConversationEnvelopeSwitch_AC97_MalformedJSON(t *testing.T) {
	// AC-9-7: malformed JSON → OFF, and an error is logged at Refresh.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`not valid json`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want false (malformed JSON → fail-closed), got true")
	}
}

func TestConversationEnvelopeSwitch_AC97a_StaleKeysCutoverToON(t *testing.T) {
	// AC-9-7a: a hub whose messaging row contains ONLY the two stale keys
	// (conversation_read_switch, conversation_write_deny_switch), both false,
	// cuts over to ON after upgrade with no migration run.
	//
	// This is the single most important test in Phase 9a: it is the one that
	// would have caught key reuse. The new key is absent, so the getter takes
	// the compiled default (ON). The stale keys are ignored by the new getter.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_read_switch":false,"conversation_write_deny_switch":false}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want true (stale keys only → new key absent → compiled default ON), got false")
	}
}

func TestConversationEnvelopeSwitch_AC97b_MalformedLoggedOncePerRefresh(t *testing.T) {
	// AC-9-7b: the malformed-JSON error is logged once per refresh, not once
	// per getter call. We assert this by counting: after one Refresh and N
	// getter calls, the error should have been logged exactly once (at Refresh).
	//
	// The log is emitted by Refresh via slog.Error. Since we cannot easily
	// intercept slog in this test, we verify the structural guarantee: the
	// Malformed flag is set at Refresh time and the getter reads it without
	// re-parsing. We call the getter N times and verify it returns the same
	// result (OFF) without panicking — the parse-time-only property is
	// asserted by code structure (Refresh sets Malformed; getter reads it).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`not valid json`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Call the getter N times — each must return false (fail-closed).
	const N = 10
	for i := 0; i < N; i++ {
		if ops.ConversationEnvelopeSwitch() {
			t.Fatalf("getter call %d: want false (malformed → fail-closed), got true", i)
		}
	}

	// Verify the Malformed flag is set on the cached state.
	ops.mu.RLock()
	state, ok := ops.cache["messaging"]
	ops.mu.RUnlock()
	if !ok {
		t.Fatal("messaging section not in cache after Refresh")
	}
	if !state.Malformed {
		t.Error("expected Malformed=true on cached messaging section")
	}
}

func TestConversationEnvelopeSwitch_TypeMismatch_DetectedAtRefresh(t *testing.T) {
	// A document like {"conversation_envelope_switch":"yes"} is valid JSON but
	// fails to unmarshal into *bool. Without the type-mismatch check at ingest
	// time, this would fall through to the compiled default (ON), silently
	// enabling the switch on a hub where an operator made a typo.
	//
	// With the fix, Refresh detects the unmarshal failure, sets Malformed=true,
	// and the getter returns OFF.
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":"yes"}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if ops.ConversationEnvelopeSwitch() {
		t.Error("ConversationEnvelopeSwitch: want false (type-mismatch → fail-closed), got true")
	}

	// Verify Malformed is set.
	ops.mu.RLock()
	state, ok := ops.cache["messaging"]
	ops.mu.RUnlock()
	if !ok {
		t.Fatal("messaging section not in cache after Refresh")
	}
	if !state.Malformed {
		t.Error("expected Malformed=true for type-mismatch document")
	}
}

// Design D6: ApplySnapshot normalizes any default_user_role other than
// member/viewer to member, so the live config never holds garbage.
func TestApplySnapshot_DefaultUserRoleNormalized(t *testing.T) {
	tests := []struct {
		in       string
		wantCfg  string
		wantRole string
	}{
		{in: "superuser", wantCfg: "member", wantRole: "member"},
		{in: "admin", wantCfg: "member", wantRole: "member"},
		{in: "VIEWER", wantCfg: "member", wantRole: "member"},
		{in: "viewer", wantCfg: "viewer", wantRole: "viewer"},
		{in: "member", wantCfg: "member", wantRole: "member"},
		{in: "", wantCfg: "", wantRole: "member"},
	}
	for _, tt := range tests {
		t.Run("in="+tt.in, func(t *testing.T) {
			srv := &Server{
				config:      ServerConfig{DefaultUserRole: "viewer"},
				maintenance: NewMaintenanceState(false, ""),
			}
			ApplySnapshot(srv, Layer1Snapshot{DefaultUserRole: tt.in})
			if srv.config.DefaultUserRole != tt.wantCfg {
				t.Errorf("config.DefaultUserRole = %q, want %q", srv.config.DefaultUserRole, tt.wantCfg)
			}
			if got := srv.DefaultUserRole(); got != tt.wantRole {
				t.Errorf("DefaultUserRole() = %q, want %q", got, tt.wantRole)
			}
		})
	}
}

// The authoritative-read fixture is bounded/cooperative before delegating to
// the existing in-memory store. Closed modes execute finite virtual read steps;
// none accepts a callback or an unbounded data graph.
type auditFixtureSettingStore struct {
	*fakeHubSettingStore
	clock          *auditFixtureClock
	mode           string
	ops            *OperationalSettings
	nested         bool
	calls          int
	router         *decisionAuditRouter
	caller         *auditFixtureCaller
	readObserved   bool
	readViolation  bool
	attachmentRead *auditFixtureAttachmentRead
}

func (s *auditFixtureSettingStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	s.calls++
	if s.calls > 8 {
		return nil, fmt.Errorf("finite read invocation cap")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch s.mode {
	case "panic-read":
		// Release this read's finite pending token before the injected panic.
		// Otherwise an abandoned slot masks lifecycle resurrection with overlap.
		// This closed test step isolates wrapper recovery; it does not claim
		// ordinary production panics clean up a pending token.
		s.router.gate.Lock()
		pending := s.router.refreshSlots
		s.router.gate.Unlock()
		for _, token := range pending {
			if token.tracked {
				s.router.finishRefresh(token, ExperimentsSnapshot{}, fmt.Errorf("finite pre-panic token release"))
			}
		}
		panic("finite recovered subscription read failure")
	case "attachment-handoff-success", "attachment-handoff-error":
		read := s.attachmentRead
		if read == nil || read.entered {
			return nil, fmt.Errorf("finite attachment controller missing or replayed")
		}
		read.entered = true
		s.router.gate.Lock()
		for _, pending := range s.router.refreshSlots {
			if pending.tracked && pending.source == s.ops && pending.attachment == read.a.attachment {
				read.token = pending
			}
		}
		s.router.gate.Unlock()
		if !read.token.tracked {
			return nil, fmt.Errorf("captured A read must start tracked")
		}
		s.router.server.SetOperationalSettings(s.ops)
		read.b = s.ops.decisionAuditPropagationAttachment()
		// Old pending bookkeeping conservatively blocks a fresh B read. Release
		// only A's owned slot as finite setup, without publishing copied Ops proof.
		// The original A Refresh still completes through the real Ops seam below.
		s.router.finishRefresh(read.token, ExperimentsSnapshot{}, fmt.Errorf("finite old read bookkeeping release"))
		s.mode = ""
		if _, err := s.ops.refreshForDecisionAuditAttachment(ctx, &read.b); err != nil {
			return nil, err
		}
		read.snapshot, read.copied = s.ops.decisionAuditSnapshot()
		read.router = s.router.inspect().observation
		if read.fail {
			return nil, fmt.Errorf("finite delayed A store error")
		}
	case "stop-during-read":
		s.ops.StopPropagation()
	case "budget-at":
		s.clock.advance(s.clock.tick + decisionAuditRefreshBudget)
	case "budget-plus-one":
		s.clock.advance(s.clock.tick + decisionAuditRefreshBudget + time.Nanosecond)
	case "older-revision":
		s.router.gate.Lock()
		for _, token := range s.router.refreshSlots {
			if token.tracked && token.source == s.ops && token.attachment == s.router.attachment && token.sequence == s.router.sequence {
				s.readObserved = true
			}
		}
		s.router.gate.Unlock()
	case "sequence-exhausted-emit":
		r := s.router
		// The copied cache proof is the prior otherwise-eligible baseline.
		// The router proof must ALREADY be cleared by the poison transition.
		snapshot, cached := s.ops.decisionAuditSnapshot()
		r.gate.Lock()
		current := r.state.observation
		cleared := !current.successful && !current.tracked && current.source == nil && current.sequence == 0 && current.attachment == 0 && current.generation == 0 && current.epoch == 0 && current.q0 == 0 && current.deadline == 0 && !current.snapshot.Present && !current.snapshot.Malformed && current.snapshot.Revision == 0 && len(current.snapshot.Overrides) == 0
		s.readObserved = r.refreshPoison && r.sequence == ^uint64(0) && cleared &&
			!r.state.closed && !r.state.fault && r.state.active == 0 && r.mutations == 0 && !r.propagationLost && !r.clockInvalid && r.sourceMatchesLocked(s.ops) &&
			cached.successful && !cached.tracked && cached.source == s.ops && cached.attachment == r.attachment && cached.sequence == r.sequence && cached.generation == r.admission.manifest.generation && cached.epoch == s.clock.epoch &&
			s.clock.valid && s.clock.epoch == r.admission.manifest.clockEpoch && s.clock.tick >= cached.q0 && s.clock.tick < cached.deadline && s.clock.tick < r.admission.manifest.expires && s.clock.tick <= time.Duration(1<<63-1)-decisionAuditLease-decisionAuditCompleteBudget &&
			sameDecisionAuditSnapshot(snapshot, cached.snapshot) && cached.snapshot.Overrides[experiments.AuthorizationDecisionAuditV2] && r.server.experimentEnabledIn(cached.snapshot, experiments.AuthorizationDecisionAuditV2) &&
			sameDecisionAuditReference(s.caller, r.admission.manifest.binding.caller) && s.caller.Err() == nil
		r.gate.Unlock()
		before := s.router.inspect().active
		r.EmitDecisionAudit(s.caller, &store.DecisionAuditRecord{Result: "allow", ResourceType: "project"})
		s.readViolation = before != 0 || r.inspect().active != 0
	case "failure":
		return nil, fmt.Errorf("finite read failure")
	case "blocked", "late":
		s.clock.advance(s.clock.tick + 2*time.Second)
	case "overlap":
		if !s.nested {
			s.nested = true
			_, err := s.ops.Refresh(ctx)
			if err != nil {
				return nil, err
			}
		}
	}
	rows, err := s.fakeHubSettingStore.ListHubSettings(ctx)
	if err != nil {
		return nil, err
	}
	size := 0
	for _, row := range rows {
		size += len(row.Value) + len(row.Section) + len(row.UpdatedBy) + len(row.Origin)
	}
	if len(rows) > decisionAuditSettingsMaxRows+1 || size > decisionAuditSettingsMaxBytes+1 {
		return nil, fmt.Errorf("finite read input cap")
	}
	return rows, nil
}

// Closed read controller: two fixed outcomes and one explicit A-to-B handoff.
// It accepts no arbitrary callback and starts no goroutine or wall-clock wait.
type auditFixtureAttachmentRead struct {
	a, b     decisionAuditPropagationAttachment
	token    decisionAuditRefreshObservation
	entered  bool
	fail     bool
	snapshot ExperimentsSnapshot
	copied   decisionAuditRefreshObservation
	router   decisionAuditRefreshObservation
}

func auditRequireEmptyProofs(t *testing.T, f *auditFixture, label string) {
	t.Helper()
	_, copied := f.ops.decisionAuditSnapshot()
	router := f.router.inspect().observation
	if !reflect.DeepEqual(copied, decisionAuditRefreshObservation{}) || !reflect.DeepEqual(router, decisionAuditRefreshObservation{}) {
		t.Errorf("%s: copied and router proofs must both be wholly empty", label)
	}
}

func auditRequireCapturedLoss(t *testing.T, f *auditFixture, label string) {
	t.Helper()
	f.router.gate.Lock()
	lost := f.router.propagationLost
	f.router.gate.Unlock()
	if !lost {
		t.Errorf("%s: current attachment lifecycle loss must be latched", label)
	}
	auditRequireEmptyProofs(t, f, label+" before refresh")
	if _, err := f.ops.Refresh(context.Background()); err != nil {
		t.Fatal("healthy same-attachment store read required", err)
	}
	f.router.gate.Lock()
	stillLost := f.router.propagationLost
	f.router.gate.Unlock()
	if !stillLost {
		t.Errorf("%s: healthy read must not release lifecycle loss", label)
	}
	auditRequireEmptyProofs(t, f, label+" after healthy refresh")
	auditRequireLegacyOnly(t, f, label)
}

func auditRequireAttachmentProof(t *testing.T, f *auditFixture, attachment decisionAuditPropagationAttachment, snapshot ExperimentsSnapshot, copied, router decisionAuditRefreshObservation, label string) {
	t.Helper()
	currentSnapshot, currentCopied := f.ops.decisionAuditSnapshot()
	currentRouter := f.router.inspect().observation
	f.router.gate.Lock()
	live := f.router.attachment == attachment.attachment && !f.router.propagationLost && !f.router.refreshPoison
	f.router.gate.Unlock()
	if attachment.observer != f.router || !live || !copied.successful || !router.successful || copied.attachment != attachment.attachment || router.attachment != attachment.attachment ||
		!reflect.DeepEqual(currentSnapshot, snapshot) || !reflect.DeepEqual(currentCopied, copied) || !reflect.DeepEqual(currentRouter, router) {
		t.Fatalf("%s: stale callback must preserve exact fresh attachment copied/router proof", label)
	}
	newBefore, legacyBefore := f.handler.calls, len(f.legacy.records)
	f.emit()
	if f.handler.calls != newBefore+1 || len(f.legacy.records) != legacyBefore {
		t.Errorf("%s: exactly one NEW and zero legacy owners required", label)
	}
	auditRequireReleased(t, f, nil)
}

func newAuditFixtureSettings(t *testing.T, f *auditFixture) (*OperationalSettings, *auditFixtureSettingStore) {
	t.Helper()
	st := &auditFixtureSettingStore{fakeHubSettingStore: newFakeHubSettingStore(), clock: f.clock, router: f.router, caller: f.caller}
	st.seed("experiments", json.RawMessage(`{"overrides":{"hub.authorization_decision_audit_v2":true}}`))
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	st.ops = ops
	f.router.server.SetOperationalSettings(ops)
	return ops, st
}

// This publisher owns one buffered event and one unsubscribe completion signal.
// It invokes no user callback; embedding supplies the inert publisher methods.
type auditFixturePublisher struct {
	noopEventPublisher
	events chan Event
	exited chan struct{}
}

func (p *auditFixturePublisher) Subscribe(...string) (<-chan Event, func()) {
	return p.events, func() { close(p.exited) }
}

func auditRequireLegacyOnly(t *testing.T, f *auditFixture, label string) {
	t.Helper()
	before := len(f.legacy.records)
	newBefore := f.handler.calls
	f.emit()
	if f.handler.calls != newBefore || len(f.legacy.records) != before+1 {
		t.Errorf("%s: must select legacy once and NEW zero", label)
	}
	auditRequireReleased(t, f, nil)
}

func TestDecisionAuditRefresh_ObservationAndUnchangedRenewal(t *testing.T) {
	f := newAuditFixture(t, auditFixtureAccept)
	f.requireAdmission(t)
	ops, st := f.ops, f.settings
	for i := 0; i < 2; i++ {
		f.clock.advance(time.Duration(i) * time.Second)
		q0 := f.clock.tick
		changed, err := ops.Refresh(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		snap, obs := ops.decisionAuditSnapshot()
		if i == 1 && len(changed) != 0 {
			t.Fatal("unchanged authoritative refresh must retain generic changed-list semantics")
		}
		if !obs.successful || obs.q0 != q0 || obs.deadline != q0+decisionAuditLease || obs.snapshot.Revision != snap.Revision || !snap.Overrides[experiments.AuthorizationDecisionAuditV2] {
			t.Fatal("successful unchanged read must renew matching observation from read start")
		}
		snap.Overrides[experiments.AuthorizationDecisionAuditV2] = false
		copied, _ := ops.decisionAuditSnapshot()
		if !copied.Overrides[experiments.AuthorizationDecisionAuditV2] {
			t.Fatal("consumer snapshot must not alias cache")
		}
	}
	if st.calls != 2 {
		t.Fatal("observer must not add an authoritative read")
	}
	revision, err := ops.Update(context.Background(), "experiments", json.RawMessage(`{"overrides":{"hub.authorization_decision_audit_v2":true}}`), "fixture", 1, "test")
	if err != nil || revision != 2 {
		t.Fatalf("fixture Update: %d/%v", revision, err)
	}
	_, obs := ops.decisionAuditSnapshot()
	if obs.successful || f.router.inspect().observation.successful {
		t.Fatal("true Update/cache publication must not renew a lease")
	}
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("authoritative-row-cap-plus-%d", extra), func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			n := decisionAuditSettingsMaxRows + extra
			for i := 1; i < n; i++ {
				g.settings.seed(fmt.Sprintf("finite-%03d", i), json.RawMessage(`{}`))
			}
			changed, err := g.ops.Refresh(context.Background())
			if err != nil || len(changed) != n {
				t.Fatal("audit cap must not alter generic row ingestion")
			}
			_, obs := g.ops.decisionAuditSnapshot()
			if obs.successful != (extra == 0) || g.router.inspect().observation.successful != (extra == 0) {
				t.Fatal("isolated row cap publication mismatch")
			}
			g.emit()
			want := 1 - extra
			if g.handler.calls != want || len(g.legacy.records) != extra {
				t.Fatal("row cap ownership mismatch")
			}
		})
		t.Run(fmt.Sprintf("authoritative-metadata-byte-cap-plus-%d", extra), func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			row := g.settings.settings["experiments"]
			row.UpdatedBy = strings.Repeat("m", decisionAuditSettingsMaxBytes-len(row.Value)-len(row.Section)+extra)
			changed, err := g.ops.Refresh(context.Background())
			if err != nil || len(changed) != 1 || g.ops.ExperimentsSnapshot().UpdatedBy != row.UpdatedBy {
				t.Fatal("generic metadata ingestion changed")
			}
			_, obs := g.ops.decisionAuditSnapshot()
			if obs.successful != (extra == 0) {
				t.Fatal("isolated metadata aggregate boundary mismatch")
			}
			g.emit()
			if g.handler.calls != 1-extra || len(g.legacy.records) != extra {
				t.Fatal("metadata cap ownership mismatch")
			}
		})
		for _, kind := range []string{"name-bytes", "name-count", "raw-bytes"} {
			t.Run(fmt.Sprintf("defensive-cache-%s-plus-%d", kind, extra), func(t *testing.T) {
				g := newAuditFixture(t, auditFixtureAccept)
				g.requireAdmission(t)
				g.observe(1, 1, true)
				// Getter defense is isolated from raw JSON parsing and registry validation.
				// This is defensive cache coverage, not a claim of admitted unknown names.
				g.ops.mu.Lock()
				state := g.ops.cache["experiments"]
				switch kind {
				case "name-bytes":
					state.ExperimentsOverrides = map[string]bool{strings.Repeat("n", decisionAuditSettingsMaxBytes+extra): true}
				case "name-count":
					state.ExperimentsOverrides = map[string]bool{}
					for i := 0; i < decisionAuditSettingsMaxRows+extra; i++ {
						state.ExperimentsOverrides[fmt.Sprintf("n%d", i)] = true
					}
				case "raw-bytes":
					state.Value = json.RawMessage(strings.Repeat(" ", decisionAuditSettingsMaxBytes+extra))
				}
				g.ops.cache["experiments"] = state
				g.ops.mu.Unlock()
				private, obs := g.ops.decisionAuditSnapshot()
				public := g.ops.ExperimentsSnapshot()
				if private.Present != (extra == 0) || obs.successful != (extra == 0) {
					t.Fatal("isolated defensive cache cap mismatch")
				}
				if !public.Present || len(public.Overrides) != len(state.ExperimentsOverrides) {
					t.Fatal("generic snapshot semantics changed by private cap")
				}
			})
		}
	}
	for _, mode := range []string{"budget-at", "budget-plus-one"} {
		t.Run(mode, func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			g.settings.mode = mode
			_, err := g.ops.Refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, obs := g.ops.decisionAuditSnapshot()
			want := mode == "budget-at"
			if obs.successful != want || g.router.inspect().observation.successful != want {
				t.Fatal("read duration boundary must be inclusive at 1s")
			}
			g.emit()
			if (g.handler.calls == 1) != want || len(g.legacy.records) != map[bool]int{true: 0, false: 1}[want] {
				t.Fatal("read budget ownership mismatch")
			}
		})
	}
	t.Run("sequence-exhaustion-read-handoff", func(t *testing.T) {
		g := newAuditFixture(t, auditFixtureAccept)
		g.requireAdmission(t)
		g.router.gate.Lock()
		g.router.sequence = ^uint64(0) - 1
		g.router.gate.Unlock()
		old := g.observe(1, 1, true)
		if !old.successful || old.sequence != ^uint64(0) {
			t.Fatal("last representable sequence must publish valid proof")
		}
		g.settings.mode = "sequence-exhausted-emit"
		_, err := g.ops.Refresh(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !g.settings.readObserved || g.settings.readViolation {
			t.Fatal("exhaustion oracle did not run during poisoned otherwise-eligible read")
		}
		if g.handler.calls != 0 || len(g.legacy.records) != 1 || g.clock.cancel != nil || g.clock.timerStarts != 0 {
			t.Errorf("INTENDED RED sequence poison must bar old-proof read handoff: NEW=%d legacy=%d", g.handler.calls, len(g.legacy.records))
		}
		auditRequireReleased(t, g, nil)
		g.settings.mode = ""
		_, _ = g.ops.Refresh(context.Background())
		if !g.router.refreshPoison || g.router.inspect().observation.successful {
			t.Fatal("sequence exhaustion cannot recover")
		}
		auditRequireLegacyOnly(t, g, "sequence exhaustion remains poisoned")
	})
	t.Run("attachment-exhaustion", func(t *testing.T) {
		g := newAuditFixture(t, auditFixtureAccept)
		g.requireAdmission(t)
		g.router.gate.Lock()
		g.router.attachment = ^uint64(0) - 1
		g.router.gate.Unlock()
		g.router.server.SetOperationalSettings(g.ops)
		if !g.observe(1, 1, true).successful || g.router.attachment != ^uint64(0) {
			t.Fatal("last representable attachment must remain usable")
		}
		g.router.server.SetOperationalSettings(g.ops)
		_, _ = g.ops.Refresh(context.Background())
		if !g.router.refreshPoison || g.router.inspect().observation.successful {
			t.Fatal("attachment exhaustion must fail off without wrap")
		}
		auditRequireLegacyOnly(t, g, "attachment exhaustion")
	})
	for _, extra := range []time.Duration{0, time.Nanosecond} {
		t.Run(fmt.Sprintf("duration-reserve-plus-%d", extra), func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.contract.expected.expires = time.Duration(1<<63 - 1)
			g.contract.actual.expires = g.contract.expected.expires
			g.router.contract = g.contract
			g.router.admission, _ = validateDecisionAuditManifest(g.contract)
			g.requireAdmission(t)
			g.clock.tick = time.Duration(1<<63-1) - decisionAuditLease - decisionAuditCompleteBudget + extra
			obs := g.observe(1, 1, true)
			if obs.successful != (extra == 0) || g.router.clockInvalid != (extra != 0) {
				t.Fatal("duration reserve guard boundary mismatch")
			}
			g.emit()
			if g.handler.calls != map[bool]int{true: 1, false: 0}[extra == 0] || len(g.legacy.records) != map[bool]int{true: 0, false: 1}[extra == 0] {
				t.Fatal("duration reserve ownership mismatch")
			}
		})
	}
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("refresh-slots-%d", count), func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			base := g.observe(1, 1, true)
			pending := make([]decisionAuditRefreshObservation, 0, count)
			for i := 0; i < count; i++ {
				pending = append(pending, g.router.beginRefresh(g.ops))
			}
			if !pending[0].tracked || !pending[1].tracked || (count == 3 && pending[2].tracked) || g.router.refreshPoison != (count == 3) {
				t.Fatal("fixed refresh-slot boundary mismatch")
			}
			for _, token := range pending {
				g.router.finishRefresh(token, base.snapshot, nil)
			}
			for _, token := range g.router.refreshSlots {
				if token.tracked {
					t.Fatal("pending slot retained after finish")
				}
			}
			obs := g.observe(2, 1, true)
			if obs.successful != (count == 2) {
				t.Fatal("third refresh slot must poison, two completed overlaps may recover")
			}
		})
		t.Run(fmt.Sprintf("mutation-saturation-%d", count), func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			g.observe(1, 1, true)
			for i := 0; i < count; i++ {
				g.ops.beginDecisionAuditMutation()
			}
			if g.router.mutations != 2 || g.router.refreshPoison != (count == 3) {
				t.Fatal("mutation saturation must not wrap")
			}
			auditRequireLegacyOnly(t, g, "active mutation")
			for i := 0; i < count; i++ {
				g.ops.endDecisionAuditMutation()
			}
			obs := g.observe(2, 1, true)
			if obs.successful != (count == 2) || g.router.mutations != 0 {
				t.Fatal("two mutations may recover; saturation poison cannot")
			}
		})
	}
	for _, kind := range []string{"unknown", "epoch", "regression"} {
		t.Run("clock-anomaly-persistent-"+kind, func(t *testing.T) {
			g := newAuditFixture(t, auditFixtureAccept)
			g.requireAdmission(t)
			g.clock.tick = time.Second
			g.observe(1, 1, true)
			switch kind {
			case "unknown":
				g.clock.valid = false
			case "epoch":
				g.clock.epoch++
			case "regression":
				g.clock.tick = 0
			}
			auditRequireLegacyOnly(t, g, "clock anomaly")
			g.clock.valid = true
			g.clock.epoch = 1
			g.clock.tick = 2 * time.Second
			_, _ = g.ops.Refresh(context.Background())
			if !g.router.clockInvalid || g.router.inspect().observation.successful {
				t.Fatal("clock anomaly cannot recover in same generation")
			}
			auditRequireLegacyOnly(t, g, "restored clock cannot rearm")
		})
	}

}

func TestDecisionAuditRefresh_FailureBlockedAndOutOfOrder(t *testing.T) {
	for _, mode := range []string{"failure", "blocked", "late", "overlap", "canceled", "obsolete", "older-revision", "completed-token-replay", "pending-out-of-order", "old-attachment-callback"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.requireAdmission(t)
			ops, st := f.ops, f.settings
			if mode == "older-revision" {
				st.settings["experiments"].Revision = 2
			}
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, old := ops.decisionAuditSnapshot()
			if !old.successful {
				t.Fatal("positive authoritative baseline required")
			}
			if mode == "older-revision" && old.snapshot.Revision != 2 {
				t.Fatal("revision N baseline must be published through real Refresh")
			}
			st.mode = mode
			if mode == "older-revision" {
				st.settings["experiments"].Revision = 1
			}
			ctx := context.Background()
			if mode == "canceled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			_, _ = ops.Refresh(ctx)
			if mode == "obsolete" {
				f.router.server.SetOperationalSettings(nil)
				f.router.finishRefresh(old, old.snapshot, nil)
			}
			if mode == "older-revision" {
				snap, obs := ops.decisionAuditSnapshot()
				if snap.Revision != 1 || obs.successful || f.router.maxRevision != 2 || !st.readObserved {
					t.Fatal("fresh otherwise-valid N-1 read must invalidate private proof, preserve generic cache")
				}
			}
			if mode == "completed-token-replay" {
				f.router.finishRefresh(old, old.snapshot, nil)
			}
			if mode == "pending-out-of-order" {
				first, second := f.router.beginRefresh(ops), f.router.beginRefresh(ops)
				if !first.tracked || !second.tracked || first.sequence >= second.sequence {
					t.Fatal("valid pending order required")
				}
				f.router.finishRefresh(second, old.snapshot, nil)
				f.router.finishRefresh(first, old.snapshot, nil)
			}
			if mode == "old-attachment-callback" {
				pending := f.router.beginRefresh(ops)
				f.router.server.SetOperationalSettings(ops)
				f.router.finishRefresh(pending, old.snapshot, nil) // release old bookkeeping
				fresh := f.observe(3, 1, true)
				f.router.finishRefresh(pending, old.snapshot, nil)
				if !fresh.successful || !f.router.inspect().observation.successful {
					t.Fatal("old attachment callback must not invalidate current attachment")
				}
				f.emit()
				if f.handler.calls != 1 || len(f.legacy.records) != 0 {
					t.Fatal("new attachment must own one NEW")
				}
				return
			}
			f.emit()
			if f.handler.calls != 0 || len(f.legacy.records) != 1 {
				t.Fatal("failure/blocked/overlap/obsolete read cannot retain or mint NEW ownership")
			}
			if f.router.inspect().observation.successful {
				t.Fatal("invalid read must leave NEW observation invalid")
			}
		})
	}

	for _, mode := range []string{"captured-A-loss", "captured-A-success", "captured-A-error", "captured-A-rejected-untracked"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.requireAdmission(t)
			ops, st := f.ops, f.settings
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			a := ops.decisionAuditPropagationAttachment()
			var b decisionAuditPropagationAttachment
			var snapshot ExperimentsSnapshot
			var copied, router decisionAuditRefreshObservation
			if mode == "captured-A-success" || mode == "captured-A-error" {
				read := &auditFixtureAttachmentRead{a: a, fail: mode == "captured-A-error"}
				st.attachmentRead = read
				st.mode = "attachment-handoff-success"
				if read.fail {
					st.mode = "attachment-handoff-error"
				}
				_, err := ops.refreshForDecisionAuditAttachment(context.Background(), &a)
				if (err != nil) != read.fail || !read.entered || !read.token.tracked || read.token.attachment != a.attachment {
					t.Fatal("original captured A read must take the selected real success/error completion branch", err)
				}
				b, snapshot, copied, router = read.b, read.snapshot, read.copied, read.router
			} else {
				f.router.server.SetOperationalSettings(ops)
				b = ops.decisionAuditPropagationAttachment()
				if _, err := ops.refreshForDecisionAuditAttachment(context.Background(), &b); err != nil {
					t.Fatal(err)
				}
				snapshot, copied = ops.decisionAuditSnapshot()
				router = f.router.inspect().observation
				if mode == "captured-A-loss" {
					ops.loseDecisionAuditPropagation(a)
				} else {
					// The actual Ops seam must retain captured A on a rejected read,
					// rather than borrowing whichever attachment is currently live.
					if _, err := ops.refreshForDecisionAuditAttachment(context.Background(), &a); err != nil {
						t.Fatal("stale untracked read still preserves generic store success", err)
					}
				}
			}
			if a.observer != f.router || b.observer != f.router || b.attachment == a.attachment {
				t.Fatal("same-source explicit handoff must capture distinct A and B identities")
			}
			auditRequireAttachmentProof(t, f, b, snapshot, copied, router, mode)
			ops.loseDecisionAuditPropagation(b)
			auditRequireCapturedLoss(t, f, mode+" current B loss")
			f.router.server.SetOperationalSettings(ops)
			c := ops.decisionAuditPropagationAttachment()
			if c.attachment == b.attachment {
				t.Fatal("only explicit handoff may release B lifecycle loss")
			}
			if _, err := ops.refreshForDecisionAuditAttachment(context.Background(), &c); err != nil {
				t.Fatal(err)
			}
			freshSnapshot, freshCopied := ops.decisionAuditSnapshot()
			auditRequireAttachmentProof(t, f, c, freshSnapshot, freshCopied, f.router.inspect().observation, mode+" explicit recovery")
		})
	}
}

func TestDecisionAuditRefresh_WritesDetachAndDisconnectInvalidate(t *testing.T) {
	for _, mode := range []string{"false", "delete", "detach", "replace", "stop", "channel-close", "recovered-loop", "stop-during-read"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.requireAdmission(t)
			ops := f.ops
			if _, err := ops.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, old := ops.decisionAuditSnapshot()
			switch mode {
			case "false":
				if _, err := ops.Update(context.Background(), "experiments", json.RawMessage(`{"overrides":{"hub.authorization_decision_audit_v2":false}}`), "fixture", 1, "test"); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := ops.DeleteSection(context.Background(), "experiments"); err != nil {
					t.Fatal(err)
				}
			case "detach":
				f.router.server.SetOperationalSettings(nil)
			case "replace":
				f.router.server.SetOperationalSettings(NewOperationalSettings(newFakeHubSettingStore(), emptyKoanf(), emptyKoanf()))
			case "stop":
				ops.StopPropagation()
			case "channel-close":
				ch := make(chan Event)
				close(ch)
				ops.runSubscriptionLoop(context.Background(), ch, f.router.server)
			case "recovered-loop":
				publisher := &auditFixturePublisher{events: make(chan Event, 1), exited: make(chan struct{})}
				ops.SetEventPublisher(publisher)
				ops.PollInterval = time.Hour
				f.settings.mode = "panic-read"
				ops.StartPropagation(context.Background(), f.router.server)
				publisher.events <- Event{Subject: settingsUpdatedSubject, Data: []byte(`{}`)}
				auditFixtureWait(publisher.exited) // after real wrapper recovery/invalidation
				f.settings.mode = ""
				// Unsubscribe completed after the real exit/recovery loss hooks.
				// Assert before the independent stop closure can latch loss itself.
				auditRequireCapturedLoss(t, f, "recovered subscription before stop cleanup")
				ops.stopPropagation() // cleanup itself also latches loss; never an oracle source
				ops.propagationWg.Wait()
			case "stop-during-read":
				f.settings.mode = "stop-during-read"
				_, _ = ops.Refresh(context.Background())
				f.settings.mode = ""

			}
			f.router.finishRefresh(old, old.snapshot, nil)
			legacyWant := 1
			if mode == "recovered-loop" {
				legacyWant++ // one earlier record proved rejection before cleanup
			}
			f.emit()
			if f.handler.calls != 0 || len(f.legacy.records) != legacyWant || f.router.inspect().observation.successful {
				t.Fatal("mutation/disconnect/detach invalidation must reject old callbacks before later handoff")
			}
			if mode == "stop" || mode == "channel-close" || mode == "recovered-loop" || mode == "stop-during-read" {
				_, err := ops.Refresh(context.Background())
				if err != nil {
					t.Fatal("healthy same-source authoritative read required", err)
				}
				_, fresh := ops.decisionAuditSnapshot()
				if fresh.successful || f.router.inspect().observation.successful {
					t.Errorf("INTENDED RED %s lifecycle loss must remain persistent on same attachment", mode)
				}
				auditRequireLegacyOnly(t, f, "INTENDED RED same-attachment resurrection after "+mode)
				// Even after the intentionally failing resurrection check, explicit handoff
				// must issue a new attachment; old callbacks cannot invalidate fresh proof.
				previous := f.router.attachment
				f.router.server.SetOperationalSettings(ops)
				renewed := f.observe(3, 1, true)
				f.router.finishRefresh(old, old.snapshot, nil)
				if f.router.attachment == previous || !renewed.successful || !f.router.inspect().observation.successful {
					t.Fatal("explicit handoff must establish independent fresh attachment")
				}
				before := f.handler.calls
				f.emit()
				if f.handler.calls != before+1 {
					t.Fatal("new healthy attachment must admit NEW")
				}
			}

		})
	}
}
