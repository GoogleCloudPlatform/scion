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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// TestHandlePutServerConfig_AgentEndpoint_InvalidRejected proves that the
// file-mode admin server-config write path validates server.hub.agent_endpoint
// with the same rules the Hub applies at startup, rather than persisting a
// value that would only fail later when the Hub next restarts, and that the
// 400 body never echoes a credential from a rejected value.
func TestHandlePutServerConfig_AgentEndpoint_InvalidRejected(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		secrets []string // substrings that must not appear anywhere in the 400 body
	}{
		{name: "invalid scheme", value: "ftp://not-valid"},
		{name: "credentials in userinfo", value: "http://someuser:supersecret@hub.example.com", secrets: []string{"someuser", "supersecret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")

			srv := &Server{}
			admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

			body := fmt.Sprintf(`{"server":{"hub":{"agent_endpoint":%q}}}`, tt.value)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", bytes.NewBufferString(body))
			req = req.WithContext(contextWithIdentity(req.Context(), admin))
			rr := httptest.NewRecorder()
			srv.handleAdminServerConfig(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "server.hub.agent_endpoint") {
				t.Errorf("expected the 400 body to carry the validator's message naming server.hub.agent_endpoint, got: %s", rr.Body.String())
			}
			for _, secret := range tt.secrets {
				if strings.Contains(rr.Body.String(), secret) {
					t.Errorf("expected the 400 body not to contain %q, got: %s", secret, rr.Body.String())
				}
			}

			if _, err := os.Stat(settingsPath); err == nil {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("expected nothing persisted for an invalid value, but settings.yaml was written: %s", data)
			} else if !os.IsNotExist(err) {
				t.Fatalf("unexpected error checking settings.yaml: %v", err)
			}
		})
	}
}

// TestHandlePutServerConfig_AgentEndpoint_ValidPersistedNormalized proves
// that a valid value is written to settings.yaml in its normalized form —
// the same form ValidateAgentEndpoint returns and the Hub stamps into agents
// at startup — so the persisted value and the effective value never diverge.
func TestHandlePutServerConfig_AgentEndpoint_ValidPersistedNormalized(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(tmpHome, ".scion", "settings.yaml")

	srv := &Server{}
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")

	body := `{"server":{"hub":{"agent_endpoint":"HTTP://hub-internal.example.com:8080/"}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config", bytes.NewBufferString(body))
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.yaml not written: %v", err)
	}

	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse persisted settings: %v", err)
	}

	wantNormalized, err := config.ValidateAgentEndpoint("HTTP://hub-internal.example.com:8080/")
	if err != nil {
		t.Fatalf("ValidateAgentEndpoint: %v", err)
	}

	server, _ := raw["server"].(map[string]interface{})
	hub, _ := server["hub"].(map[string]interface{})
	got, _ := hub["agent_endpoint"].(string)
	if got != wantNormalized {
		t.Errorf("persisted agent_endpoint = %q, want the normalized form %q", got, wantNormalized)
	}
}
