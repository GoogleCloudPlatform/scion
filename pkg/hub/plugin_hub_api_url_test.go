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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

// Hub-managed broker plugins run on the hub's host. They get hub_url (the
// public endpoint, for links people open) and hub_api_url (the hub's
// loopback URL, for their own API calls, so a proxy such as IAP in front of
// the public endpoint never sees them). Plugins on another host
// (self-managed, HA) must not get the loopback URL.

const (
	testPluginPublicURL = "https://hub.example.com"
	testPluginAPIURL    = "http://localhost:8080"
)

func pluginAPIURLServer() *Server {
	return &Server{config: ServerConfig{
		HubEndpoint:     testPluginPublicURL,
		PluginHubAPIURL: testPluginAPIURL,
	}}
}

func TestGetPluginHubCreds_HubAPIURL(t *testing.T) {
	srv := pluginAPIURLServer()

	creds := srv.getPluginHubCreds(context.Background(), "telegram", true)
	if got := creds["hub_url"]; got != testPluginPublicURL {
		t.Errorf("hub_url = %q, want the public endpoint %q", got, testPluginPublicURL)
	}
	if got := creds["hub_api_url"]; got != testPluginAPIURL {
		t.Errorf("hub_api_url = %q, want the loopback URL %q", got, testPluginAPIURL)
	}

	creds = srv.getPluginHubCreds(context.Background(), "telegram", false)
	if got, ok := creds["hub_api_url"]; ok {
		t.Errorf("hub_api_url = %q for a plugin not on the hub's host, want it absent", got)
	}
	if got := creds["hub_url"]; got != testPluginPublicURL {
		t.Errorf("hub_url = %q, want the public endpoint %q", got, testPluginPublicURL)
	}
}

func TestGetPluginHubCreds_NoHubAPIURLWhenUnknown(t *testing.T) {
	srv := &Server{config: ServerConfig{HubEndpoint: testPluginPublicURL}}
	if got, ok := srv.getPluginHubCreds(context.Background(), "telegram", true)["hub_api_url"]; ok {
		t.Errorf("hub_api_url = %q with no PluginHubAPIURL, want it absent", got)
	}
}

// TestHubManagedPluginURLCreds covers the server startup path, which
// injects credentials only into hub-managed plugins.
func TestHubManagedPluginURLCreds(t *testing.T) {
	creds := pluginAPIURLServer().HubManagedPluginURLCreds()
	if creds["hub_url"] != testPluginPublicURL || creds["hub_api_url"] != testPluginAPIURL {
		t.Errorf("creds = %v, want hub_url %q and hub_api_url %q", creds, testPluginPublicURL, testPluginAPIURL)
	}
}

// TestResolveIntegrationMergedConfig_HubAPIURLGating covers reconfigure and
// restart: hub_api_url replaces a stale value for a hub-managed plugin and
// is never handed to a self-managed or HA one.
func TestResolveIntegrationMergedConfig_HubAPIURLGating(t *testing.T) {
	tests := []struct {
		name        string
		selfManaged bool
		mode        plugin.DeploymentMode
		wantAPIURL  string
	}{
		{name: "hub-managed", wantAPIURL: testPluginAPIURL},
		{name: "self-managed", selfManaged: true},
		{name: "HA", mode: plugin.DeploymentModeHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newMockIntegrationManager()
			mgr.plugins["telegram"] = map[string]string{
				"hub_url":     "http://stale:9999",
				"hub_api_url": "http://stale-api:9999",
			}
			mgr.selfManaged["telegram"] = tt.selfManaged
			if tt.mode != "" {
				mgr.deploymentModes["telegram"] = tt.mode
			}

			merged := pluginAPIURLServer().resolveIntegrationMergedConfig(context.Background(), mgr, "telegram")
			if got := merged["hub_url"]; got != testPluginPublicURL {
				t.Errorf("hub_url = %q, want %q", got, testPluginPublicURL)
			}
			got, ok := merged["hub_api_url"]
			if tt.wantAPIURL == "" {
				if ok {
					t.Errorf("hub_api_url = %q, want it absent (the stale cached value must not carry over either)", got)
				}
			} else if got != tt.wantAPIURL {
				t.Errorf("hub_api_url = %q, want %q", got, tt.wantAPIURL)
			}
		})
	}
}

// TestActivateInstalledIntegration_HubAPIURLGating covers activation of an
// installed plugin from the admin UI.
func TestActivateInstalledIntegration_HubAPIURLGating(t *testing.T) {
	tests := []struct {
		name       string
		entry      config.V1PluginEntry
		wantAPIURL bool
	}{
		{name: "hub-managed", entry: config.V1PluginEntry{Path: "./telegram"}, wantAPIURL: true},
		{name: "plugin mode", entry: config.V1PluginEntry{Path: "./telegram", Mode: "plugin"}, wantAPIURL: true},
		{name: "self-managed flag", entry: config.V1PluginEntry{Path: "./telegram", SelfManaged: true}},
		{name: "self-managed mode", entry: config.V1PluginEntry{Mode: "self-managed", Address: "localhost:9090"}},
		{name: "HA", entry: config.V1PluginEntry{Mode: "grpc", Address: "telegram:9090"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, mgr := newActivationServer(t, newMigrationSecretBackend())
			srv.config.HubEndpoint = testPluginPublicURL
			srv.config.PluginHubAPIURL = testPluginAPIURL

			entry := tt.entry
			if err := srv.activateInstalledIntegration(context.Background(), mgr, "telegram", &entry); err != nil {
				t.Fatalf("activateInstalledIntegration: %v", err)
			}
			if len(mgr.loadOneEntries) != 1 {
				t.Fatalf("LoadOne calls = %d, want 1", len(mgr.loadOneEntries))
			}
			cfg := mgr.loadOneEntries[0].Config
			if got := cfg["hub_url"]; got != testPluginPublicURL {
				t.Errorf("hub_url = %q, want %q", got, testPluginPublicURL)
			}
			got, ok := cfg["hub_api_url"]
			if tt.wantAPIURL && got != testPluginAPIURL {
				t.Errorf("hub_api_url = %q, want %q", got, testPluginAPIURL)
			}
			if !tt.wantAPIURL && ok {
				t.Errorf("hub_api_url = %q, want it absent", got)
			}
		})
	}
}

// TestPluginHubAPIURL_IsHubInjectedKey checks that hub_api_url is treated
// like hub_url: hidden from the settings the admin UI shows and never
// carried over from a cached config.
func TestPluginHubAPIURL_IsHubInjectedKey(t *testing.T) {
	filtered := filterSensitiveConfig("telegram", map[string]string{
		"hub_url":        testPluginPublicURL,
		"hub_api_url":    testPluginAPIURL,
		"webhook_listen": ":9094",
	})
	if _, ok := filtered["hub_api_url"]; ok {
		t.Error("hub_api_url should be filtered from the shown settings")
	}
	if filtered["webhook_listen"] != ":9094" {
		t.Errorf("webhook_listen = %q, want it kept", filtered["webhook_listen"])
	}
	if !hubKeys["hub_api_url"] || !reconfigureRuntimeKeys["hub_api_url"] {
		t.Error("hub_api_url should be listed with hub_url in hubKeys and reconfigureRuntimeKeys")
	}
}
