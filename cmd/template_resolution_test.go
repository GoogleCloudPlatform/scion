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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestParseTemplateScope(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedScope string
		expectedName  string
	}{
		{
			name:          "no scope prefix",
			input:         "custom-claude",
			expectedScope: "",
			expectedName:  "custom-claude",
		},
		{
			name:          "global scope prefix",
			input:         "global:claude",
			expectedScope: "global",
			expectedName:  "claude",
		},
		{
			name:          "grove scope prefix",
			input:         "grove:custom-template",
			expectedScope: "grove",
			expectedName:  "custom-template",
		},
		{
			name:          "project scope prefix (synonym for grove)",
			input:         "project:custom-template",
			expectedScope: "grove",
			expectedName:  "custom-template",
		},
		{
			name:          "user scope prefix",
			input:         "user:my-template",
			expectedScope: "user",
			expectedName:  "my-template",
		},
		{
			name:          "unknown prefix treated as name",
			input:         "unknown:template",
			expectedScope: "",
			expectedName:  "unknown:template",
		},
		{
			name:          "multiple colons",
			input:         "grove:my:template",
			expectedScope: "grove",
			expectedName:  "my:template",
		},
		{
			name:          "empty string",
			input:         "",
			expectedScope: "",
			expectedName:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, name := parseTemplateScope(tt.input)
			if scope != tt.expectedScope {
				t.Errorf("parseTemplateScope(%q) scope = %q, want %q", tt.input, scope, tt.expectedScope)
			}
			if name != tt.expectedName {
				t.Errorf("parseTemplateScope(%q) name = %q, want %q", tt.input, name, tt.expectedName)
			}
		})
	}
}

