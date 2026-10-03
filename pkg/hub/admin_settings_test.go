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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// NOTE: Auth gating for handleAdminServerConfig (non-admin and unauthenticated
// rejection) is now enforced by routeGuard via Permission metadata, not by inline
// checks. See TestRouteGuardSettingsConversion in routeguard_settings_test.go.

func TestHandleAdminServerConfig_MethodNotAllowed(t *testing.T) {
	srv := &Server{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestHandleAdminServerConfig_Get(t *testing.T) {
	srv := &Server{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	// Should return 200 with at least schema_version, even if settings.yaml doesn't exist
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body ServerConfigResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body.SchemaVersion == "" {
		t.Error("expected non-empty schema_version")
	}
}

func TestMaskSensitiveFields(t *testing.T) {
	sc := serverConfigForMaskTest()
	resp := &ServerConfigResponse{
		Server: &sc,
	}

	maskSensitiveFields(resp)

	if resp.Server.Auth.DevToken != "********" {
		t.Errorf("expected masked dev token, got %s", resp.Server.Auth.DevToken)
	}
	if resp.Server.Broker.BrokerToken != "********" {
		t.Errorf("expected masked broker token, got %s", resp.Server.Broker.BrokerToken)
	}
	if resp.Server.Database.URL != "********" {
		t.Errorf("expected masked db URL, got %s", resp.Server.Database.URL)
	}
}

func TestApplySettingsUpdates_PreservesServerKeys(t *testing.T) {
	// Simulate existing settings.yaml with a github_app section
	raw := map[string]interface{}{
		"schema_version": "1",
		"server": map[string]interface{}{
			"mode":      "workstation",
			"log_level": "info",
			"github_app": map[string]interface{}{
				"app_id":           12345,
				"webhooks_enabled": true,
				"installation_url": "https://github.com/apps/my-app",
			},
		},
	}

	// Update request changes log_level but doesn't include github_app
	logLevel := "debug"
	req := &ServerConfigUpdateRequest{
		Server: &config.V1ServerConfig{
			LogLevel: logLevel,
		},
	}

	applySettingsUpdates(raw, req)

	serverMap, ok := raw["server"].(map[string]interface{})
	if !ok {
		t.Fatal("expected server to be a map")
	}

	// github_app should be preserved
	ghApp, ok := serverMap["github_app"]
	if !ok {
		t.Fatal("github_app was lost from server config after update")
	}
	ghAppMap, ok := ghApp.(map[string]interface{})
	if !ok {
		t.Fatalf("expected github_app to be a map, got %T", ghApp)
	}
	if ghAppMap["app_id"] != 12345 {
		t.Errorf("expected app_id 12345, got %v", ghAppMap["app_id"])
	}
	if ghAppMap["webhooks_enabled"] != true {
		t.Errorf("expected webhooks_enabled true, got %v", ghAppMap["webhooks_enabled"])
	}

	// Updated field should be present
	if serverMap["log_level"] != "debug" {
		t.Errorf("expected log_level debug, got %v", serverMap["log_level"])
	}
}

func TestApplySettingsUpdates_AutoExposePortsNilEnabled(t *testing.T) {
	// When AutoExposePorts is provided but Enabled is nil, the key should be
	// deleted to avoid persisting an empty auto_expose_ports: {} block.
	raw := map[string]interface{}{
		"schema_version": "1",
		"auto_expose_ports": map[string]interface{}{
			"enabled": true,
		},
	}

	req := &ServerConfigUpdateRequest{
		AutoExposePorts: &config.AutoExposePortsSettings{
			Enabled: nil, // nil signals deletion
		},
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["auto_expose_ports"]; ok {
		t.Error("expected auto_expose_ports key to be deleted when Enabled is nil")
	}
}

func TestApplySettingsUpdates_AutoExposePortsWithEnabled(t *testing.T) {
	// When AutoExposePorts is provided with a non-nil Enabled, the key should
	// be set normally.
	raw := map[string]interface{}{
		"schema_version": "1",
	}

	enabled := true
	req := &ServerConfigUpdateRequest{
		AutoExposePorts: &config.AutoExposePortsSettings{
			Enabled: &enabled,
		},
	}

	applySettingsUpdates(raw, req)

	aep, ok := raw["auto_expose_ports"]
	if !ok {
		t.Fatal("expected auto_expose_ports key to be present")
	}
	aepMap, ok := aep.(map[string]interface{})
	if !ok {
		t.Fatalf("expected auto_expose_ports to be a map, got %T", aep)
	}
	if aepMap["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", aepMap["enabled"])
	}
}

func TestApplySettingsUpdates_AutoExposePortsNilRequest(t *testing.T) {
	// When AutoExposePorts itself is nil in the request, the existing value
	// should be preserved (no change).
	raw := map[string]interface{}{
		"schema_version": "1",
		"auto_expose_ports": map[string]interface{}{
			"enabled": true,
		},
	}

	req := &ServerConfigUpdateRequest{
		AutoExposePorts: nil,
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["auto_expose_ports"]; !ok {
		t.Error("expected auto_expose_ports to be preserved when request field is nil")
	}
}

// Upstream review finding (GoogleCloudPlatform/scion#2115 follow-up,
// ptone/scion#2315): deciding whether to delete the whole auto_expose_ports
// section by checking the single named field Enabled != nil is fragile once
// AutoExposePortsSettings gains a second field — a request that sets only
// the new field, with Enabled omitted, would wrongly delete the section.
// applySettingsUpdates now uses isZeroStruct (the same helper already used
// for the quotas section) so the decision is section-generic: it looks at
// every field, not one hardcoded name. This is directly exercised by
// isZeroStruct's own tests (TestIsZeroStruct) for the current single-field
// AutoExposePortsSettings; this test locks in the equivalent behavior through
// the actual applySettingsUpdates entry point.
func TestApplySettingsUpdates_AutoExposePortsSectionGenericZeroCheck(t *testing.T) {
	// A struct with every field nil/zero must delete the section, regardless
	// of which field(s) AutoExposePortsSettings has.
	raw := map[string]interface{}{
		"schema_version":    "1",
		"auto_expose_ports": map[string]interface{}{"enabled": true},
	}
	applySettingsUpdates(raw, &ServerConfigUpdateRequest{AutoExposePorts: &config.AutoExposePortsSettings{}})
	if _, ok := raw["auto_expose_ports"]; ok {
		t.Error("expected auto_expose_ports to be deleted when every field of AutoExposePortsSettings is nil")
	}

	// Any field being set must keep the section.
	enabled := true
	raw2 := map[string]interface{}{"schema_version": "1"}
	applySettingsUpdates(raw2, &ServerConfigUpdateRequest{AutoExposePorts: &config.AutoExposePortsSettings{Enabled: &enabled}})
	if _, ok := raw2["auto_expose_ports"]; !ok {
		t.Error("expected auto_expose_ports to be kept when a field of AutoExposePortsSettings is set")
	}
}

func TestApplySettingsUpdates_QuotasNilEnforceBrokerQuotas(t *testing.T) {
	// When Quotas is provided but EnforceBrokerQuotas is nil, the key should
	// be deleted to avoid persisting an empty quotas: {} block.
	raw := map[string]interface{}{
		"schema_version": "1",
		"quotas": map[string]interface{}{
			"enforce_broker_quotas": false,
		},
	}

	req := &ServerConfigUpdateRequest{
		Quotas: &config.QuotaSettings{
			EnforceBrokerQuotas: nil, // nil signals deletion
		},
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["quotas"]; ok {
		t.Error("expected quotas key to be deleted when EnforceBrokerQuotas is nil")
	}
}

func TestApplySettingsUpdates_QuotasWithEnforceBrokerQuotas(t *testing.T) {
	// When Quotas is provided with a non-nil EnforceBrokerQuotas, the key
	// should be set normally.
	raw := map[string]interface{}{
		"schema_version": "1",
	}

	enforced := false
	req := &ServerConfigUpdateRequest{
		Quotas: &config.QuotaSettings{
			EnforceBrokerQuotas: &enforced,
		},
	}

	applySettingsUpdates(raw, req)

	q, ok := raw["quotas"]
	if !ok {
		t.Fatal("expected quotas key to be present")
	}
	qMap, ok := q.(map[string]interface{})
	if !ok {
		t.Fatalf("expected quotas to be a map, got %T", q)
	}
	if qMap["enforce_broker_quotas"] != false {
		t.Errorf("expected enforce_broker_quotas=false, got %v", qMap["enforce_broker_quotas"])
	}
}

func TestApplySettingsUpdates_QuotasNilRequest(t *testing.T) {
	// When Quotas itself is nil in the request, the existing value should be
	// preserved (no change).
	raw := map[string]interface{}{
		"schema_version": "1",
		"quotas": map[string]interface{}{
			"enforce_broker_quotas": false,
		},
	}

	req := &ServerConfigUpdateRequest{
		Quotas: nil,
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["quotas"]; !ok {
		t.Error("expected quotas to be preserved when request field is nil")
	}
}

// Upstream review finding (GoogleCloudPlatform/scion#2115, gemini-code-assist,
// pkg/hub/admin_settings.go): deciding whether to delete the whole quotas
// section by checking the single named field EnforceBrokerQuotas != nil is
// fragile once QuotaSettings gains a second field — a request that sets only
// the new field, with EnforceBrokerQuotas omitted, would wrongly delete the
// section. applySettingsUpdates now uses isZeroStruct (the same helper
// admin_settings_db.go already uses for this exact "is anything meaningfully
// set" question), so the decision is section-generic: it looks at every
// field, not one hardcoded name. This is directly exercised by isZeroStruct's
// own tests (TestIsZeroStruct) for the current single-field QuotaSettings;
// this test locks in the equivalent behavior through the actual
// applySettingsUpdates entry point.
func TestApplySettingsUpdates_QuotasSectionGenericZeroCheck(t *testing.T) {
	// A struct with every field nil/zero must delete the section, regardless
	// of which field(s) QuotaSettings has.
	raw := map[string]interface{}{
		"schema_version": "1",
		"quotas":         map[string]interface{}{"enforce_broker_quotas": true},
	}
	applySettingsUpdates(raw, &ServerConfigUpdateRequest{Quotas: &config.QuotaSettings{}})
	if _, ok := raw["quotas"]; ok {
		t.Error("expected quotas to be deleted when every field of QuotaSettings is nil")
	}

	// Any field being set must keep the section.
	enabled := true
	raw2 := map[string]interface{}{"schema_version": "1"}
	applySettingsUpdates(raw2, &ServerConfigUpdateRequest{Quotas: &config.QuotaSettings{EnforceBrokerQuotas: &enabled}})
	if _, ok := raw2["quotas"]; !ok {
		t.Error("expected quotas to be kept when a field of QuotaSettings is set")
	}
}

// TestSingleFieldSettingsStructsGuard fails when AutoExposePortsSettings,
// QuotaSettings or AgentSecretsSettings gains a field, since the section
// zero-check tests above (TestApplySettingsUpdates_AutoExposePortsSectionGenericZeroCheck,
// TestApplySettingsUpdates_QuotasSectionGenericZeroCheck and
// TestApplySettingsUpdates_AgentSecretsSectionGenericZeroCheck) only ever
// exercise the current single field of each struct: a new field would go
// unverified by those "any field set" cases.
func TestSingleFieldSettingsStructsGuard(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"config.AutoExposePortsSettings", reflect.TypeOf(config.AutoExposePortsSettings{})},
		{"config.QuotaSettings", reflect.TypeOf(config.QuotaSettings{})},
		{"config.AgentSecretsSettings", reflect.TypeOf(config.AgentSecretsSettings{})},
	} {
		if n := tc.typ.NumField(); n != 1 {
			t.Errorf("%s has %d fields, want 1: add a case that sets only the new field to the section zero-check tests, then update the expected field count in TestSingleFieldSettingsStructsGuard", tc.name, n)
		}
	}
}

// TestApplySettingsUpdates_AgentSecretsNilUserScopeOnly mirrors
// TestApplySettingsUpdates_QuotasNilEnforceBrokerQuotas: when AgentSecrets is
// provided but UserScopeOnly is nil, the key should be deleted to avoid
// persisting an empty agent_secrets: {} block.
func TestApplySettingsUpdates_AgentSecretsNilUserScopeOnly(t *testing.T) {
	raw := map[string]interface{}{
		"schema_version": "1",
		"agent_secrets": map[string]interface{}{
			"user_scope_only": true,
		},
	}

	req := &ServerConfigUpdateRequest{
		AgentSecrets: &config.AgentSecretsSettings{
			UserScopeOnly: nil, // nil signals deletion
		},
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["agent_secrets"]; ok {
		t.Error("expected agent_secrets key to be deleted when UserScopeOnly is nil")
	}
}

// TestApplySettingsUpdates_AgentSecretsWithUserScopeOnly mirrors
// TestApplySettingsUpdates_QuotasWithEnforceBrokerQuotas.
func TestApplySettingsUpdates_AgentSecretsWithUserScopeOnly(t *testing.T) {
	raw := map[string]interface{}{
		"schema_version": "1",
	}

	on := true
	req := &ServerConfigUpdateRequest{
		AgentSecrets: &config.AgentSecretsSettings{
			UserScopeOnly: &on,
		},
	}

	applySettingsUpdates(raw, req)

	as, ok := raw["agent_secrets"]
	if !ok {
		t.Fatal("expected agent_secrets key to be present")
	}
	asMap, ok := as.(map[string]interface{})
	if !ok {
		t.Fatalf("expected agent_secrets to be a map, got %T", as)
	}
	if asMap["user_scope_only"] != true {
		t.Errorf("expected user_scope_only=true, got %v", asMap["user_scope_only"])
	}
}

// TestApplySettingsUpdates_AgentSecretsNilRequest mirrors
// TestApplySettingsUpdates_QuotasNilRequest.
func TestApplySettingsUpdates_AgentSecretsNilRequest(t *testing.T) {
	raw := map[string]interface{}{
		"schema_version": "1",
		"agent_secrets": map[string]interface{}{
			"user_scope_only": true,
		},
	}

	req := &ServerConfigUpdateRequest{
		AgentSecrets: nil,
	}

	applySettingsUpdates(raw, req)

	if _, ok := raw["agent_secrets"]; !ok {
		t.Error("expected agent_secrets to be preserved when request field is nil")
	}
}

// TestApplySettingsUpdates_AgentSecretsSectionGenericZeroCheck mirrors
// TestApplySettingsUpdates_QuotasSectionGenericZeroCheck: the decision to
// delete the section must look at every field (isZeroStruct), not one
// hardcoded name, so it stays correct if AgentSecretsSettings ever gains a
// second field.
func TestApplySettingsUpdates_AgentSecretsSectionGenericZeroCheck(t *testing.T) {
	raw := map[string]interface{}{
		"schema_version": "1",
		"agent_secrets":  map[string]interface{}{"user_scope_only": true},
	}
	applySettingsUpdates(raw, &ServerConfigUpdateRequest{AgentSecrets: &config.AgentSecretsSettings{}})
	if _, ok := raw["agent_secrets"]; ok {
		t.Error("expected agent_secrets to be deleted when every field of AgentSecretsSettings is nil")
	}

	on := true
	raw2 := map[string]interface{}{"schema_version": "1"}
	applySettingsUpdates(raw2, &ServerConfigUpdateRequest{AgentSecrets: &config.AgentSecretsSettings{UserScopeOnly: &on}})
	if _, ok := raw2["agent_secrets"]; !ok {
		t.Error("expected agent_secrets to be kept when a field of AgentSecretsSettings is set")
	}
}

// TestHandlePutServerConfig_AgentSecretsUserScopeOnly_PersistedAndAppliedWithoutRestart
// mirrors TestHandlePutServerConfig_EnforceBrokerQuotas_PersistedAndAppliedWithoutRestart:
// a file-mode admin PUT writes settings.yaml and applies the new value live,
// with no restart (design ptone/scion#2291 §5.2).
func TestHandlePutServerConfig_AgentSecretsUserScopeOnly_PersistedAndAppliedWithoutRestart(t *testing.T) {
	srv := &Server{}
	if srv.agentSecretsUserScopeOnly() {
		t.Fatal("expected agentSecretsUserScopeOnly()=false before any PUT (permissive default)")
	}

	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"server":{"hub":{"port":9810}},"agent_secrets":{"user_scope_only":true}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	agentSecrets, _ := raw["agent_secrets"].(map[string]interface{})
	if got, ok := agentSecrets["user_scope_only"].(bool); !ok || got != true {
		t.Errorf("persisted agent_secrets.user_scope_only = %v, want true (settings.yaml: %s)", agentSecrets["user_scope_only"], data)
	}

	// No restart: the in-memory config must already reflect the new value.
	if !srv.agentSecretsUserScopeOnly() {
		t.Error("expected agentSecretsUserScopeOnly()=true immediately after PUT, without a restart")
	}

	// GET must reflect the persisted value too (design §10 test 3: "GET
	// includes agent_secrets").
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfig(getRR)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var getResp ServerConfigResponse
	if err := json.NewDecoder(getRR.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	if getResp.AgentSecrets == nil || getResp.AgentSecrets.UserScopeOnly == nil || !*getResp.AgentSecrets.UserScopeOnly {
		t.Errorf("GET response AgentSecrets = %+v, want UserScopeOnly=true", getResp.AgentSecrets)
	}
}

// TestApplySettingsUpdates_ClearFieldsToBlank is a regression test for
// ptone/scion#860: clearing a field to blank in the admin UI should delete
// the key from settings.yaml, not preserve the old value.
//
// Round-trip: set value → save → clear to blank → save → confirm deleted.
func TestApplySettingsUpdates_ClearFieldsToBlank(t *testing.T) {
	// Step 1: Start with existing settings containing the fields we'll clear.
	raw := map[string]interface{}{
		"schema_version":          "1",
		"default_max_duration":    "2h",
		"default_max_turns":       200,
		"default_max_model_calls": 500,
		"default_model":           "gemini-2.0-flash",
		"default_thinking_level":  3,
	}

	// Verify they're present.
	for _, key := range []string{
		"default_max_duration", "default_max_turns",
		"default_max_model_calls", "default_model", "default_thinking_level",
	} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("precondition: expected %q to be present", key)
		}
	}

	// Step 2: Send an update with empty/zero values to clear each field.
	emptyStr := ""
	zeroInt := 0
	req := &ServerConfigUpdateRequest{
		DefaultMaxDuration:   &emptyStr,
		DefaultMaxTurns:      &zeroInt,
		DefaultMaxModelCalls: &zeroInt,
		DefaultModel:         &emptyStr,
		DefaultThinkingLevel: &zeroInt,
	}

	applySettingsUpdates(raw, req)

	// Step 3: Verify all fields are deleted from the raw map.
	for _, key := range []string{
		"default_max_duration", "default_max_turns",
		"default_max_model_calls", "default_model", "default_thinking_level",
	} {
		if _, ok := raw[key]; ok {
			t.Errorf("expected %q to be deleted after clearing to blank, but it still exists with value %v", key, raw[key])
		}
	}

	// schema_version should still be present (not affected).
	if _, ok := raw["schema_version"]; !ok {
		t.Error("schema_version should be preserved")
	}
}

// TestApplySettingsUpdates_OmittedFieldsPreserved verifies that when a field
// is NOT included in the update request (nil pointer), the existing value is
// preserved. This is the complement of TestApplySettingsUpdates_ClearFieldsToBlank.
func TestApplySettingsUpdates_OmittedFieldsPreserved(t *testing.T) {
	raw := map[string]interface{}{
		"schema_version":       "1",
		"default_max_duration": "2h",
		"default_max_turns":    200,
		"default_model":        "gemini-2.0-flash",
	}

	// Request with nil pointers — fields are omitted, not cleared.
	req := &ServerConfigUpdateRequest{
		DefaultMaxDuration: nil,
		DefaultMaxTurns:    nil,
		DefaultModel:       nil,
	}

	applySettingsUpdates(raw, req)

	// All fields should be preserved.
	if raw["default_max_duration"] != "2h" {
		t.Errorf("expected default_max_duration to be preserved as '2h', got %v", raw["default_max_duration"])
	}
	if raw["default_max_turns"] != 200 {
		t.Errorf("expected default_max_turns to be preserved as 200, got %v", raw["default_max_turns"])
	}
	if raw["default_model"] != "gemini-2.0-flash" {
		t.Errorf("expected default_model to be preserved as 'gemini-2.0-flash', got %v", raw["default_model"])
	}
}

func serverConfigForMaskTest() config.V1ServerConfig {
	return config.V1ServerConfig{
		Auth: &config.V1AuthConfig{
			DevToken: "secret-token-123",
		},
		Broker: &config.V1BrokerConfig{
			BrokerToken: "broker-secret-456",
		},
		Database: &config.V1DatabaseConfig{
			Driver: "sqlite",
			URL:    "/path/to/db",
		},
	}
}

// fileModePutServerConfig issues a file-mode PUT with HOME pointed at a temp
// dir and returns the recorder and the settings.yaml path.
func fileModePutServerConfig(t *testing.T, srv *Server, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr, filepath.Join(tmpHome, ".scion", "settings.yaml")
}

// Design D6: file-mode PUT rejects default_user_role values outside the
// schema enum with 400 and writes nothing.
func TestHandlePutServerConfig_DefaultUserRole_InvalidRejected(t *testing.T) {
	for _, v := range []string{"superuser", "admin", "Viewer"} {
		t.Run(v, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv,
				`{"server":{"auth":{"default_user_role":"`+v+`"}}}`)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "default_user_role") {
				t.Errorf("400 body should name default_user_role, got: %s", rr.Body.String())
			}
			if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("nothing should be persisted for an invalid value, got settings.yaml: %s", data)
			}
		})
	}
}

