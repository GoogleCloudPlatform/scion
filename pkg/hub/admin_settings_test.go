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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestHandleAdminServerConfig_NonAdmin(t *testing.T) {
	srv := &Server{}

	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestHandleAdminServerConfig_Unauthenticated(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

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
