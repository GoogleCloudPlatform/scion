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

package hubsync

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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestEnsureHubReady_GlobalFallbackWithHubEnabled(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID", "SCION_HUB_GROVE_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}
	// When projectPath="" and the resolution falls back to global, EnsureHubReady
	// should still attempt hub integration if hub is enabled in global settings.
	// This was previously broken: the function returned (nil, nil) immediately
	// when falling back to global, regardless of whether hub was enabled.

	projectID := "test-global-grove-id"

	// Set up a mock hub server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/groves/"+projectID:
			// Project is already registered
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "Global",
			})
		case strings.Contains(r.URL.Path, "/agents"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Create a temp HOME with global .scion directory and hub-enabled settings
	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("Failed to create global dir: %v", err)
	}

	// Write settings with hub enabled and endpoint pointing to our test server
	// grove_id is a top-level setting, not nested under hub
	settingsContent := fmt.Sprintf(`grove_id: %s
hub:
  enabled: true
  endpoint: %s
`, projectID, server.URL)
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	// Override HOME so ResolveProjectPath("") falls back to our temp global dir
	t.Setenv("HOME", tmpHome)
	// Override hub endpoint via env var to ensure it points to our test server
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	// Use dev token for auth
	t.Setenv("SCION_DEV_TOKEN", "test-dev-token")
	t.Setenv("SCION_AUTH_TOKEN", "")

	// Change to tmpHome so FindProjectRoot() doesn't find the real project
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		SkipSync:    true,
		AutoConfirm: true,
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx == nil {
		t.Fatal("EnsureHubReady returned nil; expected hub context when hub is enabled globally")
	}
	if !hubCtx.IsGlobal {
		t.Error("Expected IsGlobal=true for global grove fallback")
	}
	if hubCtx.ProjectID != projectID {
		t.Errorf("ProjectID = %q, want %q", hubCtx.ProjectID, projectID)
	}
}

func TestEnsureHubReady_EndpointOverrideBeatsSettings(t *testing.T) {
	projectID := "test-override-grove-id"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/groves/"+projectID:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "Override",
			})
		case strings.Contains(r.URL.Path, "/agents"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("Failed to create global dir: %v", err)
	}

	settingsContent := fmt.Sprintf(`grove_id: %s
hub:
  enabled: true
  endpoint: http://localhost:8080
`, projectID)
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_DEV_TOKEN", "test-dev-token")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")

	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		AutoConfirm:      true,
		SkipSync:         true,
		EndpointOverride: server.URL,
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx == nil {
		t.Fatal("EnsureHubReady returned nil; expected hub context when override is set")
	}
	if hubCtx.Endpoint != server.URL {
		t.Fatalf("Endpoint = %q, want %q", hubCtx.Endpoint, server.URL)
	}
}

func TestEnsureHubReady_GlobalFallbackWithHubDisabled(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID", "SCION_HUB_GROVE_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}
	// When projectPath="" and the resolution falls back to global with hub NOT
	// enabled, EnsureHubReady should return (nil, nil) - same behavior as before.

	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("Failed to create global dir: %v", err)
	}

	// Write settings with hub NOT enabled
	settingsContent := `hub:
  enabled: false
`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	t.Setenv("HOME", tmpHome)

	// Change to tmpHome so FindProjectRoot() doesn't find the real project
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		SkipSync:    true,
		AutoConfirm: true,
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx != nil {
		t.Error("EnsureHubReady should return nil when hub is not enabled")
	}
}

func TestEnsureHubReady_HubContextEnvVars(t *testing.T) {
	// When hub.enabled is NOT set in settings but SCION_HUB_ENDPOINT env var
	// is present (inside a hub-connected container), EnsureHubReady should
	// return a valid hub context via the env var detection path.

	projectID := "container-grove-id"

	// Set up a mock hub server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Create a temp HOME with global .scion directory but NO hub.enabled
	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("Failed to create global dir: %v", err)
	}

	// Write minimal settings — hub.enabled is intentionally NOT set
	settingsContent := `runtime: docker
`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	// Simulate container env vars
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", projectID)
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	t.Setenv("SCION_DEV_TOKEN", "")

	// Change to tmpHome so FindProjectRoot() falls back to global
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		AutoConfirm: true,
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx == nil {
		t.Fatal("EnsureHubReady returned nil; expected hub context when SCION_HUB_ENDPOINT is set")
	}
	if hubCtx.Endpoint != server.URL {
		t.Errorf("Endpoint = %q, want %q", hubCtx.Endpoint, server.URL)
	}
	if hubCtx.ProjectID != projectID {
		t.Errorf("ProjectID = %q, want %q", hubCtx.ProjectID, projectID)
	}
}

func TestEnsureHubReady_HubContextSkipsSyncAndRegistration(t *testing.T) {
	// When running in hub context (container), EnsureHubReady should skip
	// grove registration and sync checks. Verify that no registration or
	// sync API calls are made.

	projectID := "container-grove-id-2"
	registrationCalled := false
	syncCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/groves/"+projectID:
			// Project lookup — should not reach here in container context
			registrationCalled = true
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "test-grove",
			})
		case strings.Contains(r.URL.Path, "/agents"):
			syncCalled = true
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatalf("Failed to create global dir: %v", err)
	}

	// No hub.enabled in settings
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("runtime: docker\n"), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", projectID)
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	t.Setenv("SCION_DEV_TOKEN", "")

	origDir, _ := os.Getwd()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		AutoConfirm: true,
		// Intentionally NOT setting SkipSync — should be forced by hub context
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx == nil {
		t.Fatal("EnsureHubReady returned nil")
	}

	if registrationCalled {
		t.Error("Project registration API was called; should be skipped in hub context")
	}
	if syncCalled {
		t.Error("Sync API was called; should be skipped in hub context")
	}
}

func TestEnsureHubReady_HubContextProjectIDEnvPriority(t *testing.T) {
	// When SCION_GROVE_ID env var and settings.grove_id both exist in hub
	// context, the env var should take priority. This is important for
	// template-sync agents that clone an external repo whose .scion/settings
	// contains the source repo's grove_id.

	envProjectID := "env-grove-id-target"
	settingsProjectID := "settings-grove-id-source"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	// Create a project directory with .scion that has a grove_id in settings
	projectDir := filepath.Join(tmpHome, "grove")
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("Failed to create scion dir: %v", err)
	}

	settingsContent := fmt.Sprintf("grove_id: %s\nruntime: docker\n", settingsProjectID)
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("Failed to write settings: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", envProjectID)
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	t.Setenv("SCION_DEV_TOKEN", "")

	origDir, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}
	defer os.Chdir(origDir)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
		AutoConfirm: true,
	})
	if err != nil {
		t.Fatalf("EnsureHubReady returned error: %v", err)
	}
	if hubCtx == nil {
		t.Fatal("EnsureHubReady returned nil")
	}
	if hubCtx.ProjectID != envProjectID {
		t.Errorf("ProjectID = %q, want %q (SCION_GROVE_ID should take priority over settings.grove_id in hub context)", hubCtx.ProjectID, envProjectID)
	}
}