// File-mode PUT of a valid default_user_role persists it to settings.yaml and
// applies it live via reloadSettings.
func TestHandlePutServerConfig_DefaultUserRole_ViewerPersistedAndApplied(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"server":{"auth":{"default_user_role":"viewer"}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	server, _ := raw["server"].(map[string]interface{})
	auth, _ := server["auth"].(map[string]interface{})
	if got, _ := auth["default_user_role"].(string); got != "viewer" {
		t.Errorf("persisted default_user_role = %q, want viewer (settings.yaml: %s)", got, data)
	}
	if got := srv.DefaultUserRole(); got != "viewer" {
		t.Errorf("live DefaultUserRole() = %q after reload, want viewer", got)
	}
}

// Test 8 (design 4.7 P1b): file-mode PUT of quotas.enforce_broker_quotas
// persists it to settings.yaml and applies it live via reloadSettings,
// without a restart.
func TestHandlePutServerConfig_EnforceBrokerQuotas_PersistedAndAppliedWithoutRestart(t *testing.T) {
	srv := &Server{}
	if !srv.brokerQuotasEnforced() {
		t.Fatal("expected brokerQuotasEnforced()=true before any PUT (fail-safe default)")
	}

	// A real settings.yaml always has a "server" key (hub port, etc.) by the
	// time an admin edits Layer-1 settings; include one here so the file-mode
	// reload path (loadServerFromSettingsFile) recognizes the file as
	// versioned settings, exactly like a deployed hub's settings.yaml would.
	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"server":{"hub":{"port":9810}},"quotas":{"enforce_broker_quotas":false}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	quotas, _ := raw["quotas"].(map[string]interface{})
	if got, ok := quotas["enforce_broker_quotas"].(bool); !ok || got != false {
		t.Errorf("persisted quotas.enforce_broker_quotas = %v, want false (settings.yaml: %s)", quotas["enforce_broker_quotas"], data)
	}

	// No restart: the in-memory config must already reflect the new value.
	if srv.brokerQuotasEnforced() {
		t.Error("expected brokerQuotasEnforced()=false immediately after PUT, without a restart")
	}
}

// ---- default_timezone, file mode ----
//
// Before this fix, a file-mode default_timezone PUT persisted to
// settings.yaml and returned 200, but GlobalConfig had no DefaultTimezone
// field and BuildLayer1SnapshotFromFile never set
// Layer1Snapshot.DefaultTimezone, so hubAgentDefaults().DefaultTimezone (what
// agent create actually reads) stayed "" forever — the control did nothing.
// See TestHTTPAgentDispatcher_TZInjection_HubDefault_FileMode in
// httpdispatcher_test.go for the dispatch-level proof that the fix reaches
// DispatchAgentCreate's ResolvedEnv["TZ"].

// TestHandlePutServerConfig_DefaultTimezone_PersistedAndAppliedWithoutRestart
// mirrors TestHandlePutServerConfig_EnforceBrokerQuotas_PersistedAndAppliedWithoutRestart
// for default_timezone: a file-mode admin PUT writes settings.yaml and
// applies the new value live, with no restart.
func TestHandlePutServerConfig_DefaultTimezone_PersistedAndAppliedWithoutRestart(t *testing.T) {
	srv := &Server{}
	if got := srv.hubAgentDefaults().DefaultTimezone; got != "" {
		t.Fatalf("expected hubAgentDefaults().DefaultTimezone=\"\" before any PUT, got %q", got)
	}

	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"server":{"hub":{"port":9810}},"default_timezone":"Asia/Tokyo"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	if got, _ := raw["default_timezone"].(string); got != "Asia/Tokyo" {
		t.Errorf("persisted default_timezone = %q, want Asia/Tokyo (settings.yaml: %s)", raw["default_timezone"], data)
	}

	// No restart: the in-memory config must already reflect the new value,
	// through the same accessor agent create reads (hubAgentDefaultsProvider).
	if got := srv.hubAgentDefaults().DefaultTimezone; got != "Asia/Tokyo" {
		t.Errorf("hubAgentDefaults().DefaultTimezone = %q immediately after PUT, want Asia/Tokyo (no restart)", got)
	}
}

// TestHandlePutServerConfig_DefaultTimezone_ClearedPersistsEmpty is the
// round-trip complement: clearing a previously-set file-mode
// default_timezone must remove it from settings.yaml and from
// hubAgentDefaults(), not just leave the old value live. Both PUTs land in
// the same settings.yaml (HOME is set once, directly — not via
// fileModePutServerConfig, which repoints HOME at a fresh temp dir on every
// call).
func TestHandlePutServerConfig_DefaultTimezone_ClearedPersistsEmpty(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")

	srv := &Server{}
	put := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}

	if rr := put(`{"server":{"hub":{"port":9810}},"default_timezone":"Asia/Tokyo"}`); rr.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := srv.hubAgentDefaults().DefaultTimezone; got != "Asia/Tokyo" {
		t.Fatalf("precondition: hubAgentDefaults().DefaultTimezone = %q, want Asia/Tokyo", got)
	}

	if rr := put(`{"server":{"hub":{"port":9810}},"default_timezone":""}`); rr.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	if v, ok := raw["default_timezone"]; ok {
		t.Errorf("expected default_timezone to be deleted from settings.yaml after clearing, got %v", v)
	}
	if got := srv.hubAgentDefaults().DefaultTimezone; got != "" {
		t.Errorf("hubAgentDefaults().DefaultTimezone = %q after clearing, want \"\" (no restart)", got)
	}
}

