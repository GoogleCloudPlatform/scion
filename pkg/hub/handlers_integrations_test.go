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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/integrationupdate"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
)

// --- mock IntegrationManager ---

type mockIntegrationManager struct {
	plugins            map[string]map[string]string // name → config
	selfManaged        map[string]bool
	deploymentModes    map[string]plugin.DeploymentMode
	healthErr          error
	infoErr            error
	configureErr       error
	replaceConfigErr   error
	reconnectErr       error
	updateErr          error
	installErr         error
	configureCalls     []string
	replaceConfigCalls []string
	reconnectCalls     []string
	updateCalls        []string
	installCalls       []string
}

func newMockIntegrationManager() *mockIntegrationManager {
	return &mockIntegrationManager{
		plugins:         make(map[string]map[string]string),
		selfManaged:     make(map[string]bool),
		deploymentModes: make(map[string]plugin.DeploymentMode),
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

func (m *mockIntegrationManager) GetDeploymentMode(pluginType, name string) plugin.DeploymentMode {
	if pluginType != "broker" {
		return plugin.DeploymentModePlugin
	}
	if mode, ok := m.deploymentModes[name]; ok {
		return mode
	}
	if m.selfManaged[name] {
		return plugin.DeploymentModeExternal
	}
	return plugin.DeploymentModePlugin
}

func (m *mockIntegrationManager) ConfigureBroker(name string, extra map[string]string) error {
	m.configureCalls = append(m.configureCalls, name)
	return m.configureErr
}

func (m *mockIntegrationManager) ReplaceBrokerConfig(name string, cfg map[string]string) error {
	m.replaceConfigCalls = append(m.replaceConfigCalls, name)
	return m.replaceConfigErr
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

func (m *mockIntegrationManager) UpdatePlugin(name string, repoPath string) error {
	m.updateCalls = append(m.updateCalls, name)
	return m.updateErr
}

func (m *mockIntegrationManager) InstallPlugin(name, repoPath, pluginsDir, configFile string) error {
	m.installCalls = append(m.installCalls, name)
	if m.installErr != nil {
		return m.installErr
	}
	m.plugins[name] = map[string]string{}
	if configFile != "" {
		m.plugins[name]["config_file"] = configFile
	}
	return nil
}

func (m *mockIntegrationManager) GetGRPCBrokerAdapter(name string) plugin.GRPCBrokerClient {
	return nil
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

	if len(mgr.replaceConfigCalls) != 1 || mgr.replaceConfigCalls[0] != "telegram" {
		t.Errorf("expected ReplaceBrokerConfig call for telegram, got %v", mgr.replaceConfigCalls)
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

	if len(mgr.replaceConfigCalls) != 1 {
		t.Errorf("expected 1 ReplaceBrokerConfig call, got %d", len(mgr.replaceConfigCalls))
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
		{"slack", "slack"},
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

func TestFilterSensitiveConfig_Slack(t *testing.T) {
	cfg := map[string]string{
		"socket_mode":     "true",
		"listen_address":  ":3000",
		"db_path":         "~/.scion/scion-slack.db",
		"agent_cache_ttl": "5m",
		"bot_token":       "xoxb-secret",
		"app_token":       "xapp-secret",
		"signing_secret":  "secret-signing",
		"hub_url":         "https://hub.example.com",
		"config_file":     "/etc/slack.yaml",
	}

	filtered := filterSensitiveConfig("slack", cfg)

	if _, ok := filtered["bot_token"]; ok {
		t.Error("bot_token should be filtered")
	}
	if _, ok := filtered["app_token"]; ok {
		t.Error("app_token should be filtered")
	}
	if _, ok := filtered["signing_secret"]; ok {
		t.Error("signing_secret should be filtered")
	}
	if _, ok := filtered["hub_url"]; ok {
		t.Error("hub_url should be filtered")
	}
	if _, ok := filtered["config_file"]; ok {
		t.Error("config_file should be filtered")
	}
	if filtered["socket_mode"] != "true" {
		t.Errorf("expected socket_mode true, got %s", filtered["socket_mode"])
	}
	if filtered["listen_address"] != ":3000" {
		t.Errorf("expected listen_address :3000, got %s", filtered["listen_address"])
	}
	if filtered["db_path"] != "~/.scion/scion-slack.db" {
		t.Errorf("expected db_path preserved, got %s", filtered["db_path"])
	}
	if filtered["agent_cache_ttl"] != "5m" {
		t.Errorf("expected agent_cache_ttl 5m, got %s", filtered["agent_cache_ttl"])
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

// --- Update endpoint ---

func TestUpdateIntegration_SelfManaged_SQLite(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	mgr.deploymentModes["telegram"] = plugin.DeploymentModeHA

	srv := &Server{}
	srv.pluginManager = mgr
	// dbDriver is empty → requirePostgres returns 409

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/update", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for self-managed on SQLite, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateIntegration_NotFound(t *testing.T) {
	mgr := newMockIntegrationManager()
	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/nonexistent/update", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestUpdateIntegration_NoRepoPath(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/update", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (no repo path), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateIntegration_BuildError(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	mgr.updateErr = fmt.Errorf("go build failed: exit status 1")

	srv := &Server{}
	srv.config.MaintenanceConfig.RepoPath = "/some/repo"
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/update", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}
	// Error body should NOT contain raw error details
	if strings.Contains(rr.Body.String(), "go build failed") {
		t.Error("response should not leak internal error details")
	}
}

// --- Install endpoint ---

func TestInstallIntegration_NilPluginManager(t *testing.T) {
	srv := &Server{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/install", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for nil plugin manager, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestInstallIntegration_AlreadyInstalled(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/telegram/install", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for already-installed, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestInstallIntegration_UnknownPlugin(t *testing.T) {
	mgr := newMockIntegrationManager()

	srv := &Server{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/evil-plugin/install", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown plugin, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- Available integrations endpoint ---

func TestListAvailableIntegrations_NoRepoPath(t *testing.T) {
	srv := &Server{}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/available", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result []AvailableIntegration
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty list, got %d", len(result))
	}
}

func TestListAvailableIntegrations_WithSource(t *testing.T) {
	repoDir := t.TempDir()
	// Create source directories for telegram (available) but not discord
	if err := os.MkdirAll(filepath.Join(repoDir, "extras", "scion-telegram"), 0755); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()
	// telegram is NOT installed, discord is NOT installed either
	// but only telegram has a source dir

	srv := &Server{}
	srv.config.MaintenanceConfig.RepoPath = repoDir
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/available", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result []AvailableIntegration
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 available, got %d", len(result))
	}
	if result[0].Name != "telegram" {
		t.Errorf("expected telegram, got %s", result[0].Name)
	}
}

func TestListAvailableIntegrations_IncludesSlack(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "extras", "scion-slack"), 0755); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()

	srv := &Server{}
	srv.config.MaintenanceConfig.RepoPath = repoDir
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/available", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result []AvailableIntegration
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	found := false
	for _, a := range result {
		if a.Name == "slack" && a.Platform == "slack" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected slack in available integrations, got %v", result)
	}
}

func TestListAvailableIntegrations_ExcludesInstalled(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "extras", "scion-telegram"), 0755); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{} // already installed

	srv := &Server{}
	srv.config.MaintenanceConfig.RepoPath = repoDir
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/available", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result []AvailableIntegration
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 available (already installed), got %d", len(result))
	}
}

// --- Mode 3 (HA) integration tests ---

func TestRequirePostgres_SQLite(t *testing.T) {
	srv := &Server{}
	// dbDriver is empty — SQLite or unconfigured
	rr := httptest.NewRecorder()
	ok := srv.requirePostgres(rr)

	if ok {
		t.Fatal("requirePostgres should return false for non-postgres driver")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
}

func TestRequirePostgres_Postgres(t *testing.T) {
	srv := &Server{dbDriver: "postgres"}
	rr := httptest.NewRecorder()
	ok := srv.requirePostgres(rr)

	if !ok {
		t.Fatal("requirePostgres should return true for postgres driver")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (default), got %d", rr.Code)
	}
}

func TestUpdateIntegration_HA_Accepted(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/discord/update", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	updateID := resp["update_id"]
	if updateID == "" {
		t.Fatal("expected update_id in response")
	}

	uid, err := uuid.Parse(updateID)
	if err != nil {
		t.Fatalf("update_id is not a valid UUID: %v", err)
	}

	row, err := client.IntegrationUpdate.Get(req.Context(), uid)
	if err != nil {
		t.Fatalf("failed to query created update row: %v", err)
	}
	if row.Integration != "discord" {
		t.Errorf("expected integration discord, got %s", row.Integration)
	}
	if string(row.State) != "requested" {
		t.Errorf("expected state requested, got %s", row.State)
	}
	if row.RequestedBy != "u1" {
		t.Errorf("expected requested_by u1, got %s", row.RequestedBy)
	}
}

func TestGetUpdateStatus_ByID(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// First create an update via the HA flow
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/discord/update", nil)
	createReq = createReq.WithContext(contextWithIdentity(createReq.Context(), admin))
	createRR := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(createRR, createReq)

	if createRR.Code != http.StatusAccepted {
		t.Fatalf("create: expected 202, got %d: %s", createRR.Code, createRR.Body.String())
	}

	var createResp map[string]string
	if err := json.NewDecoder(createRR.Body).Decode(&createResp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	updateID := createResp["update_id"]

	// Now GET the update status by ID
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord/update/"+updateID, nil)
	getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), admin))
	getRR := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}

	var resp IntegrationUpdateResponse
	if err := json.NewDecoder(getRR.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != updateID {
		t.Errorf("expected id %s, got %s", updateID, resp.ID)
	}
	if resp.Integration != "discord" {
		t.Errorf("expected integration discord, got %s", resp.Integration)
	}
	if resp.State != "requested" {
		t.Errorf("expected state requested, got %s", resp.State)
	}
}

func TestGetUpdateStatus_Latest(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// Create first update, then mark it completed so the 409 guard allows a second.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/discord/update", nil)
	req1 = req1.WithContext(contextWithIdentity(req1.Context(), admin))
	rr1 := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr1, req1)
	if rr1.Code != http.StatusAccepted {
		t.Fatalf("create 0: expected 202, got %d: %s", rr1.Code, rr1.Body.String())
	}
	var resp1 map[string]string
	if err := json.NewDecoder(rr1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	firstID, _ := uuid.Parse(resp1["update_id"])
	client.IntegrationUpdate.UpdateOneID(firstID).
		SetState(integrationupdate.StateCompleted).
		SaveX(context.Background())

	// Create second update.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/discord/update", nil)
	req2 = req2.WithContext(contextWithIdentity(req2.Context(), admin))
	rr2 := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr2, req2)
	if rr2.Code != http.StatusAccepted {
		t.Fatalf("create 1: expected 202, got %d: %s", rr2.Code, rr2.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord/update/latest", nil)
	getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), admin))
	getRR := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}

	var resp IntegrationUpdateResponse
	if err := json.NewDecoder(getRR.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Integration != "discord" {
		t.Errorf("expected integration discord, got %s", resp.Integration)
	}
	if resp.ID == "" {
		t.Error("expected a non-empty update ID")
	}
}

func TestGetUpdateStatus_NotFound(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	srv := &Server{dbDriver: "postgres"}
	srv.entClient = client

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord/update/"+uuid.New().String(), nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetUpdateStatus_InvalidID(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	srv := &Server{dbDriver: "postgres"}
	srv.entClient = client

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord/update/not-a-uuid", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetUpdateStatus_SQLiteReturns409(t *testing.T) {
	srv := &Server{}
	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord/update/latest", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for update status on SQLite, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateConfig_HA_Integration(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"guild_id":"12345","application_id":"67890"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/discord/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify config was persisted to integration_configs table
	rows, err := client.IntegrationConfig.Query().All(req.Context())
	if err != nil {
		t.Fatalf("query integration configs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 config row, got %d", len(rows))
	}
	if rows[0].Integration != "discord" {
		t.Errorf("expected integration discord, got %s", rows[0].Integration)
	}
}

func TestUpdateConfig_NonHA_NeedsConfigFile(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	// selfManaged is false → non-HA path

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"webhook_listen":":9095"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/telegram/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no config file for non-HA), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestIsHAIntegration(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{}

	if !srv.isHAIntegration(mgr, "discord") {
		t.Error("expected discord (HA mode) to be HA")
	}
	if srv.isHAIntegration(mgr, "telegram") {
		t.Error("expected telegram (no mode set) to not be HA")
	}
}

func TestGetUpdateStatus_CrossIntegrationRejected(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.plugins["telegram"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA
	mgr.deploymentModes["telegram"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	// Create an update for discord
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/discord/update", nil)
	createReq = createReq.WithContext(contextWithIdentity(createReq.Context(), admin))
	createRR := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(createRR, createReq)

	if createRR.Code != http.StatusAccepted {
		t.Fatalf("create: expected 202, got %d: %s", createRR.Code, createRR.Body.String())
	}

	var createResp map[string]string
	if err := json.NewDecoder(createRR.Body).Decode(&createResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	discordUpdateID := createResp["update_id"]

	// Try to GET that discord update via the telegram endpoint — should 404
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/telegram/update/"+discordUpdateID, nil)
	getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), admin))
	getRR := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(getRR, getReq)

	if getRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-integration ID, got %d: %s", getRR.Code, getRR.Body.String())
	}
}

func TestUpdateConfig_HA_SetsUpdatedBy(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"guild_id":"99999"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/discord/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rows, err := client.IntegrationConfig.Query().All(req.Context())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].UpdatedBy != "u1" {
		t.Errorf("expected updated_by u1, got %q", rows[0].UpdatedBy)
	}
}

// --- Deployment mode tests ---

func TestListIntegrations_DeploymentMode(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	mgr.plugins["discord"] = map[string]string{}
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

	byName := make(map[string]IntegrationSummary)
	for _, s := range result {
		byName[s.Name] = s
	}

	if tg, ok := byName["telegram"]; ok {
		if tg.DeploymentMode != "plugin" {
			t.Errorf("telegram: expected deployment_mode=plugin, got %q", tg.DeploymentMode)
		}
	}

	if dc, ok := byName["discord"]; ok {
		if dc.DeploymentMode != "external" {
			t.Errorf("discord: expected deployment_mode=external, got %q", dc.DeploymentMode)
		}
	}
}

func TestGetIntegration_DeploymentMode(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

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

	if detail.DeploymentMode != "plugin" {
		t.Errorf("expected deployment_mode=plugin, got %q", detail.DeploymentMode)
	}
}

func TestIsHAIntegration_Modes(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{}
	srv.pluginManager = mgr

	if srv.isHAIntegration(mgr, "telegram") {
		t.Error("plugin-mode telegram should not be HA")
	}

	if !srv.isHAIntegration(mgr, "discord") {
		t.Error("HA-mode discord should be HA")
	}

	// selfManaged without deploymentModes should NOT be HA.
	mgr2 := newMockIntegrationManager()
	mgr2.selfManaged["slack"] = true
	if srv.isHAIntegration(mgr2, "slack") {
		t.Error("self-managed without HA mode should not be HA")
	}
}

func TestUpdateConfig_HA_SkipsReconfigure(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	body := `{"settings":{"guild_id":"12345"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/integrations/discord/config", strings.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(mgr.replaceConfigCalls) != 0 {
		t.Errorf("expected no ReplaceBrokerConfig calls for HA integration, got %v", mgr.replaceConfigCalls)
	}
}

func TestGetIntegration_HA_ReadsFromPostgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires Postgres backend; set SCION_TEST_POSTGRES_URL and build with -tags integration")
	}
	client := enttest.NewClient(t)

	mgr := newMockIntegrationManager()
	mgr.plugins["discord"] = map[string]string{
		"guild_id": "boot-value",
		"hub_url":  "https://hub.example.com",
	}
	mgr.deploymentModes["discord"] = plugin.DeploymentModeHA

	// Write config to Postgres — this is what PUT would have done.
	provider := config.NewPostgresConfigProvider(client, "discord")
	if err := provider.Save(context.Background(), map[string]string{
		"guild_id":       "db-value",
		"application_id": "app-from-db",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	srv := &Server{dbDriver: "postgres"}
	srv.pluginManager = mgr
	srv.entClient = client

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/discord", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var detail IntegrationDetail
	if err := json.NewDecoder(rr.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Settings should reflect Postgres values, not boot-time map.
	if detail.Settings["guild_id"] != "db-value" {
		t.Errorf("guild_id: expected db-value, got %q", detail.Settings["guild_id"])
	}
	if detail.Settings["application_id"] != "app-from-db" {
		t.Errorf("application_id: expected app-from-db, got %q", detail.Settings["application_id"])
	}
	// Internal keys should be filtered out.
	if _, ok := detail.Settings["hub_url"]; ok {
		t.Error("hub_url should be filtered from settings")
	}
}

func TestInstallPlugin_PassesConfigFile(t *testing.T) {
	mgr := newMockIntegrationManager()

	if err := mgr.InstallPlugin("telegram", "/repo", "/plugins", "~/.scion/scion-telegram.yaml"); err != nil {
		t.Fatalf("InstallPlugin: %v", err)
	}

	cfg := mgr.GetPluginConfig("broker", "telegram")
	if cfg == nil {
		t.Fatal("expected non-nil config after install")
	}
	if cfg["config_file"] == "" {
		t.Error("expected config_file to be set after InstallPlugin with configFile parameter")
	}
	if cfg["config_file"] != "~/.scion/scion-telegram.yaml" {
		t.Errorf("expected config_file=~/.scion/scion-telegram.yaml, got %q", cfg["config_file"])
	}
}

func TestGetIntegration_ReadsFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "telegram.yaml")
	if err := os.WriteFile(configFile, []byte("webhook_listen: \":9095\"\ndb_path: /tmp/tg.db\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{
		"config_file":    configFile,
		"webhook_listen": ":9094",
		"hub_url":        "https://hub.example.com",
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
		t.Fatalf("decode: %v", err)
	}

	// Settings should reflect the YAML file values, not the boot-time map.
	if detail.Settings["webhook_listen"] != ":9095" {
		t.Errorf("webhook_listen: expected :9095 (from file), got %q", detail.Settings["webhook_listen"])
	}
	if detail.Settings["db_path"] != "/tmp/tg.db" {
		t.Errorf("db_path: expected /tmp/tg.db, got %q", detail.Settings["db_path"])
	}
	if _, ok := detail.Settings["hub_url"]; ok {
		t.Error("hub_url should be filtered from settings")
	}
}
