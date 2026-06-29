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
	"strings"
	"testing"
)

// --- mock IntegrationManager ---

type mockIntegrationManager struct {
	plugins        map[string]map[string]string // name → config
	selfManaged    map[string]bool
	healthErr      error
	infoErr        error
	configureErr   error
	reconnectErr   error
	configureCalls []string
	reconnectCalls []string
}

func newMockIntegrationManager() *mockIntegrationManager {
	return &mockIntegrationManager{
		plugins:     make(map[string]map[string]string),
		selfManaged: make(map[string]bool),
	}
}

func (m *mockIntegrationManager) ListPlugins() []string {
	keys := make([]string, 0, len(m.plugins))
	for name := range m.plugins {
		keys = append(keys, "broker:"+name)
	}
	return keys
}

func (m *mockIntegrationManager) HasPlugin(pluginType, name string) bool {
	if pluginType != "broker" {
		return false
	}
	_, ok := m.plugins[name]
	return ok
}

func (m *mockIntegrationManager) GetPluginConfig(pluginType, name string) map[string]string {
	if pluginType != "broker" {
		return nil
	}
	cfg, ok := m.plugins[name]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out
}

func (m *mockIntegrationManager) IsSelfManaged(pluginType, name string) bool {
	if pluginType != "broker" {
		return false
	}
	return m.selfManaged[name]
}

func (m *mockIntegrationManager) ConfigureBroker(name string, extra map[string]string) error {
	m.configureCalls = append(m.configureCalls, name)
	return m.configureErr
}

func (m *mockIntegrationManager) Reconnect(pluginType, name string) error {
	m.reconnectCalls = append(m.reconnectCalls, name)
	return m.reconnectErr
}

func (m *mockIntegrationManager) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	if m.healthErr != nil {
		return "", "", nil, m.healthErr
	}
	return "healthy", "all good", map[string]string{"connections": "5"}, nil
}

func (m *mockIntegrationManager) BrokerInfo(name string) (string, string, []string, error) {
	if m.infoErr != nil {
		return "", "", nil, m.infoErr
	}
	return "v0.8.2", "telegram", []string{"send", "receive"}, nil
}

// --- Auth tests ---