func TestTruncateHash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short hash unchanged",
			input:    "sha256:abc123",
			expected: "sha256:abc123",
		},
		{
			name:     "exact 20 chars unchanged",
			input:    "12345678901234567890",
			expected: "12345678901234567890",
		},
		{
			name:     "long hash truncated",
			input:    "sha256:abcdef0123456789abcdef0123456789",
			expected: "sha256:abcdef0123456...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateHash(tt.input)
			if result != tt.expected {
				t.Errorf("truncateHash(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFormatTemplateNotFoundError(t *testing.T) {
	// Test that the error message is formatted correctly
	err := formatTemplateNotFoundError("test-template", "/some/project/path")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errMsg := err.Error()

	// Check that key information is present
	if !contains(errMsg, "test-template") {
		t.Error("error message should contain template name")
	}
	if !contains(errMsg, "not found") {
		t.Error("error message should indicate template not found")
	}
	if !contains(errMsg, "project scope") {
		t.Error("error message should mention project scope")
	}
	if !contains(errMsg, "scion template sync") {
		t.Error("error message should provide guidance on how to create template")
	}
}

func TestFormatTemplateNotFoundErrorNoProject(t *testing.T) {
	// Test with empty project path
	err := formatTemplateNotFoundError("test-template", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	errMsg := err.Error()

	// Should not have project scope line when no project path
	if contains(errMsg, "project scope") {
		t.Error("error message should not mention project scope when no project path")
	}
}

func TestPromptChoiceAutoConfirm(t *testing.T) {
	// Backup original values
	origAutoConfirm := autoConfirm
	defer func() { autoConfirm = origAutoConfirm }()

	t.Run("auto-confirm with default returns default", func(t *testing.T) {
		autoConfirm = true
		choice, err := promptChoice("Choice", "H", []string{"U", "H", "C"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if choice != "H" {
			t.Errorf("expected default choice 'H', got %q", choice)
		}
	})

	t.Run("auto-confirm without default returns error", func(t *testing.T) {
		autoConfirm = true
		_, err := promptChoice("Choice", "", []string{"U", "H", "C"})
		if err == nil {
			t.Fatal("expected error for no default in auto-confirm mode")
		}
		if !contains(err.Error(), "non-interactive") {
			t.Errorf("error should mention non-interactive mode, got: %s", err.Error())
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestDetectHarnessType(t *testing.T) {
	t.Run("detects from harness_config field", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `harness_config: claude`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "test", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "claude" {
			t.Errorf("expected 'claude', got %q", harness)
		}
	})

	t.Run("detects from legacy harness field", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `{"harness": "gemini"}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "test", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "gemini" {
			t.Errorf("expected 'gemini', got %q", harness)
		}
	})

	t.Run("harness_config takes priority over legacy harness", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `{"harness_config": "claude", "harness": "gemini"}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "test", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "claude" {
			t.Errorf("expected 'claude', got %q", harness)
		}
	})

	t.Run("falls back to default_harness_config", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `default_harness_config: claude`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.yaml"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "my-custom-template", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "claude" {
			t.Errorf("expected 'claude', got %q", harness)
		}
	})

	t.Run("infers from template name", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `{}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "my-claude-template", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "claude" {
			t.Errorf("expected 'claude', got %q", harness)
		}
	})

	t.Run("returns empty string when undetectable", func(t *testing.T) {
		tmpDir := t.TempDir()
		configContent := `{}`
		if err := os.WriteFile(filepath.Join(tmpDir, "scion-agent.json"), []byte(configContent), 0644); err != nil {
			t.Fatal(err)
		}
		tpl := &config.Template{Name: "my-custom-template", Path: tmpDir}
		harness, err := detectHarnessType(tpl)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if harness != "" {
			t.Errorf("expected empty string, got %q", harness)
		}
	})
}

func TestBrokerHasLocalAccess(t *testing.T) {
	const projectID = "grove-123"
	const brokerID = "broker-456"

	t.Run("returns true when broker has local path", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/groves/"+projectID+"/providers" && r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"providers": []map[string]interface{}{
						{
							"brokerId":   brokerID,
							"brokerName": "local-broker",
							"localPath":  "/home/user/project/.scion",
							"status":     "online",
						},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		hubCtx := &HubContext{
			Client:   client,
			BrokerID: brokerID,
		}

		if !brokerHasLocalAccess(context.Background(), hubCtx, projectID) {
			t.Error("expected brokerHasLocalAccess to return true for broker with local path")
		}
	})

	t.Run("returns false when broker has no local path", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/groves/"+projectID+"/providers" && r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"providers": []map[string]interface{}{
						{
							"brokerId":   brokerID,
							"brokerName": "remote-broker",
							"status":     "online",
						},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		hubCtx := &HubContext{
			Client:   client,
			BrokerID: brokerID,
		}

		if brokerHasLocalAccess(context.Background(), hubCtx, projectID) {
			t.Error("expected brokerHasLocalAccess to return false for broker without local path")
		}
	})

	t.Run("returns false when broker ID does not match", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/groves/"+projectID+"/providers" && r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"providers": []map[string]interface{}{
						{
							"brokerId":   "other-broker",
							"brokerName": "other",
							"localPath":  "/some/path",
							"status":     "online",
						},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		hubCtx := &HubContext{
			Client:   client,
			BrokerID: brokerID,
		}

		if brokerHasLocalAccess(context.Background(), hubCtx, projectID) {
			t.Error("expected brokerHasLocalAccess to return false when broker ID doesn't match")
		}
	})

	t.Run("returns false when no broker ID set", func(t *testing.T) {
		hubCtx := &HubContext{
			BrokerID: "",
		}

		if brokerHasLocalAccess(context.Background(), hubCtx, projectID) {
			t.Error("expected brokerHasLocalAccess to return false when no broker ID is set")
		}
	})

	t.Run("returns false when no grove ID", func(t *testing.T) {
		hubCtx := &HubContext{
			BrokerID: brokerID,
		}

		if brokerHasLocalAccess(context.Background(), hubCtx, "") {
			t.Error("expected brokerHasLocalAccess to return false when no grove ID is provided")
		}
	})

	t.Run("returns false when API returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		client, err := hubclient.New(server.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		hubCtx := &HubContext{
			Client:   client,
			BrokerID: brokerID,
		}

		if brokerHasLocalAccess(context.Background(), hubCtx, projectID) {
			t.Error("expected brokerHasLocalAccess to return false when API returns error")
		}
	})
}