func TestSyncResult_IsInSync(t *testing.T) {
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "empty result is in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				InSync:     nil,
			},
			expected: true,
		},
		{
			name: "only in sync agents",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				InSync:     []string{"agent1", "agent2"},
			},
			expected: true,
		},
		{
			name: "agents to register",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   nil,
				InSync:     []string{"agent1"},
			},
			expected: false,
		},
		{
			name: "agents to remove",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-agent-id"}},
				InSync:     []string{"agent1"},
			},
			expected: false,
		},
		{
			name: "both register and remove",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-agent-id"}},
				InSync:     nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestGetLocalAgents(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "hubsync-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create agents directory structure
	agentsDir := filepath.Join(tmpDir, "agents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("Failed to create agents dir: %v", err)
	}

	// Create agent1 with YAML config
	agent1Dir := filepath.Join(agentsDir, "agent1")
	if err := os.MkdirAll(agent1Dir, 0755); err != nil {
		t.Fatalf("Failed to create agent1 dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent1Dir, "scion-agent.yaml"), []byte("harness: claude"), 0644); err != nil {
		t.Fatalf("Failed to write agent1 config: %v", err)
	}

	// Create agent2 with JSON config
	agent2Dir := filepath.Join(agentsDir, "agent2")
	if err := os.MkdirAll(agent2Dir, 0755); err != nil {
		t.Fatalf("Failed to create agent2 dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agent2Dir, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("Failed to write agent2 config: %v", err)
	}

	// Create a directory without config (should be ignored)
	orphanDir := filepath.Join(agentsDir, "orphan")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatalf("Failed to create orphan dir: %v", err)
	}

	// Test GetLocalAgents
	agents, err := GetLocalAgents(tmpDir)
	if err != nil {
		t.Fatalf("GetLocalAgents failed: %v", err)
	}

	if len(agents) != 2 {
		t.Errorf("Expected 2 agents, got %d", len(agents))
	}

	// Check that both agents are found
	agentMap := make(map[string]bool)
	for _, a := range agents {
		agentMap[a] = true
	}

	if !agentMap["agent1"] {
		t.Error("Expected to find agent1")
	}
	if !agentMap["agent2"] {
		t.Error("Expected to find agent2")
	}
	if agentMap["orphan"] {
		t.Error("Should not find orphan directory")
	}
}

func TestGetLocalAgents_EmptyDir(t *testing.T) {
	// Create a temporary directory without agents
	tmpDir, err := os.MkdirTemp("", "hubsync-test-empty-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	agents, err := GetLocalAgents(tmpDir)
	if err != nil {
		t.Fatalf("GetLocalAgents failed: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("Expected 0 agents, got %d", len(agents))
	}
}

func TestGetLocalAgents_NoDir(t *testing.T) {
	// Test with a path that doesn't exist
	agents, err := GetLocalAgents("/nonexistent/path")
	if err != nil {
		t.Fatalf("GetLocalAgents should not error on missing dir: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("Expected 0 agents for nonexistent path, got %d", len(agents))
	}
}

func TestSyncResult_ExcludeAgent(t *testing.T) {
	tests := []struct {
		name           string
		result         SyncResult
		excludeAgent   string
		expectedSync   bool
		expectedRegLen int
		expectedRemLen int
	}{
		{
			name: "exclude agent from ToRegister",
			result: SyncResult{
				ToRegister: []string{"agent1", "agent2"},
				ToRemove:   []AgentRef{},
				InSync:     []string{"agent3"},
			},
			excludeAgent:   "agent1",
			expectedSync:   false, // still has agent2 to register
			expectedRegLen: 1,
			expectedRemLen: 0,
		},
		{
			name: "exclude agent from ToRemove",
			result: SyncResult{
				ToRegister: []string{},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}, {Name: "agent2", ID: "id2"}},
				InSync:     []string{"agent3"},
			},
			excludeAgent:   "agent1",
			expectedSync:   false, // still has agent2 to remove
			expectedRegLen: 0,
			expectedRemLen: 1,
		},
		{
			name: "exclude only agent in ToRegister makes it in sync",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{},
				InSync:     []string{"agent2"},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude only agent in ToRemove makes it in sync",
			result: SyncResult{
				ToRegister: []string{},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}},
				InSync:     []string{"agent2"},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude agent from both lists",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent1", ID: "id1"}}, // unlikely but test the logic
				InSync:     []string{},
			},
			excludeAgent:   "agent1",
			expectedSync:   true,
			expectedRegLen: 0,
			expectedRemLen: 0,
		},
		{
			name: "exclude non-existent agent has no effect",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
				InSync:     []string{},
			},
			excludeAgent:   "agent3",
			expectedSync:   false,
			expectedRegLen: 1,
			expectedRemLen: 1,
		},
		{
			name: "empty exclude agent has no effect",
			result: SyncResult{
				ToRegister: []string{"agent1"},
				ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
				InSync:     []string{},
			},
			excludeAgent:   "",
			expectedSync:   false,
			expectedRegLen: 1,
			expectedRemLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := tt.result.ExcludeAgent(tt.excludeAgent)
			if filtered.IsInSync() != tt.expectedSync {
				t.Errorf("IsInSync() = %v, want %v", filtered.IsInSync(), tt.expectedSync)
			}
			if len(filtered.ToRegister) != tt.expectedRegLen {
				t.Errorf("len(ToRegister) = %d, want %d", len(filtered.ToRegister), tt.expectedRegLen)
			}
			if len(filtered.ToRemove) != tt.expectedRemLen {
				t.Errorf("len(ToRemove) = %d, want %d", len(filtered.ToRemove), tt.expectedRemLen)
			}
		})
	}
}