func TestIntegrations_Unauthenticated(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrations(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestIntegrations_NonAdmin(t *testing.T) {
	srv := &Server{}
	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrations(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestIntegrationByName_Unauthenticated(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestIntegrationByName_NonAdmin(t *testing.T) {
	srv := &Server{}
	member := NewAuthenticatedUser("u1", "member@example.com", "Member", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

// --- List endpoint ---

func TestListIntegrations_Empty(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrations(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var result []IntegrationSummary
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty list, got %d", len(result))
	}
}

func TestListIntegrations_WithPlugins(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{"webhook_listen": ":9094"}
	mgr.plugins["discord"] = map[string]string{"guild_id": "12345"}
	mgr.selfManaged["discord"] = true

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrations(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result []IntegrationSummary
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 integrations, got %d", len(result))
	}

	byName := make(map[string]IntegrationSummary)
	for _, s := range result {
		byName[s.Name] = s
	}

	tg, ok := byName["telegram"]
	if !ok {
		t.Fatal("telegram not in list")
	}
	if tg.Platform != "telegram" {
		t.Errorf("expected platform telegram, got %s", tg.Platform)
	}
	if tg.SelfManaged {
		t.Error("telegram should not be self-managed")
	}
	if tg.Status == nil || tg.Status.Version != "v0.8.2" {
		t.Error("expected status with version v0.8.2")
	}

	dc, ok := byName["discord"]
	if !ok {
		t.Fatal("discord not in list")
	}
	if !dc.SelfManaged {
		t.Error("discord should be self-managed")
	}
}

func TestListIntegrations_MethodNotAllowed(t *testing.T) {
	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrations(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// --- Detail endpoint ---

func TestGetIntegration_NotFound(t *testing.T) {
	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/nonexistent", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestGetIntegration_OK(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{
		"webhook_listen": ":9094",
		"hub_url":        "https://hub.example.com",
		"bot_token":      "should-be-filtered",
	}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var detail IntegrationDetail
	if err := json.NewDecoder(rr.Body).Decode(&detail); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if detail.Name != "telegram" {
		t.Errorf("expected name telegram, got %s", detail.Name)
	}
	if detail.Platform != "telegram" {
		t.Errorf("expected platform telegram, got %s", detail.Platform)
	}
	if _, ok := detail.Settings["bot_token"]; ok {
		t.Error("bot_token should be filtered from settings")
	}
	if _, ok := detail.Settings["hub_url"]; ok {
		t.Error("hub_url should be filtered from settings")
	}
	if detail.Settings["webhook_listen"] != ":9094" {
		t.Errorf("expected webhook_listen :9094, got %s", detail.Settings["webhook_listen"])
	}
	if detail.Status == nil || !detail.Status.Connected {
		t.Error("expected connected status")
	}
}

func TestGetIntegration_MethodNotAllowed(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/integrations/telegram", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// --- Health endpoint ---

func TestIntegrationHealth_OK(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram/health", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var status IntegrationStatus
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if status.Health != "healthy" {
		t.Errorf("expected healthy, got %s", status.Health)
	}
	if !status.Connected {
		t.Error("expected connected")
	}
	if status.Version != "v0.8.2" {
		t.Errorf("expected version v0.8.2, got %s", status.Version)
	}
}

func TestIntegrationHealth_NotFound(t *testing.T) {
	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/nonexistent/health", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// --- Restart endpoint ---

func TestRestartIntegration_OK(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/restart", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(mgr.configureCalls) != 1 || mgr.configureCalls[0] != "telegram" {
		t.Errorf("expected ConfigureBroker call for telegram, got %v", mgr.configureCalls)
	}
}

func TestRestartIntegration_NotFound(t *testing.T) {
	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/nonexistent/restart", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestRestartIntegration_MethodNotAllowed(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram/restart", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// --- Config PUT endpoint ---

func TestUpdateConfig_NoConfigFile(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"webhook_listen":":9095"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/telegram/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no config file), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateConfig_WithConfigFile(t *testing.T) {
	dir := t.TempDir()
	configFile := dir + "/telegram.yaml"

	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{
		"config_file":    configFile,
		"webhook_listen": ":9094",
	}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"webhook_listen":":9095","db_path":"/tmp/tg.db"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/telegram/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(mgr.configureCalls) != 1 {
		t.Errorf("expected 1 ConfigureBroker call, got %d", len(mgr.configureCalls))
	}
}

func TestUpdateConfig_InvalidBody(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/telegram/config", strings.NewReader("not json"))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestUpdateConfig_UnknownSecretKey(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"secrets":{"unknown_key":"value"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/telegram/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateConfig_NotFound(t *testing.T) {
	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/nonexistent/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// --- Helper unit tests ---

func TestResolvePlatform(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"telegram", "telegram"},
		{"discord", "discord"},
		{"chat-app", "gchat"},
		{"custom", "custom"},
	}
	for _, tt := range tests {
		if got := resolvePlatform(tt.name); got != tt.expected {
			t.Errorf("resolvePlatform(%q) = %q, want %q", tt.name, got, tt.expected)
		}
	}
}

func TestFilterSensitiveConfig(t *testing.T) {
	cfg := map[string]string{
		"webhook_listen": ":9094",
		"bot_token":      "secret-token",
		"hub_url":        "https://hub.example.com",
		"hmac_key":       "secret-hmac",
		"broker_id":      "br-123",
		"config_file":    "/etc/telegram.yaml",
		"db_path":        "/var/lib/tg.db",
	}

	filtered := filterSensitiveConfig("telegram", cfg)

	if _, ok := filtered["bot_token"]; ok {
		t.Error("bot_token should be filtered")
	}
	if _, ok := filtered["hub_url"]; ok {
		t.Error("hub_url should be filtered")
	}
	if _, ok := filtered["hmac_key"]; ok {
		t.Error("hmac_key should be filtered")
	}
	if _, ok := filtered["broker_id"]; ok {
		t.Error("broker_id should be filtered")
	}
	if _, ok := filtered["config_file"]; ok {
		t.Error("config_file should be filtered")
	}
	if filtered["webhook_listen"] != ":9094" {
		t.Errorf("expected webhook_listen :9094, got %s", filtered["webhook_listen"])
	}
	if filtered["db_path"] != "/var/lib/tg.db" {
		t.Errorf("expected db_path preserved, got %s", filtered["db_path"])
	}
}

func TestPluginNameFromKey(t *testing.T) {
	tests := []struct {
		key      string
		expected string
	}{
		{"broker:telegram", "telegram"},
		{"broker:discord", "discord"},
		{"other:telegram", ""},
		{"invalid", ""},
	}
	for _, tt := range tests {
		if got := pluginNameFromKey(tt.key); got != tt.expected {
			t.Errorf("pluginNameFromKey(%q) = %q, want %q", tt.key, got, tt.expected)
		}
	}
}

// --- Unknown endpoint ---

func TestIntegrationByName_UnknownAction(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram/unknown", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