// TestHandlePutServerConfig_DefaultTimezone_InvalidRejected is the file-mode
// complement of the DB-mode validation: handlePutServerConfig must apply
// the same IANA-name check for default_timezone as the DB-mode handler, so
// an invalid name — or one of the tzdata names LoadLocation accepts but
// that do not name a portable zone (Local, localtime, posixrules, Factory)
// — is rejected rather than written to settings.yaml silently.
func TestHandlePutServerConfig_DefaultTimezone_InvalidRejected(t *testing.T) {
	for _, tz := range []string{"Not/A/Timezone", "Local", "localtime", "posixrules", "Factory"} {
		t.Run(tz, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv,
				`{"server":{"hub":{"port":9810}},"default_timezone":"`+tz+`"}`)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "default_timezone") || !strings.Contains(rr.Body.String(), tz) {
				t.Errorf("422 body should name default_timezone and the invalid value %q, got: %s", tz, rr.Body.String())
			}
			if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("nothing should be persisted for an invalid value, got settings.yaml: %s", data)
			}
			if got := srv.hubAgentDefaults().DefaultTimezone; got != "" {
				t.Errorf("hubAgentDefaults().DefaultTimezone = %q after a rejected PUT, want \"\"", got)
			}
		})
	}
}

// TestHandlePutServerConfig_DefaultTimezone_ValidPersisted is the file-mode
// complement of TestPutServerConfigDB_DefaultTimezone_Valid, extended with
// the aliases the picker's alias handling cares about (both spellings of
// Kathmandu/Katmandu are real IANA names, and the server stores whichever
// string the client sent verbatim, with no canonicalization).
func TestHandlePutServerConfig_DefaultTimezone_ValidPersisted(t *testing.T) {
	for _, tz := range []string{"Europe/Berlin", "Asia/Kathmandu", "Asia/Katmandu", "UTC"} {
		t.Run(tz, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv,
				`{"server":{"hub":{"port":9810}},"default_timezone":"`+tz+`"}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			data, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatalf("settings.yaml not written: %v", err)
			}
			var raw map[string]interface{}
			if err := yamlv3.Unmarshal(data, &raw); err != nil {
				t.Fatalf("parse settings.yaml: %v", err)
			}
			if got, _ := raw["default_timezone"].(string); got != tz {
				t.Errorf("persisted default_timezone = %q, want %q (settings.yaml: %s)", raw["default_timezone"], tz, data)
			}
			if got := srv.hubAgentDefaults().DefaultTimezone; got != tz {
				t.Errorf("hubAgentDefaults().DefaultTimezone = %q, want %q (no restart)", got, tz)
			}
		})
	}
}

// TestApplySettingsUpdates_ClearTopLevelStrings covers ptone/scion#2535: an
// explicit "" for a top-level string setting must delete the key from
// settings.yaml (not persist an empty string), and a nil pointer must leave
// the stored value unchanged.
func TestApplySettingsUpdates_ClearTopLevelStrings(t *testing.T) {
	keys := []string{
		"active_profile",
		"default_template",
		"default_harness_config",
		"image_registry",
		"workspace_path",
		"default_max_agent_role",
		"default_agent_role",
		"default_runtime_broker",
	}
	newRaw := func() map[string]interface{} {
		raw := map[string]interface{}{"schema_version": "1"}
		for _, k := range keys {
			raw[k] = "old-" + k
		}
		return raw
	}
	empty := func() *string { s := ""; return &s }

	t.Run("empty string deletes", func(t *testing.T) {
		raw := newRaw()
		applySettingsUpdates(raw, &ServerConfigUpdateRequest{
			ActiveProfile:        empty(),
			DefaultTemplate:      empty(),
			DefaultHarnessConfig: empty(),
			ImageRegistry:        empty(),
			WorkspacePath:        empty(),
			DefaultMaxAgentRole:  empty(),
			DefaultAgentRole:     empty(),
			DefaultRuntimeBroker: empty(),
		})
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				t.Errorf("expected %s to be deleted, got %q", k, v)
			}
		}
	})

	t.Run("nil leaves unchanged", func(t *testing.T) {
		raw := newRaw()
		applySettingsUpdates(raw, &ServerConfigUpdateRequest{})
		for _, k := range keys {
			if raw[k] != "old-"+k {
				t.Errorf("expected %s unchanged, got %v", k, raw[k])
			}
		}
	})

	t.Run("non-empty sets", func(t *testing.T) {
		raw := newRaw()
		v := "new"
		applySettingsUpdates(raw, &ServerConfigUpdateRequest{ActiveProfile: &v, WorkspacePath: &v})
		if raw["active_profile"] != "new" || raw["workspace_path"] != "new" {
			t.Errorf("expected values set, got %v / %v", raw["active_profile"], raw["workspace_path"])
		}
	})
}

// TestHandlePutServerConfig_ClearTopLevelStrings_RoundTrip is the
// handler-level complement of TestApplySettingsUpdates_ClearTopLevelStrings
// (ptone/scion#2535): a file-mode PUT that sends "" for each top-level
// string setting must remove those keys from the written settings.yaml,
// while a key the request omits is left as it was.
func TestHandlePutServerConfig_ClearTopLevelStrings_RoundTrip(t *testing.T) {
	keys := []string{
		"active_profile",
		"default_template",
		"default_harness_config",
		"image_registry",
		"workspace_path",
		"default_max_agent_role",
		"default_agent_role",
		"default_runtime_broker",
	}

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(scionDir, "settings.yaml")

	seed := map[string]interface{}{
		"schema_version": "1",
		"default_model":  "keep-model",
	}
	for _, k := range keys {
		seed[k] = "old-" + k
	}
	seedData, err := yamlv3.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, seedData, 0600); err != nil {
		t.Fatal(err)
	}

	body := map[string]string{}
	for _, k := range keys {
		body[k] = ""
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(bodyJSON)))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not readable: %v", err)
	}
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse settings.yaml: %v", err)
	}
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			t.Errorf("expected %s to be absent from settings.yaml after clearing, got %v", k, v)
		}
	}
	if got, _ := raw["default_model"].(string); got != "keep-model" {
		t.Errorf("default_model was not in the request and should be unchanged, got %v (settings.yaml: %s)", raw["default_model"], data)
	}
}

// File-mode PUT rejects a shared_dir_size that is not a Kubernetes quantity,
// naming the key, and writes nothing.
func TestHandlePutServerConfig_SharedDirSize_InvalidRejected(t *testing.T) {
	for body, key := range map[string]string{
		`{"runtimes":{"gke":{"type":"kubernetes","shared_dir_size":"1TB"}}}`: "runtimes.gke.shared_dir_size",
		`{"profiles":{"big":{"runtime":"gke","shared_dir_size":"lots"}}}`:    "profiles.big.shared_dir_size",
	} {
		t.Run(key, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), key) {
				t.Errorf("400 body should name %s, got: %s", key, rr.Body.String())
			}
			if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("nothing should be persisted for an invalid value, got settings.yaml: %s", data)
			}
		})
	}
}

// File-mode PUT accepts a valid shared_dir_size and persists it.
func TestHandlePutServerConfig_SharedDirSize_ValidPersisted(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv,
		`{"runtimes":{"gke":{"type":"kubernetes","shared_dir_size":"1Ti"}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shared_dir_size: 1Ti") {
		t.Errorf("settings.yaml should carry the size, got: %s", data)
	}
}