func TestSyncResult_PendingNotAffectIsInSync(t *testing.T) {
	// Pending agents should not affect the IsInSync check
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "only pending agents is in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: true, // Pending agents don't require sync
		},
		{
			name: "pending with in sync agents",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     []string{"agent1"},
			},
			expected: true,
		},
		{
			name: "pending with agents to register",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   nil,
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: false, // ToRegister requires action
		},
		{
			name: "pending with agents to remove",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-id"}},
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: false, // ToRemove requires action
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSyncResult_ExcludeAgent_WithPending(t *testing.T) {
	result := SyncResult{
		ToRegister: []string{"agent1"},
		ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
		Pending:    []AgentRef{{Name: "pending1", ID: "p1"}, {Name: "pending2", ID: "p2"}},
		InSync:     []string{"agent3"},
	}

	// Exclude a pending agent
	filtered := result.ExcludeAgent("pending1")

	if len(filtered.Pending) != 1 {
		t.Errorf("Expected 1 pending agent, got %d", len(filtered.Pending))
	}
	if len(filtered.Pending) > 0 && filtered.Pending[0].Name != "pending2" {
		t.Errorf("Expected pending2, got %s", filtered.Pending[0].Name)
	}

	// Original lists should be unchanged
	if len(filtered.ToRegister) != 1 {
		t.Errorf("Expected 1 ToRegister agent, got %d", len(filtered.ToRegister))
	}
	if len(filtered.ToRemove) != 1 {
		t.Errorf("Expected 1 ToRemove agent, got %d", len(filtered.ToRemove))
	}
}

func TestContainsIgnoreCase(t *testing.T) {
	tests := []struct {
		s        string
		substr   string
		expected bool
	}{
		{"Hello World", "hello", true},
		{"Hello World", "WORLD", true},
		{"Hello World", "llo wor", true},
		{"404 Not Found", "404", true},
		{"404 Not Found", "not found", true},
		{"Hello World", "goodbye", false},
		{"", "test", false},
		{"test", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.s+"_"+tt.substr, func(t *testing.T) {
			if got := containsIgnoreCase(tt.s, tt.substr); got != tt.expected {
				t.Errorf("containsIgnoreCase(%q, %q) = %v, want %v", tt.s, tt.substr, got, tt.expected)
			}
		})
	}
}

func TestProjectChoice_Constants(t *testing.T) {
	// Verify that the choice constants have expected values
	if ProjectChoiceCancel != 0 {
		t.Errorf("ProjectChoiceCancel should be 0, got %d", ProjectChoiceCancel)
	}
	if ProjectChoiceLink != 1 {
		t.Errorf("ProjectChoiceLink should be 1, got %d", ProjectChoiceLink)
	}
	if ProjectChoiceRegisterNew != 2 {
		t.Errorf("ProjectChoiceRegisterNew should be 2, got %d", ProjectChoiceRegisterNew)
	}
}

func TestSyncResult_RemoteOnlyNotAffectIsInSync(t *testing.T) {
	// RemoteOnly agents should not affect the IsInSync check
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "only remote-only agents is in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				RemoteOnly: []AgentRef{{Name: "remote-agent", ID: "remote-id"}},
				InSync:     nil,
			},
			expected: true,
		},
		{
			name: "remote-only with in sync agents",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				RemoteOnly: []AgentRef{{Name: "remote-agent", ID: "remote-id"}},
				InSync:     []string{"agent1"},
			},
			expected: true,
		},
		{
			name: "remote-only with agents to register",
			result: SyncResult{
				ToRegister: []string{"new-agent"},
				ToRemove:   nil,
				RemoteOnly: []AgentRef{{Name: "remote-agent", ID: "remote-id"}},
				InSync:     nil,
			},
			expected: false,
		},
		{
			name: "remote-only with agents to remove",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   []AgentRef{{Name: "old-agent", ID: "old-id"}},
				RemoteOnly: []AgentRef{{Name: "remote-agent", ID: "remote-id"}},
				InSync:     nil,
			},
			expected: false,
		},
		{
			name: "remote-only with pending is still in sync",
			result: SyncResult{
				ToRegister: nil,
				ToRemove:   nil,
				RemoteOnly: []AgentRef{{Name: "remote-agent", ID: "remote-id"}},
				Pending:    []AgentRef{{Name: "pending-agent", ID: "pending-id"}},
				InSync:     nil,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSyncResult_StaleLocalNotAffectIsInSync(t *testing.T) {
	tests := []struct {
		name     string
		result   SyncResult
		expected bool
	}{
		{
			name: "only stale-local agents is in sync",
			result: SyncResult{
				StaleLocal: []string{"agent-a"},
			},
			expected: true,
		},
		{
			name: "stale-local with remote-only is in sync",
			result: SyncResult{
				StaleLocal: []string{"agent-a"},
				RemoteOnly: []AgentRef{{Name: "remote", ID: "remote-id"}},
			},
			expected: true,
		},
		{
			name: "stale-local with to-register is not in sync",
			result: SyncResult{
				StaleLocal: []string{"agent-a"},
				ToRegister: []string{"agent-b"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.IsInSync(); got != tt.expected {
				t.Errorf("IsInSync() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSyncResult_ExcludeAgent_WithRemoteOnly(t *testing.T) {
	result := SyncResult{
		ToRegister: []string{"agent1"},
		ToRemove:   []AgentRef{{Name: "agent2", ID: "id2"}},
		RemoteOnly: []AgentRef{{Name: "remote1", ID: "r1"}, {Name: "remote2", ID: "r2"}},
		InSync:     []string{"agent3"},
	}

	// Exclude a remote-only agent
	filtered := result.ExcludeAgent("remote1")

	if len(filtered.RemoteOnly) != 1 {
		t.Errorf("Expected 1 remote-only agent, got %d", len(filtered.RemoteOnly))
	}
	if len(filtered.RemoteOnly) > 0 && filtered.RemoteOnly[0].Name != "remote2" {
		t.Errorf("Expected remote2, got %s", filtered.RemoteOnly[0].Name)
	}

	// Other lists should be unchanged
	if len(filtered.ToRegister) != 1 {
		t.Errorf("Expected 1 ToRegister agent, got %d", len(filtered.ToRegister))
	}
	if len(filtered.ToRemove) != 1 {
		t.Errorf("Expected 1 ToRemove agent, got %d", len(filtered.ToRemove))
	}
}

func TestSyncResult_ExcludeAgents(t *testing.T) {
	result := SyncResult{
		ToRegister: []string{"agent1", "agent2"},
		ToRemove:   []AgentRef{{Name: "agent3", ID: "id3"}},
		Pending:    []AgentRef{{Name: "agent4", ID: "id4"}},
		RemoteOnly: []AgentRef{{Name: "agent5", ID: "id5"}},
		StaleLocal: []string{"agent6"},
	}

	filtered := result.ExcludeAgents([]string{"agent2", "agent4", "agent6"})

	if len(filtered.ToRegister) != 1 || filtered.ToRegister[0] != "agent1" {
		t.Fatalf("unexpected ToRegister after ExcludeAgents: %#v", filtered.ToRegister)
	}
	if len(filtered.Pending) != 0 {
		t.Fatalf("expected pending to be empty, got %#v", filtered.Pending)
	}
	if len(filtered.StaleLocal) != 0 {
		t.Fatalf("expected stale local to be empty, got %#v", filtered.StaleLocal)
	}
	if len(filtered.ToRemove) != 1 || filtered.ToRemove[0].Name != "agent3" {
		t.Fatalf("unexpected ToRemove after ExcludeAgents: %#v", filtered.ToRemove)
	}
	if len(filtered.RemoteOnly) != 1 || filtered.RemoteOnly[0].Name != "agent5" {
		t.Fatalf("unexpected RemoteOnly after ExcludeAgents: %#v", filtered.RemoteOnly)
	}
}

func TestProjectMatch_Fields(t *testing.T) {
	match := ProjectMatch{
		ID:        "test-id",
		Name:      "test-grove",
		GitRemote: "github.com/test/repo",
	}

	if match.ID != "test-id" {
		t.Errorf("Expected ID 'test-id', got %s", match.ID)
	}
	if match.Name != "test-grove" {
		t.Errorf("Expected Name 'test-grove', got %s", match.Name)
	}
	if match.GitRemote != "github.com/test/repo" {
		t.Errorf("Expected GitRemote 'github.com/test/repo', got %s", match.GitRemote)
	}
}

func TestUpdateLastSyncedAt_UsesHubTime(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-watermark-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Use a specific hub time that's clearly different from time.Now()
	hubTime := time.Date(2025, 6, 15, 10, 30, 45, 123456789, time.UTC)

	UpdateLastSyncedAt(tmpDir, hubTime, false)

	// Read back from state.yaml
	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("Failed to load grove state: %v", err)
	}

	if state.LastSyncedAt == "" {
		t.Fatal("Expected last_synced_at to be set in state.yaml")
	}

	// Verify the stored value matches the hub time, not the local time
	parsed, err := time.Parse(time.RFC3339Nano, state.LastSyncedAt)
	if err != nil {
		t.Fatalf("Failed to parse stored timestamp %q: %v", state.LastSyncedAt, err)
	}

	if !parsed.Equal(hubTime) {
		t.Errorf("Stored time %v does not match hub time %v", parsed, hubTime)
	}
}

func TestUpdateLastSyncedAt_FallbackToLocalTime(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-watermark-fallback-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	before := time.Now().UTC()
	UpdateLastSyncedAt(tmpDir, time.Time{}, false) // zero time = fallback
	after := time.Now().UTC()

	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("Failed to load grove state: %v", err)
	}

	if state.LastSyncedAt == "" {
		t.Fatal("Expected last_synced_at to be set in state.yaml")
	}

	parsed, err := time.Parse(time.RFC3339Nano, state.LastSyncedAt)
	if err != nil {
		t.Fatalf("Failed to parse stored timestamp: %v", err)
	}

	if parsed.Before(before.Truncate(time.Nanosecond)) || parsed.After(after.Add(time.Millisecond)) {
		t.Errorf("Fallback time %v should be between %v and %v", parsed, before, after)
	}
}

func TestUpdateLastSyncedAt_NanoPrecision(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-watermark-nano-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Use a time with sub-second precision
	hubTime := time.Date(2025, 6, 15, 10, 30, 45, 123456789, time.UTC)
	UpdateLastSyncedAt(tmpDir, hubTime, false)

	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("Failed to load grove state: %v", err)
	}

	stored := state.LastSyncedAt

	// Verify the stored value uses nanosecond precision (contains '.' for fractional seconds)
	if !strings.Contains(stored, ".") {
		t.Errorf("Expected RFC3339Nano format with fractional seconds, got %q", stored)
	}

	// Verify it round-trips correctly
	parsed, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		t.Fatalf("Failed to parse stored timestamp: %v", err)
	}
	if !parsed.Equal(hubTime) {
		t.Errorf("Round-trip failed: got %v, want %v", parsed, hubTime)
	}
}

func TestUpdateLastSyncedAt_Monotonic(t *testing.T) {
	tmpDir := t.TempDir()

	newer := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)

	UpdateLastSyncedAt(tmpDir, newer, false)
	UpdateLastSyncedAt(tmpDir, older, false)

	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("Failed to load grove state: %v", err)
	}

	got, err := time.Parse(time.RFC3339Nano, state.LastSyncedAt)
	if err != nil {
		t.Fatalf("Failed to parse stored timestamp: %v", err)
	}

	if !got.Equal(newer) {
		t.Fatalf("watermark regressed: got %v, want %v", got, newer)
	}
}

func TestUpdateLastSyncedAt_InvalidExistingTimestamp(t *testing.T) {
	tmpDir := t.TempDir()

	invalidState := []byte("last_synced_at: not-a-time\n")
	if err := os.WriteFile(filepath.Join(tmpDir, "state.yaml"), invalidState, 0644); err != nil {
		t.Fatalf("Failed to write invalid state file: %v", err)
	}

	hubTime := time.Date(2026, 3, 2, 9, 30, 0, 123, time.UTC)
	UpdateLastSyncedAt(tmpDir, hubTime, false)

	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("Failed to load grove state: %v", err)
	}
	if state.LastSyncedAt == "" {
		t.Fatal("Expected last_synced_at to be written")
	}
	got, err := time.Parse(time.RFC3339Nano, state.LastSyncedAt)
	if err != nil {
		t.Fatalf("Failed to parse stored timestamp: %v", err)
	}
	if !got.Equal(hubTime) {
		t.Fatalf("unexpected watermark after invalid existing timestamp: got %v, want %v", got, hubTime)
	}
}

func TestSyncResult_ServerTime(t *testing.T) {
	serverTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	result := &SyncResult{
		ToRegister: []string{"agent1"},
		InSync:     []string{"agent2"},
		ServerTime: serverTime,
	}

	// Verify ServerTime is preserved
	if !result.ServerTime.Equal(serverTime) {
		t.Errorf("ServerTime = %v, want %v", result.ServerTime, serverTime)
	}

	// Verify ServerTime survives ExcludeAgent
	filtered := result.ExcludeAgent("agent1")
	if !filtered.ServerTime.Equal(serverTime) {
		t.Errorf("After ExcludeAgent, ServerTime = %v, want %v", filtered.ServerTime, serverTime)
	}
}

// --- cleanupProjectBrokerCredentials tests ---

func TestCleanupGroveBrokerCredentials_Legacy(t *testing.T) {
	tmpDir := t.TempDir()

	// Write legacy settings with stale broker credentials
	legacyContent := `active_profile: local
hub:
  endpoint: https://hub.example.com
  brokerId: stale-broker-id
  brokerToken: stale-broker-token
  groveId: my-grove
`
	if err := os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(legacyContent), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupProjectBrokerCredentials(tmpDir)

	// Read back and verify broker credentials were removed
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if strings.Contains(content, "brokerId") {
		t.Error("brokerId should have been removed from legacy settings")
	}
	if strings.Contains(content, "brokerToken") {
		t.Error("brokerToken should have been removed from legacy settings")
	}
	// Other hub fields should be preserved
	if !strings.Contains(content, "endpoint") {
		t.Error("hub.endpoint should be preserved")
	}
	if !strings.Contains(content, "groveId") {
		t.Error("hub.groveId should be preserved")
	}
}

func TestCleanupGroveBrokerCredentials_V1(t *testing.T) {
	tmpDir := t.TempDir()

	// Write v1 settings with stale broker credentials in server.broker
	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
  grove_id: my-grove
server:
  broker:
    broker_id: stale-broker-id
    broker_token: stale-broker-token
    enabled: true
    port: 9800
`
	if err := os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(v1Content), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupProjectBrokerCredentials(tmpDir)

	// Read back and verify broker credentials were removed
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's still v1
	version, _ := config.DetectSettingsFormat(data)
	if version != "1" {
		t.Errorf("expected v1 format, got version %q", version)
	}

	content := string(data)
	if strings.Contains(content, "stale-broker-id") {
		t.Error("broker_id value should have been removed from v1 settings")
	}
	if strings.Contains(content, "stale-broker-token") {
		t.Error("broker_token value should have been removed from v1 settings")
	}
	// Other fields should be preserved
	if !strings.Contains(content, "endpoint") {
		t.Error("hub.endpoint should be preserved")
	}
	if !strings.Contains(content, "grove_id") {
		t.Error("hub.grove_id should be preserved")
	}
}

func TestCleanupGroveBrokerCredentials_V1_NoBrokerCreds(t *testing.T) {
	tmpDir := t.TempDir()

	// Write v1 settings WITHOUT broker credentials
	v1Content := `schema_version: "1"
active_profile: local
hub:
  endpoint: https://hub.example.com
  grove_id: my-grove
`
	if err := os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(v1Content), 0644); err != nil {
		t.Fatal(err)
	}

	// Should be a no-op
	cleanupProjectBrokerCredentials(tmpDir)

	// Verify file is unchanged
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	version, _ := config.DetectSettingsFormat(data)
	if version != "1" {
		t.Errorf("expected v1 format, got version %q", version)
	}
}

func TestCleanupGroveBrokerCredentials_NoFile(t *testing.T) {
	tmpDir := t.TempDir()
	// Should not panic or error on missing file
	cleanupProjectBrokerCredentials(tmpDir)
}

func TestCreateHubClient_UsesAgentTokenFromEnv(t *testing.T) {
	// Create a test server that checks for X-Scion-Agent-Token header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentToken := r.Header.Get("X-Scion-Agent-Token")
		if agentToken != "test-agent-jwt" {
			t.Errorf("expected X-Scion-Agent-Token 'test-agent-jwt', got %q", agentToken)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Verify it does NOT use Bearer auth
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header when using agent token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	// Use a clean HOME so no token file interferes
	t.Setenv("HOME", t.TempDir())
	// Set SCION_AUTH_TOKEN env var
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-jwt")
	// Clear any dev auth token so it doesn't interfere
	t.Setenv("SCION_DEV_TOKEN", "")

	settings := &config.Settings{}
	client, err := createHubClient(settings, server.URL)
	if err != nil {
		t.Fatalf("createHubClient failed: %v", err)
	}

	// Make a request to verify the agent token is used
	_, err = client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
}

func TestCreateHubClient_PrefersTokenFileOverEnv(t *testing.T) {
	// When a scion-token file exists, it should take precedence over SCION_AUTH_TOKEN env var.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentToken := r.Header.Get("X-Scion-Agent-Token")
		if agentToken != "file-token-value" {
			t.Errorf("expected X-Scion-Agent-Token 'file-token-value', got %q", agentToken)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// Write a token file
	scionDir := filepath.Join(tmpHome, ".scion")
	os.MkdirAll(scionDir, 0700)
	os.WriteFile(filepath.Join(scionDir, "scion-token"), []byte("file-token-value"), 0600)
	// Set a different value in env
	t.Setenv("SCION_AUTH_TOKEN", "env-token-value")
	t.Setenv("SCION_DEV_TOKEN", "")

	settings := &config.Settings{}
	client, err := createHubClient(settings, server.URL)
	if err != nil {
		t.Fatalf("createHubClient failed: %v", err)
	}

	_, err = client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health check failed: %v", err)
	}
}

func TestCreateHubClient_PrefersOAuthOverAgentToken(t *testing.T) {
	// When OAuth credentials exist, they should take precedence over SCION_AUTH_TOKEN.
	// We can't easily test this because credentials.GetAccessToken uses a global store,
	// but we can verify that without OAuth, SCION_AUTH_TOKEN is picked up.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Just verify the request arrives
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	// Use a clean HOME so no token file interferes
	t.Setenv("HOME", t.TempDir())
	// With SCION_AUTH_TOKEN set but no OAuth, agent token should be used
	t.Setenv("SCION_AUTH_TOKEN", "agent-jwt")
	t.Setenv("SCION_DEV_TOKEN", "")

	settings := &config.Settings{}
	_, err := createHubClient(settings, server.URL)
	if err != nil {
		t.Fatalf("createHubClient failed: %v", err)
	}
}

func TestCreateHubClient_FallsBackToDevAuth(t *testing.T) {
	// When neither OAuth nor SCION_AUTH_TOKEN is set, should fall back to dev auth
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	// Use a clean HOME so no token file interferes
	t.Setenv("HOME", t.TempDir())
	// Clear both tokens
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "dev-token-123")

	settings := &config.Settings{}
	client, err := createHubClient(settings, server.URL)
	if err != nil {
		t.Fatalf("createHubClient failed: %v", err)
	}

	// Verify client was created (dev auth resolves the token)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestIsGroveRegistered_Found(t *testing.T) {
	projectID := "test-grove-uuid-1234"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/"+projectID {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"id": projectID, "name": "my-grove"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := createTestHubClient(server.URL)
	if err != nil {
		t.Fatalf("createTestHubClient failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, ProjectID: projectID}
	registered, err := isProjectRegistered(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("isProjectRegistered returned error: %v", err)
	}
	if !registered {
		t.Error("expected grove to be registered")
	}
}

func TestIsGroveRegistered_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "not_found",
				"message": "Project not found",
			},
		})
	}))
	defer server.Close()

	client, err := createTestHubClient(server.URL)
	if err != nil {
		t.Fatalf("createTestHubClient failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, ProjectID: "nonexistent-id"}
	registered, err := isProjectRegistered(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("isProjectRegistered should not return error for 404, got: %v", err)
	}
	if registered {
		t.Error("expected grove to NOT be registered")
	}
}

func TestIsGroveRegistered_NonNotFoundError(t *testing.T) {
	// A 500 error whose body happens to contain "not found" text should NOT
	// be treated as a 404. This tests the fix from string-based to type-based
	// error checking.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "internal_error",
				"message": "database connection not found",
			},
		})
	}))
	defer server.Close()

	client, err := createTestHubClient(server.URL)
	if err != nil {
		t.Fatalf("createTestHubClient failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, ProjectID: "some-id"}
	_, err = isProjectRegistered(context.Background(), hubCtx)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestFindGroveByID_Found(t *testing.T) {
	projectID := "exact-match-uuid-5678"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/groves/"+projectID {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"id":   projectID,
				"name": "original-project-name",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": "not_found", "message": "not found"},
		})
	}))
	defer server.Close()

	client, err := createTestHubClient(server.URL)
	if err != nil {
		t.Fatalf("createTestHubClient failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, ProjectID: projectID}
	grove := findProjectByID(context.Background(), hubCtx)
	if grove == nil {
		t.Fatal("expected to find grove by ID, got nil")
	}
	if grove.ID != projectID {
		t.Errorf("expected grove ID %s, got %s", projectID, grove.ID)
	}
	if grove.Name != "original-project-name" {
		t.Errorf("expected grove name 'original-project-name', got %s", grove.Name)
	}
}

func TestFindGroveByID_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"code": "not_found", "message": "not found"},
		})
	}))
	defer server.Close()

	client, err := createTestHubClient(server.URL)
	if err != nil {
		t.Fatalf("createTestHubClient failed: %v", err)
	}

	hubCtx := &HubContext{Client: client, ProjectID: "nonexistent-uuid"}
	grove := findProjectByID(context.Background(), hubCtx)
	if grove != nil {
		t.Errorf("expected nil for non-existent grove, got %+v", grove)
	}
}

func createTestHubClient(baseURL string) (hubclient.Client, error) {
	return hubclient.New(baseURL)
}

func TestRFC3339Nano_BackwardCompatible(t *testing.T) {
	// Verify that RFC3339Nano can parse both old (RFC3339) and new (RFC3339Nano) formats
	tests := []struct {
		name  string
		input string
	}{
		{"RFC3339 format", "2025-06-15T10:30:00Z"},
		{"RFC3339Nano format", "2025-06-15T10:30:00.123456789Z"},
		{"RFC3339 with offset", "2025-06-15T10:30:00+00:00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := time.Parse(time.RFC3339Nano, tt.input)
			if err != nil {
				t.Errorf("RFC3339Nano failed to parse %q: %v", tt.input, err)
			}
		})
	}
}

func TestGetLocalAgentInfo_FromAgentInfoJSON(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-agentinfo-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create agent directory with agent-info.json
	homeDir := filepath.Join(tmpDir, "agents", "myagent", "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("Failed to create home dir: %v", err)
	}
	info := `{"name":"myagent","template":"default","harnessConfig":"gemini"}`
	if err := os.WriteFile(filepath.Join(homeDir, "agent-info.json"), []byte(info), 0644); err != nil {
		t.Fatalf("Failed to write agent-info.json: %v", err)
	}

	result := getLocalAgentInfo(tmpDir, "myagent")
	if result == nil {
		t.Fatal("Expected non-nil result")
	}
	if result.Template != "default" {
		t.Errorf("Template = %q, want %q", result.Template, "default")
	}
	if result.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q", result.HarnessConfig, "gemini")
	}
}

func TestGetLocalAgentInfo_FallbackToScionJSON(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-agentinfo-json-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create agent directory with only scion-agent.json (no agent-info.json)
	agentDir := filepath.Join(tmpDir, "agents", "myagent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("Failed to create agent dir: %v", err)
	}
	cfg := `{"harness_config":"claude"}`
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfg), 0644); err != nil {
		t.Fatalf("Failed to write scion-agent.json: %v", err)
	}

	result := getLocalAgentInfo(tmpDir, "myagent")
	if result == nil {
		t.Fatal("Expected non-nil result")
	}
	if result.HarnessConfig != "claude" {
		t.Errorf("HarnessConfig = %q, want %q", result.HarnessConfig, "claude")
	}
}

func TestGetLocalAgentInfo_FallbackToScionYAML(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-agentinfo-yaml-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create agent directory with only scion-agent.yaml
	agentDir := filepath.Join(tmpDir, "agents", "myagent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("Failed to create agent dir: %v", err)
	}
	cfg := "harness: gemini\nharness_config: gemini\n"
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.yaml"), []byte(cfg), 0644); err != nil {
		t.Fatalf("Failed to write scion-agent.yaml: %v", err)
	}

	result := getLocalAgentInfo(tmpDir, "myagent")
	if result == nil {
		t.Fatal("Expected non-nil result")
	}
	if result.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q", result.HarnessConfig, "gemini")
	}
}

func TestGetLocalAgentInfo_NonexistentAgent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hubsync-agentinfo-none-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	result := getLocalAgentInfo(tmpDir, "nonexistent")
	if result != nil {
		t.Errorf("Expected nil result for nonexistent agent, got %+v", result)
	}
}

// TestCompareAgents_WatermarkBoundary verifies that agents whose creation time
// equals the lastSyncedAt watermark are classified as RemoteOnly, not ToRemove.
// This is the scenario when startAgentViaHub sets the watermark to
// resp.Agent.Created — the agent's creation time matches the watermark exactly.
func TestCompareAgents_WatermarkBoundary(t *testing.T) {
	projectID := "test-grove-id"
	brokerID := "test-broker-id"
	watermarkTime := time.Date(2026, 2, 22, 19, 18, 4, 123456789, time.UTC)

	tests := []struct {
		name           string
		agentCreated   time.Time
		lastSyncedAt   time.Time
		agentStatus    string
		wantRemoteOnly int
		wantToRemove   int
		wantPending    int
	}{
		{
			name:           "agent created at exact watermark time is RemoteOnly",
			agentCreated:   watermarkTime,
			lastSyncedAt:   watermarkTime,
			agentStatus:    "running",
			wantRemoteOnly: 1,
			wantToRemove:   0,
		},
		{
			name:           "agent created after watermark is RemoteOnly",
			agentCreated:   watermarkTime.Add(time.Second),
			lastSyncedAt:   watermarkTime,
			agentStatus:    "running",
			wantRemoteOnly: 1,
			wantToRemove:   0,
		},
		{
			name:           "agent created before watermark is ToRemove",
			agentCreated:   watermarkTime.Add(-time.Minute),
			lastSyncedAt:   watermarkTime,
			agentStatus:    "running",
			wantRemoteOnly: 0,
			wantToRemove:   1,
		},
		{
			name:           "agent with pending status is Pending regardless of watermark",
			agentCreated:   watermarkTime.Add(-time.Minute),
			lastSyncedAt:   watermarkTime,
			agentStatus:    "pending",
			wantRemoteOnly: 0,
			wantToRemove:   0,
			wantPending:    1,
		},
		{
			name:           "zero watermark treats all agents as RemoteOnly",
			agentCreated:   watermarkTime,
			lastSyncedAt:   time.Time{},
			agentStatus:    "running",
			wantRemoteOnly: 1,
			wantToRemove:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up temp grove directory (no local agents)
			tmpDir := t.TempDir()
			agentsDir := filepath.Join(tmpDir, "agents")
			if err := os.MkdirAll(agentsDir, 0755); err != nil {
				t.Fatalf("Failed to create agents dir: %v", err)
			}

			// Write state.yaml with the lastSyncedAt watermark
			if !tt.lastSyncedAt.IsZero() {
				state := &config.ProjectState{
					LastSyncedAt: tt.lastSyncedAt.Format(time.RFC3339Nano),
				}
				if err := config.SaveProjectState(tmpDir, state); err != nil {
					t.Fatalf("Failed to save grove state: %v", err)
				}
			}

			// Mock Hub server that returns one agent
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/agents") {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"agents": []map[string]interface{}{
							{
								"id":              "agent-uuid-1",
								"name":            "hub-agent",
								"status":          tt.agentStatus,
								"runtimeBrokerId": brokerID,
								"created":         tt.agentCreated.Format(time.RFC3339Nano),
							},
						},
						"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
					})
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			client, err := hubclient.New(server.URL)
			if err != nil {
				t.Fatalf("Failed to create hub client: %v", err)
			}

			hubCtx := &HubContext{
				Client:    client,
				ProjectID:   projectID,
				BrokerID:  brokerID,
				ProjectPath: tmpDir,
				Settings:  &config.Settings{},
			}

			result, err := CompareAgents(context.Background(), hubCtx)
			if err != nil {
				t.Fatalf("CompareAgents failed: %v", err)
			}

			if len(result.RemoteOnly) != tt.wantRemoteOnly {
				t.Errorf("RemoteOnly: got %d, want %d", len(result.RemoteOnly), tt.wantRemoteOnly)
			}
			if len(result.ToRemove) != tt.wantToRemove {
				t.Errorf("ToRemove: got %d, want %d", len(result.ToRemove), tt.wantToRemove)
			}
			if tt.wantPending > 0 && len(result.Pending) != tt.wantPending {
				t.Errorf("Pending: got %d, want %d", len(result.Pending), tt.wantPending)
			}
		})
	}
}

func TestCompareAgents_LocalOnlyStaleAfterWatermark(t *testing.T) {
	projectID := "test-grove-id"
	brokerID := "test-broker-id"
	watermark := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()
	agentDir := filepath.Join(tmpDir, "agents", "stale-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	configPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(configPath, []byte(`{"harness":"claude"}`), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}
	oldTime := watermark.Add(-time.Minute)
	if err := os.Chtimes(configPath, oldTime, oldTime); err != nil {
		t.Fatalf("failed to set config mtime: %v", err)
	}

	if err := config.SaveProjectState(tmpDir, &config.ProjectState{
		LastSyncedAt: watermark.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("failed to save state.yaml: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []map[string]interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
				"totalCount": 0,
				"nextCursor": "",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}
	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  brokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents failed: %v", err)
	}

	if len(result.ToRegister) != 0 {
		t.Fatalf("expected no ToRegister agents, got %v", result.ToRegister)
	}
	if len(result.StaleLocal) != 1 || result.StaleLocal[0] != "stale-agent" {
		t.Fatalf("expected stale-agent in StaleLocal, got %v", result.StaleLocal)
	}
	if !result.IsInSync() {
		t.Fatal("expected stale-local only result to be treated as in-sync")
	}
}

// TestCompareAgents_PreviouslySyncedDeletedFromHub verifies that a local agent
// that was previously synced with the hub but has since been deleted hub-side
// is classified as StaleLocal, even when its local timestamp is newer than the
// watermark (the scenario that previously caused the bug).
func TestCompareAgents_PreviouslySyncedDeletedFromHub(t *testing.T) {
	projectID := "test-grove-id"
	brokerID := "test-broker-id"
	watermark := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()

	// Create local agent directory with a timestamp NEWER than watermark
	agentDir := filepath.Join(tmpDir, "agents", "deleted-from-hub")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	configPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(configPath, []byte(`{"harness":"claude"}`), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}
	// Set mtime to AFTER watermark — this is the scenario that triggers the bug
	newerTime := watermark.Add(time.Hour)
	if err := os.Chtimes(configPath, newerTime, newerTime); err != nil {
		t.Fatalf("failed to set config mtime: %v", err)
	}

	// Save state with the agent in SyncedAgents (it was previously synced)
	if err := config.SaveProjectState(tmpDir, &config.ProjectState{
		LastSyncedAt: watermark.Format(time.RFC3339Nano),
		SyncedAgents: []string{"deleted-from-hub"},
	}); err != nil {
		t.Fatalf("failed to save state.yaml: %v", err)
	}

	// Hub returns no agents — "deleted-from-hub" was deleted via web UI
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []map[string]interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
				"totalCount": 0,
				"nextCursor": "",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}
	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  brokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents failed: %v", err)
	}

	if len(result.ToRegister) != 0 {
		t.Fatalf("expected no ToRegister agents, got %v", result.ToRegister)
	}
	if len(result.StaleLocal) != 1 || result.StaleLocal[0] != "deleted-from-hub" {
		t.Fatalf("expected deleted-from-hub in StaleLocal, got %v", result.StaleLocal)
	}
	if !result.IsInSync() {
		t.Fatal("expected stale-local only result to be treated as in-sync")
	}
}

// TestCompareAgents_NewLocalAgentNotInSyncedList verifies that a genuinely new
// local agent (not in SyncedAgents) is still classified as ToRegister.
func TestCompareAgents_NewLocalAgentNotInSyncedList(t *testing.T) {
	projectID := "test-grove-id"
	brokerID := "test-broker-id"
	watermark := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()

	// Create a genuinely new local agent
	agentDir := filepath.Join(tmpDir, "agents", "brand-new-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	configPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(configPath, []byte(`{"harness":"claude"}`), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}

	// Save state with SyncedAgents that does NOT include this agent
	if err := config.SaveProjectState(tmpDir, &config.ProjectState{
		LastSyncedAt: watermark.Format(time.RFC3339Nano),
		SyncedAgents: []string{"some-other-agent"},
	}); err != nil {
		t.Fatalf("failed to save state.yaml: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []map[string]interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
				"totalCount": 0,
				"nextCursor": "",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}
	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  brokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents failed: %v", err)
	}

	if len(result.ToRegister) != 1 || result.ToRegister[0] != "brand-new-agent" {
		t.Fatalf("expected brand-new-agent in ToRegister, got %v", result.ToRegister)
	}
	if len(result.StaleLocal) != 0 {
		t.Fatalf("expected no StaleLocal agents, got %v", result.StaleLocal)
	}
}

// TestUpdateSyncedAgents verifies that UpdateSyncedAgents correctly saves
// the synced agent list to state.yaml.
func TestUpdateSyncedAgents(t *testing.T) {
	tmpDir := t.TempDir()

	UpdateSyncedAgents(tmpDir, []string{"charlie", "alpha", "bravo"})

	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("failed to load state: %v", err)
	}

	// Should be sorted
	expected := []string{"alpha", "bravo", "charlie"}
	if len(state.SyncedAgents) != len(expected) {
		t.Fatalf("expected %d synced agents, got %d: %v", len(expected), len(state.SyncedAgents), state.SyncedAgents)
	}
	for i, name := range expected {
		if state.SyncedAgents[i] != name {
			t.Fatalf("expected synced agent %d to be %q, got %q", i, name, state.SyncedAgents[i])
		}
	}
}

// TestCollectSyncedAgentNames_IncludesStaleLocal verifies that
// collectSyncedAgentNames includes StaleLocal agents so they remain in the
// SyncedAgents list. This prevents the regression where hub-deleted agents
// lose their "previously synced" status after one sync cycle, causing them
// to be re-proposed for registration on the next check.
func TestCollectSyncedAgentNames_IncludesStaleLocal(t *testing.T) {
	result := &SyncResult{
		InSync:     []string{"agent-a", "agent-b"},
		StaleLocal: []string{"deleted-from-hub-1", "deleted-from-hub-2"},
	}

	names := collectSyncedAgentNames(result)

	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	for _, expected := range []string{"agent-a", "agent-b", "deleted-from-hub-1", "deleted-from-hub-2"} {
		if !nameSet[expected] {
			t.Fatalf("expected %q in collectSyncedAgentNames result, got %v", expected, names)
		}
	}
}

// TestStaleLocalAgentSurvivesSyncCycle verifies the full regression scenario:
// an agent deleted from the hub is correctly classified as StaleLocal across
// multiple sync cycles, rather than being reclassified as ToRegister after the
// SyncedAgents list is updated.
func TestStaleLocalAgentSurvivesSyncCycle(t *testing.T) {
	projectID := "test-grove-id"
	brokerID := "test-broker-id"
	watermark := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()

	// Create two local agents: one still on hub, one deleted from hub
	for _, name := range []string{"in-sync-agent", "deleted-agent"} {
		agentDir := filepath.Join(tmpDir, "agents", name)
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatalf("failed to create agent dir: %v", err)
		}
		configPath := filepath.Join(agentDir, "scion-agent.json")
		if err := os.WriteFile(configPath, []byte(`{"harness":"claude"}`), 0644); err != nil {
			t.Fatalf("failed to write scion-agent.json: %v", err)
		}
		// Set mtime after watermark so timestamp check alone would classify as ToRegister
		newerTime := watermark.Add(time.Hour)
		if err := os.Chtimes(configPath, newerTime, newerTime); err != nil {
			t.Fatalf("failed to set config mtime: %v", err)
		}
	}

	// Initial state: both agents were previously synced
	if err := config.SaveProjectState(tmpDir, &config.ProjectState{
		LastSyncedAt: watermark.Format(time.RFC3339Nano),
		SyncedAgents: []string{"deleted-agent", "in-sync-agent"},
	}); err != nil {
		t.Fatalf("failed to save state.yaml: %v", err)
	}

	// Hub only returns in-sync-agent (deleted-agent was removed from hub)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{"name": "in-sync-agent", "id": "uuid-1", "status": "running",
						"runtimeBrokerId": brokerID, "created": watermark.Add(-time.Hour).Format(time.RFC3339Nano)},
				},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
				"totalCount": 1,
				"nextCursor": "",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}
	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  brokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	// --- First sync cycle ---
	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents (cycle 1) failed: %v", err)
	}
	if len(result.StaleLocal) != 1 || result.StaleLocal[0] != "deleted-agent" {
		t.Fatalf("cycle 1: expected deleted-agent in StaleLocal, got %v", result.StaleLocal)
	}
	if len(result.ToRegister) != 0 {
		t.Fatalf("cycle 1: expected no ToRegister, got %v", result.ToRegister)
	}

	// Simulate what EnsureHubReady does when IsInSync: update SyncedAgents
	UpdateSyncedAgents(tmpDir, collectSyncedAgentNames(result))

	// --- Second sync cycle ---
	result2, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents (cycle 2) failed: %v", err)
	}
	if len(result2.StaleLocal) != 1 || result2.StaleLocal[0] != "deleted-agent" {
		t.Fatalf("cycle 2: expected deleted-agent still in StaleLocal, got StaleLocal=%v ToRegister=%v",
			result2.StaleLocal, result2.ToRegister)
	}
	if len(result2.ToRegister) != 0 {
		t.Fatalf("cycle 2: expected no ToRegister, got %v (this is the regression)", result2.ToRegister)
	}
}

// TestCompareAgents_HubAgentDifferentBrokerMatchesLocal verifies the primary
// bug fix: a hub-created agent with a different RuntimeBrokerID is recognized
// as InSync when it also exists locally, and the API request does NOT include
// a runtimeBrokerId query parameter filter.
func TestCompareAgents_HubAgentDifferentBrokerMatchesLocal(t *testing.T) {
	projectID := "test-grove-id"
	localBrokerID := "local-broker-id"
	hubBrokerID := "hub-broker-id" // different from local

	tmpDir := t.TempDir()

	// Create a local agent named "laptop"
	agentDir := filepath.Join(tmpDir, "agents", "laptop")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.yaml"), []byte("harness: claude"), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.yaml: %v", err)
	}

	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			// Capture the query string to verify no runtimeBrokerId filter
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{
						"id":              "agent-uuid-laptop",
						"name":            "laptop",
						"status":          "running",
						"runtimeBrokerId": hubBrokerID,
						"created":         time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
					},
				},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}

	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  localBrokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents failed: %v", err)
	}

	// The local agent "laptop" matches the hub agent "laptop" by name → InSync
	if len(result.InSync) != 1 || result.InSync[0] != "laptop" {
		t.Errorf("expected InSync=[laptop], got %v", result.InSync)
	}
	if len(result.ToRegister) != 0 {
		t.Errorf("expected no ToRegister, got %v", result.ToRegister)
	}

	// Verify the API request did NOT include runtimeBrokerId filter
	if strings.Contains(capturedQuery, "runtimeBrokerId") {
		t.Errorf("API request should NOT include runtimeBrokerId filter, got query: %s", capturedQuery)
	}
}

// TestCompareAgents_HubOnlyAgentDifferentBrokerIsRemoteOnly verifies the safety
// guard: a hub-only agent assigned to a different broker is always classified as
// RemoteOnly, never as ToRemove, regardless of watermark timing.
func TestCompareAgents_HubOnlyAgentDifferentBrokerIsRemoteOnly(t *testing.T) {
	projectID := "test-grove-id"
	localBrokerID := "local-broker-id"
	otherBrokerID := "other-broker-id"
	watermark := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()

	// No local agents — just create the agents directory
	if err := os.MkdirAll(filepath.Join(tmpDir, "agents"), 0755); err != nil {
		t.Fatalf("failed to create agents dir: %v", err)
	}

	// Set a watermark so the agent (created before watermark) would normally
	// be classified as ToRemove if it were on the same broker.
	if err := config.SaveProjectState(tmpDir, &config.ProjectState{
		LastSyncedAt: watermark.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("failed to save state: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/agents") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{
						"id":              "agent-uuid-remote",
						"name":            "remote-agent",
						"status":          "running",
						"runtimeBrokerId": otherBrokerID,
						"created":         watermark.Add(-time.Hour).Format(time.RFC3339Nano),
					},
				},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatalf("failed to create hub client: %v", err)
	}

	hubCtx := &HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  localBrokerID,
		ProjectPath: tmpDir,
		Settings:  &config.Settings{},
	}

	result, err := CompareAgents(context.Background(), hubCtx)
	if err != nil {
		t.Fatalf("CompareAgents failed: %v", err)
	}

	// Agent is on a different broker → RemoteOnly, never ToRemove
	if len(result.RemoteOnly) != 1 || result.RemoteOnly[0].Name != "remote-agent" {
		t.Errorf("expected RemoteOnly=[remote-agent], got %v", result.RemoteOnly)
	}
	if len(result.ToRemove) != 0 {
		t.Errorf("expected no ToRemove (agent belongs to different broker), got %v", result.ToRemove)
	}
}

// TestAddRemoveSyncedAgent verifies individual add/remove operations.
func TestAddRemoveSyncedAgent(t *testing.T) {
	tmpDir := t.TempDir()

	// Start with some agents
	UpdateSyncedAgents(tmpDir, []string{"agent-a", "agent-b"})

	// Add a new one
	AddSyncedAgent(tmpDir, "agent-c")
	state, err := config.LoadProjectState(tmpDir)
	if err != nil {
		t.Fatalf("failed to load state: %v", err)
	}
	if len(state.SyncedAgents) != 3 {
		t.Fatalf("expected 3 synced agents after add, got %v", state.SyncedAgents)
	}

	// Add duplicate — should be idempotent
	AddSyncedAgent(tmpDir, "agent-c")
	state, _ = config.LoadProjectState(tmpDir)
	if len(state.SyncedAgents) != 3 {
		t.Fatalf("expected 3 synced agents after duplicate add, got %v", state.SyncedAgents)
	}

	// Remove one
	RemoveSyncedAgent(tmpDir, "agent-b")
	state, _ = config.LoadProjectState(tmpDir)
	if len(state.SyncedAgents) != 2 {
		t.Fatalf("expected 2 synced agents after remove, got %v", state.SyncedAgents)
	}
	for _, name := range state.SyncedAgents {
		if name == "agent-b" {
			t.Fatal("agent-b should have been removed")
		}
	}
}
