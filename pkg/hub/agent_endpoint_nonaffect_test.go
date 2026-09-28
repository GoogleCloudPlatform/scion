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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetPluginHubCreds_UsesHubEndpointNotAgentEndpoint proves that the
// agent-endpoint override — used only for the SCION_HUB_ENDPOINT value
// injected into agents — does not leak into the hub_url credential handed to
// broker plugins (chat-app, Slack, Discord, Telegram, Teams), which build
// user-facing registration links from it.
func TestGetPluginHubCreds_UsesHubEndpointNotAgentEndpoint(t *testing.T) {
	srv := &Server{
		config: ServerConfig{
			HubEndpoint:   "https://hub.example.com",
			AgentEndpoint: "http://192.0.2.10:8080",
		},
	}

	creds := srv.getPluginHubCreds(context.Background(), "test-plugin")

	if got := creds["hub_url"]; got != "https://hub.example.com" {
		t.Errorf("hub_url = %q, want the regular hub endpoint %q, not the agent-endpoint override", got, "https://hub.example.com")
	}
}

// TestInstallIntegration_SelfManaged_BridgeConfigUsesHubEndpoint proves that
// the bridge bootstrap config template written on self-managed integration
// install — the same template a chat bridge reads for its own hub_url — uses
// the regular hub endpoint even when the agent-endpoint override is set.
// This is the sibling of TestInstallIntegration_SelfManaged_CreatesAdminConfig
// with AgentEndpoint additionally configured.
func TestInstallIntegration_SelfManaged_BridgeConfigUsesHubEndpoint(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}

	mgr := newMockIntegrationManager()

	srv, admin, ctx := integAdminServer(t, "inst-aenocfg")
	srv.pluginManager = mgr
	srv.config.HubEndpoint = "http://hub.example.com:8080"
	srv.config.AgentEndpoint = "http://192.0.2.10:8080"

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/integrations/a2a-bridge/install", nil)
	req = req.WithContext(contextWithIdentity(ctx, admin))
	rr := httptest.NewRecorder()
	srv.handleAdminIntegrationByName(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	bridgeConfigPath := filepath.Join(tmpHome, ".scion", "scion-a2a-bridge.yaml")
	bridgeData, err := os.ReadFile(bridgeConfigPath)
	if err != nil {
		t.Fatalf("bridge config file not created: %v", err)
	}
	bridgeStr := string(bridgeData)
	if !strings.Contains(bridgeStr, "http://hub.example.com:8080") {
		t.Errorf("bridge config must use the regular hub endpoint, got: %s", bridgeStr)
	}
	if strings.Contains(bridgeStr, "192.0.2.10") {
		t.Errorf("bridge config must not use the agent-endpoint override, got: %s", bridgeStr)
	}
}
